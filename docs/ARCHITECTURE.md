# Architecture

## The four ledgers

`ptctl` models PT operations as reconciliation among four independent records:

1. **Site record** — site torrent ID, account state, rules, promotions, and
   site-defined actions.
2. **Metafile manifest** — exact original bytes, v1/v2 hashes, file layout,
   piece hashes or Merkle roots, and tracker-specific wrapping.
3. **Downloader job** — save path, file selection, progress, state, and the
   path namespace seen by the client.
4. **Storage inventory** — the bytes that exist, their location, file identity,
   and evidence confidence.

No ledger is authoritative for every question. A client showing 100% can have
stale resume data; matching names and sizes do not prove content; equal
infohashes do not make two private site artifacts interchangeable.

## Ports and adapters

```text
internal/domain
    stable IDs, capabilities, summaries, snapshots

internal/metafile
    bounded bencode, exact info hashing, manifests, mapped verification

internal/metastore
    versioned private exact-byte artifacts and allowlisted sealed state records,
    permission checks, atomic no-clobber

internal/storage
    bounded inventory, path semantics, identity guards, namespace mapping

internal/storageindex
    immutable profiles, streaming snapshot format, descriptor selection,
    live reobservation of historical candidate locators

internal/seed
    discovery orchestration and evidence-gated deterministic plans

internal/reconcile
    axis-separated, read-only four-ledger reports

internal/site
    optional AuthChecker / AccountReader / TorrentSearcher /
    BonusCatalogReader ports and a capability-gated effectful metafile port
        `-- tjupt

internal/downloader
    normalized client state
        |-- qbittorrent
        `-- transmission
```

The core never imports a concrete site or downloader implementation. Site
adapters do not receive filesystem handles, and storage code does not receive
credentials. TJUPT is one adapter, not a special case in the content model.

## Read-only ledger reconciliation

`reconcile report` is the second vertical slice. One invocation resolves and
parses one exact metafile, optionally reads one authenticated site detail page,
optionally opens one read-only downloader session, reads a
bounded job ledger, performs ordinary storage discovery and content proof, then
reads the job ledger again. For one uniquely identified ordinary multi-file job, `auto`
mode attempts one bounded per-file read before the storage proof and sends a
second afterward only when the first completed. The outer job observations and
successful inner file observations form a serial bracket, not an atomic
transaction.

The metafile input is exactly one of an ordinary `--torrent FILE` or the paired
`--metafile-store DIR --metafile-variant ID` selector. A stored object is
bounded, re-hashed, and parsed before reconciliation begins. The selection does
not weaken or alter any relation, outcome, or zero-write guarantee below.

The report deliberately keeps five relations separate:

1. `site_metafile` is either a user-declared reference, optionally augmented by
   a same-invocation live remote-ID page observation, or an explicitly selected,
   jointly verified sealed historical site-to-whole-variant record;
2. `metafile_variant_relation` asks whether the downloader exposes the exact
   private `.torrent` bytes;
3. `client_infohash_relation` compares algorithm-tagged v1/v2 claims;
4. `storage_content_proof` carries the ordinary piece/Merkle proof;
5. `verified_source_vs_job_path` compares a verified single-file path, or each
   verified multi-file binding by manifest index, with the downloader's
   lexical claims under one explicit namespace mapping.

There is no single `matched` boolean. The overall lattice is `consistent`,
`partial`, `conflict`, `ambiguous`, `incomplete`, or `integrity_failed`, while every relation keeps
its own evidence level and blocker codes. A different client path means that
verified reusable bytes exist elsewhere; it is not a content mismatch. A
scattered source can align only when every physical manifest binding
independently maps to the corresponding stable client file claim; no shared
host source root is inferred.

An explicit `--site-binding-record` requires the stored metafile selector from
the same private store. It is loaded before downloader credential input or
requests and never selected by ref, time, or enumeration. A valid historical
binding does not upgrade any storage/client/path axis and does not change the
downloader raw-metafile relation; a mismatch conflicts, while unavailable or
corrupt authority prevents overall consistency. A bare `--site-ref` remains a
non-gating declaration. Explicit `--site-cookie-stdin` makes the live detail
axis gating: failure is incomplete, while success adds only a current site
claim. With a downloader in the same invocation, one strict bounded
`--credential-bundle-stdin` object supplies both secrets so stdin is consumed
exactly once; all credential-free gates run first.

qBittorrent's generic `hash` is an opaque job key. The adapter derives typed
claims only from strictly bounded magnet `xt` values: BTIH is 20 bytes and
BTMH must be the SHA-256 multihash `1220` plus 32 bytes. Pure v1 and pure v2
jobs must expose exactly their required family; hybrid reconciliation requires
both. Names, sizes, progress, state, save path, and generic hash length never
establish identity. The magnet URI is discarded after parsing because its
tracker or web-seed parameters may contain credentials.

Transmission's `hash_string` is a complete SHA-1 infohash and is normalized as
an algorithm-tagged v1 claim only. It is never interpreted as v2, truncated,
or combined with names, sizes, progress, state, or paths to infer another
identity family. Consequently Transmission can establish an exact client job
for a pure-v1 metafile, but a pure-v2 or hybrid metafile remains incomplete.
Protocol 5.3.x uses Transmission's legacy request envelope and field names;
protocol 6.x uses JSON-RPC 2.0 and snake case. The first CSRF 409 version header
selects the envelope, and the following session read must confirm it.

Each adapter's job array is decoded incrementally: the job limit is checked
before row N+1 is decoded, and each object has its own field-count cap. Duplicate JSON
fields and duplicate opaque job keys fail the snapshot. State is reduced to a
known normalized code or `unknown`, so untrusted free text cannot become report
output. Evidence labels and opening request budgets come from code-owned
descriptors for audited built-in drivers, never from snapshot text.

The downloader adapter does not expose raw private metafile bytes, so equal
typed infohash claims still leave `metafile_variant_relation=unobservable`.
Likewise, downloader paths are untrusted remote claims: they are parsed only
for lexical comparison and are never opened as host paths. The path relation
can become consistent for a single-file job in a stable, complete seeding state
whose reported size agrees with the metafile. For an ordinary multi-file job it
additionally requires two stable, complete indexed file observations: every row
must match its manifest index and size, remain selected and complete, and have
an effective path equal to the independently projected verified source
binding. The qBittorrent path contract is fixed as `save_path` plus the returned
relative file path; `content_path` must be a consistent ancestor. Transmission
uses `download_dir` plus the current `name` as its content root and
`download_dir` plus each ordered `files[].name` as the effective file path;
the paired `file_stats` row supplies selection and completion. The
implementation never tries alternate path formulas until one happens to match.

