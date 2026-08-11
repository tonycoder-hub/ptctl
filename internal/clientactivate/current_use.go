package clientactivate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

// CurrentUseOptions re-establishes the invocation-scoped client namespace used
// by one completed activation plan. No credential or endpoint is retained.
type CurrentUseOptions struct {
	ClientConfigID string
	HostRoot       string
	ClientRoot     string
	ClientWindows  bool
	FileLimits     downloader.JobFileLedgerLimits
}

// CurrentUseAuthority is process-local authority to compare one bounded live
// downloader observation with the exact final and completed activation plan.
// It performs no request until VerifyCurrentUse is called.
type CurrentUseAuthority struct {
	prepared   *PreparedAuthority
	completion *VerifiedCompletion
	plan       Plan
	useID      string
}

// CurrentUseObservation contains only normalized, non-secret downloader
// claims. Raw job keys and client paths remain process-local.
type CurrentUseObservation struct {
	UseID                  string                       `json:"use_id"`
	ActivationOperationID  string                       `json:"activation_operation_id"`
	ActivationPlanID       string                       `json:"activation_plan_id"`
	TerminalMarkerID       string                       `json:"terminal_marker_id"`
	ClientConfigID         string                       `json:"client_config_id"`
	PathMappingID          string                       `json:"path_mapping_id"`
	JobID                  string                       `json:"job_id"`
	FileLayoutID           string                       `json:"file_layout_id"`
	CompleteFileSnapshotID string                       `json:"complete_file_snapshot_id"`
	JobState               string                       `json:"job_state"`
	JobProgress            float64                      `json:"job_progress"`
	ObservedAtStart        string                       `json:"observed_at_start"`
	ObservedAtEnd          string                       `json:"observed_at_end"`
	RequestsMade           int                          `json:"requests_made"`
	FilesObserved          int                          `json:"files_observed"`
	AllSelected            bool                         `json:"all_selected"`
	AllComplete            bool                         `json:"all_complete"`
	Final                  materialize.FinalObservation `json:"materialized_final"`
	Assurance              string                       `json:"assurance"`
}

// VerifiedCurrentUse is a same-invocation capability. JSON can retain its
// public observation but cannot recreate the private activation/final binding.
type VerifiedCurrentUse struct {
	authority   *CurrentUseAuthority
	observation CurrentUseObservation
	started     time.Time
	ended       time.Time
}

