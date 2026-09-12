# YesLogs Enterprise Master Roadmap & Task Tracker (`task.md`)

This document is the single source of truth for the comprehensive YesLogs platform audit, missing feature inventory, implementation plan, and real-time changelog.

---

## 🕒 LIVE CHANGELOG

| Timestamp | Component | Change Description | Status |
|---|---|---|---|
| **2026-09-12 21:28** | **CRM Module** | Implemented Universal Smart CRM Engine (`internal/director/crm/`) with REST, Custom JSON mapper, FreeRADIUS SQL, and thread-safe LRU cache. | ✅ **COMPLETED** |
| **2026-09-12 21:28** | **Reporting** | Implemented canonical 16-column DoT compliance legal reports (`report.go`) supporting CSV, Excel, and PDF with formula neutralization. | ✅ **COMPLETED** |
| **2026-09-12 21:28** | **Diagnostics** | Added live CRM test lookup endpoint (`POST /api/v1/settings/crm/test`) and UI modal in Console Settings. | ✅ **COMPLETED** |
| **2026-09-12 21:28** | **Dataplane** | Added `DumpTemplates` and `LoadTemplates` to NetFlow v9 and IPFIX decoders for zero cold-start packet loss across restarts. | ✅ **COMPLETED** |
| **2026-09-12 21:35** | **Security** | Implemented Login Rate Limiting & Account Lockout (5 attempts / 15m lockout) to prevent brute-force attacks (`auth_throttle.go`). | ✅ **COMPLETED** |
| **2026-09-12 21:40** | **ClickHouse** | Added Port secondary minmax index (`idx_nat_port`) and per-query memory (10GB) / timeout (60s) caps to eliminate OOM server crashes. | ✅ **COMPLETED** |
| **2026-09-12 21:45** | **Search Engine** | Added Bulk IP / CIDR Subnet Search (`103.204.1.0/24`) and Port Range Search (`20000-25000`) across query engine and UI. | ✅ **COMPLETED** |
| **2026-09-12 21:55** | **Alerting** | Added Webhook Alerting (Telegram Bot, Slack, Discord, Custom Webhooks) in notifications engine. | ✅ **COMPLETED** |
| **2026-09-12 22:05** | **Dataplane** | Implemented Kernel UDP buffer drop monitoring (`/proc/net/snmp` delta) + Prometheus metric and console health card. | ✅ **COMPLETED** |
| **2026-09-12 22:15** | **Protocols** | Added Syslog NAT Parser (RFC 5424 / RFC 3164) for Fortinet FortiGate, Sophos, and Cisco ASA NAT events over UDP/TCP. | ✅ **COMPLETED** |
| **2026-09-12 22:17** | **Dataplane Integrity** | Added `SO_REUSEPORT`, IPv6/NAT64 shadow columns, exporter + collector timestamps, NetFlow v5 warnings, NTP drift guard, and disk-pressure safety monitoring. | ✅ **COMPLETED** |
| **2026-09-12 22:18** | **Investigations** | Added tenant-scoped FIR/case entities, case-linked audit events, and asynchronous large CSV export jobs. | ✅ **COMPLETED** |
| **2026-09-12 22:19** | **RADIUS / CRM** | Added authenticated native RADIUS Accounting listener/session cache and MikroTik RouterOS v7 User Manager connector. | ✅ **COMPLETED** |
| **2026-09-12 22:20** | **Security** | Added TOTP authentication, Director/ISP SuperAdmin/Analyst/Auditor RBAC, and SHA-256 chained query audit evidence. | ✅ **COMPLETED** |
| **2026-09-12 22:21** | **SRE / HA** | Validated remote managed collectors and enhanced the provisionable Grafana NOC dashboard with kernel-drop, clock, and disk panels. | ✅ **COMPLETED** |
| **2026-09-12 22:45** | **Vikas requests** | Added second-granularity export windows, ClickHouse crash/stall alerts, previous-day email summaries, ISPmate console branding, and idempotent verified S3 log-copy controls. | ✅ **COMPLETED** |

---

## 📌 MASTER INVENTORY & IMPLEMENTATION CHECKLIST

