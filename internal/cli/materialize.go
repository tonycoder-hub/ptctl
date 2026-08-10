package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

const (
	materializeExecutionDefaultTimeout = 24 * time.Hour
	materializeExecutionMaxTimeout     = 7 * 24 * time.Hour
	materializeControlDefaultTimeout   = time.Minute
	materializeControlMaxTimeout       = time.Hour
)

type materializeExecutionFlags struct {
	output         *string
	torrentPath    *string
	storeRoot      *string
	variantID      *string
	searchRoots    stringListFlag
	targetRoot     *string
	expectedPlanID *string
	acknowledge    *bool
	allowNetwork   *bool
	timeout        *time.Duration

	maxDepth            *int
	maxDirectories      *int
	maxEntries          *int
	maxDirectoryEntries *int
	maxCandidates       *int
	maxPathBytes        *int64

	maxCandidatesPerFile *int
	maxCandidateEdges    *int
	maxStates            *int
	maxVerifiedLayouts   *int
	maxProofBytes        *int64
}

func (a *app) seedMaterialize(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		a.seedMaterializeHelp()
		return nil
	}
	switch args[0] {
	case "run":
		return a.seedMaterializeRun(args[1:])
	case "resume":
		return a.seedMaterializeResume(args[1:])
	case "status":
		return a.seedMaterializeStatus(args[1:])
	case "abandon":
		return a.seedMaterializeAbandon(args[1:])
	case "prune":
		return a.seedMaterializePrune(args[1:])
	default:
		return usageError("unknown seed materialize subcommand %q", args[0])
	}
}

func (a *app) seedMaterializeHelp() {
	fmt.Fprint(a.stdout, `Usage:
  ptctl seed materialize run (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) --search-root PATH [--search-root PATH...] --target PATH --expect-plan-id ID --acknowledge-filesystem-write [flags]
  ptctl seed materialize resume (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) --target PATH --expect-plan-id ID --acknowledge-filesystem-write [--search-root PATH...] [flags] OPERATION_ID
  ptctl seed materialize status --target PATH [flags] [OPERATION_ID]
  ptctl seed materialize abandon --target PATH --acknowledge-abandon [flags] OPERATION_ID
  ptctl seed materialize prune --target PATH --expect-plan-id ID --acknowledge-operation-state-deletion [--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID] [flags] OPERATION_ID

Materialize is copy-only and target-root-local. Run requires one complete live
discovery and the reviewed plan ID from seed discover --target with the same
selector, search roots, and target. The standalone seed plan ID is not an
execution selector. Resume reads fresh source roots only for a journaled or
partially staged operation. Status without an operation ID performs a bounded
name-only listing and never selects a latest operation. Abandon is terminal,
writes one journal event, retains staged and scratch bytes, and never deletes
source, staging, or published content.

Prune is the only deletion-capable materialize command. It accepts exactly one
terminal operation, deletes only its owner-private heavy state, never touches
the source or final layout, and retains an exact private tombstone. A newly
started committed prune requires the exact metafile; an abandoned operation or
a retry with a durable retention intent does not.

Reports never include absolute source, target, journal, scratch, or staging paths.
Use each subcommand's --help for its complete bounded flag surface.
`)
}

func addMaterializeExecutionFlags(fs *flag.FlagSet) *materializeExecutionFlags {
	values := &materializeExecutionFlags{}
	values.output = fs.String("output", "table", "table or json")
	values.torrentPath = fs.String("torrent", "", "metafile path")
	values.storeRoot = fs.String("metafile-store", "", "private metafile store root; pair with --metafile-variant")
	values.variantID = fs.String("metafile-variant", "", "whole-metafile sha256 artifact ID; pair with --metafile-store")
	fs.Var(&values.searchRoots, "search-root", "live source root to scan; repeatable")
	values.targetRoot = fs.String("target", "", "existing local target storage root")
	values.expectedPlanID = fs.String("expect-plan-id", "", "reviewed 24-hex seed-discover target plan ID")
	values.acknowledge = fs.Bool("acknowledge-filesystem-write", false, "acknowledge private journal, staging, and target-layout writes")
	values.allowNetwork = fs.Bool("allow-network", false, "allow explicit network/UNC source roots; never applies to target")
	values.timeout = fs.Duration("timeout", materializeExecutionDefaultTimeout, "shared discovery and materialization wall-clock budget")

	inventoryDefaults := storage.DefaultInventoryLimits()
	values.maxDepth = fs.Int("max-depth", inventoryDefaults.MaxDepth, "maximum live discovery directory depth")
	values.maxDirectories = fs.Int("max-directories", inventoryDefaults.MaxDirectories, "maximum live discovery directories opened")
	values.maxEntries = fs.Int("max-entries", inventoryDefaults.MaxEntries, "maximum live discovery entries examined")
	values.maxDirectoryEntries = fs.Int("max-directory-entries", inventoryDefaults.MaxEntriesPerDirectory, "maximum entries accepted from one directory")
	values.maxCandidates = fs.Int("max-candidates", inventoryDefaults.MaxCandidates, "maximum matching regular files retained")
	values.maxPathBytes = fs.Int64("max-path-bytes", inventoryDefaults.MaxPathBytes, "maximum retained discovery relative-path bytes")

	matchDefaults := metafile.DefaultSourceMatchLimits()
	values.maxCandidatesPerFile = fs.Int("max-candidates-per-file", matchDefaults.MaxCandidatesPerFile, "maximum candidates explored for one torrent file")
	values.maxCandidateEdges = fs.Int("max-candidate-edges", matchDefaults.MaxCandidateEdges, "maximum manifest-to-candidate edges considered")
	values.maxStates = fs.Int("max-states", matchDefaults.MaxStates, "maximum candidate assignment states")
	values.maxVerifiedLayouts = fs.Int("max-verified-layouts", matchDefaults.MaxVerifiedLayouts, "maximum verified alternatives retained")
	values.maxProofBytes = fs.Int64("max-proof-bytes", matchDefaults.MaxProofWorkBytes, "maximum physical and virtual bytes charged to discovery proof work")
	return values
}

