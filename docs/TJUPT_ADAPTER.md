# TJUPT adapter

TJUPT is the first experimental site implementation, not a special case
embedded in the content core. It has not had a credentialed live smoke test in
this repository. The adapter declares three ordinary read capabilities plus
one explicitly effectful metafile capability:

- session check;
- torrent search;
- bonus catalog inspection;
- acknowledged metafile fetch into the private store.

Each command sends at most one bounded GET, uses the configured TJUPT HTTPS
origin, does not retry, and never submits a form. Ordinary reads refuse
cross-origin redirects. The effectful metafile fetch refuses every redirect so
its explicit acknowledgement authorizes at most one tracker-visible request.
Page recognition is fail-closed: a login page is unauthenticated, a positively
recognized bonus/search page is accepted, and maintenance, challenge, or
unknown HTML is indeterminate/an error rather than a successful empty result.
Ordinary site reads perform no intentional filesystem write. TJUPT-related
local store mutations are `metafile store init`, `metafile store import`, and
the store phase of `site metafile fetch`; storage profile/index commands are a
separate filesystem-ledger boundary and never contact TJUPT.

## Why the bonus catalog remains site-defined

TJUPT identifies itself as NexusPHP. Its authenticated `mybonusapps.php` page
is a bonus/karma surface. The public NexusPHP language catalog illustrates the
range of actions that installations may expose: upload/download credit,
invites, VIP or custom titles, bonus gifts and charity, no-ad periods, H&R
cancellation, attendance cards, medals, temporary invites, rainbow IDs, and
username-change cards. It also describes seeding rewards as functions of time,
size, seeder count, weight, and site-specific factors.

