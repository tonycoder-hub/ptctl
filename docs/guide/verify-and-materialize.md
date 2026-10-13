# Verify, discover, plan, and materialize

[Command guide](README.md) · [Documentation index](../README.md)

> Discovery from a sealed storage snapshot (`--state-store` with `--storage-profile`) is described in [storage.md](storage.md). Retention design details are in [MATERIALIZE_RETENTION.md](../MATERIALIZE_RETENTION.md).

Verify an exact content root. v1 uses its cross-file piece stream, v2 uses
per-file Merkle trees, and hybrid requires both proofs. For a multi-file
torrent, `--content` is the directory represented by the torrent's top-level
name. For a single-file torrent, it can be the file itself or its parent.

```bash
ptctl torrent verify --content "D:\PT\Release" release.torrent
```

`bytes_verified` counts physical bytes. Per-algorithm `proof_stream_bytes` can
be larger for v1 because it includes virtual padding. Stability is explicitly
labeled non-atomic.

Find renamed or scattered bytes across several roots, prove them against the
torrent, and optionally produce a zero-write target plan:

```bash
ptctl seed discover \
  --torrent release.torrent \
  --search-root "D:\Media" \
  --search-root "E:\Archive" \
  --target "D:\PT" \
  --output json
```

Discovery has mandatory limits for roots, depth, directories, entries,
retained paths, per-file candidates, candidate edges considered, solver states,
verified alternatives, and proof work. A size match is only `candidate`; only
full v1/v2 evidence is `verified`. If any relevant scan or proof budget is
exhausted, the source
outcome is `incomplete`, even when one verified candidate was found. Two
verified layouts are `verified_ambiguous` and are never silently reduced to the
first. Absolute paths are hidden by default; use `--show-absolute-paths` only
for a private local report.

Generate a plan from an already exact torrent layout:

```bash
ptctl seed plan \
  --torrent release.torrent \
  --source "D:\Media\Release" \
  --target "D:\PT" \
  --output json
```

The standalone `seed plan` result remains a zero-write review artifact:
`effect` is `none`, readiness is `layout_only`, and `ready_to_apply` is false.
Its serialized fields are not source authority, but its `exact_root` 24-hex
plan ID can select a later `seed materialize run --source` with the same
metafile, exact source, and target. The writing invocation reopens and hashes
the source again before creating any journal:

```bash
ptctl seed materialize run \
  --torrent release.torrent \
  --source "D:\Media\Release" \
  --target "D:\PT" \
  --expect-plan-id 0123456789abcdef01234567 \
  --acknowledge-filesystem-write \
  --output json
```

For scattered or indexed sources, review `seed discover --target` with the
intended metafile, source selector, and target (as in the preceding discovery
examples), retain that discovery plan's 24-hex ID, then acknowledge the
journal, staging, and target writes:

```bash
ptctl seed materialize run \
  --torrent release.torrent \
  --search-root "D:\Media" \
  --search-root "E:\Archive" \
  --target "D:\PT" \
  --expect-plan-id 0123456789abcdef01234567 \
  --acknowledge-filesystem-write \
  --output json
```

`run` does not consume plan or discovery JSON as proof. In the same invocation
it either exactly verifies the explicit `--source`, repeats bounded live
discovery with the same selector/search-root/target shape and requires one
uniquely verified source, or repeats the exact
stored-profile/descriptor/match selection and reopens and cryptographically
verifies that chosen source map. It rebuilds the copy-only plan from
process-local proof and compares the fresh ID before creating a journal.
Stored selection never claims current uniqueness and requires all four
flags `--state-store`, `--storage-profile`, `--snapshot-record`, and
`--select-source-match` on both preview and early-phase run/resume.
It then uses a private target-root-local
journal and same-filesystem staging, exactly verifies staged and final bytes,
and publishes the top-level layout without clobber. The materialize limits are
fixed by the installed version; only the existing discovery limits are CLI
flags. Network/UNC `--search-root` values still require `--allow-network`; an
explicit `--source` may itself be remote and therefore remains an intentional
read-side-effect boundary. A target root must be a supported local filesystem.

