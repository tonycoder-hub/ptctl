package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/security"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/sites"
	"github.com/tonycoder-hub/ptctl/internal/storageindex"
)

var (
	Version = "dev"
	Commit  = "unknown"
)

type app struct {
	stdin            io.Reader
	stdout           io.Writer
	stderr           io.Writer
	registry         *site.Registry
	refreshDiscovery refreshDiscoveryFunc
}

type refreshDiscoveryFunc func(
	context.Context,
	*storageindex.Repository,
	*metafile.MetaInfo,
	storageindex.Profile,
	storageindex.CandidateLimits,
	seed.DiscoverOptions,
	storageindex.RefreshOptions,
) (seed.DiscoveryResult, error)

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{
		stdin: stdin, stdout: stdout, stderr: stderr,
		registry: sites.NewRegistry(),
	}
	return runWithApp(args, a)
}

func runWithApp(args []string, a *app) int {
	if len(args) == 0 {
		a.help()
		return 0
	}
	var err error
	switch args[0] {
	case "help":
		err = a.helpTopic(args[1:])
	case "-h", "--help":
		a.help()
		return 0
	case "version", "--version":
		err = a.version(args[1:])
	case "site":
		err = a.site(args[1:])
	case "torrent":
		err = a.torrent(args[1:])
	case "metafile":
		err = a.metafileCommand(args[1:])
	case "storage":
		err = a.storage(args[1:])
	case "client":
		err = a.client(args[1:])
	case "reconcile":
		err = a.reconcileCommand(args[1:])
	case "seed":
		err = a.seed(args[1:])
	default:
		err = usageError("unknown command %q", args[0])
	}
	if err == nil {
		return 0
	}
	fmt.Fprintf(a.stderr, "error: %s\n", terminalSafe(security.Redact(err.Error())))
	var usage *usageErr
	if errors.As(err, &usage) {
		fmt.Fprintln(a.stderr, "run 'pt help' for usage (ptctl is also supported)")
		return 2
	}
	var integrity *integrityErr
	if errors.As(err, &integrity) {
		return 3
	}
	var inconclusive *inconclusiveErr
	if errors.As(err, &inconclusive) {
		return 4
	}
	return 1
}

func (a *app) refreshAndDiscoverFromIndex(
	ctx context.Context,
	repository *storageindex.Repository,
	meta *metafile.MetaInfo,
	profile storageindex.Profile,
	candidateLimits storageindex.CandidateLimits,
	options seed.DiscoverOptions,
	refreshOptions storageindex.RefreshOptions,
) (seed.DiscoveryResult, error) {
	if a.refreshDiscovery != nil {
		return a.refreshDiscovery(ctx, repository, meta, profile, candidateLimits, options, refreshOptions)
	}
	return seed.RefreshAndDiscoverFromIndex(ctx, repository, meta, profile, candidateLimits, options, refreshOptions)
}

