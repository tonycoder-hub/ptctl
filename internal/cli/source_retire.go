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

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/sourceretire"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

const (
	sourceRetireDefaultTimeout = 24 * time.Hour
	sourceRetireMaxTimeout     = 7 * 24 * time.Hour
)

type sourceRetireFlags struct {
	output               *string
	torrentPath          *string
	storeRoot            *string
	variantID            *string
	searchRoots          stringListFlag
	targetRoot           *string
	materializeOperation *string
	materializePlanID    *string
	activationOperation  *string
	activationPlanID     *string
	showAbsolute         *bool
	allowNetwork         *bool
	requireEligible      *bool
	timeout              *time.Duration
	maxDepth             *int
	maxDirectories       *int
	maxEntries           *int
	maxDirectoryEntries  *int
	maxCandidates        *int
	maxPathBytes         *int64
	maxCandidatesPerFile *int
	maxCandidateEdges    *int
	maxStates            *int
	maxVerifiedLayouts   *int
	maxProofBytes        *int64
}

func (a *app) seedRetire(args []string) error {
	if len(args) == 0 {
		return usageError("seed retire subcommand is required")
	}
	switch args[0] {
	case "help", "-h", "--help":
		a.seedRetireHelp()
		return nil
	case "plan":
		return a.seedRetirePlan(args[1:])
	default:
		return usageError("unknown seed retire subcommand %q", args[0])
	}
}

func (a *app) seedRetireHelp() {
	fmt.Fprint(a.stdout, `Usage:
  ptctl seed retire plan (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) --search-root PATH [--search-root PATH...] --target PATH --materialize-operation ID --materialize-plan-id ID --activation-operation ID --activation-plan-id ID [flags]

The plan command is read-only. It requires one complete live source discovery,
a current exact materialized-final proof, and one canonical terminal client
activation journal. It emits only an eligibility plan: deletion_authority is
always none, it performs zero writes, no file or directory is removed, and serialized JSON is never
accepted later as proof.

Search roots must deliberately identify the source names under review. If they
also discover the published final, ambiguity or final-overlap blocks the plan.
Only content-bearing regular-file names are represented; empty files, padding,
directories, and broader cleanup remain out of scope.
`)
}

func addSourceRetireFlags(fs *flag.FlagSet) *sourceRetireFlags {
	values := &sourceRetireFlags{}
	values.output = fs.String("output", "table", "table or json")
	values.torrentPath = fs.String("torrent", "", "metafile path")
	values.storeRoot = fs.String("metafile-store", "", "private metafile store root; pair with --metafile-variant")
	values.variantID = fs.String("metafile-variant", "", "whole-metafile sha256 artifact ID; pair with --metafile-store")
	fs.Var(&values.searchRoots, "search-root", "live source root to scan; repeatable")
	values.targetRoot = fs.String("target", "", "existing local materialized target root")
	values.materializeOperation = fs.String("materialize-operation", "", "explicit materialize operation ID")
	values.materializePlanID = fs.String("materialize-plan-id", "", "reviewed materialize plan ID")
	values.activationOperation = fs.String("activation-operation", "", "explicit terminal client activation operation ID")
	values.activationPlanID = fs.String("activation-plan-id", "", "reviewed client activation plan ID")
	values.showAbsolute = fs.Bool("show-absolute-paths", false, "include selected absolute source paths in output")
	values.allowNetwork = fs.Bool("allow-network", false, "allow explicit network/UNC source roots; never applies to target")
	values.requireEligible = fs.Bool("require-eligible", false, "exit 4 after the report unless outcome is eligible_for_separate_review")
	values.timeout = fs.Duration("timeout", sourceRetireDefaultTimeout, "shared final, journal, discovery, and proof wall-clock budget")

	inventory := storage.DefaultInventoryLimits()
	values.maxDepth = fs.Int("max-depth", inventory.MaxDepth, "maximum live discovery directory depth")
	values.maxDirectories = fs.Int("max-directories", inventory.MaxDirectories, "maximum live discovery directories opened")
	values.maxEntries = fs.Int("max-entries", inventory.MaxEntries, "maximum live discovery entries examined")
	values.maxDirectoryEntries = fs.Int("max-directory-entries", inventory.MaxEntriesPerDirectory, "maximum entries accepted from one directory")
	values.maxCandidates = fs.Int("max-candidates", inventory.MaxCandidates, "maximum matching regular files retained")
	values.maxPathBytes = fs.Int64("max-path-bytes", inventory.MaxPathBytes, "maximum retained discovery relative-path bytes")

	match := metafile.DefaultSourceMatchLimits()
	values.maxCandidatesPerFile = fs.Int("max-candidates-per-file", match.MaxCandidatesPerFile, "maximum candidates explored for one torrent file")
	values.maxCandidateEdges = fs.Int("max-candidate-edges", match.MaxCandidateEdges, "maximum manifest-to-candidate edges considered")
	values.maxStates = fs.Int("max-states", match.MaxStates, "maximum candidate assignment states")
	values.maxVerifiedLayouts = fs.Int("max-verified-layouts", match.MaxVerifiedLayouts, "maximum verified alternatives retained")
	values.maxProofBytes = fs.Int64("max-proof-bytes", match.MaxProofWorkBytes, "maximum physical and virtual bytes charged to source proof work")
	return values
}

