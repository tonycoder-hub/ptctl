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

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/downloader/qbittorrent"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
	"github.com/tonycoder-hub/ptctl/internal/security"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/tjupt"
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
  ptctl site bonus-catalog --cookie-stdin [--output table|json] SITE
  ptctl site metafile fetch --cookie-stdin --acknowledge-site-effect --metafile-store DIR [--output table|json] SITE REMOTE_ID

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

  ptctl client status --driver qbittorrent --url URL --username USER --password-stdin [--output table|json]
  ptctl client list --driver qbittorrent --url URL --username USER --password-stdin [--output table|json]

  ptctl reconcile report (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) (--search-root PATH... | --state-store DIR --storage-profile PROFILE) [--output table|json]

  ptctl seed plan (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) --source PATH --target PATH [--output table|json]
  ptctl seed discover (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) (--search-root PATH... | --state-store DIR --storage-profile PROFILE) [--target PATH] [--output table|json]
  ptctl version [--output table|json]

Safety defaults:
  * Site operations are one bounded GET per invocation, with no automatic retry.
  * Session cookies are accepted only through stdin and are never persisted.
  * .torrent tracker URLs are reduced to origins; passkeys are never printed.
  * The metafile store preserves exact private bytes with owner-only access and atomic no-clobber commits.
  * v1, v2, and hybrid verification use exact content proofs; names and sizes are not proof.
  * Seed discovery and materialization planning have hard scan/proof budgets and perform no writes.
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
	case "metafile":
		if len(args) >= 2 && args[1] == "fetch" {
			return a.siteMetafileFetch(args[2:])
		}
		return usageError("site metafile requires fetch")
	default:
		return usageError("unknown site subcommand %q", args[0])
	}
}

