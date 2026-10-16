# Automating ptctl

[Documentation index](README.md) · [Project README](../README.md)

This page is for scripts and AI agents that call `ptctl` as a tool. It covers
the machine-facing contract: JSON output, exit codes, secret input, and which
flags authorize an effect. The [command guide](guide/README.md) has the full
semantics of every command.

## Ground rules

- Pass `--output json` and parse stdout. Table output is for people.
- Never put a secret in argv; ptctl rejects secret arguments. Pipe it to the
  matching `--*-stdin` flag (see [Secrets](#secrets)).
- Never add an `--acknowledge-*` flag on your own initiative. Each one is a
  human decision about a tracker-visible, downloader, or filesystem effect.
- Never loop, parallelize, or retry site requests. Each site read is one bounded
  request, and repeated reads can get an account restricted.
- Keep the operation, plan, review, and record IDs that reports print. Recovery
  commands take them explicitly; ptctl never selects "the latest" operation.
- Absolute paths are hidden by default. `--show-absolute-paths` discloses
  private local paths; do not use it for output that leaves the machine.

## JSON envelope

Every JSON result is a single object on stdout, indented with two spaces:

```json
{
  "schema": "ptctl.dev/v1",
  "kind": "site.descriptor.list",
  "data": [],
  "warnings": []
}
```

- `schema` is `ptctl.dev/v1` for every command. The schema is experimental
  while ptctl is alpha.
- `kind` names the shape of `data`. Dispatch on `kind`, and treat an
  unrecognized kind as unsupported.
- `warnings` is omitted when empty.
- An error that stops a command before it has a report is not JSON. It is
  written to stderr as `error: <message>`; usage errors add the line
  `run 'ptctl help' for usage`.

### Kinds

`jsonKind` in `internal/cli/output.go` is the source of truth.

| Area | Kinds |
|---|---|
| General | `version` |
| Site reads | `site.descriptor.list` (`site list`, `site capabilities`), `site.session`, `site.account`, `site.torrent.list`, `site.torrent.detail`, `site.bonus.catalog`, `site.bonus.offer_review` |
| Site login | `site.login` |
| Bonus exchange | `site.bonus.exchange.prepare`, `site.bonus.exchange.submit`, `site.bonus.exchange.status`, `site.bonus.exchange.operation_list`, `site.bonus.exchange.prune`, `site.bonus.exchange.forget` |
| Site metafiles | `site.metafile.fetch`, `site.metafile.binding.list`, `site.metafile.binding.inspect` |
| Metafiles | `metafile.manifest` (`torrent inspect`), `content.verification` (`torrent verify`), `metafile.store.init`, `metafile.store.import`, `metafile.store.inspect` |
| Storage | `storage.probe`, `storage.path_mapping`, `storage.profile.create`, `storage.profile.inspect`, `storage.index.refresh`, `storage.index.inspect`, `storage.index.prune` |
| Discovery and plans | `content.source_discovery` (`seed discover`, `storage index refresh-discover`), `content.layout_plan` (`seed plan`) |
| Materialize | `content.materialization`, `content.materialization.operation_list`, `content.materialization.retention`, `content.materialization.forget` |
| Downloader reads | `downloader.status`, `downloader.torrent.list` |
| Client workflows | `client.adoption`, `client.adoption.retention`, `client.adoption.forget`, `client.activation`, `client.activation.retention`, `client.activation.forget`, `client.stop`, `client.stop.retention`, `client.stop.forget`, `client.removal`, `client.removal.retention`, `client.removal.forget` |
| Source retirement | `content.source_retirement_plan`, `content.source_retirement`, `content.source_retirement.operation_list`, `content.source_retirement.retention`, `content.source_retirement.forget` |
| Parent cleanup | `content.source_retirement.parent_cleanup_plan`, `content.source_retirement.parent_cleanup`, `content.source_retirement.parent_cleanup.retention`, `content.source_retirement.parent_cleanup.forget` |
| Reconciliation | `ledger.reconciliation` (`reconcile report`, `reconcile refresh-report`) |

## Exit codes

| Code | Meaning |
|---|---|
| `0` | The command succeeded, or a report-oriented command produced its report. |
| `1` | Operational failure: I/O, network, site or downloader error, interruption, or an outcome whose durability or removal is uncertain. |
| `2` | Invalid usage, including a missing acknowledgement flag, an unknown `SITE`, or a capability or credential kind the named site does not declare. |
| `3` | Integrity failure: a content, metafile, journal, record, or store mismatch or corruption. |
| `4` | Inconclusive or policy-blocked: the report does not meet an explicit requirement, the reviewed plan no longer matches, or a bounded inventory is incomplete. |

Most commands are report-first: they print their report on stdout and then
exit nonzero. Whenever stdout is non-empty, parse it before acting on the exit
code. Usage errors are detected before work starts and normally print no
report.

Some reports are deliberately non-failing by default. These opt-in flags turn
an unmet requirement into exit `4` after the report:

| Flag | Command | Exit `4` unless |
|---|---|---|
| `--require-verified` | `seed discover` | `source_outcome` is `verified_unique` or `verified_selected` |
| `--require-verified` | `storage index refresh-discover` | `source_outcome` is `verified_unique` |
| `--require-reconciled` | `reconcile report` | the independently reported local axes are `consistent` |
| `--require-eligible` | `seed retire plan` | the outcome is `eligible_for_separate_review` |
| `--require-cleanable` | `seed retire parent-cleanup plan` | at least one stable empty parent is reviewable |

Per-command details are in
[exit codes and report-first results](guide/exit-codes-and-reports.md).

## Secrets

`ptctl site login SITE` saves the site credential once: the TJUPT session
cookie it issues, or the M-Team API key after one status read accepts it.
Every later `site` command for that site, including `site metafile fetch` and
`site bonus exchange submit`, then uses the saved credential without a flag.
An agent saves one without a terminal by piping it,
`site login --cookie-stdin SITE` or `site login --api-key-stdin SITE`, and
`ptctl site logout SITE` deletes it. `reconcile report` uses the saved
credential when given `--site-live`, and then reads only the downloader
password from stdin. An explicit stdin flag below always takes
precedence over the saved credential, and a saved credential never stands in
for an acknowledgement flag.

| Flag | Used by | Stdin content |
|---|---|---|
| `--cookie-stdin` | `site` reads, `site bonus exchange submit` for cookie-only sites, `site metafile fetch` for cookie sites (TJUPT) | The complete `Cookie` header value |
| `--api-key-stdin` | `site status`, `site account`, `site search`, `site detail`, and `site metafile fetch` for sites that authenticate with an API key (M-Team) | The site-issued API key |
| `--password-stdin` | `client` commands, `seed retire` commands that contact a downloader, and `reconcile` with a downloader | The downloader password |
| `--site-cookie-stdin` | `reconcile report` with a live site observation and no downloader, on a cookie site (TJUPT) | The complete `Cookie` header value |
| `--site-api-key-stdin` | `reconcile report` with a live site observation and no downloader, on an API-key site (M-Team) | The site-issued API key |
| `--credential-bundle-stdin` | `reconcile report` with both a live site observation and a downloader | `{"schema":"ptctl.credentials/v1","site_cookie":"...","downloader_password":"..."}`, or `site_api_key` in place of `site_cookie` |
| `--credential-bundle-stdin` | `site bonus exchange submit` for sites that confirm with the account password (TJUPT) | `{"schema":"ptctl.credentials/v1","site_cookie":"...","site_password":"..."}` |
| `--credential-bundle-stdin` | `site login` | `{"schema":"ptctl.credentials/v1","site_username":"...","site_password":"...","site_totp":"123456"}`; `site_totp` is optional |
| `--cookie-stdin`, `--api-key-stdin` | `site login` | The credential to save; one status read checks it first and nothing is saved when it fails |

- An interactive terminal on stdin is refused; the secret must be piped. The
  exception is `site login` without a stdin flag, which is for people: it
  prompts on the terminal with the password, code, or API key hidden, and
  exits `2` when stdin is not a terminal. Agents pass the bundle,
  `--cookie-stdin`, or `--api-key-stdin`. `site bonus exchange submit` with
  the saved cookie on a site that confirms with the account password (TJUPT)
  asks for that password the same way, before its timeout starts; agents pass
  the bundle instead.
- Single secrets are limited to 64 KiB and the bundle to 128 KiB. The bundle is
  strict JSON: unknown, duplicate, or missing fields are rejected.
- Site reads, metafile fetch, and reconciliation validate usage, capabilities,
  and local inputs before reading stdin.
- Credentials stay in memory. They are never printed or forwarded in error
  text, and the only one persisted is the site credential `site login` saves.
  Without the bundle, or with `--cookie-stdin` or `--api-key-stdin`, it goes
  to `ptctl/sessions/SITE.cookie` or `SITE.api-key` under the user config
  directory, a plaintext file readable only by its owner. With
  `--cookie-file PATH` the session cookie is saved to that owner-only file
  instead, and `--output json` then reports a `site.login` object with
  `cookie_file` and `session_expires_at` but not the cookie; an API key save
  reports `api_key_file`. With the bundle and no `--cookie-file`, the cookie
  is written to stdout, which must be a pipe or an owner-only file, and is not
  saved. Account and downloader passwords are never saved.
  `ptctl help login` documents every flag.
- A saved credential is refused, without being shown, when it is not a
  regular file, when other users can read it (outside Windows), or when it is
  not exactly one valid credential line. Sign in again or run
  `ptctl site logout SITE` to clear it.
- `ptctl site capabilities SITE` lists the authentication methods a site
  accepts: `cookie_header` takes `--cookie-stdin`, `api_key_header` takes
  `--api-key-stdin`. With neither a flag nor a saved credential the command
  exits `2` and names both the `site login` command and the flag.
- A failed ordinary site read prints `site CMD failed`. Adapters that opt in
  (currently M-Team) append one fixed cause after a colon: the credential has
  the wrong format, the site rejected the credential, the site rate limited
  the request, or the host resolved only to blocked addresses. Match on these
  fixed strings; server-supplied text is never shown.

## Effects and acknowledgements

Every write and every tracker-visible or downloader effect requires its own
flag, except `site login`, whose only effect is the sign-in the command names. Each effectful run also takes the ID printed by its zero-write review
(`--expect-plan-id`, `--expect-adoption-plan-id`,
`--expect-activation-plan-id`, `--expect-stop-plan-id`,
`--expect-removal-plan-id`, `--expect-cleanup-plan-id`, or
`--expect-review-id`). The writing invocation recomputes the review and refuses
to proceed if it no longer matches.

| Flag | Command | Authorizes |
|---|---|---|
| `--acknowledge-site-effect` | `site metafile fetch` | One tracker-visible GET of a private metafile into the private store, preceded on a linked route (M-Team) by at most two counted requests for a site-issued link |
| `--acknowledge-bonus-exchange` | `site bonus exchange submit` | At most one bonus-exchange POST after a durable attempt marker |
| `--acknowledge-state-prune` | `site bonus exchange prune` | Replacing one terminal exchange's records with a tombstone |
| `--acknowledge-history-forget` | `site bonus exchange forget` | Irreversibly deleting that last historical evidence |
| `--acknowledge-filesystem-write` | `seed materialize run`, `resume` | Journal, staging, and no-clobber publication under the target root |
| `--acknowledge-abandon` | `seed materialize abandon` | One terminal journal event; staged bytes are kept |
| `--acknowledge-client-add` | `client adopt run`, `resume` | One stopped add of the exact job |
| `--acknowledge-client-re-adoption`, `--acknowledge-client-re-add-after-removal` | `client adopt run`, `resume` | One new stopped add after the explicit prior terminal job disappeared, or after its attributed keep-data removal |
| `--acknowledge-existing-stopped-adoption` | `client adopt run`, `resume` with `--adopt-existing-stopped` | An observation-only adoption record with no downloader mutation |
| `--acknowledge-client-recheck`, `--acknowledge-client-start` | `client activate run`, `resume` | Recheck, then optional start, of the exact adopted job |
| `--acknowledge-client-stop` | `client stop run`, `resume` | One stop request for the exact job |
| `--acknowledge-client-removal` | `client remove run`, `resume` | One removal request that keeps local data |
| `--acknowledge-repeat-add`, `-recheck`, `-start`, `-stop`, `-removal` | the matching `resume` | Sending a request again after an earlier attempt's outcome is unknown |
| `--acknowledge-source-deletion` | `seed retire run`, `resume` | Unlinking the reviewed exact source names |
| `--acknowledge-empty-parent-removal` | `seed retire parent-cleanup run`, `resume` | Removing reviewed immediate parents that are still empty |
| `--acknowledge-operation-state-deletion` | every other `prune` | Deleting one terminal operation's private state; a tombstone remains |
| `--acknowledge-historical-evidence-deletion` | every other `forget` | Irreversibly deleting that tombstone and its last recovery marker |

Commands without an acknowledgement flag either write nothing or write only to
an explicitly named private store; the
[write-boundary table](../README.md#write-boundaries) lists both groups.

## A safe loop for agents

1. Run the zero-write review (`plan`, `discover`, `review`, `inspect`, or
   `report`) with `--output json`.
2. Show a person the report and the exact effectful command you propose,
   including its acknowledgement flag and expected review ID.
3. Run that command once, only after explicit approval.
4. On a nonzero exit, read the report. Do not retry; use `status` with the
   printed operation ID, and `resume` only with an ID taken from a report.
