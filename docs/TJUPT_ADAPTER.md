# TJUPT adapter

TJUPT is the first experimental site implementation, not a special case
embedded in the content core. It has not had a credentialed live smoke test in
this repository. The adapter declares six ordinary read capabilities plus two
separately acknowledged effectful capabilities:

- session check;
- account snapshot read;
- torrent search;
- bounded torrent-detail observation;
- bonus catalog inspection;
- exact-option, zero-write bonus-offer review;
- durable at-most-once bonus exchange;
- acknowledged metafile fetch into the private store.

Ordinary read and metafile commands send at most one bounded GET. Bonus exchange
is the only form-submission surface: it sends one fresh review GET and at most
one POST after a durable attempt marker. All site transports use the configured
TJUPT HTTPS origin, do not retry, and refuse to follow redirects. Each
effectful acknowledgement is therefore scoped to a fixed request count.
Page recognition is fail-closed: a login page is unauthenticated, a positively
recognized bonus/search/detail page is accepted, and maintenance, challenge,
or unknown HTML is indeterminate/an error rather than a successful empty
result.
Ordinary site reads perform no intentional filesystem write. TJUPT-related
local store mutations are `metafile store init`, `metafile store import`, and
the store phase of `site metafile fetch`, plus the intent/attempt/outcome record
phases of `site bonus exchange`; storage profile/index commands are a separate
filesystem-ledger boundary and never contact TJUPT.

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

When the compatibility catalog parser sees exactly one canonical numeric
hidden `option`, it includes that value as a selector hint. It never substitutes
the displayed row number, and an absent/ambiguous selector remains unknown.
The stricter review below always refetches and uniquely rebinds the option; it
does not consume catalog JSON as authority.

## Zero-write bonus-offer review

The catalog preview is complemented by a stricter exact-option observation:

```bash
printf '%s' "$TJUPT_COOKIE" | ptctl site bonus review --cookie-stdin tjupt OPTION
```

Only the exact built-in production origin declares `bonus.offer.review`.
Output mode, capability, cookie authentication, fixed response/parser budgets,
and a canonical non-negative decimal option are validated before stdin is
read. The command opens one fresh HTTP/1.1 session and sends one bounded GET to
`mybonusapps.php`; it performs no POST, redirect, retry, compression, proxy,
or second request.

The bounded tokenizer requires authenticated page markers, a balance, exactly
one form carrying the selected hidden `option`, at least two visible
site-defined columns, and a recognized form structure. Duplicate options,
duplicate attributes, nested forms, malformed selected-form structure,
external form-owner controls, form-associated custom elements, browser-magic
charset controls, alternate command/popover submitters, base-URL or
submit-action overrides, unknown response types, and form/field/token/text
budget overflow fail closed. The public projection
classifies the submit control as available, disabled, or unknown and separately
classifies additional user input. A form is considered structurally supported
only for the production `mybonusapps.php` POST route; recognizing it still
grants no submission authority. The tokenizer does not execute JavaScript, so
the normalized route and retained text remain static HTML claims rather than
browser-rendered proof. The `visible_text_bytes` usage counter refers to bounded
non-markup tokenizer text; it does not assert CSS-rendered visibility.
HTML must be valid UTF-8; an explicitly declared conflicting charset is rejected
before parsing.

Hidden field values and names do not enter the report. Instead, control
name/type/disabledness order and the stable submitter value are reduced to a
domain-separated one-way form-shape ID. The semantic review ID binds that
shape, option, visible columns, action route, availability, and input mode,
while deliberately excluding changing opaque hidden values, balance, and
observation time. Both identifiers are review aids and stable correlators, not
signatures or replay authority. The typed result has
process-local authority only; JSON round trips lose it. The exchange workflow
below therefore refetches the page and uses only fresh opaque fields; this
review command itself never submits a purchase or redemption form.

The identifiers are versioned to the audited parser contract rather than being
permanent TJUPT identifiers. If parser hardening changes which controls affect
submission, their values may change intentionally and the operator must perform
a fresh review and prepare a new intent.

## Acknowledged bonus exchange

The effectful workflow is intentionally three commands rather than a replayable
`buy` shortcut:

```bash
ptctl site bonus exchange prepare \
  --state-store STATE --expect-review-id REVIEW_ID tjupt OPTION

printf '%s' "$TJUPT_COOKIE" | ptctl site bonus exchange submit \
  --state-store STATE --intent-record INTENT_RECORD_ID \
  --expect-review-id REVIEW_ID --cookie-stdin \
  --acknowledge-bonus-exchange tjupt OPTION

ptctl site bonus exchange status \
  --state-store STATE --intent-record INTENT_RECORD_ID
```

Prepare is local-only. Submit validates the exact production origin, capability,
cookie method, canonical option, budgets, private store, sealed intent, explicit
review ID, and current operation state before reading stdin. It then refetches
`mybonusapps.php`, requires the same semantic review ID and a unique supported
input-free static form, and retains its hidden values only in the live session.
The ordered successful controls must also fit the sealed intent's exact field
and encoded-byte budgets under the same encoder used by POST. Those limits
become part of the one-shot in-memory authority; changing them later is
rejected before submission.

