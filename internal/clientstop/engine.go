package clientstop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

type RunOptions struct {
	TargetRoot      string
	Prepared        *PreparedPlan
	ExpectedPlanID  string
	Session         downloader.ExistingJobStopSession
	AcknowledgeStop bool
}

func Preview(prepared *PreparedPlan) (Report, error) {
	report := newReport(prepared)
	if err := prepared.validate(); err != nil {
		report.classify(err)
		return report, err
	}
	report.Outcome = OutcomeReady
	report.Blockers = []string{}
	report.Warnings = append(report.Warnings,
		"the reviewed request stops one exact downloader job; request acceptance, stopped state, and exact final bytes remain separate non-atomic evidence")
	return report, nil
}

func Run(ctx context.Context, options RunOptions) (Report, error) {
	report := newReport(options.Prepared)
	report.Plan.ExpectedID = options.ExpectedPlanID
	report.Plan.Matches = options.Prepared != nil && options.Prepared.planID == options.ExpectedPlanID
	if err := validateRunOptions(options); err != nil {
		report.classify(err)
		return report, err
	}
	plan := options.Prepared.plan
	operationID := OperationIDForPlan(options.Prepared.planID)
	report.Operation = OperationReport{ID: operationID.String(), Status: "initializing", Phase: "planned"}
	handle, creation, err := createJournal(ctx, options.TargetRoot, plan, options.Prepared.planID)
	report.applyCreation(creation)
	if err != nil && journalCreationMayHaveChangedState(creation) {
		report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "initialization_incomplete", "intent_not_durable", true
	}
	if errors.Is(err, fsbind.ErrAlreadyExists) {
		var recovery journalRecoveryReceipt
		handle, recovery, err = openJournal(ctx, options.TargetRoot, operationID, true, &plan)
		report.applyRecovery(recovery)
		if err != nil && journalRecoveryMayHaveChangedState(recovery) {
			report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "recovery_incomplete", "marker_recovery_incomplete", true
		}
		if err == nil && (handle.state.Intent.PlanID != options.Prepared.planID || handle.state.Completion != nil || len(handle.state.Attempts) != 0) {
			advanced := reportFromJournal(handle.state)
			advanced.Plan.ExpectedID = options.ExpectedPlanID
			advanced.Plan.Matches = handle.state.Intent.PlanID == options.ExpectedPlanID
			advanced.applyCreation(creation)
			advanced.applyRecovery(recovery)
			report = advanced
			_ = handle.Close()
			handle = nil
			err = fmt.Errorf("%w: client stop operation already advanced; use resume", ErrPolicy)
		}
	}
	if err != nil {
		report.classify(err)
		return report, err
	}
	defer handle.Close()
	report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "active", "journaled", true

	current, currentObservation, err := options.Prepared.current.Reobserve(ctx, options.Session)
	if err != nil {
		report.classify(err)
		return report, err
	}
	if !current.JobStarted() || currentObservation.UseID != plan.UseID || currentObservation.JobID != plan.JobID ||
		currentObservation.FileLayoutID != plan.FileLayoutID || currentObservation.CompleteFileSnapshotID != plan.CompleteFileSnapshotID ||
		currentObservation.JobState != plan.ReviewedJobState {
		err = fmt.Errorf("%w: current downloader use differs from the reviewed client stop plan", ErrPolicy)
		report.classify(err)
		return report, err
	}
	report.Before = currentObservation
	request, ok := current.MutationRequest()
	if !ok || request.JobKey != options.Prepared.request.JobKey {
		err = fmt.Errorf("%w: current downloader job locator changed", ErrIntegrity)
		report.classify(err)
		return report, err
	}
	return executeAttempt(ctx, handle, current, options.Session, plan, options.Prepared.planID, 1, "", request, &report)
}

