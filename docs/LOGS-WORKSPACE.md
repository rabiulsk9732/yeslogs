# Logs workspace — v1.14

Filters live in a responsive modal. The main page keeps five existing statistics
cards, applied-filter chips and a full-width result table. Opening Logs performs
no search; the modal initially shows the last 15 minutes in IST. Quick presets
include 15 minutes, one hour, 24 hours and all hot logs. Explicit paired dates
retain the existing hot/S3 search semantics. Advanced filters hold private IP,
destination, exporter subscriber, protocol and the audit reason.

Cancel discards draft edits without changing the applied search, statistics,
export URLs or result pages. Clear filters clears only the draft. Submitting
valid filters applies them, closes the modal and starts the audited request.
Dashboard and historical report links open the modal with their existing scoped
filters; historical IP metadata still requires review rather than guessing which
IP field it belonged to.

The table distinguishes initial, loading, no matches, cancelled, failed and
populated results. Errors never become “zero records”. Failed results clear
success counters; malformed responses are identified as rendering failures.
Requests are aborted/discarded after cancellation, replacement, logout or
navigation. Record details include all available CRM information and distinguish
exporter identity, CRM resolution, missing NAT and unchanged addresses.

The browser keeps at most eight result pages for 60 seconds within one applied
search. Previously loaded pages are labelled with their load time. Refresh and
new searches bypass/clear this cache; navigating away or losing authentication
discards it. There is no persistent browser storage or shared cross-user cache.
Row HTML is cached with its response, and columns use fixed widths to reduce
repeat layout work. Tables scroll internally on small screens; modal footer
buttons remain visible. Export filters always describe the applied search.

## Validation

`node scripts/check-logs.cjs [release-directory]` checks both roles, populated
NAT/CRM rows, escaped text, no records, validation, draft cancel, quick ranges,
page cache and refresh, exports, detail dialogs, 200-row pages, cancellation,
malformed/server/network failures, recovery and mobile layouts. It also reports
cached 200-row page paint timings over 20 switches. Set `LOGS_SHOTS_DIR` to save
screenshots. Fixture browser checks are not authenticated production API tests.

Release CI also checks the other console modules and dashboard drill-downs.
Static delivery follows `deploy/CONSOLE-RELEASES.md`; collector code, query
semantics and ClickHouse settings are unchanged by this UI release.

## Measured search performance — 11 September 2026

Read-only tests ran on the existing installation with approximately **7.57
billion hot rows / 84 GiB compressed**, while ingestion continued. These are
small, point-in-time database measurements, not end-to-end response guarantees
or proof that the database engine became faster in this release. Search scopes
and resource limits are recorded in the attached JSON files.

| Database workload | Concurrency | Samples | Median | p95 | Outcome |
| --- | ---: | ---: | ---: | ---: | --- |
| Hot exact IP + port, 15 min | 1 | 8 | 108 ms | 117 ms | Returned rows |
| Hot exact IP + port, 15 min | 8 | 8 | 271 ms | 341 ms | Returned rows |
| Hot device, 15 min | 1 | 8 | 261 ms | 327 ms | 50 rows |
| Hot device, 15 min | 8 | 8 | 707 ms | 819 ms | 50 rows |
| Hot device, 24 hours | 1 | 8 | 945 ms | 1,661 ms | 50 rows |
| Hot device, 24 hours | 8 | 8 | 8,782 ms | 8,982 ms | 50 rows |
| Hot device, page 20 | 8 | 8 | 488 ms | 641 ms | 50 rows |
| S3 3 September, device / 15 min | 1 | 4 | 1,158 ms | 1,492 ms | 5,000-row result window |
| S3 3 September, exact endpoint / 15 min | 1 | 4 | 769 ms | 837 ms | 5,000-row result window; positive public port |
| S3 1 July, device / 15 min | 1 | 4 | 4,228 ms | 4,886 ms | 5,000-row result window |
| S3 1 July, exact endpoint / 15 min | 1 | 4 | 4,075 ms | 6,214 ms | 13 rows |
| S3 3 September, full device day | 1 | 1 | — | — | Hit the benchmark's 2 GiB memory limit after 9.28 s |

Hot results include the capped deduplicated count and a 50-row page. S3 tests
read up to 5,000 deduplicated rows, matching the current archive merge window;
they omit hot/cold planning and merging. All numbers exclude session handling,
CRM enrichment, WAN time and browser work. No OS or S3 cache was flushed. The
benchmark limits each query to two threads; hot probes used 512 MiB / 12 s and
archive probes used up to 2 GiB / 30 s. A resource-limit failure is not a
successful fast search and does not establish the production query's limit.

A separate Chromium fixture run switched cached 200-row pages 20 times per role:
Director median/p95 **214/425 ms**, ISP **174/316 ms**. These are browser-only
paint measurements, not live search response times.

Near-instant results are realistic for narrow indexed searches and cached
pages. They are **not guaranteed for 24-hour concurrency bursts, multi-day
archive scans or slow CRM responses**. For further backend work, measure query
planning/count, rows, archive reads and CRM separately through the authenticated
API. Full-day archive sorting needs a bounded execution design or different
archive indexing before an “instant under extreme load” claim is justified.

Raw reports:

- `benchmarks/2026-09-11-hot.json`
- `benchmarks/2026-09-11-s3-recent-window.json`
- `benchmarks/2026-09-11-s3-july-window.json`
- `benchmarks/2026-09-11-s3-full-day-limit.json`
- `benchmarks/2026-09-11-s3-exact-positive-port.json`
- `benchmarks/2026-09-11-browser-pages.json`

Reproduce with `scripts/benchmark-search.py`. Its default is a bounded local
read-only hot workload. `--cold` reads persisted archive settings through the
local MariaDB socket without printing credentials. Choose an ISP/device that
actually existed on the archive day. `--archive-day` and
`--archive-sample-window` can choose an actual historical record window.
The script stops increasing concurrency for a case after a query fails.
