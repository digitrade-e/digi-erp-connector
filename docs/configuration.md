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

The file is written atomically with mode 0600 — it contains the API credentials
in plaintext. **A change only takes effect when the daemon restarts** (GUI →
"Restart server", or `Restart-Service digi-erp-connectord`).

## Complete example

This is the live production configuration on the b4l box, which is the most
involved case — a replacement for an old electron-mssql-app install:

```yaml
erp: hasavshevet
apiListen: '[::]:8082'
debug: false
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
    username: bfl-reads
    password: <operator-set>
    secret: <64 hex chars, generated on first start>
    tokenTTL: 30m
queries:
    timeoutSeconds: 30
    maxRows: 100000
```

A fresh install is much smaller: `erp`, `apiListen`, the `auth` block and the
`db` block.

## Top level

| Key | Default | Meaning |
|---|---|---|
| `erp` | `hasavshevet` | `hasavshevet`, `sap`, or `priority` (selectable, not implemented). Decides which price/stock backend runs and whether the order pipeline is usable. |
| `apiListen` | `127.0.0.1:8080` | `host:port` to bind. **Required.** See below. |
| `debug` | `false` | Verbose request logging. Never logs credentials or the DB password. |
| `erpUser` | `""` | Hasavshevet login name written into order files. The GUI's "Test user" checks it exists in the customer's `USERS` table. |
| `imageFolders` | `[]` | Absolute paths served by `/api/folders/list` and `/api/file`. Nothing outside this list is reachable. |

### `apiListen` — bind address

| Value | Reachable from |
|---|---|
| `127.0.0.1:8080` | this machine only (the safe default) |
| `[::]:8082` | every interface, IPv4 and IPv6 (dual-stack) |
| `0.0.0.0:8082` | every IPv4 interface |

Use a loopback address unless the backend is genuinely on another host. If you do
expose it, the credential is the only thing standing in front of a database —
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

## Hasavshevet receipts

Optional, for `POST /api/sendReceipt` — see
[hasavshevet-send-receipt.md](hasavshevet-send-receipt.md). All four keys are
omitted from the file when unset, and without `sendReceiptDir` the route answers
`503 RECEIPTS_NOT_CONFIGURED`. None has a GUI widget; a GUI save keeps them.

| Key | Meaning |
|---|---|
| `sendReceiptDir` | The receipt importer's folder: only `KUPAIN_{n}.doc` and `KUPAIN.prm` are written here. **Must be absolute and must not be `sendOrderDir`** (compared after resolving `..`, trailing separators and symlinks; case-insensitively on Windows) — the daemon will not start and the GUI will not save. Receipts are configured only with `erp: hasavshevet` and this set. |
| `hasReceiptBatFile` | Model (b), the BAT way: this BAT runs once (via `cmd.exe /C`, from its own folder, 90 s cap) after each receipt is written. **Absolute.** Empty (and no `hasReceiptParamFile`) is model (a): files only, imported by hand. |
| `hasReceiptParamFile` | Model (b), the `has.exe` way — the mirror of the orders' `hasExePath` + `hasParamFile`: the orders' `has.exe` (`hasExePath`) runs once with this parameter file, from `sendReceiptDir`, 90 s cap, after each receipt is written. **Absolute, needs an absolute `hasExePath`, and is refused together with `hasReceiptBatFile`** (ambiguous) — the daemon will not start and the GUI will not save. `hasExePath` alone imports no receipt. |
| `receiptCardNames` | `{issuer: {code: name}, brand: {code: name}}` — overrides of the receipt's card name (F17), keyed by the raw HYP code. Codes compare as numbers (`"06"` is `6`). A name that is not Windows-1255 or is wider than 25 bytes, a blank code, or two keys of one table naming the same code (`"6"` and `"06"`) stops the daemon and the GUI save. |

The receipt number store (`receipts/receiptNumber.json`), the idempotency index
(`receipts/index/`), the process lock (`receipts/writer.lock`) and history copies
(`history/receipts/{n}/`) live in the **data directory**, never in
`sendReceiptDir`; on Linux the receipt directories are 0700 and their files
0600. The store is created only by `cutover-seed -receipt-seed N`; until then
receipts answer 503. Back up `receipts/` with the rest of the data directory — a
lost index turns a retry into a second receipt.