Expected paths are projected from the opaque same-call `VerifiedSource`, not
from mutable discovery report fields. The public report copy deliberately
drops that process-local capability. Mapping scope records an opaque mapping ID
and exact POSIX or Windows comparison semantics. A shared top-level
`content_path` alone remains insufficient. Any nonempty file attribute
(including padding or symlink semantics) and non-padding empty files are
conservatively unsupported for the full-layout claim. Windows comparisons
require exact case because case sensitivity can vary by directory or remote
filesystem.

## Capability negotiation

Capabilities are small and explicit:

- `auth.check`
- `account.read`
- `torrent.search`
- `torrent.detail`
- `torrent.metafile.read_effectful`
- `bonus.catalog.read`

Site-specific form or action writes will eventually live under a namespaced
action schema. They will not be forced into universal fields. The effectful
metafile capability is instead a tracker-visible GET plus a private-store
publication boundary; it grants no general site-write or detail-read authority.

## Read-only torrent detail

`site detail` is the first concrete `torrent.detail` port. It validates the
built-in TJUPT production origin, stable route ID, canonical positive-decimal
remote ID, fixed request/body/header budgets, and cookie authentication method
before reading stdin. One fresh HTTP/1.1 request reads
`details.php?id=REMOTE_ID`; redirects, retries, compression, proxies, and
`hit=1` are absent. The reference NexusPHP implementation uses `hit` to update
the view counter, so its omission is an explicit no-counter-effect choice.

The adapter accepts only a recognized authenticated page with a bounded heading
and an internal action/download link carrying exactly the selected ID. The
public projection retains a display title, optional peer counts, whether a
matching download reference was present, and a separate request receipt. It
does not retain raw HTML, descriptions, URLs, or arbitrary server text.

This observation is a site claim at one non-atomic interval. The typed reader
returns opaque process-local authority; its public JSON projection cannot be
replayed as evidence. Reconciliation can consume that authority in the same
invocation to show that the selected remote-ID page was observed. It does not
bind the site ID to a whole-raw metafile variant, cannot replace the historical
exact fetch binding, and cannot upgrade any local or downloader evidence axis.

Downloader ledgers expose normalized capabilities separately:
algorithm-tagged infohashes, content paths, raw metafiles, and indexed job
files. Both built-in read adapters declare indexed job files only when they can
supply all required path, size, progress, selection, and seed-state fields;
partial rows do not become a weaker “supported” layout.

## Identity

- A site release is `(site_id, remote_id)`.
- A v1 content identity is a 20-byte infohash.
- A v2 content identity is a 32-byte infohash.
- A metafile variant is SHA-256 over the complete original `.torrent` byte
  stream, including the tracker-specific wrapping around `info`.
- A discovery observation identifies one scan-time `(root, raw relative path,
  file identity, size, mtime)` tuple; it is not durable proof.

These identities are related, not interchangeable. Two sites may wrap the same
`info` dictionary with different announce passkeys. Those files must never be
merged or sent to another site.

## Private metafile store

The private metafile store is the durable metafile-ledger slice:

```text
metafile store init --store DIR
    -> create or recognize one versioned, permission-isolated store

metafile store import --store DIR FILE.torrent
    -> bounded exact-byte read and strict parse
    -> whole-raw SHA-256 artifact identity
    -> same-store, same-filesystem private temporary object
    -> sync and atomic no-clobber publication

metafile store inspect --store DIR METAFILE_VARIANT_ID
    -> bounded object read, digest verification, and strict parse
```

Initialization is explicit: import never silently claims an arbitrary existing
directory as a store. Re-initializing the same valid store is idempotent.
Import copies the accepted raw bytes; it never moves, rewrites, or deletes the
source. Reading can still update atime, hydrate an offline placeholder, or incur
remote-filesystem cost. Object names are derived only from the fixed-size ASCII
digest, not from untrusted torrent or HTTP filenames. Re-importing the same
bytes returns `already_present` without replacing the object. Two complete byte
streams with one infohash but different trackers, passkeys, or outer
dictionaries remain two objects.

Store publication is a narrowly scoped filesystem mutation. The store root and
objects require owner-only access; POSIX permissions and Windows ownership/DACL
state are checked rather than inferred from a successful open. Symlink/reparse
traversal and stores whose permission or atomic no-clobber semantics cannot be
established fail closed. Before store-layout or artifact mutation, the operation
holds one reviewed root identity and requires the root, `objects`, `tmp`,
staging file, and final object to remain on the same reviewed filesystem. POSIX
creation below that root, inspection, publication, cleanup, and directory
flushing are handle-relative; Linux additionally binds device and mount ID,
while macOS binds device and filesystem ID. Windows pins every path
prefix plus the three private directories with no-delete guards, checks the
volume identity, and rechecks the named binding before reporting success.
Volatile memory filesystems are not durable store backends. Temporary objects
remain private before no-replace publication. POSIX publishes with a no-replace
link and then `fsync`s the final directory; Windows uses no-replace `MoveFileEx`
with write-through. Publication and durability confirmation are separate report
facts. If the object became complete and visible but the
post-publication durability confirmation fails, the outcome is
`published_durability_unconfirmed`, `writes_performed` may be `1`, and the
visible object is neither claimed absent nor deleted as rollback. The store is
not an encrypted vault: compromise of the account, backing filesystem, or
privileged administrator still exposes private announce material.

Reports identify the store and artifact opaquely. Store and import-source
absolute paths are omitted by default and appear only under the explicit
`--show-absolute-paths` disclosure; object paths are never emitted. Raw
metafile bytes, announce URLs, web seeds, and passkeys never enter JSON, human
output, or errors.

All metafile consumers use one source-selection rule:

- `torrent inspect` and `torrent verify` retain their single positional file,
  or accept the paired `--metafile-store DIR --metafile-variant ID` flags with
  no positional metafile;
- `seed discover`, `seed plan`, `seed materialize run|resume`, and `reconcile
  report` retain `--torrent FILE`, or accept the same stored pair;
- half a stored pair, both source forms, or an extra positional metafile is
  usage error `2`.

After source resolution, every command receives the same parsed `MetaInfo` and
keeps its existing JSON kind, evidence lattice, and exit semantics. Store-backed
inspect, verification, discovery, planning, and reconciliation only read the
store and remain zero-write. Materialize may use that selector, but still needs
its independent write acknowledgement and never writes to the store. Store
corruption is an explicit integrity failure, not a fallback to a file with the
same infohash.

