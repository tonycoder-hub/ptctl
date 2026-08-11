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

	"github.com/tonycoder-hub/ptctl/internal/sourceretire"
)

const (
	parentCleanupDefaultTimeout = time.Minute
	parentCleanupMaxTimeout     = time.Hour
)

func (a *app) seedRetireParentCleanup(args []string) error {
	if len(args) == 0 {
		return usageError("seed retire parent-cleanup subcommand is required")
	}
	switch args[0] {
	case "help", "-h", "--help":
		a.seedRetireParentCleanupHelp()
		return nil
	case "plan":
		return a.seedRetireParentCleanupPlan(args[1:])
	case "run":
		return a.seedRetireParentCleanupRun(args[1:])
	case "resume":
		return a.seedRetireParentCleanupResume(args[1:])
	case "status":
		return a.seedRetireParentCleanupStatus(args[1:])
	default:
		return usageError("unknown seed retire parent-cleanup subcommand %q", args[0])
	}
}

func (a *app) seedRetireParentCleanupHelp() {
	fmt.Fprint(a.stdout, `Usage:
  ptctl seed retire parent-cleanup plan --target PATH --retirement-operation ID --retirement-plan-id ID --search-root PATH [--search-root PATH...] [flags]
  ptctl seed retire parent-cleanup run --target PATH --retirement-operation ID --retirement-plan-id ID --search-root PATH [--search-root PATH...] --expect-cleanup-plan-id ID --acknowledge-empty-parent-removal [flags]
  ptctl seed retire parent-cleanup resume --target PATH --search-root PATH [--search-root PATH...] --expect-cleanup-plan-id ID --acknowledge-empty-parent-removal [flags] OPERATION_ID
  ptctl seed retire parent-cleanup status --target PATH [--output table|json] OPERATION_ID

The plan is strictly zero-write. It reads one explicit live terminal source-
retirement journal, proves the exact retired names currently absent in the same
root scope, and considers only their immediate parents. Search roots and higher
ancestors are always protected. A candidate must retain the journaled identity
and be observed empty twice. Child names are never reported.

A pruned retirement tombstone is historical evidence only and cannot recreate
the path authority required by a new plan. Run repeats the live review and must
reproduce an explicit cleanup-plan ID before it writes a private journal.
Each exact immediate parent gets a durable attempt marker, a fresh post-journal
empty observation, identity-bound empty-directory removal, and a completion
marker. Resume selects one exact operation; status is historical and read-only.
No command recursively removes ancestors, search roots, files, or non-empty
directories.
`)
}

func (a *app) seedRetireParentCleanupPlan(args []string) error {
	fs := newFlagSet("seed retire parent-cleanup plan")
	var flagOutput strings.Builder
	fs.SetOutput(&flagOutput)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  ptctl seed retire parent-cleanup plan --target PATH --retirement-operation ID --retirement-plan-id ID --search-root PATH [--search-root PATH...] [flags]")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Zero writes and zero deletion. Only stable empty immediate parents can become review candidates.")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
	}
	output := fs.String("output", "table", "table or json")
	target := fs.String("target", "", "existing materialized target root containing the terminal retirement journal")
	operationValue := fs.String("retirement-operation", "", "explicit terminal source-retirement operation ID")
	planID := fs.String("retirement-plan-id", "", "reviewed source-retirement plan ID")
	var roots stringListFlag
	fs.Var(&roots, "search-root", "exact original source-root scope; repeatable")
	allowNetwork := fs.Bool("allow-network", false, "allow explicit network/UNC source roots")
	showAbsolute := fs.Bool("show-absolute-paths", false, "include candidate or retained parent paths in output")
	requireCleanable := fs.Bool("require-cleanable", false, "exit 4 after the report unless at least one stable empty parent is reviewable")
	timeout := fs.Duration("timeout", parentCleanupDefaultTimeout, "shared terminal-journal and parent-observation wall-clock budget")
	limits := sourceretire.DefaultParentCleanupLimits()
	maxParents := fs.Int("max-parents", limits.MaxParents, "maximum distinct immediate parents considered")
	maxPathBytes := fs.Int64("max-path-bytes", limits.MaxPathBytes, "maximum journaled immediate-parent path bytes considered")
	maxEntryNameBytes := fs.Int64("max-entry-name-bytes", limits.MaxEntryNameBytes, "maximum name bytes charged while proving one parent non-empty")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(a.stdout, flagOutput.String())
			return nil
		}
		return usageError("seed retire parent-cleanup plan: %s", sourceRetireFlagError(flagOutput.String(), err))
	}
	if fs.NArg() != 0 {
		return usageError("seed retire parent-cleanup plan accepts flags only")
	}
	if *target == "" || *operationValue == "" || *planID == "" || len(roots) == 0 {
		return usageError("seed retire parent-cleanup plan requires target, retirement operation/plan, and at least one search root")
	}
	for _, root := range roots {
		if root == "" {
			return usageError("seed retire parent-cleanup plan requires every --search-root to be non-empty")
		}
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > parentCleanupMaxTimeout {
		return usageError("seed retire parent-cleanup plan --timeout must be greater than zero and no more than 1h")
	}
	operation, err := sourceretire.ParseOperationID(*operationValue)
	if err != nil || !validSourceRetireExecutionPlanID(*planID) {
		return usageError("seed retire parent-cleanup plan requires canonical retirement operation and plan IDs")
	}
	derived, err := sourceretire.OperationIDForPlanID(*planID)
	if err != nil || derived != operation {
		return usageError("seed retire parent-cleanup plan operation and retirement plan IDs disagree")
	}
	limits.MaxParents, limits.MaxPathBytes, limits.MaxEntryNameBytes = *maxParents, *maxPathBytes, *maxEntryNameBytes
	if err := limits.Validate(); err != nil {
		return usageError("seed retire parent-cleanup plan limits are invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, planErr := sourceretire.BuildParentCleanupPlan(ctx, sourceretire.ParentCleanupOptions{
		TargetRoot: *target, OperationID: operation, ExpectedPlanID: *planID,
		SearchRoots: append([]string(nil), roots...), AllowNetwork: *allowNetwork,
		Limits: limits, RetirementLimits: sourceretire.DefaultExecutionLimits(), ShowAbsolutePaths: *showAbsolute,
	})
	return a.finishParentCleanupPlan(*output, *requireCleanable, report, planErr)
}

