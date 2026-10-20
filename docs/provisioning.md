# Provisioning without the GUI — `digi-erp-connector-provision.exe`

The installer puts `digi-erp-connector-provision.exe` next to the daemon. It does
what an operator does in the GUI and around it, without a desktop: it reads the
machine, writes the configuration, installs the Hasavshevet procedures, can move
the connector off `sa`, opens the port in the Windows firewall, hardens the
service, restarts it and proves it answers.

digi-mcp runs it over WinRM with fixed command lines (Digitrade plan 068). A human
can run it the same way in an elevated PowerShell. The code is
`internal/provision`; `cmd/digi-erp-connector-provision` is only the entry point.

## Commands

| Command | Input (stdin) | What it does | Writes |
|---|---|---|---|
| `discover [--spec -]` | optional `{"db":{"user","password"[,"host","port"]}}` | Reads the machine: SQL Server instances, their ports and login mode, the connector's service and `config.yaml` (secrets shown as `set`/`missing`), the listeners, the firewall, has.exe candidates and, with a credential, which databases look like Hasavshevet or SAP B1. Names the defects of a hand install and suggests a spec | nothing |
| `apply --spec -` | the spec (below) | Makes the connector match the spec | see below |
| `verify [--spec -]` | optional `{"expectVersion","firewallRule"}` | Checks the running connector against `config.yaml`: service, login, `/api/ping`, `/api/health`, the order queue, recovery, the SQL dependency, the firewall rule | nothing |
| `restore <changeSet> [--assume-idle]` | none | Puts a change set's pre-image back and restarts on it | the pre-image's files, firewall rule, dependencies |
| `version` | none | Prints the build's version | nothing |

**Secrets come on stdin only**, never on the command line. The output is exactly one
JSON object, `{"command","ok","tool","refusal"?,"result"?}`. Every byte printed goes
through a redaction pass over the secrets in the input and the passwords born during
the run. Exit codes: 0 done, 1 refused (the `refusal` says why, with a `fix`), 2 usage.

## The spec

```jsonc
{
  "version": 1,
  "changeSet": "cs-erp01-0001",          // names the pre-image; 8–64 of A–Z a–z 0–9 _ -
  "erp": "hasavshevet",                   // or "sap"
  "apiListen": "0.0.0.0:8082",            // an IP literal, never loopback
  "tls": { "mode": "self-signed", "hosts": ["203.0.113.58"] },   // optional; or {"mode":"off"}
  "db": {
    "host": "127.0.0.1\\WIZSOFT",         // host, or host\instance (port 0 = ask the SQL Server Browser)
    "port": 0,
    "database": "erp_db",
    "user": "sa", "password": "…",        // the credential the client gave
    "leastPrivilege": true,               // create erp_connector_read; sa is used once and not stored
    "allowSa": false                      // keep sa as the connector's login (refused otherwise)
  },
  "sendOrderDir": "C:\\digiorders",
  "importer": { "mode": "none" },         // or {"mode":"hasExe","hasExePath","hasParamFile"} or {"mode":"bat","hasBatFile"}
  "auth": { "username": "digi-hub", "password": "…" },     // the one credential; at least 16 characters
  "firewall": { "allowFrom": ["198.51.100.241"] },          // optional; ruleName defaults to digi-erp-connector
  "service": { "dependOn": ["MSSQL$WIZSOFT"] },             // optional
  "assumeIdle": false,                    // restart even when the order queue cannot be read
  "expect": { "configSha256": "…" }       // optional: refuse if config.yaml changed since discover
}
```

A field that is left out keeps what the machine has: a missing `tls`, `firewall` or
`service` means "leave it alone", not "remove it". Everything not in the spec —
receipts, image folders, query limits, `erpUser`, the token lifetime, the signing
secret — stays as it was.

## What `apply` does, in order

Nothing is written before every check has passed:

1. The whole spec is valid; the resulting config passes the daemon's own startup
   checks (`api.CheckConfig`); the order folder and the importer files exist;
   `config.yaml` is what `expect` says.
2. **The order queue is empty.** The queue lives in memory, so a restart would lose
   accepted orders. `apply` logs in to the running connector with the credential in
   `config.yaml` and reads `GET /api/sendOrder`. If it cannot read the queue (a
   connector older than that route, or one still running a config whose credential
   is gone), it refuses with `pending_unknown` unless `assumeIdle` is set.
3. The database answers with the given credential.

Then:

4. **Pre-image** to `provision\<changeSet>\`: `config.yaml`, the DB secrets, the TLS
   pair, the firewall rule, the service's dependencies. The first capture of a
   change set wins, so re-running it never overwrites the state from before it began.
   The last five pre-images are kept. They hold `config.yaml`, credential in clear,
   under the data directory's DACL.
5. The data directory gets the daemon's DACL.
6. Hasavshevet: `GPRICE_Bulk` and `GetOnHandStockForSkus` are created where missing.
7. `leastPrivilege`: `erp_connector_read` is created or given a new password (born
   here, kept only in DPAPI and SQL Server), with the grants of
   `scripts/sql/least-privilege-read-node.sql`, and logged in once to prove it. A
   login that already works with the stored password is kept.
8. TLS: a self-signed pair (ECDSA P-256, 10 years) whose SANs are `tls.hosts`.
   erp-manager checks the host name, so the address in its `baseUrl` must be one of
   them. A pair that already covers the hosts and has more than a year left is kept.
9. The DB secret through DPAPI, read back. Then `config.yaml`.
10. The firewall rule: inbound TCP on `apiListen`'s port, from `allowFrom` only. If every
    firewall profile is off, the result says the rule filters nothing (`firewall_inert`).
11. The service's dependencies and its recovery (restart after 1, 5 and 30 minutes,
    also on non-crash failures).
12. **Restart, only if needed**: when something the service reads changed, when it runs
    a config older than `config.yaml`, or when it is not running. `apply` asks the
    order queue again right before the stop.
13. **Port proof**: within 30 s the connector must log in with the new credential and
    answer `/api/ping`. `/api/health` is reported. If the proof fails, everything
    from step 4 is put back and the service is started on it again (`rolledBack`).

A second identical `apply` changes nothing: no write, no new password, no restart.

## Refusals

| Code | Meaning |
|---|---|
| `spec_invalid`, `change_set_id` | the input is malformed |
| `loopback_listen` | `apiListen` is loopback: the Hub cannot reach it |
| `sa_login` | the connector would keep `sa`: set `leastPrivilege` or `allowSa` |
| `importer_required` / `importer_missing` | orders are configured but no importer is named / the named file is not on the machine |
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
| `port_proof_failed` | the restarted connector did not answer; the pre-image was put back |
| `preimage_missing` | `restore` was given an unknown change set |
| `unsupported_os` | not Windows |

## What it does not do

- **No SQL from the input.** The only values written into SQL are a database name
  that passed `hasavshevet.IsSafeDBName` and a generated password from a quote-free
  alphabet.
- **It never removes a SQL login.** `restore` puts the old config back, which stops
  using `erp_connector_read`, and the login stays. A credential only moves forward.
- **It never turns a firewall profile on.** That would change the client's whole
  server.
- **It does not replace `cutover-seed` yet.** `cutover-seed -receipt-seed N` is still
  the only way to create the receipt number store (hasavshevet-send-receipt.md).

## Running it by hand

In an elevated PowerShell, from the install folder:

```powershell
.\digi-erp-connector-provision.exe discover | Out-File discover.json
Get-Content spec.json | .\digi-erp-connector-provision.exe apply --spec -
.\digi-erp-connector-provision.exe verify
```

Delete `spec.json` afterwards: it holds the client's database password.
