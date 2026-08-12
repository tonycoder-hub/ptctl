package clientactivate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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

type CurrentUseExpectation struct {
	Driver                string                         `json:"driver"`
	UseID                 string                         `json:"use_id"`
	ActivationOperationID string                         `json:"activation_operation_id"`
	ActivationPlanID      string                         `json:"activation_plan_id"`
	TerminalMarkerID      string                         `json:"terminal_marker_id"`
	ClientConfigID        string                         `json:"client_config_id"`
	PathMappingID         string                         `json:"path_mapping_id"`
	JobID                 string                         `json:"job_id"`
	FileLayoutID          string                         `json:"file_layout_id"`
	FileLimits            downloader.JobFileLedgerLimits `json:"file_limits"`
	Final                 materialize.FinalObservation   `json:"materialized_final"`
}

// CurrentUseObservation contains only normalized, non-secret downloader
// claims. Raw job keys and client paths remain process-local.
type CurrentUseObservation struct {
	Driver                 string                         `json:"driver"`
	UseID                  string                         `json:"use_id"`
	ActivationOperationID  string                         `json:"activation_operation_id"`
	ActivationPlanID       string                         `json:"activation_plan_id"`
	TerminalMarkerID       string                         `json:"terminal_marker_id"`
	ClientConfigID         string                         `json:"client_config_id"`
	PathMappingID          string                         `json:"path_mapping_id"`
	JobID                  string                         `json:"job_id"`
	FileLayoutID           string                         `json:"file_layout_id"`
	CompleteFileSnapshotID string                         `json:"complete_file_snapshot_id"`
	JobState               string                         `json:"job_state"`
	JobProgress            float64                        `json:"job_progress"`
	ObservedAtStart        string                         `json:"observed_at_start"`
	ObservedAtEnd          string                         `json:"observed_at_end"`
	RequestsMade           int                            `json:"requests_made"`
	FilesObserved          int                            `json:"files_observed"`
	FileLimits             downloader.JobFileLedgerLimits `json:"file_limits"`
	AllSelected            bool                           `json:"all_selected"`
	AllComplete            bool                           `json:"all_complete"`
	Final                  materialize.FinalObservation   `json:"materialized_final"`
	Assurance              string                         `json:"assurance"`
}

// VerifiedCurrentUse is a same-invocation capability. JSON can retain its
// public observation but cannot recreate the private activation/final binding.
type VerifiedCurrentUse struct {
	authority   *CurrentUseAuthority
	observation CurrentUseObservation
	jobKey      string
	session     downloader.LedgerSession
	started     time.Time
	ended       time.Time
}

type CurrentAbsenceObservation struct {
	Driver          string                       `json:"driver"`
	UseID           string                       `json:"use_id"`
	JobID           string                       `json:"job_id"`
	Status          string                       `json:"status"`
	ObservedAtStart string                       `json:"observed_at_start"`
	ObservedAtEnd   string                       `json:"observed_at_end"`
	RequestsMade    int                          `json:"requests_made"`
	JobsExamined    int                          `json:"jobs_examined"`
	Final           materialize.FinalObservation `json:"materialized_final"`
	Assurance       string                       `json:"assurance"`
}

