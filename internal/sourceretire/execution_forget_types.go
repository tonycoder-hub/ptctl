package sourceretire

import (
	"fmt"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const (
	ExecutionForgetOutcomeForgotten             = "forgotten"
	ExecutionForgetOutcomeAbsentUnattributed    = "absent_unattributed"
	ExecutionForgetOutcomeBlocked               = "blocked"
	ExecutionForgetOutcomeInterrupted           = "interrupted"
	ExecutionForgetOutcomeIntegrityFailed       = "integrity_failed"
	ExecutionForgetOutcomeDurabilityUnconfirmed = "durability_unconfirmed"
)

type ExecutionForgetLimits struct {
	MaxMarkerBytes int64 `json:"max_marker_bytes"`
}

func DefaultExecutionForgetLimits() ExecutionForgetLimits {
	return ExecutionForgetLimits{MaxMarkerBytes: maximumExecutionForgetBytes}
}

func (limits ExecutionForgetLimits) Validate() error {
	if limits.MaxMarkerBytes <= 0 || limits.MaxMarkerBytes > maximumExecutionForgetBytes {
		return fmt.Errorf("%w: source retirement forget limits are invalid", ErrExecutionPolicy)
	}
	return nil
}

type ExecutionForgetOptions struct {
	TargetRoot     string                `json:"-"`
	OperationID    OperationID           `json:"operation_id"`
	ExpectedPlanID string                `json:"expected_plan_id"`
	Acknowledge    bool                  `json:"acknowledge"`
	Limits         ExecutionForgetLimits `json:"limits"`
}

type ExecutionForgetAuthorityReport struct {
	State                           string `json:"state"`
	MarkerID                        string `json:"marker_id,omitempty"`
	MarkerDurable                   bool   `json:"marker_durable"`
	RetentionIntentMarkerID         string `json:"retention_intent_marker_id,omitempty"`
	RetentionCompleteMarkerID       string `json:"retention_complete_marker_id,omitempty"`
	ExactTombstoneEvidenceAvailable bool   `json:"exact_tombstone_evidence_available"`
	TargetHistoricalEvidenceErased  bool   `json:"target_historical_evidence_erased"`
}

type ExecutionForgetWriteReport struct {
	MarkerTemporaryFiles        int   `json:"marker_temporary_files_created"`
	MarkerTemporaryBytes        int64 `json:"marker_temporary_bytes_written"`
	MarkerPublicationAttempts   int   `json:"marker_publication_attempts"`
	MarkerPublications          int   `json:"marker_publications"`
	AmbiguousMarkerPublications int   `json:"ambiguous_marker_publications"`
	RemovalAttempts             int   `json:"removal_attempts"`
	FilesRemoved                int   `json:"files_removed"`
	DirectoriesRemoved          int   `json:"directories_removed"`
	BytesRemoved                int64 `json:"bytes_removed"`
	AmbiguousRemovals           int   `json:"ambiguous_removals"`
}

type ExecutionForgetRemovalReport struct {
	MarkerTemporary    fsbind.Removal               `json:"marker_temporary"`
	RetentionIntent    fsbind.Removal               `json:"retention_intent"`
	RetentionComplete  fsbind.Removal               `json:"retention_complete"`
	RetentionDirectory fsbind.Removal               `json:"retention_directory"`
	OperationSubtree   fsbind.PrivateSubtreeRemoval `json:"operation_subtree"`
	RootIntent         fsbind.Removal               `json:"root_intent"`
}

type ExecutionForgetReport struct {
	Outcome               string                         `json:"outcome"`
	Effect                []string                       `json:"effect"`
	WritesPerformed       int                            `json:"writes_performed"`
	WritesUncertain       bool                           `json:"writes_uncertain"`
	Operation             ExecutionOperationReport       `json:"operation"`
	Target                ExecutionRetentionTargetReport `json:"target"`
	Authority             ExecutionForgetAuthorityReport `json:"authority"`
	Proof                 ExecutionRetentionProofReport  `json:"historical_proof"`
	Writes                ExecutionForgetWriteReport     `json:"writes"`
	RootIntentPublication fsbind.Publication             `json:"root_intent_publication"`
	Removals              ExecutionForgetRemovalReport   `json:"removals"`
	Limits                ExecutionForgetLimits          `json:"limits"`
	Blockers              []Finding                      `json:"blockers"`
	Issues                []Finding                      `json:"issues"`
	Warnings              []string                       `json:"warnings"`
}

type executionForgetMarkerReceipt struct {
	TemporaryCreated      bool
	TemporaryBytesWritten int64
	AlreadyPresent        bool
	Publication           fsbind.Publication
	TemporaryRemoval      fsbind.Removal
}

func newExecutionForgetReport(options ExecutionForgetOptions) ExecutionForgetReport {
	return ExecutionForgetReport{
		Outcome: ExecutionForgetOutcomeInterrupted,
		Effect: []string{
			"read_private_source_retirement_tombstone",
			"write_private_source_retirement_forget_intent",
			"delete_private_source_retirement_tombstone",
			"delete_private_source_retirement_forget_intent",
		},
		Operation: ExecutionOperationReport{ID: options.OperationID.String(), PlanID: options.ExpectedPlanID,
			Status: "inspection_incomplete", Phase: "unknown"},
		Target:    ExecutionRetentionTargetReport{StabilityAssurance: "not_observed"},
		Authority: ExecutionForgetAuthorityReport{State: "not_started"},
		Proof:     ExecutionRetentionProofReport{Assurance: "not_observed"},
		Limits:    options.Limits,
		Blockers:  []Finding{}, Issues: []Finding{},
		Warnings: []string{
			"forget irreversibly removes one retained source-retirement tombstone and its final historical attribution",
			"source names, the materialized final, downloader state, and every other operation remain outside this authority",
			"after the last recovery marker is durably removed, a repeated call can observe only unattributed absence",
			"operation, plan, filesystem, client, and lineage references are stable pseudonyms and are not anonymization",
		},
	}
}
