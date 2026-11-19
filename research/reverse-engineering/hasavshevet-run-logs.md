# Hasavshevet — Masofon run logs: one order's own result

Follow-up to [hasavshevet-importer.md](hasavshevet-importer.md) (P1, P3). Question: after the connector runs a
Masofon BAT for an order, how does it tell **this order's** result — imported or refused, and why — and report it to
the backend? Observed on a production Hashavshevet 2026a SP2.1 installation (read-only, several months of logs) and
checked against the connector as of v1.0.34.

## 1. What Hashavshevet writes

### Where

Masofon2's settings screen ("הגדרות יבוא אוטומטי לחשבשבת") has two general folders. They are stored in
`masofon.ini` **beside `has.exe`**, section `[general]`:

| Screen field | Key | Meaning |
|---|---|---|
| מחיצת יומן שגיאות (error-log folder) | `ErrDataPath` | every run of every profile leaves its log here |
| מחיצת קיצורי דרך (shortcuts folder) | `ShortCatPath` (sic) | where Masofon2 puts the BATs' shortcuts |

There is one folder per installation, shared by all profiles, and **different on every client** (often on another
drive, sometimes rotated by hand to a new folder). A `masofon.ini` found elsewhere (e.g. an empty one in the
shortcuts folder) is not the one `has.exe` reads.

### The file

`<bat>.ok.DMY-dd.mm.yy.HMS-hh.mm.ss.LOG` or `<bat>.err.…LOG`, Windows-1255, written when the run **ends** (the name's
time is the last line's time). Content:

```
[<station>_<pid>.<bat>] ---><command line>         one block per command of the profile (';' in [commands])
[dd/mm/yy hh:mm:ss] <input file>:                  one section per file the command read (wildcards expanded)
[dd/mm/yy hh:mm:ss] קליטת תנועות מלאי הסתיימה בהצלחה    that file imported
[dd/mm/yy hh:mm:ss] =============================
   — or, per refusal —
[dd/mm/yy hh:mm:ss] <value>\t<code and message>\t<row>\t     e.g. "123\tStock.063 מסמך בספרור זה כבר נקלט בעבר\t1"
   — or —
<file pattern>:  File Not Found(SetDsn)            the command found no input file
   — after the sections, possibly —
[…] WizImportError.log  תקלת מערכת, פרטים בקבצי לוג במחיצת התוכנה   system error; details in
                                                   <has.exe folder>\DataImpExpLogs\WizImportError*.log
```

### What it means

1. **One log can hold several files' results.** A profile whose data file is a wildcard (`imovein*.dat`) imports
   every matching file in one run and writes one section per file; a profile can also hold several commands. One
   `err` log seen held three imported files and one refused file.
2. **`ok` / `err` is the run's, not the file's.** `err` = at least one thing went wrong anywhere in the run. On the
   installation surveyed, ~97 % of one profile's `err` logs were only "File Not Found" (the BAT ran with nothing to
   import), and an `err` log can follow a file that **imported** (a later `WizImportError`, e.g. the input could not
   be renamed to `.BAK`).
3. **A refusal is almost never in `ImportLog`.** `ImportLog` gets one row per *imported* file (the wildcard expanded
   into the real file name, `Status=Success`). For the same profile and period: ~1 900 `Success` rows, **1** `Failed`
   row, against thousands of refused lines in the logs. So: success is confirmed by `ImportLog` (P1); **the reason for
   a refusal exists only in the run log**.
4. **A refused file stays and is retried by every run** (one was retried every 5 minutes for hours). Each retry
   writes a new `err` log with the same refusal. The most common refusal of an order import is `Stock.063` "a
   document with this number was already imported": the file was imported before and is being read again.
5. **Other runs share the folder and the BAT.** A scheduler or another integration may run the same BAT around the
   same time; the newest log is not necessarily the run the connector started.

## 2. What the connector does today (v1.0.34)

- `attachRunLog` reads `ErrDataPath` from `masofon.ini` in the BAT's folder, else `hasExePath`'s. There is no setting
  for it, and nothing when the INI is elsewhere or unreadable.
- `FindRunLog` takes the **newest** log of the BAT written since the importer started.
- `readRunLog` reads the **whole** log: `OK` from the file name, every tab line as an error, the last "קליטת…" as the
  summary.
- `ImportLog` decides when it has our file's row; otherwise `!OK` → `failed`, `NoInput` → `not_logged`.

Consequences: with a wildcard or multi-command profile, or a concurrent run of the same BAT, another file's refusal is
reported as the order's, or the order's own refusal is missed. A harmless `File Not Found` from another command makes
the order `failed`. `Stock.063` (already imported) is reported as a plain failure.

