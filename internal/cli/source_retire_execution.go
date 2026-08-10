package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/sourceretire"
)

type preparedSourceRetireLocal struct {
	meta       *metafile.MetaInfo
	final      *materialize.VerifiedFinal
	activation *clientactivate.VerifiedCompletion
	currentUse *clientactivate.CurrentUseAuthority
	discovery  *seed.DiscoveryResult
}

func (a *app) seedRetireRun(args []string) error {
	fs := newFlagSet("seed retire run")
	var flagOutput strings.Builder
	fs.SetOutput(&flagOutput)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  ptctl seed retire run (live selectors from seed retire plan) --expect-plan-id ID --acknowledge-source-deletion [flags]")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Rebuilds the live review, journals exact per-name deletion, and never removes directories, aliases, empty files, padding, or the published final.")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
	}
	values := addSourceRetireFlags(fs, false)
	expected := fs.String("expect-plan-id", "", "reviewed sha256 source-retirement plan ID")
	acknowledge := fs.Bool("acknowledge-source-deletion", false, "acknowledge irreversible identity-bound source-name deletion")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(a.stdout, flagOutput.String())
			return nil
		}
		return usageError("seed retire run: %s", sourceRetireFlagError(flagOutput.String(), err))
	}
	if fs.NArg() != 0 {
		return usageError("seed retire run accepts flags only")
	}
	if !validSourceRetireExecutionPlanID(*expected) || !*acknowledge {
		return usageError("seed retire run requires a canonical --expect-plan-id and --acknowledge-source-deletion")
	}
	prepared, err := values.validate(fs, "seed retire run")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), prepared.timeout)
	defer cancel()
	operation, err := sourceretire.OperationIDForPlanID(*expected)
	if err != nil {
		return usageError("seed retire run requires a canonical --expect-plan-id")
	}
	status, statusErr := sourceretire.Status(ctx, sourceretire.StatusOptions{TargetRoot: prepared.targetRoot,
		OperationID: operation, Limits: sourceretire.DefaultExecutionLimits()})
	if status.Operation.Status != "not_found" {
		switch status.Operation.Status {
		case "retained", "pruning", "retention_initializing", "historical_complete", "forgetting":
			return a.finishSourceRetireExecution(prepared.output, status, statusErr)
		}
		if statusErr != nil || status.Outcome == sourceretire.ExecutionOutcomeIntegrity || status.Outcome == sourceretire.ExecutionOutcomeBlocked {
			return a.finishSourceRetireExecution(prepared.output, status, statusErr)
		}
	}
	local, err := a.prepareSourceRetireLocal(ctx, prepared, true)
	if err != nil {
		return err
	}
	review := sourceretire.BuildOptions{Meta: local.meta, Discovery: local.discovery, Final: local.final, Activation: local.activation, ShowAbsolutePaths: false}
	runOptions := sourceretire.RunOptions{Review: review, ExpectedPlanID: *expected,
		SearchRoots: append([]string(nil), prepared.discovery.SearchRoots...), Acknowledge: true, Limits: sourceretire.DefaultExecutionLimits()}
	if !sourceRetireNeedsClientSession(local.meta, *local.discovery, local.final) {
		report, operationErr := sourceretire.Run(ctx, runOptions)
		return a.finishSourceRetireExecution(prepared.output, report, operationErr)
	}
	credential, err := readDownloaderCredential(a.stdin, prepared.username)
	if err != nil {
		return err
	}
	session, openErr := prepared.adapter.OpenReadSession(ctx, credential)
	if openErr != nil {
		requests, _ := downloader.RequestsMadeFromError(openErr)
		session = &failedSourceRetireSession{requests: requests}
	}
	defer session.Close()
	runOptions.Review.ClientUse, runOptions.Review.ClientSession = local.currentUse, session
	report, operationErr := sourceretire.Run(ctx, runOptions)
	return a.finishSourceRetireExecution(prepared.output, report, operationErr)
}

