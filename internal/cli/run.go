package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/clientremove"
	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/downloader/qbittorrent"
	"github.com/tonycoder-hub/ptctl/internal/downloader/transmission"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
	"github.com/tonycoder-hub/ptctl/internal/security"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/tjupt"
	"github.com/tonycoder-hub/ptctl/internal/sitebinding"
	"github.com/tonycoder-hub/ptctl/internal/sourceretire"
	"github.com/tonycoder-hub/ptctl/internal/storage"
	"github.com/tonycoder-hub/ptctl/internal/storageindex"
)

var (
	Version = "dev"
	Commit  = "unknown"
)

type app struct {
	stdin    io.Reader
	stdout   io.Writer
	stderr   io.Writer
	registry *site.Registry
}

type envelope struct {
	Schema   string   `json:"schema"`
	Kind     string   `json:"kind"`
	Data     any      `json:"data"`
	Warnings []string `json:"warnings,omitempty"`
}

type readOnlyDownloaderDriver interface {
	downloader.Driver
	configuredLedgerDriver
}

type configuredLedgerDriver interface {
	downloader.LedgerDriver
	ClientConfigID(username string) (string, error)
}

func newReadOnlyDownloaderDriver(name, endpoint string) (readOnlyDownloaderDriver, error) {
	switch name {
	case downloader.DriverQBittorrent:
		return qbittorrent.New(endpoint)
	case downloader.DriverTransmission:
		return transmission.New(endpoint)
	default:
		return nil, fmt.Errorf("unsupported read-only downloader driver %q", name)
	}
}

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{
		stdin: stdin, stdout: stdout, stderr: stderr,
		registry: site.NewRegistry(tjupt.New("")),
	}
	if len(args) == 0 {
		a.help()
		return 0
	}
	var err error
	switch args[0] {
	case "help", "-h", "--help":
		a.help()
		return 0
	case "version":
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
	fmt.Fprintf(stderr, "error: %s\n", terminalSafe(security.Redact(err.Error())))
	var usage *usageErr
	if errors.As(err, &usage) {
		fmt.Fprintln(stderr, "run 'ptctl help' for usage")
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

func (a *app) help() {
	fmt.Fprint(a.stdout, `ptctl — a conservative private-tracker content CLI

Usage:
  ptctl site list [--output table|json]
  ptctl site capabilities [--output table|json] [SITE]
  ptctl site status --cookie-stdin [--output table|json] SITE
  ptctl site account --cookie-stdin [--output table|json] SITE
  ptctl site search --cookie-stdin [--output table|json] SITE QUERY...
  ptctl site detail --cookie-stdin [--output table|json] SITE REMOTE_ID
  ptctl site bonus-catalog --cookie-stdin [--output table|json] SITE
  ptctl site bonus review --cookie-stdin [--output table|json] SITE OPTION
  ptctl site bonus exchange prepare --state-store DIR --expect-review-id ID [--output table|json] SITE OPTION
  ptctl site bonus exchange submit --state-store DIR --intent-record RECORD_ID --expect-review-id ID --cookie-stdin --acknowledge-bonus-exchange [--output table|json] SITE OPTION
  ptctl site bonus exchange status --state-store DIR --intent-record RECORD_ID [--output table|json]
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
  ptctl storage index refresh --state-store DIR --profile PROFILE [--output table|json]
  ptctl storage index inspect --state-store DIR --profile PROFILE [--snapshot-record ID] [--output table|json]

  ptctl client status --driver qbittorrent|transmission --url URL --username USER --password-stdin [--output table|json]
  ptctl client list --driver qbittorrent|transmission --url URL --username USER --password-stdin [--output table|json]
  ptctl client adopt plan --metafile-store DIR --metafile-variant ID --target PATH --materialize-operation ID --materialize-plan-id ID --host-root PATH --client-root PATH --client-style posix|windows --driver qbittorrent|transmission --url URL --username USER --password-stdin [--adopt-existing-stopped | --prior-adoption-operation ID --prior-adoption-plan-id ID] [--output table|json]
  ptctl client adopt run [same selectors] --expect-adoption-plan-id ID (--acknowledge-client-add [--acknowledge-client-re-adoption] | --adopt-existing-stopped --acknowledge-existing-stopped-adoption) [--output table|json]
  ptctl client adopt resume [same selectors] --expect-adoption-plan-id ID ([--acknowledge-client-add --acknowledge-repeat-add] [--acknowledge-client-re-adoption] | --adopt-existing-stopped --acknowledge-existing-stopped-adoption) [--output table|json] OPERATION_ID
  ptctl client adopt status --target PATH [--output table|json] OPERATION_ID
  ptctl client adopt prune --target PATH --expect-adoption-plan-id ID --acknowledge-operation-state-deletion [--output table|json] OPERATION_ID
  ptctl client adopt forget --target PATH --expect-adoption-plan-id ID --acknowledge-historical-evidence-deletion [--output table|json] OPERATION_ID
  ptctl client activate plan [adoption selectors] --adoption-operation ID --adoption-plan-id ID [--start-after-recheck] [--output table|json]
  ptctl client activate run [same selectors] --expect-activation-plan-id ID --acknowledge-client-recheck [--output table|json]
  ptctl client activate resume [same selectors] --expect-activation-plan-id ID [explicit acknowledgement flags] [--output table|json] OPERATION_ID
  ptctl client activate status --target PATH [--output table|json] OPERATION_ID
  ptctl client activate prune --target PATH --expect-activation-plan-id ID --acknowledge-operation-state-deletion [--output table|json] OPERATION_ID
  ptctl client activate forget --target PATH --expect-activation-plan-id ID --acknowledge-historical-evidence-deletion [--output table|json] OPERATION_ID
  ptctl client remove plan [activation/final/mapping/client selectors] [--output table|json]
  ptctl client remove run [same selectors] --expect-removal-plan-id ID --acknowledge-client-removal [--output table|json]
  ptctl client remove resume [same selectors] --expect-removal-plan-id ID [--acknowledge-client-removal --acknowledge-repeat-removal] [--output table|json] OPERATION_ID
  ptctl client remove status --target PATH --expect-removal-plan-id ID [--output table|json] OPERATION_ID
  ptctl client remove prune --target PATH --expect-removal-plan-id ID --acknowledge-operation-state-deletion [--output table|json] OPERATION_ID
  ptctl client remove forget --target PATH --expect-removal-plan-id ID --acknowledge-historical-evidence-deletion [--output table|json] OPERATION_ID

  ptctl reconcile report (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) (--source PATH | --search-root PATH... | --state-store DIR --storage-profile PROFILE | --target PATH --materialize-operation ID --materialize-plan-id ID) [--adoption-operation ID --adoption-plan-id ID] [--activation-operation ID --activation-plan-id ID] [--retirement-operation ID --retirement-plan-id ID --retirement-search-root PATH... [--parent-cleanup-operation ID --parent-cleanup-plan-id ID --parent-cleanup-search-root PATH...]] [--output table|json]

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
  * Ordinary site reads are one bounded GET per invocation. Bonus exchange is one fresh GET plus at most one acknowledged POST after a durable marker. Site requests are never automatically retried.
  * Session cookies are accepted only through stdin and are never persisted.
  * .torrent tracker URLs are reduced to origins; passkeys are never printed.
  * The metafile store preserves exact private bytes with owner-only access and atomic no-clobber commits.
  * v1, v2, and hybrid verification use exact content proofs; names and sizes are not proof.
  * Seed discovery and materialization planning have hard scan/proof budgets and perform no writes.
  * Seed materialize run/resume copy only and never clobber; prune has a separate acknowledgement and deletes only one explicit operation's private state while retaining its tombstone; forget has a third acknowledgement and irreversibly deletes only that exact tombstone plus its last recovery marker.
  * Seed retire plan performs fresh proof reads only and grants no deletion authority. Run/resume require a separate exact plan ID and deletion acknowledgement, journal every explicit name, and never remove directories, aliases, padding, empty files, or final content. Prune has its own acknowledgement and deletes only one terminal operation's private journal while retaining a tombstone; forget has a third acknowledgement and deletes only that exact tombstone plus its last recovery marker.
  * Seed retire parent-cleanup plan is zero-write. Its separately acknowledged run/resume journal and remove only reviewed same-identity empty immediate parents; they never recurse into ancestors, search roots, files, or non-empty directories. Prune replaces one terminal cleanup journal with an exact no-path tombstone; forget separately and irreversibly removes only that exact tombstone through a final root-level recovery marker.
  * Client adoption has two reviewed actions: add one absent exact-infohash job in stopped mode, or record an observation-only lineage for one already-present exact stopped job without submitting a metafile or mutating the downloader. Transmission is v1-only; a matching built-in driver can feed the separate reviewed recheck/start workflow. Adoption never rechecks, resumes, moves, or deletes content. Its separately acknowledged prune deletes only one terminal private journal after sealing an exact tombstone.
  * Client activation only rechecks or starts the reviewed exact existing job, with at most one non-retried mutation per invocation. Its separately acknowledged local prune deletes only one terminal private journal after sealing an exact tombstone and never reads a credential or contacts the client.
  * Client removal targets one reviewed typed-identity job, explicitly keeps local data, journals intent before one non-retried request, and requires exact queue absence plus final filesystem re-verification before completion.
  * Storage index snapshots are immutable candidate hints; only a same-call complete live scan can prove current uniqueness or absence.
  * Reconciliation uses one client login, two bounded job-ledger reads, at most two bounded same-job file-list reads, and no client or filesystem writes.
`)
}

func (a *app) version(args []string) error {
	fs := newFlagSet("version")
	output := fs.String("output", "table", "table or json")
	if err := fs.Parse(args); err != nil {
		return usageError("version: %v", err)
	}
	if fs.NArg() != 0 {
		return usageError("version takes no positional arguments")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	data := map[string]string{"version": Version, "commit": Commit}
	if *output == "json" {
		return writeJSON(a.stdout, data, nil)
	}
	if *output != "table" {
		return usageError("--output must be table or json")
	}
	fmt.Fprintf(a.stdout, "ptctl %s (%s)\n", terminalSafe(Version), terminalSafe(Commit))
	return nil
}

func (a *app) site(args []string) error {
	if len(args) == 0 {
		return usageError("site subcommand is required")
	}
	switch args[0] {
	case "list":
		return a.siteList(args[1:])
	case "capabilities":
		return a.siteCapabilities(args[1:])
	case "status", "account", "search", "bonus-catalog":
		return a.siteRead(args[0], args[1:])
	case "detail":
		return a.siteDetail(args[1:])
	case "bonus":
		if len(args) >= 2 {
			switch args[1] {
			case "review":
				return a.siteBonusReview(args[2:])
			case "exchange":
				return a.siteBonusExchange(args[2:])
			}
		}
		return usageError("site bonus requires review or exchange")
	case "metafile":
		if len(args) >= 2 {
			switch args[1] {
			case "fetch":
				return a.siteMetafileFetch(args[2:])
			case "binding":
				return a.siteMetafileBinding(args[2:])
			}
		}
		return usageError("site metafile requires fetch or binding")
	default:
		return usageError("unknown site subcommand %q", args[0])
	}
}

func (a *app) client(args []string) error {
	if len(args) > 0 && args[0] == "adopt" {
		return a.clientAdopt(args[1:])
	}
	if len(args) > 0 && args[0] == "activate" {
		return a.clientActivate(args[1:])
	}
	if len(args) > 0 && args[0] == "remove" {
		return a.clientRemove(args[1:])
	}
	if len(args) == 0 || (args[0] != "status" && args[0] != "list") {
		return usageError("client requires status, list, adopt, activate, or remove")
	}
	command := args[0]
	fs := newFlagSet("client " + command)
	output := fs.String("output", "table", "table or json")
	driverName := fs.String("driver", downloader.DriverQBittorrent, "read-only downloader driver: qbittorrent or transmission")
	endpoint := fs.String("url", "", "downloader API origin or RPC URL")
	username := fs.String("username", "", "downloader username")
	passwordStdin := fs.Bool("password-stdin", false, "read password from stdin")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *endpoint == "" {
		return usageError("client %s requires --url and no positional arguments", command)
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if _, ok := downloader.DescribeLedgerDriver(*driverName); !ok {
		return usageError("--driver must be qbittorrent or transmission for read-only client commands")
	}
	if !*passwordStdin {
		return usageError("--password-stdin is required; downloader passwords are never accepted in argv")
	}
	driver, err := newReadOnlyDownloaderDriver(*driverName, *endpoint)
	if err != nil {
		return err
	}
	credential, err := readDownloaderCredential(a.stdin, *username)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var data any
	if command == "status" {
		data, err = driver.Status(ctx, credential)
	} else {
		data, err = driver.Torrents(ctx, credential)
	}
	if err != nil {
		return err
	}
	if *output == "json" {
		return writeJSON(a.stdout, data, nil)
	}
	if *output != "table" {
		return usageError("--output must be table or json")
	}
	return writeClientHuman(a.stdout, command, data)
}

func (a *app) reconcileCommand(args []string) error {
	if len(args) == 0 || args[0] != "report" {
		return usageError("reconcile requires report")
	}
	return a.reconcileReport(args[1:])
}

func (a *app) reconcileReport(args []string) error {
	fs := newFlagSet("reconcile report")
	var flagOutput strings.Builder
	fs.SetOutput(&flagOutput)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  ptctl reconcile report (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) (--source PATH | --search-root PATH... | --state-store DIR --storage-profile PROFILE | --target PATH --materialize-operation ID --materialize-plan-id ID) [--adoption-operation ID --adoption-plan-id ID] [--activation-operation ID --activation-plan-id ID] [--removal-operation ID --removal-plan-id ID | --retirement-operation ID --retirement-plan-id ID --retirement-search-root PATH... [--parent-cleanup-operation ID --parent-cleanup-plan-id ID --parent-cleanup-search-root PATH...]] [flags]")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "The report can observe one live site detail page before bracketing optional downloader reads around an explicitly selected exact-layout source or bounded storage discovery. It performs zero writes. Exact --source proves only that selected layout, not filesystem-wide uniqueness. The materialize selector requires one explicit operation and reviewed plan ID, then proves its current exact final namespace before immediately repeating ordinary exact-source verification; these are sequential non-atomic observations. Optional adoption selectors read one canonical terminal client-adoption journal or retained tombstone—whether created by stopped add or observation-only existing-job adoption—and bind its historical completion to the already requested current exact typed job claim without another client request; downloader job incarnation remains unobservable. Optional activation selectors similarly bind one canonical terminal activation journal to the current bracket and, when adoption is also selected, must share that adoption lineage. Optional removal selectors instead require activation, read one canonical terminal keep-data removal journal or retained tombstone, and bind its attributed completion to typed job absence in the existing two-read client bracket. Optional retirement selectors read one canonical terminal source-retirement journal and twice reobserve its exact retired names absent under explicit source roots. Optional parent-cleanup selectors additionally require that retirement proof, read one canonical terminal cleanup journal, and twice reobserve the exact removed parent names absent; a retained cleanup tombstone is historical only and cannot restore path authority. Adoption and removal are mutually exclusive because their current-job predicates conflict; removal and source-retirement terminal modes are also mutually exclusive in this slice. A live detail page is only a current site claim for the remote ID; it cannot expose or prove the private metafile variant, and host/client path comparison remains lexical only.")
		fmt.Fprintln(fs.Output(), "With --client-file-layout=auto, an eligible multi-file torrent adds at most two bounded file-list reads for one unique exact downloader job. The reads bracket storage proof, share the command timeout, and are never retried.")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Client-only reads use --driver qbittorrent|transmission --url URL --username USER --password-stdin. Exact stopped adoption and reviewed existing-job recheck/start support both built-in drivers; Transmission mutation authority is v1-only. Site-detail-only reads use --site-ref SITE/REMOTE_ID --site-cookie-stdin. When both are requested, replace both secret flags with --credential-bundle-stdin and pipe strict JSON containing schema, site_cookie, and downloader_password.")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
	}
	output := fs.String("output", "table", "table or json")
	torrentPath := fs.String("torrent", "", "metafile path")
	storeRoot := fs.String("metafile-store", "", "private metafile store root; pair with --metafile-variant")
	variantID := fs.String("metafile-variant", "", "whole-metafile sha256 artifact ID; pair with --metafile-store")
	exactSource := fs.String("source", "", "explicit exact-layout source file or content root; mutually exclusive with discovery")
	materializeTarget := fs.String("target", "", "materialize target root; requires --materialize-operation and --materialize-plan-id")
	materializeOperationValue := fs.String("materialize-operation", "", "explicit committed or retained materialize operation ID; requires --target and --materialize-plan-id")
	materializePlanID := fs.String("materialize-plan-id", "", "reviewed materialize plan ID; requires --target and --materialize-operation")
	adoptionOperationValue := fs.String("adoption-operation", "", "explicit terminal client-adoption operation ID; requires materialized-final, client, and mapping selectors")
	adoptionPlanID := fs.String("adoption-plan-id", "", "reviewed stopped-adoption plan ID; requires --adoption-operation")
	activationOperationValue := fs.String("activation-operation", "", "explicit terminal client-activation operation ID; requires materialized-final, client, and mapping selectors")
	activationPlanID := fs.String("activation-plan-id", "", "reviewed client-activation plan ID; requires --activation-operation")
	removalOperationValue := fs.String("removal-operation", "", "explicit terminal keep-data client-removal operation ID; requires activation and materialized-final selectors")
	removalPlanID := fs.String("removal-plan-id", "", "reviewed client-removal plan ID; requires --removal-operation")
	retirementOperationValue := fs.String("retirement-operation", "", "explicit terminal source-retirement operation ID; requires activation and materialized-final selectors")
	retirementPlanID := fs.String("retirement-plan-id", "", "reviewed sha256 source-retirement plan ID; requires --retirement-operation")
	retirementAllowNetwork := fs.Bool("retirement-allow-network", false, "allow explicit network/UNC retirement source roots; never applies to the target")
	var retirementSearchRoots stringListFlag
	fs.Var(&retirementSearchRoots, "retirement-search-root", "original source root used by the retirement operation; repeatable and required with retirement selectors")
	parentCleanupOperationValue := fs.String("parent-cleanup-operation", "", "explicit terminal parent-cleanup operation ID; requires source-retirement selectors")
	parentCleanupPlanID := fs.String("parent-cleanup-plan-id", "", "reviewed sha256 parent-cleanup plan ID; requires --parent-cleanup-operation")
	parentCleanupAllowNetwork := fs.Bool("parent-cleanup-allow-network", false, "allow explicit network/UNC parent-cleanup source roots; never applies to the target")
	var parentCleanupSearchRoots stringListFlag
	fs.Var(&parentCleanupSearchRoots, "parent-cleanup-search-root", "original source root used by the parent-cleanup operation; repeatable and required with parent-cleanup selectors")
	var searchRoots stringListFlag
	fs.Var(&searchRoots, "search-root", "storage root to scan; repeatable")
	stateStore := fs.String("state-store", "", "initialized private state store; pair with --storage-profile")
	storageProfile := fs.String("storage-profile", "", "stored profile name or immutable ID; pair with --state-store")
	snapshotRecord := fs.String("snapshot-record", "", "explicit descriptor record ID for stored-profile mode")
	driverName := fs.String("driver", downloader.DriverQBittorrent, "read-only downloader driver: qbittorrent or transmission; part of the optional client group")
	endpoint := fs.String("url", "", "downloader API origin or RPC URL; part of the optional client group")
	username := fs.String("username", "", "downloader username; part of the optional client group")
	passwordStdin := fs.Bool("password-stdin", false, "read downloader password from stdin; part of the optional client group")
	siteCookieStdin := fs.Bool("site-cookie-stdin", false, "read a site Cookie header from stdin and observe --site-ref live")
	credentialBundleStdin := fs.Bool("credential-bundle-stdin", false, "read strict JSON containing site_cookie and downloader_password when live site and client reads are both requested")
	clientFileLayout := fs.String("client-file-layout", "auto", "multi-file downloader layout reads: auto or off; requires the client group when explicit")
	clientFileDefaults := downloader.DefaultJobFileLedgerLimits()
	maxClientFiles := fs.Int("max-client-files", clientFileDefaults.MaxFiles, "maximum downloader files read for the one exact job; requires the client group when explicit")
	maxClientFilePathBytes := fs.Int64("max-client-file-path-bytes", clientFileDefaults.MaxPathBytes, "maximum cumulative downloader file-path bytes per read; requires the client group when explicit")
	maxClientFileResponseBytes := fs.Int64("max-client-file-response-bytes", clientFileDefaults.MaxResponseBytes, "maximum downloader file-list response bytes per read; requires the client group when explicit")
	hostRoot := fs.String("host-root", "", "optional host namespace root paired with --client-root")
	clientRoot := fs.String("client-root", "", "optional downloader namespace root paired with --host-root")
	clientStyle := fs.String("client-style", "posix", "downloader path style: posix or windows; requires host/client roots")
	siteRefValue := fs.String("site-ref", "", "optional user-declared SITE/REMOTE_ID reference")
	siteBindingRecordValue := fs.String("site-binding-record", "", "explicit sealed site-binding record ID; requires the stored metafile selector")
	showAbsolute := fs.Bool("show-absolute-paths", false, "include absolute host and downloader paths in output")
	allowNetwork := fs.Bool("allow-network", false, "allow explicit network/UNC search roots")
	timeout := fs.Duration("timeout", time.Hour, "shared site, downloader, scan, and verification wall-clock budget")
	requireReconciled := fs.Bool("require-reconciled", false, "exit 4 after the report unless outcome is consistent")

	inventoryDefaults := storage.DefaultInventoryLimits()
	maxDepth := fs.Int("max-depth", inventoryDefaults.MaxDepth, "maximum directory depth")
	maxDirectories := fs.Int("max-directories", inventoryDefaults.MaxDirectories, "maximum directories opened")
	maxEntries := fs.Int("max-entries", inventoryDefaults.MaxEntries, "maximum directory entries examined")
	maxDirectoryEntries := fs.Int("max-directory-entries", inventoryDefaults.MaxEntriesPerDirectory, "maximum entries accepted from one directory")
	maxCandidates := fs.Int("max-candidates", inventoryDefaults.MaxCandidates, "maximum matching regular files retained")
	maxPathBytes := fs.Int64("max-path-bytes", inventoryDefaults.MaxPathBytes, "maximum retained relative-path bytes")

	matchDefaults := metafile.DefaultSourceMatchLimits()
	maxCandidatesPerFile := fs.Int("max-candidates-per-file", matchDefaults.MaxCandidatesPerFile, "maximum candidates explored for one torrent file")
	maxCandidateEdges := fs.Int("max-candidate-edges", matchDefaults.MaxCandidateEdges, "maximum manifest-file to source-candidate edges considered")
	maxStates := fs.Int("max-states", matchDefaults.MaxStates, "maximum candidate assignment states")
	maxVerifiedLayouts := fs.Int("max-verified-layouts", matchDefaults.MaxVerifiedLayouts, "maximum verified alternatives retained")
	maxProofBytes := fs.Int64("max-proof-bytes", matchDefaults.MaxProofWorkBytes, "maximum physical and virtual bytes charged to proof work")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(a.stdout, flagOutput.String())
			return nil
		}
		detail := strings.TrimSpace(flagOutput.String())
		if detail == "" {
			detail = err.Error()
		}
		return usageError("reconcile report: %s", detail)
	}
	if fs.NArg() != 0 {
		return usageError("reconcile report accepts flags only; unexpected argument %q", fs.Arg(0))
	}
	explicit := make(map[string]bool)
	fs.Visit(func(item *flag.Flag) { explicit[item.Name] = true })
	exactRequested := explicit["source"]
	indexedRequested := explicit["state-store"] || explicit["storage-profile"] || explicit["snapshot-record"]
	materializedRequested := explicit["target"] || explicit["materialize-operation"] || explicit["materialize-plan-id"]
	adoptionRequested := explicit["adoption-operation"] || explicit["adoption-plan-id"]
	activationRequested := explicit["activation-operation"] || explicit["activation-plan-id"]
	removalRequested := explicit["removal-operation"] || explicit["removal-plan-id"]
	retirementRequested := explicit["retirement-operation"] || explicit["retirement-plan-id"] || explicit["retirement-search-root"] || explicit["retirement-allow-network"]
	parentCleanupRequested := explicit["parent-cleanup-operation"] || explicit["parent-cleanup-plan-id"] || explicit["parent-cleanup-search-root"] || explicit["parent-cleanup-allow-network"]
	if exactRequested && *exactSource == "" {
		return usageError("reconcile report requires --source to be non-empty")
	}
	if materializedRequested && (!explicit["target"] || !explicit["materialize-operation"] || !explicit["materialize-plan-id"] || *materializeTarget == "" || *materializeOperationValue == "" || *materializePlanID == "") {
		return usageError("materialized-final mode requires non-empty --target, --materialize-operation, and --materialize-plan-id")
	}
	if adoptionRequested && (!explicit["adoption-operation"] || !explicit["adoption-plan-id"] || *adoptionOperationValue == "" || *adoptionPlanID == "") {
		return usageError("client-adoption mode requires non-empty --adoption-operation and --adoption-plan-id")
	}
	if activationRequested && (!explicit["activation-operation"] || !explicit["activation-plan-id"] || *activationOperationValue == "" || *activationPlanID == "") {
		return usageError("client-activation mode requires non-empty --activation-operation and --activation-plan-id")
	}
	if removalRequested && (!explicit["removal-operation"] || !explicit["removal-plan-id"] || *removalOperationValue == "" || *removalPlanID == "") {
		return usageError("client-removal mode requires non-empty --removal-operation and --removal-plan-id")
	}
	if removalRequested && retirementRequested {
		return usageError("client-removal and source-retirement reconciliation modes are mutually exclusive")
	}
	if adoptionRequested && removalRequested {
		return usageError("client-adoption and client-removal reconciliation modes are mutually exclusive")
	}
	if retirementRequested && (!explicit["retirement-operation"] || !explicit["retirement-plan-id"] || !explicit["retirement-search-root"] ||
		*retirementOperationValue == "" || *retirementPlanID == "" || len(retirementSearchRoots) == 0) {
		return usageError("source-retirement mode requires non-empty --retirement-operation, --retirement-plan-id, and at least one --retirement-search-root")
	}
	if len(retirementSearchRoots) > 64 {
		return usageError("reconcile report accepts at most 64 --retirement-search-root values")
	}
	if parentCleanupRequested && (!explicit["parent-cleanup-operation"] || !explicit["parent-cleanup-plan-id"] || !explicit["parent-cleanup-search-root"] ||
		*parentCleanupOperationValue == "" || *parentCleanupPlanID == "" || len(parentCleanupSearchRoots) == 0) {
		return usageError("parent-cleanup mode requires non-empty --parent-cleanup-operation, --parent-cleanup-plan-id, and at least one --parent-cleanup-search-root")
	}
	if len(parentCleanupSearchRoots) > 64 {
		return usageError("reconcile report accepts at most 64 --parent-cleanup-search-root values")
	}
	if !exactRequested && len(searchRoots) == 0 && !indexedRequested && !materializedRequested {
		return usageError("reconcile report requires --source, --search-root, stored-profile mode, or the complete materialized-final selector")
	}
	sourceModes := 0
	if exactRequested {
		sourceModes++
	}
	if len(searchRoots) > 0 {
		sourceModes++
	}
	if indexedRequested {
		sourceModes++
	}
	if materializedRequested {
		sourceModes++
	}
	if sourceModes > 1 {
		return usageError("--source, --search-root, stored-profile index mode, and materialized-final mode are mutually exclusive")
	}
	if indexedRequested && (*stateStore == "" || *storageProfile == "") {
		return usageError("stored-profile mode requires non-empty --state-store and --storage-profile")
	}
	if indexedRequested && explicit["allow-network"] {
		return usageError("--allow-network is fixed by the immutable storage profile in stored-profile mode")
	}
	for _, name := range []string{"max-depth", "max-directories", "max-entries", "max-directory-entries"} {
		if indexedRequested && explicit[name] {
			return usageError("--%s applies only to live --search-root scanning; refresh limits are fixed by the storage profile", name)
		}
	}
	if exactRequested || materializedRequested {
		for _, name := range []string{"allow-network", "max-depth", "max-directories", "max-entries", "max-directory-entries", "max-candidates", "max-path-bytes", "max-candidates-per-file", "max-candidate-edges", "max-states", "max-verified-layouts", "max-proof-bytes"} {
			if explicit[name] {
				return usageError("--%s applies only to discovery source modes", name)
			}
		}
	}
	var materializeOperation materialize.OperationID
	if materializedRequested {
		parsedOperation, parseErr := materialize.ParseOperationID(*materializeOperationValue)
		if parseErr != nil {
			return usageError("--materialize-operation requires a canonical operation ID")
		}
		if !validMaterializePlanID(*materializePlanID) {
			return usageError("--materialize-plan-id requires a canonical reviewed plan ID")
		}
		materializeOperation = parsedOperation
	}
	var adoptionOperation clientadopt.OperationID
	if adoptionRequested {
		parsedOperation, parseErr := clientadopt.ParseOperationID(*adoptionOperationValue)
		if parseErr != nil {
			return usageError("--adoption-operation requires a canonical operation ID")
		}
		if !validMaterializePlanID(*adoptionPlanID) {
			return usageError("--adoption-plan-id requires a canonical reviewed plan ID")
		}
		if clientadopt.OperationIDForPlan(*adoptionPlanID) != parsedOperation {
			return usageError("--adoption-operation does not match --adoption-plan-id")
		}
		adoptionOperation = parsedOperation
	}
	var activationOperation clientactivate.OperationID
	if activationRequested {
		parsedOperation, parseErr := clientactivate.ParseOperationID(*activationOperationValue)
		if parseErr != nil {
			return usageError("--activation-operation requires a canonical operation ID")
		}
		if !validMaterializePlanID(*activationPlanID) {
			return usageError("--activation-plan-id requires a canonical reviewed plan ID")
		}
		activationOperation = parsedOperation
	}
	var removalOperation clientremove.OperationID
	if removalRequested {
		parsedOperation, parseErr := clientremove.ParseOperationID(*removalOperationValue)
		if parseErr != nil {
			return usageError("--removal-operation requires a canonical operation ID")
		}
		if !validMaterializePlanID(*removalPlanID) {
			return usageError("--removal-plan-id requires a canonical reviewed plan ID")
		}
		if clientremove.OperationIDForPlan(*removalPlanID) != parsedOperation {
			return usageError("--removal-operation does not match --removal-plan-id")
		}
		removalOperation = parsedOperation
	}
	var retirementOperation sourceretire.OperationID
	if retirementRequested {
		parsedOperation, parseErr := sourceretire.ParseOperationID(*retirementOperationValue)
		if parseErr != nil {
			return usageError("--retirement-operation requires a canonical operation ID")
		}
		if !validSourceRetireExecutionPlanID(*retirementPlanID) {
			return usageError("--retirement-plan-id requires a canonical reviewed sha256 plan ID")
		}
		derivedOperation, deriveErr := sourceretire.OperationIDForPlanID(*retirementPlanID)
		if deriveErr != nil || derivedOperation != parsedOperation {
			return usageError("--retirement-operation does not match --retirement-plan-id")
		}
		retirementOperation = parsedOperation
	}
	var parentCleanupOperation sourceretire.ParentCleanupOperationID
	if parentCleanupRequested {
		parsedOperation, parseErr := sourceretire.ParseParentCleanupOperationID(*parentCleanupOperationValue)
		if parseErr != nil {
			return usageError("--parent-cleanup-operation requires a canonical operation ID")
		}
		if !validSourceRetireExecutionPlanID(*parentCleanupPlanID) {
			return usageError("--parent-cleanup-plan-id requires a canonical reviewed sha256 plan ID")
		}
		derivedOperation, deriveErr := sourceretire.ParentCleanupOperationIDForPlanID(*parentCleanupPlanID)
		if deriveErr != nil || derivedOperation != parsedOperation {
			return usageError("--parent-cleanup-operation does not match --parent-cleanup-plan-id")
		}
		parentCleanupOperation = parsedOperation
	}
	var explicitDescriptor metastore.RecordID
	if explicit["snapshot-record"] {
		parsedDescriptor, parseErr := metastore.ParseRecordID(*snapshotRecord)
		if parseErr != nil {
			return usageError("--snapshot-record is invalid")
		}
		explicitDescriptor = parsedDescriptor
	}
	input, err := flaggedMetafileInput("reconcile report", *torrentPath, *storeRoot, *variantID, explicit["torrent"], explicit["metafile-store"], explicit["metafile-variant"])
	if err != nil {
		return err
	}
	var siteBindingRecordID metastore.RecordID
	if explicit["site-binding-record"] {
		if input.storeRoot == "" {
			return usageError("--site-binding-record requires --metafile-store and --metafile-variant")
		}
		parsedRecordID, parseErr := metastore.ParseRecordID(*siteBindingRecordValue)
		if parseErr != nil {
			return usageError("--site-binding-record requires a canonical sealed record ID")
		}
		siteBindingRecordID = parsedRecordID
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > 7*24*time.Hour {
		return usageError("--timeout must be greater than zero and no more than 168h")
	}

	clientConnectionFlagNames := []string{"driver", "url", "username"}
	clientRequested := false
	for _, name := range append(append([]string{}, clientConnectionFlagNames...), "password-stdin", "credential-bundle-stdin") {
		clientRequested = clientRequested || explicit[name]
	}
	siteDetailRequested := explicit["site-cookie-stdin"] || explicit["credential-bundle-stdin"]
	if explicit["password-stdin"] && !*passwordStdin || explicit["site-cookie-stdin"] && !*siteCookieStdin || explicit["credential-bundle-stdin"] && !*credentialBundleStdin {
		return usageError("secret stdin flags cannot be explicitly disabled")
	}
	if explicit["credential-bundle-stdin"] && (explicit["password-stdin"] || explicit["site-cookie-stdin"]) {
		return usageError("--credential-bundle-stdin is mutually exclusive with --password-stdin and --site-cookie-stdin")
	}
	if clientRequested {
		missing := make([]string, 0, len(clientConnectionFlagNames))
		for _, name := range clientConnectionFlagNames {
			if !explicit[name] {
				missing = append(missing, "--"+name)
			}
		}
		if len(missing) > 0 {
			return usageError("the optional client group requires %s", strings.Join(missing, ", "))
		}
		if _, ok := downloader.DescribeLedgerDriver(*driverName); !ok {
			return usageError("--driver must be qbittorrent or transmission for reconciliation")
		}
		if *endpoint == "" || *username == "" {
			return usageError("the optional client group requires non-empty --url and --username")
		}
		if siteDetailRequested {
			if !explicit["credential-bundle-stdin"] {
				return usageError("combined live site and downloader reads require --credential-bundle-stdin")
			}
		} else if !explicit["password-stdin"] {
			return usageError("client-only reconciliation requires --password-stdin")
		}
	}
	if siteDetailRequested && !clientRequested && !explicit["site-cookie-stdin"] {
		return usageError("site-detail-only reconciliation requires --site-cookie-stdin")
	}
	clientFileFlagNames := []string{"client-file-layout", "max-client-files", "max-client-file-path-bytes", "max-client-file-response-bytes"}
	for _, name := range clientFileFlagNames {
		if explicit[name] && !clientRequested {
			return usageError("--%s requires the complete optional client group", name)
		}
	}
	if *clientFileLayout != "auto" && *clientFileLayout != "off" {
		return usageError("--client-file-layout must be auto or off")
	}
	clientFileLimits := clientFileDefaults
	clientFileLimits.MaxFiles = *maxClientFiles
	clientFileLimits.MaxPathBytes = *maxClientFilePathBytes
	clientFileLimits.MaxResponseBytes = *maxClientFileResponseBytes
	if err := clientFileLimits.Validate(); err != nil {
		return usageError("reconcile report: %v", err)
	}

	mappingRootsRequested := explicit["host-root"] || explicit["client-root"] || *hostRoot != "" || *clientRoot != ""
	if (*hostRoot == "") != (*clientRoot == "") || mappingRootsRequested && (*hostRoot == "" || *clientRoot == "") {
		return usageError("--host-root and --client-root must be provided together")
	}
	if explicit["client-style"] && !mappingRootsRequested {
		return usageError("--client-style requires --host-root and --client-root")
	}
	if *clientStyle != "posix" && *clientStyle != "windows" {
		return usageError("--client-style must be posix or windows")
	}
	if explicit["site-ref"] && *siteRefValue == "" {
		return usageError("--site-ref must be exactly SITE/REMOTE_ID")
	}

	siteRef, err := parseSiteRef(*siteRefValue)
	if err != nil {
		return usageError("reconcile report: %v", err)
	}
	if siteDetailRequested && siteRef == nil {
		return usageError("live site detail reconciliation requires --site-ref SITE/REMOTE_ID")
	}
	inventoryLimits := inventoryDefaults
	inventoryLimits.MaxDepth = *maxDepth
	inventoryLimits.MaxDirectories = *maxDirectories
	inventoryLimits.MaxEntries = *maxEntries
	inventoryLimits.MaxEntriesPerDirectory = *maxDirectoryEntries
	inventoryLimits.MaxCandidates = *maxCandidates
	inventoryLimits.MaxPathBytes = *maxPathBytes
	matchLimits := matchDefaults
	matchLimits.MaxCandidatesPerFile = *maxCandidatesPerFile
	matchLimits.MaxCandidateEdges = *maxCandidateEdges
	matchLimits.MaxStates = *maxStates
	matchLimits.MaxVerifiedLayouts = *maxVerifiedLayouts
	matchLimits.MaxProofWorkBytes = *maxProofBytes
	if err := inventoryLimits.Validate(); err != nil {
		return usageError("reconcile report: %v", err)
	}
	if !indexedRequested && !exactRequested && !materializedRequested && len(searchRoots) > inventoryLimits.MaxRoots {
		return usageError("reconcile report accepts at most %d --search-root values", inventoryLimits.MaxRoots)
	}
	if err := matchLimits.Validate(); err != nil {
		return usageError("reconcile report: %v", err)
	}
	if mappingRootsRequested {
		if err := storage.ValidatePathMappingConfig(*hostRoot, *clientRoot, *clientStyle == "windows"); err != nil {
			return usageError("reconcile report path mapping is invalid: %v", err)
		}
	}
	if activationRequested {
		if !materializedRequested || !clientRequested || !mappingRootsRequested {
			return usageError("client-activation reconciliation requires the complete materialized-final, client, and host/client mapping selectors")
		}
		if *clientFileLayout != "auto" || clientFileLimits != downloader.DefaultJobFileLedgerLimits() {
			return usageError("client-activation reconciliation requires --client-file-layout=auto and the default bounded client file limits")
		}
	}
	if adoptionRequested {
		if !materializedRequested || !clientRequested || !mappingRootsRequested {
			return usageError("client-adoption reconciliation requires the complete materialized-final, client, and host/client mapping selectors")
		}
		if *clientFileLayout != "auto" || clientFileLimits != downloader.DefaultJobFileLedgerLimits() {
			return usageError("client-adoption reconciliation requires --client-file-layout=auto and the default bounded client file limits")
		}
	}
	if removalRequested {
		if !activationRequested || !materializedRequested || !clientRequested || !mappingRootsRequested {
			return usageError("client-removal reconciliation requires the complete activation, materialized-final, client, and host/client mapping selectors")
		}
		if *clientFileLayout != "auto" || clientFileLimits != downloader.DefaultJobFileLedgerLimits() {
			return usageError("client-removal reconciliation requires --client-file-layout=auto and the default bounded client file limits")
		}
	}
	if retirementRequested {
		if !activationRequested || !materializedRequested || !clientRequested || !mappingRootsRequested {
			return usageError("source-retirement reconciliation requires the complete activation, materialized-final, client, and host/client mapping selectors")
		}
		if *clientFileLayout != "auto" || clientFileLimits != downloader.DefaultJobFileLedgerLimits() {
			return usageError("source-retirement reconciliation requires --client-file-layout=auto and the default bounded client file limits")
		}
	}
	if parentCleanupRequested && !retirementRequested {
		return usageError("parent-cleanup reconciliation requires the complete source-retirement selectors")
	}

	var clientAdapter downloader.LedgerDriver
	var clientConfigID string
	if clientRequested {
		clientAdapter, err = newReadOnlyDownloaderDriver(*driverName, *endpoint)
		if err != nil {
			return usageError("reconcile report downloader endpoint is invalid: %v", err)
		}
		if adoptionRequested || activationRequested {
			configured, ok := clientAdapter.(configuredLedgerDriver)
			if !ok {
				return usageError("reconcile report downloader configuration identity is unavailable")
			}
			clientConfigID, err = configured.ClientConfigID(*username)
			if err != nil {
				return usageError("reconcile report downloader configuration is invalid")
			}
		}
	}
	var siteDetailReader site.TorrentDetailReader
	var siteDetailConfig site.TorrentDetailConfig
	if siteDetailRequested {
		adapter, ok := a.registry.Get(siteRef.SiteID)
		if !ok {
			return usageError("live site detail adapter is unavailable")
		}
		descriptor := adapter.Descriptor()
		if !descriptor.Supports(domain.CapabilityDetail) || !descriptor.SupportsAuth(domain.AuthMethodCookieHeader) {
			return usageError("site %q does not support authenticated torrent detail reads", descriptor.ID)
		}
		siteDetailReader, ok = adapter.(site.TorrentDetailReader)
		if !ok {
			return usageError("site %q does not implement its declared torrent detail capability", descriptor.ID)
		}
		siteDetailConfig, err = siteDetailReader.TorrentDetailConfig()
		if err != nil || siteDetailConfig.Validate() != nil {
			return usageError("site torrent detail configuration is unavailable")
		}
		if err := siteDetailReader.ValidateTorrentDetailRef(*siteRef); err != nil {
			return usageError("live site detail reference is invalid")
		}
	}
	meta, err := loadMetafileInput(context.Background(), input)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	adoptionSelection := reconcile.ClientAdoptionSelection{Requested: adoptionRequested}
	var verifiedAdoption *clientadopt.VerifiedCompletion
	adoptionGateFailed := false
	adoptionIntegrityFailed := false
	if adoptionRequested {
		adoptionSelection.CompletionAttempted = true
		verified, _, completionErr := clientadopt.VerifyCompletion(ctx, clientadopt.CompletionProofOptions{
			TargetRoot: *materializeTarget, OperationID: adoptionOperation, ExpectedPlanID: *adoptionPlanID,
		})
		if completionErr != nil {
			adoptionSelection.StopReason = reconciliationAdoptionStopReason(ctx, completionErr)
			adoptionGateFailed = true
			adoptionIntegrityFailed = adoptionSelection.StopReason == "adoption_completion_integrity_failed"
		} else {
			plan := verified.Plan()
			semantics := "posix_exact"
			if *clientStyle == "windows" {
				semantics = "windows_exact"
			}
			mappingID := reconcile.PathMappingFingerprint(reconcile.PathMappingOptions{
				HostRoot: *hostRoot, ClientRoot: *clientRoot, ClientWindows: *clientStyle == "windows",
			})
			if plan.Driver != *driverName || plan.ClientConfigID != clientConfigID || plan.PathMappingID != mappingID ||
				plan.ClientPathSemantics != semantics || plan.MetafileVariantID != meta.MetafileVariantID ||
				plan.InfoHashV1 != meta.InfoHashV1 || plan.InfoHashV2 != meta.InfoHashV2 ||
				plan.MaterializeOperationID != materializeOperation.String() || plan.MaterializePlanID != *materializePlanID {
				adoptionSelection.StopReason = "adoption_completion_selector_mismatch"
				adoptionGateFailed = true
			} else {
				verifiedAdoption = verified
				adoptionSelection.Completion = verified
				adoptionSelection.CurrentJob = verified
			}
		}
	}
	activationSelection := reconcile.ClientActivationSelection{Requested: activationRequested}
	var verifiedActivation *clientactivate.VerifiedCompletion
	activationGateFailed := false
	activationIntegrityFailed := false
	if activationRequested {
		verified, _, completionErr := clientactivate.VerifyCompletion(ctx, clientactivate.CompletionProofOptions{
			TargetRoot: *materializeTarget, OperationID: activationOperation, ExpectedPlanID: *activationPlanID,
		})
		if completionErr != nil {
			activationSelection.StopReason = reconciliationActivationStopReason(ctx, completionErr)
			activationGateFailed = true
			activationIntegrityFailed = activationSelection.StopReason == "activation_completion_integrity_failed"
		} else {
			plan := verified.Plan()
			semantics := "posix_exact"
			if *clientStyle == "windows" {
				semantics = "windows_exact"
			}
			mappingID := reconcile.PathMappingFingerprint(reconcile.PathMappingOptions{
				HostRoot: *hostRoot, ClientRoot: *clientRoot, ClientWindows: *clientStyle == "windows",
			})
			if plan.Driver != *driverName || plan.ClientConfigID != clientConfigID || plan.PathMappingID != mappingID ||
				plan.ClientPathSemantics != semantics || plan.FileLimits != clientFileLimits ||
				plan.MetafileVariantID != meta.MetafileVariantID || plan.InfoHashV1 != meta.InfoHashV1 || plan.InfoHashV2 != meta.InfoHashV2 ||
				plan.MaterializeOperationID != materializeOperation.String() || plan.MaterializePlanID != *materializePlanID {
				activationSelection.StopReason = "activation_completion_selector_mismatch"
				activationGateFailed = true
			} else if verifiedAdoption != nil {
				adoptionObservation := verifiedAdoption.Observation()
				if plan.AdoptionOperationID != adoptionObservation.OperationID || plan.AdoptionPlanID != adoptionObservation.PlanID ||
					plan.AdoptionCompletionID != adoptionObservation.CompletionID {
					activationSelection.StopReason = "activation_completion_selector_mismatch"
					activationGateFailed = true
				} else {
					verifiedActivation = verified
					activationSelection.Completion = verified
				}
			} else {
				verifiedActivation = verified
				activationSelection.Completion = verified
			}
		}
	}
	removalSelection := reconcile.ClientRemovalSelection{Requested: removalRequested}
	var verifiedRemoval *clientremove.VerifiedCompletion
	removalGateFailed := false
	removalIntegrityFailed := false
	if removalRequested {
		if activationGateFailed || verifiedActivation == nil {
			removalSelection.StopReason = "removal_prerequisite_unavailable"
			removalGateFailed = true
		} else {
			removalSelection.CompletionAttempted = true
			verified, observation, completionErr := clientremove.VerifyCompletion(ctx, clientremove.CompletionProofOptions{
				TargetRoot: *materializeTarget, OperationID: removalOperation, ExpectedPlanID: *removalPlanID,
			})
			if completionErr != nil {
				removalSelection.StopReason = reconciliationRemovalStopReason(ctx, completionErr)
				removalGateFailed = true
				removalIntegrityFailed = removalSelection.StopReason == "removal_completion_integrity_failed"
			} else {
				removalSelection.Completion = verified
				verifiedRemoval = verified
				activationObservation := verifiedActivation.Observation()
				plan := verified.Plan()
				mappingID := reconcile.PathMappingFingerprint(reconcile.PathMappingOptions{
					HostRoot: *hostRoot, ClientRoot: *clientRoot, ClientWindows: *clientStyle == "windows",
				})
				if observation.MetafileVariantID != meta.MetafileVariantID || plan.Driver != *driverName ||
					plan.ClientConfigID != clientConfigID || plan.PathMappingID != mappingID || plan.FileLimits != clientFileLimits ||
					plan.MaterializeOperationID != materializeOperation.String() || plan.MaterializePlanID != *materializePlanID ||
					plan.ActivationOperationID != activationOperation.String() || plan.ActivationPlanID != *activationPlanID ||
					plan.ActivationTerminalID != activationObservation.TerminalMarkerID {
					removalSelection.StopReason = "removal_completion_selector_mismatch"
					removalGateFailed = true
				} else if observation.CompletionBasis != "accepted_response_then_exact_absence" {
					removalSelection.StopReason = "removal_absence_causality_unproven"
					removalGateFailed = true
				}
			}
		}
	}
	retirementSelection := reconcile.SourceRetirementSelection{Requested: retirementRequested}
	var verifiedRetirement *sourceretire.VerifiedCompletion
	retirementGateFailed := false
	retirementCompletionGateFailed := false
	retirementIntegrityFailed := false
	if retirementRequested {
		if activationGateFailed || verifiedActivation == nil {
			retirementSelection.StopReason = "retirement_prerequisite_unavailable"
			retirementGateFailed = true
			retirementCompletionGateFailed = true
		} else {
			retirementSelection.CompletionAttempted = true
			verified, observation, completionErr := sourceretire.VerifyCompletion(ctx, sourceretire.CompletionProofOptions{
				TargetRoot: *materializeTarget, OperationID: retirementOperation, ExpectedPlanID: *retirementPlanID,
				Limits: sourceretire.DefaultExecutionLimits(),
			})
			if completionErr != nil {
				retirementSelection.StopReason = reconciliationRetirementCompletionStopReason(ctx, completionErr)
				retirementGateFailed = true
				retirementCompletionGateFailed = true
				retirementIntegrityFailed = retirementSelection.StopReason == "retirement_completion_integrity_failed"
			} else {
				retirementSelection.Completion = verified
				verifiedRetirement = verified
				activationObservation := verifiedActivation.Observation()
				if observation.MetafileVariantID != meta.MetafileVariantID ||
					observation.MaterializeOperationID != materializeOperation.String() || observation.MaterializePlanID != *materializePlanID ||
					observation.ActivationOperationID != activationOperation.String() || observation.ActivationPlanID != *activationPlanID ||
					observation.ClientCompletionID != activationObservation.TerminalMarkerID {
					retirementSelection.StopReason = "retirement_completion_selector_mismatch"
					retirementGateFailed = true
					retirementCompletionGateFailed = true
				} else if observation.RetainedTombstone {
					retirementSelection.StopReason = "retirement_current_absence_unavailable"
					retirementGateFailed = true
				} else {
					retirementSelection.AbsenceAttempted = true
					absence, _, absenceErr := sourceretire.VerifyCurrentAbsence(ctx, verified, append([]string(nil), retirementSearchRoots...), *retirementAllowNetwork)
					if absenceErr != nil {
						retirementSelection.StopReason = reconciliationRetirementAbsenceStopReason(ctx, absenceErr)
						retirementGateFailed = true
					} else {
						retirementSelection.CurrentAbsence = absence
					}
				}
			}
		}
	}
	parentCleanupSelection := reconcile.ParentCleanupSelection{Requested: parentCleanupRequested}
	parentCleanupGateFailed := false
	parentCleanupIntegrityFailed := false
	if parentCleanupRequested {
		if retirementCompletionGateFailed || verifiedRetirement == nil {
			parentCleanupSelection.StopReason = "parent_cleanup_prerequisite_unavailable"
			parentCleanupGateFailed = true
		} else {
			parentCleanupSelection.CompletionAttempted = true
			verified, observation, completionErr := sourceretire.VerifyParentCleanupCompletion(ctx, sourceretire.ParentCleanupCompletionProofOptions{
				TargetRoot: *materializeTarget, OperationID: parentCleanupOperation, ExpectedCleanupPlanID: *parentCleanupPlanID,
				Limits: sourceretire.DefaultParentCleanupExecutionLimits(),
			})
			if completionErr != nil {
				parentCleanupSelection.StopReason = reconciliationParentCleanupCompletionStopReason(ctx, completionErr)
				parentCleanupGateFailed = true
				parentCleanupIntegrityFailed = parentCleanupSelection.StopReason == "parent_cleanup_completion_integrity_failed"
			} else {
				parentCleanupSelection.Completion = verified
				retirementObservation := verifiedRetirement.Observation()
				if observation.RetirementOperationID != retirementOperation.String() ||
					observation.RetirementPlanID != *retirementPlanID ||
					observation.RetirementCompletionID != retirementObservation.CompletionID ||
					observation.SearchScopeID != retirementObservation.SearchScopeID ||
					observation.TargetRootIdentity != retirementObservation.TargetRootIdentity {
					parentCleanupSelection.StopReason = "parent_cleanup_completion_selector_mismatch"
					parentCleanupGateFailed = true
				} else if observation.RetainedTombstone {
					parentCleanupSelection.StopReason = "parent_cleanup_current_absence_unavailable"
					parentCleanupGateFailed = true
				} else {
					parentCleanupSelection.AbsenceAttempted = true
					absence, _, absenceErr := sourceretire.VerifyCurrentParentCleanupAbsence(ctx, verified, append([]string(nil), parentCleanupSearchRoots...), *parentCleanupAllowNetwork)
					if absenceErr != nil {
						parentCleanupSelection.StopReason = reconciliationParentCleanupAbsenceStopReason(ctx, absenceErr)
						parentCleanupGateFailed = true
					} else {
						parentCleanupSelection.CurrentAbsence = absence
						if retirementSelection.CurrentAbsence == nil {
							derivedAbsence, _, deriveErr := sourceretire.BindCurrentRetiredNameAbsenceFromParentCleanup(verifiedRetirement, verified, absence)
							if deriveErr != nil {
								parentCleanupSelection.StopReason = "parent_cleanup_current_absence_unavailable"
								parentCleanupGateFailed = true
							} else {
								retirementSelection.CurrentAbsence = derivedAbsence
								retirementSelection.StopReason = ""
								retirementGateFailed = false
							}
						}
					}
				}
			}
		}
	}
	var indexedRepository *storageindex.Repository
	var indexedProfile storageindex.Profile
	var indexedDescriptorID metastore.RecordID
	if indexedRequested {
		store, openErr := metastore.Open(*stateStore)
		if openErr != nil {
			return openErr
		}
		indexedRepository, err = storageindex.NewRepository(store, storageindex.DefaultLimits())
		if err != nil {
			return err
		}
		profileSelection, selectionErr := indexedRepository.SelectProfile(ctx, *storageProfile)
		if selectionErr != nil {
			return selectionErr
		}
		if liveErr := storageindex.ValidateProfileForLiveUse(profileSelection.Profile, storageindex.DefaultLimits()); liveErr != nil {
			return liveErr
		}
		snapshotSelection, selectionErr := indexedRepository.SelectSnapshot(ctx, profileSelection.Profile, explicitDescriptor)
		if selectionErr != nil {
			return selectionErr
		}
		indexedProfile = profileSelection.Profile
		indexedDescriptorID = snapshotSelection.DescriptorRecordID
	}
	siteSelection := reconcile.SiteBindingSelection{Requested: explicit["site-binding-record"], RecordID: siteBindingRecordID}
	siteBindingGateFailed := false
	siteBindingIntegrityFailed := false
	if siteSelection.Requested {
		bindingStore, openErr := metastore.Open(input.storeRoot)
		if openErr != nil {
			siteSelection.StopReason = "site_binding_load_failed"
			siteBindingGateFailed = true
		} else {
			bindingRepository, repositoryErr := sitebinding.NewRepository(bindingStore, sitebinding.DefaultLimits())
			if repositoryErr != nil {
				siteSelection.StopReason = "site_binding_load_failed"
				siteBindingGateFailed = true
			} else {
				verifiedBinding, _, loadErr := bindingRepository.Load(ctx, siteBindingRecordID)
				if loadErr != nil {
					siteSelection.StopReason = siteBindingLoadStopReason(loadErr)
					siteBindingGateFailed = true
					siteBindingIntegrityFailed = siteSelection.StopReason == "site_binding_integrity_failed"
				} else if adapterErr := validateSiteBindingAdapter(a.registry, verifiedBinding); adapterErr != nil {
					siteSelection.StopReason = "site_binding_adapter_mismatch"
					siteBindingGateFailed = true
				} else {
					siteSelection.Verified = verifiedBinding
					public := verifiedBinding.PublicCopy()
					boundRef := domain.TorrentRef{SiteID: public.Record.SiteID, RemoteID: public.Record.RemoteID}
					if public.Record.MetafileVariantID != meta.MetafileVariantID || siteRef != nil && *siteRef != boundRef {
						siteBindingGateFailed = true
					}
				}
			}
		}
	}
	var siteCredential site.Credential
	var clientCredential downloader.Credential
	if !siteBindingGateFailed && !adoptionGateFailed && !activationGateFailed && !removalGateFailed && !retirementGateFailed && !parentCleanupGateFailed {
		switch {
		case siteDetailRequested && clientRequested:
			siteCredential, clientCredential, err = readReconciliationCredentialBundle(a.stdin, *username)
		case siteDetailRequested:
			siteCredential, err = readCredential(a.stdin)
		case clientRequested:
			clientCredential, err = readDownloaderCredential(a.stdin, *username)
		}
		if err != nil {
			return err
		}
	}
	siteDetailSelection := reconcile.SiteDetailSelection{Requested: siteDetailRequested, Config: siteDetailConfig}
	if siteDetailRequested {
		if siteBindingGateFailed {
			siteDetailSelection.StopReason = "site_detail_skipped_by_binding_gate"
		} else if adoptionGateFailed || activationGateFailed || removalGateFailed || retirementGateFailed || parentCleanupGateFailed {
			siteDetailSelection.StopReason = "site_detail_skipped_by_prerequisite_gate"
		} else {
			siteDetailSelection = readReconciliationSiteDetail(ctx, siteDetailReader, siteDetailConfig, *siteRef, siteCredential)
		}
	}

	bracket := reconcile.ClientBracket{
		Requested:      clientRequested,
		FileLayoutMode: *clientFileLayout,
		FileLimits:     clientFileLimits,
	}
	if clientRequested && (siteBindingGateFailed || adoptionGateFailed || activationGateFailed || removalGateFailed || retirementGateFailed || parentCleanupGateFailed) {
		bracket.StopReason = "client_snapshot_incomplete"
	}
	var session downloader.LedgerSession
	var fileJobKey string
	fileBeforeComplete := false
	if clientRequested && !siteBindingGateFailed && !adoptionGateFailed && !activationGateFailed && !removalGateFailed && !retirementGateFailed {
		session, err = clientAdapter.OpenReadSession(ctx, clientCredential)
		if err != nil {
			session = nil
			if requests, ok := downloader.RequestsMadeFromError(err); ok {
				bracket.RequestsMade = requests
			}
			bracket.StopReason = reconciliationClientStopReason(ctx, err, "client_session_failed")
		} else {
			before, readErr := session.ReadLedger(ctx)
			bracket.RequestsMade = session.RequestsMade()
			if readErr != nil {
				bracket.StopReason = reconciliationClientStopReason(ctx, readErr, "client_snapshot_before_failed")
			} else {
				bracket.Before = &before
				if *clientFileLayout == "auto" {
					if key, ok := reconcile.SelectExactJobForFileRead(meta, before, clientFileLimits); ok {
						fileJobKey = key
						bracket.FileAttempted = true
						fileRequestsBefore := session.RequestsMade()
						filesBefore, fileErr := session.ReadJobFiles(ctx, fileJobKey, clientFileLimits)
						bracket.FilesBefore = &filesBefore
						fileRequestsAfter := session.RequestsMade()
						if fileRequestsAfter >= fileRequestsBefore {
							bracket.FileRequestsMade += fileRequestsAfter - fileRequestsBefore
						}
						bracket.RequestsMade = fileRequestsAfter
						switch {
						case fileErr != nil:
							bracket.FileStopReason = reconciliationClientStopReason(ctx, fileErr, "client_file_snapshot_before_failed")
						case !filesBefore.Complete:
							bracket.FileStopReason = "client_file_snapshot_incomplete"
						default:
							fileBeforeComplete = true
						}
					}
				}
			}
		}
	}

	discoverOptions := seed.DiscoverOptions{
		SearchRoots:       append([]string(nil), searchRoots...),
		InventoryLimits:   inventoryLimits,
		MatchLimits:       matchLimits,
		AllowNetwork:      *allowNetwork,
		ShowAbsolutePaths: *showAbsolute,
		TimeBudget:        *timeout,
		Strategy:          "copy",
	}
	var reportMapping *reconcile.PathMappingOptions
	if mappingRootsRequested {
		discoverOptions.ClientMapping = &seed.ClientMappingOptions{HostRoot: *hostRoot, ClientRoot: *clientRoot, ClientWindows: *clientStyle == "windows"}
		reportMapping = &reconcile.PathMappingOptions{HostRoot: *hostRoot, ClientRoot: *clientRoot, ClientWindows: *clientStyle == "windows"}
	}
	var discovery seed.DiscoveryResult
	var discoveryErr error
	materializedSelection := reconcile.MaterializedFinalSelection{Requested: materializedRequested}
	materializedFinalIntegrityFailed := false
	if materializedRequested {
		verifiedFinal, sourceBridge, observed, _, finalErr := materialize.VerifyCurrentFinalSource(ctx, materialize.FinalProofOptions{
			Meta: meta, TargetRoot: *materializeTarget, OperationID: materializeOperation,
			ExpectedPlanID: *materializePlanID, Limits: materialize.DefaultLimits(),
		}, seed.ExactSourceOptions{
			ShowAbsolutePaths: *showAbsolute,
			TimeBudget:        *timeout,
		})
		discovery = observed
		materializedSelection.Final = verifiedFinal
		materializedSelection.Source = sourceBridge
		if finalErr != nil {
			materializedSelection.StopReason = reconciliationMaterializedFinalStopReason(ctx, finalErr, verifiedFinal != nil)
			materializedFinalIntegrityFailed = errors.Is(finalErr, materialize.ErrIntegrity) || errors.Is(finalErr, materialize.ErrCorruptJournal)
		} else if activationRequested && verifiedActivation != nil {
			currentUse, prepareErr := clientactivate.PrepareCurrentUse(verifiedFinal, verifiedActivation, clientactivate.CurrentUseOptions{
				ClientConfigID: clientConfigID, HostRoot: *hostRoot, ClientRoot: *clientRoot,
				ClientWindows: *clientStyle == "windows", FileLimits: clientFileLimits,
			})
			if prepareErr != nil {
				activationSelection.StopReason = "activation_completion_selector_mismatch"
			} else if removalRequested && verifiedRemoval != nil {
				activationSelection.CurrentAbsence = currentUse
			} else {
				activationSelection.CurrentUse = currentUse
			}
		}
	} else if exactRequested {
		discovery, discoveryErr = seed.ObserveExactSource(ctx, meta, *exactSource, seed.ExactSourceOptions{
			ShowAbsolutePaths: *showAbsolute,
			TimeBudget:        *timeout,
		})
	} else if indexedRequested {
		candidateLimits := storageindex.DefaultCandidateLimits()
		candidateLimits.MaxCandidates = inventoryLimits.MaxCandidates
		candidateLimits.MaxPathBytes = inventoryLimits.MaxPathBytes
		candidateLimits.MaxIssues = inventoryLimits.MaxIssues
		indexed, queryErr := indexedRepository.LoadCandidates(ctx, indexedProfile, indexedDescriptorID, wantedMetafileSizes(meta), candidateLimits)
		if queryErr != nil {
			discoveryErr = queryErr
		} else {
			discovery, discoveryErr = seed.DiscoverFromIndex(ctx, meta, indexedProfile, indexed, discoverOptions)
		}
	} else {
		discovery, discoveryErr = seed.Discover(ctx, meta, discoverOptions)
	}
	if session != nil && bracket.Before != nil {
		if fileBeforeComplete {
			fileRequestsBefore := session.RequestsMade()
			filesAfter, fileErr := session.ReadJobFiles(ctx, fileJobKey, clientFileLimits)
			bracket.FilesAfter = &filesAfter
			fileRequestsAfter := session.RequestsMade()
			if fileRequestsAfter >= fileRequestsBefore {
				bracket.FileRequestsMade += fileRequestsAfter - fileRequestsBefore
			}
			bracket.RequestsMade = fileRequestsAfter
			switch {
			case fileErr != nil:
				bracket.FileStopReason = reconciliationClientStopReason(ctx, fileErr, "client_file_snapshot_after_failed")
			case !filesAfter.Complete:
				bracket.FileStopReason = "client_file_snapshot_incomplete"
			}
		}
		after, readErr := session.ReadLedger(ctx)
		bracket.RequestsMade = session.RequestsMade()
		if readErr != nil {
			bracket.StopReason = reconciliationClientStopReason(ctx, readErr, "client_snapshot_after_failed")
		} else {
			bracket.After = &after
		}
	}
	if session != nil {
		bracket.RequestsMade = session.RequestsMade()
		_ = session.Close()
	}
	if discoveryErr != nil && exactRequested {
		if len(discovery.Blockers) == 0 {
			discovery.Blockers = append(discovery.Blockers, seed.DiscoveryBlocker{
				Code:    "source.exact_root_observation_incomplete",
				Message: "the explicitly selected exact source could not be observed completely",
			})
		}
		discoveryErr = nil
	}
	if discoveryErr != nil {
		return discoveryErr
	}
	verifiedSource, _ := discovery.VerifiedSource(meta)
	report, err := reconcile.Build(reconcile.BuildInput{
		Meta:              meta,
		Discovery:         discovery,
		VerifiedSource:    verifiedSource,
		Client:            bracket,
		SiteRef:           siteRef,
		SiteBinding:       siteSelection,
		SiteDetail:        siteDetailSelection,
		MaterializedFinal: materializedSelection,
		ClientAdoption:    adoptionSelection,
		ClientActivation:  activationSelection,
		ClientRemoval:     removalSelection,
		SourceRetirement:  retirementSelection,
		ParentCleanup:     parentCleanupSelection,
		PathMapping:       reportMapping,
		ShowAbsolutePaths: *showAbsolute,
	})
	if err != nil {
		return err
	}
	if *output == "json" {
		err = writeJSON(a.stdout, report, nil)
	} else {
		err = writeReconciliationHuman(a.stdout, report)
	}
	if err != nil {
		return err
	}
	if siteBindingIntegrityFailed {
		return &integrityErr{message: "the explicit site binding record or linked metafile artifact failed integrity verification"}
	}
	if materializedFinalIntegrityFailed {
		return &integrityErr{message: "the explicit materialize operation or current final layout failed integrity verification"}
	}
	if adoptionIntegrityFailed {
		return &integrityErr{message: "the explicit terminal client-adoption journal failed integrity verification"}
	}
	if activationIntegrityFailed {
		return &integrityErr{message: "the explicit terminal client-activation journal failed integrity verification"}
	}
	if removalIntegrityFailed {
		return &integrityErr{message: "the explicit terminal keep-data client-removal journal failed integrity verification"}
	}
	if retirementIntegrityFailed {
		return &integrityErr{message: "the explicit terminal source-retirement journal failed integrity verification"}
	}
	if parentCleanupIntegrityFailed {
		return &integrityErr{message: "the explicit terminal parent-cleanup journal failed integrity verification"}
	}
	if *requireReconciled && report.Outcome != "consistent" {
		return &inconclusiveErr{message: "reconciliation outcome is not consistent"}
	}
	return nil
}

func parseSiteRef(value string) (*domain.TorrentRef, error) {
	if value == "" {
		return nil, nil
	}
	if strings.Count(value, "/") != 1 {
		return nil, fmt.Errorf("--site-ref must be exactly SITE/REMOTE_ID")
	}
	siteID, remoteID, _ := strings.Cut(value, "/")
	if len(siteID) == 0 || len(siteID) > 128 || len(remoteID) == 0 || len(remoteID) > 256 {
		return nil, fmt.Errorf("--site-ref SITE and REMOTE_ID must be 1..128 and 1..256 bytes respectively")
	}
	for _, part := range []string{siteID, remoteID} {
		for _, r := range part {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' || r == ':' {
				continue
			}
			return nil, fmt.Errorf("--site-ref accepts only ASCII letters, digits, dot, underscore, hyphen, and colon")
		}
	}
	return &domain.TorrentRef{SiteID: siteID, RemoteID: remoteID}, nil
}

func siteBindingLoadStopReason(err error) string {
	switch {
	case errors.Is(err, sitebinding.ErrCorruptBinding), errors.Is(err, sitebinding.ErrInvalidBinding),
		errors.Is(err, metastore.ErrCorruptArtifact), errors.Is(err, metastore.ErrCorruptRecord),
		errors.Is(err, metastore.ErrRecordConsumerIncomplete):
		return "site_binding_integrity_failed"
	default:
		return "site_binding_load_failed"
	}
}

func reconciliationActivationStopReason(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), ctx.Err() != nil:
		return "activation_context_cancelled"
	case errors.Is(err, clientactivate.ErrIntegrity):
		return "activation_completion_integrity_failed"
	case errors.Is(err, clientactivate.ErrPolicy), errors.Is(err, clientactivate.ErrOperationNotFound):
		return "activation_completion_load_failed"
	default:
		return "activation_completion_load_failed"
	}
}