The store commands add stable `ptctl.dev/v1` kinds
`metafile.store.init`, `metafile.store.import`, and
`metafile.store.inspect`. Their data keeps outcome, effect, write count, store
format/privacy state, exact artifact identity, assurance, limits/usage, and
non-null blocker/issue/warning arrays separate. `initialized`,
`already_initialized`, `stored`, `already_present`, and verified inspection are
exit `0`; environmental/store failures are `1`, usage is `2`, and invalid input
or stored-object corruption is `3`. `published_durability_unconfirmed` is a
reported operational failure with exit `1` even though its write count may be
`1`; `published_post_commit_failure` records a completed publication whose
later cleanup or validation did not complete. The store does not use exit `4`;
report-first discovery/reconciliation requirements and blocked materialize
controls use it in their own command contracts.

Publication assurance is not reconstructed from current bytes. New successful
publications are `confirmed_this_invocation`; read-only inspection and
idempotent `already_*` results set historical publication assurance to
unobservable while independently reporting current digest, parse, and privacy
checks. Their no-clobber and durability booleans therefore remain false rather
than making a historical claim.

Artifact-read accounting has an explicit `artifact_bytes_known` bit. Import
reports the bytes it consumed even when validation fails; a failed stored-object
read whose exact byte count is unavailable reports unknown rather than a false
zero.

The write count is a logical commit count: it records publication of the
accepted format marker or immutable object, while private temporary and
uninitialized staging entries are not counted as committed artifacts.

## B1: effectful site metafile fetch

`site metafile fetch` is the only path from the site ledger to a new private
metafile artifact:

```text
site metafile fetch --cookie-stdin --acknowledge-site-effect
    --metafile-store DIR SITE REMOTE_ID
    -> validate one site, one remote ID, limits, capability, and store
    -> read the cookie only after that zero-write preflight succeeds
    -> send one bounded GET, with no redirect and no retry
    -> strictly validate the complete exact response
    -> publish only through the existing private, no-clobber store primitive
    -> after successful artifact import, publish a sealed historical binding
```

The command does not fetch a torrent-detail page and does not infer another
remote ID. It has no raw stdout, arbitrary destination, overwrite, or sidecar
mode. The store must already be initialized; fetch never turns an arbitrary
directory into a store. A site adapter must explicitly declare
`torrent.metafile.read_effectful`, support the selected stdin authentication
method, and validate its own remote-ID syntax without credentials. This keeps
the CLI and report generic when a second site adapter is added.

The site-to-metafile relation first becomes an invocation-scoped
`observed_exact_variant` only when the store import explicitly returns a valid
exact `ArtifactRef` and its whole-response digest and consumed-byte receipt
agree. The CLI does not independently re-hash a response to upgrade a
pre-publication or import failure into that relation. Such a failure retains
only its bounded request/response receipt. Once the import has established the
exact reference, a later durability or post-publication failure does not erase
the report-only observation.

B2 adds a binding-last `site.metafile.binding.v1` commit marker only after the
artifact import returns complete success. The sealed record preserves the
canonical site ref, adapter origin/route, whole-raw artifact link, bounded
single-request account, and historical observation interval. Its publication
and every load jointly verify the record and referenced private artifact in one
operation-bound physical store session. This is not a two-object transaction:
an artifact may remain as an unreferenced immutable object if marker publication
fails. Reconciliation accepts only an explicit record ID and returns an opaque
process-local `VerifiedSiteBinding`; serialization loses that authority. There
is no list/latest/ref-based automatic selector.

The site observation and store publication/durability facts remain independent
report axes. A GET may complete with no exact relation or published object,
while an exact reference may exist even when the publication's durability or
post-commit completion is not confirmed. Request attempts and logical store
writes are therefore reported separately; neither is inferred from the other
or from the overall outcome.

Raw response bytes, request and redirect URLs, cookies, announce URLs,
passkeys, server filenames, object paths, and temporary paths never enter JSON,
human output, errors, or a side record. The ordinary store-root path disclosure
remains opt-in. Non-usage failures are reported before the command returns its
operational or integrity exit; site fetch itself does not use exit `4`.

## Immutable storage profiles and sealed candidate snapshots

The private store also accepts four fixed, internal sealed-record kinds. They
are not a user-controlled namespace and do not expose a generic put/cat CLI:

```text
storage.profile.v1
storage.index.data.v1
storage.index.descriptor.v1
site.metafile.binding.v1       # historical exact-response provenance
```

Record identity is domain-separated as SHA-256 over a fixed record domain, the
allowlisted kind, and every payload byte. It is a different type from a
metafile `ArtifactID`; identical bytes under different schemas cannot alias.
Import and load stream through the existing operation-bound private root,
owner-only staging, no-replace publication, final-directory durability, and
fresh named-identity checks. Load grants a synchronous reader only and succeeds
after both the consumer and the store observe EOF, the full domain digest
matches, and handle/name metadata remains stable. Listing is bounded by entries,
records, and path bytes; unknown `objects` entries fail closed.

A storage profile is immutable configuration: exact roots, platform/path
encoding, one-filesystem/network policy, and scan budgets determine its
authority ID. Its display name and creation time do not. Reusing one name for a
different declaration is a conflict; multiple records with the same name and
same declaration are logically idempotent. Raw absolute roots are stored only
inside the owner-only record and are omitted from public reports by default.
Foreign-platform profiles remain inspectable but cannot be used for live
refresh/query; before any root access, the consumer requires the current GOOS,
absolute clean native paths, and scan limits representable by its encoder.

Refresh uses the profile declaration as follows:

```text
profile roots
    -> bounded deterministic lexical DFS
    -> stream header + regular-file NDJSON rows + complete footer
    -> seal storage.index.data.v1
    -> seal storage.index.descriptor.v1 (commit record)
```

The scanner emits declared root ID, raw base64 relative components, size,
mtime, and a non-authoritative identity hint. It never follows symlinks or
Windows reparse points, never crosses the captured root filesystem, and retains
no whole-tree slice in memory. Rows are globally ordered by root ID and raw
components so duplicate/unsorted locators can be rejected while streaming.
Every count, line, total byte, path, component, directory, entry, file, issue,
and root dimension has a hard bound. Directory N+1 sentinels count against the
global entry budget, and cumulative traversal-name bytes are charged even for
non-regular entries. An incomplete scan closes the data stream without a
footer, so the data object is not published. Public refresh reports remove
relative issue paths and root/filesystem identity hints.

