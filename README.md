# ptctl

`ptctl` is a conservative, content-first CLI for private BitTorrent trackers.
It treats a tracker website, a downloader, and a filesystem as separate trust
domains and reconciles them around verifiable torrent metadata.

> Status: `v0.4.0-alpha` development. Ordinary inspection, downloader reads,
> content proof, discovery, planning, and reconciliation are intentionally
> zero-write. Persistent writes are confined to explicit private-store/index
> operations, the acknowledged `site metafile fetch`, the separately
> acknowledged target-root-local `seed materialize run|resume|abandon|prune|forget`
> workflow, reviewed `client adopt run|resume|prune|forget` stopped-add or
> observation-only existing-stopped operations,
> explicit `client activate run|resume|prune|forget` recheck/start operations,
> acknowledged `client remove run|resume|prune|forget` exact-job removal that always
> retains local data, acknowledged source-name retirement, and the separate
> acknowledged `seed retire parent-cleanup run|resume|prune|forget` empty-directory boundary. The
> fetch also crosses a separate,
> tracker-visible read boundary. Materialize creates a new target layout;
> `prune` can delete only one explicitly selected operation's owner-private
> heavy state and retains its tombstone; `seed materialize forget` has a third
> acknowledgement and irreversibly removes only that exact tombstone plus its
> last recovery marker. Client adoption never mutates an
> existing job; activation is limited to the reviewed exact job's recheck and
> optional start transitions. `client adopt prune` separately seals one
> terminal completion tombstone before deleting only that operation's private
> request journal; `client adopt forget` has a third acknowledgement and
> irreversibly removes only that adoption tombstone plus its last recovery
> marker. `client activate prune` does the same for one explicitly
> selected terminal recheck/start journal while retaining the exact historical
> completion needed by source-retirement review; `client activate forget` has
> a third acknowledgement and irreversibly removes only that retained
> activation tombstone plus its last recovery marker. Outside the separately
> acknowledged source-name retirement and empty-parent-cleanup workflows, no
> listed operation overwrites, moves, rewrites, or deletes a source namespace
> or published final layout. Retirement can unlink only the reviewed exact
> source names; parent cleanup can remove only reviewed exact empty immediate
> parents; `seed retire prune` separately deletes only one
> terminal retirement operation's private journal and retains its tombstone;
> `seed retire forget` has a third acknowledgement and irreversibly removes
> that exact tombstone plus its last recovery marker. Parent cleanup can remove
> only reviewed same-identity empty immediate parents, never recursively;
> `parent-cleanup prune` replaces only one terminal private cleanup journal with
> an exact no-path tombstone, and `parent-cleanup forget` separately removes
> only that tombstone plus its last recovery marker.
> Reads may still update
> atime or hydrate an offline placeholder.

中文简介：`ptctl` 不是把 PT 网页机械地搬进终端。它以 `.torrent`、
实际文件、下载器任务和站点记录这四本账为核心，先精确校验，再生成
清晰、可审计的报告与计划。TJUPT 是首个实验性只读站点适配器，而不是写死
在核心里的唯一站点。

## Why this shape?

The durable PT workflow is:

1. discover a release on a site;
2. preserve the exact metafile variant;
3. locate bytes on one or more storage systems;
4. verify v1 pieces and/or v2 Merkle commitments;
5. build a seedable layout without overwrite or deletion;
6. map the host path to the downloader's view;
7. reconcile the site, downloader, metafile, and storage records.

Bonus shops, invites, comments, and uploads are site-specific actions. They are
capabilities at the edge, not assumptions in the core domain model.

## Implemented today

- strict, bounded bencode parsing;
- exact v1 infohash calculation from the original `info` byte slice;
- exact whole-metafile SHA-256 variant identity, kept distinct from infohashes;
- a versioned private metafile store with explicit initialization, exact-byte
  import, content-addressed no-clobber objects, and verified inspection;
- immutable storage profiles plus bounded, deterministic, streaming filesystem
  inventory snapshots sealed into the same owner-only private store;
- descriptor-last snapshot publication, domain-separated record IDs, bounded
  latest-generation selection, root-identity invalidation, and live
  identity-bound reobservation of historical locators;
- explicit sealed-snapshot source-map selection for materialization, with the
  profile, descriptor, generation, and exact match bound into the reviewed
  plan while every selected locator is reopened and cryptographically verified
  again in the writing invocation;
- one mutually exclusive metafile selector across inspect, verify, seed, and
  reconciliation commands: an ordinary file, or an initialized store plus its
  whole-metafile variant ID;
- structural v1/v2 validation, including cryptographic reduction of each v2
  piece layer back to its file `pieces root`;
- hybrid inspection that parses both layouts and rejects disagreements;
- exact v1 SHA-1 verification across multi-file boundaries;
- streaming v2 SHA-256 Merkle verification with 16 KiB leaves and BEP 52 EOF
  padding rules, independently for each file;
- conjunctive hybrid verification: one physical read feeds both the v1 piece
  stream and v2 file tree for every file;
- virtual zeros for v1 padding files; v2 padding and symbolic-link leaves fail
  closed until their distinct semantics are implemented;
- bounded, deterministic discovery across repeatable storage roots, retaining
  only regular files of required sizes and never following symlinks, Windows
  reparse points, or mount boundaries;
- exact scattered-source matching: v1 candidates are pruned at cross-file
  piece boundaries, v2 candidates by file Merkle root, and every survivor
  passes the ordinary authoritative verifier;
- stable source outcomes (`verified_unique`, `verified_ambiguous`, `not_found`,
  or `incomplete`) kept separate from optional target/client handoff state;
- read-only storage probing and explicit host-to-client path mapping;
- zero-write, `layout_only` plans for v1, v2, and hybrid metafiles, bound to a
  detected-stable verification observation with apply-time re-verification
  requirements; an `exact_root` plan ID can select the later acknowledged
  `--source` write while serialized plan fields themselves remain non-authority;
- acknowledged, copy-only `seed materialize run|resume` with a private
  target-root-local journal, same-filesystem staging, exact stage/final proof,
  no-clobber publication, fixed hard limits, explicit operation-ID recovery,
  bounded status/listing, terminal no-delete abandon, and separately
  acknowledged exact pruning of one terminal operation's private state while
  retaining a durable tombstone, followed only on explicit request by
  recoverable, identity-bound deletion of that tombstone and its last
  attribution marker;
- tracker output reduced to origins so announce passkeys are not printed;
- traversal, separator, Windows device-name, case-collision, and conservative
  Unicode-normalization checks;
- typed, capability-checked site ports instead of a monolithic driver;
- an experimental TJUPT session check, torrent search, torrent-detail
  observation, and bonus catalog parser
  through one bounded, same-origin HTTPS GET per invocation, with fail-closed
  page recognition, no redirect, and no retry; detail requests deliberately
  omit NexusPHP's view-counting `hit` parameter and remain site claims only;
- an explicitly acknowledged TJUPT metafile fetch for one remote ID, using one
  bounded GET with no redirect or retry and publishing the strictly validated
  exact response only into an initialized private metafile store;
- qBittorrent and Transmission status and torrent-list reads over HTTPS (or
  explicit numeric loopback HTTP), with passwords accepted only through stdin;
- read-only reconciliation that can first observe one authenticated live site
  detail page, then brackets storage proof with two snapshots from one audited
  read-only downloader session, accepts either bounded discovery or one
  explicitly selected exact layout, stream-decodes a bounded job ledger, and
  reports variant, infohash, content-proof, and path relations as separate
  evidence axes; an exact layout proves only the selected root, never
  filesystem-wide uniqueness; an explicit materialize operation/plan can add a
  current `verified_materialized_final` relation only through an opaque
  same-invocation final-to-source bridge; qBittorrent supplies typed v1/v2 magnet claims,
  and Transmission supplies only its full SHA-1/v1 `hash_string` claim;