type preparedSourceRetire struct {
	input                metafileInput
	output               string
	targetRoot           string
	materializeOperation materialize.OperationID
	materializePlanID    string
	activationOperation  clientactivate.OperationID
	activationPlanID     string
	showAbsolute         bool
	requireEligible      bool
	timeout              time.Duration
	discovery            seed.DiscoverOptions
}

func (values *sourceRetireFlags) validate(fs *flag.FlagSet) (preparedSourceRetire, error) {
	var result preparedSourceRetire
	if len(values.searchRoots) == 0 || *values.targetRoot == "" || *values.materializeOperation == "" ||
		*values.materializePlanID == "" || *values.activationOperation == "" || *values.activationPlanID == "" {
		return result, usageError("seed retire plan requires search roots and every materialize/activation selector")
	}
	for _, root := range values.searchRoots {
		if root == "" {
			return result, usageError("seed retire plan requires every --search-root to be non-empty")
		}
	}
	if err := validateOutput(*values.output); err != nil {
		return result, err
	}
	if *values.timeout <= 0 || *values.timeout > sourceRetireMaxTimeout {
		return result, usageError("--timeout must be greater than zero and no more than 168h")
	}
	materializeOperation, err := materialize.ParseOperationID(*values.materializeOperation)
	if err != nil || !validMaterializePlanID(*values.materializePlanID) {
		return result, usageError("seed retire plan requires canonical materialize operation and plan IDs")
	}
	activationOperation, err := clientactivate.ParseOperationID(*values.activationOperation)
	if err != nil || !validMaterializePlanID(*values.activationPlanID) {
		return result, usageError("seed retire plan requires canonical activation operation and plan IDs")
	}
	input, err := flaggedMetafileInput("seed retire plan", *values.torrentPath, *values.storeRoot, *values.variantID,
		flagWasSet(fs, "torrent"), flagWasSet(fs, "metafile-store"), flagWasSet(fs, "metafile-variant"))
	if err != nil {
		return result, err
	}
	inventory := storage.DefaultInventoryLimits()
	inventory.MaxDepth, inventory.MaxDirectories = *values.maxDepth, *values.maxDirectories
	inventory.MaxEntries, inventory.MaxEntriesPerDirectory = *values.maxEntries, *values.maxDirectoryEntries
	inventory.MaxCandidates, inventory.MaxPathBytes = *values.maxCandidates, *values.maxPathBytes
	if err := inventory.Validate(); err != nil {
		return result, usageError("seed retire plan discovery limits are invalid: %v", err)
	}
	if len(values.searchRoots) > inventory.MaxRoots {
		return result, usageError("seed retire plan accepts at most %d --search-root values", inventory.MaxRoots)
	}
	match := metafile.DefaultSourceMatchLimits()
	match.MaxCandidatesPerFile, match.MaxCandidateEdges = *values.maxCandidatesPerFile, *values.maxCandidateEdges
	match.MaxStates, match.MaxVerifiedLayouts = *values.maxStates, *values.maxVerifiedLayouts
	match.MaxProofWorkBytes = *values.maxProofBytes
	if err := match.Validate(); err != nil {
		return result, usageError("seed retire plan proof limits are invalid: %v", err)
	}
	return preparedSourceRetire{input: input, output: *values.output, targetRoot: *values.targetRoot,
		materializeOperation: materializeOperation, materializePlanID: *values.materializePlanID,
		activationOperation: activationOperation, activationPlanID: *values.activationPlanID,
		showAbsolute: *values.showAbsolute, requireEligible: *values.requireEligible, timeout: *values.timeout,
		discovery: seed.DiscoverOptions{SearchRoots: append([]string(nil), values.searchRoots...), InventoryLimits: inventory,
			MatchLimits: match, AllowNetwork: *values.allowNetwork, ShowAbsolutePaths: false,
			TimeBudget: *values.timeout, Strategy: materialize.StrategyCopy}}, nil
}