func reconciliationAdoptionStopReason(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), ctx.Err() != nil:
		return "adoption_context_cancelled"
	case errors.Is(err, clientadopt.ErrIntegrity):
		return "adoption_completion_integrity_failed"
	case errors.Is(err, clientadopt.ErrPolicy), errors.Is(err, clientadopt.ErrOperationNotFound):
		return "adoption_completion_load_failed"
	default:
		return "adoption_completion_load_failed"
	}
}

func reconciliationRemovalStopReason(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), ctx.Err() != nil:
		return "removal_context_cancelled"
	case errors.Is(err, clientremove.ErrIntegrity):
		return "removal_completion_integrity_failed"
	case errors.Is(err, clientremove.ErrPolicy), errors.Is(err, clientremove.ErrOperationNotFound):
		return "removal_completion_load_failed"
	default:
		return "removal_completion_load_failed"
	}
}

func reconciliationRetirementCompletionStopReason(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), ctx.Err() != nil:
		return "retirement_context_cancelled"
	case errors.Is(err, sourceretire.ErrExecutionIntegrity):
		return "retirement_completion_integrity_failed"
	case errors.Is(err, sourceretire.ErrExecutionPolicy), errors.Is(err, sourceretire.ErrOperationNotFound):
		return "retirement_completion_load_failed"
	default:
		return "retirement_completion_load_failed"
	}
}

