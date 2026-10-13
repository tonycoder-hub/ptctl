# Contributing

Contributions should preserve the boundary between sites, downloaders, storage,
and the content core.

Report security issues privately as described in the
[security policy](SECURITY.md), not in a public issue. If you work with a
coding agent, point it at [AGENTS.md](AGENTS.md); it is a concise version of
this page plus a repository map.

## Development setup

You need only the Go version named in `go.mod` (1.24 or newer). The test suite
is offline: it uses synthetic fixtures and local fakes, and never contacts a
real tracker or downloader.

```bash
go build -trimpath -o ptctl ./cmd/ptctl
go test ./...
```

## Before opening a pull request

```bash
gofmt -w ./cmd ./internal
go vet ./...
go test -race ./...
```

Add tests for malformed inputs and failure paths, not only happy paths. New JSON
fields must remain backward compatible within the `ptctl.dev/v1` envelope.

CI runs exactly these steps on Linux, macOS, and Windows:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -race ./...
go build -trimpath ./cmd/ptctl
```

`internal/fsbind`, `internal/metastore`, and `internal/storage` have
platform-specific files. If you change them, also run
`GOOS=windows go vet ./...` and `GOOS=linux go vet ./...` locally.

## Commits and compatibility

- Keep commits small and focused. Never mix a behavior-preserving refactor with
  a behavior change; reviewers check refactors by confirming that nothing
  observable moved.
- CLI text, JSON fields, `kind` values, stop reasons, public error messages,
  and exit codes are a contract that tests assert verbatim and scripts parse.
  Change one only deliberately, with its tests and documentation in the same
  commit.
- Site IDs, origins, route IDs, and record schemas are persisted in sealed
  records. Never change one; add a new `.vN` identifier instead.

## Where documentation goes

| Change | Update |
|---|---|
| A command or flag | The matching page in [docs/guide/](docs/guide/README.md), and `ptctl help` |
| A JSON kind, exit code, secret flag, or acknowledgement | [docs/AUTOMATION.md](docs/AUTOMATION.md) |
| A feature added or deliberately left out | [docs/FEATURES.md](docs/FEATURES.md) |
| A design decision or durable workflow | [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) |
| A security control or known gap | [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md) |
| A site adapter | Its own `docs/<SITE>_ADAPTER.md`, and [docs/ADDING_A_SITE.md](docs/ADDING_A_SITE.md) if an extension point changed |

Keep relative links working, and give every new page under `docs/` a link
back to the [documentation index](docs/README.md).

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

New site adapters follow [docs/ADDING_A_SITE.md](docs/ADDING_A_SITE.md): they
are registered in `internal/site/sites` and must pass the offline
`internal/site/sitetest` conformance suite.
