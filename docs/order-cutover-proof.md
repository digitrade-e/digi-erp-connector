# Order cutover proof: byte-compare and upgrade rehearsal (DIG-1304)

Before BFL's first real order goes through the platform (Digitrade plan 075 P1-6, Jira DIG-1304), three things are
proven on copies and on a test box, never on BFL's write node:

1. **Byte-compare.** For N real legacy orders, the `IMOVEIN_<n>.doc` the connector writes is byte for byte the file
   the legacy Digitrade app wrote for the same order.
2. **GROOPY rehearsal.** An upgrade over a running install and a downgrade both keep the order counter and the order
   index, and no number is issued twice.
3. **Receipts.** The release that carries the order changes passes the receipts' tests and a receipt round on GROOPY.

Two tools in this repo do the checking. Neither touches a production box, and neither writes anything but its own
output file.

| Tool | What it does |
|---|---|
| `cmd/imovein-compare` | `diff`: compares two files by the legacy layout and names every field that differs. `run`: sends each order's `sendOrder` body through the connector's own route, queue and writer, offline, and compares the file with the legacy one. `show`: lists a legacy file's fields per record. |
| `cmd/connector-rehearsal` | `snapshot`: records a box's numbering state (counter, order index, history, receipt store and index, format floor) as numbers and hashes. `check`: names everything an upgrade or downgrade must never do between two snapshots. |

> **The repo is public.** Real legacy files, `sendOrder` bodies and the tools' reports hold customer names, addresses
> and BFL's prices. Keep them in a private folder outside the repo (e.g. `~/dig-1304/`), never in a commit, a PR, an
> issue or Jira. Attach only `connector-rehearsal` snapshots and check output, which hold numbers and hashes only.

## 1. Byte-compare

### 1.1 What to fetch (by hand, read-only)

None of this is in the workspace. The legacy files live only on the legacy server, and the local checkout of the old
app does not have them.

| # | What | Where it is | Why |
|---|---|---|---|
| 1 | The legacy order files, one per order: `IMOVEIN_<n>.doc`, and `TMP_IMOVEIN_<n>.doc` for a return | legacy server, `sync/herp_files/<sifrur>/` | the expected bytes |
| 2 | For each customer in those orders, the Hashavshevet `Accounts` row: `AccountKey`, `FullName`, `Address`, `City`, `Phone`, `Agent` | BFL's read node (a saved query), or read back from the legacy file's fields 2, 5, 6, 7, 35, 11 | the connector takes fields 5, 6, 7, 35 from `Accounts` on the box; offline the harness needs them as the manifest's `account` |
| 3 | For each order, the `sendOrder` body that BFL's mapping produces for the re-keyed cart | local stack: B2B mirror `backend_data`, `erp_request_log.request_payload` of the cart's order send (see 1.2) | the input |

**Which orders, and how many (N).** The plan does not fix N. The minimum that covers the ticket is **5**. Each one is
a different path through the mapping:

1. an order to warehouse 81;
2. an order to warehouse 12;
3. an order with an overall discount, line discounts, a remark with a customer part and a manager part
   (`customer|manager`), a Hebrew item name and a fractional quantity;
4. a price offer (document 40, warehouse 8);
5. a return (document 74, warehouse 8, `TMP_` file).

Add more if the first run shows a field that differs for some orders only.

**Recent orders are best.** Use orders for which BFL's answers in plan 075 §8 (Q1, Q2, Q6, Q7; DIG-1288) already
held, so the expected values are the ones the cutover config will produce.

### 1.2 Re-key the orders and capture their bodies (local stack)

On the local stack with the **bflstore** mirror loaded (`cat ~/.config/digi/last-client`) and the post-refresh safety
steps of the `local-dev` skill done. Those steps blank the ERP keys, so a release cannot reach BFL.

