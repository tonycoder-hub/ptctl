# ptctl

`ptctl` is a conservative, content-first CLI for private BitTorrent trackers.
It treats a tracker website, a downloader, and a filesystem as separate trust
domains and reconciles them around verifiable torrent metadata.

> Status: `v0.4.0-alpha` development. Ordinary inspection, downloader reads,
> content proof, discovery, planning, and reconciliation are intentionally
> zero-write. Persistent writes are confined to explicit private-store/index
> operations, the acknowledged `site metafile fetch`, the separately
> acknowledged target-root-local `seed materialize run|resume|abandon|prune`
> workflow, exact `client adopt run|resume` stopped-add operations, and
> explicit `client activate run|resume` recheck/start operations. The
> fetch also crosses a separate,
> tracker-visible read boundary. Materialize creates a new target layout;
> `prune` can delete only one explicitly selected operation's owner-private
> heavy state and retains its tombstone. Client adoption never mutates an
> existing job; activation is limited to the reviewed exact job's recheck and
> optional start transitions. No listed operation overwrites, moves, rewrites, or deletes a
> source or published final layout; reads
> may still update atime or hydrate an offline placeholder.

中文简介：`ptctl` 不是把 PT 网页机械地搬进终端。它以 `.torrent`、
实际文件、下载器任务和站点记录这四本账为核心，先精确校验，再生成
清晰、可审计的报告与计划。TJU PT 是首个实验性只读站点适配器，而不是写死
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
  requirements;
- acknowledged, copy-only `seed materialize run|resume` with a private
  target-root-local journal, same-filesystem staging, exact stage/final proof,
  no-clobber publication, fixed hard limits, explicit operation-ID recovery,
  bounded status/listing, terminal no-delete abandon, and separately
  acknowledged exact pruning of one terminal operation's private state while
  retaining a durable tombstone;
- tracker output reduced to origins so announce passkeys are not printed;
- traversal, separator, Windows device-name, case-collision, and conservative
  Unicode-normalization checks;
- typed, capability-checked site ports instead of a monolithic driver;
- an experimental TJUPT session check, torrent search, and bonus catalog parser
  through one bounded, same-origin HTTPS GET per invocation, with fail-closed
  page recognition and no retry;
- an explicitly acknowledged TJUPT metafile fetch for one remote ID, using one
  bounded GET with no redirect or retry and publishing the strictly validated
  exact response only into an initialized private metafile store;
- qBittorrent status and torrent-list reads over HTTPS (or explicit numeric
  loopback HTTP), with passwords accepted only through stdin;
- read-only reconciliation that brackets storage proof with two qBittorrent
  ledger snapshots from one login, stream-decodes a bounded job ledger,
  extracts typed v1/v2 claims from bounded magnet `xt` fields, and reports
  variant, infohash, content-proof, and path relations as separate evidence
  axes;
- bounded qBittorrent per-file ledgers for one uniquely identified ordinary
  multi-file job, with stable index/size/selection/completion checks and
  per-binding host-to-client path comparison;
- explicitly acknowledged exact qBittorrent stopped-job adoption downstream
  of a current materialized-final proof, with typed queue-absence gating, a
  durable request-intent journal, one non-retried add POST, after-ledger/path
  checks, exact final re-verification, and no automatic replay of an unknown
  request;
- explicit qBittorrent recheck and optional controlled start downstream of a
  canonical stopped-adoption completion, with version-bound v4/v5 routes,
  durable per-request intent, exact typed job/per-file layout reobservation,
  current-final re-verification, and no automatic replay of unknown requests;
- zero-write source-retirement eligibility planning from a new complete live
  source proof, current exact materialized-final proof, and canonical terminal
  client activation journal, plus stable before/after reads of the exact live
  typed-infohash job and its effective paths from one qBittorrent session, with
  explicit final-overlap/alias rejection and no deletion authority;
- versioned experimental JSON envelopes (`ptctl.dev/v1`) and control-safe
  human-readable tables.

