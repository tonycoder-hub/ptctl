# Reconciliation

[Command guide](README.md) · [Documentation index](../README.md)

> `reconcile refresh-report` is described with storage indexes in [storage.md](storage.md).

Reconcile one exact metafile with verified bytes and an audited downloader's
read-only ledger. Two bounded torrent-list reads bracket the storage proof. For
one unique ordinary multi-file job, `auto` mode attempts up to two bounded
per-file reads around that proof; the second is sent only after a complete
first read. The qBittorrent path uses one login and makes at most five HTTP
requests. Transmission deliberately uses the first CSRF 409 as a version
handshake, performs one version read, and makes at most six HTTP requests. Both
paths are serial and never automatically retry.
No pause, recheck, move, add, or filesystem write is performed.

For a current completeness proof over a stored profile, use `reconcile
refresh-report` as documented above. That command is intentionally not
read-only: its only writes are the immutable index data and descriptor records
reported in `writes_performed`; it performs no downloader or content mutation.

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
For `start_after_stop`, the activation completion also exposes its exact stop
operation, plan, completion, and attributed-basis lineage as historical
evidence; the same existing downloader bracket must still establish current
use, and that historical stop never upgrades the independent client, path, or
content axes.

To require attribution to one canonical terminal client stop while also
proving that the exact current job is still stopped, add the explicit stop
operation and its reviewed plan ID to the same activation reconciliation:

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl reconcile report \
  --metafile-store .ptctl-private \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\Seed" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --activation-operation sha256:ACTIVATION_OPERATION_DIGEST \
  --activation-plan-id ACTIVATION_PLAN_ID \
  --stop-operation sha256:STOP_OPERATION_DIGEST \
  --stop-plan-id STOP_PLAN_ID \
  --driver qbittorrent \
  --url https://seedbox.example \
  --username admin \
  --password-stdin \
  --host-root 'D:\Seed' \
  --client-root /downloads \
  --client-style posix \
  --output json
```

Both stop flags are required together and require the complete activation,
materialized-final, downloader, mapping, and default automatic file-layout
selectors. Before password stdin or network access, the command reads exactly
that live terminal stop journal or complete retained tombstone and checks its
metafile, final, activation, driver/configuration, mapping, job, and layout
lineage. Only `accepted_response_then_exact_stopped` is attributed to the
reviewed stop. A stopped state observed after an unknown response remains
`historical_stop_causality_unproven`, prevents consistency, and does not consume
the downloader credential.

Attributed history is then combined with the existing two-read downloader
bracket and current exact final proof. Both current snapshots must still expose
the same complete exact job in a supported stopped state; no additional request
or mutation is sent. The additive `client_stop` ledger keeps historical
completion proof and the process-local current-stopped bridge separate, while
the five relation axes remain unchanged. A complete retained tombstone can
recreate the historical capability through a fresh bound read; public JSON
cannot. Even a consistent result is sequential and non-atomic and cannot prove
remote job incarnation, continued stopped state after return, or a raw private
metafile match. Stop and keep-data removal selectors are mutually exclusive.
Corrupt stop evidence produces a full report followed by integrity exit `3`.

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

---

Previous: [Source retirement and parent cleanup](source-retirement.md) · Next: [Exit codes and report-first results](exit-codes-and-reports.md)