func PrepareCurrentUse(final *materialize.VerifiedFinal, completion *VerifiedCompletion, options CurrentUseOptions) (*CurrentUseAuthority, error) {
	if final == nil || !final.Verified() || completion == nil || !completion.Verified() || !canonicalSHA256ID(options.ClientConfigID) {
		return nil, fmt.Errorf("%w: current client-use authority is unavailable", ErrPolicy)
	}
	if err := options.FileLimits.Validate(); err != nil {
		return nil, fmt.Errorf("%w: current client-use file limits are invalid", ErrPolicy)
	}
	plan := completion.Plan()
	finalObservation := final.Observation()
	projection, err := final.ProjectClientPaths(options.HostRoot, options.ClientRoot, options.ClientWindows)
	if err != nil {
		return nil, fmt.Errorf("%w: current client-use path projection is invalid", ErrPolicy)
	}
	completionObservation := completion.Observation()
	operationID, parseErr := ParseOperationID(completionObservation.OperationID)
	if parseErr != nil || !completion.Matches(operationID, completionObservation.PlanID,
		finalObservation.MetafileVariantID, finalObservation.OperationID, finalObservation.MaterializePlanID,
		finalObservation.FinalObjectIdentity) {
		return nil, fmt.Errorf("%w: activation completion differs from the current final", ErrIntegrity)
	}
	wantedSemantics := "posix_exact"
	if options.ClientWindows {
		wantedSemantics = "windows_exact"
	}
	if plan.ClientConfigID != options.ClientConfigID || plan.PathMappingID != projection.PathMappingID ||
		plan.ClientPathSemantics != wantedSemantics || plan.ExpectedSavePathRef != projection.SavePathRef ||
		plan.ExpectedContentPathRef != projection.ContentPathRef || plan.FileLimits != options.FileLimits {
		return nil, fmt.Errorf("%w: current client-use mapping differs from the activation plan", ErrPolicy)
	}
	if plan.MetafileVariantID != finalObservation.MetafileVariantID || plan.InfoHashV1 != finalObservation.InfoHashV1 ||
		plan.InfoHashV2 != finalObservation.InfoHashV2 || plan.MaterializeOperationID != finalObservation.OperationID ||
		plan.MaterializePlanID != finalObservation.MaterializePlanID || plan.TargetRootIdentity != finalObservation.TargetRootIdentity ||
		plan.FinalObjectIdentity != finalObservation.FinalObjectIdentity || plan.MultiFile != finalObservation.MultiFile ||
		plan.ManifestFiles != finalObservation.ManifestFiles || plan.ContentBytes != finalObservation.ContentBytes ||
		projection.ManifestFiles != finalObservation.ManifestFiles {
		return nil, fmt.Errorf("%w: current final differs from the activation plan", ErrIntegrity)
	}
	prepared := &PreparedAuthority{
		verifiedFinal: final, final: finalObservation, projection: projection, clientConfigID: options.ClientConfigID,
		driver: plan.Driver, expectedJobID: plan.JobID, windows: options.ClientWindows, fileLimits: options.FileLimits,
	}
	useID, err := currentUseID(plan)
	if err != nil {
		return nil, err
	}
	return &CurrentUseAuthority{prepared: prepared, completion: completion, plan: plan, useID: useID}, nil
}

