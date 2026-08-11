package clientremove

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

type StatusOptions struct {
	TargetRoot     string
	OperationID    OperationID
	ExpectedPlanID string
}

type ResumeOptions struct {
	TargetRoot         string
	OperationID        OperationID
	ExpectedPlanID     string
	Authority          *clientactivate.CurrentUseAuthority
	Session            downloader.ExistingJobRemovalSession
	AcknowledgeRemoval bool
	AcknowledgeRepeat  bool
}

func Status(ctx context.Context, options StatusOptions) (Report, error) {
	report := selectorReport(options.OperationID)
	report.Plan.ExpectedID = options.ExpectedPlanID
	if err := validateStatusOptions(options); err != nil {
		report.classify(err)
		return report, err
	}
	handle, _, err := openJournal(ctx, options.TargetRoot, options.OperationID, false, nil)
	if err != nil {
		var forgetting *forgetInProgressError
		if errors.As(err, &forgetting) {
			applyForgetControlReport(&report, forgetting)
			return report, nil
		}
		report.classify(err)
		return report, err
	}
	defer handle.Close()
	if handle.state.Retained {
		applyRetainedJournalReport(&report, handle.state)
		if handle.state.RetentionIntentID != "" && handle.state.Intent.PlanID != options.ExpectedPlanID {
			err = fmt.Errorf("%w: client removal selector differs from the retained tombstone", ErrPolicy)
			report.classify(err)
			return report, err
		}
		return report, nil
	}
	report = reportFromJournal(handle.state)
	report.Plan.ExpectedID, report.Plan.Matches = options.ExpectedPlanID, handle.state.Intent.PlanID == options.ExpectedPlanID
	if handle.state.Intent.PlanID != options.ExpectedPlanID {
		err = fmt.Errorf("%w: client removal selector differs from the journal", ErrPolicy)
		report.classify(err)
		return report, err
	}
	report.Outcome = statusOutcome(handle.state)
	return report, nil
}

