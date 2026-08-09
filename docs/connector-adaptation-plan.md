# What the connector has to do to fit erp-manager

**Audience:** whoever maintains digi-erp-connector.
**Author:** the erp-manager side.
**Status:** reply to [erp-manager-migration-plan.md](erp-manager-migration-plan.md), which asked
erp-manager to move to a static API token. We are not doing that in this window.

> **Connector side: §3 and §4 are implemented** on branch `feature/auth-exchange`
> (2026-08-09). R1–R6 are covered and pinned by `internal/api/auth_exchange_test.go`;
> `POST /api/query` was not reintroduced. The resulting feature is documented in
> [authentication.md](authentication.md). Still open: the §6 rollout — new credentials
> generated on the box, sent out of band, and ClientConnection rows 67 and 76 updated —
> and only then a release and a deploy. b4l stays on 1.0.4 until all of that is done.

**The answer in one line:** the wire protocol was never the problem — the *credentials* were.
Keep `POST /auth/token` and make it a first-class feature with per-install, operator-generated
credentials and a per-install signing secret. Same protocol, both real weaknesses gone, and
erp-manager needs no code change at all.

---

## 0. ⚠ Do not install the current build on a customer box

`949f4fc "Delete the electron-mssql-app compatibility layer"` is on `origin/main`, so `auto-tag`
has already fired and `release-windows` has published an installer built from it. **That installer
breaks BFL the moment it is installed.**

Exactly what happens: erp-manager calls `POST http://84.110.65.54:8082/auth/token`, gets `404`,
`login()` throws `Failed to login to Digitrade MSSQL. Status code: 404`
(`DigitradeMssqlService.php:39-42`), `process()` converts it to a `BadRequestHttpException`, and
every ERP-backed screen and sync for that client fails. There is no fallback path: erp-manager has
no static-token mode, which is precisely what the migration plan was asking us to build.

Until this plan is implemented, treat every release after `267e1ad` as **not deployable to a
customer**. The b4l box should stay on `1.0.4`.

## 1. Why erp-manager is not doing the static-token migration now

Not a refusal — a sequencing and blast-radius problem. Three concrete reasons:

**The fields the plan asks us to delete are shared.** `ClientConnection` serves *every* ERP, not
just this connector. `authEndpoint`, `authLogin` and `authPassword` are read by `SapB1Service:25-30`,
`DynamicsService:25`, `DigitradeService:22-26` and `PriorityService:23-24`; `token` is written as a
session cache by `SapB1Service:54`, `DynamicsService:60`, `DigitradeService:50`, `Fetcher:43` and
`ErpController:52`. Removing them breaks SAP B1, Dynamics, Digitrade REST and Priority. §4 of the
migration plan — "remove `authEndpoint`, remove `authLogin`, remove `authPassword`, repurpose
`token`" — is written as if the entity belonged to this integration alone. It does not.

**So it has to be additive, which makes it a real project.** A new `apiToken` column, a Doctrine
migration, write-only API Platform exposure so the token is never echoed back (today the entity is
a bare `#[ApiResource]` — every field is readable, which is why `authPassword` and `token` show up
in `GET /api/client_connections`), a `hasApiToken` flag for the SPA, connection-type-conditional
form rendering in `erp-manager-next-app`, and a save-time validation call. That is backend **and**
frontend across two repos and two planes — which in this workspace means an `/architect` pass
before any code, not a same-day change.

**There is a live production integration in the middle.** BFL is serving customers through this
path right now. The safe order is: make the connector's credentials strong first (cheap, one repo),
then migrate the credential *model* later (expensive, four repos) — not both at once against a live
box.

## 2. What erp-manager requires, and where the connector stands

Requirements are derived from unchanged erp-manager code. Reference:
[erp-manager-integration.md](erp-manager-integration.md) §5–§6, which is still accurate.

| # | Requirement | Now | Action |
|---|---|---|---|
| R1 | `POST {authEndpoint}` accepting JSON `{"username","password"}`, answering **200** with a body containing **`access_token`** | ❌ route deleted | **restore** as a first-class feature |
| R2 | `Authorization: Bearer <the issued token>` accepted on every data route | ❌ only the static token is accepted | **restore** dual acceptance |
| R3 | A bad or expired credential answers **exactly 401** | ✅ `middleware/auth.go` | keep — see below |
| R4 | `GET /api/ping` reachable | ❌ deleted | restore, or accept a one-line change from us (§4) |
| R5 | The five saved-query routes plus the `create_custom_sql` alias | ✅ `server.go:88-94` | keep |
| R6 | `value` on `sqlqueries/{name}`; a **bare array** on `custom_sql` | ✅ `dto/queries.go:50-51` | keep — do not "clean up" |

