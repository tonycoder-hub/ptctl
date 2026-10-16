# Site reads and bonus exchange

[Command guide](README.md) · [Documentation index](../README.md)

> Fetching an exact private metafile from a site (`site metafile fetch`) and auditing its sealed bindings are described with the private store in [metafiles.md](metafiles.md). The page-level TJUPT contract is in [TJUPT_ADAPTER.md](../TJUPT_ADAPTER.md), and M-Team is covered in [MTEAM_ADAPTER.md](../MTEAM_ADAPTER.md).

Read TJUPT without putting a cookie in shell history. Stdin must contain the
complete `Cookie` header value from a session you control. Interactive TTY
secret input is refused. Do not paste the value into issues, logs, or chat.
For ordinary `status`, `account`, `search`, and `bonus-catalog` reads, output
mode, site/capability/authentication support, the declared typed port, and a
nonblank search query where applicable are all validated before stdin is read.
Runtime adapter failures are reduced to fixed public diagnostics; cancellation
and deadlines remain distinguishable, but cookie or response text is never
forwarded.

```bash
printf '%s' "$TJUPT_COOKIE" | ptctl site status --cookie-stdin tjupt
printf '%s' "$TJUPT_COOKIE" | ptctl site account --cookie-stdin tjupt
printf '%s' "$TJUPT_COOKIE" | ptctl site search --cookie-stdin tjupt "Ubuntu"
printf '%s' "$TJUPT_COOKIE" | ptctl site detail --cookie-stdin tjupt REMOTE_ID
printf '%s' "$TJUPT_COOKIE" | ptctl site bonus-catalog --cookie-stdin tjupt
printf '%s' "$TJUPT_COOKIE" | ptctl site bonus review --cookie-stdin tjupt OPTION
```

Each read-only TJUPT command above performs at most one bounded GET and never
retries. Do not loop or parallelize site reads. `site detail` also refuses redirects and sends
only `details.php?id=REMOTE_ID`: it does not send `hit=1`, follow the download
link, fetch the metafile, or persist an observation. Its display title, optional
peer counts, size, and snatch count, and its matching internal link are current
site claims, not metafile identity or storage-content proof.

TJUPT search results also carry the category ID, publish time (in UTC),
seeders, leechers, snatches, and promotion when the results table has its
usual shape; otherwise a result keeps only its name and size.

Instead of copying the cookie out of a browser, `site login` can sign in with
the account password and save a new session cookie for the commands above.
Run it in a terminal and answer the prompts:

```console
$ ptctl site login tjupt
Signing in to TJUPT / 北洋园PT with one login request; failed logins count toward the site's lockout.
Username: alice
Password (hidden):
Two-step code (hidden; press Enter if none):
site login succeeded: site=tjupt username=alice session_expires=2026-10-23T08:00:00Z requests=2
session cookie saved to /home/alice/.config/ptctl/sessions/tjupt.cookie (owner-only)
use it with: ptctl site status --cookie-stdin tjupt < '/home/alice/.config/ptctl/sessions/tjupt.cookie'
```

The password and the two-step code are not echoed. Press Enter at the code
prompt when the account has no two-step verification; spaces inside a code are
ignored. Ctrl+C at a prompt quits without sending anything. The cookie is saved
to `ptctl/sessions/SITE.cookie` under the user config directory (`~/.config` on
Linux, `~/Library/Application Support` on macOS, `%AppData%` on Windows),
readable only by you, and replaced atomically by the next login. No command
reads it automatically; pass it on stdin as the last line shows.
`--cookie-file PATH` saves it somewhere else, and `--session-days` picks the
session length the site offers (TJUPT: 1, 7, 14, 30, or 90; default 7).
`ptctl help login` describes every flag.

Scripts and agents pass the credentials instead with `--credential-bundle-stdin`,
as one strict `ptctl.credentials/v1` bundle on stdin: `site_username`,
`site_password`, and, for an account with two-step verification, the current
one-time code in `site_totp`. A terminal on stdin is then refused. With
`--cookie-file` the cookie goes to that file and `--output json` reports a
`site.login` object, without the cookie, on stdout. Without `--cookie-file` the
cookie line and nothing else goes to stdout, which must be a pipe or an
owner-only file, and `--output json` is refused. One way to build the bundle
without putting a secret in argv, here with the 1Password CLI supplying the
code:

