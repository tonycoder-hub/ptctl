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
URLs. HTTP response bodies are not included in errors. Torrent-detail reports
retain only an allowlisted display title, optional peer counts, and fixed
evidence codes from a positively recognized page; descriptions, raw HTML, and
request/download URLs are never report fields.

Discovery output hides absolute roots, source files, and target roots by
default. It emits stable root IDs, display-safe relative paths, and raw
components in base64. `--show-absolute-paths` is an explicit local disclosure.
Fatal diagnostics identify roots by ordinal or opaque ID where practical.

Materialize is stricter: neither JSON, tables, nor errors expose absolute or
raw source, target, operation, journal, staging, or scratch paths, and there is
no `--show-absolute-paths` mode. Full operation IDs, plan IDs, metafile variant
IDs, and filesystem identities remain stable correlators, not anonymity.

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
loopback, link-local, multicast, and unspecified addresses are rejected.
Redirects are rejected. Proxies are disabled for site reads to avoid silently
forwarding cookies.

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

Ordinary status, search, bonus, and detail reads use the same fresh HTTP/1.1,
no-reuse, no-redirect transport as the effectful fetch. The detail route sends
only the canonical `id`; it deliberately omits NexusPHP's view-counting `hit`
parameter and never follows the download reference found in the page.

`site metafile fetch` is scoped to one validated remote ID and one GET. It does
not perform a preceding detail lookup, follow a redirect, retry, or fan out to
related IDs.

A live reconciliation may first use one independent, bounded site-detail GET.
It then uses one qBittorrent login and two bounded torrent-list reads,
sequentially and without retry. When `auto` observes one uniquely
identified ordinary multi-file job, it attempts up to two bounded file-list
reads around storage proof; the second is sent only after a complete first
read. The downloader path therefore makes at most five HTTP requests including
login; a combined site+downloader invocation makes at most six.
Authentication, rate-limit, HTTP, parse, or timeout failures make the
downloader axis incomplete; they do not trigger re-login, fan-out across queue
jobs, or a client mutation.
The audit session disables HTTP connection reuse and HTTP/2 so Go's transport
cannot transparently replay a failed idempotent GET behind the request counter;
the cookie jar still carries the authenticated session across fresh
connections.

Site-only and downloader-only reconciliation retain their single-secret stdin
formats. A combined invocation requires one `ptctl.credentials/v1` JSON object,
hard-limited to 128 KiB, with exactly the three allowlisted fields. Duplicate,
unknown, missing, trailing, invalid UTF-8, and unpaired-surrogate input is
rejected before either network session opens. Raw bundle bytes and both secrets
remain in memory only and never enter a report or diagnostic.

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

Torrent-detail HTML is separately capped at 4 MiB by default and 8 MiB at the
hard ceiling, with a 64 KiB response-header cap. Invalid UTF-8, login,
challenge, redirect, wrong media type, missing authenticated markers, an
unbounded/invalid heading, or no exact selected-ID action link fails closed.
Only fixed metadata fields survive parsing; the result never becomes a
metafile, content, or current-variant proof.

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

A sealed index may authorize only one explicitly named match that the current
invocation successfully reopens and cryptographically verifies. This produces
`verified_selected`, never a claim that candidate enumeration was complete.
The immutable profile, descriptor, generation, and match are domain-separated
into the reviewed plan ID; a different snapshot or locator identity cannot
silently reproduce it. Public JSON drops the process-local source capability.
The intermediate candidate query also has a private digest over its profile,
records, accounting, diagnostics, and fresh observations, so relabeling a live
candidate DTO or replaying JSON cannot synthesize this selection authority.

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
Selecting a store object for inspect, verify, discovery, planning, or
reconciliation grants no write authority to that command. For materialize the
stored pair supplies metafile bytes only; its separate acknowledgement grants
target-root writes, never store mutation. The paired `--metafile-store` and
`--metafile-variant` selector is mutually exclusive with a positional metafile
or `--torrent`; selector validation happens before reconciliation reads a
downloader password from stdin or materialize accesses either filesystem.

