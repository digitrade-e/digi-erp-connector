# Operations

Installing, upgrading, rolling back and verifying a customer machine. Config keys
are in [configuration.md](configuration.md); when something is broken go to
[troubleshooting.md](troubleshooting.md).

For the specific settings of the live production box, see
`../.claude/docs/04-operations.md`.

## Fresh install

1. Download `digi-erp-connector-setup-<version>.exe` from the GitHub release.
2. Run it as administrator. It installs to `C:\Program Files\digi-erp-connector`,
   registers the `digi-erp-connectord` service as auto-start, starts it, and adds
   a desktop shortcut that elevates the GUI via `launch-admin.vbs`.
3. The service will start and immediately stop: there is no config yet
   (`server.log` says *config not found*). Expected.
4. Launch the GUI from the desktop shortcut (it must run elevated). Set the ERP,
   `apiListen`, DB host/port/user/database and the password, click **Generate key**
   for the bearer token, then **שמירה** (Save).
5. Click **Test connection**. For Hasavshevet, saving also installs the
   `GPRICE_Bulk` and `GetOnHandStockForSkus` stored procedures — so the DB user
   needs `CREATE`/`ALTER` rights at least once.
6. Click **Start server**, then verify (below).
7. Give the bearer token to whoever configures the backend.

**Harden the service** (see below) — the installer does not do this, and without
it a reboot can leave the API dead.

### Non-interactive install

For a machine with no desktop session, or to script a rollout, use
`cmd/cutover-seed` instead of steps 4–6:

```powershell
cutover-seed.exe -listen "127.0.0.1:8080" -erp hasavshevet `
  -db-host localhost -db-port 1433 -db-user sa -db-name CUSTOMERDB `
  -db-password "<password>" -encrypt=true -trust=true
```

It writes `config.yaml`, stores the password through DPAPI (verifying the
round-trip), generates a bearer token if none exists, and can import a
`queries.json`. It uses the application's own code paths, so the on-disk formats
cannot drift. Note the password appears in the process list and shell history —
acceptable on a machine where you are already handling the customer's credentials,
not on a shared one.

## Service hardening — do this on every install

The daemon **exits if the database is unreachable at startup**. Without a
dependency on SQL Server and restart-on-failure, a reboot in which SQL Server
becomes ready after the connector leaves the API dead until someone starts it by
hand.

```powershell
# elevated. Adjust the SQL Server service name: Get-Service *MSSQL*
sc.exe config digi-erp-connectord depend= "MSSQL$INSTANCENAME"
sc.exe failure digi-erp-connectord reset= 86400 `
  actions= restart/15000/restart/30000/restart/60000
```

Verify with `sc.exe qc digi-erp-connectord` and `sc.exe qfailure digi-erp-connectord`.

If the backend is remote, also confirm an inbound firewall rule exists for the
port. A rule scoped to a *program* will not cover a replacement binary — prefer a
port rule.

## Upgrading

The installer **does not stop the service**, and a running executable cannot be
overwritten. Stop it first:

```powershell
Stop-Service digi-erp-connectord
Start-Process .\digi-erp-connector-setup-<version>.exe -ArgumentList '/VERYSILENT','/SUPPRESSMSGBOXES','/NORESTART' -Wait
Start-Service digi-erp-connectord     # the installer also tries to start it
```

What survives an upgrade:

- **Everything in the data directory** — config, queries, secrets, logs. The
  installer only writes to `C:\Program Files\…`.
- **The service definition**, including `depend=` and the recovery actions. The
  installer's `sc create` fails harmlessly when the service already exists. Only a
  full *uninstall* removes the service and therefore the hardening.

Re-running the hardening commands afterwards is harmless and worth it as a check.

## Verifying

```powershell
# 1. is it listening on the configured port?
Get-NetTCPConnection -State Listen | Where-Object LocalPort -eq 8080

# 2. does it reach the database?
curl.exe -s http://127.0.0.1:8080/api/health -H "Authorization: Bearer <token>"
# -> {"status":"ok"}

# 3. does a real query run?
curl.exe -s "http://127.0.0.1:8080/api/sqlqueries/<name>" -H "Authorization: Bearer <token>"
```

**Passing JSON bodies from PowerShell 5.1:** it strips double quotes when handing
a string to a native executable, so `curl -d '{"a":1}'` silently sends invalid
JSON. Write the body to a file and use `--data-binary "@file"`.

### Verifying a change did not alter behaviour

When upgrading or refactoring, the strongest check is to compare responses before
and after. Capture every saved query plus a set of endpoint probes, normalise
(sort each row's keys, include the JSON type of each value), deploy, capture
again, and diff. Anything other than a timestamp differing is a regression worth
investigating. This is how each production change on the b4l box was validated —
see `../.claude/docs/03-implementation-log.md`.

## Rolling back

In order of preference:

1. **Re-install the previous release** — but check what changed first. A release
   that predates a feature the backend depends on (for instance the legacy
   compatibility layer) will break it immediately.
2. **Restore the previous binaries** — stop the service, copy the old
   `digi-erp-connectord.exe` / `digi-erp-connector.exe` over the installed ones,
   start it. Keep a copy before every deploy.
3. **Restore config** — `config.yaml`, `queries.json` and `secrets/` are all
   independently restorable; keep a copy of the data directory before any change
   that touches it.

Any deployment script should verify health after the change and roll itself back
automatically if the probe fails, so a failed deploy never leaves the API down.

## Logs

| File | What |
|---|---|
| `%PROGRAMDATA%\digi-erp-connector\server.log` | Daemon: startup sequence, config summary, DB connection, every legacy-compat route hit, errors |
| `%PROGRAMDATA%\digi-erp-connector\ui.log` | GUI: startup, window creation, run loop |

Neither ever contains the bearer token or the DB password. `server.log` is written
by the service (LocalSystem), so a non-elevated process may not be able to append
to it — if the GUI or a tool seems to log nothing, that is usually why.

Useful check — is the backend still using the compatibility layer?

```powershell
Select-String -Path C:\ProgramData\digi-erp-connector\server.log -Pattern 'legacy-compat route used'
```

## Uninstalling

Uninstall from Programs and Features, or run `unins000.exe` in the install
directory. It stops and deletes the service (**and with it the hardening**) and
removes the installed files. It does **not** remove the data directory: config,
queries, secrets and logs remain, so a re-install picks up where it left off.
Delete `%PROGRAMDATA%\digi-erp-connector\` by hand if you want it gone.
