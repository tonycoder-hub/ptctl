# Client activation, stop, and removal

[Command guide](README.md) · [Documentation index](../README.md)

> These commands start from a completed adoption; see [downloader.md](downloader.md).

Recheck that adopted job, then optionally start it only after a durable
completion observation:

The example uses qBittorrent. For a Transmission v1 adoption, use
`--driver transmission`, the full RPC URL, and the Transmission credential;
the plan binds that driver and cannot be replayed against the other adapter.

```bash
# Review. Add --start-after-recheck to include the optional start transition
# in the plan ID.
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client activate plan \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --adoption-operation sha256:ADOPTION_OPERATION_DIGEST \
  --adoption-plan-id ADOPTION_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin --start-after-recheck --output json

# Run authorizes only one recheck request. A reviewed start is always a
# separate later resume so one invocation never sends two effectful POSTs.
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client activate run \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --adoption-operation sha256:ADOPTION_OPERATION_DIGEST \
  --adoption-plan-id ADOPTION_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin --start-after-recheck \
  --expect-activation-plan-id ACTIVATION_PLAN_ID \
  --acknowledge-client-recheck --output json

# After qBittorrent has been observed complete and stopped, start is still a
# separate explicit acknowledgement because it may announce or transfer data.
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client activate resume \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --adoption-operation sha256:ADOPTION_OPERATION_DIGEST \
  --adoption-plan-id ADOPTION_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin --start-after-recheck \
  --expect-activation-plan-id ACTIVATION_PLAN_ID \
  --acknowledge-client-start sha256:ACTIVATION_OPERATION_DIGEST

ptctl client activate status --target "D:\PT" \
  sha256:ACTIVATION_OPERATION_DIGEST

# Once activation is terminal, retain its exact historical completion while
# deleting only this operation's original recheck/start marker chain.
ptctl client activate prune \
  --target "D:\PT" \
  --expect-activation-plan-id ACTIVATION_PLAN_ID \
  --acknowledge-operation-state-deletion \
  --output json \
  sha256:ACTIVATION_OPERATION_DIGEST

# After downstream retention is no longer needed, irreversibly erase this one
# exact activation tombstone and its final historical attribution.
ptctl client activate forget \
  --target "D:\PT" \
  --expect-activation-plan-id ACTIVATION_PLAN_ID \
  --acknowledge-historical-evidence-deletion \
  --output json \
  sha256:ACTIVATION_OPERATION_DIGEST
```

