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
    versioned private exact-byte artifacts, permission checks, atomic no-clobber

internal/storage
    bounded inventory, path semantics, identity guards, namespace mapping

internal/seed
    discovery orchestration and evidence-gated deterministic plans

internal/reconcile
    axis-separated, read-only four-ledger reports

internal/site
    optional AuthChecker / AccountReader / TorrentSearcher /
    BonusCatalogReader ports
        `-- tjupt

internal/downloader
    normalized client state
        `-- qbittorrent
```

The core never imports a concrete site or downloader implementation. Site
adapters do not receive filesystem handles, and storage code does not receive
credentials. TJUPT is one adapter, not a special case in the content model.

## Read-only ledger reconciliation

`reconcile report` is the second vertical slice. One invocation resolves and
parses one exact metafile, opens one read-only downloader session, reads a
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

1. `site_metafile` records only a user-declared site reference in this slice;
2. `metafile_variant_relation` asks whether the downloader exposes the exact
   private `.torrent` bytes;
3. `client_infohash_relation` compares algorithm-tagged v1/v2 claims;
4. `storage_content_proof` carries the ordinary piece/Merkle proof;
5. `verified_source_vs_job_path` compares a verified single-file path, or each
   verified multi-file binding by manifest index, with the downloader's
   lexical claims under one explicit namespace mapping.

There is no single `matched` boolean. The overall lattice is `consistent`,
`partial`, `conflict`, `ambiguous`, or `incomplete`, while every relation keeps
its own evidence level and blocker codes. A different client path means that
verified reusable bytes exist elsewhere; it is not a content mismatch. A
scattered source can align only when every physical manifest binding
independently maps to the corresponding stable client file claim; no shared
host source root is inferred.

qBittorrent's generic `hash` is an opaque job key. The adapter derives typed
claims only from strictly bounded magnet `xt` values: BTIH is 20 bytes and
BTMH must be the SHA-256 multihash `1220` plus 32 bytes. Pure v1 and pure v2
jobs must expose exactly their required family; hybrid reconciliation requires
both. Names, sizes, progress, state, save path, and generic hash length never
establish identity. The magnet URI is discarded after parsing because its
tracker or web-seed parameters may contain credentials.

The job array is decoded incrementally: the job limit is checked before row
N+1 is decoded, and each object has its own field-count cap. Duplicate JSON
fields and duplicate opaque job keys fail the snapshot. State is reduced to a
known qBittorrent code or `unknown`, so untrusted free text cannot become
report output.

The downloader adapter does not expose raw private metafile bytes, so equal
typed infohash claims still leave `metafile_variant_relation=unobservable`.
Likewise, qBittorrent paths are untrusted remote claims: they are parsed only
for lexical comparison and are never opened as host paths. The path relation
can become consistent for a single-file job in a stable, complete seeding state
whose reported size agrees with the metafile. For an ordinary multi-file job it
additionally requires two stable, complete indexed file observations: every row
must match its manifest index and size, remain selected and complete, and have
an effective path equal to the independently projected verified source
binding. The qBittorrent path contract is fixed as `save_path` plus the returned
relative file path; `content_path` must be a consistent ancestor. The
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

Site-specific writes will eventually live under a namespaced action schema.
They will not be forced into universal fields. The alpha exposes no site
writes.

Downloader ledgers negotiate normalized capabilities separately:
algorithm-tagged infohashes, content paths, raw metafiles, and indexed job
files. The qBittorrent adapter declares indexed job files only when it can
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
- `seed discover`, `seed plan`, and `reconcile report` retain `--torrent FILE`,
  or accept the same stored pair;
- half a stored pair, both source forms, or an extra positional metafile is
  usage error `2`.

After source resolution, every command receives the same parsed `MetaInfo` and
keeps its existing JSON kind, evidence lattice, and exit semantics. Store-backed
inspect, verification, discovery, planning, and reconciliation only read the
store and remain zero-write. Store corruption is an explicit integrity failure,
not a fallback to a file with the same infohash.

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
later cleanup or validation did not complete. Exit `4` remains reserved for the
existing report-first `--require-verified` and `--require-reconciled` contracts.

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

This store is the required B1 gate for a future
`torrent.metafile.read_effectful` site method. That method must validate the
store and usage before reading credentials, label and require acknowledgement
of the tracker-visible GET, validate the bounded response, and pass its exact
bytes through the same no-clobber import primitive. It must not introduce a
second arbitrary-destination or overwrite path.

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
requested. A future durable inventory should persist a storage profile ID and
raw relative components, never assume one machine's absolute path is portable.

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
- `incomplete`: uniqueness or absence cannot be established because scanning
  or verification stopped early.

Only `verified_unique` has a selected source. Target conflicts or client-path
mapping errors can block the handoff without erasing the source outcome. A
produced plan remains `layout_only`, `effect:none`, and `ready_to_apply:false`;
its blockers explain which mutation and reconciliation controls are absent.

`bytes_verified` is physical content read. Per-algorithm
`proof_stream_bytes` may differ because v1 includes virtual padding while v2
does not. Every result declares its non-atomic stability assurance. v2 padding
and symbolic-link leaves fail closed until their filesystem semantics can be
modeled safely.

## Planned mutation workflow

A future apply command must be recoverable, not a raw `mv`:

```text
pause client -> record journal -> materialize -> verify target
             -> switch client location -> client recheck -> restore state
             -> separately confirm source retirement
```

It must support resume and rollback, never overwrite by default, and never
delete borrowed or externally owned files. Reflink/copy are safer defaults than
hardlink because a client repair through a shared inode can corrupt a media
library.

## Plugin direction

Go in-process plugins, `dlopen`, and evaluated scripts are intentionally out of
scope because they inherit keyring, network, and filesystem authority. New
adapters are built in and reviewed. If external adapters become necessary
after several implementations exist, they should use a versioned capability
protocol in a sandboxed process or WASM runtime with domain and filesystem
allowlists.
