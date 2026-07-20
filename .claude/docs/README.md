# .claude/docs — project memory for AI agents (and humans)

Session-to-session context for anyone (human or agent) working on this repo.
Read these before making structural changes; read root `CLAUDE.md` for hard
constraints and `PLAN.md` for the original research + migration plan.

| File | What's in it |
|---|---|
| `01-project-origin.md` | The two parent repos and exactly what was taken from each |
| `02-decisions.md` | Every key decision with rationale (raw-SQL deletion, auth, trust model, naming, ...) |
| `03-implementation-log.md` | Chronological record of the 2026-07-20 initial build |
| `04-operations.md` | Build/test commands, release pipeline, git auth on the dev machine, runtime layout |
| `05-saved-queries-migration.md` | How to migrate electron-mssql-app queries + migrations already performed |

Update these when reality changes — especially 02 (new decisions) and 05
(new customer migrations).
