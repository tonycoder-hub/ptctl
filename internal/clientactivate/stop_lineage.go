package clientactivate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

// TerminalStopStartProof is the code-owned process-local bridge implemented by
// the terminal client-stop package. A serialized public value cannot recreate
// the bound journal proof that backs this method.
type TerminalStopStartProof interface {
	ActivationStartPrerequisite() (TerminalStopPrerequisite, bool)
}

// TerminalStopPrerequisite is the bounded cross-package projection of one
// attributed, canonical terminal stop. It contains no downloader job locator.
type TerminalStopPrerequisite struct {
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
	StoppedJobState        string
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

// TerminalStopLink is the immutable, public lineage committed into a
// start-after-stop activation plan. It never carries process authority.
type TerminalStopLink struct {
	OperationID            string `json:"operation_id"`
	PlanID                 string `json:"plan_id"`
	IntentID               string `json:"intent_id"`
	CompletionID           string `json:"completion_id"`
	CompletionBasis        string `json:"completion_basis"`
	UseID                  string `json:"use_id"`
	CompleteFileSnapshotID string `json:"complete_file_snapshot_id"`
	StoppedJobState        string `json:"stopped_job_state"`
	ObservedAtEnd          string `json:"observed_at_end"`
	ActivationOperationID  string `json:"activation_operation_id"`
	ActivationPlanID       string `json:"activation_plan_id"`
	ActivationTerminalID   string `json:"activation_terminal_id"`
}

func (value TerminalStopPrerequisite) validate() error {
	identity := downloader.TypedIdentity{InfoHashV1: value.InfoHashV1, InfoHashV2: value.InfoHashV2}
	policy, supported := downloader.DescribeExistingJobStopDriver(value.Driver)
	if !supported || !policy.SupportsIdentity(identity) || !validStopOperationForPlan(value.OperationID, value.PlanID) ||
		!canonicalSHA256ID(value.IntentID) || !canonicalSHA256ID(value.CompletionID) ||
		value.CompletionBasis != "accepted_response_then_exact_stopped" || !canonicalSHA256ID(value.UseID) ||
		!canonicalSHA256ID(value.JobID) || !canonicalSHA256ID(value.FileLayoutID) ||
		!canonicalSHA256ID(value.CompleteFileSnapshotID) || !completeStoppedState(value.StoppedJobState) ||
		!canonicalSHA256ID(value.ClientConfigID) || !canonicalSHA256ID(value.PathMappingID) ||
		!canonicalSHA256ID(value.ActivationOperationID) || !canonicalPlanID(value.ActivationPlanID) ||
		!canonicalSHA256ID(value.ActivationTerminalID) || !canonicalSHA256ID(value.MetafileVariantID) ||
		!canonicalSHA256ID(value.MaterializeOperationID) || !canonicalPlanID(value.MaterializePlanID) ||
		value.ActivationOperationID != OperationIDForPlan(value.ActivationPlanID).String() ||
		value.ManifestFiles <= 0 || value.ContentBytes < 0 || value.ObservedAtStart.IsZero() ||
		value.ObservedAtEnd.Before(value.ObservedAtStart) || identity.Validate() != nil {
		return fmt.Errorf("%w: terminal client-stop prerequisite is invalid", ErrPolicy)
	}
	if parsed, err := fsbind.ParseIdentity(value.TargetRootIdentity); err != nil || parsed.IsZero() {
		return fmt.Errorf("%w: terminal client-stop root identity is invalid", ErrPolicy)
	}
	if parsed, err := fsbind.ParseIdentity(value.FinalObjectIdentity); err != nil || parsed.IsZero() {
		return fmt.Errorf("%w: terminal client-stop final identity is invalid", ErrPolicy)
	}
	return nil
}

func (value TerminalStopPrerequisite) planLink() TerminalStopLink {
	return TerminalStopLink{
		OperationID: value.OperationID, PlanID: value.PlanID, IntentID: value.IntentID,
		CompletionID: value.CompletionID, CompletionBasis: value.CompletionBasis, UseID: value.UseID,
		CompleteFileSnapshotID: value.CompleteFileSnapshotID, StoppedJobState: value.StoppedJobState,
		ObservedAtEnd:         value.ObservedAtEnd.UTC().Format(time.RFC3339Nano),
		ActivationOperationID: value.ActivationOperationID, ActivationPlanID: value.ActivationPlanID,
		ActivationTerminalID: value.ActivationTerminalID,
	}
}

func (link TerminalStopLink) Validate() error {
	observedEnd, err := time.Parse(time.RFC3339Nano, link.ObservedAtEnd)
	if err != nil || observedEnd.IsZero() || !validStopOperationForPlan(link.OperationID, link.PlanID) ||
		!canonicalSHA256ID(link.IntentID) || !canonicalSHA256ID(link.CompletionID) ||
		link.CompletionBasis != "accepted_response_then_exact_stopped" || !canonicalSHA256ID(link.UseID) ||
		!canonicalSHA256ID(link.CompleteFileSnapshotID) || !completeStoppedState(link.StoppedJobState) ||
		!canonicalSHA256ID(link.ActivationOperationID) || !canonicalPlanID(link.ActivationPlanID) ||
		link.ActivationOperationID != OperationIDForPlan(link.ActivationPlanID).String() ||
		!canonicalSHA256ID(link.ActivationTerminalID) {
		return fmt.Errorf("%w: terminal client-stop plan link is invalid", ErrPolicy)
	}
	return nil
}

func validStopOperationForPlan(operationID, planID string) bool {
	if !canonicalSHA256ID(operationID) || !canonicalPlanID(planID) {
		return false
	}
	return operationID == stopOperationIDForPlan(planID)
}

func stopOperationIDForPlan(planID string) string {
	if !canonicalPlanID(planID) {
		return ""
	}
	digest := sha256.Sum256([]byte("ptctl-client-stop-operation-v1\x00" + planID))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (link TerminalStopLink) observedEnd() time.Time {
	value, _ := time.Parse(time.RFC3339Nano, link.ObservedAtEnd)
	return value
}