The data record is published before its descriptor. A data record without a
descriptor is an ignored orphan. Data durability failure prevents descriptor
publication. Descriptor durability failure preserves its visible-write receipt
without claiming a durable commit. A successful descriptor publication is not
reported `stored` until the descriptor and its data record coexist and pass
their domain-separated digests in one operation-bound physical-store session.
Latest selection loads and validates every bounded descriptor for the exact
profile revision and chooses the unique
largest monotonic generation; a tie is ambiguous, listing N+1 is incomplete,
and a corrupt/dangling newest descriptor never causes fallback to an older
generation. An explicit descriptor record ID bypasses listing only, not
freshness semantics.

Snapshot consumption is candidate-only. It first verifies the entire sealed
descriptor and NDJSON record, while retaining only torrent-required sizes under
current candidate/path budgets. Parsed rows remain provisional until EOF and
record digest verification finish. Each locator is then resolved again beneath
the current declared root, link/mount traversal is rejected, and fresh
root/file identities are captured. A changed root invalidates all candidates
under that root. Changed file identity or mtime is counted as stale but may
remain a candidate if the current regular file has the exact required size;
the ordinary identity-bound torrent verifier still decides content truth.
Hardlink aliases remain distinct paths and candidate edges.

The live candidate value itself carries a private, non-serialized authority
digest over the immutable profile revision, descriptor/data identities,
bounded accounting, diagnostics, every historical row, and every fresh
observation/locator. Seed discovery rejects a copied DTO if any of those public
fields changed; a JSON round trip cannot recreate candidate authority.

Historical inventory completeness and current search completeness are separate
axes. In unselected snapshot `seed discover`:

- a live exact match may be reported with `best_evidence=verified`;
- `source_outcome` always remains `incomplete`;
- zero matches never becomes `not_found`;
- multiple exact matches add positive ambiguity evidence but remain incomplete;
- selection, client/target handoff, and materialization plan remain blocked.

An explicit snapshot execution path is deliberately a choice rather than a
search conclusion. The caller supplies one descriptor record ID and one exact
source-match ID returned by a prior bounded preview. Every locator in that map
is reopened beneath the immutable profile root and the ordinary identity-bound
v1/v2/hybrid verifier runs again. If the selected map survives, the result is
`verified_selected`; it never becomes `verified_unique`, `not_found`, or a
current completeness token. A domain-separated selection-scope ID binds the
profile ID, snapshot generation, descriptor record, and match ID into the
materialize plan. Public JSON retains only this opaque review data and loses
the process-local `VerifiedSource` capability.
Budgets on unselected alternatives do not erase an already complete proof of
the selected map, but they keep verification completeness false and cannot be
used to infer how many other current layouts exist.

`reconcile report` consumes the same result and therefore cannot become
`consistent` from a historical snapshot. Only ordinary same-invocation full
`--search-root` enumeration can currently establish current absence or unique
selection. Refresh is a separate explicit write; read commands never update an
index implicitly.

## Read-only storage discovery

`seed discover` is the first vertical reconciliation slice:

```text
manifest slots
    -> bounded multi-root metadata inventory
    -> exact-size candidates
    -> bounded v1/v2 candidate solver
    -> ordinary authoritative SourceMap verifier
    -> unique / ambiguous / not-found / incomplete source outcome
    -> optional zero-write target plan and client-path projection
```

Search roots are canonicalized and overlapping roots are rejected. A custom
bounded DFS opens one directory at a time, sorts entries deterministically,
retains only wanted sizes, does not follow symlinks or Windows reparse points,
and does not cross a filesystem boundary. Roots, depth, directories, entries,
per-directory entries, retained candidates, retained raw path bytes, and issue
count all have mandatory process hard caps.

The public report uses a root ID plus relative components and raw base64.
Absolute host paths are process-local details and are hidden unless explicitly
requested. The durable index persists an immutable storage profile ID and raw
relative components, then performs live reobservation; it never assumes one
machine's absolute path or an old identity hint is current proof.

An inventory hit is only `candidate/exact_size`. Basename and suffix agreement
affect deterministic exploration order, never evidence level. v2/hybrid
candidates can be rejected by an exact per-file Merkle root. v1 has no general
per-file commitment, so it is explored as one manifest-order SHA-1 stream and
pruned only when a completed piece disagrees. The solver never materializes a
Cartesian product.

Candidate states, candidates per manifest file, total manifest-to-candidate
edges considered during preparation, verified alternatives, proof work, and
diagnostic issues are also bounded. Empty files and virtual padding consume solver state. Virtual v1
padding consumes proof-work budget before any zero hashing. The active binding
prefix is push/pop state, so memory is linear in manifest depth rather than
quadratic.

Every retained result still passes `VerifySourceMap`. The resulting
`VerifiedSource` is an opaque process-local token bound to the exact metafile
variant, normalized manifest-index bindings, and live file snapshots. The plan
builder cannot be given a user-constructed `Verified=true` JSON object.

## Path and file-identity model

Manifest paths are indexed by file number and retain raw components. They are
checked for traversal, separators, controls, NUL, Windows ADS/device names,
trailing dot/space behavior, case collisions, and a conservative Unicode
normalization policy before target planning.

Each discovered file keeps its original root, raw relative components,
scan-time file information, platform identity, size, and mtime. Every proof
open rechecks the root and each component, opens the candidate, compares the
opened handle with the scan-time file identity, and rechecks the component
chain. The verifier also checks the same open handle before and after reading.
This detects ordinary rename/replacement races but is still best-effort and
non-atomic: it is not equivalent to POSIX `openat` confinement, a Windows
handle-relative traversal, or a storage snapshot.

Host-to-client mapping is a separate lexical claim. It proves that a host path
can be represented below configured namespace roots; it does not contact the
downloader or prove reachability.

Filesystem case, normalization, reflink, random-read, and consistency behavior
cannot be inferred reliably from the process OS for CIFS, FUSE, APFS variants,
or per-directory Windows settings. The alpha keeps those properties unknown or
labels them as assumptions.

## Evidence and outcomes

- `candidate`: name/path/size are plausible.
- `likely`: reserved for an independent digest or trusted provenance; current
  discovery does not promote size matches to likely.
- `verified`: every required v1 piece and/or v2 Merkle commitment agrees.

v1 is one SHA-1 stream across file boundaries. v2 is a separate 16 KiB-leaf
SHA-256 Merkle tree for each file. A hybrid is verified only when one physical
read feeds both proof families; v1 virtual padding feeds only its v1 stream.
Piece layers are proof material, not trust roots: parsing reduces each layer
with BEP 52 zero-subtree rules and compares it with the corresponding file
`pieces root`.

Discovery source outcome and optional handoff are separate axes:

- `verified_unique`: the complete search found exactly one verified layout;
- `verified_ambiguous`: at least two distinct layouts are verified, even if a
  later budget prevents retaining more alternatives;
