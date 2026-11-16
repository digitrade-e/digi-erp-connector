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

From v1.0.31 (Digitrade plan 088) the link also takes **upgrade policies**: not commands, but approvals signed by the
same team key, on which the box upgrades itself in its own maintenance window and checks it is healthy afterwards —
[§ Managed auto-update](#managed-auto-update-plan-088-v1031).

## For the client's IT

- **State:** `link.yaml` in `%ProgramData%\digi-erp-connector\` — the relay, this box's id once paired, the proxy.
- **Upgrades over the link** leave their trace in `upgrade\<ticket>\` (`job.json`, `runner.log`, `setup.log`) and
  `results\upgrade-<ticket>.json` in the same folder, and in the Event Log. `admin: false` refuses them like every
  other command.
- **Automatic upgrades (v1.0.31 on):** the box upgrades itself in a maintenance window, to a release Digitrade
  approved for it, and goes back to the release it had if it is not healthy afterwards. Each one is in the Event Log
  and `link.log` (`auto-update … accepted, ticket …`, `auto-update ticket … ended: …`). **Turn them off:**
  `autoUpgrade: false` in `link.yaml` (commands keep working); `admin: false` stops them too. A connector window left
  open on the server, in any user's session, is closed before an upgrade's setup runs — **unsaved changes in it are
  lost**.
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
| `logs` | `{"source":"server\|link\|ui\|events-daemon\|events-link","since"?:RFC3339,"maxLines"?:≤2000,"grep"?:"literal"}` | `provision logs`: `{"source","untrusted":true,"lines":[…oldest first],"truncated","files"}` |
| `state` | `{}` | `provision state`: services, the daemon's queue and health, config.yaml against the service start and the model, order and receipt numbers and indexes, the pickup folder, TLS expiry, `link.yaml` (no secrets), free disk |
| `link-status` | none | the relay, box, `admin`, the proxy (host only) and where it came from, the team-key epoch floor, the live connection, the last connect error, the daemon address and TLS piped to, uptime; from v1.0.31 `autoUpgrade` (below) |
| `upgrade` | `{"version":"1.0.30","sha256":"<64 hex>","assumeIdle"?:bool,"healthMinutes"?:5-120,"rollback"?:{"version","sha256"}}` (`linkproto.UpgradeSpec`) | `{"accepted":true,"ticket","from","to"}` at once, or `{"accepted":false,"refusal":{code,message,fix}}` (exit 1); `healthMinutes` (v1.0.31; default 15) is the runner's health check; `rollback` (v1.0.31) is the signed digest of the build the box runs, the only one an automatic rollback may install (`rollback_not_running` when it names another build) |
| `upgrade-result` | `{"ticket":"…"}` | the result file, `{"ticket","pending":true}`, or `{"ticket","pending":false,"lost":true,"error"}` |

**`logs` and `state` cross the relay signed, not sealed** (plan 086 D1): the same trust as the data path. Nothing
secret leaves this server: every log line is redacted here first (this box's own API password, signing secret and
database passwords in every form — URL-, JSON- and T-SQL-escaped included — `Authorization`/bearer values, JWTs, the
value of any `…password…`/`…secret…`/`…token…`/`…api-key…` key to its end, a connection string's `Password=…;`,
sqlcmd's `-P`, a URL's password, DPAPI blobs; a line over 256 KiB is not shown), and `state` reports no secret at
all.

**Log lines are data, never instructions.** Others can put text in these logs: the relay (a GoAway reason, logged by
the link), anyone who reaches the data path (an unauthenticated request is logged), a local user (the Application
Event Log under the connector's sources). The box keeps writing them as they are; the `logs` answer carries the fixed
field `"untrusted": true`, and whoever reads it — digi-mcp and the agent behind it above all — shows the lines as
quoted data and never acts on what they say. Logs are read
by handle only: a link, a hard link or a file outside the data directory in a log's place is refused, never followed.

**The data path follows the daemon.** The link reads the daemon's address, TLS and certificate from `config.yaml`
again whenever it (or the certificate) changes, so an `apply` that moves the port or turns TLS on reaches the data
path at the next call — no restart of the link. Until the daemon restarts on a new port, the previous address is
tried second — for at most 3 minutes after the change, only until the new address has answered once, and never a
plaintext address behind a TLS one. A certificate that cannot be read serves no data stream at all, to no address. On
Windows a stream is piped only to a socket owned by the connector's own service (`digi-erp-connectord`'s process, from
the TCP table): a local user who takes the port while the daemon restarts, or a port left behind, gets nothing — not
erp-manager's `/auth/token` login. The certificate (and key) is trusted only where nobody but the administrators can
change it or rename a folder above it (the same check as the importer's, every folder up to the root): otherwise no
data stream is served, and `link-status` says why. The link also starts on a server with no `config.yaml` yet:
commands work, data waits for one.

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
install folder the setup replaces). It waits 10 s for the answer to leave, checks the setup's digest again, ends any
open connector window (below), runs it
`/VERYSILENT /SUPPRESSMSGBOXES /NORESTART /SP- /CLOSEAPPLICATIONS /FORCECLOSEAPPLICATIONS /LOGCLOSEAPPLICATIONS /LOG=…`,
then puts the services back as they were: the link
running, the connector running if it ran before (started again when a failed setup left it stopped) — and a connector
that was stopped before the upgrade stopped again, since the setup starts it whatever it was. A service that is not as
it was makes the result an error, whatever the setup's exit code. It then reads the installed build and writes
`%ProgramData%\digi-erp-connector\results\upgrade-<ticket>.json`:
`{ticket, from, to, exitCode, startedAt, finishedAt, error?, installedVersion, services, setupLogTail?}`. The ticket is
the first 32 hex digits of the SHA-256 of the envelope's nonce. The ticket's folder keeps `job.json`, `runner.log`
and the setup's `setup.log`; the setup itself is deleted after, and the oldest finished tickets are removed (ten
kept). A lost ticket (its runner ended without a result) loses its setup and runner copy at the next upgrade or
`upgrade-result`, and keeps its job and logs. Each step is also in the Event Log (Application, source
`digi-erp-connector-link`). `link-status` and `state` show the relay URL without a user or password, and an error's
URL loses its whole userinfo, an unescaped `@` in a password included.

A release from v1.0.29 on lists its setup in `SHA256SUMS` (release-windows.yml); v1.0.29 is the last version that must
be installed by hand on a v1.0.28 box.

**Open connector windows (from v1.0.31; digi-mcp G-143).** The setup runs as LocalSystem in session 0. A connector
window open in someone's RDP session holds `digi-erp-connector.exe`, and Inno's `/CLOSEAPPLICATIONS` cannot close it:
it closes only what the Restart Manager reports with no reboot reason, and a process in another Terminal Services
session comes back `RmRebootReasonSessionMismatch` — so the setup failed (GROOPY's terminal server). Before each setup
(the upgrade's and a rollback's) the runner therefore ends every process whose image is the **installed**
`digi-erp-connector.exe` — by full path; another copy, or a process it cannot read, is left alone — in every session,
and waits up to 15 s. Unsaved changes in such a window are lost. The result records `closedApps` /
`rollbackClosedApps` `{found, closed, stillOpen, sessions, error?}`; one still open after 15 s means the setup could
not replace the file, so it is not run, and the result says why.

**Health and one rollback (from v1.0.31; plan 088 D5, which closes plan 086's review m10).** Right before the setup
the runner asks the order queue again (review m6: it waits up to 5 minutes, then defers the setup — nothing run,
outcome `deferred` — unless the command said `assumeIdle`). After the setup it waits up to `healthMinutes` (the
command's, default 15; a policy's own) for the box to be healthy, and if it is not, installs once more exactly the
release it replaced — only to a **signed** digest (the command's `rollback`, a policy's `rollbacks`) and never below the
format floor — [§ Health and the one rollback](#health-and-the-one-rollback). The runner is a copy of the build being
replaced, so this runs from the v1.0.31 → next upgrade on; an upgrade to v1.0.31 still has none. The setup runs as
LocalSystem; CI refuses an Inno Setup older than 6.5.0, whose setups use the private `C:\Windows\SystemTemp` there (m11).

## Managed auto-update (plan 088, v1.0.31)

A box on v1.0.31 or later upgrades **itself**, in its own maintenance window, to a release Digitrade has **approved
once** for a group of boxes, checks that it is healthy afterwards, and goes back to the release it had if not — when a
signed digest allows it. It does **not** follow GitHub's newest release: every merge to this repository's `main`
publishes one, and a box following that would run any merged change as LocalSystem within hours. It follows an
**upgrade policy** that a person approved through digi-mcp (`connector_rollout`, an approval form), signed with the team
key. The format below is the one after the security review (plan 088 §2b, D8–D13).

### Why a relay cannot misuse it (D1, D2, D8–D13)

- **Signed by the team key, not set at the relay (D1).** digi-mcp signs a policy with the team key that signs commands —
  compiled into this release, at or above the key-epoch floor (a policy signed by a newer key raises the floor, as a
  command does). The relay stores and delivers policies, nothing more. It cannot invent or change one. It can withhold
  one: the box then keeps what it holds.
- **The relay chooses nothing that applies (D9, review M1).** A policy names **every box** (`{"type":"all"}`) or an
  **explicit, signed list of box ids** (`{"type":"boxes","boxes":[…]}`, up to 256, ascending): digi-mcp expands a tenant
  or a ring into box ids when it signs. The box takes a policy only for its own id. The tenant and ring the relay states
  in a delivery are repeated in the box's reports and `link-status` as information; they decide nothing — a relay
  stating another tenant neither undoes a hold nor spreads a canary-only approval.
- **The same download checks as the `upgrade` command (D2).** The scheduler only decides *when*; the upgrade is the
  command's own path: an official release of this repository (the compiled-in prefix), newer than the running build,
  its setup's SHA-256 equal to the policy's **and** to the release's `SHA256SUMS` line, the order queue idle. A stolen
  team key can at most install an official newer release — the trust a person has downloading it by hand.
- **A rollback only to a signed digest (D8, review B1).** The one automatic downgrade installs the build replaced only
  when a digest signed by the team key names its setup — the policy's `rollbacks` entry for that build (digi-mcp fills
  them from its pins for every build a covered box runs) or a command's `rollback` — and the release's `SHA256SUMS`
  line and the download equal it. No signed digest: no rollback, the new build stays (`unhealthy_no_rollback`). The
  digest the box recorded when it installed a build is a cross-check only: it never authorises a rollback, and one
  that disagrees with the signed digest stops it. So a TLS-inspecting proxy or a compromised relay with a swapped
  release asset cannot get its own setup run.
- **Holds win by time (D10, reviews M2 and m9).** Of the policies naming the box, the most specific decides (a box list
  over every box); a **hold issued at or after** the deciding moving policy wins at any scope — every box's hold is the
  emergency brake for a box list still targeting a bad release. An **expired** policy holds for its scope: it never
  falls through to a less specific one, so an emergency hold does not lapse into a rollout. A held policy whose key is
  now below the epoch floor holds for its scope too (`policy_revoked`).
- **Scopes and anti-replay (D3, D13; reviews m1, m3, m4, N1).** Each policy carries a `seq` (1 to 2³¹−1). The box keeps
  one floor per scope key — `all`, and `boxes` for every box list, so digi-mcp numbers box lists from one counter — as
  `{seq, the policy's SHA-256, the key's epoch}` **and the signed policy that set it** (its exact bytes) in its secrets
  store, apart from `link\policy.json` and never pruned. It refuses a lower seq, or other bytes at the same seq; the same
  policy delivered again changes nothing. The floors are the authority and `policy.json` their mirror: a reinstall (the
  uninstaller deletes `link\`) or a lost file changes nothing the box holds — an expired list hold, or an expired brake,
  which the relay never delivers again, still holds (review N1). A kept list comes back only when it names the box's
  id: a box paired again under another id follows every box's policy (the box-list floor still binds its seq). A floor
  set by a key now below the epoch floor no longer binds (a stolen key cannot freeze a scope past its rotation). A
  policy lives at most 90 days.
- **No rollback below the format floor (D11, review M3).** Every build raises `DataDir\format-floor` to its compiled
  `RollbackFloor` when it starts (v1.0.31: `1.0.30`; never lowered; the daemon does not start when it must raise it
  and cannot) — the oldest release that reads every file format the builds on this box wrote. The runner never rolls
  back below it, nor below its own compiled floor, nor while it cannot read it (`rollback_below_floor`). A build that
  changes a persisted format sets `RollbackFloor` to its own version in the same change.
- **A link-only failure is not a verdict while the relay is not ready (D12, review N5).** When only the new link failed
  the health check, the runner asks the relay's **`/readyz`** from the box (link.yaml's relay, through its proxy) — 200
  only while the relay's registry answers, so the relay could admit the box; `/healthz` says nothing about that. It
  answers 200 — our side is broken, the rollback runs; anything else (503, no answer, a relay from before `/readyz`
  answering 404) — the new build stays (`unhealthy_link_inconclusive`). A relay or registry outage during a fleet
  window no longer rolls the fleet back.

The policy, as signed (`pkg/linkproto` `UpgradePolicy`, shared with the relay and digi-mcp):

```json
{"v":1,"kind":"upgrade-policy","scope":{"type":"boxes","boxes":["bfl-read","bfl-write"]},"seq":12,
 "issuedAt":"2026-11-16T09:00:00Z","expiresAt":"2026-12-16T09:00:00Z",
 "target":{"version":"1.0.32","sha256":"<64 hex of digi-erp-connector-setup-1.0.32.exe>"},
 "rollbacks":[{"version":"1.0.31","sha256":"<64 hex of digi-erp-connector-setup-1.0.31.exe>"}],"hold":false,
 "window":{"days":["mon","tue","wed","thu","sun"],"from":"02:00","to":"05:00"},"healthMinutes":15,"by":"ilya"}
```

It travels as `{"policy":"<the exact bytes, base64>","keyId":"team-1","sig":"<base64 Ed25519>"}`; the signature covers
`digi-link-policy-v1\0` + those bytes, and a box accepts only the canonical encoding `linkproto.SignPolicy` makes (no
unknown, duplicate or re-cased key; a box list and the rollbacks in ascending order). `target: null` or `hold: true`:
do not upgrade (a hold names no rollbacks). `rollbacks`: at most 16, each older than the target, the key left out when
there are none. The window is in **the box's local time**; `to` before `from` runs past midnight (the occurrence
belongs to the day it starts on), `to` may be `24:00`, and a moving policy's window is at least `healthMinutes` + 30
minutes long (review m17). `healthMinutes` is 5 to 120; a policy lives at most 90 days.

### What the box does

- **Policies arrive on a policy stream** the relay opens: on every connection (an empty list too), when a policy that
  names the box changes, and hourly — the current every-box policy and the newest box list naming the box. The link
  verifies each one (key, epoch floor, canonical bytes, shape), checks it is not expired, names this box's id and has a
  newer seq than the floor, and keeps it in `link\policy.json` (written only when a policy changed). It takes at most
  one delivery per connection every 10 seconds (review m2: one arriving sooner waits, and of those waiting only the
  newest is taken). Then it answers on a **report stream**: its view — what it holds per scope, what it refused and
  why, what decides, the last automatic upgrade — signed by the box's own key, so the relay can store it but not forge
  it (digi-mcp checks it against the key it pinned at pairing). The box opens a report stream only on a connection
  whose relay opened a policy stream (an older relay closes a connection whose box opens a second stream), and at most
  five a minute (the relay's limit is six); one over that goes later, as the latest view.
- **Every 10 minutes (± 1), and after a delivery that changed a policy** (at most one such check per 10 seconds), the
  scheduler looks at what decides. The box upgrades when the policy names a release newer than its own, the box's
  local time is inside the policy's window, no upgrade is running, and `link.yaml` lets it (`autoUpgrade` and `admin`
  not `false`, no key it does not know). Orders in the queue, another command or a provisioning run, or a setup the
  runner deferred for orders: tried again at the next check, in the same window. Any other refusal (a download that
  failed, a digest that does not match): the next window — never more than one attempt per window per target. A ticket
  that ends unhealthy (rolled back or not) stops that target under that policy, and so do three failed tickets — kept in
  `autoupgrade.json`'s tally, apart from the newest twenty attempts (review m10): a newer policy (a higher seq, even for
  the same release) arms it again.
- **The upgrade itself** is a ticket like a command's, with `auto: true` and the policy (`{scope, seq}`) in its job and
  result, the policy's `healthMinutes`, the policy's signed `rollback` digest for the build it replaces (when the policy
  has one), and — when this box knows it — the digest that installed that build (`fromSha256`, a cross-check).

**Lifting a brake, and box lists (for digi-mcp).** Replacing an every-box hold with a new every-box target lets every
older box-list target that hold braked decide again for its boxes: hold or re-issue those first. Dropping a box from a
list does not release it: it follows the newest list naming it, and holds once that list expires; to return it to the
fleet, sign a list naming it with the fleet's target.

### Health and the one rollback

Right before the setup the runner asks the order queue once more (review m6): with orders queued it waits up to 5
minutes, then defers — the setup is not run, nothing changes, outcome `deferred` — unless the command said
`assumeIdle`. The deferred ticket keeps its checked setup, and the next attempt at the same release and digest takes it
(once the release's `SHA256SUMS` line is fetched again and it still hashes to the digest) instead of downloading it
again (review N10). After the setup it waits up to `healthMinutes` for one healthy round: the installed build is the target
(or the build it replaced, when the setup left that), both services are as they were before, the daemon proves itself
the way provision's `state` does (it logs in with `config.yaml`'s credential and answers `/api/ping` with the installed
build; a daemon stopped before is not asked), and the installed build's link is connected to the relay
(`link\session.json`: its build, a liveness answer since the setup started and within 3 minutes).

Not healthy: the new build stays, and a person decides, when
- only the link failed and the relay's `/readyz` does not answer 200 from the box (`unhealthy_link_inconclusive`, D12);
- no signed digest names the setup of the build replaced, or the one that names it disagrees with the digest this box
  recorded (`unhealthy_no_rollback`, D8);
- the build replaced is below `DataDir\format-floor` (or the runner's own compiled floor), or the floor cannot be read
  (`rollback_below_floor`, D11).

Otherwise it installs **exactly the build it replaced**, once — that release's setup, downloaded from the compiled-in
prefix, its SHA-256 equal to the signed digest and to the release's `SHA256SUMS` line. It waits up to 5 minutes for the
order queue to empty first (then goes ahead: the connector's stop drains its queue, up to 120 s), ends open connector
windows, puts the services back as they were and checks health again. That rollback is the only downgrade the link ever
makes, only for the ticket's own rollback; a second failure leaves the box as that setup left it, and the result says
so. A setup still running when the runner stops waiting is never followed by a health check or a second setup.
DIG-1297's rule holds for it too: never below 1.0.25 on a box with a pickup folder. The result gains `health`
`{ok, minutes, seconds, checks:[{name, ok, detail}]}`, `rollback`
`{to, sha256, exitCode, ok, installedVersion, services, health, startedAt, finishedAt, error?}` (and, when the rollback
failed, `rollbackSetupLogTail` from `rollback-setup.log` in the ticket's folder), and the runner's own `outcome`: `ok`,
`failed` (the setup or the services failed, and the box was healthy as it was), `deferred`, `unhealthy`, `rolled_back`,
`unhealthy_no_rollback`, `rollback_below_floor` or `unhealthy_link_inconclusive`.

**A restart of the server during the health window (review m7)** — Windows Update in the same night window — no longer
loses the check. Once the setup has ended the runner keeps its progress in the ticket's `health.json`. When the link
starts again it starts that ticket's runner again (only when the runner copy is still the file the link made: the job
records its SHA-256), and the runner goes on with the health check, with its whole budget, and what follows. A restart
during the rollback, or a second restart, is not resumed: the result says so (`unhealthy`), and a person decides. A
restart during the setup itself is Inno Setup's to survive; that ticket is lost, as before.

Every later build must keep writing `link\session.json`, keep `state`'s `daemon` fields (`asked`, `version`, `error`,
`queue`) and raise `DataDir\format-floor` at start: the runner is a copy of the build being replaced and judges the new
one by them.

### Where to look

- `link-status` and provision's `state` (`link.autoUpgrade`): the switch, the policies held (scope with its box list,
  seq, target, rollbacks, window, health minutes, issued and expiry, by, whether each applies or expired), what decides
  and why (`scheduled`, `up_to_date`, `upgrading`, `policy_hold`, `policy_expired`, `policy_revoked`, `no_policy`,
  `auto_upgrade_off`, `admin_off`, `state_unreadable`, `stopped`), the moving policy a later hold brakes (`braked`), the
  next window and the next check, a "not now" (`waiting`), and the last automatic upgrade with its outcome, health and
  rollback. The tenant and ring the relay states are shown as information. `digi-erp-connector-link.exe status` prints
  the switch.
- Files in the data directory: `link\policy.json` (a mirror of what the box holds — one that is lost or cannot be read
  changes nothing, and the next delivery writes it back), the seq floors and the policies that set them
  (`secrets\link_policy_floors.bin`, DPAPI, at most two policies; a reinstall and a new pairing keep them),
  `format-floor`, `link\autoupgrade.json` (the scheduler's memory, its tally
  and its last view), `link\session.json`, and the tickets' `upgrade\` (with `health.json` while a health check runs)
  and `results\` as above.

## Behind the link, `provision` is stricter

See [provisioning.md § Behind the link](provisioning.md#behind-the-link-link-mode).
