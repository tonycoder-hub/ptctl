# ptctl

`ptctl` is a conservative, content-first CLI for private BitTorrent trackers.
It treats a tracker website, a downloader, and a filesystem as separate trust
domains and reconciles them around verifiable torrent metadata.

> Status: `v0.3.0-alpha` development. The implemented surface is intentionally
> read-only. `seed discover`, `seed plan`, and `reconcile report` verify and
> explain layouts and ledgers but do not apply them.

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
- tracker output reduced to origins so announce passkeys are not printed;
- traversal, separator, Windows device-name, case-collision, and conservative
  Unicode-normalization checks;
- typed, capability-checked site ports instead of a monolithic driver;
- an experimental TJUPT session check, torrent search, and bonus catalog parser
  through one bounded, same-origin HTTPS GET per invocation, with fail-closed
  page recognition and no retry;
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
- versioned experimental JSON envelopes (`ptctl.dev/v1`) and control-safe
  human-readable tables.

Not implemented yet: metafile download, a persistent storage index, downloader
mutation, attributed/empty-file client-layout reconciliation, journaled
plan application, deletion, automatic plan execution, site writes, browser
login, third-party executable plugins, ratio manipulation, or Cloudflare
bypass.

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

Run `ptctl help`, `ptctl seed discover --help`, or
`ptctl reconcile report --help` for the complete surface.

Exit code `0` means a report or requested read succeeded, `1` an operational
failure, `2` invalid usage, and `3` an explicit integrity mismatch. Discovery
is report-oriented, so blocked results still print and return `0` by default.
Add `--require-verified` to return `4` unless `source_outcome` is exactly
`verified_unique`; target-plan or client-mapping failure does not erase source
evidence. `torrent verify` prints its result before returning `3`.
Reconciliation is also report-oriented. Add `--require-reconciled` to return
`4` unless the independently reported local axes are `consistent`; the report
is still printed first.

## Security model in one paragraph

A private `.torrent` is secret-bearing because its announce URL often contains
a personal passkey. Site cookies and downloader credentials are also secrets,
while filesystem reads can update atime, hydrate cloud placeholders, or incur
network cost. `ptctl` keeps credentials in memory, rejects secret arguments,
emits no request bodies, blocks cross-origin/downgrade redirects, never retries
site reads, bounds network and filesystem work, hides absolute discovery paths
by default, defaults conflicts to failure, and has no delete or apply command.
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
