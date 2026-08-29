package clientactivate

import (
	"context"
	"fmt"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

type AuthorityOptions struct {
	Driver         string
	ClientConfigID string
	HostRoot       string
	ClientRoot     string
	ClientWindows  bool
	FileLimits     downloader.JobFileLedgerLimits
}

// PreparedAuthority retains the current exact final, one canonical lineage
// prerequisite, and raw path-mapping authority only in memory. A downloader
// observation completes a deterministic PreparedPlan.
type PreparedAuthority struct {
	verifiedFinal     *materialize.VerifiedFinal
	verifiedAdoption  *clientadopt.VerifiedCompletion
	terminalStopProof TerminalStopStartProof
	terminalStop      *TerminalStopPrerequisite
	priorPlan         Plan
	final             materialize.FinalObservation
	adoption          clientadopt.CompletionObservation
	projection        materialize.FinalClientProjection
	clientConfigID    string
	driver            string
	expectedJobID     string
	windows           bool
	fileLimits        downloader.JobFileLedgerLimits
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
	adoptionPlan := adoption.Plan()
	driver := options.Driver
	if driver == "" {
		driver = adoptionPlan.Driver
	}
	policy, supported := downloader.DescribeExistingJobControlDriver(driver)
	identity := downloader.TypedIdentity{InfoHashV1: final.Observation().InfoHashV1, InfoHashV2: final.Observation().InfoHashV2}
	if !supported || adoptionPlan.Driver != driver || !policy.SupportsIdentity(identity) {
		return nil, fmt.Errorf("%w: adoption driver cannot provide the required existing-job control authority", ErrPolicy)
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
		driver: driver, windows: options.ClientWindows, fileLimits: options.FileLimits,
	}, nil
}

// PrepareStartAfterStopAuthority binds a prior terminal activation, one
// attributed terminal stop, the current exact final, and the invocation-scoped
// client namespace before any downloader credential or request is needed.
func PrepareStartAfterStopAuthority(final *materialize.VerifiedFinal, completion *VerifiedCompletion,
	stop TerminalStopStartProof, options AuthorityOptions) (*PreparedAuthority, error) {
	if stop == nil {
		return nil, fmt.Errorf("%w: terminal client-stop authority is unavailable", ErrPolicy)
	}
	current, err := PrepareCurrentUse(final, completion, CurrentUseOptions{
		ClientConfigID: options.ClientConfigID, HostRoot: options.HostRoot, ClientRoot: options.ClientRoot,
		ClientWindows: options.ClientWindows, FileLimits: options.FileLimits,
	})
	if err != nil {
		return nil, err
	}
	prerequisite, ok := stop.ActivationStartPrerequisite()
	if !ok || prerequisite.validate() != nil {
		return nil, fmt.Errorf("%w: terminal client-stop authority cannot authorize start", ErrPolicy)
	}
	expectation := current.Expectation()
	priorPlan := completion.Plan()
	priorObservation := completion.Observation()
	priorObservedEnd, priorTimeErr := time.Parse(time.RFC3339Nano, priorObservation.ObservedAtEnd)
	finalObservation := final.Observation()
	if priorTimeErr != nil || priorObservedEnd.IsZero() || prerequisite.ObservedAtStart.Before(priorObservedEnd) ||
		prerequisite.Driver != expectation.Driver || prerequisite.UseID != expectation.UseID ||
		prerequisite.JobID != expectation.JobID || prerequisite.FileLayoutID != expectation.FileLayoutID ||
		prerequisite.ClientConfigID != expectation.ClientConfigID || prerequisite.PathMappingID != expectation.PathMappingID ||
		prerequisite.ActivationOperationID != expectation.ActivationOperationID ||
		prerequisite.ActivationPlanID != expectation.ActivationPlanID || prerequisite.ActivationTerminalID != expectation.TerminalMarkerID ||
		prerequisite.MetafileVariantID != finalObservation.MetafileVariantID || prerequisite.InfoHashV1 != finalObservation.InfoHashV1 ||
		prerequisite.InfoHashV2 != finalObservation.InfoHashV2 || prerequisite.MaterializeOperationID != finalObservation.OperationID ||
		prerequisite.MaterializePlanID != finalObservation.MaterializePlanID || prerequisite.TargetRootIdentity != finalObservation.TargetRootIdentity ||
		prerequisite.FinalObjectIdentity != finalObservation.FinalObjectIdentity || prerequisite.MultiFile != finalObservation.MultiFile ||
		prerequisite.ManifestFiles != finalObservation.ManifestFiles || prerequisite.ContentBytes != finalObservation.ContentBytes {
		return nil, fmt.Errorf("%w: terminal client-stop authority differs from the prior activation or exact final", ErrIntegrity)
	}
	prepared := current.prepared
	if prepared == nil || priorPlan.Validate() != nil {
		return nil, fmt.Errorf("%w: prior activation authority is unavailable", ErrIntegrity)
	}
	value := prerequisite
	prepared.terminalStopProof, prepared.terminalStop, prepared.priorPlan = stop, &value, priorPlan
	prepared.adoption = historicalAdoption(priorPlan)
	return prepared, nil
}

func mustAdoptionOperation(value string) clientadopt.OperationID {
	id, _ := clientadopt.ParseOperationID(value)
	return id
}

func BuildPlan(authority *PreparedAuthority, descriptor downloader.ExistingJobControlDescriptor, observed clientObservation, startAfterRecheck bool) (*PreparedPlan, error) {
	if authority == nil || authority.verifiedFinal == nil ||
		descriptor.Validate() != nil || descriptor.Driver != authority.driver {
		return nil, fmt.Errorf("%w: activation plan authority is unavailable", ErrPolicy)
	}
	action := ActionRecheckOnly
	var stopLink *TerminalStopLink
	if authority.terminalStop != nil {
		if authority.terminalStopProof == nil || startAfterRecheck || authority.verifiedAdoption != nil || authority.priorPlan.Validate() != nil {
			return nil, fmt.Errorf("%w: start-after-stop authority is contradictory", ErrPolicy)
		}
		if err := observed.validateForStoppedStartPlan(authority, *authority.terminalStop); err != nil {
			return nil, err
		}
		action = ActionStartAfterStop
		value := authority.terminalStop.planLink()
		stopLink = &value
	} else {
		if authority.verifiedAdoption == nil {
			return nil, fmt.Errorf("%w: stopped-adoption authority is unavailable", ErrPolicy)
		}
		if err := observed.validateForPlan(authority); err != nil {
			return nil, err
		}
		if startAfterRecheck {
			action = ActionRecheckThenStart
		}
	}
	final, adoption, projection := authority.final, authority.adoption, authority.projection
	if action == ActionStartAfterStop {
		prior := authority.priorPlan
		adoption = historicalAdoption(prior)
	}
	plan := Plan{
		Schema: PlanSchemaV1, Action: action, Driver: authority.driver, ClientConfigID: authority.clientConfigID,
		Control: descriptor, PathMappingID: projection.PathMappingID, ClientPathSemantics: projection.PathSemantics,
		ExpectedSavePathRef: projection.SavePathRef, ExpectedContentPathRef: projection.ContentPathRef,
		ExpectedFileLayoutID: observed.fileLayoutID, JobID: observed.jobID,
		MetafileVariantID: final.MetafileVariantID, InfoHashV1: final.InfoHashV1, InfoHashV2: final.InfoHashV2,
		MaterializeOperationID: final.OperationID, MaterializePlanID: final.MaterializePlanID,
		AdoptionOperationID: adoption.OperationID, AdoptionPlanID: adoption.PlanID, AdoptionCompletionID: adoption.CompletionID,
		TerminalStop:       stopLink,
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