- bounded qBittorrent or Transmission per-file ledgers for one uniquely
  identified ordinary multi-file job, with stable index, size, selection,
  completion, and per-binding host-to-client path checks, including physical
  zero-length files when an exact-layout observation binds their names and
  identities;
- explicitly acknowledged exact qBittorrent or Transmission stopped-job
  adoption downstream of a current materialized-final proof: the default action
  gates one non-retried stopped add on typed queue absence, while the separate
  observation-only action records one already-present exact stopped job using
  two same-session typed/path observations around exact final re-verification
  and sends no client mutation; an explicit prior terminal completion can
  authorize a separate new stopped-add lineage only after another complete
  queue-absence observation and a dedicated acknowledgement, while preserving
  the prior evidence; Transmission is intentionally v1-only because its RPC
  ledger has no typed v2 identity;
- explicit read-only reconciliation of one canonical terminal client-adoption
  journal or retained tombstone with the current exact typed job claim from
  the existing downloader bracket and the current exact materialized final;
  this adds no request, preserves five relations, and does not claim that the
  current job is the same remote incarnation as the historically adopted job;
- explicit qBittorrent or Transmission recheck and optional controlled start
  downstream of a canonical same-driver stopped-adoption completion, with
  version-bound qB v4/v5 routes and Transmission v5.3/v6 methods,
  durable per-request intent, exact typed job/per-file layout reobservation,
  current-final re-verification, and no automatic replay of unknown requests;
  Transmission control remains v1-only;
- explicitly reviewed qBittorrent or Transmission removal of one current exact
  typed-identity job while retaining all local data, with a durable request
  intent, no fan-out or automatic retry, complete queue-absence observation,
  and another exact materialized-final verification before completion;
  Transmission remains v1-only;
- explicit read-only reconciliation of one canonical terminal keep-data
  removal journal or retained tombstone with current typed queue absence in
  the existing two-read client bracket and current exact materialized-final
  content; historical removal, current absence, and local bytes remain
  separate, and a causality-unproven completion cannot reconcile;
- zero-write source-retirement eligibility planning from a new complete live
  source proof, current exact materialized-final proof, and canonical terminal
  client activation journal, plus stable before/after reads of the exact live
  typed-infohash job and its effective paths from one qBittorrent or
  Transmission session, with explicit final-overlap/alias rejection and no
  deletion authority; Transmission remains v1-only;
- separately acknowledged, journaled source-name retirement that reproduces
  the same live plan, binds exact source parents/names and identities, records
  durable per-name attempts/completions, supports explicit crash recovery, and
  reverifies the final and live client use without deleting directories,
  aliases, empty/padding entries, or downloader jobs;
- separately acknowledged pruning of one terminal source-retirement journal
  into an exact historical tombstone, followed only on explicit request by
  recoverable, identity-bound deletion of that tombstone and its final
  attribution marker;
- explicit read-only reconciliation of one terminal source-retirement lineage,
  keeping historical completion, a same-invocation two-pass observation of the
  exact retired names currently absent, current downloader use, and current
  materialized-final content proof separate; pruned tombstones remain
  historical only and a reappeared retired name is a current conflict;
- credential-free, zero-write planning for exact immediate parents left by one
  live terminal source-retirement journal: retired names are proved absent in
  the original root scope, search roots and ancestors are protected, child
  names are never reported, and only same-identity parents observed empty twice
  become review candidates; the serialized plan grants no deletion authority;
- separately acknowledged, journaled execution of that exact parent-cleanup
  review: the writing invocation repeats the live review, preflights every
  candidate before its first write, removes only same-identity immediate
  parents that are still empty, and supports explicit resume and historical
  status without recursive ancestor cleanup;
- separately acknowledged pruning of one terminal parent-cleanup journal into
  an exact no-path historical tombstone, followed only on explicit request by
  recoverable identity-bound deletion of that tombstone and its final root
  attribution marker;
- explicit read-only reconciliation of one terminal parent-cleanup lineage,
  keeping its historical completion and a same-invocation two-pass observation
  of the exact removed parent names separate from source retirement, current
  downloader use, and current final-content proof; a live cleanup journal can
  conservatively imply retired-name absence even after the retirement journal
  was pruned, while a pruned cleanup tombstone remains historical only;
- versioned experimental JSON envelopes (`ptctl.dev/v1`) and control-safe
  human-readable tables.

Not implemented yet: current-filesystem negative/uniqueness proofs from an
index alone, background refresh/watchers, downloader pause/location or broader
existing-job mutation,
client-layout reconciliation for attributed file semantics such as padding or
symlink leaves and for zero-length files without an observed physical binding,
  reflink/hardlink or cross-filesystem materialization, automatic execution of
  serialized plan reports, recursive or policy-selected source-parent cleanup,
  staging cleanup or rollback, published-layout deletion, site
writes, browser login, third-party executable plugins, ratio manipulation, or
Cloudflare bypass.

## Install

Requires Go 1.24 or newer.

```bash
go install github.com/tonycoder-hub/ptctl/cmd/ptctl@latest
```

For a local checkout:

```bash
go build -trimpath -o ptctl ./cmd/ptctl
go test ./...
```

## Quick tour

Inspect a metafile without exposing its announce path or query string:

```bash
ptctl torrent inspect release.torrent
ptctl torrent inspect --output json release.torrent
```

Preserve a private metafile as an immutable exact-byte artifact. Initialization
is explicit because the store must enforce private permissions and atomic
no-clobber commits. Import copies the accepted raw bytes and leaves the source
file in place without rewriting or deleting it; the source read may still
update atime, hydrate a placeholder, or incur remote-filesystem cost:

```bash
ptctl metafile store init --store "D:\Private\ptctl-metafiles"
ptctl metafile store import --store "D:\Private\ptctl-metafiles" release.torrent
ptctl metafile store inspect \
  --store "D:\Private\ptctl-metafiles" \
  sha256:WHOLE_METAFILE_SHA256
```

Fetch one exact private metafile directly into that initialized store. This is
an effectful operation: the tracker may record the GET, so the acknowledgement
must be explicit. All usage, the single remote ID, limits, capability, and
store assurances are checked before the cookie is read from stdin:

```bash
printf '%s' "$TJUPT_COOKIE" | ptctl site metafile fetch \
  --cookie-stdin \
  --acknowledge-site-effect \
  --metafile-store "D:\Private\ptctl-metafiles" \
  tjupt REMOTE_ID
```

The command performs no torrent-detail lookup and sends at most one GET. It
follows no redirect, performs no retry, and offers no raw stdout or arbitrary
destination. The validated response goes through the ordinary exact-byte,
no-clobber store publication path. Its report may record the invocation-scoped
`observed_exact_variant` relation only when that store import explicitly
returns a valid exact artifact reference and its whole-response digest and
consumed-byte receipt agree. A pre-publication or import failure retains only
the bounded request/response receipt; the CLI never hashes the response again
to promote it into a site-to-variant observation. Once an exact reference has
been established, a later durability or post-publication failure does not erase
that invocation-only observation.

After, and only after, a fully successful private artifact import, the command
publishes a sealed `site.metafile.binding.v1` record in the same store. The
record is a binding-last commit marker for one historical exact response. Its
record ID is printed as the explicit handoff to reconciliation. Publication
rechecks the sealed record and referenced private artifact under one
operation-bound physical store identity; it is not a two-object transaction,
a site signature, or proof that the site's current mapping is unchanged. A
new acknowledged GET creates a new historical observation even when it returns
the same variant. Bindings can be audited later without credentials:

