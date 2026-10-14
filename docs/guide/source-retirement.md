# Source retirement and parent cleanup

[Command guide](README.md) · [Documentation index](../README.md)

> Retirement starts from a terminal client activation; see [client-lifecycle.md](client-lifecycle.md).

Review which current source file names are eligible for a separately
acknowledged retirement operation:

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl seed retire plan \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --search-root "D:\Media\Original" \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --activation-operation sha256:ACTIVATION_OPERATION_DIGEST \
  --activation-plan-id ACTIVATION_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin \
  --require-eligible \
  --output json
```

This command performs zero writes and always reports
`deletion_authority: none`. It repeats complete live discovery in the explicit
search roots, reads the canonical terminal activation marker selected by its
reviewed action, and uses one authenticated read-only qBittorrent or
Transmission session to observe the exact typed-infohash job before and after
local proof. qBittorrent single-file planning makes one login plus two bounded
job-ledger reads; Transmission uses its fixed two-request CSRF/version
bootstrap plus the same two reads. Ordinary multi-file planning adds two
bounded file-ledger reads. It never retries or mutates the client. Transmission
source retirement is v1-only because its audited ledger exposes no typed v2
identity. The exact published final and selected source bytes are reverified
inside that live-client bracket, and a selected source inside or aliasing the
final is rejected; if a source or the final cannot be resolved for that check,
the plan is incomplete rather than eligible. Default output
contains one-way source-path references; raw source paths
require `--show-absolute-paths`. Only content-bearing regular-file names are
represented. Empty files, padding, directories, and cleanup remain out of
scope. Unselected hardlink or alias names may remain, so the plan never
claims that storage would be reclaimed. The terminal activation marker remains
historical; current job identity, state, and effective paths are established
separately by the bounded live reads. Those values are still non-atomic client
claims and do not prove a remote open inode. Serialized plan JSON is not
execution authority. JSON kind is `content.source_retirement_plan`.

No other downloader job may still use a source name. The same complete
job-ledger responses that prove current use of the plan job are checked for
every other job, without extra requests: its content root (or its save path
when it claims no content root) is translated only through the exact reviewed
`--host-root`/`--client-root` mapping, confined below the host root, and
compared with each selected source name using metadata-only calls. Planning is
blocked with `source.other_client_job_overlap` when another job's root equals,
contains, or lies inside a source name before or after host symlink resolution,
is the same file object as a source (for example a hard-linked cross-seed copy),
or is the same directory as a source parent (for example a bind mount). It is
blocked with `source.other_client_job_unmapped` when another job's path lies
outside the reviewed client root, or the reviewed mapping cannot be reproduced,
because non-overlap cannot then be proven; remove or relocate that job, or use
an activation mapping whose client root covers every job. A truncated,
contradictory, or failed ledger, or a host observation that fails for any reason
other than absence, reports `client.other_use_enumeration_incomplete` and never
yields an eligible plan. The check does not descend into another job's
directory tree, so a link or mount nested inside that tree is not detected, and
it does not consult other ptctl records: materialized finals are independent
byte copies, and adopted finals are covered as downloader jobs.

The examples use qBittorrent. For a terminal Transmission v1 activation, use
`--driver transmission`, the full RPC URL, and the Transmission credential;
all materialize, activation, mapping, and source selectors must still reproduce
the exact reviewed lineage.

To cross the irreversible boundary, repeat every selector from the plan and
provide both its full SHA-256 plan ID and the dedicated acknowledgement:

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl seed retire run \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --search-root "D:\Media\Original" \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --activation-operation sha256:ACTIVATION_OPERATION_DIGEST \
  --activation-plan-id ACTIVATION_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin \
  --expect-plan-id sha256:SOURCE_RETIREMENT_PLAN_DIGEST \
  --acknowledge-source-deletion \
  --output json

ptctl seed retire status --target "D:\PT" --output json \
  sha256:SOURCE_RETIREMENT_OPERATION_DIGEST

ptctl seed retire status --target "D:\PT" --output json

ptctl seed retire parent-cleanup plan \
  --target "D:\PT" \
  --retirement-operation sha256:SOURCE_RETIREMENT_OPERATION_DIGEST \
  --retirement-plan-id sha256:SOURCE_RETIREMENT_PLAN_DIGEST \
  --search-root "D:\Media\Original" \
  --require-cleanable \
  --output json

ptctl seed retire parent-cleanup run \
  --target "D:\PT" \
  --retirement-operation sha256:SOURCE_RETIREMENT_OPERATION_DIGEST \
  --retirement-plan-id sha256:SOURCE_RETIREMENT_PLAN_DIGEST \
  --search-root "D:\Media\Original" \
  --expect-cleanup-plan-id sha256:PARENT_CLEANUP_PLAN_DIGEST \
  --acknowledge-empty-parent-removal \
  --output json

ptctl seed retire parent-cleanup status --target "D:\PT" --output json \
  sha256:PARENT_CLEANUP_OPERATION_DIGEST

ptctl seed retire parent-cleanup resume \
  --target "D:\PT" \
  --search-root "D:\Media\Original" \
  --expect-cleanup-plan-id sha256:PARENT_CLEANUP_PLAN_DIGEST \
  --acknowledge-empty-parent-removal \
  --output json \
  sha256:PARENT_CLEANUP_OPERATION_DIGEST

ptctl seed retire parent-cleanup prune --target "D:\PT" \
  --expect-cleanup-plan-id sha256:PARENT_CLEANUP_PLAN_DIGEST \
  --acknowledge-operation-state-deletion \
  --output json \
  sha256:PARENT_CLEANUP_OPERATION_DIGEST

ptctl seed retire parent-cleanup forget --target "D:\PT" \
  --expect-cleanup-plan-id sha256:PARENT_CLEANUP_PLAN_DIGEST \
  --acknowledge-historical-evidence-deletion \
  --output json \
  sha256:PARENT_CLEANUP_OPERATION_DIGEST

ptctl seed retire prune --target "D:\PT" \
  --expect-plan-id sha256:SOURCE_RETIREMENT_PLAN_DIGEST \
  --acknowledge-operation-state-deletion \
  --output json \
  sha256:SOURCE_RETIREMENT_OPERATION_DIGEST

ptctl seed retire forget --target "D:\PT" \
  --expect-plan-id sha256:SOURCE_RETIREMENT_PLAN_DIGEST \
  --acknowledge-historical-evidence-deletion \
  --output json \
  sha256:SOURCE_RETIREMENT_OPERATION_DIGEST
```

