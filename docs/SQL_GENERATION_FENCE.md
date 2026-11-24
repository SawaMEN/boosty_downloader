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

## Direct-access inventory (12.4.29)

A Go AST regression test inventories functions that contain selectors named
jobs or imports in production server files. The baseline has 42 entries.
New or stale entries fail the test and require review. Reads are included
because a pointer may escape through a read. This is deliberately conservative:
it is not type-aware, does not track aliases, and cannot detect a new mutation
inside an already listed function. No cached authorization path is enabled.

## Single-job copy-on-write boundary (12.4.30)

Ordinary single-job operations replace the record through
replaceStorageJobLocked; a failed save restores the exact previous pointer.
ACL changes no longer leave a newer timestamp behind on rollback. Creation
rejects an unrelated existing ID rather than overwriting it. Batch operations,
imports and special physical journals remain separate audit targets. This is
not a cached parity proof.

## Batch copy-on-write (12.4.31)

Ordinary imports, combined scheduling and group trash now use a single
batch persistence boundary. Duplicate IDs are rejected before mutation;
rollback restores the exact previous pointers. Inputs and outputs are
detached from storage-owned records. Physical archive journals are still
separate. No cached parity or authorization fast path is enabled.

## Physical journal classification (12.4.32)

Archive publication, restore and purge share the SQL recovery/parity
sequence. Their final metadata generation is classified by a streaming
SHA-256 with no-follow file access and size/identity checks. Existing size
policies are preserved. SQL export refuses a pending SQL-primary intent.
This does not replace a full parity scan or enable a cached proof.

## Bounded revision-preview projection

The administrator-only aggregate report groups at most 32 physical archive
identities. It obtains nonblocking archive read gates first, then performs a
full SQL parity audit and matches all identities under Storage.mu. A second
full audit and identity check precede inclusion of the batch in the report.
No metadata pointers escape; no authorization or mutation guard is cached.
Busy and changed identities are omitted, and any parity failure rejects the
entire response. This is an advisory read-only report, not a deletion plan or
a consistent global filesystem snapshot. The normal full guard remains
unchanged for all point reads, grants and mutations.

## Cursor-local scan buffers

Ordered archive and import scans reuse scalar Scan destinations and decode
payload_json directly from sql.RawBytes before advancing the cursor. The
Job/AuthorImport value is freshly decoded for every row, including optional
fields and grants; no RawBytes or mutable record is retained across rows.
All ordering, ownership, timestamp, ACL, orphan and inventory checks remain
unchanged. This is an allocation optimization, not a cached parity proof.

## Validated equality without serialization

For exactly deep-equal Job/AuthorImport values, the fast proof validates all
potentially fallible JSON fields: UTC timestamps with years 0–9999 and
finite tier/purchase prices. The remaining current schema consists of plain
strings, booleans, integers, string slices and known optional structures.
Other values, non-UTC timestamps, differing representations and any failed
proof use the original two-Marshal JSON comparison. A schema audit test
requires explicit review of new encoders, types or fallible fields. No
row, grant, identity or generation check is skipped, and no proof is cached.

## Small ACL validation and matching

Archive payload validation and SQL grant matching use a bounded linear scan
for at most eight grants. A bitmask tracks consumed entries without altering
payload order or creating a map. Larger ACLs use hash-based validation and
matching to avoid quadratic work. Both ordered full scans and point reads
reject missing, extra, duplicate and malformed grants. The matcher assumes
payload validation has already succeeded; all call sites enforce this.
No SQL generation checks, access decisions or mutation guards are cached.

## Exact no-op SQL commit

After recovery and validation of both existing metadata images, an exact
byte-for-byte unchanged pair with nil hooks requires no new intent or SQL
write. A fresh complete SQL parity transaction remains mandatory, followed
by no-follow verification of both unchanged mirror files and directory sync.
Any extra hook, changed byte, or malformed input retains the ordinary path
or fails closed. Identical ACL slices skip only construction of the grant
delta after the existing persisted ACL has been validated. This is not an
authorization cache, nor a change to recovery or durability of real writes.

## Streaming shadow reconciliation plan

The non-authoritative shadow still performs complete ordered SQL validation
on every periodic sweep, before any change or extra callback. It retains
only seen desired IDs, changed before/after records and deletion identities,
not a second full SQL inventory. The complete plan is built and applied
inside the same transaction; a late invalid row cannot cause partial
publication. Archive writes precede archive removals, then imports, as
before. No change to authority, access decisions, timer or recovery policy.