func (a *app) help() {
	fmt.Fprint(a.stdout, `pt cli — a conservative private-tracker content CLI

Read-only start (ptctl remains a compatible command):
  pt version
  pt help torrent
  pt help client
  pt help config
  pt torrent inspect FILE.torrent
  pt torrent verify --content PATH FILE.torrent
  pt reconcile report --torrent FILE.torrent --source PATH --output json

The local-only reconciliation is partial until a downloader and path mapping
are supplied. See docs/READ_ONLY_QUICKSTART.md for the short read-only guide.

Usage:
  ptctl site list [--output table|json]
  ptctl site capabilities [--output table|json] [SITE]
  ptctl site login --credential-bundle-stdin [--session-days N] SITE
  ptctl site status (--cookie-stdin | --api-key-stdin) [--output table|json] SITE
  ptctl site account (--cookie-stdin | --api-key-stdin) [--output table|json] SITE
  ptctl site search (--cookie-stdin | --api-key-stdin) [--output table|json] SITE QUERY...
  ptctl site detail --cookie-stdin [--output table|json] SITE REMOTE_ID
  ptctl site bonus-catalog --cookie-stdin [--output table|json] SITE
  ptctl site bonus review --cookie-stdin [--output table|json] SITE OPTION
  ptctl site bonus exchange prepare --state-store DIR --expect-review-id ID [--output table|json] SITE OPTION
  ptctl site bonus exchange submit --state-store DIR --intent-record RECORD_ID --expect-review-id ID (--cookie-stdin | --credential-bundle-stdin) --acknowledge-bonus-exchange [--output table|json] SITE OPTION
  ptctl site bonus exchange status --state-store DIR --intent-record RECORD_ID [--output table|json]
  ptctl site bonus exchange list --state-store DIR [--max-operations N] [--output table|json]
  ptctl site bonus exchange prune --state-store DIR --intent-record RECORD_ID --operation-id ID --acknowledge-state-prune [--output table|json]
  ptctl site bonus exchange forget --state-store DIR --retention-record RECORD_ID --operation-id ID --acknowledge-history-forget [--output table|json]
  ptctl site metafile fetch --cookie-stdin --acknowledge-site-effect --metafile-store DIR [--output table|json] SITE REMOTE_ID
  ptctl site metafile binding list --metafile-store DIR [--output table|json]
  ptctl site metafile binding inspect --metafile-store DIR [--output table|json] RECORD_ID

  ptctl torrent inspect [--output table|json] (FILE.torrent | --metafile-store DIR --metafile-variant ID)
  ptctl torrent verify --content PATH [--output table|json] (FILE.torrent | --metafile-store DIR --metafile-variant ID)

  ptctl metafile store init --store DIR [--output table|json]
  ptctl metafile store import --store DIR [--output table|json] FILE.torrent
  ptctl metafile store inspect --store DIR [--output table|json] METAFILE_VARIANT_ID

  ptctl storage probe [--output table|json] PATH
  ptctl storage map --host-root PATH --client-root PATH [--client-style posix|windows] HOST_PATH
  ptctl storage profile create --state-store DIR --name NAME --search-root PATH [--search-root PATH...] [--output table|json]
  ptctl storage profile inspect --state-store DIR [--output table|json] PROFILE
  ptctl storage index refresh --state-store DIR --profile PROFILE [--retain-generations N] [--output table|json]
  ptctl storage index refresh-discover --state-store DIR --profile PROFILE (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) [--target PATH] [--output table|json]
  ptctl storage index inspect --state-store DIR --profile PROFILE [--snapshot-record ID] [--output table|json]
  ptctl storage index prune --state-store DIR --profile PROFILE --acknowledge-index-prune [--retain-generations N] [--output table|json]

  ptctl client status --driver qbittorrent|transmission --url URL --username USER --password-stdin [--output table|json]
  ptctl client list --driver qbittorrent|transmission --url URL --username USER --password-stdin [--output table|json]
  ptctl client adopt plan --metafile-store DIR --metafile-variant ID --target PATH --materialize-operation ID --materialize-plan-id ID --host-root PATH --client-root PATH --client-style posix|windows --driver qbittorrent|transmission --url URL --username USER --password-stdin [--adopt-existing-stopped | --prior-adoption-operation ID --prior-adoption-plan-id ID | --prior-removal-operation ID --prior-removal-plan-id ID] [--output table|json]
  ptctl client adopt run [same selectors] --expect-adoption-plan-id ID (--acknowledge-client-add [--acknowledge-client-re-adoption | --acknowledge-client-re-add-after-removal] | --adopt-existing-stopped --acknowledge-existing-stopped-adoption) [--output table|json]
  ptctl client adopt resume [same selectors] --expect-adoption-plan-id ID ([--acknowledge-client-add --acknowledge-repeat-add] [--acknowledge-client-re-adoption | --acknowledge-client-re-add-after-removal] | --adopt-existing-stopped --acknowledge-existing-stopped-adoption) [--output table|json] OPERATION_ID
  ptctl client adopt status --target PATH [--output table|json] OPERATION_ID
  ptctl client adopt prune --target PATH --expect-adoption-plan-id ID --acknowledge-operation-state-deletion [--output table|json] OPERATION_ID
  ptctl client adopt forget --target PATH --expect-adoption-plan-id ID --acknowledge-historical-evidence-deletion [--output table|json] OPERATION_ID
  ptctl client activate plan [adoption selectors] --adoption-operation ID --adoption-plan-id ID [--start-after-recheck] [--output table|json]
  ptctl client activate run [same selectors] --expect-activation-plan-id ID --acknowledge-client-recheck [--output table|json]
  ptctl client activate resume [same selectors] --expect-activation-plan-id ID [explicit acknowledgement flags] [--output table|json] OPERATION_ID
  ptctl client activate status --target PATH [--output table|json] OPERATION_ID
  ptctl client activate prune --target PATH --expect-activation-plan-id ID --acknowledge-operation-state-deletion [--output table|json] OPERATION_ID
  ptctl client activate forget --target PATH --expect-activation-plan-id ID --acknowledge-historical-evidence-deletion [--output table|json] OPERATION_ID
  ptctl client stop plan [activation/final/mapping/client selectors] [--output table|json]
  ptctl client stop run [same selectors] --expect-stop-plan-id ID --acknowledge-client-stop [--output table|json]
  ptctl client stop resume [same selectors] --expect-stop-plan-id ID [--acknowledge-client-stop --acknowledge-repeat-stop] [--output table|json] OPERATION_ID
  ptctl client stop status --target PATH --expect-stop-plan-id ID [--output table|json] OPERATION_ID
  ptctl client stop prune --target PATH --expect-stop-plan-id ID --acknowledge-operation-state-deletion [--output table|json] OPERATION_ID
  ptctl client stop forget --target PATH --expect-stop-plan-id ID --acknowledge-historical-evidence-deletion [--output table|json] OPERATION_ID
  ptctl client remove plan [activation/final/mapping/client selectors] [--output table|json]
  ptctl client remove run [same selectors] --expect-removal-plan-id ID --acknowledge-client-removal [--output table|json]
  ptctl client remove resume [same selectors] --expect-removal-plan-id ID [--acknowledge-client-removal --acknowledge-repeat-removal] [--output table|json] OPERATION_ID
  ptctl client remove status --target PATH --expect-removal-plan-id ID [--output table|json] OPERATION_ID
  ptctl client remove prune --target PATH --expect-removal-plan-id ID --acknowledge-operation-state-deletion [--output table|json] OPERATION_ID
  ptctl client remove forget --target PATH --expect-removal-plan-id ID --acknowledge-historical-evidence-deletion [--output table|json] OPERATION_ID

  ptctl reconcile report (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) (--source PATH | --search-root PATH... | --state-store DIR --storage-profile PROFILE | --target PATH --materialize-operation ID --materialize-plan-id ID) [--adoption-operation ID --adoption-plan-id ID] [--activation-operation ID --activation-plan-id ID] [--stop-operation ID --stop-plan-id ID] [--removal-operation ID --removal-plan-id ID | --retirement-operation ID --retirement-plan-id ID --retirement-search-root PATH... [--parent-cleanup-operation ID --parent-cleanup-plan-id ID --parent-cleanup-search-root PATH...]] [--output table|json]
  ptctl reconcile refresh-report (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) --state-store DIR --storage-profile PROFILE [--driver DRIVER --url URL --username USER --password-stdin] [--output table|json]

  ptctl seed plan (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) --source PATH --target PATH [--output table|json]
  ptctl seed discover (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) (--search-root PATH... | --state-store DIR --storage-profile PROFILE) [--target PATH] [--output table|json]
  ptctl seed materialize run (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) (--source PATH | --search-root PATH... | --state-store DIR --storage-profile PROFILE --snapshot-record RECORD --select-source-match MATCH) --target PATH --expect-plan-id ID --acknowledge-filesystem-write [--output table|json]
  ptctl seed materialize resume (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) --target PATH --expect-plan-id ID --acknowledge-filesystem-write [source selector] [--output table|json] OPERATION_ID
  ptctl seed materialize status --target PATH [--output table|json] [OPERATION_ID]
  ptctl seed materialize abandon --target PATH --acknowledge-abandon [--output table|json] OPERATION_ID
  ptctl seed materialize prune --target PATH --expect-plan-id ID --acknowledge-operation-state-deletion [--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID] [--output table|json] OPERATION_ID
  ptctl seed materialize forget --target PATH --expect-plan-id ID --acknowledge-historical-evidence-deletion [--output table|json] OPERATION_ID
  ptctl seed retire plan (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) --search-root PATH --target PATH --materialize-operation ID --materialize-plan-id ID --activation-operation ID --activation-plan-id ID --host-root PATH --client-root PATH --client-style posix|windows --driver qbittorrent|transmission --url URL --username USER --password-stdin [--output table|json]
  ptctl seed retire run [same selectors] --expect-plan-id ID --acknowledge-source-deletion [--output table|json]
  ptctl seed retire resume [same selectors] --expect-plan-id ID --acknowledge-source-deletion [--output table|json] OPERATION_ID
  ptctl seed retire status --target PATH [--output table|json] [OPERATION_ID]
  ptctl seed retire prune --target PATH --expect-plan-id ID --acknowledge-operation-state-deletion [--output table|json] OPERATION_ID
  ptctl seed retire forget --target PATH --expect-plan-id ID --acknowledge-historical-evidence-deletion [--output table|json] OPERATION_ID
  ptctl seed retire parent-cleanup plan --target PATH --retirement-operation ID --retirement-plan-id ID --search-root PATH [--search-root PATH...] [--output table|json]
  ptctl seed retire parent-cleanup run --target PATH --retirement-operation ID --retirement-plan-id ID --search-root PATH [--search-root PATH...] --expect-cleanup-plan-id ID --acknowledge-empty-parent-removal [--output table|json]
  ptctl seed retire parent-cleanup resume --target PATH --search-root PATH [--search-root PATH...] --expect-cleanup-plan-id ID --acknowledge-empty-parent-removal [--output table|json] OPERATION_ID
  ptctl seed retire parent-cleanup status --target PATH [--output table|json] OPERATION_ID
  ptctl seed retire parent-cleanup prune --target PATH --expect-cleanup-plan-id ID --acknowledge-operation-state-deletion [--output table|json] OPERATION_ID
  ptctl seed retire parent-cleanup forget --target PATH --expect-cleanup-plan-id ID --acknowledge-historical-evidence-deletion [--output table|json] OPERATION_ID
  ptctl version [--output table|json]

Safety defaults:
  * Ordinary site reads are one bounded request per invocation: a GET, or a read-only POST for JSON API sites such as M-Team. Bonus exchange is one fresh GET plus at most one acknowledged POST after a durable marker. Site requests are never automatically retried.
  * Session cookies and site API keys are accepted only through stdin and are never persisted.
  * Site login reads the account password and optional one-time code only from stdin, sends one login request plus one verifying read, never retries, and writes the new session cookie only to a non-terminal stdout.
  * .torrent tracker URLs are reduced to origins; passkeys are never printed.
  * The metafile store preserves exact private bytes with owner-only access and atomic no-clobber commits.
  * v1, v2, and hybrid verification use exact content proofs; names and sizes are not proof.
  * Seed discovery and materialization planning have hard scan/proof budgets and perform no writes. Storage index refresh-discover is a separately named explicit write boundary which publishes one immutable generation before same-call proof.
  * Seed materialize run/resume copy only and never clobber; prune has a separate acknowledgement and deletes only one explicit operation's private state while retaining its tombstone; forget has a third acknowledgement and irreversibly deletes only that exact tombstone plus its last recovery marker.
  * Seed retire plan performs fresh proof reads only and grants no deletion authority. Run/resume require a separate exact plan ID and deletion acknowledgement, journal every explicit name, and never remove directories, aliases, padding, empty files, or final content. Prune has its own acknowledgement and deletes only one terminal operation's private journal while retaining a tombstone; forget has a third acknowledgement and deletes only that exact tombstone plus its last recovery marker.
  * Seed retire parent-cleanup plan is zero-write. Its separately acknowledged run/resume journal and remove only reviewed same-identity empty immediate parents; they never recurse into ancestors, search roots, files, or non-empty directories. Prune replaces one terminal cleanup journal with an exact no-path tombstone; forget separately and irreversibly removes only that exact tombstone through a final root-level recovery marker.
  * Client adoption has three reviewed actions: add one absent exact-infohash job in stopped mode, re-add one job after an explicitly attributed terminal removal, or record an observation-only lineage for one already-present exact stopped job without submitting a metafile or mutating the downloader. Transmission is v1-only; a matching built-in driver can feed the separate reviewed recheck/start workflow. Adoption never rechecks, resumes, moves, or deletes content. Its separately acknowledged prune deletes only one terminal private journal after sealing an exact tombstone.
  * Client activation only rechecks or starts the reviewed exact existing job, with at most one non-retried mutation per invocation. Its separately acknowledged local prune deletes only one terminal private journal after sealing an exact tombstone and never reads a credential or contacts the client.
  * Client stop targets one reviewed complete started exact job, journals intent before one non-retried request, and requires a fresh exact stopped-state observation plus final filesystem re-verification before completion. Recovery observes first and never repeats an inconclusive request without a separate acknowledgement.
  * Client removal targets one reviewed typed-identity job, explicitly keeps local data, journals intent before one non-retried request, and requires exact queue absence plus final filesystem re-verification before completion.
  * Storage index snapshots are immutable candidate hints; only a same-call complete live scan can prove current uniqueness or absence.
  * Ordinary reconcile report uses one client login, two bounded job-ledger reads, at most two bounded same-job file-list reads, and no client or filesystem writes. The separately named refresh-report may publish one immutable storage-index generation between those reads.
  * Explicit stop reconciliation reads one canonical terminal stop journal or complete retention tombstone before credentials, then binds only attributed stop history to the existing current stopped-job bracket without another request. Historical stop evidence never proves current job incarnation.
`)
}

