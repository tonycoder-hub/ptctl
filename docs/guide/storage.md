# Storage profiles and indexes

[Command guide](README.md) · [Documentation index](../README.md)

> The rest of reconciliation is in [reconcile.md](reconcile.md); same-call discovery without an index is in [verify-and-materialize.md](verify-and-materialize.md).

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

ptctl storage index refresh-discover \
  --state-store "D:\Private\ptctl-metafiles" \
  --profile media \
  --torrent release.torrent \
  --target "D:\PT" \
  --output json

ptctl storage index inspect \
  --state-store "D:\Private\ptctl-metafiles" \
  --profile media

ptctl storage index prune \
  --state-store "D:\Private\ptctl-metafiles" \
  --profile media \
  --acknowledge-index-prune
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
leave an orphan data record, but latest selection lists descriptors only.

There is no store-wide lock, so generation publication is optimistic. Refresh
chooses one more than the highest published generation of the exact profile
and re-lists that profile's descriptors immediately before and after its
descriptor commit. A refresh that sees a competing descriptor at its
generation fails with a `generation_conflict` stop reason and is never
reported `stored`; rerun it. If two concurrent writers did publish the same
maximum generation, latest selection reports it as ambiguous (never ordered by
wall-clock time) until the next successful refresh publishes a strictly newer
generation, which is the recovery.

Index generations are retained per profile. After a `stored` refresh,
`storage index refresh` removes that profile's generations older than the
newest `--retain-generations` distinct generations (default 3; `0` disables
it) and reports the removal under `retention`, separately from the two
publication writes. `storage index refresh-discover` and `reconcile
refresh-report` do not prune; run `storage index prune --acknowledge-index-prune`
for those workflows. Prune removes a superseded snapshot's data record first
and then its descriptor; rerunning it after an interruption finishes the job.
It never removes the latest generation (or either side of a tie at it),
protected records, or other profiles' records. Latest selection lists at most
256 descriptors across all profiles; an index already over that budget reports
`record_listing_incomplete` and asks for prune. Refresh and prune use a larger
maintenance listing (10,000 records), and prune also acts on a verified partial
listing (outcome `partial`, exit 4), so repeat it until the outcome is
`complete` for each profile. A pruned generation can no longer be read through
an explicit `--snapshot-record`; those commands fail closed.

A sealed descriptor that no longer decodes fails closed for every profile
unless its digest-verified bytes carry one exact top-level `profile_id` that
names a different valid profile; such records are counted as
`foreign_invalid_descriptors` and only block the profile they name.

`storage index refresh-discover` is the explicit combined write/read boundary.
Before writing, it validates the immutable profile, metafile, mapping, and all
candidate/proof budgets; a manifest deeper than `--max-states` stops with zero
index writes. Otherwise it publishes exactly the ordinary immutable data and
descriptor generation, reopens every requested-size locator, and runs the
ordinary exact v1/v2/hybrid matcher in the same process. Only a complete,
unchanged invocation may return `verified_unique`, `verified_ambiguous`, or
`not_found`. Its report keeps the two publication receipts and bounded scan
accounting but omits profile paths and filesystem identity hints. This is a
sequential, bracketed, non-atomic observation—not a durable freshness token.
JSON or a later process cannot recreate its authority.

The same authority can be consumed inside the downloader bracket with the
separately named effectful reconciliation command:

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl reconcile refresh-report \
  --torrent release.torrent \
  --state-store "D:\Private\ptctl-metafiles" \
  --storage-profile media \
  --driver qbittorrent \
  --url https://seedbox.example \
  --username admin \
  --password-stdin \
  --host-root 'D:\' \
  --client-root /downloads \
  --output json
```

`refresh-report` performs the first bounded client snapshot, publishes and
revalidates one complete index generation, runs live candidate/content proof,
then performs the second client snapshot. Its report retains the exact index
write count and marks the fresh-index scope explicitly. Historical snapshot
selectors and alternate storage modes are rejected. When a client is selected,
the first snapshot must finish no later than refresh start, and refresh plus
live reobservation must finish no later than the second snapshot starts. An
out-of-bracket interval reports
`storage.index_refresh_outside_client_bracket`: already published writes and
independent storage proof remain visible, but the combined outcome cannot be
`consistent`. Materialized-final, adoption, activation, stop, removal,
retirement, and parent-cleanup modes are structurally incompatible with this
refresh boundary; the core builder fails such composition closed with
`storage.index_refresh_mode_conflict`, even for non-CLI callers. Site evidence,
the client bracket, and path mapping remain composable. Ordinary `reconcile
report` remains zero-write and cannot promote a retained refresh DTO into write
authority.

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
discovery, or the explicit effectful `storage index refresh-discover`, whenever
current uniqueness or absence itself is required.
If a safety budget stops evaluation of other historical assignments after the
selected map has been proved, only that selected map remains authorized; the
report stays explicit about incomplete alternative enumeration.

---

Previous: [Metafiles and the private store](metafiles.md) · Next: [Verify, discover, plan, and materialize](verify-and-materialize.md)