1. Re-key each legacy order as a cart: same customer, same lines in the same order, quantities, list prices and
   discounts, same remarks. The legacy file itself holds all of it: `imovein-compare show <legacy file>` lists every
   field of every record, decoded (lines: 22–29; discount 17, remarks 37, warehouse 12, document 4).
2. Apply the cutover config of plan 075's Resume block (mappings per DIG-1295 comment 17136, `historyId ← releaseKey`,
   `header/*`, `ORDER_DOCUMENT_DISCOUNT=YES`) to the **local** erp-manager only.
3. Release each cart on the approval desk. Point the bflstore ERP connection at a sink that answers 202, for example
   a local `digi-erp-connectord` built from this repo with model (a) and `sendOrderFormat: legacy` (§1.4), so that the
   release completes and the body is logged.
4. Read each body: `select request_payload from erp_request_log where cart_id = <id> order by id desc limit 1;` and
   save it as `req/<name>.json`.

> Not verified on 2026-11-18: the local mirror had 0 `erp_request_log` rows, and no release had run on the local
> stack. Check step 4's row on the first cart before re-keying the rest.

### 1.3 Run the comparison

The folder layout, outside the repo:

```
~/dig-1304/
  manifest.json
  req/order-81.json    req/order-12.json    req/order-disc.json    req/offer.json    req/return.json
  legacy/IMOVEIN_<n1>.doc    …    legacy/TMP_IMOVEIN_<n5>.doc
  out/
```

`manifest.json` (paths are relative to it):

```json
{
  "allow": [],
  "cases": [
    {"name": "order-81", "request": "req/order-81.json", "legacy": "legacy/IMOVEIN_123456.doc",
     "account": {"accountKey": "…", "fullName": "…", "address": "…", "city": "…", "phone": "…", "agent": "…"}},
    {"name": "return", "request": "req/return.json", "legacy": "legacy/TMP_IMOVEIN_123460.doc", "account": {…}}
  ]
}
```

```bash
go run ./cmd/imovein-compare run -out ~/dig-1304/out ~/dig-1304/manifest.json
```

What it does for each case:

- **Number.** It takes the order number from the legacy file's field 3 (MyID), or from the case's `number` if set,
  and sets the counter so that the order gets exactly that number. In a live run the connector's own counter decides
  field 3. The harness pins it so that every other byte can be compared.
- **Real path.** It posts the body to the connector's own `POST /api/sendOrder` handler, queue and writer, configured
  as BFL's write node will be (`sendOrderPickupDir` + `sendOrderFormat: legacy`). This happens in a temporary folder
  that is removed afterwards. The only parts not taken from the box are the `Accounts` lookup (a body with no
  `account` gets the manifest's) and the currency rate (1, the connector's default without a database; BFL's orders
  are in shekels).
- **Checks.** It checks that the connector names the file like the legacy did (`TMP_` for a return), compares the
  bytes, and keeps the connector's file in `out/` as `<case>.<file name>`.

Exit 0 means every case matches, 1 means something differs, 2 means the manifest or a file could not be read.

**The acceptance run has an empty `allow`.** If a re-keyed cart cannot carry the legacy order's date, the dates
(fields 9, 10, 36) are the only fields that may be allowed (`"allow": [9, 10, 36]`). Say so on the ticket.

### 1.4 Files written by a real connector

For files that a running connector wrote, on the local stack (step 1.2.3) or on GROOPY, compare them directly:

```bash
go run ./cmd/imovein-compare diff -allow 3 legacy/IMOVEIN_123456.doc pickup/IMOVEIN_<m>.doc
```

Flags go before the two files. Field 3 differs there by design, because the connector numbered the order from its own
counter. So does field 8 when the body carries no `asmahta2`: the connector then writes the order number there too.
In that case use `-allow 3,8`.

### 1.5 Reading the report

```
== order-disc: FAIL (order 123458, connector file IMOVEIN_123458.doc)
  DIFFERENT (3 records in the legacy file)
  record 2 field 26 (הנחה % - 26) at byte 1838: legacy "6.5", connector "6.50"
```

