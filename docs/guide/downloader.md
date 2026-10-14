# Downloader reads, path mapping, and adoption

[Command guide](README.md) · [Documentation index](../README.md)

> Recheck/start, stop, and keep-data removal continue in [client-lifecycle.md](client-lifecycle.md).

Map a host path to a Dockerized downloader namespace:

```bash
ptctl storage map \
  --host-root "D:\PT" \
  --client-root /downloads \
  --client-style posix \
  "D:\PT\Release"
```

Read downloader state (read-only commands support qBittorrent and
Transmission):

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client status \
  --driver qbittorrent \
  --url https://seedbox.example \
  --username admin \
  --password-stdin

printf '%s' "$TRANSMISSION_PASSWORD" | ptctl client list \
  --driver transmission \
  --url https://seedbox.example/transmission/rpc \
  --username admin \
  --password-stdin
```

Adopt one exact materialized layout into qBittorrent or Transmission without
starting it. The default action adds an absent stopped job:

```bash
# First review the deterministic adoption plan. MATERIALIZE_PLAN_ID is the
# reviewed seed discover --target ID used by the committed materialize run.
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client adopt plan \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --host-root 'D:\' \
  --client-root /downloads \
  --client-style posix \
  --driver qbittorrent \
  --url https://seedbox.example \
  --username admin \
  --password-stdin \
  --output json

printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client adopt run \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --host-root 'D:\' \
  --client-root /downloads \
  --client-style posix \
  --driver qbittorrent \
  --url https://seedbox.example \
  --username admin \
  --password-stdin \
  --expect-adoption-plan-id ADOPTION_PLAN_ID \
  --acknowledge-client-add \
  --output json
```

If the exact job already exists in stopped state at the reviewed final path,
select the distinct observation-only action in both plan and run:

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client adopt plan \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin \
  --adopt-existing-stopped \
  --output json

printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client adopt run \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin \
  --adopt-existing-stopped \
  --expect-adoption-plan-id ADOPTION_PLAN_ID \
  --acknowledge-existing-stopped-adoption \
  --output json
```

For Transmission, use `--driver transmission`, its full RPC URL (for example
`https://seedbox.example/transmission/rpc`), and the Transmission credential.
Only v1 metafiles are eligible: Transmission's audited `hash_string` claim is
a complete SHA-1/v1 identity, but the RPC does not expose a typed v2 hash.

The default action is intentionally a one-way, stopped-add handoff. It requires
the exact raw metafile from the owner-only metafile store, a current exact proof
of the committed (or retained) materialized final, and a complete typed-infohash
queue observation proving that the target job is absent. It then publishes a
private target-root-local request intent before exactly one add POST. The POST
submits the exact stored bytes with the reviewed save path and requests a
stopped/paused job; for qBittorrent it also disables automatic torrent
management and requests the original content layout, so client defaults cannot
relocate or re-root the data. It never changes an existing job, moves data, starts a
recheck, resumes transfer, deletes content, or retires the source.

The normal qBittorrent run uses one login, one complete ledger read before the
request, one add POST, and one complete ledger read after it. Transmission uses
its fixed two-request CSRF/version handshake followed by the same
before/add/after sequence, for five requests total. Success additionally
requires
one unique exact typed-infohash job in a stopped state, the reviewed size and
lexical save/content paths, a second exact verification of the materialized
final, and a durable completion marker. Its outcome is
`adopted_pending_client_recheck`, not “seeding verified”. Neither client proves
the raw private variant it stored, and no client recheck is performed.

Observation-only adoption is a separate plan action and acknowledgement. It
requires one already-present unique exact typed-infohash job with the reviewed
size, stopped state, and exact lexical save/content paths. In one read session,
ptctl records a first complete job-ledger observation, freshly re-verifies the
exact materialized final, then records a later complete observation of the same
opaque job and state. qBittorrent therefore uses one login plus two ledger
reads; Transmission uses its fixed two-request handshake plus two ledger reads.
The action sends no add/recheck/start/move request, does not load the one-shot
raw-metafile submission payload, and cannot observe which private metafile
wrapper originally created the job. Its outcome is
`existing_stopped_job_adopted_pending_client_recheck`. The bracket is serial
and non-atomic; it does not prove a downloader job incarnation or that the
client has checked the bytes. Adoption checks only the job-level size and
lexical paths. The separate activation workflow performs the bounded per-file
layout observations before it can recheck or start a multi-file job.

