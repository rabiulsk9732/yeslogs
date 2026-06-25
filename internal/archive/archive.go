// Package archive exports one day of flow data from ClickHouse and uploads it to
// an S3-compatible bucket. v0.3.1 produces gzip-compressed CSV; the parquet
// format is reserved (config export_format) and currently falls back to CSV.gz.
package archive

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/natflow/natflow-dataplane/internal/archive/s3"
	"github.com/natflow/natflow-dataplane/internal/metrics"
)

var csvHeader = []string{
	"isp_id", "device_id", "src_ip", "src_port", "dst_ip", "dst_port",
	"nat_public_ip", "nat_public_port", "protocol", "bytes", "packets",
	"flow_start", "flow_end", "flow_type", "exporter_ip",
}

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
	if format == "parquet" {
		e.log.Warn("parquet export not implemented in v0.3.1; writing CSV.gz instead")
	}
	dateStr := day.UTC().Format("2006-01-02")

	tmp, err := os.CreateTemp("", "natflow-archive-*.csv.gz")
	if err != nil {
		return Result{}, fmt.Errorf("temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	gz := gzip.NewWriter(tmp)
	cw := csv.NewWriter(gz)
	if err := cw.Write(csvHeader); err != nil {
		return Result{}, err
	}

	q := fmt.Sprintf(`SELECT isp_id, device_id, src_ip, src_port, dst_ip, dst_port,
		nat_public_ip, nat_public_port, protocol, bytes, packets,
		flow_start, flow_end, flow_type, exporter_ip
		FROM %s.%s WHERE event_date = ? AND isp_id = ?`, e.db, e.table)
	rows, err := e.conn.Query(ctx, q, dateStr, ispID)
	if err != nil {
		return Result{}, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var (
		count                      int64
		dbISP, devID               uint32
		srcIP, dstIP, natIP, expIP net.IP
		srcPort, dstPort, natPort  uint16
		proto                      uint8
		nbytes, npkts              uint64
		fstart, fend               time.Time
		ftype                      string
	)
	for rows.Next() {
		if err := rows.Scan(&dbISP, &devID, &srcIP, &srcPort, &dstIP, &dstPort,
			&natIP, &natPort, &proto, &nbytes, &npkts, &fstart, &fend, &ftype, &expIP); err != nil {
			return Result{}, fmt.Errorf("scan: %w", err)
		}
		rec := []string{
			strconv.FormatUint(uint64(dbISP), 10), strconv.FormatUint(uint64(devID), 10),
			srcIP.String(), strconv.FormatUint(uint64(srcPort), 10),
			dstIP.String(), strconv.FormatUint(uint64(dstPort), 10),
			natIP.String(), strconv.FormatUint(uint64(natPort), 10),
			strconv.FormatUint(uint64(proto), 10),
			strconv.FormatUint(nbytes, 10), strconv.FormatUint(npkts, 10),
			fstart.UTC().Format(time.RFC3339), fend.UTC().Format(time.RFC3339),
			ftype, expIP.String(),
		}
		if err := cw.Write(rec); err != nil {
			return Result{}, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return Result{}, fmt.Errorf("rows: %w", err)
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return Result{}, err
	}
	if err := gz.Close(); err != nil {
		return Result{}, err
	}
	if err := tmp.Sync(); err != nil {
		return Result{}, err
	}

	info, err := tmp.Stat()
	if err != nil {
		return Result{}, err
	}
	size := info.Size()
	if _, err := tmp.Seek(0, 0); err != nil {
		return Result{}, err
	}

	rel := s3.ArchiveRel(ispID, day, "csv.gz")
	start := time.Now()
	key, err := e.s3.Upload(ctx, rel, tmp, size, "application/gzip")
	if err != nil {
		return Result{}, err
	}
	if e.metrics != nil {
		e.metrics.ArchiveUploadLatency.Observe(float64(time.Since(start).Milliseconds()))
		e.metrics.ArchiveRows.Add(float64(count))
		e.metrics.ArchiveBytes.Add(float64(size))
	}
	e.log.Info("archived day", "date", dateStr, "isp_id", ispID, "rows", count, "bytes", size, "key", key)
	return Result{Rows: count, Bytes: size, Key: key}, nil
}
