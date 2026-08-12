package clientadopt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

// TerminalRemovalReAddProof is implemented by the client-removal package only
// while one attributed terminal keep-data removal has been rebound from its
// live journal or exact retention tombstone. Serialized public reports cannot
// recreate the process-local authority behind this method.
type TerminalRemovalReAddProof interface {
	AdoptionReAddPrerequisite() (TerminalRemovalPrerequisite, bool)
}

// TerminalRemovalPrerequisite is the bounded cross-package projection needed
// to review one new stopped-add lineage after an attributed keep-data removal.
// It deliberately contains no downloader job locator or filesystem path.
type TerminalRemovalPrerequisite struct {
	Driver                 string
	OperationID            string
	PlanID                 string
	IntentID               string
	CompletionID           string
	CompletionBasis        string
	UseID                  string
	JobID                  string
	FileLayoutID           string
	CompleteFileSnapshotID string
	ClientConfigID         string
	PathMappingID          string
	ActivationOperationID  string
	ActivationPlanID       string
	ActivationTerminalID   string
	MetafileVariantID      string
	InfoHashV1             string
	InfoHashV2             string
	MaterializeOperationID string
	MaterializePlanID      string
	TargetRootIdentity     string
	FinalObjectIdentity    string
	MultiFile              bool
	ManifestFiles          int
	ContentBytes           int64
	ObservedAtStart        time.Time
	ObservedAtEnd          time.Time
	RetainedTombstone      bool
}

// TerminalRemovalLink is immutable historical lineage committed into a
// re-add-after-removal plan. It is public audit data, never execution authority.
type TerminalRemovalLink struct {
	OperationID            string `json:"operation_id"`
	PlanID                 string `json:"plan_id"`
	IntentID               string `json:"intent_id"`
	CompletionID           string `json:"completion_id"`
	CompletionBasis        string `json:"completion_basis"`
	UseID                  string `json:"use_id"`
	JobID                  string `json:"job_id"`
	FileLayoutID           string `json:"file_layout_id"`
	CompleteFileSnapshotID string `json:"complete_file_snapshot_id"`
	ObservedAtEnd          string `json:"observed_at_end"`
	ActivationOperationID  string `json:"activation_operation_id"`
	ActivationPlanID       string `json:"activation_plan_id"`
	ActivationTerminalID   string `json:"activation_terminal_id"`
}

func (value TerminalRemovalPrerequisite) validate() error {
	identity := downloader.TypedIdentity{InfoHashV1: value.InfoHashV1, InfoHashV2: value.InfoHashV2}
	descriptor, supported := downloader.DescribeStoppedAddDriver(value.Driver)
	if !supported || !descriptor.SupportsIdentity(identity) || identity.Validate() != nil ||
		!validRemovalOperationForPlan(value.OperationID, value.PlanID) ||
		!canonicalSHA256ID(value.IntentID) || !canonicalSHA256ID(value.CompletionID) ||
		value.CompletionBasis != "accepted_response_then_exact_absence" || !canonicalSHA256ID(value.UseID) ||
		!canonicalSHA256ID(value.JobID) || !canonicalSHA256ID(value.FileLayoutID) ||
		!canonicalSHA256ID(value.CompleteFileSnapshotID) || !canonicalSHA256ID(value.ClientConfigID) ||
		!canonicalSHA256ID(value.PathMappingID) || !validActivationOperationForPlan(value.ActivationOperationID, value.ActivationPlanID) ||
		!canonicalSHA256ID(value.ActivationTerminalID) || !canonicalSHA256ID(value.MetafileVariantID) ||
		!canonicalSHA256ID(value.MaterializeOperationID) || !canonicalPlanID(value.MaterializePlanID) ||
		value.ManifestFiles <= 0 || value.ManifestFiles > 100_000 || value.ContentBytes < 0 ||
		value.ObservedAtStart.IsZero() || value.ObservedAtEnd.Before(value.ObservedAtStart) {
		return fmt.Errorf("%w: terminal client-removal prerequisite is invalid", ErrPolicy)
	}
	if parsed, err := fsbind.ParseIdentity(value.TargetRootIdentity); err != nil || parsed.IsZero() {
		return fmt.Errorf("%w: terminal client-removal root identity is invalid", ErrPolicy)
	}
	if parsed, err := fsbind.ParseIdentity(value.FinalObjectIdentity); err != nil || parsed.IsZero() {
		return fmt.Errorf("%w: terminal client-removal final identity is invalid", ErrPolicy)
	}
	return nil
}

func (value TerminalRemovalPrerequisite) planLink() TerminalRemovalLink {
	return TerminalRemovalLink{
		OperationID: value.OperationID, PlanID: value.PlanID, IntentID: value.IntentID,
		CompletionID: value.CompletionID, CompletionBasis: value.CompletionBasis, UseID: value.UseID,
		JobID: value.JobID, FileLayoutID: value.FileLayoutID, CompleteFileSnapshotID: value.CompleteFileSnapshotID,
		ObservedAtEnd: value.ObservedAtEnd.UTC().Format(time.RFC3339Nano), ActivationOperationID: value.ActivationOperationID,
		ActivationPlanID: value.ActivationPlanID, ActivationTerminalID: value.ActivationTerminalID,
	}
}

func (link TerminalRemovalLink) Validate() error {
	observedEnd, err := time.Parse(time.RFC3339Nano, link.ObservedAtEnd)
	if err != nil || observedEnd.IsZero() || link.ObservedAtEnd != observedEnd.UTC().Format(time.RFC3339Nano) ||
		!validRemovalOperationForPlan(link.OperationID, link.PlanID) ||
		!canonicalSHA256ID(link.IntentID) || !canonicalSHA256ID(link.CompletionID) ||
		link.CompletionBasis != "accepted_response_then_exact_absence" || !canonicalSHA256ID(link.UseID) ||
		!canonicalSHA256ID(link.JobID) || !canonicalSHA256ID(link.FileLayoutID) ||
		!canonicalSHA256ID(link.CompleteFileSnapshotID) ||
		!validActivationOperationForPlan(link.ActivationOperationID, link.ActivationPlanID) ||
		!canonicalSHA256ID(link.ActivationTerminalID) {
		return fmt.Errorf("%w: terminal client-removal plan link is invalid", ErrPolicy)
	}
	return nil
}

func (link TerminalRemovalLink) observedEnd() time.Time {
	value, _ := time.Parse(time.RFC3339Nano, link.ObservedAtEnd)
	return value
}

func validRemovalOperationForPlan(operationID, planID string) bool {
	if !canonicalSHA256ID(operationID) || !canonicalPlanID(planID) {
		return false
	}
	digest := sha256.Sum256([]byte("ptctl-client-removal-operation-v1\x00" + planID))
	return operationID == "sha256:"+hex.EncodeToString(digest[:])
}

func validActivationOperationForPlan(operationID, planID string) bool {
	if !canonicalSHA256ID(operationID) || !canonicalPlanID(planID) {
		return false
	}
	digest := sha256.Sum256([]byte("ptctl-client-activation-operation-v1\x00" + planID))
	return operationID == "sha256:"+hex.EncodeToString(digest[:])
}
