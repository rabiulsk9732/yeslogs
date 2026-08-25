# PHP reference API for YesLogs CRM enrichment

This is a framework-free PHP 8.1+ implementation of
`yeslogs.crm.lookup.v1`. It is intended as handoff code for the ISP/CRM vendor,
not as code that runs inside the YesLogs server.

The endpoint accepts up to 1,000 YesLogs lookups, bulk-loads them into a
connection-local MySQL temporary table, performs one indexed `radacct` join,
joins the resolved username to a normalized subscriber view, and returns one
result per `referenceCode`.

## Files

- `public/session-lookups.php` — complete HTTP endpoint.
- `config.example.php` — environment-driven configuration template.
- `schema-reference.sql` — customer-view and index examples to adapt.
- `sample-request.json` — request for a curl/contract test.
- `tests/contract-test.php` — dependency-free contract/result unit test.

## Requirements

- PHP 8.1 or newer.
- `pdo_mysql` extension.
- MySQL/MariaDB access to `radacct` and the CRM subscriber data.
- HTTPS on the public endpoint.

## Installation

1. Copy this directory to the CRM application/server.
2. Keep only `public/` under the web root.
3. Copy `config.example.php` to `config.php`, outside `public/`.
4. Inject these values through PHP-FPM/container/systemd secrets:

```text
YESLOGS_API_KEY=<strong random bearer key>
CRM_DB_DSN=mysql:host=127.0.0.1;port=3306;dbname=radius;charset=utf8mb4
CRM_DB_USER=yeslogs_reader
CRM_DB_PASSWORD=<database password>
CRM_RADACCT_TIMEZONE=Asia/Kolkata
CRM_RADACCT_TABLE=radacct
CRM_SUBSCRIBER_VIEW=yeslogs_subscribers
CRM_NAS_MATCH_MODE=when_present
```

Use `CRM_RADACCT_TIMEZONE=UTC` if the `radacct` DATETIME columns store UTC.
Timezone correctness is mandatory; a wrong setting can identify the wrong
subscriber.

5. Adapt and review `schema-reference.sql`. The endpoint expects the standard
FreeRADIUS columns:

```text
radacctid, acctsessionid, username, nasipaddress, framedipaddress,
acctstarttime, acctstoptime
```

It expects `yeslogs_subscribers` to expose:

```text
username, account_id, name, address, phone
```

The CRM vendor can implement that name as a view over any internal customer
schema. This keeps customer-specific table details out of the API code.

6. Point HTTPS routing at `public/session-lookups.php`. The final URL may be:

```text
https://crm.example.in/api/yeslogs/v1/session-lookups
```

YesLogs accepts any final HTTPS URL; redirects are deliberately rejected.

## Contract test

```bash
curl --fail-with-body \
  -X POST 'https://crm.example.in/api/yeslogs/v1/session-lookups' \
  -H 'Authorization: Bearer REPLACE_WITH_API_KEY' \
  -H 'Content-Type: application/json' \
  -H 'X-YesLogs-Schema: yeslogs.crm.lookup.v1' \
  -H 'Idempotency-Key: YLREQ-3C15D73E92F2A80F8C7B4D11' \
  --data-binary @sample-request.json
```

Expected successful shape:

```json
{
  "schemaVersion": "yeslogs.crm.lookup.v1",
  "requestId": "YLREQ-3C15D73E92F2A80F8C7B4D11",
  "results": [
    {
      "referenceCode": "YL-20260717-2A3C5E7910BF4D827639A1C0",
      "status": "matched",
      "subscriber": {
        "accountId": "CUST-90031",
        "username": "radius-user-31",
        "name": "Customer Name",
        "address": "Customer Address",
        "phone": "9999999999"
      },
      "session": {
        "acctSessionId": "RAD-SESSION-123"
      }
    }
  ]
}
```

The other valid statuses are `not_found`, `ambiguous`, and `error`. The
reference endpoint returns `ambiguous` instead of guessing when overlapping or
stale-open RADIUS sessions match the same IP and timestamp.

Run the bundled unit test before deployment:

```bash
php tests/contract-test.php
```

## Matching performed by the example

The standard session condition is:

```sql
radacct.framedipaddress = localIp
AND radacct.acctstarttime <= eventTime
AND (
  radacct.acctstoptime IS NULL
  OR radacct.acctstoptime >= eventTime
)
```

`CRM_NAS_MATCH_MODE` controls `nasipaddress`:

- `required`: NAS IP must match.
- `when_present`: match it when YesLogs supplied it; recommended starting mode.
- `ignore`: use IP and timestamp only.

If the CRM maps `nasIdentifier` or `deviceId` through another table, the vendor
should adapt the clearly marked SQL section in `findMatches()`.

## Production checklist

- Keep the endpoint behind HTTPS.
- Use a long, random per-ISP Bearer key and rotate it periodically.
- Allowlist the YesLogs server IP at the firewall/reverse proxy when possible.
- Rate-limit the endpoint while allowing four concurrent requests.
- Give the DB user only `SELECT` and `CREATE TEMPORARY TABLES`.
- Confirm the recommended `radacct` lookup index with `EXPLAIN`.
- Keep `yeslogs_subscribers.username` unique.
- Do not log request bodies, local IPs, usernames, addresses, phone numbers, or
  authorization headers.
- Return `ambiguous`; never guess between overlapping sessions.

The authoritative wire contract is `../../docs/CRM-INTEGRATION.md`.
