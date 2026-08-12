// Package clientactivate coordinates explicit recheck and optional start of a
// previously journaled stopped-job adoption. Downloader claims remain separate
// from the same-invocation exact filesystem proof.
package clientactivate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const (
	PlanSchemaV1                  = "ptctl.client-activation-plan/v1"
	IntentSchemaV1                = "ptctl.client-activation-intent/v1"
	AttemptSchemaV1               = "ptctl.client-activation-attempt/v1"
	RecheckStartedSchemaV1        = "ptctl.client-recheck-started/v1"
	RecheckCompleteSchemaV1       = "ptctl.client-recheck-complete/v1"
	ActivationSchemaV1            = "ptctl.client-activation-complete/v1"
	DriverQBittorrent             = downloader.DriverQBittorrent
	DriverTransmission            = downloader.DriverTransmission
	ActionRecheckOnly             = "recheck_only"
	ActionRecheckThenStart        = "recheck_then_start"
	ActionStartAfterStop          = "start_after_stop"
	AttemptActionRecheck          = "recheck"
	AttemptActionStart            = "start"
	maximumActionAttempts         = 3
	maximumMarkerBytes      int64 = 64 << 10
)

var (
	ErrPolicy                   = errors.New("client activation is blocked by policy")
	ErrIntegrity                = errors.New("client activation state failed integrity validation")
	ErrOperationNotFound        = errors.New("client activation operation was not found")
	ErrInitializationIncomplete = errors.New("client activation journal initialization is incomplete")
	ErrRequestUnknown           = errors.New("client activation request result is unknown")
)

type Plan struct {
	Schema                 string                                  `json:"schema"`
	Action                 string                                  `json:"action"`
	Driver                 string                                  `json:"driver"`
	ClientConfigID         string                                  `json:"client_config_id"`
	Control                downloader.ExistingJobControlDescriptor `json:"control"`
	PathMappingID          string                                  `json:"path_mapping_id"`
	ClientPathSemantics    string                                  `json:"client_path_semantics"`
	ExpectedSavePathRef    string                                  `json:"expected_save_path_ref"`
	ExpectedContentPathRef string                                  `json:"expected_content_path_ref"`
	ExpectedFileLayoutID   string                                  `json:"expected_file_layout_id"`
	JobID                  string                                  `json:"job_id"`
	MetafileVariantID      string                                  `json:"metafile_variant_id"`
	InfoHashV1             string                                  `json:"info_hash_v1,omitempty"`
	InfoHashV2             string                                  `json:"info_hash_v2,omitempty"`
	MaterializeOperationID string                                  `json:"materialize_operation_id"`
	MaterializePlanID      string                                  `json:"materialize_plan_id"`
	AdoptionOperationID    string                                  `json:"adoption_operation_id"`
	AdoptionPlanID         string                                  `json:"adoption_plan_id"`
	AdoptionCompletionID   string                                  `json:"adoption_completion_id"`
	TerminalStop           *TerminalStopLink                       `json:"terminal_stop,omitempty"`
	TargetRootIdentity     string                                  `json:"target_root_identity"`
	FinalObjectIdentity    string                                  `json:"final_object_identity"`
	MultiFile              bool                                    `json:"multi_file"`
	ManifestFiles          int                                     `json:"manifest_files"`
	ContentBytes           int64                                   `json:"content_bytes"`
	FileLimits             downloader.JobFileLedgerLimits          `json:"file_limits"`
}