```bash
jq -n --arg user "$TJUPT_USER" \
  --rawfile password "$PASSWORD_FILE" \
  --rawfile totp <(op item get "$TJUPT_ITEM" --otp) \
  '{schema: "ptctl.credentials/v1",
    site_username: $user,
    site_password: ($password | rtrimstr("\n")),
    site_totp: ($totp | rtrimstr("\n"))}' | \
  ptctl site login --credential-bundle-stdin --cookie-file "$COOKIE_FILE" --output json tjupt

ptctl site status --cookie-stdin tjupt < "$COOKIE_FILE"
```

```json
{
  "schema": "ptctl.dev/v1",
  "kind": "site.login",
  "data": {
    "site_id": "tjupt",
    "username": "alice",
    "session_days": 7,
    "session_expires_at": "2026-10-23T08:00:00Z",
    "cookie_file": "/home/alice/tjupt.cookie",
    "requests_attempted": 2
  }
}
```

Leave out `site_totp` when the account has no two-step verification. In text
mode, stderr reports `site login succeeded: site=tjupt username=...
session_expires=... requests=2` and, with a cookie file, where it was saved.

Login sends one POST and never retries. If the site issues a session cookie,
ptctl waits two seconds and makes one read with it, and keeps the cookie only
if that read shows the same signed-in account. Otherwise it exits `1` with
`site login stopped: REASON`:

| Reason | Meaning |
|---|---|
| `login_credentials_rejected` | The site refused the username or password. |
| `login_two_factor_required` | The account needs a one-time code and none was given. |
| `login_two_factor_rejected` | The one-time code was wrong or expired. |
| `login_response_unrecognized` | The site answered with a page ptctl does not recognize. |
| `login_session_unverified` | A cookie was issued, but the verifying read did not show the same signed-in account. The cookie is discarded. |
| `login_request_failed` | The request failed or its response was rejected before classification. The site may still have counted the attempt. |

TJUPT blocks the client IP after ten failed logins. When the page states how
many attempts are left, the error line ends with `(the site reports N login
attempts left before it blocks this IP)`. Fix the input before trying again,
and never loop `site login`. Usage errors exit `2` before any prompt or stdin
read. A failed login leaves an earlier cookie file untouched.

M-Team reads use a site-issued API key instead of a cookie, passed with
`--api-key-stdin` under the same stdin-only rules. `status`, `account`,
`search`, and `detail` each send one read-only POST to M-Team's JSON API and
never retry; search results add seeders, leechers, promotion, and category-name
columns. Metafile fetch and bonus commands are not available for M-Team yet,
and `reconcile` reads live site detail with a cookie only. Setup, results, and
error lines are in [MTEAM_ADAPTER.md](../MTEAM_ADAPTER.md):

```bash
printf '%s' "$MTEAM_API_KEY" | ptctl site status --api-key-stdin mteam
printf '%s' "$MTEAM_API_KEY" | ptctl site account --api-key-stdin mteam
printf '%s' "$MTEAM_API_KEY" | ptctl site search --api-key-stdin mteam "Ubuntu"
printf '%s' "$MTEAM_API_KEY" | ptctl site detail --api-key-stdin mteam REMOTE_ID
```

`site account` reads the authenticated `mybonus.php` page and reports the
username and canonical bonus balance, plus what the page's user panel states:
uploaded total, user class, seeding points, invites, and seeding/leeching
counts. A panel field that is missing or stated inconsistently stays absent.
TJUPT shows no downloaded total or ratio, so those are never reported. The
snapshot is a point-in-time site claim, not a signed ledger or proof of a
bonus exchange.

