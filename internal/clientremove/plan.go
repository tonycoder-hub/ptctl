package clientremove

import (
	"fmt"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

type PreparedPlan struct {
	plan    Plan
	planID  string
	current *clientactivate.VerifiedCurrentUse
	request downloader.ExistingJobMutationRequest
}

func BuildPlan(current *clientactivate.VerifiedCurrentUse, descriptor downloader.ExistingJobRemovalDescriptor) (*PreparedPlan, error) {
	if current == nil || !current.Verified() || descriptor.Validate() != nil {
		return nil, fmt.Errorf("%w: current-use or removal authority is unavailable", ErrPolicy)
	}
	observation := current.Observation()
	request, ok := current.RemovalRequest()
	if !ok || request.JobKey == "" || descriptor.Driver != observation.Driver {
		return nil, fmt.Errorf("%w: current-use removal authority is unavailable", ErrPolicy)
	}
	final := observation.Final
	plan := Plan{
		Schema: PlanSchemaV1, Action: ActionRemoveKeepData, DeleteLocalData: false,
		Driver: observation.Driver, ClientConfigID: observation.ClientConfigID, Removal: descriptor,
		UseID: observation.UseID, JobID: observation.JobID, FileLayoutID: observation.FileLayoutID,
		CompleteFileSnapshotID: observation.CompleteFileSnapshotID, PathMappingID: observation.PathMappingID,
		ActivationOperationID: observation.ActivationOperationID, ActivationPlanID: observation.ActivationPlanID,
		ActivationTerminalID: observation.TerminalMarkerID, MetafileVariantID: final.MetafileVariantID,
		InfoHashV1: final.InfoHashV1, InfoHashV2: final.InfoHashV2,
		MaterializeOperationID: final.OperationID, MaterializePlanID: final.MaterializePlanID,
		TargetRootIdentity: final.TargetRootIdentity, FinalObjectIdentity: final.FinalObjectIdentity,
		MultiFile: final.MultiFile, ManifestFiles: final.ManifestFiles, ContentBytes: final.ContentBytes,
		FileLimits: observation.FileLimits,
	}
	planID, err := PlanID(plan)
	if err != nil {
		return nil, err
	}
	return &PreparedPlan{plan: plan, planID: planID, current: current, request: request}, nil
}

func (prepared *PreparedPlan) Plan() Plan {
	if prepared == nil || prepared.plan.Validate() != nil {
		return Plan{}
	}
	return prepared.plan
}

func (prepared *PreparedPlan) ID() string {
	if prepared == nil {
		return ""
	}
	return prepared.planID
}

func (prepared *PreparedPlan) validate() error {
	if prepared == nil || prepared.current == nil || !prepared.current.Verified() || prepared.plan.Validate() != nil || prepared.request.JobKey == "" {
		return fmt.Errorf("%w: prepared client removal plan is unavailable", ErrPolicy)
	}
	computed, err := PlanID(prepared.plan)
	if err != nil || computed != prepared.planID {
		return fmt.Errorf("%w: prepared client removal plan changed", ErrIntegrity)
	}
	currentPlan, err := BuildPlan(prepared.current, prepared.plan.Removal)
	if err != nil || currentPlan.planID != prepared.planID || currentPlan.request.JobKey != prepared.request.JobKey {
		return fmt.Errorf("%w: current-use authority differs from the removal plan", ErrIntegrity)
	}
	return nil
}