Activation never treats a successful POST as a completed recheck. It records
the request intent first, then requires either a durable observation of a
checking state followed by a complete stopped observation, or an incomplete to
complete transition bracketed by the same invocation's request. A fast recheck
that starts and finishes between observations can therefore remain
`recheck_request_result_unknown`; repeating it requires both
`--acknowledge-client-recheck` and `--acknowledge-repeat-recheck`. Start has the
same non-replay rule and its own repeat acknowledgement. qBittorrent 4.x uses
the reviewed `resume` route while 5.x uses `start`; an unknown major is
unsupported rather than guessed. Transmission RPC 5.3 binds the legacy
`torrent-verify`/`torrent-start` methods, while RPC 6 binds
`torrent_verify`/`torrent_start`; both use a one-element full SHA-1 hash-string
selector and never replay an expired CSRF request. See the official
[current Transmission action specification](https://github.com/transmission/transmission/blob/main/docs/rpc-spec.md#31-torrent-action-requests)
and [Transmission 4.0.6 action specification](https://github.com/transmission/transmission/blob/4.0.6/docs/rpc-spec.md#31-torrent-action-requests).
Completion remains a bracketed client claim
plus a same-invocation exact final proof, not proof of a raw private variant or
an atomic client/filesystem snapshot. Each invocation sends at most one
effectful client POST. JSON kind is `client.activation`.

After an attributed terminal stop, the same command family can review a new
start-only activation without rechecking again. Supply the complete
`--stop-operation` / `--stop-plan-id` pair instead of the adoption pair, omit
`--start-after-recheck`, and acknowledge the start directly on `run`:

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client activate plan \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --stop-operation sha256:STOP_OPERATION_DIGEST \
  --stop-plan-id STOP_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin --output json

printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client activate run \
  [the same selectors] \
  --expect-activation-plan-id RESTART_ACTIVATION_PLAN_ID \
  --acknowledge-client-start --output json
```

This path accepts only a live terminal stop journal or complete retained stop
tombstone whose completion basis is
`accepted_response_then_exact_stopped`. It locally reloads and binds that
stop's exact prior activation before password stdin, then freshly observes the
same complete stopped job, layout, mapping, and final. A causality-unproven
stop cannot authorize it. One invocation sends at most one start request; an
unknown result is observed first on resume and requires both the base and
repeat start acknowledgements before another request. Its terminal
`start_after_stop` activation is ordinary downstream authority: reconciliation,
another stop, retention pruning, current-use checks, and later source
retirement consume it without treating serialized JSON as proof.

Stop one reviewed complete started exact job without removing it or changing
its location:

```bash
# Read-only review. This proves the current exact job is complete and started.
ptctl client stop plan \
  --metafile-store "D:\\ptctl-store" \
  --metafile-variant sha256:METAFILE_DIGEST \
  --target "D:\\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --activation-operation sha256:ACTIVATION_OPERATION_DIGEST \
  --activation-plan-id ACTIVATION_PLAN_ID \
  --host-root "D:\\PT" --client-root "/downloads" --client-style posix \
  --driver qbittorrent --url "https://127.0.0.1:8080" \
  --username USER --password-stdin --output json

# Journal the exact reviewed intent, then issue at most one native stop request.
ptctl client stop run \
  [the same selectors] \
  --expect-stop-plan-id STOP_PLAN_ID \
  --acknowledge-client-stop --output json

# Recovery observes first. If a previous request may have crossed the boundary
# and the job is still started, repeating it additionally requires both flags.
ptctl client stop resume \
  [the same selectors] \
  --expect-stop-plan-id STOP_PLAN_ID \
  --acknowledge-client-stop --acknowledge-repeat-stop \
  --output json sha256:STOP_OPERATION_DIGEST

# Historical, credential-free inspection; it makes no downloader request.
ptctl client stop status --target "D:\\PT" \
  --expect-stop-plan-id STOP_PLAN_ID --output json \
  sha256:STOP_OPERATION_DIGEST

# Compact only this terminal stop operation's private request journal. No
# password or downloader connection is used.
ptctl client stop prune --target "D:\\PT" \
  --expect-stop-plan-id STOP_PLAN_ID \
  --acknowledge-operation-state-deletion \
  --output json sha256:STOP_OPERATION_DIGEST

# Irreversibly erase only that exact compact historical tombstone.
ptctl client stop forget --target "D:\\PT" \
  --expect-stop-plan-id STOP_PLAN_ID \
  --acknowledge-historical-evidence-deletion \
  --output json sha256:STOP_OPERATION_DIGEST
```

The stop plan binds the terminal activation lineage, typed identity, opaque
job/file-layout references, complete snapshot, current started state, path
mapping, exact materialized final, built-in protocol, and one exact native
route (`pause` for qBittorrent 4, `stop` for qBittorrent 5, or
`torrent-stop`/`torrent_stop` for Transmission RPC 5/6). The opaque downloader
job locator never enters JSON. HTTP acceptance is only request evidence:
completion also requires the same authenticated session to observe the exact
job in a complete stopped state with the same layout and a fresh exact final
proof. Unknown transport results remain resumable but are never repeated
automatically. JSON kind is `client.stop`.

Stop pruning is a distinct local deletion authority. It requires one full stop
operation ID, the reviewed stop plan ID, and
`--acknowledge-operation-state-deletion`. Before deleting anything, it seals
the exact terminal intent, bounded attempt chain, sparse response chain,
completion, and all domain-separated marker IDs into a canonical owner-private
retention intent. Only after that intent is durably published and rebound does
it remove the original request markers and empty scratch directory; an exact
retention completion closes the tombstone. Crashes at either boundary advance
only through the same explicit selector. Ordinary run/resume cannot cross the
retention boundary, and status labels the retained proof historical. JSON kind
is `client.stop.retention`.

Stop `forget` is a third, narrower irreversible boundary with its own
`--acknowledge-historical-evidence-deletion`. It publishes a deterministic
owner-private root recovery intent before touching the exact tombstone, then
removes only those retained markers and the exact operation subtree, confirms
durable absence, and removes the recovery intent last. Partial transitions are
recoverable only through the same operation and plan IDs. Neither command
opens a downloader session, reads a password, touches the job or content, or
selects latest/by-age state. Once the last marker is gone, later absence is
reported as `absent_unattributed`, never as proof of an earlier forget. JSON
kind is `client.stop.forget`.

Activation `prune` is a distinct local-only deletion boundary. It takes one
full activation operation ID, the reviewed activation plan ID, and
`--acknowledge-operation-state-deletion`; it accepts no downloader credential
and performs no network request. Before deleting anything, it seals the exact
canonical activation intent, terminal completion, and a bounded manifest of
every original marker's name, domain-separated ID, and size into an
owner-private retention intent. It then removes only those exact marker files
and the empty scratch directory, audits the remaining namespace, and publishes
a retention completion. An intent-only crash state blocks ordinary resume and
is advanced only by repeating the same explicit prune selector. A complete
tombstone can recreate process-local `clientactivate.VerifiedCompletion`
authority for source-retirement review through a fresh bound read; copied JSON
cannot. It is historical evidence, not proof of current downloader state.
JSON kind is `client.activation.retention`; `pruned` and `already_pruned`
return `0`.

Activation `forget` is a third, narrower irreversible boundary. It accepts
only the same full operation ID and reviewed activation plan ID plus
`--acknowledge-historical-evidence-deletion`; it reads no credential and makes
no network request. Before touching the tombstone it copies the exact retention
intent and completion into a deterministic owner-private root recovery intent.
It then removes only the two retained markers, their empty directory, and the
exact operation subtree, rechecks durable absence, and finally removes the last
root intent. A crash after staging, root publication, or operation removal is
recoverable only by repeating the same explicit forget selector. While either
intent is visible, status reports `forgetting` and ordinary resume, completion
proof, and prune stop before client access. After confirmed last-marker removal
there is intentionally no idempotence evidence: a repeated call reports
`absent_unattributed`, never `already_forgotten`. JSON kind is
`client.activation.forget`. Deleting durable evidence cannot revoke an opaque
`VerifiedCompletion` capability that was already issued in another live call.

Remove that exact current downloader job while deliberately retaining the
materialized files:

```bash
# Review only. The plan binds the current activation, exact typed job and file
# layout, client mapping/configuration, materialized final, and protocol route.
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client remove plan \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --activation-operation sha256:ACTIVATION_OPERATION_DIGEST \
  --activation-plan-id ACTIVATION_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin --output json

# Execute exactly the reviewed keep-data removal.
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client remove run \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --activation-operation sha256:ACTIVATION_OPERATION_DIGEST \
  --activation-plan-id ACTIVATION_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin \
  --expect-removal-plan-id REMOVAL_PLAN_ID \
  --acknowledge-client-removal --output json

ptctl client remove status --target "D:\PT" \
  --expect-removal-plan-id REMOVAL_PLAN_ID \
  sha256:REMOVAL_OPERATION_DIGEST

# Compact only this terminal operation's private request journal. No password
# or downloader connection is used.
ptctl client remove prune --target "D:\PT" \
  --expect-removal-plan-id REMOVAL_PLAN_ID \
  --acknowledge-operation-state-deletion \
  sha256:REMOVAL_OPERATION_DIGEST

# Irreversibly erase only that compact historical tombstone.
ptctl client remove forget --target "D:\PT" \
  --expect-removal-plan-id REMOVAL_PLAN_ID \
  --acknowledge-historical-evidence-deletion \
  sha256:REMOVAL_OPERATION_DIGEST
```

The removal port has no delete-data option. qBittorrent receives exactly one
`hashes` value with `deleteFiles=false`; Transmission receives exactly one full
v1 hash with `delete-local-data=false` (RPC 5.3) or
`delete_local_data=false` (RPC 6). See the official
[qBittorrent WebUI API](https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-%28qBittorrent-5.0%29#delete-torrents)
and [Transmission RPC specification](https://github.com/transmission/transmission/blob/main/docs/rpc-spec.md#31-torrent-action-requests).
HTTP/2, connection reuse, redirects, proxy use, fan-out, and automatic replay
are disabled. A successful response is kept separate from proof: completion
requires a later complete typed ledger to show the exact job absent and then a
fresh exact verification of the materialized final. If the request result is
unknown, resume checks absence first and attributes no causality; repeating a
still-present removal requires both `--acknowledge-client-removal` and
`--acknowledge-repeat-removal`. Local-only `status` reports historical marker
state and never claims that the queue or files are currently unchanged. JSON
kind is `client.removal`. If source-name retirement is desired, complete that
separate live-client proof and acknowledged transition first; removal never
infers or authorizes source retirement.

Removal pruning is a separate local deletion authority. It copies the exact
terminal intent, bounded attempt chain, sparse response chain, completion, and
their marker IDs into a canonical owner-private retention intent. Only after
that intent is durable and rebound may it delete the original request markers
and empty scratch directory; an exact tombstone completion is published last.
Crashes after either boundary are advanced only by the same full operation and
reviewed plan selectors. Ordinary `run`/`resume` stop at this boundary, while
read-only `status` labels the proof as historical and makes no client request.
JSON kind is `client.removal.retention`.

Removal `forget` has its own irreversible acknowledgement. It first publishes
a deterministic owner-private root recovery marker containing the exact
tombstone, then removes only the selected retention markers and empty operation
subtree, confirms their durable absence, and removes the recovery marker last.
After that final deletion a repeat can report only `absent_unattributed`, not
`already_forgotten`. It never reads a downloader credential, contacts a client,
deletes content, or selects another operation. JSON kind is
`client.removal.forget`.

---

Previous: [Downloader reads, path mapping, and adoption](downloader.md) · Next: [Source retirement and parent cleanup](source-retirement.md)