- `not_found`: a complete search found none;
- `verified_selected`: one caller-selected historical locator map passed exact
  live reobservation and content verification; no uniqueness is claimed;
- `incomplete`: uniqueness or absence cannot be established because scanning
  or verification stopped early.

`verified_unique` and `verified_selected` can retain a process-local selected
source, but only the former is a current search result. Target conflicts or
client-path mapping errors can block the handoff without erasing the source
outcome. A produced plan remains `layout_only`, `effect:none`, and
`ready_to_apply:false`; its blockers explain which mutation and reconciliation
controls are absent.

`bytes_verified` is physical content read. Per-algorithm
`proof_stream_bytes` may differ because v1 includes virtual padding while v2
does not. Every result declares its non-atomic stability assurance. v2 padding
and symbolic-link leaves fail closed until their filesystem semantics can be
modeled safely.

## Journaled copy-only materialization

`seed materialize` is the first content-write slice. It is deliberately
narrower than downloader coordination and has six explicit controls:

```text
run     selector + live roots OR explicit stored match + target + reviewed plan ID + write ack
resume  selector + target + reviewed plan ID + write ack + optional source selector + operation ID
status  target + optional explicit operation ID
abandon target + abandon ack + explicit operation ID
prune   target + reviewed plan ID + deletion ack + explicit operation ID
forget  target + reviewed plan ID + historical-evidence-deletion ack + explicit operation ID
```

Both plan surfaces remain `layout_only`, `effect:none`, and
`ready_to_apply:false`. `run` accepts only the 24-hex plan ID reviewed from
`seed discover --target` with the same metafile selector and target. Live mode
also repeats the same search roots and requires `verified_unique`. Indexed mode
requires one explicit profile, descriptor record, and source-match ID; it
returns `verified_selected`, reopens every chosen locator, and reruns exact
content proof without claiming current uniqueness. Its domain-separated
selection scope is part of the plan ID. The standalone `seed plan` uses
`exact_root` source semantics and its ID is not a materialize execution
selector. `run` does not deserialize either plan/discovery JSON or treat a
historical report as proof. Under one timeout it reproduces the chosen source
mode, consumes only the same-invocation opaque `VerifiedSource`, and rebuilds
the copy-only plan. A mismatch blocks before a journal is created. Each source
copy is identity/precondition bracketed against that same-call authority; the
complete staged layout is then exactly verified before publication.

The target is an existing supported local filesystem root. A bound root
identity from plan review must still match, the final top-level name must be
absent, and staging remains on that filesystem. The engine creates an opaque
operation subtree, immutable intent, and hash-chained event journal, stages by
copy, verifies the complete staged layout against v1/v2/hybrid commitments,
records publication intent, performs no-replace top-level publication, verifies
the complete final layout, and commits. Its durable phases are:

```text
journaled -> stage_created -> file_staged* -> stage_verified
          -> publish_intent -> published -> final_verified -> committed
          -> abandoned  # only from an unpublished pre-intent phase
```

The intent contains the exact metafile variant, reviewed plan ID, target-root
identity, final raw relative component, manifest budgets, and fixed installed
limits. It deliberately contains no absolute target path and no source locator.
The event chain binds every created/staged/published object identity without
turning historical identity into content proof. Reports likewise contain no
absolute or raw source, target, journal, scratch, or staging path, and the CLI
offers no materialize path-disclosure flag.

Materialize limits are always `materialize.DefaultLimits()` and are sealed into
the intent; there are no CLI overrides. The run/resume scan and proof budgets
remain the ordinary discovery flags. Reports use `content.materialization` and
keep declared effect, actual/uncertain writes, operation phase/resumability,
plan/source/target assurance, every fixed limit/usage counter, and non-null
blocker/issue/warning arrays separate. Journal object publications, durability
confirmations, staged/final publication attempts, bytes, and ambiguous writes
are not collapsed into the outcome.

`resume` first replays the explicit journal. Only `journaled`, `stage_created`,
and `file_staged` phases may read supplied live roots or the same explicit
stored selection and require fresh same-mode source authority. Later phases
ignore supplied source selectors and reverify staged or final bytes; a durable
journal is recovery evidence, never source-content authority.
Committed recovery rechecks the final namespace and content and returns
`already_committed`. No command enumerates and silently selects an operation.

`status ID` replays one journal without claiming current content proof.
`status` without an ID performs a bounded target-root name listing; each result
is `not_inspected`, the JSON kind is
`content.materialization.operation_list`, and incomplete listing returns a
report before exit `4`. `abandon` writes one terminal event only before
publication intent. It retains stage and scratch objects and is neither
cleanup, deletion, nor rollback.

`prune` is the separate retention transition defined in
[MATERIALIZE_RETENTION.md](MATERIALIZE_RETENTION.md). It accepts only an
explicit committed or abandoned operation. Before its first deletion, a new
committed prune reloads the exact metafile and reverifies the current published
layout; an abandoned prune proves the final name absent. It publishes a small
canonical retention intent no-replace, then performs a bounded exact inventory
and identity-bound post-order removal of only the original intent, journal,
scratch, and stage state. A canonical completion marker is published only
after the operation root is exactly reduced to its lock plus retention marker
directory. The retained tombstone makes retries idempotent and never becomes
authority over source or final bytes.

`forget` is a third, irreversible authority boundary after prune. It accepts
only the explicit complete tombstone, copies its exact canonical intent and
completion evidence into a target-root-level no-path recovery marker, then
removes the retention markers and bound operation subtree. The recovery marker
is re-read after operation removal and deleted last. While it exists, every
ordinary materialize mutation/current-final path fails closed and status/list
reports `forgetting` without advancing it. Successful completion deliberately
leaves no on-target attribution; subsequent absence is
`absent_unattributed`, never `already_forgotten`. Its JSON kind is
`content.materialization.forget`. Source bytes, published final content,
downloader state, and unrelated operations are outside this authority.

All usage, selector, acknowledgement, timeout, and budget checks precede
metafile, source-root, target-root, and journal I/O. Non-usage failures remain
report-first: exit `0` is a successful transition/read, `1` is operational
interruption or uncertain/unconfirmed publication, `2` is usage, `3` is exact
content or journal integrity failure, and `4` is a policy/source/plan/selector
blocker, explicit operation not found, or incomplete operation listing. A
failure can have nonzero or uncertain writes; its explicit operation ID is the
only resume handoff.
The final forget transition returns `0` only for a newly confirmed deletion;
once its last marker is absent, unattributed absence is operational exit `1`.

## Exact stopped-job adoption