`resume` takes the same local/live selectors, expected plan ID, deletion
acknowledgement, and one explicit operation ID. Neither run nor resume accepts
plan JSON as proof or selects a latest operation. `status` with no operation ID
performs one bounded target-root name listing and returns only canonical source
retirement IDs. Ordinary operation directories are `not_inspected`; a matching
root forget marker is `forget_in_progress_not_inspected`. It does not open
their journals, select a latest operation, expose unrelated names, or claim
terminal/source-absence state. Passing an ID retains the exact historical
journal, tombstone, or visible forget-marker read.

`parent-cleanup plan` is a credential-free, zero-write follow-up that must run
before `prune`, because only the live terminal journal retains the exact source
parents. It reuses the full operation ID, retirement plan ID, and original
search-root scope; proves every retired name absent twice; and then considers
only the journaled immediate parents. A search root is always
`protected_search_root`, a parent with any observed entry is
`retained_nonempty`, and only an unchanged parent observed empty twice is an
`empty_stable_candidate`. The deterministic cleanup-plan ID excludes the
timestamped absence-observation ID and optional path display, so a later proof
can reproduce it without trusting serialized JSON. Child entry names are never
emitted. The planning command performs no removal and reports
`cleanup_authority: none`. Its JSON kind is
`content.source_retirement.parent_cleanup_plan`.

`parent-cleanup run` is the separate irreversible boundary. It requires the
retirement operation and plan IDs, the reviewed cleanup-plan ID, the original
explicit search-root scope, and `--acknowledge-empty-parent-removal`. Before its
first write it repeats the full live review in the same invocation, matches the
plan ID, retains process-local authority, binds the target root, and proves
every candidate can still be rebound with the reviewed identity and is still
empty. CLI execution-protocol limits are fixed; the repeated-review flags may
tighten those limits but cannot raise them. It then creates one owner-private,
target-root-local journal. A durable
attempt marker precedes each removal; after that intentional delay the parent
is observed again, its direct parent is handle-bound, and only the exact empty
directory identity is removed. A durable removed marker records either a
confirmed removal or absence recovered after a prior durable attempt and
parent-directory synchronization. Completion is published only after every
journaled parent is observed absent again.

`parent-cleanup resume` selects one exact operation and must resupply the same
cleanup-plan ID, search-root scope, and acknowledgement. It never enumerates or
selects a latest operation. `status` reads only the explicit private journal;
its absence claims are historical and it does not reopen source roots. No
parent-cleanup command recursively removes ancestors, search roots, files, or
non-empty directories, and no serialized report is accepted as authority.
Resume authority begins only once the canonical intent is durable; an earlier
partial initialization fails closed and is not inferred into a resumable
operation. Execution JSON kind is
`content.source_retirement.parent_cleanup`.

`parent-cleanup prune` is a distinct local deletion boundary. It accepts one
explicit terminal cleanup operation, the matching cleanup-plan ID, and
`--acknowledge-operation-state-deletion`. Before deleting anything it validates
the canonical terminal cleanup journal, seals a private retention intent that
contains no absolute parent paths, and performs a bounded exact inventory of
the flat `journal/` and empty `scratch/` namespaces. Only those identity-bound
private objects are removed. A retention completion is published after the
operation root is exactly its lock plus the two-marker retention directory.
Intent-only or post-removal crash states are advanced only by repeating the
same prune selector; ordinary run, resume, and status stop at the durable prune
boundary. The tombstone retains cleanup/retirement lineage and removed-parent
counts as historical evidence, but cannot recover a parent path or prove that
a parent remains absent. JSON kind is
`content.source_retirement.parent_cleanup.retention`.

