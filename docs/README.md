# Documentation index

## Start here by task

| I need to… | Read |
|---|---|
| understand how the whole thing works | [architecture.md](architecture.md) |
| call the API from the backend | [api.md](api.md), then [saved-queries.md](saved-queries.md) |
| know what erp-manager actually sends and expects | [erp-manager-integration.md](erp-manager-integration.md) |
| register or change a SQL query | [saved-queries.md](saved-queries.md) |
| install, upgrade or roll back a customer machine | [operations.md](operations.md) |
| configure a machine (every key explained) | [configuration.md](configuration.md) |
| work out why something is broken | [troubleshooting.md](troubleshooting.md) |
| change the code | [development.md](development.md) + [../CLAUDE.md](../CLAUDE.md) |
| migrate the backend off the old login/password auth | [erp-manager-migration-plan.md](erp-manager-migration-plan.md) |
| know what the backend needs back before a release can ship | [connector-adaptation-plan.md](connector-adaptation-plan.md) |
| run the database and the ERP folder on different servers | [deployment-topologies.md](deployment-topologies.md) |
| understand price/stock per ERP | [price-and-stock.md](price-and-stock.md) |
| send orders into Hasavshevet | [hasavshevet-send-order.md](hasavshevet-send-order.md) |
| review the security posture | [security.md](security.md) |
| lock down the SQL login the connector uses | [../scripts/sql/](../scripts/sql/) + [security.md](security.md) |
| set up service auto-start | [autostart.md](autostart.md) |

## Every document

**Reference**

- [api.md](api.md) — routes, request/response bodies, the complete error-code table
- [erp-manager-integration.md](erp-manager-integration.md) — the backend contract: the six calls erp-manager makes, the credentials it sends, the response shapes it depends on, and what fails silently
- [configuration.md](configuration.md) — every `config.yaml` key, defaults, secrets, file layout
- [saved-queries.md](saved-queries.md) — the data-access model: CRUD, parameter binding, execution limits, trust model
- [price-and-stock.md](price-and-stock.md) — `POST /api/priceAndStockHandler` per ERP
- [hasavshevet-send-order.md](hasavshevet-send-order.md) — the order pipeline and the IMOVEIN file format

**Understanding and changing it**

- [architecture.md](architecture.md) — components, request lifecycle, packages, concurrency, failure behaviour
- [development.md](development.md) — build, test, CI gates, house rules, how to add an endpoint
- [security.md](security.md) — threat model, auth, path hardening, secrets, the documented exceptions

**Running it**

- [operations.md](operations.md) — install, upgrade, rollback, service hardening, verification
- [deployment-topologies.md](deployment-topologies.md) — single node, or a split read/write pair when the database and the import folder are on different servers
- [troubleshooting.md](troubleshooting.md) — symptom → cause → fix, and where the logs are
- [autostart.md](autostart.md) — Windows service registration
- [erp-manager-migration-plan.md](erp-manager-migration-plan.md) — handover for the backend developer: switching to the static API token
- [connector-adaptation-plan.md](connector-adaptation-plan.md) — the backend's reply: what this connector must restore before a build is deployable, and how to fix the credentials without deleting the protocol
- [erp-manager-integration.md](erp-manager-integration.md) — how the live backend calls this connector

## Project history and decisions

`../.claude/docs/` is the project's memory rather than product documentation —
useful when you need to know *why* something is the way it is:

- `01-project-origin.md` — the two predecessor repos and the merge rule
- `02-decisions.md` — every significant decision with its rationale
- `03-implementation-log.md` — what was built when, including the production cutover
- `04-operations.md` — the live production box: its exact settings and artefacts
- `05-saved-queries-migration.md` — how the 25 production queries were migrated

`../PLAN.md` is the original migration plan. It is **historical** — where it
disagrees with the docs here, the docs here are right.

## Conventions in these docs

- Paths are Windows-first, since that is the deployment target; the Linux
  equivalent is given where the daemon supports it.
- Anything marked **hard constraint** is enforced in code and asserted by tests.
  Do not work around it; see [../CLAUDE.md](../CLAUDE.md).
