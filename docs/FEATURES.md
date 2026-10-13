# Features

[Documentation index](README.md) · [Project README](../README.md)

This is the complete inventory of what ptctl implements today and what it
deliberately does not. For step-by-step usage, see the
[command guide](guide/README.md).

## Implemented today

- strict, bounded bencode parsing;
- exact v1 infohash calculation from the original `info` byte slice;
- exact whole-metafile SHA-256 variant identity, kept distinct from infohashes;
- a versioned private metafile store with explicit initialization, exact-byte
  import, content-addressed no-clobber objects, verified inspection, and
  same-user/same-session cross-process Windows coordination for the owner/DACL
  layout setup window without serializing immutable object publication;
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
- an experimental TJUPT session check, bounded partial account snapshot,
  torrent search, torrent-detail observation, bonus catalog parser, and
  exact-option zero-write bonus review
  through one bounded, same-origin HTTPS GET per invocation, with fail-closed
  page recognition, no redirect, and no retry; detail requests deliberately
  omit NexusPHP's view-counting `hit` parameter; bonus review records a bounded
  form-shape digest and semantic review ID but never submits the form; both
  remain site claims only;
- an explicitly acknowledged, durable TJUPT bonus-exchange state machine:
  private reviewed intent, fresh exact-option re-observation, deterministic
  no-clobber attempt marker, at most one non-retried POST, and a separately
  sealed confirmed/rejected/unknown outcome; its credential-free bounded
  operation inventory verifies live intents plus historical retention/forget
  markers without selecting a latest operation or inferring linked state; a
  marker without an outcome is permanently submission-unknown and is never
  retried automatically; an acknowledged local prune can replace only a
  complete terminal chain with an exact non-executable tombstone, and a
  separate acknowledged forget can erase that last historical evidence;
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
- explicitly reviewed qBittorrent or Transmission stop of one current complete
  started exact job, with a durable request intent, one non-retried native
  stop request, same-session stopped-state/file-layout proof, and another exact
  materialized-final verification before completion; a lost response may
  produce causality-unproven stopped evidence but is never replayed without a
  second acknowledgement; Transmission remains v1-only;
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

## Not implemented yet

Not implemented yet: current-filesystem negative/uniqueness proofs from an
index alone, background refresh/watchers, downloader location or broader
existing-job mutation beyond the exact stopped transition,
client-layout reconciliation for attributed file semantics such as padding or
symlink leaves and for zero-length files without an observed physical binding,
  reflink/hardlink or cross-filesystem materialization, automatic execution of
  serialized plan reports, recursive or policy-selected source-parent cleanup,
  staging cleanup or rollback, published-layout deletion, site
writes, browser login, third-party executable plugins, ratio manipulation, or
Cloudflare bypass.
