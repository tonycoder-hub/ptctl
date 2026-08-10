# TJUPT adapter

TJUPT is the first experimental site implementation, not a special case
embedded in the content core. It has not had a credentialed live smoke test in
this repository. The adapter currently declares three read capabilities:

- session check;
- torrent search;
- bonus catalog inspection.

Each command performs one bounded GET, uses the configured TJUPT HTTPS origin,
does not retry, refuses cross-origin redirects, and never submits a form. Page
recognition is fail-closed: a login page is unauthenticated, a positively
recognized bonus/search page is accepted, and maintenance, challenge, or
unknown HTML is indeterminate/an error rather than a successful empty result.
These current site reads perform no intentional filesystem write. Local
`metafile store init` and `metafile store import` are a separate, explicitly
writing boundary and do not contact TJUPT.

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

## B1: effectful metafile fetch gate

The natural next adapter methods remain torrent details and a clearly labeled
metafile fetch, but TJUPT does not yet declare or expose
`torrent.metafile.read_effectful`. A metafile GET may be recorded by the
tracker, and its response contains a passkey. B1 is gated on the private store
above and must not add a raw stdout or arbitrary-destination download path.

The planned command shape is:

```bash
printf '%s' "$TJUPT_COOKIE" | ptctl site metafile fetch \
  --cookie-stdin \
  --acknowledge-site-effect \
  --metafile-store PRIVATE_STORE \
  tjupt REMOTE_ID
```

Before reading the cookie or sending a request, B1 must validate all usage,
limits, the remote ID, and the initialized store's privacy/no-clobber
assurance. It then performs one bounded, same-origin GET with no retry, rejects
login/challenge/maintenance/HTML responses, strictly validates the exact raw
metafile, and hands those bytes to the same store import primitive. The report
may bind `(tjupt, remote_id)` to the resulting whole-raw variant for that
observation, but must not expose the request URL, response body, announce path,
passkey, cookie, temporary path, or absolute store path by default.

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

Store commands use exit `0` for successful and idempotent outcomes, `1` for
operational/store failures, `2` for invalid or conflicting selectors, and `3`
for invalid or corrupt metafile bytes. Exit `4` remains reserved for the
existing report-first verification/reconciliation requirement flags; B1 must
not silently redefine it.