Materialize has a separate target-root write boundary. `run` and `resume`
require `--acknowledge-filesystem-write`; `abandon` requires the narrower
`--acknowledge-abandon`; `prune` requires
`--acknowledge-operation-state-deletion`, one full operation ID, and the
reviewed plan ID. All selectors, acknowledgements, timeouts, fixed-limit
invariants, operation/plan IDs, and syntactic discovery budgets are validated
before any metafile, search-root, target-root, or journal I/O. An acknowledgement
does not authorize overwrite, downloader mutation, or source changes. Only the
prune acknowledgement authorizes deletion, and only inside the selected
owner-private operation subtree; the final layout, source, other operations,
and retained tombstone remain outside that authority. Deleting that exact
tombstone is a separate `forget` operation requiring the same explicit full
selectors plus `--acknowledge-historical-evidence-deletion`; it cannot select
latest/by-age state or touch content.

Client adoption has a separate downloader-write boundary. `plan` performs one
complete typed ledger observation but writes nothing. `run` requires
`--acknowledge-client-add`; a repeat after an unknown result additionally
requires `--acknowledge-repeat-add`. Before password stdin or network access,
the CLI validates the exact private-store artifact selector, committed or
retained materialize selector, current target-root/final proof, host/client
mapping, endpoint, user-derived client configuration ID, reviewed adoption plan
ID, and any existing operation selector it can inspect locally. The
acknowledgement authorizes only one exact-metafile qBittorrent add request in
stopped mode plus the small private target-root-local journal. It does not
authorize changing an existing job, rechecking, starting, pausing, moving,
removing, deleting, or retiring content.

One complete before-ledger must prove typed identity absence. A generic qB job
hash, name, size, path, progress, or state never selects identity; unavailable,
invalid, partial, conflicting, or duplicate typed rows make absence
unprovable. The canonical request intent is durable before the POST. The
effectful transport is HTTP/1.1-only, fresh/no-keepalive, proxy-free,
redirect-free, bounded, serial, and non-retrying. A lost response remains
unknown even if the body may have reached qBittorrent. Resume reads the current
ledger first and does not repeat without both acknowledgements.
Read-only status validates the canonical marker namespace but does not infer a
historical directory-fsync result. Effectful resume refreshes that durability
boundary before it reads the client ledger or relies on a marker.

Adoption pruning is a separate local deletion authority. It requires one full
operation ID, its reviewed plan ID, and
`--acknowledge-operation-state-deletion`; all syntax and hard limits are
checked without reading password stdin. It first seals the exact canonical
intent, bounded attempt chain, completion, and marker IDs into a private
retention intent. Only after that marker is durable may it remove the selected
operation's original marker files and empty scratch directory, followed by an
exact namespace audit and a retention completion marker. A crash after the
intent blocks ordinary resume and can be recovered only by the same explicit
prune selector. No latest/by-age selection, downloader request, final-content
write, or cross-operation deletion is authorized. A complete tombstone can
create downstream completion authority only through a fresh bound read; its
public DTO and JSON round trip remain powerless and it does not establish
current downloader state.

Adoption tombstone forgetting is a third, separately acknowledged local
deletion authority. It accepts only one full operation ID and reviewed plan ID.
The exact retained intent, completion, and bounded attempt chain are copied
into a canonical domain-separated root recovery intent before any retained
marker is removed. Its deterministic owner-private name and payload bind the
target-root and operation identities; publication is no-clobber. Staged intent,
root intent, partial retained-marker deletion, empty-directory removal,
operation-root removal, and final-marker removal are fail-closed recovery
boundaries. Status does not sync, and resume, completion proof, and prune stop
before credentials or network access. Unexpected objects, another operation,
client jobs, content, and source names are outside authority. Once the last
marker is durably absent, later absence is unattributed. Deleting storage cannot
revoke an opaque process-local completion capability issued before forgetting.

After the POST, a terminal marker requires one unique exact typed job, stopped
state, reviewed size and exact lexical save/content paths, plus a second exact
final verification. These are bracketed, non-atomic observations. They neither
prove that qBittorrent stored the submitted private variant nor that it has
checked or is reading the materialized bytes. The public report uses only
one-way client/path/job references and never includes host/client paths,
endpoint, username, password, generic job key, magnet URI, tracker material,
or raw metafile bytes.