One connector process per data directory: the receipt writer holds an OS lock on
`receipts/writer.lock` while it runs. A second process on the same directory
serves orders and queries but answers receipts `503 RECEIPT_DATA_IN_USE`, and
takes over once the first has stopped. The file itself is not the lock — one
left behind by a crash locks nobody out — so never delete it to "unlock" a
connector; stop the other process.

## Data directory permissions (Windows)

The data directory gets a **protected DACL** — nothing inherited from
`%ProgramData%`, whose default ACL lets every local user read — granting Full
Control to exactly `SYSTEM`, `BUILTIN\Administrators` and the account of the
process that sets it (the service: LocalSystem, so in practice the first two),
plus **OWNER RIGHTS with READ_CONTROL only**. Everything under it inherits that:
`config.yaml`, `secrets\`, `queries.json`, the logs and the receipt data.

**What is protected, when:**

| When | By | What |
|---|---|---|
| Every service start, **before the config is read** — so also the installer's first `sc start`, which has no config yet | the daemon | the data directory and everything under it (below). If this fails the daemon **does not start**. |
| Every service start, right after `config.yaml` loads | the daemon | `config.yaml` itself (the same DACL, set explicitly). A failure is logged; the daemon starts — the file inherits the directory's DACL anyway. |
| Every save in the elevated GUI | the GUI | `config.yaml`. |
| The GUI save that creates `config.yaml`, or the first save after a GUI launch that found no data directory | the GUI | the data directory and everything under it, as the daemon does. |

A GUI failure is a "Saved, but…" warning in the status bar and a line in the
log, not a refused save: the file is saved, and the service sets the DACL again
when it starts. Nothing to configure; nothing happens on Linux.

**Everything under the directory is reset.** After protecting the directory, the
daemon walks it (without following links) and resets every file and folder that
carries a DACL of its own — protected, NULL, or with explicit ACEs — to the
inherited DACL alone. A folder planted before the first protected start (a
`receipts\` created by a standard user with its own ACL, say) therefore does not
survive, and neither do explicit ACEs an operator added under the data directory
by hand: they are dropped at the next start. Entries that already only inherit
are just read, so a normal start rewrites nothing.

**Symlinks and junctions refuse startup.** If the data directory itself, or
anything under it, is a symlink, a junction or another reparse point that
redirects a path, the daemon changes nothing and does not start (logged:
`a symlink or junction is in the data directory`): receipt data written through
it would land outside the protected tree. Remove the link. (Data Deduplication
files count as regular files.)

**Owner.** When the daemon runs as SYSTEM it also makes SYSTEM the directory's
owner (a failure is logged, not fatal). Under any other account the owner is left
as it is.

Why OWNER RIGHTS: without it, whoever *owns* the directory or a file in it keeps
an implicit right to read and rewrite its DACL, whatever the DACL says — and
the owner can be a plain user, since Users may create folders in
`%ProgramData%`. The OWNER RIGHTS ACE replaces those implicit rights with what it
grants: the owner can still see who has access (READ_CONTROL, which `icacls`
and Explorer need), but cannot change it or read data unless it is SYSTEM, an
Administrator or the service account.

Consequence: anything that reads the data directory must run **elevated** or as
SYSTEM — the GUI does (desktop shortcut), and so must `cutover-seed`, Notepad on
`server.log`, or a backup job.

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

**Required.** This installation's only credential: the caller posts the username
and password to `POST /auth/token` and gets a short-lived token back. Full
description in [authentication.md](authentication.md).

```yaml
auth:
    username: bfl-reads
    password: <operator-set; generate it>
    secret: <64 hex chars; the daemon writes this on first start>
    tokenTTL: 30m
```

| Key | Default | Meaning |
|---|---|---|
| `username` | — | **Required.** Operator-set; there is exactly one account. |
| `password` | — | **Required.** Operator-set — use the GUI's *Generate*, or `cutover-seed -auth-user`. |
| `secret` | generated | HS256 signing key, 32 random bytes hex. Written by the daemon on first start if blank. Changing it invalidates every issued token. |
| `tokenTTL` | `30m` | Go duration. A malformed value falls back to the default rather than failing startup; the GUI refuses to save one. |

A blank `username` or `password` **stops the service** — the same reasoning as
the `tls` pair above, and with more at stake: an exchange that accepts empty
strings is an open door, and there is no second credential to fall back to.

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
