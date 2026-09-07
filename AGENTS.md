# YesLogs project decisions

- The access model is Director → multiple ISP tenants. Enforce ISP isolation
  in server-side queries/actions as well as the UI. Director scope is this
  installation unless actual fleet aggregation is implemented.
- Retain the implemented console theme. For dashboard work, read
  `docs/DASHBOARD-SCOPE.md` for the selected v1 component scope, metric semantics
  and known gaps. The document is an implementation target, not evidence that
  all components already exist.
- Public UI releases use the static Caddy pipeline described in
  `deploy/CONSOLE-RELEASES.md`. Do not rebuild/restart natlog for HTML/CSS/JS
  changes: restarting its UDP receiver creates an ingestion gap.
- This checkout has a post-commit hook that pushes main automatically. Commit
  only the intended, reviewed changes. Do not stage unrelated user work.
- Runtime backend changes are outside static hot reload. Preserve the
  collector's ingestion continuity when planning backend deployments.