func (a *app) seedRetireResume(args []string) error {
	fs := newFlagSet("seed retire resume")
	var flagOutput strings.Builder
	fs.SetOutput(&flagOutput)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  ptctl seed retire resume (same local/live selectors) --expect-plan-id ID --acknowledge-source-deletion [flags] OPERATION_ID")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Resumes one explicit private journal. It never rediscovers or selects a latest operation.")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
	}
	values := addSourceRetireFlags(fs, false)
	expected := fs.String("expect-plan-id", "", "reviewed sha256 source-retirement plan ID")
	acknowledge := fs.Bool("acknowledge-source-deletion", false, "acknowledge irreversible identity-bound source-name deletion")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(a.stdout, flagOutput.String())
			return nil
		}
		return usageError("seed retire resume: %s", sourceRetireFlagError(flagOutput.String(), err))
	}
	if fs.NArg() != 1 {
		return usageError("seed retire resume requires exactly one OPERATION_ID")
	}
	operation, err := sourceretire.ParseOperationID(fs.Arg(0))
	if err != nil || !validSourceRetireExecutionPlanID(*expected) || !*acknowledge {
		return usageError("seed retire resume requires canonical operation/plan IDs and --acknowledge-source-deletion")
	}
	prepared, err := values.validate(fs, "seed retire resume")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), prepared.timeout)
	defer cancel()
	status, statusErr := sourceretire.Status(ctx, sourceretire.StatusOptions{TargetRoot: prepared.targetRoot,
		OperationID: operation, Limits: sourceretire.DefaultExecutionLimits()})
	if statusErr != nil || status.Outcome == sourceretire.ExecutionOutcomeIntegrity || status.Outcome == sourceretire.ExecutionOutcomeBlocked {
		return a.finishSourceRetireExecution(prepared.output, status, statusErr)
	}
	if status.Operation.Status == "retained" || status.Operation.Status == "pruning" || status.Operation.Status == "retention_initializing" || status.Operation.Status == "forgetting" {
		return a.finishSourceRetireExecution(prepared.output, status, nil)
	}
	if status.Operation.PlanID != *expected {
		report, operationErr := sourceretire.Resume(ctx, sourceretire.ResumeOptions{TargetRoot: prepared.targetRoot,
			OperationID: operation, ExpectedPlanID: *expected, Acknowledge: true, Limits: sourceretire.DefaultExecutionLimits()})
		return a.finishSourceRetireExecution(prepared.output, report, operationErr)
	}
	local, err := a.prepareSourceRetireLocal(ctx, prepared, false)
	if err != nil {
		return err
	}
	resume := sourceretire.ResumeOptions{Meta: local.meta, Final: local.final, Activation: local.activation, ClientUse: local.currentUse,
		TargetRoot: prepared.targetRoot, OperationID: operation, ExpectedPlanID: *expected,
		SearchRoots: append([]string(nil), prepared.discovery.SearchRoots...), Acknowledge: true, Limits: sourceretire.DefaultExecutionLimits()}
	// This local, zero-write pass proves the explicit journal, search-root scope,
	// final, activation, and remaining source names before credential I/O.
	preflight, preflightErr := sourceretire.Resume(ctx, resume)
	addSourceRetireExecutionEffects(&preflight, "read_exact_materialized_final", "read_private_client_activation_journal")
	if preflightErr != nil || preflight.Outcome == sourceretire.ExecutionOutcomeIntegrity || preflight.Outcome == sourceretire.ExecutionOutcomeIncomplete ||
		preflight.Outcome == sourceretire.ExecutionOutcomePartial || preflight.Outcome == sourceretire.ExecutionOutcomeAlreadyRetired ||
		!sourceRetireExecutionFinding(preflight.Blockers, "client.authority_unavailable") {
		return a.finishSourceRetireExecution(prepared.output, preflight, preflightErr)
	}
	credential, err := readDownloaderCredential(a.stdin, prepared.username)
	if err != nil {
		return err
	}
	session, openErr := prepared.adapter.OpenReadSession(ctx, credential)
	if openErr != nil {
		requests, _ := downloader.RequestsMadeFromError(openErr)
		session = &failedSourceRetireSession{requests: requests}
	}
	defer session.Close()
	resume.ClientSession = session
	report, operationErr := sourceretire.Resume(ctx, resume)
	addSourceRetireExecutionEffects(&report, "read_exact_materialized_final", "read_private_client_activation_journal")
	return a.finishSourceRetireExecution(prepared.output, report, operationErr)
}

