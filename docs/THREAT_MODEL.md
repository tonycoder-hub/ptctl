# Threat model

## Assets

- tracker cookies, RSS keys, auth keys, and passkeys;
- private metafiles whose announce URL contains a passkey;
- downloader credentials with broad queue and filesystem authority;
- local and mounted content, often the only copy of large media;
- torrent names, search queries, absolute paths, and account statistics.

## Trust boundaries

The tracker, site adapter, downloader RPC endpoint, local filesystem, mounted
remote, terminal, logs, CI environment, and GitHub repository are distinct
boundaries. Data crossing one boundary does not grant authority over another.

## Major threats and present controls

### Secret leakage

Secrets are accepted only through stdin, held in memory, and excluded from
structured output. Tracker URLs are reduced to origins. Error boundaries redact
cookies, authorization headers, common token keys, announce URLs, and download
URLs. HTTP response bodies are not included in errors.

Discovery output hides absolute roots, source files, and target roots by
default. It emits stable root IDs, display-safe relative paths, and raw
components in base64. `--show-absolute-paths` is an explicit local disclosure.
Fatal diagnostics identify roots by ordinal or opaque ID where practical.

The redactor and path hiding are defense in depth, not permission to log secret
structures or publish private reports.

### SSRF and redirect leakage

Site origins must be HTTPS. DNS answers are checked before dialing and private,
loopback, link-local, multicast, and unspecified addresses are rejected. Each
redirect is checked for the same scheme, host, and effective port. Proxies are
disabled for site reads to avoid silently forwarding cookies.

Downloader endpoints are a separate, explicit trust decision because seedboxes
often live on private networks. They require HTTPS; plain HTTP is accepted only
for an explicit numeric loopback address.

### Retry storms and account bans

The TJUPT adapter performs one bounded GET per command and never retries. HTTP
429 is terminal. There is no cross-process limiter in the alpha, so callers
must not loop or parallelize site commands. There is no Cloudflare or CAPTCHA
bypass.

### Parser, scanner, and solver exhaustion

Bencode input, string size, depth, and node count are bounded. HTTP bodies and
qBittorrent responses have explicit limits. Torrent piece length is capped.

Storage discovery has non-disableable limits for roots, depth, directories,
entries, entries in one directory, retained candidates, retained raw path
bytes (including aliases), and diagnostics. The directory reader requests at
most `limit+1` entries and rejects an over-limit directory instead of retaining
an arbitrary OS-order prefix.

Candidate matching separately bounds candidates per file, total candidate
edges, manifest transitions, verified alternatives, proof work, and issues. It
explores v1 assignments as one incremental piece stream and v2 assignments
after exact file-root pruning; it never allocates the Cartesian product. Empty
files and padding consume state.
Virtual zeros are charged before hashing so a small metafile cannot force
unbounded CPU work. Bindings use one push/pop prefix, keeping live memory linear
in manifest depth.

Any limit hit sets the relevant `complete` flag false. Incomplete work cannot be
reported as not-found or uniquely verified. If two layouts were already proven,
ambiguity remains a positive fact even if later work stops.

### Filesystem escape, races, and corruption

Search roots must be explicit, non-overlapping directories. Inventory does not
follow symbolic links or Windows reparse points, does not retain special files,
and stays on the root filesystem. Windows UNC roots require `--allow-network`.
Mounted remote filesystems on Unix cannot be classified reliably from path
syntax and must be treated as an explicit user trust/cost decision. Cloud
placeholders represented as reparse points are skipped.

Torrent components are validated before joining, including file/directory
prefix collisions. A discovered file records root/component identity and the
platform file identity, size, and mtime. Each proof open rechecks the component
chain, compares the opened handle with that observation, and rechecks the chain
afterward. Hashing checks the same handle before and after each read. Hybrid
verification feeds both hash families from one physical read rather than
combining independent observations.

These controls detect ordinary replacement and metadata races but do not form
an atomic snapshot or a security boundary against a malicious concurrent
writer. Root/component traversal is best-effort and not handle-relative. A
filesystem snapshot or OS-specific `openat`/Windows-handle implementation is
needed for stronger guarantees. Reports and plans label this assurance
non-atomic.

The preview has no apply, overwrite, move, or delete command. A layout plan
records source metadata preconditions but still requires exact apply-time
verification.

### Read side effects and remote storage

"Read-only" means zero intentional filesystem mutation, not zero observable
side effect. Metadata and content reads may update atime, wake disks, hydrate a
cloud placeholder, traverse a FUSE/SMB backend, or incur network cost. Network
paths that can be recognized syntactically are opt-in, and scans use one
goroutine with no retry. Context cancellation is checked between operations,
but a blocked filesystem syscall may not be interruptible. Users should narrow
roots and budgets before scanning mounted remote storage.

### Supply chain

The runtime currently uses the Go standard library only. GitHub Actions are
pinned to full commit SHAs and receive read-only repository permission. Tests
construct synthetic metafiles; real tracker artifacts are forbidden.

## Known gaps before mutation support

- handle-relative root confinement or storage-snapshot integration;
- durable, serializable file identity for apply-time preconditions;
- durable OS-keyring or audited credential-helper integration;
- journaled copy/reflink workflows with crash injection and rollback;
- downloader add/recheck/location transitions and private-mode verification;
- persistent storage profiles/index with stale-observation invalidation;
- per-account cross-process site rate-limit coordination;
- signed releases, SBOM, and build provenance.

No filesystem or tracker mutation should be added until the relevant gap has a
testable control and a failure-recovery story.
