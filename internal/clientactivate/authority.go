package clientactivate

import (
	"context"
	"fmt"
	"time"
)

// CompletionProofOptions selects one explicit activation journal. Reading the
// journal is local and read-only; it does not contact the downloader or refresh
// the historical directory-durability observation.
type CompletionProofOptions struct {
	TargetRoot     string
	OperationID    OperationID
	ExpectedPlanID string
}

// CompletionObservation is the public, non-authoritative description of one
// canonical terminal activation journal read. It is historical client evidence
// and never substitutes for a current materialized-final proof.
type CompletionObservation struct {
	OperationID            string `json:"operation_id"`
	PlanID                 string `json:"plan_id"`
	TerminalMarkerID       string `json:"terminal_marker_id"`
	Action                 string `json:"action"`
	TerminalPhase          string `json:"terminal_phase"`
	MetafileVariantID      string `json:"metafile_variant_id"`
	MaterializeOperationID string `json:"materialize_operation_id"`
	MaterializePlanID      string `json:"materialize_plan_id"`
	ClientConfigID         string `json:"client_config_id"`
	PathMappingID          string `json:"path_mapping_id"`
	JobID                  string `json:"job_id"`
	JobState               string `json:"job_state"`
	ObservedAtStart        string `json:"observed_at_start"`
	ObservedAtEnd          string `json:"observed_at_end"`
	RecheckCompletionID    string `json:"recheck_completion_id"`
	ActivationCompletionID string `json:"activation_completion_id,omitempty"`
	FinalObjectIdentity    string `json:"final_object_identity"`
	FinalVerificationBasis string `json:"final_verification_basis"`
	Assurance              string `json:"assurance"`
}

// VerifiedCompletion is process-local authority that the complete, canonical
// marker chain for the activation plan's reviewed terminal action was read from
// the bound target-root journal in this invocation. Public JSON cannot recreate
// this authority. The observation remains historical and non-atomic.
type VerifiedCompletion struct {
	authority *verifiedCompletionAuthority
}

type verifiedCompletionAuthority struct {
	plan         Plan
	planID       string
	operationID  OperationID
	recheck      RecheckCompletion
	recheckID    MarkerID
	activation   *ActivationCompletion
	activationID MarkerID
	retained     bool
}

// VerifyCompletion requires the terminal marker selected by the reviewed plan:
// recheck completion for recheck-only, and activation completion when start was
// reviewed. An intermediate checked state is deliberately not terminal for a
// recheck-then-start plan.
func VerifyCompletion(ctx context.Context, options CompletionProofOptions) (*VerifiedCompletion, CompletionObservation, error) {
	if options.TargetRoot == "" || !canonicalPlanID(options.ExpectedPlanID) {
		return nil, CompletionObservation{}, fmt.Errorf("%w: activation completion selector is invalid", ErrPolicy)
	}
	if _, err := ParseOperationID(options.OperationID.String()); err != nil || OperationIDForPlan(options.ExpectedPlanID) != options.OperationID {
		return nil, CompletionObservation{}, fmt.Errorf("%w: activation completion selector is invalid", ErrPolicy)
	}
	handle, _, err := openJournal(ctx, options.TargetRoot, options.OperationID, false, nil)
	if err != nil {
		return nil, CompletionObservation{}, err
	}
	defer handle.Close()
	state := handle.state
	if state.Pending != "" || state.RecheckCompletion == nil || state.RecheckCompletionID == "" ||
		state.Retained && !state.RetentionComplete ||
		state.Intent.PlanID != options.ExpectedPlanID || state.Intent.OperationID != options.OperationID ||
		state.RecheckCompletion.PlanID != options.ExpectedPlanID || state.RecheckCompletion.OperationID != options.OperationID {
		return nil, CompletionObservation{}, fmt.Errorf("%w: terminal activation completion is unavailable or disagrees with the selector", ErrPolicy)
	}
	if state.Intent.Plan.Action == ActionRecheckThenStart {
		if state.ActivationCompletion == nil || state.ActivationCompletionID == "" ||
			state.ActivationCompletion.PlanID != options.ExpectedPlanID || state.ActivationCompletion.OperationID != options.OperationID {
			return nil, CompletionObservation{}, fmt.Errorf("%w: the reviewed start transition has no terminal completion", ErrPolicy)
		}
	} else if state.Intent.Plan.Action != ActionRecheckOnly {
		return nil, CompletionObservation{}, fmt.Errorf("%w: activation completion action is unsupported", ErrIntegrity)
	}
	authority := &verifiedCompletionAuthority{
		plan: state.Intent.Plan, planID: state.Intent.PlanID, operationID: state.Intent.OperationID,
		recheck: *state.RecheckCompletion, recheckID: state.RecheckCompletionID, retained: state.Retained,
	}
	if state.ActivationCompletion != nil {
		value := *state.ActivationCompletion
		authority.activation, authority.activationID = &value, state.ActivationCompletionID
	}
	verified := &VerifiedCompletion{authority: authority}
	if !verified.Verified() {
		return nil, CompletionObservation{}, fmt.Errorf("%w: terminal activation completion is invalid", ErrIntegrity)
	}
	return verified, verified.Observation(), nil
}