func reconciliationRetirementAbsenceStopReason(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), ctx.Err() != nil:
		return "retirement_context_cancelled"
	case errors.Is(err, sourceretire.ErrRetiredNamePresent):
		return "retirement_source_name_reappeared"
	default:
		return "retirement_current_absence_unavailable"
	}
}

func reconciliationParentCleanupCompletionStopReason(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), ctx.Err() != nil:
		return "parent_cleanup_context_cancelled"
	case errors.Is(err, sourceretire.ErrExecutionIntegrity):
		return "parent_cleanup_completion_integrity_failed"
	case errors.Is(err, sourceretire.ErrExecutionPolicy), errors.Is(err, sourceretire.ErrOperationNotFound):
		return "parent_cleanup_completion_load_failed"
	default:
		return "parent_cleanup_completion_load_failed"
	}
}

func reconciliationParentCleanupAbsenceStopReason(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), ctx.Err() != nil:
		return "parent_cleanup_context_cancelled"
	case errors.Is(err, sourceretire.ErrRemovedParentPresent):
		return "parent_cleanup_removed_parent_reappeared"
	default:
		return "parent_cleanup_current_absence_unavailable"
	}
}

func validateSiteBindingAdapter(registry *site.Registry, binding *sitebinding.VerifiedSiteBinding) error {
	if registry == nil || binding == nil || !binding.Verified() {
		return fmt.Errorf("site binding adapter proof is unavailable")
	}
	public := binding.PublicCopy()
	if err := public.Record.Validate(); err != nil {
		return fmt.Errorf("site binding record is invalid")
	}
	ref := domain.TorrentRef{SiteID: public.Record.SiteID, RemoteID: public.Record.RemoteID}
	adapter, ok := registry.Get(ref.SiteID)
	if !ok {
		return fmt.Errorf("site binding adapter is unavailable")
	}
	descriptor := adapter.Descriptor()
	if descriptor.ID != ref.SiteID || !descriptor.Supports(domain.CapabilityMetafile) {
		return fmt.Errorf("site binding adapter capability is unavailable")
	}
	fetcher, ok := adapter.(site.MetafileFetcher)
	if !ok {
		return fmt.Errorf("site binding adapter port is unavailable")
	}
	config, err := fetcher.MetafileFetchConfig()
	if err != nil || config.Validate() != nil || config.Origin != public.Record.Origin || config.RouteID != public.Record.RouteID {
		return fmt.Errorf("site binding adapter provenance does not match")
	}
	descriptorOrigin, err := canonicalSiteDescriptorOrigin(descriptor.BaseURL)
	if err != nil || descriptorOrigin != config.Origin {
		return fmt.Errorf("site binding adapter origin is unsafe")
	}
	if err := fetcher.ValidateMetafileRef(ref); err != nil {
		return fmt.Errorf("site binding remote reference is not canonical")
	}
	return nil
}

