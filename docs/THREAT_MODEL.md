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

Private metafile-store reports expose only an opaque store ID, the
whole-metafile SHA-256 variant ID, and safe parsed metadata by default. Source,
and store-root absolute paths require `--show-absolute-paths`; object paths are
never report fields. Raw artifact bytes, announce URLs, web seeds, passkeys,
and temporary paths are also never report fields. The store protects artifacts
with filesystem permissions; it does not encrypt them, so a process or
administrator that can read the store can recover every embedded tracker
secret.

Sealed storage profile/index records are equally private. They contain absolute
profile roots, raw relative filename components, sizes, mtimes, and platform
identity hints. Public profile/index reports omit absolute and inventory-
relative paths plus raw filesystem/root identity hints by default, but
disclosed record/profile/root IDs are stable correlators and not anonymity.
Anyone who can read the private store can recover the complete historical inventory.

### SSRF and redirect leakage

Site origins must be HTTPS. DNS answers are checked before dialing and private,
loopback, link-local, multicast, and unspecified addresses are rejected. Each
redirect is checked for the same scheme, host, and effective port. Proxies are
disabled for site reads to avoid silently forwarding cookies.

The effectful metafile fetch is stricter: it rejects every redirect so that one
acknowledgement cannot expand into another tracker-visible request. The report
accounts for whether the sole transport attempt started even when DNS, TLS, or
response processing later fails; it never derives a zero request count merely
from an error.

Downloader endpoints are a separate, explicit trust decision because seedboxes
often live on private networks. They require HTTPS; plain HTTP is accepted only
for an explicit numeric loopback address.

### Retry storms and account bans

The TJUPT adapter sends at most one bounded GET per command and never retries.
HTTP 429 is terminal. There is no cross-process limiter in the alpha, so
callers must not loop or parallelize site commands. There is no Cloudflare or
CAPTCHA bypass.

`site metafile fetch` is scoped to one validated remote ID and one GET. It does
not perform a preceding detail lookup, follow a redirect, retry, or fan out to
related IDs.

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
Metafile-store import and inspection use the same bounded parser and cap raw
artifact bytes before hashing or retaining them. An on-disk object name or
side record is never trusted as its digest or parsed identity.

The site metafile response is bounded while entering the private store import
pipeline and must be a complete, strictly valid metafile before publication.
An observed exact-variant relation additionally requires that store import
explicitly return a valid exact artifact reference whose whole-response digest
and consumed-byte receipt agree. Login, challenge, maintenance, unknown HTML,
oversized, and partial responses fail closed and never become an empty or
weaker artifact.

A persistent site binding is attempted only after complete artifact-import
success. Its canonical record is capped at 256 KiB, rejects duplicate/unknown
fields and non-canonical framing, and carries no credential, URL, announce
material, raw response, or path. Publication and load jointly hash the sealed
record and complete private artifact under one operation-bound store root.
Only an explicit record ID can create process-local reconciliation authority;
JSON round trips and public DTOs cannot recreate it.

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

The private metafile store has a narrower, stronger write boundary than content
discovery. `metafile store init` creates or validates a versioned store with
owner-only access. `metafile store import` first validates the complete input,
writes a private temporary object inside the same store and filesystem, flushes
it, and publishes it with a no-replace operation. POSIX uses a no-replace link
and then `fsync`s the final directory; Windows uses no-replace `MoveFileEx` with
write-through. The source is never moved, rewritten, or deleted, although its
read may update atime, hydrate a placeholder, or incur remote-filesystem cost.
A concurrent identical import is idempotent; a digest or content disagreement
is an integrity failure, never permission to overwrite.

After an initialization root exists, validation and store-layout/artifact
mutation are tied to one operation-bound root identity. POSIX uses held
directory handles for subdirectory creation, reads, staging, publication,
cleanup, and durability flushes; Linux checks both device and mount ID, and
macOS checks device plus filesystem ID. Windows holds no-delete guards
for every path prefix and for the root, `objects`, and `tmp`, verifies the volume
identity, and brackets `MoveFileEx` with identity checks. Root replacement,
subdirectory replacement, mount substitution, or a cross-volume object fails
closed. If identity is lost after publication, the receipt still records the
visible write and the operation cannot return ordinary success. tmpfs, ramfs,
and other volatile or unreviewed filesystems are rejected as durable stores.

