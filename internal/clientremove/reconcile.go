package clientremove

import (
	"time"

	"github.com/tonycoder-hub/ptctl/internal/reconcile"
)

// ReconciliationRemovalCompletion exposes a bounded public view only while
// the process-local terminal removal authority remains valid.
func (verified *VerifiedCompletion) ReconciliationRemovalCompletion() (reconcile.ClientRemovalCompletion, bool) {
	if !verified.Verified() {
		return reconcile.ClientRemovalCompletion{}, false
	}
	value := verified.Observation()
	started, startErr := time.Parse(time.RFC3339Nano, value.ObservedAtStart)
	ended, endErr := time.Parse(time.RFC3339Nano, value.ObservedAtEnd)
	if startErr != nil || endErr != nil || started.IsZero() || ended.Before(started) {
		return reconcile.ClientRemovalCompletion{}, false
	}
	return reconcile.ClientRemovalCompletion{
		Driver: value.Driver, OperationID: value.OperationID, PlanID: value.PlanID, IntentID: value.IntentID,
		CompletionID: value.CompletionID, CompletionBasis: value.CompletionBasis, UseID: value.UseID, JobID: value.JobID,
		FileLayoutID: value.FileLayoutID, CompleteSnapshotID: value.CompleteFileSnapshotID,
		ClientConfigID: value.ClientConfigID, PathMappingID: value.PathMappingID,
		ActivationOperationID: value.ActivationOperationID, ActivationPlanID: value.ActivationPlanID,
		ActivationTerminalID: value.ActivationTerminalID, MetafileVariantID: value.MetafileVariantID,
		MaterializeOperationID: value.MaterializeOperationID, MaterializePlanID: value.MaterializePlanID,
		TargetRootIdentity: value.TargetRootIdentity, FinalObjectIdentity: value.FinalObjectIdentity,
		MultiFile: value.MultiFile, ManifestFiles: value.ManifestFiles, ContentBytes: value.ContentBytes,
		ObservedAtStart: started, ObservedAtEnd: ended, JobsExamined: value.JobsExamined,
		RetainedTombstone: value.RetainedTombstone, Assurance: value.Assurance,
	}, true
}
