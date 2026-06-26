# NATLog — Production Plan (IPDR Compliance Platform)

**Purpose.** A multi-tenant **IPDR / CGNAT lawful-logging platform** for ISPs. The
software owner (Director / Sayra) onboards ISP tenants; each ISP's CGNAT/BNG
routers export NAT translation logs (NetFlow v9 / IPFIX with post-NAT fields);
the platform stores them with retention and lets an analyst **reverse-resolve a
public IP+port+time back to the subscriber** and **download a court-ready report**
— with a full audit trail. (India DoT/CERT-In: 180-day retention, in-country,
on-demand to LEA.)

**What sells it:** the **UI/UX** and the **fetchers** (ingestion + lawful query +
report). Customers don't inspect the dataplane internals — they judge the console
and whether a lookup → report just works.

## Architecture (single service)
- One binary **`natlog`** = dataplane (UDP collector) + control plane (console/API).
- Stores: **ClickHouse** (flow/IPDR records), **MariaDB** (tenants/users/devices/audit).
- Deploy: systemd (`natlog`, `clickhouse-server`, `mariadb`); installer in `scripts/install.sh`.
- In-process device registry: UI device add → dataplane applies in ~15s (no restart).

## Modules

| # | Module | State | Remaining for launch |
|---|--------|-------|----------------------|
| 1 | Platform & deploy (systemd, installer, config) | ✅ | smoke-tested on boot |
| 2 | Auth & RBAC (director/isp, bcrypt, signed sessions, CSRF) | ✅ | session-expiry UX |
| 3 | Tenancy — ISP onboarding (CRUD, enable/disable, ISP-admin login) | ✅ | — |
| 4 | Devices/exporters (per-ISP, in-process push) | ✅ | last-seen / flow count column |
| 5 | Ingestion dataplane (v5/v9/IPFIX, postNAT) | ✅ | — |
| 6 | **IPDR Lawful Search** (public IP+port+time → subscriber, audited) ⭐ | ✅ | bulk (multi-IP) — P1 |
| 7 | **Reports** (CSV / PDF / Excel, case header) | ✅ | report history — P1 |
| 8 | **Audit** (every lookup recorded) | ✅ | filters/export — P1 |
| 9 | **Retention & Archive** (TTL + S3 cold archive) | ⚠️ backend only | **surface in UI (P0)** |
| 10 | Overview/Dashboard (compact ops KPIs + health) | ⚠️ | **compact rebuild (P0)** |
| 11 | **UI/UX system** (compact, vendored offline assets, consistent) | ⚠️ | **vendor assets + polish (P0)** |
| 12 | Observability (Prometheus, health) | ✅ | surface health in Overview |
| 13 | Testing — Go race ✅ ; **Playwright E2E** | ⚠️ | **E2E suite (P0)** |
| 14 | Security (tenant isolation, CSRF, no-CDN, TLS) | ✅ core | **review new endpoints (P0)** + TLS doc |

## Launch checklist (P0, tonight)
1. **Vendor assets** — jQuery + Font Awesome + fonts embedded (no CDN; IPDR boxes are often air-gapped).
2. **UI/UX polish** — compact dense layout, consistent components, empty/loading/error states, responsive.
3. **Retention & Archive page** — retention policy, archived-days list, run archive, storage usage (wire `internal/archive`).
4. **Overview** — compact KPIs (today's flows, retention window, storage, recent queries, collector health).
5. **Playwright E2E** — login, IP search → resolve → download CSV/PDF/XLSX, ISP/device CRUD, tenant isolation (ISP user can't see others), CSRF enforced.
6. **Adversarial security review** — search/report/audit endpoints (SQLi, tenant scope, IDOR, injection in report headers), fix findings.
7. **Deploy + verify** — systemd restart, public reachability, E2E green, go race green.
8. **Commit + tag** `v0.6.0-ipdr-console`.

## Delivered after v0.6.0 (settings/system/device-edit)
- **Settings** (Director, DB-backed + live-applied): Dataplane tuning (batch/flush/workers/queue/backpressure/unknown-mode), global Skip Rules, Retention (days → ClickHouse TTL), S3 Archive (endpoint/bucket/keys, secret-masked). Saved to MySQL `settings` table; applied to the running dataplane immediately via an applier callback (no restart).
- **Device edit** — full per-device modal (name, exporter IP, device_id, protocol, profile, skip rules, enable) via `PUT /api/v1/devices/{id}` (tenant-scoped).
- **System** (Director) — host CPU load / memory / disk + natlog process (goroutines, heap, uptime) + ingest (flows today) from `/proc` + runtime.
- E2E: 14/14 Playwright green.

## P1 (next)
Bulk multi-IP search (Excel upload), report history + SFTP delivery, user management UI, scheduled retention/archive jobs, per-ISP retention policy, TLS reverse-proxy guide.

## Verification
- `go test ./... -race` (all packages green)
- `npx playwright test` (E2E suite green against the live `natlog` service)
- Manual: open console, run a lawful lookup, download all 3 report formats, confirm audit entry.
