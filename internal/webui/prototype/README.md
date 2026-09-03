# cP:Restic interface prototype

> Question: Which information architecture should replace the current WHM
> pages? Three variants are switchable with `?variant=` and use the same
> realistic data across every core page.

This is deliberately throwaway, read-only prototype code. It is not wired to
the service and must not be included in a production plugin package.

Run it from the repository root:

```bash
python3 -m http.server 4173 --directory internal/webui/prototype
```

Then open:

- `http://127.0.0.1:4173/?variant=A&page=overview`
- `http://127.0.0.1:4173/?variant=B&page=overview`
- `http://127.0.0.1:4173/?variant=C&page=overview`

Use the bottom switcher or the left/right arrow keys to compare directions.
Arrow keys are ignored while a form control is focused. Use the page
navigation inside each direction to review Overview, Accounts, Destinations,
Schedules, Restore, History, and Settings.

The Restore tab starts by asking whether the operator needs one account or
multiple accounts. These states can also be opened directly:

- `?variant=A&page=restore` — choose a restore type
- `?variant=A&page=restore&restore=single&account=northwind` — choose a date
  and any combination of Panel Config, Home Dir, Cron Jobs, Databases,
  Database Users, Domains, Certificates, Email, and FTP
- `?variant=A&page=restore&restore=multiple&scope=selected` — select accounts
  and a snapshot for each one
- `?variant=A&page=restore&restore=multiple&scope=all` — include every account
  with a successful snapshot and disclose accounts that cannot be restored

Change `variant=A` to `B` or `C` to compare the same restore flow in another
direction. All restore and review actions are inert.

## Directions

- **A — Operational rail:** persistent navigation, compact health summary,
  exceptions and remediation first.
- **B — Guided workspace:** narrative health summary, progressive tasks, and
  more explanation for an occasional operator.
- **C — Recovery console:** dense master/detail surfaces, keyboard-friendly
  command structure, and high information density for experienced operators.

## Decision

Winner: **A — Operational rail**, selected by the user on 2026-09-03.

Keep:

- Persistent left navigation with an obvious active page.
- Compact, operations-first hierarchy that surfaces health and exceptions.
- Restore flow with a visible plan summary beside the configuration form.
- Progressive restore choice: one account or multiple accounts.

Discard:

- B's wider guided narrative as the default shell.
- C's dense dark recovery-console shell as the default experience.

Next step: rewrite direction A in the production Go templates, preserving the
existing authorization and cPanel/WHM integration boundaries. Do not promote
the prototype JavaScript directly. Delete this directory after the production
implementation is complete and verified.
