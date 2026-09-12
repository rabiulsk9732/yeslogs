# Enterprise safeguards and workflows

The features tracked in `task.md` are configured through `configs/collector.yaml`
or `configs/natlog.yaml`. Apply `migrations/clickhouse.sql` before using a
standalone Director; unified `natlog` applies the additive flow columns itself.
Run `natlog --migrate` for the control-plane tables and columns.

## Dataplane listeners and safeguards

- NetFlow UDP listeners use one `SO_REUSEPORT` socket per configured worker on
  Linux. Ephemeral test ports retain a single socket.
- Set `receiver.ports.syslog_udp` and/or `syslog_tcp` to `514` (or an
  unprivileged forwarded port) for Fortinet/Sophos key-value and Cisco ASA NAT
  messages. TCP framing is newline-delimited RFC 6587.
- Set `receiver.ports.radius_accounting: 1813` and a strong `radius.secret`.
  Authenticated Start/Interim/Stop records maintain the in-process subscriber
  cache used to populate an otherwise-empty flow username.
- `monitoring` controls the NTP source, 500 ms policy, ClickHouse filesystem,
  and 85/90 percent disk thresholds. At the safety threshold unified `natlog`
  starts a guarded archive sweep; it never drops a partition unless the
  existing archive verification proves every hot row was exported.
- Prometheus exposes `kernel_udp_receive_errors_total`,
  `kernel_udp_rcvbuf_errors`, `collector_ntp_offset_seconds`,
  `collector_ntp_healthy`, `clickhouse_disk_used_percent`,
  `clickhouse_disk_pressure_level`, and
  `netflow5_misconfiguration_packets_total`.

IPv6 endpoints are stored in additive `*_v6` columns, keeping the installed
IPv4 schema upgrade metadata-only. Archives emit the effective IPv4/IPv6 text
address. `exporter_flow_start`, `exporter_flow_end`, and `collector_received`
retain both untrusted wire time and the trusted collector receipt timeline.

## Security and investigations API

All mutation calls require the normal `X-CSRF-Token` header.

- `POST /api/v1/account/totp/setup` returns a Base32 secret and `otpauth://`
  URI. Confirm with `POST /api/v1/account/totp/confirm` and `{"code":"123456"}`.
  Login then accepts `totp` in addition to email/password. Disabling TOTP needs
  the current password and code at `/api/v1/account/totp/disable`. Confirmed and
  pending TOTP secrets are AES-GCM encrypted at rest with the installation key.
- Tenant roles are `isp` (SuperAdmin), `analyst` (search/export), and `auditor`
  (view/search without export or configuration writes). `director` remains the
  installation-wide role. Enforcement is server-side for JSON and legacy
  routes.
- `GET/POST /api/v1/cases` and `GET/PUT /api/v1/cases/{id}` manage tenant-scoped
  FIR/police cases. Search accepts `caseId`; reports accept `case_id`. Audit rows
  store the official reference.
- `POST /api/v1/exports` accepts `{"filter": <SearchFilter>, "caseId": 1,
  "schema":"dot16"}` and returns `202`. Poll `/api/v1/exports/{id}` and download
  the completed CSV at `/api/v1/exports/{id}/download`. Jobs page through hot
  storage in the background rather than enforcing the interactive 100k limit;
  each job is bounded to five million rows and 30 minutes.

The CRM settings screen supports the YesLogs v1 batch REST contract and
MikroTik RouterOS v7 User Manager over HTTPS. RouterOS uses the configured API
username and the encrypted connector credential as its Basic-auth password.
The reusable `internal/director/crm` package also contains the custom-JSON REST
mapper and direct FreeRADIUS SQL connector.

Each new `query_audit` event stores `prev_hash` and `row_hash`, a canonical
SHA-256 chain over the previous link and complete event payload. Existing audit
history remains readable; the first post-upgrade record anchors a new chain.

## Remote collectors and NOC dashboard

The existing Director agent API and managed collector mode provide the
distributed topology: collectors pull tenant/device policy and write to the
central ClickHouse endpoint. Provision `deploy/grafana/natflow-dashboard.json`
for ingest, writer, device, kernel UDP loss, NTP, and disk-pressure panels.
