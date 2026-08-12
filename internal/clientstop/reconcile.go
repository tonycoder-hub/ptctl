package clientstop

import (
	"time"

	"github.com/tonycoder-hub/ptctl/internal/reconcile"
)

// ReconciliationStopCompletion exposes a bounded public view only while the
// process-local terminal stop authority remains valid.
func (verified *VerifiedCompletion) ReconciliationStopCompletion() (reconcile.ClientStopCompletion, bool) {
	if !verified.Verified() {
		return reconcile.ClientStopCompletion{}, false
	}
	value := verified.Observation()
	started, startErr := time.Parse(time.RFC3339Nano, value.ObservedAtStart)
	ended, endErr := time.Parse(time.RFC3339Nano, value.ObservedAtEnd)
	if startErr != nil || endErr != nil || started.IsZero() || ended.Before(started) {
		return reconcile.ClientStopCompletion{}, false
	}
	return reconcile.ClientStopCompletion{
		Driver: value.Driver, OperationID: value.OperationID, PlanID: value.PlanID, IntentID: value.IntentID,
		CompletionID: value.CompletionID, CompletionBasis: value.CompletionBasis, UseID: value.UseID, JobID: value.JobID,
		FileLayoutID: value.FileLayoutID, CompleteSnapshotID: value.CompleteFileSnapshotID, StoppedJobState: value.StoppedJobState,
		ClientConfigID: value.ClientConfigID, PathMappingID: value.PathMappingID,
		ActivationOperationID: value.ActivationOperationID, ActivationPlanID: value.ActivationPlanID,
		ActivationTerminalID: value.ActivationTerminalID, MetafileVariantID: value.MetafileVariantID,
		InfoHashV1: value.InfoHashV1, InfoHashV2: value.InfoHashV2,
		MaterializeOperationID: value.MaterializeOperationID, MaterializePlanID: value.MaterializePlanID,
		TargetRootIdentity: value.TargetRootIdentity, FinalObjectIdentity: value.FinalObjectIdentity,
		MultiFile: value.MultiFile, ManifestFiles: value.ManifestFiles, ContentBytes: value.ContentBytes,
		ObservedAtStart: started, ObservedAtEnd: ended, RetainedTombstone: value.RetainedTombstone, Assurance: value.Assurance,
	}, true
}

// ReconcileCurrentStopped binds the historical stop authority to an existing
// same-invocation activation current-use proof. It performs no request and
// does not claim that the downloader exposes a stable job incarnation.
func (verified *VerifiedCompletion) ReconcileCurrentStopped(current reconcile.ClientActivationCurrentUse) (reconcile.ClientStopCurrentJob, bool) {
	if !verified.Verified() || current.Driver == "" {
		return reconcile.ClientStopCurrentJob{}, false
	}
	plan, completion := verified.authority.intent.Plan, verified.authority.completion
	if current.Driver != plan.Driver || current.UseID != plan.UseID || current.JobID != plan.JobID ||
		current.FileLayoutID != plan.FileLayoutID || current.CompleteSnapshotID != plan.CompleteFileSnapshotID ||
		current.FinalObjectIdentity != plan.FinalObjectIdentity || current.JobProgress != 1 ||
		!completeStoppedState(current.JobState) || current.ObservedAtStart.Before(completion.ObservedAtEnd) {
		return reconcile.ClientStopCurrentJob{}, false
	}
	return reconcile.ClientStopCurrentJob{
		Driver: current.Driver, UseID: current.UseID, JobID: current.JobID, FileLayoutID: current.FileLayoutID,
		CompleteSnapshotID: current.CompleteSnapshotID, JobState: current.JobState, JobProgress: current.JobProgress,
		ObservedAtStart: current.ObservedAtStart, ObservedAtEnd: current.ObservedAtEnd,
		FinalObjectIdentity: current.FinalObjectIdentity,
		Assurance:           "same_invocation_existing_reconciliation_bracket_bound_to_canonical_terminal_client_stop_and_exact_final_with_current_stopped_typed_job_claim_non_atomic_without_job_incarnation_proof",
	}, true
}