func (plan Plan) Validate() error {
	if plan.Schema != PlanSchemaV1 ||
		(plan.Action != ActionRecheckOnly && plan.Action != ActionRecheckThenStart && plan.Action != ActionStartAfterStop) ||
		!canonicalSHA256ID(plan.ClientConfigID) || !canonicalSHA256ID(plan.PathMappingID) ||
		!canonicalSHA256ID(plan.ExpectedSavePathRef) || !canonicalSHA256ID(plan.ExpectedContentPathRef) ||
		!canonicalSHA256ID(plan.ExpectedFileLayoutID) || !canonicalSHA256ID(plan.JobID) ||
		!canonicalSHA256ID(plan.MetafileVariantID) || !canonicalSHA256ID(plan.AdoptionCompletionID) ||
		!canonicalSHA256ID(plan.MaterializeOperationID) || !canonicalSHA256ID(plan.AdoptionOperationID) ||
		!canonicalPlanID(plan.MaterializePlanID) || !canonicalPlanID(plan.AdoptionPlanID) ||
		(plan.ClientPathSemantics != "posix_exact" && plan.ClientPathSemantics != "windows_exact") ||
		plan.ManifestFiles <= 0 || plan.ContentBytes < 0 || plan.FileLimits.Validate() != nil ||
		plan.Control.Validate() != nil {
		return fmt.Errorf("%w: client activation plan is invalid", ErrPolicy)
	}
	if plan.Action == ActionStartAfterStop {
		if plan.TerminalStop == nil || plan.TerminalStop.Validate() != nil {
			return fmt.Errorf("%w: client activation terminal-stop lineage is invalid", ErrPolicy)
		}
	} else if plan.TerminalStop != nil {
		return fmt.Errorf("%w: recheck activation unexpectedly contains terminal-stop lineage", ErrPolicy)
	}
	identity := downloader.TypedIdentity{InfoHashV1: plan.InfoHashV1, InfoHashV2: plan.InfoHashV2}
	policy, supported := downloader.DescribeExistingJobControlDriver(plan.Driver)
	if identity.Validate() != nil || !supported || !policy.SupportsIdentity(identity) || plan.Control.Driver != plan.Driver {
		return fmt.Errorf("%w: client activation typed identity is invalid", ErrPolicy)
	}
	if identity, err := fsbind.ParseIdentity(plan.TargetRootIdentity); err != nil || identity.IsZero() {
		return fmt.Errorf("%w: target root identity is invalid", ErrPolicy)
	}
	if identity, err := fsbind.ParseIdentity(plan.FinalObjectIdentity); err != nil || identity.IsZero() {
		return fmt.Errorf("%w: final object identity is invalid", ErrPolicy)
	}
	return nil
}

type OperationID string

func ParseOperationID(value string) (OperationID, error) {
	if !canonicalSHA256ID(value) {
		return "", fmt.Errorf("invalid client activation operation ID")
	}
	return OperationID(value), nil
}

func (id OperationID) String() string { return string(id) }

type MarkerID string

func parseMarkerID(value string) (MarkerID, error) {
	if !canonicalSHA256ID(value) {
		return "", fmt.Errorf("invalid client activation marker ID")
	}
	return MarkerID(value), nil
}

func (id MarkerID) String() string { return string(id) }

type Intent struct {
	Schema                string      `json:"schema"`
	OperationID           OperationID `json:"operation_id"`
	OperationRootIdentity string      `json:"operation_root_identity"`
	PlanID                string      `json:"plan_id"`
	Plan                  Plan        `json:"plan"`
}

func (intent Intent) Validate() error {
	if intent.Schema != IntentSchemaV1 || intent.OperationRootIdentity == "" || intent.Plan.Validate() != nil || !canonicalPlanID(intent.PlanID) {
		return fmt.Errorf("%w: activation intent is invalid", ErrIntegrity)
	}
	if identity, err := fsbind.ParseIdentity(intent.OperationRootIdentity); err != nil || identity.IsZero() {
		return fmt.Errorf("%w: activation operation root identity is invalid", ErrIntegrity)
	}
	computed, err := PlanID(intent.Plan)
	if err != nil || computed != intent.PlanID || OperationIDForPlan(computed) != intent.OperationID {
		return fmt.Errorf("%w: activation intent identity disagrees", ErrIntegrity)
	}
	return nil
}

