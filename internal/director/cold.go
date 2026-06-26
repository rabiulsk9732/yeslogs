package director

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ColdS3 is the S3 config used to read archived flow logs back via ClickHouse's
// s3() table function (cold search beyond the hot-storage window).
type ColdS3 struct {
	Endpoint  string // e.g. https://idr01.zata.ai
	Bucket    string
	Prefix    string // object path prefix (e.g. "natlog")
	AccessKey string
	SecretKey string
	Region    string
	Format    string // "parquet" (default) or "csvgz" — must match the archive format
}

func (c ColdS3) enabled() bool { return c.Endpoint != "" && c.Bucket != "" }

// chFormat maps the configured archive format to the ClickHouse format name + the
// object extension used in the S3 key.
func (c ColdS3) chFormat() (name, ext string) {
	if c.Format == "csvgz" || c.Format == "csv" {
		return "CSVWithNames", "csv.gz"
	}
	return "Parquet", "parquet"
}

// coldSchema describes the archived CSV columns (only needed for CSVWithNames;
// Parquet carries its own schema). Matches archive.ExportDay output.
const coldSchema = "isp_id UInt32, device_id UInt32, src_ip String, src_port UInt16, " +
	"dst_ip String, dst_port UInt16, nat_public_ip String, nat_public_port UInt16, " +
	"protocol UInt8, bytes UInt64, packets UInt64, flow_start String, flow_end String, " +
	"flow_type String, exporter_ip String"

// urlForDays builds an s3() path that reads ONLY the given archived days (date
// pruning) instead of globbing the whole bucket. ISP 0 (director) reads all ISPs.
func (c ColdS3) urlForDays(ispID uint32, days []string) string {
	parts := []string{strings.TrimRight(c.Endpoint, "/"), c.Bucket}
	if p := strings.Trim(c.Prefix, "/"); p != "" {
		parts = append(parts, p)
	}
	base := strings.Join(parts, "/")
	isp := "isp_id=*"
	if ispID != 0 {
		isp = fmt.Sprintf("isp_id=%d", ispID)
	}
	_, ext := c.chFormat()
	var segs []string
	for _, d := range days {
		t, err := time.Parse("2006-01-02", d)
		if err != nil {
			continue
		}
		segs = append(segs, fmt.Sprintf("year=%04d/month=%02d/day=%02d/part-000.%s", t.Year(), int(t.Month()), t.Day(), ext))
	}
	switch len(segs) {
	case 0:
		return base + "/" + isp + "/**/*." + ext // fallback (shouldn't happen)
	case 1:
		return base + "/" + isp + "/" + segs[0]
	default:
		return base + "/" + isp + "/{" + strings.Join(segs, ",") + "}"
	}
}

