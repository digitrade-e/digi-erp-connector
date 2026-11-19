# Hasavshevet's importer, and what the connector should change

How Hasavshevet 2026a SP2.1 turns our IMOVEIN / KUPAIN files into documents, established on a production install on
2026-11-19 from its binaries, scripts and (read-only) database; then the changes that follow for the connector,
in priority order. **CONFIRMED** = seen directly; **INFERRED** = follows from what was seen; open points end the note.

## 1. How the importer works

### The chain

```
connector writes IMOVEIN.doc/.prm ──► cmd /C <profile>.bat
  BAT:  call has.exe /"<dir>\importsql.prj -batch" /<server>\<instance> -p"<bat file name>"
  has.exe reads the import command from masofon.ini, on the line named after the BAT
  has.exe runs the stock-import script (MLAYIN.HDS) for import type -6 (issue) or -118 (do not issue)
     1  parse the file through the PRM (COM WizImportLogic, 32-bit)
     2  stage every line in table ImportStock, tagged with a new ImportUniqueID
     3  fill defaults; validate: each failing line gets ErrorMessage 'Stock.NNN'
     4  any refused line (and no ImportWithErrors option) → the WHOLE FILE fails, nothing is merged
     5  merge into Stock (status 5) + StockMoves; a new document whenever number, account or type changes
     6  reprice / copies / print style; issue (status 1) or leave not issued (status 0)
     7  one ImportLog row (Status=Success|Failed, the Stock.ID range created)
        + one log file <bat>.ok.…LOG or <bat>.err.…LOG in masofon.ini's ErrDataPath
     8  success only: rename the input <file>.DMY_…HMS_….MASOFON2.BAK; a failed file stays and is retried next run
```

All CONFIRMED (script control flow, live tables, and log/rename counts on a production install). **The process
exit code means nothing**: no script sets one and the generated BAT ends with a bare `EXIT`. `partial` exists only
for imports a user runs by hand.

- **The BAT holds no parameters** (CONFIRMED). `-p"<name>"` makes `has.exe` look its own name up in `masofon.ini`.
  The profile line there names the company, the import type, the **data file** (may be a wildcard), the PRM, the
  Hashavshevet user and the options. A profile whose data file is not the file we write imports nothing, without an
  error. Copying or renaming a BAT breaks it.
- **Staging is emptied after a run** (CONFIRMED: `ImportStock` is empty between runs), so the per-line errors are not
  left in the database; they are in the run's `.err` log.
- **`has.exe` executes the positional `[commands]` line**, split on `-`: a dash in any folder, file or user name the
  profile uses shifts every later argument. (`[commandsV2]` is what the Masofon editor shows.)
- **Concurrency.** A BAT's double-run guard (`<bat>.IsWorking`) is deleted by the second instance after 10 s, and two
  different BATs never guard each other. The importer writes `schema.ini` / `imn.txt` into the **input folder**, so
  two imports reading one folder collide. All stock imports share the staging table (deadlocks on it are common on a
  busy install). Production installs often already have another integration polling its own folder every few
  seconds — the connector must never share a folder, a data file or a BAT with one.

### Where our fields land (CONFIRMED)

The staging insert takes IMOVEIN's fields in IMOVEIN's order, so:

| IMOVEIN line | Connector value | `Stock` column |
|---|---|---|
| 2 | account key | `AccountKey` |
| **3** | **the order number (MyID / `jobId`)** | **`DocNumber`** |
| 4 | document type code | `DocumentID` **after translation** (below) |
| 8 | `asmahta2` (else the order number) | `Reference` |
| 9 | `createdDate` | `ValueDate` |
| 22–24, 26 | sku, quantity, price, discount | `StockMoves.ItemKey`, `Quantity`, `Price`, `DiscountPrc` |

`DocNumber` and `Reference` must be **at most 9 digits, digits only** (rules `Stock.006/010`, `Stock.009`).

### Document types are Hashavshevet-6 codes (CONFIRMED)

Line 4 is read as a **Hashavshevet-6 code** and translated through the company's table `DocumentsDefV6`
(`Version6Code` → `ID`). On the install surveyed: 30 → order, 31 → agent order, **32 → export order**, 40 → price
quote, 74 → agent return. So:

