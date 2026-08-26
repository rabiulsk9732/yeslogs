# Changelog

Releases of **natlog** — the NetFlow/IPFIX collector and IPDR console behind YesLogs.

These records answer lawful requests under DoT retention obligations, so this file
records what changed to the *evidence* as much as what changed to the code: what
was being lost, when it started, when it stopped, and how it was measured. Flow
export is fire-and-forget — nothing retransmits — so a record not stored is an
answer that can never be given.

Releases before v1.8.0 are recorded in git tags (`git tag --sort=-v:refname`).

---

## v1.8.0 — 2026-08-26 · "nothing that can answer a request may be discarded"

A day of audit found the collector was destroying the very records it exists to
hold, in five separate places, and had been for months. Everything below was
measured on the live fleet, not inferred.

### Evidence was being destroyed

**Volume-reduction rules were applied to NAT translation records.** They were
written to thin out traffic flows; applied to a translation they delete a
subscriber mapping permanently. All three were doing it:

| Rule | What it destroyed | Measured |
|---|---|---|
| `skip_zero` | 100% of separate-template NAT event records | box 4 device 3: everything |
| `skip_dns` | every translation to/from port 53 | 24.8% of all NAT events |
| `skip_private` | translations between two private addresses | normal under CGNAT |

A NAT event record carries the translation, both ports, `natEvent` and often the
subscriber's username, and **no byte counter** — it records an allocation, not
traffic. Cisco, Juniper, Nokia and DandyBNG all emit them this way, and natlog
was discarding them as empty husks. The device was then graded "no NAT fields"
and its owner told to fix an exporter that was already correct. The defect was
ours.

Any record carrying a post-NAT address **and** a port — or an address that
differs from the source, which covers 1:1 and deterministic NAT without PAT — is
now kept unconditionally. Traffic flows are still reduced.

Proof, from the hour the fix landed. Not a diurnal ramp — a step from nothing:

| Box | 08:00 | 09:00 | 10:00 | 11:00 | 12:00 |
|---|---|---|---|---|---|
| 2 | 0 | 0 | 0 | 0 | **51,537** |
| 3 | 0 | 0 | 0 | 0 | **36,407** |

**A batch ClickHouse would not accept was dropped.** Three retries over about two
seconds, then gone. This has cost real evidence twice:

- **Box 1, eight days** (2026-07-27 → 2026-08-03). An unclean shutdown left 195
  broken parts; ClickHouse refused to attach `flow_logs` at all; natlog kept
  accepting flows and discarding every batch until someone restarted it. Absent
  from hot storage and from S3 alike, for all three of that box's ISPs.
- **Box 3, three hours** (2026-08-26, ~28.6M records), from the same shape of
  fault.

Failed batches now go to a crash-safe disk spool (fsync, then rename) and replay
when ClickHouse returns. It survives restart — a spool that forgot its contents
would lose exactly what it exists to protect. A partial write is discarded on
start, because a truncated batch is worse than none. The size cap is enforced,
because filling the disk would take the collector down and cost every exporter;
reaching it counts `spool_records_lost_total`, now the only remaining path to
permanent loss. A batch refused repeatedly is quarantined, never deleted.

**The archive path was destroying and orphaning records.** `ArchiveSweep`
exported one object per *registered* ISP and then dropped the whole partition:
rows belonging to no registered tenant — `isp_id 0` from observe mode, or a
deleted ISP — were destroyed having never reached S3. It now refuses to drop
unless every row in the partition was exported. The manual archive endpoint never
wrote the archived-day marker, so its objects sat in S3 unreachable, invisible
from the moment the TTL removed the day from hot. And marking a day still present
in hot would have returned every record twice, so cold search now skips any
archived day still served from hot storage.

### Evidence that was arriving and being thrown away