func reconciliationClientStopReason(ctx context.Context, err error, fallback string) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return "context_cancelled"
	}
	return fallback
}

func reconciliationMaterializedFinalStopReason(ctx context.Context, err error, finalObserved bool) string {
	switch {
	case errors.Is(err, materialize.ErrIntegrity), errors.Is(err, materialize.ErrCorruptJournal):
		return "materialized_final_integrity_failed"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "materialized_final_context_cancelled"
	case errors.Is(err, materialize.ErrPolicy), errors.Is(err, materialize.ErrOperationNotFound):
		return "materialized_final_policy_blocked"
	case ctx.Err() != nil:
		return "materialized_final_context_cancelled"
	case finalObserved:
		return "materialized_final_source_bridge_failed"
	default:
		return "materialized_final_verification_failed"
	}
}

func (a *app) siteList(args []string) error {
	fs := newFlagSet("site list")
	output := fs.String("output", "table", "table or json")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usageError("site list accepts only --output")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	items := a.registry.Descriptors()
	if *output == "json" {
		return writeJSON(a.stdout, items, nil)
	}
	if *output != "table" {
		return usageError("--output must be table or json")
	}
	w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSTABILITY\tBASE URL\tCAPABILITIES")
	for _, item := range items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", terminalSafe(item.ID), terminalSafe(item.Name), terminalSafe(item.Stability), terminalSafe(item.BaseURL), len(item.Capabilities))
	}
	return w.Flush()
}

func (a *app) siteCapabilities(args []string) error {
	fs := newFlagSet("site capabilities")
	output := fs.String("output", "table", "table or json")
	if err := fs.Parse(args); err != nil || fs.NArg() > 1 {
		return usageError("site capabilities accepts zero or one SITE")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	items := a.registry.Descriptors()
	if fs.NArg() == 1 {
		driver, ok := a.registry.Get(fs.Arg(0))
		if !ok {
			return fmt.Errorf("unknown site %q", fs.Arg(0))
		}
		items = []domain.SiteDescriptor{driver.Descriptor()}
	}
	if *output == "json" {
		return writeJSON(a.stdout, items, nil)
	}
	if *output != "table" {
		return usageError("--output must be table or json")
	}
	w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SITE\tKIND\tVALUE")
	for _, item := range items {
		for _, method := range item.AuthMethods {
			fmt.Fprintf(w, "%s\tauth\t%s\n", terminalSafe(item.ID), terminalSafe(string(method)))
		}
		for _, capability := range item.Capabilities {
			fmt.Fprintf(w, "%s\tcapability\t%s\n", terminalSafe(item.ID), terminalSafe(string(capability)))
		}
	}
	return w.Flush()
}

func (a *app) siteRead(command string, args []string) error {
	fs := newFlagSet("site " + command)
	output := fs.String("output", "table", "table or json")
	cookieStdin := fs.Bool("cookie-stdin", false, "read session Cookie header value from stdin")
	if err := fs.Parse(args); err != nil {
		return usageError("site %s: %v", command, err)
	}
	if fs.NArg() < 1 {
		return usageError("site %s requires SITE", command)
	}
	if command != "search" && fs.NArg() != 1 {
		return usageError("site %s requires exactly one SITE", command)
	}
	if command == "search" && fs.NArg() < 2 {
		return usageError("site search requires SITE and QUERY")
	}
	if !*cookieStdin {
		return usageError("--cookie-stdin is required; credentials are never accepted in argv")
	}
	adapter, ok := a.registry.Get(fs.Arg(0))
	if !ok {
		return fmt.Errorf("unknown site %q", fs.Arg(0))
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	capability := map[string]domain.Capability{
		"status":        domain.CapabilityAuthCheck,
		"account":       domain.CapabilityAccountRead,
		"search":        domain.CapabilitySearch,
		"bonus-catalog": domain.CapabilityBonusRead,
	}[command]
	descriptor := adapter.Descriptor()
	if !descriptor.Supports(capability) {
		return fmt.Errorf("site %q does not declare capability %q", descriptor.ID, capability)
	}
	if !descriptor.SupportsAuth(domain.AuthMethodCookieHeader) {
		return fmt.Errorf("site %q does not support cookie_header authentication", descriptor.ID)
	}
	var read func(context.Context, site.Credential) (any, error)
	var warnings []string
	switch command {
	case "status":
		reader, ok := adapter.(site.AuthChecker)
		if !ok {
			return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, capability)
		}
		read = func(ctx context.Context, credential site.Credential) (any, error) {
			return reader.CheckSession(ctx, credential)
		}
	case "account":
		reader, ok := adapter.(site.AccountReader)
		if !ok {
			return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, capability)
		}
		read = func(ctx context.Context, credential site.Credential) (any, error) {
			return reader.Account(ctx, credential)
		}
	case "search":
		query := strings.TrimSpace(strings.Join(fs.Args()[1:], " "))
		if query == "" {
			return usageError("site search query is empty")
		}
		reader, ok := adapter.(site.TorrentSearcher)
		if !ok {
			return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, capability)
		}
		read = func(ctx context.Context, credential site.Credential) (any, error) {
			return reader.Search(ctx, credential, query)
		}
	case "bonus-catalog":
		reader, ok := adapter.(site.BonusCatalogReader)
		if !ok {
			return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, capability)
		}
		read = func(ctx context.Context, credential site.Credential) (any, error) {
			return reader.BonusCatalog(ctx, credential)
		}
		warnings = append(warnings, "catalog is read-only; this command never submits purchase or redemption forms")
	default:
		return usageError("unknown site read command %q", command)
	}
	credential, err := readCredential(a.stdin)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	data, err := read(ctx, credential)
	if err != nil {
		return siteReadPublicError(ctx, command, err)
	}
	if *output == "json" {
		return writeJSON(a.stdout, data, warnings)
	}
	return writeSiteHuman(a.stdout, command, data, warnings)
}

