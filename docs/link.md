# The link — `digi-erp-connector-link`

The connector's outbound link to Digitrade's relay (Digitrade plan 083). The server needs **only outbound HTTPS (443)
to `link.digitrade-admin.org`**: no inbound port, no port forward, no WinRM, no IP allow-list at the client. The
connector's API can then listen on `127.0.0.1` only.

```
this server                                           Digitrade (Google Cloud)
digi-erp-connector-link ── wss://link.digitrade-admin.org ──► relay ◄── ERP-manager, the shop: /c/<box>/api/...
  ├─ data streams  → 127.0.0.1:<apiListen> (the connector)            ◄── digi-mcp: signed commands
  └─ command streams → digi-erp-connector-provision.exe <fixed command>
```

## What it is

- A Windows service, **`digi-erp-connector-link`**, LocalSystem, automatic start. It is separate from the connector's
  own service so that restarting the connector (after a configuration change or an upgrade) never cuts it.
- It keeps one WebSocket to the relay, with many streams inside it. Its proxy, first found wins: `proxy:` in
  `link.yaml`; `HTTPS_PROXY` in the service's environment; the machine's WinHTTP proxy (`netsh winhttp show proxy`);
  WPAD; else direct. A LocalSystem service does not see a user's own Internet settings (an IE / Edge proxy, a PAC file
  by policy), so the preflight names them when they differ, with the fix (`netsh winhttp import proxy source=ie`, or
  `link.yaml`'s proxy). Only outbound HTTPS (443) to `link.digitrade-admin.org` is needed; nothing inbound. It reconnects by itself, with a pause that grows to a minute; it moves to a fresh connection every ~20
  hours and whenever the relay asks, opening the new one before closing the old.
- Its keys are made on this server and never leave it: an Ed25519 key that proves who the box is and signs every
  answer, and an X25519 key that opens secrets Digitrade seals to it (the SQL password, for instance). Both are in the
  data directory's `secrets\` (DPAPI, machine scope).

## What it runs — and what it never runs

A command reaches this server only as an envelope **signed by Digitrade's team key**, which is compiled into this
release (the relay cannot add or change one). The link checks the signature, the key's generation, that the command
is for this box, that it has not expired and was not seen before, and the digest of its input. Then it runs **only
`digi-erp-connector-provision.exe`**, with one of: `version`, `discover`, `apply` (and `--check`), `options` (and
`--check`), `verify`, `restore <change-set>`; or answers `hashes` itself (the digests of the installed executables).
There is no shell and no other program. Every answer is signed by this server's key.

From v1.0.29 (Digitrade plan 086) also, each a fixed `provision` command line
([provisioning.md](provisioning.md)):

| Command | Runs | Input |
|---|---|---|
| `auth-check`, `auth` | `auth [--check] --spec -` — the API credential alone | **sealed only** |
| `db-check`, `db` | `db [--check] --spec -` — the database login and settings alone | **sealed only** |
| `service-check`, `service` | `service [--check] --spec -` — the service's SQL Server dependencies and recovery | JSON |
| `firewall-remove-check`, `firewall-remove` | `firewall-remove [--check] --spec -` — the connector's own inbound rules | JSON |
| `receipts-check`, `receipts` | `receipts [--check] --spec -` — the receipt settings | JSON |
| `orders-check`, `orders` | `orders [--check] --spec -` — the order settings, model (a) included | JSON |
| `receipt-seed` | `receipt-seed <N>`, one argument N (1–9 digits) — the receipt number store, write-if-absent | none |
| `daemon-restart`, `daemon-stop`, `daemon-start` | `daemon <action>`, optional argument `--assume-idle` | none |

A command whose input carries a password is refused `input_not_sealed`, and never run, unless its input is sealed to
this server's key: `auth`, `db` (**sealed only** above), `apply` and `apply-check` always, and any other command —
`discover` with `db.password`, say — whenever its input holds a password field (`password`, `passwd`, `pwd`, `secret`
in any key, at any depth, with a value). The refusal guards against a mistake on the sender's side, nothing more: an
unsealed input has already crossed the relay when this server refuses it. Keeping the relay from reading a password is
the sender's job — digi-mcp seals the input of every command it sends.

Since v1.0.29 (Digitrade plan 086) the set also has, run by `provision` the same way: **`logs`** (the tail of
`server.log`, `link.log`, `ui.log` or either service's Event Log, redacted on this server before it is sent) and
**`state`** (read-only diagnostics). And, answered by the link itself: **`link-status`**, **`upgrade`** and
**`upgrade-result`** (below). See [§ Plan 086 commands](#plan-086-commands-v1029).

## For the client's IT

- **State:** `link.yaml` in `%ProgramData%\digi-erp-connector\` — the relay, this box's id once paired, the proxy.
- **Upgrades over the link** leave their trace in `upgrade\<ticket>\` (`job.json`, `runner.log`, `setup.log`) and
  `results\upgrade-<ticket>.json` in the same folder, and in the Event Log. `admin: false` refuses them like every
  other command.
- **Turn remote commands off:** set `admin: false` in `link.yaml`. Data keeps flowing (the shop's sync and orders);
  every command is refused and logged.
- **Every command** is logged to `link.log` (same folder) and to the Windows **Event Log** (Application, source
  `digi-erp-connector-link`): what ran, for which change set, by whom, and how it ended.
- **Remove it:** uninstall the connector, or stop and delete the service; its key goes with the data directory.

## Setup: the preflight, then the code

`setup.exe` checks the server before it installs anything (from a temporary copy of the link), one line each with the
fix: administrator, Windows version, the existing install, the proxy the service will use, DNS, outbound 443 (or the
proxy), HTTPS to the relay, whether TLS is inspected on the way, the clock (more than 2 minutes off is a stop: signed
commands would expire), and the WebSocket itself. A STOP asks "Install anyway?" (the connector works without the link);
a silent install logs it to setup's log. The same check by hand, any time, writing nothing:

```powershell
& 'C:\Program Files\digi-erp-connector\digi-erp-connector-link.exe' preflight   # exit 0 ready, 3 warnings, 1 a STOP
```

Then the code: on setup's "Connect to Digitrade" page, or `setup.exe /VERYSILENT /PAIRCODE=ABCDE-FGHJK` for the
client's IT. Setup never asks for a SQL password, opens a port, touches the firewall or turns WinRM on.

## Pairing (once)

Digitrade gives a code (`ABCDE-FGHJK`, single use, 30 minutes):

```powershell
& 'C:\Program Files\digi-erp-connector\digi-erp-connector-link.exe' pair ABCDE-FGHJK
```

It prints a **fingerprint**; read it to Digitrade, who confirm it before anything else happens. `status` shows the
relay, the box id and whether commands are on; `fingerprint` prints it again.

## Plan 086 commands (v1.0.29)

| command | stdin | answer |
|---|---|---|
| `logs` | `{"source":"server\|link\|ui\|events-daemon\|events-link","since"?:RFC3339,"maxLines"?:≤2000,"grep"?:"literal"}` | `provision logs`: `{"source","lines":[…oldest first],"truncated","files"}` |
| `state` | `{}` | `provision state`: services, the daemon's queue and health, config.yaml against the service start and the model, order and receipt numbers and indexes, the pickup folder, TLS expiry, `link.yaml` (no secrets), free disk |
| `link-status` | none | the relay, box, `admin`, the proxy (host only) and where it came from, the team-key epoch floor, the live connection, the last connect error, the daemon address and TLS piped to, uptime |
| `upgrade` | `{"version":"1.0.30","sha256":"<64 hex>","assumeIdle"?:bool}` | `{"accepted":true,"ticket","from","to"}` at once, or `{"accepted":false,"refusal":{code,message,fix}}` (exit 1) |
| `upgrade-result` | `{"ticket":"…"}` | the result file, `{"ticket","pending":true}`, or `{"ticket","pending":false,"lost":true,"error"}` |

**`logs` and `state` cross the relay signed, not sealed** (plan 086 D1): the same trust as the data path. Nothing
secret leaves this server: every log line is redacted here first (this box's own API password, signing secret and
database passwords in every form — URL-, JSON- and T-SQL-escaped included — `Authorization`/bearer values, JWTs, the
value of any `…password…`/`…secret…`/`…token…`/`…api-key…` key to its end, a connection string's `Password=…;`,
sqlcmd's `-P`, a URL's password, DPAPI blobs; a line over 256 KiB is not shown), and `state` reports no secret at
all. Logs are read
by handle only: a link, a hard link or a file outside the data directory in a log's place is refused, never followed.

**The data path follows the daemon.** The link reads the daemon's address, TLS and certificate from `config.yaml`
again whenever it (or the certificate) changes, so an `apply` that moves the port or turns TLS on reaches the data
path at the next call — no restart of the link. Until the daemon restarts on a new port, the previous address is
tried second — for at most 3 minutes after the change, only until the new address has answered once, and never a
plaintext address behind a TLS one. A certificate that cannot be read serves no data stream at all, to no address. On
Windows a stream is piped only to a socket owned by the connector's own service (`digi-erp-connectord`'s process, from
the TCP table): a local user who takes the port while the daemon restarts, or a port left behind, gets nothing — not
erp-manager's `/auth/token` login. The link also starts on a server with no `config.yaml` yet: commands work, data
waits for one.

### Upgrade over the link

`upgrade` installs **only an official release of this repository**, and only a newer one (plan 086 D2): the link
downloads `SHA256SUMS` and `digi-erp-connector-setup-<version>.exe` itself, from
`https://github.com/digitrade-e/digi-erp-connector/releases/download/v<version>/` only (compiled in), through its own
proxy, and runs the setup only when its SHA-256 equals **both** the digest in the signed envelope and the release's
`SHA256SUMS` line. It refuses while the connector holds queued orders (unless `assumeIdle`), a build that is not a
release (`dev`), and a server whose pickup folder (DIG-1297) the target would ignore. Every check runs before it
answers `accepted`; a refused upgrade leaves nothing behind.

It never runs beside another command or a provisioning run: `busy` while the other command slot is in use or a run
(over the link or WinRM) holds `DataDir\provision\.lock`. The link takes that lock for its checks and the download, and
the runner holds it from before the setup until the services run again and the result is written. While a ticket is
pending every provision command is refused `upgrade_running` — by the link, and by `provision` itself for a WinRM run
— while the link's own commands (`upgrade-result`, `link-status`, `hashes`) still answer. The lock never holds up the
link's own stop: it is released when the link process ends.

The setup stops the link itself, so the link hands over to a **runner**: a copy of `digi-erp-connector-link.exe` in
`%ProgramData%\digi-erp-connector\upgrade\<ticket>\`, started detached (it outlives the link and is outside the
install folder the setup replaces). It waits 10 s for the answer to leave, checks the setup's digest again, runs it
`/VERYSILENT /SUPPRESSMSGBOXES /NORESTART /SP- /LOG=…`, then puts the services back as they were: the link
running, the connector running if it ran before (started again when a failed setup left it stopped) — and a connector
that was stopped before the upgrade stopped again, since the setup starts it whatever it was. A service that is not as
it was makes the result an error, whatever the setup's exit code. It then reads the installed build and writes
`%ProgramData%\digi-erp-connector\results\upgrade-<ticket>.json`:
`{ticket, from, to, exitCode, startedAt, finishedAt, error?, installedVersion, services, setupLogTail?}`. The ticket is
the first 32 hex digits of the SHA-256 of the envelope's nonce. The ticket's folder keeps `job.json`, `runner.log`
and the setup's `setup.log`; the setup itself is deleted after, and the oldest finished tickets are removed (ten
kept). Each step is also in the Event Log (Application, source `digi-erp-connector-link`).

A release from v1.0.29 on lists its setup in `SHA256SUMS` (release-windows.yml); v1.0.29 is the last version that must
be installed by hand on a v1.0.28 box.

## Behind the link, `provision` is stricter

See [provisioning.md § Behind the link](provisioning.md#behind-the-link-link-mode).
