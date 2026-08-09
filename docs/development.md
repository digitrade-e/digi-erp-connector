# Development

Read [../CLAUDE.md](../CLAUDE.md) before changing code — it lists the constraints
that are enforced rather than suggested. This document is the practical side:
build, test, CI, and how to add things without breaking the rules.

## Toolchain

Go, version pinned in `go.mod`; CI resolves it with `go-version-file`, so bump it
there and nowhere else.

```bash
go build ./...
go vet ./...
go test ./...
gofmt -l ./cmd ./internal    # must print nothing
```

**The GUI is Windows-only.** `cmd/digi-erp-connector` consists of
`//go:build windows` files, so on Linux the package has no `main` and
`go build ./...` fails. Build and test on Windows.

Release-style binaries:

```bash
go build -trimpath -ldflags "-s -w" -o digi-erp-connectord.exe ./cmd/digi-erp-connectord
go build -trimpath -ldflags "-s -w -H=windowsgui" -o digi-erp-connector.exe ./cmd/digi-erp-connector
```

`-H=windowsgui` suppresses the console window. `*.exe` is gitignored — never commit
binaries.

## Tests

Table-driven, standard library only. `t.TempDir()` for the filesystem, `httptest`
for HTTP, `t.Setenv("PROGRAMDATA", …)` to redirect the data directory so a test can
never touch a real installation.

```bash
go test ./...
go test ./internal/queries/ -run TestRun -v
go test -cover ./...
```

Where the suites are, and what they actually protect:

| Package | Protects |
|---|---|
| `internal/queries` | Parameter binding and type inference; the JSON shape of scanned values (datetime and decimal formatting — **a wire contract with a live backend**) |
| `internal/api` | The route table (`http.ServeMux` panics on conflicting patterns, so constructing the server is itself a test), the auth matrix, and that the deleted legacy routes stay deleted |
| `internal/api/handlers` | Saved-query CRUD over `httptest`, send-order validation |
| `internal/erp/hasavshevet` | The 2891-byte IMOVEIN record layout, Windows-1255 encoding, sequential order numbers |
| `internal/files` | Traversal rejection and allow-list enforcement |
| `internal/config`, `internal/secrets`, `internal/platform/atomicfile` | Round-trips, atomic replacement, and that failures are *reported* rather than swallowed |
| `internal/db` | DSN construction, password URL-encoding, the TLS options |

One test is environment-gated and skips by default:
`MIGRATED_QUERIES_PATH=<file> go test -run TestMigratedFileParses ./internal/queries`
validates that a real `queries.json` parses.

### Coverage, honestly

The order pipeline (`erp/hasavshevet/send_order.go`) and the SAP price/stock query
are the largest and least covered code in the repo. The IMOVEIN byte layout and
order numbering are tested; `ProcessOrder` end-to-end is not, and `erp/sap` has no
tests at all. If you touch either, that is the gap to close — ideally by comparing
generated output against known-good captured files.

## CI

Two workflows, both required reading if you change the release process:

| Workflow | Trigger | Does |
|---|---|---|
| `ci` | every push and pull request | gofmt check, vet, build, test — on `windows-latest` |
| `auto-tag` | push to `main` | creates the next patch tag |
| `release-windows` | `auto-tag` completing | **vet + test**, then builds both binaries, compiles the Inno Setup installer, publishes the release |

Consequences worth knowing:

- **Every push to `main` publishes a release.** Do not push half-finished work
  there. `[skip ci]` in the head commit message skips the workflows entirely —
  appropriate for documentation-only pushes.
- **A test failure blocks the release but not the tag.** `auto-tag` runs first, so
  a failed release leaves a tag with no release attached. Deliberate: better a
  dangling tag than a published installer nobody tested. Delete the tag, fix, push
  again.
- CI runs on Windows because of the GUI build tags and because the config/secrets
  tests exercise `PROGRAMDATA` and DPAPI.

## House rules

These exist because each one was violated at least once and cost something.

**Write files only through `platform/atomicfile.Write`.** Three separate
temp+rename implementations had drifted apart and one of them reported success
when the rename failed, so a failed secret write looked fine and the daemon later
failed to start with no explanation.

**Decode request bodies with `decodeJSONBody`** (`handlers/json.go`). It applies
the shared 1 MiB limit, rejects trailing data and keeps numbers exact. Do not add
per-handler decoders or per-handler size constants.

**Respond with `respond.JSON` / `respond.Error` only.** One error envelope,
`{error, code, details}`, with a stable machine-readable `code`. There is no
second error shape any more.

**Never return a raw driver or filesystem error to a caller.** Log the detail, give
the caller a generic message plus a code. A DB error names schemas, hosts and
logins.

**`readFormConfig` must start from `f.cfg`**, never a zero `config.Config`, or a
GUI save silently drops every key without a widget, which on a production box
means resetting settings nobody intended to touch.

**Keep the tree gofmt-clean.** `.gitattributes` pins `.go` files to LF so the
formatting check is meaningful; before it existed, `core.autocrlf` made all 52
files look unformatted and hid four real problems.

## Adding an endpoint

1. Request/response structs in `internal/api/dto/`.
2. A `New…Handler(deps) http.HandlerFunc` constructor in `internal/api/handlers/`,
   one file per endpoint group. Decode with `decodeJSONBody`, respond with
   `respond.*`, map domain errors to codes explicitly.
3. Register in `internal/api/server.go` using Go 1.22 method patterns
   (`mux.Handle("POST /api/thing", wrap(h))`). `wrap` gives you logging, rate
   limiting and auth — do not bypass it.
4. Document the route and its error codes in [api.md](api.md).
5. Test it: `server_test.go` already fails if the route table conflicts; add
   behaviour tests over `httptest`.

Business logic belongs in an `internal/` package, not in the handler and never in
`cmd/`. A handler should validate, delegate and translate errors.

## Adding a saved query in production

Do not edit `queries.json` by hand on a live machine — the daemon owns it and holds
it in memory. Use the CRUD endpoints (`POST /api/custom_sql`,
`PATCH /api/custom_sql/{name}`). Hand-editing is for migration only, before the
daemon starts.

## Changing response value formatting

Don't, unless you mean to. Datetimes render as `…T00:00:00.000Z` and
`DECIMAL`/`NUMERIC`/`MONEY` as shortest-form JSON numbers specifically to match
what the old Node connector emitted, because a live backend parses it.
`internal/queries/normalize_test.go` pins this. Changing it is a breaking API
change and needs coordinating with the backend.

## Project memory

`../.claude/docs/` records why things are the way they are — the two predecessor
repos, every significant decision with its rationale, the implementation log
including the production cutover, and the live box's exact settings. When something
looks arbitrary, the answer is usually in `02-decisions.md`.
