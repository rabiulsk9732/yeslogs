# Capture Policies — v1.12.0-policies

The index has five desktop cards: available policies, global presets, ISP-specific
presets, presets with no skip rules, and presets with one or more skip rules. The
scope filter affects both the cards and the list; text/rule filters affect the list.
Refresh preserves filters and pagination. The module uses `#/capture-policies`.

Create/edit/details/typed-delete use square modals, jQuery AJAX, inline validation,
label icons, right-aligned required stars and field prompts. All three rule choices
are explicit Keep/Skip selects; creation defaults to Keep. No skip rules means only
that this preset skips none of those categories: other collector filters still apply.

## Presets and saved device configuration

The old page incorrectly claimed that editing a preset updates every linked device.
Policy CRUD now has a real edit endpoint, but changes only the preset. Exporter
records retain their saved skip flags and enabled status. To apply a changed preset,
select it and save a device on Devices. Details show visible linked registrations
and whether their saved rules match the preset. No bulk application is performed.

Policy scope is immutable. Unused policies can be renamed. References are checked
on the server inside the store transaction: linked policies cannot be renamed or
deleted until their devices are reassigned to another preset or custom rules. Both
enabled and disabled registrations count. Stored flow logs are unaffected by CRUD.
References are matched conservatively by policy name and eligible tenant scope.

Director manages global and ISP presets. ISP users can read global presets and
manage their own presets. Usage counts and device names on global presets are
limited to the signed-in ISP. Details show at most 50 linked devices, with a Devices
shortcut scoped by ISP and policy search. Names shared between separate ISPs are
allowed; name collisions involving a global preset or the same ISP are rejected.
Legacy overlapping names must be resolved before editing those presets.

## API and storage

`GET /api/v1/policies` preserves the existing policy fields and adds version tokens,
usage counts, edit/delete permissions, `editablePolicies: true`, and Director-only
ISP choices. `GET /api/v1/policies/{id}` returns one visible policy and limited device
details. POST creates; PUT edits; DELETE requires the exact name and current version.

Name (1–64 characters without control characters), scope and all three boolean
rules are validated server-side. CSRF and tenant ownership apply to every write.
Update/delete compare a content version derived from persisted policy fields.
The store locks the policy namespace, checks the expected snapshot and references,
and commits each mutation in one transaction. Concurrent edits from the same
snapshot cannot overwrite each other. A no-op save is accepted.

No new database column or table is needed. Existing collector-side device creation,
policy resolution and config reload contracts remain in place. Public control-plane
writes are serialized by the management gateway. Direct out-of-band writes through
SQL or the private legacy backend are outside that gateway serialization contract.

Failed reads retain a clearly labelled stale list. Failed writes preserve input;
network/5xx outcomes require closing and refreshing before retry. Expired sessions
return to login. Older APIs without the edit capability cannot enable the new form.

## Verification and delivery

Go API tests cover scope, CSRF, validation, reference protection, unchanged device
rules and gateway routing. Store tests cover real MariaDB snapshot comparisons,
concurrent edits and no-op writes in an isolated `yeslogs_policy_test_` schema.
Browser tests in `scripts/check-policies.cjs` cover both roles, layout, CRUD, errors,
read-only global presets, routing and mobile dialogs. Existing Devices/ISP/Dashboard
browser suites remain part of CI.

The standalone management gateway owns policy routes in this release. Deploy a
verified candidate on a second loopback port and switch Caddy only after probes
pass. Then record its revision and probe URL in the UI fetcher configuration.
The collector baseline, UDP receiver, agent configuration source and flow API
process stay unchanged. See `deploy/ISP-MANAGEMENT.md` for the service procedure.
