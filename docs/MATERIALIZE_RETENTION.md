# Materialize operation retention

This document defines the first deletion-capable `ptctl` workflow. It is a
follow-on to copy-only journaled materialization, not a general filesystem
cleaner and not downloader coordination.

## Goal

`seed materialize abandon` deliberately retains staged and scratch bytes. A
committed operation also retains its private intent and journal after the final
layout has been verified. Retaining everything is the safest initial recovery
policy, but it is not a sustainable steady state.

The retention slice adds one explicit command:

```text
ptctl seed materialize prune \
  --target PATH \
  --expect-plan-id 24_HEX \
  --acknowledge-operation-state-deletion \
  [--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID] \
  OPERATION_ID
```

`prune` removes only owner-private operation state. It never removes or
rewrites a source, the published final layout, another materialize operation,
or a downloader job. It retains a small tombstone so an explicit retry can
distinguish an already-pruned operation from an operation that never existed.
The command must therefore say `pruned`, not `cleaned up`, `rolled back`, or
`deleted completely`.

## Authority and preconditions

Every invocation requires all of the following before the first mutation:

1. a canonical full operation ID;
2. the exact target-root locator and a fresh `fsbind` root identity;
3. the reviewed 24-hex plan ID from the operation report;
4. the dedicated deletion acknowledgement;
5. a complete, canonical operation journal or an already sealed retention
   marker;
6. an operation phase of exactly `committed` or `abandoned`.

For `committed`, the caller must also supply the exact metafile selector. The
variant must match the intent and the current final namespace and bytes must
pass the ordinary v1/v2/hybrid verifier in the pruning invocation. For
`abandoned`, no metafile is required, but the journal must prove that
publication intent was never entered and the current final name must still be
absent.

The external plan ID is a selection guard, not proof. A mismatch is a policy
blocker and does not make a healthy journal corrupt. A serialized report,
tombstone, journal hash, filename, object identity, size, mtime, or downloader
claim can never replace the required current final proof for a newly started
committed prune.

## State machine

The existing operation directory and operation lock remain the authority for
the whole transition. Prune adds one reserved owner-private directory:

```text
.ptctl-materialize-<digest>/retention/
    intent.json
    complete.json  # present only after exact pruning completes
```

The transition is:

```text
committed | abandoned
        |
        | exact terminal preflight
        v
publish retention/intent.json no-replace
        |
        | bounded exact deletion of heavy operation state
        v
publish retention/complete.json no-replace
        |
        v
retained tombstone
```

The durable intent marker happens before any deletion, so a crash cannot leave
a partially pruned directory looking like a normal resumable materialize
operation. Once `retention/` exists, ordinary materialize resume and abandon
must fail closed; only explicit prune/status can interpret it. An older binary
sees an unexpected namespace object and also fails closed.

The intent and completion markers use private same-directory temporary files,
no-replace rename, file and directory sync, named-entry identity rechecks, and
the existing operation lock. Publication ambiguity, binding loss, or
unconfirmed durability is preserved in the receipt. Recovery never infers
success merely because an original file or directory is absent.

## Retention marker

The operation subtree gets a dedicated owner-private `retention` directory.
Canonical intent and completion markers are published there with private
same-directory temporary files and no-replace rename. This path has a separate
fixed budget and does not consume the materialize scratch budget, because prune
must remain possible when ordinary journal scratch is full.

The marker contains no absolute path and no credential. It binds:

- schema and operation ID;
- operation-subtree and target-root identities;
- exact intent digest, terminal event ID, terminal phase, plan ID, and whole
  metafile variant ID;
- for committed operations, the final object identity and the basis
  `exact_final_verified_before_retirement`;
- for abandoned operations, the basis
  `publication_absent_before_retirement`.

The intent marker records a historical, bracketed observation. It does not
claim that the final layout is immutable or still verified at a later time. A
marker is accepted only under the same bound target root and operation-subtree
identity. The completion marker additionally binds the intent-marker digest
and is accepted only after a fresh exact audit of the retained namespace. JSON
round-tripping a public report never recreates that authority.

## Exact deletion

Deletion is handle-relative and no-follow. Before deleting an allowed subtree,
the implementation performs a bounded deterministic inventory and rejects:

- an unexpected top-level name;
- symlink, junction, reparse point, mount boundary, device, socket, pipe, or
  any other non-regular/non-directory object;
- an object outside the bound operation subtree;
- an owner/privacy mismatch;
- a regular file with more than one hard link;
- a duplicate, prefix collision, identity change, or named-entry replacement;
- any entry, depth, component, path-byte, retained-memory, content-byte, or
  wall-clock budget overflow.

After a complete preflight, regular files are removed and directories are
removed in deterministic post-order. Every removal is identity-bound,
rechecked by name, and followed by parent-directory durability confirmation.
If a removal becomes visible but durability or the final binding cannot be
confirmed, the report preserves the visible/ambiguous receipt and the next
explicit invocation resumes from the operation ID plus the intent marker.

The original intent and journal remain until the retention intent marker is
durable. The completion marker is published only after an exact audit proves
that the retained subtree contains the fixed operation lock and retention
directory/markers, and no staged content, scratch files, source locator,
original intent, or journal events.

## Reports and exits

The JSON kind remains under the materialize namespace and reports independent
facts for:

- selected operation and observed retention state;
- plan/variant match;
- current committed-final proof or unpublished-abandoned proof;
- retention-directory and intent/completion-marker publication receipts;
- files, directories, and bytes removed;
- attempted, visible, ambiguous, and durability-confirmed mutations;
- fixed limits and actual usage;
- blockers, issues, and warnings as non-null arrays.

Stable outcomes are `pruned`, `already_pruned`, `blocked`, `interrupted`, and
`integrity_failed`. Default reports never contain the target, final, operation,
journal, scratch, stage, or source path. Operation, plan, variant, marker, and
filesystem IDs are stable correlators and are not anonymity.

Exit codes follow the existing report-first contract: `0` for `pruned` or
`already_pruned`, `1` for operational interruption or an output failure, `2`
for usage, `3` for proven journal/marker/content integrity failure, and `4` for
policy/selector blockers or an explicit operation not found.

## Fixed budgets

Retention budgets are installed-version constants and are recorded in every
report. They cover both the read-only preflight and deletion work:

- root names considered;
- namespace objects and path bytes;
- conservative retained-memory bytes, directory-list allocations, and
  component references;
- directory entries and depth;
- regular bytes considered and removed;
- original intent and journal bytes;
- retention marker bytes and temporary objects;
- findings retained;
- wall-clock time.

N+1 observations are charged. A budget can make the operation incomplete but
can never authorize a partial tree to be called retained. Once the intent
marker has been published, an incomplete result remains explicitly recoverable
under the original operation ID.

## Non-goals

This slice does not:

- remove the retained tombstone itself;
- delete or retire a source layout;
- delete or modify the published final layout;
- infer that a downloader is using the final layout;
- pause, add, relocate, recheck, or resume a downloader;
- merge operations, choose the newest operation, or prune by age;
- scan all target roots automatically;
- implement quotas, background garbage collection, or cross-filesystem trash;
- weaken the no-delete semantics of `abandon`.

Deleting retained tombstones, executing a reviewed source-retirement plan, and
downloader coordination need separate explicit authorities and recovery
stories. The zero-write `seed retire plan` evidence slice does not change this
retention operation's scope.
