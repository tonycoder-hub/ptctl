package clientactivate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

type legacyPlanV1 struct {
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
	TargetRootIdentity     string                                  `json:"target_root_identity"`
	FinalObjectIdentity    string                                  `json:"final_object_identity"`
	MultiFile              bool                                    `json:"multi_file"`
	ManifestFiles          int                                     `json:"manifest_files"`
	ContentBytes           int64                                   `json:"content_bytes"`
	FileLimits             downloader.JobFileLedgerLimits          `json:"file_limits"`
}

type legacyActivationCompletionV1 struct {
	Schema                 string      `json:"schema"`
	OperationID            OperationID `json:"operation_id"`
	PlanID                 string      `json:"plan_id"`
	RecheckCompletionID    MarkerID    `json:"recheck_completion_id"`
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

type legacyRetentionIntentV1 struct {
	Schema                 string                `json:"schema"`
	OperationID            OperationID           `json:"operation_id"`
	OperationRootIdentity  string                `json:"operation_root_identity"`
	TargetRootIdentity     string                `json:"target_root_identity"`
	PlanID                 string                `json:"plan_id"`
	IntentID               MarkerID              `json:"intent_id"`
	Intent                 Intent                `json:"intent"`
	RecheckCompletionID    MarkerID              `json:"recheck_completion_id"`
	RecheckCompletion      RecheckCompletion     `json:"recheck_completion"`
	ActivationCompletionID MarkerID              `json:"activation_completion_id,omitempty"`
	ActivationCompletion   *ActivationCompletion `json:"activation_completion,omitempty"`
	Markers                []RetainedMarkerLink  `json:"markers"`
	Basis                  string                `json:"basis"`
}

func TestExistingActivationCanonicalBytesRemainV1Compatible(t *testing.T) {
	fixture := makeTerminalActivation(t, true)
	verified, _, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.activation.targetRoot, OperationID: fixture.operation, ExpectedPlanID: fixture.planID,
	})
	if err != nil || verified == nil || !verified.Verified() || verified.authority.activation == nil {
		t.Fatalf("verified=%v err=%v", verified != nil && verified.Verified(), err)
	}
	plan := verified.Plan()
	currentPlan, err := encodeCanonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	legacyPlan, err := encodeCanonical(legacyPlanFromCurrent(plan))
	if err != nil || !bytes.Equal(currentPlan, legacyPlan) {
		t.Fatalf("existing plan canonical bytes changed\ncurrent=%s\nlegacy=%s\nerr=%v", currentPlan, legacyPlan, err)
	}
	legacyPlanDigest := sha256.Sum256(append([]byte("ptctl-client-activation-plan-v1\x00"), legacyPlan...))
	if got := hex.EncodeToString(legacyPlanDigest[:12]); got != fixture.planID {
		t.Fatalf("legacy plan ID=%s want=%s", got, fixture.planID)
	}

	completion := *verified.authority.activation
	currentCompletion, _, err := encodeActivationCompletion(completion)
	if err != nil {
		t.Fatal(err)
	}
	legacyCompletion, err := encodeCanonical(legacyActivationCompletionV1{
		Schema: completion.Schema, OperationID: completion.OperationID, PlanID: completion.PlanID,
		RecheckCompletionID: completion.RecheckCompletionID, StartAttemptID: completion.StartAttemptID,
		ObservedAtStart: completion.ObservedAtStart, ObservedAtEnd: completion.ObservedAtEnd,
		JobID: completion.JobID, JobState: completion.JobState, JobProgress: completion.JobProgress,
		FileLayoutID: completion.FileLayoutID, CompleteFileSnapshotID: completion.CompleteFileSnapshotID,
		FinalObjectIdentity: completion.FinalObjectIdentity, FinalVerificationBasis: completion.FinalVerificationBasis,
	})
	if err != nil || !bytes.Equal(currentCompletion, legacyCompletion) {
		t.Fatalf("existing completion canonical bytes changed\ncurrent=%s\nlegacy=%s\nerr=%v", currentCompletion, legacyCompletion, err)
	}

	if _, err := Prune(context.Background(), fixture.options()); err != nil {
		t.Fatal(err)
	}
	directory, _ := operationDirectoryName(fixture.operation)
	retentionRaw, err := os.ReadFile(filepath.Join(fixture.activation.targetRoot, directory, retentionDirectoryName, retentionIntentName))
	if err != nil {
		t.Fatal(err)
	}
	marker, _, err := DecodeRetentionIntent(bytes.NewReader(retentionRaw))
	if err != nil || marker.RecheckCompletion == nil {
		t.Fatalf("decode retained marker err=%v", err)
	}
	legacyRetention, err := encodeCanonical(legacyRetentionIntentV1{
		Schema: marker.Schema, OperationID: marker.OperationID, OperationRootIdentity: marker.OperationRootIdentity,
		TargetRootIdentity: marker.TargetRootIdentity, PlanID: marker.PlanID, IntentID: marker.IntentID, Intent: marker.Intent,
		RecheckCompletionID: marker.RecheckCompletionID, RecheckCompletion: *marker.RecheckCompletion,
		ActivationCompletionID: marker.ActivationCompletionID, ActivationCompletion: marker.ActivationCompletion,
		Markers: marker.Markers, Basis: marker.Basis,
	})
	if err != nil || !bytes.Equal(retentionRaw, legacyRetention) {
		t.Fatalf("existing retention canonical bytes changed\ncurrent=%s\nlegacy=%s\nerr=%v", retentionRaw, legacyRetention, err)
	}
}

func legacyPlanFromCurrent(plan Plan) legacyPlanV1 {
	return legacyPlanV1{
		Schema: plan.Schema, Action: plan.Action, Driver: plan.Driver, ClientConfigID: plan.ClientConfigID, Control: plan.Control,
		PathMappingID: plan.PathMappingID, ClientPathSemantics: plan.ClientPathSemantics,
		ExpectedSavePathRef: plan.ExpectedSavePathRef, ExpectedContentPathRef: plan.ExpectedContentPathRef,
		ExpectedFileLayoutID: plan.ExpectedFileLayoutID, JobID: plan.JobID, MetafileVariantID: plan.MetafileVariantID,
		InfoHashV1: plan.InfoHashV1, InfoHashV2: plan.InfoHashV2, MaterializeOperationID: plan.MaterializeOperationID,
		MaterializePlanID: plan.MaterializePlanID, AdoptionOperationID: plan.AdoptionOperationID,
		AdoptionPlanID: plan.AdoptionPlanID, AdoptionCompletionID: plan.AdoptionCompletionID,
		TargetRootIdentity: plan.TargetRootIdentity, FinalObjectIdentity: plan.FinalObjectIdentity, MultiFile: plan.MultiFile,
		ManifestFiles: plan.ManifestFiles, ContentBytes: plan.ContentBytes, FileLimits: plan.FileLimits,
	}
}
