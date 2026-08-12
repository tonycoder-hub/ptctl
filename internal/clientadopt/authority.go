package clientadopt

import (
	"context"
	"fmt"
	"time"
)

type CompletionProofOptions struct {
	TargetRoot     string
	OperationID    OperationID
	ExpectedPlanID string
}

type CompletionObservation struct {
	Driver                 string `json:"driver"`
	Action                 string `json:"action"`
	OperationID            string `json:"operation_id"`
	PlanID                 string `json:"plan_id"`
	CompletionID           string `json:"completion_id"`
	MetafileVariantID      string `json:"metafile_variant_id"`
	MetafileBytes          int64  `json:"metafile_bytes"`
	InfoHashV1             string `json:"info_hash_v1,omitempty"`
	InfoHashV2             string `json:"info_hash_v2,omitempty"`
	MaterializeOperationID string `json:"materialize_operation_id"`
	MaterializePlanID      string `json:"materialize_plan_id"`
	ClientConfigID         string `json:"client_config_id"`
	PathMappingID          string `json:"path_mapping_id"`
	ClientPathSemantics    string `json:"client_path_semantics"`
	ExpectedSavePathRef    string `json:"expected_save_path_ref"`
	ExpectedContentPathRef string `json:"expected_content_path_ref"`
	JobID                  string `json:"job_id"`
	JobState               string `json:"job_state"`
	TargetRootIdentity     string `json:"target_root_identity"`
	FinalObjectIdentity    string `json:"final_object_identity"`
	MultiFile              bool   `json:"multi_file"`
	ManifestFiles          int    `json:"manifest_files"`
	ContentBytes           int64  `json:"content_bytes"`
	ObservedAtStart        string `json:"observed_at_start"`
	ObservedAtEnd          string `json:"observed_at_end"`
	RetainedTombstone      bool   `json:"retained_tombstone"`
	RemovalOperationID     string `json:"removal_operation_id,omitempty"`
	RemovalPlanID          string `json:"removal_plan_id,omitempty"`
	RemovalCompletionID    string `json:"removal_completion_id,omitempty"`
	RemovalCompletionBasis string `json:"removal_completion_basis,omitempty"`
	Assurance              string `json:"assurance"`
}

// VerifiedCompletion is process-local authority that one canonical adoption
// completion marker was read through the bound target-root journal in this
// invocation. It does not imply that the historical directory fsync was
// refreshed or that current downloader state still matches the marker.
type VerifiedCompletion struct {
	authority *verifiedCompletionAuthority
}

type verifiedCompletionAuthority struct {
	plan         Plan
	planID       string
	operationID  OperationID
	attempt      Attempt
	completion   Completion
	completionID MarkerID
	retained     bool
}

func VerifyCompletion(ctx context.Context, options CompletionProofOptions) (*VerifiedCompletion, CompletionObservation, error) {
	if options.TargetRoot == "" || !canonicalPlanID(options.ExpectedPlanID) ||
		OperationIDForPlan(options.ExpectedPlanID) != options.OperationID {
		return nil, CompletionObservation{}, fmt.Errorf("%w: adoption completion selector is invalid", ErrPolicy)
	}
	if _, err := ParseOperationID(options.OperationID.String()); err != nil {
		return nil, CompletionObservation{}, fmt.Errorf("%w: adoption completion selector is invalid", ErrPolicy)
	}
	handle, _, err := openJournal(ctx, options.TargetRoot, options.OperationID, false, nil)
	if err != nil {
		return nil, CompletionObservation{}, err
	}
	defer handle.Close()
	state := handle.state
	if state.Pending != "" || state.Completion == nil || state.CompletionID == "" || len(state.Attempts) == 0 ||
		state.Retained && !state.RetentionComplete ||
		state.Intent.PlanID != options.ExpectedPlanID || state.Intent.OperationID != options.OperationID ||
		state.Completion.PlanID != options.ExpectedPlanID || state.Completion.OperationID != options.OperationID {
		return nil, CompletionObservation{}, fmt.Errorf("%w: adoption completion is unavailable or disagrees with the selector", ErrPolicy)
	}
	authority := &verifiedCompletionAuthority{
		plan: state.Intent.Plan, planID: state.Intent.PlanID, operationID: state.Intent.OperationID,
		attempt:    state.Attempts[len(state.Attempts)-1],
		completion: *state.Completion, completionID: state.CompletionID, retained: state.Retained,
	}
	verified := &VerifiedCompletion{authority: authority}
	return verified, verified.Observation(), nil
}