// SearchCold reads the given archived days back from S3 via the s3() table
// function, applying the same filter. Returns natRecords identical to hot Search.
func (r *FlowReader) SearchCold(ctx context.Context, f SearchFilter, limit int, c ColdS3, days []string) ([]natRecord, error) {
	name, _ := c.chFormat()
	parquet := name == "Parquet"
	// flow_start is native DateTime in Parquet; a string in CSV.
	tsExpr := "flow_start"
	if !parquet {
		tsExpr = "parseDateTimeBestEffortOrNull(flow_start)"
	}

	var conds []string
	var args []any
	add := func(cond string, a any) { conds = append(conds, cond); args = append(args, a) }
	if f.PublicIP != "" {
		add("nat_public_ip = ?", f.PublicIP)
	}
	if f.PrivateIP != "" {
		add("src_ip = ?", f.PrivateIP)
	}
	if f.DestIP != "" {
		add("dst_ip = ?", f.DestIP)
	}
	if f.PublicPort > 0 {
		add("nat_public_port = ?", uint16(f.PublicPort))
	}
	if pn := protoNum(f.Proto); pn > 0 {
		add("protocol = ?", pn)
	}
	if f.DeviceID > 0 {
		add("device_id = ?", f.DeviceID)
	}
	if !f.From.IsZero() {
		add(tsExpr+" >= ?", f.From.UTC())
	}
	if !f.To.IsZero() {
		add(tsExpr+" <= ?", f.To.UTC())
	}
	if f.ISPID != 0 {
		add("isp_id = ?", f.ISPID)
	}
	if len(conds) == 0 {
		return nil, fmt.Errorf("no filter")
	}

	url := c.urlForDays(f.ISPID, days)
	var src string
	if parquet {
		src = fmt.Sprintf("s3(%s, %s, %s, 'Parquet')", quote(url), quote(c.AccessKey), quote(c.SecretKey))
	} else {
		src = fmt.Sprintf("s3(%s, %s, %s, 'CSVWithNames', %s)", quote(url), quote(c.AccessKey), quote(c.SecretKey), quote(coldSchema))
	}
	q := fmt.Sprintf(`SELECT %s AS ts, device_id, src_ip, src_port, nat_public_ip, nat_public_port,
		dst_ip, dst_port, protocol, flow_type FROM %s WHERE %s ORDER BY ts DESC LIMIT %d`,
		tsExpr, src, strings.Join(conds, " AND "), limit)
	rs, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := make([]natRecord, 0, 64)
	for rs.Next() {
		var ts time.Time
		var sip, pip, dip, ft string
		var dev uint32
		var sp, pp, dp uint16
		var pr uint8
		if err := rs.Scan(&ts, &dev, &sip, &sp, &pip, &pp, &dip, &dp, &pr, &ft); err != nil {
			return out, err
		}
		ts = ts.In(istLoc)
		out = append(out, natRecord{
			Date: ts.Format("2006-01-02"), Clock: ts.Format("15:04:05"), Time: ts.Format("2006-01-02 15:04:05"),
			Sub: fmt.Sprintf("DEV-%d", dev), DevID: dev, PrivIP: sip, PrivPort: int(sp),
			PubIP: pip, PubPort: int(pp), Proto: protoName(pr),
			Dest: fmt.Sprintf("%s:%d", dip, dp), Action: strings.ToUpper(ft),
		})
	}
	return out, rs.Err()
}

// quote returns s as a ClickHouse string literal — escaping backslashes first,
// then single quotes (order matters), so config values containing \ or ' can't
// break out of the literal.
func quote(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "'", "\\'")
	return "'" + s + "'"
}

// searchAll runs the hot search and, when the query window overlaps archived
// days, also the cold (S3) search for exactly those days, then merges
// newest-first up to limit. The bool reports whether cold storage was consulted.
func (s *Server) searchAll(ctx context.Context, f SearchFilter, limit int) ([]natRecord, bool, error) {
	hot, err := s.flows.Search(ctx, f, limit)
	if err != nil {
		return nil, false, err
	}
	cold := s.coldInfo()
	days := s.archivedDaysInRange(ctx, f)
	if !cold.enabled() || len(days) == 0 {
		return hot, false, nil
	}
	cr, cerr := s.flows.SearchCold(ctx, f, limit, cold, days)
	if cerr != nil {
		s.log.Error("cold search failed; returning hot results only", "error", cerr)
		return hot, true, nil // degrade gracefully — never fail a search because S3 is slow/down
	}
	merged := append(hot, cr...)
	sort.Slice(merged, func(i, j int) bool { return merged[i].Time > merged[j].Time })
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, true, nil
}

// archivedDaysInRange returns archived days (YYYY-MM-DD) overlapping the query's
// time window — used to read only the needed S3 objects (date pruning).
func (s *Server) archivedDaysInRange(ctx context.Context, f SearchFilter) []string {
	ad, err := s.store.ListArchivedDays(ctx, 4000)
	if err != nil || len(ad) == 0 {
		return nil
	}
	var out []string
	for _, a := range ad {
		day, e := time.ParseInLocation("2006-01-02", a.Day, istLoc)
		if e != nil {
			continue
		}
		dayEnd := day.Add(24 * time.Hour)
		if !f.From.IsZero() && dayEnd.Before(f.From) {
			continue
		}
		if !f.To.IsZero() && day.After(f.To) {
			continue
		}
		out = append(out, a.Day)
	}
	return out
}