func (a *app) seedRetireStatus(args []string) error {
	fs := newFlagSet("seed retire status")
	output := fs.String("output", "table", "table or json")
	target := fs.String("target", "", "existing local materialized target root")
	timeout := fs.Duration("timeout", time.Minute, "journal inspection wall-clock budget")
	listDefaults := sourceretire.DefaultExecutionOperationListLimits()
	maxRootEntries := fs.Int("max-root-entries", listDefaults.MaxRootEntries, "maximum target-root entries examined when no operation ID is given")
	maxOperations := fs.Int("max-operations", listDefaults.MaxOperations, "maximum operation IDs retained when no operation ID is given")
	maxNameBytes := fs.Int64("max-name-bytes", listDefaults.MaxNameBytes, "maximum target-root name bytes retained when no operation ID is given")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 || *target == "" {
		return usageError("seed retire status requires --target and at most one OPERATION_ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > time.Hour {
		return usageError("seed retire status --timeout must be greater than zero and no more than 1h")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if fs.NArg() == 0 {
		limits := sourceretire.ExecutionOperationListLimits{MaxRootEntries: *maxRootEntries, MaxOperations: *maxOperations, MaxNameBytes: *maxNameBytes}
		if err := limits.Validate(); err != nil {
			return usageError("seed retire status list limits are invalid")
		}
		result, listErr := sourceretire.ListExecutionOperations(ctx, *target, limits)
		if writeErr := a.writeSourceRetireOperationList(*output, result); writeErr != nil {
			return writeErr
		}
		if listErr != nil {
			if errors.Is(listErr, sourceretire.ErrExecutionPolicy) {
				return &inconclusiveErr{message: "source retirement operation listing was blocked; see report"}
			}
			return fmt.Errorf("source retirement operation listing failed; see report")
		}
		if !result.Complete {
			return &inconclusiveErr{message: "source retirement operation listing was incomplete; see report"}
		}
		return nil
	}
	for _, name := range []string{"max-root-entries", "max-operations", "max-name-bytes"} {
		if flagWasSet(fs, name) {
			return usageError("--%s applies only to status without an OPERATION_ID", name)
		}
	}
	operation, err := sourceretire.ParseOperationID(fs.Arg(0))
	if err != nil {
		return usageError("seed retire status requires a canonical sha256 OPERATION_ID")
	}
	report, operationErr := sourceretire.Status(ctx, sourceretire.StatusOptions{TargetRoot: *target,
		OperationID: operation, Limits: sourceretire.DefaultExecutionLimits()})
	if err := a.writeSourceRetireExecutionReport(*output, report); err != nil {
		return err
	}
	if errors.Is(operationErr, sourceretire.ErrExecutionIntegrity) || report.Outcome == sourceretire.ExecutionOutcomeIntegrity {
		return &integrityErr{message: "source retirement journal failed integrity validation; see report"}
	}
	if report.Outcome == sourceretire.ExecutionOutcomeBlocked {
		return &inconclusiveErr{message: "source retirement operation was not found or could not be inspected; see report"}
	}
	if operationErr != nil {
		return fmt.Errorf("source retirement status was interrupted; see report")
	}
	return nil
}

func (a *app) seedRetirePrune(args []string) error {
	fs := newFlagSet("seed retire prune")
	output := fs.String("output", "table", "table or json")
	target := fs.String("target", "", "existing local materialized target root")
	expected := fs.String("expect-plan-id", "", "reviewed sha256 source-retirement plan ID")
	acknowledge := fs.Bool("acknowledge-operation-state-deletion", false, "acknowledge deletion of private operation state and retention of a tombstone")
	timeout := fs.Duration("timeout", time.Minute, "operation-state pruning wall-clock budget")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *target == "" {
		return usageError("seed retire prune requires --target and exactly one OPERATION_ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if !validSourceRetireExecutionPlanID(*expected) {
		return usageError("seed retire prune requires a canonical --expect-plan-id")
	}
	if !*acknowledge {
		return usageError("seed retire prune requires --acknowledge-operation-state-deletion")
	}
	if *timeout <= 0 || *timeout > time.Hour {
		return usageError("seed retire prune --timeout must be greater than zero and no more than 1h")
	}
	operation, err := sourceretire.ParseOperationID(fs.Arg(0))
	if err != nil {
		return usageError("seed retire prune requires a canonical sha256 OPERATION_ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, operationErr := sourceretire.PruneExecution(ctx, sourceretire.ExecutionPruneOptions{
		TargetRoot: *target, OperationID: operation, ExpectedPlanID: *expected, Acknowledge: true,
		JournalLimits: sourceretire.DefaultExecutionLimits(), RetentionLimits: sourceretire.DefaultExecutionRetentionLimits(),
	})
	if err := a.writeSourceRetireRetentionReport(*output, report); err != nil {
		return err
	}
	if errors.Is(operationErr, sourceretire.ErrExecutionIntegrity) || report.Outcome == sourceretire.ExecutionRetentionOutcomeIntegrityFailed {
		return &integrityErr{message: "source retirement retention state failed integrity validation; see report"}
	}
	if report.Outcome == sourceretire.ExecutionRetentionOutcomeBlocked {
		return &inconclusiveErr{message: "source retirement operation-state pruning was blocked; see report"}
	}
	if operationErr != nil || report.Outcome == sourceretire.ExecutionRetentionOutcomeInterrupted {
		return fmt.Errorf("source retirement operation-state pruning was interrupted; see report")
	}
	return nil
}

func (a *app) seedRetireForget(args []string) error {
	fs := newFlagSet("seed retire forget")
	output := fs.String("output", "table", "table or json")
	target := fs.String("target", "", "existing local materialized target root")
	expected := fs.String("expect-plan-id", "", "reviewed sha256 source-retirement plan ID")
	acknowledge := fs.Bool("acknowledge-historical-evidence-deletion", false, "acknowledge irreversible deletion of the retained tombstone and final historical attribution")
	timeout := fs.Duration("timeout", time.Minute, "historical-evidence deletion wall-clock budget")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *target == "" {
		return usageError("seed retire forget requires --target and exactly one OPERATION_ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if !validSourceRetireExecutionPlanID(*expected) {
		return usageError("seed retire forget requires a canonical --expect-plan-id")
	}
	if !*acknowledge {
		return usageError("seed retire forget requires --acknowledge-historical-evidence-deletion")
	}
	if *timeout <= 0 || *timeout > time.Hour {
		return usageError("seed retire forget --timeout must be greater than zero and no more than 1h")
	}
	operation, err := sourceretire.ParseOperationID(fs.Arg(0))
	if err != nil {
		return usageError("seed retire forget requires a canonical sha256 OPERATION_ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, operationErr := sourceretire.ForgetExecutionTombstone(ctx, sourceretire.ExecutionForgetOptions{
		TargetRoot: *target, OperationID: operation, ExpectedPlanID: *expected, Acknowledge: true,
		Limits: sourceretire.DefaultExecutionForgetLimits(),
	})
	if err := a.writeSourceRetireForgetReport(*output, report); err != nil {
		return err
	}
	if errors.Is(operationErr, sourceretire.ErrExecutionIntegrity) || report.Outcome == sourceretire.ExecutionForgetOutcomeIntegrityFailed {
		return &integrityErr{message: "source retirement historical evidence failed integrity validation; see report"}
	}
	if report.Outcome == sourceretire.ExecutionForgetOutcomeBlocked {
		return &inconclusiveErr{message: "source retirement historical-evidence deletion was blocked; see report"}
	}
	if report.Outcome == sourceretire.ExecutionForgetOutcomeAbsentUnattributed {
		return fmt.Errorf("source retirement historical evidence is absent without a remaining attribution marker; see report")
	}
	if operationErr != nil || report.Outcome != sourceretire.ExecutionForgetOutcomeForgotten {
		return fmt.Errorf("source retirement historical-evidence deletion was interrupted or its durability is unconfirmed; see report")
	}
	return nil
}

func (a *app) prepareSourceRetireLocal(ctx context.Context, prepared preparedSourceRetire, discover bool) (preparedSourceRetireLocal, error) {
	var result preparedSourceRetireLocal
	meta, err := loadMetafileInput(ctx, prepared.input)
	var inputIntegrity *integrityErr
	if errors.As(err, &inputIntegrity) {
		return result, inputIntegrity
	}
	if errors.Is(err, metastore.ErrCorruptArtifact) {
		return result, &integrityErr{message: "source retirement metafile failed integrity validation"}
	}
	if err != nil {
		return result, fmt.Errorf("source retirement metafile input could not be read")
	}
	final, _, err := materialize.VerifyCurrentFinal(ctx, materialize.FinalProofOptions{Meta: meta, TargetRoot: prepared.targetRoot,
		OperationID: prepared.materializeOperation, ExpectedPlanID: prepared.materializePlanID, Limits: materialize.DefaultLimits()})
	if errors.Is(err, materialize.ErrIntegrity) {
		return result, &integrityErr{message: "source retirement current final failed exact verification"}
	}
	if err != nil {
		return result, fmt.Errorf("source retirement current final could not be verified")
	}
	activation, _, err := clientactivate.VerifyCompletion(ctx, clientactivate.CompletionProofOptions{TargetRoot: prepared.targetRoot,
		OperationID: prepared.activationOperation, ExpectedPlanID: prepared.activationPlanID})
	if errors.Is(err, clientactivate.ErrIntegrity) {
		return result, &integrityErr{message: "source retirement client activation journal failed integrity validation"}
	}
	if err != nil {
		return result, fmt.Errorf("source retirement terminal client completion could not be verified")
	}
	currentUse, err := clientactivate.PrepareCurrentUse(final, activation, clientactivate.CurrentUseOptions{
		ClientConfigID: prepared.clientConfigID, HostRoot: prepared.hostRoot, ClientRoot: prepared.clientRoot,
		ClientWindows: prepared.clientWindows, FileLimits: downloader.DefaultJobFileLedgerLimits(),
	})
	if errors.Is(err, clientactivate.ErrIntegrity) {
		return result, &integrityErr{message: "source retirement live-client selectors disagree with the terminal activation"}
	}
	if errors.Is(err, clientactivate.ErrPolicy) {
		return result, &inconclusiveErr{message: "source retirement live-client selectors disagree with the reviewed activation"}
	}
	if err != nil {
		return result, fmt.Errorf("source retirement live-client authority could not be prepared")
	}
	result = preparedSourceRetireLocal{meta: meta, final: final, activation: activation, currentUse: currentUse}
	if discover {
		discovery, discoverErr := seed.Discover(ctx, meta, prepared.discovery)
		if discoverErr != nil {
			return preparedSourceRetireLocal{}, fmt.Errorf("source retirement live source discovery failed")
		}
		result.discovery = &discovery
	}
	return result, nil
}

func (a *app) finishSourceRetireExecution(output string, report sourceretire.ExecutionReport, operationErr error) error {
	if err := a.writeSourceRetireExecutionReport(output, report); err != nil {
		return err
	}
	if errors.Is(operationErr, sourceretire.ErrExecutionIntegrity) || report.Outcome == sourceretire.ExecutionOutcomeIntegrity {
		return &integrityErr{message: "source retirement execution failed integrity validation; see report"}
	}
	if report.Outcome == sourceretire.ExecutionOutcomeBlocked {
		return &inconclusiveErr{message: "source retirement execution was blocked; see report"}
	}
	if operationErr != nil || report.Outcome == sourceretire.ExecutionOutcomeIncomplete || report.Outcome == sourceretire.ExecutionOutcomePartial {
		return fmt.Errorf("source retirement execution was interrupted or remains partial; see report")
	}
	return nil
}

func (a *app) writeSourceRetireExecutionReport(output string, report sourceretire.ExecutionReport) error {
	if output == "json" {
		return writeJSON(a.stdout, report, nil)
	}
	if output != "table" {
		return usageError("--output must be table or json")
	}
	return writeSourceRetireExecutionHuman(a.stdout, report)
}

func (a *app) writeSourceRetireOperationList(output string, result sourceretire.ExecutionOperationListResult) error {
	if output == "json" {
		return writeJSON(a.stdout, result, nil)
	}
	if output != "table" {
		return usageError("--output must be table or json")
	}
	w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "COMPLETE\t%t\nEFFECT\t%s\nWRITES PERFORMED\t%d\nSTOP REASON\t%s\nMAX ROOT ENTRIES\t%d\nMAX OPERATIONS\t%d\nMAX NAME BYTES\t%d\nROOT ENTRIES EXAMINED\t%d\nROOT NAME BYTES\t%d\n",
		result.Complete, terminalSafe(result.Effect), result.WritesPerformed, terminalSafe(materializeValueOr(result.StopReason, "none")),
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

func (a *app) writeSourceRetireRetentionReport(output string, report sourceretire.ExecutionRetentionReport) error {
	if output == "json" {
		return writeJSON(a.stdout, report, nil)
	}
	if output != "table" {
		return usageError("--output must be table or json")
	}
	return writeSourceRetireRetentionHuman(a.stdout, report)
}

func (a *app) writeSourceRetireForgetReport(output string, report sourceretire.ExecutionForgetReport) error {
	if output == "json" {
		return writeJSON(a.stdout, report, nil)
	}
	if output != "table" {
		return usageError("--output must be table or json")
	}
	return writeSourceRetireForgetHuman(a.stdout, report)
}

func writeSourceRetireForgetHuman(out io.Writer, report sourceretire.ExecutionForgetReport) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nWRITES UNCERTAIN\t%t\n",
		terminalSafe(report.Outcome), terminalSafe(strings.Join(report.Effect, ",")), report.WritesPerformed, report.WritesUncertain)
	writeSourceRetireFindings(w, "BLOCKERS", report.Blockers)
	writeSourceRetireFindings(w, "ISSUES", report.Issues)
	fmt.Fprintf(w, "\nOPERATION\nID\t%s\nPLAN\t%s\nSTATUS\t%s\nPHASE\t%s\nRESUMABLE\t%t\n",
		terminalSafe(report.Operation.ID), terminalSafe(report.Operation.PlanID), terminalSafe(report.Operation.Status),
		terminalSafe(report.Operation.Phase), report.Operation.Resumable)
	fmt.Fprintf(w, "\nAUTHORITY\nSTATE\t%s\nFORGET MARKER\t%s\nMARKER DURABLE\t%t\nRETENTION INTENT\t%s\nRETENTION COMPLETE\t%s\nEXACT TOMBSTONE EVIDENCE AVAILABLE\t%t\nTARGET HISTORICAL EVIDENCE ERASED\t%t\n",
		terminalSafe(report.Authority.State), terminalSafe(report.Authority.MarkerID), report.Authority.MarkerDurable,
		terminalSafe(report.Authority.RetentionIntentMarkerID), terminalSafe(report.Authority.RetentionCompleteMarkerID),
		report.Authority.ExactTombstoneEvidenceAvailable, report.Authority.TargetHistoricalEvidenceErased)
	fmt.Fprintf(w, "\nHISTORICAL PROOF\nBASIS\t%s\nINTENT\t%s\nTERMINAL COMPLETION\t%s\nFILES RETIRED\t%d\nBYTES RETIRED\t%d\nASSURANCE\t%s\n",
		terminalSafe(report.Proof.Basis), terminalSafe(report.Proof.IntentID), terminalSafe(report.Proof.TerminalCompletionID),
		report.Proof.FilesRetired, report.Proof.BytesRetired, terminalSafe(report.Proof.Assurance))
	fmt.Fprintf(w, "\nWRITE BREAKDOWN\nMARKER TEMPORARIES\t%d\nMARKER PUBLICATIONS\t%d\nAMBIGUOUS MARKER PUBLICATIONS\t%d\nREMOVAL ATTEMPTS\t%d\nFILES REMOVED\t%d\nDIRECTORIES REMOVED\t%d\nBYTES REMOVED\t%d\nAMBIGUOUS REMOVALS\t%d\n",
		report.Writes.MarkerTemporaryFiles, report.Writes.MarkerPublications, report.Writes.AmbiguousMarkerPublications, report.Writes.RemovalAttempts,
		report.Writes.FilesRemoved, report.Writes.DirectoriesRemoved, report.Writes.BytesRemoved, report.Writes.AmbiguousRemovals)
	fmt.Fprintf(w, "\nLIMITS\nMAX MARKER BYTES\t%d\n", report.Limits.MaxMarkerBytes)
	if len(report.Warnings) > 0 {
		fmt.Fprintln(w, "\nWARNINGS")
		for _, warning := range report.Warnings {
			fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
		}
	}
	return w.Flush()
}

func writeSourceRetireRetentionHuman(out io.Writer, report sourceretire.ExecutionRetentionReport) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nWRITES UNCERTAIN\t%t\n",
		terminalSafe(report.Outcome), terminalSafe(strings.Join(report.Effect, ",")), report.WritesPerformed, report.WritesUncertain)
	writeSourceRetireFindings(w, "BLOCKERS", report.Blockers)
	writeSourceRetireFindings(w, "ISSUES", report.Issues)
	fmt.Fprintf(w, "\nOPERATION\nID\t%s\nPLAN\t%s\nSTATUS\t%s\nPHASE\t%s\nRESUMABLE\t%t\n",
		terminalSafe(report.Operation.ID), terminalSafe(report.Operation.PlanID), terminalSafe(report.Operation.Status),
		terminalSafe(report.Operation.Phase), report.Operation.Resumable)
	fmt.Fprintf(w, "\nTOMBSTONE\nSTATE\t%s\nINTENT MARKER\t%s\nCOMPLETE MARKER\t%s\nINTENT DURABLE\t%t\nCOMPLETION DURABLE\t%t\nEXACT\t%t\nPRUNE RESUMABLE\t%t\n",
		terminalSafe(report.Markers.State), terminalSafe(report.Markers.IntentMarkerID), terminalSafe(report.Markers.CompleteMarkerID),
		report.Markers.IntentDurable, report.Markers.CompletionDurable, report.Markers.ExactTombstone, report.Markers.PruneResumable)
	fmt.Fprintf(w, "\nHISTORICAL PROOF\nBASIS\t%s\nINTENT\t%s\nTERMINAL COMPLETION\t%s\nFILES RETIRED\t%d\nBYTES RETIRED\t%d\nASSURANCE\t%s\n",
		terminalSafe(report.Proof.Basis), terminalSafe(report.Proof.IntentID), terminalSafe(report.Proof.TerminalCompletionID),
		report.Proof.FilesRetired, report.Proof.BytesRetired, terminalSafe(report.Proof.Assurance))
	fmt.Fprintf(w, "\nWRITE BREAKDOWN\nCONTROL DIRECTORIES\t%d\nMARKER TEMPORARIES\t%d\nMARKER PUBLICATIONS\t%d\nREMOVAL ATTEMPTS\t%d\nFILES REMOVED\t%d\nDIRECTORIES REMOVED\t%d\nBYTES REMOVED\t%d\nAMBIGUOUS REMOVALS\t%d\n",
		report.Writes.ControlDirectoriesCreated, report.Writes.MarkerTemporaryFiles, report.Writes.MarkerPublications,
		report.Writes.RemovalAttempts, report.Writes.FilesRemoved, report.Writes.DirectoriesRemoved,
		report.Writes.BytesRemoved, report.Writes.AmbiguousRemovals)
	fmt.Fprintf(w, "\nLIMITS / USED\nMAX OBJECTS\t%d\nMAX PATH BYTES\t%d\nMAX BYTES\t%d\nMAX MEMORY BYTES\t%d\nOBJECTS\t%d\nPATH BYTES\t%d\nBYTES\t%d\nMEMORY BYTES\t%d\n",
		report.Limits.MaxObjects, report.Limits.MaxPathBytes, report.Limits.MaxBytes, report.Limits.MaxMemoryBytes,
		report.Used.ObjectsConsidered, report.Used.PathBytesConsidered, report.Used.BytesConsidered, report.Used.MemoryBytesConsidered)
	if len(report.Warnings) > 0 {
		fmt.Fprintln(w, "\nWARNINGS")
		for _, warning := range report.Warnings {
			fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
		}
	}
	return w.Flush()
}

func writeSourceRetireExecutionHuman(out io.Writer, report sourceretire.ExecutionReport) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nWRITES UNCERTAIN\t%t\nDELETION PERFORMED\t%t\n",
		terminalSafe(report.Outcome), terminalSafe(strings.Join(report.Effect, ",")), report.WritesPerformed, report.WritesUncertain, report.DeletionPerformed)
	writeSourceRetireFindings(w, "BLOCKERS", report.Blockers)
	writeSourceRetireFindings(w, "ISSUES", report.Issues)
	fmt.Fprintf(w, "\nOPERATION\nID\t%s\nPLAN\t%s\nSTATUS\t%s\nPHASE\t%s\nRESUMABLE\t%t\nINTENT\t%s\nCOMPLETION\t%s\n",
		terminalSafe(report.Operation.ID), terminalSafe(report.Operation.PlanID), terminalSafe(report.Operation.Status),
		terminalSafe(report.Operation.Phase), report.Operation.Resumable, terminalSafe(report.Operation.IntentID), terminalSafe(report.Operation.CompletionID))
	fmt.Fprintf(w, "\nWRITE BREAKDOWN\nJOURNAL OBJECTS\t%d\nJOURNAL BYTES\t%d\nDELETION ATTEMPTS\t%d\nNAMES REMOVED\t%d\nBYTES REMOVED\t%d\nDURABILITY UNCONFIRMED\t%d\nAMBIGUOUS REMOVALS\t%d\n",
		report.Writes.JournalObjectsPublished, report.Writes.JournalBytesWritten, report.Writes.DeletionAttempts,
		report.Writes.NamesRemoved, report.Writes.BytesRemoved, report.Writes.DurabilityUnconfirmed, report.Writes.AmbiguousRemovals)
	fmt.Fprintf(w, "\nLIMITS / USED\nMAX FILES\t%d\nMAX PATH BYTES\t%d\nMAX INTENT BYTES\t%d\nMAX MARKER BYTES\t%d\nMAX CONTENT BYTES\t%d\nFILES\t%d\nPATH BYTES\t%d\nCONTENT BYTES\t%d\nJOURNAL BYTES\t%d\n",
		report.Limits.MaxFiles, report.Limits.MaxPathBytes, report.Limits.MaxIntentBytes, report.Limits.MaxMarkerBytes,
		report.Limits.MaxContentBytes, report.Used.FilesConsidered, report.Used.PathBytes, report.Used.ContentBytes, report.Used.JournalBytes)
	if len(report.Files) > 0 {
		fmt.Fprintln(w, "\nSOURCE NAMES")
		fmt.Fprintln(w, "SEQ\tMANIFEST\tBYTES\tSTATUS\tPATH REF\tBASIS")
		for _, file := range report.Files {
			fmt.Fprintf(w, "%d\t%d\t%d\t%s\t%s\t%s\n", file.Sequence, file.ManifestIndex, file.SizeBytes,
				terminalSafe(file.Status), terminalSafe(file.SourcePathRef), terminalSafe(file.Basis))
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

func sourceRetireExecutionFinding(findings []sourceretire.Finding, code string) bool {
	for _, finding := range findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func sourceRetireFlagError(output string, err error) string {
	if detail := strings.TrimSpace(output); detail != "" {
		return detail
	}
	return err.Error()
}

func validSourceRetireExecutionPlanID(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == 32
}

func addSourceRetireExecutionEffects(report *sourceretire.ExecutionReport, values ...string) {
	if report == nil {
		return
	}
	seen := make(map[string]struct{}, len(report.Effect)+len(values))
	for _, value := range report.Effect {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if _, present := seen[value]; !present {
			report.Effect = append(report.Effect, value)
			seen[value] = struct{}{}
		}
	}
	sort.Strings(report.Effect)
}