Every report hands back a full opaque operation ID as soon as one is durably
recoverable. Recovery always selects that ID explicitly:

```bash
ptctl seed materialize status --target "D:\PT"
ptctl seed materialize status --target "D:\PT" sha256:OPERATION_DIGEST

ptctl seed materialize resume \
  --torrent release.torrent \
  --target "D:\PT" \
  --expect-plan-id 0123456789abcdef01234567 \
  --acknowledge-filesystem-write \
  --search-root "D:\Media" \
  sha256:OPERATION_DIGEST

ptctl seed materialize abandon \
  --target "D:\PT" \
  --acknowledge-abandon \
  sha256:OPERATION_DIGEST

ptctl seed materialize prune \
  --torrent release.torrent \
  --target "D:\PT" \
  --expect-plan-id 0123456789abcdef01234567 \
  --acknowledge-operation-state-deletion \
  sha256:OPERATION_DIGEST

ptctl seed materialize forget \
  --target "D:\PT" \
  --expect-plan-id 0123456789abcdef01234567 \
  --acknowledge-historical-evidence-deletion \
  sha256:OPERATION_DIGEST
```

An indexed execution uses the same reviewed plan ID:

```bash
ptctl seed materialize run \
  --torrent release.torrent \
  --state-store "D:\Private\ptctl-metafiles" \
  --storage-profile media \
  --snapshot-record sha256:DESCRIPTOR_DIGEST \
  --select-source-match sha256:MATCH_DIGEST \
  --target "D:\PT" \
  --expect-plan-id 0123456789abcdef01234567 \
  --acknowledge-filesystem-write
```

`status` without an ID performs a bounded name-only listing whose entries are
`not_inspected`; it never chooses a latest operation. `resume` re-verifies an
explicit exact root, reads fresh live roots, or repeats the explicit stored
selection only for `journaled`, `stage_created`, or `file_staged` phases.
Omitting source authority in one of those phases returns a blocked report;
after stage verification, supplied source selectors are not read because
staged/final bytes are the recovery authority. `abandon` is allowed only before publication intent. It
appends one terminal journal event and deliberately retains staging and scratch
bytes: it is not cleanup, rollback, or deletion.

`prune` is the operation-state deletion boundary. It selects exactly one full
operation ID and reviewed plan ID. A newly started committed prune
also requires the exact metafile and reverifies the current final namespace and
bytes; an abandoned operation instead proves the final name absent. It then
publishes a durable private retention intent before deleting only the original
intent, journal, scratch, and stage objects. A small exact tombstone remains,
so retries are explicit and idempotent. Prune never removes source bytes, the
published final layout, another operation, or a downloader job.

`forget` is a distinct, narrower, irreversible boundary. It accepts only the
explicit complete tombstone left by prune and requires a third acknowledgement.
Before deleting a retained marker it publishes a canonical root-level recovery
intent bound to the same target root, operation, plan, and exact tombstone; it
removes that intent last. It never touches source bytes, the published final,
downloader state, or another operation. After confirmed completion no ptctl
attribution remains, so a repeated call reports `absent_unattributed` and
returns `1` instead of inventing `already_forgotten`.

Materialize JSON has kind `content.materialization`; the ID listing uses
`content.materialization.operation_list`; prune uses
`content.materialization.retention`; forget uses
`content.materialization.forget`. Reports always keep effect, actual
and uncertain writes, phase, plan/source/target assurance, fixed limits/usage,
and non-null blocker/issue/warning arrays separate. They never expose absolute
source, target, journal, staging, or scratch paths, and have no path-disclosure
flag. Reads can still update atime, hydrate placeholders, or incur remote cost.

---

Previous: [Storage profiles and indexes](storage.md) · Next: [Site reads and bonus exchange](sites.md)