func Resume(ctx context.Context, options ResumeOptions) (Report, error) {
	report := selectorReport(options.OperationID)
	report.Plan.ExpectedID = options.ExpectedPlanID
	if err := validateResumeOptions(options); err != nil {
		report.classify(err)
		return report, err
	}
	handle, recovery, err := openJournal(ctx, options.TargetRoot, options.OperationID, true, nil)
	report.applyRecovery(recovery)
	if err != nil {
		var forgetting *forgetInProgressError
		if errors.As(err, &forgetting) {
			applyForgetControlReport(&report, forgetting)
			return report, err
		}
		report.classify(err)
		return report, err
	}
	defer handle.Close()
	if handle.state.Retained {
		applyRetainedJournalReport(&report, handle.state)
		err = fmt.Errorf("%w: retained client removal state cannot be resumed", ErrPolicy)
		report.Blockers = append(report.Blockers, "client_removal.prune_boundary")
		report.classify(err)
		return report, err
	}
	report = reportFromJournal(handle.state)
	report.applyRecovery(recovery)
	report.Plan.ExpectedID, report.Plan.Matches = options.ExpectedPlanID, handle.state.Intent.PlanID == options.ExpectedPlanID
	plan := handle.state.Intent.Plan
	if handle.state.Intent.PlanID != options.ExpectedPlanID || !expectationMatchesPlan(options.Authority.Expectation(), plan) {
		err = fmt.Errorf("%w: resume authority or selector differs from the client removal journal", ErrPolicy)
		report.classify(err)
		return report, err
	}

	notBefore := latestAttemptEnd(handle.state)
	absence, absenceObservation, absenceErr := clientactivate.VerifyExpectedJobAbsent(ctx, options.Authority, options.Session, notBefore)
	if absenceErr == nil {
		if handle.state.Completion != nil {
			report.Absence = AbsenceReport{Status: "exact_typed_job_absent", Observation: absenceObservation}
			report.Final = absenceObservation.Final
			report.Assurance.QueueEvidence = "fresh_complete_typed_queue_absence"
			report.Assurance.FilesystemEvidence = "fresh_exact_final_reverification_after_historical_completion"
			report.Outcome, report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = OutcomeAlreadyComplete, "complete", "removal_complete", false
			return report, nil
		}
		if len(handle.state.Attempts) == 0 {
			err = fmt.Errorf("%w: exact job disappeared before any reviewed removal request", ErrPolicy)
			report.Absence = AbsenceReport{Status: "exact_typed_job_absent_before_attempt", Observation: absenceObservation}
			report.classify(err)
			return report, err
		}
		sequence := len(handle.state.Attempts)
		response, hasResponse := handle.state.Responses[sequence]
		responseID := handle.state.ResponseIDs[sequence]
		attributed := hasResponse && response.Complete
		return completeAbsence(ctx, handle, plan, handle.state.Intent.PlanID, handle.state.AttemptIDs[sequence-1], responseID,
			attributed, absence, absenceObservation, &report)
	}
	if ctx.Err() != nil || !errors.Is(absenceErr, clientactivate.ErrPolicy) {
		report.classify(absenceErr)
		return report, absenceErr
	}
	if handle.state.Completion != nil {
		err = fmt.Errorf("%w: a historically removed job is currently present or unobservable", ErrPolicy)
		report.classify(err)
		return report, err
	}

	current, currentObservation, currentErr := clientactivate.VerifyCurrentUse(ctx, options.Authority, options.Session)
	if currentErr != nil {
		report.classify(currentErr)
		return report, currentErr
	}
	descriptor, descriptorErr := options.Session.ReadExistingJobRemovalDescriptor(ctx)
	if descriptorErr != nil {
		report.classify(descriptorErr)
		return report, descriptorErr
	}
	prepared, err := BuildPlan(current, descriptor)
	if err != nil || prepared.planID != handle.state.Intent.PlanID {
		if err == nil {
			err = fmt.Errorf("%w: live current-use state differs from the client removal journal", ErrPolicy)
		}
		report.classify(err)
		return report, err
	}
	report.Before = currentObservation
	sequence := len(handle.state.Attempts) + 1
	if sequence > maximumAttempts {
		err = fmt.Errorf("%w: client removal attempt budget is exhausted", ErrPolicy)
		report.classify(err)
		return report, err
	}
	if !effectfulAttemptPossible(handle.state) {
		if !options.AcknowledgeRemoval {
			err = fmt.Errorf("%w: client removal acknowledgement is required", ErrPolicy)
			report.classify(err)
			return report, err
		}
	} else if !options.AcknowledgeRepeat {
		err = fmt.Errorf("%w: the prior removal result is unknown; an explicit repeat acknowledgement is required", ErrRequestUnknown)
		report.classify(err)
		return report, err
	}
	request, ok := current.RemovalRequest()
	if !ok || request.JobKey != prepared.request.JobKey {
		err = fmt.Errorf("%w: current downloader job locator is unavailable", ErrIntegrity)
		report.classify(err)
		return report, err
	}
	previous := MarkerID("")
	if len(handle.state.AttemptIDs) > 0 {
		previous = handle.state.AttemptIDs[len(handle.state.AttemptIDs)-1]
	}
	return executeAttempt(ctx, handle, current, options.Session, plan, handle.state.Intent.PlanID, sequence, previous, request, &report)
}

func validateStatusOptions(options StatusOptions) error {
	if options.TargetRoot == "" || !canonicalPlanID(options.ExpectedPlanID) || OperationIDForPlan(options.ExpectedPlanID) != options.OperationID {
		return fmt.Errorf("%w: client removal status selector is invalid", ErrPolicy)
	}
	return nil
}

