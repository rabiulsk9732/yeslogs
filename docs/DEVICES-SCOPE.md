# Devices — v1.11.0-devices

The Devices index uses the existing Director → ISP access model and AdminLTE2-style
console theme. Both roles receive five cards on desktop: total registrations,
enabled registrations, online, silent and no recent data. Director can scope the
cards and list to one ISP. ISP sessions receive only their own devices from the
server; the UI also filters that response by the signed-in ISP.

Search, status and protocol filters affect the list; the ISP filter affects both
cards and list. Refresh preserves filters, page size and the current page where
possible. The page URL is `#/devices`, including refresh and browser history.

## Registration and changes

Create, edit, details, enable/disable confirmation and typed-name deletion use
square modals. Forms use jQuery AJAX, CSRF headers, inline validation, label icons,
right-aligned required stars and input/select prompts. Exporter addresses accept
IPv4 and IPv6 literals; addresses are normalized before saving. Duplicate addresses
are checked against the visible list and a fresh list before submission. Server
validation and uniqueness constraints remain authoritative across tenants.

Creation uses the existing API's enabled-by-default behavior and automatic device
ID allocation. Edit sends an explicit `Enabled` boolean: previously omitting it
silently disabled an enabled device. ISP and device identity are displayed read-only;
this release does not move devices between tenants or rewrite historical identities.

Named policies are restricted to the chosen ISP and global presets. Their skip
rules are previewed before saving. Custom rule selections survive switching to a
policy and back. Missing policies must be explicitly replaced. Duplicate policy
names in the effective scope are blocked because the API resolves policies by name.
Policy availability and rule values are checked again before create/edit submission.
The Details modal shows the device's saved rules, which can differ from an updated
policy until the next device save.

Writes disable controls and prevent duplicate submission or dismissal while pending.
A fresh device read checks for changes since opening the modal. Failed submissions
retain input and display the server error. Ambiguous network/server write failures
require closing and refreshing before retrying. Deletion retains stored logs;
subsequent unregistered traffic follows the collector's configured policy. Device
details link to Logs with both ISP and device ID selected.

## Health and API boundaries

Health comes from `/api/v1/devices`: stored evidence over a three-day lookback plus
collector signals, using the configured silence threshold. It is not a raw packet
counter. Disabled registrations are excluded from online/silent/no-data counts.
Missing or unrecognized health is unavailable, never silently treated as zero or
no data. Incomplete health totals display a dash with known and unavailable counts.
Timestamps are labelled IST. Refresh is manual and shows the last successful time;
a failed refresh leaves a visible stale-data message.

This static release does not change backend contracts. The existing Devices API
has no atomic version token, so the fresh-read check cannot exclude a concurrent
write between that read and the mutation. Device ID allocation and policy-name
resolution are also existing server contracts. Friendly-name validation is enforced
by the UI; backend validation covers IP, identity, protocol/profile and uniqueness.
The collector health signal map is keyed by device ID rather than ISP+device ID;
its existing cross-tenant identity-collision limitation is not changed here.

Delivery uses the verified static console pipeline. No collector or management
service restart is required. Browser checks in `scripts/check-devices.cjs` exercise
both roles with isolated data; production verification uses read-only device APIs
and opens modals without saving changes to real exporters.
