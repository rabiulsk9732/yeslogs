# YesLogs project decisions

- The access model is Director → multiple ISP tenants. Enforce ISP isolation
  in server-side queries/actions as well as the UI. Director scope is this
  installation unless actual fleet aggregation is implemented.
- Retain the implemented console theme. For dashboard work, read
  `docs/DASHBOARD-SCOPE.md` for the implemented v1.9.0-dashboard scope, metric
  semantics and known API boundaries. Both roles have ten cards: two rows of
  five on desktop, as explicitly requested by the owner.
- Public UI releases use the static Caddy pipeline described in
  `deploy/CONSOLE-RELEASES.md`. Do not rebuild/restart natlog for HTML/CSS/JS
  changes: restarting its UDP receiver creates an ingestion gap.
- This checkout has a post-commit hook that pushes main automatically. Commit
  only the intended, reviewed changes. Do not stage unrelated user work.
- Runtime backend changes are outside static hot reload. Preserve the
  collector's ingestion continuity when planning backend deployments.

- Module index pages start with five statistics cards on desktop. Create and
  edit use modals with jQuery AJAX submission, inline client validation and
  matching server validation. Apply this as each module is refined; the locked
  Dashboard retains its explicit ten-card layout.
- ISP onboarding requires name, unique username/email, phone, password and
  confirmation, and an explicit status. Edit preserves the password if blank.

- Preserve the active module in the URL so refresh and browser Back/Forward
  restore it, subject to the signed-in role's permissions.
- Form fields use an icon at the left of the label, a red required star at the
  right edge of the label row, and a descriptive input placeholder or initial
  select prompt. Apply this rule to every form as its module is refined.