```bash
ptctl site metafile binding list \
  --metafile-store "D:\Private\ptctl-metafiles"

ptctl site metafile binding inspect \
  --metafile-store "D:\Private\ptctl-metafiles" \
  sha256:SEALED_BINDING_RECORD_ID
```

`list` is a bounded, deterministic name inventory. It returns unverified
record locators only, never reads their payloads or artifacts, and never
selects a latest observation. `inspect` accepts one explicit record ID and
jointly re-hashes, strictly parses, and privacy-checks that record's linked
private metafile under one bound store session. Its result remains historical;
it does not contact the site or claim that the current remote mapping is
unchanged. JSON kinds are `site.metafile.binding.list` and
`site.metafile.binding.inspect`.

The artifact ID hashes the complete raw `.torrent` byte stream, not just its
`info` dictionary. Two private variants with the same infohash therefore remain
different artifacts. Import is idempotent: an already present, byte-identical
variant succeeds without another write. The store is permission-isolated, not
encrypted; anyone who can read the store can read the embedded announce
passkey. Store and import-source absolute paths are hidden unless
`--show-absolute-paths` is explicitly requested; object paths are never
emitted.

No-clobber publication and durability confirmation are distinct. If the object
was completely published but the following directory durability check fails,
the report uses `published_durability_unconfirmed` and `writes_performed` may be
`1`; the complete object may remain visible. `ptctl` never reports that case as
zero-write or removes the published object as rollback.

Before creating store layout entries or staging an artifact, the operation
binds one reviewed root identity. The root, `objects`, `tmp`, staging file, and
final object must then remain on that same reviewed local filesystem; POSIX uses
handle-relative operations and Windows pins the path namespace with no-delete
directory guards. A rename, replacement, mount change, or volume change fails
closed. Volatile memory filesystems such as tmpfs and ramfs are not accepted as
durable stores.

Publication assurance is invocation-scoped. A newly initialized store or newly
stored object can report `confirmed_this_invocation`. Read-only inspection and
idempotent `already_*` outcomes verify current bytes and privacy, but label the
historical publication as unobservable instead of inferring an old no-clobber
or durability event. A failure after publication and durability but before all
post-commit checks is reported as `published_post_commit_failure`.

Every existing metafile consumer accepts exactly one source. Legacy file forms
remain unchanged. The stored form replaces the positional file or `--torrent`
with the following pair:

```bash
ptctl torrent inspect \
  --metafile-store "D:\Private\ptctl-metafiles" \
  --metafile-variant sha256:WHOLE_METAFILE_SHA256

ptctl seed discover \
  --metafile-store "D:\Private\ptctl-metafiles" \
  --metafile-variant sha256:WHOLE_METAFILE_SHA256 \
  --search-root "D:\Media"
```

An explicitly selected historical site binding can be added only when
reconciliation reads its metafile from that same private store:

```bash
ptctl reconcile report \
  --metafile-store "D:\Private\ptctl-metafiles" \
  --metafile-variant sha256:WHOLE_METAFILE_SHA256 \
  --site-binding-record sha256:SEALED_BINDING_RECORD_ID \
  --search-root "D:\Media"
```

`--site-ref SITE/REMOTE_ID` is an optional expected-ref cross-check. By itself
it remains `declared_unbound`. A binding record cannot be paired with
`--torrent`, loaded from a separate store, or selected implicitly by
enumeration/latest. The name-only list exists solely to hand an explicit record
ID to inspect or reconciliation.

The same pair is supported by `torrent verify`, `seed discover`, `seed plan`,
`seed materialize run|resume`, and `reconcile report`. Supplying only half the
pair, mixing it with a file/`--torrent`, or adding a positional metafile to the
stored form is invalid usage. Loading from the store rechecks the object digest
and parses the exact bytes before invoking the same core. Inspection,
verification, discovery, planning, and reconciliation consumers remain
zero-write; materialize still requires its independent filesystem-write
acknowledgement.

Create an immutable filesystem scope and explicitly refresh its private index.
The state store is the same initialized, owner-only store format used for
private metafiles; profile and index record IDs are domain-separated from
metafile artifact IDs:

```bash
ptctl storage profile create \
  --state-store "D:\Private\ptctl-metafiles" \
  --name media \
  --search-root "D:\Media" \
  --search-root "E:\Archive"

ptctl storage index refresh \
  --state-store "D:\Private\ptctl-metafiles" \
  --profile media

ptctl storage index inspect \
  --state-store "D:\Private\ptctl-metafiles" \
  --profile media
```

Profiles bind exact roots, platform/path semantics, mount/network policy, and
scan budgets. They are immutable: changing that declaration requires a new
profile name. A profile can be inspected on another operating system, but
live refresh/query rejects it before interpreting native path bytes; current-
platform roots must also remain absolute and clean. Profile scan limits cannot
exceed the sealed-index encoder's file, path, or component capacity. Refresh
walks regular files deterministically without following links, reparse points,
or mount boundaries. It streams a bounded NDJSON data
record first and publishes a small descriptor only after the enumeration and
data publication are complete. After descriptor publication, both immutable
records are digest-verified together under one operation-bound physical store
root before the result can be `stored`. A failed descriptor publication can
leave an orphan data record, but latest selection lists descriptors only. Concurrent
writers that produce the same maximum generation are reported as ambiguous;
they are never ordered by wall-clock time.

Use a sealed snapshot as a bounded candidate source without scanning every
directory again:

```bash
ptctl seed discover \
  --torrent release.torrent \
  --state-store "D:\Private\ptctl-metafiles" \
  --storage-profile media \
  --output json
```

Each retained historical locator is resolved beneath the current profile root,
root/file identity is reobserved, and survivors still pass the ordinary exact
v1/v2/hybrid verifier. Without an explicit selection, `source_outcome` remains
`incomplete`, even when one candidate is currently `verified`: a historical
snapshot cannot prove that no new, removed, or renamed alternative exists now.
Unselected snapshot mode therefore never emits a materialization plan, never
reports `not_found` or `verified_unique`, and never makes reconciliation
`consistent`.

For copy-only materialization, a user may explicitly choose one currently
verified source-map ID from one explicit immutable descriptor:

```bash
ptctl seed discover \
  --torrent release.torrent \
  --state-store "D:\Private\ptctl-metafiles" \
  --storage-profile media \
  --snapshot-record sha256:DESCRIPTOR_DIGEST \
  --select-source-match sha256:MATCH_DIGEST \
  --target "D:\PT" \
  --output json
```

That result is `verified_selected`, not `verified_unique`. The selection-scope
ID binds the immutable profile, snapshot generation, descriptor record, and
exact match into the plan ID. The selected locator set is reopened and exactly
verified in that invocation; a serialized report has no authority. New or
unindexed alternatives remain unobserved but do not make the explicitly chosen
exact source unsafe to copy. Changing the descriptor, match, locator identity,
or target requires a new review. Use ordinary same-call `--search-root`
discovery whenever current uniqueness or absence itself is required.
If a safety budget stops evaluation of other historical assignments after the
selected map has been proved, only that selected map remains authorized; the
report stays explicit about incomplete alternative enumeration.

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

Map a host path to a Dockerized downloader namespace:

```bash
ptctl storage map \
  --host-root "D:\PT" \
  --client-root /downloads \
  --client-style posix \
  "D:\PT\Release"
```

Read TJUPT without putting a cookie in shell history. Stdin must contain the
complete `Cookie` header value from a session you control. Interactive TTY
secret input is refused. Do not paste the value into issues, logs, or chat.

```bash
printf '%s' "$TJUPT_COOKIE" | ptctl site status --cookie-stdin tjupt
printf '%s' "$TJUPT_COOKIE" | ptctl site search --cookie-stdin tjupt "Ubuntu"
printf '%s' "$TJUPT_COOKIE" | ptctl site detail --cookie-stdin tjupt REMOTE_ID
printf '%s' "$TJUPT_COOKIE" | ptctl site bonus-catalog --cookie-stdin tjupt
```

