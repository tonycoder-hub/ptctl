package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/storage"
	"github.com/tonycoder-hub/ptctl/internal/storageindex"
)

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
	if result.IndexRefresh != nil {
		inventoryInterval := "-"
		if !result.IndexRefresh.ObservedAtStart.IsZero() && !result.IndexRefresh.ObservedAtEnd.IsZero() {
			inventoryInterval = result.IndexRefresh.ObservedAtStart.UTC().Format(time.RFC3339Nano) + " .. " + result.IndexRefresh.ObservedAtEnd.UTC().Format(time.RFC3339Nano)
		}
		currentSearchEnd := "-"
		if !result.IndexRefresh.CurrentSearchObservedAtEnd.IsZero() {
			currentSearchEnd = result.IndexRefresh.CurrentSearchObservedAtEnd.UTC().Format(time.RFC3339Nano)
		}
		fmt.Fprintln(w, "\nINDEX REFRESH")
		fmt.Fprintf(w, "STATUS\t%s\nWRITES\t%d\nPROFILE\t%s\nGENERATION\t%d\nSNAPSHOT\t%s\nDATA RECORD\t%s\nDESCRIPTOR RECORD\t%s\nINVENTORY INTERVAL\t%s\nFILES OBSERVED\t%d\nPATH BYTES\t%s\nASSURANCE\t%s\nCURRENT SEARCH\t%s\nCURRENT SEARCH END\t%s\nCURRENT SEARCH ASSURANCE\t%s\nCANDIDATES\t%d / %d\nCANDIDATE PATH BYTES\t%s / %s\n",
			terminalSafe(result.IndexRefresh.Status), result.IndexRefresh.WritesPerformed,
			terminalSafe(result.IndexRefresh.ProfileID), result.IndexRefresh.Generation,
			terminalSafe(result.IndexRefresh.SnapshotID), terminalSafe(result.IndexRefresh.DataRecord.ID.String()),
			terminalSafe(result.IndexRefresh.DescriptorRecord.ID.String()), terminalSafe(inventoryInterval), result.IndexRefresh.ScanUsed.FilesEmitted,
			humanBytes(result.IndexRefresh.ScanUsed.EmittedPathBytes), terminalSafe(result.IndexRefresh.Assurance),
			terminalSafe(result.IndexRefresh.CurrentSearchStatus), terminalSafe(currentSearchEnd), terminalSafe(result.IndexRefresh.CurrentSearchAssurance),
			result.IndexRefresh.CandidateUsed.CandidatesRetained, result.IndexRefresh.CandidateLimits.MaxCandidates,
			humanBytes(result.IndexRefresh.CandidateUsed.RetainedPathBytes), humanBytes(result.IndexRefresh.CandidateLimits.MaxPathBytes))
		if len(result.IndexRefresh.StopReasons) > 0 {
			fmt.Fprintf(w, "STOP REASONS\t%s\n", terminalSafe(strings.Join(result.IndexRefresh.StopReasons, ",")))
		}
		if len(result.IndexRefresh.CandidateStopReasons) > 0 {
			fmt.Fprintf(w, "CANDIDATE STOPS\t%s\n", terminalSafe(strings.Join(result.IndexRefresh.CandidateStopReasons, ",")))
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
