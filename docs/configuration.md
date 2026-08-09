# Configuration

One YAML file holds everything except the DB password, which is encrypted
separately. The GUI writes it; you can also edit it by hand or seed it with
`cmd/cutover-seed`.

| | Windows | Linux (daemon only) |
|---|---|---|
| Config | `%PROGRAMDATA%\digi-erp-connector\config.yaml` | `/etc/digi-erp-connector/config.yaml` |
| Data dir | `%PROGRAMDATA%\digi-erp-connector\` | `/etc/digi-erp-connector/` |

The Windows path comes from the `PROGRAMDATA` environment variable, falling back
to `C:\ProgramData`. Tests exploit that to redirect the data directory, which is
why they can never touch a live installation.

The file is written atomically with mode 0600 — it contains the bearer token in
plaintext. **A change only takes effect when the daemon restarts** (GUI →
"Restart server", or `Restart-Service digi-erp-connectord`).

## Complete example

This is the live production configuration on the b4l box, which is the most
involved case — a replacement for an old electron-mssql-app install:

```yaml
erp: hasavshevet
apiListen: '[::]:8082'
debug: false
bearerToken: <64 hex chars>
erpUser: ""
imageFolders: []
sendOrderDir: ""
hasExePath: ""
hasParamFile: ""
hasBatFile: ""
db:
    driver: mssql
    host: localhost
    port: 1433
    user: sa
    database: BFL
    encrypt: true
    trustServerCertificate: true
auth:
    enabled: true
    username: bfl-reads
    password: <operator-set>
    secret: <64 hex chars, generated on first start>
    tokenTTL: 30m
queries:
    timeoutSeconds: 30
    maxRows: 100000
```

A fresh install is much smaller: `erp`, `apiListen`, `bearerToken`, the `db`
block, and nothing else. The `auth` block is shown because BFL's backend
authenticates with a username and password — it is what that box will run once
the exchange ships; leave it out unless a caller needs it.

## Top level

| Key | Default | Meaning |
|---|---|---|
| `erp` | `hasavshevet` | `hasavshevet`, `sap`, or `priority` (selectable, not implemented). Decides which price/stock backend runs and whether the order pipeline is usable. |
| `apiListen` | `127.0.0.1:8080` | `host:port` to bind. **Required.** See below. |
| `debug` | `false` | Verbose request logging. Never logs the token or DB password. |
| `bearerToken` | — | The static API credential. **Required unless the `auth` block below is enabled** — an installation must have one credential or the other. Generate with the GUI's "Generate key" (32 random bytes, hex). |
| `erpUser` | `""` | Hasavshevet login name written into order files. The GUI's "Test user" checks it exists in the customer's `USERS` table. |
| `imageFolders` | `[]` | Absolute paths served by `/api/folders/list` and `/api/file`. Nothing outside this list is reachable. |

### `apiListen` — bind address

| Value | Reachable from |
|---|---|
| `127.0.0.1:8080` | this machine only (the safe default) |
| `[::]:8082` | every interface, IPv4 and IPv6 (dual-stack) |
| `0.0.0.0:8082` | every IPv4 interface |

Use a loopback address unless the backend is genuinely on another host. If you do
expose it, the bearer token is the only thing standing in front of a database —
and remember a Windows firewall rule is also required. The production box uses
`[::]:8082` because its backend is remote; that is a deliberate, documented
exception, not a template.

## Hasavshevet order pipeline

Only meaningful when `erp: hasavshevet`. Leave empty to accept orders but not
process them (they will fail in the worker) — or rather, do not accept orders at
all until these are set.

| Key | Meaning |
|---|---|
| `sendOrderDir` | Working directory for `IMOVEIN.doc`/`.prm`. History copies go to `history/<orderNumber>/`, and `lastOrderNumber.json` lives here. |
| `hasBatFile` | Masofon-generated BAT, e.g. `C:\Hash7\digi.bat`. **Takes precedence over `hasExePath`.** Run via `cmd.exe /C` from its own directory so relative paths inside it resolve. |
| `hasExePath` | Path to `has.exe`, used when no BAT is configured. |
| `hasParamFile` | Parameter file passed to `has.exe`. |

Leave both `hasBatFile` and `hasExePath` empty and the files are still written but
nothing imports them — occasionally useful for diagnosis.

## `db`

| Key | Default | Meaning |
|---|---|---|
| `driver` | `mssql` | Only MSSQL is supported. |
| `host` | `localhost` | **Required.** Hostname or `host\instance`. |
| `port` | `1433` | **Required**, 1–65535. |
| `user` | — | **Required.** SQL login. |
| `database` | `""` | Initial catalog. Required when `erp: hasavshevet`. |
| `encrypt` | `false` | Emit `encrypt=true` in the DSN. When false the option is omitted entirely and the driver default applies. |
| `trustServerCertificate` | `false` | Emit `TrustServerCertificate=true`. Needed with a self-signed certificate — a default local SQL Server install has one. Only meaningful together with `encrypt`. |

Both TLS options default off so that existing installations keep whatever the
driver negotiated when they were commissioned. Turning `encrypt` on without
`trustServerCertificate` against a self-signed certificate fails with a
certificate error.

**The password is not in this file.** It is stored encrypted at
`secrets/db_password_<erp>.bin` (Windows DPAPI, machine scope — any process on the
machine can decrypt it, which is what lets the LocalSystem service read what the
GUI wrote). Set it via the GUI's Password field, or
`cutover-seed -db-password …`. Leaving the GUI field blank keeps the stored value.

Note the key is per-ERP: switching `erp` means the password must be entered again.

Pool settings are not configurable: 10 open, 10 idle, 30-minute lifetime, 5s ping.

## `tls`

| Key | Default | Meaning |
|---|---|---|
| `certFile` | — | PEM certificate (chain: leaf first) |
| `keyFile` | — | PEM private key |

Absent means plaintext HTTP. Setting **either** counts as "TLS requested", so a
half-configured block is a startup error rather than a silent fallback to
plaintext. The pair is loaded at startup, so a wrong path or a mismatched key
stops the service immediately instead of failing on the first request. TLS 1.2 is
the minimum version.

Configure this whenever `apiListen` is not a loopback address; see
[security.md](security.md#tls) for generating a certificate.

## `auth`

The optional credential exchange at `POST /auth/token`, for backends that
authenticate with a username and password rather than a static token. Absent or
`enabled: false` means the route does not exist and only `bearerToken`
authenticates. Full description in [authentication.md](authentication.md).

```yaml
auth:
    enabled: true
    username: bfl-reads
    password: <operator-set; generate it>
    secret: <64 hex chars; the daemon writes this on first start>
    tokenTTL: 30m