`bonus-catalog` lists one row per exchange form on `mybonus.php`: the form's
hidden `option` as its selector, the offer name, price, submit label, and a
status (`available`, `available, requires input`, `disabled`, or `unknown`).
The displayed row number is dropped because TJUPT numbers rows differently
from options. `site bonus review` selects that canonical decimal option from a
fresh live `mybonus.php` page. It reports retained site-defined text columns, balance,
current submit availability, whether extra user input is required, the
allowlisted action route, and one-way form-shape/review IDs. Hidden field values,
raw HTML, and form URLs are not emitted. The action route is a static HTML
claim; the command does not execute JavaScript or browser rendering/runtime
behavior. It performs zero writes and zero form submissions. Its JSON is
deliberately non-authoritative; the separate exchange workflow therefore
fetches a fresh page, reproduces the reviewed semantics, and crosses an
explicit acknowledgement and recovery boundary.

TJUPT confirms each exchange with the account password, so its `submit` takes
the cookie and password together as one strict JSON bundle on stdin
(`--credential-bundle-stdin`) and refuses `--cookie-stdin`; see the TJUPT
example below. A site that needs only the cookie takes `--cookie-stdin`.

To cross that boundary, first initialize a private state store (the same
versioned owner-only store format used by metafile artifacts), review the exact
option, and prepare a local intent from the review ID:

```bash
ptctl metafile store init --store "$PTCTL_STATE"

printf '%s' "$SITE_COOKIE" | \
  ptctl site bonus review --cookie-stdin --output json SITE OPTION

ptctl site bonus exchange prepare \
  --state-store "$PTCTL_STATE" \
  --expect-review-id REVIEW_ID \
  SITE OPTION
```

`prepare` performs no network request and prints an explicit intent record ID.
Submitting requires that exact record, the same site/option/review selectors,
and a separate acknowledgement:

```bash
printf '%s' "$SITE_COOKIE" | \
  ptctl site bonus exchange submit \
    --state-store "$PTCTL_STATE" \
    --intent-record INTENT_RECORD_ID \
    --expect-review-id REVIEW_ID \
    --cookie-stdin \
    --acknowledge-bonus-exchange \
    SITE OPTION

ptctl site bonus exchange status \
  --state-store "$PTCTL_STATE" \
  --intent-record INTENT_RECORD_ID

ptctl site bonus exchange list \
  --state-store "$PTCTL_STATE"

ptctl site bonus exchange prune \
  --state-store "$PTCTL_STATE" \
  --intent-record INTENT_RECORD_ID \
  --operation-id OPERATION_ID \
  --acknowledge-state-prune

ptctl site bonus exchange forget \
  --state-store "$PTCTL_STATE" \
  --retention-record RETENTION_RECORD_ID \
  --operation-id OPERATION_ID \
  --acknowledge-history-forget
```

For TJUPT, replace the `submit` stdin and flag with the bundle. One way to
build it without putting either secret in argv, assuming each is kept in its
own owner-only file:

```bash
jq -n --rawfile cookie "$COOKIE_FILE" --rawfile password "$PASSWORD_FILE" \
  '{schema: "ptctl.credentials/v1",
    site_cookie: ($cookie | rtrimstr("\n")),
    site_password: ($password | rtrimstr("\n"))}' | \
  ptctl site bonus exchange submit \
    --state-store "$PTCTL_STATE" \
    --intent-record INTENT_RECORD_ID \
    --expect-review-id REVIEW_ID \
    --credential-bundle-stdin \
    --acknowledge-bonus-exchange \
    tjupt OPTION
```

