package clientstop

import (
	"context"
	"fmt"
	"time"
)

// CompletionProofOptions selects one explicit terminal client-stop operation.
// Reading the live journal or retained tombstone is local and read-only; it
// never contacts the downloader or refreshes historical durability evidence.
type CompletionProofOptions struct {
	TargetRoot     string
	OperationID    OperationID
	ExpectedPlanID string
}

// CompletionObservation is a public, non-authoritative view of one canonical
// terminal client stop. It is historical evidence and does not by itself prove
// that the downloader job is still stopped.
type CompletionObservation struct {
	Driver                 string `json:"driver"`
	OperationID            string `json:"operation_id"`
	PlanID                 string `json:"plan_id"`
	IntentID               string `json:"intent_id"`
	CompletionID           string `json:"completion_id"`
	CompletionBasis        string `json:"completion_basis"`
	UseID                  string `json:"use_id"`
	JobID                  string `json:"job_id"`
	FileLayoutID           string `json:"file_layout_id"`
	CompleteFileSnapshotID string `json:"complete_file_snapshot_id"`
	StoppedJobState        string `json:"stopped_job_state"`
	ClientConfigID         string `json:"client_config_id"`
	PathMappingID          string `json:"path_mapping_id"`
	ActivationOperationID  string `json:"activation_operation_id"`
	ActivationPlanID       string `json:"activation_plan_id"`
	ActivationTerminalID   string `json:"activation_terminal_id"`
	MetafileVariantID      string `json:"metafile_variant_id"`
	InfoHashV1             string `json:"info_hash_v1,omitempty"`
	InfoHashV2             string `json:"info_hash_v2,omitempty"`
	MaterializeOperationID string `json:"materialize_operation_id"`
	MaterializePlanID      string `json:"materialize_plan_id"`
	TargetRootIdentity     string `json:"target_root_identity"`
	FinalObjectIdentity    string `json:"final_object_identity"`
	MultiFile              bool   `json:"multi_file"`
	ManifestFiles          int    `json:"manifest_files"`
	ContentBytes           int64  `json:"content_bytes"`
	ObservedAtStart        string `json:"observed_at_start"`
	ObservedAtEnd          string `json:"observed_at_end"`
	RetainedTombstone      bool   `json:"retained_tombstone"`
	Assurance              string `json:"assurance"`
}

type verifiedCompletionAuthority struct {
	intent       Intent
	planID       string
	operationID  OperationID
	intentID     MarkerID
	attempt      Attempt
	attemptID    MarkerID
	response     *Response
	responseID   MarkerID
	completion   Completion
	completionID MarkerID
	retained     bool
}

// VerifiedCompletion is process-local authority that one complete canonical
// client-stop chain was read through the bound target-root journal or exact
// retained tombstone. JSON cannot recreate this authority.
type VerifiedCompletion struct {
	authority *verifiedCompletionAuthority
}

func VerifyCompletion(ctx context.Context, options CompletionProofOptions) (*VerifiedCompletion, CompletionObservation, error) {
	if options.TargetRoot == "" || !canonicalPlanID(options.ExpectedPlanID) ||
		OperationIDForPlan(options.ExpectedPlanID) != options.OperationID {
		return nil, CompletionObservation{}, fmt.Errorf("%w: client stop completion selector is invalid", ErrPolicy)
	}
	if _, err := ParseOperationID(options.OperationID.String()); err != nil {
		return nil, CompletionObservation{}, fmt.Errorf("%w: client stop completion selector is invalid", ErrPolicy)
	}
	if err := ctx.Err(); err != nil {
		return nil, CompletionObservation{}, err
	}
	handle, _, err := openJournal(ctx, options.TargetRoot, options.OperationID, false, nil)
	if err != nil {
		return nil, CompletionObservation{}, err
	}
	defer handle.Close()
	state := handle.state
	if state.Pending != "" || state.Completion == nil || state.CompletionID == "" || state.IntentID == "" ||
		state.Retained && !state.RetentionComplete || state.Intent.OperationID != options.OperationID ||
		state.Intent.PlanID != options.ExpectedPlanID || state.Completion.OperationID != options.OperationID ||
		state.Completion.PlanID != options.ExpectedPlanID {
		return nil, CompletionObservation{}, fmt.Errorf("%w: terminal client stop completion is unavailable or disagrees with the selector", ErrPolicy)
	}
	authority := &verifiedCompletionAuthority{
		intent: state.Intent, planID: state.Intent.PlanID, operationID: state.Intent.OperationID,
		intentID: state.IntentID, completion: *state.Completion, completionID: state.CompletionID, retained: state.Retained,
	}
	for index, id := range state.AttemptIDs {
		if id == state.Completion.AttemptID && index < len(state.Attempts) {
			authority.attempt, authority.attemptID = state.Attempts[index], id
			break
		}
	}
	if state.Completion.ResponseID != "" {
		for sequence, id := range state.ResponseIDs {
			if id.String() == state.Completion.ResponseID {
				value := state.Responses[sequence]
				authority.response, authority.responseID = &value, id
				break
			}
		}
	}
	verified := &VerifiedCompletion{authority: authority}
	if !verified.Verified() {
		return nil, CompletionObservation{}, fmt.Errorf("%w: terminal client stop completion is invalid", ErrIntegrity)
	}
	return verified, verified.Observation(), nil
}