- our default ORDER 30 / 32 is "order" / "export order": the currency switch picks between those two types;
- `Stock.DocumentID` holds the **translated** id (an order sent as 31 is found under the agent-order id), and the
  translation is per company — one tenant's mapping says nothing about another's.

### Automatic numbering replaces our number (CONFIRMED)

A document type with `DocumentsDef.FixNumerator = 1` gets the type's next number when it is issued: **our
`DocNumber` (line 3) is replaced**, and only `Reference` (line 8, `asmahta2`, else our order number) keeps a number
of ours. On the install surveyed, the types behind codes **30 (order)** and **32 (export order)** — the connector's
ORDER defaults — are auto-numbered; 31 (agent order), 40 (quote) and 74 (agent return) are not. Which type a tenant
uses is the tenant's decision; the connector must know which case it is in.

### Validation (CONFIRMED)

~75 rules, codes `Stock.001`…`Stock.106`. The ones our orders can hit: account missing (only when the profile may
not create accounts), item missing (only when it may not create items), item key empty or zeros, quantity zero or
not a number, price out of range, document type unknown in the company or not importable, `DocNumber` /
`Reference` / agent over 9 digits, warehouse not digits, VAT % / discount % invalid, currency not in `Currencies`,
dates unreadable, and the duplicate rule below.

**One refused line fails the whole file** in a Masofon run (unless the profile sets `ImportWithErrors`): nothing is
imported, the file stays in the folder and is **retried by every later run**, failing again each time. On the install
surveyed, 85 files of another integration were stuck like that, re-logged every 30 minutes. Most frequent refusals
there: "quantity to close is zero", "base document locked for closing", duplicates, currency/rate errors; "item not
found" was rare. A `.err` log holding only `File Not Found` is a run that found no input — harmless.

### Hashavshevet's own duplicate guard (CONFIRMED)

Off by default. With the Masofon option `DISQUALIFICATIONOFDUPLICATION=YES` **and** an issuing import (type −6),
rule `Stock.063` refuses a document when an **issued** (`Status = 1`) document exists with the same translated
`DocumentID`, `AccountKey`, `ValueDate` and `DocNumber`.

It does **not** catch: a resend with another value date, a draft (status 0) or −118 import, a document closed since,
**any auto-numbered type** (issuing replaced the number it compares), or another writer's documents. When it fires,
the whole file fails and — like any refused file — is retried every run until someone moves it.

On the install surveyed, the B2B profile with the option on produced 1 duplicate in ~2.5 years; a second writer
(a WMS) with the option off re-imports numbers the B2B profile already used — 8 085 same-key pairs. The guard only
protects a writer from its own resends, and a shared `DocNumber` space is normal on a busy install.

### The import log — the proof of an import (CONFIRMED)

Table **`ImportLog`**, one row per run, kept for years:

| Column | Content |
|---|---|
| `FileName` | the exact input file the run took |
| `IsMasofonCommand` | `<bat>=<command>&Company=<name>&Status=<Success / Failed>` (manual imports: `No…`, may be `partial`) |
| `ExtraData` | `StationID=…&FirstStockID=<n>&LastStockID=<m>&Ver=<version>&finish=<hh:mm:ss>` |
| `FingerPrint` | `<ImportUniqueID>:…` |
| `ImportDate` | date only |

Every document a run creates carries `Stock.MasofLogFrom = CreateDate='…'&IsMasofon=1'&ImportUniqueID=<id>`, the
same id as the first 19 characters of `FingerPrint`, and `Stock.MasofCreateDate`.

**The connector's existing read-only login can read all of it** (it is `db_datareader`).

Caveat: refused Masofon runs are not reliably in `ImportLog` (2 `Failed` rows in 120 days against many `.err`
logs); a `Success` row is reliable proof, a missing row is not proof of failure. The `.ok`/`.err` log in
`ErrDataPath` is the complete per-run record, and the `.MASOFON2.BAK` rename happens on success only.

### `Stock.Status` (CONFIRMED values, INFERRED meaning of 2)

0 = not issued (what type −118 leaves; drafts), 1 = issued, 2 = closed (rare), 5 = being imported (only inside a run).

### Other things worth knowing

- **JSON input exists.** The parser also reads JSON (and writes a JSON error file) for stock documents and receipts —
  Wizsoft's "HConnect" path. Its schema is not public. It would remove the fixed-width/Windows-1255 class of bugs,
  but only through Masofon/`has.exe`: the COM engine on its own only stages rows, the script does the rest. Do not
  call the COM objects directly.