// VerifiedCurrentAbsence proves only that one complete live queue observation
// in the same session no longer contained the exact typed job, followed by an
// exact final re-verification. It does not attribute causality to a request.
type VerifiedCurrentAbsence struct {
	authority   *CurrentUseAuthority
	before      *VerifiedCurrentUse
	observation CurrentAbsenceObservation
	session     downloader.LedgerSession
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

func (authority *CurrentUseAuthority) Expectation() CurrentUseExpectation {
	if authority == nil || authority.prepared == nil || authority.completion == nil || !authority.completion.Verified() ||
		authority.plan.Validate() != nil || !canonicalSHA256ID(authority.useID) {
		return CurrentUseExpectation{}
	}
	completion := authority.completion.Observation()
	return CurrentUseExpectation{
		Driver: authority.plan.Driver, UseID: authority.useID, ActivationOperationID: completion.OperationID,
		ActivationPlanID: completion.PlanID, TerminalMarkerID: completion.TerminalMarkerID,
		ClientConfigID: authority.plan.ClientConfigID, PathMappingID: authority.plan.PathMappingID,
		JobID: authority.plan.JobID, FileLayoutID: authority.plan.ExpectedFileLayoutID,
		FileLimits: authority.plan.FileLimits, Final: authority.prepared.final,
	}
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
		Driver: plan.Driver, UseID: authority.useID, ActivationOperationID: completionObservation.OperationID,
		ActivationPlanID: completionObservation.PlanID, TerminalMarkerID: completionObservation.TerminalMarkerID,
		ClientConfigID: plan.ClientConfigID, PathMappingID: plan.PathMappingID,
		JobID: observed.jobID, FileLayoutID: observed.fileLayoutID,
		CompleteFileSnapshotID: observed.completeSnapshotID, JobState: observed.job.State, JobProgress: observed.job.Progress,
		ObservedAtStart: observed.ledger.ObservedAtStart.UTC().Format(time.RFC3339Nano),
		ObservedAtEnd:   observed.ledger.ObservedAtEnd.UTC().Format(time.RFC3339Nano),
		RequestsMade:    requestDelta, FilesObserved: filesObserved, FileLimits: plan.FileLimits,
		AllSelected: observed.allSelected, AllComplete: observed.allComplete,
		Final:     finalObservation,
		Assurance: "same_invocation_bounded_typed_job_and_effective_path_claim_followed_by_exact_final_reverification_non_atomic",
	}
	verified := &VerifiedCurrentUse{authority: authority, observation: public, jobKey: observed.job.Hash, session: session,
		started: observed.ledger.ObservedAtStart, ended: observed.ledger.ObservedAtEnd}
	if !verified.Verified() {
		return nil, CurrentUseObservation{RequestsMade: nonnegativeRequestDelta(requestDelta)},
			fmt.Errorf("%w: current client-use observation is contradictory", ErrIntegrity)
	}
	return verified, public, nil
}