`natEvent` (IE 230) and `username` (IE 371) are now decoded, stored, archived,
displayed and **searchable**. natEvent distinguishes an allocation from a release,
which is what bounds *when* a mapping was held — a request asks who held an
address **at** a time, not whether it was ever seen. The username is the
subscriber identity the BNG already knows: when present, no CRM resolution and no
inference from an address that may have been reallocated since.

Columns are added at startup with `ADD COLUMN IF NOT EXISTS` — metadata-only, no
rewrite of billions of rows — rather than a manual migration someone can forget
across four collectors. Cold reads allow missing columns so archives written
before today still open.

### One clock

Every record is now stamped with the **collector's** IST clock. The exporter's
flow time is not used at all — not preferred, not fallen back from, not consulted.

What was there before kept the exporter's time when it looked plausible and
substituted receive time when it did not, which produces the worst possible
table: two clocks mixed with nothing marking which is which. Two records a minute
apart on the wire could land days apart, and no query, report or export could
tell them apart afterwards. One exporter on this fleet runs 38.5 hours behind.

`flow_end` now equals `flow_start`, so the "Avg Session Duration" tile would read
a permanent 0; the raw dashboard shows answerable-record count instead, and the
rollup path shows packets — a figure it can state honestly.

### Compliance audit made to work

The per-device and per-day queries shared one 20-second deadline, so the cheap
retention-gap detector was lost along with the expensive scan **on every cycle**
— 35 and 36 failures in 26 hours on boxes 2 and 3. Gap detection had therefore
never run on the two busiest collectors. Split apart, it immediately found three
retained days on box 3 holding zero records (2026-07-07/08/09).

Per-day answerability is now measured one day at a time and stored
(`flow_ipdr_days`): the full-window query needed >170s and ClickHouse refused it
as too slow, but a single day costs 1.5–6.4s. The backfill is self-healing — it
compares stored counts against the free live count and recomputes any day that
grew — and yields to anything with a deadline, after an earlier version of it
starved the audit it feeds.

A day whose translation count has not been measured reports **"not measured"**,
never zero: a query that gave up is not evidence of a legal gap.

### Console

- Flows whose post-NAT address equals the source are shown instead of hidden.
  That filter was suppressing **97.6%** of one exporter's records (13.4M of
  13.8M) and hundreds of millions fleet-wide, so the console read empty while
  ingest was plainly busy.
- New **Subscriber** column and search field, driven by the exporter-reported
  username.
- Alert mail is now multipart text + HTML, signed *YesLogs Operations*. Plain
  text stays the source of truth — a mail unreadable in a terminal is no use at
  04:00 — and the HTML view says exactly the same thing.

### Operational notes

- **`/var/lib/natlog/spool` holds unwritten evidence. Do not clear it.**
- Configure `clickhouse.spool_dir` and `spool_max_gb`; without them a refused
  batch is still dropped.
- Do not re-enable a skip rule to save disk. Raise retention or archive instead.
- Never conclude "this exporter is not sending NAT" from the `nat_public_ip`
  column. Capture and decode its templates first (`nf9.py`, `nf9data.py`,
  `clock.py`). That mistake was made today and put in writing to a customer.
- New metrics: `spool_records_saved_total`, `spool_records_replayed_total`,
  `spool_records_lost_total`, `spool_files`, `spool_bytes`,
  `spool_oldest_seconds`. A growing `spool_oldest_seconds` means replay is not
  keeping up.

### Known gaps, unrecoverable

| Box | Window | Cause |
|---|---|---|
| 1 | 2026-07-27 → 2026-08-03 | broken parts blocked `flow_logs`; every batch discarded |
| 3 | 2026-07-07 → 2026-07-09 | pre-dates gap detection; cause unknown |
| 3 | 2026-08-26 04:24 → 07:32 | host-level fault; alert fired, mail failed on DNS |

### Still open

- Box 4 has no SMTP credentials, so its alerting cannot deliver.
- DandyBNG declares IE 371 but sends it empty; filling it needs subscriber-aware
  NAT logging (AAA/RADIUS binding) on the BNG. **This one is genuinely
  exporter-side.**
