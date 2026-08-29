package materialize

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
	return ForgetLimits{MaxMarkerBytes: maxForgetMarkerBytes}
}

func (limits ForgetLimits) Validate() error {
	if limits.MaxMarkerBytes <= 0 || limits.MaxMarkerBytes > maxForgetMarkerBytes {
		return fmt.Errorf("%w: materialize forget limits are invalid", ErrPolicy)
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

type ForgetProofReport struct {
	Basis               string `json:"basis"`
	TerminalEventID     string `json:"terminal_event_id,omitempty"`
	TerminalPhase       string `json:"terminal_phase,omitempty"`
	MetafileVariantID   string `json:"metafile_variant_id,omitempty"`
	HistoricalAuthority bool   `json:"historical_authority"`
	Assurance           string `json:"assurance"`
}

type ForgetTargetReport struct {
	ExpectedRootIdentity string `json:"expected_root_identity,omitempty"`
	ObservedRootIdentity string `json:"observed_root_identity,omitempty"`
	RootIdentityBound    bool   `json:"root_identity_bound"`
	FinalState           string `json:"final_state"`
	StabilityAssurance   string `json:"stability_assurance"`
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
	Target                ForgetTargetReport    `json:"target"`
	Authority             ForgetAuthorityReport `json:"authority"`
	Proof                 ForgetProofReport     `json:"historical_proof"`
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
		Effect: []string{
			"read_private_materialize_tombstone",
			"write_private_materialize_forget_intent",
			"delete_private_materialize_tombstone",
			"delete_private_materialize_forget_intent",
		},
		Operation: OperationReport{
			ID: options.OperationID.String(), Status: "inspection_incomplete",
			PhaseBefore: "unknown", PhaseAfter: "unknown",
		},
		Plan:      PlanReport{ExpectedID: options.ExpectedPlanID, Strategy: StrategyCopy},
		Target:    ForgetTargetReport{FinalState: "not_observed", StabilityAssurance: "not_observed"},
		Authority: ForgetAuthorityReport{State: "not_started"},
		Proof:     ForgetProofReport{Assurance: "not_observed"},
		Limits:    options.Limits, Blockers: []Finding{}, Issues: []Finding{},
		Warnings: []string{
			"forget irreversibly removes one retained materialize tombstone and its final historical attribution",
			"source bytes, the published final layout, downloader state, and every other operation remain outside this authority",
			"after the last recovery marker is durably removed, a repeated call can observe only unattributed absence",
			"operation, plan, filesystem, and metafile references are stable pseudonyms and are not anonymization",
		},
	}
}