Client activation is a second, narrower existing-job mutation boundary. It is
unavailable without a same-invocation exact final authority and a canonical
stopped-adoption completion from the same client configuration and path
mapping. `run` requires `--acknowledge-client-recheck`; optional start requires
`--start-after-recheck` in the reviewed plan, then a later `resume` with
`--acknowledge-client-start`. Repeating an inconclusive request additionally
requires the matching repeat acknowledgement. No acknowledgement grants pause,
move, removal, deletion, source retirement, or a different job selector.

Activation pruning is a separate local deletion authority. It requires one
full activation operation ID, its reviewed plan ID, and
`--acknowledge-operation-state-deletion`; all syntax and hard limits are
checked without password stdin or network access. It first seals the exact
terminal activation intent and completion plus a bounded manifest of every
original marker's canonical name, domain-separated ID, and size. Only after
that retention intent is durable and rebound may it remove those exact files
and the empty scratch directory. It then audits the exact remaining namespace
and publishes a retention completion. A crash after the intent blocks ordinary
resume and can be advanced only by the same explicit prune selector. There is
no latest/by-age selection, downloader request, content write, source unlink,
or cross-operation deletion authority. A complete tombstone can recreate
downstream activation-completion authority only through a fresh bound read;
its public DTO and JSON round trip remain powerless and it does not establish
current downloader state.

Activation tombstone forgetting is a third, separately acknowledged local
deletion authority. It accepts only one explicit full operation ID and its
reviewed plan ID. The complete retained intent and completion are copied into a
canonical, domain-separated root recovery intent before any retained marker is
removed. The marker name is deterministic from the operation ID, the file is
owner-private, publication is no-clobber, and its payload is bound to the exact
target-root and operation identities. Staged intent, root intent, partial marker
deletion, empty-directory removal, operation-root removal, and final-marker
removal are all fail-closed recovery boundaries. Ordinary status does not sync;
resume, completion proof, and prune stop before credential or network access.
The authority cannot remove content, source names, client jobs, another
operation, or any unexpected object. Once the last marker is durably absent,
success attribution is intentionally gone and subsequent absence is reported
as unattributed rather than idempotently successful. This storage transition
cannot revoke an opaque process-local completion capability already issued to a
concurrent caller before forgetting began.

Source-retirement planning is a separate read-only boundary. It accepts a
downloader password only from stdin but accepts no mutation acknowledgement and
always reports zero writes, zero deletion, and `deletion_authority: none`. Eligibility requires a
new complete unique live source verification, a current exact final proof, and
one canonical terminal activation marker chain or complete retention tombstone.
Recheck-only is terminal at
recheck completion; reviewed start requires activation completion. The client
marker remains historical. Current use is proved separately with one login and
two bounded typed-job reads in one session; ordinary multi-file layouts add two
bounded indexed file reads. No request is retried and no client mutation route
is called. Identity, stable complete state, and effective paths remain client
claims and are never treated as proof of a current process, remote open inode,
or private metafile ownership.

For every content-bearing source name, the planner places named regular-file
reobservation, post-selection exact source reverification, and exact final
verification between the two live-client observations.
It rejects a source path inside the final namespace and rejects a source/final
`SameFile` alias. Source paths are not opened for writing and qB paths are never
used as host paths. Default reports retain only domain-separated path
references; explicit path disclosure does not change the plan ID. The result
does not cover empty files, padding, directories, symlink targets, cleanup, or
unlink semantics. Unselected hardlink or alias names may remain, and no space
reclamation is claimed. The qB effective paths are parsed only as remote lexical
claims and are never passed to host filesystem APIs. The plan is non-atomic
review evidence, not a promise that a later delete is safe.

Source-retirement execution is a distinct irreversible boundary. `run` and
`resume` require `--acknowledge-source-deletion`, a full reviewed SHA-256 plan
ID, the same metafile/final/activation/mapping/client selectors, and explicit
source roots. `run` repeats the complete source/final/client proof and compares
the fresh plan ID before its first write. It ignores any path-disclosure request
and never parses plan JSON as authority. `resume` first validates the explicit
private journal, root scope, and all local authorities before password stdin or
network access. Status reads only one explicit journal or retained tombstone
and never reads source roots or credentials. `prune` has a different
`--acknowledge-operation-state-deletion` boundary and accepts only the explicit
operation plus reviewed plan ID; it does not inherit source-unlink authority or
read a client credential.