func (verified *VerifiedCompletion) Verified() bool {
	if verified == nil || verified.authority == nil {
		return false
	}
	authority := verified.authority
	if authority.plan.Validate() != nil || authority.recheck.Validate() != nil || authority.recheckID == "" {
		return false
	}
	if authority.plan.Action == ActionRecheckOnly {
		return authority.activation == nil && authority.activationID == ""
	}
	return authority.plan.Action == ActionRecheckThenStart && authority.activation != nil &&
		authority.activation.Validate() == nil && authority.activationID != ""
}

func (verified *VerifiedCompletion) Observation() CompletionObservation {
	if !verified.Verified() {
		return CompletionObservation{}
	}
	authority := verified.authority
	plan, completion := authority.plan, authority.recheck
	markerID, phase := authority.recheckID.String(), "recheck_complete_stopped"
	jobState, finalBasis := completion.JobState, completion.FinalVerificationBasis
	observedAtStart, observedAtEnd := completion.ObservedAtStart, completion.ObservedAtEnd
	activationID := ""
	if authority.activation != nil {
		markerID, phase = authority.activationID.String(), "started_client_claim_observed"
		activationID = authority.activationID.String()
		jobState, finalBasis = authority.activation.JobState, authority.activation.FinalVerificationBasis
		observedAtStart, observedAtEnd = authority.activation.ObservedAtStart, authority.activation.ObservedAtEnd
	}
	return CompletionObservation{
		OperationID: authority.operationID.String(), PlanID: authority.planID, TerminalMarkerID: markerID,
		Action: plan.Action, TerminalPhase: phase, MetafileVariantID: plan.MetafileVariantID,
		MaterializeOperationID: plan.MaterializeOperationID, MaterializePlanID: plan.MaterializePlanID,
		ClientConfigID: plan.ClientConfigID, PathMappingID: plan.PathMappingID, JobID: plan.JobID,
		JobState: jobState, ObservedAtStart: observedAtStart.UTC().Format(time.RFC3339Nano),
		ObservedAtEnd:       observedAtEnd.UTC().Format(time.RFC3339Nano),
		RecheckCompletionID: authority.recheckID.String(), ActivationCompletionID: activationID,
		FinalObjectIdentity: plan.FinalObjectIdentity, FinalVerificationBasis: finalBasis,
		Assurance: completionAssurance(authority),
	}
}

func completionAssurance(authority *verifiedCompletionAuthority) string {
	if authority != nil && authority.retained {
		return "same_invocation_bound_canonical_activation_retention_tombstone_read_without_durability_or_client_refresh"
	}
	return "same_invocation_bound_canonical_terminal_activation_read_without_durability_or_client_refresh"
}

func (verified *VerifiedCompletion) Matches(operation OperationID, planID, variantID, materializeOperationID, materializePlanID, finalObjectIdentity string) bool {
	if !verified.Verified() {
		return false
	}
	authority := verified.authority
	return authority.operationID == operation && authority.planID == planID && authority.plan.MetafileVariantID == variantID &&
		authority.plan.MaterializeOperationID == materializeOperationID && authority.plan.MaterializePlanID == materializePlanID &&
		authority.plan.FinalObjectIdentity == finalObjectIdentity
}

func (verified *VerifiedCompletion) Plan() Plan {
	if !verified.Verified() {
		return Plan{}
	}
	return verified.authority.plan
}
