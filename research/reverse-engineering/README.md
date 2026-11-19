# Research — reverse engineering

Notes on how the ERPs we write into behave internally, and what that means for the connector. Each note ends in a
list of concrete changes; none of them is implemented by the note itself.

| Note | ERP | Date |
|---|---|---|
| [hasavshevet-importer.md](hasavshevet-importer.md) | Hasavshevet (Wizsoft H-ERP) 2026a SP2.1 — Masofon, `has.exe`, the stock-document and receipt import | 2026-11-19 |
| [hasavshevet-run-logs.md](hasavshevet-run-logs.md) | Hasavshevet 2026a SP2.1 — Masofon run logs: where they are, one log for many files, an order's own result for the backend (P9–P14) | 2026-11-19 |

This repository is public. These notes describe observable behaviour (file formats, database tables and columns, the
import log) and our conclusions. Decompiled code, vendor scripts, client data and credentials are not copied here;
the evidence and the method are kept in Digitrade's private knowledge base (digi-brain,
`reverse-engineering/hashavshevet/`).
