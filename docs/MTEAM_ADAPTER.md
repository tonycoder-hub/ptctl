# M-Team adapter

[Documentation index](README.md) · [Adding a site](ADDING_A_SITE.md)

M-Team (馒头PT, TP :: mTorrent) is the second experimental site and the first
that uses a site-issued API key instead of a session cookie. The adapter talks
only to M-Team's own hosts, starting at the official JSON API at
`https://api2.m-team.cc`, and declares four reads and one acknowledged
download:

- session check (`site status`);
- account snapshot (`site account`);
- torrent search (`site search`), with category names;
- torrent detail (`site detail`);
- metafile download (`site metafile fetch`), which is tracker-visible and needs
  `--acknowledge-site-effect`; see [Metafile download](#metafile-download).

The reads were smoke-tested live on 2026-10-16 with a newly created API key:
`site status` reported `authenticated`, `site account` read the profile, `site
search` returned results with category names in both table and JSON output, and
`site detail` observed a public software torrent and reported `not_found` for
an ID with no torrent. A creation time the API returned for that key matched
UTC+8, confirming how [times](#results) are read. The download was checked live
the same day for connectivity only, against an ID with no torrent, because the
test account was still in its newbie-task period. The unit tests use synthetic
responses shaped after the public API.

## Getting an API key

Sign in to the M-Team website and open 控制台 (Console) → 实验室 (Lab) →
建立存取令牌 (Create access token). Treat the token like a password: anyone
holding it can act as you through the API. Revoke it on the same page if it
leaks.

## Usage

Save the key once. `site login mteam` asks for it with echo off, checks it with
one status read (`POST /api/member/profile`), and saves it only if the API
accepts it:

```bash
ptctl site login mteam
```

After that, every M-Team command uses the saved key:

```bash
ptctl site status mteam
ptctl site account mteam
ptctl site search mteam "Ubuntu"
ptctl site search --output json mteam "Ubuntu"
ptctl site detail mteam REMOTE_ID
ptctl site metafile fetch --acknowledge-site-effect --metafile-store "$PTCTL_STORE" mteam REMOTE_ID
```

The key is saved in plaintext to `<user config dir>/ptctl/sessions/mteam.api-key`
(on macOS `~/Library/Application Support`, on Linux `$XDG_CONFIG_HOME` or
`~/.config`, on Windows `%AppData%`). The file is owner-only, in an owner-only
directory, and ptctl refuses to use it if group or other users can read it or
if it is a symlink. `ptctl site logout mteam` deletes it; revoke the key on the
M-Team page as well if it may have leaked. The key is never accepted as an
argument and never printed.

Scripts and agents can save the key without a terminal, or skip saving and pipe
it to one command. A piped key always takes precedence over the saved one:

```bash
ptctl site login --api-key-stdin --output json mteam < "$API_KEY_FILE"
op read op://Private/M-Team/api-key | ptctl site account --api-key-stdin mteam
```

Without a saved key and without `--api-key-stdin`, an M-Team command fails with
a usage error that names `ptctl site login mteam`, before anything is read or
sent. `--cookie-stdin` is refused because the adapter does not accept cookies.

## Requests

Each read sends exactly one request and never retries (the download takes more;
see [Metafile download](#metafile-download)):

| Command | Request | Body |
|---|---|---|
| `site status`, `site account` | `POST /api/member/profile` | an empty `multipart/form-data` form |
| `site search` | `POST /api/torrent/search` | `{"mode":"normal","keyword":QUERY,"pageNumber":1,"pageSize":50}` |
| `site detail` | `POST /api/torrent/detail` | a `multipart/form-data` form with the one field `id` |

The API exposes these reads as POST requests. All three endpoints only read;
the reads send no other endpoint. In particular, `site detail` never calls
`/api/torrent/viewHits`, which the website's own detail page calls to count a
view. The request uses the same strict transport as every
site read:

- HTTPS to the fixed origin only, HTTP/1.1, a fresh connection per command;
- no proxy, no redirect following, no automatic retry;
- the key travels only in the `x-api-key` header, and the `User-Agent`
  identifies ptctl honestly;
- request bodies are capped at 16 KiB and responses at 4 MiB;
- the body is not replayable, so the HTTP stack cannot resend it silently.

Search queries must be at most 256 bytes of printable UTF-8. Do not loop or
parallelize commands; leave a few seconds between them, because ptctl has no
limiter that spans processes.

## Results

**Status.** `authenticated` with the username when the profile has one;
`unauthenticated` when the API rejects the key (HTTP 401 or API code `401`);
`indeterminate` when the API answers but the profile is not recognized.

**Account.** Username, uploaded and downloaded bytes, share ratio, and bonus
(魔力值). Ratio and bonus are kept as the decimal text the API returned. A
counter that is missing or malformed is left absent rather than guessed, but a
profile without a username is an error rather than a partial account.

**Search.** The first page of up to 50 results in normal mode. Each row has the
ref `mteam/ID`, name, size, category ID and name, publish time, seeders,
leechers, completed count, and promotion. The table view adds `SEEDERS`,
`LEECHERS`, `PROMOTION`, and, last, `CATEGORY` columns. A response without a
result list, or with any row that has no canonical ID or name, fails the whole
search instead of returning a partial or empty page.

**Categories.** JSON keeps the site's category ID in `category` and adds its
name in `category_name`, for example `"category": "422"` with
`"category_name": "软件"`. Names are the simplified-Chinese `nameChs` values of
`/api/torrent/categoryList` as of 2026-10-16, compiled into ptctl so a search
costs no extra request. A few newer categories carry traditional-script names on
the site itself (`電子書`, `遊戲`) and are kept verbatim. An ID that is not in the
table, such as a category the site adds later, keeps its ID and gets no name;
the table shows `-`.

**Detail.** One torrent's name, size, seeders, leechers, and completed count.
The answer must echo the requested ID and carry a name, or the read fails as
`unrecognized_response`; a counter that is missing or malformed is left absent.
The evidence basis is the same list every detail route reports. For this route,
`authenticated_torrent_detail_page` means the API accepted the key and answered
with code `0`, and `exact_remote_id_page_link` means the answer echoed the exact
requested ID. `DOWNLOAD REFERENCE` is always `false`, because the API answer has
no download link to observe. An ID with no visible torrent stops as
`not_found`; the API reports it as code `1` with a fixed message, and only that
exact pair is read as not found, because the API uses code `1` for other
refusals too. Live-site reconciliation reads the same detail, as described
in [Reconciliation](#reconciliation).

**Times.** The API returns times without a zone, such as `2026-10-14
09:30:00`. ptctl reads them as UTC+8, the site's time zone, and reports them in
UTC.

**Promotions.** Discounts are reported as traffic factors:

| API discount | Upload | Download | Table shows |
|---|---|---|---|
| `NORMAL` or absent | 1 | 1 | `-` |
| `FREE` | 1 | 0 | `free` |
| `_2X_FREE` | 2 | 0 | `up 2x, free` |
| `PERCENT_50` | 1 | 0.5 | `down 0.5x` |
| `PERCENT_70` | 1 | 0.7 | `down 0.7x` |
| `_2X` | 2 | 1 | `up 2x` |
| `_2X_PERCENT_50` | 2 | 0.5 | `up 2x, down 0.5x` |

A promotion with an end time also shows `until TIME`. An unknown discount name
is shown as `unknown` and omitted from JSON, never guessed.

## Errors

The API's own message text is never shown, because it is chosen by the server.
These lines name the cause instead:

| Error | Meaning | What to do |
|---|---|---|
| `site CMD failed: the credential read from stdin does not have the format this site uses` | The key contains spaces, non-ASCII characters, or is too long. | Copy the token again. Leading and trailing whitespace is already trimmed. |
| `site CMD failed: the site rejected the credential` | The API refused the key (`account` and `search`; `status` reports `unauthenticated` instead). | Check that the token is current, or create a new one. |
| `site login stopped: login_credentials_rejected` | `site login mteam` checked the key and the API did not accept it. Nothing was saved. | Copy the token again, or create a new one. |
| `refusing to use saved API key PATH because other users can access it; ...` | The saved key file is readable by group or other users. | Run `chmod 600 PATH`, or sign in again with `ptctl site login mteam`. |
| `site CMD failed: the site rate limited the request; ...` | HTTP 429 or API code `4`. | Wait before trying again. ptctl never retries. |
| `site CMD failed: the site host resolved only to private or reserved addresses, ...` | DNS returned only blocked addresses, typically from a proxy using fake-IP DNS (`198.18.0.0/15`, as in Clash or Mihomo fake-IP mode). | Exclude `api2.m-team.cc` from fake-IP (for example with `fake-ip-filter`) or use a resolver that returns real addresses. ptctl never uses a proxy. |
| `site CMD timed out: context deadline exceeded` | The 45-second deadline passed. | Check connectivity. |
| `site CMD failed` | Anything else: an unexpected HTTP status such as 403, a response that is not JSON, or an unknown API code. | If it persists with a valid key, open an issue with the command and time. Never include the key. |
| `site torrent detail read failed` | `site detail` did not observe the torrent. The report's `REQUEST STOP` names the reason, such as `not_found`, `authentication_required`, `rate_limited`, or `unrecognized_response`. | Check the ID and key; wait before trying again after `rate_limited`. |
| `site metafile fetch failed` | `site metafile fetch` stored nothing. The report's `REQUEST STOP` names the reason; see [Metafile download](#metafile-download). | Check the ID and key. After `rate_limited`, wait, and remember the per-torrent daily limit. Open an issue for a persistent `redirect_rejected`, never including the link. |

## Metafile download

`site metafile fetch` saves one torrent's exact `.torrent` into the private
store, as described in [metafiles.md](guide/metafiles.md). M-Team hands out the
file only through a signed link, so the route `mteam.download_by_token.v1` takes
two or three requests, in this order, and stops at the first one that fails:

1. `POST /api/torrent/genDlToken` on `api2.m-team.cc`, with the API key and a
   `multipart/form-data` form with the one field `id`. The answer carries a
   signed download link. It is accepted only as HTTPS on an M-Team host, with
   the path `/api/rss/dlv2`, signed for exactly the requested ID.
2. After a 3-second pause, one `GET` of that link. The API key is not sent: the
   signature in the link is the credential.
3. Only when that answer redirects to an HTTPS host under `m-team.cc` or
   `m-team.io` on the default port, one `GET` of the target. Any other target,
   including another site, a lookalike domain, an explicit port, plain HTTP,
   `magnet:`, or a second redirect, stops as `redirect_rejected` without being
   contacted.

The signed link and any redirect target are secrets. Each `GET` uses a fresh,
credential-free connection bound to that one host, with the same strict
transport as the reads otherwise. Neither URL is ever printed, logged, put in
the report, or stored; errors name the cause, not the URL. The last answer must
be the bencoded metafile itself, which is then checked and sealed exactly as for
TJUPT. The API's refusals arrive as HTTP 200 JSON and are mapped to the
report's `REQUEST STOP`:

| Answer | `REQUEST STOP` |
|---|---|
| The key or link was refused (HTTP 401 or 403, or API code `401`) | `authentication_required` |
| No such torrent (code `1` with the fixed not-found message and no data) | `not_found` |
| The per-torrent daily limit (the site allows 10 downloads of one torrent a day), a request burst, API code `4`, or HTTP 429 | `rate_limited` |
| A link or redirect outside the hosts above | `redirect_rejected` |
| Any other status, media type, or answer | `http_status_rejected`, `content_type_rejected`, `empty_response`, or `unrecognized_response` |

Each link request may count as a download on the site, even when no metafile
comes back. Keep that in mind on accounts with download conditions, such as
during the newbie-task period.

**Accounting.** The report and the sealed binding record keep the
single-request shape TJUPT uses and add the link requests next to it.
`max_requests` stays 1 and counts the metafile `GET`. `max_link_requests` is 2,
and `link_requests_attempted` counts the token request plus a redirect hop, so
`requests_made` is 2 or 3. `redirects_followed` stays 0: the redirect hop is a
separate counted request, not one the HTTP stack follows on its own. The
acknowledgement scope is `site_link_requests_then_one_remote_metafile_get`, the
evidence basis starts with `effectful_site_issued_link_get_for_remote_id`, and
the table output adds `LINK REQUEST LIMIT` and `LINK REQUESTS USED`. Direct
routes omit these fields, so TJUPT output and records are unchanged.

**Live check.** On 2026-10-16 the route was run only against IDs with no
torrent, so no file was downloaded. `genDlToken` signs a link even for an ID
that does not exist, so `not_found` can only be read from the link's answer:
ptctl reported `not_found` with `requests_made` 2, the token request and the
link `GET`. The link points at `api2.m-team.cc`, the API host itself. The
redirect hop for a real torrent has not been observed live yet; whatever host it
uses must pass the allowlist above, or the fetch stops as `redirect_rejected`
before contacting it. For how other tools use the same API, see
[MoviePilot #6950](https://github.com/jxxghp/MoviePilot/issues/6950),
[pt-tools #585](https://github.com/sunerpy/pt-tools/pull/585),
[Jackett #15744](https://github.com/Jackett/Jackett/issues/15744), and
[ptool](https://pkg.go.dev/github.com/sagan/ptool/site/mtorrent).

A `.torrent` downloaded from the website yourself can still be imported into the
private store instead. The file contains your passkey, so keep it private:

```bash
ptctl metafile store import --store "$PTCTL_STORE" release.torrent
```

## Reconciliation

`reconcile report --site-ref mteam/ID --site-live` adds the live detail read
to a reconciliation report with the saved API key, and reads nothing from
stdin. With a downloader in the same command, add `--password-stdin` and pipe
only the downloader password. A script that keeps the key itself pipes it with
`--site-api-key-stdin`, or, with a downloader, uses `--credential-bundle-stdin`
with `site_api_key` in place of `site_cookie`. The read is the one
`/api/torrent/detail` request above; it never asks for a signed download link.
See [Reconciliation](guide/reconcile.md).

## Not supported yet

Bonus exchange, RSS, seeding lists, and messages are not implemented.

## Fixtures

Tests in `internal/site/mteam` use only synthetic responses, the key
`MTEAM-KEY-CANARY-...`, a signed link with `sign=SIGN-CANARY`, and a synthetic
metafile announcing to `tracker.invalid`. Never commit a real API key, a
profile response, a signed download URL, or a real `.torrent`.