func (a *app) seedRetirePlan(args []string) error {
	fs := newFlagSet("seed retire plan")
	var flagOutput strings.Builder
	fs.SetOutput(&flagOutput)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  ptctl seed retire plan (--torrent FILE.torrent | --metafile-store DIR --metafile-variant ID) --search-root PATH [--search-root PATH...] --target PATH --materialize-operation ID --materialize-plan-id ID --activation-operation ID --activation-plan-id ID [flags]")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Zero writes and zero deletion. The result is review evidence only; no serialized plan is executable authority.")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
	}
	values := addSourceRetireFlags(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(a.stdout, flagOutput.String())
			return nil
		}
		detail := strings.TrimSpace(flagOutput.String())
		if detail == "" {
			detail = err.Error()
		}
		return usageError("seed retire plan: %s", detail)
	}
	if fs.NArg() != 0 {
		return usageError("seed retire plan accepts flags only")
	}
	prepared, err := values.validate(fs)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), prepared.timeout)
	defer cancel()
	meta, err := loadMetafileInput(ctx, prepared.input)
	var inputIntegrity *integrityErr
	if errors.As(err, &inputIntegrity) {
		return inputIntegrity
	}
	if errors.Is(err, metastore.ErrCorruptArtifact) {
		return &integrityErr{message: "source retirement metafile failed integrity validation"}
	}
	if err != nil {
		return fmt.Errorf("source retirement metafile input could not be read")
	}
	final, _, err := materialize.VerifyCurrentFinal(ctx, materialize.FinalProofOptions{Meta: meta, TargetRoot: prepared.targetRoot,
		OperationID: prepared.materializeOperation, ExpectedPlanID: prepared.materializePlanID, Limits: materialize.DefaultLimits()})
	if errors.Is(err, materialize.ErrIntegrity) {
		return &integrityErr{message: "source retirement current final failed exact verification"}
	}
	if err != nil {
		return fmt.Errorf("source retirement current final could not be verified")
	}
	activation, _, err := clientactivate.VerifyCompletion(ctx, clientactivate.CompletionProofOptions{TargetRoot: prepared.targetRoot,
		OperationID: prepared.activationOperation, ExpectedPlanID: prepared.activationPlanID})
	if errors.Is(err, clientactivate.ErrIntegrity) {
		return &integrityErr{message: "source retirement client activation journal failed integrity validation"}
	}
	if err != nil {
		return fmt.Errorf("source retirement terminal client completion could not be verified")
	}
	discovery, err := seed.Discover(ctx, meta, prepared.discovery)
	if err != nil {
		return fmt.Errorf("source retirement live source discovery failed")
	}
	report, operationErr := sourceretire.Build(ctx, sourceretire.BuildOptions{Meta: meta, Discovery: &discovery,
		Final: final, Activation: activation, ShowAbsolutePaths: prepared.showAbsolute})
	if err := a.writeSourceRetireReport(prepared.output, report); err != nil {
		return err
	}
	if errors.Is(operationErr, sourceretire.ErrIntegrity) {
		return &integrityErr{message: "source retirement proof changed during planning"}
	}
	if prepared.requireEligible && report.Outcome != sourceretire.OutcomeEligible {
		return &inconclusiveErr{message: "source retirement outcome is not eligible_for_separate_review"}
	}
	// Incomplete reports are report-first and use exit 0 by default, matching
	// other read-only evidence workflows. --require-eligible makes them exit 4.
	return nil
}

func (a *app) writeSourceRetireReport(output string, report sourceretire.Report) error {
	if output == "json" {
		return writeJSON(a.stdout, report, nil)
	}
	if output != "table" {
		return usageError("--output must be table or json")
	}
	return writeSourceRetireHuman(a.stdout, report)
}