`parent-cleanup forget` is the final, separately acknowledged boundary. It
accepts only that complete exact tombstone, the same full operation and plan
IDs, and `--acknowledge-historical-evidence-deletion`. It first publishes an
owner-private root-level intent containing the exact no-path tombstone, then
removes the two retained markers, empty retention directory, and exact
lock-only operation subtree. The root marker is removed last after target-root
durability is confirmed. A crash can resume only from the same explicit IDs;
there is no list/latest or age selector. Once the last marker is gone, a repeat
reports `absent_unattributed`, never historical idempotent success. JSON kind
is `content.source_retirement.parent_cleanup.forget`.

`prune` is a second, narrower deletion boundary. It accepts only one explicit
terminal operation ID, the exact reviewed plan ID, and
`--acknowledge-operation-state-deletion`. Before removing anything it validates
the complete canonical retirement journal, publishes a durable private
retention intent, and inventories the flat `journal/` plus empty `scratch/`
namespace within fixed object/path/byte/memory limits. It then removes only
those identity-bound private control objects and publishes an exact completion
marker. A crash resumes from the same explicit ID and durable intent; it never
selects by age or latest state. The final, downloader job, source parents, and
all content names are outside this authority. The retained tombstone preserves
the plan, intent/completion digests, final/client lineage, and retired counts as
historical evidence; it does not prove that a retired source name remains
absent now. JSON kind is `content.source_retirement.retention`.

`forget` is a third and final deletion boundary. It accepts only the same full
operation and plan IDs plus `--acknowledge-historical-evidence-deletion`, and
only a complete exact retained tombstone is eligible. Before touching that
tombstone, ptctl copies its complete no-path historical evidence into an
owner-private root-level intent. It then removes the two retained markers, the
empty retention directory, and the exact lock-only operation subtree; the root
intent is removed last. A crash before that last step resumes only from the
same explicit IDs. The materialized final, source parents and names, downloader
job, and every other operation remain outside this authority. Once the last
marker is durably absent, no on-target authority remains from which a later
invocation can distinguish a previous successful forget from an unknown
selector, so a repeated call reports `absent_unattributed` rather than
`already_forgotten`. JSON kind is `content.source_retirement.forget`.

`run` rebuilds the complete live plan in the same invocation and compares its
ID before any journal write. It then creates one owner-private operation under
the materialized target root. The private intent binds exact absolute source
parents/names, parent and file identities, sizes, the final identity, client
completion, live-use ID, search-root scope, and fixed protocol budgets. Public
reports expose only pseudonymous path references.

For each source name, a durable attempt marker precedes an identity-and-size
bound, no-follow unlink; a durable completion marker follows confirmed
absence. A crash after the attempt but before completion is recoverable only as
`absence_after_durable_attempt_and_parent_durability_recovered`. Absence without a prior attempt,
identity replacement, a changed parent/root, corrupt journal data, final proof
change, or client identity/layout change fails closed. A read error while
comparing source and final bytes is reported as an interrupted, resumable
observation; only differing bytes or lengths are integrity failures. Immediately before the
first unlink of each `run` or `resume` invocation, after the long source and
final reads and the durable attempt marker, one more complete job-ledger read
repeats the other-job check for every name not yet retired. A blocker leaves
every name in place and the operation resumable; a failed or incomplete read is
reported as interrupted, also resumable. Once every name is
retired, the same authenticated client session first reobserves current use and
the exact final is then reverified; the terminal marker is published only after
both. ptctl never re-logs in or retries inside a session, so an expired or
failed downloader session after a removal leaves a journaled, resumable
`partial` operation: `resume` with a fresh session re-proves current use and
completes without removing anything again. Resume verifies remaining source
bytes before its first downloader read, so only the per-name comparisons that
bracket each unlink fall inside the session's before/after window.

A normal qBittorrent single-file run makes one login and four bounded ledger
reads (five HTTP requests total); Transmission makes its fixed two-request
bootstrap plus those four reads (six total). Ordinary multi-file runs add
three bounded file-list reads, for eight qBittorrent or nine Transmission
requests. Active resume uses two proof observations after bootstrap plus the
pre-unlink ledger read when a name remains to unlink: four/six qBittorrent or
five/seven Transmission requests for single/multi-file (one fewer when resume
only recovers already-absent names). There are
no retries and no client mutations. `status` reads only one explicit private
journal or retained tombstone and neither reads credentials nor contacts the
client. Terminal resume also avoids credential I/O and does not require the
source parent directories: a parent removed after completion (for example by
parent cleanup) counts as absence of its retired names, while a present parent
must still be the recorded directory and must not contain a retired name.
Source-name retirement
itself does not remove a parent directory, the final, another name for a
hardlinked inode, an empty/padding entry, or a downloader job, and no report
claims reclaimed storage or rollback. Execution JSON kind is
`content.source_retirement`.

---

Previous: [Client activation, stop, and removal](client-lifecycle.md) · Next: [Reconciliation](reconcile.md)