`client adopt` is the first downloader-write slice. It is downstream of a
committed copy-only materialize operation and deliberately stops before any
client verification or transfer-state transition:

```text
exact stored metafile + current exact final + complete typed queue absence
  -> durable target-root-local request intent
  -> one built-in downloader add POST requesting stopped/paused state
  -> complete typed queue observation of one exact stopped job
  -> exact final reverify
  -> durable adopted-pending-recheck marker
```

The process-local `materialize.VerifiedFinal` is the only bridge from a
committed journal or sealed retention tombstone to adoption. It reloads the
exact metafile, binds the materialize operation and reviewed discovery plan,
audits the current final namespace, verifies every v1/v2/hybrid byte, and
retains the target locator only in memory. A `FinalObservation` exposes opaque
identities and proof counts but no path. Its client projection uses one
invocation-scoped host/client mapping; public plans contain only domain-separated
path and mapping references.

`downloader.MutationSession` extends the bounded ledger session with one
`AddStopped` port. The exact raw payload is an opaque, one-shot
`MetafilePayload` loaded only from the bound private metastore object. The
qBittorrent implementation reuses one authenticated session and streams one
bounded multipart POST. The Transmission implementation uses its fixed
two-request CSRF/version handshake and streams one bounded base64 payload in
the version-appropriate `torrent_add` or `torrent-add` envelope with paused
state requested. Both implementations disable HTTP/2, connection reuse,
redirects, proxy use, and automatic retry, and report request/byte completion
separately. Generic job hashes remain opaque locators. qBittorrent can establish
typed v1 and v2 identity; Transmission adoption is restricted to v1 because
its audited ledger exposes only the SHA-1 `hash_string` identity.

Before a new operation, one complete ledger must prove absence. Any unavailable,
invalid, partial, conflicting, or duplicate typed identity fails closed.
Hybrid identity requires both hash families on the same qBittorrent job and is
therefore ineligible for Transmission adoption. Names, sizes, progress, and
paths never select a job. After the POST, success requires one unique exact
job, expected size, stopped state, and exact lexical save/content paths. For
Transmission it also requires the same invocation's explicit accepted-add
response whose returned v1 hash matches the reviewed identity: a duplicate
response is rejected, and an unknown response cannot be attributed later
merely because an exact job appears. This is still a client
claim: the completion marker says `adopted_pending_client_recheck`, while the
raw metafile variant remains unobservable to either downloader.

A completed adoption may be used only as an explicit lineage input when its
exact job is no longer present. The caller supplies the full prior
operation ID and reviewed prior plan ID; a bound local read of either the
canonical completion journal or its sealed retention tombstone recreates an
opaque `VerifiedCompletion`. `BuildPlan` then requires every identity-critical
field to agree with the new current-final projection: driver, client
configuration, path mapping and semantics, save/content references, exact
metafile variant and typed hashes, materialize operation/plan, target/final
identities, and content shape. The new plan incorporates the prior operation,
plan, and completion IDs, so it has a distinct deterministic operation ID.

This authority is deliberately historical and narrow. It cannot prove why the
old client job disappeared or that a removal was intentional. A fresh complete
typed ledger must independently prove current absence, and the effectful call
requires the ordinary add acknowledgement plus a dedicated re-adoption
acknowledgement. No latest/by-age enumeration occurs, the prior operation or
tombstone is not deleted or rewritten, and public JSON cannot recreate the
process-local completion authority. The same rule applies to qBittorrent and
v1-only Transmission adoption.

The deterministic operation directory is reserved by the materialize layout
validator and contains canonical no-clobber intent, bounded attempt, and
completion markers. A request attempt is durable before the POST. If its result
is unknown, resume performs a ledger read first and never repeats the POST
unless both add and repeat acknowledgements are explicit. At most three
explicit attempts are representable. An initialization crash is recoverable
only when the operation namespace is exactly empty apart from its lock and
optional empty scratch directory; unexpected objects remain integrity failures.
`status` never contacts the client and cannot call an unobserved state ready.
It also remains filesystem-read-only: canonical marker presence is reported,
but directory durability is refreshed only by effectful resume before client
I/O.

Terminal adoption state has a distinct retention transition:

```text
exact terminal intent + bounded attempt chain + completion
  -> durable owner-private retention intent containing the exact canonical chain
  -> remove only original intent/attempt/completion markers and empty scratch
  -> exact remaining namespace audit
  -> durable retention completion
```

`client adopt prune` is selected only by one full operation ID plus its reviewed
adoption plan ID and requires a separate operation-state-deletion
acknowledgement. It never opens a downloader session, reads a credential,
touches the materialized final, or selects latest/by-age state. An intent-only
crash state is advanced only by prune; ordinary run/resume/status never crosses
the deletion boundary. The complete tombstone is still read through the same
bound target and operation identities. Only that same-invocation read creates
`clientadopt.VerifiedCompletion`, so activation can consume the historical
completion without treating public JSON as authority. qBittorrent and
Transmission completion can feed only their matching built-in recheck/start
port; Transmission requires an exact typed v1 identity and cannot authorize a
v2 or hybrid activation.
The tombstone does not claim that the client job or final is current.

`client adopt forget` is the separately acknowledged irreversible boundary
after prune. It selects one full adoption operation ID and reviewed plan ID.
Before deleting any retained marker it copies the canonical retention intent,
completion, and bounded attempt chain into a domain-separated deterministic
owner-private root recovery intent bound to both target-root and operation
identities:

```text
exact retained adoption tombstone
  -> staged forget intent inside the bound operation
  -> durable no-clobber root forget intent
  -> exact retained-marker and operation-subtree removal
  -> durable operation-name absence
  -> exact last root-marker removal
  -> unattributed absence
```

The staged or root intent blocks ordinary resume, completion proof, and prune;
only the same explicit forget selector may recover it. Status is read-only and
does not reassert marker durability. The final removal deliberately destroys
idempotence evidence, so a later absence cannot be attributed to prior success.
No credential, downloader request, content mutation, source unlink, or
cross-operation selection is authorized. An opaque `VerifiedCompletion`
already issued to another live caller cannot be revoked by deleting storage.

The adoption slice itself never mutates an existing job, changes its location,
starts a recheck, resumes or pauses transfer, removes a job, deletes a source,
or claims source retirement. Existing-job recheck/start and source retirement
remain separate downstream authorities with their own journals and
acknowledgements:

```text
explicit client recheck -> bracket result -> optional controlled start
explicit location change -> current per-file proof -> bracket result
source-retirement eligibility proof -> separately acknowledged per-name journal
```

