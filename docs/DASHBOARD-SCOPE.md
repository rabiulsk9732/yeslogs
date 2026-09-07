# Dashboard v1 — implementation baseline

Decided for the dashboard planning task on 2026-09-07. This document locks the
component scope for the next implementation; it does not claim that the new
dashboard or its required API data already exists.

## Theme and access model

- Retain the implemented YesLogs theme: navy sidebar, blue accents, light page
  background, white panels, compact tables, Source Sans 3 text and monospace
  addresses/numbers. Reuse existing tokens and components.
- Two access levels: Director and ISP. Director covers all tenants registered
  with this installation. Independent servers are not a federated fleet unless
  a real aggregation source has been implemented.
- ISP views and APIs must use the authenticated tenant scope. An ISP never
  chooses another tenant or sees global host capacity, other ISPs, credentials,
  infrastructure administration or cross-tenant audit details.
- The dashboard answers: Are records arriving? Are they being saved? Which
  devices need attention? What historical evidence is available to search?

## Layout, top to bottom

1. Header: Dashboard, explicit tenant scope, reporting timezone, updated time,
   refresh and pause/resume. Default chart window is the last 24 hours; the
   "today" cards explicitly use the installation's reporting timezone.
2. Six summary cards per role, defined below.
3. Stored-record trend (two-thirds width) and current collection/storage health
   (one-third). Chart buckets have real timestamps and units.
4. Prioritized "Needs attention" panel and device/tenant status table.
5. A compact recent-record preview with a link into the full Logs page.
6. Search Logs and Generate Report shortcuts. Director also gets Add ISP and
   Add Device; ISP gets its authorized device-management action.

## Director summary cards

| Card | Meaning |
| --- | --- |
| ISPs | Registered total, with enabled/disabled breakdown |
| Exporters receiving | Exporters with recent observed input / enabled exporters; show stale and unknown separately |
| Records stored today | Persisted flow-record rows for today, across the installation |
| Collector health | Observed collector/writer status, queue pressure and last successful write; unavailable is unknown |
| Storage capacity | Actual hot-store disk usage and available disk capacity, explicitly labelled |
| Devices needing attention | Distinct affected devices, with critical/warning breakdown and drill-down reasons |

The operational table has ISP, enabled/receiving exporters, records today,
latest receive time and attention status. Selecting an ISP opens its scoped
devices/logs. A selected ISP scope applies consistently to every tenant metric;
host health and disk capacity remain explicitly labelled installation-wide.

## ISP summary cards

| Card | Meaning |
| --- | --- |
| Exporters receiving | This ISP's recently observed exporters / enabled exporters |
| Records stored today | This ISP's persisted record rows for today |
| Last data received | Latest observed receive timestamp for this ISP, with freshness state |
| Searchable coverage | Observed covered days in the retention window; hot/archive availability distinguished |
| NAT data quality | Devices with usable NAT address/port fields / graded enabled devices; unknown states separate |
| Devices needing attention | This ISP's distinct affected devices and highest-severity reasons |

The operational table has device name, exporter IP, protocol, enabled state,
last receive time, collection status, NAT data quality and Logs action. It has
no other ISP names or global host resource metrics.

## Shared component behavior

- Trend: stored records per hour for the last 24 hours, using timestamped
  buckets. Call it "Records stored", not live ingestion throughput: stored
  rows can include backfilled data and are not a packets-per-second signal.
- Director health panel: collector, persistence, queue/backlog, disk and
  archive job status, based on actual observations.
- ISP health panel: its exporter freshness, record availability, NAT field
  quality and coverage gaps. Archive status must be tenant-scoped and backed
  by actual inventory; never infer successful archival from configuration.
- Needs attention: silent exporters, input with no successful writes, unusable
  NAT fields, and observed coverage gaps. Director additionally sees verified
  queue, disk and archive failures. Show reason, last observation and a useful
  next action. Count a device once even when it has multiple findings.
- NAT field quality is a technical data-quality signal, not a legal compliance
  certification. Packet-loss percentages require suitable measured counters;
  gaps and silence alone do not establish the number of lost packets.
- Recent records: latest 10 stored records, event timestamp with timezone,
  device, private/public IP:port, protocol and destination; Director also sees
  ISP. Label the time semantics and link to the full Logs page. Full search,
  pagination and export configuration remain on their existing pages.
- Refresh lightweight summaries/recent records every 15 seconds while the
  dashboard is visible. Refresh charts/expensive coverage summaries at most
  every 60 seconds or from a bounded cache. Pause when hidden, prevent
  overlapping requests, preserve user focus and allow a manual refresh.
- Every component supports loading, empty, stale and error states. Keep the
  last successful data visible with its timestamp if a refresh fails. Unknown
  must not become zero, healthy or "no flows".
- Desktop: six compact summary cards with responsive wrapping; two-column
  panels. Small screens: stacked panels and horizontally scrollable tables.

## Current implementation gaps to resolve

- `overview.go` counts enabled devices as "Active Exporters"; configuration
  state is not proof of live collection.
- `pgOverview` hardcodes an "ALL OK" health badge and shares that health panel
  between roles.
- Current cards mix process-lifetime counters and today's records; each metric
  needs explicit time semantics. Do not compare them as a loss/delivery ratio.
- The hourly chart currently receives bare values without timestamp labels.
- The 15-second tail refresh updates recent rows but not all overview cards or
  charts; the redesigned refresh must update the corresponding components.
- Device freshness based on stored event times is not the same as receive-time
  telemetry. New metrics must identify their actual source and limitations.
- Some summary queries discard errors. Propagate partial/unavailable states
  rather than rendering successful-looking zeroes.

## Implementation and release boundary

Implement Director first, then the ISP view using the same component contracts
and explicit tenant tests. Reuse trustworthy existing endpoints where their
scope and time semantics fit. Add required telemetry/API work explicitly; do
not manufacture unavailable metrics to complete a visual layout.

Static UI changes use the GitHub CI → console-live → 30-second fetcher pipeline
documented in [CONSOLE-RELEASES.md](../deploy/CONSOLE-RELEASES.md). Backend API and
collector changes need a separate deployment plan that addresses ingestion
continuity. Do not restart `natlog` merely to deliver dashboard HTML/CSS/JS.
