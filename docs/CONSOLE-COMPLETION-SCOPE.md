# Combined console release — v1.13.0-console

This release completes refinement of all remaining sidebar modules under the
existing Director → ISP model, preserving the locked Dashboard, ISP, Devices and
Capture Policies modules. Delivery is one reviewed release, not a per-page rollout.

| Page | Work |
|---|---|
| Users | Five statistics, scoped directory, create/edit/detail/delete modals, password confirmation/change/reset, server validation and protected primary accounts |
| Logs | Five search-result statistics, consistent fields, IPv4/port/time validation, clear/reset, AJAX errors and stale-response protection |
| Reports | Five recent-export statistics, filters, details, scoped search handoff; no claim of exact replay from incomplete audit metadata |
| Audit | Five recent-activity statistics, search/type/date filters, pagination, details and export of the visible audit data |
| Retention | Five storage statistics, honest tenant byte labels, archive history, day filtering and explicit archive confirmation |
| Compliance | Five technical-readiness statistics, filters/details, unknown-state handling; no blanket assertion of regulatory compliance |
| Dataplanes | Five collector statistics, refresh, collector details and truthful runtime/resource availability |
| Settings | Five configuration statistics, section summaries and edit modals, inline validation, fresh saved values and preserved secrets |

All module indexes start with five cards on desktop. The Dashboard retains ten.
Forms use label icons, right red required stars, descriptive prompts and square
controls. Create/edit actions use modals and jQuery AJAX. URL navigation survives
reload and browser history, and pending responses cannot replace another module.

Runtime settings, search, export, archive and telemetry remain on the existing
collector. User management is upgraded in the separate management gateway using
a verified alternate-port deployment. Production verification does not change
accounts, device rules, policies, runtime settings or archived data.

Settings writes carry a gateway-issued snapshot version. The gateway serializes
configuration mutations, checks current runtime and persisted values (including
credential rotations), validates fields, and forwards valid changes to the
collector. Old Settings tabs must refresh before saving. Concurrent CRM changes
can require reopening the connector because credentials share one stored section.
Accounts expose password changes, never password recovery or stored credentials.

The release refines the application's existing feature set. Separate backlog
features such as a new fleet architecture, intra-ISP roles, bulk lookup or scheduled
report delivery are not represented as implemented by these UI changes.
