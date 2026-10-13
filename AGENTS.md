# AGENTS.md

Guidance for coding agents (Claude Code, Codex, and others) working in this
repository. People are welcome to read it too; the [README](README.md) and the
[documentation index](docs/README.md) are the general entry points. If you are
calling the `ptctl` binary as a tool rather than changing its code, read
[docs/AUTOMATION.md](docs/AUTOMATION.md) instead.

## What ptctl is

`ptctl` is a conservative, content-first CLI for private BitTorrent trackers.
It reconciles four independent ledgers (site record, metafile manifest,
downloader job, and storage inventory) around exact, verifiable torrent
metadata. Safety outranks convenience: it fails closed, reports before it
exits, never guesses missing data, never retries a site request, and never
leaks a secret. Read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the
design and [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md) for the controls.

## Checks

These are exactly the CI steps. Run all four before you consider a change done:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -race ./...
go build -trimpath ./cmd/ptctl
```

CI runs them on Linux, macOS, and Windows with the Go version from `go.mod`.
`internal/fsbind`, `internal/metastore`, and `internal/storage` contain
platform-specific files; after touching them, also run
`GOOS=windows go vet ./...` and `GOOS=linux go vet ./...`.

The whole suite is offline. Tests use `httptest` servers and synthetic
fixtures; nothing contacts a real tracker or downloader.

## Repository map

| Path | Responsibility |
|---|---|
| `cmd/ptctl` | `main`; calls `cli.Run` |
| `internal/cli` | Flag parsing, credential stdin, table/JSON rendering, and exit codes. One file per command group: `site*.go`, `torrent.go`, `metafile_store.go`, `storage*.go`, `client*.go`, `seed.go`, `materialize.go`, `source_retire*.go`, `reconcile*.go`; shared output in `output.go`; dispatch and help in `run.go` |
| `internal/domain` | Stable shared types: site descriptors, capabilities, auth methods, summaries, and snapshots |
| `internal/metafile` | Bounded bencode, exact v1/v2/hybrid hashing, manifests, and content verification |
| `internal/metastore` | Owner-only private store: exact-byte metafile artifacts and allowlisted sealed state records with atomic no-clobber publication |
| `internal/storage` | Bounded filesystem inventory, path semantics, identity guards, and host-to-client path mapping |
| `internal/storageindex` | Immutable storage profiles and sealed snapshot indexes |
| `internal/seed` | Source discovery and evidence-gated layout plans |
| `internal/materialize` | Journaled copy-only materialization |
| `internal/fsbind` | Fail-closed filesystem boundary for journaled operations |
| `internal/reconcile` | Axis-separated four-ledger reports |
| `internal/site` | Site ports (one interface per capability), `Adapter`, `Registry`, credentials, and descriptor validation |
| `internal/site/httpguard` | The only HTTP client site adapters use: HTTPS, one strict attempt, no redirect following, and bounded responses |
| `internal/site/sites` | The list of built-in site adapters |
| `internal/site/sitetest` | Offline conformance suite every site adapter must pass |
| `internal/site/tjupt` | TJUPT adapter, the reference implementation |
| `internal/sitebinding` | Sealed records binding a site fetch to an exact metafile |
| `internal/bonusexchange` | Durable, at-most-once bonus-exchange state machine |
| `internal/downloader` | Downloader ports and normalized state; `qbittorrent` and `transmission` subpackages |
| `internal/clientadopt`, `clientactivate`, `clientstop`, `clientremove` | Journaled downloader workflows for one exact job |
| `internal/sourceretire` | Source-retirement plans and execution, plus empty-parent cleanup |
| `internal/security` | Secret redaction applied to every error message |

## Invariants

Do not break these. If a task seems to require it, stop and ask.

1. **Observable behavior is a contract.** CLI text, JSON fields, `kind`
   values, stop reasons and the order they are checked in, public error
   messages, and exit codes are asserted by tests and parsed by scripts. A
   refactor must keep them byte-identical. A deliberate change needs a reason
   and its test updates in the same commit.
2. **Persisted identifiers are frozen.** Site IDs, route IDs, origins, and
   record schemas are stored in sealed records and verified on load. A new
   route shape gets a new `.vN` ID; existing IDs never change.
   `internal/site/tjupt/identity_test.go` pins TJUPT's.
3. **Fail closed.** Missing data is unknown, never zero. Budget exhaustion is
   `incomplete`, never "not found" or "unique". Names and sizes are only
   candidates; only piece or Merkle evidence is verified.
4. **One request, no retry.** A site read sends at most one bounded request,
   and nothing retries automatically. Bonus-exchange POSTs and downloader
   mutations make a durable attempt marker before the one request is sent;
   repeating an attempt whose outcome is unknown needs its own acknowledgement.
5. **Secrets only through stdin.** Cookies and passwords arrive through
   `--*-stdin` (interactive terminals are refused) and never appear in argv,
   output, errors, logs, or fixtures. Errors pass through `security.Redact`.
6. **Writes are explicit.** Ordinary reads, discovery, planning, and
   reconciliation write nothing. Never add a write, a tracker-visible request,
   or a downloader mutation to a read path; effects belong in separately
   named, acknowledged commands.
7. **Capabilities are declared, not guessed.** The CLI checks the descriptor
   and the typed port before it reads any credential. Every adapter must pass
   `sitetest.TestAdapter`.
8. **Fixtures are synthetic.** Never commit a real cookie, passkey, announce
   URL, private `.torrent`, downloader password, or HTML containing personal
   data. Use obviously fake values containing `CANARY`; redaction tests check
   that they never leak.

## Common tasks

**Add a site.** Follow [docs/ADDING_A_SITE.md](docs/ADDING_A_SITE.md): it
covers registration in `internal/site/sites`, the `internal/site/sitetest`
conformance suite, port contracts, persisted identity, and the known gaps for
API-token sites such as M-Team. `internal/site/tjupt` and
[docs/TJUPT_ADAPTER.md](docs/TJUPT_ADAPTER.md) show a complete adapter. Do not
extract a shared NexusPHP or "site kit" layer until a second adapter shows
which behavior is actually shared.

**Add or change a command.** Wire it into `runWithApp` and `help` in
`internal/cli/run.go`, implement it in its command-group file, and give any
new report type a case in `jsonKind` in `internal/cli/output.go`. Add tests in
`internal/cli` that assert exact output, then update the matching guide in
`docs/guide/` and the kind list in [docs/AUTOMATION.md](docs/AUTOMATION.md).

**Change a public message or stop reason.** Search the `_test.go` files for
the exact string first; most are asserted verbatim. Stop reasons that
`internal/cli` does not recognize are collapsed into a generic public code, so
a new one must be added to the CLI allowlist as well.

**Touch a journaled workflow** (materialize, adoption, activation, stop,
removal, retirement, cleanup, or bonus exchange). Read its section of
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) and
[docs/THREAT_MODEL.md](docs/THREAT_MODEL.md) first. Crash recovery, prune, and
forget semantics are deliberate and covered by long test suites.

**Edit documentation.** User guides live in `docs/guide/`, the feature
inventory in [docs/FEATURES.md](docs/FEATURES.md), design in
`docs/ARCHITECTURE.md`, security controls in `docs/THREAT_MODEL.md`, and the
tool contract in `docs/AUTOMATION.md`. Keep relative links working.

## Working style

- Keep commits small. Never mix a behavior-preserving refactor with a behavior
  change.
- Prefer reading the code to guessing: most packages have exhaustive tests that
  document intended edge cases.
- Match the surrounding code's naming, comment density, and error style.
  Public error text is fixed and generic by design; do not add detail that
  could carry a secret, a URL, or response content.