func validateResumeOptions(options ResumeOptions) error {
	if err := validateStatusOptions(StatusOptions{TargetRoot: options.TargetRoot, OperationID: options.OperationID, ExpectedPlanID: options.ExpectedPlanID}); err != nil {
		return err
	}
	if options.Authority == nil || options.Session == nil {
		return fmt.Errorf("%w: client removal resume authority is unavailable", ErrPolicy)
	}
	return nil
}

func selectorReport(operation OperationID) Report {
	report := newReport(nil)
	report.Operation = OperationReport{ID: operation.String(), Status: "inspection_incomplete", Phase: "unknown"}
	return report
}

func reportFromJournal(state journalState) Report {
	report := newReport(nil)
	report.Plan = PlanReport{ID: state.Intent.PlanID, Data: state.Intent.Plan}
	report.Assurance.QueueEvidence = "historical_journal_only_not_currently_observed"
	report.Assurance.FilesystemEvidence = "historical_plan_only_not_currently_reverified"
	report.Operation = OperationReport{ID: state.Intent.OperationID.String(), Status: "active", Phase: "journaled", Resumable: true}
	if len(state.Attempts) > 0 {
		sequence := len(state.Attempts)
		report.Operation.Phase = "request_result_unknown"
		if response, exists := state.Responses[sequence]; exists {
			report.Mutation.Receipt = receiptFromResponse(response)
			if response.Complete {
				report.Mutation.Status, report.Operation.Phase = "accepted", "accepted_response_observed"
			} else if response.RequestsAttempted == 1 {
				report.Mutation.Status = "request_result_unknown"
			} else {
				report.Mutation.Status, report.Operation.Phase = "not_sent", "request_not_sent"
			}
		} else {
			report.Mutation.Status = "request_result_not_journaled"
		}
	}
	if state.Completion != nil {
		report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "complete", "removal_complete", false
		report.Outcome = OutcomeHistoricalComplete
		report.Assurance.CompletionBasis = state.Completion.Basis
		if state.Completion.Basis == "exact_absence_after_unknown_attempt_causality_unproven" {
			report.Warnings = append(report.Warnings, "the historical completion did not attribute job absence to the removal request")
		}
	}
	return report
}

func applyRetainedJournalReport(report *Report, state journalState) {
	if report == nil {
		return
	}
	expected := report.Plan.ExpectedID
	selectedOperation := report.Operation.ID
	report.Effect = "read_private_client_removal_retention_tombstone"
	report.Operation = OperationReport{ID: selectedOperation, Status: "retention_initializing", Phase: "retention_initializing", Resumable: false}
	report.Retention = JournalRetentionReport{State: "initializing"}
	report.Outcome = OutcomeIncomplete
	if state.RetentionIntentID == "" {
		report.Blockers = append(report.Blockers, "client_removal.prune_incomplete")
		return
	}
	plan := state.Intent.Plan
	report.Plan = PlanReport{ID: state.Intent.PlanID, ExpectedID: expected, Matches: expected == "" || expected == state.Intent.PlanID, Data: plan}
	report.Operation.ID = state.Intent.OperationID.String()
	report.Final = historicalRemovalFinal(plan, "historical_retention_tombstone_current_final_not_observed")
	report.Assurance.QueueEvidence = "historical_retention_tombstone_only_not_currently_observed"
	report.Assurance.FilesystemEvidence = "historical_plan_only_not_currently_reverified"
	report.Assurance.CompletionBasis = state.Completion.Basis
	report.Retention = JournalRetentionReport{State: "intent_recorded", IntentMarkerID: state.RetentionIntentID.String(),
		IntentDurable: state.RetentionIntentDurable, HistoricalTerminalEvidence: true}
	report.Operation.Status, report.Operation.Phase = "pruning", "retention_intent_recorded"
	if state.RetentionComplete {
		report.Outcome = OutcomeHistoricalRetained
		report.Operation.Status, report.Operation.Phase = "retained", "retained_removal_completion"
		report.Retention.State = "complete"
		report.Retention.CompletionMarkerID = state.RetentionCompleteID.String()
		report.Retention.CompletionDurable = true
	} else {
		report.Blockers = append(report.Blockers, "client_removal.prune_incomplete")
	}
	report.Warnings = append(report.Warnings,
		"the retained tombstone is historical evidence and does not prove current downloader absence or current materialized bytes")
}