func siteReadPublicError(ctx context.Context, command string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("site %s canceled: %w", command, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("site %s timed out: %w", command, context.DeadlineExceeded)
	}
	return fmt.Errorf("site %s failed", command)
}

func (a *app) torrent(args []string) error {
	if len(args) == 0 {
		return usageError("torrent subcommand is required")
	}
	switch args[0] {
	case "inspect":
		fs := newFlagSet("torrent inspect")
		output := fs.String("output", "table", "table or json")
		storeRoot := fs.String("metafile-store", "", "private metafile store root; pair with --metafile-variant")
		variantID := fs.String("metafile-variant", "", "whole-metafile sha256 artifact ID; pair with --metafile-store")
		if err := fs.Parse(args[1:]); err != nil {
			return usageError("torrent inspect: %v", err)
		}
		if err := validateOutput(*output); err != nil {
			return err
		}
		input, err := positionalMetafileInput("torrent inspect", fs.Args(), *storeRoot, *variantID, flagWasSet(fs, "metafile-store"), flagWasSet(fs, "metafile-variant"))
		if err != nil {
			return err
		}
		meta, err := loadMetafileInput(context.Background(), input)
		if err != nil {
			return err
		}
		if *output == "json" {
			return writeJSON(a.stdout, meta, nil)
		}
		if *output != "table" {
			return usageError("--output must be table or json")
		}
		return writeMetaHuman(a.stdout, meta)
	case "verify":
		fs := newFlagSet("torrent verify")
		output := fs.String("output", "table", "table or json")
		content := fs.String("content", "", "exact file (single-file) or torrent root directory (multi-file)")
		storeRoot := fs.String("metafile-store", "", "private metafile store root; pair with --metafile-variant")
		variantID := fs.String("metafile-variant", "", "whole-metafile sha256 artifact ID; pair with --metafile-store")
		if err := fs.Parse(args[1:]); err != nil {
			return usageError("torrent verify: %v", err)
		}
		if *content == "" {
			return usageError("torrent verify requires --content PATH")
		}
		if *output != "table" && *output != "json" {
			return usageError("--output must be table or json")
		}
		input, err := positionalMetafileInput("torrent verify", fs.Args(), *storeRoot, *variantID, flagWasSet(fs, "metafile-store"), flagWasSet(fs, "metafile-variant"))
		if err != nil {
			return err
		}
		meta, err := loadMetafileInput(context.Background(), input)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result, err := metafile.Verify(ctx, meta, *content)
		if err != nil {
			return err
		}
		if *output == "json" {
			err = writeJSON(a.stdout, result, nil)
		} else {
			err = writeVerifyHuman(a.stdout, result)
		}
		if err != nil {
			return err
		}
		if !result.Verified {
			return &integrityErr{message: fmt.Sprintf("content failed exact torrent verification (%d of %d piece proofs matched)", result.PiecesMatched, result.PiecesExpected)}
		}
		return nil
	default:
		return usageError("unknown torrent subcommand %q", args[0])
	}
}

func (a *app) storage(args []string) error {
	if len(args) == 0 {
		return usageError("storage subcommand is required")
	}
	switch args[0] {
	case "probe":
		fs := newFlagSet("storage probe")
		output := fs.String("output", "table", "table or json")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
			return usageError("storage probe requires one PATH")
		}
		if err := validateOutput(*output); err != nil {
			return err
		}
		result, err := storage.ProbeReadOnly(fs.Arg(0))
		if err != nil {
			return err
		}
		if *output == "json" {
			return writeJSON(a.stdout, result, nil)
		}
		if *output != "table" {
			return usageError("--output must be table or json")
		}
		return writeProbeHuman(a.stdout, result)
	case "map":
		fs := newFlagSet("storage map")
		output := fs.String("output", "table", "table or json")
		hostRoot := fs.String("host-root", "", "host-visible storage root")
		clientRoot := fs.String("client-root", "", "downloader-visible storage root")
		clientStyle := fs.String("client-style", "posix", "posix or windows")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 || *hostRoot == "" || *clientRoot == "" {
			return usageError("storage map requires --host-root, --client-root, and HOST_PATH")
		}
		if *clientStyle != "posix" && *clientStyle != "windows" {
			return usageError("--client-style must be posix or windows")
		}
		if err := validateOutput(*output); err != nil {
			return err
		}
		result, err := storage.MapHostToClient(*hostRoot, fs.Arg(0), *clientRoot, *clientStyle == "windows")
		if err != nil {
			return err
		}
		if *output == "json" {
			return writeJSON(a.stdout, result, nil)
		}
		if *output != "table" {
			return usageError("--output must be table or json")
		}
		fmt.Fprintf(a.stdout, "%s\n", terminalSafe(result.ClientPath))
		return nil
	case "profile":
		return a.storageProfileCommand(args[1:])
	case "index":
		return a.storageIndexCommand(args[1:])
	default:
		return usageError("unknown storage subcommand %q", args[0])
	}
}

func (a *app) seed(args []string) error {
	if len(args) == 0 {
		return usageError("seed subcommand is required")
	}
	if args[0] == "discover" {
		return a.seedDiscover(args[1:])
	}
	if args[0] == "materialize" {
		return a.seedMaterialize(args[1:])
	}
	if args[0] == "retire" {
		return a.seedRetire(args[1:])
	}
	if args[0] != "plan" {
		return usageError("unknown seed subcommand %q", args[0])
	}
	fs := newFlagSet("seed plan")
	output := fs.String("output", "table", "table or json")
	torrentPath := fs.String("torrent", "", "metafile path")
	storeRoot := fs.String("metafile-store", "", "private metafile store root; pair with --metafile-variant")
	variantID := fs.String("metafile-variant", "", "whole-metafile sha256 artifact ID; pair with --metafile-store")
	source := fs.String("source", "", "verified source content")
	target := fs.String("target", "", "target storage root")
	strategy := fs.String("strategy", "copy", "copy")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *source == "" || *target == "" {
		return usageError("seed plan requires a metafile input, --source, and --target")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *strategy != "copy" {
		return usageError("seed plan --strategy must be copy")
	}
	input, err := flaggedMetafileInput("seed plan", *torrentPath, *storeRoot, *variantID, flagWasSet(fs, "torrent"), flagWasSet(fs, "metafile-store"), flagWasSet(fs, "metafile-variant"))
	if err != nil {
		return err
	}
	meta, err := loadMetafileInput(context.Background(), input)
	if err != nil {
		return err
	}
	plan, err := seed.BuildMaterializePlan(context.Background(), meta, *source, *target, *strategy)
	if err != nil {
		if errors.Is(err, seed.ErrSourceIntegrity) {
			return &integrityErr{message: err.Error()}
		}
		return err
	}
	if *output == "json" {
		return writeJSON(a.stdout, plan, nil)
	}
	if *output != "table" {
		return usageError("--output must be table or json")
	}
	return writePlanHuman(a.stdout, plan)
}

func (a *app) seedDiscover(args []string) error {
	fs := newFlagSet("seed discover")
	var flagOutput strings.Builder
	fs.SetOutput(&flagOutput)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  ptctl seed discover (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) (--search-root PATH... | --state-store DIR --storage-profile PROFILE) [flags]")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Live-root discovery can establish current uniqueness. Stored-profile discovery reobserves and exactly verifies historical locators but never infers current uniqueness. With an explicit --snapshot-record and --select-source-match, one reviewed exact source assignment may emit a target plan; this is explicit selection, not a negative or uniqueness proof. Both modes perform zero writes.")
		fmt.Fprintln(fs.Output(), "Host/client mapping applies to discovered sources, or to planned targets when --target is set.")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
	}
	output := fs.String("output", "table", "table or json")
	torrentPath := fs.String("torrent", "", "metafile path")
	storeRoot := fs.String("metafile-store", "", "private metafile store root; pair with --metafile-variant")
	variantID := fs.String("metafile-variant", "", "whole-metafile sha256 artifact ID; pair with --metafile-store")
	var searchRoots stringListFlag
	fs.Var(&searchRoots, "search-root", "storage root to scan; repeatable")
	stateStore := fs.String("state-store", "", "initialized private state store; pair with --storage-profile")
	storageProfile := fs.String("storage-profile", "", "stored profile name or immutable ID; pair with --state-store")
	snapshotRecord := fs.String("snapshot-record", "", "explicit descriptor record ID for stored-profile mode")
	selectedSourceMatch := fs.String("select-source-match", "", "explicit sha256 source-match ID from the selected stored snapshot")
	target := fs.String("target", "", "optional target storage root for a layout-only plan")
	strategy := fs.String("strategy", "copy", "layout plan strategy; only copy is supported")
	showAbsolute := fs.Bool("show-absolute-paths", false, "include absolute host paths in output")
	allowNetwork := fs.Bool("allow-network", false, "allow explicit network/UNC search roots")
	requireVerified := fs.Bool("require-verified", false, "exit 4 unless source_outcome is verified_unique or verified_selected")
	timeout := fs.Duration("timeout", time.Hour, "shared scan and verification wall-clock budget")
	hostRoot := fs.String("host-root", "", "optional host namespace root for source paths, or target paths with --target")
	clientRoot := fs.String("client-root", "", "optional downloader namespace root paired with --host-root")
	clientStyle := fs.String("client-style", "posix", "downloader path style: posix or windows; requires host/client roots")

	inventoryDefaults := storage.DefaultInventoryLimits()
	maxDepth := fs.Int("max-depth", inventoryDefaults.MaxDepth, "maximum directory depth")
	maxDirectories := fs.Int("max-directories", inventoryDefaults.MaxDirectories, "maximum directories opened")
	maxEntries := fs.Int("max-entries", inventoryDefaults.MaxEntries, "maximum directory entries examined")
	maxDirectoryEntries := fs.Int("max-directory-entries", inventoryDefaults.MaxEntriesPerDirectory, "maximum entries accepted from one directory")
	maxCandidates := fs.Int("max-candidates", inventoryDefaults.MaxCandidates, "maximum matching regular files retained")
	maxPathBytes := fs.Int64("max-path-bytes", inventoryDefaults.MaxPathBytes, "maximum retained relative-path bytes")

	matchDefaults := metafile.DefaultSourceMatchLimits()
	maxCandidatesPerFile := fs.Int("max-candidates-per-file", matchDefaults.MaxCandidatesPerFile, "maximum candidates explored for one torrent file")
	maxCandidateEdges := fs.Int("max-candidate-edges", matchDefaults.MaxCandidateEdges, "maximum manifest-file to source-candidate edges considered")
	maxStates := fs.Int("max-states", matchDefaults.MaxStates, "maximum candidate assignment states")
	maxVerifiedLayouts := fs.Int("max-verified-layouts", matchDefaults.MaxVerifiedLayouts, "maximum verified alternatives retained")
	maxProofBytes := fs.Int64("max-proof-bytes", matchDefaults.MaxProofWorkBytes, "maximum physical and virtual bytes charged to proof work")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(a.stdout, flagOutput.String())
			return nil
		}
		detail := strings.TrimSpace(flagOutput.String())
		if detail == "" {
			detail = err.Error()
		}
		return usageError("seed discover: %s", detail)
	}
	if fs.NArg() != 0 {
		return usageError("seed discover accepts flags only; unexpected argument %q", fs.Arg(0))
	}
	explicit := make(map[string]bool)
	fs.Visit(func(item *flag.Flag) { explicit[item.Name] = true })
	indexedRequested := explicit["state-store"] || explicit["storage-profile"] || explicit["snapshot-record"] || explicit["select-source-match"]
	if len(searchRoots) == 0 && !indexedRequested {
		return usageError("seed discover requires --search-root or the --state-store/--storage-profile pair")
	}
	if len(searchRoots) > 0 && indexedRequested {
		return usageError("--search-root and stored-profile index mode are mutually exclusive")
	}
	if indexedRequested && (*stateStore == "" || *storageProfile == "") {
		return usageError("stored-profile mode requires non-empty --state-store and --storage-profile")
	}
	if explicit["snapshot-record"] && !indexedRequested {
		return usageError("--snapshot-record requires stored-profile mode")
	}
	if explicit["select-source-match"] {
		if !explicit["snapshot-record"] {
			return usageError("--select-source-match requires an explicit --snapshot-record")
		}
		if !validSHA256Selector(*selectedSourceMatch) {
			return usageError("--select-source-match requires a canonical sha256 source-match ID")
		}
	}
	if indexedRequested && explicit["allow-network"] {
		return usageError("--allow-network is fixed by the immutable storage profile in stored-profile mode")
	}
	for _, name := range []string{"max-depth", "max-directories", "max-entries", "max-directory-entries"} {
		if indexedRequested && explicit[name] {
			return usageError("--%s applies only to live --search-root scanning; refresh limits are fixed by the storage profile", name)
		}
	}
	var explicitDescriptor metastore.RecordID
	if explicit["snapshot-record"] {
		parsedDescriptor, parseErr := metastore.ParseRecordID(*snapshotRecord)
		if parseErr != nil {
			return usageError("--snapshot-record is invalid")
		}
		explicitDescriptor = parsedDescriptor
	}
	input, err := flaggedMetafileInput("seed discover", *torrentPath, *storeRoot, *variantID, explicit["torrent"], explicit["metafile-store"], explicit["metafile-variant"])
	if err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > 7*24*time.Hour {
		return usageError("--timeout must be greater than zero and no more than 168h")
	}
	if *strategy != "copy" {
		return usageError("--strategy must be copy; layout strategies are used only with --target")
	}
	if explicit["strategy"] && *target == "" {
		return usageError("--strategy requires --target")
	}
	if *clientStyle != "posix" && *clientStyle != "windows" {
		return usageError("--client-style must be posix or windows")
	}
	if (*hostRoot == "") != (*clientRoot == "") {
		return usageError("--host-root and --client-root must be provided together")
	}
	if explicit["client-style"] && *hostRoot == "" {
		return usageError("--client-style requires --host-root and --client-root")
	}
	inventoryLimits := inventoryDefaults
	inventoryLimits.MaxDepth = *maxDepth
	inventoryLimits.MaxDirectories = *maxDirectories
	inventoryLimits.MaxEntries = *maxEntries
	inventoryLimits.MaxEntriesPerDirectory = *maxDirectoryEntries
	inventoryLimits.MaxCandidates = *maxCandidates
	inventoryLimits.MaxPathBytes = *maxPathBytes
	matchLimits := matchDefaults
	matchLimits.MaxCandidatesPerFile = *maxCandidatesPerFile
	matchLimits.MaxCandidateEdges = *maxCandidateEdges
	matchLimits.MaxStates = *maxStates
	matchLimits.MaxVerifiedLayouts = *maxVerifiedLayouts
	matchLimits.MaxProofWorkBytes = *maxProofBytes
	if err := inventoryLimits.Validate(); err != nil {
		return usageError("seed discover: %v", err)
	}
	if err := matchLimits.Validate(); err != nil {
		return usageError("seed discover: %v", err)
	}
	if *hostRoot != "" {
		if err := storage.ValidatePathMappingConfig(*hostRoot, *clientRoot, *clientStyle == "windows"); err != nil {
			return usageError("seed discover path mapping is invalid: %v", err)
		}
	}
	meta, err := loadMetafileInput(context.Background(), input)
	if err != nil {
		return err
	}
	options := seed.DiscoverOptions{
		SearchRoots:           append([]string(nil), searchRoots...),
		InventoryLimits:       inventoryLimits,
		MatchLimits:           matchLimits,
		AllowNetwork:          *allowNetwork,
		ShowAbsolutePaths:     *showAbsolute,
		TimeBudget:            *timeout,
		TargetRoot:            *target,
		Strategy:              *strategy,
		ExplicitSourceMatchID: *selectedSourceMatch,
	}
	if *hostRoot != "" {
		options.ClientMapping = &seed.ClientMappingOptions{HostRoot: *hostRoot, ClientRoot: *clientRoot, ClientWindows: *clientStyle == "windows"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var result seed.DiscoveryResult
	if indexedRequested {
		store, openErr := metastore.Open(*stateStore)
		if openErr != nil {
			return openErr
		}
		repository, repositoryErr := storageindex.NewRepository(store, storageindex.DefaultLimits())
		if repositoryErr != nil {
			return repositoryErr
		}
		profileSelection, selectionErr := repository.SelectProfile(ctx, *storageProfile)
		if selectionErr != nil {
			return selectionErr
		}
		if liveErr := storageindex.ValidateProfileForLiveUse(profileSelection.Profile, storageindex.DefaultLimits()); liveErr != nil {
			return liveErr
		}
		snapshotSelection, selectionErr := repository.SelectSnapshot(ctx, profileSelection.Profile, explicitDescriptor)
		if selectionErr != nil {
			return selectionErr
		}
		candidateLimits := storageindex.DefaultCandidateLimits()
		candidateLimits.MaxCandidates = inventoryLimits.MaxCandidates
		candidateLimits.MaxPathBytes = inventoryLimits.MaxPathBytes
		candidateLimits.MaxIssues = inventoryLimits.MaxIssues
		indexed, queryErr := repository.LoadCandidates(ctx, profileSelection.Profile, snapshotSelection.DescriptorRecordID, wantedMetafileSizes(meta), candidateLimits)
		if queryErr != nil {
			return queryErr
		}
		result, err = seed.DiscoverFromIndex(ctx, meta, profileSelection.Profile, indexed, options)
	} else {
		result, err = seed.Discover(ctx, meta, options)
	}
	if err != nil {
		return err
	}
	if *output == "json" {
		err = writeJSON(a.stdout, result, nil)
	} else {
		err = writeDiscoveryHuman(a.stdout, result)
	}
	if err != nil {
		return err
	}
	if *requireVerified && result.SourceOutcome != "verified_unique" && result.SourceOutcome != "verified_selected" {
		return &inconclusiveErr{message: "seed discovery source outcome is not verified_unique or verified_selected"}
	}
	return nil
}

func readCredential(reader io.Reader) (site.Credential, error) {
	if err := rejectTTYSecret(reader); err != nil {
		return site.Credential{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, (64<<10)+1))
	if err != nil {
		return site.Credential{}, fmt.Errorf("read session credential from stdin: %w", err)
	}
	if len(raw) > 64<<10 {
		return site.Credential{}, fmt.Errorf("session credential exceeds 64 KiB")
	}
	value := strings.TrimSpace(string(raw))
	return site.NewCookieCredential(value)
}

func readDownloaderCredential(reader io.Reader, username string) (downloader.Credential, error) {
	if err := rejectTTYSecret(reader); err != nil {
		return downloader.Credential{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, (64<<10)+1))
	if err != nil {
		return downloader.Credential{}, fmt.Errorf("read downloader password from stdin: %w", err)
	}
	if len(raw) > 64<<10 {
		return downloader.Credential{}, fmt.Errorf("downloader password exceeds 64 KiB")
	}
	return downloader.NewCredential(username, strings.TrimRight(string(raw), "\r\n"))
}

func rejectTTYSecret(reader io.Reader) error {
	file, ok := reader.(*os.File)
	if !ok {
		return nil
	}
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect secret input: %w", err)
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return fmt.Errorf("refusing to read a secret from an interactive terminal; pipe it to the --*-stdin command")
	}
	return nil
}