The implemented qBittorrent/Transmission activation and client-neutral
retirement paths support explicit resume,
preserve no-overwrite/no-implicit-selection defaults, and never infer source
retirement from materialization or stopped-job adoption. Location change and
job removal remain unimplemented.
Reflink may eventually be safer than hardlink because a client repair through
a shared inode can corrupt a media library, but the implemented filesystem
strategy remains copy only.

## Explicit client recheck and controlled start

`client activate` consumes three live authorities in one process: a current
`materialize.VerifiedFinal`, a canonical `clientadopt.VerifiedCompletion`, and
one fresh authenticated downloader mutation session. Public JSON from any of
those workflows is not authority. The activation plan binds the materialize
and adoption operation/plan IDs, exact metafile variant and typed infohashes,
target/final identities, invocation-scoped mapping references, one opaque job
reference, exact file-layout digest, driver-specific protocol generation, and
whether start is reviewed:

```text
current exact final + canonical stopped adoption + exact stopped client job
  -> reviewed activation plan
  -> durable recheck-attempt marker
  -> one non-retried recheck POST
  -> durable checking observation OR same-invocation incomplete->complete edge
  -> complete stopped job + exact indexed layout + current final reverify
  -> durable recheck-completion marker
  -> optional durable start-attempt marker + one non-retried start POST
  -> complete started client claim + current final reverify
  -> durable activation-completion marker
```

`downloader.ExistingJobMutationSession` extends the same bounded ledger/file
session with a one-shot control descriptor and `Recheck`/`Start` ports. The qB
adapter reads the application version once: supported 4.x sessions bind start
to `/api/v2/torrents/resume`, while supported 5.x sessions bind it to
`/api/v2/torrents/start`; both bind recheck to
`/api/v2/torrents/recheck`. Transmission derives its descriptor from the
already audited two-request RPC bootstrap: v5.3 binds `torrent-verify` and
`torrent-start`, while v6 binds `torrent_verify` and `torrent_start`. Its
selector is exactly one complete 40-hex v1 hash string; generic IDs, v2
prefixes, hybrids, and caller-selected methods are rejected. Callers never
provide a route. Open, descriptor, ledger, optional file ledger, and each POST
are serial and exactly counted. HTTP/2, connection reuse, proxying, redirect,
retry, and queue fan-out remain
disabled. The opaque qB key is URL-form encoded only at the adapter boundary
and never enters plans, journals, reports, or errors. Transmission request IDs
are retained only as bounded receipt metadata so the exact JSON request length
can be independently reproduced.

A `200` response proves only that one application-layer request returned. It
does not prove that the downloader entered or completed checking. The strongest
completion
path first seals an observed `checkingUP|checkingDL|checkingResumeData` state,
then on a later invocation observes a complete stopped job. A second accepted
path begins from an incomplete stopped job and observes complete stopped state
immediately after the same invocation's request. If a complete job checks too quickly to
expose either edge, the request remains unknown. Resume observes first and
never repeats an unknown recheck or start unless its separate base and repeat
acknowledgements are both present. A reviewed start is a later resume after
the recheck-completion marker is durable, so one invocation sends at most one
effectful client POST.

For ordinary multi-file jobs, every observation includes one bounded
driver-specific file ledger. Manifest indices must be contiguous and exact;
effective paths, sizes,
selection, progress, and completion are checked for every file against the
same process-local final projection. Single-file jobs use the exact content
path, total size, stopped/started state, and progress claim. In both cases the
filesystem final is reverified before a request and before a completion marker.
These are bracketed, non-atomic client and filesystem observations; they do not
prove the client's current inode, the private metafile variant, or continuous
stability after the last observation.

The deterministic `.ptctl-client-activate-<digest>` directory uses the same
bound owner-private fs journal primitives as adoption, but has independent
canonical intent, bounded recheck/start attempt, recheck-started,
recheck-completion, and activation-completion markers. Read-only `status`
chooses only an explicit operation ID, performs no network request or sync, and
does not upgrade historical marker presence into current client or durability
evidence.

Terminal activation state has a separate retention transition:

```text
exact terminal activation intent + completion + bounded original-marker manifest
  -> durable owner-private retention intent
  -> remove only the selected original markers and empty scratch
  -> exact remaining namespace audit
  -> durable retention completion
```

`client activate prune` is selected only by one full operation ID plus its
reviewed activation plan ID and requires the separate operation-state-deletion
acknowledgement. It never opens a downloader session, reads a credential,
touches the published final or source files, or selects latest/by-age state.
The intent records each original canonical marker's fixed name,
domain-separated ID, and exact size, so partial deletion is recoverable without
letting a retained DTO choose new names. An intent-only crash state is advanced
only by prune; ordinary resume stops before credential or network access. The
complete tombstone can create `clientactivate.VerifiedCompletion` only through
a same-invocation bound read of the exact target, operation, intent, and
completion. JSON round trips remain powerless, and the tombstone does not claim
that the downloader job or final layout is currently unchanged.

`client activate forget` is the separately acknowledged irreversible boundary
after prune. Its selector is exactly one full activation operation ID, the
reviewed activation plan ID, and the historical-evidence-deletion
acknowledgement. Before deleting any retained marker it copies the canonical
retention intent and completion into a domain-separated, deterministic,
owner-private root recovery intent bound to both the target-root and operation
identities. The transition is:

```text
exact retained activation tombstone
  -> staged forget intent inside the bound operation
  -> durable no-clobber root forget intent
  -> exact retained-marker and operation-subtree removal
  -> durable operation-name absence
  -> exact last root-marker removal
  -> unattributed absence
```

The staged or root intent blocks ordinary resume, completion proof, and prune;
only the same explicit forget selector may recover it. Status remains local and
read-only and does not reassert marker durability. The final removal deliberately
destroys the evidence needed for idempotent attribution, so a later call cannot
claim historical success. It never opens a downloader session, reads a
credential, mutates content, retires sources, or selects another operation.

## Source-retirement review and execution

`seed retire plan` is the zero-write evidence half of source retirement.
It consumes the exact metafile, one complete live
`seed.Discover` result with its opaque `VerifiedSource`, a current
`materialize.VerifiedFinal`, and a `clientactivate.VerifiedCompletion` read from
one explicit terminal activation journal or complete retention tombstone. That
completion and final establish
a process-local `CurrentUseAuthority`; one authenticated read-only downloader
session then supplies two bounded observations of the exact typed-infohash job
and, for ordinary multi-file layouts, its indexed effective paths. Recheck-only plans become terminal at
their recheck-completion marker; a plan that reviewed start is not terminal
until its activation-completion marker exists. Public status/JSON cannot
recreate any process-local capability.