## 3. What the connector should change

### P9 — `hasRunLogDir`: the error-log folder as a setting

- New setting `hasRunLogDir` (absolute; empty = today's behaviour: `ErrDataPath` from `masofon.ini` beside the BAT,
  else beside `has.exe`). Set it when the INI cannot be read by the service's account or the logs are elsewhere.
- `discover` reports both: `masofon.errDataPath` (from the INI) and the folder actually used, and a defect when
  neither is readable (`masofon_run_log_dir_unreadable`) or when they differ (note, not defect).
- Read the INI on every order (the folder can be changed in Masofon2 at any time); never write it.

### P10 — Read the order's own section, not the whole log

Parse a log into **command blocks** (`[...] --->`) and **file sections** (`<path>:`), and decide per section:

| Section | Outcome |
|---|---|
| ends with "הסתיימה בהצלחה" and has no refusal line | `imported` |
| refusal lines (tab-separated) | `refused`, with each line as `{value, code, message, row}` (`code` = `Stock.NNN` when present) |
| only `Stock.063` refusals | `duplicate` (already in Hashavshevet; not a new failure) |
| `File Not Found(SetDsn)` for a pattern that matches our file | `no_input` (the file was not there when the BAT ran) |
| a transient marker (another station issuing, a second run, deadlock — P3's list) | `retry` (the profile's next run imports it) |

The order's section is the one whose path equals the file the connector wrote (case-insensitive, `\`/`/`, quotes
trimmed), or for a wildcard command the file name that the pattern expanded to. Run-level lines (`WizImportError`)
are kept apart as `systemError` and do not override an `imported` section. A log with **no** section header (seen on
single-file profiles after a system error) falls back to today's whole-log reading.

### P11 — Pick the run that read our file

Among the BAT's logs whose name time (and mtime) is at or after the importer's start (−2 s), take the **earliest one
that has a section for our file**, not the newest. When the connector starts `has.exe` directly, the `<pid>` in the
block header can also be matched to the process it started. No log with our section within the importer's run →
`not_found` (with the number of logs seen).

### P12 — Attach the system error's detail

When a log has `WizImportError`, read `<has.exe folder>\DataImpExpLogs\WizImportError*.log` lines time-stamped within
the run (first section time … log name time), at most 20, and attach them (`systemErrors`). Read-only; the files grow
for years, so read the tail only.

### P13 — One verdict for the backend

Add `importCheck.outcome`, decided from `ImportLog` (P1) **and** the section (P10), in this order:

| `outcome` | When | Backend |
|---|---|---|
| `imported` | `ImportLog` Success with our document, or section `imported` | order done |
| `duplicate` | section `duplicate` (`Stock.063`) | already in the ERP: done, do not resend |
| `refused` | section `refused`, or `ImportLog` Failed | show `errors[]` (`code`, `message`, `row`) to the user; fix and resend |
| `retry` | section `retry` | wait; the next run of the profile imports it (the file stays) |
| `not_run` | `no_input`, or the profile does not read our file (discover's `masofon_data_file_mismatch`) | setup problem |
| `unknown` | no row, no section, unreadable | ask the client to check Hashavshevet |

`result` (P1) and `runLog` stay as they are (additive JSON only). The backend reads it from the order status
(`GET /api/sendOrder/{jobId}` → `importCheck.outcome`, `importCheck.runLog.errors`); the line contents are values from
the order itself (item codes, numbers), never other orders', because only our section is returned.

### P14 — A refused order stops being retried

With P10, `hasMoveRefusedOrders` moves the file aside only for `refused` (never `retry`, never `duplicate`, never
when the section is someone else's). Without it, a refused file is retried by every run and fills the folder (tens of
thousands of logs in a few months on the installation surveyed).

### Model (a) (pickup folder, another program runs the BAT)

The connector does not run the importer, so today it checks nothing. With P9–P11 it could check later: when the
pickup file disappears (renamed to `.BAK` or moved), look for the section with that file name in the logs written
since the file was written, and for its `ImportLog` row; report through the same `importCheck`. Separate change.

## 4. Tests to write

- Parser: golden logs (Windows-1255) — single file imported; wildcard with 3 imported + 1 refused (our file each of
  the four); two commands; `File Not Found` in one command and our file imported in the other; `Stock.063` only;
  imported then `WizImportError`; no section header; a transient marker.
- Run choice: two logs since start, our file only in the older one; a log from before start.
- Outcome table: each row of P13, including `ImportLog` Success with an `err` log (→ `imported`).
- `hasRunLogDir` validation (absolute, exists) and fallback order.
