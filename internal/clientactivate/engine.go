package clientactivate

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

type RunOptions struct {
	Authority          *PreparedAuthority
	ExpectedPlanID     string
	Session            downloader.ExistingJobMutationSession
	StartAfterRecheck  bool
	AcknowledgeRecheck bool
	RepeatRecheck      bool
	AcknowledgeStart   bool
	RepeatStart        bool
}

type StatusOptions struct {
	TargetRoot  string
	OperationID OperationID
}

// PreflightRun performs every target-root and deterministic-operation check
// that can be completed before reading downloader credentials. The reviewed
// activation plan itself still depends on a fresh bounded client observation.
func PreflightRun(ctx context.Context, authority *PreparedAuthority, expectedPlanID string) error {
	if authority == nil || authority.verifiedFinal == nil || !canonicalPlanID(expectedPlanID) {
		return fmt.Errorf("%w: activation run selector is invalid", ErrPolicy)
	}
	fresh, observation, err := authority.verifiedFinal.Reverify(ctx)
	if err != nil {
		return err
	}
	if fresh == nil || !fresh.Verified() || observation.TargetRootIdentity != authority.final.TargetRootIdentity ||
		observation.FinalObjectIdentity != authority.final.FinalObjectIdentity {
		return fmt.Errorf("%w: current final differs before activation", ErrIntegrity)
	}
	authority.verifiedFinal, authority.final = fresh, observation
	targetRoot, ok := fresh.ProcessTargetRoot()
	if !ok {
		return fmt.Errorf("%w: target-root authority is unavailable", ErrPolicy)
	}
	handle, _, err := openJournal(ctx, targetRoot, OperationIDForPlan(expectedPlanID), false, nil)
	if errors.Is(err, ErrOperationNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = handle.Close()
	return fmt.Errorf("%w: deterministic activation operation already exists; use resume", ErrPolicy)
}

// PreflightResume reads and validates the selected activation journal without
// credentials or downloader traffic. It does not refresh journal durability.
func PreflightResume(ctx context.Context, authority *PreparedAuthority, targetRoot string, operationID OperationID, expectedPlanID string) (Report, error) {
	report := newReport(authority, expectedPlanID)
	report.Effect = []string{"read_private_client_activation_journal", "read_exact_materialized_final"}
	report.Operation = OperationReport{ID: operationID.String(), Status: "inspection_incomplete", PhaseBefore: "unknown", PhaseAfter: "unknown"}
	report.Journal.Status = "inspection_incomplete"
	if authority == nil || targetRoot == "" || !canonicalPlanID(expectedPlanID) || OperationIDForPlan(expectedPlanID) != operationID {
		err := fmt.Errorf("%w: activation resume selector is invalid", ErrPolicy)
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	handle, _, err := openJournal(ctx, targetRoot, operationID, false, nil)
	if err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	defer handle.Close()
	reportFromState(&report, handle.state)
	if handle.state.Retained && !handle.state.RetentionIntentDurable {
		applyRetainedJournalReport(&report, handle.state)
		err = fmt.Errorf("%w: client activation retention initialization requires explicit prune", ErrPolicy)
		report.finalize()
		return report, err
	}
	if handle.state.Intent.PlanID != expectedPlanID || validateAuthorityAgainstPlan(authority, handle.state.Intent.Plan) != nil {
		err = fmt.Errorf("%w: activation journal differs from the current reviewed authority", ErrPolicy)
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if handle.state.Retained {
		applyRetainedJournalReport(&report, handle.state)
		err = fmt.Errorf("%w: retained client activation state cannot be resumed", ErrPolicy)
		report.finalize()
		return report, err
	}
	report.Outcome = OutcomeReady
	report.finalize()
	return report, nil
}

// FailureReport produces a bounded public report for failures that happen
// before a live activation plan can be built (for example login failure).
func FailureReport(authority *PreparedAuthority, expectedPlanID, action string, requestsMade int, err error) Report {
	report := newReport(authority, expectedPlanID)
	report.Client.RequestsMade = requestsMade
	switch action {
	case "run":
		report.Effect = append(report.Effect, "write_private_client_activation_journal", downloader.ControlEffectRecheck)
	case "resume":
		report.Effect = append(report.Effect, "read_private_client_activation_journal", "write_private_client_activation_journal")
	}
	classifyFailure(&report, err)
	report.finalize()
	return report
}

func Preview(ctx context.Context, authority *PreparedAuthority, session downloader.ExistingJobMutationSession, startAfterRecheck bool) (Report, error) {
	report := newReport(authority, "")
	if authority == nil || session == nil {
		err := fmt.Errorf("%w: activation preview authority is unavailable", ErrPolicy)
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	descriptor, err := readControlDescriptor(ctx, session)
	if err != nil {
		report.Client.RequestsMade = session.RequestsMade()
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	observed, err := observeClientBounded(ctx, authority, session)
	report.Client.RequestsMade = session.RequestsMade()
	report.recordClientObservationUsage(observed)
	if err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	report.observeClient(observed)
	prepared, err := BuildPlan(authority, descriptor, observed, startAfterRecheck)
	if err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	report.applyPrepared(prepared)
	freshFinal, err := prepared.ReverifyFinal(ctx)
	if err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	report.Outcome = OutcomeReady
	report.Operation.Status = "not_created"
	report.Operation.PhaseBefore, report.Operation.PhaseAfter = "planned", "planned"
	report.Operation.Resumable = false
	report.Final.Status, report.Final.Observation, report.Final.PostAction = "verified_current", freshFinal, &freshFinal
	report.finalize()
	return report, nil
}

func Run(ctx context.Context, options RunOptions) (Report, error) {
	report := newReport(options.Authority, options.ExpectedPlanID)
	report.Effect = append(report.Effect, "write_private_client_activation_journal", downloader.ControlEffectRecheck)
	if !canonicalPlanID(options.ExpectedPlanID) || options.Authority == nil || options.Session == nil || !options.AcknowledgeRecheck ||
		options.RepeatRecheck || options.RepeatStart || options.AcknowledgeStart {
		err := fmt.Errorf("%w: activation run acknowledgement or authority is invalid", ErrPolicy)
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	descriptor, err := readControlDescriptor(ctx, options.Session)
	if err != nil {
		report.Client.RequestsMade = options.Session.RequestsMade()
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	observed, err := observeClientBounded(ctx, options.Authority, options.Session)
	report.Client.RequestsMade = options.Session.RequestsMade()
	report.recordClientObservationUsage(observed)
	if err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	report.observeClient(observed)
	prepared, err := BuildPlan(options.Authority, descriptor, observed, options.StartAfterRecheck)
	if err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	report.applyPrepared(prepared)
	if prepared.planID != options.ExpectedPlanID {
		err = fmt.Errorf("%w: reviewed activation plan ID differs", ErrPolicy)
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if _, err = prepared.ReverifyFinal(ctx); err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	targetRoot, ok := prepared.authority.verifiedFinal.ProcessTargetRoot()
	if !ok {
		err = fmt.Errorf("%w: target-root authority is unavailable", ErrPolicy)
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	handle, creation, err := createJournal(ctx, targetRoot, prepared.plan, prepared.planID)
	report.recordCreation(creation)
	if err != nil {
		if creation.Subtree.Created || creation.Mkdir.DirectoriesCreated > 0 || creation.Intent.TemporaryCreated || creation.Intent.Publication.Attempted {
			report.Operation.Status = "initialization_incomplete"
			report.Operation.PhaseBefore, report.Operation.PhaseAfter = "planned", "initialization_incomplete"
			report.Operation.Resumable = true
			report.Journal.Status = "initialization_incomplete"
		}
		if errors.Is(err, fsbind.ErrAlreadyExists) {
			report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "existing_uninspected", "unknown", false
			err = fmt.Errorf("%w: activation operation already exists; use resume", ErrPolicy)
		}
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	defer handle.Close()
	report.Operation = OperationReport{ID: prepared.operationID.String(), Status: "active", PhaseBefore: "planned", PhaseAfter: "intent_recorded", Resumable: true}
	report.Journal = journalReport(handle.state)
	err = continueOperation(ctx, prepared, handle, options.Session, options, observed, &report)
	report.Client.RequestsMade = options.Session.RequestsMade()
	report.Journal = journalReport(handle.state)
	report.Operation.PhaseAfter = phaseForState(handle.state)
	if err != nil {
		classifyFailure(&report, err)
	}
	report.finalize()
	return report, err
}

func Resume(ctx context.Context, operationID OperationID, options RunOptions) (Report, error) {
	report := newReport(options.Authority, options.ExpectedPlanID)
	report.Effect = append(report.Effect, "write_private_client_activation_journal", "observe_or_resume_client_activation")
	report.Operation = OperationReport{ID: operationID.String(), Status: "inspection_incomplete", PhaseBefore: "unknown", PhaseAfter: "unknown"}
	report.Journal.Status = "inspection_incomplete"
	if _, err := ParseOperationID(operationID.String()); err != nil || !canonicalPlanID(options.ExpectedPlanID) || options.Authority == nil || options.Session == nil ||
		options.RepeatRecheck && !options.AcknowledgeRecheck || options.RepeatStart && !options.AcknowledgeStart {
		err := fmt.Errorf("%w: activation resume selector or acknowledgement is invalid", ErrPolicy)
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	targetRoot, ok := options.Authority.verifiedFinal.ProcessTargetRoot()
	if !ok {
		err := fmt.Errorf("%w: target-root authority is unavailable", ErrPolicy)
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	// Inspect the operation before any downloader request. Initialization
	// recovery is deferred until the reviewed plan can be reconstructed.
	handle, recovery, openErr := openJournal(ctx, targetRoot, operationID, false, nil)
	initializationIncomplete := errors.Is(openErr, ErrInitializationIncomplete)
	if openErr != nil && !initializationIncomplete {
		classifyFailure(&report, openErr)
		report.finalize()
		return report, openErr
	}
	if handle != nil {
		defer func() {
			if handle != nil {
				_ = handle.Close()
			}
		}()
		reportFromState(&report, handle.state)
		if handle.state.Retained && !handle.state.RetentionIntentDurable {
			applyRetainedJournalReport(&report, handle.state)
			err := fmt.Errorf("%w: client activation retention initialization requires explicit prune", ErrPolicy)
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
		if handle.state.Intent.PlanID != options.ExpectedPlanID || handle.state.Intent.OperationID != operationID ||
			validateAuthorityAgainstPlan(options.Authority, handle.state.Intent.Plan) != nil {
			err := fmt.Errorf("%w: activation operation belongs to a different reviewed authority", ErrPolicy)
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
		if handle.state.Retained {
			applyRetainedJournalReport(&report, handle.state)
			err := fmt.Errorf("%w: retained client activation state cannot be resumed", ErrPolicy)
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
	}
	descriptor, err := readControlDescriptor(ctx, options.Session)
	if err != nil {
		report.Client.RequestsMade = options.Session.RequestsMade()
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	observed, err := observeClientBounded(ctx, options.Authority, options.Session)
	report.Client.RequestsMade = options.Session.RequestsMade()
	report.recordClientObservationUsage(observed)
	if err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	report.observeClient(observed)
	var prepared *PreparedPlan
	if initializationIncomplete {
		prepared, err = BuildPlan(options.Authority, descriptor, observed, options.StartAfterRecheck)
		if err == nil && (prepared.planID != options.ExpectedPlanID || prepared.operationID != operationID) {
			err = fmt.Errorf("%w: incomplete operation differs from the reviewed activation plan", ErrPolicy)
		}
		if err != nil {
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
		handle, recovery, err = openJournal(ctx, targetRoot, operationID, true, &prepared.plan)
		if err != nil {
			report.recordRecovery(recovery)
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
		defer func() {
			if handle != nil {
				_ = handle.Close()
			}
		}()
	} else {
		plan := handle.state.Intent.Plan
		if descriptor != plan.Control || observed.jobID != plan.JobID || observed.fileLayoutID != plan.ExpectedFileLayoutID {
			err = fmt.Errorf("%w: current client or control protocol differs from the activation plan", ErrPolicy)
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
		prepared = &PreparedPlan{plan: plan, planID: handle.state.Intent.PlanID, operationID: operationID, authority: options.Authority, jobKey: observed.job.Hash, before: observed}
		_ = handle.Close()
		handle, recovery, err = openJournal(ctx, targetRoot, operationID, true, &plan)
		if err != nil {
			report.recordRecovery(recovery)
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
	}
	report.recordRecovery(recovery)
	report.applyPrepared(prepared)
	reportFromState(&report, handle.state)
	if _, err = prepared.ReverifyFinal(ctx); err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	err = continueOperation(ctx, prepared, handle, options.Session, options, observed, &report)
	report.Client.RequestsMade = options.Session.RequestsMade()
	report.Journal = journalReport(handle.state)
	report.Operation.PhaseAfter = phaseForState(handle.state)
	if err != nil {
		classifyFailure(&report, err)
	}
	report.finalize()
	return report, err
}

func continueOperation(ctx context.Context, prepared *PreparedPlan, handle *journalHandle, session downloader.ExistingJobMutationSession, options RunOptions, observed clientObservation, report *Report) error {
	state := handle.state
	switch {
	case state.ActivationCompletion != nil && observed.ledger.ObservedAtStart.Before(state.ActivationCompletion.ObservedAtEnd):
		return fmt.Errorf("%w: current client observation predates activation completion", ErrIntegrity)
	case len(state.StartAttempts) > 0 && observed.ledger.ObservedAtStart.Before(state.StartAttempts[len(state.StartAttempts)-1].ObservedAtEnd):
		return fmt.Errorf("%w: current client observation predates the latest start attempt", ErrIntegrity)
	case state.RecheckCompletion != nil && observed.ledger.ObservedAtStart.Before(state.RecheckCompletion.ObservedAtEnd):
		return fmt.Errorf("%w: current client observation predates recheck completion", ErrIntegrity)
	case state.RecheckStarted != nil && observed.ledger.ObservedAtStart.Before(state.RecheckStarted.ObservedAtEnd):
		return fmt.Errorf("%w: current client observation predates the checking observation", ErrIntegrity)
	case len(state.RecheckAttempts) > 0 && observed.ledger.ObservedAtStart.Before(state.RecheckAttempts[len(state.RecheckAttempts)-1].ObservedAtEnd):
		return fmt.Errorf("%w: current client observation predates the latest recheck attempt", ErrIntegrity)
	}
	if state.ActivationCompletion != nil {
		if !startedState(observed.job.State) || observed.job.Progress != 1 || !observed.allSelected || !observed.allComplete {
			return fmt.Errorf("%w: historical activation completion differs from current client state", ErrPolicy)
		}
		final, err := prepared.ReverifyFinal(ctx)
		if err != nil {
			return err
		}
		report.Final.Status, report.Final.PostAction = "verified_current", &final
		report.Outcome = OutcomeStartedClientClaim
		report.Operation.Status, report.Operation.Resumable = "terminal", false
		return nil
	}
	if state.RecheckCompletion != nil {
		if state.Intent.Plan.Action == ActionRecheckOnly {
			if !completeStoppedState(observed.job.State) || observed.job.Progress != 1 || !observed.allSelected || !observed.allComplete {
				return fmt.Errorf("%w: historical recheck completion differs from current client state", ErrPolicy)
			}
			report.Outcome = OutcomeAlreadyCheckedStopped
			report.Operation.Status, report.Operation.Resumable = "terminal", false
			return nil
		}
		if len(state.StartAttemptIDs) > 0 && startedState(observed.job.State) {
			return completeActivation(ctx, prepared, handle, observed, report)
		}
		if len(state.StartAttempts) > 0 && !(options.AcknowledgeStart && options.RepeatStart) {
			report.Outcome = OutcomeStartRequestUnknown
			report.Operation.Status, report.Operation.Resumable = "active", true
			return ErrRequestUnknown
		}
		if len(state.StartAttempts) > 0 && (!completeStoppedState(observed.job.State) || observed.job.Progress != 1 || !observed.allSelected || !observed.allComplete) {
			report.Outcome = OutcomeStartRequestUnknown
			report.Operation.Status, report.Operation.Resumable = "active", true
			return ErrRequestUnknown
		}
		if len(state.StartAttemptIDs) == 0 && startedState(observed.job.State) {
			return fmt.Errorf("%w: client started without a journaled start request", ErrPolicy)
		}
		if !completeStoppedState(observed.job.State) || observed.job.Progress != 1 || !observed.allSelected || !observed.allComplete {
			return fmt.Errorf("%w: current client is not complete and stopped after recheck", ErrPolicy)
		}
		if len(state.StartAttempts) == 0 && !options.AcknowledgeStart {
			report.Outcome = OutcomeCheckedStopped
			report.Operation.Status, report.Operation.Resumable = "active", true
			return nil
		}
		return performStart(ctx, prepared, handle, session, options, observed, report)
	}
	if state.RecheckStarted != nil {
		if checkingState(observed.job.State) {
			report.Outcome = OutcomeRecheckInProgress
			report.Operation.Status, report.Operation.Resumable = "active", true
			return nil
		}
		if completeStoppedState(observed.job.State) && observed.job.Progress == 1 && observed.allSelected && observed.allComplete {
			if err := completeRecheck(ctx, prepared, handle, observed, "durable_checking_observation_then_complete", report); err != nil {
				return err
			}
			if handle.state.Intent.Plan.Action == ActionRecheckThenStart && options.AcknowledgeStart {
				return performStart(ctx, prepared, handle, session, options, observed, report)
			}
			return nil
		}
		return fmt.Errorf("%w: observed recheck did not complete with all content", ErrPolicy)
	}
	if len(state.RecheckAttempts) > 0 {
		if checkingState(observed.job.State) {
			return recordRecheckStarted(ctx, prepared, handle, observed, report)
		}
		if !(options.AcknowledgeRecheck && options.RepeatRecheck) {
			report.Outcome = OutcomeRecheckRequestUnknown
			report.Operation.Status, report.Operation.Resumable = "active", true
			return ErrRequestUnknown
		}
	}
	if !options.AcknowledgeRecheck {
		return fmt.Errorf("%w: explicit recheck acknowledgement is required", ErrPolicy)
	}
	return performRecheck(ctx, prepared, handle, session, observed, report)
}

func performRecheck(ctx context.Context, prepared *PreparedPlan, handle *journalHandle, session downloader.ExistingJobMutationSession, before clientObservation, report *Report) error {
	if !stoppedState(before.job.State) || !before.allSelected || before.fileLayoutID != prepared.plan.ExpectedFileLayoutID {
		return fmt.Errorf("%w: exact job is not safely stopped for recheck", ErrPolicy)
	}
	if _, err := prepared.ReverifyFinal(ctx); err != nil {
		return err
	}
	receipt, _, err := handle.appendAttempt(ctx, AttemptActionRecheck, before)
	report.recordMarker(receipt)
	if err != nil {
		return err
	}
	report.Journal = journalReport(handle.state)
	return executeAction(ctx, prepared, handle, session, AttemptActionRecheck, before, report)
}

func performStart(ctx context.Context, prepared *PreparedPlan, handle *journalHandle, session downloader.ExistingJobMutationSession, options RunOptions, before clientObservation, report *Report) error {
	if !options.AcknowledgeStart || !completeStoppedState(before.job.State) || before.job.Progress != 1 || !before.allSelected || !before.allComplete {
		return fmt.Errorf("%w: exact checked job is not safely stopped for start", ErrPolicy)
	}
	if _, err := prepared.ReverifyFinal(ctx); err != nil {
		return err
	}
	receipt, _, err := handle.appendAttempt(ctx, AttemptActionStart, before)
	report.recordMarker(receipt)
	if err != nil {
		return err
	}
	report.Journal = journalReport(handle.state)
	return executeAction(ctx, prepared, handle, session, AttemptActionStart, before, report)
}

func executeAction(ctx context.Context, prepared *PreparedPlan, handle *journalHandle, session downloader.ExistingJobMutationSession, action string, before clientObservation, report *Report) error {
	if session == nil {
		return fmt.Errorf("%w: downloader mutation session is unavailable", ErrPolicy)
	}
	requestCountBefore := session.RequestsMade()
	var receipt downloader.ExistingJobMutationReceipt
	var mutationErr error
	request := downloader.ExistingJobMutationRequest{JobKey: before.job.Hash}
	if action == AttemptActionStart {
		receipt, mutationErr = session.Start(ctx, request)
	} else {
		receipt, mutationErr = session.Recheck(ctx, request)
	}
	report.Client.ActionReceipt = safeActionReceipt(receipt, action, before.job.Hash)
	requestDelta := session.RequestsMade() - requestCountBefore
	if requestDelta > 0 {
		report.Client.ActionAttempted = action
		if action == AttemptActionStart {
			report.addEffect(downloader.ControlEffectStart)
		} else {
			report.addEffect(downloader.ControlEffectRecheck)
		}
	}
	if requestDelta < 0 || requestDelta > 1 || requestDelta == 0 && mutationErr == nil {
		return fmt.Errorf("%w: downloader action request count is contradictory", ErrIntegrity)
	}
	if requestDelta == 0 {
		report.addIssue("client.action_not_observed", "the session recorded no effectful request; explicit repeat acknowledgement is required")
		if action == AttemptActionStart {
			report.Outcome = OutcomeStartRequestUnknown
		} else {
			report.Outcome = OutcomeRecheckRequestUnknown
		}
		report.Operation.Status, report.Operation.Resumable = "active", true
		return errors.Join(ErrRequestUnknown, mutationErr)
	}
	if mutationErr == nil && !validActionReceipt(receipt, action, before.job.Hash) {
		return fmt.Errorf("%w: downloader action receipt is contradictory", ErrIntegrity)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrRequestUnknown, err)
	}
	after, observeErr := observeClientBounded(ctx, prepared.authority, session)
	report.recordClientObservationUsage(after)
	if observeErr != nil {
		report.Client.Status = "observation_incomplete"
		report.Client.Assurance = "client_state_not_established"
		return errors.Join(ErrRequestUnknown, mutationErr, observeErr)
	}
	report.observeClient(after)
	if after.ledger.ObservedAtStart.Before(before.ledger.ObservedAtEnd) {
		return fmt.Errorf("%w: downloader observations are not ordered across the action", ErrIntegrity)
	}
	if after.jobID != prepared.plan.JobID || after.fileLayoutID != prepared.plan.ExpectedFileLayoutID {
		return fmt.Errorf("%w: downloader job identity or layout changed across the action", ErrIntegrity)
	}
	if action == AttemptActionRecheck {
		if checkingState(after.job.State) {
			if mutationErr != nil {
				report.addIssue("client.recheck_response_unconfirmed", "checking was observed after an unconfirmed recheck response")
			}
			return recordRecheckStarted(ctx, prepared, handle, after, report)
		}
		if completeStoppedState(after.job.State) && after.job.Progress == 1 && after.allSelected && after.allComplete &&
			(before.job.Progress < 1 || !completeStoppedState(before.job.State)) {
			if mutationErr != nil {
				report.addIssue("client.recheck_response_unconfirmed", "completion was observed after an unconfirmed recheck response")
			}
			return completeRecheck(ctx, prepared, handle, after, "same_invocation_incomplete_to_complete_transition", report)
		}
		report.Outcome = OutcomeRecheckRequestUnknown
		report.Operation.Status, report.Operation.Resumable = "active", true
		return errors.Join(ErrRequestUnknown, mutationErr)
	}
	if startedState(after.job.State) && after.job.Progress == 1 && after.allSelected && after.allComplete {
		if mutationErr != nil {
			report.addIssue("client.start_response_unconfirmed", "started state was observed after an unconfirmed start response")
		}
		return completeActivation(ctx, prepared, handle, after, report)
	}
	report.Outcome = OutcomeStartRequestUnknown
	report.Operation.Status, report.Operation.Resumable = "active", true
	return errors.Join(ErrRequestUnknown, mutationErr)
}

func recordRecheckStarted(ctx context.Context, prepared *PreparedPlan, handle *journalHandle, observed clientObservation, report *Report) error {
	if len(handle.state.RecheckAttempts) == 0 || observed.ledger.ObservedAtStart.Before(handle.state.RecheckAttempts[len(handle.state.RecheckAttempts)-1].ObservedAtEnd) {
		return fmt.Errorf("%w: checking observation predates its recheck attempt", ErrIntegrity)
	}
	final, err := prepared.ReverifyFinal(ctx)
	if err != nil {
		return err
	}
	last := handle.state.RecheckAttemptIDs[len(handle.state.RecheckAttemptIDs)-1]
	value := RecheckStarted{Schema: RecheckStartedSchemaV1, OperationID: handle.state.Intent.OperationID, PlanID: handle.state.Intent.PlanID,
		AttemptID: last, ObservedAtStart: observed.ledger.ObservedAtStart, ObservedAtEnd: observed.ledger.ObservedAtEnd,
		JobID: observed.jobID, JobState: observed.job.State, FileLayoutID: observed.fileLayoutID, FinalObjectIdentity: final.FinalObjectIdentity}
	receipt, _, err := handle.appendRecheckStarted(ctx, value)
	report.recordMarker(receipt)
	if err != nil {
		return err
	}
	report.Final.Status, report.Final.PostAction = "verified_current", &final
	report.Outcome = OutcomeRecheckInProgress
	report.Operation.Status, report.Operation.Resumable = "active", true
	return nil
}

func completeRecheck(ctx context.Context, prepared *PreparedPlan, handle *journalHandle, observed clientObservation, basis string, report *Report) error {
	if len(handle.state.RecheckAttempts) == 0 {
		return fmt.Errorf("%w: recheck completion has no attempt", ErrIntegrity)
	}
	previousEnd := handle.state.RecheckAttempts[len(handle.state.RecheckAttempts)-1].ObservedAtEnd
	if basis == "durable_checking_observation_then_complete" {
		if handle.state.RecheckStarted == nil {
			return fmt.Errorf("%w: recheck completion has no checking observation", ErrIntegrity)
		}
		previousEnd = handle.state.RecheckStarted.ObservedAtEnd
	}
	if observed.ledger.ObservedAtStart.Before(previousEnd) {
		return fmt.Errorf("%w: recheck completion observation is not ordered", ErrIntegrity)
	}
	final, err := prepared.ReverifyFinal(ctx)
	if err != nil {
		return err
	}
	last := handle.state.RecheckAttemptIDs[len(handle.state.RecheckAttemptIDs)-1]
	startedID := ""
	if basis == "durable_checking_observation_then_complete" {
		startedID = handle.state.RecheckStartedID.String()
	}
	value := RecheckCompletion{Schema: RecheckCompleteSchemaV1, OperationID: handle.state.Intent.OperationID, PlanID: handle.state.Intent.PlanID,
		AttemptID: last, StartedID: startedID, Basis: basis, ObservedAtStart: observed.ledger.ObservedAtStart, ObservedAtEnd: observed.ledger.ObservedAtEnd,
		JobID: observed.jobID, JobState: observed.job.State, JobProgress: observed.job.Progress, FileLayoutID: observed.fileLayoutID,
		CompleteFileSnapshotID: observed.completeSnapshotID, FinalObjectIdentity: final.FinalObjectIdentity,
		FinalVerificationBasis: "same_invocation_post_recheck_exact_reverification"}
	receipt, _, err := handle.appendRecheckCompletion(ctx, value)
	report.recordMarker(receipt)
	if err != nil {
		return err
	}
	report.Final.Status, report.Final.PostAction = "verified_current", &final
	report.Outcome = OutcomeCheckedStopped
	if handle.state.Intent.Plan.Action == ActionRecheckOnly {
		report.Operation.Status, report.Operation.Resumable = "terminal", false
	} else {
		report.Operation.Status, report.Operation.Resumable = "active", true
	}
	return nil
}

func completeActivation(ctx context.Context, prepared *PreparedPlan, handle *journalHandle, observed clientObservation, report *Report) error {
	if len(handle.state.StartAttemptIDs) == 0 || handle.state.RecheckCompletion == nil {
		return fmt.Errorf("%w: activation completion has no durable request authority", ErrIntegrity)
	}
	if !startedState(observed.job.State) || observed.job.Progress != 1 || !observed.allSelected || !observed.allComplete {
		return fmt.Errorf("%w: client start completion is not fully observed", ErrPolicy)
	}
	if len(handle.state.StartAttempts) == 0 || observed.ledger.ObservedAtStart.Before(handle.state.StartAttempts[len(handle.state.StartAttempts)-1].ObservedAtEnd) {
		return fmt.Errorf("%w: start completion observation is not ordered", ErrIntegrity)
	}
	final, err := prepared.ReverifyFinal(ctx)
	if err != nil {
		return err
	}
	value := ActivationCompletion{Schema: ActivationSchemaV1, OperationID: handle.state.Intent.OperationID, PlanID: handle.state.Intent.PlanID,
		RecheckCompletionID: handle.state.RecheckCompletionID, StartAttemptID: handle.state.StartAttemptIDs[len(handle.state.StartAttemptIDs)-1],
		ObservedAtStart: observed.ledger.ObservedAtStart, ObservedAtEnd: observed.ledger.ObservedAtEnd, JobID: observed.jobID,
		JobState: observed.job.State, JobProgress: observed.job.Progress, FileLayoutID: observed.fileLayoutID,
		CompleteFileSnapshotID: observed.completeSnapshotID, FinalObjectIdentity: final.FinalObjectIdentity,
		FinalVerificationBasis: "same_invocation_post_start_exact_reverification"}
	receipt, _, err := handle.appendActivationCompletion(ctx, value)
	report.recordMarker(receipt)
	if err != nil {
		return err
	}
	report.Final.Status, report.Final.PostAction = "verified_current", &final
	report.Outcome = OutcomeStartedClientClaim
	report.Operation.Status, report.Operation.Resumable = "terminal", false
	return nil
}

func Status(ctx context.Context, options StatusOptions) (Report, error) {
	report := newReport(nil, "")
	report.Effect = []string{"read_private_client_activation_journal"}
	report.Operation = OperationReport{ID: options.OperationID.String(), Status: "inspection_incomplete", PhaseBefore: "unknown", PhaseAfter: "unknown"}
	report.Journal.Status = "inspection_incomplete"
	if options.TargetRoot == "" {
		err := fmt.Errorf("%w: activation status target is empty", ErrPolicy)
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	handle, _, err := openJournal(ctx, options.TargetRoot, options.OperationID, false, nil)
	if err != nil {
		var forgetting *forgetInProgressError
		if errors.As(err, &forgetting) {
			applyForgetControlReport(&report, forgetting)
			report.finalize()
			return report, nil
		}
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	defer handle.Close()
	if handle.state.Retained {
		applyRetainedJournalReport(&report, handle.state)
		report.finalize()
		return report, nil
	}
	reportFromState(&report, handle.state)
	phase := phaseForState(handle.state)
	switch {
	case handle.state.ActivationCompletion != nil:
		report.Outcome = OutcomeHistoricalStarted
		report.Operation.Status, report.Operation.Resumable = "terminal", false
	case handle.state.RecheckCompletion != nil && handle.state.Intent.Plan.Action == ActionRecheckOnly:
		report.Outcome = OutcomeHistoricalChecked
		report.Operation.Status, report.Operation.Resumable = "terminal", false
	case handle.state.RecheckCompletion != nil:
		report.Outcome = OutcomeHistoricalChecked
		report.Operation.Status, report.Operation.Resumable = "active", true
	case handle.state.RecheckStarted != nil:
		report.Outcome = OutcomeRecheckInProgress
		report.Operation.Status, report.Operation.Resumable = "active", true
	case len(handle.state.RecheckAttempts) > 0:
		report.Outcome = OutcomeRecheckRequestUnknown
		report.Operation.Status, report.Operation.Resumable = "active", true
	default:
		report.Outcome = OutcomeIncomplete
		report.Operation.Status, report.Operation.Resumable = "active", true
	}
	report.Operation.PhaseBefore, report.Operation.PhaseAfter = phase, phase
	report.finalize()
	return report, nil
}

func reportFromState(report *Report, state journalState) {
	if report == nil || state.IntentID == "" {
		return
	}
	planID := state.Intent.PlanID
	prepared := &PreparedPlan{plan: state.Intent.Plan, planID: planID, operationID: state.Intent.OperationID}
	report.applyPrepared(prepared)
	phase := phaseForState(state)
	report.Operation = OperationReport{ID: state.Intent.OperationID.String(), Status: "active", PhaseBefore: phase, PhaseAfter: phase, Resumable: true}
	report.Journal = journalReport(state)
	report.Final = FinalReport{Status: "historical_activation_intent_current_not_observed", Observation: historicalFinal(state.Intent.Plan)}
	report.Adoption = AdoptionReport{Status: "historical_adoption_completion_reference_current_not_observed", Observation: historicalAdoption(state.Intent.Plan)}
}

func journalReport(state journalState) JournalReport {
	legacyDurable := state.Durable && !state.Retained
	return JournalReport{Status: phaseForState(state), IntentDurable: state.IntentID != "" && legacyDurable,
		RecheckAttempts: len(state.RecheckAttempts), RecheckStartedDurable: state.RecheckStarted != nil && legacyDurable,
		RecheckCompletionDurable: state.RecheckCompletion != nil && legacyDurable, StartAttempts: len(state.StartAttempts),
		ActivationCompletionDurable: state.ActivationCompletion != nil && legacyDurable, PendingRecoveryMarker: state.Pending != "",
		RetentionState: retentionStateLabel(state), RetentionIntentPresent: state.Retained && state.RetentionIntentDurable,
		RetentionCompletionPresent: state.Retained && state.RetentionComplete && state.RetentionCompleteID != "",
		RetentionIntentDurable:     false, RetentionCompletionDurable: false}
}

func phaseForState(state journalState) string {
	switch {
	case state.Retained && state.RetentionComplete:
		return "retained_activation_completion"
	case state.Retained && state.RetentionIntentDurable:
		return "retention_intent_recorded"
	case state.Retained:
		return "retention_initializing"
	case state.Pending != "":
		return "marker_recovery_required"
	case state.ActivationCompletion != nil:
		return "started_client_claim_observed"
	case len(state.StartAttempts) > 0:
		return "start_request_result_unknown"
	case state.RecheckCompletion != nil:
		return "recheck_complete_stopped"
	case state.RecheckStarted != nil:
		return "recheck_in_progress"
	case len(state.RecheckAttempts) > 0:
		return "recheck_request_result_unknown"
	case state.IntentID != "":
		return "intent_recorded"
	default:
		return "initialization_incomplete"
	}
}

func retentionStateLabel(state journalState) string {
	switch {
	case state.Retained && state.RetentionComplete:
		return "complete"
	case state.Retained && state.RetentionIntentDurable:
		return "intent_recorded"
	case state.Retained:
		return "initializing"
	default:
		return "not_requested"
	}
}

func applyRetainedJournalReport(report *Report, state journalState) {
	if report == nil {
		return
	}
	reportFromState(report, state)
	report.Operation.Resumable = false
	report.Journal = journalReport(state)
	report.Effect = append(report.Effect, "read_private_client_activation_retention_state")
	switch {
	case state.RetentionComplete && state.ActivationCompletion != nil:
		report.Outcome, report.Operation.Status = OutcomeHistoricalStarted, "retained"
		report.Client.Status = "historical_activation_tombstone_current_client_not_observed"
	case state.RetentionComplete:
		report.Outcome, report.Operation.Status = OutcomeHistoricalChecked, "retained"
		report.Client.Status = "historical_recheck_tombstone_current_client_not_observed"
	case state.RetentionIntentDurable:
		report.Outcome, report.Operation.Status = OutcomeIncomplete, "pruning"
		report.addBlocker("operation.prune_required", "a durable retention intent exists; only explicit client activate prune may complete deletion")
	default:
		report.Outcome, report.Operation.Status = OutcomeIncomplete, "retention_initializing"
		report.addBlocker("operation.prune_required", "a reserved retention boundary exists; only explicit client activate prune may recover it")
	}
	report.Warnings = append(report.Warnings, "retained activation evidence is historical and does not establish current downloader state or marker durability")
}

func validateAuthorityAgainstPlan(authority *PreparedAuthority, plan Plan) error {
	if authority == nil || plan.Validate() != nil || authority.clientConfigID != plan.ClientConfigID ||
		authority.final.OperationID != plan.MaterializeOperationID || authority.final.MaterializePlanID != plan.MaterializePlanID ||
		authority.final.MetafileVariantID != plan.MetafileVariantID || authority.final.InfoHashV1 != plan.InfoHashV1 || authority.final.InfoHashV2 != plan.InfoHashV2 ||
		authority.final.TargetRootIdentity != plan.TargetRootIdentity || authority.final.FinalObjectIdentity != plan.FinalObjectIdentity ||
		authority.final.MultiFile != plan.MultiFile || authority.final.ManifestFiles != plan.ManifestFiles || authority.final.ContentBytes != plan.ContentBytes ||
		authority.adoption.OperationID != plan.AdoptionOperationID || authority.adoption.PlanID != plan.AdoptionPlanID || authority.adoption.CompletionID != plan.AdoptionCompletionID ||
		authority.adoption.JobID != plan.JobID || authority.projection.PathMappingID != plan.PathMappingID || authority.projection.PathSemantics != plan.ClientPathSemantics ||
		authority.projection.SavePathRef != plan.ExpectedSavePathRef || authority.projection.ContentPathRef != plan.ExpectedContentPathRef || authority.fileLimits != plan.FileLimits {
		return fmt.Errorf("%w: current authority differs from the activation intent", ErrPolicy)
	}
	return nil
}

func historicalFinal(plan Plan) materialize.FinalObservation {
	return materialize.FinalObservation{OperationID: plan.MaterializeOperationID, MaterializePlanID: plan.MaterializePlanID,
		MetafileVariantID: plan.MetafileVariantID, InfoHashV1: plan.InfoHashV1, InfoHashV2: plan.InfoHashV2,
		TargetRootIdentity: plan.TargetRootIdentity, FinalObjectIdentity: plan.FinalObjectIdentity, MultiFile: plan.MultiFile,
		ManifestFiles: plan.ManifestFiles, ContentBytes: plan.ContentBytes, AuthorityBasis: "historical_activation_intent", Assurance: "current_final_not_observed"}
}

func historicalAdoption(plan Plan) clientadopt.CompletionObservation {
	return clientadopt.CompletionObservation{OperationID: plan.AdoptionOperationID, PlanID: plan.AdoptionPlanID, CompletionID: plan.AdoptionCompletionID,
		MetafileVariantID: plan.MetafileVariantID, ClientConfigID: plan.ClientConfigID, PathMappingID: plan.PathMappingID,
		ClientPathSemantics: plan.ClientPathSemantics, ExpectedSavePathRef: plan.ExpectedSavePathRef,
		ExpectedContentPathRef: plan.ExpectedContentPathRef, JobID: plan.JobID, FinalObjectIdentity: plan.FinalObjectIdentity,
		Assurance: "historical_activation_intent_current_adoption_not_observed"}
}

func validActionReceipt(receipt downloader.ExistingJobMutationReceipt, action, jobKey string) bool {
	effect := downloader.ControlEffectRecheck
	if action == AttemptActionStart {
		effect = downloader.ControlEffectStart
	}
	expectedBytes := int64(len(url.Values{"hashes": {jobKey}}.Encode()))
	return receipt.Effect == effect && receipt.Complete && receipt.RequestsAttempted == 1 && receipt.AutomaticRetries == 0 &&
		receipt.RedirectsFollowed == 0 && receipt.RequestBytesKnown && receipt.RequestBytes == expectedBytes && receipt.StopReason == "" &&
		!receipt.ObservedAtStart.IsZero() && !receipt.ObservedAtEnd.Before(receipt.ObservedAtStart)
}

func readControlDescriptor(ctx context.Context, session downloader.ExistingJobMutationSession) (downloader.ExistingJobControlDescriptor, error) {
	if session == nil || session.RequestsMade() != 1 {
		return downloader.ExistingJobControlDescriptor{}, fmt.Errorf("%w: downloader control session is not fresh", ErrIntegrity)
	}
	before := session.RequestsMade()
	descriptor, err := session.ReadExistingJobControlDescriptor(ctx)
	after := session.RequestsMade()
	if after-before < 0 || after-before > 1 || err == nil && after-before != 1 {
		return downloader.ExistingJobControlDescriptor{}, fmt.Errorf("%w: downloader control descriptor request count is contradictory", ErrIntegrity)
	}
	if err == nil && descriptor.Validate() != nil {
		return downloader.ExistingJobControlDescriptor{}, fmt.Errorf("%w: downloader control descriptor is contradictory", ErrIntegrity)
	}
	return descriptor, err
}

func observeClientBounded(ctx context.Context, authority *PreparedAuthority, session downloader.LedgerSession) (clientObservation, error) {
	var empty clientObservation
	if session == nil {
		return empty, fmt.Errorf("%w: downloader ledger session is unavailable", ErrPolicy)
	}
	before := session.RequestsMade()
	observed, err := observeClient(ctx, authority, session)
	after := session.RequestsMade()
	delta := after - before
	observed.ledgerRequestMade = delta >= 1
	observed.fileRequestMade = delta >= 2
	if delta < 0 || delta > 2 {
		return empty, fmt.Errorf("%w: downloader observation request count is contradictory", ErrIntegrity)
	}
	if err == nil {
		expected := 1
		if authority != nil && authority.final.MultiFile {
			expected = 2
		}
		if delta != expected {
			return empty, fmt.Errorf("%w: downloader observation request count is contradictory", ErrIntegrity)
		}
	}
	return observed, err
}

func safeActionReceipt(value downloader.ExistingJobMutationReceipt, action, jobKey string) downloader.ExistingJobMutationReceipt {
	effect := downloader.ControlEffectRecheck
	if action == AttemptActionStart {
		effect = downloader.ControlEffectStart
	}
	result := downloader.ExistingJobMutationReceipt{Effect: effect, RequestsAttempted: -1, AutomaticRetries: -1, RedirectsFollowed: -1}
	if !value.ObservedAtStart.IsZero() && !value.ObservedAtEnd.Before(value.ObservedAtStart) {
		result.ObservedAtStart, result.ObservedAtEnd = value.ObservedAtStart, value.ObservedAtEnd
	}
	if value.RequestsAttempted >= 0 && value.RequestsAttempted <= 1 {
		result.RequestsAttempted = value.RequestsAttempted
	}
	if value.AutomaticRetries >= 0 && value.AutomaticRetries <= 16 {
		result.AutomaticRetries = value.AutomaticRetries
	}
	if value.RedirectsFollowed >= 0 && value.RedirectsFollowed <= 16 {
		result.RedirectsFollowed = value.RedirectsFollowed
	}
	if value.RequestBytes >= 0 && value.RequestBytes <= 1024 {
		result.RequestBytes, result.RequestBytesKnown = value.RequestBytes, value.RequestBytesKnown
	}
	result.Complete = validActionReceipt(value, action, jobKey)
	switch value.StopReason {
	case "context_cancelled", "job_locator_invalid", "control_descriptor_unavailable", "action_invalid", "request_build_failed", "session_unavailable", "transport_failed", "response_read_failed", "http_rejected", "response_invalid":
		result.StopReason = value.StopReason
	case "":
	default:
		result.StopReason = "client_mutation_failed"
	}
	return result
}

func classifyFailure(report *Report, err error) {
	if report == nil || err == nil {
		return
	}
	switch {
	case errors.Is(err, ErrIntegrity), errors.Is(err, materialize.ErrIntegrity):
		report.Outcome = OutcomeIntegrityFailed
		report.Operation.Status = "integrity_failed"
		report.Operation.Resumable = false
		report.addBlocker("integrity.activation_state_invalid", "client activation authority or exact content failed integrity validation")
	case errors.Is(err, ErrRequestUnknown):
		if report.Journal.StartAttempts > 0 || report.Client.ActionAttempted == AttemptActionStart {
			report.Outcome = OutcomeStartRequestUnknown
		} else {
			report.Outcome = OutcomeRecheckRequestUnknown
		}
		report.Operation.Status, report.Operation.Resumable = "active", true
		report.addBlocker("client.request_result_unknown", "an effectful client request cannot be safely replayed without explicit repeat acknowledgement")
	case errors.Is(err, ErrInitializationIncomplete):
		report.Outcome = OutcomeBlocked
		report.Operation.Status = "initialization_incomplete"
		report.Operation.PhaseAfter = "initialization_incomplete"
		report.Operation.Resumable = report.Operation.ID != ""
		report.Journal.Status = "initialization_incomplete"
		report.addBlocker("operation.initialization_incomplete", "client activation initialization can continue only through explicit reviewed resume")
	case errors.Is(err, ErrOperationNotFound):
		report.Outcome = OutcomeBlocked
		report.Operation.Status, report.Operation.Resumable = "not_found", false
		report.addBlocker("operation.not_found", "the explicit client activation operation does not exist")
	case errors.Is(err, ErrPolicy):
		report.Outcome = OutcomeBlocked
		if report.Operation.Status == "not_created" || report.Operation.Status == "inspection_incomplete" {
			report.Operation.Resumable = false
		}
		report.addBlocker("policy.activation_blocked", "client activation preconditions are not satisfied")
	default:
		report.Outcome = OutcomeIncomplete
		report.addBlocker("operation.interrupted", "client activation was interrupted before a conclusive state was established")
	}
}