func VerifyCurrentUse(ctx context.Context, authority *CurrentUseAuthority, session downloader.LedgerSession) (*VerifiedCurrentUse, CurrentUseObservation, error) {
	if authority == nil || authority.prepared == nil || authority.completion == nil || session == nil {
		return nil, CurrentUseObservation{}, fmt.Errorf("%w: current client-use observation authority is unavailable", ErrPolicy)
	}
	if err := ctx.Err(); err != nil {
		return nil, CurrentUseObservation{}, err
	}
	completionObservation := authority.completion.Observation()
	requestsBefore := session.RequestsMade()
	observed, err := observeClient(ctx, authority.prepared, session)
	requestsAfter := session.RequestsMade()
	requestDelta := requestsAfter - requestsBefore
	expectedRequests := 1
	if authority.plan.MultiFile {
		expectedRequests = 2
	}
	if requestDelta < 0 || requestDelta > expectedRequests {
		return nil, CurrentUseObservation{RequestsMade: nonnegativeRequestDelta(requestDelta)},
			fmt.Errorf("%w: downloader current-use request count is contradictory", ErrIntegrity)
	}
	if err != nil {
		if errors.Is(err, ErrIntegrity) {
			err = fmt.Errorf("%w: live downloader claims conflict with the reviewed activation", ErrPolicy)
		}
		return nil, CurrentUseObservation{RequestsMade: nonnegativeRequestDelta(requestDelta)}, err
	}
	if requestDelta != expectedRequests {
		return nil, CurrentUseObservation{RequestsMade: nonnegativeRequestDelta(requestDelta)},
			fmt.Errorf("%w: downloader current-use request count is contradictory", ErrIntegrity)
	}
	plan := authority.plan
	if observed.jobID != plan.JobID || observed.fileLayoutID != plan.ExpectedFileLayoutID ||
		!canonicalSHA256ID(observed.completeSnapshotID) {
		return nil, CurrentUseObservation{RequestsMade: nonnegativeRequestDelta(requestDelta)},
			fmt.Errorf("%w: live downloader identity or layout differs from the completed activation plan", ErrPolicy)
	}
	if !observed.allSelected || !observed.allComplete || observed.job.Progress != 1 ||
		(!completeStoppedState(observed.job.State) && !startedState(observed.job.State)) {
		return nil, CurrentUseObservation{RequestsMade: nonnegativeRequestDelta(requestDelta)},
			fmt.Errorf("%w: live downloader job is not complete on the reviewed final layout", ErrPolicy)
	}
	preparedPlan := &PreparedPlan{plan: plan, planID: completionPlanID(authority.completion), authority: authority.prepared}
	finalObservation, err := preparedPlan.ReverifyFinal(ctx)
	if err != nil {
		return nil, CurrentUseObservation{RequestsMade: nonnegativeRequestDelta(requestDelta)}, err
	}
	filesObserved := 1
	if plan.MultiFile {
		filesObserved = len(observed.files.Files)
	}
	public := CurrentUseObservation{
		UseID: authority.useID, ActivationOperationID: completionObservation.OperationID,
		ActivationPlanID: completionObservation.PlanID, TerminalMarkerID: completionObservation.TerminalMarkerID,
		ClientConfigID: plan.ClientConfigID, PathMappingID: plan.PathMappingID,
		JobID: observed.jobID, FileLayoutID: observed.fileLayoutID,
		CompleteFileSnapshotID: observed.completeSnapshotID, JobState: observed.job.State, JobProgress: observed.job.Progress,
		ObservedAtStart: observed.ledger.ObservedAtStart.UTC().Format(time.RFC3339Nano),
		ObservedAtEnd:   observed.ledger.ObservedAtEnd.UTC().Format(time.RFC3339Nano),
		RequestsMade:    requestDelta, FilesObserved: filesObserved, AllSelected: observed.allSelected, AllComplete: observed.allComplete,
		Final:     finalObservation,
		Assurance: "same_invocation_bounded_typed_job_and_effective_path_claim_followed_by_exact_final_reverification_non_atomic",
	}
	verified := &VerifiedCurrentUse{authority: authority, observation: public,
		started: observed.ledger.ObservedAtStart, ended: observed.ledger.ObservedAtEnd}
	if !verified.Verified() {
		return nil, CurrentUseObservation{RequestsMade: nonnegativeRequestDelta(requestDelta)},
			fmt.Errorf("%w: current client-use observation is contradictory", ErrIntegrity)
	}
	return verified, public, nil
}

func (verified *VerifiedCurrentUse) Verified() bool {
	if verified == nil || verified.authority == nil || verified.authority.prepared == nil || verified.authority.completion == nil ||
		!verified.authority.completion.Verified() || !canonicalSHA256ID(verified.observation.UseID) ||
		verified.observation.UseID != verified.authority.useID || !canonicalSHA256ID(verified.observation.JobID) ||
		!canonicalSHA256ID(verified.observation.ActivationOperationID) || !canonicalPlanID(verified.observation.ActivationPlanID) ||
		!canonicalSHA256ID(verified.observation.TerminalMarkerID) || !canonicalSHA256ID(verified.observation.ClientConfigID) ||
		!canonicalSHA256ID(verified.observation.PathMappingID) ||
		!canonicalSHA256ID(verified.observation.FileLayoutID) || !canonicalSHA256ID(verified.observation.CompleteFileSnapshotID) ||
		verified.observation.JobProgress != 1 || !verified.observation.AllSelected || !verified.observation.AllComplete ||
		verified.observation.RequestsMade <= 0 || verified.observation.FilesObserved <= 0 || verified.started.IsZero() ||
		verified.ended.Before(verified.started) || verified.observation.Final.FinalObjectIdentity != verified.authority.plan.FinalObjectIdentity {
		return false
	}
	completion := verified.authority.completion.Observation()
	plan := verified.authority.plan
	if verified.observation.ActivationOperationID != completion.OperationID || verified.observation.ActivationPlanID != completion.PlanID ||
		verified.observation.TerminalMarkerID != completion.TerminalMarkerID || verified.observation.ClientConfigID != plan.ClientConfigID ||
		verified.observation.PathMappingID != plan.PathMappingID || verified.observation.JobID != plan.JobID ||
		verified.observation.FileLayoutID != plan.ExpectedFileLayoutID {
		return false
	}
	return completeStoppedState(verified.observation.JobState) || startedState(verified.observation.JobState)
}

