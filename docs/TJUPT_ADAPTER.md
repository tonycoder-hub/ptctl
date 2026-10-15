# TJUPT adapter

[Documentation index](README.md) · [Adding a site](ADDING_A_SITE.md)

TJUPT is the first experimental site implementation, not a special case
embedded in the content core. Its read commands were run against the live site
with a real session in October 2026. The adapter declares six ordinary read
capabilities plus one separately acknowledged effectful capability:

- session check;
- account snapshot read;
- torrent search;
- bounded torrent-detail observation;
- bonus catalog inspection;
- exact-option, zero-write bonus-offer review;
- acknowledged metafile fetch into the private store.

It does not declare bonus exchange; see
[Bonus exchange is withheld](#bonus-exchange-is-withheld).

Every TJUPT command sends at most one bounded GET, and none submits a form.
All site transports use the configured
TJUPT HTTPS origin, do not retry, and refuse to follow redirects. Each
effectful acknowledgement is therefore scoped to a fixed request count.
Page recognition is fail-closed: a login page is unauthenticated, and so is a
GET answered with an unfollowed redirect to the same origin's `login.php`
(TJUPT's expired-cookie response; `site status` reports `unauthenticated`
and strict reads stop with `authentication_required`). Any other redirect stays
`redirect_rejected`. A positively
recognized bonus/search/detail page is accepted, and maintenance, challenge,
or unknown HTML is indeterminate/an error rather than a successful empty
result. Search rows are read from the NexusPHP `<table class="torrents">`
with each row's nested `torrentname` table kept intact, so the later size
cell is not cut off; sizes use NexusPHP's 1024-based `KB`/`MB`/`GB`/`TB`/`PB`
labels, and a size of 2^63 bytes (8 EiB) or more is left absent rather than
reported wrapped or clamped. A results page whose rows cannot be recognized
is an error, not an empty result; only a recognized TJUPT empty-search message
(such as `没有种子。请用准确的关键字重试。`) yields zero results. The query `0`
is not special on the live site: it returns the ordinary listing.

The header row names its columns with icon classes (`time`, `size`,
`seeders`, `leechers`, `snatched`) and the `类型` label. When each of those
maps to exactly one column and a row has exactly as many top-level cells as
the header, the result also carries the category ID (the `cat` link value),
the publish time (TJUPT's UTC+8 wall clock, reported in UTC), seeders,
leechers, snatches, and the promotion. The promotion is read from the row
highlight class, the word and `pro_*` icon markers, and TJUPT's explicit
`Upload Rate`/`Download Rate` factors. A row without any marker is normal
(1x up, 1x down). Markers that disagree, an `*_promotion` label without
factors, or an unknown `pro_*` icon leave the promotion unknown rather than
guessed, and its end time is kept only from a single `剩余时间` title on a
promoted row. A header or row that does not fit this shape keeps only the name
and size.

TJUPT's published source cuts a name longer than 105 to 150 characters
(depending on font size and subtitle) two characters short and ends it with
`..`. Unless the user has turned on name
tooltips, the link's `title` keeps the full name. The result uses that title
only when it extends exactly what the list shows; otherwise it keeps the
shortened name. A row whose details link does not carry a canonical positive
ID is skipped, because `site detail` and `site metafile fetch` could not use
its ref.

Ordinary site reads perform no intentional filesystem write. TJUPT-related
local store mutations are `metafile store init`, `metafile store import`, and
the store phase of `site metafile fetch`; storage profile/index commands are a
separate filesystem-ledger boundary and never contact TJUPT.

The live site occasionally stalls partway through a response or resets the
connection. Each read is one HTTP request that must finish within 30 seconds
and is never retried automatically, so such a read fails closed (a generic
`site ... failed` error, or `site_request_failed` in a detail or review
receipt) rather than returning partial data. Rerunning the read-only command is
safe.

## Account snapshot

`site account --cookie-stdin tjupt` performs one bounded GET to the exact
authenticated `mybonus.php` route. It requires valid UTF-8, a bounded
username from the recognized page title, and a canonical bonus decimal. It
also reads the user panel (`<table id="info_block">`) when the page has
exactly one: uploaded total, user class (the site's own name, such as
`威震一方`), seeding points, invites, and the seeding and leeching counts next
to the panel's icons. A field is kept only when every statement of it in the
panel agrees, and is otherwise absent. TJUPT's panel shows no downloaded
total or ratio, so those stay absent; the panel's network-access line is
never read.

The snapshot is read-only and point-in-time. It neither authorizes a bonus form
submission nor proves that a prior exchange caused a balance change.

## Why the bonus catalog remains site-defined

TJUPT identifies itself as NexusPHP. Its authenticated `mybonus.php` page is
the bonus exchange surface (`mybonusapps.php` is an unrelated games page that
also shows the balance). The public NexusPHP language catalog illustrates the
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

Each catalog row is one exchange form. Its selector is the form's single
canonical hidden `option`. Its columns are the offer's `<h1>` name (not its
rules text), the row's other visible cells such as the price, the submit
label, and a status in bonus review's vocabulary: `available`,
`available, requires input`, `disabled`, or `unknown`. TJUPT numbers rows
differently from their options (option 14 is displayed as 13), so the
displayed number is dropped and never used as a selector. A form whose
controls carry script handlers, such as the gift offer's `onchange`, or fall
outside the modeled form structure is `unknown`. A duplicate or ambiguous
selector fails the whole catalog. The stricter review below always refetches
and uniquely rebinds the option; it does not consume catalog JSON as
authority.

## Zero-write bonus-offer review

The catalog preview is complemented by a stricter exact-option observation:

```bash
printf '%s' "$TJUPT_COOKIE" | ptctl site bonus review --cookie-stdin tjupt OPTION
```

Only the exact built-in production origin declares `bonus.offer.review`.
Output mode, capability, cookie authentication, fixed response/parser budgets,
and a canonical non-negative decimal option are validated before stdin is
read. The command opens one fresh HTTP/1.1 session and sends one bounded GET to
`mybonus.php`; it performs no POST, redirect, retry, compression, proxy,
or second request. Its route ID is `tjupt.mybonus_offer_review.v1`; the
earlier `tjupt.mybonusapps_*` IDs named the games page and are retired.

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
only for the production `mybonus.php` POST route; recognizing it still
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
process-local authority only; JSON round trips lose it. An exchange must
therefore refetch the page and use only fresh opaque fields; this review
command itself never submits a purchase or redemption form.

The identifiers are versioned to the audited parser contract rather than being
permanent TJUPT identifiers. If parser hardening changes which controls affect
submission, their values may change intentionally and the operator must perform
a fresh review and prepare a new intent.

## Bonus exchange is withheld

TJUPT does not declare `bonus.exchange.submit_effectful`. Its exchange form
does not complete on the first POST: the site answers with a page that asks for
the account password, and ptctl does not take account passwords. Rather than
send a POST that can only stop halfway, the production adapter withholds the
capability, its `BonusExchangeConfig` reports exchange unavailable, and
`site bonus exchange submit ... tjupt OPTION` is refused as usage before stdin
is read. The catalog and review are unaffected.

On the live site, posting an offer's form to `mybonus.php?action=exchange`
returns a `确认操作` page that states the price and the offer and carries a new
form with only the hidden `option` and a `passwd` password field. The balance
does not change until that form is confirmed.

The form machinery stays in the adapter: a fresh review GET, an exact-option
rebind against the review ID, at most one POST of the freshly observed form
after a durable attempt marker, and fixed outcome codes from the audited
NexusPHP route's `do=` redirects. Its action route ID is
`tjupt.mybonus_exchange_post.v1`. Its tests run against a test-only adapter
that enables it, so a future design that handles the password step can build
on it. The generic workflow is described in
[ARCHITECTURE.md](ARCHITECTURE.md#durable-at-most-once-bonus-exchange) and the
[sites guide](guide/sites.md).

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

For ordinary `site status`, `site account`, `site search`, and
`site bonus-catalog`, the CLI validates output mode, the selected adapter's
capability and cookie authentication declaration, the corresponding typed
port, and a nonblank search query where relevant before it reads stdin.
Adapter runtime errors are exposed only as fixed safe diagnostics, with typed
context cancellation or deadline causes preserved; raw cookie, request, and
response details do not cross the CLI boundary.

## Fixtures

Parser tests use synthetic HTML with fictitious users and values, including
challenge, maintenance, empty-search, torrent-detail, title-with-size, and
bonus-form ambiguity/budget cases. The search table, user panel, detail rows,
and bonus page fixtures follow the structure of pages observed live in
October 2026. Fuzz targets check the search, bonus page, user panel, and size
parsers' output invariants on arbitrary input. Captured TJUPT HTML is deliberately absent because it can contain
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
counter element. The size is read from the `基本信息` row only when that row
names the same torrent ID (`种子ID: ID | 大小: SIZE`), and the snatch count only
from links to `viewsnatches.php?id=ID`; statements that disagree leave either
field absent. The report never emits the raw HTML, request/download URL,
description, cookie, redirect location, or arbitrary server diagnostics.

TJUPT answers an ID with no visible torrent (one that never existed, was
deleted, or sits in the recycle bin) with HTTP 200 and a `错误` /
`没有该ID的种子` message box inside the normal authenticated page. A page that
is not a recognizable detail page, has no detail heading, and carries exactly
one such box stops with `not_found` rather than `unrecognized_response`. Any
other site message, such as the permission page for a banned torrent, stays
`unrecognized_response`.

The result is only a same-invocation site-page observation. Its display title
may include promotion decoration; names, peer counts, size, snatches, and
links are site claims. It is not a metafile identity, proof of the site's current private
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
