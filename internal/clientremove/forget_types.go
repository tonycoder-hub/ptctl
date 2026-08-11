package clientremove

import (
	"fmt"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

type ForgetOutcome string

const (
	ForgetOutcomeForgotten             ForgetOutcome = "forgotten"
	ForgetOutcomeAbsentUnattributed    ForgetOutcome = "absent_unattributed"
	ForgetOutcomeBlocked               ForgetOutcome = "blocked"
	ForgetOutcomeInterrupted           ForgetOutcome = "interrupted"
	ForgetOutcomeIntegrityFailed       ForgetOutcome = "integrity_failed"
	ForgetOutcomeDurabilityUnconfirmed ForgetOutcome = "durability_unconfirmed"
)

type ForgetLimits struct {
	MaxMarkerBytes int64 `json:"max_marker_bytes"`
}

func DefaultForgetLimits() ForgetLimits {
	return ForgetLimits{MaxMarkerBytes: maximumForgetMarkerBytes}
}

func (limits ForgetLimits) Validate() error {
	if limits.MaxMarkerBytes <= 0 || limits.MaxMarkerBytes > maximumForgetMarkerBytes {
		return fmt.Errorf("%w: client removal forget limits are invalid", ErrPolicy)
	}
	return nil
}

type ForgetOptions struct {
	TargetRoot     string       `json:"-"`
	OperationID    OperationID  `json:"operation_id"`
	ExpectedPlanID string       `json:"expected_plan_id"`
	Acknowledge    bool         `json:"acknowledge"`
	Limits         ForgetLimits `json:"limits"`
}

type ForgetAuthorityReport struct {
	State                           string `json:"state"`
	MarkerID                        string `json:"marker_id,omitempty"`
	MarkerDurable                   bool   `json:"marker_durable"`
	RetentionIntentMarkerID         string `json:"retention_intent_marker_id,omitempty"`
	RetentionCompleteMarkerID       string `json:"retention_complete_marker_id,omitempty"`
	ExactTombstoneEvidenceAvailable bool   `json:"exact_tombstone_evidence_available"`
	TargetHistoricalEvidenceErased  bool   `json:"target_historical_evidence_erased"`
}

type ForgetWriteReport struct {
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

type ForgetRemovalReport struct {
	MarkerTemporary    fsbind.Removal               `json:"marker_temporary"`
	RetentionIntent    fsbind.Removal               `json:"retention_intent"`
	RetentionComplete  fsbind.Removal               `json:"retention_complete"`
	RetentionDirectory fsbind.Removal               `json:"retention_directory"`
	OperationSubtree   fsbind.PrivateSubtreeRemoval `json:"operation_subtree"`
	RootIntent         fsbind.Removal               `json:"root_intent"`
}

type ForgetReport struct {
	Outcome               ForgetOutcome         `json:"outcome"`
	Effect                []string              `json:"effect"`
	WritesPerformed       int                   `json:"writes_performed"`
	WritesUncertain       bool                  `json:"writes_uncertain"`
	Operation             OperationReport       `json:"operation"`
	Plan                  PlanReport            `json:"plan"`
	Target                RetentionTargetReport `json:"target"`
	Authority             ForgetAuthorityReport `json:"authority"`
	Proof                 RetentionProofReport  `json:"historical_proof"`
	Writes                ForgetWriteReport     `json:"writes"`
	RootIntentPublication fsbind.Publication    `json:"root_intent_publication"`
	Removals              ForgetRemovalReport   `json:"removals"`
	Limits                ForgetLimits          `json:"limits"`
	Blockers              []Finding             `json:"blockers"`
	Issues                []Finding             `json:"issues"`
	Warnings              []string              `json:"warnings"`
}

type forgetMarkerReceipt struct {
	TemporaryCreated      bool
	TemporaryBytesWritten int64
	AlreadyPresent        bool
	Publication           fsbind.Publication
	TemporaryRemoval      fsbind.Removal
}

func newForgetReport(options ForgetOptions) ForgetReport {
	return ForgetReport{
		Outcome: ForgetOutcomeInterrupted,
		Effect: []string{"read_private_client_removal_tombstone", "write_private_client_removal_forget_intent",
			"delete_private_client_removal_tombstone", "delete_private_client_removal_forget_intent"},
		Operation: OperationReport{ID: options.OperationID.String(), Status: "inspection_incomplete", PhaseBefore: "unknown", PhaseAfter: "unknown"},
		Plan:      PlanReport{ExpectedID: options.ExpectedPlanID}, Target: RetentionTargetReport{StabilityAssurance: "not_observed"},
		Authority: ForgetAuthorityReport{State: "not_started"}, Proof: RetentionProofReport{Assurance: "not_observed"},
		Limits: options.Limits, Blockers: []Finding{}, Issues: []Finding{},
		Warnings: []string{
			"forget irreversibly removes one retained client-removal tombstone and its final historical attribution",
			"materialized bytes, downloader state, source names, and every other operation remain outside this authority",
			"forget cannot erase reports, backups, or other copies already exported outside the selected target root",
			"after the last recovery marker is durably removed, a repeated call can observe only unattributed absence",
			"operation, plan, filesystem, client, and marker references are stable pseudonyms and are not anonymization",
		},
	}
}
