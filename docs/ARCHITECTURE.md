# Architecture

## The four ledgers

`ptctl` models PT operations as reconciliation among four independent records:

1. **Site record** — site torrent ID, account state, rules, promotions, and
   site-defined actions.
2. **Metafile manifest** — exact original bytes, v1/v2 hashes, file layout,
   piece hashes or Merkle roots, and tracker-specific wrapping.
3. **Downloader job** — save path, selection, progress, state, and the path
   namespace seen by the client.
4. **Storage inventory** — the bytes that exist, their location, identity, and
   evidence confidence.

No single record is automatically authoritative for all questions. A client
showing 100% may have stale resume data; matching names and sizes do not prove
content; equal infohashes do not make two site records interchangeable.

## Ports and adapters

```text
internal/domain
    stable IDs, capabilities, summaries, snapshots

internal/metafile
    bounded bencode decoder, exact info slice hashing, manifests, verification

internal/storage
    path semantics, root confinement, read-only probing, namespace mapping

internal/seed
    evidence-gated, deterministic plans

internal/site
    optional AuthChecker / AccountReader / TorrentSearcher / BonusCatalogReader ports
        └── tjupt

internal/downloader
    normalized client state
        └── qbittorrent
```

The core never imports a site or downloader implementation. Site adapters do
not receive filesystem handles, and storage code does not receive credentials.

## Capability negotiation

Capabilities are small and explicit:

- `auth.check`
- `account.read`
- `torrent.search`
- `torrent.detail`
- `torrent.metafile.read_effectful`
- `bonus.catalog.read`

Site-specific writes will eventually live under a namespaced action schema.
They will not be forced into universal fields. The preview exposes no site
writes.

## Identity

- A site release is `(site_id, remote_id)`.
- A v1 content identity is a 20-byte infohash.
- A v2 content identity is a 32-byte infohash.
- A metafile variant includes the complete private wrapper around `info`.

These identities are related, not interchangeable. Two sites may wrap the same
`info` dictionary with different announce passkeys. Those raw files must never
be merged or sent to the other site.

## Path model

Business logic should eventually persist `(storage_id, relative_components)`;
an absolute path belongs to a specific process namespace. A mapping explicitly
relates a host root such as `D:\PT` to a downloader root such as `/downloads`.

Before materialization, every path is checked for traversal, separators,
control characters, NUL,
Windows ADS/device names, trailing dot/space behavior, case collisions, and a
conservative Unicode-normalization policy. Existing source paths are resolved
component by component and must remain under the declared root.

Filesystem case and normalization behavior cannot be inferred reliably from
the process OS for CIFS, FUSE, APFS variants, or per-directory Windows settings.
The alpha labels that evidence `host_os_assumption`; it must not be treated as
a measured storage property.

## Evidence levels

- `candidate`: name/path/size are plausible.
- `likely`: an independent quick digest or trusted provenance agrees.
- `verified`: every v1 piece or v2 Merkle requirement agrees.

Only exactly verified content can enter a layout plan. The current `seed plan`
implements v1 verification and emits copy operations; it never applies them.
Its readiness is always `layout_only`, its evidence is scoped to
`source_snapshot:v1_piece_verified`, and it reports missing target-semantics,
site identity, downloader mapping, and apply-time verification as blockers.

## Planned mutation workflow

A future apply command must be a recoverable workflow, not a raw `mv`:

```text
pause client → record journal → materialize → verify target
→ switch client location → client recheck → restore state
→ separately confirm source retirement
```

It must support resume and rollback, never overwrite by default, and never
delete borrowed or externally owned files. Reflink/copy are safer defaults than
hardlink because a client repair through a shared inode can corrupt a media
library.

## Plugin direction

Go in-process plugins, `dlopen`, and evaluated scripts are intentionally out of
scope: they inherit keyring, network, and filesystem authority. New adapters are
built in and reviewed. If external adapters become necessary after several site
implementations exist, they should use a versioned capability protocol in a
sandboxed process or WASM runtime with domain and filesystem allowlists.