Publication and durability confirmation are not one fact. A failure before
publication cannot create an accepted final object. After a complete object has
become visible, however, final-directory `fsync` or write-through confirmation
can fail. That result is `published_durability_unconfirmed` and
`writes_performed` may be `1`; the implementation must not claim zero visible
writes, overwrite the object, or delete it as rollback. A subsequent operation
must treat the object as untrusted until the normal bounded digest and parse
checks succeed.

Nor can a later read reconstruct historical publication evidence. Inspection
and idempotent import revalidate current digest, parse, and privacy properties,
but label historical no-clobber/durability as unobservable. A publication that
crossed its durability boundary but then failed cleanup or final validation is
reported separately as `published_post_commit_failure`; it is never folded into
an ordinary success.

POSIX stores require verified ownership plus owner-only directory/file modes.
Windows stores require a verified owner-only DACL, and reparse traversal is
rejected. Object path components are fixed ASCII digest material, avoiding
case-folding, Unicode-normalization, reserved-name, and server-filename
injection. An unsupported format, an existing unrecognized directory, or a
filesystem whose privacy/no-clobber behavior cannot be established fails
closed. The first format does not accept UNC/network stores. Unknown future
formats are not migrated in place automatically.

Every stored-artifact open re-hashes and parses the bounded original bytes.
Selecting a store object for inspect, verify, seed, or reconciliation grants no
write authority to that command. The paired `--metafile-store` and
`--metafile-variant` selector is mutually exclusive with a positional metafile
or `--torrent`; selector validation happens before reconciliation reads a
downloader password from stdin.

Allowlisted sealed state records share the private store's root binding,
owner-only staging, no-replace, durability, and corruption controls, but use a
kind-separated digest domain distinct from private metafile artifacts. There is
no generic record filename or payload CLI. Record loads stream and require the
consumer to observe EOF before the record can be accepted; provisional parsed
rows are discarded if the final digest, named identity, metadata, framing, or
schema check fails. Listing reads N+1 and becomes incomplete at entry, matching
record, or path-byte limits. Unknown entries fail closed instead of being
silently ignored.

Persistent filesystem snapshots introduce a stale-observation threat. A
complete descriptor proves only that one bounded enumeration completed at its
recorded interval. It does not prove the current filesystem contains no new,
removed, renamed, or permission-hidden files. An explicit descriptor selector
and a successfully re-hashed candidate do not strengthen that negative fact.
Consequently snapshot-only discovery always reports current search incomplete,
never emits a plan, never claims `not_found` or `verified_unique`, and cannot
make reconciliation consistent.

Profiles bind exact root bytes, platform/path semantics, one-filesystem/network
policy, and hard scan budgets. Display names and creation times are not
authority. A profile from another GOOS is inspectable but rejected for live
use before its native path bytes or downloader credentials are used. Same-GOOS
live use also requires absolute clean roots and scan limits no larger than the
sealed encoder can represent. On consumption, every retained locator is
decoded from canonical base64 and re-resolved beneath the current profile root.
Symlink, junction,
reparse, mount, non-regular, missing, size-changed, or unsafe paths are dropped.
Current filesystem/root identity must agree with the snapshot root observation;
a mismatch invalidates all candidate rows for that root. File identity or mtime
changes are only stale hints: a same-sized current regular file may proceed as
a candidate, but only the same-call identity-bound v1/v2/hybrid verifier can
upgrade its content evidence. Hardlink aliases remain separate paths so an
alias cannot be hidden by identity-based deduplication.