The target-root-local operation subtree is protected by the same bound-root,
owner-private, no-follow journal primitives as materialization, with its own
reserved prefix. Its intent is canonical, size-bounded, and private because it
contains exact absolute source parents/names. Any component reserved for ptctl
materialize, adoption, activation, or retirement control state is ineligible
as source content. Every public path is instead a domain-separated pseudonymous
reference. The implementation pre-encodes the
intent and all fixed marker shapes before creating the subtree, so an
undersized protocol budget cannot leave predictable initialization debris.
Operation/journal/scratch namespaces are bounded and exact; duplicate keys,
unknown fields, trailing data, invalid identities, unsafe objects, replacement
of a bound parent/root/name, and marker disagreement are integrity failures.

For each selected ordinary content-bearing name, a canonical durable attempt
marker precedes the unlink. The unlink is relative to a bound direct-parent
handle and requires the exact reviewed name, identity, regular type, and size;
links/reparse points and directories are never followed or removed. Parent
directory durability is separate from observed absence. A completion marker is
published only after absence is observed. After a crash, absence is recoverable
only if the durable attempt already exists; otherwise it is unexplained loss
and fails integrity. An ambiguous unlink or durability failure retains its
attempt/receipt and returns partial rather than retrying blindly.

After all names, the exact materialized final is reverified and the same
authenticated downloader session reobserves the exact typed job/effective
layout before the terminal marker. These brackets do not freeze a remote client
or filesystem after the last check. No operation removes a source parent,
empty/padding entry, unselected hardlink name, final, client job, or private
metafile. No outcome promises block reclamation, cleanup, rollback, or the
absence of an out-of-band writer.

All usage, metafile, current-final, terminal-activation, mapping, endpoint, and
live-source discovery/preflight failures are handled before password stdin and
before opening a downloader session. Authentication or bounded read failure is
reported as incomplete with the exact request count available from the adapter;
it does not trigger re-login, retry, or a fallback name/path selector. Public
reports omit the endpoint, username, password, opaque job key, magnet URI, and
raw host/client paths.

The CLI validates artifact/materialize/adoption/mapping/endpoint selectors and
inspects an explicit resume journal before password stdin. The activation plan
also includes a fresh qB version descriptor and current exact job-layout
digest, so a new run must read the credential and bounded client state before
it can compare the reviewed activation plan ID. Any mismatch is still rejected
before journal creation or mutation. The core requires a fresh session with
exactly one login request, then counts exactly one descriptor GET, one ledger
GET, an optional one-file-ledger GET for the unique job, and at most one
explicitly selected POST per transition.

The qB control adapter accepts only known 4.x/5.x application generations and
binds their different resume/start route internally. Effectful requests are
HTTP/1.1-only, proxy-free, no-keepalive, redirect-free, bounded, serial, and
non-retried. The opaque job locator is used only as a URL-encoded `hashes` form
value. Transport, HTTP, response, report, and journal errors never include that
locator, response bodies/headers, endpoint, username, credential, magnet URI,
or tracker material.

A successful request receipt is not a recheck or start completion receipt.
Recheck completion requires a durable checking observation followed by a
complete stopped observation, or a same-invocation incomplete-to-complete edge
after a durable request intent. A fast check with neither observable edge stays
unknown. Start completion requires a durable recheck completion and start
attempt, complete started state, exact file-layout claims, and another current
final proof. Cancellation or an unreadable after-ledger leaves an attempted
request unknown. Resume observes first and never automatically replays it.
Start is authorized only by a later resume after durable recheck completion;
one invocation sends at most one effectful client POST.

Every marker is canonical, size-bounded, no-clobber, identity-bound, and reread
by name across a namespace bracket. The journal permits at most three explicit
attempts per action and at most one pending scratch marker. Read-only status
does not refresh directory durability and calls current client/final state
unobserved. Even a successful terminal report means same-invocation bracketed,
non-atomic exact filesystem proof plus bounded client claims; it does not prove
that qB holds the private metafile variant, has the same inode open, or cannot
change immediately after observation.

