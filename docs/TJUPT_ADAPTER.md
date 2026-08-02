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

## Next read capabilities

The natural next adapter methods are torrent details and a clearly labeled
metafile fetch. A metafile GET is potentially effectful: trackers may record a
download, and the result contains a passkey. It will require atomic private
storage and an explicit user acknowledgement before being exposed.
