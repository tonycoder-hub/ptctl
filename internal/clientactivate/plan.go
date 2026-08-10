package clientactivate

import (
	"context"
	"fmt"

	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

type AuthorityOptions struct {
	ClientConfigID string
	HostRoot       string
	ClientRoot     string
	ClientWindows  bool
	FileLimits     downloader.JobFileLedgerLimits
}

// PreparedAuthority retains current exact-final, canonical adoption, and raw
// path-mapping authority only in memory. A downloader observation completes a
// deterministic PreparedPlan.
type PreparedAuthority struct {
	verifiedFinal    *materialize.VerifiedFinal
	verifiedAdoption *clientadopt.VerifiedCompletion
	final            materialize.FinalObservation
	adoption         clientadopt.CompletionObservation
	projection       materialize.FinalClientProjection
	clientConfigID   string
	expectedJobID    string
	windows          bool
	fileLimits       downloader.JobFileLedgerLimits
}

type PreparedPlan struct {
	plan        Plan
	planID      string
	operationID OperationID
	authority   *PreparedAuthority
	jobKey      string
	before      clientObservation
}

func PrepareAuthority(final *materialize.VerifiedFinal, adoption *clientadopt.VerifiedCompletion, options AuthorityOptions) (*PreparedAuthority, error) {
	if final == nil || !final.Verified() || adoption == nil || !adoption.Verified() || !canonicalSHA256ID(options.ClientConfigID) {
		return nil, fmt.Errorf("%w: final or adoption authority is unavailable", ErrPolicy)
	}
	if err := options.FileLimits.Validate(); err != nil {
		return nil, fmt.Errorf("%w: client file-ledger limits are invalid", ErrPolicy)
	}
	finalObservation, adoptionObservation := final.Observation(), adoption.Observation()
	projection, err := final.ProjectClientPaths(options.HostRoot, options.ClientRoot, options.ClientWindows)
	if err != nil {
		return nil, fmt.Errorf("%w: client path projection is invalid", ErrPolicy)
	}
	if projection.ManifestFiles != finalObservation.ManifestFiles ||
		!adoption.Matches(mustAdoptionOperation(adoptionObservation.OperationID), adoptionObservation.PlanID,
			finalObservation.MetafileVariantID, options.ClientConfigID, projection.PathMappingID) ||
		adoptionObservation.MetafileVariantID != finalObservation.MetafileVariantID ||
		adoptionObservation.ExpectedSavePathRef != projection.SavePathRef ||
		adoptionObservation.ExpectedContentPathRef != projection.ContentPathRef ||
		adoptionObservation.FinalObjectIdentity != finalObservation.FinalObjectIdentity ||
		!stoppedState(adoptionObservation.JobState) {
		return nil, fmt.Errorf("%w: adoption authority differs from the current exact final or mapping", ErrIntegrity)
	}
	return &PreparedAuthority{
		verifiedFinal: final, verifiedAdoption: adoption, final: finalObservation, adoption: adoptionObservation,
		projection: projection, clientConfigID: options.ClientConfigID, expectedJobID: adoptionObservation.JobID,
		windows: options.ClientWindows, fileLimits: options.FileLimits,
	}, nil
}

func mustAdoptionOperation(value string) clientadopt.OperationID {
	id, _ := clientadopt.ParseOperationID(value)
	return id
}

func BuildPlan(authority *PreparedAuthority, descriptor downloader.ExistingJobControlDescriptor, observed clientObservation, startAfterRecheck bool) (*PreparedPlan, error) {
	if authority == nil || authority.verifiedFinal == nil || authority.verifiedAdoption == nil || descriptor.Validate() != nil {
		return nil, fmt.Errorf("%w: activation plan authority is unavailable", ErrPolicy)
	}
	if err := observed.validateForPlan(authority); err != nil {
		return nil, err
	}
	action := ActionRecheckOnly
	if startAfterRecheck {
		action = ActionRecheckThenStart
	}
	final, adoption, projection := authority.final, authority.adoption, authority.projection
	plan := Plan{
		Schema: PlanSchemaV1, Action: action, Driver: DriverQBittorrent, ClientConfigID: authority.clientConfigID,
		Control: descriptor, PathMappingID: projection.PathMappingID, ClientPathSemantics: projection.PathSemantics,
		ExpectedSavePathRef: projection.SavePathRef, ExpectedContentPathRef: projection.ContentPathRef,
		ExpectedFileLayoutID: observed.fileLayoutID, JobID: observed.jobID,
		MetafileVariantID: final.MetafileVariantID, InfoHashV1: final.InfoHashV1, InfoHashV2: final.InfoHashV2,
		MaterializeOperationID: final.OperationID, MaterializePlanID: final.MaterializePlanID,
		AdoptionOperationID: adoption.OperationID, AdoptionPlanID: adoption.PlanID, AdoptionCompletionID: adoption.CompletionID,
		TargetRootIdentity: final.TargetRootIdentity, FinalObjectIdentity: final.FinalObjectIdentity,
		MultiFile: final.MultiFile, ManifestFiles: final.ManifestFiles, ContentBytes: final.ContentBytes,
		FileLimits: authority.fileLimits,
	}
	planID, err := PlanID(plan)
	if err != nil {
		return nil, err
	}
	return &PreparedPlan{plan: plan, planID: planID, operationID: OperationIDForPlan(planID), authority: authority, jobKey: observed.job.Hash, before: observed}, nil
}

func (prepared *PreparedPlan) Plan() Plan {
	if prepared == nil {
		return Plan{}
	}
	return prepared.plan
}

func (prepared *PreparedPlan) PlanID() string {
	if prepared == nil {
		return ""
	}
	return prepared.planID
}

func (prepared *PreparedPlan) OperationID() OperationID {
	if prepared == nil {
		return ""
	}
	return prepared.operationID
}

func (prepared *PreparedPlan) ReverifyFinal(ctx context.Context) (materialize.FinalObservation, error) {
	if prepared == nil || prepared.authority == nil || prepared.authority.verifiedFinal == nil {
		return materialize.FinalObservation{}, fmt.Errorf("%w: current final authority is unavailable", ErrPolicy)
	}
	fresh, observation, err := prepared.authority.verifiedFinal.Reverify(ctx)
	if err != nil {
		return materialize.FinalObservation{}, err
	}
	plan := prepared.plan
	if fresh == nil || !fresh.Verified() || observation.OperationID != plan.MaterializeOperationID || observation.MaterializePlanID != plan.MaterializePlanID ||
		observation.MetafileVariantID != plan.MetafileVariantID || observation.InfoHashV1 != plan.InfoHashV1 || observation.InfoHashV2 != plan.InfoHashV2 ||
		observation.TargetRootIdentity != plan.TargetRootIdentity || observation.FinalObjectIdentity != plan.FinalObjectIdentity ||
		observation.MultiFile != plan.MultiFile || observation.ManifestFiles != plan.ManifestFiles || observation.ContentBytes != plan.ContentBytes ||
		observation.BytesVerified != plan.ContentBytes {
		return materialize.FinalObservation{}, fmt.Errorf("%w: current materialized final differs from the activation plan", ErrIntegrity)
	}
	prepared.authority.verifiedFinal = fresh
	prepared.authority.final = observation
	return observation, nil
}