func (values *materializeExecutionFlags) validate(fs *flag.FlagSet, command string, rootsRequired bool) (metafileInput, seed.DiscoverOptions, error) {
	var input metafileInput
	if *values.targetRoot == "" {
		return input, seed.DiscoverOptions{}, usageError("%s requires --target", command)
	}
	if !*values.acknowledge {
		return input, seed.DiscoverOptions{}, usageError("%s requires --acknowledge-filesystem-write", command)
	}
	if !validMaterializePlanID(*values.expectedPlanID) {
		return input, seed.DiscoverOptions{}, usageError("%s requires a canonical 24-hex --expect-plan-id", command)
	}
	if err := validateOutput(*values.output); err != nil {
		return input, seed.DiscoverOptions{}, err
	}
	if *values.timeout <= 0 || *values.timeout > materializeExecutionMaxTimeout {
		return input, seed.DiscoverOptions{}, usageError("--timeout must be greater than zero and no more than 168h")
	}
	if rootsRequired && len(values.searchRoots) == 0 {
		return input, seed.DiscoverOptions{}, usageError("%s requires at least one --search-root", command)
	}
	for _, root := range values.searchRoots {
		if root == "" {
			return input, seed.DiscoverOptions{}, usageError("%s requires every --search-root to be non-empty", command)
		}
	}
	if len(values.searchRoots) == 0 && materializeDiscoveryFlagExplicit(fs) {
		return input, seed.DiscoverOptions{}, usageError("live discovery flags require at least one --search-root")
	}

	inventoryLimits := storage.DefaultInventoryLimits()
	inventoryLimits.MaxDepth = *values.maxDepth
	inventoryLimits.MaxDirectories = *values.maxDirectories
	inventoryLimits.MaxEntries = *values.maxEntries
	inventoryLimits.MaxEntriesPerDirectory = *values.maxDirectoryEntries
	inventoryLimits.MaxCandidates = *values.maxCandidates
	inventoryLimits.MaxPathBytes = *values.maxPathBytes
	if err := inventoryLimits.Validate(); err != nil {
		return input, seed.DiscoverOptions{}, usageError("%s discovery limits are invalid: %v", command, err)
	}
	if len(values.searchRoots) > inventoryLimits.MaxRoots {
		return input, seed.DiscoverOptions{}, usageError("%s accepts at most %d --search-root values", command, inventoryLimits.MaxRoots)
	}

	matchLimits := metafile.DefaultSourceMatchLimits()
	matchLimits.MaxCandidatesPerFile = *values.maxCandidatesPerFile
	matchLimits.MaxCandidateEdges = *values.maxCandidateEdges
	matchLimits.MaxStates = *values.maxStates
	matchLimits.MaxVerifiedLayouts = *values.maxVerifiedLayouts
	matchLimits.MaxProofWorkBytes = *values.maxProofBytes
	if err := matchLimits.Validate(); err != nil {
		return input, seed.DiscoverOptions{}, usageError("%s proof limits are invalid: %v", command, err)
	}
	if err := materialize.DefaultLimits().Validate(); err != nil {
		return input, seed.DiscoverOptions{}, fmt.Errorf("materialize default limits are invalid")
	}

	parsedInput, err := flaggedMetafileInput(command, *values.torrentPath, *values.storeRoot, *values.variantID,
		flagWasSet(fs, "torrent"), flagWasSet(fs, "metafile-store"), flagWasSet(fs, "metafile-variant"))
	if err != nil {
		return input, seed.DiscoverOptions{}, err
	}
	return parsedInput, seed.DiscoverOptions{
		SearchRoots: append([]string(nil), values.searchRoots...), InventoryLimits: inventoryLimits,
		MatchLimits: matchLimits, AllowNetwork: *values.allowNetwork, ShowAbsolutePaths: false,
		TimeBudget: *values.timeout, TargetRoot: *values.targetRoot, Strategy: materialize.StrategyCopy,
	}, nil
}

func materializeDiscoveryFlagExplicit(fs *flag.FlagSet) bool {
	explicit := false
	discoveryNames := map[string]bool{
		"allow-network": true, "max-depth": true, "max-directories": true,
		"max-entries": true, "max-directory-entries": true, "max-candidates": true,
		"max-path-bytes": true, "max-candidates-per-file": true, "max-candidate-edges": true,
		"max-states": true, "max-verified-layouts": true, "max-proof-bytes": true,
	}
	fs.Visit(func(item *flag.Flag) {
		if discoveryNames[item.Name] {
			explicit = true
		}
	})
	return explicit
}

func validMaterializePlanID(value string) bool {
	if len(value) != 24 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 12
}

