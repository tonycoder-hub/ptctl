# Changelog

Each release has one section, headed by its exact `VERSION`. A merge to `main`
that changes `VERSION` publishes that section as the release notes; see
[docs/RELEASING.md](docs/RELEASING.md). Links inside a section are absolute,
because the notes are also shown on the release page.

## v0.4.0-alpha.1

The first published release of pt cli. Both `pt` and the compatible `ptctl`
entry point are in every package. Everything here is alpha: the commands, the
`ptctl.dev/v1` JSON schema, and the private-store formats can still change.

### What works

- **Metafiles:** strict bounded bencode, exact v1/v2/hybrid identity, and a
  private exact-byte, no-clobber metafile store.
- **Content proof:** v1 piece and v2 Merkle verification, bounded discovery of
  renamed or scattered bytes, and copy-only, journaled materialization.
- **TJUPT:** session, account, search, detail, and bonus reads; acknowledged
  metafile fetch; at-most-once bonus exchange; and `site login` with password
  and optional two-step code, saving the session cookie to an owner-only file.
- **M-Team:** session, account, search (with paging and filters), detail, own
  torrent lists (seeding, leeching, completed, incomplete, uploaded), and bonus
  reads; acknowledged metafile fetch and at-most-once bonus exchange through the
  official API; and `site login` saving the API key.
- **Saved credentials:** after `site login`, every site command uses the saved
  credential; `site logout` removes it.
- **Downloaders:** qBittorrent and Transmission reads, plus reviewed stopped
  adoption, recheck/start, stop, and keep-data removal of one exact job.
- **Reconciliation:** one report that keeps site, downloader, metafile, and
  storage evidence on separate axes.
- **Source retirement:** reviewed unlinking of exact source names and removal
  of exact empty parents.
- **Automation:** `--output json` on every command, stable exit codes, stdin-only
  secrets, and explicit `--acknowledge-*` flags for every effect; see
  [docs/AUTOMATION.md](https://github.com/tonycoder-hub/ptctl/blob/v0.4.0-alpha.1/docs/AUTOMATION.md).

### Known limits

- M-Team metafile fetch and bonus exchange pass offline tests but were not run
  against a real torrent or exchange. M-Team messages and RSS are not supported.
- A TJUPT session cookie expires; run `site login` again when a read reports
  the session is no longer authenticated.
- Concurrent `pt` processes do not share a request rate limit; run site
  commands one at a time.
- Packages exist for Windows amd64, Linux amd64, and macOS arm64 only, and are
  not code-signed or notarized.