```text
explicit live source roots -> complete unique exact source map
explicit materialize operation + exact current final reverify
explicit activation operation + reviewed terminal marker chain
same activation client configuration + invocation path mapping
  -> bounded live typed-job/effective-path observation
  -> per-index named-source identity checks
  -> reject source beneath or physically aliasing the final
  -> exact post-selection source reverify
  -> second exact current-final reverify
  -> second bounded live typed-job/effective-path observation
  -> require stable same-session job, state, layout, and paths
  -> review-only source-retirement plan (deletion authority: none)
```

The plan ID binds the metafile variant and typed hashes, materialize and
activation selectors, terminal marker, current-use authority ID, source-selection ID, target/final
identities, and for every content-bearing physical source its manifest index,
size, modification observation, and domain-separated path reference. Displaying
absolute source paths is presentation-only and does not change the ID. Empty
files and padding have no source deletion candidate; directories are never
listed. Search roots that include the final normally make discovery ambiguous;
if the final itself is uniquely selected, the overlap check blocks it.

The serialized plan is intentionally not executable. Source proof remains
same-invocation and bracketed, client completion is historical, and current
client state/path evidence is a bounded, bracketed, non-atomic lexical claim.
It does not prove a remote open inode, raw private variant, or state after the
last read. None of those authorities are granted by planning.

`seed retire run` supplies the separate mutation boundary. It accepts the same
selectors, a full expected review-plan ID, and
`--acknowledge-source-deletion`. It rebuilds the review with process-local
source/final/client authority and compares the ID before creating any operation
state. A deterministic but explicitly reported operation ID selects an
owner-private `.ptctl-source-retire-<digest>` subtree under the already bound
materialized target root. Resume and status accept only that full ID; neither
enumerates or selects a latest operation.

```text
same-call eligible review + exact expected plan ID + deletion acknowledgement
  -> bind each source's direct parent and exact ordinary regular-file name
  -> preflight canonical intent/attempt/completion marker budgets
  -> durable private intent (absolute source paths remain private)
  -> for each selected content-bearing name:
       durable deletion-attempt marker
       identity + size reobservation
       no-follow unlink of that one name
       parent-directory durability observation
       durable deletion-completion marker
  -> exact published-final reverify
  -> same-session live client-use reobservation
  -> durable operation-completion marker
```

The durable intent binds the exact plan, normalized search-root scope,
materialized target/final identities, activation completion, live-use ID,
source selection, limits, and for every source its parent/name, parent and file
identity, size, timestamp observation, manifest index, and pseudonymous path
reference. Absolute paths never enter the public execution DTO. The source
parents are rebound through filesystem handles on resume; qBittorrent and
Transmission paths remain lexical claims and are never used for host I/O.
The driver is re-established from the terminal activation authority and is
emitted in both the completion and current-use observations. Transmission
requires an exact v1 identity throughout; v2-only and hybrid activations cannot
authorize its retirement path.

An absent name is accepted only when its durable attempt marker already
exists. That crash window produces a recovered completion basis, not a claim
that the previous process observed unlink success. Absence before an attempt,
name/identity replacement, parent/root replacement, corrupt/noncanonical
markers, or disagreement with final/client authority fails closed. Partial
success is not rolled back: already completed names remain absent and the
report returns the explicit resumable operation. Status is read-only historical
journal evidence; it never proves a retired name is still absent. Terminal
resume rebinds local selectors and confirms the exact names remain absent but
does not read a password or contact the client.

Source-retirement operation discovery is a separate bounded, read-only name
inventory. Without an explicit ID, `status` examines the target root once,
retains only canonical operation IDs up to fixed entry/name/result limits.
Operation directories are `not_inspected`; a canonical visible root forget
marker is `forget_in_progress_not_inspected`. It neither opens a journal nor
selects a latest operation, and malformed objects in the reserved
source-retirement namespace make the listing incomplete rather than being
ignored.

Terminal source-retirement journals may be retired only through the separate
`seed retire prune` transition. The selector is one full operation ID plus its
reviewed plan ID; prune has no implicit list/latest or age-based selector. A
canonical
retention intent, bound to the operation-root and target-root identities,
preserves the exact intent/completion digests, metafile/materialize/activation
lineage, source-selection ID, final identity, client snapshot, and retired
file/byte totals before any private marker is removed. The implementation then
performs one bounded exact inventory of the flat journal namespace, requires an
empty scratch directory, removes identity-bound private objects in post-order,
and publishes a completion marker only after the operation root is exactly the
lock plus retention directory. An intent-only crash state is interpreted only
by explicit prune; run/resume/status never cross it. The completed tombstone is
historical audit evidence, not current source-absence proof.

An exact retained source-retirement tombstone can cross one further explicit
boundary through `seed retire forget`. The command requires the full operation
ID, reviewed plan ID, and a dedicated historical-evidence-deletion
acknowledgement; it has no list/latest or age selector. Before any retained
marker is removed, a canonical owner-private intent is published directly
beneath the same bound target root. That intent embeds the complete no-path
retention intent and completion plus their domain-separated IDs, operation and
root identities, plan ID, and retired counts. It is therefore sufficient to
finish one interrupted removal without reopening a deleted operation subtree.

```text
exact complete retained tombstone + explicit selectors + acknowledgement
  -> exact tombstone audit
  -> durable root-level forget intent
  -> remove complete marker, intent marker, and empty retention directory
  -> remove exact lock-only operation subtree while holding its authority
  -> re-read the unchanged root intent
  -> remove the root intent last and confirm target-root durability
```

Only the root intent may authorize recovery after the operation subtree is
gone. A lockless empty residue can be removed only by the exact directory
identity embedded in that intent. Unexpected objects, changed bytes, hardlinks,
identity drift, ambiguous removal, or lost root binding fail closed. Successful
completion deliberately destroys the final on-target attribution record.
Subsequent absence is reported as `absent_unattributed`, never as historical
idempotence; there is no `already_forgotten` state to infer from an empty
namespace.

The source-name remover deliberately does not delete content directories,
final content, empty or padding entries, other hardlink/alias names, or
downloader jobs. A successful unlink therefore does not prove reclaimed
blocks. Source deletion, final verification, and client observation are
bracketed non-atomic facts rather than one frozen cross-system transaction.

## Plugin direction

Go in-process plugins, `dlopen`, and evaluated scripts are intentionally out of
scope because they inherit keyring, network, and filesystem authority. New
adapters are built in and reviewed. If external adapters become necessary
after several implementations exist, they should use a versioned capability
protocol in a sandboxed process or WASM runtime with domain and filesystem
allowlists.
