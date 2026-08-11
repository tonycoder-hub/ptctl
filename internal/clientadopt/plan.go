package clientadopt

import (
	"fmt"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

type PlanOptions struct {
	Driver         string
	ClientConfigID string
	HostRoot       string
	ClientRoot     string
	ClientWindows  bool
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
	plan := Plan{
		Schema: PlanSchemaV1, Action: ActionAddStopped, Driver: driver,
		ClientConfigID: options.ClientConfigID, PathMappingID: projection.PathMappingID,
		ClientPathSemantics: projection.PathSemantics, ExpectedSavePathRef: projection.SavePathRef,
		ExpectedContentPathRef: projection.ContentPathRef, MetafileVariantID: observation.MetafileVariantID,
		MetafileBytes: observation.MetafileBytes, InfoHashV1: observation.InfoHashV1, InfoHashV2: observation.InfoHashV2,
		MaterializeOperationID: observation.OperationID, MaterializePlanID: observation.MaterializePlanID,
		TargetRootIdentity: observation.TargetRootIdentity, FinalObjectIdentity: observation.FinalObjectIdentity,
		MultiFile: observation.MultiFile, ManifestFiles: observation.ManifestFiles, ContentBytes: observation.ContentBytes,
	}
	planID, err := PlanID(plan)
	if err != nil {
		return nil, err
	}
	return &PreparedPlan{
		plan: plan, planID: planID, operation: OperationIDForPlan(planID), verified: verified,
		projection: projection, windows: options.ClientWindows,
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