func executeAttempt(ctx context.Context, handle *journalHandle, current *clientactivate.VerifiedCurrentUse,
	session downloader.ExistingJobStopSession, plan Plan, planID string, sequence int, previous MarkerID,
	request downloader.ExistingJobMutationRequest, report *Report) (Report, error) {
	operationID := OperationIDForPlan(planID)
	currentObservation := current.Observation()
	attempt := Attempt{Schema: AttemptSchemaV1, OperationID: operationID, PlanID: planID, Sequence: sequence,
		UseID: plan.UseID, JobID: plan.JobID, FileLayoutID: plan.FileLayoutID, FileSnapshotID: plan.CompleteFileSnapshotID,
		ObservedAtStart: parseObservedTime(currentObservation.ObservedAtStart), ObservedAtEnd: parseObservedTime(currentObservation.ObservedAtEnd),
		JobState: currentObservation.JobState, JobProgress: currentObservation.JobProgress}
	if sequence > 1 {
		attempt.PreviousAttemptID = previous.String()
	}
	attemptReceipt, attemptID, err := handle.appendAttempt(ctx, attempt)
	report.applyMarker(attemptReceipt)
	if err != nil {
		report.classify(err)
		return *report, err
	}
	report.Operation.Phase = "request_intent_durable"
	requestsBefore := session.RequestsMade()
	mutationReceipt, mutationErr := session.Stop(ctx, request)
	requestDelta := session.RequestsMade() - requestsBefore
	report.Writes.DownloaderRequests += nonnegative(requestDelta)
	report.Effect = downloader.StopEffect
	if err := validateMutationReceipt(plan, request, mutationReceipt, requestDelta, mutationErr); err != nil {
		report.Mutation.Status = "invalid_receipt"
		if requestDelta == 1 {
			report.Operation.Phase = "request_result_unknown"
		}
		report.classify(err)
		return *report, err
	}
	response := responseFromReceipt(operationID, planID, attemptID, mutationReceipt)
	responseReceipt, responseID, appendErr := handle.appendResponse(ctx, sequence, response)
	report.applyMarker(responseReceipt)
	if appendErr != nil {
		report.Mutation.Status, report.Operation.Phase = "request_result_not_journaled", "request_result_unknown"
		err = fmt.Errorf("%w: client stop response could not be journaled", ErrRequestUnknown)
		report.classify(err)
		return *report, err
	}
	report.Mutation.Receipt = publicReceipt(mutationReceipt)
	if mutationReceipt.Complete {
		report.Mutation.Status, report.Operation.Phase = "accepted", "accepted_response_observed"
	} else if mutationReceipt.RequestsAttempted == 1 {
		report.Mutation.Status, report.Operation.Phase = "request_result_unknown", "request_result_unknown"
	} else {
		report.Mutation.Status = "not_sent"
	}
	if mutationReceipt.RequestsAttempted != 1 {
		if mutationErr == nil {
			mutationErr = fmt.Errorf("client stop request was not attempted")
		}
		report.classify(mutationErr)
		return *report, mutationErr
	}
	if err := ctx.Err(); err != nil {
		if mutationReceipt.Complete && mutationErr == nil {
			report.classify(err)
			return *report, err
		}
		unknown := fmt.Errorf("%w: client stop request was attempted before cancellation", ErrRequestUnknown)
		report.classify(unknown)
		return *report, unknown
	}
	stopped, stoppedObservation, stoppedErr := clientactivate.VerifyCurrentJobStopped(ctx, current, session)
	if stoppedErr != nil {
		report.Stopped.Status = "not_proven"
		if errors.Is(stoppedErr, clientactivate.ErrIntegrity) || errors.Is(stoppedErr, materialize.ErrIntegrity) {
			report.classify(stoppedErr)
			return *report, stoppedErr
		}
		if errors.Is(stoppedErr, clientactivate.ErrPolicy) {
			if mutationReceipt.Complete && mutationErr == nil {
				blocked := fmt.Errorf("%w: accepted stop response was not followed by exact stopped state", ErrPolicy)
				report.classify(blocked)
				return *report, blocked
			}
			unknown := fmt.Errorf("%w: exact downloader stopped state was not proven", ErrRequestUnknown)
			report.classify(unknown)
			return *report, unknown
		}
		if !mutationReceipt.Complete || mutationErr != nil {
			unknown := errors.Join(ErrRequestUnknown, stoppedErr)
			report.classify(unknown)
			return *report, unknown
		}
		report.classify(stoppedErr)
		return *report, stoppedErr
	}
	return completeStopped(ctx, handle, plan, planID, attemptID, responseID, mutationReceipt.Complete && mutationErr == nil,
		stopped, stoppedObservation, report)
}

