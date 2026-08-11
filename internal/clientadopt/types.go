// Package clientadopt coordinates one exact, already-materialized layout with
// a downloader. One action adds an absent built-in downloader job in stopped
// mode. A second, observation-only action adopts one already-existing stopped
// job only after two exact typed/path observations bracket another exact final
// verification. Neither action changes an existing job, rechecks, moves,
// deletes, or retires data.
package clientadopt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

const (
	PlanSchemaV1               = "ptctl.client-adopt-plan/v1"
	IntentSchemaV1             = "ptctl.client-adopt-intent/v1"
	AttemptSchemaV1            = "ptctl.client-adopt-attempt/v1"
	CompletionSchemaV1         = "ptctl.client-adopt-completion/v1"
	ActionAddStopped           = "add_stopped"
	ActionAdoptExistingStopped = "adopt_existing_stopped"
	DriverQBittorrent          = "qbittorrent"
	DriverTransmission         = "transmission"

	operationDirectoryPrefix = materialize.ClientAdoptOperationDirectoryPrefix
	maximumAttempts          = 3
	maximumMarkerBytes       = int64(32 << 10)
)

var (
	ErrPolicy                   = errors.New("client adoption is blocked by policy")
	ErrIntegrity                = errors.New("client adoption state failed integrity validation")
	ErrOperationNotFound        = errors.New("client adoption operation was not found")
	ErrInitializationIncomplete = errors.New("client adoption journal initialization is incomplete")
	ErrRequestUnknown           = errors.New("client add request result is unknown")
)

type Plan struct {
	Schema                    string `json:"schema"`
	Action                    string `json:"action"`
	Driver                    string `json:"driver"`
	ClientConfigID            string `json:"client_config_id"`
	PathMappingID             string `json:"path_mapping_id"`
	ClientPathSemantics       string `json:"client_path_semantics"`
	ExpectedSavePathRef       string `json:"expected_save_path_ref"`
	ExpectedContentPathRef    string `json:"expected_content_path_ref"`
	MetafileVariantID         string `json:"metafile_variant_id"`
	MetafileBytes             int64  `json:"metafile_bytes"`
	InfoHashV1                string `json:"info_hash_v1,omitempty"`
	InfoHashV2                string `json:"info_hash_v2,omitempty"`
	MaterializeOperationID    string `json:"materialize_operation_id"`
	MaterializePlanID         string `json:"materialize_plan_id"`
	TargetRootIdentity        string `json:"target_root_identity"`
	FinalObjectIdentity       string `json:"final_object_identity"`
	MultiFile                 bool   `json:"multi_file"`
	ManifestFiles             int    `json:"manifest_files"`
	ContentBytes              int64  `json:"content_bytes"`
	PriorAdoptionOperationID  string `json:"prior_adoption_operation_id,omitempty"`
	PriorAdoptionPlanID       string `json:"prior_adoption_plan_id,omitempty"`
	PriorAdoptionCompletionID string `json:"prior_adoption_completion_id,omitempty"`
}

func (plan Plan) Validate() error {
	if plan.Schema != PlanSchemaV1 || (plan.Action != ActionAddStopped && plan.Action != ActionAdoptExistingStopped) ||
		!canonicalSHA256ID(plan.ClientConfigID) || !canonicalSHA256ID(plan.PathMappingID) ||
		!canonicalSHA256ID(plan.ExpectedSavePathRef) || !canonicalSHA256ID(plan.ExpectedContentPathRef) ||
		!canonicalSHA256ID(plan.MetafileVariantID) || plan.MetafileBytes <= 0 || plan.MetafileBytes > 32<<20 ||
		!canonicalSHA256ID(plan.MaterializeOperationID) || !canonicalPlanID(plan.MaterializePlanID) ||
		plan.TargetRootIdentity == "" || plan.FinalObjectIdentity == "" || plan.ManifestFiles <= 0 || plan.ManifestFiles > 100_000 ||
		plan.ContentBytes < 0 || (plan.InfoHashV1 == "" && plan.InfoHashV2 == "") {
		return fmt.Errorf("%w: adoption plan is invalid", ErrPolicy)
	}
	if plan.ClientPathSemantics != "posix_exact" && plan.ClientPathSemantics != "windows_exact" {
		return fmt.Errorf("%w: adoption path semantics are invalid", ErrPolicy)
	}
	if err := (downloader.TypedIdentity{InfoHashV1: plan.InfoHashV1, InfoHashV2: plan.InfoHashV2}).Validate(); err != nil {
		return fmt.Errorf("%w: adoption typed identity is invalid", ErrPolicy)
	}
	typedIdentity := downloader.TypedIdentity{InfoHashV1: plan.InfoHashV1, InfoHashV2: plan.InfoHashV2}
	if !downloader.LedgerDriverSupportsIdentity(plan.Driver, typedIdentity) {
		return fmt.Errorf("%w: adoption driver cannot prove the required typed identity", ErrPolicy)
	}
	if plan.Action == ActionAddStopped {
		descriptor, ok := downloader.DescribeStoppedAddDriver(plan.Driver)
		if !ok || !descriptor.SupportsIdentity(typedIdentity) {
			return fmt.Errorf("%w: adoption driver cannot submit the required typed identity", ErrPolicy)
		}
	}
	if _, err := materialize.ParseOperationID(plan.MaterializeOperationID); err != nil {
		return fmt.Errorf("%w: materialize operation ID is invalid", ErrPolicy)
	}
	if identity, err := fsbind.ParseIdentity(plan.TargetRootIdentity); err != nil || identity.IsZero() {
		return fmt.Errorf("%w: target root identity is invalid", ErrPolicy)
	}
	if identity, err := fsbind.ParseIdentity(plan.FinalObjectIdentity); err != nil || identity.IsZero() {
		return fmt.Errorf("%w: final object identity is invalid", ErrPolicy)
	}
	priorFields := 0
	for _, value := range []string{plan.PriorAdoptionOperationID, plan.PriorAdoptionPlanID, plan.PriorAdoptionCompletionID} {
		if value != "" {
			priorFields++
		}
	}
	if priorFields != 0 && priorFields != 3 {
		return fmt.Errorf("%w: prior adoption lineage is incomplete", ErrPolicy)
	}
	if plan.Action != ActionAddStopped && priorFields != 0 {
		return fmt.Errorf("%w: observation-only adoption cannot consume prior adoption lineage", ErrPolicy)
	}
	if priorFields == 3 {
		priorOperation, err := ParseOperationID(plan.PriorAdoptionOperationID)
		if err != nil || !canonicalPlanID(plan.PriorAdoptionPlanID) || OperationIDForPlan(plan.PriorAdoptionPlanID) != priorOperation {
			return fmt.Errorf("%w: prior adoption selector is invalid", ErrPolicy)
		}
		if _, err := parseMarkerID(plan.PriorAdoptionCompletionID); err != nil {
			return fmt.Errorf("%w: prior adoption completion is invalid", ErrPolicy)
		}
	}
	return nil
}

