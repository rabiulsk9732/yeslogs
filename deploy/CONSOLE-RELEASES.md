# Console releases without collector restarts

The public console is served by Caddy from `/srv/yeslogs-ui/current`.
The existing `natlog` process continues to serve APIs and receive UDP packets.
Direct access to port 8080 still shows the binary's embedded UI; use the public
domain to see static console releases.

## Delivery

1. Commit on `main`. The optional local `post-commit` hook pushes that commit to
   GitHub. It never stages or commits unfinished files. If pushing fails, the
   commit stays local and the hook prints the retry command.
2. GitHub Actions validates asset references and JavaScript, tests release
   safeguards and rollback, and opens the built release in Chromium. Only a
   passing current `main` commit is promoted to the `console-live` branch.
3. `yeslogs-ui-fetch.timer` checks that branch every 30 seconds using a separate
   bare repository. Failed/pending CI never advances that branch.
4. The installed fetcher validates the committed bundle again, checks backend
   compatibility, and atomically swaps the `current` symlink. Failed origin/API
   probes restore the previous symlink. It never invokes systemctl or executes
   scripts fetched from Git.

HTML is not cached. Assets use commit-specific URLs with immutable caching;
older releases stay available for tabs opened before an update. Refreshing a
page picks up the new release; active forms are not forcibly reloaded.

## Install on the primary origin

Prerequisites: Caddy, Python 3.9+, Git, Node.js 18+, a working root GitHub SSH key,
and GitHub Actions enabled with permission for the promotion job to write
`console-live`. Do not point this setup at another host without setting its
own backend revision and testing its origin route.

```sh
install -d /usr/local/lib/yeslogs-ui /var/lib/yeslogs-ui /srv/yeslogs-ui/releases
install -m 0755 scripts/console-release.py /usr/local/lib/yeslogs-ui/console-release.py
install -m 0644 deploy/console-release.json.example /etc/natlog/console-release.json
# Set backend_revision to the installed binary's vcs.revision from:
go version -m /usr/local/bin/natlog
# Edit /etc/natlog/console-release.json with that revision and the site's host.

# Bootstrap an already reviewed, backend-compatible committed release:
revision=$(git rev-parse HEAD)
python3 scripts/console-release.py build --revision "$revision" --output "/srv/yeslogs-ui/releases/$revision"
ln -s "releases/$revision" /srv/yeslogs-ui/current

install -m 0644 deploy/tls/console-static.caddy /etc/caddy/console-static.caddy
# Add `import /etc/caddy/console-static.caddy` above the site block.
# Inside the block replace the reverse_proxy with `import yeslogs_console_static`.
# Keep the site's existing hostname, TLS mode, encoding and other settings.
caddy validate --config /etc/caddy/Caddyfile
caddy reload --config /etc/caddy/Caddyfile

install -m 0644 deploy/systemd/yeslogs-ui-fetch.service deploy/systemd/yeslogs-ui-fetch.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now yeslogs-ui-fetch.timer
```

For automatic push after local commits, install `scripts/hooks/post-commit` as
`.git/hooks/post-commit` (mode 0755) in this checkout. If a hook already exists,
integrate it instead of overwriting it. Feature branches are not auto-pushed.
The hook is local configuration; other checkouts need their own installation.

## Status and rollback

```sh
systemctl status yeslogs-ui-fetch.timer
journalctl -u yeslogs-ui-fetch.service -n 30
curl -H 'Host: logs.sayratechnologies.in' http://127.0.0.1/ui-version.json
readlink /srv/yeslogs-ui/previous

# Pause delivery before manually selecting a retained release:
systemctl stop yeslogs-ui-fetch.timer
systemctl stop yeslogs-ui-fetch.service
python3 /usr/local/lib/yeslogs-ui/console-release.py rollback FULL_COMMIT_SHA
# Resume after reverting/fixing main and letting CI approve the corrected commit:
systemctl start yeslogs-ui-fetch.timer
```

The fetcher does not automatically update itself, Caddy configuration, or the
service units. Install reviewed operational changes explicitly.

## Backend boundary and packet loss

Go code is compiled into `natlog`; changing it is not a static UI release.
The fetcher compares `cmd/`, `internal/` (excluding the console), `configs/`,
`go.mod` and `go.sum` with the configured backend revision and blocks incompatible
releases. After a separately planned backend deployment, update that baseline
to the revision actually installed. Never advance it just to bypass the check.

The current unified process closes UDP sockets before draining accepted writer
queues on shutdown. The disk spool protects accepted batches during database
failures; it cannot recover UDP packets sent while no receiver is listening.
No loss count can be inferred just from a successful restart. UI delivery now
avoids this restart gap. Backend upgrades need a separate ingestion continuity
design (for example, a durable ingress layer); this pipeline does not promise
zero packet loss for backend restarts or network failures.

## Separately deployed ISP management

The ISP module now uses the standalone Director gateway documented in
[ISP-MANAGEMENT.md](ISP-MANAGEMENT.md). The fetcher can verify an independently
installed `management_revision` for `cmd/director` and `internal/director`, with
an exact-revision loopback process probe. The collector `backend_revision`
remains unchanged. This is an additional verified component baseline, not an
automatic backend upgrade or permission to skip collector compatibility checks.
