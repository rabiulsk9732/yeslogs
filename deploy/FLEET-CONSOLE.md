# Console delivery on legacy fleet boxes

Use the static console and standalone management gateway for console releases.
`scripts/deploy-fleet.sh` installs and restarts the collector; it is not the UI
rollout path. The primary already uses `deploy/CONSOLE-RELEASES.md` and
`deploy/ISP-MANAGEMENT.md`. The bootstrap below is for a host which still exposes
the embedded console on TCP 8080 and has no existing web proxy.

The layout is Caddy on TCP 80 → management on loopback 8083 → the existing
collector on loopback 8080. An isolated nftables table redirects new incoming
connections to the old `http://host:8080` URL to Caddy. Loopback management traffic,
UDP collection, SSH and forwarded/container traffic retain their existing paths.
HTTP and the existing session key/cookie policy are preserved; this does not
provision a domain or TLS certificate.

## Prepare, verify, activate

1. Audit every configured target in `/etc/natlog/fleet.conf`: running binary
   revision/hash, PID/start time, HTTP ports, firewall, existing proxy and service
   files. Do not reuse the primary's database credentials or backend baseline.
2. Record `systemctl show natlog -p MainPID -p ExecMainStartTimestamp` in
   `/var/backups/yeslogs/console-fleet-v1.13/collector-before.txt` (protected
   directory), and back up its natlog YAML. Install Python with PyYAML, Git and
   Node 18+ without automatically restarting existing services. Recheck the PID.
3. Assemble a payload with the reviewed clean Director executable, a verified
   Linux Caddy executable, `console-release.py`, `bootstrap-fleet-console.py`,
   `console-static.caddy`, `console-route.nft`, the three `yeslogs-fleet-*.service`
   units and the existing UI fetch service/timer. `manifest.json` has `revision`
   (the Director's exact commit) and `files` (basename → full SHA-256). Verify the
   build stamp before writing the manifest. Transfer through the existing root
   SSH connection; no application credentials or private Git keys are copied.
4. Run the bootstrap's `prepare --payload PATH --host PUBLIC_IPV4` phase. It
   backs up the control-plane DB, fingerprints existing records, checks source
   compatibility against the actual collector, builds the CI-approved UI from
   Git, and starts Caddy and the gateway. It reads this host's session key and DSN
   locally, and creates the additive ISP profile table through normal migration.
   Existing ISP records, users, devices, policies and settings are preserved.
5. Through an SSH tunnel to loopback TCP 80, verify both available roles, all
   module pages, account versions, settings snapshots and modal validation.
   `scripts/check-fleet-console.cjs` accepts `FLEET_ORIGIN`, `EXPECTED_REVISION`
   and a protected `FLEET_SESSION_FILE` with short-lived cookies for existing
   accounts. It blocks all API mutations. Never print or commit session files.
6. Run `activate`. It permits TCP 80 if UFW is active, enables the isolated
   redirect/guard and services, then enables the 30-second CI-approved fetcher.
   Verify externally through the original TCP 8080 URL and the new TCP 80 URL.
   Compare collector PID/start, binary hash, control-plane fingerprints and
   receiver counters with the recorded baseline. Zero packet loss is not inferred
   from these checks; counters do not measure loss before the receiver.

The bootstrap deliberately refuses an existing proxy/deployment or repeat
preparation. Inspect the protected state before recovering a partial installation;
do not overwrite backups or rerun it blindly. Normal subsequent UI releases use
Git main → CI → console-live → automatic fetch. Backend changes remain blocked
until their corresponding component is separately deployed and verified.

Boxes 2–4 retain collector revision `46fafc626c353a168c778da028dd95ee67ed7c1d`
while the primary retains `83b0e5b1a78093dafb83f635e2405241a4c98463`. These
collectors expose the existing API contracts, but their read-path improvements
and performance differ. This console rollout does not claim that their collector
binaries were upgraded. The gateway supplies the current ISP, account and policy
APIs and protects runtime settings writes; runtime/flow APIs stay on each collector.

## Rollback

Pause `yeslogs-ui-fetch.timer` and its service. For a complete bootstrap rollback,
stop `yeslogs-fleet-console-route.service` to remove only its own nftables table;
new connections to TCP 8080 again reach the original collector. Then stop/disable
Caddy and `yeslogs-fleet-management.service` after their requests drain. Remove
only the TCP 80 UFW rule added by this rollout if it is no longer needed. Retain
the additive ISP profile table and never restore the DB backup over subsequent
operator changes. The old embedded UI lacks the newer management workflows.

For a later UI-only rollback, retain the gateway and use the normal static
release rollback instead. Keep the timer paused until the promoted Git revision
matches the intended release.
