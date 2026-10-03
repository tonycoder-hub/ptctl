# Synthetic read-only sample

`demo.txt` contains exactly `pt cli synthetic sample\n` (24 bytes, LF).
`demo.torrent.b64` encodes a minimal private v1 single-file torrent with one
16 KiB piece and no announce URL, tracker, passkey, or real account data.
`expected.json` records the exact content SHA-256, whole-metafile SHA-256,
v1 infohash, and content length. The CI package includes the decoded
`demo.torrent` for immediate inspection and verification.

In an extracted macOS/Linux package, run `sh examples/readonly/try.sh` for a
five-step offline walkthrough with the bundled binary. It includes expected
partial-report and corrupt-copy failures, leaves the original samples unchanged,
and needs no Go, Python, network, or credentials. Use `./pt help config` to learn
how explicit paths and downloader connection flags work.

If inspecting source without a package, this PowerShell snippet decodes only
these public synthetic bytes into this directory (an explicit sample-file write):

```powershell
[IO.File]::WriteAllBytes((Join-Path (Get-Location) 'examples/readonly/demo.torrent'), [Convert]::FromBase64String((Get-Content -Raw examples/readonly/demo.torrent.b64)))
```

From the package root, use `pt.exe` (or the compatible `ptctl.exe`):

```powershell
.\pt.exe torrent inspect --output json .\examples\readonly\demo.torrent
.\pt.exe torrent verify --content .\examples\readonly\demo.txt --output json .\examples\readonly\demo.torrent
```

Expect `verified=true`, one expected/matched piece, and exit 0. Changing a byte
without changing file length must produce `verified=false` and exit 3. CI makes
that corrupt copy only in its temporary directory. The shared acceptance
script also creates temporary v1 multi-file (including an empty file), v2
piece-layer, and hybrid fixtures, checking each hash family and same-size
corruption. It exercises all exit codes and runs loopback fake qBittorrent
and Transmission ledgers with
synthetic authentication canaries, including rejected path mappings; it has
no real endpoint or credential input.

Do not replace these bytes with a private torrent or captured client data.
See `docs/READ_ONLY_QUICKSTART.md` at the package/repository root for path
mapping, partial-report semantics, and real-client read-only usage.