func (verified *VerifiedCompletion) Verified() bool {
	if verified == nil || verified.authority == nil {
		return false
	}
	authority := verified.authority
	plan, completion := authority.plan, authority.completion
	if plan.Validate() != nil || completion.Validate() != nil || authority.completionID == "" ||
		!canonicalPlanID(authority.planID) || OperationIDForPlan(authority.planID) != authority.operationID ||
		authority.attempt.OperationID != authority.operationID || authority.attempt.PlanID != authority.planID ||
		completion.OperationID != authority.operationID || completion.PlanID != authority.planID ||
		completion.ContentPathRef != plan.ExpectedContentPathRef || completion.FinalObjectIdentity != plan.FinalObjectIdentity {
		return false
	}
	if !completionMatchesPlan(completion, plan, authority.attempt) {
		return false
	}
	if computed, err := PlanID(plan); err != nil || computed != authority.planID {
		return false
	}
	if _, computed, err := encodeCompletion(completion); err != nil || computed != authority.completionID {
		return false
	}
	return true
}

func (verified *VerifiedCompletion) Observation() CompletionObservation {
	if !verified.Verified() {
		return CompletionObservation{}
	}
	authority := verified.authority
	plan, completion := authority.plan, authority.completion
	observation := CompletionObservation{
		Driver: plan.Driver, Action: plan.Action, OperationID: authority.operationID.String(), PlanID: authority.planID, CompletionID: authority.completionID.String(),
		MetafileVariantID: plan.MetafileVariantID, MetafileBytes: plan.MetafileBytes,
		InfoHashV1: plan.InfoHashV1, InfoHashV2: plan.InfoHashV2,
		MaterializeOperationID: plan.MaterializeOperationID, MaterializePlanID: plan.MaterializePlanID,
		ClientConfigID: plan.ClientConfigID, PathMappingID: plan.PathMappingID,
		ClientPathSemantics: plan.ClientPathSemantics, ExpectedSavePathRef: plan.ExpectedSavePathRef,
		ExpectedContentPathRef: plan.ExpectedContentPathRef, JobID: completion.JobID, JobState: completion.JobState,
		TargetRootIdentity: plan.TargetRootIdentity, FinalObjectIdentity: completion.FinalObjectIdentity,
		MultiFile: plan.MultiFile, ManifestFiles: plan.ManifestFiles, ContentBytes: plan.ContentBytes,
		ObservedAtStart:   completion.ObservedAtStart.UTC().Format(time.RFC3339Nano),
		ObservedAtEnd:     completion.ObservedAtEnd.UTC().Format(time.RFC3339Nano),
		RetainedTombstone: authority.retained, Assurance: completionAssurance(authority),
	}
	if plan.TerminalRemoval != nil {
		observation.RemovalOperationID = plan.TerminalRemoval.OperationID
		observation.RemovalPlanID = plan.TerminalRemoval.PlanID
		observation.RemovalCompletionID = plan.TerminalRemoval.CompletionID
		observation.RemovalCompletionBasis = plan.TerminalRemoval.CompletionBasis
	}
	return observation
}

func completionAssurance(authority *verifiedCompletionAuthority) string {
	if authority != nil && authority.plan.Action == ActionAdoptExistingStopped && authority.retained {
		return "same_invocation_bound_canonical_existing_stopped_adoption_retention_tombstone_read_without_durability_refresh_or_current_client_observation"
	}
	if authority != nil && authority.plan.Action == ActionAdoptExistingStopped {
		return "same_invocation_bound_canonical_existing_stopped_adoption_completion_read_without_durability_refresh"
	}
	if authority != nil && authority.retained {
		return "same_invocation_bound_canonical_adoption_retention_tombstone_read_without_durability_refresh_or_current_client_observation"
	}
	return "same_invocation_bound_canonical_adoption_completion_read_without_durability_refresh"
}

func (verified *VerifiedCompletion) Matches(operation OperationID, planID, variantID, clientConfigID, pathMappingID string) bool {
	if !verified.Verified() {
		return false
	}
	authority := verified.authority
	return authority.operationID == operation && authority.planID == planID &&
		authority.plan.MetafileVariantID == variantID && authority.plan.ClientConfigID == clientConfigID &&
		authority.plan.PathMappingID == pathMappingID
}

func (verified *VerifiedCompletion) Plan() Plan {
	if !verified.Verified() {
		return Plan{}
	}
	return verified.authority.plan
}