Not implemented yet: current-filesystem negative/uniqueness proofs from an
index alone, background refresh/watchers, site torrent-detail reads,
downloader pause/location/removal or broader existing-job mutation,
attributed/empty-file client-layout reconciliation,
reflink/hardlink or cross-filesystem materialization, automatic execution of
serialized plan reports, source/staging cleanup or rollback, source or
published-layout deletion, site
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
the same variant. No command selects a binding by remote ID or by “latest”.

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
`--torrent`, loaded from a separate store, or selected by enumeration/latest.

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
v1/v2/hybrid verifier. Nevertheless `source_outcome` remains `incomplete`, even
when one candidate is currently `verified`: a historical snapshot cannot prove
that no new, removed, or renamed alternative exists now. Snapshot-only mode
therefore never emits a materialization plan, never reports `not_found` or
`verified_unique`, and never makes reconciliation `consistent`. Use ordinary
same-call `--search-root` discovery when current uniqueness or absence is
required. `--snapshot-record` can bypass bounded latest listing, but does not
upgrade freshness or proof semantics.

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
Its `exact_root` plan ID is not an execution selector. To review a layout for
materialize, use `seed discover --target` with the intended selector, search
roots, and target (as in the preceding discovery example), retain that
discovery plan's 24-hex ID, then acknowledge the journal, staging, and target
writes:

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
it repeats bounded live discovery with that same selector/search-root/target
shape, requires one uniquely verified source, rebuilds the copy-only plan from
the process-local proof, and compares the fresh ID before creating a journal.
It then uses a private target-root-local
journal and same-filesystem staging, exactly verifies staged and final bytes,
and publishes the top-level layout without clobber. The materialize limits are
fixed by the installed version; only the existing discovery limits are CLI
flags. Network/UNC source roots still require `--allow-network`; a target root
must be a supported local filesystem.

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
```

`status` without an ID performs a bounded name-only listing whose entries are
`not_inspected`; it never chooses a latest operation. `resume` reads fresh
search roots only for `journaled`, `stage_created`, or `file_staged` phases.
Omitting roots in one of those phases returns a blocked report; after stage
verification, supplied roots are not read because staged/final bytes are the
recovery authority. `abandon` is allowed only before publication intent. It
appends one terminal journal event and deliberately retains staging and scratch
bytes: it is not cleanup, rollback, or deletion.

`prune` is the only deletion-capable materialize command. It selects exactly
one full operation ID and reviewed plan ID. A newly started committed prune
also requires the exact metafile and reverifies the current final namespace and
bytes; an abandoned operation instead proves the final name absent. It then
publishes a durable private retention intent before deleting only the original
intent, journal, scratch, and stage objects. A small exact tombstone remains,
so retries are explicit and idempotent. Prune never removes source bytes, the
published final layout, another operation, or a downloader job.

Materialize JSON has kind `content.materialization`; the ID listing uses
`content.materialization.operation_list`; prune uses
`content.materialization.retention`. Reports always keep effect, actual
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
printf '%s' "$TJUPT_COOKIE" | ptctl site bonus-catalog --cookie-stdin tjupt
```

Each TJUPT command performs at most one bounded GET and never retries. Do not
loop or parallelize site reads.

Read qBittorrent state:

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client status \
  --url https://seedbox.example \
  --username admin \
  --password-stdin
```

Adopt one exact materialized layout into qBittorrent without starting it:

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

Version 1 is intentionally a one-way, stopped-add handoff. It requires the
exact raw metafile from the owner-only metafile store, a current exact proof of
the committed (or retained) materialized final, and a complete typed-infohash
queue observation proving that the target job is absent. It then publishes a
private target-root-local request intent before exactly one add POST. The POST
submits the exact stored bytes with the reviewed save path and requests a
stopped/paused job. It never changes an existing job, moves data, starts a
recheck, resumes transfer, deletes content, or retires the source.

The normal run uses one login, one complete ledger read before the request, one
add POST, and one complete ledger read after it. Success additionally requires
one unique exact typed-infohash job in a stopped state, the reviewed size and
lexical save/content paths, a second exact verification of the materialized
final, and a durable completion marker. Its outcome is
`adopted_pending_client_recheck`, not “seeding verified”: qBittorrent does not
expose the raw private variant and no client recheck is performed.

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
```

`status` is local and read-only: it never reads a password or contacts the
client, and therefore reports current client state as unobserved. It validates
canonical marker presence but does not issue a directory sync, so only an
effectful resume refreshes journal durability before relying on those markers.
An operation ID is deterministic from the reviewed adoption plan; no command
enumerates or chooses a “latest” operation. JSON kind `client.adoption` keeps declared
effects, actual/uncertain journal writes, request counts, before/after typed
identity states, current-final proof basis, and non-null findings separate.
Raw host/client paths, endpoint, username, password, opaque qB job key, magnet
URI, tracker URL, passkey, and raw metafile bytes never enter the report.

Recheck that adopted job, then optionally start it only after a durable
completion observation:

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
unsupported rather than guessed. Completion remains a bracketed client claim
plus a same-invocation exact final proof, not proof of a raw private variant or
an atomic client/filesystem snapshot. Each invocation sends at most one
effectful client POST. JSON kind is `client.activation`.

