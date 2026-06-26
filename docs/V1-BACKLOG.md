# YesLogs Director — v1 readiness & backlog

Output of the final pre-v1 audit (multi-agent: ops-readiness · product · UI · code-risk,
with independent verification). 40 gaps found; 4 verified launch-blockers.

## v1 launch-blockers — STATUS

| # | Blocker | Status |
|---|---------|--------|
| 1 | Default `session_key` passed validation (forgeable sessions/CSRF) | ✅ **Fixed** — startup now rejects the `CHANGE-ME*` placeholder and requires ≥32 chars. |
| 2 | No real backup of the MariaDB control-plane | ✅ **Fixed** — `scripts/backup-cp.sh` (mysqldump → gz, retention, optional S3) + `natflow-backup.timer` (daily 02:30); restore runbook in the script header. |
| 3 | Console served plaintext on public `:8080` | ✅ **Artifacts shipped** — `deploy/tls/` (nginx + Caddy) + `docs/TLS.md`; app warns on insecure bind. **Operator step:** point a domain, run the proxy, set `bind:127.0.0.1` + `cookie_secure:true`. |
| 4 | No user management (one immutable login/ISP) | ✅ **Built** — Users page: add/list/delete users, admin password reset, self password change; director manages all, ISP admin manages own tenant; can't self-delete or cross-tenant. |

**v1 is launch-ready once the operator completes the TLS step (#3).**

## Post-v1 backlog (prioritized)

### Security / Ops
- **High** — Login rate-limiting / lockout + failed-login auditing (no throttle today). `M`
- **Med** — Prometheus alert rules (no-flows-N-min, queue near max, disk %, archive-fail, collector stale) + fix `healthcheck.sh` defaults. `M`
- **Med** — systemd `MemoryMax`/`CPUQuota` + ClickHouse `max_server_memory_usage` (one component can OOM the box). `S`
- **Med** — Versioned DB migrations (currently create-if-not-exists only; no ALTER path). `M`
- **Med** — ClickHouse hot-window backup (`clickhouse-backup`) to bound loss below the archive interval. `M`
- **Low** — `natlog.service` `TimeoutStopSec` (≤queue rows lost on SIGKILL); delete `admin-credentials.txt` after first login; deprecate root-running legacy collector unit; secret-rotation runbook. `S`

### Product
- **High** — Bulk / multi-IP / CSV-upload search + CIDR ranges (one combined audited report). `L`
- **High** — Operational alerting/notifications (email/webhook/Slack). `L`
- **Med** — Scheduled / delivered reports (SMTP/SFTP, cron cadence). `L`
- **Med** — Multi-collector fleet: agent token provisioning + fleet view (last-seen/version/devices). `M`
- **Med** — Per-ISP retention/S3/archive settings (currently global). `M`
- **Med** — Intra-tenant RBAC (isp_admin vs analyst vs read-only). `M`
- **Med** — Tenant-facing API + API keys for IPDR lookups (same audit). `L`
- **Med** — IPv6 support (schema + search are IPv4-only). `L`
- **Med** — Server-side session revocation (per-user token version → password change / disable invalidates live 12h sessions). `M`
- **Med** — Audit: date-range filter, pagination, CSV/PDF export (currently view-only, 100-row cap). `S`
- *Note:* report export caps at 5,000 rows and the UI labels it "capped at 5000" — acceptable for v1; raise to streamed full-export when bulk search lands.

### UI polish
- **High** — "Flows stored today" can read 0 when the exporter clock is behind (device NTP) — verify it uses the IST day boundary; surface the cause. `M`
- **Med** — Show ISP **name** (not raw id) in Devices/Audit tables; rename `ISP_ID` header. `M`
- **Med** — Logs date pickers show US `mm/dd/yyyy`; force IST `YYYY-MM-DD HH:MM`. `M`
- **Med** — Overview queue denominator (3,000,000) vs configured max queue rows (500,000) mismatch — reconcile/relabel. `S`
- **Low** — Audit PROTO casing (`any` vs `Any`); Retention "Data window" hero; neutral metric icons currently use warning colors. `S`

### Code-risk (hardening)
- **High** — Login brute-force throttle (same as ops item). `M`
- **Med** — Report-export CSRF token travels in the GET query string — prefer POST/short-lived token + `Referrer-Policy: no-referrer`. `S`
- **Low** — `parseUint32`/`parseTime` swallow errors (return 400 on bad isp/from/to); XLSX writer should strip XML-illegal control chars; surface ISP-admin creation error on ISP create. `S`

_Effort: S = hours, M = a day, L = multi-day._
