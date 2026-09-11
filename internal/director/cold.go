package director

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/natflow/natflow-dataplane/internal/director/store"
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
	"nat_event UInt8, username String, " +
	"protocol UInt8, bytes UInt64, packets UInt64, flow_start String, flow_end String, " +
	"flow_type String, exporter_ip String, nat_dest_ip String, nat_dest_port UInt16"

// The explicit schema below supplies defaults for columns absent from older
// archives. Keep all available evidence in the key, including both NAT sides.
const dedupKeyCold = "isp_id, flow_start, flow_end, src_ip, src_port, dst_ip, dst_port, " +
	"nat_public_ip, nat_public_port, nat_dest_ip, nat_dest_port, protocol, flow_type, bytes, packets, nat_event, username"

func coldParquetSchema() string {
	return strings.ReplaceAll(strings.ReplaceAll(coldSchema, "flow_start String", "flow_start DateTime('Asia/Kolkata')"), "flow_end String", "flow_end DateTime('Asia/Kolkata')")
}

// coldReadSettings let a glob span archives written before nat_event/username
// existed alongside newer ones. Without this a single old object in the range
// fails the whole query, which would make every pre-2026-08-26 day unreadable —
// trading a new column for the loss of months of searchable evidence.
const coldReadSettings = " SETTINGS input_format_parquet_allow_missing_columns = 1, " +
	"input_format_csv_allow_variable_number_of_columns = 1, " +
	"input_format_with_names_use_header = 1, input_format_defaults_for_omitted_fields = 1, " +
	"input_format_parquet_skip_columns_with_unsupported_types_in_schema_inference = 1"

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

	f.destinationNATAvailable = true // Explicit schema supplies unknown defaults for old archives.
	conds, args, ok := coldWhere(f, tsExpr)
	if !ok {
		return nil, fmt.Errorf("no filter")
	}

	url := c.urlForDays(f.ISPID, days)
	var src string
	if parquet {
		src = fmt.Sprintf("s3(%s, %s, %s, 'Parquet', %s)", quote(url), quote(c.AccessKey), quote(c.SecretKey), quote(coldParquetSchema()))
	} else {
		src = fmt.Sprintf("s3(%s, %s, %s, 'CSVWithNames', %s)", quote(url), quote(c.AccessKey), quote(c.SecretKey), quote(coldSchema))
	}
	q := fmt.Sprintf(`SELECT %s AS ts, isp_id, device_id, exporter_ip, src_ip, src_port, nat_public_ip, nat_public_port,
		dst_ip, dst_port, nat_dest_ip, nat_dest_port, protocol, flow_type, username, nat_event FROM %s
		WHERE %s ORDER BY ts DESC LIMIT 1 BY %s LIMIT %d%s`,
		tsExpr, src, strings.Join(conds, " AND "), dedupKeyCold, limit, coldReadSettings)
	rs, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := make([]natRecord, 0, 64)
	for rs.Next() {
		var ts time.Time
		var rec natRecord
		var sp, pp, dp, pdp uint16
		var pr uint8
		var ft string
		if err := rs.Scan(&ts, &rec.ISPID, &rec.DevID, &rec.ExporterIP, &rec.PrivIP, &sp, &rec.PubIP, &pp,
			&rec.DstIP, &dp, &rec.PostDstIP, &pdp, &pr, &ft, &rec.Username, &rec.NatEvent); err != nil {
			return out, err
		}
		rec.At = ts.UTC()
		rec.Date, rec.Clock, rec.Time = ts.In(istLoc).Format("2006-01-02"), ts.In(istLoc).Format("15:04:05"), ts.In(istLoc).Format("2006-01-02 15:04:05")
		rec.Sub = fmt.Sprintf("DEV-%d", rec.DevID)
		rec.PrivPort, rec.PubPort, rec.DstPort, rec.PostDstPort = int(sp), int(pp), int(dp), int(pdp)
		rec.Proto, rec.Action = protoName(pr), strings.ToUpper(ft)
		rec.Dest = fmt.Sprintf("%s:%d", rec.DstIP, dp)
		rec.setNATTranslation()
		out = append(out, rec)
	}
	return out, rs.Err()
}

