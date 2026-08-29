package clientadopt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
)

type RunOptions struct {
	Prepared                   *PreparedPlan
	ExpectedPlanID             string
	Metafile                   *metastore.ArtifactPayload
	Session                    downloader.LedgerSession
	AcknowledgeAdd             bool
	AcknowledgeExistingStopped bool
	AcknowledgeReAdoption      bool
	AcknowledgeRemovalReAdd    bool
	RepeatAdd                  bool
}

type StatusOptions struct {
	TargetRoot  string
	OperationID OperationID
}

func FailureReport(prepared *PreparedPlan, expectedPlanID, operation string, requestsMade int, err error) Report {
	report := newReport(prepared, expectedPlanID)
	if operation == "run" || operation == "resume" {
		report.Effect = append(report.Effect, "write_private_client_adoption_journal")
		if prepared != nil && isStoppedAddAction(prepared.plan.Action) {
			report.Effect = append(report.Effect, "submit_exact_metafile_stopped")
		}
	}
	if operation == "resume" || operation == "status" {
		report.Effect = append(report.Effect, "read_private_client_adoption_journal")
	}
	if requestsMade >= 0 {
		report.Client.RequestsMade = requestsMade
	}
	classifyFailure(&report, err)
	report.finalize()
	return report
}

func Preview(ctx context.Context, prepared *PreparedPlan, session downloader.LedgerSession) (Report, error) {
	report := newReport(prepared, "")
	if err := validatePrepared(prepared, "", nil); err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if session != nil {
		report.Client.RequestsMade = session.RequestsMade()
	}
	if err := validateFreshLedgerSession(session, prepared); err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if err := refreshPreparedFinal(ctx, prepared, &report); err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	assessment, err := readIdentityLedger(ctx, session, prepared)
	report.Client.RequestsMade = session.RequestsMade()
	applyBeforeAssessment(&report, assessment)
	if err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if prepared.plan.Action == ActionAdoptExistingStopped {
		if assessment.status != downloader.LedgerIdentityExactUnique {
			err = requireExactIdentity(&report, assessment)
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
		job, jobErr := validateObservedJob(prepared, assessment.result.ExactJob)
		if jobErr != nil {
			classifyFailure(&report, jobErr)
			report.finalize()
			return report, jobErr
		}
		applyObservedJob(&report, prepared, job)
		report.Outcome = OutcomeReady
		report.Client.Status = "existing_unique_exact_stopped_job_ready_for_observation_only_adoption"
		report.Client.Assurance = "single_complete_typed_job_and_path_observation_plus_current_exact_final_non_atomic"
		report.finalize()
		return report, nil
	}
	if assessment.status != downloader.LedgerIdentityAbsent {
		err = identityGateError(&report, assessment, true)
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	report.Outcome = OutcomeReady
	report.Client.Status = "target_identity_absent"
	report.Client.Assurance = "single_complete_typed_ledger_observation_non_atomic"
	report.finalize()
	return report, nil
}

// PreflightRun verifies deterministic selectors and target-root absence before
// a caller reads credentials or opens a network session.
func PreflightRun(ctx context.Context, prepared *PreparedPlan, expectedPlanID string, payload *metastore.ArtifactPayload) error {
	if err := validatePrepared(prepared, expectedPlanID, payload); err != nil {
		return err
	}
	if isStoppedAddAction(prepared.plan.Action) {
		if err := validateMetafilePayload(prepared, payload); err != nil {
			return err
		}
	} else if payload != nil {
		return fmt.Errorf("%w: observation-only adoption must not receive a private metafile payload", ErrPolicy)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	targetRoot, ok := prepared.targetRoot()
	if !ok {
		return fmt.Errorf("%w: target root authority is unavailable", ErrPolicy)
	}
	session, rootInfo, err := fsbind.BindExisting(targetRoot)
	if err != nil {
		return err
	}
	defer session.Close()
	if rootInfo.Identity.String() != prepared.plan.TargetRootIdentity {
		return fmt.Errorf("%w: target root identity differs from the reviewed plan", ErrIntegrity)
	}
	directoryName, _ := operationDirectoryName(prepared.operation)
	_, err = session.InspectRoot(ctx, directoryName)
	if errors.Is(err, fsbind.ErrNotFound) {
		return nil
	}
	if err != nil {
		return classifyJournalError(err)
	}
	return fmt.Errorf("%w: deterministic adoption operation already exists; use resume", ErrPolicy)
}

func Run(ctx context.Context, options RunOptions) (Report, error) {
	report := newReport(options.Prepared, options.ExpectedPlanID)
	report.Effect = append(report.Effect, "write_private_client_adoption_journal")
	if options.Prepared != nil && isStoppedAddAction(options.Prepared.plan.Action) {
		report.Effect = append(report.Effect, "submit_exact_metafile_stopped")
	}
	if err := validatePrepared(options.Prepared, options.ExpectedPlanID, options.Metafile); err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if isStoppedAddAction(options.Prepared.plan.Action) {
		if err := validateMetafilePayload(options.Prepared, options.Metafile); err != nil {
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
		if !options.AcknowledgeAdd || options.AcknowledgeExistingStopped {
			err := fmt.Errorf("%w: explicit downloader-add acknowledgement is required", ErrPolicy)
			report.addBlocker("acknowledgement.client_add_required", "stopped-add adoption requires its exact acknowledgement and no observation-only acknowledgement")
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
	} else if options.Metafile != nil || options.AcknowledgeAdd || options.RepeatAdd || options.AcknowledgeReAdoption || options.AcknowledgeRemovalReAdd || !options.AcknowledgeExistingStopped {
		err := fmt.Errorf("%w: explicit observation-only existing-job adoption acknowledgement is required", ErrPolicy)
		report.addBlocker("acknowledgement.existing_stopped_adoption_required", "observation-only adoption requires its dedicated acknowledgement and cannot use add or re-adoption authority")
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if options.Prepared.prior != nil && !options.AcknowledgeReAdoption {
		err := fmt.Errorf("%w: explicit re-adoption acknowledgement is required", ErrPolicy)
		report.addBlocker("acknowledgement.client_re_adoption_required", "re-adoption after a prior terminal job disappeared requires its dedicated acknowledgement")
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if options.Prepared.removal != nil && !options.AcknowledgeRemovalReAdd {
		err := fmt.Errorf("%w: explicit removal-authorized re-add acknowledgement is required", ErrPolicy)
		report.addBlocker("acknowledgement.client_re_add_after_removal_required", "a new stopped add after an attributed terminal removal requires its dedicated acknowledgement")
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if (options.Prepared.removal == nil && options.AcknowledgeRemovalReAdd) ||
		(options.Prepared.prior == nil && options.AcknowledgeReAdoption) {
		err := fmt.Errorf("%w: lineage acknowledgement does not match the reviewed adoption plan", ErrPolicy)
		report.addBlocker("acknowledgement.lineage_mismatch", "the supplied re-adoption acknowledgement does not match the plan's bound historical lineage")
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if options.Session != nil {
		report.Client.RequestsMade = options.Session.RequestsMade()
	}
	if err := validateFreshLedgerSession(options.Session, options.Prepared); err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if err := refreshPreparedFinal(ctx, options.Prepared, &report); err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	assessment, err := readIdentityLedger(ctx, options.Session, options.Prepared)
	report.Client.RequestsMade = options.Session.RequestsMade()
	applyBeforeAssessment(&report, assessment)
	if err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if isStoppedAddAction(options.Prepared.plan.Action) {
		if assessment.status != downloader.LedgerIdentityAbsent {
			err = identityGateError(&report, assessment, true)
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
	} else {
		if assessment.status != downloader.LedgerIdentityExactUnique {
			err = requireExactIdentity(&report, assessment)
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
		if _, err = validateObservedJob(options.Prepared, assessment.result.ExactJob); err != nil {
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
	}
	targetRoot, _ := options.Prepared.targetRoot()
	handle, creation, err := createJournal(ctx, targetRoot, options.Prepared.plan, options.Prepared.planID)
	report.recordCreation(creation)
	if err != nil {
		if creation.Subtree.Created || creation.Mkdir.DirectoriesCreated > 0 || creation.Intent.TemporaryCreated || creation.Intent.Publication.Attempted {
			report.Operation.Status = "initialization_incomplete"
			report.Operation.PhaseBefore = "planned"
			report.Operation.PhaseAfter = "initialization_incomplete"
			report.Operation.Resumable = true
			report.Journal.Status = "initialization_incomplete"
		}
		if errors.Is(err, fsbind.ErrAlreadyExists) {
			report.Operation.Status = "existing_uninspected"
			report.Operation.PhaseAfter = "unknown"
			report.Operation.Resumable = false
			report.addBlocker("operation.exists", "the deterministic adoption operation already exists; use resume")
			err = fmt.Errorf("%w: adoption operation already exists", ErrPolicy)
		}
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	defer handle.Close()
	report.Operation.Status = "active"
	report.Operation.PhaseBefore = "planned"
	report.Operation.PhaseAfter = "intent_recorded"
	report.Operation.Resumable = true
	report.Journal = journalReport(handle.state)
	if options.Prepared.plan.Action == ActionAdoptExistingStopped {
		return executeExistingObservation(ctx, options, handle, assessment, report)
	}
	return executeAdd(ctx, options, handle, assessment, report)
}

func Resume(ctx context.Context, operationID OperationID, options RunOptions) (Report, error) {
	report := newReport(options.Prepared, options.ExpectedPlanID)
	report.Effect = append(report.Effect, "read_private_client_adoption_journal", "write_private_client_adoption_journal")
	if options.Prepared != nil && isStoppedAddAction(options.Prepared.plan.Action) {
		report.Effect = append(report.Effect, "submit_exact_metafile_stopped")
	}
	if err := validatePrepared(options.Prepared, options.ExpectedPlanID, options.Metafile); err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if isStoppedAddAction(options.Prepared.plan.Action) {
		if options.AcknowledgeExistingStopped {
			err := fmt.Errorf("%w: stopped-add resume cannot use observation-only acknowledgement", ErrPolicy)
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
	} else if options.Metafile != nil || options.AcknowledgeAdd || options.RepeatAdd || options.AcknowledgeReAdoption || options.AcknowledgeRemovalReAdd || !options.AcknowledgeExistingStopped {
		err := fmt.Errorf("%w: observation-only resume requires its dedicated acknowledgement and no add authority", ErrPolicy)
		report.addBlocker("acknowledgement.existing_stopped_adoption_required", "observation-only adoption resume requires its dedicated acknowledgement and cannot repeat an add")
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if operationID != options.Prepared.operation {
		err := fmt.Errorf("%w: explicit operation does not match the reviewed adoption plan", ErrPolicy)
		report.addBlocker("operation.id_mismatch", "the explicit operation ID does not match the deterministic reviewed plan")
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	report.Operation = OperationReport{
		ID: operationID.String(), Status: "inspection_incomplete", PhaseBefore: "unknown", PhaseAfter: "unknown",
	}
	if options.Session != nil {
		report.Client.RequestsMade = options.Session.RequestsMade()
	}
	if err := validateFreshLedgerSession(options.Session, options.Prepared); err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if err := refreshPreparedFinal(ctx, options.Prepared, &report); err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	targetRoot, _ := options.Prepared.targetRoot()
	handle, recovered, err := openJournal(ctx, targetRoot, operationID, true, &options.Prepared.plan)
	report.recordRecovery(recovered)
	if err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	defer handle.Close()
	if handle.state.Retained {
		applyRetainedJournalReport(&report, handle.state)
		err := fmt.Errorf("%w: retained client adoption state cannot be resumed", ErrPolicy)
		report.addBlocker("operation.prune_boundary", "the adoption journal was pruned; only its historical completion tombstone remains")
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if handle.state.Intent.Plan != options.Prepared.plan || handle.state.Intent.PlanID != options.Prepared.planID {
		err := fmt.Errorf("%w: journal intent differs from the reviewed adoption plan", ErrPolicy)
		report.addBlocker("plan.journal_mismatch", "the selected operation belongs to a different reviewed plan")
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	report.Operation.Status = "active"
	report.Operation.PhaseBefore = phaseForState(handle.state)
	report.Operation.PhaseAfter = report.Operation.PhaseBefore
	report.Operation.Resumable = handle.state.Completion == nil
	report.Journal = journalReport(handle.state)
	if handle.state.Completion != nil {
		report.Operation.Status = "terminal"
		report.Operation.Resumable = false
	}
	assessment, err := readIdentityLedger(ctx, options.Session, options.Prepared)
	report.Client.RequestsMade = options.Session.RequestsMade()
	applyBeforeAssessment(&report, assessment)
	if err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if handle.state.Completion != nil {
		if assessment.status != downloader.LedgerIdentityExactUnique {
			err = requireExactIdentity(&report, assessment)
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
		job, jobErr := validateObservedJob(options.Prepared, assessment.result.ExactJob)
		if jobErr != nil {
			classifyFailure(&report, jobErr)
			report.finalize()
			return report, jobErr
		}
		if jobID(job.Hash) != handle.state.Completion.JobID {
			jobErr = fmt.Errorf("%w: the current exact job has a different opaque identity from the journaled observation", ErrPolicy)
			report.addBlocker("client.job_identity_changed", "the current exact typed-infohash job is not the same opaque job observed at completion")
			classifyFailure(&report, jobErr)
			report.finalize()
			return report, jobErr
		}
		applyObservedJob(&report, options.Prepared, job)
		report.Outcome = OutcomeAlreadyAdopted
		report.Operation.Status = "terminal"
		report.Operation.PhaseAfter = "adopted_pending_client_recheck"
		report.Operation.Resumable = false
		if options.Prepared.plan.Action == ActionAdoptExistingStopped {
			report.Client.Assurance = "current_single_ledger_observation_plus_historical_existing_job_bracket_record_non_atomic"
		} else {
			report.Client.Assurance = "current_single_ledger_observation_plus_historical_exact_submission_record_non_atomic"
		}
		report.finalize()
		return report, nil
	}
	if options.Prepared.plan.Action == ActionAdoptExistingStopped {
		if assessment.status != downloader.LedgerIdentityExactUnique {
			err = requireExactIdentity(&report, assessment)
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
		if _, err = validateObservedJob(options.Prepared, assessment.result.ExactJob); err != nil {
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
		return executeExistingObservation(ctx, options, handle, assessment, report)
	}
	if assessment.status == downloader.LedgerIdentityExactUnique {
		if len(handle.state.Attempts) == 0 {
			err = fmt.Errorf("%w: exact job appeared without a journaled add request intent", ErrPolicy)
			report.addBlocker("client.exact_job_not_attributable", "an exact job exists but this operation recorded no add request intent")
			classifyFailure(&report, err)
			report.finalize()
			return report, err
		}
		if options.Prepared.plan.Driver == DriverTransmission {
			report.Outcome = OutcomeRequestUnknown
			report.Operation.PhaseAfter = "request_result_unknown"
			report.Operation.Resumable = true
			report.Client.Status = "exact_job_observed_without_durable_transmission_acceptance"
			report.addBlocker("client.transmission_add_acceptance_unavailable", "an exact Transmission job exists after an interrupted prior request, but no same-invocation accepted add response is available to attribute it")
			report.finalize()
			return report, ErrRequestUnknown
		}
		return finalizeObserved(ctx, options, handle, assessment, assessment.started, nil,
			"same_invocation_post_add_exact_reverification",
			"submitted_exact_variant_plus_current_typed_job_and_path_claim_plus_post_add_exact_final_reverification_non_atomic", report)
	}
	if assessment.status != downloader.LedgerIdentityAbsent {
		err = identityGateError(&report, assessment, false)
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if len(handle.state.Attempts) > 0 && !options.RepeatAdd {
		report.Outcome = OutcomeRequestUnknown
		report.Operation.PhaseAfter = "request_result_unknown"
		report.Operation.Resumable = true
		report.Client.Status = "request_result_unknown_target_absent_currently"
		report.addBlocker("request.repeat_acknowledgement_required", "the prior add may have executed; another POST requires explicit repeat acknowledgement")
		report.finalize()
		return report, ErrRequestUnknown
	}
	if options.Prepared.prior != nil && !options.AcknowledgeReAdoption {
		err = fmt.Errorf("%w: explicit re-adoption acknowledgement is required", ErrPolicy)
		report.addBlocker("acknowledgement.client_re_adoption_required", "re-adoption after a prior terminal job disappeared requires its dedicated acknowledgement")
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if options.Prepared.removal != nil && !options.AcknowledgeRemovalReAdd {
		err = fmt.Errorf("%w: explicit removal-authorized re-add acknowledgement is required", ErrPolicy)
		report.addBlocker("acknowledgement.client_re_add_after_removal_required", "a new stopped add after an attributed terminal removal requires its dedicated acknowledgement")
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if (options.Prepared.removal == nil && options.AcknowledgeRemovalReAdd) ||
		(options.Prepared.prior == nil && options.AcknowledgeReAdoption) {
		err = fmt.Errorf("%w: lineage acknowledgement does not match the reviewed adoption plan", ErrPolicy)
		report.addBlocker("acknowledgement.lineage_mismatch", "the supplied re-adoption acknowledgement does not match the plan's bound historical lineage")
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if !options.AcknowledgeAdd {
		err = fmt.Errorf("%w: explicit downloader-add acknowledgement is required", ErrPolicy)
		report.addBlocker("acknowledgement.client_add_required", "client adoption requires explicit acknowledgement of the stopped add request")
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	return executeAdd(ctx, options, handle, assessment, report)
}

func Status(ctx context.Context, options StatusOptions) (Report, error) {
	report := newReport(nil, "")
	report.Effect = []string{"read_private_client_adoption_journal"}
	report.Operation = OperationReport{ID: options.OperationID.String(), Status: "inspection_incomplete", PhaseBefore: "unknown", PhaseAfter: "unknown"}
	report.Final = FinalReport{Status: "not_observed"}
	report.Client = ClientReport{Status: "not_observed", BeforeIdentity: "not_observed", AfterIdentity: "not_observed", VariantRelation: "historical_unobservable", Assurance: "not_observed"}
	handle, _, err := openJournal(ctx, options.TargetRoot, options.OperationID, false, nil)
	if err != nil {
		var forgetting *forgetInProgressError
		if errors.As(err, &forgetting) {
			applyForgetControlReport(&report, forgetting)
			report.finalize()
			return report, nil
		}
		if errors.Is(err, ErrInitializationIncomplete) {
			report.Outcome = OutcomeIncomplete
			report.Operation.Status = "initialization_incomplete"
			report.Operation.PhaseBefore = "initialization_incomplete"
			report.Operation.PhaseAfter = "initialization_incomplete"
			report.Operation.Resumable = true
			report.Journal.Status = "initialization_incomplete"
			report.addBlocker("journal.initialization_incomplete", "resume with the exact reviewed plan can complete an otherwise empty journal initialization")
			report.finalize()
			return report, err
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
	plan := handle.state.Intent.Plan
	report.Plan = planReport(plan, handle.state.Intent.PlanID, "")
	applyHistoricalTerminalRemoval(&report, plan)
	report.Final.Observation = historicalFinalObservation(plan)
	report.Journal = journalReport(handle.state)
	report.Warnings = append(report.Warnings, "read-only status observes canonical journal markers but does not refresh directory durability")
	report.Operation.Status = "active"
	report.Operation.PhaseBefore = phaseForState(handle.state)
	report.Operation.PhaseAfter = report.Operation.PhaseBefore
	report.Operation.Resumable = handle.state.Completion == nil
	switch {
	case handle.state.Pending != "":
		report.Outcome = OutcomeIncomplete
		report.Operation.PhaseAfter = "marker_recovery_required"
		report.Journal.Status = "marker_recovery_required"
		report.addBlocker("journal.pending_recovery", "an exact private marker must be recovered by resume before the operation can advance")
	case handle.state.Completion != nil:
		report.Outcome = OutcomeHistoricalAdopted
		report.Operation.Status = "terminal"
		report.Operation.Resumable = false
		if plan.Action == ActionAdoptExistingStopped {
			report.Client.Status = "historical_existing_stopped_adoption_recorded_current_client_not_observed"
		} else {
			report.Client.Status = "historical_adoption_recorded_current_client_not_observed"
		}
	case len(handle.state.Attempts) > 0:
		if plan.Action == ActionAdoptExistingStopped {
			report.Outcome = OutcomeIncomplete
			report.Client.Status = "historical_existing_job_observation_current_client_not_observed"
			report.addBlocker("client.current_state_not_observed", "status is read-only and cannot complete the second existing-job observation or current final proof")
		} else {
			report.Outcome = OutcomeRequestUnknown
			report.Client.Status = "historical_request_intent_current_client_not_observed"
			report.addBlocker("client.current_state_not_observed", "status is read-only and cannot determine whether the journaled add request executed")
		}
	default:
		report.Outcome = OutcomeIncomplete
		if plan.Action == ActionAdoptExistingStopped {
			report.Client.Status = "historical_intent_no_existing_job_observation"
			report.addBlocker("client.current_state_not_observed", "status is read-only and cannot prove that one exact stopped job currently matches the final")
		} else {
			report.Client.Status = "historical_intent_no_add_request_current_client_not_observed"
			report.addBlocker("client.current_state_not_observed", "status is read-only and cannot prove that the exact typed identity is currently absent")
		}
	}
	report.finalize()
	return report, nil
}

func executeAdd(ctx context.Context, options RunOptions, handle *journalHandle, before downloaderAssessment, report Report) (Report, error) {
	if err := validateMetafilePayload(options.Prepared, options.Metafile); err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	mutationSession, ok := options.Session.(downloader.MutationSession)
	if !ok {
		err := fmt.Errorf("%w: stopped-add mutation authority is unavailable", ErrPolicy)
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	receipt, err := handle.appendAttempt(ctx, before, nil)
	report.recordMarker(receipt)
	report.Journal = journalReport(handle.state)
	report.Operation.PhaseAfter = "request_intent_recorded"
	if err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	savePath, ok := options.Prepared.savePath()
	if !ok {
		err = fmt.Errorf("%w: process-local client save path is unavailable", ErrIntegrity)
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	requestsBefore := options.Session.RequestsMade()
	mutation, mutationErr := mutationSession.AddStopped(ctx, downloader.AddStoppedRequest{
		Metafile: options.Metafile, SavePath: savePath, Identity: options.Prepared.typedIdentity(),
	})
	requestsAfter := options.Session.RequestsMade()
	report.Client.AddAttempted = requestsAfter > requestsBefore
	report.Client.AddReceipt = safeMutationReceipt(mutation, options.Prepared.plan.MetafileBytes)
	report.Client.RequestsMade = requestsAfter
	requestDelta := requestsAfter - requestsBefore
	if requestDelta < 0 || requestDelta > 1 || requestDelta == 0 && mutationErr == nil {
		err = fmt.Errorf("%w: downloader effectful request count is invalid", ErrIntegrity)
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	if requestDelta == 0 {
		report.Outcome = OutcomeRequestUnknown
		report.Operation.PhaseAfter = "request_result_unknown"
		report.addIssue("client.add_not_observed", "the session recorded no add request, but the durable request intent requires explicit repeat acknowledgement")
		report.finalize()
		return report, errors.Join(ErrRequestUnknown, mutationErr)
	}
	if mutationErr == nil && !validSuccessfulMutation(mutation, options.Prepared.plan.MetafileBytes) {
		mutationErr = fmt.Errorf("%w: downloader mutation receipt is contradictory", ErrIntegrity)
	}
	if mutationErr != nil && (errors.Is(mutationErr, context.Canceled) || errors.Is(mutationErr, context.DeadlineExceeded)) {
		report.Outcome = OutcomeRequestUnknown
		report.Operation.PhaseAfter = "request_result_unknown"
		report.addIssue("client.add_interrupted", "the stopped add request was attempted but its result is unknown")
		report.finalize()
		return report, errors.Join(ErrRequestUnknown, mutationErr)
	}
	after, readErr := readIdentityLedger(ctx, options.Session, options.Prepared)
	report.Client.RequestsMade = options.Session.RequestsMade()
	applyAfterAssessment(&report, after)
	if readErr != nil {
		report.Outcome = OutcomeRequestUnknown
		report.Operation.PhaseAfter = "request_result_unknown"
		report.addIssue("client.after_snapshot_failed", "the add request was attempted but the after ledger could not be completed")
		report.finalize()
		return report, errors.Join(ErrRequestUnknown, readErr)
	}
	if after.status != downloader.LedgerIdentityExactUnique {
		gateErr := requireExactIdentity(&report, after)
		report.Outcome = OutcomeRequestUnknown
		report.Operation.PhaseAfter = "request_result_unknown"
		report.finalize()
		return report, errors.Join(ErrRequestUnknown, mutationErr, gateErr)
	}
	if mutation.StopReason == "already_exists" {
		if options.Prepared.plan.Driver != DriverTransmission {
			err = fmt.Errorf("%w: downloader returned an unsupported duplicate-add receipt", ErrIntegrity)
			classifyFailure(&report, err)
			report.Operation.PhaseAfter = "request_result_unknown"
			report.finalize()
			return report, err
		}
		report.Outcome = OutcomeBlocked
		report.Operation.PhaseAfter = "request_rejected_existing_job"
		report.Operation.Resumable = true
		report.Client.Status = "add_rejected_existing_exact_job"
		report.addBlocker("client.add_rejected_existing_job", "Transmission explicitly reported a duplicate; the exact job is not attributable to this adoption operation")
		report.finalize()
		return report, errors.Join(ErrPolicy, mutationErr)
	}
	if mutationErr != nil && options.Prepared.plan.Driver == DriverTransmission {
		report.Outcome = OutcomeRequestUnknown
		report.Operation.PhaseAfter = "request_result_unknown"
		report.Operation.Resumable = true
		report.Client.Status = "transmission_add_acceptance_unavailable"
		report.addIssue("client.transmission_add_unconfirmed", "the Transmission add response was not accepted, so the exact job observed afterward is not attributable to this operation")
		report.finalize()
		return report, errors.Join(ErrRequestUnknown, mutationErr)
	}
	if mutationErr != nil {
		report.addIssue("client.add_response_unconfirmed", "the add response failed, but a unique exact stopped job was observed afterward")
	}
	return finalizeObserved(ctx, options, handle, after, after.started, nil,
		"same_invocation_post_add_exact_reverification",
		"submitted_exact_variant_plus_current_typed_job_and_path_claim_plus_post_add_exact_final_reverification_non_atomic", report)
}

func executeExistingObservation(ctx context.Context, options RunOptions, handle *journalHandle, before downloaderAssessment, report Report) (Report, error) {
	first, err := validateObservedJob(options.Prepared, before.result.ExactJob)
	if err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	receipt, err := handle.appendAttempt(ctx, before, &first)
	report.recordMarker(receipt)
	report.Journal = journalReport(handle.state)
	report.Operation.PhaseAfter = "existing_job_observation_recorded"
	if err != nil {
		classifyFailure(&report, err)
		report.finalize()
		return report, err
	}
	observation, err := reverifyObservedFinal(ctx, options, &report)
	if err != nil {
		classifyFailure(&report, err)
		report.Operation.PhaseAfter = "existing_job_bracket_final_reverification_failed"
		report.finalize()
		return report, err
	}
	after, readErr := readIdentityLedger(ctx, options.Session, options.Prepared)
	report.Client.RequestsMade = options.Session.RequestsMade()
	applyAfterAssessment(&report, after)
	if readErr != nil {
		classifyFailure(&report, readErr)
		report.Operation.PhaseAfter = "existing_job_bracket_incomplete"
		report.finalize()
		return report, readErr
	}
	if after.started.Before(before.ended) {
		err = fmt.Errorf("%w: existing stopped job observations are not temporally ordered", ErrPolicy)
		report.addBlocker("client.existing_job_timeline_invalid", "the second observation does not follow the first observation and intervening exact-final verification")
		classifyFailure(&report, err)
		report.Operation.PhaseAfter = "existing_job_bracket_unstable"
		report.finalize()
		return report, err
	}
	if after.status != downloader.LedgerIdentityExactUnique {
		err = requireExactIdentity(&report, after)
		classifyFailure(&report, err)
		report.Operation.PhaseAfter = "existing_job_bracket_unstable"
		report.finalize()
		return report, err
	}
	second, err := validateObservedJob(options.Prepared, after.result.ExactJob)
	if err != nil {
		classifyFailure(&report, err)
		report.Operation.PhaseAfter = "existing_job_bracket_unstable"
		report.finalize()
		return report, err
	}
	if jobID(first.Hash) != jobID(second.Hash) || first.State != second.State {
		err = fmt.Errorf("%w: existing stopped job identity changed across the adoption bracket", ErrPolicy)
		report.addBlocker("client.existing_job_changed", "the exact stopped job changed between the two observation-only adoption snapshots")
		classifyFailure(&report, err)
		report.Operation.PhaseAfter = "existing_job_bracket_unstable"
		report.finalize()
		return report, err
	}
	return finalizeObserved(ctx, options, handle, after, before.started, &observation,
		"same_invocation_existing_stopped_job_bracket_and_exact_reverification",
		"two_complete_typed_job_and_path_observations_bracketing_same_invocation_exact_final_reverification_non_atomic_without_client_mutation", report)
}

func finalizeObserved(ctx context.Context, options RunOptions, handle *journalHandle, assessment downloaderAssessment, observedStart time.Time,
	verifiedObservation *materialize.FinalObservation, verificationBasis, assurance string, report Report) (Report, error) {
	job, err := validateObservedJob(options.Prepared, assessment.result.ExactJob)
	if err != nil {
		classifyFailure(&report, err)
		report.Operation.PhaseAfter = "request_result_unknown"
		report.finalize()
		return report, err
	}
	applyObservedJob(&report, options.Prepared, job)
	var observation materialize.FinalObservation
	if verifiedObservation == nil {
		observation, err = reverifyObservedFinal(ctx, options, &report)
		if err != nil {
			classifyFailure(&report, err)
			report.Operation.PhaseAfter = "client_observed_final_reverification_failed"
			report.finalize()
			return report, err
		}
	} else {
		observation = *verifiedObservation
	}
	lastAttempt := handle.state.AttemptIDs[len(handle.state.AttemptIDs)-1]
	completion := Completion{
		Schema: CompletionSchemaV1, OperationID: handle.state.Intent.OperationID, PlanID: handle.state.Intent.PlanID,
		AttemptID: lastAttempt, ObservedAtStart: observedStart, ObservedAtEnd: assessment.ended,
		JobID: jobID(job.Hash), JobState: job.State, ContentPathRef: options.Prepared.plan.ExpectedContentPathRef,
		FinalObjectIdentity: observation.FinalObjectIdentity, FinalVerificationBasis: verificationBasis,
	}
	receipt, err := handle.appendCompletion(ctx, completion)
	report.recordMarker(receipt)
	report.Journal = journalReport(handle.state)
	if err != nil {
		classifyFailure(&report, err)
		report.Operation.PhaseAfter = "completion_publication_incomplete"
		report.finalize()
		return report, err
	}
	if options.Prepared.plan.Action == ActionAdoptExistingStopped {
		report.Outcome = OutcomeExistingAdoptedPendingRecheck
	} else {
		report.Outcome = OutcomeAdoptedPendingRecheck
	}
	report.Operation.Status = "terminal"
	report.Operation.PhaseAfter = "adopted_pending_client_recheck"
	report.Operation.Resumable = false
	report.Client.Status = "unique_exact_stopped_job_observed"
	report.Client.Assurance = assurance
	report.finalize()
	return report, nil
}

func reverifyObservedFinal(ctx context.Context, options RunOptions, report *Report) (materialize.FinalObservation, error) {
	if options.Prepared == nil || options.Prepared.verified == nil {
		return materialize.FinalObservation{}, fmt.Errorf("%w: materialized final authority is unavailable", ErrPolicy)
	}
	verified, observation, err := options.Prepared.verified.Reverify(ctx)
	if err != nil {
		return materialize.FinalObservation{}, err
	}
	if verified == nil || observation.MetafileVariantID != options.Prepared.plan.MetafileVariantID ||
		observation.TargetRootIdentity != options.Prepared.plan.TargetRootIdentity || observation.FinalObjectIdentity != options.Prepared.plan.FinalObjectIdentity ||
		observation.OperationID != options.Prepared.plan.MaterializeOperationID || observation.MaterializePlanID != options.Prepared.plan.MaterializePlanID {
		return materialize.FinalObservation{}, fmt.Errorf("%w: materialized final differs from the reviewed plan", ErrIntegrity)
	}
	if report != nil {
		report.Final.PostAction = &observation
	}
	return observation, nil
}

func readIdentityLedger(ctx context.Context, session downloader.LedgerSession, prepared *PreparedPlan) (downloaderAssessment, error) {
	result := downloaderAssessment{status: downloader.LedgerIdentityIncomplete}
	if session == nil || prepared == nil {
		return result, fmt.Errorf("downloader ledger authority is unavailable")
	}
	beforeRequests := session.RequestsMade()
	snapshot, err := session.ReadLedger(ctx)
	afterRequests := session.RequestsMade()
	if err != nil {
		return result, err
	}
	if afterRequests-beforeRequests != 1 {
		return result, fmt.Errorf("%w: downloader ledger request count is invalid", ErrIntegrity)
	}
	if snapshot.Driver != prepared.plan.Driver || !snapshot.Capabilities.TypedInfoHashes || !snapshot.Capabilities.ContentPath {
		return result, fmt.Errorf("%w: downloader ledger lacks the reviewed typed-identity or content-path capability", ErrPolicy)
	}
	if err := downloader.ValidateLedgerDriverClaims(snapshot); err != nil {
		return result, fmt.Errorf("%w: downloader ledger claim provenance is invalid", ErrIntegrity)
	}
	assessment, err := downloader.AssessLedgerIdentity(snapshot, prepared.typedIdentity())
	result = downloaderAssessment{status: assessment.Status, started: snapshot.ObservedAtStart, ended: snapshot.ObservedAtEnd, result: assessment}
	if prepared.removal != nil && snapshot.ObservedAtStart.Before(prepared.removal.ObservedAtEnd) {
		return result, fmt.Errorf("%w: downloader absence observation predates the attributed terminal removal", ErrIntegrity)
	}
	return result, err
}

func validateFreshLedgerSession(session downloader.LedgerSession, prepared *PreparedPlan) error {
	if session == nil || prepared == nil {
		return fmt.Errorf("%w: downloader ledger session is unavailable", ErrPolicy)
	}
	descriptor, ok := downloader.DescribeLedgerDriver(prepared.plan.Driver)
	if !ok || session.RequestsMade() != descriptor.OpenRequests {
		return fmt.Errorf("%w: downloader session opening request count is invalid", ErrIntegrity)
	}
	return nil
}

func validateObservedJob(prepared *PreparedPlan, job *downloader.Torrent) (downloader.Torrent, error) {
	if prepared == nil || job == nil {
		return downloader.Torrent{}, fmt.Errorf("%w: unique exact downloader job is unavailable", ErrPolicy)
	}
	wantedContent, ok := prepared.contentPath()
	if !ok || job.ContentPath == "" {
		return downloader.Torrent{}, fmt.Errorf("%w: downloader content path is unavailable", ErrPolicy)
	}
	equal, err := reconcile.EqualClientPaths(wantedContent, job.ContentPath, prepared.windows)
	if err != nil || !equal {
		return downloader.Torrent{}, fmt.Errorf("%w: downloader content path differs from the reviewed materialized final", ErrPolicy)
	}
	wantedSave, _ := prepared.savePath()
	equal, err = reconcile.EqualClientPaths(wantedSave, job.SavePath, prepared.windows)
	if err != nil || !equal {
		return downloader.Torrent{}, fmt.Errorf("%w: downloader save path differs from the reviewed target root", ErrPolicy)
	}
	if job.SizeBytes != prepared.plan.ContentBytes {
		return downloader.Torrent{}, fmt.Errorf("%w: downloader job size differs from the exact metafile", ErrIntegrity)
	}
	if !stoppedState(job.State) {
		return downloader.Torrent{}, fmt.Errorf("%w: downloader job is not in a stopped state", ErrPolicy)
	}
	return *job, nil
}

func applyBeforeAssessment(report *Report, assessment downloaderAssessment) {
	report.Client.BeforeIdentity = string(assessment.status)
	report.Client.JobsExaminedBefore = assessment.result.JobsExamined
}

func applyAfterAssessment(report *Report, assessment downloaderAssessment) {
	report.Client.AfterIdentity = string(assessment.status)
	report.Client.JobsExaminedAfter = assessment.result.JobsExamined
}

func applyObservedJob(report *Report, prepared *PreparedPlan, job downloader.Torrent) {
	report.Client.AfterIdentity = string(downloader.LedgerIdentityExactUnique)
	report.Client.JobID = jobID(job.Hash)
	report.Client.JobState = job.State
	report.Client.ContentPathRef = report.Plan.ExpectedContentPathRef
	if prepared != nil && prepared.plan.Action == ActionAdoptExistingStopped {
		report.Client.VariantRelation = "existing_job_private_variant_unobservable"
	} else {
		report.Client.VariantRelation = "submitted_exact_variant_client_storage_unobservable"
	}
}

func identityGateError(report *Report, assessment downloaderAssessment, before bool) error {
	where := "after"
	if before {
		where = "before"
	}
	switch assessment.status {
	case downloader.LedgerIdentityExactUnique:
		report.addBlocker("client.exact_job_exists", "an exact typed-infohash job already exists; stopped-add adoption never mutates existing jobs")
		report.Client.Status = "exact_job_exists"
		return fmt.Errorf("%w: exact downloader job already exists", ErrPolicy)
	case downloader.LedgerIdentityAmbiguous:
		report.addBlocker("client.multiple_exact_jobs", "multiple exact typed-infohash jobs make adoption ambiguous")
	case downloader.LedgerIdentityConflict:
		report.addBlocker("client.typed_identity_conflict", "a downloader job conflicts across required typed infohash families")
	case downloader.LedgerIdentityIncomplete:
		report.addBlocker("client.typed_identity_incomplete", "the complete queue cannot prove target identity absence or uniqueness")
	case downloader.LedgerIdentityAbsent:
		return nil
	default:
		report.addBlocker("client.snapshot_incomplete", "the downloader ledger identity result is invalid")
	}
	report.Client.Status = where + "_identity_" + string(assessment.status)
	return fmt.Errorf("%w: downloader identity gate is %s", ErrPolicy, assessment.status)
}

func requireExactIdentity(report *Report, assessment downloaderAssessment) error {
	if assessment.status == downloader.LedgerIdentityExactUnique {
		return nil
	}
	if assessment.status == downloader.LedgerIdentityAbsent {
		report.addBlocker("client.exact_job_absent", "the complete current queue does not contain the journaled exact typed-infohash job")
		report.Client.Status = "exact_job_absent"
		return fmt.Errorf("%w: exact downloader job is absent", ErrPolicy)
	}
	return identityGateError(report, assessment, false)
}

func validatePrepared(prepared *PreparedPlan, expectedPlanID string, payload *metastore.ArtifactPayload) error {
	if prepared == nil || prepared.verified == nil || !prepared.verified.Verified() || prepared.plan.Validate() != nil ||
		prepared.planID == "" || prepared.operation == "" {
		return fmt.Errorf("%w: prepared adoption plan is invalid", ErrPolicy)
	}
	computed, err := PlanID(prepared.plan)
	if err != nil || computed != prepared.planID || OperationIDForPlan(computed) != prepared.operation {
		return fmt.Errorf("%w: prepared adoption plan identity differs", ErrIntegrity)
	}
	if expectedPlanID != "" && expectedPlanID != prepared.planID {
		return fmt.Errorf("%w: reviewed adoption plan ID differs", ErrPolicy)
	}
	hasPrior := prepared.plan.PriorAdoptionOperationID != ""
	if hasPrior != (prepared.prior != nil) {
		return fmt.Errorf("%w: prepared prior adoption authority is unavailable", ErrPolicy)
	}
	if hasPrior {
		if !prepared.prior.Verified() {
			return fmt.Errorf("%w: prepared prior adoption authority is invalid", ErrPolicy)
		}
		observation := prepared.prior.Observation()
		if observation.OperationID != prepared.plan.PriorAdoptionOperationID || observation.PlanID != prepared.plan.PriorAdoptionPlanID ||
			observation.CompletionID != prepared.plan.PriorAdoptionCompletionID {
			return fmt.Errorf("%w: prepared prior adoption authority differs from the reviewed lineage", ErrIntegrity)
		}
	}
	hasRemoval := prepared.plan.TerminalRemoval != nil
	if hasRemoval != (prepared.removal != nil && prepared.removalProof != nil) {
		return fmt.Errorf("%w: prepared terminal-removal authority is unavailable", ErrPolicy)
	}
	if hasRemoval {
		value, ok := prepared.removalProof.AdoptionReAddPrerequisite()
		if !ok || value.validate() != nil || value != *prepared.removal || value.planLink() != *prepared.plan.TerminalRemoval {
			return fmt.Errorf("%w: prepared terminal-removal authority differs from the reviewed lineage", ErrIntegrity)
		}
	}
	if prepared.plan.Action == ActionAdoptExistingStopped && payload != nil {
		return fmt.Errorf("%w: observation-only adoption cannot consume raw metafile payload authority", ErrPolicy)
	}
	if payload != nil {
		return validateMetafilePayload(prepared, payload)
	}
	return nil
}

func validateMetafilePayload(prepared *PreparedPlan, payload *metastore.ArtifactPayload) error {
	if prepared == nil || payload == nil {
		return fmt.Errorf("%w: exact private metafile payload is unavailable", ErrPolicy)
	}
	if payload.VariantID() != prepared.plan.MetafileVariantID || payload.SizeBytes() != prepared.plan.MetafileBytes {
		return fmt.Errorf("%w: private metafile payload differs from the reviewed plan", ErrIntegrity)
	}
	return nil
}

func refreshPreparedFinal(ctx context.Context, prepared *PreparedPlan, report *Report) error {
	if prepared == nil || prepared.verified == nil || !prepared.verified.Verified() {
		return fmt.Errorf("%w: current materialized final authority is unavailable", ErrPolicy)
	}
	fresh, observation, err := prepared.verified.Reverify(ctx)
	if err != nil {
		return err
	}
	plan := prepared.plan
	if fresh == nil || !fresh.Verified() || observation.OperationID != plan.MaterializeOperationID ||
		observation.MaterializePlanID != plan.MaterializePlanID || observation.MetafileVariantID != plan.MetafileVariantID ||
		observation.MetafileBytes != plan.MetafileBytes || observation.InfoHashV1 != plan.InfoHashV1 || observation.InfoHashV2 != plan.InfoHashV2 ||
		observation.TargetRootIdentity != plan.TargetRootIdentity || observation.FinalObjectIdentity != plan.FinalObjectIdentity ||
		observation.MultiFile != plan.MultiFile || observation.ManifestFiles != plan.ManifestFiles || observation.ContentBytes != plan.ContentBytes ||
		observation.BytesVerified != plan.ContentBytes {
		return fmt.Errorf("%w: current materialized final differs from the reviewed adoption plan", ErrIntegrity)
	}
	if report != nil {
		report.Final.Status = "verified_current"
		report.Final.Observation = observation
	}
	return nil
}

func validSuccessfulMutation(receipt downloader.MutationReceipt, expectedBytes int64) bool {
	return receipt.Effect == "submit_exact_metafile_stopped" && receipt.Complete && receipt.RequestsAttempted == 1 &&
		receipt.AutomaticRetries == 0 && receipt.RedirectsFollowed == 0 && receipt.BytesSubmittedKnown &&
		receipt.BytesSubmitted == expectedBytes && receipt.StopReason == "" && !receipt.ObservedAtStart.IsZero() &&
		!receipt.ObservedAtEnd.Before(receipt.ObservedAtStart)
}

func safeMutationReceipt(value downloader.MutationReceipt, expectedBytes int64) downloader.MutationReceipt {
	result := downloader.MutationReceipt{
		Effect: "submit_exact_metafile_stopped", RequestsAttempted: -1,
		AutomaticRetries: -1, RedirectsFollowed: -1,
	}
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
	if value.BytesSubmitted >= 0 && value.BytesSubmitted <= 32<<20 {
		result.BytesSubmitted = value.BytesSubmitted
		result.BytesSubmittedKnown = value.BytesSubmittedKnown
	}
	result.Complete = validSuccessfulMutation(value, expectedBytes)
	switch value.StopReason {
	case "context_cancelled", "payload_invalid", "save_path_invalid", "payload_unavailable", "request_build_failed", "session_unavailable", "transport_failed", "response_read_failed", "http_rejected", "response_invalid", "csrf_expired", "already_exists":
		result.StopReason = value.StopReason
	case "":
	default:
		result.StopReason = "client_mutation_failed"
	}
	return result
}

func journalReport(state journalState) JournalReport {
	status := phaseForState(state)
	return JournalReport{
		Status: status, IntentDurable: state.IntentID != "" && state.Durable, AttemptsRecorded: len(state.Attempts),
		CompletionDurable: state.Completion != nil && state.CompletionID != "" && state.Durable, PendingRecoveryMarker: state.Pending != "",
		RetentionState: retentionStateLabel(state), RetentionIntentPresent: state.Retained && state.RetentionIntentDurable,
		RetentionCompletionPresent: state.Retained && state.RetentionComplete && state.RetentionCompleteID != "",
		// A read-only tombstone observation cannot reconstruct a historical
		// directory-fsync result. Only the effectful prune receipt reports
		// retention-marker durability.
		RetentionIntentDurable: false, RetentionCompletionDurable: false,
	}
}

func phaseForState(state journalState) string {
	switch {
	case state.Retained && state.RetentionComplete:
		return "retained_adoption_completion"
	case state.Retained && state.RetentionIntentDurable:
		return "retention_intent_recorded"
	case state.Retained:
		return "retention_initializing"
	case state.Pending != "":
		return "marker_recovery_required"
	case state.Completion != nil:
		return "adopted_pending_client_recheck"
	case len(state.Attempts) > 0:
		if state.Intent.Plan.Action == ActionAdoptExistingStopped {
			return "existing_job_observation_incomplete"
		}
		return "request_result_unknown"
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
	report.Operation.Resumable = false
	report.Operation.PhaseBefore = phaseForState(state)
	report.Operation.PhaseAfter = report.Operation.PhaseBefore
	report.Journal = journalReport(state)
	report.Effect = append(report.Effect, "read_private_client_adoption_retention_state")
	if state.RetentionIntentID != "" {
		plan := state.Intent.Plan
		expected := report.Plan.ExpectedID
		report.Plan = planReport(plan, state.Intent.PlanID, expected)
		applyHistoricalTerminalRemoval(report, plan)
		report.Final.Observation = historicalFinalObservation(plan)
	}
	switch {
	case state.RetentionComplete:
		report.Outcome = OutcomeHistoricalAdopted
		report.Operation.Status = "retained"
		report.Client.Status = "historical_adoption_tombstone_current_client_not_observed"
		report.Warnings = append(report.Warnings, "the retained tombstone can authorize downstream proof but cannot establish current downloader state")
		report.Warnings = append(report.Warnings, "read-only retained status observes canonical markers but does not refresh their directory durability")
	case state.RetentionIntentDurable:
		report.Outcome = OutcomeIncomplete
		report.Operation.Status = "pruning"
		report.Client.Status = "historical_adoption_retention_incomplete"
		report.addBlocker("operation.prune_required", "a durable retention intent exists; only explicit client adopt prune may complete deletion")
	default:
		report.Outcome = OutcomeIncomplete
		report.Operation.Status = "retention_initializing"
		report.Client.Status = "historical_adoption_retention_unsealed"
		report.addBlocker("operation.prune_required", "a reserved retention boundary exists; only explicit client adopt prune may recover it")
	}
}

func historicalFinalObservation(plan Plan) materialize.FinalObservation {
	return materialize.FinalObservation{
		OperationID: plan.MaterializeOperationID, MaterializePlanID: plan.MaterializePlanID,
		MetafileVariantID: plan.MetafileVariantID, MetafileBytes: plan.MetafileBytes,
		InfoHashV1: plan.InfoHashV1, InfoHashV2: plan.InfoHashV2,
		TargetRootIdentity: plan.TargetRootIdentity, FinalObjectIdentity: plan.FinalObjectIdentity,
		MultiFile: plan.MultiFile, ManifestFiles: plan.ManifestFiles, ContentBytes: plan.ContentBytes,
		AuthorityBasis: "historical_adoption_intent", Assurance: "current_final_not_observed",
	}
}

func jobID(opaque string) string {
	digest := sha256.Sum256([]byte("ptctl-downloader-job-v1\x00" + opaque))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func classifyFailure(report *Report, err error) {
	if report == nil || err == nil {
		return
	}
	switch {
	case errors.Is(err, ErrIntegrity), errors.Is(err, materialize.ErrIntegrity):
		report.Outcome = OutcomeIntegrityFailed
		report.Operation.Resumable = false
		report.addIssue("integrity.verification_failed", "a private journal, target identity, or exact content invariant failed")
	case errors.Is(err, ErrOperationNotFound):
		report.Outcome = OutcomeBlocked
		report.Operation.Status = "not_found"
		report.Operation.Resumable = false
		report.addBlocker("operation.not_found", "the explicit client adoption operation does not exist")
	case errors.Is(err, ErrPolicy), errors.Is(err, materialize.ErrPolicy), errors.Is(err, fsbind.ErrAlreadyExists):
		report.Outcome = OutcomeBlocked
	case errors.Is(err, ErrRequestUnknown):
		report.Outcome = OutcomeRequestUnknown
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		report.Outcome = OutcomeIncomplete
		report.addIssue("operation.interrupted", "the operation was interrupted before a current result could be established")
	default:
		report.Outcome = OutcomeIncomplete
		report.addIssue("operation.incomplete", "the operation could not establish a complete result")
	}
}