Each TJUPT command performs at most one bounded GET and never retries. Do not
loop or parallelize site reads. `site detail` also refuses redirects and sends
only `details.php?id=REMOTE_ID`: it does not send `hit=1`, follow the download
link, fetch the metafile, or persist an observation. Its display title, optional
peer counts, and matching internal link are current site claims, not metafile
identity or storage-content proof.

Read downloader state (read-only commands support qBittorrent and
Transmission):

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client status \
  --driver qbittorrent \
  --url https://seedbox.example \
  --username admin \
  --password-stdin

printf '%s' "$TRANSMISSION_PASSWORD" | ptctl client list \
  --driver transmission \
  --url https://seedbox.example/transmission/rpc \
  --username admin \
  --password-stdin
```

Adopt one exact materialized layout into qBittorrent or Transmission without
starting it. The default action adds an absent stopped job:

```bash
# First review the deterministic adoption plan. MATERIALIZE_PLAN_ID is the
# reviewed seed discover --target ID used by the committed materialize run.
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client adopt plan \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --host-root 'D:\' \
  --client-root /downloads \
  --client-style posix \
  --driver qbittorrent \
  --url https://seedbox.example \
  --username admin \
  --password-stdin \
  --output json

printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client adopt run \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --host-root 'D:\' \
  --client-root /downloads \
  --client-style posix \
  --driver qbittorrent \
  --url https://seedbox.example \
  --username admin \
  --password-stdin \
  --expect-adoption-plan-id ADOPTION_PLAN_ID \
  --acknowledge-client-add \
  --output json
```

If the exact job already exists in stopped state at the reviewed final path,
select the distinct observation-only action in both plan and run:

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client adopt plan \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin \
  --adopt-existing-stopped \
  --output json

printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client adopt run \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin \
  --adopt-existing-stopped \
  --expect-adoption-plan-id ADOPTION_PLAN_ID \
  --acknowledge-existing-stopped-adoption \
  --output json
```

For Transmission, use `--driver transmission`, its full RPC URL (for example
`https://seedbox.example/transmission/rpc`), and the Transmission credential.
Only v1 metafiles are eligible: Transmission's audited `hash_string` claim is
a complete SHA-1/v1 identity, but the RPC does not expose a typed v2 hash.

The default action is intentionally a one-way, stopped-add handoff. It requires
the exact raw metafile from the owner-only metafile store, a current exact proof
of the committed (or retained) materialized final, and a complete typed-infohash
queue observation proving that the target job is absent. It then publishes a
private target-root-local request intent before exactly one add POST. The POST
submits the exact stored bytes with the reviewed save path and requests a
stopped/paused job. It never changes an existing job, moves data, starts a
recheck, resumes transfer, deletes content, or retires the source.

The normal qBittorrent run uses one login, one complete ledger read before the
request, one add POST, and one complete ledger read after it. Transmission uses
its fixed two-request CSRF/version handshake followed by the same
before/add/after sequence, for five requests total. Success additionally
requires
one unique exact typed-infohash job in a stopped state, the reviewed size and
lexical save/content paths, a second exact verification of the materialized
final, and a durable completion marker. Its outcome is
`adopted_pending_client_recheck`, not “seeding verified”. Neither client proves
the raw private variant it stored, and no client recheck is performed.

Observation-only adoption is a separate plan action and acknowledgement. It
requires one already-present unique exact typed-infohash job with the reviewed
size, stopped state, and exact lexical save/content paths. In one read session,
ptctl records a first complete job-ledger observation, freshly re-verifies the
exact materialized final, then records a later complete observation of the same
opaque job and state. qBittorrent therefore uses one login plus two ledger
reads; Transmission uses its fixed two-request handshake plus two ledger reads.
The action sends no add/recheck/start/move request, does not load the one-shot
raw-metafile submission payload, and cannot observe which private metafile
wrapper originally created the job. Its outcome is
`existing_stopped_job_adopted_pending_client_recheck`. The bracket is serial
and non-atomic; it does not prove a downloader job incarnation or that the
client has checked the bytes. Adoption checks only the job-level size and
lexical paths. The separate activation workflow performs the bounded per-file
layout observations before it can recheck or start a multi-file job.

If that exact terminal job later disappears, ptctl does not silently reuse the
old operation or choose a latest completion. A new `plan`, `run`, or `resume`
may instead supply the exact pair
`--prior-adoption-operation PRIOR_OPERATION_ID` and
`--prior-adoption-plan-id PRIOR_PLAN_ID`. The local canonical completion or
retention tombstone is verified before password stdin or network access and
must describe the same driver, client configuration, path mapping, exact
metafile, materialized final, and typed identity. The current complete client
ledger must again prove exact absence. The resulting plan binds the prior
operation, plan, and completion marker into a new independent operation ID;
execution additionally requires `--acknowledge-client-re-adoption` alongside
`--acknowledge-client-add`. The prior journal or tombstone remains intact.
This flow does not infer why the job disappeared, attribute a removal, or
authorize mutation of any remaining job. A forgotten prior completion cannot
authorize re-adoption.

