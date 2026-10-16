# Command guide

[Documentation index](../README.md) · [Project README](../../README.md)

These guides walk through every ptctl command family in roughly the order a
release moves through them: preserve the metafile, find and prove the bytes,
build a seedable layout, hand it to a downloader, retire the old source, and
reconcile all four ledgers. Examples use placeholder paths, IDs, and secrets.
Keep real cookies, passwords, and private metafiles out of shell history, logs,
and issues.

| Guide | Covers |
|---|---|
| [Metafiles and the private store](metafiles.md) | `torrent inspect`, `metafile store`, `site metafile fetch`, sealed site bindings, and stored-metafile selectors |
| [Storage profiles and indexes](storage.md) | `storage profile`, `storage index`, `reconcile refresh-report`, snapshot-backed discovery, and explicit source selection |
| [Verify, discover, plan, and materialize](verify-and-materialize.md) | `torrent verify`, `seed discover`, `seed plan`, and `seed materialize` |
| [Site reads and bonus exchange](sites.md) | `site status`, `account`, `search`, `torrents`, `detail`, `bonus-catalog`, `site bonus review`, and `site bonus exchange` |
| [Downloader reads, path mapping, and adoption](downloader.md) | `storage map`, `client status`, `client list`, and `client adopt` |
| [Client activation, stop, and removal](client-lifecycle.md) | `client activate`, `client stop`, and `client remove` |
| [Source retirement and parent cleanup](source-retirement.md) | `seed retire` and `seed retire parent-cleanup` |
| [Reconciliation](reconcile.md) | `reconcile report` and its materialize, adoption, activation, stop, removal, retirement, and parent-cleanup attribution modes |
| [Exit codes and report-first results](exit-codes-and-reports.md) | exit codes for discovery, reconciliation, site bindings, bonus exchange, materialize, retirement, client removal, the metafile store, storage indexes, and metafile fetch |

## Complete command surface

Run `ptctl help`, `ptctl metafile store`, `ptctl site metafile fetch --help`,
`ptctl site metafile binding --help`,
`ptctl storage profile`, `ptctl storage index`, `ptctl seed discover --help`,
`ptctl seed materialize --help`, `ptctl client activate --help`,
`ptctl client stop --help`,
`ptctl client remove --help`,
`ptctl seed retire --help`, or
`ptctl reconcile report --help` for the
complete surface.

## Current status and write boundaries

> Status: `v0.4.0-alpha` development. Ordinary inspection, downloader reads,
> content proof, discovery, planning, and reconciliation are intentionally
> zero-write. Persistent writes are confined to explicit private-store/index
> operations, the acknowledged `site metafile fetch`, the separately
> acknowledged target-root-local `seed materialize run|resume|abandon|prune|forget`
> workflow, reviewed `client adopt run|resume|prune|forget` stopped-add,
> attributed post-removal stopped re-add, or observation-only existing-stopped
> operations,
> explicit `client activate run|resume|prune|forget` recheck/start operations,
> acknowledged `client stop run|resume|prune|forget` exact-job stopped transitions,
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
> optional start transitions, or one start-only transition authorized by an
> attributed terminal stop of the same completed exact job. `client adopt prune` separately seals one
> terminal completion tombstone before deleting only that operation's private
> request journal; `client adopt forget` has a third acknowledgement and
> irreversibly removes only that adoption tombstone plus its last recovery
> marker. `client activate prune` does the same for one explicitly
> selected terminal recheck/start journal while retaining the exact historical
> completion needed by source-retirement review; `client activate forget` has
> a third acknowledgement and irreversibly removes only that retained
> activation tombstone plus its last recovery marker. `client stop prune`
> likewise replaces one terminal stop request journal with an exact historical
> tombstone; `client stop forget` separately and irreversibly removes only that
> tombstone plus its final recovery marker. Outside the separately
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