Before any POST, a deterministic sealed attempt marker is published and jointly
re-verified with the intent under one physically bound store root. Only the
invocation that newly created that marker may continue. The ordered form is
sent once with a non-replayable body over a fresh HTTP/1.1 connection. Redirects
are not followed; exact site-local `do=` outcomes from the audited NexusPHP
route are reduced to fixed confirmation/rejection codes. All other complete or
partial responses are unknown.

The adapter returns a second process-local authority for the safe request
receipt. Together with the live reservation authority, it permits one canonical
outcome record. Confirmed, rejected, unknown, and known-not-submitted are
canonical terminal record states. The submitting invocation calls an outcome
durable only after publication and joint record-set verification succeed. If
no outcome can be safely sealed after a marker exists, the operation stays
`attempt_reserved_submission_unknown`.
Neither a new process nor JSON can recover submission authority, and rerunning
submit is rejected before the cookie is read. Status is credential-free and
never contacts TJUPT.

The marker coordinates one prepared operation within one preserved, uncloned
private-store history. A separately prepared operation is a separate explicitly
acknowledged submission. Copying, rolling back, or deleting operation records
can create an independent history that a local file marker cannot coordinate.
Read-only status proves the records currently visible in its selected store
through a bounded, detected-stable but
non-atomic scan; it does not infer the historical directory-sync/no-clobber
receipt, so current record verification and invocation-scoped durability are
reported separately.
The JSON assurance block likewise separates
`attempt_marker_blocks_future_submissions` from
`submission_request_bound_verified`: the first records the verified no-clobber
coordination state in the selected history, while the second requires a valid
live request receipt or a jointly verified outcome record carrying that
receipt. Neither extends coordination to copied or rolled-back stores.

After usage validation the commands are report-first. Prepare/status and a
durably confirmed submission use exit `0`; rejected, unknown, not-submitted, or
operationally incomplete submission results use exit `1`; usage is `2`; and
verified sealed-state corruption uses integrity exit `3`. This workflow does
not assign a meaning to exit `4`.

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
challenge, maintenance, empty-search, torrent-detail, title-with-size, and
bonus-form ambiguity/budget cases. Captured TJUPT HTML is deliberately absent because it can contain
account information, CSRF tokens, or identifiers. Any future fixture must be
generated or reviewed for secret canaries before commit.

## Torrent detail observation

The read-only command is intentionally narrow:

```bash
printf '%s' "$TJUPT_COOKIE" | ptctl site detail --cookie-stdin tjupt REMOTE_ID
```

Only the exact built-in production origin declares `torrent.detail`. Usage,
output mode, capability, authentication method, canonical positive-decimal
remote ID, origin, route, and fixed response budgets are validated before the
cookie is read. The request is a fresh HTTP/1.1 GET for
`details.php?id=REMOTE_ID`; it has no redirect, retry, compression, proxy, or
second request. In particular it omits `hit=1`. The reference NexusPHP
implementation increments the torrent view counter only when that parameter is
present, so ptctl deliberately does not authorize it: [NexusPHP
`details.php`](https://github.com/xiaomlove/nexusphp/blob/master/details.php).

Recognition requires authenticated UI, a bounded display heading, and an
internal action/download link carrying exactly the requested remote ID.
Seeder/leecher counts are optional and are retained only from the bounded peer
counter element. The report never emits the raw HTML, request/download URL,
description, cookie, redirect location, or arbitrary server diagnostics.

The result is only a same-invocation site-page observation. Its display title
may include promotion decoration; names, peer counts, and links are site
claims. It is not a metafile identity, proof of the site's current private
variant, a site signature, or storage-content proof, and it is not persisted.
The typed port returns opaque process-local authority. An explicitly
requested `reconcile report --site-ref tjupt/ID --site-cookie-stdin` may consume
that authority in the same invocation, but only to record that the selected
remote-ID page was observed; JSON replay cannot recreate it and it never proves
which private `.torrent` variant the site currently serves.

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
`torrent.detail`; invoking one does not invoke or upgrade the other. A metafile
GET may be recorded by the tracker, and its response contains a
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

`site metafile binding list --metafile-store DIR` is deliberately weaker: it
performs one bounded name inventory and returns only sorted, unverified record
locators. `site metafile binding inspect --metafile-store DIR RECORD_ID` then
jointly verifies one explicit canonical record and linked private artifact and
rechecks the adapter's production origin/route contract. Neither command reads
a cookie, sends a request, selects by remote ID, or chooses a latest record.

`reconcile report` consumes that provenance only through an explicit
`--site-binding-record` combined with the same store's
`--metafile-store/--metafile-variant`. It never enumerates by site/ref or picks
the newest record. The adapter ref/origin/route is revalidated before any
downloader password read or request. A valid binding is historical evidence
only and cannot upgrade incomplete storage/client/path axes or make the
downloader's raw private metafile observable.

When live site detail and a read-only downloader are requested together,
reconciliation requires `--credential-bundle-stdin` and accepts exactly:

```json
{"schema":"ptctl.credentials/v1","site_cookie":"SID=...","downloader_password":"..."}
```

The bounded detail GET runs before the downloader session. Its current site
claim remains separate from the sealed historical exact-response relation.

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