If that exact terminal job later disappears, ptctl does not silently reuse the
old operation or choose a latest completion. A new `plan`, `run`, or `resume`
may instead supply the exact pair
`--prior-adoption-operation PRIOR_OPERATION_ID` and
`--prior-adoption-plan-id PRIOR_PLAN_ID`. The local canonical completion or
retention tombstone is verified before password stdin or network access and
must describe the same driver, client configuration, path mapping, exact
metafile, materialized final, and typed identity. The current complete client
ledger must again prove exact absence. The resulting plan binds the prior
operation, plan, and completion marker into a new independent operation ID;
execution additionally requires `--acknowledge-client-re-adoption` alongside
`--acknowledge-client-add`. The prior journal or tombstone remains intact.
This flow does not infer why the job disappeared, attribute a removal, or
authorize mutation of any remaining job. A forgotten prior completion cannot
authorize re-adoption.

When the disappearance was caused by ptctl's own completed keep-data removal,
use the distinct removal-attributed lineage instead:

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client adopt run \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --host-root 'D:\' --client-root /downloads --client-style posix \
  --driver qbittorrent --url https://seedbox.example --username admin \
  --password-stdin \
  --prior-removal-operation sha256:REMOVAL_OPERATION_DIGEST \
  --prior-removal-plan-id REMOVAL_PLAN_ID \
  --expect-adoption-plan-id ADOPTION_PLAN_ID \
  --acknowledge-client-add \
  --acknowledge-client-re-add-after-removal \
  --output json
```

This creates the separate `readd_stopped_after_removal` action. Before stdin
or client I/O, the explicit removal selector must rebind a live journal or
exact retention tombstone whose completion basis is
`accepted_response_then_exact_absence`. An unattributed unknown-request
absence, public JSON, a partial selector, or a generic prior-adoption selector
cannot authorize this action. A fresh complete queue observation after the
removal completion must still prove absence. The reviewed plan binds the
removal operation, plan, intent, completion, use, job, file-layout, activation,
and observation-time lineage; changing only live-versus-retained storage form
does not change the plan ID. The resulting adoption completion remains a normal
stopped-job authority for reconciliation and activation.

Transmission accepts only an explicit `torrent_added` response; its documented
`torrent_duplicate` success envelope is treated as a rejected adoption, not as
evidence that this invocation created the observed job. The modern and legacy
envelopes follow the official
[current RPC specification](https://github.com/transmission/transmission/blob/main/docs/rpc-spec.md#34-adding-a-torrent)
and
[Transmission 4.0.6 RPC specification](https://github.com/transmission/transmission/blob/4.0.6/docs/rpc-spec.md#34-adding-a-torrent).
If a Transmission add response is lost, a later same-hash job is not
automatically attributed to the request. The user may remove that
external/conflicting job and explicitly repeat the stopped add, but ptctl will
not mutate it.

If the POST response or after-ledger is lost, the durable attempt remains
`request_result_unknown`. Resume first reads the current queue and never
repeats the POST automatically:

```bash
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client adopt resume \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --host-root 'D:\' \
  --client-root /downloads \
  --client-style posix \
  --driver qbittorrent \
  --url https://seedbox.example \
  --username admin \
  --password-stdin \
  --expect-adoption-plan-id ADOPTION_PLAN_ID \
  sha256:ADOPTION_OPERATION_DIGEST