Each line names the record (one record per order line), the PRM field and its title, the byte offset in the file,
and both values decoded from Windows-1255. A `problem:` line is about shape, not a field:

- the record count;
- a line that is not 1296 bytes (the connector is on `sendOrderFormat: official`, or the file is not IMOVEIN);
- CR bytes (the file was copied in text mode: fetch it in binary);
- a separator that is not a space;
- a file name other than the legacy's.

Where to look first:

| Field | Usually |
|---|---|
| 4, 12 | the mapping's document type or warehouse for that mode (DIG-1295) |
| 5, 6, 7, 35 | `Accounts` vs the legacy's customer fields, or the DIG-1315 `header` mapping |
| 17, 26 | the discount: overall vs per line, and its decimals (`ORDER_DOCUMENT_DISCOUNT`) |
| 18 | VAT: the legacy writes `18.0` (four characters) |
| 24 | list price vs net price: only `originalPrice` reaches IMOVEIN |
| 28, 37 | text the legacy cut differently (§ below) |

**Differences that come from the legacy and are not bugs.** These are listed in
[hasavshevet-send-order.md § The legacy layout](hasavshevet-send-order.md#the-legacy-layout-dig-1297--sendorderformat-legacy):

- The legacy cut remarks at 250 UTF-8 bytes, and half a Hebrew letter makes iconv drop the whole field.
- It turned a newline in a name into a space, where the connector writes `. `.
- It rounded half up.
- Its `iconv //TRANSLIT` spells some symbols out (`©` → `(C)`), where the connector writes `?`.

Write each one you find on the ticket with the case name, and decide with BFL whether it matters to WizSched.

### 1.6 What gets committed

Real files are never committed. When a real case shows a byte the connector writes wrong, reproduce the order
synthetically:

1. Add the case to `internal/erp/hasavshevet/testdata/imovein-legacy/cases.json`, with made-up names and prices.
2. Regenerate the goldens by **running** `generate.php` (its header gives the `docker run` line) and add the
   matching request to `legacyRequestCases` in `imovein_legacy_test.go`.
3. Fix the writer until `TestLegacy*` passes.

The precedent is `testdata/kupain/KUPAIN_redacted.doc`. `internal/imovcompare`'s own tests run the harness on those
synthetic goldens, so the harness itself is pinned.

## 2. GROOPY rehearsal: upgrade and downgrade

Run by day by Ilya, on GROOPY only. GROOPY is the test box where plan 068 passed on 2026-10-20; it ran v1.0.19 on
2026-10-21. **Read its version first**: digi-mcp `fleet_inventory`, or the connector's `state`.

**Builds.**

- **Candidate**: the build that will go on BFL's write node, from this repo's current `main`.
- **Downgrade target**: the newest release before the candidate, and **never below v1.0.30**
  (`formatfloor.RollbackFloor`). Below it, the order index and the counter recovery read differently; see
  [troubleshooting.md § A downgrade below v1.0.30](troubleshooting.md#a-downgrade-below-v1030-plan-087). If GROOPY is
  still on a build older than the target, the first upgrade goes to the target and is not part of the proof.

**Tool.** Build `connector-rehearsal` for the box and copy it there:

```bash
GOOS=windows GOARCH=amd64 go build -o connector-rehearsal.exe ./cmd/connector-rehearsal
```

`<data>` is `C:\ProgramData\digi-erp-connector`. `<send>` is `sendOrderDir` from its `config.yaml`, the folder that
holds `lastOrderNumber.json`. Keep every snapshot under `D:\dig-1304\` (or any folder outside both).

| Step | Do | Then |
|---|---|---|
| 0 | `GET /api/sendOrder` answers `{"queued":0,"running":0}` | `connector-rehearsal snapshot -data <data> -send <send> -out S0.json` |
| 1 | **Backup** (§3) with the service stopped, then start it | snapshot `S1`; `check S0.json S1.json`: no finding |
| 2 | Send a test order **T1** through GROOPY's test path | snapshot `S2`; `check S1 S2`: one new index entry, counter +1 |
| 3 | **Upgrade** to the candidate over the running install (`connector_upgrade`, or the setup by hand) | `GET /api/ping` `version` = candidate; snapshot `S3`; `check S2 S3`: no finding |
| 4 | Send T1 again (same `historyId`): 202 with T1's `jobId` and `replay: true`, nothing written. Send **T2**: number = T1 + 1 | snapshot `S4`; `check S3 S4` |
| 5 | **Receipts round** (§4) | snapshot `S5`; `check S4 S5` |
| 6 | **Downgrade** to the target: stop the service, run the target's setup, start it. The link never installs an older build except its own rollback, so this is done by hand | `version` = target; snapshot `S6`; `check S5 S6`: no finding |
| 7 | Send T2 again: replay, T2's number. Send **T3**: number = T2 + 1 | snapshot `S7`; `check S6 S7` |
| 8 | **Upgrade** to the candidate again | snapshot `S8`; `check S7 S8`, then `check S0 S8`: no finding over the whole run |

**What a finding means.** `check` exits 1 and prints one `FINDING:` line per problem:

- the counter went backwards, is missing or is below a number already issued;
- an index entry was lost, renumbered or became unreadable;
- a number was issued twice;
- the receipt store went backwards;
- `format-floor` was lowered.

Any finding stops the rehearsal. **Never repair it by lowering `lastOrderNumber.json` or deleting an index file**:
see [troubleshooting.md § The order index](troubleshooting.md#the-order-index-dig-1291).

**Attach to DIG-1304:** the snapshots `S0`–`S8` and every `check` output. They hold numbers, states and hashes only.

## 3. Backups (GROOPY in the rehearsal, BFL's write node before the install)

With the service **stopped**. Never copy or restore `DataDir\receipts` while it runs: a running writer trusts its
in-memory recovery state.

```powershell
Invoke-RestMethod … /api/sendOrder            # {"queued":0,"running":0} first
Stop-Service digi-erp-connectord
$b = "D:\backup\connector-$(Get-Date -Format yyyyMMdd-HHmm)"
robocopy "C:\ProgramData\digi-erp-connector" "$b\DataDir" /E /COPY:DAT /R:1 /W:1
Copy-Item "<sendOrderDir>\lastOrderNumber.json" "$b\lastOrderNumber.json"
Start-Service digi-erp-connectord
```

Add a `connector-rehearsal snapshot` beside the backup. The dated folder and the snapshot together are the
acceptance criterion "the DataDir backup and the copy of `lastOrderNumber.json` exist, dated, before the install".

**Never restore `lastOrderNumber.json` backwards.** If the DataDir is ever restored, the order index moves the counter
past every number it holds at the next start. The backup copy of the counter is evidence, not something to put back.

## 4. Receipts in the same release

- **Tests.** On the candidate's commit, `go test -count=1 ./internal/erp/hasavshevet/ -run 'Receipt|Kupain|ImportMu'`
  and the handler's `go test -count=1 ./internal/api/handlers/ -run SendReceipt` pass. CI runs the whole suite, on
  Windows as well.
- **On GROOPY**, if receipts are configured there (`sendReceiptDir`):
  1. Send one receipt.
  2. Send it again with the same `idempotencyKey`. It must come back with the same `receiptNumber` and
     `written: false`.
  3. Send a receipt while a test order is being written. Both must succeed: they share the import lock, and the
     receipt waits for the order (at most 20 s). `503 RECEIPT_WRITER_BUSY` would mean that the order held the lock
     longer than that.

  The checks at steps 3, 6 and 8 of §2 show the receipt store and index unchanged across the upgrades and the
  downgrade.
- The receipts on BFL's write node stay dormant (outbound endpoint 14 `alive = false`) until cutover day. The release
  for orders is a release for receipts too.
