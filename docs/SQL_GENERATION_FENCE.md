# SQL generation fence: safety requirements

The current SQL-primary guard must remain a full parity scan. A database
generation token alone cannot prove that mutable Go maps still match SQL.

## Probe (12.4.26)

The diagnostic probe uses PRAGMA data_version, total_changes() and
PRAGMA schema_version on the same pinned database/sql.Conn. It is not
used by production authorization. A new connection invalidates the
baseline. A rollback may change total_changes and must invalidate a
cache conservatively. Multiple separate queries are not an atomic fence.

## Before enabling any fast path

1. Route every in-memory mutation through an audited generation boundary,
   including changes through pointers, slices and nested objects.
2. Bind the SQL proof to one connection and a consistent transaction,
   with a full audit on connection change or unknown generation.
3. Validate per-object identity and ACL at use; detect external commits,
   schema changes and uncertain rollback/commit outcomes.
4. Retain full audits on startup, recovery, backend transition, and
   any suspicious or unclassified mutation. Never use a cached proof to
   authorize an operation after a database error.
5. Test direct in-memory drift, unchanged-row corruption, external
   connections, rollback, process restart, and concurrent access.
6. Treat physical database corruption as a separate integrity problem;
   neither data_version nor total_changes detects arbitrary file edits.

A proof covering only SQLite is not a proof of SQL/JSON/in-memory parity.
No schema migration or backend default change is needed for this probe.

## Ownership boundary (12.4.27)

Storage now clones known mutable Job/AuthorImport fields at ordinary caller
entry/exit boundaries. This prevents shared ACL slices and nested pointers
from escaping those methods. SQL reads decoded from a fresh snapshot are
already detached. Internal in-place writes and archive publication paths
still need a separate inventory and generation accounting. The probe does
not authorize a fast path.

## Conservative mutation epoch (12.4.28)

The process-local metadataMutationEpoch is invalidated at ordinary save
boundaries and before the special archive publication, restore and purge
intents. The counter never rewinds on rollback and saturates at MaxUint64
(which must never certify a cached proof). It can advance more than once
per logical operation and resets on process restart. It counts attempts,
not committed transactions. Startup initialization has no existing cache.
It does not detect direct writes bypassing these boundaries; the full
SQL/ACL parity scan remains required. Do not enable a fast path until
all such paths and nested object ownership have been audited.
