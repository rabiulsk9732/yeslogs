# Serving YesLogs Director over HTTPS (required before launch)

The console + API carry credentials, session cookies, and subscriber/IPDR query
results. **Never expose them in plaintext to real customers.** Put a TLS-terminating
reverse proxy in front and keep the app on loopback.

## Steps
1. **Point a domain** at this server (A record → public IP).
2. **Bind the app to loopback + secure cookies** — in `/etc/natlog/natlog.yaml`:
   ```yaml
   cp:
     bind: "127.0.0.1:8080"   # proxy reaches it locally
     cookie_secure: true       # cookies only sent over HTTPS
   ```
   `systemctl restart natlog`
3. **Run a TLS proxy** (pick one):
   - **Caddy (auto-HTTPS):** edit `deploy/tls/Caddyfile` (set your domain) → `caddy run --config deploy/tls/Caddyfile`
   - **nginx + certbot:** `certbot --nginx -d director.example.com`, then use `deploy/tls/nginx-yeslogs-director.conf`.
4. **Firewall:** allow 443 (and 80 for the ACME/redirect); the app's 8080 should NOT be public anymore. NetFlow/IPFIX UDP (2055/9995/4739) stay open to your exporters.

`natlog` logs a warning at startup whenever it binds a non-loopback address with
`cookie_secure:false` — that warning must be clear before going live.
