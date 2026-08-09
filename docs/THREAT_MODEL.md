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

Reconciliation also hides downloader content paths by default and emits a
one-way path reference instead. qBittorrent magnet URIs are never placed in a
domain object or report: the adapter extracts only bounded, typed `xt` hashes
and discards tracker, web-seed, display-name, and other query data. The generic
client hash remains an opaque locator and is represented by a derived report
ID where possible.
These stable references support local correlation; they are hiding controls,
not anonymity, and a guessable path may still be tested by dictionary attack.

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

A live reconciliation uses one qBittorrent login and two bounded torrent-list
reads, sequentially and without retry. When `auto` observes one uniquely
identified ordinary multi-file job, it attempts up to two bounded file-list
reads around storage proof; the second is sent only after a complete first
read. That path therefore makes at most five HTTP requests including login.
Authentication, rate-limit, HTTP, parse, or timeout failures make the
downloader axis incomplete; they do not trigger re-login, fan-out across queue
jobs, or a client mutation.
The audit session disables HTTP connection reuse and HTTP/2 so Go's transport
cannot transparently replay a failed idempotent GET behind the request counter;
the cookie jar still carries the authenticated session across fresh
connections.

### Parser, scanner, and solver exhaustion

Bencode input, string size, depth, and node count are bounded. HTTP bodies and
qBittorrent responses have explicit limits. Torrent piece length is capped.

The qBittorrent ledger is capped at 8 MiB and 25,000 jobs. Each magnet claim
is capped at 64 KiB, 256 query pairs, eight `xt` values, and 256 bytes per
decoded `xt`. The job array is decoded one object at a time, the N+1 job is
rejected before decoding, and each object is capped at 256 fields. Query keys
and `xt` claims must decode to strict ASCII; tracker and web-seed values are
not materialized. Duplicate JSON fields and opaque job keys fail closed.

Each qBittorrent file-list response has mandatory row, decoded path-byte, and
response-byte limits with lower defaults and non-disableable hard caps. Rows
are decoded incrementally, and row N+1 stops the snapshot before it can expand
memory. Required fields have explicit presence checks; missing zero-valued
fields cannot masquerade as an empty, skipped, or incomplete file. Duplicate
JSON fields or indices, malformed UTF-8, unpaired escaped UTF-16 surrogates,
invalid relative components, and unknown priorities fail the observation.
Before and after snapshots retain only bounded normalized rows and stable
findings, never the raw body.

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

Downloader reconciliation takes one identity snapshot before storage proof and
another afterward using the same authenticated session. For eligible ordinary
multi-file jobs, a file snapshot is taken after the first identity read and a
second file snapshot before the final identity read. Typed hashes, opaque job
key, content/save paths, size, and every conclusion-bearing indexed file field
must remain stable. A change makes the client relation incomplete. Even equal
outer and inner snapshots are only a bracketed, non-atomic observation: a job
may change between reads. Downloading, checking, moving, or allocating state
never upgrades a lexical path match into proof that the client is currently
reading the verified files. Client-reported paths are never passed to host
filesystem APIs.
Unknown client state text is normalized to `unknown` before entering a ledger
or report. Single-file path agreement also requires the client-reported size to
match. The expected path is derived from the same-call process-local storage
proof plus the invocation mapping; exported discovery fields cannot substitute
for that proof, and the public report drops the capability entirely.
For an ordinary multi-file job, every nonempty physical manifest index must
appear exactly once with its expected size, remain selected and fully seeded,
and have an effective lexical path equal to the independently mapped
same-call source binding. A matching top-level content path alone cannot reveal
skipped or renamed files. The qBittorrent formula is fixed as `save_path` plus
the returned relative file path, while `content_path` must be a consistent
ancestor; alternate formulas are not tried opportunistically. Any nonempty
file attribute (including padding or symlink semantics) and non-padding empty
files remain unsupported for this full-layout claim. Windows path case is
compared exactly rather than assuming case-insensitive semantics for a
particular directory or remote filesystem.

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
