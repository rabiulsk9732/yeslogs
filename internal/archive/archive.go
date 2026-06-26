// Package archive exports one day of flow data from ClickHouse and uploads it to
// an S3-compatible bucket. v0.3.1 produces gzip-compressed CSV; the parquet
// format is reserved (config export_format) and currently falls back to CSV.gz.
package archive

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/natflow/natflow-dataplane/internal/archive/s3"
	"github.com/natflow/natflow-dataplane/internal/metrics"
)

// chLit escapes a value for inlining as a ClickHouse string literal. Used only
// for server-side config (S3 URL + credentials), not for user input.
func chLit(s string) string { return strings.ReplaceAll(s, "'", "\\'") }

// Exporter reads from ClickHouse and writes day partitions to S3.
type Exporter struct {
	conn    driver.Conn
	s3      *s3.Client
	db      string
	table   string
	metrics *metrics.Metrics
	log     *slog.Logger
}

// New builds an Exporter.
func New(conn driver.Conn, client *s3.Client, db, table string, m *metrics.Metrics, log *slog.Logger) *Exporter {
	return &Exporter{conn: conn, s3: client, db: db, table: table, metrics: m, log: log.With("component", "archive")}
}

// Result summarizes one export.
type Result struct {
	Rows  int64
	Bytes int64
	Key   string
}

// ExportDay exports rows for ispID on the given day to S3. format is "csvgz" or
// "parquet" (parquet falls back to csvgz in v0.3.1).
func (e *Exporter) ExportDay(ctx context.Context, ispID uint32, day time.Time, format string) (Result, error) {
	if e.metrics != nil {
		e.metrics.ArchiveRuns.Inc()
	}
	res, err := e.exportDay(ctx, ispID, day, format)
	if err != nil && e.metrics != nil {
		e.metrics.ArchiveErrors.Inc()
	}
	return res, err
}

func (e *Exporter) exportDay(ctx context.Context, ispID uint32, day time.Time, format string) (Result, error) {
	dateStr := day.UTC().Format("2006-01-02")

	// Count first — an empty day writes no object.
	var cu uint64
	cq := fmt.Sprintf(`SELECT count() FROM %s.%s WHERE event_date = ? AND isp_id = ?`, e.db, e.table)
	if err := e.conn.QueryRow(ctx, cq, dateStr, ispID).Scan(&cu); err != nil {
		return Result{}, fmt.Errorf("count: %w", err)
	}
	if cu == 0 {
		return Result{}, nil
	}
	count := int64(cu)

	chFormat, ext := "Parquet", "parquet"
	if format == "csvgz" || format == "csv" {
		chFormat, ext = "CSVWithNames", "csv.gz"
	}
	rel := s3.ArchiveRel(ispID, day, ext)
	url := e.s3.ObjectURL(rel)

	// ClickHouse streams the partition straight to S3 (no Go-side buffering /
	// temp file) — scales to very large days. IPs as dotted strings, times as
	// native DateTime. event_date/isp_id stay bound params; only server config
	// (URL + credentials) is interpolated.
	start := time.Now()
	ins := fmt.Sprintf(`INSERT INTO FUNCTION s3('%s','%s','%s','%s')
		SELECT isp_id, device_id,
			IPv4NumToString(src_ip) AS src_ip, src_port,
			IPv4NumToString(dst_ip) AS dst_ip, dst_port,
			IPv4NumToString(nat_public_ip) AS nat_public_ip, nat_public_port,
			protocol, bytes, packets, flow_start, flow_end, flow_type,
			IPv4NumToString(exporter_ip) AS exporter_ip
		FROM %s.%s WHERE event_date = ? AND isp_id = ?`,
		chLit(url), chLit(e.s3.AccessKey()), chLit(e.s3.SecretKey()), chFormat, e.db, e.table)
	if err := e.conn.Exec(ctx, ins, dateStr, ispID); err != nil {
		return Result{}, fmt.Errorf("export to s3: %w", err)
	}
	size, _ := e.s3.Stat(ctx, rel)
	key := e.s3.Key(rel)
	if e.metrics != nil {
		e.metrics.ArchiveUploadLatency.Observe(float64(time.Since(start).Milliseconds()))
		e.metrics.ArchiveRows.Add(float64(count))
		e.metrics.ArchiveBytes.Add(float64(size))
	}
	e.log.Info("archived day", "date", dateStr, "isp_id", ispID, "rows", count, "bytes", size, "key", key, "format", chFormat)
	return Result{Rows: count, Bytes: size, Key: key}, nil
}
