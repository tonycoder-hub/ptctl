# ptctl documentation

[Project README](../README.md)

Start with the [project README](../README.md) for what ptctl is, how to install
it, and which commands can write. This page routes each kind of reader to the
documents that matter to them.

## Using ptctl

- [Command guide](guide/README.md): every command family in workflow order,
  with examples, plus the complete status and write-boundary statement.
- [Exit codes and report-first results](guide/exit-codes-and-reports.md): what
  each exit code means for each command family.
- [Features](FEATURES.md): everything implemented today and what is
  deliberately not implemented.

## Contributing code

- [CONTRIBUTING.md](../CONTRIBUTING.md): checks, fixture policy, and adapter
  policy.
- [Architecture](ARCHITECTURE.md): the four ledgers, ports and adapters,
  evidence and outcomes, and each durable workflow's design.
- [TJUPT adapter](TJUPT_ADAPTER.md): the reference site adapter and its
  page-level contract.
- [Materialize retention](MATERIALIZE_RETENTION.md): the prune/forget state
  machine for materialize operations.

## Security review

- [Threat model](THREAT_MODEL.md): assets, trust boundaries, and present
  controls.
- [Security policy](../SECURITY.md): how to report a vulnerability.
