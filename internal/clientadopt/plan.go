package clientadopt

import (
	"fmt"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

type PlanOptions struct {
	Driver               string
	ClientConfigID       string
	HostRoot             string
	ClientRoot           string
	ClientWindows        bool
	AdoptExistingStopped bool
	PriorCompletion      *VerifiedCompletion
}

// PreparedPlan retains the exact-final authority and raw projected client
// paths only in memory. Public Plan contains one-way references instead.
type PreparedPlan struct {
	plan       Plan
	planID     string
	operation  OperationID
	verified   *materialize.VerifiedFinal
	projection materialize.FinalClientProjection
	windows    bool
	prior      *VerifiedCompletion
}

func BuildPlan(verified *materialize.VerifiedFinal, options PlanOptions) (*PreparedPlan, error) {
	if verified == nil || !verified.Verified() || !canonicalSHA256ID(options.ClientConfigID) {
		return nil, fmt.Errorf("%w: exact final or client configuration authority is unavailable", ErrPolicy)
	}
	observation := verified.Observation()
	projection, err := verified.ProjectClientPaths(options.HostRoot, options.ClientRoot, options.ClientWindows)
	if err != nil {
		return nil, fmt.Errorf("%w: client path projection is invalid", ErrPolicy)
	}
	if _, ok := projection.SavePath(); !ok {
		return nil, fmt.Errorf("%w: client save path is unavailable", ErrPolicy)
	}
	if _, ok := projection.ContentPath(); !ok {
		return nil, fmt.Errorf("%w: client content path is unavailable", ErrPolicy)
	}
	driver := options.Driver
	if driver == "" {
		driver = DriverQBittorrent
	}
	action := ActionAddStopped
	if options.AdoptExistingStopped {
		action = ActionAdoptExistingStopped
	}
	plan := Plan{
		Schema: PlanSchemaV1, Action: action, Driver: driver,
		ClientConfigID: options.ClientConfigID, PathMappingID: projection.PathMappingID,
		ClientPathSemantics: projection.PathSemantics, ExpectedSavePathRef: projection.SavePathRef,
		ExpectedContentPathRef: projection.ContentPathRef, MetafileVariantID: observation.MetafileVariantID,
		MetafileBytes: observation.MetafileBytes, InfoHashV1: observation.InfoHashV1, InfoHashV2: observation.InfoHashV2,
		MaterializeOperationID: observation.OperationID, MaterializePlanID: observation.MaterializePlanID,
		TargetRootIdentity: observation.TargetRootIdentity, FinalObjectIdentity: observation.FinalObjectIdentity,
		MultiFile: observation.MultiFile, ManifestFiles: observation.ManifestFiles, ContentBytes: observation.ContentBytes,
	}
	if options.PriorCompletion != nil {
		if options.AdoptExistingStopped {
			return nil, fmt.Errorf("%w: observation-only adoption cannot consume prior adoption lineage", ErrPolicy)
		}
		if !options.PriorCompletion.Verified() {
			return nil, fmt.Errorf("%w: prior adoption completion authority is invalid", ErrPolicy)
		}
		priorPlan := options.PriorCompletion.Plan()
		priorObservation := options.PriorCompletion.Observation()
		if priorPlan.Driver != plan.Driver || priorPlan.ClientConfigID != plan.ClientConfigID ||
			priorPlan.PathMappingID != plan.PathMappingID || priorPlan.ClientPathSemantics != plan.ClientPathSemantics ||
			priorPlan.ExpectedSavePathRef != plan.ExpectedSavePathRef || priorPlan.ExpectedContentPathRef != plan.ExpectedContentPathRef ||
			priorPlan.MetafileVariantID != plan.MetafileVariantID || priorPlan.MetafileBytes != plan.MetafileBytes ||
			priorPlan.InfoHashV1 != plan.InfoHashV1 || priorPlan.InfoHashV2 != plan.InfoHashV2 ||
			priorPlan.MaterializeOperationID != plan.MaterializeOperationID || priorPlan.MaterializePlanID != plan.MaterializePlanID ||
			priorPlan.TargetRootIdentity != plan.TargetRootIdentity || priorPlan.FinalObjectIdentity != plan.FinalObjectIdentity ||
			priorPlan.MultiFile != plan.MultiFile || priorPlan.ManifestFiles != plan.ManifestFiles || priorPlan.ContentBytes != plan.ContentBytes ||
			priorObservation.FinalObjectIdentity != plan.FinalObjectIdentity {
			return nil, fmt.Errorf("%w: prior adoption completion belongs to a different final or client configuration", ErrPolicy)
		}
		plan.PriorAdoptionOperationID = priorObservation.OperationID
		plan.PriorAdoptionPlanID = priorObservation.PlanID
		plan.PriorAdoptionCompletionID = priorObservation.CompletionID
	}
	planID, err := PlanID(plan)
	if err != nil {
		return nil, err
	}
	return &PreparedPlan{
		plan: plan, planID: planID, operation: OperationIDForPlan(planID), verified: verified,
		projection: projection, windows: options.ClientWindows, prior: options.PriorCompletion,
	}, nil
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
	return prepared.operation
}

func (prepared *PreparedPlan) Action() string {
	if prepared == nil {
		return ""
	}
	return prepared.plan.Action
}

func (prepared *PreparedPlan) ObservationOnly() bool {
	return prepared != nil && prepared.plan.Action == ActionAdoptExistingStopped
}

func (prepared *PreparedPlan) typedIdentity() downloader.TypedIdentity {
	return downloader.TypedIdentity{InfoHashV1: prepared.plan.InfoHashV1, InfoHashV2: prepared.plan.InfoHashV2}
}

func (prepared *PreparedPlan) targetRoot() (string, bool) {
	if prepared == nil || prepared.verified == nil {
		return "", false
	}
	return prepared.verified.ProcessTargetRoot()
}

func (prepared *PreparedPlan) savePath() (string, bool) {
	if prepared == nil {
		return "", false
	}
	return prepared.projection.SavePath()
}

func (prepared *PreparedPlan) contentPath() (string, bool) {
	if prepared == nil {
		return "", false
	}
	return prepared.projection.ContentPath()
}