On **R1**, two details that are easy to get wrong because erp-manager is stricter than the old app's
documented contract:

- `login()` requires HTTP **200** exactly (`:39-42`). A `201` or a `204` is a failure.
- It reads **only** `access_token` (`:45`). `token_type` and `expires_in` are parsed and discarded,
  so their values do not matter — but if `access_token` is absent from a `200`, erp-manager stores
  `null` and the next call goes out as a bare `Bearer ` and 401s in a loop. Always include it.

On **R3**, your new `middleware/auth.go` comment already states the rule correctly. Worth keeping
literally: erp-manager caches the issued token in `client_connection.token` **forever**, never looks
at `expires_in`, and re-authenticates *only* on a 401 — once, then gives up (`process():187-200`).
A 403, a 500 or an HTML error page for a dead token means a human has to clear that column by hand.

## 3. The part that actually fixes the security problem

The migration plan's §2 makes three criticisms. Two are entirely fair, and neither requires deleting
the protocol:

| Criticism | Fix, connector-side only |
|---|---|
| the password is `123456` and never rotates | make it per-install and operator-set, generated in the GUI like the bearer token. erp-manager stores whatever it is told; the value is a database row, not code. |
| the signing secret shipped in the old Node source, so anyone who saw that code can mint a valid token without the password | generate a random 32-byte secret on first run, per install, and never ship a default. This is the serious one, and it is a `crypto/rand` call. |
| four settings where one suffices | true, and it stays true — the cost of one integration that cannot yet be changed. Nest them so it reads as one feature, not four knobs. |

Concretely, promote the block out of "legacy compatibility" and into the connector's own auth model,
so nobody deletes it again as dead weight:

```yaml
bearerToken: <32 random bytes, hex>     # unchanged: for curl, monitoring, and the B2B backend
auth:
  enabled: true                         # the credential exchange
  username: <operator-set>              # NOT "digitrade"
  password: <GUI-generated>             # NOT "123456"
  secret: <auto-generated on first run>  # NEVER a shipped constant
  tokenTTL: 30m
```

Keep accepting **both** credentials on every route, exactly as `AuthWithLegacy` did: constant-time
static comparison first, signature check second. That already worked in production, costs nothing,
and is what lets the eventual static-token migration be a configuration change on our side rather
than another coordinated deploy.

What we ask you **not** to do: reintroduce `POST /api/query`. Nothing in erp-manager has ever called
it, and the raw-SQL switch can stay deleted. `allowRawSQL: true` was set on b4l, so confirm from
`server.log` that nothing else uses it before you assume the same.

## 4. `/api/ping` — currently a silent false positive

erp-manager's **CHECK CONNECTION** button in the connection editor resolves to
`/api/erp/{connId}/ping` (`erp-manager-next-app/src/utils/getCheckConnectionUrl.ts`), which reaches
the connector through the generic passthrough `ErpController::getData()` as `GET {baseUrl}/ping`.

With `/api/ping` deleted this does not report an error. `fetch()` maps **404 → `[]`**
(`DigitradeMssqlService.php:72-74`), the controller returns that as `200 []`, and the SPA's
`.then(() => setOpenSuccessModal(true))` fires. **The operator gets a green "connection OK" from a
connector that answered 404.** That is worse than a failure, and it is why R4 is on the list.

Two ways out, and we are fine with either:

- **Connector restores `GET /api/ping`** (recommended): `{"ok":true,"ts":<epoch ms>}`, no database
  touch, authenticated and rate-limited like everything else. ~15 lines, no risk, and it keeps the
  meaning the button wants — *is the service up and is my credential good* — which `/api/health`
  does **not** answer, because health pings the database and returns 503 when MSSQL is down
  (`handlers/health.go:18-21`). A connector with a healthy API and a stopped SQL Server would report
  "connection failed".
- **We repoint the button to `health`** — a one-line change in that URL builder. Say the word and we
  will ship it, accepting the 503-on-DB-down semantics.

Either way the false positive itself should be fixed on our side, and we will: the SPA must check
the response, not merely that JSON parsed. That is our bug, not yours.

## 5. TLS — agreed, and it needs nothing from us

