package sourceretire

import (
	"fmt"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const (
	ParentCleanupForgetOutcomeForgotten             = "forgotten"
	ParentCleanupForgetOutcomeAbsentUnattributed    = "absent_unattributed"
	ParentCleanupForgetOutcomeBlocked               = "blocked"
	ParentCleanupForgetOutcomeInterrupted           = "interrupted"
	ParentCleanupForgetOutcomeIntegrityFailed       = "integrity_failed"
	ParentCleanupForgetOutcomeDurabilityUnconfirmed = "durability_unconfirmed"
)

type ParentCleanupForgetLimits struct {
	MaxMarkerBytes int64 `json:"max_marker_bytes"`
}

func DefaultParentCleanupForgetLimits() ParentCleanupForgetLimits {
	return ParentCleanupForgetLimits{MaxMarkerBytes: maximumParentCleanupForgetBytes}
}

func (limits ParentCleanupForgetLimits) Validate() error {
	if limits.MaxMarkerBytes <= 0 || limits.MaxMarkerBytes > maximumParentCleanupForgetBytes {
		return fmt.Errorf("%w: parent cleanup forget limits are invalid", ErrExecutionPolicy)
	}
	return nil
}

type ParentCleanupForgetOptions struct {
	TargetRoot            string                    `json:"-"`
	OperationID           ParentCleanupOperationID  `json:"operation_id"`
	ExpectedCleanupPlanID string                    `json:"expected_cleanup_plan_id"`
	Acknowledge           bool                      `json:"acknowledge"`
	Limits                ParentCleanupForgetLimits `json:"limits"`
}

type ParentCleanupForgetAuthorityReport struct {
	State                           string `json:"state"`
	MarkerID                        string `json:"marker_id,omitempty"`
	MarkerDurable                   bool   `json:"marker_durable"`
	RetentionIntentMarkerID         string `json:"retention_intent_marker_id,omitempty"`
	RetentionCompleteMarkerID       string `json:"retention_complete_marker_id,omitempty"`
	ExactTombstoneEvidenceAvailable bool   `json:"exact_tombstone_evidence_available"`
	TargetHistoricalEvidenceErased  bool   `json:"target_historical_evidence_erased"`
}

type ParentCleanupForgetWriteReport struct {
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

type ParentCleanupForgetRemovalReport struct {
	MarkerTemporary    fsbind.Removal               `json:"marker_temporary"`
	RetentionIntent    fsbind.Removal               `json:"retention_intent"`
	RetentionComplete  fsbind.Removal               `json:"retention_complete"`
	RetentionDirectory fsbind.Removal               `json:"retention_directory"`
	OperationSubtree   fsbind.PrivateSubtreeRemoval `json:"operation_subtree"`
	RootIntent         fsbind.Removal               `json:"root_intent"`
}

type ParentCleanupForgetReport struct {
	Outcome               string                                `json:"outcome"`
	Effect                []string                              `json:"effect"`
	WritesPerformed       int                                   `json:"writes_performed"`
	WritesUncertain       bool                                  `json:"writes_uncertain"`
	Operation             ParentCleanupExecutionOperationReport `json:"operation"`
	Target                ExecutionRetentionTargetReport        `json:"target"`
	Authority             ParentCleanupForgetAuthorityReport    `json:"authority"`
	Proof                 ParentCleanupRetentionProofReport     `json:"historical_proof"`
	Writes                ParentCleanupForgetWriteReport        `json:"writes"`
	RootIntentPublication fsbind.Publication                    `json:"root_intent_publication"`
	Removals              ParentCleanupForgetRemovalReport      `json:"removals"`
	Limits                ParentCleanupForgetLimits             `json:"limits"`
	Blockers              []Finding                             `json:"blockers"`
	Issues                []Finding                             `json:"issues"`
	Warnings              []string                              `json:"warnings"`
}

type parentCleanupForgetMarkerReceipt struct {
	TemporaryCreated      bool
	TemporaryBytesWritten int64
	AlreadyPresent        bool
	Publication           fsbind.Publication
	TemporaryRemoval      fsbind.Removal
}

func newParentCleanupForgetReport(options ParentCleanupForgetOptions) ParentCleanupForgetReport {
	return ParentCleanupForgetReport{
		Outcome: ParentCleanupForgetOutcomeInterrupted,
		Effect: []string{
			"read_private_parent_cleanup_tombstone",
			"write_private_parent_cleanup_forget_intent",
			"delete_private_parent_cleanup_tombstone",
			"delete_private_parent_cleanup_forget_intent",
		},
		Operation: ParentCleanupExecutionOperationReport{ID: options.OperationID.String(), PlanID: options.ExpectedCleanupPlanID,
			Status: "inspection_incomplete", Phase: "unknown"},
		Target:    ExecutionRetentionTargetReport{StabilityAssurance: "not_observed"},
		Authority: ParentCleanupForgetAuthorityReport{State: "not_started"},
		Proof:     ParentCleanupRetentionProofReport{Assurance: "not_observed"},
		Limits:    options.Limits,
		Blockers:  []Finding{}, Issues: []Finding{},
		Warnings: []string{
			"forget irreversibly removes one retained parent-cleanup tombstone and its final historical attribution",
			"source names, the materialized final, downloader state, and every other operation remain outside this authority",
			"after the last recovery marker is durably removed, a repeated call can observe only unattributed absence",
			"operation, plan, filesystem, client, and lineage references are stable pseudonyms and are not anonymization",
		},
	}
}