func (a *app) seedMaterializeRun(args []string) error {
	fs := newFlagSet("seed materialize run")
	values := addMaterializeExecutionFlags(fs)
	if handled, err := parseStorageFlags(a, fs, args,
		"ptctl seed materialize run (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) --search-root PATH [--search-root PATH...] --target PATH --expect-plan-id ID --acknowledge-filesystem-write [flags]",
		"Repeats the reviewed seed discover --target shape, matches its plan ID, then journals, stages, exactly verifies, and no-clobber publishes one target layout. A standalone seed plan ID is not accepted. No absolute path is reported."); handled || err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usageError("seed materialize run accepts flags only")
	}
	input, discoveryOptions, err := values.validate(fs, "seed materialize run", true)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *values.timeout)
	defer cancel()
	meta, err := loadMetafileInput(ctx, input)
	if err != nil {
		safeErr := materializeMetafileLoadError(err)
		report := materializeRunInputFailureReport(ctx, *values.targetRoot, *values.expectedPlanID, safeErr)
		return a.finishMaterialize(*values.output, report, safeErr)
	}
	discovery, discoveryErr := seed.Discover(ctx, meta, discoveryOptions)
	if discoveryErr != nil {
		report, preparationErr := materialize.Run(ctx, materialize.RunOptions{
			Meta: meta, TargetRoot: *values.targetRoot, ExpectedPlanID: *values.expectedPlanID,
			Limits: materialize.DefaultLimits(),
		})
		removeMaterializeFinding(&report, &report.Blockers, "source.process_authority_missing")
		if len(report.Blockers) != 0 {
			return a.finishMaterialize(*values.output, report, preparationErr)
		}
		report.Outcome = materialize.OutcomeInterrupted
		report.Source.Outcome = "incomplete"
		appendMaterializeIssue(&report, "source.discovery_failed", "live source discovery could not be completed")
		return a.finishMaterialize(*values.output, report, fmt.Errorf("materialize source discovery failed"))
	}
	report, operationErr := materialize.Run(ctx, materialize.RunOptions{
		Meta: meta, Discovery: &discovery, TargetRoot: *values.targetRoot,
		ExpectedPlanID: *values.expectedPlanID, Limits: materialize.DefaultLimits(),
	})
	return a.finishMaterialize(*values.output, report, operationErr)
}

func (a *app) seedMaterializeResume(args []string) error {
	fs := newFlagSet("seed materialize resume")
	values := addMaterializeExecutionFlags(fs)
	if handled, err := parseStorageFlags(a, fs, args,
		"ptctl seed materialize resume (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) --target PATH --expect-plan-id ID --acknowledge-filesystem-write [--search-root PATH...] [flags] OPERATION_ID",
		"Resumes one explicit target-root-local operation. Fresh live discovery is read only while journaled or partially staged; later phases reverify staged or final bytes without source roots."); handled || err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageError("seed materialize resume requires exactly one OPERATION_ID")
	}
	operationID, err := materialize.ParseOperationID(fs.Arg(0))
	if err != nil {
		return usageError("seed materialize resume requires a canonical sha256 OPERATION_ID")
	}
	input, discoveryOptions, err := values.validate(fs, "seed materialize resume", false)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *values.timeout)
	defer cancel()
	status, statusErr := materialize.Status(ctx, materialize.ControlOptions{
		TargetRoot: *values.targetRoot, OperationID: operationID, Limits: materialize.DefaultLimits(),
	})
	if statusErr != nil {
		return a.finishMaterialize(*values.output, status, statusErr)
	}
	meta, err := loadMetafileInput(ctx, input)
	if err != nil {
		safeErr := materializeMetafileLoadError(err)
		status.Plan.ExpectedID = *values.expectedPlanID
		status.Plan.Matches = false
		classifyMaterializeMetafileFailure(&status, safeErr)
		return a.finishMaterialize(*values.output, status, safeErr)
	}

	needsDiscovery := materializePhaseNeedsDiscovery(status.Operation.PhaseAfter)
	var discovery *seed.DiscoveryResult
	if needsDiscovery && len(values.searchRoots) > 0 {
		observed, discoveryErr := seed.Discover(ctx, meta, discoveryOptions)
		if discoveryErr != nil {
			status.Outcome = materialize.OutcomeInterrupted
			status.Plan.ExpectedID = *values.expectedPlanID
			status.Plan.Matches = status.Plan.ObservedID == *values.expectedPlanID
			status.Source.Mode = "live_discovery"
			status.Source.Outcome = "incomplete"
			appendMaterializeIssue(&status, "source.discovery_failed", "fresh live source discovery could not be completed")
			return a.finishMaterialize(*values.output, status, fmt.Errorf("materialize source discovery failed"))
		}
		discovery = &observed
	}
	report, operationErr := materialize.Resume(ctx, materialize.ResumeOptions{
		Meta: meta, Discovery: discovery, TargetRoot: *values.targetRoot, OperationID: operationID,
		ExpectedPlanID: *values.expectedPlanID, Limits: materialize.DefaultLimits(),
	})
	if !needsDiscovery && len(values.searchRoots) > 0 {
		report.Warnings = append(report.Warnings, "explicit search roots were not read because this durable phase no longer requires source authority")
	}
	return a.finishMaterialize(*values.output, report, operationErr)
}

func materializePhaseNeedsDiscovery(phase string) bool {
	switch phase {
	case string(materialize.PhaseJournaled), string(materialize.PhaseStageCreated), string(materialize.PhaseFileStaged):
		return true
	default:
		return false
	}
}

func materializeMetafileLoadError(err error) error {
	var integrity *integrityErr
	if errors.As(err, &integrity) {
		return integrity
	}
	return fmt.Errorf("materialize metafile input could not be read")
}