type Attempt struct {
	Schema            string      `json:"schema"`
	OperationID       OperationID `json:"operation_id"`
	Action            string      `json:"action"`
	Sequence          int         `json:"sequence"`
	PreviousAttemptID string      `json:"previous_attempt_id,omitempty"`
	PlanID            string      `json:"plan_id"`
	PrerequisiteID    string      `json:"prerequisite_id"`
	ObservedAtStart   time.Time   `json:"observed_at_start"`
	ObservedAtEnd     time.Time   `json:"observed_at_end"`
	JobID             string      `json:"job_id"`
	BeforeState       string      `json:"before_state"`
	BeforeProgress    float64     `json:"before_progress"`
	FileLayoutID      string      `json:"file_layout_id"`
}

func (attempt Attempt) Validate() error {
	if attempt.Schema != AttemptSchemaV1 || (attempt.Action != AttemptActionRecheck && attempt.Action != AttemptActionStart) ||
		attempt.Sequence <= 0 || attempt.Sequence > maximumActionAttempts || !canonicalPlanID(attempt.PlanID) ||
		!canonicalSHA256ID(attempt.PrerequisiteID) || !canonicalSHA256ID(attempt.JobID) ||
		!canonicalSHA256ID(attempt.FileLayoutID) || attempt.ObservedAtStart.IsZero() ||
		attempt.ObservedAtEnd.Before(attempt.ObservedAtStart) || math.IsNaN(attempt.BeforeProgress) ||
		math.IsInf(attempt.BeforeProgress, 0) || attempt.BeforeProgress < 0 || attempt.BeforeProgress > 1 {
		return fmt.Errorf("%w: activation request intent is invalid", ErrIntegrity)
	}
	if _, err := ParseOperationID(attempt.OperationID.String()); err != nil {
		return fmt.Errorf("%w: activation request operation is invalid", ErrIntegrity)
	}
	if attempt.Sequence == 1 && attempt.PreviousAttemptID != "" {
		return fmt.Errorf("%w: first activation request has a predecessor", ErrIntegrity)
	}
	if attempt.Sequence > 1 {
		if _, err := parseMarkerID(attempt.PreviousAttemptID); err != nil {
			return fmt.Errorf("%w: activation request predecessor is invalid", ErrIntegrity)
		}
	}
	if attempt.Action == AttemptActionRecheck && !stoppedState(attempt.BeforeState) {
		return fmt.Errorf("%w: recheck request was not bracketed by a stopped job", ErrIntegrity)
	}
	if attempt.Action == AttemptActionStart && (!completeStoppedState(attempt.BeforeState) || attempt.BeforeProgress != 1) {
		return fmt.Errorf("%w: start request was not bracketed by a complete stopped job", ErrIntegrity)
	}
	return nil
}

type RecheckStarted struct {
	Schema              string      `json:"schema"`
	OperationID         OperationID `json:"operation_id"`
	PlanID              string      `json:"plan_id"`
	AttemptID           MarkerID    `json:"attempt_id"`
	ObservedAtStart     time.Time   `json:"observed_at_start"`
	ObservedAtEnd       time.Time   `json:"observed_at_end"`
	JobID               string      `json:"job_id"`
	JobState            string      `json:"job_state"`
	FileLayoutID        string      `json:"file_layout_id"`
	FinalObjectIdentity string      `json:"final_object_identity"`
}

func (started RecheckStarted) Validate() error {
	if started.Schema != RecheckStartedSchemaV1 || !canonicalPlanID(started.PlanID) ||
		!canonicalSHA256ID(started.JobID) || !canonicalSHA256ID(started.FileLayoutID) ||
		started.ObservedAtStart.IsZero() || started.ObservedAtEnd.Before(started.ObservedAtStart) || !checkingState(started.JobState) {
		return fmt.Errorf("%w: recheck-started observation is invalid", ErrIntegrity)
	}
	if _, err := ParseOperationID(started.OperationID.String()); err != nil {
		return fmt.Errorf("%w: recheck-started operation is invalid", ErrIntegrity)
	}
	if _, err := parseMarkerID(started.AttemptID.String()); err != nil {
		return fmt.Errorf("%w: recheck-started request is invalid", ErrIntegrity)
	}
	if identity, err := fsbind.ParseIdentity(started.FinalObjectIdentity); err != nil || identity.IsZero() {
		return fmt.Errorf("%w: recheck-started final identity is invalid", ErrIntegrity)
	}
	return nil
}