func (verified *VerifiedCurrentUse) Verified() bool {
	if verified == nil || verified.authority == nil || verified.authority.prepared == nil || verified.authority.completion == nil || verified.session == nil ||
		!verified.authority.completion.Verified() || !canonicalSHA256ID(verified.observation.UseID) ||
		verified.observation.UseID != verified.authority.useID || !canonicalSHA256ID(verified.observation.JobID) ||
		!canonicalSHA256ID(verified.observation.ActivationOperationID) || !canonicalPlanID(verified.observation.ActivationPlanID) ||
		!canonicalSHA256ID(verified.observation.TerminalMarkerID) || !canonicalSHA256ID(verified.observation.ClientConfigID) ||
		!canonicalSHA256ID(verified.observation.PathMappingID) ||
		!canonicalSHA256ID(verified.observation.FileLayoutID) || !canonicalSHA256ID(verified.observation.CompleteFileSnapshotID) ||
		verified.observation.Driver != verified.authority.plan.Driver ||
		!validCurrentJobKey(verified.jobKey) || opaqueJobID(verified.jobKey) != verified.observation.JobID ||
		verified.observation.JobProgress != 1 || !verified.observation.AllSelected || !verified.observation.AllComplete ||
		verified.observation.RequestsMade <= 0 || verified.observation.FilesObserved <= 0 || verified.observation.FileLimits.Validate() != nil ||
		verified.observation.FileLimits != verified.authority.plan.FileLimits || verified.started.IsZero() ||
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

// MutationRequest returns the same-session opaque job locator only while this
// invocation still holds the verified current-use capability. The locator is
// deliberately omitted from every public observation and JSON report. Callers
// still need a narrower, code-owned mutation interface before using it.
func (verified *VerifiedCurrentUse) MutationRequest() (downloader.ExistingJobMutationRequest, bool) {
	if !verified.Verified() {
		return downloader.ExistingJobMutationRequest{}, false
	}
	return downloader.ExistingJobMutationRequest{JobKey: verified.jobKey}, true
}

// RemovalRequest is retained for the keep-data removal workflow. It does not
// grant any authority beyond MutationRequest.
func (verified *VerifiedCurrentUse) RemovalRequest() (downloader.ExistingJobMutationRequest, bool) {
	return verified.MutationRequest()
}

func (verified *VerifiedCurrentUse) JobStarted() bool {
	return verified.Verified() && startedState(verified.observation.JobState)
}

func (verified *VerifiedCurrentUse) JobStopped() bool {
	return verified.Verified() && completeStoppedState(verified.observation.JobState)
}

func (verified *VerifiedCurrentUse) SameSession(session downloader.LedgerSession) bool {
	return verified.Verified() && sameLedgerSession(verified.session, session)
}

// Reobserve repeats the bounded current-use proof in the same session and
// requires every identity-critical claim to remain stable.
func (verified *VerifiedCurrentUse) Reobserve(ctx context.Context, session downloader.LedgerSession) (*VerifiedCurrentUse, CurrentUseObservation, error) {
	if !verified.SameSession(session) {
		return nil, CurrentUseObservation{}, fmt.Errorf("%w: current client-use session authority is unavailable", ErrPolicy)
	}
	after, observation, err := VerifyCurrentUse(ctx, verified.authority, session)
	if err != nil {
		return nil, observation, err
	}
	if !verified.StableWith(after) {
		return nil, observation, fmt.Errorf("%w: current downloader use changed across the removal bracket", ErrPolicy)
	}
	return after, observation, nil
}

// VerifyCurrentJobStopped closes a same-session stop bracket. The job state is
// allowed to move from a complete started state to a complete stopped state;
// every identity, layout, selection, progress, and final-filesystem claim must
// remain stable and the second observation must begin after the first ended.
func VerifyCurrentJobStopped(ctx context.Context, before *VerifiedCurrentUse, session downloader.LedgerSession) (*VerifiedCurrentUse, CurrentUseObservation, error) {
	if before == nil || !before.JobStarted() || !before.SameSession(session) {
		return nil, CurrentUseObservation{}, fmt.Errorf("%w: started current-use authority is unavailable", ErrPolicy)
	}
	after, observation, err := VerifyCurrentUse(ctx, before.authority, session)
	if err != nil {
		return nil, observation, err
	}
	if !after.JobStopped() || !before.stableAcrossStop(after) {
		return nil, observation, fmt.Errorf("%w: downloader job did not make a stable transition to stopped", ErrPolicy)
	}
	return after, observation, nil
}

// VerifyExpectedJobStopped is the recovery form of VerifyCurrentJobStopped.
// It proves a fresh stopped observation against the reviewed activation and
// exact final, but does not attribute that state to an earlier request.
func VerifyExpectedJobStopped(ctx context.Context, authority *CurrentUseAuthority, session downloader.LedgerSession, notBefore time.Time) (*VerifiedCurrentUse, CurrentUseObservation, error) {
	if authority == nil || authority.prepared == nil || authority.completion == nil || !authority.completion.Verified() ||
		authority.plan.Validate() != nil || session == nil {
		return nil, CurrentUseObservation{}, fmt.Errorf("%w: expected current-use authority is unavailable", ErrPolicy)
	}
	after, observation, err := VerifyCurrentUse(ctx, authority, session)
	if err != nil {
		return nil, observation, err
	}
	if !after.JobStopped() || !notBefore.IsZero() && after.started.Before(notBefore) {
		return nil, observation, fmt.Errorf("%w: downloader does not prove the reviewed exact job stopped after the requested boundary", ErrPolicy)
	}
	return after, observation, nil
}

func VerifyCurrentJobAbsent(ctx context.Context, before *VerifiedCurrentUse, session downloader.LedgerSession) (*VerifiedCurrentAbsence, CurrentAbsenceObservation, error) {
	if !before.Verified() || session == nil || !sameLedgerSession(before.session, session) {
		return nil, CurrentAbsenceObservation{}, fmt.Errorf("%w: current client-use session authority is unavailable", ErrPolicy)
	}
	verified, observation, err := VerifyExpectedJobAbsent(ctx, before.authority, session, before.ended)
	if err != nil {
		return nil, observation, err
	}
	verified.before = before
	if !verified.Verified() {
		return nil, CurrentAbsenceObservation{}, fmt.Errorf("%w: current client absence bracket is contradictory", ErrIntegrity)
	}
	return verified, observation, nil
}

// VerifyExpectedJobAbsent observes a complete typed queue for one prepared
// activation authority without requiring the job to still exist. It is used by
// recovery after a removal request may already have crossed the boundary.
func VerifyExpectedJobAbsent(ctx context.Context, authority *CurrentUseAuthority, session downloader.LedgerSession, notBefore time.Time) (*VerifiedCurrentAbsence, CurrentAbsenceObservation, error) {
	if authority == nil || authority.prepared == nil || authority.completion == nil || !authority.completion.Verified() ||
		authority.plan.Validate() != nil || !canonicalSHA256ID(authority.useID) || session == nil {
		return nil, CurrentAbsenceObservation{}, fmt.Errorf("%w: expected client-use authority is unavailable", ErrPolicy)
	}
	if err := ctx.Err(); err != nil {
		return nil, CurrentAbsenceObservation{}, err
	}
	requestsBefore := session.RequestsMade()
	ledger, err := session.ReadLedger(ctx)
	requestDelta := session.RequestsMade() - requestsBefore
	partial := CurrentAbsenceObservation{RequestsMade: nonnegativeRequestDelta(requestDelta)}
	if err != nil {
		return nil, partial, err
	}
	if requestDelta != 1 || ledger.Driver != authority.plan.Driver || !notBefore.IsZero() && ledger.ObservedAtStart.Before(notBefore) {
		return nil, partial, fmt.Errorf("%w: downloader absence bracket is contradictory", ErrIntegrity)
	}
	if err := downloader.ValidateLedgerDriverClaims(ledger); err != nil {
		return nil, partial, fmt.Errorf("%w: downloader absence claim provenance is invalid", ErrIntegrity)
	}
	final := authority.prepared.final
	assessment, err := downloader.AssessLedgerIdentity(ledger, downloader.TypedIdentity{InfoHashV1: final.InfoHashV1, InfoHashV2: final.InfoHashV2})
	if err != nil {
		return nil, partial, err
	}
	if assessment.Status != downloader.LedgerIdentityAbsent || assessment.ExactJob != nil || assessment.ExactJobCount != 0 {
		return nil, partial, fmt.Errorf("%w: downloader does not prove the reviewed exact job absent", ErrPolicy)
	}
	preparedPlan := &PreparedPlan{plan: authority.plan, planID: completionPlanID(authority.completion), authority: authority.prepared}
	finalObservation, err := preparedPlan.ReverifyFinal(ctx)
	if err != nil {
		return nil, partial, err
	}
	public := CurrentAbsenceObservation{
		Driver: authority.plan.Driver, UseID: authority.useID, JobID: authority.plan.JobID,
		Status: "exact_typed_job_absent", ObservedAtStart: ledger.ObservedAtStart.UTC().Format(time.RFC3339Nano),
		ObservedAtEnd: ledger.ObservedAtEnd.UTC().Format(time.RFC3339Nano), RequestsMade: requestDelta,
		JobsExamined: assessment.JobsExamined, Final: finalObservation,
		Assurance: "same_session_complete_typed_queue_absence_followed_by_exact_final_reverification_non_atomic_without_causality_attribution",
	}
	verified := &VerifiedCurrentAbsence{authority: authority, observation: public, session: session, started: ledger.ObservedAtStart, ended: ledger.ObservedAtEnd}
	if !verified.Verified() {
		return nil, partial, fmt.Errorf("%w: current client absence observation is contradictory", ErrIntegrity)
	}
	return verified, public, nil
}

func (verified *VerifiedCurrentAbsence) Verified() bool {
	if verified == nil || verified.authority == nil || verified.authority.prepared == nil || verified.session == nil ||
		verified.authority.completion == nil || !verified.authority.completion.Verified() || verified.authority.plan.Validate() != nil ||
		verified.observation.Status != "exact_typed_job_absent" || verified.observation.Driver != verified.authority.plan.Driver ||
		verified.observation.UseID != verified.authority.useID || verified.observation.JobID != verified.authority.plan.JobID ||
		verified.observation.RequestsMade != 1 || verified.ended.Before(verified.started) ||
		verified.observation.Final != verified.authority.prepared.final {
		return false
	}
	if verified.before != nil && (!verified.before.Verified() || verified.authority != verified.before.authority ||
		!sameLedgerSession(verified.session, verified.before.session) || verified.started.Before(verified.before.ended)) {
		return false
	}
	return true
}

func (verified *VerifiedCurrentAbsence) Observation() CurrentAbsenceObservation {
	if !verified.Verified() {
		return CurrentAbsenceObservation{}
	}
	return verified.observation
}

func (verified *VerifiedCurrentAbsence) Matches(useID, jobID, variantID, materializeOperationID, materializePlanID, finalObjectIdentity string) bool {
	if !verified.Verified() {
		return false
	}
	final := verified.observation.Final
	return verified.observation.UseID == useID && verified.observation.JobID == jobID && final.MetafileVariantID == variantID &&
		final.OperationID == materializeOperationID && final.MaterializePlanID == materializePlanID && final.FinalObjectIdentity == finalObjectIdentity
}

func (verified *VerifiedCurrentUse) StableWith(after *VerifiedCurrentUse) bool {
	if !verified.Verified() || !after.Verified() {
		return false
	}
	beforeObservation, afterObservation := verified.observation, after.observation
	return verified.authority.useID == after.authority.useID && beforeObservation.UseID == afterObservation.UseID &&
		sameLedgerSession(verified.session, after.session) && verified.jobKey == after.jobKey &&
		beforeObservation.Driver == afterObservation.Driver &&
		beforeObservation.JobID == afterObservation.JobID && beforeObservation.FileLayoutID == afterObservation.FileLayoutID &&
		beforeObservation.CompleteFileSnapshotID == afterObservation.CompleteFileSnapshotID &&
		beforeObservation.ActivationOperationID == afterObservation.ActivationOperationID &&
		beforeObservation.ActivationPlanID == afterObservation.ActivationPlanID &&
		beforeObservation.TerminalMarkerID == afterObservation.TerminalMarkerID &&
		beforeObservation.JobState == afterObservation.JobState && beforeObservation.JobProgress == afterObservation.JobProgress &&
		beforeObservation.Final == afterObservation.Final && !after.started.Before(verified.ended)
}

func (verified *VerifiedCurrentUse) stableAcrossStop(after *VerifiedCurrentUse) bool {
	if !verified.Verified() || !after.Verified() {
		return false
	}
	beforeObservation, afterObservation := verified.observation, after.observation
	return verified.authority.useID == after.authority.useID && beforeObservation.UseID == afterObservation.UseID &&
		sameLedgerSession(verified.session, after.session) && verified.jobKey == after.jobKey &&
		beforeObservation.Driver == afterObservation.Driver && beforeObservation.JobID == afterObservation.JobID &&
		beforeObservation.FileLayoutID == afterObservation.FileLayoutID &&
		beforeObservation.CompleteFileSnapshotID == afterObservation.CompleteFileSnapshotID &&
		beforeObservation.ActivationOperationID == afterObservation.ActivationOperationID &&
		beforeObservation.ActivationPlanID == afterObservation.ActivationPlanID &&
		beforeObservation.TerminalMarkerID == afterObservation.TerminalMarkerID &&
		beforeObservation.JobProgress == afterObservation.JobProgress && beforeObservation.AllSelected == afterObservation.AllSelected &&
		beforeObservation.AllComplete == afterObservation.AllComplete && beforeObservation.FileLimits == afterObservation.FileLimits &&
		beforeObservation.Final == afterObservation.Final && !after.started.Before(verified.ended)
}

func sameLedgerSession(left, right downloader.LedgerSession) bool {
	if left == nil || right == nil {
		return false
	}
	leftValue, rightValue := reflect.ValueOf(left), reflect.ValueOf(right)
	return leftValue.IsValid() && rightValue.IsValid() && leftValue.Type() == rightValue.Type() && leftValue.Type().Comparable() && leftValue.Interface() == rightValue.Interface()
}

func validCurrentJobKey(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for index := range value {
		if value[index] < 0x21 || value[index] > 0x7e {
			return false
		}
	}
	return true
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