func materializeRunInputFailureReport(ctx context.Context, targetRoot, expectedPlanID string, failure error) materialize.Report {
	report, _ := materialize.Run(ctx, materialize.RunOptions{
		TargetRoot: targetRoot, ExpectedPlanID: expectedPlanID, Limits: materialize.DefaultLimits(),
	})
	removeMaterializeFinding(&report, &report.Blockers, "manifest.unsupported_layout")
	classifyMaterializeMetafileFailure(&report, failure)
	return report
}

func classifyMaterializeMetafileFailure(report *materialize.Report, failure error) {
	if report == nil {
		return
	}
	report.Source = materialize.SourceReport{Mode: "not_requested", Outcome: "not_requested"}
	var integrity *integrityErr
	if errors.As(failure, &integrity) {
		report.Outcome = materialize.OutcomeIntegrityFailed
		appendMaterializeIssue(report, "metafile.integrity_failed", "the selected exact metafile could not be validated")
		return
	}
	report.Outcome = materialize.OutcomeInterrupted
	appendMaterializeIssue(report, "metafile.read_failed", "the selected metafile input could not be completely read")
}

func appendMaterializeIssue(report *materialize.Report, code, message string) {
	if report == nil {
		return
	}
	if report.Limits.MaxFindings <= 0 || report.Used.FindingsRetained >= report.Limits.MaxFindings {
		report.Used.FindingOverflow++
		return
	}
	report.Issues = append(report.Issues, materialize.Finding{Code: code, Message: message})
	report.Used.FindingsRetained++
}

func removeMaterializeFinding(report *materialize.Report, findings *[]materialize.Finding, code string) {
	if report == nil || findings == nil {
		return
	}
	retained := (*findings)[:0]
	removed := 0
	for _, finding := range *findings {
		if finding.Code == code {
			removed++
			continue
		}
		retained = append(retained, finding)
	}
	*findings = retained
	report.Used.FindingsRetained -= removed
	if report.Used.FindingsRetained < 0 {
		report.Used.FindingsRetained = 0
	}
}