func (a *app) seedRetireParentCleanupRun(args []string) error {
	fs := newFlagSet("seed retire parent-cleanup run")
	var flagOutput strings.Builder
	fs.SetOutput(&flagOutput)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  ptctl seed retire parent-cleanup run --target PATH --retirement-operation ID --retirement-plan-id ID --search-root PATH [--search-root PATH...] --expect-cleanup-plan-id ID --acknowledge-empty-parent-removal [flags]")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Rebuilds the zero-write review, then journals and removes only its exact empty immediate-parent candidates.")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
	}
	output := fs.String("output", "table", "table or json")
	target := fs.String("target", "", "existing materialized target root containing the terminal retirement journal")
	retirementOperationValue := fs.String("retirement-operation", "", "explicit terminal source-retirement operation ID")
	retirementPlanID := fs.String("retirement-plan-id", "", "reviewed source-retirement plan ID")
	expectedCleanupPlanID := fs.String("expect-cleanup-plan-id", "", "reviewed sha256 parent-cleanup plan ID")
	acknowledge := fs.Bool("acknowledge-empty-parent-removal", false, "acknowledge exact irreversible empty-directory removal")
	var roots stringListFlag
	fs.Var(&roots, "search-root", "exact original source-root scope; repeatable")
	allowNetwork := fs.Bool("allow-network", false, "allow explicit network/UNC source roots")
	showAbsolute := fs.Bool("show-absolute-paths", false, "include exact reviewed parent paths in output")
	timeout := fs.Duration("timeout", parentCleanupDefaultTimeout, "shared live-review, journal, and removal wall-clock budget")
	planLimits := sourceretire.DefaultParentCleanupLimits()
	maxParents := fs.Int("max-parents", planLimits.MaxParents, "maximum distinct immediate parents considered by the repeated review")
	maxPathBytes := fs.Int64("max-path-bytes", planLimits.MaxPathBytes, "maximum journaled immediate-parent path bytes considered")
	maxEntryNameBytes := fs.Int64("max-entry-name-bytes", planLimits.MaxEntryNameBytes, "maximum name bytes charged while proving one parent non-empty")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(a.stdout, flagOutput.String())
			return nil
		}
		return usageError("seed retire parent-cleanup run: %s", sourceRetireFlagError(flagOutput.String(), err))
	}
	if fs.NArg() != 0 {
		return usageError("seed retire parent-cleanup run accepts flags only")
	}
	if *target == "" || *retirementOperationValue == "" || *retirementPlanID == "" || *expectedCleanupPlanID == "" || len(roots) == 0 || !*acknowledge {
		return usageError("seed retire parent-cleanup run requires target, retirement and cleanup plan selectors, search roots, and --acknowledge-empty-parent-removal")
	}
	for _, root := range roots {
		if root == "" {
			return usageError("seed retire parent-cleanup run requires every --search-root to be non-empty")
		}
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > parentCleanupMaxTimeout {
		return usageError("seed retire parent-cleanup run --timeout must be greater than zero and no more than 1h")
	}
	retirementOperation, err := sourceretire.ParseOperationID(*retirementOperationValue)
	if err != nil || !validSourceRetireExecutionPlanID(*retirementPlanID) || !validSourceRetireExecutionPlanID(*expectedCleanupPlanID) {
		return usageError("seed retire parent-cleanup run requires canonical retirement operation/plan and cleanup plan IDs")
	}
	derivedRetirement, err := sourceretire.OperationIDForPlanID(*retirementPlanID)
	if err != nil || derivedRetirement != retirementOperation {
		return usageError("seed retire parent-cleanup run operation and retirement plan IDs disagree")
	}
	planLimits.MaxParents, planLimits.MaxPathBytes, planLimits.MaxEntryNameBytes = *maxParents, *maxPathBytes, *maxEntryNameBytes
	if err := planLimits.Validate(); err != nil {
		return usageError("seed retire parent-cleanup run review limits are invalid")
	}
	executionLimits := sourceretire.DefaultParentCleanupExecutionLimits()
	if planLimits.MaxParents > executionLimits.MaxParents || planLimits.MaxPathBytes > executionLimits.MaxPathBytes ||
		planLimits.MaxEntryNameBytes > executionLimits.MaxEntryNameBytes {
		return usageError("seed retire parent-cleanup run review limits exceed the fixed execution protocol limits")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, operationErr := sourceretire.RunParentCleanup(ctx, sourceretire.ParentCleanupRunOptions{
		Review: sourceretire.ParentCleanupOptions{
			TargetRoot: *target, OperationID: retirementOperation, ExpectedPlanID: *retirementPlanID,
			SearchRoots: append([]string(nil), roots...), AllowNetwork: *allowNetwork, Limits: planLimits,
			RetirementLimits: sourceretire.DefaultExecutionLimits(),
		},
		ExpectedCleanupPlanID: *expectedCleanupPlanID, Acknowledge: true,
		Limits: executionLimits, ShowAbsolutePaths: *showAbsolute,
	})
	return a.finishParentCleanupExecution(*output, report, operationErr)
}

func (a *app) seedRetireParentCleanupResume(args []string) error {
	fs := newFlagSet("seed retire parent-cleanup resume")
	var flagOutput strings.Builder
	fs.SetOutput(&flagOutput)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  ptctl seed retire parent-cleanup resume --target PATH --search-root PATH [--search-root PATH...] --expect-cleanup-plan-id ID --acknowledge-empty-parent-removal [flags] OPERATION_ID")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Continues one exact private journal. It never enumerates or selects a latest operation.")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
	}
	output := fs.String("output", "table", "table or json")
	target := fs.String("target", "", "existing materialized target root containing the parent-cleanup journal")
	expected := fs.String("expect-cleanup-plan-id", "", "reviewed sha256 parent-cleanup plan ID")
	acknowledge := fs.Bool("acknowledge-empty-parent-removal", false, "acknowledge exact irreversible empty-directory removal")
	var roots stringListFlag
	fs.Var(&roots, "search-root", "exact original source-root scope; repeatable")
	allowNetwork := fs.Bool("allow-network", false, "allow explicit network/UNC source roots")
	showAbsolute := fs.Bool("show-absolute-paths", false, "include exact journaled parent paths in output")
	timeout := fs.Duration("timeout", parentCleanupDefaultTimeout, "journal, observation, and removal wall-clock budget")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(a.stdout, flagOutput.String())
			return nil
		}
		return usageError("seed retire parent-cleanup resume: %s", sourceRetireFlagError(flagOutput.String(), err))
	}
	if fs.NArg() != 1 || *target == "" || *expected == "" || len(roots) == 0 || !*acknowledge {
		return usageError("seed retire parent-cleanup resume requires target, cleanup plan ID, search roots, acknowledgement, and one OPERATION_ID")
	}
	for _, root := range roots {
		if root == "" {
			return usageError("seed retire parent-cleanup resume requires every --search-root to be non-empty")
		}
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > parentCleanupMaxTimeout {
		return usageError("seed retire parent-cleanup resume --timeout must be greater than zero and no more than 1h")
	}
	operation, err := sourceretire.ParseParentCleanupOperationID(fs.Arg(0))
	derived, deriveErr := sourceretire.ParentCleanupOperationIDForPlanID(*expected)
	if err != nil || deriveErr != nil || derived != operation {
		return usageError("seed retire parent-cleanup resume requires matching canonical operation and cleanup plan IDs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, operationErr := sourceretire.ResumeParentCleanup(ctx, sourceretire.ParentCleanupResumeOptions{
		TargetRoot: *target, OperationID: operation, ExpectedCleanupPlanID: *expected,
		SearchRoots: append([]string(nil), roots...), AllowNetwork: *allowNetwork, Acknowledge: true,
		Limits: sourceretire.DefaultParentCleanupExecutionLimits(), ShowAbsolutePaths: *showAbsolute,
	})
	return a.finishParentCleanupExecution(*output, report, operationErr)
}

