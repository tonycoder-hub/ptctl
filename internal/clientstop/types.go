// Package clientstop coordinates one explicitly reviewed downloader job stop.
// Request acceptance, stopped-state observation, and exact filesystem proof
// remain separate evidence axes.
package clientstop

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const (
	PlanSchemaV1       = "ptctl.client-stop-plan/v1"
	IntentSchemaV1     = "ptctl.client-stop-intent/v1"
	AttemptSchemaV1    = "ptctl.client-stop-attempt/v1"
	ResponseSchemaV1   = "ptctl.client-stop-response/v1"
	CompletionSchemaV1 = "ptctl.client-stop-complete/v1"

	ActionStopExactJob = "stop_exact_existing_job"
	maximumAttempts    = 3
	maximumMarkerBytes = int64(64 << 10)
)

var (
	ErrPolicy                   = errors.New("client stop is blocked by policy")
	ErrIntegrity                = errors.New("client stop state failed integrity validation")
	ErrOperationNotFound        = errors.New("client stop operation was not found")
	ErrInitializationIncomplete = errors.New("client stop journal initialization is incomplete")
	ErrMarkerRecoveryRequired   = errors.New("client stop journal marker recovery is required")
	ErrRequestUnknown           = errors.New("client stop request result is unknown")
)

type Plan struct {
	Schema                 string                               `json:"schema"`
	Action                 string                               `json:"action"`
	Driver                 string                               `json:"driver"`
	ClientConfigID         string                               `json:"client_config_id"`
	Stop                   downloader.ExistingJobStopDescriptor `json:"stop"`
	UseID                  string                               `json:"use_id"`
	JobID                  string                               `json:"job_id"`
	FileLayoutID           string                               `json:"file_layout_id"`
	CompleteFileSnapshotID string                               `json:"complete_file_snapshot_id"`
	ReviewedJobState       string                               `json:"reviewed_job_state"`
	PathMappingID          string                               `json:"path_mapping_id"`
	ActivationOperationID  string                               `json:"activation_operation_id"`
	ActivationPlanID       string                               `json:"activation_plan_id"`
	ActivationTerminalID   string                               `json:"activation_terminal_id"`
	MetafileVariantID      string                               `json:"metafile_variant_id"`
	InfoHashV1             string                               `json:"info_hash_v1,omitempty"`
	InfoHashV2             string                               `json:"info_hash_v2,omitempty"`
	MaterializeOperationID string                               `json:"materialize_operation_id"`
	MaterializePlanID      string                               `json:"materialize_plan_id"`
	TargetRootIdentity     string                               `json:"target_root_identity"`
	FinalObjectIdentity    string                               `json:"final_object_identity"`
	MultiFile              bool                                 `json:"multi_file"`
	ManifestFiles          int                                  `json:"manifest_files"`
	ContentBytes           int64                                `json:"content_bytes"`
	FileLimits             downloader.JobFileLedgerLimits       `json:"file_limits"`
}

func (plan Plan) Validate() error {
	if plan.Schema != PlanSchemaV1 || plan.Action != ActionStopExactJob || !canonicalSHA256ID(plan.ClientConfigID) ||
		plan.Stop.Validate() != nil || plan.Stop.Driver != plan.Driver || !canonicalSHA256ID(plan.UseID) ||
		!canonicalSHA256ID(plan.JobID) || !canonicalSHA256ID(plan.FileLayoutID) ||
		!canonicalSHA256ID(plan.CompleteFileSnapshotID) || !startedState(plan.ReviewedJobState) ||
		!canonicalSHA256ID(plan.PathMappingID) || !canonicalSHA256ID(plan.ActivationOperationID) ||
		!canonicalPlanID(plan.ActivationPlanID) || !canonicalSHA256ID(plan.ActivationTerminalID) ||
		!canonicalSHA256ID(plan.MetafileVariantID) || !canonicalSHA256ID(plan.MaterializeOperationID) ||
		!canonicalPlanID(plan.MaterializePlanID) || plan.ManifestFiles <= 0 || plan.ContentBytes < 0 || plan.FileLimits.Validate() != nil {
		return fmt.Errorf("%w: client stop plan is invalid", ErrPolicy)
	}
	identity := downloader.TypedIdentity{InfoHashV1: plan.InfoHashV1, InfoHashV2: plan.InfoHashV2}
	policy, supported := downloader.DescribeExistingJobStopDriver(plan.Driver)
	if identity.Validate() != nil || !supported || !policy.SupportsIdentity(identity) {
		return fmt.Errorf("%w: client stop typed identity is invalid", ErrPolicy)
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
		return "", fmt.Errorf("invalid client stop operation ID")
	}
	return OperationID(value), nil
}

func (id OperationID) String() string { return string(id) }

type MarkerID string

