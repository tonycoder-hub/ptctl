# Threat model

## Assets

- tracker cookies, RSS keys, auth keys, and passkeys;
- private metafiles whose announce URL contains a passkey;
- downloader credentials with broad queue and filesystem authority;
- local and mounted content, often the only copy of large media;
- torrent names, search queries, paths, and account statistics.

## Trust boundaries

The tracker, site adapter, downloader RPC endpoint, local filesystem, mounted
remote, terminal, logs, CI environment, and GitHub repository are distinct
boundaries. Data crossing one boundary does not grant authority over another.

## Major threats and present controls

### Secret leakage

Secrets are accepted only through stdin, held in memory, and excluded from
structured output. Tracker URLs are reduced to origins. Error boundaries apply
redaction for cookies, authorization headers, common token keys, announce URLs,
and download URLs. HTTP response bodies are not included in errors.

The redactor is defense in depth, not permission to log sensitive structures.

### SSRF and redirect leakage

Site origins must be HTTPS. DNS answers are checked before dialing and private,
loopback, link-local, multicast, and unspecified addresses are rejected. Each
redirect is checked for the same scheme, host, and effective port. Proxies are
disabled for site reads to avoid silently forwarding cookies.

Downloader endpoints are a separate, explicit trust decision because seedboxes
often live on private networks. They require HTTPS; plain HTTP is accepted only
for an explicit loopback address.

### Retry storms and account bans

The TJUPT adapter performs one bounded GET per command and never retries. HTTP
429 is returned as a terminal error. There is no cross-process limiter in the
alpha, so callers must not loop or parallelize site commands. There is no
Cloudflare or CAPTCHA bypass.

### Parser exhaustion

Bencode input, string size, depth, and node count are bounded. HTTP bodies and
qBittorrent responses have explicit limits. Torrent piece length is capped.

### Filesystem escape and corruption

Torrent components are validated before joining, including file/directory
prefix collisions. Existing paths are resolved
under an explicit root; case-colliding paths are rejected under insensitive
semantics. Hashing records file identity, size, and mtime before reading and
rejects detectable changes afterward. The v2 verifier also checks the same
open handle before and after each file read. Hybrid verification feeds both
hash families from that one read rather than combining two observations. This
is still detected stability, not an atomic filesystem snapshot or protection
against a malicious concurrent writer. The preview has no apply, overwrite,
move, or delete command. A plan records source metadata preconditions but
still requires exact apply-time re-verification.

### Supply chain

The runtime currently uses the Go standard library only. GitHub Actions are
pinned to full commit SHAs and receive read-only repository permission. Tests
construct synthetic metafiles; real tracker artifacts are forbidden.

## Known gaps before mutation support

- durable, serializable file identity and atomic-snapshot integration for
  adversarial concurrent modification;
- durable OS-keyring or audited credential-helper integration;
- reparse-point-safe Windows mutation using handle-relative APIs;
- journaled copy/reflink workflows with crash injection;
- downloader add/recheck/location transitions and private-mode verification;
- per-account cross-process rate-limit coordination;
- signed releases, SBOM, and build provenance.

No filesystem or tracker mutation should be added until the relevant gap has a
testable control and failure-recovery story.