Snapshot publication is deliberately two-step. The streaming data object is
sealed first; only a complete, confirmed data publication permits a descriptor
commit. Orphan data is not discoverable as a snapshot. Concurrent writers can
publish different descriptors at one generation; maximum-generation ties are
ambiguous rather than resolved by an untrusted wall clock. A descriptor or
listing budget failure never falls back to an older record. Data and descriptor
write/durability receipts remain separate, including post-publication failure.
Before returning `stored`, both record IDs are re-opened and digest-verified in
one root-bound store session so a root replacement cannot splice a descriptor
and data observation from different physical stores. This remains a
detected-stable, non-atomic observation; every later read revalidates the sealed
records again.

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

For inspect, verify, discovery, planning, reconciliation, ordinary site reads,
and downloader reads, "read-only" means zero intentional filesystem mutation,
not zero observable side effect. Metadata and content reads may update atime,
wake disks, hydrate a cloud placeholder, traverse a FUSE/SMB backend, or incur
network cost. Network paths that can be recognized syntactically are opt-in,
and scans use one goroutine with no retry. Context cancellation is checked
between operations, but a blocked filesystem syscall may not be interruptible.
Users should narrow roots and budgets before scanning mounted remote storage.

`metafile store init`, `metafile store import`, `storage profile create`,
`storage index refresh`, and the artifact/binding phases of `site metafile fetch` are the
explicit write exceptions. Their reported write
count covers logical publication of an accepted store marker or immutable
object, not private temporary or uninitialized staging entries. It is nonzero
when that accepted state became visible, including a possible count of `1` for
`published_durability_unconfirmed`. Store inspect and every existing artifact
consumer remain zero-write.

The B1 site metafile fetch crosses two independent boundaries. The tracker may
record its passkey-bearing GET, so the command requires an explicit
site-effect acknowledgement scoped to one site reference. All usage, limits,
the one remote ID, capability, authentication method, and initialized store's
privacy/no-clobber assurances are validated before the cookie is read from
stdin. It then sends at most one bounded GET, with no detail lookup, redirect,
or retry, and passes the bounded exact response directly to the existing
no-clobber store primitive, which requires strict validation before publication.

The invocation-only `observed_exact_variant` relation records the exact store
reference established for that site reference during this invocation. A
pre-publication or import failure
retains only the bounded request/response receipt. The CLI must not independently
hash received bytes and promote that failure into a relation. Once store import
has returned the valid exact reference, a later durability or post-publication
failure does not erase the observation, but the observation still does not
prove durability. Request accounting, logical store writes, and durability
assurance remain separate even on failure. No raw response, request URL,
cookie, announce material, object or temporary path may be written to stdout,
reports, errors, or an arbitrary destination.

Only after a fully successful artifact import may the command publish a sealed
historical `site.metafile.binding.v1` commit marker. Marker publication is
binding-last rather than a two-object transaction; an orphan immutable artifact
is harmless when marker publication fails. Reconciliation requires the exact
record ID and the artifact selector from the same store, jointly revalidates
both objects, and repeats the installed adapter's canonical ref/origin/route
check before reading downloader credentials. It never chooses a record by
remote ID or observation time. The record proves only a past exact response,
not current site state or freshness.

### Supply chain

The runtime uses the Go standard library plus `golang.org/x/sys` for audited
OS-specific filesystem security primitives. GitHub Actions are pinned to full
commit SHAs and receive read-only repository permission. Tests construct
synthetic metafiles; real tracker artifacts are forbidden.

## Known gaps before mutation support

- handle-relative root confinement or storage-snapshot integration;
- durable, serializable file identity for apply-time preconditions;
- durable OS-keyring or audited credential-helper integration;
- journaled copy/reflink workflows with crash injection and rollback;
- downloader add/recheck/location transitions and private-mode verification;
- current-filesystem completeness tokens or journal-backed incremental index
  invalidation; the existing sealed snapshot is candidate-only;
- encryption-at-rest or an audited external-key design for private metafile
  stores on filesystems where owner-only ACLs cannot be enforced;
- per-account cross-process site rate-limit coordination;
- signed releases, SBOM, and build provenance.

No content, downloader, or tracker mutation should be added until the relevant
gap has a testable control and a failure-recovery story. The private metafile
store is limited to immutable, no-clobber artifact publication and grants no
authority over seeded content.