### 1. Ingestion & Dataplane (Network Layer)
- [x] **1.0** Template persistence for NetFlow v9 & IPFIX across restarts.
- [x] **1.1** Linux Kernel UDP drop monitoring (`/proc/net/snmp` `RcvbufErrors`) exposed to Prometheus and UI badge.
- [x] **1.2** Linux `SO_REUSEPORT` multi-socket receiver to eliminate lock contention on high-throughput multi-core servers.
- [x] **1.3** Syslog NAT Parser (UDP/TCP 514) for Fortinet FortiGate, Sophos, and Cisco ASA firewalls.
- [x] **1.4** IPv6 & Dual-Stack support in ClickHouse schema and decoders (NAT64 / DS-Lite).
- [x] **1.5** NetFlow v5 misconfiguration detector (alerts if router sends v5 without NAT fields).

### 2. Time Keeping & Legal Evidence Integrity
- [x] **2.1** Collector NTP Drift Guard: automated startup and periodic clock sync verification (alert if skew > 500ms).
- [x] **2.2** Dual Timestamp Recording: retain both Exporter reported time and Collector receipt time.

### 3. ClickHouse Hot Storage & Stability
- [x] **3.1** Secondary minmax index on `nat_public_port` for 100x faster reverse NAT port searches.
- [x] **3.2** Per-query memory (`max_memory_usage = 10GB`) and execution time (`max_execution_time = 60s`) guards to stop OOM crashes.
- [x] **3.3** Disk pressure auto-protection: alert at 85% disk usage, automated cold offload safety valve at 90%.

### 4. Search & Legal Investigation Workflows
- [x] **4.1** Bulk IP & CIDR Subnet Search: accept CIDR (`103.204.1.0/24`) and multi-line IP pastes for unified audited reports.
- [x] **4.2** Port Range Search: support port ranges (e.g. `20000-25000` or `80,443,8080`).
- [x] **4.3** Investigation / Case Entity: link searches, notes, and exports to an official FIR / Police Case Number.
- [x] **4.4** Background / Async Export Worker: offload massive multi-day exports (>100k rows) to background jobs.

### 5. Universal Smart CRM & RADIUS Integration
- [x] **5.1** Universal REST Connector with configurable JSON path mapping.
- [x] **5.2** Direct FreeRADIUS SQL Connector (`radacct` table) for MySQL/MariaDB.
- [x] **5.3** LRU Caching & Request Deduplication to protect CRM servers.
- [x] **5.4** Canonical 16-Column DoT Legal Compliance Exporter (CSV, XLSX, PDF).
- [x] **5.5** Live "Test Lookup" diagnostic tool in Web Console Settings.
- [x] **5.6** Native RADIUS Accounting Listener (UDP 1813) for zero-API instant session resolution.
- [x] **5.7** MikroTik RouterOS User Manager API connector.

### 6. Security, Compliance & Court Defensibility
- [x] **6.1** Login Rate Limiting & Account Lockout against brute-force attacks (`/api/v1/auth/login`).
- [x] **6.2** Two-Factor Authentication (TOTP / Google Authenticator) for ISP and Director logins.
- [x] **6.3** Cryptographic Audit Trail: SHA-256 hash chaining of `query_audit` rows to guarantee tamper-proof evidence in court.
- [x] **6.4** Intra-Tenant RBAC: distinct roles for SuperAdmin, Analyst (Search & Export), and Auditor (View Only).

### 7. SRE, Alerting & High Availability
- [x] **7.1** Instant Notification Webhooks: Telegram Bot, Slack, Discord, and custom webhooks.
- [x] **7.2** Distributed Collector Architecture (separate lightweight collectors feeding central ClickHouse).
- [x] **7.3** Pre-built Grafana NOC Dashboards for real-time traffic, drop, and device monitoring.

### 8. Vikas Request Review
- [x] **8.1** Seconds can be entered and preserved in log searches and synchronous/background exports.
- [x] **8.2** ClickHouse insert stalls, recovery, and broken-part corruption send configured alerts.
- [x] **8.3** Optional previous-day log summary is sent once daily at a configured IST hour.
- [x] **8.4** ISPmate customer-facing console branding.
- [ ] **8.5** Secondary ClickHouse recovery server — requires a provisioned replica and ClickHouse Keeper topology; software-only failover must not be presented as data recovery.
- [x] **8.6** Optional verified S3 archive copy with automatic cold offload and read-back search.