func (a *app) seedRetireParentCleanupStatus(args []string) error {
	fs := newFlagSet("seed retire parent-cleanup status")
	output := fs.String("output", "table", "table or json")
	target := fs.String("target", "", "existing materialized target root containing the parent-cleanup journal")
	showAbsolute := fs.Bool("show-absolute-paths", false, "include exact journaled parent paths in output")
	timeout := fs.Duration("timeout", parentCleanupDefaultTimeout, "private-journal inspection wall-clock budget")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *target == "" {
		return usageError("seed retire parent-cleanup status requires --target and exactly one OPERATION_ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > parentCleanupMaxTimeout {
		return usageError("seed retire parent-cleanup status --timeout must be greater than zero and no more than 1h")
	}
	operation, err := sourceretire.ParseParentCleanupOperationID(fs.Arg(0))
	if err != nil {
		return usageError("seed retire parent-cleanup status requires a canonical sha256 OPERATION_ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, operationErr := sourceretire.ParentCleanupStatus(ctx, sourceretire.ParentCleanupStatusOptions{
		TargetRoot: *target, OperationID: operation, Limits: sourceretire.DefaultParentCleanupExecutionLimits(),
		ShowAbsolutePaths: *showAbsolute,
	})
	return a.finishParentCleanupExecution(*output, report, operationErr)
}

func (a *app) finishParentCleanupExecution(output string, report sourceretire.ParentCleanupExecutionReport, operationErr error) error {
	if err := a.writeParentCleanupExecution(output, report); err != nil {
		return err
	}
	if errors.Is(operationErr, sourceretire.ErrExecutionIntegrity) || report.Outcome == sourceretire.ParentCleanupExecutionOutcomeIntegrity {
		return &integrityErr{message: "source-retirement parent cleanup failed integrity validation; see report"}
	}
	if report.Outcome == sourceretire.ParentCleanupExecutionOutcomeBlocked {
		return &inconclusiveErr{message: "source-retirement parent cleanup was blocked; see report"}
	}
	if operationErr != nil || report.Outcome == sourceretire.ParentCleanupExecutionOutcomeIncomplete || report.Outcome == sourceretire.ParentCleanupExecutionOutcomePartial {
		return fmt.Errorf("source-retirement parent cleanup was interrupted or remains partial; see report")
	}
	return nil
}

func (a *app) writeParentCleanupExecution(output string, report sourceretire.ParentCleanupExecutionReport) error {
	if output == "json" {
		return writeJSON(a.stdout, report, nil)
	}
	if output != "table" {
		return usageError("--output must be table or json")
	}
	return writeParentCleanupExecutionHuman(a.stdout, report)
}

func writeParentCleanupExecutionHuman(out io.Writer, report sourceretire.ParentCleanupExecutionReport) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nWRITES UNCERTAIN\t%t\nDELETION PERFORMED\t%t\n",
		terminalSafe(report.Outcome), terminalSafe(strings.Join(report.Effect, ",")), report.WritesPerformed,
		report.WritesUncertain, report.DeletionPerformed)
	writeSourceRetireFindings(w, "BLOCKERS", report.Blockers)
	writeSourceRetireFindings(w, "ISSUES", report.Issues)
	fmt.Fprintf(w, "\nOPERATION\nID\t%s\nPLAN\t%s\nSTATUS\t%s\nPHASE\t%s\nRESUMABLE\t%t\nINTENT\t%s\nCOMPLETION\t%s\n",
		terminalSafe(valueOrDash(report.Operation.ID)), terminalSafe(valueOrDash(report.Operation.PlanID)), terminalSafe(report.Operation.Status),
		terminalSafe(report.Operation.Phase), report.Operation.Resumable, terminalSafe(valueOrDash(report.Operation.IntentID)),
		terminalSafe(valueOrDash(report.Operation.CompletionID)))
	fmt.Fprintf(w, "\nTARGET / LINEAGE\nROOT IDENTITY BOUND\t%t\nSAME FILESYSTEM\t%t\nRETIREMENT OPERATION\t%s\nRETIREMENT PLAN\t%s\nRETIREMENT COMPLETION\t%s\nSEARCH SCOPE\t%s\n",
		report.Target.RootIdentityBound, report.Target.SameFilesystem, terminalSafe(valueOrDash(report.RetirementOperationID)),
		terminalSafe(valueOrDash(report.RetirementPlanID)), terminalSafe(valueOrDash(report.RetirementCompletionID)),
		terminalSafe(valueOrDash(report.SearchScopeID)))
	if report.Eligibility != nil {
		fmt.Fprintf(w, "\nSAME-CALL REVIEW\nOUTCOME\t%s\nPLAN\t%s\nCANDIDATE PARENTS\t%d\n",
			terminalSafe(report.Eligibility.Outcome), terminalSafe(valueOrDash(report.Eligibility.Plan.ID)), report.Eligibility.Plan.CandidateParents)
	}
	if len(report.Directories) > 0 {
		fmt.Fprintln(w, "\nDIRECTORIES")
		fmt.Fprintln(w, "SEQ\tSTATUS\tRETIRED FILES\tPARENT\tBASIS")
		for _, directory := range report.Directories {
			path := directory.ParentPathRef
			if directory.ParentPath != "" {
				path = directory.ParentPath
			}
			fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\n", directory.Sequence, terminalSafe(directory.Status), directory.RetiredFiles,
				terminalSafe(path), terminalSafe(valueOrDash(directory.RemovalBasis)))
		}
	}
	fmt.Fprintf(w, "\nWRITE BREAKDOWN\nSCRATCH FILES\t%d\nSCRATCH BYTES\t%d\nJOURNAL OBJECTS\t%d\nEXISTING JOURNAL OBJECTS\t%d\nREMOVAL ATTEMPTS\t%d\nDIRECTORIES REMOVED\t%d\nDURABILITY UNCONFIRMED\t%d\nAMBIGUOUS PUBLICATIONS\t%d\nAMBIGUOUS REMOVALS\t%d\n",
		report.Writes.ScratchFilesCreated, report.Writes.ScratchBytesWritten, report.Writes.JournalObjectsPublished,
		report.Writes.JournalObjectsExisting, report.Writes.RemovalAttempts, report.Writes.DirectoriesRemoved,
		report.Writes.DurabilityUnconfirmed, report.Writes.AmbiguousPublications, report.Writes.AmbiguousRemovals)
	fmt.Fprintf(w, "\nLIMITS / USED\nMAX PARENTS\t%d\nMAX PATH BYTES\t%d\nMAX INTENT BYTES\t%d\nMAX MARKER BYTES\t%d\nMAX SCRATCH BYTES\t%d\nMAX ENTRY NAME BYTES\t%d\nPARENTS\t%d\nPATH BYTES\t%d\nDIRECTORY READS\t%d\nENTRIES OBSERVED\t%d\nENTRY NAME BYTES\t%d\nJOURNAL BYTES\t%d\n",
		report.Limits.MaxParents, report.Limits.MaxPathBytes, report.Limits.MaxIntentBytes, report.Limits.MaxMarkerBytes,
		report.Limits.MaxScratchBytes, report.Limits.MaxEntryNameBytes, report.Used.ParentsConsidered, report.Used.ParentPathBytes,
		report.Used.DirectoryReads, report.Used.EntriesObserved, report.Used.EntryNameBytes, report.Used.JournalBytes)
	if len(report.Warnings) > 0 {
		fmt.Fprintln(w, "\nWARNINGS")
		for _, warning := range report.Warnings {
			fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
		}
	}
	return w.Flush()
}

