# Dashboard — v1.9.0-dashboard release contract

This replaces the initial six-card proposal. The owner requested **ten cards,
two rows of five**, and delegated the remaining component choices. The scope
below is implemented in the static console for both Director and ISP roles.

## Theme and layout

Retain the existing navy sidebar, blue accents, colored summary tiles, white
panels, compact tables, Source Sans 3 and monospace addresses/numbers. At desktop
widths above 1100px the dashboard has exactly two rows of five equal-width
cards. At 1100px and below, use two columns and stacked content panels; tables
scroll within their panels. No horizontal overflow of the document.

The header identifies the role/scope, reporting timezone (Asia/Kolkata / IST),
last successful response, Refresh and Pause/Resume. Shortcuts open Logs,
Reports and Add Device; Director also has Add ISP. The sidebar identifies ISP
sessions as ISP CONSOLE. The toolbar findings button opens Needs attention.

## Ten Director cards, in display order

| Row | Cards | Semantics |
| --- | --- | --- |
| 1 | Registered ISPs; Enabled exporters; Online exporters; Silent exporters; No recent evidence | This installation only. Configuration, observed liveness and missing measurements are distinct. |
| 2 | Records stored today; Flows decoded; Flows skipped; Hot data size; Queue pressure | Today in IST; decoded/skipped since process start; compressed flow-table size; current queue occupancy. |

## Ten ISP cards, in display order

| Row | Cards | Semantics |
| --- | --- | --- |
| 1 | Registered exporters; Enabled exporters; Online exporters; Silent exporters; No recent evidence | Only the authenticated ISP. Disabled devices are excluded from liveness counts. |
| 2 | Records stored today; Records in window; Subscriber IPs seen; Logged traffic volume; Public NAT IPs seen | Today or the explicitly labelled window starting yesterday 00:00 IST. Subscriber IPs are approximate distinct IPs, not an account count. Traffic volume is flow bytes, not disk usage. |

Ten cards remain visible during loading/failure. Missing measurements are
reported as unavailable/unknown, not zero. Failed refreshes retain previous
values with a Stale label. Large counts can be compacted visually; their full
value remains in the accessible label and tooltip. Cards link to useful pages.

## Components below the cards

1. **Records stored by hour:** 24 labelled hourly buckets for **today**, the
   semantics the current endpoint actually supports. Total, scale and accessible
   per-hour values. This is not a rolling-24-hour or live-packets chart.
2. **Collector resources / Collection health:** Director sees process uptime,
   queue pressure, actual disk free/total, memory and CPU load. ISP sees only its
   enabled/online exporters, latest evidence timestamp and unmeasured devices.
3. **Needs attention:** deduplicated device findings for silence, no evidence,
   unavailable observations and NAT quality. Also observed coverage gaps;
   Director additionally sees measured disk/queue pressure. Findings link to
   Logs, quality review or the relevant operational page. No hardcoded ALL OK.
4. **NAT data quality & coverage:** fully populated NAT fields, incomplete ports,
   devices needing review, unknown grades, audit timestamp, observed dates and
   reported missing dates. This is technical evidence quality, not a legal
   certification or a claim that archives have been verified.
5. **ISP collection status / Exporter status:** searchable, filterable,
   ten-row pagination. Director gets per-ISP account state, enabled/total,
   online, observation/quality findings and last evidence. ISP gets its own
   devices, IPs, protocol, observations and NAT grades. Search Logs preselects
   the corresponding ISP/device without running an unaudited automatic search.
6. **Recent stored records:** latest ten returned records with timestamp/IST,
   device, private/public IP:port, protocol and destination. Director also sees
   ISP when the device can be attributed unambiguously. The full Logs page
   retains search and export functions. Missing NAT addresses and unchanged
   translations are labelled.

## Refresh and failure behavior

- Overview and device inventory/health: every 15 seconds while visible.
- Record/chart snapshots and Director host metrics: at most every 60 seconds
  automatically. Manual Refresh requests new responses.
- Expensive NAT/coverage audit: separately loaded, at most every five minutes
  automatically; manual retry is available after failure. The API can return
  cached audits, so the audit's own generated timestamp is shown.
- No overlapping summary refreshes. Network requests have bounded timeouts;
  navigating away or losing authentication aborts/discards dashboard requests.
- Pause/hidden tabs stop scheduled polling. Resume and visibility changes
  restore polling. Manual Refresh works while paused. Table filters, page and
  focus are retained across refreshes.
- Independent components load as their responses arrive. Failures show a
  partial/stale banner and per-source timestamps; last-known values are kept.
- Empty and failed responses have different presentations. An API response
  timestamp is not presented as the time a record was persisted.

## Scope and existing API boundaries

- Director means all tenants registered to this installation, not aggregation
  of independent servers. The summary stays installation-wide; table search
  does not silently change its scope.
- ISP dashboard requests only existing tenant-scoped endpoints. It never
  requests `/system` or `/isps`, renders no global host metrics, and additionally
  filters inventory to the signed-in tenant. Server-side scope remains the
  authorization boundary; hiding UI elements alone is not authorization.
- Liveness follows the existing configured silence threshold and three-day
  evidence lookback. Evidence can include stored flow timestamps and observed
  dropped-flow signals; this is not a raw UDP receive-time or packet-loss meter.
- Current summary APIs can suppress individual database-query errors, and
  rollups can lag. The frontend handles transport/shape/availability failures
  but cannot detect every query error hidden by a successful API response.
- Current recent-record payloads omit ISP IDs. A repeated DeviceID across
  tenants is not attributed by guessing; the ISP column says Not reported.
- Last successful storage-write telemetry, verified archive inventory and
  real rolling timestamped buckets require backend contracts. They are not
  fabricated in this release. Existing quality observations and resources
  provide the supported v1 panels.

## Validation and release

`scripts/check-dashboard.cjs` exercises both roles against synthetic APIs,
5-by-2 desktop layout, responsive layout, role-scoped requests, enabled vs
online states, missing health, stale/error/empty states, refresh/pause/hidden
tabs, preserved filters, duplicate DeviceID attribution and Logs drill-down.
GitHub Actions runs it against the exact versioned release bundle.

The Director dashboard is additionally checked against live APIs in a browser.
UI delivery follows [CONSOLE-RELEASES.md](../deploy/CONSOLE-RELEASES.md): Git push,
CI, promotion to console-live and atomic deployment by the fetcher. The release
tag is applied only after CI and live verification. The collector must retain
its existing PID/start time; no natlog rebuild/restart is part of this release.