Transmission accepts only an explicit `torrent_added` response; its documented
`torrent_duplicate` success envelope is treated as a rejected adoption, not as
evidence that this invocation created the observed job. The modern and legacy
envelopes follow the official
[current RPC specification](https://github.com/transmission/transmission/blob/main/docs/rpc-spec.md#34-adding-a-torrent)
and
[Transmission 4.0.6 RPC specification](https://github.com/transmission/transmission/blob/4.0.6/docs/rpc-spec.md#34-adding-a-torrent).
If a Transmission add response is lost, a later same-hash job is not
automatically attributed to the request. The user may remove that
external/conflicting job and explicitly repeat the stopped add, but ptctl will
not mutate it.

If the POST response or after-ledger is lost, the durable attempt remains
`request_result_unknown`. Resume first reads the current queue and never
repeats the POST automatically:

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client adopt resume \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --host-root 'D:\' \
  --client-root /downloads \
  --client-style posix \
  --driver qbittorrent \
  --url https://seedbox.example \
  --username admin \
  --password-stdin \
  --expect-adoption-plan-id ADOPTION_PLAN_ID \
  sha256:ADOPTION_OPERATION_DIGEST

# Only if a second POST is deliberately accepted:
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client adopt resume \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --host-root 'D:\' \
  --client-root /downloads \
  --client-style posix \
  --driver qbittorrent \
  --url https://seedbox.example \
  --username admin \
  --password-stdin \
  --expect-adoption-plan-id ADOPTION_PLAN_ID \
  --acknowledge-client-add \
  --acknowledge-repeat-add \
  sha256:ADOPTION_OPERATION_DIGEST

ptctl client adopt status \
  --target "D:\PT" \
  sha256:ADOPTION_OPERATION_DIGEST

# Once adoption has an exact terminal completion, retain that authority while
# deleting only the selected operation's heavy request-attempt journal.
ptctl client adopt prune \
  --target "D:\PT" \
  --expect-adoption-plan-id ADOPTION_PLAN_ID \
  --acknowledge-operation-state-deletion \
  --output json \
  sha256:ADOPTION_OPERATION_DIGEST

# After no downstream workflow needs this historical adoption authority,
# irreversibly erase the exact tombstone and its final attribution marker.
ptctl client adopt forget \
  --target "D:\PT" \
  --expect-adoption-plan-id ADOPTION_PLAN_ID \
  --acknowledge-historical-evidence-deletion \
  --output json \
  sha256:ADOPTION_OPERATION_DIGEST
```

`status` is local and read-only: it never reads a password or contacts the
client, and therefore reports current client state as unobserved. It validates
canonical marker presence but does not issue a directory sync, so only an
effectful resume refreshes journal durability before relying on those markers.
An operation ID is deterministic from the reviewed adoption plan; no command
enumerates or chooses a “latest” operation. JSON kind `client.adoption` keeps declared
effects, actual/uncertain journal writes, request counts, before/after typed
identity states, current-final proof basis, and non-null findings separate.
Re-adoption reports expose only the opaque prior operation, plan, and
completion IDs; planning/execution also records the explicit historical-read
effect, while later local status does not pretend to re-read the prior proof.
No report copies the prior job locator or paths.
Raw host/client paths, endpoint, username, password, opaque downloader job key, magnet
URI, tracker URL, passkey, and raw metafile bytes never enter the report.

`prune` is a separate local-only deletion boundary. It requires the full
operation ID, the reviewed adoption plan ID, and
`--acknowledge-operation-state-deletion`; it accepts no client credential and
makes no network request. Before deletion it copies the canonical intent,
bounded attempt chain, completion, and their domain-separated IDs into a
durable owner-private retention intent. It then removes only the selected
operation's original markers and empty scratch directory and publishes a
retention completion. For either built-in driver, the resulting two-marker
tombstone remains usable by `client activate` only after a same-invocation
bound read recreates opaque `VerifiedCompletion` authority. The activation
driver must match the adoption driver; Transmission authority remains limited
to a complete typed v1 hash.
JSON or a copied public observation cannot do so. An intent-only crash state
blocks ordinary resume and is recoverable only by repeating the same explicit
prune selector. JSON kind is
`client.adoption.retention`; `pruned` and `already_pruned` return `0`.

Adoption `forget` is a third, narrower irreversible boundary. It accepts only
the full operation ID, reviewed adoption plan ID, and
`--acknowledge-historical-evidence-deletion`; it reads no credential and makes
no network request. Before touching the tombstone it copies the exact retained
intent and completion, including the bounded attempt chain, into a deterministic
owner-private root recovery intent. It then removes only those retained
markers, their empty directory, and the exact operation subtree, confirms
durable absence, and removes the last root intent. Staged, published, and
operation-removed crash states are recovered only by repeating the same
selector. While either intent exists, status reports `forgetting` and resume,
completion proof, and prune stop before client access. After final removal a
repeat reports `absent_unattributed`, not historical success. JSON kind is
`client.adoption.forget`. Forget cannot revoke a process-local
`VerifiedCompletion` already issued to another live caller.

Recheck that adopted job, then optionally start it only after a durable
completion observation:

The example uses qBittorrent. For a Transmission v1 adoption, use
`--driver transmission`, the full RPC URL, and the Transmission credential;
the plan binds that driver and cannot be replayed against the other adapter.

```bash
# Review. Add --start-after-recheck to include the optional start transition
# in the plan ID.
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client activate plan \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --adoption-operation sha256:ADOPTION_OPERATION_DIGEST \
  --adoption-plan-id ADOPTION_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin --start-after-recheck --output json

# Run authorizes only one recheck request. A reviewed start is always a
# separate later resume so one invocation never sends two effectful POSTs.
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client activate run \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --adoption-operation sha256:ADOPTION_OPERATION_DIGEST \
  --adoption-plan-id ADOPTION_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin --start-after-recheck \
  --expect-activation-plan-id ACTIVATION_PLAN_ID \
  --acknowledge-client-recheck --output json

# After qBittorrent has been observed complete and stopped, start is still a
# separate explicit acknowledgement because it may announce or transfer data.
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client activate resume \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --adoption-operation sha256:ADOPTION_OPERATION_DIGEST \
  --adoption-plan-id ADOPTION_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin --start-after-recheck \
  --expect-activation-plan-id ACTIVATION_PLAN_ID \
  --acknowledge-client-start sha256:ACTIVATION_OPERATION_DIGEST

ptctl client activate status --target "D:\PT" \
  sha256:ACTIVATION_OPERATION_DIGEST

# Once activation is terminal, retain its exact historical completion while
# deleting only this operation's original recheck/start marker chain.
ptctl client activate prune \
  --target "D:\PT" \
  --expect-activation-plan-id ACTIVATION_PLAN_ID \
  --acknowledge-operation-state-deletion \
  --output json \
  sha256:ACTIVATION_OPERATION_DIGEST

# After downstream retention is no longer needed, irreversibly erase this one
# exact activation tombstone and its final historical attribution.
ptctl client activate forget \
  --target "D:\PT" \
  --expect-activation-plan-id ACTIVATION_PLAN_ID \
  --acknowledge-historical-evidence-deletion \
  --output json \
  sha256:ACTIVATION_OPERATION_DIGEST
```

Activation never treats a successful POST as a completed recheck. It records
the request intent first, then requires either a durable observation of a
checking state followed by a complete stopped observation, or an incomplete to
complete transition bracketed by the same invocation's request. A fast recheck
that starts and finishes between observations can therefore remain
`recheck_request_result_unknown`; repeating it requires both
`--acknowledge-client-recheck` and `--acknowledge-repeat-recheck`. Start has the
same non-replay rule and its own repeat acknowledgement. qBittorrent 4.x uses
the reviewed `resume` route while 5.x uses `start`; an unknown major is
unsupported rather than guessed. Transmission RPC 5.3 binds the legacy
`torrent-verify`/`torrent-start` methods, while RPC 6 binds
`torrent_verify`/`torrent_start`; both use a one-element full SHA-1 hash-string
selector and never replay an expired CSRF request. See the official
[current Transmission action specification](https://github.com/transmission/transmission/blob/main/docs/rpc-spec.md#31-torrent-action-requests)
and [Transmission 4.0.6 action specification](https://github.com/transmission/transmission/blob/4.0.6/docs/rpc-spec.md#31-torrent-action-requests).
Completion remains a bracketed client claim
plus a same-invocation exact final proof, not proof of a raw private variant or
an atomic client/filesystem snapshot. Each invocation sends at most one
effectful client POST. JSON kind is `client.activation`.

Activation `prune` is a distinct local-only deletion boundary. It takes one
full activation operation ID, the reviewed activation plan ID, and
`--acknowledge-operation-state-deletion`; it accepts no downloader credential
and performs no network request. Before deleting anything, it seals the exact
canonical activation intent, terminal completion, and a bounded manifest of
every original marker's name, domain-separated ID, and size into an
owner-private retention intent. It then removes only those exact marker files
and the empty scratch directory, audits the remaining namespace, and publishes
a retention completion. An intent-only crash state blocks ordinary resume and
is advanced only by repeating the same explicit prune selector. A complete
tombstone can recreate process-local `clientactivate.VerifiedCompletion`
authority for source-retirement review through a fresh bound read; copied JSON
cannot. It is historical evidence, not proof of current downloader state.
JSON kind is `client.activation.retention`; `pruned` and `already_pruned`
return `0`.

Activation `forget` is a third, narrower irreversible boundary. It accepts
only the same full operation ID and reviewed activation plan ID plus
`--acknowledge-historical-evidence-deletion`; it reads no credential and makes
no network request. Before touching the tombstone it copies the exact retention
intent and completion into a deterministic owner-private root recovery intent.
It then removes only the two retained markers, their empty directory, and the
exact operation subtree, rechecks durable absence, and finally removes the last
root intent. A crash after staging, root publication, or operation removal is
recoverable only by repeating the same explicit forget selector. While either
intent is visible, status reports `forgetting` and ordinary resume, completion
proof, and prune stop before client access. After confirmed last-marker removal
there is intentionally no idempotence evidence: a repeated call reports
`absent_unattributed`, never `already_forgotten`. JSON kind is
`client.activation.forget`. Deleting durable evidence cannot revoke an opaque
`VerifiedCompletion` capability that was already issued in another live call.

Remove that exact current downloader job while deliberately retaining the
materialized files:

```bash
# Review only. The plan binds the current activation, exact typed job and file
# layout, client mapping/configuration, materialized final, and protocol route.
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client remove plan \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --activation-operation sha256:ACTIVATION_OPERATION_DIGEST \
  --activation-plan-id ACTIVATION_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin --output json

# Execute exactly the reviewed keep-data removal.
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client remove run \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --activation-operation sha256:ACTIVATION_OPERATION_DIGEST \
  --activation-plan-id ACTIVATION_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin \
  --expect-removal-plan-id REMOVAL_PLAN_ID \
  --acknowledge-client-removal --output json

ptctl client remove status --target "D:\PT" \
  --expect-removal-plan-id REMOVAL_PLAN_ID \
  sha256:REMOVAL_OPERATION_DIGEST

# Compact only this terminal operation's private request journal. No password
# or downloader connection is used.
ptctl client remove prune --target "D:\PT" \
  --expect-removal-plan-id REMOVAL_PLAN_ID \
  --acknowledge-operation-state-deletion \
  sha256:REMOVAL_OPERATION_DIGEST

# Irreversibly erase only that compact historical tombstone.
ptctl client remove forget --target "D:\PT" \
  --expect-removal-plan-id REMOVAL_PLAN_ID \
  --acknowledge-historical-evidence-deletion \
  sha256:REMOVAL_OPERATION_DIGEST
```

The removal port has no delete-data option. qBittorrent receives exactly one
`hashes` value with `deleteFiles=false`; Transmission receives exactly one full
v1 hash with `delete-local-data=false` (RPC 5.3) or
`delete_local_data=false` (RPC 6). See the official
[qBittorrent WebUI API](https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-%28qBittorrent-5.0%29#delete-torrents)
and [Transmission RPC specification](https://github.com/transmission/transmission/blob/main/docs/rpc-spec.md#31-torrent-action-requests).
HTTP/2, connection reuse, redirects, proxy use, fan-out, and automatic replay
are disabled. A successful response is kept separate from proof: completion
requires a later complete typed ledger to show the exact job absent and then a
fresh exact verification of the materialized final. If the request result is
unknown, resume checks absence first and attributes no causality; repeating a
still-present removal requires both `--acknowledge-client-removal` and
`--acknowledge-repeat-removal`. Local-only `status` reports historical marker
state and never claims that the queue or files are currently unchanged. JSON
kind is `client.removal`. If source-name retirement is desired, complete that
separate live-client proof and acknowledged transition first; removal never
infers or authorizes source retirement.

Removal pruning is a separate local deletion authority. It copies the exact
terminal intent, bounded attempt chain, sparse response chain, completion, and
their marker IDs into a canonical owner-private retention intent. Only after
that intent is durable and rebound may it delete the original request markers
and empty scratch directory; an exact tombstone completion is published last.
Crashes after either boundary are advanced only by the same full operation and
reviewed plan selectors. Ordinary `run`/`resume` stop at this boundary, while
read-only `status` labels the proof as historical and makes no client request.
JSON kind is `client.removal.retention`.

Removal `forget` has its own irreversible acknowledgement. It first publishes
a deterministic owner-private root recovery marker containing the exact
tombstone, then removes only the selected retention markers and empty operation
subtree, confirms their durable absence, and removes the recovery marker last.
After that final deletion a repeat can report only `absent_unattributed`, not
`already_forgotten`. It never reads a downloader credential, contacts a client,
deletes content, or selects another operation. JSON kind is
`client.removal.forget`.

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
final is rejected. Default output
contains one-way source-path references; raw source paths
require `--show-absolute-paths`. Only content-bearing regular-file names are
represented. Empty files, padding, directories, and cleanup remain out of
scope. Unselected hardlink or alias names may remain, so the plan never
claims that storage would be reclaimed. The terminal activation marker remains
historical; current job identity, state, and effective paths are established
separately by the bounded live reads. Those values are still non-atomic client
claims and do not prove a remote open inode. Serialized plan JSON is not
execution authority. JSON kind is `content.source_retirement_plan`.

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
change, or client identity/layout change fails closed. Once every name is
retired, the exact final is reverified and the same authenticated client
session reobserves current use before the terminal marker is published.

A normal qBittorrent single-file run makes one login and three bounded ledger
reads (four HTTP requests total); Transmission makes its fixed two-request
bootstrap plus those three reads (five total). Ordinary multi-file runs add
three bounded file-list reads, for seven qBittorrent or eight Transmission
requests. Active resume uses two proof observations after bootstrap: three/five
qBittorrent or four/six Transmission requests for single/multi-file. There are
no retries and no client mutations. `status` reads only one explicit private
journal or retained tombstone and neither reads credentials nor contacts the
client. Terminal resume also avoids credential I/O. Source-name retirement
itself does not remove a parent directory, the final, another name for a
hardlinked inode, an empty/padding entry, or a downloader job, and no report
claims reclaimed storage or rollback. Execution JSON kind is
`content.source_retirement`.

Reconcile one exact metafile with verified bytes and an audited downloader's
read-only ledger. Two bounded torrent-list reads bracket the storage proof. For
one unique ordinary multi-file job, `auto` mode attempts up to two bounded
per-file reads around that proof; the second is sent only after a complete
first read. The qBittorrent path uses one login and makes at most five HTTP
requests. Transmission deliberately uses the first CSRF 409 as a version
handshake, performs one version read, and makes at most six HTTP requests. Both
paths are serial and never automatically retry.
No pause, recheck, move, add, or filesystem write is performed.

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl reconcile report \
  --torrent release.torrent \
  --search-root "D:\Media" \
  --search-root "D:\Archive" \
  --driver qbittorrent \
  --url https://seedbox.example \
  --username admin \
  --password-stdin \
  --host-root 'D:\' \
  --client-root /downloads \
  --client-style posix \
  --client-file-layout auto \
  --site-ref tjupt/123 \
  --output json
```

When the intended layout is already known, replace all `--search-root` flags
with `--source PATH`. That path is reopened as one exact single-file object or
multi-file content root and every non-padding manifest name is verified in the
same invocation. The report uses `verified_exact_root` and can reconcile that
selected layout with the client, but it does not claim that another matching
copy does not exist elsewhere.

A committed or retained materialize operation can be selected instead of a
raw path or discovery source:

```bash
ptctl reconcile report \
  --torrent release.torrent \
  --target "D:\Seed" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --output json
```

All three materialize flags are required. The command first validates the
explicit journal or retention tombstone and exactly verifies the current final
namespace and bytes. It then immediately reopens that final through the
ordinary exact-source verifier. Only the process-local opaque pairing of those
two proofs can produce `verified_materialized_final`; a public report, a JSON
round trip, or an independently supplied path cannot recreate it. The two
local observations are sequential and bracketed non-atomic, and optional
downloader before/after reads enclose both. A damaged final is reported before
the command returns integrity exit `3`. `--source`, live search roots, a stored
profile selector, and this materialized-final selector are mutually exclusive.

To require client-adoption attribution, add the explicit adoption operation and
its reviewed plan ID to materialized-final reconciliation. The terminal record
retains whether that lineage came from stopped add or observation-only adoption:

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl reconcile report \
  --metafile-store .ptctl-private \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\Seed" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --adoption-operation sha256:ADOPTION_OPERATION_DIGEST \
  --adoption-plan-id ADOPTION_PLAN_ID \
  --driver qbittorrent \
  --url https://seedbox.example \
  --username admin \
  --password-stdin \
  --host-root 'D:\Seed' \
  --client-root /downloads \
  --client-style posix \
  --output json
```

Both adoption flags are required together, along with the complete
materialized-final, downloader, and path-mapping selectors,
`client-file-layout=auto`, and the default bounded file limits. Before stdin or
network access, ptctl reads the selected live terminal journal or retained
tombstone and checks its metafile, materialize, driver, client-configuration,
and mapping lineage. The `client_adoption` ledger keeps that historical
completion separate from a process-local bridge to the current exact typed
job claim in the already existing Before/After bracket. No additional request
is sent. Historical adoption cannot upgrade infohash, local content, or path
evidence, and downloader APIs expose no generation with which to exclude a
remove-and-readd incarnation. Public JSON cannot recreate either authority.
When activation is also selected, its embedded adoption operation, plan, and
completion IDs must match this explicit lineage. Adoption and keep-data
removal reconciliation are mutually exclusive because one requires current
exact presence and the other current exact absence.

To require attribution to one canonical terminal client-activation journal,
add its explicit reviewed selectors to materialized-final reconciliation:

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl reconcile report \
  --metafile-store .ptctl-private \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\Seed" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --activation-operation sha256:ACTIVATION_OPERATION_DIGEST \
  --activation-plan-id ACTIVATION_PLAN_ID \
  --driver qbittorrent \
  --url https://seedbox.example \
  --username admin \
  --password-stdin \
  --host-root 'D:\Seed' \
  --client-root /downloads \
  --client-style posix \
  --output json
```

Both activation flags are required together. This mode also requires the full
materialized-final, client, and path-mapping selectors, `client-file-layout=auto`,
and the fixed default file-ledger limits used by activation. The command reads
the terminal journal and checks its metafile, materialize, driver, client
configuration, and mapping IDs before reading the password or contacting the
downloader. It then validates the activation against the same existing
Before/After downloader bracket; it sends no additional request. The
`client_activation` ledger reports historical completion proof and current-use
bridge proof separately. Historical activation never upgrades infohash,
storage-content, or lexical-path evidence. Missing or unbound requested history
is incomplete, a positive selector/configuration/final disagreement is a
conflict, and journal corruption retains integrity exit `3` after the report.
Serialized output cannot recreate either process-local authority.

To reconcile a completed keep-data client removal instead of current client
use, add the activation lineage plus one explicit removal operation and its
reviewed plan ID:

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl reconcile report \
  --metafile-store .ptctl-private \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\Seed" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --activation-operation sha256:ACTIVATION_OPERATION_DIGEST \
  --activation-plan-id ACTIVATION_PLAN_ID \
  --removal-operation sha256:REMOVAL_OPERATION_DIGEST \
  --removal-plan-id REMOVAL_PLAN_ID \
  --driver qbittorrent \
  --url https://seedbox.example \
  --username admin \
  --password-stdin \
  --host-root 'D:\Seed' \
  --client-root /downloads \
  --client-style posix \
  --output json
```

Both removal flags are required together and are mutually exclusive with the
source-retirement selectors in this slice. Before reading the password or
contacting the downloader, the command verifies the live terminal journal or
retained removal tombstone and checks its metafile, materialize, activation,
driver, client-configuration, mapping, job, and layout lineage. Forgetting the
tombstone deliberately removes this historical authority. Only a completion
whose accepted removal response was followed by exact queue absence can close
the removal axis; absence observed after an unknown response remains
`historical_absence_causality_unproven` and makes the requested report
`incomplete`.

No extra downloader request is sent. The same Before/After typed ledger reads
that bracket the exact final must both show the reviewed job absent, so no
per-file request is attempted. A successful qBittorrent path therefore remains
one login plus two ledger reads; Transmission remains its two-request
bootstrap plus two ledger reads. The report keeps exactly five relations and
adds a separate `client_removal` ledger: `client_infohash_relation` is
`absent`, `verified_source_vs_job_path` is `not_comparable`, and consistency
requires the current exact final plus the bound activation and removal
lineages. It does not claim that the downloader is using the retained bytes,
that the absence was observed atomically, or that the job cannot reappear
after return. Public JSON cannot recreate either process-local capability.

To require source-retirement attribution as well, add one explicit terminal
retirement operation, its reviewed SHA-256 plan ID, and the original source
root scope:

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl reconcile report \
  --metafile-store .ptctl-private \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\Seed" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --activation-operation sha256:ACTIVATION_OPERATION_DIGEST \
  --activation-plan-id ACTIVATION_PLAN_ID \
  --retirement-operation sha256:RETIREMENT_OPERATION_DIGEST \
  --retirement-plan-id sha256:RETIREMENT_PLAN_DIGEST \
  --retirement-search-root "D:\Originals" \
  --driver qbittorrent \
  --url https://seedbox.example \
  --username admin \
  --password-stdin \
  --host-root 'D:\Seed' \
  --client-root /downloads \
  --client-style posix \
  --output json
```

All three retirement flags are required, and `--retirement-search-root` may be
repeated only to reproduce the exact reviewed source scope. Before reading the
password or contacting the downloader, the command reads the canonical
terminal retirement journal, checks its metafile/materialize/activation
lineage, rebinds each original source parent inside that explicit scope, and
observes every retired name absent twice. The existing downloader bracket is
then reused without another client request. The `source_retirement` ledger
reports historical completion and current-absence proof independently from
the activation, downloader, and storage ledgers; it does not add a sixth
relation or upgrade any of the five existing relations. A reappeared retired
name is a current `conflict` and stops before credential input. A pruned
retirement tombstone has deliberately discarded direct path authority, so by
itself it can show historical completion only. A separately selected live
parent-cleanup journal may still close that current-absence axis by proving the
removed parent names absent under the same retirement lineage; it never
recreates the pruned paths. Corrupt
journal or tombstone state retains report-first integrity exit `3`. Reports
never emit the original source paths. Windows UNC/network source roots are
rejected before access unless `--retirement-allow-network` is explicit; this
permission never applies to the materialized target. Even a consistent result
is a sequence of identity-bound, bracketed non-atomic observations, not a
promise that a name cannot reappear after the command returns.

To require the terminal empty-parent cleanup as another explicit ledger axis,
add its full operation, reviewed plan, and original source-root scope to the
same command:

```bash
  --parent-cleanup-operation sha256:PARENT_CLEANUP_OPERATION_DIGEST \
  --parent-cleanup-plan-id sha256:PARENT_CLEANUP_PLAN_DIGEST \
  --parent-cleanup-search-root "D:\Originals"
```

All three selectors are required together and require the complete retirement
selectors. Before secret stdin or downloader I/O, ptctl reads the canonical
terminal cleanup journal, checks its exact retirement operation/plan/completion,
scope, target-root identity, and retired-file count, then observes every
removed immediate-parent name absent twice. Parent reappearance is a current
`conflict`; unavailable scope or namespace authority is `incomplete`. The
`parent_cleanup` ledger reports historical completion and current absence as
separate process-local proofs and leaves the existing five relations unchanged.
The removed-parent observation may conservatively imply absence of all retired
child names in that parent, but it remains sequential and non-atomic. A retained
parent-cleanup tombstone contains no paths and therefore cannot supply current
absence or make the requested report consistent. Neither public JSON nor a
later invocation can recreate these capabilities.

The [qBittorrent WebUI API torrent-list fields](https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-%28qBittorrent-5.0%29#get-torrent-list)
are treated as untrusted client claims. Its generic `hash` remains an opaque
job locator. Typed identities come only from strictly parsed `xt=urn:btih:...` and
`xt=urn:btmh:1220...` claims; the complete magnet URI is immediately
discarded because it may contain tracker or web-seed secrets.

Transmission 4.0.x uses the legacy 5.3.x RPC protocol; Transmission 4.1 and
later use JSON-RPC 2.0 with snake-case fields. The adapter determines that
choice from the official CSRF version header, then checks the session-reported
version. Its complete 40-hex [`hash_string`](https://github.com/transmission/transmission/blob/main/docs/rpc-spec.md#33-torrent-accessor-torrent_get)
is an explicit v1/SHA-1 claim only: it is never truncated, length-guessed into
v2, or allowed to make a pure-v2 or hybrid job exact. A later 409, redirect,
unknown protocol major, or server that omits the opening CSRF handshake fails
closed without replay.

A declared
`--site-ref` does not contact the site and is never presented as a verified
metafile binding. Neither read adapter exposes the raw private metafile through
its ledger, so `metafile_variant_relation` remains `unobservable` even when
typed infohashes agree.

To observe that remote ID live without a downloader, add
`--site-cookie-stdin`. To observe both the site and a downloader in one command,
use `--credential-bundle-stdin` instead of the two single-secret flags and pipe
one bounded strict JSON object:

```json
{"schema":"ptctl.credentials/v1","site_cookie":"SID=...","downloader_password":"..."}
```

Unknown, duplicate, missing, trailing, oversized, invalid-UTF-8, and unpaired
surrogate input is rejected. All adapter capability/origin/ref, metafile,
store, storage-profile, mapping, endpoint, and budget checks finish before this
stdin is read. The site detail request runs first. Its opaque same-invocation
authority adds only a current remote-ID site claim to `site_metafile`; the
relation remains unbound unless an explicit sealed historical binding is also
verified. A requested live read that does not complete makes the overall report
`incomplete`; a successful read never upgrades storage, downloader, path, or
exact-private-variant proof.

When `--site-binding-record` is explicit, the site axis is accepted only from
a same-invocation opaque authority produced by jointly re-reading the sealed
record and its referenced whole-raw private artifact. It is reported as a
historical exact-response observation; it never upgrades incomplete storage,
client, or path evidence and never changes the downloader's raw-metafile
unobservability. Binding verification happens before downloader password stdin
or any downloader request.

For an ordinary multi-file job, qBittorrent's bounded
[torrent-contents endpoint](https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-%28qBittorrent-5.0%29#get-torrent-contents)
or Transmission's ordered
[`files` and `file_stats`](https://github.com/transmission/transmission/blob/main/docs/rpc-spec.md#33-torrent-accessor-torrent_get)
supplies indexed relative paths, sizes, progress, seed state, and selection.
`auto` reads only the one uniquely identified job, once before and once after
local proof. Every index must remain stable, agree with the metafile, be
selected and complete, and map exactly from the same-call verified host source
into the downloader's lexical namespace. Ordinary physical zero-length files
participate only when that proof observed their names and regular-file
identities, as explicit `--source` does. Discovery that cannot attribute an
empty name remains fail-closed. Any nonempty file attribute (including padding
or symlink semantics) remains unsupported for this full-layout claim.
Use `--client-file-layout off` to retain a partial report without the two file
reads. Matching only a top-level content path is never enough.

For a single-file job, consistency additionally requires the client-reported
size, complete seeding state, and `content_path` to agree. The expected client
path is recomputed from the same-call opaque storage proof and the explicit
invocation mapping; mutable discovery JSON is never path authority. Reports
name the exact POSIX/Windows comparison mode and an opaque mapping ID. Client
paths remain remote, non-atomic lexical claims and are never opened on the host.

Run `ptctl help`, `ptctl metafile store`, `ptctl site metafile fetch --help`,
`ptctl site metafile binding --help`,
`ptctl storage profile`, `ptctl storage index`, `ptctl seed discover --help`,
`ptctl seed materialize --help`, `ptctl client activate --help`,
`ptctl client remove --help`,
`ptctl seed retire --help`, or
`ptctl reconcile report --help` for the
complete surface.

Exit code `0` means a report or requested read succeeded, `1` an operational
failure, `2` invalid usage, and `3` an explicit integrity mismatch. Discovery
is report-oriented, so blocked results still print and return `0` by default.
Add `--require-verified` to return `4` unless `source_outcome` is exactly
`verified_unique`; target-plan or client-mapping failure does not erase source
evidence. `torrent verify` prints its result before returning `3`.
Reconciliation is also report-oriented. Add `--require-reconciled` to return
`4` unless the independently reported local axes are `consistent`; the report
is still printed first.

Site-binding inspection returns `0` only after the explicit record and linked
private artifact verify together and the current built-in adapter accepts the
recorded provenance. Missing or unreadable records return `1`, usage returns
`2`, record/artifact corruption returns `3`, and unsupported adapter provenance
returns `4`, always after any non-usage report. Locator listing returns `0`
when its bounded inventory is complete and report-first `4` when a record,
entry, or name-byte limit stops it.

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

## Security model in one paragraph

A private `.torrent` is secret-bearing because its announce URL often contains
a personal passkey. Site cookies and downloader credentials are also secrets,
while filesystem reads can update atime, hydrate cloud placeholders, or incur
network cost. `ptctl` keeps credentials in memory, rejects secret arguments,
emits no request bodies, blocks cross-origin/downgrade redirects, never retries
site reads, bounds network and filesystem work, hides private store/object and
discovery/reconciliation absolute paths by default, never exposes materialize
paths, defaults conflicts to failure, and confines private operation-state
deletion to separately acknowledged materialize, adoption, activation, and
source-retirement prune commands; exact tombstone erasure and source-name
unlink each have their own narrower acknowledgement. Commands such
as `storage probe` and `seed plan` keep their documented path-display
contracts. The private store uses owner-only permissions and atomic no-clobber
publication for both metafiles and allowlisted sealed state records; this is
access control, not encryption. Store init/import, storage profile
creation/index refresh, the artifact plus sealed-binding phases of an
acknowledged site metafile fetch, and acknowledged target-root-local
materialize operations (including explicit retention pruning and exact
tombstone forgetting), acknowledged exact stopped-job adoption (including
explicit retention pruning and exact tombstone forgetting), reviewed client
recheck/start (including explicit retention pruning and exact tombstone
forgetting), and acknowledged
source-name retirement (including its separate journal pruning and explicit
last-evidence forget transition), and exact empty-parent cleanup (including
its separate no-path journal pruning and explicit last-evidence forget
transition), are
the explicit write exceptions to the otherwise
zero-write operational surface.
See [THREAT_MODEL.md](docs/THREAT_MODEL.md).

## Architecture

```text
           +--------------------- Core ----------------------+
           | bencode | manifest | exact verify | seed match |
           +---------+----------+--------------+------------+
                     |          |              |
             Site adapters  Client adapters  Storage inventory
                  |              |                 |
                TJUPT    qBittorrent/Transmission  local/mounted*
```

`*` A mounted remote is not seedable merely because it can be listed. Random
read behavior, mount health, client mapping, consistency, and cost must be
established separately.

The detailed design is in [ARCHITECTURE.md](docs/ARCHITECTURE.md), and the
site boundary is in [TJUPT_ADAPTER.md](docs/TJUPT_ADAPTER.md).

Metafile behavior is grounded in [BEP 3](https://www.bittorrent.org/beps/bep_0003.html),
[BEP 47](https://www.bittorrent.org/beps/bep_0047.html), and
[BEP 52](https://www.bittorrent.org/beps/bep_0052.html).

## Project principles

- Missing data is unknown, never silently zero.
- Names and sizes produce candidates; only piece/Merkle evidence is verified.
- Budget exhaustion means incomplete, never no-match or unique.
- No site capability is guessed when an adapter does not declare it.
- A GET that changes tracker accounting must be labeled effectful before
  support is added.
- No ratio cheating, fake upload, DHT/PEX leakage for private torrents, or
  challenge bypass will be accepted.
- No real cookie, passkey, private metafile, or unredacted HTML fixture belongs
  in the repository.

## License

Apache-2.0. See [LICENSE](LICENSE).
