# Contributing

Contributions should preserve the boundary between sites, downloaders, storage,
and the content core.

## Before opening a pull request

The content-verification and reconciliation core is shared across Windows,
macOS, and Linux. Local validation and CI exercise the same implementation:

```bash
gofmt -w ./cmd ./internal
git diff --check
go vet ./...
go test -race ./...
go build -trimpath -o dist/ ./cmd/pt ./cmd/ptctl
```

Local Go builds, tests, and synthetic binary acceptance are permitted for this
project; this supersedes the earlier local-validation restriction recorded in
PR #8. A successful Mac test validates the shared core on Mac. Cross-compiling
a Windows binary establishes compilation only; Windows path, permission, and
package execution still require a Windows runner. These are compatibility
boundaries, not separate platform-specific core features.

The three-platform workflow runs `go vet ./...`, `go test -race ./...`, builds
both `cmd/pt` and the compatible `cmd/ptctl` with the same version/commit, and
runs `scripts/readonly_acceptance.py` against both delivered binaries after native
system extraction. Every platform compares two builds and byte-identical packages
before artifact upload; the final assembly checks that all three targets agree
on version and commit. CI and release calls share `.github/workflows/build.yml`.
The release controls have standard-library regression tests:
`python -B -m unittest discover -s scripts/tests -v`.
See [RELEASING.md](docs/RELEASING.md) for version/tag rules and draft-only delivery.
For a local snapshot, inject `cli.Version` and `cli.Commit` with the same `-X`
linker flags used by CI, then run the acceptance script with the two binaries
and matching `--version` / `--commit` values. Mark uncommitted builds as local
snapshots instead of attributing their changes to the clean base commit.
Python helpers use only the standard library. The synthetic acceptance starts
loopback-only fake downloader endpoints and never needs real credentials.

Use an authorized PR to trigger CI; pushing or publishing needs separate
authorization in an agent task. Record the run URL and exact tested commit.
Do not describe an unexecuted workflow as passing validation.

Add tests for malformed inputs and failure paths, not only happy paths. New JSON
fields must remain backward compatible within the `ptctl.dev/v1` envelope.

## Fixture policy

Never commit:

- a real private `.torrent`;
- an announce, RSS, or download URL containing a passkey/auth key;
- a site cookie or downloader password;
- captured HTML containing personal account data;
- a qBittorrent backup/resume file from a real client.

Construct synthetic bencode and HTML in tests. Use obvious canary secrets and
assert that they cannot reach output.

## Adapter policy

An adapter must declare small capabilities, use bounded parsing, fail closed on
unknown authentication/challenge states, and never guess unsupported actions.
Site write support requires a separate design review, CSRF handling, idempotency
analysis, and explicit user confirmation.
