# Provisioning without the GUI — `digi-erp-connector-provision.exe`

The installer puts `digi-erp-connector-provision.exe` next to the daemon. It does
what an operator does in the GUI and around it, without a desktop: it reads the
machine, writes the configuration, installs the Hasavshevet procedures, can move
the connector off `sa`, opens the port in the Windows firewall, hardens the
service, restarts it and proves it answers.

digi-mcp runs it over WinRM with fixed command lines (Digitrade plan 068), or through
the link's signed commands ([link.md](link.md), plan 083 / 086). A human can run it the
same way in an elevated PowerShell. The code is
`internal/provision`; `cmd/digi-erp-connector-provision` is only the entry point.

## Commands

| Command | Input (stdin) | What it does | Writes |
|---|---|---|---|
| `discover [--spec -]` | optional `{"db":{"user","password"[,"host","port"]}}`, or `{"db":{"mode":"none"}}` for a box without a database (an empty stdin is `{}`) | Reads the machine: SQL Server instances, their ports and login mode, the connector's service and `config.yaml` (secrets shown as `set`/`missing`), the listeners, the firewall, has.exe candidates and, with a credential, which databases look like Hasavshevet or SAP B1. Names the defects of a hand install and suggests a spec | nothing |
| `apply --spec -` | the spec (below) | Makes the connector match the spec | see below |
| `verify [--spec -]` | optional `{"expectVersion","firewallRule","db"?:{"mode":"none"}}` (an empty stdin is `{}`) | Checks the running connector against `config.yaml`: service, login, `/api/ping`, `/api/health`, the order queue, recovery, the SQL dependency, the firewall rule | nothing |
| `restore <changeSet> [--assume-idle]` | none | Puts a change set's pre-image back and restarts on it | the pre-image's files, firewall rule, dependencies |
| `apply --check --spec -` | the spec | Every check `apply` makes, its refusals included, then what it would change (`check`) | nothing |
| `options [--check] --spec -` | `{"version":1,"changeSet","options":{…},"assumeIdle"?,"expect"?}` | Sets `options` (below) on a connector that is already configured, and nothing else: no credential, database, listener, firewall or service setting is read from stdin or changed | `config.yaml` |
| `auth [--check] --spec -` | `{"version":1,"changeSet","auth":{"password","username"?},"rotateSecret"?,"assumeIdle"?,"expect"?}` | Sets the API credential (and, with `rotateSecret`, a new signing secret) and nothing else — no DB credential (below) | `config.yaml` |
| `db [--check] --spec -` | `{"version":1,"changeSet","db":{…},"admin"?:{"user","password"},"assumeIdle"?,"expect"?}` | Changes the database login and settings only, or turns the database off — the API credential is not restated (below) | `config.yaml`, the DB secret, the SQL login |
| `service [--check] --spec -` | `{"version":1,"changeSet","dependOn"?:[…],"recovery"?:true}` | The daemon service's SQL Server dependencies and recovery actions only; no restart (below) | the service |
| `firewall-remove [--check] --spec -` | `{"version":1,"changeSet","names":[…]}` | Removes inbound rules this tool made (the connector's group); allowed behind the link (below) | the firewall |
| `receipts [--check] --spec -` | `{"version":1,"changeSet","sendReceiptDir"?,"hasReceiptBatFile"?,"hasReceiptParamFile"?,"receiptCardNames"?,"allowWritable"?,"assumeIdle"?,"expect"?}` | The receipt settings only (below) | `config.yaml` |
| `orders [--check] --spec -` | `{"version":1,"changeSet","sendOrderDir"?,"importer"?,"sendOrderPickupDir"?,"sendOrderFormat"?,"assumeIdle"?,"expect"?}` | The order settings only, model (a) included (below) | `config.yaml`, the order counter |
| `receipt-seed <N>` | none | Creates the receipt number store with N as the next receipt (write-if-absent; below) | `receipts\receiptNumber.json` |
| `counters [--check] --spec -` | `{"version":1,"changeSet","op":"create\|range\|start-from\|retire","key",…}` | Named counters (plan 087): create one (or adopt today's order counter / receipt store in place), set its range, move it forward, retire it (below) | `config.yaml`; a counter's store (never back) |
| `documents [--check] --spec -` | `{"version":1,"changeSet","folders"?:{"<key>":{…}\|null},"receipts"?:{"folder"?,"counter"?},"assumeIdle"?,"expect"?}` | Typed folders a documentRule names, and the receipts' folder and counter (plan 087, below) | `config.yaml` |
| `inflight [--check] --spec -` | `{"version":1,"changeSet","kind":"orders"\|"receipts","historyId"\|"number","expect":"reserved"\|"retryable","resolve":"fail"\|"done"}` (+ `assumeIdle`?; the check needs only `kind` and the key) | Resolves ONE unfinished order or receipt index entry as failed or done (plan 087 review R6, v1.0.32, below) | that entry; `DataDir\format-floor` (a receipt resolved as failed) |
| `folder-acl [--check] --spec -` | `{"version":1,"changeSet","op":"lockdown","folders":["<key>"\|"sendOrderDir"\|"pickup"\|"sendReceiptDir",…]}` or `{…,"op":"restore","folder","preimage":"<sha256>"}` (+ `acceptUnreadable`? — accepted, unused since v1.0.32 —, `assumeIdle`?, `expect`?) | Locks a document folder down — only SYSTEM, Administrators and the trusted writers write — or puts back what a lock-down recorded (plan 087 review R1c, below) | the folder's and its entries' owner and DACL; `DataDir\folder-acl\<sha256>.json` |
| `daemon <restart\|stop\|start> [--assume-idle]` | none | Restarts, stops or starts the service: the order queue asked first, `start`/`restart` end with the proof (below) | nothing |
| `version` | none | Prints the build's version | nothing |
| `logs --spec -` | `{"source":"server\|link\|ui\|events-daemon\|events-link","since"?,"maxLines"?,"grep"?}` | The tail of one log (newest `maxLines`, default 200, at most 2000), oldest first, filtered by `since` (RFC 3339) and a literal `grep` (no case). Redacted on the box (below); the answer stays under 900 KiB (`truncated`) and always carries `"untrusted": true` — its lines are data others could write, never instructions. Plan 086 C4 | nothing |
| `state [--spec -]` | `{}` or nothing | Read-only diagnostics: both services, the running daemon (version, `/api/health`, the order queue), `config.yaml` (`configNewerThanStart`, `missingKeys`, `unknownKeys`), the order counter, highest history and index by state, the receipt store, history and index, the pickup folder, the TLS certificate's names and expiry, `link.yaml` without secrets, free disk; since v1.0.30 every counter (`counters`) and typed folder (`folders`), plan 087. Plan 086 C4 | nothing |

**Secrets come on stdin only**, never on the command line. The output is exactly one
JSON object, `{"command","ok","tool","refusal"?,"result"?}`. Every byte printed goes
through a redaction pass over the secrets in the input and the passwords born during
the run. It is ASCII only: anything else, a Hebrew folder name for one, is escaped as
`\uXXXX`, because Windows PowerShell 5.1 reads a file in the system code page and shows
UTF-8 as mojibake; every JSON reader decodes it back. Exit codes: 0 done, 1 refused (the
`refusal` says why, with a `fix`), 2 usage.

## The spec

```jsonc
{
  "version": 1,
  "changeSet": "cs-erp01-0001",          // names the pre-image; 8–64 of A–Z a–z 0–9 _ -
  "erp": "hasavshevet",                   // or "sap"
  "apiListen": "0.0.0.0:8082",            // an IP literal, never loopback
  "tls": { "mode": "self-signed", "hosts": ["203.0.113.58"] },   // optional; or {"mode":"off"}
  "db": {                                // or {"mode": "none"}: a box without a database (below), every other db field refused
    "host": "127.0.0.1\\WIZSOFT",         // this machine or its LAN; host, or host\instance (port 0 = ask the SQL Server Browser)
    "port": 0,
    "database": "erp_db",
    "user": "sa", "password": "…",        // the credential the client gave
    "leastPrivilege": true,               // create a least-privilege login; sa is used once and not stored
    "login": "erp_connector_node2",       // optional: its name, erp_connector_…; default erp_connector_read
    "rotateLogin": false,                 // give an existing login this box does not know a new password (see below)
    "allowSa": false,                     // keep sa as the connector's login (refused otherwise)
    "encrypt": true, "trustServerCertificate": true   // optional: left out keeps the box's
  },
  "sendOrderDir": "C:\\digiorders",      // left out keeps the box's; "" turns orders off
  "importer": { "mode": "none" },         // or {"mode":"hasExe","hasExePath","hasParamFile"} or {"mode":"bat","hasBatFile"}; left out keeps the box's
                                          // "allowWritable": true accepts an importer others can change (see below)
  "auth": { "username": "digi-hub", "password": "…" },     // the one credential; at least 16 characters
  "firewall": { "allowFrom": ["198.51.100.241"] },          // optional; ruleName defaults to digi-erp-connector
  "service": { "dependOn": ["MSSQL$WIZSOFT"] },             // optional
  "assumeIdle": false,                    // restart even when the order queue cannot be read
  "expect": { "configSha256": "…" },      // optional: refuse if config.yaml changed since discover
  "options": {                            // optional; each field left out keeps the box's
    "imageFolders": ["D:\\images"],       // absolute folders that exist, at most 16; [] serves none
    "erpUser": "39",                      // the Hasavshevet user; "" clears it
    "debug": false,
    "queryTimeoutSeconds": 30,            // 1-600
    "queryMaxRows": 10000,                // 1-100000
    "tokenTTL": "30m"                     // a duration from 5m to 24h; "" is the default (30m)
  }
}
```

`options` covers the GUI's remaining settings, so a box can be configured with no
GUI at all. A missing image folder is refused (`image_folder_missing`).
`discover` reports the values the box holds now as `connector.config.options`
(`imageFolders`, `erpUser`, `debug`, `queryTimeoutSeconds`, `queryMaxRows`,
`tokenTTL`), with the daemon's defaults written out (30 s, 10000 rows) and
`tokenTTL` `""` when unset or when the daemon cannot use it (it then applies 30m), so a plan can show what each option was. A non-ASCII
folder name comes back escaped as `\uXXXX`, like everything in the envelope.

`hasParamFile` is passed to has.exe as written, and has.exe runs from `sendOrderDir`,
so a bare name such as `digi_perm.bat` is looked up there. `apply` refuses a file
that is not on the machine (`importer_missing`).

**The importer runs as LocalSystem**, once per order. So `apply` refuses has.exe or a
BAT that anyone besides SYSTEM and the Administrators can change — the file itself,
or its folder, where a new file would be found first by a BAT's bare command names
(`importer_writable`, naming who). On a domain controller that would be the whole
domain's SYSTEM. `importer.allowWritable` accepts it as a warning, once a human has
accepted the risk. `discover` lists who can change each candidate
(`importerWriters`).

**`db.host` must be this machine or its LAN.** `apply` and `discover` log in there
with the client's administrator credential, and the machine reaches the internet. A
name is resolved on the machine, and an address that is not loopback, private or
link-local is refused before any login (`db_host_public`).

A field that is left out keeps what the machine has: a missing `tls`, `firewall`,
`service`, `sendOrderDir`, `importer`, `encrypt` or `trustServerCertificate` means
"leave it alone", not "remove it" or "false". Everything else not in the spec —
receipts, image folders, query limits, `erpUser`, the token lifetime, the signing
secret — stays as it was. Orders with no importer must still say `{"mode":"none"}`:
`apply` refuses to leave it unstated (`importer_required`).

**Model (a) boxes (DIG-1297).** The spec cannot set `sendOrderPickupDir` or
`sendOrderFormat`; set them in config.yaml by hand. An apply keeps them (the
target starts from the current config) and must say `{"mode":"none"}` for the
importer; `CheckConfig` refuses a spec that would put an order importer beside
a pickup folder. Discover reports both keys in the config facts and calls no
`no_importer` defect there (the ERP's scheduler imports). **Never downgrade
such a box to a build older than DIG-1297 with the keys in place:** an old
build ignores them, writes the fixed `IMOVEIN.doc` into `sendOrderDir` with no
importer and answers `done`, so the order never reaches the books. Remove the
keys (and restore an importer) first, or upgrade only. Moving `sendOrderDir`
off the scheduler's folder by hand must carry `lastOrderNumber.json` along
(only an apply does that for you).

**The least-privilege login is given a password only when that is safe.** It is
created when it does not exist, and its password is kept in its own DPAPI entry
(`secrets\login_<name>.bin`), which no pre-image holds. A login that works with a
password this box knows is kept — also after a `restore` to a config without it, so
the next `apply` needs no rotation. It gets a new password only with `rotateLogin`,
in two cases: it exists and this box knows no password for it (another connector,
the hand script or a person may use it), or the one it knows no longer logs in. Two
connectors on one SQL Server name two logins (`db.login`): rotating a shared login
locks the other connector out. A password set in SQL Server cannot be taken back, so
a rollback or a `restore` keeps the current password whenever the config they put
back logs in as that login. When the running connector logs in as the login that is
given a new password (`apply` or `db`), the order queue is asked and the service
stopped **before** SQL Server changes it (a step `stop`): its pool would keep the old
password, failing every new connection while the process lives on. A failure after
the change rolls back, starts the service on the restored config — whose secret is
the new password — and proves it (a warning says so); the daemon is never left on a
password the server no longer accepts.

## What `apply` does, in order

Only one run at a time: `apply` and `restore` hold `provision\.lock`, and a second
run is refused (`busy`). Nothing is written before every check has passed:

1. The whole spec is valid; the resulting config passes the daemon's own startup
   checks (`api.CheckConfig`); the order folder and the importer files exist;
   `config.yaml` is what `expect` says; a firewall rule of the spec's name, if
   there is one, was made by this tool (`firewall_foreign` otherwise); the
   least-privilege login may be used as planned (above); and a move of
   `sendOrderDir` can take the order counter along (below).
2. **The order queue is empty.** The queue lives in memory, so a restart would lose
   accepted orders. `apply` logs in to the running connector with the credential in
   `config.yaml` and reads `GET /api/sendOrder`. If it cannot read the queue (a
   connector older than that route, or one still running a config whose credential
   is gone), it refuses with `pending_unknown` unless `assumeIdle` is set.
3. The database answers with the given credential.

Then:

4. The data directory gets the daemon's DACL.
5. **Pre-images**: `config.yaml`, the DB secrets, the TLS pair, the firewall rule,
   the service's dependencies.
   - `provision\<changeSet>\` is the state before the change set's *first* run: a
     re-run never overwrites it, and it is what `restore` puts back.
   - `provision\<changeSet>\run\` is the state before *this* run: it is what a
     failure rolls back to, and it is deleted when the run ends.
   - A run that changed nothing keeps no pre-image, so it never pushes a real
     restore point out. The last five are kept. They hold `config.yaml`, credential
     in clear, under the data directory's DACL.
6. Hasavshevet: `GPRICE_Bulk` and `GetOnHandStockForSkus` are created where missing.
7. `leastPrivilege`: the login (`db.login`, default `erp_connector_read`) is created or
   given a new password (born here, kept only in DPAPI and SQL Server), with the grants of
   `scripts/sql/least-privilege-read-node.sql`, and logged in once to prove it. A
   login that already works with the stored password is kept.
8. TLS: a self-signed pair (ECDSA P-256, 10 years) whose SANs are `tls.hosts`. A pair
   that already covers the hosts and has more than a year left is kept. Neither PHP
   client verifies it — erp-manager and the B2B backend set `verify_peer` and
   `verify_host` false — so it stops a passive listener, not an active one.
9. The DB secret through DPAPI, read back. Then `config.yaml`.
10. The firewall rule: inbound TCP on `apiListen`'s port, from `allowFrom` only,
    tagged with the group `digi-erp-connector`. A rule of that name without the group
    is the client's and is never changed or removed. If every firewall profile is
    off, the result says the rule filters nothing (`firewall_inert`).
11. The service's dependencies and its recovery (restart after 1, 5 and 30 minutes,
    also on non-crash failures).
12. **Restart, only if needed**: when something the service reads changed, when it runs
    a config older than `config.yaml`, or when it is not running. `apply` asks the
    order queue again right before the stop. **The order counter moves with
    `sendOrderDir`**: `lastOrderNumber.json` is copied to the new folder while the
    service is stopped. A folder without it would start again at 1 and hand
    Hasavshevet numbers it already has. If both folders hold different counters,
    `apply` refuses (`order_number_conflict`). A rollback or a `restore` that returns to
    the older folder carries the higher counter back into it the same way: the copy
    left there is stale.
13. **Port proof**: within 30 s the connector must log in with the new credential and
    answer `/api/ping`. `/api/health` is reported. If the proof fails, this run's
    pre-image is put back, and the service ends as it was before the run: running on
    the restored config, or stopped (`rolledBack`).

A second identical `apply` changes nothing: no write, no new password, no restart.

## `options`: settings on a configured connector

`apply` always restates the credential (`auth.password`), so a caller that does not hold
the one in use would replace it and cut off whoever logs in with it now. `options`
takes the `options` block alone, with the same bounds as the spec's, and carries no
credential: an `auth` key on its stdin is refused as an unknown field.

In order: the input's checks (at least one option, the bounds, the change set), the
drift guard (`expect.configSha256`), the daemon's own startup check on the result, the
image folders it names must exist (`image_folder_missing`); then, only when
`config.yaml` would change, the order queue (`orders_pending`), the change set's
pre-image, the write, and the restart with the port proved by the box's own
credential. A failure after the pre-image puts it back. `restore <changeSet>` undoes
it. Unchanged values write nothing, and restart only a running daemon older than the file
(a hand edit it never loaded) when it runs; a stopped service stays stopped. `--check` stops before the first write and reports
`check.config` and `check.restart`. No `config.yaml` is `config_refused`: run
`apply` first.

## `auth`: the API credential alone

`apply` restates the database credential with every run, so a caller that holds none — digi-mcp setting the password
it generated for erp-manager (G-127) — could not change the API credential without it. `auth` takes
`auth.password` (16–256 characters of plain text, no edge spaces: apply's guard, `auth_password_weak`), optionally
`auth.username` (left out keeps the box's) and `rotateSecret` (a new signing secret, the GUI's *Regenerate*: every
token issued until now stops working and callers log in again on their next 401). A `db` key is refused as an
unknown field.

In order: the input's checks, the drift guard, the daemon's own startup check on the result; then, only when
`config.yaml` would change, the order queue (`orders_pending` / `pending_unknown`, `assumeIdle`), the change set's
pre-image of **`config.yaml` alone**, the write, the restart, and the proof with the **new** credential:
`/auth/token`, `/api/ping`, `/api/health` (not asked on a box without a database). A failed proof puts `config.yaml`
back and the service as it was (`rolledBack`); `restore <changeSet>` undoes a success. The same credential again
writes nothing and does not restart, but proves the credential on the running daemon (or restarts a daemon older
than the file); a stopped service stays stopped. The password never appears in the output.

## `db`: the database alone

```jsonc
{
  "version": 1, "changeSet": "cs-bfl-db-0001",
  "db": {
    "host": "127.0.0.1\\WIZSOFT2017", "port": 0, "database": "WIZDB",
    "leastPrivilege": true,               // the connector's own login, erp_connector_… (login, rotateLogin as in apply)
    "encrypt": true, "trustServerCertificate": true   // or "trust": the same; left out keeps the box's
  },
  "admin": { "user": "sa", "password": "…" }   // only to create or rotate the login and install the procedures; never stored
}
```

The API credential is not restated (an `auth` key is an unknown field). With `leastPrivilege`, a login that works
with a password this box knows is **kept without any administrator** — a host, port or encryption change needs no
`sa`; `admin` (or `db.user`/`db.password`, apply's convention — not both) is needed only to create or take over the
login (`least_privilege_failed` otherwise) and installs the Hasavshevet procedures when it comes. Without
`leastPrivilege` (WinRM only: behind the link `sa_link_mode`) the connector logs in as `db.user` with `db.password`,
or with the stored password when it is left out and `db.user` is the login in use; `sa` needs `allowSa`. `"db":
{"mode": "none"}` turns the database off.

Guarantees as `apply`'s: every check before the first write (the database host on this machine or its LAN, the
order queue, the login opened), the change set's pre-image of `config.yaml` and the DB secret (a least-privilege
login's own key never goes back), then the login, the secret, `config.yaml`, the restart and the proof **including
`/api/health`**: a connector that answers but not its database after the change is `port_proof_failed`, and the
pre-image goes back. Nothing to change writes nothing and proves the running daemon.

## A box without a database (`"db": {"mode": "none"}`)

A write node that only sends orders (BFL's DIGI) has no SQL Server of its own. `apply`, `db`, `discover` and
`verify` take `"db": {"mode": "none"}`; any other `db` field beside it is refused. `apply` writes a `config.yaml`
that names no database, opens nothing, and skips the login, the procedures and the DB secret (left as it is);
behind the link it needs no `leastPrivilege`. The proof does not ask `/api/health` (`proof.health`: `no_database`):
its 503 there is by design. `discover` suggests `dbMode: none` when asked, or when `config.yaml` names no database
(the note `no_database`). `verify` with `"db": {"mode": "none"}` passes its `database` check when `config.yaml`
names none, and fails it when it names one; without it, a config with no database fails that check and says so.

## `service`: dependencies and recovery alone

`dependOn` is the whole list of SQL Server services the daemon depends on (`MSSQLSERVER` or `MSSQL$<instance>`, at
most 8; `[]` removes them all; left out keeps the list); `recovery: true` sets the restart-on-failure actions. Every
dependency must be a service installed on this machine (`dependency_missing`) and not Disabled
(`dependency_disabled`): either would stop the daemon starting at the next start or boot, which this command never
sees. One that is installed but not running (a Manual instance, stopped) is accepted with a warning: the service
manager starts it with the connector. The change set's pre-image keeps the dependencies before (`restore` puts them
back); the recovery actions are not in a pre-image. No restart: the service manager reads both at the next start.

## `firewall-remove`: closing the old inbound rules

Once a box is on the link nothing listens on the network, and `apply` refuses a `firewall` block there. This
command removes the named inbound rules (1–16) that **this tool made** — the connector's group; a rule of that name
that is not is refused for the whole run (`firewall_foreign`), and so is one of ours that `restore` could not
re-create as it is — anything but one inbound TCP allow rule on one port: a Block rule, several rules of one name, UDP,
a port range (`firewall_unrestorable`; remove it by hand). A name with no rule is already gone. Every rule
is read before the first removal; the change set's pre-image keeps each removed rule, `restore <changeSet>`
re-creates them, and a removal failing half way puts back what the run removed. No restart. `--check` reports
`check.firewall` (`remove` / `keep`) and `check.rules`.

## `daemon`: restart, stop, start

`daemon restart|stop|start [--assume-idle]` is what a person does in services.msc after a hand edit or before a
reboot. A stop or a restart of a running service asks the order queue first (it is in memory: a restart drops it;
`--assume-idle` goes on when it cannot be read, and is the only way to stop a service whose `config.yaml` does not
read). A start or a restart first checks `config.yaml` with the daemon's own startup check, so a running service is
never stopped onto a config it would refuse (`config_refused`). `start` and `restart` end with the proof with
`config.yaml`'s credential (`/auth/token`, `/api/ping`, `/api/health`); a start of a running service only proves it,
a restart of a stopped one starts it. Nothing is written, so there is no pre-image and nothing to roll back: a
failed proof is reported (`port_proof_failed`) and the service is left as the action left it. It takes the run lock
(`busy` while an `apply` runs).

## `receipts`, `orders`: those settings alone

Each field left out keeps the box's; both run `options`' flow (the pre-image of `config.yaml`, the order queue, the
restart only on change, the proof with the box's own credential, the rollback). Neither carries a credential.

- **`receipts`**: `sendReceiptDir` (`""` turns receipts off), model (b)'s `hasReceiptBatFile` or
  `hasReceiptParamFile` (`""` clears one), `receiptCardNames` (the whole block; `{}` clears it). Refused: a folder or
  file that is not on the machine (`receipt_dir_missing`, `importer_missing`), a runner others can change
  (`importer_writable`; `allowWritable` over WinRM only — it runs as LocalSystem once per receipt), a receipt folder
  others can write to (`folder_writable`, plan 087 review M1 — the same `allowWritable` over WinRM), anything the
  daemon's own receipt guard refuses (`config_refused`: the order folder, both runners). Behind the link: plain local
  paths outside the system folders, a `.bat`/`.cmd` only, never `allowWritable`.
- **`orders`**: `sendOrderDir` and `importer` with `apply`'s checks (the importer stated, its files present and
  administrators-only, the counter carried to a new folder at the restart, two counters refused), and model (a)'s
  `sendOrderPickupDir` (`""` off) and `sendOrderFormat` (`official` / `legacy`; required with a pickup folder —
  the daemon's `ValidateOrders`). `sendOrderDir` and the pickup folder must be ones only the administrators can write
  to (`folder_writable`, plan 087 review M1; a top-level `allowWritable` accepts them over WinRM — the importer keeps
  `importer.allowWritable`). Behind the link: `apply`'s importer rules, folders outside the system folders, never
  `allowWritable`.

**The scope of `folder_writable`** (review R1a): `orders`, `receipts` and `documents` refuse only a folder the
command *adds, moves* (a new path), or newly makes typed or bound. Every other folder the box writes documents into
that others can write to is reported — a `folder_writable:` warning and an entry in the result's `writableFolders`
(`{name, path, writers, …}`, as `state`'s) — never refused, as the daemon treats it: a box upgraded with folders never
locked down keeps taking these commands. `apply` only reports. Behind the link a scheduler account that must write
there is trusted by name (`documents`' `trustedWriters`), or the folder is locked down by `folder-acl`
([troubleshooting.md § A folder others can write to](troubleshooting.md#a-folder-others-can-write-to)). **Not where an
importer runs** (review T1): in `sendOrderDir` when the connector runs an order importer, in the receipt folder when it
runs a receipt importer, and in the folders of has.exe, the BATs and the parameter files, a trusted writer counts like
anyone else — it could plant what the importer loads or runs as LocalSystem. `state`'s `writableFolders` marks such a
folder `executes: true`.

## `receipt-seed <N>`

What `cutover-seed -receipt-seed N` does — the installer does not ship `cutover-seed`: creates
`receipts\receiptNumber.json` with N (1–999999999) as the number the **next** receipt gets. Write-if-absent only: an
existing store, valid or damaged, is never touched (`receipt_store_exists`), so a seed can neither lower the
numbering nor hide a damaged file; a seed at or below a receipt already written (`history\receipts\<n>`) is refused
(`receipt_seed_low`). No pre-image (there was no file) and no restart: the receipt writer reads the store on every
request.

## `counters`: named counters (plan 087, DIG-1563)

The only way a counter is created, given a range, moved or retired — signed, run by digi-mcp over the link (approval
form, audit, pre-image). The data path only reads counters (`GET /api/counters`): the B2B shop holds the same
credential, and one bad "start from max" would block every order for good. Not sealed: no secret.

```json
{"version":1,"changeSet":"cs-bfl-ctr-01","op":"create","key":"b2b","kind":"orders",
 "min":1000000,"max":1999999,"store":"lastOrderNumber"}
{"version":1,"changeSet":"cs-bfl-ctr-02","op":"create","key":"b2b_test","kind":"orders",
 "min":9000000,"max":9099999,"test":true}
{"version":1,"changeSet":"cs-bfl-ctr-03","op":"range","key":"b2b","min":1000000,"max":2999999}
{"version":1,"changeSet":"cs-bfl-ctr-04","op":"start-from","key":"b2b","from":1000500,"expectLast":1000295}
{"version":1,"changeSet":"cs-bfl-ctr-05","op":"retire","key":"b2b_test"}
```

Each op takes exactly its fields (another one is `spec_invalid`): `create` kind, min, max, test?, store?, from?;
`range` min, max; `start-from` from, expectLast; `retire` nothing — and every op `acceptUnreadable?`. Every op scans
the kind's index; an entry it cannot read refuses (`index_unreadable`, review m8: its number would not be counted in
the floor, the range checks or the in-flight count, so it could be issued again) unless `acceptUnreadable` is exactly
the number of such entries a human checked (another number refuses too; then a warning) — [one total per
command](#acceptunreadable-one-total-per-command). Keys `[a-z][a-z0-9_]{0,31}` and never a Windows
device name (`con`, `prn`, `aux`, `nul`, `com0`–`com9`, `lpt0`–`lpt9`: a store `aux.json` names a device; folder keys
too); `1 ≤ min ≤ max ≤ 999 999 999`.

- **`create`** adds `documents.counters.<key>` to `config.yaml`. `store` adopts one of today's stores **in place**:
  `lastOrderNumber` (`sendOrderDir\lastOrderNumber.json`, kind orders) or `receiptNumber`
  (`DataDir\receipts\receiptNumber.json`, kind receipts); otherwise the counter has a store of its own,
  `DataDir\counters\<key>.json` `{"last": N}`. `lastOrderNumber` needs orders configured (an absolute
  `sendOrderDir`; otherwise `counter_store` — its store would have no folder, and a store path is never relative:
  such a counter fails closed). Within a kind, today's store is adopted **before any other counter of
  that kind** (the daemon's own check, `config_refused`): the order counter numbers the orders sent without a
  documentRule. A kind with no configured counter keeps today's store under its implicit key (`orders` /
  `receipts`, range 1–999 999 999), so a box writing both adopts one store, then the other, in either order. The
  keys `orders` and `receipts` are for a counter of that kind only. The store is written while the service
  is stopped (after `config.yaml`, before the start) — only when it is missing or below the start: `from` is the
  next number, by default the lowest one above `min`, the adopted store's last and every number already issued in
  the range (order or receipt index, and history, scanned). `from` at or below one of those is `counter_lower`, past
  `max` `counter_range`; a store already above `max` is `counter_range`.
- **`range`** sets min/max: the counter's last stays inside (`last ≥ min-1`, `last ≤ max`; otherwise
  `counter_range` — move it with `start-from` first), no number already issued in the old range is above the new
  max, and no unfinished entry of the old range — an order reserved (unknown) or waiting for a resend, a receipt
  reserved — is left outside it (`counter_in_flight`, review m4: a receipt reserved at the store's last number would
  otherwise be answered 503 and never finished).
- **`start-from`** moves the store **forward only**: `from` is the number the next document gets, inside the range,
  above the last and above every number issued in the range (`counter_lower`, `counter_range`); `expectLast` is the
  last number as the plan read it (`-1` = the store is missing; a missing store is then created): another value on
  the box is `counter_moved`. With the service running: the queue is asked (`orders_pending`), the service stopped,
  the store read again (`counter_moved` if an order slipped in), set — a receipt counter only under the receipt
  writer's OS lock (`DataDir\receipts\writer.lock`), so a process still alive refuses instead of racing it — the
  service started and proved. No pre-image: **a counter never moves back**, not even by `restore`.
- **`retire`** marks it retired: it issues no new number and its range stays reserved (no other counter may take it).
  Refused while orders (reserved, or waiting for a resend) or receipts (reserved) of its range are unfinished
  (`counter_in_flight`); the daemon refuses retiring the counter of rule-less orders or of receipts.

`create`, `range` and `retire` run the config-command flow (every check first, the pre-image of `config.yaml`, the
queue, the restart, the proof, the rollback). Ranges never overlap on one box — test and production, orders and
receipts, retired ones included (`config_refused`): an order's job id, status, history folder and re-queue check are
keyed by its number alone. `--check` answers `check.counter`: `{key, last, highestIssued, setLast?, inFlight}`.
`restore` of a `create`/`range`/`retire` puts `config.yaml` back and leaves every store where it is, after the same
guards as the command (review m9): a restored range that would leave out an unfinished entry is `counter_in_flight`;
one that would exclude the counter's last number or a number already issued is `counter_range` (it would answer every
document 503 `COUNTER_EXHAUSTED`, and nothing lowers a counter); and a restore that leaves a kind with no counter —
back on today's store — is `counter_range` when that kind's counters issued numbers above the store (its recovery
over 1–999 999 999 would jump into their ranges: retire them instead). `receipt-seed` stays for a box without counters; with counters, `counters create` adopts or creates
the receipt store.

## `documents`: typed folders and the receipts binding (plan 087)

```json
{"version":1,"changeSet":"cs-bfl-doc-01",
 "folders":{
   "herp":          {"kind":"orders","path":"C:\\xampp\\htdocs\\herp","namePattern":"{prefix}IMOVEIN_{n}.doc","tempPrefix":"TMP_","layout":"legacy"},
   "herp_receipts": {"kind":"receipts","path":"C:\\xampp\\htdocs\\herp\\receipts","namePattern":"KUPAIN_{n}.doc"},
   "herp_test":     {"kind":"orders","path":"C:\\xampp\\htdocs\\herp_test","namePattern":"{prefix}IMOVEIN_{n}.doc","tempPrefix":"TMP_","layout":"legacy","test":true}},
 "receipts":{"folder":"herp_receipts","counter":"receipts"}}
```

Each folder named is set whole (an object) or removed (`null`); folders not named are kept.

**`trustedWriters`** (plan 087 review R1b): `"trustedWriters": ["BFL\\wizsched"]` replaces the box's list (`[]` clears it,
absent keeps it) — at most 8 accounts, for the ERP's own importer and scheduler, which must write into the folders
(Masofon renames what it imported). The box resolves each name to a SID at check time: one that does not resolve is
`writer_unknown`; anything but a single user account — a group, an alias, a computer, a well-known SID (Everyone,
Authenticated Users, Users, Guests, INTERACTIVE, NETWORK, ANONYMOUS, Domain Users/Guests/Computers, LOCAL SERVICE,
NETWORK SERVICE) — is `writer_too_broad`. `--check` answers `check.trustedWriters: [{name, sid, resolved, type}]` for
the approval form. Stored in `config.yaml` as `documents.trustedWriters: [{sid, name}]`; the commands, the daemon and
`state` never count those SIDs as writers (`state.writableFolders[].trusted` shows them). It is an ordinary `documents`
change (pre-image, restore), the same over WinRM and behind the link. `receipts.folder` /
`receipts.counter` bind `POST /api/sendReceipt` (a field left out keeps the box's, `""` clears it); the bound
folder's path replaces `sendReceiptDir`, its pattern the receipt file's name (`KUPAIN.prm` keeps its name).

Checked before any write: the name pattern's closed grammar (`{prefix}` optional and first, a stem of ASCII letters,
digits and `_` starting with a letter and ending in a letter or `_` (review m11: `IMOVEIN_1{n}` with n = 23 is `IMOVEIN_123`), `{n}`, `.doc`; no separator, no `..`, no Windows device name — `COM{n}.doc`
is `COM1.doc`), `tempPrefix` exactly with `{prefix}` (orders only), `layout` `official`/`legacy` for orders; the
folder exists (`folder_missing`; the connector never creates it), has no junction, link or mount point anywhere on its
path, its own name included (`folder_reparse`, review T3), is not inside the data directory (`folder_forbidden`), and
behind the link is none of the [document folder places](#where-a-document-folder-may-be) (`folder_forbidden`); and —
when the command adds or moves it, and for the receipt folder a binding newly
names — only the administrators can write to it (`folder_writable`, plan 087 review M1/R1a: `allowWritable` accepts it
over WinRM with a warning; a folder left where it was is reported in `writableFolders`); behind the link a plain local
path outside the system folders (link mode's rules), never `allowWritable`.
The daemon's own check (`config_refused`): never the `sendOrderDir` or a folder holding it; folders pairwise
distinct; a test folder never the pickup or receipt folder, and never inside a production folder (typed, pickup or receipt) nor holding one (review m11); a test counter only into a test folder, and a test receipt
folder only from a test counter (review m10; a documentRule is held to the same at the route); no model (b)
receipt importer with a test receipt folder (it would import test receipts into the books). A folder with
unfinished orders frozen for it (reserved, or waiting for a resend) is not removed and not changed at all — path,
kind, name pattern, temporary prefix, layout or test (`folder_in_flight`, review m5: the pattern decides the "number
taken" scan, the layout the record, the test flag whether production may write there). The receipt binding — the
receipt folder, its name pattern or test flag, the receipt counter — does not change while a receipt is reserved
(`receipts_in_flight`: Recover finishes it into the folder in use, under its pattern; a file already written under
the old name would be written again under the new one). The `receipts` command makes the same check when it moves or
clears `sendReceiptDir`. An index entry these guards cannot read refuses (`index_unreadable`, review R7: an unfinished
one among them would be stranded unseen) unless `acceptUnreadable` is the number of such entries a human checked —
`documents` and `receipts` take it in their input, `restore` as `--accept-unreadable=N` (the link takes it after
`--assume-idle`).

#### `acceptUnreadable`: one total per command

Plan 087 review m3. Every guard a command runs notes the unreadable entries of the index it reads — each index, the
order index and the receipt index, counted **once** (every scan reads it whole, so all see the same entries) — and the
command checks the **total** once, after its guards and before its first write: unless it is 0, `acceptUnreadable` must
be exactly that total (`index_unreadable` otherwise; the refusal's fix names the number, and its message the split,
e.g. `2 order and 1 receipt`). A `restore` that runs both the documents guards and the counters guards — the
implicit-store trap's scan included — takes the one sum (before, each guard wanted its own count, and no value got
through). An accepted total is one warning (`index_unreadable: N index entries could not be read (…) and were
accepted`).

Config-command flow; `restore` puts
`config.yaml` back, after the same in-flight guards (`folder_in_flight`, `receipts_in_flight`; review m9). Never downgrade a box below v1.0.30 with folders or counters in place: an older build ignores
`documents` (and refuses index entries holding a frozen rule), its recovery pushes the production counters into the
test ranges, and its GUI save drops the block —
[troubleshooting.md § A downgrade below v1.0.30](troubleshooting.md#a-downgrade-below-v1030-plan-087).

## `inflight`: resolve one unfinished order or receipt (plan 087 review R6)

Connector v1.0.32; digi-mcp's feature `inflight`. An entry that can only finish after a folder change blocked that
change for good (reviews m5, m9: `documents`, `receipts` and `restore` wait for unfinished entries), and the only way
out was editing the index on the box (troubleshooting.md § The order index). Signed (`inflight`, `inflight-check` over
the link; WinRM too), not sealed.

```json
{"version":1,"changeSet":"cs-bfl-inflight-01","kind":"orders","historyId":"<historyId>","expect":"reserved","resolve":"fail"}
{"version":1,"changeSet":"cs-bfl-inflight-02","kind":"receipts","number":500123,"expect":"reserved","resolve":"done"}
```

- **One entry**: an order by its `historyId`, a receipt by its `number` (`entry_missing`: none). `expect` is the
  entry's state as the check read it — a string, or `{"state", "configSha256"?}` — compare-and-set (`entry_moved`).
  `resolve` is `fail` or `done`. Only an unfinished entry: `reserved` (an order's outcome unknown after a restart, or
  a receipt Recover could not finish) or `retryable` (an order); `written` and `failed` are final (`entry_final`).
- **The candidate files** (`role`): `delivered` — the file the importer reads, in its target folder: an order's frozen
  name in its typed folder, the pickup folder's `IMOVEIN_<n>.doc` / `TMP_IMOVEIN_<n>.doc`, a receipt's file in the
  receipt folder (its pattern); `imported` — the importer's rename of it, `<name>.….MASOFON2.BAK` (model (b)'s
  `IMOVEIN.….MASOFON2.BAK` in `sendOrderDir` only when its bytes are this order's: it names no number); `history` —
  `sendOrderDir\history\<n>\IMOVEIN_<n>.doc` / `.prm`, `DataDir\history\receipts\<n>\KUPAIN_<n>.doc`. A delivered or
  imported file carries `matchesHistory` (its bytes against the history copy's, streamed whatever their size). A place that cannot be looked at (a
  typed folder no longer configured) counts as written.
- **`fail`**: only when none exists — the importer's `*.MASOFON2.BAK` rename included (review m-b: history pruned and
  the file imported is an order the books have) — `may_be_written` otherwise (check Hashavshevet, Asmahta `n`, then
  resolve it as done, or move the file aside after checking it). **Never prune `history\` while an entry is
  unfinished**: model (b)'s renames name no number, so its history copy is the only proof there. An order becomes `failed`; a receipt `failed`, its number void.
- **`done`** (review R6-1): only on proof of **delivery** — a `delivered` or `imported` file with the history copy's
  bytes; a planted file with other bytes proves nothing. A delivered or imported file with **no readable history
  copy** is `history_missing` (re-review MINOR 1): the connector writes the history copy first, so without it nothing
  shows the file is the connector's — it may be planted, or the legacy app's under the same number in a shared folder;
  check Hashavshevet and the folder by hand. The history copy
  alone is `history_only`: every path writes it BEFORE the document reaches Hashavshevet (model (a) and typed folders
  publish after it; model (b) runs has.exe after it; a receipt's `.prm` and `.doc` come after it). A **model (b)**
  order (no rule, no pickup folder configured) may be resolved on it only with `acceptHistoryOnly: true` — model (b)
  truly leaves nothing else after a real import (the next order overwrites `IMOVEIN.doc`), so a person confirms
  Hashavshevet has document `n`. **Model (a) and typed-folder orders refuse the flag** (`history_only`, re-review
  MINOR 2): their delivery leaves the file or the importer's rename of it, so the history copy alone says it was never
  delivered — check Hashavshevet and the folder by hand; if the document is missing, put the history copy where the
  importer reads it, and its rename then proves `done`. The model is judged by today's config (the entry does not
  record it): a pickup folder configured means model (a). The check answer names the flag (`acceptHistoryOnly`, `entry.proof:
  "history-only"`) for the approval form. A receipt with only its history copy is never marked written: `history_only`,
  and Recover delivers it (its folder allowed, its counter's range holding it: the next start or a repeat of its key).
  Nothing at all: `not_written`. The entry becomes `written`, naming its files; for a receipt the connector ran no
  importer (said in its warnings). `entry.proof`: `delivered`, `imported`, `history-only` or `none`.
- **Never a number again**: the entry keeps its number, owned by its key; no counter moves; the number still counts
  as issued. The entry records the change set and the time (an order's `error`, a receipt's `warnings`).
- **What the B2B sees** (also in the answer's `b2b`): an order resolved as failed — `GET /api/sendOrder/<n>` 200
  `status: failed`; a resend with its `historyId` 202 `status: failed`, `replay: true`, never written again (a new
  order needs a new `historyId`). Resolved as done — `status: done` with the files; a resend 202 `status: done`,
  `replay: true`. A receipt resolved as failed — a resend with its idempotency key `409 RECEIPT_FAILED` (a new receipt
  needs a new key); as done — 200, replayed, `import: not_run`, the resolution among its warnings. Receipts have no
  status route.
- **Busy**: an order the running connector has queued or is writing is `entry_busy` (asked with
  `GET /api/sendOrder/<n>`), before anything is stopped; a receipt is `entry_busy` while another process holds
  `DataDir\receipts\writer.lock`.
- **The run** asks the queue (`orders_pending` / `pending_unknown`, `assumeIdle`), stops the service, reads the entry
  again (`entry_moved` if it changed), resolves it — a receipt under `writer.lock` — starts the service and proves it,
  as `counters start-from`.
- **A receipt resolved as failed** is the one value a build older than v1.0.32 cannot read (it would refuse every
  receipt): the box first raises `DataDir\format-floor` to `1.0.32`, so no automatic rollback goes below it. Orders, and
  receipts resolved as done, are written in fields v1.0.31 reads: no floor.
- **`--check`** reads and writes nothing: `inflight.entry` = `{kind, key, number, state, counter, folder, rule?,
  reservedAt, ageSeconds, files?, note?, candidates: [{path, role, exists, matchesHistory?, error?}], proof}`; with
  `resolve` it refuses as the run would.
- The answer: `inflight: {entry, resolve?, acceptHistoryOnly?, resolved?, b2b?: {statusPoll, resend}}`.

## `folder-acl`: lock a document folder down (plan 087 review R1c)

digi-mcp locks down a folder the connector writes documents into itself — no human running `icacls` on the box.
Signed (`folder-acl`, `folder-acl-check` over the link; WinRM too), not sealed.

```json
{"version":1,"changeSet":"cs-bfl-acl-01","op":"lockdown","folders":["herp","sendOrderDir"]}
{"version":1,"changeSet":"cs-bfl-acl-02","op":"restore","folder":"herp","preimage":"<the sha256 the lockdown answered>"}
```

- **Named folders only**: a typed folder's key, or the roles `sendOrderDir`, `pickup`, `sendReceiptDir`, resolved from
  `config.yaml` — never a path (`spec_invalid`); one the box does not have is `folder_unknown`. At most 8 per run.
- **Judged where Windows resolves it** (review T3), link or WinRM: a junction, link or mount point anywhere on the
  configured path is `folder_reparse`; a folder in or holding one of the
  [places a document folder may not be](#where-a-document-folder-may-be) is `folder_forbidden`. Once the folder is
  opened by handle, the path the handle resolves to (`GetFinalPathNameByHandle`) is judged the same way and must be the
  configured path itself (`folder_reparse` otherwise); the write re-opens it and refuses a folder that resolves anywhere
  else than the check read. A `restore` is never blocked by these: it puts back only a recorded pre-image, on the
  objects recorded.
- **Never a folder code runs from** (review T1): a folder holding an importer file (has.exe, a BAT, a parameter file)
  or a file Windows runs — PATHEXT's extensions (never fewer than Windows' defaults), `.dll`, `.ps1`, `.scr`, `.cpl`,
  `.ocx`, `.sys`, `.drv`; never `.php` — anywhere beneath is `folder_has_executables`. Where an importer runs or reads
  the document (`sendOrderDir` with has.exe or a BAT, the receipt folder with a receipt importer, the importers'
  folders), `trustedWriters` count like anyone else: the lock-down caps them at read & execute too.
- **A lock-down never grants anything, to anyone, on any entry** (review T2). On the folder and on every entry beneath
  — walked **by handle**, never following anything — it masks the DACL the entry already has: an allow entry, explicit
  or inherited, of any type, for SYSTEM, Administrators or TrustedInstaller stays as it is; one for a `trustedWriters`
  account keeps at most Modify; one for anyone else (CREATOR OWNER and OWNER RIGHTS included) keeps at most read &
  execute — generic rights mapped first, an entry left with nothing beyond SYNCHRONIZE dropped. Conditional, callback
  and object entries keep their condition and type; deny entries stay; flags and order are kept. **No entry is ever
  added**: a trusted writer with no right before keeps none (write it had only through a group is capped with the
  group: `trusted_writer_no_write:` warns, on `--check` too — unless `carryOver`, below), and an account that could
  only add files gains no read.
- **`carryOver: true`** (lockdown only; v1.0.32; digi-mcp's feature `folder_acl_carry_over`): a trusted writer whose
  write the masking takes — it wrote through a group, the WizSched case — gets back, per entry, ONE explicit allow
  entry with exactly what it effectively had there before (AuthZ on the entry's old descriptor:
  `AuthzInitializeContextFromSid` + `AuthzAccessCheck`, its groups resolved) intersected with Modify: its effective
  rights never grow. On a folder the entry is inheritable to files (OI) and folders (CI) only as far as the old DACL
  handed the account at least those rights there; where it handed the account its write down to files but the folder
  alone gave a little more, the rights are narrowed to what was handed down so the entry stays inheritable to files —
  the scheduler must be able to rename or delete what the connector writes there after the lock-down (Modify holds no
  delete-child; review item 5). Where the write was the folder's alone, `carriedOver[].notInheritable` counts the
  folders and `carried_not_inheritable:` warns (a lower bound: an allow that may not apply counts for nothing, a
  deny that may counts); on a file never. It sits after the entry's explicit denies. An entry with a conditional
  entry for the account gets none (`skipped`), and so does one with a deny AuthZ's view cannot account for (review
  C-1: a domain account's context comes from an S4U logon — `AUTHZ_REQUIRE_S4U_LOGON`, failing closed to
  `carry_over_failed`; a local account, for which Windows has no S4U, from the machine's own account database — and
  still holds no logon-type or session SID, and may miss a domain group: a deny for INTERACTIVE, BATCH, SERVICE, a
  logon session, a service SID, or a group not in the context that is neither a builtin alias nor another user, means
  nothing is carried there). An account whose groups cannot be resolved (the domain controller does
  not answer, an error) gets nothing carried over — `carry_over_failed:` warns and `trusted_writer_no_write:` stays —
  and if one resolved for the check no longer does when the run writes, nothing is written (`carry_over_failed`). The
  answer's `carriedOver: [{sid, name, rights, entries, skipped?, notInheritable?}]` per folder; the pre-image and `restore` are
  unchanged (a restore takes the carried entries away with the rest). Without it (the default): today's lock-down.
  The folder's link to its parent is cut (its DACL protected, its inherited entries kept as explicit and masked the
  same way); every other entry keeps its own protection. An owner that is not SYSTEM, an administrator or
  TrustedInstaller is handed to Administrators.
- **The bound**: at most 20 000 entries (the folder included), counted before anything is written: more is
  `folder_too_large` (move old `*.MASOFON2.BAK` files out first). A reparse point or a multiply-linked file anywhere
  beneath is `folder_reparse`: never followed, never written through. No path-based propagation (as LocalSystem a path
  can be redirected). **Folders above are never changed**: who can still rename the folder away through one is
  reported — a `parent_writable:` warning and the folder's `parentWritable` (the lock-down of the folder itself still
  applies).
- **The pre-image**, recorded before the first write: the folder's and every entry's owner and DACL (SDDL) with its file
  ID, in a protected file `DataDir\folder-acl\<sha256>.json` — the sha256 of the file's bytes, answered as the
  folder's `preimage`. Kept on the box.
- **`restore`** takes `folder` and `preimage` and puts back **only** a pre-image this box recorded, for that folder, at
  its configured path, whose bytes still match the sha256 (otherwise `preimage_missing`) — never an SDDL from the input:
  a stolen team key can only tighten a configured folder or put back an ACL it had. Each entry is restored only if it is
  still the same object (file ID) at the same place; the others (gone, replaced, a link now) are counted as `skipped`.
  The folder itself must be the object recorded (`folder_reparse` otherwise).
- **`--check`** reads and writes nothing: per folder `{name, path, entries, entriesChanged, principals: [{sid, name,
  before, after, entries}], owners: [{sid, name, entries}], parentWritable}` — every principal with an allow entry
  **anywhere in the tree**, the strongest rights it has anywhere before and after (`full`, `modify`, `write`, `read`,
  `special`, `none`) and on how many entries they change (review T4), the owners handed to Administrators, and how many
  entries change: the approval form shows exactly who loses write — at BFL, the legacy PHP site's account if it writes
  into the folder.
- **Unfinished work does not hold it back** (v1.0.32, re-review MINOR 3; until then `folder_in_flight`). A lock-down
  and a restore change none of SYSTEM's rights, and the connector writes as SYSTEM, so a pending write is unaffected.
  Unfinished index entries (orders frozen for a typed folder, rule-less orders, reserved receipts) are not in the
  running connector's memory. The one thing the restart afterwards could lose, the queue, is asked first
  (`orders_pending`). The case this unblocks is a receipt reserved for a bound receipt folder the guard refuses
  (writable by others, behind the link): the lock-down goes through, and at the start after it the guard allows the
  folder and Recover delivers the receipt. The command reads no index any more; `acceptUnreadable` is still accepted
  as a count, and accepts nothing. A lock-down without `carryOver` can still take the ERP scheduler's write, and with
  it the import of a document already delivered there (`trusted_writer_no_write` says so). That holds for every later
  document too, and is not a reason to wait.
- **Afterwards** the service is restarted (the queue asked first) and proved: the daemon reads the new ACLs at once.
- The answer: `folderAcl: {op, folders: [{name, path, entries, entriesChanged?, principals?, owners?, carriedOver?,
  parentWritable?, preimage?, written?, restored?, skipped?}]}`. A lock-down that never answered (killed after recording) leaves its
  pre-image listed in `state`'s `folderAcl.preimages` (review T5).

## `restore` and pre-images without files

A pre-image holding no file (`service`, `firewall-remove`) changes nothing the service reads: its `restore` neither
asks the order queue nor restarts, and leaves the DB secret alone. A pre-image with files restarts only when a file
actually changed.

## Refusals

| Code | Meaning |
|---|---|
| `spec_invalid`, `change_set_id` | the input is malformed |
| `loopback_listen` | `apiListen` is loopback: the Hub cannot reach it |
| `sa_login` | the connector would keep `sa`: set `leastPrivilege` or `allowSa` |
| `importer_required` / `importer_missing` | orders are configured but no importer is named / the named file is not on the machine |
| `importer_writable` | someone besides the administrators can change the importer or its folder, or rename a folder above it (DELETE on it, or delete-child on its parent) |
| `db_host_public` | `db.host` is not this machine or its LAN |
| `send_order_dir_missing` | the order folder does not exist |
| `auth_password_weak` | the credential is shorter than 16 characters or not plain text |
| `firewall_any` | `allowFrom` opens the port to more than a /16 |
| `config_refused` | the daemon would refuse to start on the resulting config |
| `drift` | `config.yaml` changed after `discover` |
| `service_missing` | the connector is not installed |
| `orders_pending` / `pending_unknown` | the queue holds orders / cannot be read |
| `db_unreachable` | the database does not answer with the given credential |
| `procedures_failed`, `least_privilege_failed` | the login lacks the rights for that step |
| `secret_roundtrip`, `write_failed` | the secret or a file could not be written |
| `firewall_failed`, `service_failed` | the firewall or the service manager refused |
| `firewall_foreign` | a rule of the spec's name exists and this tool did not make it |
| `firewall_unrestorable` | `firewall-remove`: a rule of ours that is not one inbound TCP allow on one port; restore could not re-create it |
| `order_number_conflict` | the old and the new order folder hold different order counters |
| `busy` | another run (or an upgrade over the link) holds `provision\.lock` |
| `upgrade_running` | an upgrade over the link is pending (its job written, no result yet): its setup stops the services and replaces this program; ask `upgrade-result` |
| `port_proof_failed` | the restarted connector did not answer (for `db`: or its database did not); the pre-image was put back (`daemon`: nothing to put back) |
| `sa_link_mode` | behind the link (below), `allowSa`: the connector never keeps `sa` there |
| `importer_not_erp` | behind the link, an importer that is not `has.exe` or a `.bat`/`.cmd`, or one in a system folder |
| `image_folder_forbidden` | behind the link, an image folder in a system folder or a user's `AppData` |
| `preimage_missing` | `restore` was given an unknown change set, or one that changed nothing |
| `dependency_missing` | `service`: a dependency that is not a service on this machine |
| `dependency_disabled` | `service`: a dependency whose start type is Disabled |
| `receipt_dir_missing` | `receipts`: `sendReceiptDir` is not a folder on this machine |
| `receipt_store_exists`, `receipt_seed_low` | `receipt-seed`: the store exists already / N is at or below a receipt already written |
| `counter_unknown`, `counter_exists`, `counter_retired` | `counters`: no configured counter of that key / the key is taken (create) / the counter is retired |
| `counter_moved` | `counters start-from`: the last number is not `expectLast` (a number was issued in between) |
| `counter_lower` | `counters`: a number at or below one already issued, or below the range — a counter never moves back |
| `counter_range` | `counters`: the range would exclude the counter's last or an issued number, or `from` is outside it |
| `counter_in_flight` | `counters retire`: orders or receipts of its range are not finished; `counters range`: the new range would leave one of them out |
| `counter_store` | `counters`: the counter's store cannot be read; `create`: `store: lastOrderNumber` on a box that writes no orders |
| `index_unreadable` | `counters`: index entries of the kind cannot be read, and `acceptUnreadable` is not their number; `documents`, `receipts`, `restore`, `folder-acl` (review R7): the same, for the order and receipt index entries their guards read (`acceptUnreadable`; `restore --accept-unreadable=N`) — review m3: one total per command, each index counted once |
| `folder_unknown`, `folder_missing`, `folder_forbidden` | `documents`, `folder-acl`: no folder of that key / the path is not a folder here / inside the data directory, not a path link mode takes, or one of the [places a document folder may not be](#where-a-document-folder-may-be) (review T3: typed folders behind the link, every `folder-acl` target, and behind the link the `sendOrderDir`, pickup and receipt folders `apply`, `orders` and `receipts` set or move) |
| `folder_in_flight`, `receipts_in_flight` | `documents`: unfinished orders frozen for the folder / a reserved receipt, and the receipt folder or counter would change |
| `folder_reparse`, `folder_too_large` | `folder-acl`: a reparse point, hard link or replaced entry in the folder (never followed or written through), or — with `documents` and, behind the link, `apply`/`orders`/`receipts` — a junction, link or mount point on the configured path, or a folder whose handle resolves to another path (review T3) / more than 20 000 entries |
| `carry_over_failed` | `folder-acl` with `carryOver` (v1.0.32): a trusted writer the check carried over could not be resolved again before the lock-down wrote anything — nothing written |
| `entry_missing`, `entry_moved`, `entry_final` | `inflight` (review R6): no order with that `historyId` / no receipt with that number / the entry's state is not `expect` / it is written or failed already |
| `entry_busy` | `inflight`: the running connector has the order queued or is writing it; another process holds the receipt data |
| `may_be_written`, `not_written`, `history_only`, `history_missing` | `inflight`: `fail` while a file of the entry exists (or a place cannot be looked at) / `done` with no delivered file (nor its rename) with the history copy's bytes and no history copy / `done` on the history copy alone — a model (b) order: `acceptHistoryOnly` after confirming Hashavshevet has it; model (a), typed folders: by hand; a receipt: never (Recover delivers it) / `done` on a delivered file (or rename) with no readable history copy to show it is the connector's |
| `folder_has_executables` | `folder-acl` (review T1): the folder holds an importer file or a file Windows runs (PATHEXT, `.dll`, `.ps1`, …; never `.php`): a lock-down never tightens a folder code runs from |
| `writer_unknown`, `writer_too_broad` | `documents` `trustedWriters`: a name the box does not resolve to an account / one that is not a single user account (a group, an alias, a computer, a well-known SID) |
| `folder_writable` | `documents`, `receipts`, `orders`: accounts besides the administrators can write to a folder the connector writes documents into (or its writers could not be read); `allowWritable` over WinRM only — [troubleshooting.md](troubleshooting.md#a-folder-others-can-write-to) |
| `unsupported_os` | not Windows |
| `internal_error` | the tool itself failed; run `verify` to see the box's state |

## `logs` and `state` (plan 086 C4)

Both are read-only and travel the link signed, not sealed (plan 086 D1), so nothing secret may be in them.

**`logs`** reads only the connector's own files in the data directory — `server.log` (and `server.log.<UTC>.old`,
`server-<UTC>.log`), `link.log` (and `link.log.1`), `ui.log` (and `ui.log.<UTC>.old`), the newest three by time — by
handle (`dataacl.OpenRead`): a reparse point, a multiply-linked file or a file whose final path is outside the data
directory is refused (`log_unreadable`), never followed. `events-daemon` / `events-link` read the Application Event
Log of `digi-erp-connectord` / `digi-erp-connector-link` (Get-WinEvent, fixed script). A line without a time of its
own (a continuation) has the time of the line above it. Every line is redacted before it is measured: every form of
this box's own secrets (`config.yaml`'s API password and signing secret, the database passwords in the secret store
— as given, URL-escaped, JSON-escaped, T-SQL-doubled), `Authorization` and bearer values, JWTs, the value of any key
holding `password`/`passwd`/`pwd`/`secret`/`token`/`api_key`/`pairCode` (`dbPassword`, `DB_PASSWORD`, `X-Api-Key`,
`secretKey` too) in `k=v`, `k: v`, `"k":"v"` and T-SQL `PASSWORD = N'…'` forms — a quoted value to its real end
(`''` and `\"` inside it), an unquoted one to the next `;` or `&` or the end of the line, so a password with spaces
goes whole — sqlcmd's `-P`, a URL's password (up to the last `@`), DPAPI blobs. A line longer than 256 KiB is not
shown at all (it would reach the redaction in pieces). Lines are cut at 4 KiB; at most 64 MiB is read backwards per
answer.

**`state`'s `link.autoUpgrade`** (v1.0.31, plan 088) is managed auto-update: `link.yaml`'s `autoUpgrade` switch as it
reads now, and the link's last view from `link\autoupgrade.json` — the policies it holds, what decides (and a target a
later hold brakes), the next window and check — with the last automatic upgrade's outcome, health and rollback read
afresh from its result ([link.md § Managed auto-update](link.md#managed-auto-update-plan-088-v1031)). Downgrading by
hand: never below `DataDir\format-floor` either — it names the oldest release that reads every file format the builds
on this box wrote (plan 088 D11).

**`state`** writes nothing — no file, no DACL, no database: the daemon is asked through its own read-only routes with
the credential in `config.yaml` (an error says so when the daemon runs another one) — and only when the socket at
`config.yaml`'s address belongs to the service's own process (the TCP table against the service manager's PID);
otherwise it is not asked, and `daemon.error` says `not asked:` and who holds the port. Every run that sends that
credential (the queue checks before writes and restarts, the proofs, `verify`) makes the same check first: a
`config.yaml` written after the daemon's start can name a port a local user holds. `missingKeys` are the keys the
model always writes that `config.yaml` lacks (their defaults apply); `unknownKeys` are keys it does not know (left from
an older build, or misspelt). Index entries are read for their number and state only (a receipt's request never
leaves); each state lists its highest 50 numbers. A part that cannot be read carries its own `error`.
`writableFolders` (plan 087 review M1) lists every folder the connector writes documents into that accounts besides
the administrators can write to: `{name, path, key?, typed?, receipts?, executes?, writers, trusted?, error?}` — `[]`
when there is none (`executes`: an importer runs in or from it, so `trustedWriters` count there too; review T1).
`folderAcl.preimages` (review T5) lists the `folder-acl` pre-images recorded on the box, newest first (at most 50):
`{folder, path, sha256, at, changeSet, entries, error?}` — a lock-down killed before it answered (the link's
deadline, a crash, a reboot) still leaves the sha256 a `restore` takes; `error` marks a file that no longer matches its
sha256 or does not decode (its restore is refused).
`orders.folderTestChanged` (review R2) lists the unfinished orders whose rule froze another test flag than their
folder has now. `counters[].error` is a code (`store_missing`, `store_damaged`, `issued_unreadable`, as `GET /api/counters`) and
`counters[].detail` its text (review m7).

## Behind the link (link mode)

A box whose `link.yaml` (the data directory) names a box or a pairing code dials out to Digitrade's relay through
`digi-erp-connector-link` ([link.md](link.md)). `apply`, `options` and `discover` see it and change four rules —
the WinRM path is unchanged:

- **`apiListen` may be loopback** (`127.0.0.1:8082`): nothing reaches the API from the network, so `loopback_listen`
  is neither a refusal nor a `discover` defect, and a `firewall` block for a loopback listen is refused as pointless.
- **`allowSa` is refused** (`sa_link_mode`): whoever holds the connector's credential can register and run SQL through
  `custom_sql`, so behind the link the connector's own login is never `sa`. `sa` once, for `leastPrivilege`, is fine.
- **The order importer** is Hasavshevet's `has.exe` or a `.bat`/`.cmd`, outside `%SystemRoot%`, `%ProgramFiles%`,
  `%ProgramFiles(x86)%` and `%ProgramData%` (`importer_not_erp`): it runs as LocalSystem once per order.
- **Image folders** are outside those folders and outside any `AppData` (`image_folder_forbidden`): `/api/file` serves
  what is in them.
- **A folder documents are written into** (`documents`, `receipts`, `orders`) is one only the administrators can write
  to when the command adds or moves it (`folder_writable`; never `allowWritable`; review R1a), and the daemon writes into
  no typed folder others can write to (`503 FOLDER_WRITABLE`) — plan 087 review M1. The ERP's own accounts are trusted
  by name (`documents`' `trustedWriters`), and `folder-acl` locks a folder down without a human on the box (R1b, R1c).

Every path a command takes behind the link is a plain path on a local drive with no `.`/`..`/empty part and none of
cmd.exe's characters, and — since the system-folder rules compare text — none Windows resolves to another folder
(plan 087 review m6): no `~` (an 8.3 short name, `C:\PROGRA~1`), no `:` after the drive letter (a stream), no part
ending in `.` or a space (`C:\Windows.\System32` is `C:\Windows\System32`). `apply`'s `sendOrderDir` included
(review R4-1, v1.0.31: before, only `orders`, `receipts` and `documents` took the rule for theirs).

Then the box judges where Windows resolves them (review m6's rest, v1.0.32 — R4-1's and T3's helpers): the image
folders `options` and `apply` set, and the importer files (`has.exe`, the BATs, the parameter files) `apply`, `orders`
and `receipts` set, are refused when a junction, symbolic link or mount point is anywhere on the path, its own name
included, or — once they exist — when the path the handle resolves to (opened without following it) is not the
configured path itself (a SUBST or mapped drive, an 8.3 name, a link above). An image folder is
`image_folder_forbidden`, an importer file `importer_not_erp`. Paths left as they were are not judged again; WinRM is
unchanged.

### Where a document folder may be

Plan 087 review T3. A typed folder (`documents`, behind the link), every `folder-acl` target (link or WinRM), and —
behind the link — the `sendOrderDir`, `sendOrderPickupDir` and `sendReceiptDir` that `apply`, `orders` or `receipts`
set or move, are refused (`folder_forbidden`) when they are, are in, or hold:
- a drive's root;
- the Windows folder (`%SystemRoot%`), `%ProgramFiles%`, `%ProgramFiles(x86)%`, all of `%ProgramData%`;
- the profiles' folder (`%SystemDrive%\Users` and the registry's `ProfilesDirectory`): every profile, `Public`,
  `Default`, `All Users` — and `C:\Documents and Settings`;
- the connector's install folder and its data directory;
- or when any part of the path is `AppData`, `System Volume Information` or `$Recycle.Bin`.

And none has a junction, a symbolic link or a mount point anywhere on its path (`folder_reparse`): `C:\Users\All
Users` is a link to `C:\ProgramData`, which a rule reading only text would never see. A `sendOrderDir`, pickup or
receipt folder that exists already is also judged by the path its handle resolves to (`GetFinalPathNameByHandle`), as
a `folder-acl` target is (review R4-1, v1.0.31): it must be on a local drive, pass the rules above, and be the
configured path itself (`folder_reparse` otherwise — a SUBST or mapped drive, an 8.3 name). `C:\xampp\htdocs\herp`
and `C:\xampp\htdocs\herp\receipts` (BFL) pass. Folders left where they were are not judged again.

These bound what a signed command can make a box do (Digitrade plan 083 §8 A16). Also behind the link: `verify`
takes the loopback `apiListen` (its `listen` check), `discover` suggests `127.0.0.1:<port>`, `db` never keeps a login
the client gave (`sa_link_mode`), and `firewall-remove` is allowed — it is how the old inbound rules close.

`discover`'s suggested `dependOn` (and `verify`'s `dependency` check) names the instance that has the database's
port: a database reached as `127.0.0.1:63501` belongs to `MSSQL$WIZSOFT2017` when that instance has the port, not to
`MSSQLSERVER` (plan 083 §19).

## What it does not do

- **No SQL from the input.** The only values written into SQL are a login name of the
  form `erp_connector_…`, a database name that passed `hasavshevet.IsSafeDBName` and a
  generated password from a quote-free alphabet.
- **It never removes a SQL login.** `restore` puts the old config back, and the login
  stays. A credential only moves forward: when the config put back still uses the
  login, its current password is kept.
- **It never turns a firewall profile on.** That would change the client's whole
  server.
- **`receipt-seed` creates the receipt number store**, as `cutover-seed -receipt-seed N`
  does (hasavshevet-send-receipt.md); it never changes an existing one.

`restore` also works when `config.yaml` no longer reads — that is what it is for — but
then the running connector's order queue cannot be asked, so it needs
`--assume-idle`. It warns when the restored config's database does not answer (the
client may have changed that password since). The service's recovery actions are not
part of a pre-image: `restore` leaves them on.

**`discover` names one database.** With a credential (`{"db":{"user","password","database"}}`) it counts the
databases the login can open (`databaseCount`) and fingerprints only the one named — or, when none is named, the one
`config.yaml` uses. On a shared SQL Server the others are other companies' books: their names never leave the box
(plan 068 D12).

**`apply --check`** runs every check of a real `apply` — the spec, the queue, the database login, the login plan — and
stops before the first write. Its `check` says what would change: `config` (the fields), `login`
(`given`/`keep`/`create`/`rotate`), `dbSecret`, `tls`, `firewall`, `dependencies`, `recovery`, `orderCounter` and
`restart`. digi-mcp's plan shows it, so the box's own decisions and refusals reach the plan. The Hasavshevet
procedures are created where missing at apply; the check does not look.

`discover` also reports what the transport needs to know: `host.winrmAnalytic` (the
note `winrm_analytic_on` — that log records what crosses WinRM, secrets on stdin
included), `host.localAccountTokenFilterPolicy`, and the SQL Server Browser's start
type (`browser_not_automatic` when a named instance reached without a port depends on
it; `verify` checks it too).

## Running it by hand

In an elevated PowerShell, from the install folder:

```powershell
.\digi-erp-connector-provision.exe discover | Out-File discover.json
Get-Content spec.json | .\digi-erp-connector-provision.exe apply --spec -
.\digi-erp-connector-provision.exe verify
```

Delete `spec.json` afterwards: it holds the client's database password.