```

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Register `POST /auth/token`. Tokens it issues are accepted alongside `bearerToken`. |
| `username` | — | **Required when enabled.** Operator-set; there is exactly one account. |
| `password` | — | **Required when enabled.** Operator-set — use the GUI's *Generate*, or `cutover-seed -auth-user`. |
| `secret` | generated | HS256 signing key, 32 random bytes hex. Written by the daemon on first start if blank. Changing it invalidates every issued token. |
| `tokenTTL` | `30m` | Go duration. A malformed value falls back to the default rather than failing startup; the GUI refuses to save one. |

`enabled: true` with a blank `username` or `password` **stops the service** — the
same reasoning as the `tls` pair above. An exchange that accepts blanks is worse
than no exchange.

An installation is meant to use **one** credential: either this block or
`bearerToken`. Neither configured stops the service; both configured works and
logs a warning on every start, which is the state a box passes through while
migrating from one to the other. Clear `bearerToken` to finish the move.

The password and the secret sit in `config.yaml` in the clear, protected only by
its 0600 mode. That is deliberate: the operator has to read the password back to
give it to the calling backend.

## `queries`

| Key | Default | Meaning |
|---|---|---|
| `timeoutSeconds` | `30` | Per-execution timeout → `504 SQL_TIMEOUT`. |
| `maxRows` | `10000` | Cap across all recordsets → `413 SQL_ROW_LIMIT`. |

`maxRows` is a **functional limit, not just a safety valve**. The old Node
connector had no cap, so a query that legitimately returns more rows than this
starts failing after migration. The production box needs `100000` because one
query returns 16,183 rows. Values ≤ 0 fall back to the defaults.

## Editing safely

- **The GUI preserves keys it does not show.** `readFormConfig` starts from the
  loaded config, so `queries`, `db.encrypt` and `hasParamFile` survive a GUI save.
  This is a hard constraint — see [../CLAUDE.md](../CLAUDE.md). `legacyCompat` used
  to rely on the same rule and now has widgets of its own; `readLegacyCompat` keeps
  the habit and starts from the loaded block regardless.
- **Switching `legacyCompat.enabled` off in the GUI asks first.** It is the one
  setting whose change cuts the backend off on the next restart.
- **Hand-editing is fine**, but the daemon must be restarted, and a syntax error
  stops it starting (`server.log` will say so).
- **Do not reduce `maxRows`** on a migrated install without checking the largest
  query first.
- **Do not narrow `apiListen`** to loopback on a machine whose backend is remote.