Review which current source file names could be considered for a future,
separately authorized retirement operation:

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
reviewed action, and uses one authenticated read-only qBittorrent session to
observe the exact typed-infohash job before and after local proof. Single-file
planning makes one login plus two bounded job-ledger reads; ordinary multi-file
planning adds two bounded file-ledger reads. It never retries or mutates the
client. The exact published final and selected source bytes are reverified
inside that live-client bracket, and a selected source inside or aliasing the
final is rejected. Default output
contains one-way source-path references; raw source paths
require `--show-absolute-paths`. Only content-bearing regular-file names are
represented. Empty files, padding, directories, cleanup, and deletion remain
out of scope. Unselected hardlink or alias names may remain, so the plan never
claims that storage would be reclaimed. The terminal activation marker remains
historical; current job identity, state, and effective paths are established
separately by the bounded live reads. Those values are still non-atomic client
claims and do not prove a remote open inode. A future deletion command must
rebuild the same live plan and repeat every client/source/final proof, require a
separate acknowledgement, and journal
every unlink result; serialized plan JSON will not be authority. JSON kind is
`content.source_retirement_plan`.

Reconcile one exact metafile with verified bytes and qBittorrent's read-only
ledger. The password is used for one login; two bounded torrent-list reads
bracket the storage proof. For one unique ordinary multi-file job, `auto` mode
attempts up to two bounded per-file reads around that proof; the second is sent
only after a complete first read. The complete path is serial, makes at most
five HTTP requests including login, and never retries.
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

The [qBittorrent WebUI API torrent-list fields](https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-%28qBittorrent-5.0%29#get-torrent-list)
are treated as untrusted client claims. Its generic `hash` remains an opaque
job locator. Typed identities come only from strictly parsed `xt=urn:btih:...` and
`xt=urn:btmh:1220...` claims; the complete magnet URI is immediately
discarded because it may contain tracker or web-seed secrets. A declared
`--site-ref` does not contact the site and is never presented as a verified
metafile binding. qBittorrent does not expose the raw private metafile through
this ledger, so `metafile_variant_relation` remains `unobservable` even when
typed infohashes agree.

When `--site-binding-record` is explicit, the site axis is accepted only from
a same-invocation opaque authority produced by jointly re-reading the sealed
record and its referenced whole-raw private artifact. It is reported as a
historical exact-response observation; it never upgrades incomplete storage,
client, or path evidence and never changes qBittorrent's raw-metafile
unobservability. Binding verification happens before downloader password stdin
or any downloader request.

For an ordinary multi-file job, the bounded
[torrent-contents endpoint](https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-%28qBittorrent-5.0%29#get-torrent-contents)
supplies indexed relative paths, sizes, progress, seed state, and selection
priority. `auto` reads it only for one uniquely identified job, once before and
once after local proof. Every index must remain stable, agree with the
metafile, be selected and complete, and map exactly from the same-call verified
host source into qBittorrent's lexical namespace. Any nonempty file attribute
(including padding or symlink semantics) and non-padding empty files remain
unsupported for this full-layout claim.
Use `--client-file-layout off` to retain a partial report without the two file
reads. Matching only a top-level content path is never enough.

For a single-file job, consistency additionally requires the client-reported
size, complete seeding state, and `content_path` to agree. The expected client
path is recomputed from the same-call opaque storage proof and the explicit
invocation mapping; mutable discovery JSON is never path authority. Reports
name the exact POSIX/Windows comparison mode and an opaque mapping ID. Client
paths remain remote, non-atomic lexical claims and are never opened on the host.

Run `ptctl help`, `ptctl metafile store`, `ptctl site metafile fetch --help`,
`ptctl storage profile`, `ptctl storage index`, `ptctl seed discover --help`,
`ptctl seed materialize --help`, `ptctl seed retire --help`, or
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

Materialize validates every selector, acknowledgement, timeout, and limit
before opening a metafile, source root, target root, or journal. Successful
`run`/`resume`, a completed status/list read, a successful abandon, and
`pruned`/`already_pruned` return `0`. Operational interruption, cancellation,
publication/removal ambiguity, or
unconfirmed post-publication durability returns `1`; invalid usage or a
missing acknowledgement returns `2`; content/journal integrity failure returns
`3`; and a policy blocker, reviewed-plan mismatch, missing fresh source
authority, explicit operation not found, or incomplete bounded operation
listing returns `4`. Non-usage outcomes are written before the exit. A failure
may have nonzero or uncertain writes, and a reported operation ID is the only
recovery handoff; retries never choose an operation automatically.

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
paths, defaults conflicts to failure, and confines deletion to the separately
acknowledged operation-state-only materialize prune command. Commands such
as `storage probe` and `seed plan` keep their documented path-display
contracts. The private store uses owner-only permissions and atomic no-clobber
publication for both metafiles and allowlisted sealed state records; this is
access control, not encryption. Store init/import, storage profile
creation/index refresh, the artifact plus sealed-binding phases of an
acknowledged site metafile fetch, and acknowledged target-root-local
materialize operations (including explicit retention pruning), plus
acknowledged exact stopped-job adoption and its private request journal, are
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
                TJUPT       qBittorrent       local/mounted*
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