// coldWhere mirrors hotWhere for archived string-IP columns. Tenant isolation
// is enforced by urlForDays (isp_id=<tenant> in the object path), so do not add
// a redundant isp_id predicate here: ClickHouse 26.7 fails that predicate on a
// multi-file brace URL even though each Parquet object contains the column.
func coldWhere(f SearchFilter, tsExpr string) ([]string, []any, bool) {
	var conds []string
	var args []any
	add := func(cond string, a any) { conds = append(conds, cond); args = append(args, a) }
	addNATFilters(f, false, &conds, &args)
	if pn := protoNum(f.Proto); pn > 0 {
		add("protocol = ?", pn)
	}
	if f.DeviceID > 0 {
		add("device_id = ?", f.DeviceID)
	}
	if f.ExporterIP != "" {
		add("exporter_ip = ?", f.ExporterIP)
	}
	if f.Username != "" {
		// Archives written before 2026-08-26 have no username column; the
		// missing-columns setting makes it read as empty there, so a username
		// search simply finds nothing in those days rather than failing.
		add("username = ?", f.Username)
	}
	if f.RequireNAT {
		cond := "nat_public_ip != '' AND nat_public_ip != '0.0.0.0'"
		if f.destinationNATAvailable {
			cond = "((" + cond + ") OR (nat_dest_ip != '' AND nat_dest_ip != '0.0.0.0'))"
		}
		conds = append(conds, cond)
	}
	if !f.From.IsZero() {
		add(tsExpr+" >= ?", f.From.UTC())
	}
	if !f.To.IsZero() {
		add(tsExpr+" <= ?", f.To.UTC())
	}
	if len(conds) == 0 {
		return nil, nil, false
	}
	return conds, args, true
}

// quote returns s as a ClickHouse string literal — escaping backslashes first,
// then single quotes (order matters), so config values containing \ or ' can't
// break out of the literal.
func quote(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "'", "\\'")
	return "'" + s + "'"
}

// planColdSearch decides whether the archived tier can affect the requested
// page. No-time browsing intentionally means "latest hot data"; callers must
// provide both timestamps for S3. A capped hot result already fills every page
// the UI exposes, so older cold rows cannot affect the accessible result set.
func planColdSearch(f SearchFilter, days []string, hotTotal uint64) (bool, error) {
	if !f.From.IsZero() && !f.To.IsZero() && f.From.After(f.To) {
		return false, clientErr("From must be earlier than To")
	}
	if len(days) == 0 {
		return false, nil
	}
	if f.From.IsZero() && f.To.IsZero() {
		return false, nil
	}
	if f.From.IsZero() || f.To.IsZero() {
		return false, clientErr("Set both From and To to search the S3 archive")
	}
	if hotTotal >= countCap {
		return false, nil
	}
	return true, nil
}

// coldDayQuery reads at most limit rows from one archived day. Keeping the day
// as the unit of work is what makes a multi-year range practical: walk newest
// partitions first and stop as soon as the requested result window is full,
// instead of opening every object in one giant s3() query.
type coldDayQuery func(context.Context, string, int) ([]natRecord, error)

func walkColdDays(ctx context.Context, days []string, limit int, query coldDayQuery) (rows []natRecord, scanned int, capped bool, err error) {
	if limit <= 0 || len(days) == 0 {
		return nil, 0, false, nil
	}
	ordered := append([]string(nil), days...)
	sort.Sort(sort.Reverse(sort.StringSlice(ordered)))
	out := make([]natRecord, 0, limit)
	for _, day := range ordered {
		if err := ctx.Err(); err != nil {
			return out, scanned, false, err
		}
		remaining := limit - len(out)
		if remaining <= 0 {
			return out, scanned, true, nil
		}
		part, err := query(ctx, day, remaining)
		scanned++
		if err != nil {
			return out, scanned, false, err
		}
		out = append(out, part...)
		if len(out) >= limit {
			return out[:limit], scanned, true, nil
		}
	}
	return out, scanned, false, nil
}