func (a *app) version(args []string) error {
	fs := newFlagSet("version")
	output := fs.String("output", "table", "table or json")
	if done, err := a.parseReadOnlyFlags(fs, args, "version [--output table|json]"); done || err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usageError("version takes no positional arguments")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	data := map[string]string{"product": "pt cli", "version": Version, "commit": Commit}
	if *output == "json" {
		return writeJSON(a.stdout, data, nil)
	}
	if *output != "table" {
		return usageError("--output must be table or json")
	}
	fmt.Fprintf(a.stdout, "pt cli %s (%s)\n", terminalSafe(Version), terminalSafe(Commit))
	return nil
}

func validateOutput(output string) error {
	if output != "table" && output != "json" {
		return usageError("--output must be table or json")
	}
	return nil
}

// parseReadOnlyFlags handles help before any required-selector, file, or
// credential checks. Parser diagnostics stay discarded until Run redacts them.
func (a *app) parseReadOnlyFlags(fs *flag.FlagSet, args []string, syntax string) (bool, error) {
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(a.stdout, "Usage:\n  pt %s\n\nFlags:\n", syntax)
		fs.SetOutput(a.stdout)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
		return true, nil
	}
	if err != nil {
		return false, flagParseError(fs.Name(), err)
	}
	return false, nil
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

type usageErr struct{ message string }

func (e *usageErr) Error() string { return e.message }

func usageError(format string, args ...any) error {
	return &usageErr{message: fmt.Sprintf(format, args...)}
}

// flagParseError reports a flag parse failure as one short usage line. The
// flag package error already names the offending flag; a FlagSet whose Usage
// renders full help into a buffer must not echo that help as the error, where
// terminal escaping would fold it onto one unreadable line.
func flagParseError(command string, err error) error {
	return usageError("%s: %v (see 'ptctl %s --help')", command, err, command)
}

type integrityErr struct{ message string }

func (e *integrityErr) Error() string { return e.message }

type inconclusiveErr struct{ message string }

func (e *inconclusiveErr) Error() string { return e.message }

type stringListFlag []string

func (values *stringListFlag) String() string {
	return strings.Join(*values, ",")
}

func (values *stringListFlag) Set(value string) error {
	if value == "" {
		return fmt.Errorf("path must not be empty")
	}
	*values = append(*values, value)
	return nil
}