type RecheckCompletion struct {
	Schema                 string      `json:"schema"`
	OperationID            OperationID `json:"operation_id"`
	PlanID                 string      `json:"plan_id"`
	AttemptID              MarkerID    `json:"attempt_id"`
	StartedID              string      `json:"started_id,omitempty"`
	Basis                  string      `json:"basis"`
	ObservedAtStart        time.Time   `json:"observed_at_start"`
	ObservedAtEnd          time.Time   `json:"observed_at_end"`
	JobID                  string      `json:"job_id"`
	JobState               string      `json:"job_state"`
	JobProgress            float64     `json:"job_progress"`
	FileLayoutID           string      `json:"file_layout_id"`
	CompleteFileSnapshotID string      `json:"complete_file_snapshot_id"`
	FinalObjectIdentity    string      `json:"final_object_identity"`
	FinalVerificationBasis string      `json:"final_verification_basis"`
}

func (completion RecheckCompletion) Validate() error {
	validBasis := completion.Basis == "durable_checking_observation_then_complete" || completion.Basis == "same_invocation_incomplete_to_complete_transition"
	if completion.Schema != RecheckCompleteSchemaV1 || !canonicalPlanID(completion.PlanID) || !validBasis ||
		!canonicalSHA256ID(completion.JobID) || !canonicalSHA256ID(completion.FileLayoutID) ||
		!canonicalSHA256ID(completion.CompleteFileSnapshotID) || completion.ObservedAtStart.IsZero() ||
		completion.ObservedAtEnd.Before(completion.ObservedAtStart) || !completeStoppedState(completion.JobState) || completion.JobProgress != 1 ||
		completion.FinalVerificationBasis != "same_invocation_post_recheck_exact_reverification" {
		return fmt.Errorf("%w: recheck completion is invalid", ErrIntegrity)
	}
	if _, err := ParseOperationID(completion.OperationID.String()); err != nil {
		return fmt.Errorf("%w: recheck completion operation is invalid", ErrIntegrity)
	}
	if _, err := parseMarkerID(completion.AttemptID.String()); err != nil {
		return fmt.Errorf("%w: recheck completion request is invalid", ErrIntegrity)
	}
	if completion.Basis == "durable_checking_observation_then_complete" {
		if _, err := parseMarkerID(completion.StartedID); err != nil {
			return fmt.Errorf("%w: recheck completion started observation is invalid", ErrIntegrity)
		}
	} else if completion.StartedID != "" {
		return fmt.Errorf("%w: transition completion unexpectedly names a started observation", ErrIntegrity)
	}
	if identity, err := fsbind.ParseIdentity(completion.FinalObjectIdentity); err != nil || identity.IsZero() {
		return fmt.Errorf("%w: recheck completion final identity is invalid", ErrIntegrity)
	}
	return nil
}

type ActivationCompletion struct {
	Schema                 string      `json:"schema"`
	OperationID            OperationID `json:"operation_id"`
	PlanID                 string      `json:"plan_id"`
	RecheckCompletionID    MarkerID    `json:"recheck_completion_id,omitempty"`
	StopCompletionID       MarkerID    `json:"stop_completion_id,omitempty"`
	StartAttemptID         MarkerID    `json:"start_attempt_id"`
	ObservedAtStart        time.Time   `json:"observed_at_start"`
	ObservedAtEnd          time.Time   `json:"observed_at_end"`
	JobID                  string      `json:"job_id"`
	JobState               string      `json:"job_state"`
	JobProgress            float64     `json:"job_progress"`
	FileLayoutID           string      `json:"file_layout_id"`
	CompleteFileSnapshotID string      `json:"complete_file_snapshot_id"`
	FinalObjectIdentity    string      `json:"final_object_identity"`
	FinalVerificationBasis string      `json:"final_verification_basis"`
}