func completeStopped(ctx context.Context, handle *journalHandle, plan Plan, planID string, attemptID, responseID MarkerID,
	attributed bool, stopped *clientactivate.VerifiedCurrentUse, observation clientactivate.CurrentUseObservation,
	report *Report) (Report, error) {
	if stopped == nil || !stopped.JobStopped() || observation.UseID != plan.UseID || observation.JobID != plan.JobID ||
		observation.FileLayoutID != plan.FileLayoutID || observation.CompleteFileSnapshotID != plan.CompleteFileSnapshotID ||
		observation.Final.FinalObjectIdentity != plan.FinalObjectIdentity {
		err := fmt.Errorf("%w: stopped-state authority differs from the client stop plan", ErrIntegrity)
		report.classify(err)
		return *report, err
	}
	report.Stopped = StoppedReport{Status: "exact_typed_job_stopped", Observation: observation}
	report.Final = observation.Final
	report.Assurance.QueueEvidence = "same_session_exact_typed_job_stopped_with_complete_layout"
	report.Assurance.FilesystemEvidence = "same_invocation_exact_final_reverification_after_stopped_observation"
	basis := "exact_stopped_after_unknown_attempt_causality_unproven"
	if attributed {
		basis = "accepted_response_then_exact_stopped"
	}
	report.Assurance.CompletionBasis = basis
	completion := Completion{Schema: CompletionSchemaV1, OperationID: OperationIDForPlan(planID), PlanID: planID,
		AttemptID: attemptID, ResponseID: responseID.String(), Basis: basis, UseID: plan.UseID, JobID: plan.JobID,
		FileLayoutID: plan.FileLayoutID, CompleteFileSnapshotID: plan.CompleteFileSnapshotID, StoppedJobState: observation.JobState,
		ObservedAtStart: parseObservedTime(observation.ObservedAtStart), ObservedAtEnd: parseObservedTime(observation.ObservedAtEnd),
		FinalObjectIdentity: plan.FinalObjectIdentity, FinalVerificationBasis: "same_invocation_post_stop_exact_final_reverification"}
	completionReceipt, _, err := handle.appendCompletion(ctx, completion)
	report.applyMarker(completionReceipt)
	if err != nil {
		report.Operation.Phase = "stopped_completion_not_durable"
		report.classify(err)
		return *report, err
	}
	report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "complete", "stop_complete", false
	report.Blockers = []string{}
	if attributed {
		report.Outcome = OutcomeStopped
	} else {
		report.Outcome = OutcomeStoppedUnattributed
		report.Warnings = append(report.Warnings, "the job was stopped after an unknown request result; causality is not attributed")
	}
	return *report, nil
}

func validateRunOptions(options RunOptions) error {
	if options.TargetRoot == "" || options.Prepared == nil || options.Session == nil || !options.AcknowledgeStop ||
		!canonicalPlanID(options.ExpectedPlanID) || options.ExpectedPlanID != options.Prepared.planID {
		return fmt.Errorf("%w: client stop run options are invalid or unacknowledged", ErrPolicy)
	}
	if err := options.Prepared.validate(); err != nil {
		return err
	}
	if !options.Prepared.current.SameSession(options.Session) {
		return fmt.Errorf("%w: client stop session differs from current-use authority", ErrPolicy)
	}
	return nil
}

func validateMutationReceipt(plan Plan, request downloader.ExistingJobMutationRequest, receipt downloader.ExistingJobMutationReceipt, requestDelta int, requestErr error) error {
	response := responseFromReceipt(OperationID("sha256:"+strings.Repeat("0", 64)), strings.Repeat("0", 24), MarkerID("sha256:"+strings.Repeat("1", 64)), receipt)
	if requestDelta < 0 || requestDelta > 1 || receipt.RequestsAttempted != requestDelta || response.Validate() != nil ||
		receipt.Complete && requestErr != nil || !receipt.Complete && requestErr == nil {
		return fmt.Errorf("%w: downloader stop receipt is contradictory", ErrIntegrity)
	}
	if receipt.RequestsAttempted == 1 {
		body, err := downloader.MarshalExistingJobStopRequest(plan.Stop, request.JobKey, receipt.RequestID)
		if err != nil || !receipt.RequestBytesKnown || receipt.RequestBytes != int64(len(body)) {
			return fmt.Errorf("%w: downloader stop receipt does not match the reviewed request", ErrIntegrity)
		}
	}
	return nil
}

func responseFromReceipt(operation OperationID, planID string, attempt MarkerID, receipt downloader.ExistingJobMutationReceipt) Response {
	return Response{Schema: ResponseSchemaV1, OperationID: operation, PlanID: planID, AttemptID: attempt, Effect: receipt.Effect,
		ObservedAtStart: receipt.ObservedAtStart, ObservedAtEnd: receipt.ObservedAtEnd, Complete: receipt.Complete,
		RequestsAttempted: receipt.RequestsAttempted, AutomaticRetries: receipt.AutomaticRetries, RedirectsFollowed: receipt.RedirectsFollowed,
		RequestBytes: receipt.RequestBytes, RequestBytesKnown: receipt.RequestBytesKnown, RequestID: receipt.RequestID, StopReason: receipt.StopReason}
}

func journalCreationMayHaveChangedState(receipt journalCreationReceipt) bool {
	return receipt.Subtree.Created || receipt.Mkdir.DirectoriesCreated > 0 || receipt.Intent.TemporaryCreated ||
		receipt.Intent.TemporaryCreationUncertain || receipt.Intent.Publication.Attempted || receipt.Intent.TemporaryRemovalAttempted
}

func journalRecoveryMayHaveChangedState(receipt journalRecoveryReceipt) bool {
	return receipt.Mkdir.DirectoriesCreated > 0 || receipt.Marker.TemporaryCreated || receipt.Marker.TemporaryCreationUncertain ||
		receipt.Marker.Publication.Attempted || receipt.Marker.TemporaryRemovalAttempted
}