func parseMarkerID(value string) (MarkerID, error) {
	if !canonicalSHA256ID(value) {
		return "", fmt.Errorf("invalid client stop marker ID")
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
	if intent.Schema != IntentSchemaV1 || intent.Plan.Validate() != nil || !canonicalPlanID(intent.PlanID) {
		return fmt.Errorf("%w: client stop intent is invalid", ErrIntegrity)
	}
	if identity, err := fsbind.ParseIdentity(intent.OperationRootIdentity); err != nil || identity.IsZero() {
		return fmt.Errorf("%w: client stop operation root identity is invalid", ErrIntegrity)
	}
	computed, err := PlanID(intent.Plan)
	if err != nil || computed != intent.PlanID || OperationIDForPlan(computed) != intent.OperationID {
		return fmt.Errorf("%w: client stop intent identity disagrees", ErrIntegrity)
	}
	return nil
}

type Attempt struct {
	Schema            string      `json:"schema"`
	OperationID       OperationID `json:"operation_id"`
	PlanID            string      `json:"plan_id"`
	Sequence          int         `json:"sequence"`
	PreviousAttemptID string      `json:"previous_attempt_id,omitempty"`
	UseID             string      `json:"use_id"`
	JobID             string      `json:"job_id"`
	FileLayoutID      string      `json:"file_layout_id"`
	FileSnapshotID    string      `json:"file_snapshot_id"`
	ObservedAtStart   time.Time   `json:"observed_at_start"`
	ObservedAtEnd     time.Time   `json:"observed_at_end"`
	JobState          string      `json:"job_state"`
	JobProgress       float64     `json:"job_progress"`
}

func (attempt Attempt) Validate() error {
	if attempt.Schema != AttemptSchemaV1 || attempt.Sequence <= 0 || attempt.Sequence > maximumAttempts ||
		!canonicalPlanID(attempt.PlanID) || !canonicalSHA256ID(attempt.UseID) || !canonicalSHA256ID(attempt.JobID) ||
		!canonicalSHA256ID(attempt.FileLayoutID) || !canonicalSHA256ID(attempt.FileSnapshotID) ||
		attempt.ObservedAtStart.IsZero() || attempt.ObservedAtEnd.Before(attempt.ObservedAtStart) ||
		attempt.JobProgress != 1 || !startedState(attempt.JobState) {
		return fmt.Errorf("%w: client stop attempt is invalid", ErrIntegrity)
	}
	if _, err := ParseOperationID(attempt.OperationID.String()); err != nil {
		return fmt.Errorf("%w: client stop attempt operation is invalid", ErrIntegrity)
	}
	if attempt.Sequence == 1 && attempt.PreviousAttemptID != "" {
		return fmt.Errorf("%w: first client stop attempt has a predecessor", ErrIntegrity)
	}
	if attempt.Sequence > 1 {
		if _, err := parseMarkerID(attempt.PreviousAttemptID); err != nil {
			return fmt.Errorf("%w: client stop attempt predecessor is invalid", ErrIntegrity)
		}
	}
	return nil
}

type Response struct {
	Schema            string      `json:"schema"`
	OperationID       OperationID `json:"operation_id"`
	PlanID            string      `json:"plan_id"`
	AttemptID         MarkerID    `json:"attempt_id"`
	Effect            string      `json:"effect"`
	ObservedAtStart   time.Time   `json:"observed_at_start"`
	ObservedAtEnd     time.Time   `json:"observed_at_end"`
	Complete          bool        `json:"complete"`
	RequestsAttempted int         `json:"requests_attempted"`
	AutomaticRetries  int         `json:"automatic_retries"`
	RedirectsFollowed int         `json:"redirects_followed"`
	RequestBytes      int64       `json:"request_bytes"`
	RequestBytesKnown bool        `json:"request_bytes_known"`
	RequestID         int64       `json:"request_id,omitempty"`
	StopReason        string      `json:"stop_reason,omitempty"`
}

func (response Response) Validate() error {
	if response.Schema != ResponseSchemaV1 || !canonicalPlanID(response.PlanID) || response.Effect != downloader.StopEffect ||
		response.ObservedAtStart.IsZero() || response.ObservedAtEnd.Before(response.ObservedAtStart) ||
		response.RequestsAttempted < 0 || response.RequestsAttempted > 1 || response.AutomaticRetries != 0 ||
		response.RedirectsFollowed != 0 || response.RequestBytes < 0 || response.RequestID < 0 || !validStopReason(response.StopReason) {
		return fmt.Errorf("%w: client stop response is invalid", ErrIntegrity)
	}
	if _, err := ParseOperationID(response.OperationID.String()); err != nil {
		return fmt.Errorf("%w: client stop response operation is invalid", ErrIntegrity)
	}
	if _, err := parseMarkerID(response.AttemptID.String()); err != nil {
		return fmt.Errorf("%w: client stop response attempt is invalid", ErrIntegrity)
	}
	if response.Complete {
		if response.RequestsAttempted != 1 || !response.RequestBytesKnown || response.RequestBytes <= 0 || response.StopReason != "" {
			return fmt.Errorf("%w: accepted client stop response is contradictory", ErrIntegrity)
		}
	} else if response.StopReason == "" {
		return fmt.Errorf("%w: incomplete client stop response lacks a stop reason", ErrIntegrity)
	}
	if response.RequestsAttempted == 0 && (response.RequestBytesKnown || response.RequestBytes != 0 || response.RequestID != 0) {
		return fmt.Errorf("%w: unattempted client stop response contains request evidence", ErrIntegrity)
	}
	if response.RequestsAttempted == 1 && (!response.RequestBytesKnown || response.RequestBytes <= 0) {
		return fmt.Errorf("%w: attempted client stop response lacks exact request size", ErrIntegrity)
	}
	return nil
}

type Completion struct {
	Schema                 string      `json:"schema"`
	OperationID            OperationID `json:"operation_id"`
	PlanID                 string      `json:"plan_id"`
	AttemptID              MarkerID    `json:"attempt_id"`
	ResponseID             string      `json:"response_id,omitempty"`
	Basis                  string      `json:"basis"`
	UseID                  string      `json:"use_id"`
	JobID                  string      `json:"job_id"`
	FileLayoutID           string      `json:"file_layout_id"`
	CompleteFileSnapshotID string      `json:"complete_file_snapshot_id"`
	StoppedJobState        string      `json:"stopped_job_state"`
	ObservedAtStart        time.Time   `json:"observed_at_start"`
	ObservedAtEnd          time.Time   `json:"observed_at_end"`
	FinalObjectIdentity    string      `json:"final_object_identity"`
	FinalVerificationBasis string      `json:"final_verification_basis"`
}

func (completion Completion) Validate() error {
	validBasis := completion.Basis == "accepted_response_then_exact_stopped" || completion.Basis == "exact_stopped_after_unknown_attempt_causality_unproven"
	if completion.Schema != CompletionSchemaV1 || !canonicalPlanID(completion.PlanID) || !validBasis ||
		!canonicalSHA256ID(completion.UseID) || !canonicalSHA256ID(completion.JobID) || !canonicalSHA256ID(completion.FileLayoutID) ||
		!canonicalSHA256ID(completion.CompleteFileSnapshotID) || !completeStoppedState(completion.StoppedJobState) ||
		completion.ObservedAtStart.IsZero() || completion.ObservedAtEnd.Before(completion.ObservedAtStart) ||
		completion.FinalVerificationBasis != "same_invocation_post_stop_exact_final_reverification" {
		return fmt.Errorf("%w: client stop completion is invalid", ErrIntegrity)
	}
	if _, err := ParseOperationID(completion.OperationID.String()); err != nil {
		return fmt.Errorf("%w: client stop completion operation is invalid", ErrIntegrity)
	}
	if _, err := parseMarkerID(completion.AttemptID.String()); err != nil {
		return fmt.Errorf("%w: client stop completion attempt is invalid", ErrIntegrity)
	}
	if completion.ResponseID != "" {
		if _, err := parseMarkerID(completion.ResponseID); err != nil {
			return fmt.Errorf("%w: client stop completion response is invalid", ErrIntegrity)
		}
	}
	if completion.Basis == "accepted_response_then_exact_stopped" && completion.ResponseID == "" {
		return fmt.Errorf("%w: attributed client stop completion lacks a response", ErrIntegrity)
	}
	if identity, err := fsbind.ParseIdentity(completion.FinalObjectIdentity); err != nil || identity.IsZero() {
		return fmt.Errorf("%w: client stop completion final identity is invalid", ErrIntegrity)
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
	digest := sha256.Sum256(append([]byte("ptctl-client-stop-plan-v1\x00"), raw...))
	return hex.EncodeToString(digest[:12]), nil
}

func OperationIDForPlan(planID string) OperationID {
	if !canonicalPlanID(planID) {
		return ""
	}
	digest := sha256.Sum256([]byte("ptctl-client-stop-operation-v1\x00" + planID))
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

func startedState(value string) bool {
	switch value {
	case "uploading", "queuedUP", "stalledUP", "forcedUP":
		return true
	default:
		return false
	}
}

func completeStoppedState(value string) bool { return value == "pausedUP" || value == "stoppedUP" }

func validStopReason(value string) bool {
	switch value {
	case "", "context_cancelled", "job_locator_invalid", "stop_descriptor_unavailable", "session_unavailable",
		"request_build_failed", "transport_failed", "response_read_failed", "http_rejected", "response_invalid", "csrf_expired":
		return true
	default:
		return false
	}
}