func (a *app) client(args []string) error {
	if len(args) == 0 || (args[0] != "status" && args[0] != "list") {
		return usageError("client requires status or list")
	}
	command := args[0]
	fs := newFlagSet("client " + command)
	output := fs.String("output", "table", "table or json")
	driverName := fs.String("driver", "qbittorrent", "downloader driver")
	endpoint := fs.String("url", "", "qBittorrent Web API origin")
	username := fs.String("username", "", "qBittorrent username")
	passwordStdin := fs.Bool("password-stdin", false, "read password from stdin")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *endpoint == "" {
		return usageError("client %s requires --url and no positional arguments", command)
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *driverName != "qbittorrent" {
		return usageError("--driver currently supports only qbittorrent")
	}
	if !*passwordStdin {
		return usageError("--password-stdin is required; downloader passwords are never accepted in argv")
	}
	driver, err := qbittorrent.New(*endpoint)
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
		fmt.Fprintln(fs.Output(), "  ptctl reconcile report (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) (--search-root PATH... | --state-store DIR --storage-profile PROFILE) [flags]")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "The report brackets optional downloader reads around bounded, exact storage discovery. It performs zero writes. Downloader identity cannot expose the private metafile variant, and host/client path comparison is lexical only.")
		fmt.Fprintln(fs.Output(), "With --client-file-layout=auto, an eligible multi-file torrent adds at most two bounded file-list reads for one unique exact downloader job. The reads bracket storage proof, share the command timeout, and are never retried.")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Client flags are one optional group: --driver qbittorrent --url URL --username USER --password-stdin. If any is supplied, all are required.")
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
	driverName := fs.String("driver", "qbittorrent", "downloader driver; part of the optional client group")
	endpoint := fs.String("url", "", "qBittorrent Web API origin; part of the optional client group")
	username := fs.String("username", "", "qBittorrent username; part of the optional client group")
	passwordStdin := fs.Bool("password-stdin", false, "read downloader password from stdin; part of the optional client group")
	clientFileLayout := fs.String("client-file-layout", "auto", "multi-file downloader layout reads: auto or off; requires the client group when explicit")
	clientFileDefaults := downloader.DefaultJobFileLedgerLimits()
	maxClientFiles := fs.Int("max-client-files", clientFileDefaults.MaxFiles, "maximum downloader files read for the one exact job; requires the client group when explicit")
	maxClientFilePathBytes := fs.Int64("max-client-file-path-bytes", clientFileDefaults.MaxPathBytes, "maximum cumulative downloader file-path bytes per read; requires the client group when explicit")
	maxClientFileResponseBytes := fs.Int64("max-client-file-response-bytes", clientFileDefaults.MaxResponseBytes, "maximum downloader file-list response bytes per read; requires the client group when explicit")
	hostRoot := fs.String("host-root", "", "optional host namespace root paired with --client-root")
	clientRoot := fs.String("client-root", "", "optional downloader namespace root paired with --host-root")
	clientStyle := fs.String("client-style", "posix", "downloader path style: posix or windows; requires host/client roots")
	siteRefValue := fs.String("site-ref", "", "optional user-declared SITE/REMOTE_ID reference")
	showAbsolute := fs.Bool("show-absolute-paths", false, "include absolute host and downloader paths in output")
	allowNetwork := fs.Bool("allow-network", false, "allow explicit network/UNC search roots")
	timeout := fs.Duration("timeout", time.Hour, "shared downloader, scan, and verification wall-clock budget")
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
	indexedRequested := explicit["state-store"] || explicit["storage-profile"] || explicit["snapshot-record"]
	if len(searchRoots) == 0 && !indexedRequested {
		return usageError("reconcile report requires --search-root or the --state-store/--storage-profile pair")
	}
	if len(searchRoots) > 0 && indexedRequested {
		return usageError("--search-root and stored-profile index mode are mutually exclusive")
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
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > 7*24*time.Hour {
		return usageError("--timeout must be greater than zero and no more than 168h")
	}

	clientFlagNames := []string{"driver", "url", "username", "password-stdin"}
	clientRequested := false
	for _, name := range clientFlagNames {
		clientRequested = clientRequested || explicit[name]
	}
	if clientRequested {
		missing := make([]string, 0, len(clientFlagNames))
		for _, name := range clientFlagNames {
			if !explicit[name] {
				missing = append(missing, "--"+name)
			}
		}
		if len(missing) > 0 {
			return usageError("the optional client group requires %s", strings.Join(missing, ", "))
		}
		if *driverName != "qbittorrent" {
			return usageError("--driver currently supports only qbittorrent")
		}
		if *endpoint == "" || *username == "" || !*passwordStdin {
			return usageError("the optional client group requires non-empty --url and --username plus --password-stdin")
		}
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
	if !indexedRequested && len(searchRoots) > inventoryLimits.MaxRoots {
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

	var clientAdapter *qbittorrent.Adapter
	if clientRequested {
		clientAdapter, err = qbittorrent.New(*endpoint)
		if err != nil {
			return usageError("reconcile report downloader endpoint is invalid: %v", err)
		}
	}
	meta, err := loadMetafileInput(context.Background(), input)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
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
	var clientCredential downloader.Credential
	if clientRequested {
		clientCredential, err = readDownloaderCredential(a.stdin, *username)
		if err != nil {
			return err
		}
	}

	bracket := reconcile.ClientBracket{
		Requested:      clientRequested,
		FileLayoutMode: *clientFileLayout,
		FileLimits:     clientFileLimits,
	}
	var session downloader.LedgerSession
	var fileJobKey string
	fileBeforeComplete := false
	if clientRequested {
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
	if indexedRequested {
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

func reconciliationClientStopReason(ctx context.Context, err error, fallback string) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return "context_cancelled"
	}
	return fallback
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
	credential, err := readCredential(a.stdin)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	var data any
	var warnings []string
	switch command {
	case "status":
		reader, ok := adapter.(site.AuthChecker)
		if !ok {
			return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, capability)
		}
		data, err = reader.CheckSession(ctx, credential)
	case "account":
		reader, ok := adapter.(site.AccountReader)
		if !ok {
			return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, capability)
		}
		data, err = reader.Account(ctx, credential)
	case "search":
		reader, ok := adapter.(site.TorrentSearcher)
		if !ok {
			return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, capability)
		}
		data, err = reader.Search(ctx, credential, strings.Join(fs.Args()[1:], " "))
	case "bonus-catalog":
		reader, ok := adapter.(site.BonusCatalogReader)
		if !ok {
			return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, capability)
		}
		data, err = reader.BonusCatalog(ctx, credential)
		warnings = append(warnings, "catalog is read-only; ptctl never submits purchase or redemption forms")
	}
	if err != nil {
		return err
	}
	if *output == "json" {
		return writeJSON(a.stdout, data, warnings)
	}
	return writeSiteHuman(a.stdout, command, data, warnings)
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
		fmt.Fprintln(fs.Output(), "Live-root discovery can establish current uniqueness. Stored-profile discovery reobserves and exactly verifies historical locators, but remains incomplete and never emits a plan because a snapshot cannot prove current uniqueness. Both modes perform zero writes.")
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
	target := fs.String("target", "", "optional target storage root for a layout-only plan")
	strategy := fs.String("strategy", "copy", "layout plan strategy; only copy is supported")
	showAbsolute := fs.Bool("show-absolute-paths", false, "include absolute host paths in output")
	allowNetwork := fs.Bool("allow-network", false, "allow explicit network/UNC search roots")
	requireVerified := fs.Bool("require-verified", false, "exit 4 unless source_outcome is verified_unique; target handoff does not affect this")
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
	indexedRequested := explicit["state-store"] || explicit["storage-profile"] || explicit["snapshot-record"]
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
		SearchRoots:       append([]string(nil), searchRoots...),
		InventoryLimits:   inventoryLimits,
		MatchLimits:       matchLimits,
		AllowNetwork:      *allowNetwork,
		ShowAbsolutePaths: *showAbsolute,
		TimeBudget:        *timeout,
		TargetRoot:        *target,
		Strategy:          *strategy,
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
	if *requireVerified && result.SourceOutcome != "verified_unique" {
		return &inconclusiveErr{message: "seed discovery source outcome is not verified_unique"}
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
	case domain.BonusCatalog:
		return "site.bonus.catalog"
	case *metafile.MetaInfo:
		return "metafile.manifest"
	case metafileStoreReport:
		return typed.kind
	case siteMetafileFetchReport:
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
		fmt.Fprintf(w, "BALANCE\t%s\n", terminalSafe(valueOrUnknown(catalog.Balance)))
		for index, row := range catalog.Rows {
			fmt.Fprintf(w, "%d\t%s\n", index+1, terminalSafe(strings.Join(row.Columns, " | ")))
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
	fmt.Fprintf(w, "site\t%s\t%s\tuser declaration is not a live site binding\n", terminalSafe(report.Ledgers.Site.Status), terminalSafe(siteID))
	fmt.Fprintf(w, "metafile\t%s\t%s\t%s; %s\n", terminalSafe(report.Ledgers.Metafile.Status), terminalSafe(shortID(report.Ledgers.Metafile.VariantID)), terminalSafe(report.Ledgers.Metafile.Version), humanBytes(report.Ledgers.Metafile.PhysicalBytes))
	fmt.Fprintf(w, "storage\t%s\t%s\tprocess-local proof=%t\n", terminalSafe(report.Ledgers.Storage.Status), terminalSafe(shortID(storageID)), report.Ledgers.Storage.ProcessLocalProof)
	fmt.Fprintf(w, "downloader\t%s\t%s\trequests=%d; jobs=%d/%d\n", terminalSafe(report.Ledgers.Downloader.Status), terminalSafe(downloaderID), report.Ledgers.Downloader.RequestsMade, report.Ledgers.Downloader.JobsExaminedBefore, report.Ledgers.Downloader.JobsExaminedAfter)

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
	fmt.Fprintf(w, "\nSTORAGE SCAN\nSOURCE OUTCOME\t%s\nSCAN COMPLETE\t%t\nVERIFICATION COMPLETE\t%t\nSHARED COMMAND TIME BUDGET\t%s\nENTRIES\t%d / %d\nRETAINED FILES\t%d / %d\nCANDIDATE EDGES OBSERVED\t%d / %d (+1 proves truncation)\nCANDIDATE STATES\t%d / %d\nPROOF BUDGET CHARGED\t%s / %s\n",
		terminalSafe(discovery.SourceOutcome), discovery.Scan.Complete, discovery.Scan.VerificationComplete,
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
	fmt.Fprintf(w, "SEED DISCOVERY\nSOURCE OUTCOME\t%s\nSELECTION\t%s\nHANDOFF\t%s\nPLAN PRODUCED\t%t\nBEST EVIDENCE\t%s\nEFFECT\t%s\nWRITES\t%d\nTORRENT\t%s\nVERSION\t%s\nTIME BUDGET\t%s\nSCAN COMPLETE\t%t\nVERIFICATION COMPLETE\t%t\nENTRIES\t%d / %d\nRETAINED FILES\t%d / %d\nCANDIDATE EDGES OBSERVED\t%d / %d (+1 proves truncation)\nCANDIDATE STATES\t%d / %d\nPROOF BUDGET CHARGED\t%s / %s\nVERIFIED FOUND\t%d\nVERIFIED RETAINED\t%d\nSELECTED\t%s\n",
		terminalSafe(result.SourceOutcome), terminalSafe(result.Selection.Status), terminalSafe(result.Handoff.Status), result.Handoff.PlanProduced,
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
