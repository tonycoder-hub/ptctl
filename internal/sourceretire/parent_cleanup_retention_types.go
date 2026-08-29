package sourceretire

import (
	"fmt"
	"io"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const (
	ParentCleanupRetentionIntentSchemaV1   = "ptctl.source-retirement-parent-cleanup-retention-intent/v1"
	ParentCleanupRetentionCompleteSchemaV1 = "ptctl.source-retirement-parent-cleanup-retention-complete/v1"
	ParentCleanupRetentionBasisComplete    = "terminal_parent_cleanup_journal_exact"

	ParentCleanupRetentionOutcomePruned          = "pruned"
	ParentCleanupRetentionOutcomeAlreadyPruned   = "already_pruned"
	ParentCleanupRetentionOutcomeBlocked         = "blocked"
	ParentCleanupRetentionOutcomeInterrupted     = "interrupted"
	ParentCleanupRetentionOutcomeIntegrityFailed = "integrity_failed"

	parentCleanupRetentionIntentDomain   = "ptctl-source-retirement-parent-cleanup-retention-intent-v1\x00"
	parentCleanupRetentionCompleteDomain = "ptctl-source-retirement-parent-cleanup-retention-complete-v1\x00"
	parentCleanupRetentionDirectory      = "retention"
	parentCleanupRetentionIntentFile     = "intent.json"
	parentCleanupRetentionCompleteFile   = "complete.json"
	maximumParentCleanupRetentionMarker  = int64(16 << 10)
)

type ParentCleanupRetentionMarkerID string

func ParseParentCleanupRetentionMarkerID(value string) (ParentCleanupRetentionMarkerID, error) {
	if !canonicalSHA256ID(value) {
		return "", fmt.Errorf("invalid parent-cleanup retention marker ID")
	}
	return ParentCleanupRetentionMarkerID(value), nil
}

func (id ParentCleanupRetentionMarkerID) String() string { return string(id) }

// ParentCleanupRetentionIntent is the durable deletion boundary for one
// terminal parent-cleanup operation. It deliberately retains no absolute
// parent path or parent filesystem identity.
type ParentCleanupRetentionIntent struct {
	Schema                 string                   `json:"schema"`
	OperationID            ParentCleanupOperationID `json:"operation_id"`
	OperationRootIdentity  string                   `json:"operation_root_identity"`
	TargetRootIdentity     string                   `json:"target_root_identity"`
	CleanupPlanID          string                   `json:"cleanup_plan_id"`
	IntentID               string                   `json:"intent_id"`
	CompletionID           string                   `json:"completion_id"`
	Basis                  string                   `json:"basis"`
	RetirementOperationID  OperationID              `json:"retirement_operation_id"`
	RetirementPlanID       string                   `json:"retirement_plan_id"`
	RetirementCompletionID string                   `json:"retirement_completion_id"`
	SearchScopeID          string                   `json:"search_scope_id"`
	ParentsRemoved         int                      `json:"parents_removed"`
	RetiredFiles           int                      `json:"retired_files"`
}

func (marker ParentCleanupRetentionIntent) Validate() error {
	if marker.Schema != ParentCleanupRetentionIntentSchemaV1 || marker.Basis != ParentCleanupRetentionBasisComplete ||
		!canonicalSHA256ID(marker.CleanupPlanID) || !canonicalSHA256ID(marker.IntentID) || !canonicalSHA256ID(marker.CompletionID) ||
		!canonicalSHA256ID(marker.RetirementPlanID) || !canonicalSHA256ID(marker.RetirementCompletionID) ||
		!canonicalSHA256ID(marker.SearchScopeID) || marker.ParentsRemoved <= 0 || marker.ParentsRemoved > hardParentCleanupExecutionMaxParents ||
		marker.RetiredFiles <= 0 || marker.RetiredFiles > hardExecutionMaxFiles {
		return fmt.Errorf("%w: parent-cleanup retention intent is invalid", ErrExecutionIntegrity)
	}
	parsed, parseErr := ParseParentCleanupOperationID(marker.OperationID.String())
	derived, deriveErr := ParentCleanupOperationIDForPlanID(marker.CleanupPlanID)
	retirement, retirementErr := ParseOperationID(marker.RetirementOperationID.String())
	expectedRetirement, retirementDeriveErr := executionOperationID(marker.RetirementPlanID)
	operationRoot, operationErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
	targetRoot, targetErr := fsbind.ParseIdentity(marker.TargetRootIdentity)
	if parseErr != nil || deriveErr != nil || retirementErr != nil || retirementDeriveErr != nil ||
		parsed != marker.OperationID || derived != marker.OperationID || retirement != marker.RetirementOperationID ||
		expectedRetirement != marker.RetirementOperationID || operationErr != nil || targetErr != nil ||
		operationRoot.IsZero() || targetRoot.IsZero() {
		return fmt.Errorf("%w: parent-cleanup retention authority is invalid", ErrExecutionIntegrity)
	}
	return nil
}

type ParentCleanupRetentionComplete struct {
	Schema                string                         `json:"schema"`
	OperationID           ParentCleanupOperationID       `json:"operation_id"`
	OperationRootIdentity string                         `json:"operation_root_identity"`
	TargetRootIdentity    string                         `json:"target_root_identity"`
	IntentMarkerID        ParentCleanupRetentionMarkerID `json:"intent_marker_id"`
}

func (marker ParentCleanupRetentionComplete) Validate() error {
	if marker.Schema != ParentCleanupRetentionCompleteSchemaV1 {
		return fmt.Errorf("%w: parent-cleanup retention completion is invalid", ErrExecutionIntegrity)
	}
	operation, operationErr := ParseParentCleanupOperationID(marker.OperationID.String())
	intent, intentErr := ParseParentCleanupRetentionMarkerID(marker.IntentMarkerID.String())
	operationRoot, rootErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
	targetRoot, targetErr := fsbind.ParseIdentity(marker.TargetRootIdentity)
	if operationErr != nil || intentErr != nil || rootErr != nil || targetErr != nil || operation != marker.OperationID ||
		intent != marker.IntentMarkerID || operationRoot.IsZero() || targetRoot.IsZero() {
		return fmt.Errorf("%w: parent-cleanup retention completion authority is invalid", ErrExecutionIntegrity)
	}
	return nil
}

func EncodeParentCleanupRetentionIntent(marker ParentCleanupRetentionIntent) ([]byte, ParentCleanupRetentionMarkerID, error) {
	if err := marker.Validate(); err != nil {
		return nil, "", err
	}
	raw, id, err := encodeExecutionMarker(parentCleanupRetentionIntentDomain, marker, maximumParentCleanupRetentionMarker)
	return raw, ParentCleanupRetentionMarkerID(id), err
}

func DecodeParentCleanupRetentionIntent(reader io.Reader) (ParentCleanupRetentionIntent, ParentCleanupRetentionMarkerID, error) {
	var marker ParentCleanupRetentionIntent
	id, err := decodeExecutionMarker(reader, maximumParentCleanupRetentionMarker, parentCleanupRetentionIntentDomain, &marker, func() error { return marker.Validate() })
	if err != nil {
		return ParentCleanupRetentionIntent{}, "", err
	}
	return marker, ParentCleanupRetentionMarkerID(id), nil
}

func EncodeParentCleanupRetentionComplete(marker ParentCleanupRetentionComplete) ([]byte, ParentCleanupRetentionMarkerID, error) {
	if err := marker.Validate(); err != nil {
		return nil, "", err
	}
	raw, id, err := encodeExecutionMarker(parentCleanupRetentionCompleteDomain, marker, maximumParentCleanupRetentionMarker)
	return raw, ParentCleanupRetentionMarkerID(id), err
}

func DecodeParentCleanupRetentionComplete(reader io.Reader) (ParentCleanupRetentionComplete, ParentCleanupRetentionMarkerID, error) {
	var marker ParentCleanupRetentionComplete
	id, err := decodeExecutionMarker(reader, maximumParentCleanupRetentionMarker, parentCleanupRetentionCompleteDomain, &marker, func() error { return marker.Validate() })
	if err != nil {
		return ParentCleanupRetentionComplete{}, "", err
	}
	return marker, ParentCleanupRetentionMarkerID(id), nil
}

type ParentCleanupPruneOptions struct {
	TargetRoot            string                       `json:"-"`
	OperationID           ParentCleanupOperationID     `json:"operation_id"`
	ExpectedCleanupPlanID string                       `json:"expected_cleanup_plan_id"`
	Acknowledge           bool                         `json:"acknowledge"`
	JournalLimits         ParentCleanupExecutionLimits `json:"journal_limits"`
	RetentionLimits       ExecutionRetentionLimits     `json:"retention_limits"`
}

type ParentCleanupRetentionProofReport struct {
	Basis                  string `json:"basis"`
	IntentID               string `json:"intent_id,omitempty"`
	TerminalCompletionID   string `json:"terminal_completion_id,omitempty"`
	RetirementOperationID  string `json:"retirement_operation_id,omitempty"`
	RetirementPlanID       string `json:"retirement_plan_id,omitempty"`
	RetirementCompletionID string `json:"retirement_completion_id,omitempty"`
	SearchScopeID          string `json:"search_scope_id,omitempty"`
	ParentsRemoved         int    `json:"parents_removed"`
	RetiredFiles           int    `json:"retired_files"`
	HistoricalAuthority    bool   `json:"historical_terminal_authority"`
	Assurance              string `json:"assurance"`
}

type ParentCleanupRetentionMarkerReport struct {
	State             string `json:"state"`
	IntentMarkerID    string `json:"intent_marker_id,omitempty"`
	CompleteMarkerID  string `json:"complete_marker_id,omitempty"`
	IntentDurable     bool   `json:"intent_durable"`
	CompletionDurable bool   `json:"completion_durable"`
	ExactTombstone    bool   `json:"exact_tombstone"`
	PruneResumable    bool   `json:"prune_resumable"`
}

type ParentCleanupRetentionReport struct {
	Outcome         string                                `json:"outcome"`
	Effect          []string                              `json:"effect"`
	WritesPerformed int                                   `json:"writes_performed"`
	WritesUncertain bool                                  `json:"writes_uncertain"`
	Operation       ParentCleanupExecutionOperationReport `json:"operation"`
	Target          ExecutionRetentionTargetReport        `json:"target"`
	Proof           ParentCleanupRetentionProofReport     `json:"proof"`
	Markers         ParentCleanupRetentionMarkerReport    `json:"markers"`
	Writes          ExecutionRetentionWriteReport         `json:"writes"`
	JournalLimits   ParentCleanupExecutionLimits          `json:"journal_limits"`
	Limits          ExecutionRetentionLimits              `json:"limits"`
	Used            ExecutionRetentionUsage               `json:"used"`
	Blockers        []Finding                             `json:"blockers"`
	Issues          []Finding                             `json:"issues"`
	Warnings        []string                              `json:"warnings"`
}

func newParentCleanupRetentionReport(options ParentCleanupPruneOptions) ParentCleanupRetentionReport {
	return ParentCleanupRetentionReport{
		Outcome:       ParentCleanupRetentionOutcomeInterrupted,
		Effect:        []string{"read_private_parent_cleanup_operation_state", "write_private_parent_cleanup_retention_markers", "delete_private_parent_cleanup_operation_state"},
		Operation:     ParentCleanupExecutionOperationReport{ID: options.OperationID.String(), PlanID: options.ExpectedCleanupPlanID, Status: "inspection_incomplete", Phase: "unknown"},
		Target:        ExecutionRetentionTargetReport{StabilityAssurance: "not_observed"},
		Proof:         ParentCleanupRetentionProofReport{Assurance: "not_observed"},
		Markers:       ParentCleanupRetentionMarkerReport{State: "not_started"},
		JournalLimits: options.JournalLimits, Limits: options.RetentionLimits,
		Blockers: []Finding{}, Issues: []Finding{},
		Warnings: []string{
			"prune deletes only one explicit parent-cleanup operation journal and retains a small no-path tombstone",
			"source names, source parents, search roots, final content, and downloader state are never modified by prune",
			"the tombstone records historical terminal evidence and does not prove that removed parents remain absent now",
			"operation, plan, filesystem, and lineage references are stable pseudonyms and are not anonymization",
		},
	}
}
