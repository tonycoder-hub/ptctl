# Exit codes and report-first results

[Command guide](README.md) · [Documentation index](../README.md)

> A compact summary for scripts and agents, including the JSON envelope and every `kind`, is in [AUTOMATION.md](../AUTOMATION.md).

Exit code `0` means a report or requested read succeeded, `1` an operational
failure, `2` invalid usage, and `3` an explicit integrity mismatch. Discovery
is report-oriented, so blocked results still print and return `0` by default.
Add `--require-verified` to return `4` unless `source_outcome` is
`verified_unique`, or `verified_selected` for an explicit
`--select-source-match`; target-plan or client-mapping failure does not erase
source evidence. `torrent verify` prints its result before returning `3`.
Reconciliation is also report-oriented. Add `--require-reconciled` to return
`4` unless the independently reported local axes are `consistent`; the report
is still printed first. Storage-index corruption, non-canonical descriptor/data
records, and descriptor/data binding mismatches print the reconciliation report
before exit `3`; ordinary index/store I/O prints it before exit `1`. A
structurally incomplete report remains exit `0` by default, and only
`--require-reconciled` changes that report-oriented result to exit `4`.

Site-binding inspection returns `0` only after the explicit record and linked
private artifact verify together and the current built-in adapter accepts the
recorded provenance. Missing or unreadable records return `1`, usage returns
`2`, record/artifact corruption returns `3`, and unsupported adapter provenance
returns `4`, always after any non-usage report. Locator listing returns `0`
when its bounded inventory is complete and report-first `4` when a record,
entry, or name-byte limit stops it.

Bonus-exchange retention is also report-first. `pruned`, `already_pruned`, and
a newly confirmed `forgotten` return `0`; operational interruption, uncertain
record-removal durability, removal ambiguity, or unattributed absence after the
last forget marker return `1`; invalid usage or a missing deletion
acknowledgement returns `2`; sealed-record integrity failure returns `3`; and a
non-terminal operation, incomplete bounded inventory, explicit selector
conflict, or other retention policy blocker returns `4`. Prune and forget are
credential-free and perform no site request.

Materialize validates every selector, acknowledgement, timeout, and limit
before opening a metafile, source root, target root, or journal. Successful
`run`/`resume`, a completed status/list read, a successful abandon,
`pruned`/`already_pruned`, and a newly confirmed `forgotten` return `0`.
Unattributed absence after forget, operational interruption, cancellation,
publication/removal ambiguity, or
unconfirmed post-publication durability returns `1`; invalid usage or a
missing acknowledgement returns `2`; content/journal integrity failure returns
`3`; and a policy blocker, reviewed-plan mismatch, missing fresh source
authority, explicit operation not found, or incomplete bounded operation
listing returns `4`. Non-usage outcomes are written before the exit. A failure
may have nonzero or uncertain writes, and a reported operation ID is the only
recovery handoff; retries never choose an operation automatically.
Forget is intentionally different after its last marker is gone: an explicit
selector then yields report-first `absent_unattributed` and exit `1`, because
the tool can no longer prove either prior completion or a never-existing ID.

Source-retirement pruning uses the same report-first exit lattice:
`pruned`/`already_pruned` return `0`, operational interruption or uncertain
durability/removal returns `1`, invalid usage or a missing acknowledgement
returns `2`, marker/journal/namespace integrity failure returns `3`, and an
explicit selector, terminal-state, filesystem, or bounded-inventory policy
blocker returns `4`. It never reads downloader credentials or contacts the
client.

Parent-cleanup pruning and forgetting use the same report-first lattice:
`pruned`/`already_pruned` and a newly confirmed `forgotten` return `0`;
interruption, uncertain durability/removal, or unattributed absence after the
last forget marker return `1`; invalid usage or a missing acknowledgement
returns `2`; marker/journal/namespace integrity failure returns `3`; and an
explicit selector, terminal-state, filesystem, or bounded-inventory policy
blocker returns `4`. Neither transition reads source roots, credentials, or a
downloader.

Client-activation pruning uses the same local report-first lattice:
`pruned`/`already_pruned` return `0`, interruption or uncertain durability or
removal returns `1`, invalid usage or a missing acknowledgement returns `2`,
marker/journal/namespace integrity failure returns `3`, and an explicit
selector, terminal-state, filesystem, or bounded-inventory policy blocker
returns `4`. It never reads downloader credentials or contacts the client.

Client removal is also report-first. `ready`, `removed_keep_data`,
`removed_causality_unproven`, local-only `historical_removal_complete`, and a
freshly reverified `already_complete` return `0`; operational interruption returns `1`; invalid usage or a missing
acknowledgement returns `2`; journal, current-use, or final-content integrity
failure returns `3`; and a policy blocker, unknown request result, incomplete
initialization, or missing explicit operation returns `4`. The report records
private journal writes and the one mutation request separately. A `200`
response never by itself produces a completed outcome.

For the metafile store, exit `0` includes idempotent `already_initialized` and
`already_present` outcomes. Missing/uninitialized stores, absent objects, I/O
failures, unsupported store formats, or inability to enforce the required
privacy/atomicity controls return `1`; selector conflicts return `2`; invalid
input artifacts or a stored digest/parse mismatch return `3`. The store does
not itself use exit `4`; discovery/reconciliation requirements and blocked
materialize controls retain their separate report-first meaning. A
post-publication durability failure reports `published_durability_unconfirmed`
and returns `1`, even though its write count may already be `1`.

Storage profile creation is idempotent for the same name and immutable
declaration; reusing a name for different roots or policy fails. A complete
index refresh normally commits two writes (data then descriptor). Budget or
enumeration incompleteness prints a report and returns `4` without publishing a
descriptor. Publication/I/O failure returns `1`; a published-but-unconfirmed
record keeps its nonzero write receipt and is never rolled back or silently
reported as durable.

`site metafile fetch` returns `0` only after both the exact artifact and its
sealed historical binding are verified (newly published or already present),
`1` for site, credential, store, binding publication, or durability failures,
`2` for invalid usage or a missing acknowledgement, and `3` for an invalid
exact metafile response or corrupt artifact/binding. It does not redefine exit
`4`.
After usage validation, failures remain report-first: site request accounting
and private-store write accounting are separate facts, and a successful site
observation never implies confirmed store durability. An import failure without
an exact store reference never becomes an observation merely because response
bytes were received.

---

Previous: [Reconciliation](reconcile.md)
