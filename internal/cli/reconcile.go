package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/clientremove"
	"github.com/tonycoder-hub/ptctl/internal/clientstop"
	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/sitebinding"
	"github.com/tonycoder-hub/ptctl/internal/sourceretire"
	"github.com/tonycoder-hub/ptctl/internal/storage"
	"github.com/tonycoder-hub/ptctl/internal/storageindex"
)

func (a *app) reconcileCommand(args []string) error {
	if len(args) == 0 {
		return usageError("reconcile requires report or refresh-report")
	}
	switch args[0] {
	case "report":
		return a.reconcileReportMode(args[1:], false)
	case "refresh-report":
		return a.reconcileReportMode(args[1:], true)
	default:
		return usageError("reconcile requires report or refresh-report")
	}
}

func (a *app) reconcileReport(args []string) error {
	return a.reconcileReportMode(args, false)
}

func (a *app) reconcileReportMode(args []string, refreshIndex bool) error {
	commandName := "reconcile report"
	if refreshIndex {
		commandName = "reconcile refresh-report"
	}
	fs := newFlagSet(commandName)
	var flagOutput strings.Builder
	fs.SetOutput(&flagOutput)
	fs.Usage = func() {
		if refreshIndex {
			fmt.Fprintln(fs.Output(), "Usage:")
			fmt.Fprintln(fs.Output(), "  ptctl reconcile refresh-report (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) --state-store DIR --storage-profile PROFILE [--driver DRIVER --url URL --username USER --password-stdin] [flags]")
			fmt.Fprintln(fs.Output(), "")
			fmt.Fprintln(fs.Output(), "This explicitly effectful report writes one complete immutable storage-index generation and immediately consumes its process-local authority between optional downloader Before and After snapshots. The refresh must start after Before ends and its live search must end before After starts; endpoint equality is allowed. Materialized-final and terminal workflow modes are unavailable. It may establish current storage uniqueness or absence, but remains sequential and non-atomic. The ordinary reconcile report command remains zero-write.")
			fmt.Fprintln(fs.Output(), "")
			fmt.Fprintln(fs.Output(), "Flags:")
			fs.PrintDefaults()
			return
		}
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  ptctl reconcile report (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) (--source PATH | --search-root PATH... | --state-store DIR --storage-profile PROFILE | --target PATH --materialize-operation ID --materialize-plan-id ID) [--adoption-operation ID --adoption-plan-id ID] [--activation-operation ID --activation-plan-id ID] [--stop-operation ID --stop-plan-id ID] [--removal-operation ID --removal-plan-id ID | --retirement-operation ID --retirement-plan-id ID --retirement-search-root PATH... [--parent-cleanup-operation ID --parent-cleanup-plan-id ID --parent-cleanup-search-root PATH...]] [flags]")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "The report can observe one live site detail page before bracketing optional downloader reads around an explicitly selected exact-layout source or bounded storage discovery. It performs zero writes. Exact --source proves only that selected layout, not filesystem-wide uniqueness. The materialize selector requires one explicit operation and reviewed plan ID, then proves its current exact final namespace before immediately repeating ordinary exact-source verification; these are sequential non-atomic observations. Optional adoption selectors read one canonical terminal client-adoption journal or retained tombstone—whether created by stopped add or observation-only existing-job adoption—and bind its historical completion to the already requested current exact typed job claim without another client request; downloader job incarnation remains unobservable. Optional activation selectors similarly bind one canonical terminal activation journal to the current bracket and, when adoption is also selected, must share that adoption lineage. Optional stop selectors require that activation lineage, read one canonical attributed terminal stop journal or complete retention tombstone before credentials, and bind it to a current exact stopped job in the same bracket without another request; an unknown stop response remains causality-unproven. Optional removal selectors instead require activation, read one canonical terminal keep-data removal journal or retained tombstone, and bind its attributed completion to typed job absence in the existing two-read client bracket. Optional retirement selectors read one canonical terminal source-retirement journal and twice reobserve its exact retired names absent under explicit source roots. Optional parent-cleanup selectors additionally require that retirement proof, read one canonical terminal cleanup journal, and twice reobserve the exact removed parent names absent; a retained cleanup tombstone is historical only and cannot restore path authority. Adoption and removal are mutually exclusive because their current-job predicates conflict; stop and removal are mutually exclusive because their predicates require current presence versus absence; removal and source-retirement terminal modes are also mutually exclusive in this slice. A live detail page is only a current site claim for the remote ID; it cannot expose or prove the private metafile variant, and host/client path comparison remains lexical only.")
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
	stopOperationValue := fs.String("stop-operation", "", "explicit terminal client-stop operation ID; requires activation and materialized-final selectors")
	stopPlanID := fs.String("stop-plan-id", "", "reviewed client-stop plan ID; requires --stop-operation")
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
		return usageError("%s: %s", commandName, detail)
	}
	if fs.NArg() != 0 {
		return usageError("%s accepts flags only; unexpected argument %q", commandName, fs.Arg(0))
	}
	explicit := make(map[string]bool)
	fs.Visit(func(item *flag.Flag) { explicit[item.Name] = true })
	exactRequested := explicit["source"]
	indexedRequested := explicit["state-store"] || explicit["storage-profile"] || explicit["snapshot-record"]
	materializedRequested := explicit["target"] || explicit["materialize-operation"] || explicit["materialize-plan-id"]
	adoptionRequested := explicit["adoption-operation"] || explicit["adoption-plan-id"]
	activationRequested := explicit["activation-operation"] || explicit["activation-plan-id"]
	stopRequested := explicit["stop-operation"] || explicit["stop-plan-id"]
	removalRequested := explicit["removal-operation"] || explicit["removal-plan-id"]
	retirementRequested := explicit["retirement-operation"] || explicit["retirement-plan-id"] || explicit["retirement-search-root"] || explicit["retirement-allow-network"]
	parentCleanupRequested := explicit["parent-cleanup-operation"] || explicit["parent-cleanup-plan-id"] || explicit["parent-cleanup-search-root"] || explicit["parent-cleanup-allow-network"]
	if refreshIndex {
		if !explicit["state-store"] || !explicit["storage-profile"] || *stateStore == "" || *storageProfile == "" {
			return usageError("reconcile refresh-report requires non-empty --state-store and --storage-profile")
		}
		if explicit["snapshot-record"] {
			return usageError("--snapshot-record selects historical evidence and is unavailable for reconcile refresh-report")
		}
		if exactRequested || len(searchRoots) > 0 || materializedRequested || adoptionRequested || activationRequested || stopRequested || removalRequested || retirementRequested || parentCleanupRequested {
			return usageError("reconcile refresh-report uses only the immutable storage profile as its storage scope")
		}
	}
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
	if stopRequested && (!explicit["stop-operation"] || !explicit["stop-plan-id"] || *stopOperationValue == "" || *stopPlanID == "") {
		return usageError("client-stop mode requires non-empty --stop-operation and --stop-plan-id")
	}
	if removalRequested && (!explicit["removal-operation"] || !explicit["removal-plan-id"] || *removalOperationValue == "" || *removalPlanID == "") {
		return usageError("client-removal mode requires non-empty --removal-operation and --removal-plan-id")
	}
	if removalRequested && retirementRequested {
		return usageError("client-removal and source-retirement reconciliation modes are mutually exclusive")
	}
	if stopRequested && removalRequested {
		return usageError("client-stop and client-removal reconciliation modes are mutually exclusive")
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
	var stopOperation clientstop.OperationID
	if stopRequested {
		parsedOperation, parseErr := clientstop.ParseOperationID(*stopOperationValue)
		if parseErr != nil {
			return usageError("--stop-operation requires a canonical operation ID")
		}
		if !validMaterializePlanID(*stopPlanID) {
			return usageError("--stop-plan-id requires a canonical reviewed plan ID")
		}
		if clientstop.OperationIDForPlan(*stopPlanID) != parsedOperation {
			return usageError("--stop-operation does not match --stop-plan-id")
		}
		stopOperation = parsedOperation
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
	input, err := flaggedMetafileInput(commandName, *torrentPath, *storeRoot, *variantID, explicit["torrent"], explicit["metafile-store"], explicit["metafile-variant"])
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
		return usageError("%s: %v", commandName, err)
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
		return usageError("%s: %v", commandName, err)
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
		return usageError("%s: %v", commandName, err)
	}
	if !indexedRequested && !exactRequested && !materializedRequested && len(searchRoots) > inventoryLimits.MaxRoots {
		return usageError("reconcile report accepts at most %d --search-root values", inventoryLimits.MaxRoots)
	}
	if err := matchLimits.Validate(); err != nil {
		return usageError("%s: %v", commandName, err)
	}
	if mappingRootsRequested {
		if err := storage.ValidatePathMappingConfig(*hostRoot, *clientRoot, *clientStyle == "windows"); err != nil {
			return usageError("%s path mapping is invalid: %v", commandName, err)
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
	if stopRequested {
		if !activationRequested || !materializedRequested || !clientRequested || !mappingRootsRequested {
			return usageError("client-stop reconciliation requires the complete activation, materialized-final, client, and host/client mapping selectors")
		}
		if *clientFileLayout != "auto" || clientFileLimits != downloader.DefaultJobFileLedgerLimits() {
			return usageError("client-stop reconciliation requires --client-file-layout=auto and the default bounded client file limits")
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
			return usageError("%s downloader endpoint is invalid: %v", commandName, err)
		}
		if adoptionRequested || activationRequested {
			configured, ok := clientAdapter.(configuredLedgerDriver)
			if !ok {
				return usageError("%s downloader configuration identity is unavailable", commandName)
			}
			clientConfigID, err = configured.ClientConfigID(*username)
			if err != nil {
				return usageError("%s downloader configuration is invalid", commandName)
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
	refreshManifestBudgetBlocked := refreshIndex && len(meta.Files) > matchLimits.MaxStates
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
	stopSelection := reconcile.ClientStopSelection{Requested: stopRequested}
	stopGateFailed := false
	stopIntegrityFailed := false
	if stopRequested {
		if activationGateFailed || verifiedActivation == nil {
			stopSelection.StopReason = "stop_prerequisite_unavailable"
			stopGateFailed = true
		} else {
			stopSelection.CompletionAttempted = true
			verified, observation, completionErr := clientstop.VerifyCompletion(ctx, clientstop.CompletionProofOptions{
				TargetRoot: *materializeTarget, OperationID: stopOperation, ExpectedPlanID: *stopPlanID,
			})
			if completionErr != nil {
				stopSelection.StopReason = reconciliationStopCompletionReason(ctx, completionErr)
				stopGateFailed = true
				stopIntegrityFailed = stopSelection.StopReason == "stop_completion_integrity_failed"
			} else {
				stopSelection.Completion = verified
				plan := verified.Plan()
				activationObservation := verifiedActivation.Observation()
				mappingID := reconcile.PathMappingFingerprint(reconcile.PathMappingOptions{
					HostRoot: *hostRoot, ClientRoot: *clientRoot, ClientWindows: *clientStyle == "windows",
				})
				if observation.MetafileVariantID != meta.MetafileVariantID || plan.Driver != *driverName ||
					plan.ClientConfigID != clientConfigID || plan.PathMappingID != mappingID || plan.FileLimits != clientFileLimits ||
					plan.MaterializeOperationID != materializeOperation.String() || plan.MaterializePlanID != *materializePlanID ||
					plan.ActivationOperationID != activationOperation.String() || plan.ActivationPlanID != *activationPlanID ||
					plan.ActivationTerminalID != activationObservation.TerminalMarkerID {
					stopSelection.StopReason = "stop_completion_selector_mismatch"
					stopGateFailed = true
				} else if observation.CompletionBasis != "accepted_response_then_exact_stopped" {
					stopSelection.StopReason = "stop_causality_unproven"
					stopGateFailed = true
				} else {
					stopSelection.CurrentStopped = verified
				}
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
	var storageIndexGateErr error
	if indexedRequested {
		store, openErr := metastore.Open(*stateStore)
		if openErr != nil {
			storageIndexGateErr = openErr
		} else {
			indexedRepository, storageIndexGateErr = storageindex.NewRepository(store, storageindex.DefaultLimits())
			if storageIndexGateErr == nil {
				profileSelection, selectionErr := indexedRepository.SelectProfile(ctx, *storageProfile)
				if selectionErr != nil {
					storageIndexGateErr = selectionErr
				} else if liveErr := storageindex.ValidateProfileForLiveUse(profileSelection.Profile, storageindex.DefaultLimits()); liveErr != nil {
					storageIndexGateErr = liveErr
				} else {
					indexedProfile = profileSelection.Profile
					if refreshIndex {
						inventoryLimits = discoveryInventoryLimitsFromProfile(indexedProfile, storageindex.CandidateLimits{
							MaxCandidates: inventoryLimits.MaxCandidates, MaxPathBytes: inventoryLimits.MaxPathBytes, MaxIssues: inventoryLimits.MaxIssues,
						})
						storageIndexGateErr = inventoryLimits.Validate()
					} else {
						snapshotSelection, selectionErr := indexedRepository.SelectSnapshot(ctx, profileSelection.Profile, explicitDescriptor)
						if selectionErr != nil {
							storageIndexGateErr = selectionErr
						} else {
							indexedDescriptorID = snapshotSelection.DescriptorRecordID
						}
					}
				}
			}
		}
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
	if !refreshManifestBudgetBlocked && storageIndexGateErr == nil && !siteBindingGateFailed && !adoptionGateFailed && !activationGateFailed && !stopGateFailed && !removalGateFailed && !retirementGateFailed && !parentCleanupGateFailed {
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
		if refreshManifestBudgetBlocked {
			siteDetailSelection.StopReason = "site_detail_skipped_by_storage_preflight"
		} else if storageIndexGateErr != nil {
			siteDetailSelection.StopReason = "site_detail_skipped_by_storage_index_gate"
		} else if siteBindingGateFailed {
			siteDetailSelection.StopReason = "site_detail_skipped_by_binding_gate"
		} else if adoptionGateFailed || activationGateFailed || stopGateFailed || removalGateFailed || retirementGateFailed || parentCleanupGateFailed {
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
	if clientRequested && (refreshManifestBudgetBlocked || storageIndexGateErr != nil || siteBindingGateFailed || adoptionGateFailed || activationGateFailed || stopGateFailed || removalGateFailed || retirementGateFailed || parentCleanupGateFailed) {
		bracket.StopReason = "client_snapshot_incomplete"
	}
	var session downloader.LedgerSession
	var fileJobKey string
	fileBeforeComplete := false
	if clientRequested && !refreshManifestBudgetBlocked && storageIndexGateErr == nil && !siteBindingGateFailed && !adoptionGateFailed && !activationGateFailed && !stopGateFailed && !removalGateFailed && !retirementGateFailed {
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
	if storageIndexGateErr != nil {
		discovery = reconciliationStorageIndexUnavailableDiscovery(meta, discoverOptions, refreshIndex, reconciliationStorageIndexIntegrityError(storageIndexGateErr))
		discoveryErr = storageIndexGateErr
	} else if materializedRequested {
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
	} else if refreshIndex {
		candidateLimits := storageindex.DefaultCandidateLimits()
		candidateLimits.MaxCandidates = inventoryLimits.MaxCandidates
		candidateLimits.MaxPathBytes = inventoryLimits.MaxPathBytes
		candidateLimits.MaxIssues = inventoryLimits.MaxIssues
		discovery, discoveryErr = a.refreshAndDiscoverFromIndex(ctx, indexedRepository, meta, indexedProfile, candidateLimits, discoverOptions, storageindex.RefreshOptions{})
	} else if indexedRequested {
		candidateLimits := storageindex.DefaultCandidateLimits()
		candidateLimits.MaxCandidates = inventoryLimits.MaxCandidates
		candidateLimits.MaxPathBytes = inventoryLimits.MaxPathBytes
		candidateLimits.MaxIssues = inventoryLimits.MaxIssues
		indexed, queryErr := indexedRepository.LoadCandidates(ctx, indexedProfile, indexedDescriptorID, wantedMetafileSizes(meta), candidateLimits)
		if queryErr != nil {
			discovery = reconciliationStorageIndexUnavailableDiscovery(meta, discoverOptions, false, reconciliationStorageIndexIntegrityError(queryErr))
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
	if discoveryErr != nil && !refreshIndex && !indexedRequested {
		return discoveryErr
	}
	if discoveryErr != nil && indexedRequested && reconciliationStorageIndexIntegrityError(discoveryErr) {
		discovery = reconciliationStorageIndexIntegrityDiscovery(discovery)
	}
	verifiedSource, _ := discovery.VerifiedSource(meta)
	report, err := reconcile.Build(reconcile.BuildInput{
		Meta:                         meta,
		Discovery:                    discovery,
		VerifiedSource:               verifiedSource,
		StorageIndexRefreshRequested: refreshIndex,
		Client:                       bracket,
		SiteRef:                      siteRef,
		SiteBinding:                  siteSelection,
		SiteDetail:                   siteDetailSelection,
		MaterializedFinal:            materializedSelection,
		ClientAdoption:               adoptionSelection,
		ClientActivation:             activationSelection,
		ClientStop:                   stopSelection,
		ClientRemoval:                removalSelection,
		SourceRetirement:             retirementSelection,
		ParentCleanup:                parentCleanupSelection,
		PathMapping:                  reportMapping,
		ShowAbsolutePaths:            *showAbsolute,
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
	if discoveryErr != nil && reconciliationStorageIndexIntegrityError(discoveryErr) {
		return &integrityErr{message: "the selected storage index or sealed object failed integrity verification; see report"}
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
	if stopIntegrityFailed {
		return &integrityErr{message: "the explicit terminal client-stop journal failed integrity verification"}
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
	if discoveryErr != nil {
		return discoveryErr
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

func reconciliationStorageIndexIntegrityError(err error) bool {
	return errors.Is(err, storageindex.ErrIntegrity) || errors.Is(err, metastore.ErrCorruptRecord) ||
		errors.Is(err, metastore.ErrRecordConsumerIncomplete) || errors.Is(err, metastore.ErrCorruptArtifact)
}

func reconciliationStorageIndexUnavailableDiscovery(meta *metafile.MetaInfo, options seed.DiscoverOptions, refreshRequested, integrityFailure bool) seed.DiscoveryResult {
	effect := "read_private_storage_index+read_live_storage_metadata+read_content"
	if refreshRequested {
		effect = "read_storage_metadata+read_content"
	}
	physicalBytes := int64(0)
	for _, file := range meta.Files {
		if !strings.Contains(file.Attribute, "p") {
			physicalBytes += file.Length
		}
	}
	result := seed.DiscoveryResult{
		Effect: effect, WritesPerformed: 0, AbsolutePathsShown: options.ShowAbsolutePaths,
		Torrent: seed.DiscoveryTorrent{
			Name: meta.Name, Version: meta.Version, MetafileVariantID: meta.MetafileVariantID,
			InfoHashV1: meta.InfoHashV1, InfoHashV2: meta.InfoHashV2, PhysicalBytes: physicalBytes, Files: len(meta.Files),
		},
		SourceOutcome: "incomplete", Selection: seed.DiscoverySelection{Status: "blocked"},
		Handoff: seed.DiscoveryHandoff{Status: "blocked"}, BestEvidence: "none",
		Scan: seed.DiscoveryScan{
			Complete: false, VerificationComplete: false, TimeBudgetMillis: options.TimeBudget.Milliseconds(),
			PathConfinement: "not_started_storage_index_unavailable", SearchRoots: []seed.DiscoveryRoot{},
			InventoryLimits: options.InventoryLimits, MatchLimits: options.MatchLimits,
			StopReasons: []string{"storage_index_unavailable"}, InventoryIssues: []storage.ScanIssue{}, MatchIssues: []metafile.SourceMatchIssue{},
		},
		Files: []seed.DiscoveryFile{}, Matches: []seed.DiscoveryMatch{},
		Blockers: []seed.DiscoveryBlocker{{
			Code: "index.storage_index_unavailable", Message: "the selected storage index could not be loaded and verified in this invocation",
		}},
		Warnings: []string{"stored index records and selectors are historical evidence only and were not accepted as current content proof"},
	}
	if integrityFailure {
		return reconciliationStorageIndexIntegrityDiscovery(result)
	}
	return result
}

func reconciliationStorageIndexIntegrityDiscovery(result seed.DiscoveryResult) seed.DiscoveryResult {
	result.SourceOutcome = "integrity_failed"
	result.Selection = seed.DiscoverySelection{Status: "blocked"}
	result.Handoff = seed.DiscoveryHandoff{Status: "blocked"}
	result.BestEvidence = "none"
	result.Blockers = append(result.Blockers, seed.DiscoveryBlocker{
		Code: "index.storage_index_integrity_failed", Message: "the selected storage index failed structural or binding integrity verification",
	})
	return result
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

func reconciliationStopCompletionReason(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), ctx.Err() != nil:
		return "stop_context_cancelled"
	case errors.Is(err, clientstop.ErrIntegrity):
		return "stop_completion_integrity_failed"
	case errors.Is(err, clientstop.ErrPolicy), errors.Is(err, clientstop.ErrOperationNotFound):
		return "stop_completion_load_failed"
	default:
		return "stop_completion_load_failed"
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
	descriptorOrigin, err := site.DescriptorOrigin(descriptor)
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