func (a *app) seedMaterializeStatus(args []string) error {
	fs := newFlagSet("seed materialize status")
	output := fs.String("output", "table", "table or json")
	targetRoot := fs.String("target", "", "existing target root containing private operation journals")
	timeout := fs.Duration("timeout", materializeControlDefaultTimeout, "bounded journal read wall-clock budget")
	listDefaults := materialize.DefaultOperationListLimits()
	maxRootEntries := fs.Int("max-root-entries", listDefaults.MaxRootEntries, "maximum target-root entries examined when no operation ID is given")
	maxOperations := fs.Int("max-operations", listDefaults.MaxOperations, "maximum operation IDs retained when no operation ID is given")
	maxNameBytes := fs.Int64("max-name-bytes", listDefaults.MaxNameBytes, "maximum target-root name bytes retained when no operation ID is given")
	if handled, err := parseStorageFlags(a, fs, args,
		"ptctl seed materialize status --target PATH [flags] [OPERATION_ID]",
		"With an explicit ID, reads and replays exactly one private journal. Without an ID, performs one bounded name-only listing; rows remain not_inspected and no latest operation is selected."); handled || err != nil {
		return err
	}
	if fs.NArg() > 1 || *targetRoot == "" {
		return usageError("seed materialize status requires --target and at most one OPERATION_ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > materializeControlMaxTimeout {
		return usageError("--timeout must be greater than zero and no more than 1h")
	}
	if err := materialize.DefaultLimits().Validate(); err != nil {
		return fmt.Errorf("materialize default limits are invalid")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if fs.NArg() == 1 {
		for _, name := range []string{"max-root-entries", "max-operations", "max-name-bytes"} {
			if flagWasSet(fs, name) {
				return usageError("--%s applies only to status without an OPERATION_ID", name)
			}
		}
		operationID, err := materialize.ParseOperationID(fs.Arg(0))
		if err != nil {
			return usageError("seed materialize status requires a canonical sha256 OPERATION_ID")
		}
		report, operationErr := materialize.Status(ctx, materialize.ControlOptions{
			TargetRoot: *targetRoot, OperationID: operationID, Limits: materialize.DefaultLimits(),
		})
		return a.finishMaterialize(*output, report, operationErr)
	}

	limits := materialize.OperationListLimits{
		MaxRootEntries: *maxRootEntries, MaxOperations: *maxOperations, MaxNameBytes: *maxNameBytes,
	}
	if err := limits.Validate(); err != nil {
		return usageError("seed materialize status list limits are invalid")
	}
	result, listErr := materialize.ListOperations(ctx, *targetRoot, limits)
	if writeErr := a.writeMaterializeOperationList(*output, result); writeErr != nil {
		return writeErr
	}
	if listErr != nil {
		if errors.Is(listErr, materialize.ErrPolicy) {
			return &inconclusiveErr{message: "materialize operation listing was blocked; see report"}
		}
		return fmt.Errorf("materialize operation listing failed; see report")
	}
	if !result.Complete {
		return &inconclusiveErr{message: "materialize operation listing was incomplete; see report"}
	}
	return nil
}

func (a *app) seedMaterializeAbandon(args []string) error {
	fs := newFlagSet("seed materialize abandon")
	output := fs.String("output", "table", "table or json")
	targetRoot := fs.String("target", "", "existing target root containing the private operation journal")
	acknowledge := fs.Bool("acknowledge-abandon", false, "acknowledge terminal journal write and retained staged/scratch bytes")
	timeout := fs.Duration("timeout", materializeControlDefaultTimeout, "bounded journal transition wall-clock budget")
	if handled, err := parseStorageFlags(a, fs, args,
		"ptctl seed materialize abandon --target PATH --acknowledge-abandon [flags] OPERATION_ID",
		"Marks one unpublished operation terminal. It writes one private journal event, retains staged and scratch bytes, performs no deletion, and can never roll back published content."); handled || err != nil {
		return err
	}
	if fs.NArg() != 1 || *targetRoot == "" {
		return usageError("seed materialize abandon requires --target and exactly one OPERATION_ID")
	}
	if !*acknowledge {
		return usageError("seed materialize abandon requires --acknowledge-abandon")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > materializeControlMaxTimeout {
		return usageError("--timeout must be greater than zero and no more than 1h")
	}
	operationID, err := materialize.ParseOperationID(fs.Arg(0))
	if err != nil {
		return usageError("seed materialize abandon requires a canonical sha256 OPERATION_ID")
	}
	if err := materialize.DefaultLimits().Validate(); err != nil {
		return fmt.Errorf("materialize default limits are invalid")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, operationErr := materialize.Abandon(ctx, materialize.ControlOptions{
		TargetRoot: *targetRoot, OperationID: operationID, Limits: materialize.DefaultLimits(),
	})
	return a.finishMaterialize(*output, report, operationErr)
}

func (a *app) seedMaterializePrune(args []string) error {
	fs := newFlagSet("seed materialize prune")
	output := fs.String("output", "table", "table or json")
	targetRoot := fs.String("target", "", "existing target root containing the explicit terminal operation")
	expectedPlanID := fs.String("expect-plan-id", "", "reviewed 24-hex plan ID recorded by the operation")
	acknowledge := fs.Bool("acknowledge-operation-state-deletion", false, "acknowledge deletion of private operation state and retention of a tombstone")
	torrentPath := fs.String("torrent", "", "exact metafile path required for a newly started committed prune")
	storeRoot := fs.String("metafile-store", "", "private metafile store root; pair with --metafile-variant")
	variantID := fs.String("metafile-variant", "", "whole-metafile sha256 artifact ID; pair with --metafile-store")
	timeout := fs.Duration("timeout", materializeExecutionDefaultTimeout, "bounded proof and private-state deletion wall-clock budget")
	if handled, err := parseStorageFlags(a, fs, args,
		"ptctl seed materialize prune --target PATH --expect-plan-id ID --acknowledge-operation-state-deletion [--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID] [--output table|json] OPERATION_ID",
		"Prunes one explicit committed or abandoned operation. A new committed prune exactly reverifies the current final layout; retries resume only from a durable private retention intent. Source and final bytes are never deleted. The operation tombstone is retained."); handled || err != nil {
		return err
	}
	if fs.NArg() != 1 || *targetRoot == "" {
		return usageError("seed materialize prune requires --target and exactly one OPERATION_ID")
	}
	if !*acknowledge {
		return usageError("seed materialize prune requires --acknowledge-operation-state-deletion")
	}
	if !validMaterializePlanID(*expectedPlanID) {
		return usageError("seed materialize prune requires a canonical 24-hex --expect-plan-id")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > materializeExecutionMaxTimeout {
		return usageError("--timeout must be greater than zero and no more than 168h")
	}
	operationID, err := materialize.ParseOperationID(fs.Arg(0))
	if err != nil {
		return usageError("seed materialize prune requires a canonical sha256 OPERATION_ID")
	}
	journalLimits := materialize.DefaultLimits()
	retentionLimits := materialize.DefaultRetentionLimits()
	if err := journalLimits.Validate(); err != nil {
		return fmt.Errorf("materialize default limits are invalid")
	}
	if err := retentionLimits.Validate(); err != nil {
		return fmt.Errorf("materialize retention default limits are invalid")
	}

	inputRequested := flagWasSet(fs, "torrent") || flagWasSet(fs, "metafile-store") || flagWasSet(fs, "metafile-variant")
	var input metafileInput
	if inputRequested {
		input, err = flaggedMetafileInput("seed materialize prune", *torrentPath, *storeRoot, *variantID,
			flagWasSet(fs, "torrent"), flagWasSet(fs, "metafile-store"), flagWasSet(fs, "metafile-variant"))
		if err != nil {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var meta *metafile.MetaInfo
	if inputRequested {
		meta, err = loadMetafileInput(ctx, input)
		if err != nil {
			safeErr := materializeMetafileLoadError(err)
			report := materializeRetentionInputFailureReport(operationID, *expectedPlanID, retentionLimits, safeErr)
			return a.finishMaterializeRetention(*output, report, safeErr)
		}
	}
	report, operationErr := materialize.Prune(ctx, materialize.PruneOptions{
		Meta: meta, TargetRoot: *targetRoot, OperationID: operationID, ExpectedPlanID: *expectedPlanID,
		JournalLimits: journalLimits, RetentionLimits: retentionLimits,
	})
	return a.finishMaterializeRetention(*output, report, operationErr)
}

func materializeRetentionInputFailureReport(operationID materialize.OperationID, expectedPlanID string, retentionLimits materialize.RetentionLimits, failure error) materialize.RetentionReport {
	report := materialize.RetentionReport{
		Outcome: materialize.RetentionOutcomeInterrupted,
		Effect:  []string{"read_exact_metafile_for_retention_proof"},
		Operation: materialize.OperationReport{
			ID: operationID.String(), Status: "inspection_incomplete", PhaseBefore: "unknown", PhaseAfter: "unknown",
		},
		Plan:    materialize.PlanReport{ExpectedID: expectedPlanID, Strategy: materialize.StrategyCopy},
		Target:  materialize.RetentionTargetReport{FinalState: "not_observed", StabilityAssurance: "not_observed"},
		Proof:   materialize.RetentionProofReport{Assurance: "not_observed"},
		Markers: materialize.RetentionMarkerReport{State: "not_observed"},
		Limits:  retentionLimits, Blockers: []materialize.Finding{}, Issues: []materialize.Finding{}, Warnings: []string{},
	}
	var integrity *integrityErr
	if errors.As(failure, &integrity) {
		report.Outcome = materialize.RetentionOutcomeIntegrityFailed
		report.Issues = append(report.Issues, materialize.Finding{Code: "metafile.integrity_failed", Message: "the selected exact metafile could not be validated"})
	} else {
		report.Issues = append(report.Issues, materialize.Finding{Code: "metafile.read_failed", Message: "the selected exact metafile could not be completely read"})
	}
	return report
}

func (a *app) finishMaterializeRetention(output string, report materialize.RetentionReport, operationErr error) error {
	if writeErr := a.writeMaterializeRetentionReport(output, report); writeErr != nil {
		return writeErr
	}
	if operationErr == nil {
		return nil
	}
	switch report.Outcome {
	case materialize.RetentionOutcomeBlocked:
		return &inconclusiveErr{message: "materialize operation pruning was blocked; see report"}
	case materialize.RetentionOutcomeIntegrityFailed:
		return &integrityErr{message: "materialize operation pruning failed integrity verification; see report"}
	default:
		return fmt.Errorf("materialize operation pruning was interrupted; see report")
	}
}

func (a *app) writeMaterializeRetentionReport(output string, report materialize.RetentionReport) error {
	if output == "json" {
		return writeJSON(a.stdout, report, nil)
	}
	return writeMaterializeRetentionHuman(a.stdout, report)
}

func writeMaterializeRetentionHuman(out io.Writer, report materialize.RetentionReport) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nWRITES UNCERTAIN\t%t\nOPERATION ID\t%s\nOPERATION STATUS\t%s\nTERMINAL PHASE\t%s\n",
		terminalSafe(string(report.Outcome)), terminalSafe(strings.Join(report.Effect, ",")), report.WritesPerformed, report.WritesUncertain,
		terminalSafe(valueOrUnknown(report.Operation.ID)), terminalSafe(report.Operation.Status), terminalSafe(report.Operation.PhaseAfter))
	fmt.Fprintln(w, "\nBLOCKERS")
	writeMaterializeFindings(w, report.Blockers)
	fmt.Fprintf(w, "\nPLAN / PROOF\nEXPECTED PLAN\t%s\nOBSERVED PLAN\t%s\nMATCHES\t%t\nMETAFILE VARIANT\t%s\nBASIS\t%s\nCURRENT FINAL VERIFIED\t%t\nCURRENT PUBLICATION ABSENT\t%t\nFINAL VERIFY BYTES\t%d\nHISTORICAL MARKER AUTHORITY\t%t\nASSURANCE\t%s\n",
		terminalSafe(report.Plan.ExpectedID), terminalSafe(valueOrUnknown(report.Plan.ObservedID)), report.Plan.Matches,
		terminalSafe(report.Plan.MetafileVariantID), terminalSafe(materializeValueOr(report.Proof.Basis, "not_observed")),
		report.Proof.CurrentFinalVerified, report.Proof.CurrentPublicationAbsent, report.Proof.FinalVerifyBytes,
		report.Proof.HistoricalMarkerAuthority, terminalSafe(report.Proof.Assurance))
	fmt.Fprintf(w, "\nTARGET / RETENTION\nROOT IDENTITY BOUND\t%t\nFINAL STATE\t%s\nSTABILITY\t%s\nRETENTION STATE\t%s\nINTENT MARKER\t%s\nCOMPLETE MARKER\t%s\nINTENT DURABLE\t%t\nCOMPLETION DURABLE\t%t\nEXACT TOMBSTONE\t%t\nTOMBSTONE RETAINED\t%t\nPRUNE RESUMABLE\t%t\n",
		report.Target.RootIdentityBound, terminalSafe(report.Target.FinalState), terminalSafe(report.Target.StabilityAssurance),
		terminalSafe(report.Markers.State), terminalSafe(valueOrUnknown(report.Markers.IntentMarkerID)), terminalSafe(valueOrUnknown(report.Markers.CompleteMarkerID)),
		report.Markers.IntentDurable, report.Markers.CompletionDurable, report.Markers.ExactTombstone, report.Markers.OperationStateKept, report.Markers.PruneResumable)
	fmt.Fprintf(w, "\nWRITE / REMOVAL RECEIPTS\nCONTROL DIRECTORIES CREATED\t%d\nMARKER TEMP FILES\t%d\nMARKER TEMP BYTES\t%d\nMARKER PUBLICATION ATTEMPTS\t%d\nMARKER PUBLICATIONS\t%d\nMARKER DURABILITY CONFIRMATIONS\t%d\nMARKER TEMP REMOVALS\t%d\nREMOVAL ATTEMPTS\t%d\nFILES REMOVED\t%d\nDIRECTORIES REMOVED\t%d\nBYTES REMOVED\t%d\nAMBIGUOUS REMOVALS\t%d\n",
		report.Writes.ControlDirectoriesCreated, report.Writes.MarkerTemporaryFiles, report.Writes.MarkerTemporaryBytes,
		report.Writes.MarkerPublicationAttempts, report.Writes.MarkerPublications, report.Writes.MarkerDurabilityConfirms, report.Writes.MarkerTemporaryRemovals,
		report.Writes.RemovalAttempts, report.Writes.FilesRemoved, report.Writes.DirectoriesRemoved,
		report.Writes.BytesRemoved, report.Writes.AmbiguousRemovals)
	fmt.Fprintf(w, "\nLIMITS / USED\nMAX OBJECTS\t%d\nMAX PATH BYTES\t%d\nMAX CONTENT BYTES\t%d\nMAX MEMORY BYTES\t%d\nMAX DEPTH\t%d\nMAX FINDINGS\t%d\nOBJECTS CONSIDERED\t%d\nPATH BYTES CONSIDERED\t%d\nCONTENT BYTES CONSIDERED\t%d\nMEMORY BYTES CONSIDERED\t%d\nDIRECTORY ENTRIES EXAMINED\t%d\nDIRECTORY NAME BYTES EXAMINED\t%d\n",
		report.Limits.MaxObjects, report.Limits.MaxPathBytes, report.Limits.MaxBytes, report.Limits.MaxMemoryBytes, report.Limits.MaxDepth, report.Limits.MaxFindings,
		report.Used.ObjectsConsidered, report.Used.PathBytesConsidered, report.Used.BytesConsidered, report.Used.MemoryBytesConsidered,
		report.Used.DirectoryEntriesExamined, report.Used.DirectoryNameBytesExamined)
	fmt.Fprintln(w, "\nISSUES")
	writeMaterializeFindings(w, report.Issues)
	fmt.Fprintln(w, "\nWARNINGS")
	if len(report.Warnings) == 0 {
		fmt.Fprintln(w, "-\tnone")
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
	}
	return w.Flush()
}

func (a *app) finishMaterialize(output string, report materialize.Report, operationErr error) error {
	if writeErr := a.writeMaterializeReport(output, report); writeErr != nil {
		return writeErr
	}
	if operationErr == nil {
		return nil
	}
	if errors.Is(operationErr, materialize.ErrOperationNotFound) {
		return &inconclusiveErr{message: "materialize operation was not found; see report"}
	}
	switch report.Outcome {
	case materialize.OutcomeBlocked, materialize.OutcomeAbandoned:
		return &inconclusiveErr{message: "materialize operation was blocked; see report"}
	case materialize.OutcomeIntegrityFailed, materialize.OutcomePublishedIntegrityFailed:
		return &integrityErr{message: "materialize operation failed integrity verification; see report"}
	default:
		return fmt.Errorf("materialize operation was interrupted; see report")
	}
}

func (a *app) writeMaterializeReport(output string, report materialize.Report) error {
	if output == "json" {
		return writeJSON(a.stdout, report, nil)
	}
	return writeMaterializeHuman(a.stdout, report)
}

func writeMaterializeHuman(out io.Writer, report materialize.Report) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nWRITES UNCERTAIN\t%t\nOPERATION ID\t%s\nOPERATION STATUS\t%s\nPHASE BEFORE\t%s\nPHASE AFTER\t%s\nRESUMABLE\t%t\n",
		terminalSafe(string(report.Outcome)), terminalSafe(strings.Join(report.Effect, ",")), report.WritesPerformed, report.WritesUncertain,
		terminalSafe(valueOrUnknown(report.Operation.ID)), terminalSafe(report.Operation.Status), terminalSafe(report.Operation.PhaseBefore), terminalSafe(report.Operation.PhaseAfter), report.Operation.Resumable)
	fmt.Fprintln(w, "\nBLOCKERS")
	writeMaterializeFindings(w, report.Blockers)
	fmt.Fprintf(w, "\nPLAN\nEXPECTED ID\t%s\nOBSERVED ID\t%s\nMATCHES\t%t\nMETAFILE VARIANT\t%s\nSTRATEGY\t%s\n",
		terminalSafe(materializeValueOr(report.Plan.ExpectedID, "not_requested")), terminalSafe(valueOrUnknown(report.Plan.ObservedID)), report.Plan.Matches,
		terminalSafe(report.Plan.MetafileVariantID), terminalSafe(report.Plan.Strategy))
	fmt.Fprintf(w, "\nSOURCE\nMODE\t%s\nOUTCOME\t%s\nPRECONDITIONS RECHECKED\t%t\nCONTENT VERIFIED\t%t\n",
		terminalSafe(report.Source.Mode), terminalSafe(report.Source.Outcome), report.Source.PreconditionsRechecked, report.Source.ContentVerified)
	fmt.Fprintf(w, "\nTARGET\nPUBLICATION\t%s\nROOT IDENTITY BOUND\t%t\nSAME FILESYSTEM STAGING\t%t\nNO-CLOBBER CAPABLE\t%t\nNO-CLOBBER CONFIRMED\t%t\nSTABILITY\t%s\nSTAGE CONTENT VERIFIED\t%t\nFINAL CONTENT VERIFIED\t%t\nDURABILITY CONFIRMED\t%t\n",
		terminalSafe(report.Target.Publication), report.Target.RootIdentityBound, report.Target.SameFilesystemStage,
		report.Target.NoClobberCapable, report.Target.NoClobber, terminalSafe(report.Target.StabilityAssurance),
		report.Target.StageContentVerified, report.Target.FinalContentVerified, report.Target.DurabilityConfirmed)
	fmt.Fprintf(w, "\nWRITE BREAKDOWN\nOPERATION SUBTREES\t%d\nJOURNAL DIRECTORIES\t%d\nINTENT OBJECTS\t%d\nJOURNAL EVENTS\t%d\nJOURNAL PUBLICATION ATTEMPTS\t%d\nJOURNAL DURABILITY CONFIRMATIONS\t%d\nSCRATCH FILES\t%d\nSCRATCH BYTES WRITTEN\t%d\nSTAGED FILES\t%d\nSTAGE PUBLICATION ATTEMPTS\t%d\nSTAGED DIRECTORIES\t%d\nFINAL LAYOUT PUBLICATIONS\t%d\nFINAL PUBLICATION ATTEMPTS\t%d\nAMBIGUOUS PUBLICATIONS\t%d\nBYTES WRITTEN\t%d\n",
		report.Writes.OperationSubtrees, report.Writes.JournalDirectories, report.Writes.IntentObjects, report.Writes.JournalEvents,
		report.Writes.JournalPublicationAttempts, report.Writes.JournalDurabilityConfirms,
		report.Writes.ScratchFilesCreated, report.Writes.ScratchBytesWritten,
		report.Writes.StagedFiles, report.Writes.StagePublicationAttempts, report.Writes.StagedDirectories,
		report.Writes.FinalLayoutPublications, report.Writes.FinalPublicationAttempts,
		report.Writes.AmbiguousPublications, report.Writes.BytesWritten)
	fmt.Fprintf(w, "\nLIMITS / USED\nMAX FILES\t%d\nMAX CONTENT BYTES\t%d\nMAX PATH BYTES\t%d\nMAX DIRECTORIES\t%d\nMAX NAMESPACE OBJECTS\t%d\nMAX NAMESPACE BYTES\t%d\nCOPY BUFFER BYTES\t%d\nMAX JOURNAL EVENTS\t%d\nMAX EVENT BYTES\t%d\nMAX SCRATCH ENTRIES\t%d\nMAX SCRATCH BYTES\t%d\nMAX FINDINGS\t%d\nSOURCE COPY BYTES\t%d\nTARGET WRITE BYTES\t%d\nSTAGE VERIFY BYTES\t%d\nFINAL VERIFY BYTES\t%d\nSCRATCH ENTRIES OBSERVED\t%d\nSCRATCH BYTES OBSERVED\t%d\nFINDINGS RETAINED\t%d\nFINDING OVERFLOW\t%d\n",
		report.Limits.MaxFiles, report.Limits.MaxContentBytes, report.Limits.MaxPathBytes,
		report.Limits.MaxDirectories, report.Limits.MaxNamespaceObjects, report.Limits.MaxNamespaceBytes, report.Limits.CopyBufferBytes,
		report.Limits.MaxJournalEvents, report.Limits.MaxEventBytes,
		report.Limits.MaxScratchEntries, report.Limits.MaxScratchBytes, report.Limits.MaxFindings,
		report.Used.SourceCopyBytes, report.Used.TargetWriteBytes, report.Used.StageVerifyBytes, report.Used.FinalVerifyBytes,
		report.Used.ScratchEntries, report.Used.ScratchBytes, report.Used.FindingsRetained, report.Used.FindingOverflow)
	fmt.Fprintln(w, "\nISSUES")
	writeMaterializeFindings(w, report.Issues)
	fmt.Fprintln(w, "\nWARNINGS")
	if len(report.Warnings) == 0 {
		fmt.Fprintln(w, "-\tnone")
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
	}
	return w.Flush()
}

func materializeValueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func writeMaterializeFindings(w io.Writer, findings []materialize.Finding) {
	if len(findings) == 0 {
		fmt.Fprintln(w, "-\tnone")
		return
	}
	for _, finding := range findings {
		manifest := "-"
		if finding.ManifestIndex != nil {
			manifest = fmt.Sprint(*finding.ManifestIndex)
		}
		fmt.Fprintf(w, "-\t%s\tmanifest=%s\t%s\n", terminalSafe(finding.Code), terminalSafe(manifest), terminalSafe(finding.Message))
	}
}

func (a *app) writeMaterializeOperationList(output string, result materialize.OperationListResult) error {
	if output == "json" {
		return writeJSON(a.stdout, result, nil)
	}
	w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "COMPLETE\t%t\nEFFECT\t%s\nWRITES PERFORMED\t%d\nSTOP REASON\t%s\nMAX ROOT ENTRIES\t%d\nMAX OPERATIONS\t%d\nMAX NAME BYTES\t%d\nROOT ENTRIES EXAMINED\t%d\nROOT NAME BYTES\t%d\n",
		result.Complete, terminalSafe(result.Effect), result.Writes, terminalSafe(materializeValueOr(result.StopReason, "none")),
		result.Limits.MaxRootEntries, result.Limits.MaxOperations, result.Limits.MaxNameBytes,
		result.RootUsed.EntriesExamined, result.RootUsed.NameBytes)
	fmt.Fprintln(w, "\nOPERATIONS")
	if len(result.Operations) == 0 {
		fmt.Fprintln(w, "-\tnone")
	}
	for _, operation := range result.Operations {
		fmt.Fprintf(w, "-\t%s\t%s\n", terminalSafe(operation.ID.String()), terminalSafe(operation.Status))
	}
	fmt.Fprintln(w, "\nWARNINGS")
	if len(result.Warnings) == 0 {
		fmt.Fprintln(w, "-\tnone")
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
	}
	return w.Flush()
}
