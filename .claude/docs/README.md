# .claude/docs — project memory for AI agents (and humans)

Session-to-session context: **why** the code is the way it is. This is memory, not
product documentation.

Looking for how the app works, how to configure it, or how to run it? That is
[`../../docs/`](../../docs/README.md) — start at its index. Hard constraints are in
the root `CLAUDE.md`.

| File | What's in it |
|---|---|
| `01-project-origin.md` | The two parent repos and exactly what was taken from each |
| `02-decisions.md` | Every key decision with rationale (raw-SQL deletion, auth, trust model, naming, the compat layer, wire compatibility, ...) |
| `03-implementation-log.md` | Chronological record: the 2026-07-20 initial build, the 2026-08-05 production cutover, and the DRY/KISS refactor |
| `04-operations.md` | The dev machine, the release pipeline, git auth, and **the live production box's exact settings and rollback artefacts** |
| `05-saved-queries-migration.md` | How to migrate electron-mssql-app queries + migrations already performed |

`../../PLAN.md` is the original migration plan, kept for provenance. It is
**historical** — where it disagrees with `docs/`, `docs/` is right.

Update these when reality changes — especially `02` (new decisions), `04` (anything
about a production machine) and `05` (new customer migrations). A decision recorded
with its reasoning saves the next person from re-litigating it; one recorded without
the reasoning is nearly useless.
