# Metafiles and the private store

[Command guide](README.md) · [Documentation index](../README.md)

Inspect a metafile without exposing its announce path or query string:

```bash
ptctl torrent inspect release.torrent
ptctl torrent inspect --output json release.torrent
```

Preserve a private metafile as an immutable exact-byte artifact. Initialization
is explicit because the store must enforce private permissions and atomic
no-clobber commits. Import copies the accepted raw bytes and leaves the source
file in place without rewriting or deleting it; the source read may still
update atime, hydrate a placeholder, or incur remote-filesystem cost:

```bash
ptctl metafile store init --store "D:\Private\ptctl-metafiles"
ptctl metafile store import --store "D:\Private\ptctl-metafiles" release.torrent
ptctl metafile store inspect \
  --store "D:\Private\ptctl-metafiles" \
  sha256:WHOLE_METAFILE_SHA256
```

Fetch one exact private metafile directly into that initialized store. This is
an effectful operation: the tracker may record the GET, so the acknowledgement
must be explicit. All usage, the single remote ID, limits, capability, and
store assurances are checked before the cookie is read from stdin:

```bash
printf '%s' "$TJUPT_COOKIE" | ptctl site metafile fetch \
  --cookie-stdin \
  --acknowledge-site-effect \
  --metafile-store "D:\Private\ptctl-metafiles" \
  tjupt REMOTE_ID
```

The command performs no torrent-detail lookup and sends at most one GET. It
follows no redirect, performs no retry, and offers no raw stdout or arbitrary
destination. The validated response goes through the ordinary exact-byte,
no-clobber store publication path. Its report may record the invocation-scoped
`observed_exact_variant` relation only when that store import explicitly
returns a valid exact artifact reference and its whole-response digest and
consumed-byte receipt agree. A pre-publication or import failure retains only
the bounded request/response receipt; the CLI never hashes the response again
to promote it into a site-to-variant observation. Once an exact reference has
been established, a later durability or post-publication failure does not erase
that invocation-only observation.

After, and only after, a fully successful private artifact import, the command
publishes a sealed `site.metafile.binding.v1` record in the same store. The
record is a binding-last commit marker for one historical exact response. Its
record ID is printed as the explicit handoff to reconciliation. Publication
rechecks the sealed record and referenced private artifact under one
operation-bound physical store identity; it is not a two-object transaction,
a site signature, or proof that the site's current mapping is unchanged. A
new acknowledged GET creates a new historical observation even when it returns
the same variant. Bindings can be audited later without credentials:

```bash
ptctl site metafile binding list \
  --metafile-store "D:\Private\ptctl-metafiles"

ptctl site metafile binding inspect \
  --metafile-store "D:\Private\ptctl-metafiles" \
  sha256:SEALED_BINDING_RECORD_ID
```

`list` is a bounded, deterministic name inventory. It returns unverified
record locators only, never reads their payloads or artifacts, and never
selects a latest observation. `inspect` accepts one explicit record ID and
jointly re-hashes, strictly parses, and privacy-checks that record's linked
private metafile under one bound store session. Its result remains historical;
it does not contact the site or claim that the current remote mapping is
unchanged. JSON kinds are `site.metafile.binding.list` and
`site.metafile.binding.inspect`.

The artifact ID hashes the complete raw `.torrent` byte stream, not just its
`info` dictionary. Two private variants with the same infohash therefore remain
different artifacts. Import is idempotent: an already present, byte-identical
variant succeeds without another write. The store is permission-isolated, not
encrypted; anyone who can read the store can read the embedded announce
passkey. Store and import-source absolute paths are hidden unless
`--show-absolute-paths` is explicitly requested; object paths are never
emitted.

No-clobber publication and durability confirmation are distinct. If the object
was completely published but the following directory durability check fails,
the report uses `published_durability_unconfirmed` and `writes_performed` may be
`1`; the complete object may remain visible. `ptctl` never reports that case as
zero-write or removes the published object as rollback.

Before creating store layout entries or staging an artifact, the operation
binds one reviewed root identity. The root, `objects`, `tmp`, staging file, and
final object must then remain on that same reviewed local filesystem; POSIX uses
handle-relative operations and Windows pins the path namespace with no-delete
directory guards. A rename, replacement, mount change, or volume change fails
closed. Volatile memory filesystems such as tmpfs and ramfs are not accepted as
durable stores. A store path whose existing prefix contains a symbolic link is
rejected; use the resolved physical path (for example `/private/tmp/...` rather
than `/tmp/...` on macOS).

A killed import can leave a private staging file in the store's `tmp`
directory. Later imports remove such files once they are more than 24 hours old,
a few at a time and only when the name exactly matches ptctl's own random
staging names; nothing else in `tmp` is ever deleted.

Publication assurance is invocation-scoped. A newly initialized store or newly
stored object can report `confirmed_this_invocation`. Read-only inspection and
idempotent `already_*` outcomes verify current bytes and privacy, but label the
historical publication as unobservable instead of inferring an old no-clobber
or durability event. A failure after publication and durability but before all
post-commit checks is reported as `published_post_commit_failure`.

Every existing metafile consumer accepts exactly one source. Legacy file forms
remain unchanged. The stored form replaces the positional file or `--torrent`
with the following pair:

```bash
ptctl torrent inspect \
  --metafile-store "D:\Private\ptctl-metafiles" \
  --metafile-variant sha256:WHOLE_METAFILE_SHA256

ptctl seed discover \
  --metafile-store "D:\Private\ptctl-metafiles" \
  --metafile-variant sha256:WHOLE_METAFILE_SHA256 \
  --search-root "D:\Media"
```

An explicitly selected historical site binding can be added only when
reconciliation reads its metafile from that same private store:

```bash
ptctl reconcile report \
  --metafile-store "D:\Private\ptctl-metafiles" \
  --metafile-variant sha256:WHOLE_METAFILE_SHA256 \
  --site-binding-record sha256:SEALED_BINDING_RECORD_ID \
  --search-root "D:\Media"
```

`--site-ref SITE/REMOTE_ID` is an optional expected-ref cross-check. By itself
it remains `declared_unbound`. A binding record cannot be paired with
`--torrent`, loaded from a separate store, or selected implicitly by
enumeration/latest. The name-only list exists solely to hand an explicit record
ID to inspect or reconciliation.

The same pair is supported by `torrent verify`, `seed discover`, `seed plan`,
`seed materialize run|resume`, and `reconcile report`. Supplying only half the
pair, mixing it with a file/`--torrent`, or adding a positional metafile to the
stored form is invalid usage. Loading from the store rechecks the object digest
and parses the exact bytes before invoking the same core. Inspection,
verification, discovery, planning, and reconciliation consumers remain
zero-write; materialize still requires its independent filesystem-write
acknowledgement.

---

Next: [Storage profiles and indexes](storage.md)