type OperationID string

func ParseOperationID(value string) (OperationID, error) {
	if !canonicalSHA256ID(value) {
		return "", fmt.Errorf("invalid client adoption operation ID")
	}
	return OperationID(value), nil
}

func (id OperationID) String() string { return string(id) }

type MarkerID string

func parseMarkerID(value string) (MarkerID, error) {
	if !canonicalSHA256ID(value) {
		return "", fmt.Errorf("invalid client adoption marker ID")
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
		return fmt.Errorf("%w: adoption intent is invalid", ErrIntegrity)
	}
	if identity, err := fsbind.ParseIdentity(intent.OperationRootIdentity); err != nil || identity.IsZero() {
		return fmt.Errorf("%w: adoption operation root identity is invalid", ErrIntegrity)
	}
	computed, err := PlanID(intent.Plan)
	if err != nil || computed != intent.PlanID || OperationIDForPlan(computed) != intent.OperationID {
		return fmt.Errorf("%w: adoption intent identity disagrees", ErrIntegrity)
	}
	return nil
}

type Attempt struct {
	Schema               string      `json:"schema"`
	OperationID          OperationID `json:"operation_id"`
	Sequence             int         `json:"sequence"`
	PreviousAttemptID    string      `json:"previous_attempt_id,omitempty"`
	PlanID               string      `json:"plan_id"`
	ObservedAtStart      time.Time   `json:"observed_at_start"`
	ObservedAtEnd        time.Time   `json:"observed_at_end"`
	BeforeStatus         string      `json:"before_status"`
	BeforeJobID          string      `json:"before_job_id,omitempty"`
	BeforeJobState       string      `json:"before_job_state,omitempty"`
	BeforeContentPathRef string      `json:"before_content_path_ref,omitempty"`
	ObservationBasis     string      `json:"observation_basis,omitempty"`
	MetafileVariantID    string      `json:"metafile_variant_id"`
	MetafileBytes        int64       `json:"metafile_bytes"`
}

func (attempt Attempt) Validate() error {
	if attempt.Schema != AttemptSchemaV1 || attempt.Sequence <= 0 || attempt.Sequence > maximumAttempts ||
		!canonicalPlanID(attempt.PlanID) ||
		!canonicalSHA256ID(attempt.MetafileVariantID) || attempt.MetafileBytes <= 0 ||
		attempt.ObservedAtStart.IsZero() || attempt.ObservedAtEnd.Before(attempt.ObservedAtStart) {
		return fmt.Errorf("%w: adoption attempt is invalid", ErrIntegrity)
	}
	if _, err := ParseOperationID(attempt.OperationID.String()); err != nil {
		return fmt.Errorf("%w: adoption attempt operation is invalid", ErrIntegrity)
	}
	if attempt.Sequence == 1 && attempt.PreviousAttemptID != "" {
		return fmt.Errorf("%w: first adoption attempt has a predecessor", ErrIntegrity)
	}
	if attempt.Sequence > 1 {
		if _, err := parseMarkerID(attempt.PreviousAttemptID); err != nil {
			return fmt.Errorf("%w: adoption attempt predecessor is invalid", ErrIntegrity)
		}
	}
	switch attempt.BeforeStatus {
	case string(downloader.LedgerIdentityAbsent):
		if attempt.BeforeJobID != "" || attempt.BeforeJobState != "" || attempt.BeforeContentPathRef != "" || attempt.ObservationBasis != "" {
			return fmt.Errorf("%w: absent adoption attempt contains existing-job evidence", ErrIntegrity)
		}
	case string(downloader.LedgerIdentityExactUnique):
		if !canonicalSHA256ID(attempt.BeforeJobID) || !stoppedState(attempt.BeforeJobState) ||
			!canonicalSHA256ID(attempt.BeforeContentPathRef) ||
			attempt.ObservationBasis != "same_invocation_first_existing_stopped_job_observation" {
			return fmt.Errorf("%w: existing-job adoption observation is invalid", ErrIntegrity)
		}
	default:
		return fmt.Errorf("%w: adoption attempt identity status is invalid", ErrIntegrity)
	}
	return nil
}