func (verified *VerifiedCompletion) Verified() bool {
	if verified == nil || verified.authority == nil {
		return false
	}
	authority := verified.authority
	plan, completion := authority.intent.Plan, authority.completion
	if authority.intent.Validate() != nil || authority.attempt.Validate() != nil || completion.Validate() != nil ||
		!canonicalPlanID(authority.planID) || OperationIDForPlan(authority.planID) != authority.operationID ||
		authority.intentID == "" || authority.attemptID == "" || authority.completionID == "" {
		return false
	}
	if _, id, err := encodeIntent(authority.intent); err != nil || id != authority.intentID {
		return false
	}
	if _, id, err := encodeAttempt(authority.attempt); err != nil || id != authority.attemptID {
		return false
	}
	if _, id, err := encodeCompletion(authority.completion); err != nil || id != authority.completionID {
		return false
	}
	if completion.ResponseID == "" {
		if authority.response != nil || authority.responseID != "" || completion.Basis == "accepted_response_then_exact_stopped" {
			return false
		}
	} else {
		if authority.response == nil || authority.responseID.String() != completion.ResponseID {
			return false
		}
		if _, id, err := encodeResponse(*authority.response); err != nil || id != authority.responseID ||
			authority.response.AttemptID != authority.attemptID || authority.response.OperationID != authority.operationID ||
			authority.response.PlanID != authority.planID || completion.Basis == "accepted_response_then_exact_stopped" && !authority.response.Complete {
			return false
		}
	}
	return authority.intent.OperationID == authority.operationID && authority.intent.PlanID == authority.planID &&
		completion.OperationID == authority.operationID && completion.PlanID == authority.planID && completion.AttemptID == authority.attemptID &&
		authority.attempt.OperationID == authority.operationID && authority.attempt.PlanID == authority.planID &&
		authority.attempt.UseID == plan.UseID && authority.attempt.JobID == plan.JobID &&
		authority.attempt.FileLayoutID == plan.FileLayoutID && authority.attempt.FileSnapshotID == plan.CompleteFileSnapshotID &&
		completion.UseID == plan.UseID && completion.JobID == plan.JobID && completion.FileLayoutID == plan.FileLayoutID &&
		completion.CompleteFileSnapshotID == plan.CompleteFileSnapshotID && completion.FinalObjectIdentity == plan.FinalObjectIdentity
}

func (verified *VerifiedCompletion) Observation() CompletionObservation {
	if !verified.Verified() {
		return CompletionObservation{}
	}
	authority := verified.authority
	plan, completion := authority.intent.Plan, authority.completion
	assurance := "same_invocation_bound_canonical_terminal_client_stop_journal_read_without_current_client_inference"
	if authority.retained {
		assurance = "same_invocation_bound_canonical_client_stop_retention_tombstone_read_without_current_client_inference"
	}
	return CompletionObservation{
		Driver: plan.Driver, OperationID: authority.operationID.String(), PlanID: authority.planID,
		IntentID: authority.intentID.String(), CompletionID: authority.completionID.String(), CompletionBasis: completion.Basis,
		UseID: plan.UseID, JobID: plan.JobID, FileLayoutID: plan.FileLayoutID,
		CompleteFileSnapshotID: plan.CompleteFileSnapshotID, StoppedJobState: completion.StoppedJobState,
		ClientConfigID: plan.ClientConfigID, PathMappingID: plan.PathMappingID,
		ActivationOperationID: plan.ActivationOperationID, ActivationPlanID: plan.ActivationPlanID,
		ActivationTerminalID: plan.ActivationTerminalID, MetafileVariantID: plan.MetafileVariantID,
		InfoHashV1: plan.InfoHashV1, InfoHashV2: plan.InfoHashV2,
		MaterializeOperationID: plan.MaterializeOperationID, MaterializePlanID: plan.MaterializePlanID,
		TargetRootIdentity: plan.TargetRootIdentity, FinalObjectIdentity: plan.FinalObjectIdentity,
		MultiFile: plan.MultiFile, ManifestFiles: plan.ManifestFiles, ContentBytes: plan.ContentBytes,
		ObservedAtStart:   completion.ObservedAtStart.UTC().Format(time.RFC3339Nano),
		ObservedAtEnd:     completion.ObservedAtEnd.UTC().Format(time.RFC3339Nano),
		RetainedTombstone: authority.retained, Assurance: assurance,
	}
}

func (verified *VerifiedCompletion) Matches(operation OperationID, planID, variantID, materializeOperationID,
	materializePlanID, activationOperationID, activationPlanID, activationTerminalID, finalObjectIdentity string) bool {
	if !verified.Verified() {
		return false
	}
	plan := verified.authority.intent.Plan
	return verified.authority.operationID == operation && verified.authority.planID == planID &&
		plan.MetafileVariantID == variantID && plan.MaterializeOperationID == materializeOperationID &&
		plan.MaterializePlanID == materializePlanID && plan.ActivationOperationID == activationOperationID &&
		plan.ActivationPlanID == activationPlanID && plan.ActivationTerminalID == activationTerminalID &&
		plan.FinalObjectIdentity == finalObjectIdentity
}

func (verified *VerifiedCompletion) Plan() Plan {
	if !verified.Verified() {
		return Plan{}
	}
	return verified.authority.intent.Plan
}