func historicalRemovalFinal(plan Plan, assurance string) materialize.FinalObservation {
	return materialize.FinalObservation{OperationID: plan.MaterializeOperationID, MaterializePlanID: plan.MaterializePlanID,
		MetafileVariantID: plan.MetafileVariantID, InfoHashV1: plan.InfoHashV1, InfoHashV2: plan.InfoHashV2,
		TargetRootIdentity: plan.TargetRootIdentity, FinalObjectIdentity: plan.FinalObjectIdentity, MultiFile: plan.MultiFile,
		ManifestFiles: plan.ManifestFiles, ContentBytes: plan.ContentBytes, AuthorityBasis: "historical_removal_journal", Assurance: assurance}
}

func statusOutcome(state journalState) string {
	if state.Completion != nil {
		return OutcomeHistoricalComplete
	}
	if len(state.Attempts) > 0 {
		if !latestRequestResultUnknown(state) {
			return OutcomeIncomplete
		}
		return OutcomeRequestUnknown
	}
	return OutcomeIncomplete
}

func latestRequestResultUnknown(state journalState) bool {
	sequence := len(state.Attempts)
	if sequence == 0 {
		return false
	}
	response, observed := state.Responses[sequence]
	return !observed || response.RequestsAttempted == 1 && !response.Complete
}

func effectfulAttemptPossible(state journalState) bool {
	for sequence := range state.Attempts {
		response, observed := state.Responses[sequence+1]
		if !observed || response.RequestsAttempted == 1 {
			return true
		}
	}
	return false
}

func latestAttemptEnd(state journalState) time.Time {
	if len(state.Attempts) == 0 {
		return time.Time{}
	}
	return state.Attempts[len(state.Attempts)-1].ObservedAtEnd
}

func expectationMatchesPlan(expectation clientactivate.CurrentUseExpectation, plan Plan) bool {
	final := expectation.Final
	return expectation.Driver == plan.Driver && expectation.UseID == plan.UseID && expectation.ActivationOperationID == plan.ActivationOperationID &&
		expectation.ActivationPlanID == plan.ActivationPlanID && expectation.TerminalMarkerID == plan.ActivationTerminalID &&
		expectation.ClientConfigID == plan.ClientConfigID && expectation.PathMappingID == plan.PathMappingID && expectation.JobID == plan.JobID &&
		expectation.FileLayoutID == plan.FileLayoutID && expectation.FileLimits == plan.FileLimits && final.MetafileVariantID == plan.MetafileVariantID &&
		final.InfoHashV1 == plan.InfoHashV1 && final.InfoHashV2 == plan.InfoHashV2 && final.OperationID == plan.MaterializeOperationID &&
		final.MaterializePlanID == plan.MaterializePlanID && final.TargetRootIdentity == plan.TargetRootIdentity &&
		final.FinalObjectIdentity == plan.FinalObjectIdentity && final.MultiFile == plan.MultiFile && final.ManifestFiles == plan.ManifestFiles &&
		final.ContentBytes == plan.ContentBytes
}

func receiptFromResponse(response Response) downloader.ExistingJobMutationReceipt {
	return downloader.ExistingJobMutationReceipt{
		Effect: response.Effect, ObservedAtStart: response.ObservedAtStart, ObservedAtEnd: response.ObservedAtEnd,
		Complete: response.Complete, RequestsAttempted: response.RequestsAttempted, AutomaticRetries: response.AutomaticRetries,
		RedirectsFollowed: response.RedirectsFollowed, RequestBytes: response.RequestBytes, RequestBytesKnown: response.RequestBytesKnown,
		RequestID: response.RequestID, StopReason: response.StopReason,
	}
}