func (verified *VerifiedCurrentUse) Observation() CurrentUseObservation {
	if !verified.Verified() {
		return CurrentUseObservation{}
	}
	return verified.observation
}

func (verified *VerifiedCurrentUse) StableWith(after *VerifiedCurrentUse) bool {
	if !verified.Verified() || !after.Verified() {
		return false
	}
	beforeObservation, afterObservation := verified.observation, after.observation
	return verified.authority.useID == after.authority.useID && beforeObservation.UseID == afterObservation.UseID &&
		beforeObservation.JobID == afterObservation.JobID && beforeObservation.FileLayoutID == afterObservation.FileLayoutID &&
		beforeObservation.CompleteFileSnapshotID == afterObservation.CompleteFileSnapshotID &&
		beforeObservation.ActivationOperationID == afterObservation.ActivationOperationID &&
		beforeObservation.ActivationPlanID == afterObservation.ActivationPlanID &&
		beforeObservation.TerminalMarkerID == afterObservation.TerminalMarkerID &&
		beforeObservation.JobState == afterObservation.JobState && beforeObservation.JobProgress == afterObservation.JobProgress &&
		beforeObservation.Final == afterObservation.Final && !after.started.Before(verified.ended)
}

func (verified *VerifiedCurrentUse) Matches(useID, variantID, materializeOperationID, materializePlanID, finalObjectIdentity,
	activationOperationID, activationPlanID, terminalMarkerID string) bool {
	if !verified.Verified() {
		return false
	}
	plan := verified.authority.plan
	return verified.observation.UseID == useID && plan.MetafileVariantID == variantID &&
		plan.MaterializeOperationID == materializeOperationID && plan.MaterializePlanID == materializePlanID &&
		plan.FinalObjectIdentity == finalObjectIdentity && verified.observation.ActivationOperationID == activationOperationID &&
		verified.observation.ActivationPlanID == activationPlanID && verified.observation.TerminalMarkerID == terminalMarkerID
}

func currentUseID(plan Plan) (string, error) {
	if err := plan.Validate(); err != nil {
		return "", err
	}
	payload := struct {
		Schema                 string `json:"schema"`
		ClientConfigID         string `json:"client_config_id"`
		PathMappingID          string `json:"path_mapping_id"`
		JobID                  string `json:"job_id"`
		FileLayoutID           string `json:"file_layout_id"`
		MetafileVariantID      string `json:"metafile_variant_id"`
		MaterializeOperationID string `json:"materialize_operation_id"`
		MaterializePlanID      string `json:"materialize_plan_id"`
		FinalObjectIdentity    string `json:"final_object_identity"`
	}{
		Schema: "ptctl.client-current-use/v1", ClientConfigID: plan.ClientConfigID, PathMappingID: plan.PathMappingID,
		JobID: plan.JobID, FileLayoutID: plan.ExpectedFileLayoutID, MetafileVariantID: plan.MetafileVariantID,
		MaterializeOperationID: plan.MaterializeOperationID, MaterializePlanID: plan.MaterializePlanID,
		FinalObjectIdentity: plan.FinalObjectIdentity,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte("ptctl-client-current-use-v1\x00"), raw...))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func completionPlanID(completion *VerifiedCompletion) string {
	if completion == nil || !completion.Verified() {
		return ""
	}
	return completion.Observation().PlanID
}

func nonnegativeRequestDelta(value int) int {
	if value < 0 {
		return 0
	}
	return value
}
