package sourceretire

import "github.com/tonycoder-hub/ptctl/internal/reconcile"

// ReconciliationRetirementCompletion exposes a bounded public view only while
// the process-local retirement completion authority remains valid.
func (verified *VerifiedCompletion) ReconciliationRetirementCompletion() (reconcile.SourceRetirementCompletion, bool) {
	if !verified.Verified() {
		return reconcile.SourceRetirementCompletion{}, false
	}
	value := verified.Observation()
	return reconcile.SourceRetirementCompletion{
		OperationID: value.OperationID, PlanID: value.PlanID, IntentID: value.IntentID, CompletionID: value.CompletionID,
		SearchScopeID: value.SearchScopeID, MetafileVariantID: value.MetafileVariantID,
		MaterializeOperationID: value.MaterializeOperationID, MaterializePlanID: value.MaterializePlanID,
		ActivationOperationID: value.ActivationOperationID, ActivationPlanID: value.ActivationPlanID,
		ClientCompletionID: value.ClientCompletionID, CurrentClientUseID: value.CurrentClientUseID,
		SourceSelectionID: value.SourceSelectionID, TargetRootIdentity: value.TargetRootIdentity,
		FinalObjectIdentity: value.FinalObjectIdentity, ClientSnapshotID: value.ClientSnapshotID,
		FilesRetired: value.FilesRetired, BytesRetired: value.BytesRetired, RetainedTombstone: value.RetainedTombstone,
		Assurance: value.Assurance,
	}, true
}

// ReconcileCurrentRetiredNameAbsence returns the public view of the already
// completed local absence observation. It performs no filesystem or network
// operation and cannot be recreated from its JSON DTO.
func (verified *VerifiedCurrentAbsence) ReconcileCurrentRetiredNameAbsence() (reconcile.SourceRetirementCurrentAbsence, bool) {
	if !verified.Verified() {
		return reconcile.SourceRetirementCurrentAbsence{}, false
	}
	value := verified.Observation()
	return reconcile.SourceRetirementCurrentAbsence{
		OperationID: value.OperationID, PlanID: value.PlanID, CompletionID: value.CompletionID,
		AbsenceID: value.AbsenceID, SearchScopeID: value.SearchScopeID, FilesChecked: value.FilesChecked,
		BytesRetired: value.BytesRetired, ParentDirectoriesChecked: value.ParentDirectoriesChecked,
		ObservedAtStart: value.ObservedAtStart, ObservedAtEnd: value.ObservedAtEnd, Assurance: value.Assurance,
	}, true
}