The target root must support the fsbind root-identity, no-link/reparse,
same-filesystem, and no-replace primitives. The plan-review root identity is
rechecked before journal creation. Journal, scratch, and stage objects live
beneath an opaque target-root-local operation subtree; the final layout is one
validated raw relative component. Source locators never enter the durable
intent. Every recovery selects one canonical full operation ID, replays an
immutable hash-chain, and rejects an intent, event, object identity, namespace,
or installed-limit disagreement. Enumeration is available only as a bounded
name-only list whose results remain `not_inspected`; no latest operation is
chosen.

The source can still change between non-atomic checks. A successful live
discovery retains process-local proof plus file preconditions; each copy is
opened and identity/precondition bracketed against that authority. The complete
stage and complete final namespace are independently verified against the exact
metafile. Recovery before stage verification therefore requires new live
discovery; recovery after it trusts neither a historical source locator nor
journaled hashes alone and reopens the staged/final objects. This detects
ordinary replacement and corruption but does not make concurrent filesystem
mutation atomic.

Publication intent is durable before the no-replace final transition. Any
ambiguous attempt or failed durability confirmation is reported with its actual
or uncertain write receipt and is not deleted as rollback. `abandon` is allowed
only before publication intent, appends one terminal event, and retains stage
and scratch bytes. It must never be described as cleanup or rollback. Source
files are only read: materialize does not move, rewrite, link, or delete them,
although their reads can still update atime or hydrate placeholders.

A materialize or source-retirement `status` call without an ID is only a
bounded root-name inventory. It returns canonical operation directories as
`not_inspected` and canonical root forget markers as
`forget_in_progress_not_inspected`, never opens their contents, never chooses a
latest operation, and emits no unrelated root name. N+1 entry, name-byte, or
retained-operation limits make the result incomplete; malformed objects under
the reserved operation prefix fail closed.

Retention pruning never treats an absent object as proof that it removed it.
Before deletion it binds the terminal journal or an already durable retention
intent, completely inventories the allowlisted private subtrees within fixed
object/path/byte/depth limits, and rejects links, reparse points, mounts,
hardlinks, unsafe types, identity drift, and unexpected names. Each unlink or
empty-directory removal is identity-bound and separately reports visibility
and parent-directory durability. The intent marker precedes deletion; the
completion marker follows an exact tombstone audit. A crash therefore resumes
from the same explicit operation ID rather than selecting or sweeping state.
The same ordering applies to a terminal source-retirement operation. Its
retention marker is authorized only by the complete canonical per-name journal;
ordinary source-retirement resume cannot cross the durable prune boundary.
Only the flat private journal and empty scratch directory are eligible for
removal. Source names, parents, the materialized final, downloader state, other
operations, and the retained tombstone remain out of scope. The source-retire
tombstone is historical and never asserts current source absence.

Forgetting either a materialize or source-retirement tombstone is a distinct
irreversible authority. It is allowed only for one explicit complete tombstone
after that workflow's dedicated acknowledgement.
The implementation publishes a private root-level intent containing the exact
no-path tombstone evidence before removing any retained marker. It removes the
operation subtree by bound identity while holding its cooperative lock, uses
the root intent to recover an exact empty lockless residue after a crash, and
removes the root intent last. The intent does not authorize source, final,
client, or unrelated-operation deletion. Root-marker replacement, hardlinks,
unexpected namespace entries, operation/root identity drift, and ambiguous or
unconfirmed durability all fail closed with actual/uncertain write receipts.
After confirmed last-marker removal, the target root has no remaining
historical attribution and absence is therefore `absent_unattributed`, not
idempotent success.

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

Layout-plan output remains zero-write and explicitly reports `layout_only`,
`effect:none`, and `ready_to_apply:false`. Materialize accepts only a reviewed
`seed discover --target` plan ID from the same selector/search-root/target
shape, never the standalone `seed plan` ID and never serialized plan/discovery
JSON as proof. `run` repeats live discovery and exact verification in the
writing invocation; early `resume` phases require the same fresh process-local
authority. The implemented mutation is copy-only and no-clobber. There is still
no move, source rewrite, source delete, overwrite, or automatic plan-execution
command.

### Read side effects and remote storage