- **The script can show message boxes** on some paths. Under LocalSystem there is no desktop: such a run never ends.
- **`has.exe` runs as the profile's Hashavshevet user** and the BAT as the caller (LocalSystem for us).
- Hashavshevet's `.PRJ` files carry a SQL connection string with a password in clear text; `discover` must never
  read them into its output.

## 2. What the connector should change

Ordered by value. File references are to this repository on 2026-11-19.

### P1 — Confirm every import from `ImportLog` (closes "done means files written")

Today an order is `written` once the files exist and the importer exited, whatever its exit code
(`internal/erp/hasavshevet/send_order.go` step 8: the exit code is only logged); receipts trust exit code 0.

Change: after the importer returns, decide from three signals, strongest first:

1. **`ImportLog`** (saved, parameterised, read-only): the newest row whose `FileName` is the file we wrote, dated
   after we started. `Status=Success` → parse `FirstStockID..LastStockID`, read those `Stock` rows (+ line count
   and total from `StockMoves`), check translated `DocumentID`, `AccountKey`, line count, and our number in
   `DocNumber` — or in `Reference` when the type is auto-numbered (§1). New index state **`imported`** with the
   `Stock.ID`; expose it in `GET /api/sendOrder/{jobId}` (`stockId`, `importStatus`).
2. **The rename**: our file became `….MASOFON2.BAK` → success (the connector already scans for it in `inflight.go`).
3. **The run log**: a `<bat>.err.….LOG` in the profile's `ErrDataPath` written after we started, and our file still
   in place → **`import_failed`**, with the log's text (P3).

Neither success nor failure within a timeout → `import_unknown`; usually a profile whose data file is not ours (P2).
The process exit code is ignored — it carries nothing.

For model (a) (a scheduler imports from the pickup folder later) the same checks run as a background pass over
`written` orders.

This also gives the platform's `confirmDocuments` reconcile (Q-075-X4) an exact answer: expose the stored
`stockId` / status through a read endpoint, so the reconcile no longer guesses by number.

### P2 — Check the Masofon profile in `discover` / `provision`

Add to `discover` (read-only, `internal/provision/discover.go`), for the configured `hasBatFile`:

- read `masofon.ini` from the BAT's folder; find the `[commandsV2]` (else `[commands]`) line named exactly as the BAT
  — missing: new defect `masofon_profile_missing`;
