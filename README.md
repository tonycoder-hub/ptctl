# pt cli

**pt cli** is a conservative, content-first CLI for private BitTorrent trackers.
It treats a tracker website, a downloader, and a filesystem as separate trust
domains and reconciles them around verifiable torrent metadata.

The primary command is `pt`; `ptctl` remains a compatible entry point. The
GitHub repository, Go module, private-store formats, and `ptctl.dev/v1` JSON
schema keep their existing names. Older `ptctl` examples below remain valid.

**Start here: [只读入门 / read-only quickstart](docs/READ_ONLY_QUICKSTART.md)**
for Windows packages, exact verification, downloader reconciliation, and a
small credential-free synthetic sample.

For an extracted macOS/Linux package, run `sh examples/readonly/try.sh` for an
offline walkthrough. Discover the core commands with `pt torrent --help`,
`pt help client list`, and `pt help config`.

> Status: `v0.4.0-alpha` development. Ordinary inspection, downloader reads,
> content proof, discovery, planning, and reconciliation are intentionally
> zero-write. Persistent writes and tracker-visible effects are confined to
> explicit store/index commands and separately acknowledged operations; see
> [write boundaries](#write-boundaries) and the full
> [status statement](docs/guide/README.md#current-status-and-write-boundaries).

中文简介：**pt cli** 以 `.torrent`、
实际文件、下载器任务和站点记录这四本账为核心，先精确校验，再生成
清晰、可审计的报告与计划。TJUPT（Cookie）与 M-Team 馒头（API 令牌）是首批
实验性站点适配器，而不是写死在核心里的站点。

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

## What works today

- **Metafiles:** strict bounded bencode, exact v1/v2/hybrid identity, and a
  private exact-byte, no-clobber metafile store.
- **Content proof:** v1 piece and v2 Merkle verification, bounded discovery of
  renamed or scattered bytes, and copy-only, journaled materialization.
- **Sites:** an experimental TJUPT adapter with session, account, search,
  detail, and bonus reads, plus acknowledged metafile fetch, at-most-once
  bonus exchange (confirmed with the account password from a stdin bundle),
  and password login with an optional two-step code that issues the session
  cookie;
  and an experimental M-Team adapter with session, account, and search reads
  through the official API and a site-issued API key.
- **Downloaders:** qBittorrent and Transmission reads, plus reviewed stopped
  adoption, recheck/start, stop, and keep-data removal of one exact job.
- **Reconciliation:** one report that keeps site, downloader, metafile, and
  storage evidence on separate axes.
- **Source retirement:** reviewed unlinking of exact source names and removal
  of exact empty parents.

The complete inventory, including what is deliberately not implemented, is in
[docs/FEATURES.md](docs/FEATURES.md).

## Write boundaries

| Boundary | Commands | Gate |
|---|---|---|
| Zero-write | `torrent inspect/verify`, `storage probe/map`, `seed discover/plan`, `client status/list`, ordinary `site` reads and `site bonus review`, `reconcile report`, and every `plan`, `status`, `list`, and `inspect` subcommand | None. Reads can still update atime, hydrate offline placeholders, or be recorded by the remote side. |
| Private store | `metafile store init/import`, `storage profile create`, `storage index refresh/refresh-discover`, `reconcile refresh-report`, `site bonus exchange prepare` | The explicit command itself. Owner-only and no-clobber; not encrypted. |
| Tracker-visible | `site metafile fetch` (one GET), `site bonus exchange submit` (at most one POST), `site login` (one POST and one verifying GET) | `--acknowledge-site-effect`, `--acknowledge-bonus-exchange`; `site login` is the command itself, with the cookie written only to a pipe or owner-only file |
| Target filesystem | `seed materialize run/resume/abandon` | `--acknowledge-filesystem-write`, `--acknowledge-abandon` |
| Downloader | `client adopt/activate/stop/remove run/resume` | `--acknowledge-client-add`, `--acknowledge-client-recheck`, `--acknowledge-client-stop`, `--acknowledge-client-removal`; repeats, re-adds, and observation-only adoption have their own flags |
| Source deletion | `seed retire run/resume`, `seed retire parent-cleanup run/resume` | `--acknowledge-source-deletion`, `--acknowledge-empty-parent-removal` |
| Local history | every `prune` and `forget` | `--acknowledge-operation-state-deletion`, `--acknowledge-historical-evidence-deletion` (bonus exchange: `--acknowledge-state-prune`, `--acknowledge-history-forget`) |

Run `ptctl help` for the exact flags of every command.

## Install

Preview packages for Windows amd64, Linux amd64, and macOS arm64 are built by
CI after tests and extracted-binary acceptance. Download the
`pt-cli-bundle-<commit>-<attempt>` artifact from a successful `ci` run on
[Actions](https://github.com/tonycoder-hub/ptctl/actions) (artifacts expire
after 14 days), then follow the
[checksum and extraction steps](docs/READ_ONLY_QUICKSTART.md). Each package
contains both commands, the short guide, synthetic samples, licenses, build
identity, and SHA-256 checksums. Packages are not code-signed or notarized.

To build from source you need Go 1.24 or newer. The published tags predate the
current code, so install from `main`:

```bash
go install github.com/tonycoder-hub/ptctl/cmd/pt@main
go install github.com/tonycoder-hub/ptctl/cmd/ptctl@main   # optional compatible name
```

Source builds report `dev` / `unknown` from `pt version`. CI stamps both
commands with `<VERSION>+g<commit-prefix>` and the full commit (on pull
requests, the tested merge commit). Maintainers stage releases with the
[release procedure](docs/RELEASING.md): it builds an existing version tag and
only ever creates an unpublished draft. See [CONTRIBUTING.md](CONTRIBUTING.md)
for local checks and synthetic binary acceptance.

## Five-minute start

Inspect a metafile without exposing its announce path or query string:

```bash
ptctl torrent inspect release.torrent
ptctl torrent inspect --output json release.torrent
```

List the built-in sites and the capabilities each one declares. Neither command
reads a credential or contacts a site:

```bash
ptctl site list
ptctl site capabilities tjupt
```

Check a site session. The complete `Cookie` header value is read only from
stdin; interactive TTY input is refused, and the cookie is never persisted:

```bash
printf '%s' "$TJUPT_COOKIE" | ptctl site status --cookie-stdin tjupt
```

For TJUPT, `site login --credential-bundle-stdin tjupt` can issue that cookie
from the account password and two-step code instead; see
[Site reads and bonus exchange](docs/guide/sites.md).

M-Team uses an API key from 控制台 → 实验室 → 建立存取令牌 instead of a cookie,
with the same stdin-only rules:

```bash
printf '%s' "$MTEAM_API_KEY" | ptctl site account --api-key-stdin mteam
printf '%s' "$MTEAM_API_KEY" | ptctl site search --api-key-stdin mteam "Ubuntu"
```

From there, the [command guide](docs/guide/README.md) walks through storing
metafiles, proving content, materializing a layout, handing it to a downloader,
and reconciling the result.

## Documentation

| If you want to… | Read |
|---|---|
| Run commands step by step | [Command guide](docs/guide/README.md) |
| See every implemented feature and known gap | [docs/FEATURES.md](docs/FEATURES.md) |
| Understand the design | [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) |
| Review security assumptions | [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md), [SECURITY.md](SECURITY.md) |
| Use M-Team with an API key | [docs/MTEAM_ADAPTER.md](docs/MTEAM_ADAPTER.md) |
| Understand the TJUPT boundary | [docs/TJUPT_ADAPTER.md](docs/TJUPT_ADAPTER.md) |
| Add support for another site | [docs/ADDING_A_SITE.md](docs/ADDING_A_SITE.md) |
| Contribute | [CONTRIBUTING.md](CONTRIBUTING.md) |
| Work on ptctl with a coding agent | [AGENTS.md](AGENTS.md) |
| Call ptctl from a script or agent | [docs/AUTOMATION.md](docs/AUTOMATION.md) |
| Browse everything by audience | [docs/README.md](docs/README.md) |

## Security model in one paragraph

A private `.torrent` is secret-bearing because its announce URL often contains
a personal passkey. Site cookies, site API keys, and downloader credentials are
also secrets,
while filesystem reads can update atime, hydrate cloud placeholders, or incur
network cost. `ptctl` keeps credentials in memory, rejects secret arguments,
emits no request bodies, blocks cross-origin/downgrade redirects, never retries
site reads, bounds network and filesystem work, hides private store/object and
discovery/reconciliation absolute paths by default, never exposes materialize
paths, defaults conflicts to failure, and confines private operation-state
deletion to separately acknowledged materialize, adoption, activation, and
source-retirement prune commands; exact tombstone erasure and source-name
unlink each have their own narrower acknowledgement. Commands such
as `storage probe` and `seed plan` keep their documented path-display
contracts. The private store uses owner-only permissions and atomic no-clobber
publication for both metafiles and allowlisted sealed state records; this is
access control, not encryption. Store init/import, storage profile
creation/index refresh, the artifact plus sealed-binding phases of an
acknowledged site metafile fetch, and acknowledged target-root-local
materialize operations (including explicit retention pruning and exact
tombstone forgetting), acknowledged exact stopped-job adoption (including
explicit retention pruning and exact tombstone forgetting), reviewed client
recheck/start (including explicit retention pruning and exact tombstone
forgetting), and acknowledged
source-name retirement (including its separate journal pruning and explicit
last-evidence forget transition), and exact empty-parent cleanup (including
its separate no-path journal pruning and explicit last-evidence forget
transition), are
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
                TJUPT    qBittorrent/Transmission  local/mounted*
               M-Team
            (more planned)
```

`*` A mounted remote is not seedable merely because it can be listed. Random
read behavior, mount health, client mapping, consistency, and cost must be
established separately.

Built-in site adapters are registered in one place, `internal/site/sites`, and
each must pass the offline `internal/site/sitetest` conformance suite.

The detailed design is in [ARCHITECTURE.md](docs/ARCHITECTURE.md), and the
site boundaries are in [TJUPT_ADAPTER.md](docs/TJUPT_ADAPTER.md) and
[MTEAM_ADAPTER.md](docs/MTEAM_ADAPTER.md).

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

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md), the [security policy](SECURITY.md), and
the [code of conduct](CODE_OF_CONDUCT.md).

## License

Apache-2.0. See [LICENSE](LICENSE).
