# Control plane (Director)

The **Director** is the multi-tenant control plane (the software owner / Sayra
plane). It onboards **ISPs** (tenants), each ISP manages its **devices**
(exporters) in its own panel, and the dataplane collectors **pull** their device
registry + policy from the Director and apply it live. Flow dashboards are
**ISP-scoped**: the Director sees all ISPs, each ISP sees only its own.

```
 Director (cmd/director)  ──MySQL──  ISPs / users / devices / agents
   │  ▲                                       
   │  │ GET /api/v1/agent/config (agent token)
   │  │                                        ┌─ ClickHouse (flows, isp_id-stamped)
   ▼  │                                        │
 collector (managed mode) ── pulls registry ──┘  reads for dashboards
   └─ receives NetFlow/IPFIX, stamps isp_id/device_id from the pulled registry
```

## Roles

- **director** — software owner. Creates ISPs, sees/manages everything.
- **isp** — tenant user, pinned to one `isp_id`. Manages only its devices, sees
  only its flows. Cross-tenant access is rejected (HTTP 403 / 404).

## Storage

Control-plane metadata lives in **MariaDB/MySQL** (`mysql_dsn`). Flow data stays
in **ClickHouse** (written by the dataplane, read by the Director for dashboards).

## Bootstrap

```bash
go build -o bin/director ./cmd/director
director --config configs/director.yaml --migrate
director --config configs/director.yaml --create-admin --email admin@you.com --password '...'
director --config configs/director.yaml --create-agent --name dp-india-01   # prints token ONCE
director --config configs/director.yaml                                     # run
```

Open the bind address, sign in as the admin, **create an ISP** (optionally with
its first ISP login), then add the ISP's **devices** (exporter IP → isp_id /
device_id / vendor profile / skip rules).

## Managed dataplane (full DP control from CP)

Point a collector at the Director in `collector.yaml`:

```yaml
director:
  url: "http://director-host:8080"
  token: "<agent token from --create-agent>"
  poll_interval_s: 30
```

In managed mode the collector pulls the device registry + exporter policy from
the Director every `poll_interval_s` (and on `SIGHUP`), applying it through the
same atomic hot-reload used for local config — no restart, no dropped ingest.
`unknown_exporter_mode` is forced to **reject**, so only Director-registered
exporters are accepted. Local `devices:`/`security:` in the YAML are ignored
while managed; the rest of `collector.yaml` (ClickHouse, ports, writers) still
applies locally.

## Security notes

- Bind the Director to localhost/VPN or front it with TLS (`cookie_secure: true`).
- Sessions are signed (HMAC-SHA256) cookies, `HttpOnly` + `SameSite=Lax`.
- Agent tokens are shown once; only their hash is stored. One token = the full
  device registry across tenants (a collector serves many ISPs' exporters), so
  treat agent tokens as infrastructure secrets.
- Passwords are bcrypt-hashed.