func (a *app) finishParentCleanupPlan(output string, requireCleanable bool, report sourceretire.ParentCleanupReport, planErr error) error {
	if err := a.writeParentCleanupPlan(output, report); err != nil {
		return err
	}
	if errors.Is(planErr, sourceretire.ErrExecutionIntegrity) || report.Outcome == sourceretire.ParentCleanupOutcomeIntegrity {
		return &integrityErr{message: "source-retirement parent-cleanup evidence failed integrity validation; see report"}
	}
	if requireCleanable && report.Outcome != sourceretire.ParentCleanupOutcomeEligible {
		return &inconclusiveErr{message: "source-retirement parent-cleanup outcome is not eligible_for_separate_cleanup_review"}
	}
	// Like the other read-only evidence commands, a complete report exits zero
	// by default even when it is blocked or incomplete. The explicit require
	// flag converts every non-eligible outcome to exit 4.
	return nil
}

func (a *app) writeParentCleanupPlan(output string, report sourceretire.ParentCleanupReport) error {
	if output == "json" {
		return writeJSON(a.stdout, report, nil)
	}
	if output != "table" {
		return usageError("--output must be table or json")
	}
	return writeParentCleanupHuman(a.stdout, report)
}

func writeParentCleanupHuman(out io.Writer, report sourceretire.ParentCleanupReport) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES\t%d\nDELETION PERFORMED\t%t\nCLEANUP AUTHORITY\t%s\n",
		terminalSafe(report.Outcome), terminalSafe(strings.Join(report.Effect, "+")), report.WritesPerformed,
		report.DeletionPerformed, terminalSafe(report.Plan.CleanupAuthority))
	writeSourceRetireFindings(w, "BLOCKERS", report.Blockers)
	writeSourceRetireFindings(w, "ISSUES", report.Issues)
	fmt.Fprintf(w, "\nPLAN\nID\t%s\nRETIREMENT OPERATION\t%s\nRETIREMENT PLAN\t%s\nRETIREMENT COMPLETION\t%s\nCURRENT ABSENCE\t%s\nSEARCH SCOPE\t%s\n",
		terminalSafe(valueOrDash(report.Plan.ID)), terminalSafe(valueOrDash(report.Plan.RetirementOperationID)),
		terminalSafe(valueOrDash(report.Plan.RetirementPlanID)), terminalSafe(valueOrDash(report.Plan.RetirementCompletionID)),
		terminalSafe(valueOrDash(report.Plan.CurrentAbsenceID)), terminalSafe(valueOrDash(report.Plan.SearchScopeID)))
	fmt.Fprintf(w, "\nPARENT CLEANUP\nRETIRED FILES\t%d\nPARENTS\t%d\nCANDIDATES\t%d\nRETAINED NONEMPTY\t%d\nPROTECTED ROOTS\t%d\nOBSERVED\t%s .. %s\nASSURANCE\t%s\n",
		report.Plan.RetiredFiles, report.Plan.ParentsConsidered, report.Plan.CandidateParents, report.Plan.RetainedParents,
		report.Plan.ProtectedRoots, parentCleanupTime(report.ParentObservedAtStart), parentCleanupTime(report.ParentObservedAtEnd),
		"identity_bound_two_pass_empty_immediate_parents_bracketed_non_atomic")
	if len(report.Plan.Directories) > 0 {
		fmt.Fprintln(w, "\nDIRECTORIES")
		fmt.Fprintln(w, "SEQ\tSTATUS\tRETIRED FILES\tPASSES\tENTRIES\tPARENT")
		for _, directory := range report.Plan.Directories {
			path := directory.ParentPathRef
			if directory.ParentPath != "" {
				path = directory.ParentPath
			}
			fmt.Fprintf(w, "%d\t%s\t%d\t%d\t%d\t%s\n", directory.Sequence, terminalSafe(directory.Status),
				directory.RetiredFiles, directory.ObservationPasses, directory.EntriesObserved, terminalSafe(path))
		}
	}
	fmt.Fprintf(w, "\nLIMITS / USED\nPARENTS\t%d / %d\nPATH BYTES\t%d / %d\nENTRY NAME BYTES\t%d / %d\nDIRECTORY READS\t%d\nENTRIES OBSERVED\t%d\n",
		report.Used.ParentsConsidered, report.Limits.MaxParents, report.Used.ParentPathBytes, report.Limits.MaxPathBytes,
		report.Used.EntryNameBytes, report.Limits.MaxEntryNameBytes, report.Used.DirectoryReads, report.Used.EntriesObserved)
	if len(report.Warnings) > 0 {
		fmt.Fprintln(w, "\nWARNINGS")
		for _, warning := range report.Warnings {
			fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
		}
	}
	return w.Flush()
}

func parentCleanupTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return terminalSafe(value.UTC().Format(time.RFC3339Nano))
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
