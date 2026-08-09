# Migration plan for the erp-manager developer

**Audience:** whoever maintains erp-manager (and the client-instance B2B backend).
**Goal:** stop authenticating with a login and password, start sending a static
API token, over HTTPS.
**Why now:** the connector's legacy compatibility layer has been deleted. Once the
new connector build is deployed, `POST /auth/token` no longer exists and
erp-manager cannot authenticate at all.

This is a coordinated change: **the connector is not deployed until you are
ready.** Nothing breaks while you work.

---

## 1. What changes, in one line

| | Today | After |
|---|---|---|
| Credential | `authLogin` + `authPassword` → JWT from `POST /auth/token` | one static API token |
| Header | `Authorization: Bearer <JWT>` | `Authorization: Bearer <token>` — *unchanged* |
| Token lifetime | 30 min, re-login on 401 | none; it does not expire |
| Transport | `http://` | `https://` |
| Routes used | `/auth/token` + 5 saved-query routes | the same 5 routes |

**Only the credential acquisition changes.** Every data call keeps the same URL,
the same header format, and the same response shapes. If your code is structured
so that "get a token" and "call the API" are separate, only the first is touched.

## 2. Why, briefly

The login/password exchange is not weaker by accident — it is weaker in kind:

- the password is `123456` and never rotates;
- the signing secret is a fixed string that shipped in the source of the old
  Node connector, so **anyone who has ever seen that code can mint a valid token**
  without knowing the password at all;
- it needs four settings (user, password, secret, expiry) where one suffices.

The static token is 32 bytes from a CSPRNG (256 bits), compared in constant time,
and there is no signing key to steal because nothing is signed.

## 3. Code changes in erp-manager

Reference points are from `src/Service/DigitradeMssqlService.php` as documented in
[erp-manager-integration.md](erp-manager-integration.md).

### 3.1 Delete `login()` (`:21-52`)

The whole method goes. Nothing replaces it — there is no exchange any more.

### 3.2 Simplify `process()` (`:173-207`)

Today:

```php
$token = $clientConnection->getToken() ?? $this->login($clientConnection);
// ... call ...
// on 401: $this->login(...) and retry once
```

After:

```php
$token = $clientConnection->getApiToken();     // from configuration, not from the wire
if (!$token) {
    throw new BadRequestHttpException(
        'No API token configured for this connector. Set it in the connection settings.'
    );
}
// ... call ...
// on 401: do NOT retry — surface the error
```

**Remove the 401-retry branch.** With a static token a 401 means the token is
wrong, so retrying with the same token loops and hides the real problem. Report it
so somebody fixes the configuration.

Keep retrying **503** instead, with a short backoff — that one is genuinely
transient (the connector answers 503 while its database is unreachable, and
recovers by itself).

### 3.3 Stop persisting a token

`ClientConnection.token` was a *cache of a token the connector issued*. It no
longer means anything. The token is now configuration, not state:

- it is never written by erp-manager, only read;
- it never expires, so there is nothing to refresh;
- clearing it by hand is no longer a recovery step for anything.

## 4. Data model and UI

Today the connection form has four auth fields. Three of them become meaningless.

| Field | Do |
|---|---|
| `authEndpoint` | **remove** — there is no exchange endpoint |
| `authLogin` | **remove** |
| `authPassword` | **remove** |
| `token` | **repurpose or replace** with `apiToken`: the operator pastes it, erp-manager only reads it |
| `baseUrl` | keep, but it must now be `https://host:port/api` |

### UI guidance

- **One field, labelled "API token"**, with help text: *"Generated in the
  connector's GUI with the Generate key button. Paste it here. It does not
  expire."*
- **Mask it** like a password, with a reveal toggle. It is equivalent to a
  database credential.
- **Never echo it back** in an API response or a log line. If your admin API
  returns the connection entity, omit or redact this field.
- **Validate on save** by calling `GET /api/health` with the token and reporting
  the result inline — the operator finds out immediately, not at the next sync.
  Expect `200 {"status":"ok"}`; treat `401` as "wrong token", `503` as "token is
  fine, the connector cannot reach its database".
- **Remove the auth endpoint/login/password inputs entirely** rather than hiding
  them. A field that exists but is ignored is a support call waiting to happen.

### Storage

Encrypt it at rest if your framework supports it. At minimum, keep it out of
logs, error messages, and anything rendered to a browser.

## 5. TLS

The connector can now serve HTTPS directly. Once the certificate is in place:

- change `baseUrl` to `https://`;
- if the certificate is internal/self-signed, either **trust the CA** in the
  backend's certificate store (preferred), or disable verification **for that host
  only** — never globally. Disabling verification still encrypts the traffic; it
  only stops you detecting an impostor.

Without TLS the API token crosses the LAN in cleartext on every request, which
would undo most of the benefit of this migration.

## 6. Rollout — fits in one maintenance window

1. **Operator:** open the connector GUI, click **Generate key**, copy the token.
   *(Optional but recommended: generate a fresh one rather than reusing the
   existing value.)*
2. **Operator:** paste it into erp-manager's connection settings; save.
3. **You:** deploy the erp-manager change (token auth, no `/auth/token`).
4. **Operator:** deploy the new connector build and restart the service.
5. **Verify** — see §7. Total expected downtime: seconds.

Order matters only between 3 and 4 in one direction: the new erp-manager works
against *both* the old and new connector, because the static token was always
accepted. **So deploy erp-manager first, confirm traffic still flows, then deploy
the connector.** That gives you a checkpoint where either side can be rolled back
independently.

## 7. Verification checklist

| Check | Expected |
|---|---|
| `GET /api/health` with the token | `200 {"status":"ok"}` |
| `GET /api/custom_sql` | bare JSON array of saved queries |
| `GET /api/sqlqueries/{name}` | object containing `value` |
| Any call with a wrong token | `401` — and **no** retry loop in your logs |
| `POST /auth/token` | `404` after the connector is deployed — confirms the old path is gone |
| A screen that reads ERP data | renders as before |

Watch for the silent failure mode: `GET /api/sqlqueries/{name}` returns **both**
`value` and a native envelope. erp-manager reads `value`. That has not changed and
is not going to — but if a screen goes blank with a `200` and nothing in the logs,
that is the field to check first.

## 8. What is NOT changing

So you can scope the work confidently:

- the five saved-query routes: same paths, same methods, same bodies;
- `POST /api/create_custom_sql` still exists as an alias;
- response shapes, including `value`, `rowsAffected`, `.000Z` datetimes and
  numeric decimals;
- `queries.json` format;
- rate limiting (25 req/s, burst 50 per IP);
- the meaning of 401.

## 9. For the B2B backend (`DigitradeMssqlService.php` on the customer VM)

It already supports a pre-provisioned static token, so it may need **no code
change** — just configure the token instead of the auth endpoint. Confirm that:

- when a token is configured it does not also try `/auth/token`;
- `POST /api/sendOrder` still expects `202 Accepted`;
- if it points at an order-writing node, the connector there may have no database,
  in which case the order must carry an `account` object — see
  [deployment-topologies.md](deployment-topologies.md).

## 10. Questions to send back

1. Can erp-manager reach the connector over HTTPS with an internal certificate,
   or does the certificate need to come from a specific CA?
2. Does anything besides erp-manager and the B2B backend call the connector — a
   monitor, a script, a scheduled job? Anything hitting `/api/ping` or
   `/api/query` will break; both are gone.
3. Do you want one token per connector instance (recommended) or a shared one?