func (completion ActivationCompletion) Validate() error {
	if completion.Schema != ActivationSchemaV1 || !canonicalPlanID(completion.PlanID) ||
		!canonicalSHA256ID(completion.JobID) || !canonicalSHA256ID(completion.FileLayoutID) ||
		!canonicalSHA256ID(completion.CompleteFileSnapshotID) || completion.ObservedAtStart.IsZero() ||
		completion.ObservedAtEnd.Before(completion.ObservedAtStart) || !startedState(completion.JobState) || completion.JobProgress != 1 ||
		completion.FinalVerificationBasis != "same_invocation_post_start_exact_reverification" {
		return fmt.Errorf("%w: activation completion is invalid", ErrIntegrity)
	}
	if _, err := ParseOperationID(completion.OperationID.String()); err != nil {
		return fmt.Errorf("%w: activation completion operation is invalid", ErrIntegrity)
	}
	hasRecheckPrerequisite := completion.RecheckCompletionID != ""
	hasStopPrerequisite := completion.StopCompletionID != ""
	if hasRecheckPrerequisite == hasStopPrerequisite {
		return fmt.Errorf("%w: activation completion prerequisite is ambiguous", ErrIntegrity)
	}
	if completion.RecheckCompletionID != "" {
		if _, err := parseMarkerID(completion.RecheckCompletionID.String()); err != nil {
			return fmt.Errorf("%w: activation completion recheck authority is invalid", ErrIntegrity)
		}
	}
	if completion.StopCompletionID != "" {
		if _, err := parseMarkerID(completion.StopCompletionID.String()); err != nil {
			return fmt.Errorf("%w: activation completion stop authority is invalid", ErrIntegrity)
		}
	}
	if _, err := parseMarkerID(completion.StartAttemptID.String()); err != nil {
		return fmt.Errorf("%w: activation completion request is invalid", ErrIntegrity)
	}
	if identity, err := fsbind.ParseIdentity(completion.FinalObjectIdentity); err != nil || identity.IsZero() {
		return fmt.Errorf("%w: activation completion final identity is invalid", ErrIntegrity)
	}
	return nil
}

func PlanID(plan Plan) (string, error) {
	if err := plan.Validate(); err != nil {
		return "", err
	}
	raw, err := encodeCanonical(plan)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte("ptctl-client-activation-plan-v1\x00"), raw...))
	return hex.EncodeToString(digest[:12]), nil
}

func OperationIDForPlan(planID string) OperationID {
	if !canonicalPlanID(planID) {
		return ""
	}
	digest := sha256.Sum256([]byte("ptctl-client-activation-operation-v1\x00" + planID))
	return OperationID("sha256:" + hex.EncodeToString(digest[:]))
}

func canonicalSHA256ID(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	digest := strings.TrimPrefix(value, "sha256:")
	if strings.ToLower(digest) != digest {
		return false
	}
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size
}

func canonicalPlanID(value string) bool {
	if len(value) != 24 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 12
}

func canonicalHex(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded)*2 == length
}

func stoppedState(value string) bool {
	switch value {
	case "pausedUP", "pausedDL", "stoppedUP", "stoppedDL":
		return true
	default:
		return false
	}
}

func completeStoppedState(value string) bool { return value == "pausedUP" || value == "stoppedUP" }

func checkingState(value string) bool {
	return value == "checkingUP" || value == "checkingDL" || value == "checkingResumeData"
}

func startedState(value string) bool {
	switch value {
	case "uploading", "queuedUP", "stalledUP", "forcedUP":
		return true
	default:
		return false
	}
}

func normalizedClientState(value string) string {
	switch value {
	case "error", "missingFiles", "uploading", "pausedUP", "queuedUP", "stalledUP", "checkingUP", "forcedUP", "stoppedUP",
		"allocating", "downloading", "metaDL", "forcedMetaDL", "pausedDL", "queuedDL", "stalledDL", "checkingDL", "forcedDL", "stoppedDL",
		"checkingResumeData", "moving", "unknown":
		return value
	default:
		return "unknown"
	}
}