func writeSourceRetireHuman(out io.Writer, report sourceretire.Report) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES\t%d\nDELETION PERFORMED\t%t\nDELETION AUTHORITY\t%s\n",
		terminalSafe(report.Outcome), terminalSafe(strings.Join(report.Effect, "+")), report.WritesPerformed,
		report.DeletionPerformed, terminalSafe(report.Plan.DeletionAuthority))
	writeSourceRetireFindings(w, "BLOCKERS", report.Blockers)
	writeSourceRetireFindings(w, "ISSUES", report.Issues)
	fmt.Fprintf(w, "\nPLAN\nID\t%s\nMODE\t%s\nVARIANT\t%s\nSOURCE SELECTION\t%s\nFILES\t%d\nBYTES\t%d\n",
		terminalSafe(report.Plan.ID), terminalSafe(report.Plan.Mode), terminalSafe(report.Plan.MetafileVariantID),
		terminalSafe(report.Plan.SourceSelectionID), report.Plan.PhysicalSourceFiles, report.Plan.ContentBytes)
	fmt.Fprintf(w, "\nSOURCE\nSTATUS\t%s\nASSURANCE\t%s\n\nMATERIALIZED FINAL\nOPERATION\t%s\nPLAN\t%s\nOBJECT IDENTITY\t%s\nASSURANCE\t%s\n",
		terminalSafe(report.Source.Status), terminalSafe(report.Source.Assurance), terminalSafe(report.Final.OperationID),
		terminalSafe(report.Final.MaterializePlanID), terminalSafe(report.Final.FinalObjectIdentity), terminalSafe(report.Final.Assurance))
	fmt.Fprintf(w, "\nSOURCE SCAN\nCOMPLETE\t%t\nVERIFICATION COMPLETE\t%t\nPATH CONFINEMENT\t%s\nSTOP REASONS\t%s\nENTRIES\t%d / %d\nCANDIDATES\t%d / %d\nPROOF BYTES\t%d / %d\n",
		report.Scan.Complete, report.Scan.VerificationComplete, terminalSafe(report.Scan.PathConfinement),
		terminalSafe(strings.Join(report.Scan.StopReasons, ",")), report.Scan.InventoryUsed.EntriesExamined,
		report.Scan.InventoryLimits.MaxEntries, report.Scan.InventoryUsed.CandidatesRetained,
		report.Scan.InventoryLimits.MaxCandidates, report.Scan.MatchUsed.ProofWorkBytesCharged,
		report.Scan.MatchLimits.MaxProofWorkBytes)
	fmt.Fprintf(w, "\nCLIENT COMPLETION\nOPERATION\t%s\nPLAN\t%s\nPHASE\t%s\nMARKER\t%s\nOBSERVED START\t%s\nOBSERVED END\t%s\nASSURANCE\t%s\n",
		terminalSafe(report.Activation.OperationID), terminalSafe(report.Activation.PlanID), terminalSafe(report.Activation.TerminalPhase),
		terminalSafe(report.Activation.TerminalMarkerID), terminalSafe(report.Activation.ObservedAtStart),
		terminalSafe(report.Activation.ObservedAtEnd), terminalSafe(report.Activation.Assurance))
	if len(report.Plan.SourceFiles) > 0 {
		fmt.Fprintln(w, "\nSOURCE FILES")
		fmt.Fprintln(w, "INDEX\tBYTES\tPATH")
		for _, file := range report.Plan.SourceFiles {
			path := file.SourcePathRef
			if file.SourcePath != "" {
				path = file.SourcePath
			}
			fmt.Fprintf(w, "%d\t%d\t%s\n", file.ManifestIndex, file.SizeBytes, terminalSafe(path))
		}
	}
	if len(report.Warnings) > 0 {
		fmt.Fprintln(w, "\nWARNINGS")
		for _, warning := range report.Warnings {
			fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
		}
	}
	return w.Flush()
}

func writeSourceRetireFindings(w *tabwriter.Writer, label string, findings []sourceretire.Finding) {
	if len(findings) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n", label)
	for _, finding := range findings {
		fmt.Fprintf(w, "%s\t%s\n", terminalSafe(finding.Code), terminalSafe(finding.Message))
	}
}