`dc7dcbd` adding `tls.certFile`/`tls.keyFile` is the right move and we support it. It costs
erp-manager **zero code**: every request already sets `verify_peer: false` and `verify_host: false`
(`DigitradeMssqlService.php:35-36`, and the same in `create`/`edit`/`delete`), so a self-signed or
internal certificate works immediately. Only `client_connection.base_url` changes from `http://` to
`https://` — a database value.

Stated plainly so it is not mistaken for an endorsement: with verification off we get encryption
but no impostor detection. Tightening that is on our list, not yours. It is still strictly better
than the token crossing the LAN in cleartext.

## 6. Rollout

Order matters, and it is the reverse of the migration plan's — because here the *connector* is what
changes.

1. **You:** implement §3, build, but do not deploy.
2. **You → us:** send the new username and password for each connector instance, out of band.
3. **Us (operator):** update `authLogin` / `authPassword` on the affected `ClientConnection` rows
   **first**. This has to precede the deploy: a new signing secret invalidates the JWT cached in
   `client_connection.token` immediately, so the first call after your deploy re-authenticates, and
   it must find the new password already stored or it fails.
4. **You:** deploy the connector and restart the service.
5. **Verify** — §7. Expected impact: at most one failed request, self-healing on the 401 retry.

**BFL has two connector instances**, and each has its own `config.yaml` and therefore its own
credentials: `ClientConnection` **67** → `:8082` (reads) and **76** → `:8083`
(`BFL-SendOrders`, the write node per [deployment-topologies.md](deployment-topologies.md)). Row 76
needs its own username/password. Row 76 currently has no cached token at all, so it authenticates on
its very first call — it will surface any mismatch immediately.

Rollback is a previous release plus the old credentials, on either side independently, because
nothing here is a schema change.

## 7. Verification

What we will run after step 4, from the erp-manager side:

| Check | Expected |
|---|---|
| `POST {authEndpoint}` with the new credentials | `200`, body contains `access_token` |
| the same with the old `digitrade`/`123456` | `401` — confirms the shipped defaults are gone |
| any data route with the issued token | `200` |
| any data route with the static `bearerToken` | `200` — dual acceptance intact |
| any data route with a wrong token | `401`, and exactly **one** re-auth attempt in our logs |
| `GET /api/custom_sql` | bare JSON array |
| `GET /api/sqlqueries/IndividualProductList` | object containing `value`, 16183 rows |
| CHECK CONNECTION in the SPA | green only when the connector really answers |
| an ERP data screen for BFL | renders as before |

The failure mode to watch is still the silent one: a `200` with an empty list and nothing in the
logs means `value` went missing. It is the first thing to check, before the SQL.

## 8. What we will do, so this is not a dead end

The static-token direction is right; it is the timing we are pushing back on. On our side, as a
separate piece of work:

- add `apiToken` to `ClientConnection` — additive, write-only through the API, with a `hasApiToken`
  flag for the SPA, leaving the shared auth fields untouched for the other four ERPs;
- render the connection form by connection type: for `DIGITRADE_MSSQL`, one masked **API token**
  field with a reveal toggle instead of endpoint/login/password;
- validate on save against `GET /api/health`, reporting `401` as "wrong token" and `503` as "token
  fine, connector cannot reach its database";
- drop the 401 retry for that connection type once the token is static, keeping a short-backoff
  retry for `503`.

That design is already sketched and it is the plan's §3–§4, implemented additively. It needs an
`/architect` pass because it crosses both planes. When it ships, `auth.enabled: false` on the
connector becomes a configuration change and the exchange can finally go.

## 9. Answers to the questions in §10 of your plan

1. **HTTPS with an internal certificate** — yes, works today with no change from us, because
   certificate verification is already disabled on every request. No specific CA is required. We
   would rather trust a real CA eventually, but that is our follow-up, not a precondition for you.
2. **Anything else calling the connector** — yes, two things beyond erp-manager. The
   **client-instance B2B backend** on the customer VM
   (`api/src/Service/ERP/DigitradeMssqlService.php`) executes saved queries via `sqlqueries/{name}`
   and POSTs `/api/sendOrder` expecting `202`; it already supports a pre-provisioned static token
   and errors explicitly when neither a token nor an `authEndpoint` is configured (`:31-36`), so it
   is the one caller that could move to static tokens today. And erp-manager's own CHECK CONNECTION
   button hits `/api/ping` — see §4. We know of nothing using `/api/query`.
3. **One token per connector instance, not shared.** BFL already proves the need: two instances,
   two ports, two config files. A shared credential would also make the split read/write topology
   impossible to revoke independently.
