# M-Team adapter

[Documentation index](README.md) · [Adding a site](ADDING_A_SITE.md)

M-Team (馒头PT, TP :: mTorrent) is the second experimental site and the first
that uses a site-issued API key instead of a session cookie. The adapter talks
only to the official JSON API at `https://api2.m-team.cc` and declares three
read capabilities:

- session check (`site status`);
- account snapshot (`site account`);
- torrent search (`site search`).

Torrent detail and metafile download are not implemented yet; see
[Not supported yet](#not-supported-yet). The three reads were smoke-tested live
on 2026-10-16 with a newly created API key: `site status` reported
`authenticated`, `site account` read the profile, and `site search` returned
results in both table and JSON output. A creation time the API returned for that
key matched UTC+8, confirming how [times](#results) are read. The unit tests use
synthetic responses shaped after the public API.

## Getting an API key

Sign in to the M-Team website and open 控制台 (Console) → 实验室 (Lab) →
建立存取令牌 (Create access token). Treat the token like a password: anyone
holding it can act as you through the API. Revoke it on the same page if it
leaks.

## Usage

The key is read only from stdin. Interactive terminal input is refused, the key
is never accepted as an argument, and it is never written to disk or printed:

```bash
printf '%s' "$MTEAM_API_KEY" | ptctl site status --api-key-stdin mteam
printf '%s' "$MTEAM_API_KEY" | ptctl site account --api-key-stdin mteam
printf '%s' "$MTEAM_API_KEY" | ptctl site search --api-key-stdin mteam "Ubuntu"
printf '%s' "$MTEAM_API_KEY" | ptctl site search --api-key-stdin --output json mteam "Ubuntu"
```

To keep the key out of shell history and your environment, pipe it from a
password manager instead, for example `op read op://Private/M-Team/api-key |
ptctl site account --api-key-stdin mteam`.

Without `--api-key-stdin`, `status`, `account`, and `search` fail with a usage
error before anything is read; `--cookie-stdin` is refused because the adapter
does not accept cookies.

## Requests

Each command sends exactly one request and never retries:

| Command | Request | Body |
|---|---|---|
| `site status`, `site account` | `POST /api/member/profile` | an empty `multipart/form-data` form |
| `site search` | `POST /api/torrent/search` | `{"mode":"normal","keyword":QUERY,"pageNumber":1,"pageSize":50}` |

The API exposes these reads as POST requests. Both endpoints only read; ptctl
sends no other endpoint. The request uses the same strict transport as every
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
ref `mteam/ID`, name, size, category ID, publish time, seeders, leechers,
completed count, and promotion. The table view adds `SEEDERS`, `LEECHERS`, and
`PROMOTION` columns. A response without a result list, or with any row that has
no canonical ID or name, fails the whole search instead of returning a partial
or empty page.

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
| `site CMD failed: the site rate limited the request; ...` | HTTP 429 or API code `4`. | Wait before trying again. ptctl never retries. |
| `site CMD failed: the site host resolved only to private or reserved addresses, ...` | DNS returned only blocked addresses, typically from a proxy using fake-IP DNS (`198.18.0.0/15`, as in Clash or Mihomo fake-IP mode). | Exclude `api2.m-team.cc` from fake-IP (for example with `fake-ip-filter`) or use a resolver that returns real addresses. ptctl never uses a proxy. |
| `site CMD timed out: context deadline exceeded` | The 45-second deadline passed. | Check connectivity. |
| `site CMD failed` | Anything else: an unexpected HTTP status such as 403, a response that is not JSON, or an unknown API code. | If it persists with a valid key, open an issue with the command and time. Never include the key. |

## Not supported yet

**Metafile download.** Downloading a `.torrent` from M-Team takes two
requests: `POST /api/torrent/genDlToken` returns a signed URL that carries a
personal credential, and fetching that URL can redirect to another host. The
site also limits how often one torrent can be downloaded per day. ptctl's
metafile fetch contract allows exactly one acknowledged request to one fixed
origin, with no redirects, and seals that single route in the persisted
`ptctl.site-metafile-binding/v1` record. Supporting M-Team needs a new,
versioned contract that acknowledges both requests by name, treats the signed
URL as a secret that is never printed or stored, pins the download host, and
accounts for the daily limit. That is a security review of its own, so it is
not part of this first adapter. For the same API, see how other tools handle
it: [MoviePilot #6950](https://github.com/jxxghp/MoviePilot/issues/6950),
[pt-tools #585](https://github.com/sunerpy/pt-tools/pull/585),
[Jackett #15744](https://github.com/Jackett/Jackett/issues/15744), and
[ptool](https://pkg.go.dev/github.com/sagan/ptool/site/mtorrent).

Until then, download the `.torrent` from the website yourself and import it
into the private store. The file contains your passkey, so keep it private:

```bash
ptctl metafile store import --store "$PTCTL_STORE" release.torrent
```

**Torrent detail.** `site detail` and live-site reconciliation seal a route
identity into their records. M-Team detail will be added together with the
download contract so both get one reviewed, versioned identity. The website's
detail page reads `/api/torrent/detail`, whose answer includes the info hash,
and also calls `/api/torrent/viewHits`, which by its name records a view; an
adapter should send only the first.

**Everything else.** Bonus exchange, RSS, seeding lists, and messages are not
implemented.

## Fixtures

Tests in `internal/site/mteam` use only synthetic responses and the key
`MTEAM-KEY-CANARY-...`. Never commit a real API key, a profile response, or a
signed download URL.
