package clientremove

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
	TargetRoot         string
	Prepared           *PreparedPlan
	ExpectedPlanID     string
	Session            downloader.ExistingJobRemovalSession
	AcknowledgeRemoval bool
}

func Preview(prepared *PreparedPlan) (Report, error) {
	report := newReport(prepared)
	if err := prepared.validate(); err != nil {
		report.classify(err)
		return report, err
	}
	report.Outcome = OutcomeReady
	report.Operation.Status = "not_created"
	report.Blockers = []string{}
	report.Warnings = append(report.Warnings,
		"the reviewed request removes one downloader job and explicitly keeps local data; queue and filesystem observations remain non-atomic")
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
			report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "initialization_incomplete", "intent_not_durable", true
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
			err = fmt.Errorf("%w: client removal operation already advanced; use resume", ErrPolicy)
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
	if currentObservation.UseID != plan.UseID || currentObservation.JobID != plan.JobID || currentObservation.FileLayoutID != plan.FileLayoutID ||
		currentObservation.CompleteFileSnapshotID != plan.CompleteFileSnapshotID {
		err = fmt.Errorf("%w: current downloader use differs from the reviewed client removal plan", ErrPolicy)
		report.classify(err)
		return report, err
	}
	report.Before = currentObservation
	request, ok := current.RemovalRequest()
	if !ok || request.JobKey != options.Prepared.request.JobKey {
		err = fmt.Errorf("%w: current downloader job locator changed", ErrIntegrity)
		report.classify(err)
		return report, err
	}
	return executeAttempt(ctx, handle, current, options.Session, plan, options.Prepared.planID, 1, "", request, &report)
}

func journalCreationMayHaveChangedState(receipt journalCreationReceipt) bool {
	return receipt.Subtree.Created || receipt.Mkdir.DirectoriesCreated > 0 || receipt.Intent.TemporaryCreated ||
		receipt.Intent.TemporaryCreationUncertain || receipt.Intent.Publication.Attempted || receipt.Intent.TemporaryRemovalAttempted
}

func journalRecoveryMayHaveChangedState(receipt journalRecoveryReceipt) bool {
	return receipt.Mkdir.DirectoriesCreated > 0 || receipt.Marker.TemporaryCreated || receipt.Marker.TemporaryCreationUncertain ||
		receipt.Marker.Publication.Attempted || receipt.Marker.TemporaryRemovalAttempted
}

func executeAttempt(ctx context.Context, handle *journalHandle, current *clientactivate.VerifiedCurrentUse,
	session downloader.ExistingJobRemovalSession, plan Plan, planID string, sequence int, previous MarkerID,
	request downloader.ExistingJobMutationRequest, report *Report) (Report, error) {
	operationID := OperationIDForPlan(planID)
	currentObservation := current.Observation()
	attempt := Attempt{
		Schema: AttemptSchemaV1, OperationID: operationID, PlanID: planID, Sequence: sequence,
		UseID: plan.UseID, JobID: plan.JobID, FileLayoutID: plan.FileLayoutID, FileSnapshotID: plan.CompleteFileSnapshotID,
		ObservedAtStart: parseObservedTime(currentObservation.ObservedAtStart), ObservedAtEnd: parseObservedTime(currentObservation.ObservedAtEnd),
		JobState: currentObservation.JobState, JobProgress: currentObservation.JobProgress, DeleteLocalData: false,
	}
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
	mutationReceipt, mutationErr := session.RemoveKeepData(ctx, request)
	requestDelta := session.RequestsMade() - requestsBefore
	report.Writes.DownloaderRequests += nonnegative(requestDelta)
	report.Effect = downloader.RemoveEffectKeepData
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
		report.Mutation.Status = "request_result_not_journaled"
		report.Operation.Phase = "request_result_unknown"
		err = fmt.Errorf("%w: client removal response could not be journaled", ErrRequestUnknown)
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
			mutationErr = fmt.Errorf("client removal request was not attempted")
		}
		report.classify(mutationErr)
		return *report, mutationErr
	}
	if err := ctx.Err(); err != nil {
		if mutationReceipt.Complete && mutationErr == nil {
			report.classify(err)
			return *report, err
		}
		unknown := fmt.Errorf("%w: client removal request was attempted before cancellation", ErrRequestUnknown)
		report.classify(unknown)
		return *report, unknown
	}
	absence, absenceObservation, absenceErr := clientactivate.VerifyCurrentJobAbsent(ctx, current, session)
	if absenceErr != nil {
		report.Absence.Status = "not_proven"
		if errors.Is(absenceErr, clientactivate.ErrIntegrity) || errors.Is(absenceErr, materialize.ErrIntegrity) {
			report.classify(absenceErr)
			return *report, absenceErr
		}
		if errors.Is(absenceErr, clientactivate.ErrPolicy) {
			if mutationReceipt.Complete && mutationErr == nil {
				blocked := fmt.Errorf("%w: the accepted removal response was not followed by exact job absence", ErrPolicy)
				report.classify(blocked)
				return *report, blocked
			}
			unknown := fmt.Errorf("%w: exact downloader job absence was not proven", ErrRequestUnknown)
			report.classify(unknown)
			return *report, unknown
		}
		if !mutationReceipt.Complete || mutationErr != nil {
			unknown := errors.Join(ErrRequestUnknown, absenceErr)
			report.classify(unknown)
			return *report, unknown
		}
		report.classify(absenceErr)
		return *report, absenceErr
	}
	return completeAbsence(ctx, handle, plan, planID, attemptID, responseID, mutationReceipt.Complete && mutationErr == nil,
		absence, absenceObservation, report)
}

