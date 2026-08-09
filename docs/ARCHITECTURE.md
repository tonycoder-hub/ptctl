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

internal/storage
    bounded inventory, path semantics, identity guards, namespace mapping

internal/seed
    discovery orchestration and evidence-gated deterministic plans

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

## Identity

- A site release is `(site_id, remote_id)`.
- A v1 content identity is a 20-byte infohash.
- A v2 content identity is a 32-byte infohash.
- A metafile variant hashes the complete private artifact around `info`.
- A discovery observation identifies one scan-time `(root, raw relative path,
  file identity, size, mtime)` tuple; it is not durable proof.

These identities are related, not interchangeable. Two sites may wrap the same
`info` dictionary with different announce passkeys. Those files must never be
merged or sent to another site.

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
