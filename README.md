# ptctl

`ptctl` is a conservative, content-first CLI for private BitTorrent trackers.
It treats a tracker website, a downloader, and a filesystem as separate trust
domains and reconciles them around verifiable torrent metadata.

> Status: early preview. The implemented surface is intentionally read-only.
> `seed plan` verifies and explains a layout but does not apply it.

中文简介：`ptctl` 不是把 PT 网页机械地搬进终端。它以 `.torrent`、实际文件、
下载器任务和站点记录这四本账为核心，先精确校验，再生成清晰、可审计的计划。
TJU PT 是首个只读站点适配器，而不是写死在核心里的唯一站点。

## Why this shape?

The durable PT workflow is:

1. discover a release on a site;
2. preserve the exact metafile variant;
3. locate bytes on one or more storage systems;
4. verify pieces, including pieces spanning file boundaries;
5. build a seedable layout without accidental overwrite or deletion;
6. map the host path to the downloader's view;
7. reconcile the site, downloader, and storage records.

Bonus shops, invites, comments, and uploads are site-specific actions. They are
capabilities at the edge, not assumptions in the core domain model.

## Implemented today

- strict, bounded bencode parsing;
- exact v1 infohash calculation from the original `info` byte slice;
- v1, v2, and hybrid metafile inspection;
- exact v1 piece verification across multi-file boundaries;
- virtual zero handling for padding files;
- tracker output reduced to origins so announce passkeys are not printed;
- traversal, separator, Windows device-name, case-collision, and conservative
  Unicode-normalization checks;
- read-only storage probing and explicit host-to-client path mapping;
- a zero-write `seed plan` backed by exact piece verification;
- TJUPT session check, account/bonus snapshot, torrent search, and bonus catalog
  parsing through single-request, rate-limited, same-origin HTTPS reads;
- qBittorrent status and torrent-list reads over HTTPS (or explicit loopback
  HTTP), with passwords accepted only through stdin;
- stable JSON envelopes (`ptctl.dev/v1`) and human-readable tables.

Not implemented yet: v2 Merkle verification, metafile download, journaled plan
application, deletion, automatic cross-seeding, site writes, browser login,
third-party executable plugins, ratio manipulation, or Cloudflare bypass.

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

Verify an exact content root. For a multi-file torrent, `--content` is the
directory represented by the torrent's top-level name. For a single-file
torrent, it can be the file itself or its parent directory.

```bash
ptctl torrent verify --content "D:\PT\Release" release.torrent
```

Map a path seen by the host to a path seen by a Dockerized downloader:

```bash
ptctl storage map \
  --host-root "D:\PT" \
  --client-root /downloads \
  --client-style posix \
  "D:\PT\Release"
```

Generate a verified, zero-write materialization plan:

```bash
ptctl seed plan \
  --torrent release.torrent \
  --source "D:\Media\Release" \
  --target "D:\PT" \
  --output json
```

Read TJUPT without putting a cookie in shell history. The value supplied on
stdin is the complete `Cookie` request-header value from a session you already
control. Do not paste it into issues, logs, or chat.

```bash
printf '%s' "$TJUPT_COOKIE" | ptctl site status --cookie-stdin tjupt
printf '%s' "$TJUPT_COOKIE" | ptctl site search --cookie-stdin tjupt "Ubuntu"
printf '%s' "$TJUPT_COOKIE" | ptctl site bonus-catalog --cookie-stdin tjupt
```

Read qBittorrent state:

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client status \
  --url https://seedbox.example \
  --username admin \
  --password-stdin
```

Run `ptctl help` for the complete command surface.

## Security model in one paragraph

A private `.torrent` is a secret-bearing artifact: its announce URL often
contains a personal passkey. Site cookies and downloader credentials are also
secrets, while filesystem access can damage irreplaceable data. `ptctl` keeps
credentials in memory, rejects secret arguments, emits no request bodies,
blocks site redirects across origins or down to HTTP, performs no automatic
retry, limits response sizes, defaults conflicts to failure, and has no delete
or apply command in this preview. See [THREAT_MODEL.md](docs/THREAT_MODEL.md).

## Architecture

```text
                  +---------------- Core ----------------+
                  | bencode | manifest | verify | plan   |
                  +---------+----------+--------+--------+
                            |          |        |
                   SiteDriver   ClientDriver   Storage view
                       |             |              |
                     TJUPT       qBittorrent     local/FUSE*
```

`*` A mounted remote is not considered seedable merely because rclone can list
it. Random-read behavior, mount health, client-visible mapping, consistency,
and cost must be established explicitly.

The detailed design is in [ARCHITECTURE.md](docs/ARCHITECTURE.md), and the
site-specific boundary is documented in
[TJUPT_ADAPTER.md](docs/TJUPT_ADAPTER.md).

## Project principles

- Missing data is unknown, never silently zero.
- Names and sizes produce candidates; only piece/Merkle evidence is verified.
- No site capability is guessed when an adapter does not declare it.
- A GET that changes tracker accounting is labeled effectful before support is
  added.
- No ratio cheating, fake upload, DHT/PEX leakage for private torrents, or
  challenge bypass will be accepted.
- No real cookie, passkey, private metafile, or unredacted HTML fixture belongs
  in the repository.

## License

Apache-2.0. See [LICENSE](LICENSE).
