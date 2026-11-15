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
- It keeps one WebSocket to the relay (through the proxy in `link.yaml`, or the environment's), with many streams
  inside it. It reconnects by itself, with a pause that grows to a minute; it moves to a fresh connection every ~20
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

## For the client's IT

- **State:** `link.yaml` in `%ProgramData%\digi-erp-connector\` — the relay, this box's id once paired, the proxy.
- **Turn remote commands off:** set `admin: false` in `link.yaml`. Data keeps flowing (the shop's sync and orders);
  every command is refused and logged.
- **Every command** is logged to `link.log` (same folder) and to the Windows **Event Log** (Application, source
  `digi-erp-connector-link`): what ran, for which change set, by whom, and how it ended.
- **Remove it:** uninstall the connector, or stop and delete the service; its key goes with the data directory.

## Pairing (once)

Digitrade gives a code (`ABCDE-FGHJK`, single use, 30 minutes):

```powershell
& 'C:\Program Files\digi-erp-connector\digi-erp-connector-link.exe' pair ABCDE-FGHJK
```

It prints a **fingerprint**; read it to Digitrade, who confirm it before anything else happens. `status` shows the
relay, the box id and whether commands are on; `fingerprint` prints it again.

## Behind the link, `provision` is stricter

See [provisioning.md § Behind the link](provisioning.md#behind-the-link-link-mode).
