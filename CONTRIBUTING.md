# Contributing

Contributions should preserve the boundary between sites, downloaders, storage,
and the content core.

## Before opening a pull request

```bash
gofmt -w ./cmd ./internal
go vet ./...
go test -race ./...
```

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

