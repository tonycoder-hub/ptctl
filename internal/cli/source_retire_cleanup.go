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
	default:
		return usageError("unknown seed retire parent-cleanup subcommand %q", args[0])
	}
}

func (a *app) seedRetireParentCleanupHelp() {
	fmt.Fprint(a.stdout, `Usage:
  ptctl seed retire parent-cleanup plan --target PATH --retirement-operation ID --retirement-plan-id ID --search-root PATH [--search-root PATH...] [flags]

The plan is strictly zero-write. It reads one explicit live terminal source-
retirement journal, proves the exact retired names currently absent in the same
root scope, and considers only their immediate parents. Search roots and higher
ancestors are always protected. A candidate must retain the journaled identity
and be observed empty twice. Child names are never reported.

A pruned retirement tombstone is historical evidence only and cannot recreate
the path authority required here. This command grants no deletion authority;
there is no parent-cleanup execution command in this slice.
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