func validateOutput(output string) error {
	if output != "table" && output != "json" {
		return usageError("--output must be table or json")
	}
	return nil
}

func wantedMetafileSizes(meta *metafile.MetaInfo) []int64 {
	if meta == nil {
		return []int64{}
	}
	seen := make(map[int64]struct{})
	for _, file := range meta.Files {
		if file.Length == 0 || strings.Contains(file.Attribute, "p") {
			continue
		}
		seen[file.Length] = struct{}{}
	}
	result := make([]int64, 0, len(seen))
	for size := range seen {
		result = append(result, size)
	}
	return result
}

func validSHA256Selector(value string) bool {
	_, err := metastore.ParseRecordID(value)
	return err == nil
}

func writeJSON(out io.Writer, data any, warnings []string) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(envelope{Schema: "ptctl.dev/v1", Kind: jsonKind(data), Data: data, Warnings: warnings})
}

func jsonKind(data any) string {
	switch typed := data.(type) {
	case map[string]string:
		return "version"
	case []domain.SiteDescriptor:
		return "site.descriptor.list"
	case domain.SessionStatus:
		return "site.session"
	case domain.AccountSnapshot:
		return "site.account"
	case []domain.TorrentSummary:
		return "site.torrent.list"
	case siteDetailReport:
		return typed.kind
	case siteBonusReviewReport:
		return typed.kind
	case siteBonusExchangeReport:
		return typed.kind
	case domain.BonusCatalog:
		return "site.bonus.catalog"
	case *metafile.MetaInfo:
		return "metafile.manifest"
	case metafileStoreReport:
		return typed.kind
	case siteMetafileFetchReport:
		return typed.kind
	case siteBindingListReport:
		return typed.kind
	case siteBindingInspectReport:
		return typed.kind
	case storageProfileReport:
		return typed.kind
	case storageIndexReport:
		return typed.kind
	case storageIndexInspectReport:
		return typed.kind
	case metafile.VerificationResult:
		return "content.verification"
	case storage.ProbeResult:
		return "storage.probe"
	case storage.PathMapping:
		return "storage.path_mapping"
	case downloader.Status:
		return "downloader.status"
	case []downloader.Torrent:
		return "downloader.torrent.list"
	case seed.Plan:
		return "content.layout_plan"
	case seed.DiscoveryResult:
		return "content.source_discovery"
	case materialize.Report:
		return "content.materialization"
	case materialize.OperationListResult:
		return "content.materialization.operation_list"
	case materialize.RetentionReport:
		return "content.materialization.retention"
	case materialize.ForgetReport:
		return "content.materialization.forget"
	case clientadopt.Report:
		return "client.adoption"
	case clientadopt.RetentionReport:
		return "client.adoption.retention"
	case clientadopt.ForgetReport:
		return "client.adoption.forget"
	case clientactivate.Report:
		return "client.activation"
	case clientactivate.RetentionReport:
		return "client.activation.retention"
	case clientactivate.ForgetReport:
		return "client.activation.forget"
	case clientremove.Report:
		return "client.removal"
	case clientremove.RetentionReport:
		return "client.removal.retention"
	case clientremove.ForgetReport:
		return "client.removal.forget"
	case sourceretire.Report:
		return "content.source_retirement_plan"
	case sourceretire.ExecutionReport:
		return "content.source_retirement"
	case sourceretire.ExecutionOperationListResult:
		return "content.source_retirement.operation_list"
	case sourceretire.ExecutionRetentionReport:
		return "content.source_retirement.retention"
	case sourceretire.ExecutionForgetReport:
		return "content.source_retirement.forget"
	case sourceretire.ParentCleanupReport:
		return "content.source_retirement.parent_cleanup_plan"
	case sourceretire.ParentCleanupExecutionReport:
		return "content.source_retirement.parent_cleanup"
	case sourceretire.ParentCleanupRetentionReport:
		return "content.source_retirement.parent_cleanup.retention"
	case sourceretire.ParentCleanupForgetReport:
		return "content.source_retirement.parent_cleanup.forget"
	case reconcile.Report:
		return "ledger.reconciliation"
	default:
		return "unknown"
	}
}

func writeSiteHuman(out io.Writer, command string, data any, warnings []string) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	switch command {
	case "status":
		status := data.(domain.SessionStatus)
		fmt.Fprintf(w, "AUTHENTICATION\t%s\nUSERNAME\t%s\nOBSERVED\t%s\n", terminalSafe(string(status.State)), terminalSafe(status.Username), status.ObservedAt.Format(time.RFC3339))
	case "account":
		account := data.(domain.AccountSnapshot)
		fmt.Fprintf(w, "SITE\t%s\nUSERNAME\t%s\n", terminalSafe(account.SiteID), terminalSafe(account.Username))
		if account.UploadedBytes != nil {
			fmt.Fprintf(w, "UPLOADED\t%s\n", humanBytes(*account.UploadedBytes))
		}
		if account.DownloadedBytes != nil {
			fmt.Fprintf(w, "DOWNLOADED\t%s\n", humanBytes(*account.DownloadedBytes))
		}
		if account.Ratio != "" {
			fmt.Fprintf(w, "RATIO\t%s\n", terminalSafe(account.Ratio))
		}
		if account.Seeding != nil {
			fmt.Fprintf(w, "SEEDING\t%d\n", *account.Seeding)
		}
		if account.Leeching != nil {
			fmt.Fprintf(w, "LEECHING\t%d\n", *account.Leeching)
		}
		fmt.Fprintf(w, "BONUS\t%s\nOBSERVED\t%s\n", terminalSafe(valueOrUnknown(account.Bonus)), account.ObservedAt.Format(time.RFC3339))
	case "search":
		fmt.Fprintln(w, "REF\tNAME\tSIZE")
		for _, item := range data.([]domain.TorrentSummary) {
			size := "unknown"
			if item.SizeBytes != nil {
				size = humanBytes(*item.SizeBytes)
			}
			fmt.Fprintf(w, "%s/%s\t%s\t%s\n", terminalSafe(item.Ref.SiteID), terminalSafe(item.Ref.RemoteID), terminalSafe(item.Name), size)
		}
	case "bonus-catalog":
		catalog := data.(domain.BonusCatalog)
		fmt.Fprintf(w, "BALANCE\t%s\nOPTION\tOFFER\n", terminalSafe(valueOrUnknown(catalog.Balance)))
		for index, row := range catalog.Rows {
			selector := row.Selector
			if selector == "" {
				selector = fmt.Sprintf("unknown-%d", index+1)
			}
			fmt.Fprintf(w, "%s\t%s\n", terminalSafe(selector), terminalSafe(strings.Join(row.Columns, " | ")))
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, warning := range warnings {
		fmt.Fprintf(out, "warning: %s\n", terminalSafe(warning))
	}
	return nil
}

func writeMetaHuman(out io.Writer, meta *metafile.MetaInfo) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "NAME\t%s\nVERSION\t%s\nVALIDATION\t%s\nMETAFILE VARIANT\t%s\nPRIVATE\t%t\nTOTAL\t%s (%d bytes)\nPIECE LENGTH\t%s\n", terminalSafe(meta.Name), terminalSafe(meta.Version), terminalSafe(meta.Validation), terminalSafe(meta.MetafileVariantID), meta.Private, humanBytes(meta.TotalLength), meta.TotalLength, humanBytes(meta.PieceLength))
	if meta.InfoHashV1 != "" {
		fmt.Fprintf(w, "V1 PIECES\t%d\n", meta.V1PieceCount)
	}
	if meta.InfoHashV1 != "" {
		fmt.Fprintf(w, "INFOHASH V1\t%s\n", terminalSafe(meta.InfoHashV1))
	}
	if meta.InfoHashV2 != "" {
		fmt.Fprintf(w, "INFOHASH V2\t%s\n", terminalSafe(meta.InfoHashV2))
	}
	for _, tracker := range meta.Trackers {
		fmt.Fprintf(w, "TRACKER ORIGIN\t%s\n", terminalSafe(tracker))
	}
	fmt.Fprintln(w, "\nBYTES\tPATH")
	for _, file := range meta.Files {
		fmt.Fprintf(w, "%d\t%s\n", file.Length, terminalSafe(strings.Join(file.Path, "/")))
	}
	return w.Flush()
}

func writeVerifyHuman(out io.Writer, result metafile.VerificationResult) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "VERIFIED\t%t\nVERSION\t%s\nEVIDENCE\t%s\nSNAPSHOT\t%s\nSTABILITY\t%s\nBYTES\t%d\nPHYSICAL FILES\t%d\nVIRTUAL PADDING\t%d\n", result.Verified, terminalSafe(result.Version), terminalSafe(result.Evidence), terminalSafe(result.SourceSnapshotID), terminalSafe(result.StabilityAssurance), result.BytesVerified, result.FilesChecked, result.PaddingBytes)
	for _, check := range result.Checks {
		status := "fail"
		if check.Verified {
			status = "pass"
		}
		fmt.Fprintf(w, "\nCHECK\t%s\nSTATUS\t%s\nEVIDENCE\t%s\nPIECES\t%d/%d\n", terminalSafe(check.Algorithm), status, terminalSafe(check.Evidence), check.PiecesMatched, check.PiecesExpected)
		if check.RootsExpected > 0 {
			fmt.Fprintf(w, "FILE ROOTS\t%d/%d\n", check.RootsMatched, check.RootsExpected)
		}
		if len(check.MismatchPieces) > 0 {
			fmt.Fprintf(w, "PROOF MISMATCHES\t%v", check.MismatchPieces)
			if check.MismatchOverflow > 0 {
				fmt.Fprintf(w, " (+%d more)", check.MismatchOverflow)
			}
			fmt.Fprintln(w)
		}
	}
	return w.Flush()
}

func writeProbeHuman(out io.Writer, result storage.ProbeResult) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "PATH\t%s\nRESOLVED\t%s\nDIRECTORY\t%t\nSEMANTICS EVIDENCE\t%s\nRANDOM READ\t%s\nSEEDABLE VIEW\t%s\nWRITE PROBE\t%s\n", terminalSafe(result.Path), terminalSafe(result.ResolvedPath), result.Directory, terminalSafe(result.SemanticsEvidence), terminalSafe(result.RandomRead), terminalSafe(result.SeedableView), terminalSafe(result.WriteProbe))
	for _, warning := range result.Warnings {
		fmt.Fprintf(w, "WARNING\t%s\n", terminalSafe(warning))
	}
	return w.Flush()
}

func writeClientHuman(out io.Writer, command string, data any) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if command == "status" {
		status := data.(downloader.Status)
		fmt.Fprintf(w, "DRIVER\t%s\nENDPOINT\t%s\nVERSION\t%s\nWEB API\t%s\nOBSERVED\t%s\n", terminalSafe(status.Driver), terminalSafe(status.Endpoint), terminalSafe(status.Version), terminalSafe(status.WebAPIVersion), status.ObservedAt.Format(time.RFC3339))
		return w.Flush()
	}
	fmt.Fprintln(w, "HASH\tPROGRESS\tSTATE\tSIZE\tNAME\tSAVE PATH")
	for _, item := range data.([]downloader.Torrent) {
		fmt.Fprintf(w, "%s\t%.1f%%\t%s\t%s\t%s\t%s\n", terminalSafe(item.Hash), item.Progress*100, terminalSafe(item.State), humanBytes(item.SizeBytes), terminalSafe(item.Name), terminalSafe(item.SavePath))
	}
	return w.Flush()
}

func writePlanHuman(out io.Writer, plan seed.Plan) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	ready := "no"
	if plan.ReadyToApply {
		ready = "yes"
	}
	fmt.Fprintf(w, "PLAN\t%s\nTORRENT\t%s\nMETAFILE VARIANT\t%s\nEFFECT\t%s\nREADY TO APPLY\t%s\nREADINESS\t%s\nEVIDENCE\t%s (%d/%d piece proofs)\nSNAPSHOT\t%s\nSTRATEGY\t%s\nSOURCE\t%s\nTARGET\t%s\nREAD\t%s\nWRITE\t%s\n", terminalSafe(plan.ID), terminalSafe(plan.TorrentName), terminalSafe(plan.MetafileVariantID), terminalSafe(plan.Effect), ready, terminalSafe(plan.Readiness), terminalSafe(plan.Evidence), plan.Verification.PiecesMatched, plan.Verification.PiecesExpected, terminalSafe(plan.Verification.SourceSnapshotID), terminalSafe(plan.Strategy), terminalSafe(plan.SourceRoot), terminalSafe(plan.TargetRoot), humanBytes(plan.EstimatedRead), humanBytes(plan.EstimatedWrite))
	fmt.Fprintln(w, "\nBLOCKERS")
	for _, blocker := range plan.Blockers {
		fmt.Fprintf(w, "-\t%s\n", terminalSafe(blocker))
	}
	fmt.Fprintln(w, "\nWARNINGS")
	for _, warning := range plan.Warnings {
		fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
	}
	fmt.Fprintln(w, "\nPLANNED ACTION\tBYTES\tSOURCE\tTARGET")
	for _, operation := range plan.Operations {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", terminalSafe(operation.Kind), operation.Bytes, terminalSafe(operation.Source), terminalSafe(operation.Target))
	}
	return w.Flush()
}