Reference: [NexusPHP `lang_mybonus.php`](https://github.com/xiaomlove/nexusphp/blob/php8/lang/en/lang_mybonus.php).

An individual tracker can enable, disable, rename, price, or replace any of
these. Therefore `ptctl` returns catalog rows as site-defined columns. It does
not pretend that every tracker has a universal `buy_vip` or `exchange_upload`
operation, and the preview implements no purchase path.

## Authentication

The adapter does not automate login, bypass CAPTCHA/Cloudflare, or read browser
cookie stores. A caller may provide the complete Cookie header value through
stdin for one process:

```bash
printf '%s' "$TJUPT_COOKIE" | ptctl site status --cookie-stdin tjupt
```

The value is held in memory and not persisted. Direct interactive terminal
input is refused to avoid echoing secrets; callers must use a pipe. A future credential provider
must use an OS keyring or an audited pipe-based helper; plaintext fallback will
not be automatic.

## Fixtures

Parser tests use synthetic HTML with fictitious users and values, including
challenge, maintenance, empty-search, and title-with-size cases. Captured
TJUPT HTML is deliberately absent because it can contain account information,
CSRF tokens, or identifiers. Any future fixture must be generated or reviewed
for secret canaries before commit.

## Private metafile prerequisite

Private `.torrent` files contain account-specific announce material. `ptctl`
therefore provides an explicit local store before exposing a site download:

```bash
ptctl metafile store init --store PRIVATE_STORE
ptctl metafile store import --store PRIVATE_STORE existing.torrent
ptctl metafile store inspect --store PRIVATE_STORE sha256:WHOLE_RAW_SHA256
```

The variant ID is SHA-256 over the complete original byte stream, not merely
the `info` dictionary. Import preserves those exact bytes without moving,
rewriting, or deleting the source; reading it may still update atime or hydrate
a placeholder. Import publishes an immutable content-addressed object with
private permissions and atomic no-clobber semantics. Re-importing the same
artifact is idempotent. This permission isolation is not encryption.
Store and import-source absolute paths are hidden by default; object paths,
announce URLs, passkeys, web seeds, and raw bytes never enter reports.

Existing `torrent inspect`, `torrent verify`, `seed discover`, `seed plan`, and
`reconcile report` commands can consume a stored variant by replacing their
file/`--torrent` source with the paired `--metafile-store DIR
--metafile-variant ID` flags. The alternatives are mutually exclusive, and all
stored consumers remain zero-write.

## B1: effectful metafile fetch

TJUPT declares `torrent.metafile.read_effectful` independently of
`torrent.detail`; no detail capability or detail request is implied. A
metafile GET may be recorded by the tracker, and its response contains a
passkey. B1 is gated on the private store above and has no raw stdout,
arbitrary destination, or caller-chosen sidecar path. Its only durable
provenance output is an internal allowlisted sealed binding record in that same
store.

The command shape is:

```bash
printf '%s' "$TJUPT_COOKIE" | ptctl site metafile fetch \
  --cookie-stdin \
  --acknowledge-site-effect \
  --metafile-store PRIVATE_STORE \
  tjupt REMOTE_ID
```

The acknowledgement must be explicit and is scoped to the single validated
remote ID in this invocation. Before reading the cookie or sending a request,
B1 validates all usage, limits, the capability and authentication method, the
remote ID, and the initialized store's privacy/no-clobber assurance. It then
performs one bounded, same-origin GET with no redirect or retry, rejects
login/challenge/maintenance/HTML responses, and passes the bounded exact bytes
directly to the same store import primitive. That pipeline requires a complete,
strictly valid metafile before no-clobber publication.

The invocation-only `observed_exact_variant` relation may bind
`(tjupt, remote_id)` to the whole-raw variant only when store import explicitly
returns a valid exact artifact reference and its whole-response digest and
consumed-byte receipt agree. A pre-publication or import failure retains only
the bounded request/response receipt; the CLI never re-hashes the received body
to upgrade that failure into a relation. Once the exact reference has been
established, a later durability or post-publication failure does not erase the
observation. It remains neither a durability claim nor a current-site claim.

After a fully successful artifact import, the command publishes a canonical
`site.metafile.binding.v1` record as a binding-last commit marker. The record
contains the canonical TJUPT production origin and
`tjupt.download_by_id.v1` route, exact whole-response artifact link, bounded
single-request account, and historical observation interval; it contains no
cookie, request URL, announce/passkey, raw bytes, filename, or path. Publication
and every load jointly verify the record and referenced private artifact under
one operation-bound store identity.

`reconcile report` consumes that provenance only through an explicit
`--site-binding-record` combined with the same store's
`--metafile-store/--metafile-variant`. It never enumerates by site/ref or picks
the newest record. The adapter ref/origin/route is revalidated before any
downloader password read or request. A valid binding is historical evidence
only and cannot upgrade incomplete storage/client/path axes or make
qBittorrent's raw private metafile observable.

The report accounts for the site request and logical store publication
separately. The request URL, redirect location, response body, announce path,
passkey, cookie, server filename, object path, and temporary path never enter
output or errors. The store root remains hidden unless the ordinary
absolute-path disclosure is explicitly requested.

If no-clobber publication succeeds but its durability confirmation fails, B1
must propagate `published_durability_unconfirmed`: a complete artifact may be
visible and the write count may be `1`. It must not claim zero writes, delete
that artifact as rollback, or retry the tracker GET.

The store operation remains bound to one reviewed local root/filesystem
identity from staging through final verification. Volatile memory filesystems
and namespace replacement fail closed.

A later inspect or idempotent import validates current bytes but cannot upgrade
that history. Its publication assurance remains unobservable; a completed
publication followed by cleanup or validation failure is reported as
`published_post_commit_failure`.

The fetch reports before returning any non-usage operational or integrity
failure. It uses exit `0` only after the artifact and sealed binding both
verify, `1` for site, credential, store, binding publication, or durability
failures, `2` for invalid usage or missing acknowledgement, and `3` for invalid
exact metafile bytes or a corrupt artifact/binding. Exit `4` remains reserved for the existing
report-first verification/reconciliation requirement flags.