# Only if a second POST is deliberately accepted:
printf '%s' "$QBITTORRENT_PASSWORD" | ptctl client adopt resume \
  --metafile-store PRIVATE_STORE \
  --metafile-variant sha256:WHOLE_METAFILE_DIGEST \
  --target "D:\PT" \
  --materialize-operation sha256:MATERIALIZE_OPERATION_DIGEST \
  --materialize-plan-id MATERIALIZE_PLAN_ID \
  --host-root 'D:\' \
  --client-root /downloads \
  --client-style posix \
  --driver qbittorrent \
  --url https://seedbox.example \
  --username admin \
  --password-stdin \
  --expect-adoption-plan-id ADOPTION_PLAN_ID \
  --acknowledge-client-add \
  --acknowledge-repeat-add \
  sha256:ADOPTION_OPERATION_DIGEST

ptctl client adopt status \
  --target "D:\PT" \
  sha256:ADOPTION_OPERATION_DIGEST

# Once adoption has an exact terminal completion, retain that authority while
# deleting only the selected operation's heavy request-attempt journal.
ptctl client adopt prune \
  --target "D:\PT" \
  --expect-adoption-plan-id ADOPTION_PLAN_ID \
  --acknowledge-operation-state-deletion \
  --output json \
  sha256:ADOPTION_OPERATION_DIGEST

# After no downstream workflow needs this historical adoption authority,
# irreversibly erase the exact tombstone and its final attribution marker.
ptctl client adopt forget \
  --target "D:\PT" \
  --expect-adoption-plan-id ADOPTION_PLAN_ID \
  --acknowledge-historical-evidence-deletion \
  --output json \
  sha256:ADOPTION_OPERATION_DIGEST
```

`status` is local and read-only: it never reads a password or contacts the
client, and therefore reports current client state as unobserved. It validates
canonical marker presence but does not issue a directory sync, so only an
effectful resume refreshes journal durability before relying on those markers.
An operation ID is deterministic from the reviewed adoption plan; no command
enumerates or chooses a “latest” operation. JSON kind `client.adoption` keeps declared
effects, actual/uncertain journal writes, request counts, before/after typed
identity states, current-final proof basis, and non-null findings separate.
Re-adoption reports expose only the opaque prior operation, plan, and
completion IDs; planning/execution also records the explicit historical-read
effect, while later local status does not pretend to re-read the prior proof.
No report copies the prior job locator or paths.
Raw host/client paths, endpoint, username, password, opaque downloader job key, magnet
URI, tracker URL, passkey, and raw metafile bytes never enter the report.

`prune` is a separate local-only deletion boundary. It requires the full
operation ID, the reviewed adoption plan ID, and
`--acknowledge-operation-state-deletion`; it accepts no client credential and
makes no network request. Before deletion it copies the canonical intent,
bounded attempt chain, completion, and their domain-separated IDs into a
durable owner-private retention intent. It then removes only the selected
operation's original markers and empty scratch directory and publishes a
retention completion. For either built-in driver, the resulting two-marker
tombstone remains usable by `client activate` only after a same-invocation
bound read recreates opaque `VerifiedCompletion` authority. The activation
driver must match the adoption driver; Transmission authority remains limited
to a complete typed v1 hash.
JSON or a copied public observation cannot do so. An intent-only crash state
blocks ordinary resume and is recoverable only by repeating the same explicit
prune selector. JSON kind is
`client.adoption.retention`; `pruned` and `already_pruned` return `0`.

Adoption `forget` is a third, narrower irreversible boundary. It accepts only
the full operation ID, reviewed adoption plan ID, and
`--acknowledge-historical-evidence-deletion`; it reads no credential and makes
no network request. Before touching the tombstone it copies the exact retained
intent and completion, including the bounded attempt chain, into a deterministic
owner-private root recovery intent. It then removes only those retained
markers, their empty directory, and the exact operation subtree, confirms
durable absence, and removes the last root intent. Staged, published, and
operation-removed crash states are recovered only by repeating the same
selector. While either intent exists, status reports `forgetting` and resume,
completion proof, and prune stop before client access. After final removal a
repeat reports `absent_unattributed`, not historical success. JSON kind is
`client.adoption.forget`. Forget cannot revoke a process-local
`VerifiedCompletion` already issued to another live caller.

---

Previous: [Site reads and bonus exchange](sites.md) · Next: [Client activation, stop, and removal](client-lifecycle.md)
