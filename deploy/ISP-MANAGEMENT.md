# ISP management without a collector restart

`yeslogs-management` uses the existing `cmd/director` binary on loopback port
8081. Its gateway owns `/api/v1/isps` and its child routes, login, logout and
session lookup. Other requests are forwarded to the existing natlog HTTP port
8080. It opens no UDP receivers, runs no archive/rollup jobs and does not supply
replacement runtime metrics. Existing session keys and the MariaDB database
are shared; the gateway rechecks user and tenant state before forwarding
session-authenticated traffic. Disabled/deleted accounts lose public access
without waiting for their cookies to expire. Agent-token config requests retain
the original endpoint and authentication. Caddy continues serving static UI.

The additive migration creates `isp_profiles` (username, phone, primary user ID,
version). Existing isps/users/devices schemas and data remain compatible with
the running collector. Legacy tenants display their oldest ISP login as primary
until a profile is saved. Their passwords are preserved when editing without a
new password. New usernames and contact phone numbers are not guessed or seeded.

ISP status controls tenant user access. Exporter enablement remains on Devices;
disabling an ISP does not stop the UDP receiver or discard stored flow records.
Deleting requires a disabled tenant, an exact-name confirmation, current version
and no devices/capture policies. It removes the tenant profile and its ISP user
accounts in one transaction, retaining flow records, archives and audit history.
Deleted numeric ISP IDs are not recycled. Control-plane mutations are serialized
by the gateway, including forwarded device/user/policy changes.

## Install an exact reviewed backend revision

1. Back up the control-plane database to a root-readable file. Build the reviewed
   committed `./cmd/director` with Go 1.25 and install it as
   `/usr/local/bin/yeslogs-management`. Record its full `vcs.revision`.
2. Create `/etc/natlog/management.yaml` using the example and the existing
   session key/DSN (mode 0600). Keep `upstream` on 127.0.0.1:8080. Do not copy the
   ClickHouse connection: the existing process serves all flow operations.
3. Install `management-guard.nft` at `/etc/natlog/management-guard.nft` and
   `systemd/yeslogs-management-guard.service`. The independent nftables table
   rejects only non-loopback TCP port 8080 so sessions cannot bypass the gateway.
   It preserves UDP collection, SSH, Caddy and other host firewall rules. Validate
   with `nft --check --file /etc/natlog/management-guard.nft` and enable the guard.
   Install `systemd/yeslogs-management.service`, start/enable it and verify
   `/healthz`, `/api/v1/management-version`, unauthenticated 401s and an existing
   Director session on port 8081. Migration only adds a table.
4. Change the fallback proxy in `/etc/caddy/console-static.caddy` to
   `127.0.0.1:8081`, validate and reload Caddy. Do not restart natlog.
5. Install the reviewed console fetcher script. Keep `backend_revision` at the
   original collector commit. Add `management_revision` equal to the installed
   management binary's full commit and `management_probe_url` equal to
   `http://127.0.0.1:8081/api/v1/management-version`. The fetcher verifies both
   source compatibility and that the management process reports this revision.
   Only cmd/director and internal/director may use this additional baseline;
   collector code and shared dependencies still match the original baseline.
6. Let successful CI promote the static console. Verify actual public assets,
   ISP directory and modal forms, and unchanged natlog PID/start time.

For later backend upgrades, build/start a candidate management process on a
second loopback port, verify it, then reload the Caddy upstream and change the
management probe/revision together. This avoids interrupting the collector.
Changes to proxied runtime handlers need a separate collector deployment; merely
building a newer Director does not activate those handlers in natlog.

## Rollback

Retain the previous Caddy fragment, fetcher config/script and UI revision. To
roll back this initial split, pause the UI fetch timer, restore its previous
config/script, restore Caddy's fallback to 8080 with validate/reload, and use the
fetcher's documented rollback to the preceding UI. Existing tables remain
compatible; retain `isp_profiles` rather than deleting saved profile data.
Stop the management service only after Caddy no longer routes to it. The old
backend supports email login but does not expose username/edit/delete features.
Do not restore the old database backup over subsequent user changes.

The backend port guard intentionally remains enabled on rollback because Caddy
also reaches the original backend over loopback. If direct remote HTTP access
is explicitly needed later, remove only `table inet yeslogs_management` and
disable its dedicated service; do not flush the host ruleset.