The password is sent only inside that one POST and is never stored or printed.
A wrong password is `rejected` with `server_password_rejected` and spends
nothing; like every terminal outcome, that operation is not retried, so prepare
a new intent after fixing the password. See
[the TJUPT adapter](../TJUPT_ADAPTER.md#bonus-exchange) for the result pages
it recognizes.

`list` is a credential-free recovery aid when an earlier command's output is
no longer available. It performs detected-stable bounded inventories of live
intent records and historical retention/forget markers, hash-verifies and
strictly parses every retained record, returns deterministic
original-intent-ID order, and
marks each row `not_inspected`, `pruning_not_inspected`,
`pruned_not_inspected`, or `forgetting_not_inspected`. It never chooses a
newest operation or infers the full attempt/outcome transition; pass one
selected intent record to `status` for that higher-evidence inspection.

Submit performs one fresh bounded GET and verifies the process-local review
authority before writing the deterministic attempt marker. Within the
adapter's supported static-HTML subset, it also runs the exact ordered form
encoder against the intent's field and encoded-byte budgets;
those same limits are bound to the live authority consumed by the POST. Only
the invocation that newly and durably creates the marker may send that exact
freshly observed form, once. The POST uses HTTP/1.1 with no connection reuse,
redirect following, or retry. An allowlisted site-local redirect or an
allowlisted result page can confirm the site response; other complete
responses are rejected or unknown. If the
POST may have started but its outcome cannot be durably recorded, `status` remains
`attempt_reserved_submission_unknown`. Re-running `submit` is refused before
stdin is read. Hidden form values and the cookie remain in memory only; sealed
records contain safe identities, counters, time bounds, and fixed outcome
codes, not raw HTML, fields, URLs, or credentials.

Form-shape and semantic review IDs are versioned correlators for the audited
parser semantics, not permanent site identifiers. A parser hardening that
changes which controls affect submission can deliberately change these IDs;
that requires a fresh review and a newly prepared intent rather than silently
reusing older authority.

The at-most-once marker coordinates one prepared operation within one
preserved private-store history. A separately prepared operation is a separate
explicitly acknowledged submission. Do not clone, roll back, or manually
delete records and then reuse an intent: no local file protocol can coordinate
independent restored copies. The only supported deletion boundaries are the
explicit terminal `prune` and `forget` protocols below. Read-only `status`
re-verifies the
records currently visible in the selected store through a bounded,
detected-stable but non-atomic scan. It does not
reconstruct the historical no-clobber or directory-sync receipt. Consequently,
its `*_record_verified` fields may be true while `outcome_durable` remains
false; that durability flag is asserted only by the invocation that completed
the publication boundary.

Reports keep two additional assurance axes separate:
`attempt_marker_blocks_future_submissions` means the selected preserved history
contains the verified operation marker that prevents a later submit invocation;
`submission_request_bound_verified` means a valid live request receipt, or a
jointly verified outcome record containing that receipt, was bound to the
operation. Neither field claims global exactly-once coordination across cloned
or rolled-back stores.

`prune` is local-only and never reads a cookie or contacts TJUPT. It requires
the exact intent-record and operation IDs plus its own acknowledgement. Only a
complete `confirmed`, `rejected`, explicit `unknown`, or `not_submitted` outcome
is eligible. `prepared` and `attempt_reserved_submission_unknown` are blocked
with zero deletion because their records remain the authority that prevents an
unsafe repeat POST. Before removing anything, prune seals and re-verifies one
deterministic retention record containing the exact canonical terminal chain
and store identity. It then removes intent, attempt, and outcome, in that
order, by their domain-separated record digests (the intent goes first so an
interrupted prune never leaves an intent without its at-most-once attempt
marker), with a deterministic owner-private residue
and directory durability checks making each crash boundary recoverable by
repeating the same selectors. A completed or recovered prune returns the
non-executable tombstone ID; it never recreates submission authority.

`forget` is a second, narrower and irreversible local boundary. It requires the
exact retention-record and operation IDs plus a distinct acknowledgement, and
first proves that all executable terminal records are already absent. It then
publishes an exact deterministic forget marker, removes the tombstone, and
removes the forget marker last. Repeating the same selector recovers an
interrupted transition while that marker remains. Once the final marker is
durably absent, a later invocation deliberately reports
`unattributed_absence`: it cannot distinguish a prior successful forget from a
record ID that never existed, and it never claims `already_forgotten`.

These commands are report-first after valid usage. Complete
prepare/status/list, a durably recorded confirmed submission,
`pruned`/`already_pruned`, and a newly confirmed `forgotten` exit `0`.
Rejected, unknown, not-submitted, operationally incomplete submissions,
retention interruption/uncertain durability, and unattributed post-forget
absence exit `1` after printing their state. Usage or a missing acknowledgement
is `2`, and verified sealed-state corruption is integrity exit `3`. A bounded
but incomplete inventory or retention policy blocker returns `4` after its
report; partial evidence is never silently promoted.

---

Previous: [Verify, discover, plan, and materialize](verify-and-materialize.md) · Next: [Downloader reads, path mapping, and adoption](downloader.md)