// searchAll returns ONE page (limit/offset) plus a bounded total. It always
// plans against indexed hot storage first and touches S3 only when an explicit,
// bounded historical window can affect the page. "capped" means total is a
// lower bound rather than an exact count.
func (s *Server) searchAll(ctx context.Context, f SearchFilter, limit, offset int) (rows []natRecord, total uint64, cold, capped bool, err error) {
	cs := s.coldInfo()
	days := s.archivedDaysInRange(ctx, f)
	// The capped count is cheap on the MergeTree primary key (~100 ms even for
	// hundreds of millions of device rows) and lets broad browsing short-circuit
	// before any remote object is opened.
	total = s.flows.SearchCount(ctx, f)
	useCold := false
	if cs.enabled() {
		useCold, err = planColdSearch(f, days, total)
		if err != nil {
			return nil, 0, false, false, err
		}
	}
	if !useCold {
		// Hot-only: true server-side pagination — only `limit` rows leave the DB.
		rows, err = s.flows.Search(ctx, f, limit, offset)
		return rows, total, false, total >= countCap, err
	}
	// Cold overlap: merge hot + the relevant archived days (each bounded by the
	// search cap), then paginate the merged set in memory (server-side, bounded).
	hot, herr := s.flows.Search(ctx, f, searchLimit, 0)
	if herr != nil {
		return nil, 0, false, false, herr
	}
	cr, _, coldCapped, cerr := walkColdDays(ctx, days, searchLimit, func(ctx context.Context, day string, limit int) ([]natRecord, error) {
		return s.flows.SearchCold(ctx, f, limit, cs, []string{day})
	})
	if cerr != nil {
		// IPDR/reporting must fail closed: returning a plausible-looking hot-only
		// result silently omits archived evidence.
		return nil, 0, true, false, fmt.Errorf("cold search: %w", cerr)
	}
	merged := append(hot, cr...)
	sort.Slice(merged, func(i, j int) bool { return merged[i].Time > merged[j].Time })
	total = uint64(len(merged))
	capped = len(hot) == searchLimit || coldCapped
	if offset > len(merged) {
		offset = len(merged)
	}
	end := offset + limit
	if end > len(merged) {
		end = len(merged)
	}
	return merged[offset:end], total, true, capped, nil
}

// archivedDaysInRange returns archived days (YYYY-MM-DD) overlapping the query's
// time window — used to read only the needed S3 objects (date pruning).
func (s *Server) archivedDaysInRange(ctx context.Context, f SearchFilter) []string {
	ad, err := s.store.ListArchivedDays(ctx, 4000)
	if err != nil || len(ad) == 0 {
		return nil
	}
	// A day can be marked archived and still be present in hot storage: the sweep
	// marks before it drops, a drop can fail, and the manual archive endpoint
	// exports without dropping at all. Hot and cold results are concatenated, so
	// reading such a day from S3 as well would return every record twice. A
	// duplicated record in a lawful-intercept report is its own kind of wrong
	// answer, and hot is the authoritative copy while it exists.
	//
	// The lookup costs nothing because dayFlowCounts answers a fleet-wide call
	// from part metadata. It used to be a GROUP BY over every row, which is how a
	// ~2s tax ended up on every single logs search.
	inHot := map[string]bool{}
	if s.flows != nil {
		if days, derr := s.flows.dayFlowCounts(ctx, f.ISPID); derr == nil {
			for _, d := range days {
				if d.Flows > 0 {
					inHot[d.Date] = true
				}
			}
		}
	}
	return selectArchivedDays(ad, inHot, f)
}

// selectArchivedDays picks the archived days that overlap the query window and
// are not still served from hot storage. Pure so the overlap and de-duplication
// rules are testable without a database.
func selectArchivedDays(ad []store.ArchivedDay, inHot map[string]bool, f SearchFilter) []string {
	var out []string
	for _, a := range ad {
		if inHot[a.Day] {
			continue
		}
		day, e := time.ParseInLocation("2006-01-02", a.Day, istLoc)
		if e != nil {
			continue
		}
		dayEnd := day.Add(24 * time.Hour)
		// Archive partitions are half-open [day, day+24h). If From is exactly
		// midnight, the previous day does not overlap and must not consume one
		// of the bounded cold-object slots.
		if !f.From.IsZero() && !dayEnd.After(f.From) {
			continue
		}
		if !f.To.IsZero() && day.After(f.To) {
			continue
		}
		out = append(out, a.Day)
	}
	return out
}