- report the profile's company, import type, data file, PRM file, user and options; defects:
  `profile_data_file_mismatch` (its data file / wildcard does not match what the connector writes: `sendOrderDir\IMOVEIN.doc`, or the typed folder's `namePattern`),
  `profile_prm_mismatch`, `profile_not_fixed_length`, `profile_no_issue` (−118 where −6 is expected, or the reverse),
  `duplicate_guard_off` (`DISQUALIFICATIONOFDUPLICATION` not `YES`) — a warning, the tenant decides;
- report `ErrDataPath` (P3 reads it);
- never output `.PRJ` contents.

`provision` should refuse an orders configuration whose profile does not import the connector's file.

### P3 — Attach the ERP's own error report, and move refused files aside

After a failed run, read the run's `<bat>.err.DMY-….LOG` in the profile's `ErrDataPath` and store its path and text
(Windows-1255 → UTF-8) with the order. Include the `Stock.NNN` codes found; map the common ones to a short text
(account missing, item missing, quantity zero, document type unknown, duplicate). The full code list is in the
private notes.

Then **move the refused file out of the input folder** (to `sendOrderDir\refused\<n>\` with its log): left in place,
Hashavshevet retries it on every later run, fails again, and — in the fixed-file model — the next order overwrites
it anyway. In the pickup model a stuck file also keeps its number "used" for the folder scan.

### P4 — Pre-validate what Hashavshevet will refuse

Before reserving a number (so the caller gets a `400` instead of a whole file refused, and retried, later), with read-only queries
cached per process:

- each `sku` exists in `Items` (unless the profile creates items), the account exists in `Accounts`;
- the line-4 code exists in `DocumentsDefV6`, and the target `DocumentsDef` row is the expected kind
  (`ForseenOrder` = −1 for ORDER, a quote for QUOATE, …) — reported once at startup / in `discover` as well;
- the currency is in `Currencies` (only ORDER sends one today);
- the order number (counter `max`) and `asmahta2` stay within 9 digits — already enforced for `asmahta2`; add a
  startup check that a counter's `max` ≤ 999 999 999.

### P5 — Put a timeout on the order importer

`runBatFile` / `runImporter` run with no timeout (`runCommand(ctx, 0, …)`; receipts have 90 s). A message box in a
script path, or a locked database, blocks every later order **and** receipt behind the shared import lock. Add a
configurable `orders.importTimeout` (default a few minutes), kill the process tree when it expires (as
`exec_cmd.go` does for receipts), mark the order `import_unknown`, and let P1's `ImportLog` lookup settle it —
never retry a killed run blindly.

### P6 — Reconcile key and number ranges

- The key that identifies our document in Hashavshevet is **translated `DocumentID` + our number + `AccountKey`**,
  confirmed by `MasofLogFrom` → `ImportLog.FileName` being our file. Our number is in `DocNumber`, or **in
  `Reference` only** when the type is auto-numbered — so for those types `asmahta2` must stay unique too (default:
  the order number). The best key is the `Stock.ID` stored by P1. `DocNumber` alone is never enough: another writer
  may use the same number (observed).
- `discover` should report `FixNumerator` for every document type the box's mappings send (read-only), so the
  platform knows which case each mode is in.
- Seed counters (plan 087 ranges) above every `DocNumber` used by the same document types, not only above our own
  history: a read-only `max(DocNumber)` per translated type at `provision counters` time, reported in `discover`.

### P7 — Recommend the duplicate guard, document what it does not cover

With `DISQUALIFICATIONOFDUPLICATION=YES`, a resend of an already issued order is refused by Hashavshevet
(`Stock.063`), which backs up the connector's `historyId` index (DIG-1291) for manual re-imports from `history/` and
for a receipt recovery that rewrites a file already taken. It does not cover drafts (type −118, `TMP_` returns),
auto-numbered types, or a document closed since; and a refused duplicate is retried every run unless P3 moves it.
Whether a tenant turns it on is the tenant's decision; `discover` reports it (P2). Receipts have their own switch
(`masofon.ini [kupa] AvoidDouble`).

### P8 — Own folder, own profile, contiguous lines

- Refuse (P2) a profile whose data file or folder another BAT in `masofon.ini` also reads, and a `sendOrderDir` /
  pickup folder that another integration writes to: the importer's temp files and the shared input collide.
- Refuse paths and Hashavshevet user names containing `-` (the positional command line is split on it).
- Keep writing one document per file with its lines contiguous (already true); never merge orders into one file.

### Not recommended

- Calling `WizImportLogic` / `WizImportMySql` COM objects from the connector: 32-bit in-process only, and they stage
  rows without the script's merge, issuing and final processing.
- Writing to `Import*` tables or `Stock` directly: the connector stays read-only on the database.
- Switching to JSON input before Wizsoft documents it (open question Q9).

## 3. Open questions (test on a test company, never production)

Answered by this research: one refused line fails the whole file (Masofon runs); the `.BAK` rename happens on
success only; the exit code carries nothing; one `.ok`/`.err` log per run in `ErrDataPath`; PRM line 1 (record
length) is not enforced, so the official/legacy 2891-vs-1297 difference is harmless; `Stock.063` compares issued
documents only and is skipped for −118 and defeated by automatic numbering.

| # | Question | Test |
|---|---|---|
| Q1 | Is every refused Masofon run in `ImportLog`, or only some? | Import a refused file on a test company; compare `ImportLog` with `ErrDataPath` |
| Q2 | `Status = 2` meaning | Ask Wizsoft, or compare with the UI |
| Q3 | Which script paths open a message box in `-batch` mode | Run failing imports interactively |
| Q4 | Line 87 (allocation number) — who fills it | Inspect an imported invoice's `TaxApiNumber*` |
| Q5 | JSON import types (`.81` variants) and schema (HConnect) | Ask Wizsoft |
| Q6 | KUPAIN end to end: receipt import type (7 issue / 126 temporary), `ImportLog` type name, `[kupa] AvoidDouble` | Add a receipt profile on a test company, import a test receipt |
| Q7 | Is `ImportWithErrors` reachable from Masofon's editor, or only in the positional line? | Ask Wizsoft, or read Masofon2's dialog |