type Completion struct {
	Schema                 string      `json:"schema"`
	OperationID            OperationID `json:"operation_id"`
	PlanID                 string      `json:"plan_id"`
	AttemptID              MarkerID    `json:"attempt_id"`
	ObservedAtStart        time.Time   `json:"observed_at_start"`
	ObservedAtEnd          time.Time   `json:"observed_at_end"`
	JobID                  string      `json:"job_id"`
	JobState               string      `json:"job_state"`
	ContentPathRef         string      `json:"content_path_ref"`
	FinalObjectIdentity    string      `json:"final_object_identity"`
	FinalVerificationBasis string      `json:"final_verification_basis"`
}

func (completion Completion) Validate() error {
	validBasis := completion.FinalVerificationBasis == "same_invocation_post_add_exact_reverification" ||
		completion.FinalVerificationBasis == "same_invocation_existing_stopped_job_bracket_and_exact_reverification"
	if completion.Schema != CompletionSchemaV1 || !canonicalPlanID(completion.PlanID) ||
		!canonicalSHA256ID(completion.JobID) || !canonicalSHA256ID(completion.ContentPathRef) ||
		completion.FinalObjectIdentity == "" || !validBasis ||
		completion.ObservedAtStart.IsZero() || completion.ObservedAtEnd.Before(completion.ObservedAtStart) || !stoppedState(completion.JobState) {
		return fmt.Errorf("%w: adoption completion is invalid", ErrIntegrity)
	}
	if _, err := ParseOperationID(completion.OperationID.String()); err != nil {
		return fmt.Errorf("%w: adoption completion operation is invalid", ErrIntegrity)
	}
	if _, err := parseMarkerID(completion.AttemptID.String()); err != nil {
		return fmt.Errorf("%w: adoption completion attempt is invalid", ErrIntegrity)
	}
	if identity, err := fsbind.ParseIdentity(completion.FinalObjectIdentity); err != nil || identity.IsZero() {
		return fmt.Errorf("%w: adoption completion final identity is invalid", ErrIntegrity)
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
	digest := sha256.Sum256(append([]byte("ptctl-client-adopt-plan-v1\x00"), raw...))
	return hex.EncodeToString(digest[:12]), nil
}

func OperationIDForPlan(planID string) OperationID {
	if !canonicalPlanID(planID) {
		return ""
	}
	digest := sha256.Sum256([]byte("ptctl-client-adopt-operation-v1\x00" + planID))
	return OperationID("sha256:" + hex.EncodeToString(digest[:]))
}

func operationDirectoryName(id OperationID) (string, error) {
	parsed, err := ParseOperationID(id.String())
	if err != nil || parsed != id {
		return "", fmt.Errorf("invalid client adoption operation ID")
	}
	return operationDirectoryPrefix + strings.TrimPrefix(id.String(), "sha256:"), nil
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

func stoppedState(value string) bool {
	switch value {
	case "pausedUP", "pausedDL", "stoppedUP", "stoppedDL":
		return true
	default:
		return false
	}
}

func attemptMatchesPlan(attempt Attempt, plan Plan) bool {
	if attempt.Validate() != nil || plan.Validate() != nil {
		return false
	}
	switch plan.Action {
	case ActionAddStopped:
		return attempt.BeforeStatus == string(downloader.LedgerIdentityAbsent)
	case ActionAdoptExistingStopped:
		return attempt.BeforeStatus == string(downloader.LedgerIdentityExactUnique) &&
			attempt.BeforeContentPathRef == plan.ExpectedContentPathRef
	default:
		return false
	}
}

func completionMatchesPlan(completion Completion, plan Plan, attempt Attempt) bool {
	if completion.Validate() != nil || !attemptMatchesPlan(attempt, plan) {
		return false
	}
	switch plan.Action {
	case ActionAddStopped:
		return completion.FinalVerificationBasis == "same_invocation_post_add_exact_reverification"
	case ActionAdoptExistingStopped:
		return completion.FinalVerificationBasis == "same_invocation_existing_stopped_job_bracket_and_exact_reverification" &&
			completion.JobID == attempt.BeforeJobID && completion.JobState == attempt.BeforeJobState &&
			completion.ContentPathRef == attempt.BeforeContentPathRef
	default:
		return false
	}
}
