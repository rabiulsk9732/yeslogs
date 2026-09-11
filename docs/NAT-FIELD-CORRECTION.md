# NAT field correction

Existing NetFlow v9 and IPFIX tests verified post-NAT source fields (225/227).
They did not cover post-NAT destination fields (226/228). This correction keeps
the verified source decoding and adds preservation of the destination fields.
Raw source/destination fields retain their original direction.

The API and reports now distinguish source, destination, both-side, unchanged
and incomplete translation data. `nat_ip` / `nat_port` identify a recorded
translation on one side: post-source for source NAT, pre-destination for
destination NAT. Both-side records expose both raw tuples instead of selecting
one endpoint arbitrarily. Raw post-source and post-destination fields remain
available in details and exports. An unchanged source with an unavailable
post-destination is incomplete evidence; it cannot establish no translation.
CRM uses the recorded subscriber side for a confirmed source/destination
translation, and skips incomplete or ambiguous records.

Device selectors carry the exporter IP as well as the configured Device ID.
Device names resolve by tenant and exporter, preventing two registrations with
the same Device ID from overwriting each other's label. Legacy device-only
links explicitly select all exporters for that ID.

## Validation

- Existing and added NetFlow v9/IPFIX tests cover source NAT, destination NAT,
  port changes, unchanged tuples and zero ports.
- Normalizer, writer, spool, archive and capture-policy checks preserve the new
  destination fields and retain legacy spool compatibility.
- A bounded passive production capture was decoded offline by the updated
  decoder and a separate template-aware byte walker: 2,000 packets, 30,175
  decoded records, zero mismatches across all eight address/port fields.
  Ten data sets preceding the first captured template were skipped equally.
  No traffic was replayed into collection. Capture files stay local.
- Director tests cover mapping classification, exact exporter scope, report
  fields, historical defaults, and destination-side CRM lookup.
- Browser checks cover separate fields, destination NAT, incomplete legacy
  data, port-only and both-side translation, duplicate Device IDs, and both
  Director/ISP roles.

## Deployment boundary

The independent management gateway can serve corrected search/report reads
with `flow_reads: true`, using the shared session key, control database and
ClickHouse reader. Other runtime operations continue through the existing
collector. See `deploy/ISP-MANAGEMENT.md`.

The additive collector schema uses `nat_dest_ip` / `nat_dest_port`, defaulting
to an unavailable address and zero for older rows. The reader tolerates both
pre-migration tables and old archive files. Defaults do not recover destination
fields that were never stored.

Decoder and writer activation requires the new collector binary. The current
receiver owns its UDP sockets directly, without socket activation or template
handoff. A plain restart can miss arriving datagrams and starts with an empty
template cache. Prepare and review the collector cutover separately; do not
advance its deployed revision merely to publish a UI release.
