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
}

func (c ColdS3) enabled() bool { return c.Endpoint != "" && c.Bucket != "" }

// coldSchema mirrors the archived CSV columns written by archive.ExportDay (IPs
// and timestamps are stored as strings in the CSV).
const coldSchema = "isp_id UInt32, device_id UInt32, src_ip String, src_port UInt16, " +
	"dst_ip String, dst_port UInt16, nat_public_ip String, nat_public_port UInt16, " +
	"protocol UInt8, bytes UInt64, packets UInt64, flow_start String, flow_end String, " +
	"flow_type String, exporter_ip String"

// coldURL builds the s3() glob for the archived objects in scope. ISP-scoped
// queries only read that tenant's prefix; a director (ispID 0) reads all.
func (c ColdS3) url(ispID uint32) string {
	parts := []string{strings.TrimRight(c.Endpoint, "/"), c.Bucket}
	if p := strings.Trim(c.Prefix, "/"); p != "" {
		parts = append(parts, p)
	}
	if ispID != 0 {
		parts = append(parts, fmt.Sprintf("isp_id=%d", ispID))
	}
	return strings.Join(parts, "/") + "/**/*.csv.gz"
}

// SearchCold runs the same filter against the archived CSVs in S3 via the s3()
// table function. Returns natRecords identical in shape to the hot Search.
func (r *FlowReader) SearchCold(ctx context.Context, f SearchFilter, limit int, c ColdS3) ([]natRecord, error) {
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
		add("parseDateTimeBestEffortOrNull(flow_start) >= ?", f.From.UTC())
	}
	if !f.To.IsZero() {
		add("parseDateTimeBestEffortOrNull(flow_start) <= ?", f.To.UTC())
	}
	if f.ISPID != 0 {
		add("isp_id = ?", f.ISPID)
	}
	if len(conds) == 0 {
		return nil, fmt.Errorf("no filter")
	}
	// The s3() endpoint + credentials are server config (not user input), so they
	// are interpolated; all filter values remain bound parameters.
	src := fmt.Sprintf("s3(%s, %s, %s, 'CSVWithNames', %s)",
		quote(c.url(f.ISPID)), quote(c.AccessKey), quote(c.SecretKey), quote(coldSchema))
	q := fmt.Sprintf(`SELECT flow_start, device_id, src_ip, src_port, nat_public_ip, nat_public_port,
		dst_ip, dst_port, protocol, flow_type FROM %s WHERE %s
		ORDER BY parseDateTimeBestEffortOrNull(flow_start) DESC LIMIT %d`,
		src, strings.Join(conds, " AND "), limit)
	rs, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := make([]natRecord, 0, 64)
	for rs.Next() {
		var fstart, sip, pip, dip, ft string
		var dev uint32
		var sp, pp, dp uint16
		var pr uint8
		if err := rs.Scan(&fstart, &dev, &sip, &sp, &pip, &pp, &dip, &dp, &pr, &ft); err != nil {
			return out, err
		}
		ts, _ := time.Parse(time.RFC3339, fstart)
		ts = ts.In(istLoc)
		out = append(out, natRecord{
			Date: ts.Format("2006-01-02"), Clock: ts.Format("15:04:05"), Time: ts.Format("2006-01-02 15:04:05"),
			Sub: fmt.Sprintf("DEV-%d", dev), PrivIP: sip, PrivPort: int(sp),
			PubIP: pip, PubPort: int(pp), Proto: protoName(pr),
			Dest: fmt.Sprintf("%s:%d", dip, dp), Action: strings.ToUpper(ft),
		})
	}
	return out, rs.Err()
}

// quote single-quotes a literal for inlining into a ClickHouse query.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "\\'") + "'" }

// searchAll runs the hot search and, when the query window reaches into archived
// days, also the cold (S3) search, then merges newest-first up to limit. The
// bool reports whether cold storage was consulted.
func (s *Server) searchAll(ctx context.Context, f SearchFilter, limit int) ([]natRecord, bool, error) {
	hot, err := s.flows.Search(ctx, f, limit)
	if err != nil {
		return nil, false, err
	}
	cold := s.coldInfo()
	if !cold.enabled() || !s.rangeHasArchived(ctx, f) {
		return hot, false, nil
	}
	cr, cerr := s.flows.SearchCold(ctx, f, limit, cold)
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

// rangeHasArchived reports whether any archived day overlaps the query's time
// window (so we only pay the S3 round-trip when there is cold data to find).
func (s *Server) rangeHasArchived(ctx context.Context, f SearchFilter) bool {
	ad, err := s.store.ListArchivedDays(ctx, 4000)
	if err != nil || len(ad) == 0 {
		return false
	}
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
		return true
	}
	return false
}