For inspect, verify, discovery, planning, reconciliation, ordinary site reads,
downloader reads, and the discovery/read phases of materialize, metadata and
content reads may update atime, wake disks, hydrate a cloud placeholder,
traverse a FUSE/SMB backend, or incur network cost. "Read-only" means zero
intentional mutation, not zero observable side effect. Network search roots
that can be recognized syntactically are opt-in, and scans use one goroutine
with no retry. Materialize targets remain local-only. Context cancellation is
checked between operations, but a blocked filesystem syscall may not be
interruptible. Users should narrow roots and budgets before scanning mounted
remote storage.

`metafile store init`, `metafile store import`, `storage profile create`,
`storage index refresh`, the artifact/binding phases of `site metafile fetch`,
acknowledged materialize operations, and acknowledged exact stopped-job
adoption/activation are the explicit write exceptions.
Store/index/fetch reported write
count covers logical publication of an accepted store marker or immutable
object, not private temporary or uninitialized staging entries. It is nonzero
when that accepted state became visible, including a possible count of `1` for
`published_durability_unconfirmed`. Materialize instead separately counts
operation subtrees/directories, journal objects/events, scratch/staged objects,
publication attempts, logical publications, ambiguous writes, and bytes. Store
inspect and every non-materialize/non-adoption/non-activation artifact consumer
remain zero-write. Adoption separately counts its operation/control directories,
temporary marker bytes, no-clobber marker publications/removals, uncertain
writes, login/ledger/add requests, and add receipt.
Activation uses the same private-write accounting shape but separately reports
descriptor/ledger/file-ledger reads, recheck/start attempt markers, exactly
attempted client actions, completion markers, and unknown request results.

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

When explicitly requested, a live detail observation runs before downloader
reads and contributes only an opaque same-invocation remote-ID page claim. Its
public DTO or JSON round trip has no authority, and its display title, peer
counts, and links cannot establish metafile identity. Failure makes the live
site axis incomplete; success cannot replace the sealed historical binding or
upgrade storage, client, path, or private-variant proof.

### Supply chain

The runtime uses the Go standard library plus `golang.org/x/sys` for audited
OS-specific filesystem security primitives. GitHub Actions are pinned to full
commit SHAs and receive read-only repository permission. Tests construct
synthetic metafiles; real tracker artifacts are forbidden.

## Known gaps before broader mutation support

- broader quota/age policy
  (materialize, adoption, activation, and source-retirement heavy state have
  exact explicit pruning and each tombstone family has a separately
  acknowledged forget transition; no operation is selected automatically by
  policy);
- reflink/cross-filesystem materialization and reviewed network-target support;
- durable OS-keyring or audited credential-helper integration;
- downloader pause/location/removal transitions, re-adoption after a terminal
  job disappears, and client-side private-variant observability;
- source-parent directory cleanup, block-reclamation accounting, and explicit
  retirement of unselected aliases; journaled retirement intentionally removes
  only the reviewed regular-file names;
- current-filesystem completeness tokens or journal-backed incremental index
  invalidation; the existing sealed snapshot is candidate-only;
- encryption-at-rest or an audited external-key design for private metafile
  stores on filesystems where owner-only ACLs cannot be enforced;
- per-account cross-process site rate-limit coordination;
- signed releases, SBOM, and build provenance.

qBittorrent location mutation remains deliberately unsupported. Its official
WebUI contract accepts a job hash and a download location, but does not expose
the no-clobber precondition, collision result, atomicity, or recoverable commit
receipt required to distinguish a safe pointer update from a data move:
[qBittorrent WebUI API 5.0](https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-%28qBittorrent-5.0%29#set-torrent-location).
ptctl will not infer those guarantees from an HTTP 200 response.

No broader deletion or downloader mutation beyond exact private operation-state
pruning and explicitly selected tombstone forgetting, acknowledged source-name
retirement, exact stopped-add, and reviewed recheck/start slices,
tracker write, or broader content strategy should be added until the relevant
gap has a testable control and a failure-recovery story. The private metafile
store grants no authority over seeded content, a materialize acknowledgement
grants no authority outside its explicit copy-only target-root-local operation,
and a client-add acknowledgement grants no authority over an existing job or
source retirement.