func completeAbsence(ctx context.Context, handle *journalHandle, plan Plan, planID string, attemptID, responseID MarkerID,
	attributed bool, absence *clientactivate.VerifiedCurrentAbsence, observation clientactivate.CurrentAbsenceObservation,
	report *Report) (Report, error) {
	if absence == nil || !absence.Verified() || !absence.Matches(plan.UseID, plan.JobID, plan.MetafileVariantID,
		plan.MaterializeOperationID, plan.MaterializePlanID, plan.FinalObjectIdentity) {
		err := fmt.Errorf("%w: downloader absence authority differs from the client removal plan", ErrIntegrity)
		report.classify(err)
		return *report, err
	}
	report.Absence = AbsenceReport{Status: "exact_typed_job_absent", Observation: observation}
	report.Final = observation.Final
	report.Assurance.QueueEvidence = "complete_typed_queue_absence"
	report.Assurance.FilesystemEvidence = "same_invocation_exact_final_reverification_after_job_absence"
	basis := "exact_absence_after_unknown_attempt_causality_unproven"
	if attributed {
		basis = "accepted_response_then_exact_absence"
	}
	report.Assurance.CompletionBasis = basis
	completion := Completion{
		Schema: CompletionSchemaV1, OperationID: OperationIDForPlan(planID), PlanID: planID, AttemptID: attemptID,
		ResponseID: responseID.String(), Basis: basis, UseID: plan.UseID, JobID: plan.JobID,
		ObservedAtStart: parseObservedTime(observation.ObservedAtStart), ObservedAtEnd: parseObservedTime(observation.ObservedAtEnd),
		JobsExamined: observation.JobsExamined, FinalObjectIdentity: plan.FinalObjectIdentity,
		FinalVerificationBasis: "same_invocation_post_removal_exact_final_reverification",
	}
	completionReceipt, _, err := handle.appendCompletion(ctx, completion)
	report.applyMarker(completionReceipt)
	if err != nil {
		report.Operation.Phase = "job_absent_completion_not_durable"
		report.classify(err)
		return *report, err
	}
	report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "complete", "removal_complete", false
	report.Blockers = []string{}
	if attributed {
		report.Outcome = OutcomeRemovedKeepData
	} else {
		report.Outcome = OutcomeRemovedUnattributed
		report.Warnings = append(report.Warnings, "the job was absent after an unknown request result; causality is not attributed")
	}
	return *report, nil
}

func validateRunOptions(options RunOptions) error {
	if options.TargetRoot == "" || options.Prepared == nil || options.Session == nil || !options.AcknowledgeRemoval ||
		!canonicalPlanID(options.ExpectedPlanID) || options.ExpectedPlanID != options.Prepared.planID {
		return fmt.Errorf("%w: client removal run options are invalid or unacknowledged", ErrPolicy)
	}
	if err := options.Prepared.validate(); err != nil {
		return err
	}
	if !options.Prepared.current.SameSession(options.Session) {
		return fmt.Errorf("%w: client removal session differs from current-use authority", ErrPolicy)
	}
	return nil
}

func validateMutationReceipt(plan Plan, request downloader.ExistingJobMutationRequest, receipt downloader.ExistingJobMutationReceipt, requestDelta int, requestErr error) error {
	response := responseFromReceipt(OperationID("sha256:"+strings.Repeat("0", 64)), strings.Repeat("0", 24), MarkerID("sha256:"+strings.Repeat("1", 64)), receipt)
	if requestDelta < 0 || requestDelta > 1 || receipt.RequestsAttempted != requestDelta || response.Validate() != nil ||
		receipt.Complete && requestErr != nil || !receipt.Complete && requestErr == nil {
		return fmt.Errorf("%w: downloader removal receipt is contradictory", ErrIntegrity)
	}
	if receipt.RequestsAttempted == 1 {
		body, err := downloader.MarshalExistingJobRemovalRequest(plan.Removal, request.JobKey, receipt.RequestID)
		if err != nil || !receipt.RequestBytesKnown || receipt.RequestBytes != int64(len(body)) {
			return fmt.Errorf("%w: downloader removal receipt does not match the reviewed request", ErrIntegrity)
		}
	}
	return nil
}

func responseFromReceipt(operation OperationID, planID string, attempt MarkerID, receipt downloader.ExistingJobMutationReceipt) Response {
	return Response{
		Schema: ResponseSchemaV1, OperationID: operation, PlanID: planID, AttemptID: attempt, Effect: receipt.Effect,
		ObservedAtStart: receipt.ObservedAtStart, ObservedAtEnd: receipt.ObservedAtEnd, Complete: receipt.Complete,
		RequestsAttempted: receipt.RequestsAttempted, AutomaticRetries: receipt.AutomaticRetries, RedirectsFollowed: receipt.RedirectsFollowed,
		RequestBytes: receipt.RequestBytes, RequestBytesKnown: receipt.RequestBytesKnown, RequestID: receipt.RequestID, StopReason: receipt.StopReason,
	}
}

func nonnegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}