func writeReconciliationHuman(out io.Writer, report reconcile.Report) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	mappingID := report.Scope.PathMappingID
	if mappingID == "" {
		mappingID = "not_requested"
	}
	fmt.Fprintf(w, "PT RECONCILIATION\nOUTCOME\t%s\nEFFECT\t%s\nWRITES\t%d\nASSURANCE\t%s\nPATH MAPPING\t%s\nCLIENT PATH SEMANTICS\t%s\n", terminalSafe(report.Outcome), terminalSafe(strings.Join(report.Effect, "+")), report.WritesPerformed, terminalSafe(report.Assurance), terminalSafe(mappingID), terminalSafe(report.Scope.ClientPathSemantics))

	fmt.Fprintln(w, "\nBLOCKERS\nCODE\tMESSAGE")
	if len(report.Blockers) == 0 {
		fmt.Fprintln(w, "-\tnone")
	} else {
		for _, blocker := range report.Blockers {
			fmt.Fprintf(w, "%s\t%s\n", terminalSafe(blocker.Code), terminalSafe(blocker.Message))
		}
	}

	fmt.Fprintln(w, "\nRELATIONS\nKIND\tSTATUS\tEVIDENCE\tBASIS\tBLOCKERS")
	for _, relation := range report.Relations {
		basis := strings.Join(relation.EvidenceBasis, ",")
		if basis == "" {
			basis = "-"
		}
		blockers := strings.Join(relation.BlockerCodes, ",")
		if blockers == "" {
			blockers = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", terminalSafe(relation.Kind), terminalSafe(relation.Status), terminalSafe(relation.EvidenceLevel), terminalSafe(basis), terminalSafe(blockers))
	}
	fmt.Fprintln(w, "METAFILE VARIANT NOTE\tdownloader APIs do not expose the private raw metafile variant; infohash linkage cannot prove variant equality")
	fmt.Fprintln(w, "PATH NOTE\thost/client namespace projection and content-path comparison are lexical only")

	siteLedger := report.Ledgers.Site
	bindingRecordID := siteLedger.BindingRecordID
	if bindingRecordID == "" {
		bindingRecordID = "-"
	}
	boundRef := "-"
	if siteLedger.Ref != nil {
		boundRef = siteLedger.Ref.SiteID + "/" + siteLedger.Ref.RemoteID
	}
	observedStart, observedEnd := "-", "-"
	if siteLedger.ObservedAtStart != nil {
		observedStart = siteLedger.ObservedAtStart.Format(time.RFC3339Nano)
	}
	if siteLedger.ObservedAtEnd != nil {
		observedEnd = siteLedger.ObservedAtEnd.Format(time.RFC3339Nano)
	}
	fmt.Fprintf(w, "\nSITE BINDING\nREQUESTED\t%t\nSELECTOR\t%s\nSTATUS\t%s\nRECORD ID\t%s\nSITE REF\t%s\nPROCESS-LOCAL PROOF\t%t\nHISTORICAL\t%t\nORIGIN\t%s\nROUTE\t%s\nOBSERVED START\t%s\nOBSERVED END\t%s\n",
		report.Scope.SiteBindingRequested, terminalSafe(report.Scope.SiteBindingSelector), terminalSafe(siteLedger.Status),
		terminalSafe(bindingRecordID), terminalSafe(boundRef), siteLedger.ProcessLocalProof, siteLedger.Historical,
		terminalSafe(valueOrUnknown(siteLedger.Origin)), terminalSafe(valueOrUnknown(siteLedger.RouteID)),
		terminalSafe(observedStart), terminalSafe(observedEnd))
	if siteLedger.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(siteLedger.StopReason))
	}
	detailLedger := siteLedger.Detail
	detailRef := "-"
	if detailLedger.Ref != nil {
		detailRef = detailLedger.Ref.SiteID + "/" + detailLedger.Ref.RemoteID
	}
	detailStart, detailEnd := "-", "-"
	if detailLedger.ObservedAtStart != nil {
		detailStart = detailLedger.ObservedAtStart.Format(time.RFC3339Nano)
	}
	if detailLedger.ObservedAtEnd != nil {
		detailEnd = detailLedger.ObservedAtEnd.Format(time.RFC3339Nano)
	}
	fmt.Fprintf(w, "\nLIVE SITE DETAIL\nREQUESTED\t%t\nSTATUS\t%s\nSITE REF\t%s\nPROCESS-LOCAL PROOF\t%t\nORIGIN\t%s\nROUTE\t%s\nOBSERVED START\t%s\nOBSERVED END\t%s\nREQUESTS\t%d\nAUTOMATIC RETRIES\t%d\nREDIRECTS FOLLOWED\t%d\nRESPONSE BYTES\t%d\n",
		report.Scope.SiteDetailRequested, terminalSafe(detailLedger.Status), terminalSafe(detailRef), detailLedger.ProcessLocalProof,
		terminalSafe(valueOrUnknown(detailLedger.Origin)), terminalSafe(valueOrUnknown(detailLedger.RouteID)),
		terminalSafe(detailStart), terminalSafe(detailEnd), detailLedger.RequestsMade, detailLedger.Used.AutomaticRetries,
		detailLedger.Used.RedirectsFollowed, detailLedger.Used.ResponseBytesRead)
	if detailLedger.Observation != nil {
		fmt.Fprintf(w, "DISPLAY TITLE\t%s\nDOWNLOAD REFERENCE\t%t\n", terminalSafe(detailLedger.Observation.DisplayTitle), detailLedger.Observation.DownloadReferenceObserved)
	}
	if detailLedger.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(detailLedger.StopReason))
	}

	materialized := report.Ledgers.Storage.MaterializedFinal
	materializedOperation, materializedPlan, materializedBasis, materializedAssurance := "-", "-", "-", "-"
	materializedBytes := int64(0)
	if materialized.Observation != nil {
		materializedOperation = shortID(materialized.Observation.OperationID)
		materializedPlan = materialized.Observation.MaterializePlanID
		materializedBasis = materialized.Observation.AuthorityBasis
		materializedAssurance = materialized.Observation.Assurance
		materializedBytes = materialized.Observation.BytesVerified
	}
	fmt.Fprintf(w, "\nMATERIALIZED FINAL\nREQUESTED\t%t\nSTATUS\t%s\nOPERATION\t%s\nPLAN\t%s\nPROCESS-LOCAL FINAL PROOF\t%t\nPROCESS-LOCAL SOURCE BRIDGE\t%t\nBYTES VERIFIED\t%d\nAUTHORITY BASIS\t%s\nASSURANCE\t%s\n",
		report.Scope.MaterializedFinalRequested, terminalSafe(materialized.Status), terminalSafe(materializedOperation), terminalSafe(materializedPlan),
		materialized.ProcessLocalFinalProof, materialized.ProcessLocalSourceBridge, materializedBytes,
		terminalSafe(materializedBasis), terminalSafe(materializedAssurance))
	if materialized.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(materialized.StopReason))
	}

	adoption := report.Ledgers.Adoption
	adoptionAction, adoptionOperation, adoptionPlan, adoptionCompletion, adoptionJob, adoptionState := "-", "-", "-", "-", "-", "-"
	adoptionStart, adoptionEnd, currentAdoptionStart, currentAdoptionEnd := "-", "-", "-", "-"
	adoptionRetained := false
	if adoption.Completion != nil {
		adoptionAction = adoption.Completion.Action
		adoptionOperation = shortID(adoption.Completion.OperationID)
		adoptionPlan = adoption.Completion.PlanID
		adoptionCompletion = shortID(adoption.Completion.CompletionID)
		adoptionStart = adoption.Completion.ObservedAtStart.Format(time.RFC3339Nano)
		adoptionEnd = adoption.Completion.ObservedAtEnd.Format(time.RFC3339Nano)
		adoptionRetained = adoption.Completion.RetainedTombstone
	}
	if adoption.CurrentJob != nil {
		adoptionJob = shortID(adoption.CurrentJob.JobID)
		adoptionState = adoption.CurrentJob.JobState
		currentAdoptionStart = adoption.CurrentJob.ObservedAtStart.Format(time.RFC3339Nano)
		currentAdoptionEnd = adoption.CurrentJob.ObservedAtEnd.Format(time.RFC3339Nano)
	}
	fmt.Fprintf(w, "\nCLIENT ADOPTION\nREQUESTED\t%t\nSTATUS\t%s\nACTION\t%s\nOPERATION\t%s\nPLAN\t%s\nCOMPLETION\t%s\nHISTORICAL\t%t\nRETAINED TOMBSTONE\t%t\nPROCESS-LOCAL COMPLETION PROOF\t%t\nPROCESS-LOCAL CURRENT-JOB BRIDGE\t%t\nCURRENT JOB\t%s\nCURRENT JOB STATE\t%s\nHISTORICAL OBSERVED START\t%s\nHISTORICAL OBSERVED END\t%s\nCURRENT OBSERVED START\t%s\nCURRENT OBSERVED END\t%s\n",
		report.Scope.ClientAdoptionRequested, terminalSafe(adoption.Status), terminalSafe(adoptionAction), terminalSafe(adoptionOperation), terminalSafe(adoptionPlan),
		terminalSafe(adoptionCompletion), adoption.Historical, adoptionRetained, adoption.ProcessLocalCompletionProof,
		adoption.ProcessLocalCurrentJobProof, terminalSafe(adoptionJob), terminalSafe(adoptionState),
		terminalSafe(adoptionStart), terminalSafe(adoptionEnd), terminalSafe(currentAdoptionStart), terminalSafe(currentAdoptionEnd))
	if adoption.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(adoption.StopReason))
	}

	activation := report.Ledgers.Activation
	activationOperation, activationPlan, terminalMarker, terminalPhase := "-", "-", "-", "-"
	activationStart, activationEnd, currentUseID, currentJob, currentLayout := "-", "-", "-", "-", "-"
	if activation.Completion != nil {
		activationOperation = shortID(activation.Completion.OperationID)
		activationPlan = activation.Completion.PlanID
		terminalMarker = shortID(activation.Completion.TerminalMarkerID)
		terminalPhase = activation.Completion.TerminalPhase
		activationStart = activation.Completion.ObservedAtStart.Format(time.RFC3339Nano)
		activationEnd = activation.Completion.ObservedAtEnd.Format(time.RFC3339Nano)
	}
	if activation.CurrentUse != nil {
		currentUseID = shortID(activation.CurrentUse.UseID)
		currentJob = shortID(activation.CurrentUse.JobID)
		currentLayout = shortID(activation.CurrentUse.FileLayoutID)
	} else if activation.CurrentAbsence != nil {
		currentUseID = shortID(activation.CurrentAbsence.UseID)
		currentJob = shortID(activation.CurrentAbsence.JobID)
		currentLayout = shortID(activation.CurrentAbsence.FileLayoutID)
	}
	fmt.Fprintf(w, "\nCLIENT ACTIVATION\nREQUESTED\t%t\nSTATUS\t%s\nOPERATION\t%s\nPLAN\t%s\nTERMINAL MARKER\t%s\nTERMINAL PHASE\t%s\nHISTORICAL\t%t\nPROCESS-LOCAL COMPLETION PROOF\t%t\nPROCESS-LOCAL CURRENT-USE BRIDGE\t%t\nPROCESS-LOCAL ABSENCE BRIDGE\t%t\nCURRENT USE / ABSENCE\t%s\nCURRENT JOB\t%s\nHISTORICAL FILE LAYOUT\t%s\nHISTORICAL OBSERVED START\t%s\nHISTORICAL OBSERVED END\t%s\n",
		report.Scope.ClientActivationRequested, terminalSafe(activation.Status), terminalSafe(activationOperation), terminalSafe(activationPlan),
		terminalSafe(terminalMarker), terminalSafe(terminalPhase), activation.Historical, activation.ProcessLocalCompletionProof,
		activation.ProcessLocalCurrentUseProof, activation.ProcessLocalCurrentAbsenceProof,
		terminalSafe(currentUseID), terminalSafe(currentJob), terminalSafe(currentLayout),
		terminalSafe(activationStart), terminalSafe(activationEnd))
	if activation.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(activation.StopReason))
	}

	removal := report.Ledgers.Removal
	removalOperation, removalPlan, removalCompletion, removalBasis := "-", "-", "-", "-"
	removalStart, removalEnd := "-", "-"
	removalRetained := false
	if removal.Completion != nil {
		removalOperation = shortID(removal.Completion.OperationID)
		removalPlan = removal.Completion.PlanID
		removalCompletion = shortID(removal.Completion.CompletionID)
		removalBasis = removal.Completion.CompletionBasis
		removalStart = removal.Completion.ObservedAtStart.Format(time.RFC3339Nano)
		removalEnd = removal.Completion.ObservedAtEnd.Format(time.RFC3339Nano)
		removalRetained = removal.Completion.RetainedTombstone
	}
	fmt.Fprintf(w, "\nCLIENT REMOVAL (KEEP DATA)\nREQUESTED\t%t\nSTATUS\t%s\nOPERATION\t%s\nPLAN\t%s\nCOMPLETION\t%s\nCOMPLETION BASIS\t%s\nHISTORICAL\t%t\nRETAINED TOMBSTONE\t%t\nPROCESS-LOCAL COMPLETION PROOF\t%t\nHISTORICAL OBSERVED START\t%s\nHISTORICAL OBSERVED END\t%s\n",
		report.Scope.ClientRemovalRequested, terminalSafe(removal.Status), terminalSafe(removalOperation), terminalSafe(removalPlan),
		terminalSafe(removalCompletion), terminalSafe(removalBasis), removal.Historical, removalRetained,
		removal.ProcessLocalCompletionProof, terminalSafe(removalStart), terminalSafe(removalEnd))
	if removal.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(removal.StopReason))
	}

	retirement := report.Ledgers.Retirement
	retirementOperation, retirementPlan, retirementCompletion, retirementAbsence := "-", "-", "-", "-"
	retirementFiles, retirementBytes, retirementParents := 0, int64(0), 0
	retirementStart, retirementEnd := "-", "-"
	retainedTombstone := false
	if retirement.Completion != nil {
		retirementOperation = shortID(retirement.Completion.OperationID)
		retirementPlan = shortID(retirement.Completion.PlanID)
		retirementCompletion = shortID(retirement.Completion.CompletionID)
		retirementFiles, retirementBytes = retirement.Completion.FilesRetired, retirement.Completion.BytesRetired
		retainedTombstone = retirement.Completion.RetainedTombstone
	}
	if retirement.CurrentAbsence != nil {
		retirementAbsence = shortID(retirement.CurrentAbsence.AbsenceID)
		retirementParents = retirement.CurrentAbsence.ParentDirectoriesChecked
		retirementStart = retirement.CurrentAbsence.ObservedAtStart.Format(time.RFC3339Nano)
		retirementEnd = retirement.CurrentAbsence.ObservedAtEnd.Format(time.RFC3339Nano)
	}
	fmt.Fprintf(w, "\nSOURCE RETIREMENT\nREQUESTED\t%t\nSTATUS\t%s\nOPERATION\t%s\nPLAN\t%s\nCOMPLETION\t%s\nHISTORICAL\t%t\nRETAINED TOMBSTONE\t%t\nPROCESS-LOCAL COMPLETION PROOF\t%t\nPROCESS-LOCAL CURRENT-ABSENCE PROOF\t%t\nCURRENT ABSENCE\t%s\nFILES RETIRED / CHECKED\t%d\nBYTES RETIRED\t%d\nPARENT DIRECTORIES CHECKED\t%d\nCURRENT OBSERVED START\t%s\nCURRENT OBSERVED END\t%s\n",
		report.Scope.SourceRetirementRequested, terminalSafe(retirement.Status), terminalSafe(retirementOperation), terminalSafe(retirementPlan),
		terminalSafe(retirementCompletion), retirement.Historical, retainedTombstone, retirement.ProcessLocalCompletionProof,
		retirement.ProcessLocalAbsenceProof, terminalSafe(retirementAbsence), retirementFiles, retirementBytes, retirementParents,
		terminalSafe(retirementStart), terminalSafe(retirementEnd))
	if retirement.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(retirement.StopReason))
	}

	parentCleanup := report.Ledgers.ParentCleanup
	cleanupOperation, cleanupPlan, cleanupCompletion, cleanupAbsence := "-", "-", "-", "-"
	cleanupParents, cleanupFiles := 0, 0
	cleanupStart, cleanupEnd := "-", "-"
	cleanupRetained := false
	if parentCleanup.Completion != nil {
		cleanupOperation = shortID(parentCleanup.Completion.OperationID)
		cleanupPlan = shortID(parentCleanup.Completion.CleanupPlanID)
		cleanupCompletion = shortID(parentCleanup.Completion.CompletionID)
		cleanupParents, cleanupFiles = parentCleanup.Completion.ParentsRemoved, parentCleanup.Completion.RetiredFiles
		cleanupRetained = parentCleanup.Completion.RetainedTombstone
	}
	if parentCleanup.CurrentAbsence != nil {
		cleanupAbsence = shortID(parentCleanup.CurrentAbsence.AbsenceID)
		cleanupStart = parentCleanup.CurrentAbsence.ObservedAtStart.Format(time.RFC3339Nano)
		cleanupEnd = parentCleanup.CurrentAbsence.ObservedAtEnd.Format(time.RFC3339Nano)
	}
	fmt.Fprintf(w, "\nPARENT CLEANUP\nREQUESTED\t%t\nSTATUS\t%s\nOPERATION\t%s\nPLAN\t%s\nCOMPLETION\t%s\nHISTORICAL\t%t\nRETAINED TOMBSTONE\t%t\nPROCESS-LOCAL COMPLETION PROOF\t%t\nPROCESS-LOCAL CURRENT-ABSENCE PROOF\t%t\nCURRENT ABSENCE\t%s\nPARENTS REMOVED / CHECKED\t%d\nRETIRED FILES\t%d\nCURRENT OBSERVED START\t%s\nCURRENT OBSERVED END\t%s\n",
		report.Scope.ParentCleanupRequested, terminalSafe(parentCleanup.Status), terminalSafe(cleanupOperation), terminalSafe(cleanupPlan),
		terminalSafe(cleanupCompletion), parentCleanup.Historical, cleanupRetained, parentCleanup.ProcessLocalCompletionProof,
		parentCleanup.ProcessLocalAbsenceProof, terminalSafe(cleanupAbsence), cleanupParents, cleanupFiles,
		terminalSafe(cleanupStart), terminalSafe(cleanupEnd))
	if parentCleanup.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(parentCleanup.StopReason))
	}

	siteID := "-"
	if report.Ledgers.Site.Ref != nil {
		siteID = report.Ledgers.Site.Ref.SiteID + "/" + report.Ledgers.Site.Ref.RemoteID
	}
	storageID := report.Ledgers.Storage.SelectedSourceID
	if storageID == "" {
		storageID = "-"
	}
	downloaderID := report.Ledgers.Downloader.Driver
	if downloaderID == "" {
		downloaderID = "-"
	}
	fmt.Fprintln(w, "\nLEDGERS\nLEDGER\tSTATUS\tID\tSUMMARY")
	siteSummary := "not requested"
	if report.Ledgers.Site.Status == "declared_unbound" {
		siteSummary = "user declaration only"
	} else if report.Ledgers.Site.ProcessLocalProof {
		siteSummary = "sealed historical exact-response observation; current site mapping unobservable"
	} else if report.Scope.SiteBindingRequested {
		siteSummary = "explicit binding could not be verified"
	}
	if report.Ledgers.Site.Detail.Status == "observed_current_ref" {
		siteSummary += "; live remote-ID detail observed (site claim only)"
	}
	fmt.Fprintf(w, "site\t%s\t%s\t%s\n", terminalSafe(report.Ledgers.Site.Status), terminalSafe(siteID), terminalSafe(siteSummary))
	fmt.Fprintf(w, "metafile\t%s\t%s\t%s; %s\n", terminalSafe(report.Ledgers.Metafile.Status), terminalSafe(shortID(report.Ledgers.Metafile.VariantID)), terminalSafe(report.Ledgers.Metafile.Version), humanBytes(report.Ledgers.Metafile.PhysicalBytes))
	storageSummary := fmt.Sprintf("process-local proof=%t", report.Ledgers.Storage.ProcessLocalProof)
	if materialized.Status != "not_requested" {
		storageSummary += fmt.Sprintf("; materialized-final=%s; source-bridge=%t", materialized.Status, materialized.ProcessLocalSourceBridge)
	}
	fmt.Fprintf(w, "storage\t%s\t%s\t%s\n", terminalSafe(report.Ledgers.Storage.Status), terminalSafe(shortID(storageID)), terminalSafe(storageSummary))
	fmt.Fprintf(w, "downloader\t%s\t%s\trequests=%d; jobs=%d/%d\n", terminalSafe(report.Ledgers.Downloader.Status), terminalSafe(downloaderID), report.Ledgers.Downloader.RequestsMade, report.Ledgers.Downloader.JobsExaminedBefore, report.Ledgers.Downloader.JobsExaminedAfter)
	adoptionSummary := "not requested"
	adoptionID := "-"
	if adoption.Completion != nil {
		adoptionID = shortID(adoption.Completion.OperationID)
		adoptionSummary = fmt.Sprintf("historical=%t; retained=%t; current-job bridge=%t", adoption.Historical, adoption.Completion.RetainedTombstone, adoption.ProcessLocalCurrentJobProof)
	} else if report.Scope.ClientAdoptionRequested {
		adoptionSummary = "explicit terminal stopped adoption could not be verified"
	}
	fmt.Fprintf(w, "client adoption\t%s\t%s\t%s\n", terminalSafe(adoption.Status), terminalSafe(adoptionID), terminalSafe(adoptionSummary))
	activationSummary := "not requested"
	activationID := "-"
	if activation.Completion != nil {
		activationID = shortID(activation.Completion.OperationID)
		activationSummary = fmt.Sprintf("historical=%t; current-use bridge=%t", activation.Historical, activation.ProcessLocalCurrentUseProof)
	} else if report.Scope.ClientActivationRequested {
		activationSummary = "explicit terminal activation could not be verified"
	}
	fmt.Fprintf(w, "client activation\t%s\t%s\t%s\n", terminalSafe(activation.Status), terminalSafe(activationID), terminalSafe(activationSummary))
	removalSummary := "not requested"
	removalID := "-"
	if removal.Completion != nil {
		removalID = shortID(removal.Completion.OperationID)
		removalSummary = fmt.Sprintf("historical=%t; retained=%t; basis=%s", removal.Historical, removal.Completion.RetainedTombstone, removal.Completion.CompletionBasis)
	} else if report.Scope.ClientRemovalRequested {
		removalSummary = "explicit terminal keep-data removal could not be verified"
	}
	fmt.Fprintf(w, "client removal\t%s\t%s\t%s\n", terminalSafe(removal.Status), terminalSafe(removalID), terminalSafe(removalSummary))
	retirementSummary := "not requested"
	retirementID := "-"
	if retirement.Completion != nil {
		retirementID = shortID(retirement.Completion.OperationID)
		retirementSummary = fmt.Sprintf("historical=%t; current-absence=%t; files=%d", retirement.Historical, retirement.ProcessLocalAbsenceProof, retirement.Completion.FilesRetired)
	} else if report.Scope.SourceRetirementRequested {
		retirementSummary = "explicit terminal source retirement could not be verified"
	}
	fmt.Fprintf(w, "source retirement\t%s\t%s\t%s\n", terminalSafe(retirement.Status), terminalSafe(retirementID), terminalSafe(retirementSummary))
	parentCleanupSummary := "not requested"
	parentCleanupID := "-"
	if parentCleanup.Completion != nil {
		parentCleanupID = shortID(parentCleanup.Completion.OperationID)
		parentCleanupSummary = fmt.Sprintf("historical=%t; current-absence=%t; parents=%d", parentCleanup.Historical, parentCleanup.ProcessLocalAbsenceProof, parentCleanup.Completion.ParentsRemoved)
	} else if report.Scope.ParentCleanupRequested {
		parentCleanupSummary = "explicit terminal parent cleanup could not be verified"
	}
	fmt.Fprintf(w, "parent cleanup\t%s\t%s\t%s\n", terminalSafe(parentCleanup.Status), terminalSafe(parentCleanupID), terminalSafe(parentCleanupSummary))

	fileLayout := report.Ledgers.Downloader.FileLayout
	fileStops := strings.Join(fileLayout.StopReasons, ",")
	if fileStops == "" {
		fileStops = "none"
	}
	fmt.Fprintf(w, "\nCLIENT FILE LAYOUT (BOUNDED)\nSTATUS\t%s\nSTABILITY\t%s\nREQUESTS\t%d\nFILES\t%d observed / %d expected\nSELECTED\t%d / %d\nCOMPLETE\t%d / %d\nBEFORE FILES CONSIDERED\t%d / %d\nAFTER FILES CONSIDERED\t%d / %d\nBEFORE PATH BYTES\t%d / %d\nAFTER PATH BYTES\t%d / %d\nBEFORE RESPONSE BYTES\t%d / %d\nAFTER RESPONSE BYTES\t%d / %d\nSTOP REASONS\t%s\n",
		terminalSafe(fileLayout.Status), terminalSafe(fileLayout.StabilityAssurance), fileLayout.RequestsMade,
		fileLayout.FilesObserved, fileLayout.FilesExpected, fileLayout.FilesSelected, fileLayout.FilesExpected, fileLayout.FilesComplete, fileLayout.FilesExpected,
		fileLayout.UsedBefore.FilesConsidered, fileLayout.Limits.MaxFiles, fileLayout.UsedAfter.FilesConsidered, fileLayout.Limits.MaxFiles,
		fileLayout.UsedBefore.PathBytes, fileLayout.Limits.MaxPathBytes, fileLayout.UsedAfter.PathBytes, fileLayout.Limits.MaxPathBytes,
		fileLayout.UsedBefore.ResponseBytes, fileLayout.Limits.MaxResponseBytes, fileLayout.UsedAfter.ResponseBytes, fileLayout.Limits.MaxResponseBytes,
		terminalSafe(fileStops))

	fmt.Fprintln(w, "\nCLIENT FILE FINDINGS (BOUNDED)\nINDEX\tSTATUS\tTORRENT PATH\tEXPECTED BYTES\tCLIENT BYTES\tCLIENT PATH")
	if len(fileLayout.Findings) == 0 {
		fmt.Fprintln(w, "-\tnone\t-\t-\t-\t-")
	} else {
		for _, finding := range fileLayout.Findings {
			clientSize := "-"
			if finding.ClientSize != nil {
				clientSize = strconv.FormatInt(*finding.ClientSize, 10)
			}
			expectedSize := "-"
			if finding.Status != "unexpected_index" {
				expectedSize = strconv.FormatInt(finding.ExpectedSize, 10)
			}
			clientPath := finding.ClientPathRef
			if finding.ClientPath != "" {
				clientPath = finding.ClientPath
			}
			if clientPath == "" {
				clientPath = "-"
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", finding.ManifestIndex, terminalSafe(finding.Status), terminalSafe(valueOrUnknown(finding.TorrentPath)), terminalSafe(expectedSize), terminalSafe(clientSize), terminalSafe(clientPath))
		}
	}
	if fileLayout.FindingOverflow > 0 {
		fmt.Fprintf(w, "...\t%d additional findings omitted by bounded retention\t-\t-\t-\t-\n", fileLayout.FindingOverflow)
	}

	fmt.Fprintln(w, "\nDOWNLOADER MATCHES\nID\tRELATION\tSTATE\tPROGRESS\tSIZE\tCONTENT PATH\tIDENTITY EVIDENCE")
	if len(report.Ledgers.Downloader.Matches) == 0 {
		fmt.Fprintln(w, "-\tnone\t-\t-\t-\t-\t-")
	} else {
		for _, match := range report.Ledgers.Downloader.Matches {
			contentPath := match.ContentPathRef
			if match.ContentPath != "" {
				contentPath = match.ContentPath
			}
			if contentPath == "" {
				contentPath = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%.1f%%\t%d\t%s\t%s\n", terminalSafe(shortID(match.ID)), terminalSafe(match.Relation), terminalSafe(match.State), match.Progress*100, match.SizeBytes, terminalSafe(contentPath), terminalSafe(strings.Join(match.IdentityEvidence, ",")))
		}
	}

	discovery := report.Ledgers.Storage.Discovery
	fmt.Fprintf(w, "\nSTORAGE SCAN\nSOURCE OUTCOME\t%s\nSOURCE SCOPE\t%s\nSCAN COMPLETE\t%t\nVERIFICATION COMPLETE\t%t\nSHARED COMMAND TIME BUDGET\t%s\nENTRIES\t%d / %d\nRETAINED FILES\t%d / %d\nCANDIDATE EDGES OBSERVED\t%d / %d (+1 proves truncation)\nCANDIDATE STATES\t%d / %d\nPROOF BUDGET CHARGED\t%s / %s\n",
		terminalSafe(discovery.SourceOutcome), terminalSafe(discovery.Scan.PathConfinement), discovery.Scan.Complete, discovery.Scan.VerificationComplete,
		(time.Duration(discovery.Scan.TimeBudgetMillis) * time.Millisecond).String(),
		discovery.Scan.InventoryUsed.EntriesExamined, discovery.Scan.InventoryLimits.MaxEntries,
		discovery.Scan.InventoryUsed.CandidatesRetained, discovery.Scan.InventoryLimits.MaxCandidates,
		discovery.Scan.MatchUsed.CandidateEdgesConsidered, discovery.Scan.MatchLimits.MaxCandidateEdges,
		discovery.Scan.MatchUsed.StatesExplored, discovery.Scan.MatchLimits.MaxStates,
		humanBytes(discovery.Scan.MatchUsed.ProofWorkBytesCharged), humanBytes(discovery.Scan.MatchLimits.MaxProofWorkBytes))

	fmt.Fprintln(w, "\nSTORAGE SCAN STOPS")
	if len(discovery.Scan.StopReasons) == 0 {
		fmt.Fprintln(w, "-\tnone")
	} else {
		for _, reason := range discovery.Scan.StopReasons {
			fmt.Fprintf(w, "-\t%s\n", terminalSafe(reason))
		}
	}
	fmt.Fprintln(w, "\nSTORAGE ISSUES\nTYPE\tCODE\tSUBJECT\tMESSAGE")
	if len(discovery.Scan.InventoryIssues) == 0 && len(discovery.Scan.MatchIssues) == 0 {
		fmt.Fprintln(w, "-\tnone\t-\t-")
	} else {
		for _, issue := range discovery.Scan.InventoryIssues {
			subject := issue.RootID
			if issue.RelativePath != "" {
				subject += ":" + issue.RelativePath
			}
			if subject == "" {
				subject = "-"
			}
			fmt.Fprintf(w, "inventory\t%s\t%s\t%s\n", terminalSafe(issue.Code), terminalSafe(subject), terminalSafe(issue.Message))
		}
		for _, issue := range discovery.Scan.MatchIssues {
			subject := shortID(issue.CandidateID)
			if subject == "" {
				subject = "-"
			}
			fmt.Fprintf(w, "verification\t%s\t%s\t%s\n", terminalSafe(issue.Code), terminalSafe(subject), terminalSafe(issue.Message))
		}
	}

	fmt.Fprintln(w, "\nVERIFIED STORAGE MATCHES\nID\tEVIDENCE\tLAYOUT\tFILES")
	if len(discovery.Matches) == 0 {
		fmt.Fprintln(w, "-\tnone\t-\t-")
	} else {
		for _, match := range discovery.Matches {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d/%d\n", terminalSafe(shortID(match.ID)), terminalSafe(match.EvidenceLevel), terminalSafe(match.Layout), match.Coverage.FilesFound, match.Coverage.FilesExpected)
		}
	}
	fmt.Fprintln(w, "\nVERIFIED STORAGE BINDINGS (BOUNDED)\nMATCH\tINDEX\tTORRENT PATH\tSOURCE\tCLIENT PATH")
	bindingsShown := 0
	bindingsOmitted := 0
	for _, match := range discovery.Matches {
		for _, binding := range match.Bindings {
			if bindingsShown >= 20 {
				bindingsOmitted++
				continue
			}
			sourcePath := binding.RelativePath
			if binding.AbsolutePath != "" {
				sourcePath = binding.AbsolutePath
			} else if binding.RootID != "" {
				sourcePath = binding.RootID + ":" + binding.RelativePath
			}
			clientPath := binding.ClientPath
			if clientPath == "" {
				clientPath = "-"
			}
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n", terminalSafe(shortID(match.ID)), binding.FileIndex, terminalSafe(binding.TorrentPath), terminalSafe(sourcePath), terminalSafe(clientPath))
			bindingsShown++
		}
	}
	if bindingsShown == 0 {
		fmt.Fprintln(w, "-\t-\tnone\t-\t-")
	}
	if bindingsOmitted > 0 {
		fmt.Fprintf(w, "...\t-\t%d additional bindings omitted by bounded rendering\t-\t-\n", bindingsOmitted)
	}
	if len(report.Warnings) > 0 {
		fmt.Fprintln(w, "\nWARNINGS")
		for _, warning := range report.Warnings {
			fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
		}
	}
	return w.Flush()
}

func writeDiscoveryHuman(out io.Writer, result seed.DiscoveryResult) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	selected := result.Selection.SelectedID
	if selected == "" {
		selected = "none"
	}
	selectionBasis := result.Selection.Basis
	if selectionBasis == "" {
		selectionBasis = "none"
	}
	selectionScope := result.Selection.ScopeID
	if selectionScope == "" {
		selectionScope = "none"
	}
	fmt.Fprintf(w, "SEED DISCOVERY\nSOURCE OUTCOME\t%s\nSELECTION\t%s\nSELECTION BASIS\t%s\nSELECTION SCOPE\t%s\nHANDOFF\t%s\nPLAN PRODUCED\t%t\nBEST EVIDENCE\t%s\nEFFECT\t%s\nWRITES\t%d\nTORRENT\t%s\nVERSION\t%s\nTIME BUDGET\t%s\nSCAN COMPLETE\t%t\nVERIFICATION COMPLETE\t%t\nENTRIES\t%d / %d\nRETAINED FILES\t%d / %d\nCANDIDATE EDGES OBSERVED\t%d / %d (+1 proves truncation)\nCANDIDATE STATES\t%d / %d\nPROOF BUDGET CHARGED\t%s / %s\nVERIFIED FOUND\t%d\nVERIFIED RETAINED\t%d\nSELECTED\t%s\n",
		terminalSafe(result.SourceOutcome), terminalSafe(result.Selection.Status), terminalSafe(selectionBasis), terminalSafe(selectionScope), terminalSafe(result.Handoff.Status), result.Handoff.PlanProduced,
		terminalSafe(result.BestEvidence), terminalSafe(result.Effect), result.WritesPerformed,
		terminalSafe(result.Torrent.Name), terminalSafe(result.Torrent.Version), (time.Duration(result.Scan.TimeBudgetMillis) * time.Millisecond).String(), result.Scan.Complete, result.Scan.VerificationComplete,
		result.Scan.InventoryUsed.EntriesExamined, result.Scan.InventoryLimits.MaxEntries,
		result.Scan.InventoryUsed.CandidatesRetained, result.Scan.InventoryLimits.MaxCandidates,
		result.Scan.MatchUsed.CandidateEdgesConsidered, result.Scan.MatchLimits.MaxCandidateEdges,
		result.Scan.MatchUsed.StatesExplored, result.Scan.MatchLimits.MaxStates,
		humanBytes(result.Scan.MatchUsed.ProofWorkBytesCharged), humanBytes(result.Scan.MatchLimits.MaxProofWorkBytes),
		result.Scan.MatchUsed.VerifiedLayouts, len(result.Matches), terminalSafe(selected))
	fmt.Fprintln(w, "\nBLOCKERS")
	if len(result.Blockers) == 0 {
		fmt.Fprintln(w, "-\tnone")
	} else {
		for _, blocker := range result.Blockers {
			fmt.Fprintf(w, "%s\t%s\n", terminalSafe(blocker.Code), terminalSafe(blocker.Message))
		}
	}
	fmt.Fprintln(w, "\nSCAN STOPS")
	if len(result.Scan.StopReasons) == 0 {
		fmt.Fprintln(w, "-\tnone")
	} else {
		for _, reason := range result.Scan.StopReasons {
			fmt.Fprintf(w, "-\t%s\n", terminalSafe(reason))
		}
	}
	fmt.Fprintln(w, "\nSCAN ISSUES\nTYPE\tCODE\tSUBJECT\tMESSAGE")
	if len(result.Scan.InventoryIssues) == 0 && len(result.Scan.MatchIssues) == 0 {
		fmt.Fprintln(w, "-\tnone\t-\t-")
	} else {
		for _, issue := range result.Scan.InventoryIssues {
			subject := issue.RootID
			if issue.RelativePath != "" {
				subject += ":" + issue.RelativePath
			}
			if subject == "" {
				subject = "-"
			}
			fmt.Fprintf(w, "inventory\t%s\t%s\t%s\n", terminalSafe(issue.Code), terminalSafe(subject), terminalSafe(issue.Message))
		}
		for _, issue := range result.Scan.MatchIssues {
			subject := shortID(issue.CandidateID)
			if subject == "" {
				subject = "-"
			}
			fmt.Fprintf(w, "verification\t%s\t%s\t%s\n", terminalSafe(issue.Code), terminalSafe(subject), terminalSafe(issue.Message))
		}
	}

	const maxHumanCandidates = 20
	fmt.Fprintln(w, "\nCANDIDATES (EXACT SIZE ONLY; NOT VERIFIED)\nTORRENT PATH\tID\tEVIDENCE\tBASIS\tRANK\tHOST")
	shownCandidates := 0
	omittedCandidates := 0
	for _, file := range result.Files {
		for _, candidate := range file.Candidates {
			if shownCandidates >= maxHumanCandidates {
				omittedCandidates++
				continue
			}
			host := candidate.RootID + ":" + candidate.RelativePath
			if candidate.AbsolutePath != "" {
				host = candidate.AbsolutePath
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", terminalSafe(file.TorrentPath), terminalSafe(shortID(candidate.ID)), terminalSafe(candidate.EvidenceLevel), terminalSafe(candidate.EvidenceBasis), terminalSafe(candidate.MatchRank), terminalSafe(host))
			shownCandidates++
		}
		if file.CandidateCount > len(file.Candidates) {
			omittedCandidates += file.CandidateCount - len(file.Candidates)
		}
	}
	if shownCandidates == 0 {
		fmt.Fprintln(w, "-\tnone\t-\t-\t-\t-")
	}
	if omittedCandidates > 0 {
		fmt.Fprintf(w, "...\t%d additional candidate rows omitted; use JSON\t-\t-\t-\t-\n", omittedCandidates)
	}

	fmt.Fprintln(w, "\nVERIFIED MATCHES\nID\tEVIDENCE\tLAYOUT\tFILES\tHOST\tCLIENT")
	if len(result.Matches) == 0 {
		fmt.Fprintln(w, "-\tnone\t-\t-\t-\t-")
	}
	for _, match := range result.Matches {
		host := fmt.Sprintf("%d paths; use JSON", len(match.Bindings))
		client := "-"
		if len(match.Bindings) == 1 {
			host = match.Bindings[0].RootID + ":" + match.Bindings[0].RelativePath
			if match.Bindings[0].AbsolutePath != "" {
				host = match.Bindings[0].AbsolutePath
			}
			if match.Bindings[0].ClientPath != "" {
				client = match.Bindings[0].ClientPath
			}
		} else if match.Mapping.Status != "not_requested" {
			client = match.Mapping.Status
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d/%d\t%s\t%s\n", terminalSafe(shortID(match.ID)), terminalSafe(match.EvidenceLevel), terminalSafe(match.Layout), match.Coverage.FilesFound, match.Coverage.FilesExpected, terminalSafe(host), terminalSafe(client))
	}
	if result.Plan != nil {
		fmt.Fprintf(w, "\nLAYOUT PLAN\t%s\nEFFECT\t%s\nEVIDENCE\t%s\nREADINESS\t%s\nREADY TO APPLY\t%t\nCLIENT MAPPING\t%s\n", terminalSafe(result.Plan.ID), terminalSafe(result.Plan.Effect), terminalSafe(result.Plan.Evidence), terminalSafe(result.Plan.Readiness), result.Plan.ReadyToApply, terminalSafe(result.Plan.ClientMapping))
		fmt.Fprintln(w, "\nPLAN BLOCKERS")
		if len(result.Plan.Blockers) == 0 {
			fmt.Fprintln(w, "-\tnone")
		} else {
			for _, blocker := range result.Plan.Blockers {
				fmt.Fprintf(w, "-\t%s\n", terminalSafe(blocker))
			}
		}
		fmt.Fprintln(w, "\nPLAN WARNINGS")
		if len(result.Plan.Warnings) == 0 {
			fmt.Fprintln(w, "-\tnone")
		} else {
			for _, warning := range result.Plan.Warnings {
				fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
			}
		}
		fmt.Fprintln(w, "\nPLANNED OPERATIONS\nTORRENT PATH\tACTION\tBYTES\tSOURCE\tTARGET\tCLIENT TARGET")
		for _, operation := range result.Plan.Operations {
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n", terminalSafe(operation.TorrentPath), terminalSafe(operation.Kind), operation.Bytes, terminalSafe(operation.Source), terminalSafe(operation.Target), terminalSafe(operation.ClientTarget))
		}
	}
	if len(result.Warnings) > 0 {
		fmt.Fprintln(w, "\nWARNINGS")
		for _, warning := range result.Warnings {
			fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
		}
	}
	return w.Flush()
}

func shortID(value string) string {
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func humanBytes(value int64) string {
	if value < 1024 {
		return strconv.FormatInt(value, 10) + " B"
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	amount := float64(value)
	unit := "B"
	for _, candidate := range units {
		amount /= 1024
		unit = candidate
		if amount < 1024 {
			break
		}
	}
	return strconv.FormatFloat(amount, 'f', 2, 64) + " " + unit
}

func terminalSafe(value string) string {
	var out strings.Builder
	for _, r := range value {
		if !unsafeTerminalRune(r) {
			out.WriteRune(r)
			continue
		}
		switch r {
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r <= 0xff {
				fmt.Fprintf(&out, `\x%02x`, r)
			} else if r <= 0xffff {
				fmt.Fprintf(&out, `\u%04x`, r)
			} else {
				fmt.Fprintf(&out, `\U%08x`, r)
			}
		}
	}
	return out.String()
}

func unsafeTerminalRune(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x061c || r == 0x200e || r == 0x200f ||
		(r >= 0x202a && r <= 0x202e) || r == 0x2028 || r == 0x2029 || (r >= 0x2066 && r <= 0x2069)
}

func valueOrUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
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
