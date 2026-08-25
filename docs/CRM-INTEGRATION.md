# YesLogs CRM / RADIUS enrichment contract

Contract version: `yeslogs.crm.lookup.v1`

YesLogs calls one independently configured HTTPS endpoint per ISP. The CRM owns
all RADIUS-session and subscriber-database matching logic; YesLogs only sends
the source identity available on an IPDR row and joins the returned subscriber
fields to that row.

## HTTP request

```http
POST /api/yeslogs/v1/session-lookups
Authorization: Bearer <ISP-issued API key>
Content-Type: application/json
Accept: application/json
X-YesLogs-Schema: yeslogs.crm.lookup.v1
Idempotency-Key: YLREQ-<random request id>
```

```json
{
  "schemaVersion": "yeslogs.crm.lookup.v1",
  "requestId": "YLREQ-3C15D73E92F2A80F8C7B4D11",
  "lookups": [
    {
      "referenceCode": "YL-20260717-2A3C5E7910BF4D827639A1C0",
      "localIp": "172.16.18.146",
      "eventTime": "2026-07-17T08:16:19Z",
      "deviceId": 4,
      "nasIdentifier": "wadhai-mikrotik-nas",
      "nasIpAddress": "198.51.100.10"
    }
  ]
}
```

Required lookup fields:

- `referenceCode`: stable YesLogs correlation identifier. The CRM must return
  this exact value with the result.
- `localIp`: the private source IP from the NAT/IPDR record.
- `eventTime`: the flow timestamp in UTC RFC3339 format.
- `deviceId`: YesLogs' numeric exporter-device identifier.

`nasIdentifier` and `nasIpAddress` are supplied when present in the YesLogs
device registry. The CRM may use either value to disambiguate overlapping
address pools.

A request contains at most 1,000 lookups. YesLogs deduplicates records with the
same ISP, device, local IP and event second before calling the endpoint.

## HTTP response

Return HTTP `200` with one result per lookup:

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

Allowed result statuses:

- `matched`: exactly one subscriber session was resolved.
- `not_found`: no session covers the supplied IP/NAS/event time.
- `ambiguous`: more than one session could match; the CRM must not guess.
- `error`: this individual lookup could not be evaluated.

For non-matched statuses, `subscriber` and `session` may be empty objects. Result
order is irrelevant; YesLogs joins exclusively by `referenceCode`.

The response `requestId`, when supplied, must equal the request `requestId`.
Unknown or duplicate reference codes invalidate that batch.

## Operational behavior

- Timeout is configurable per ISP (default 5 seconds per batch).
- Up to four export batches may run concurrently.
- Redirects are refused, so the configured URL must be the final endpoint.
- Transport or contract failures never remove IPDR rows. YesLogs marks CRM
  enrichment `unavailable` and still returns/exports the original flow data.
- Successful matches are cached in process for 15 minutes; `not_found` and
  `ambiguous` results are cached for 2 minutes.
- Subscriber PII is joined in memory for the authorized search/export. It is not
  written into ClickHouse flow logs or the S3 archive.
- The Bearer key is encrypted before it is persisted and is never returned by
  the settings API.

## CRM implementation responsibility

The CRM is responsible for locating the session that owns `localIp` at
`eventTime`, using the NAS identity when required, and joining that session to
its subscriber/customer master. YesLogs does not depend on the CRM's database
schema or table names.

## PHP reference implementation

A framework-free PHP 8.1+ endpoint, environment template, schema/index example,
sample request, and contract test are included in
[`examples/crm-php`](../examples/crm-php/README.md). The CRM vendor should adapt
only its subscriber view, database connection, `radacct` timezone, and optional
NAS matching rule; the HTTP contract above must remain unchanged.
