package clientactivate

import (
	"fmt"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

type RetainedMarkerLink struct {
	Name      string   `json:"name"`
	MarkerID  MarkerID `json:"marker_id"`
	SizeBytes int64    `json:"size_bytes"`
}

type RetentionIntent struct {
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

func (marker RetentionIntent) Validate() error {
	if marker.Schema != RetentionIntentSchemaV1 || marker.Basis != RetentionBasisTerminal ||
		marker.Intent.Validate() != nil || marker.RecheckCompletion.Validate() != nil ||
		marker.OperationID != marker.Intent.OperationID || marker.PlanID != marker.Intent.PlanID ||
		marker.RecheckCompletion.OperationID != marker.OperationID || marker.RecheckCompletion.PlanID != marker.PlanID ||
		marker.RecheckCompletion.FinalObjectIdentity != marker.Intent.Plan.FinalObjectIdentity ||
		marker.TargetRootIdentity != marker.Intent.Plan.TargetRootIdentity || len(marker.Markers) < 3 || len(marker.Markers) > 10 {
		return fmt.Errorf("%w: client activation retention intent is invalid", ErrIntegrity)
	}
	operationRoot, operationErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
	targetRoot, targetErr := fsbind.ParseIdentity(marker.TargetRootIdentity)
	if operationErr != nil || targetErr != nil || operationRoot.IsZero() || targetRoot.IsZero() || marker.OperationRootIdentity != marker.Intent.OperationRootIdentity {
		return fmt.Errorf("%w: client activation retention filesystem identity is invalid", ErrIntegrity)
	}
	_, intentID, err := encodeIntent(marker.Intent)
	if err != nil || marker.IntentID != intentID {
		return fmt.Errorf("%w: client activation retention intent link is invalid", ErrIntegrity)
	}
	_, recheckID, err := encodeRecheckCompletion(marker.RecheckCompletion)
	if err != nil || marker.RecheckCompletionID != recheckID {
		return fmt.Errorf("%w: client activation retained recheck link is invalid", ErrIntegrity)
	}
	if marker.Intent.Plan.Action == ActionRecheckOnly {
		if marker.ActivationCompletion != nil || marker.ActivationCompletionID != "" {
			return fmt.Errorf("%w: recheck-only retention unexpectedly contains start completion", ErrIntegrity)
		}
	} else if marker.Intent.Plan.Action == ActionRecheckThenStart {
		if marker.ActivationCompletion == nil || marker.ActivationCompletion.Validate() != nil {
			return fmt.Errorf("%w: activation retention lacks its reviewed terminal start", ErrIntegrity)
		}
		_, activationID, encodeErr := encodeActivationCompletion(*marker.ActivationCompletion)
		if encodeErr != nil || activationID != marker.ActivationCompletionID ||
			marker.ActivationCompletion.OperationID != marker.OperationID || marker.ActivationCompletion.PlanID != marker.PlanID ||
			marker.ActivationCompletion.RecheckCompletionID != marker.RecheckCompletionID ||
			marker.ActivationCompletion.FinalObjectIdentity != marker.Intent.Plan.FinalObjectIdentity {
			return fmt.Errorf("%w: retained activation completion link is invalid", ErrIntegrity)
		}
	} else {
		return fmt.Errorf("%w: retained activation action is unsupported", ErrIntegrity)
	}
	seen := make(map[string]bool, len(marker.Markers))
	for index, link := range marker.Markers {
		if link.Name == "" || !knownMarkerName(link.Name) || seen[link.Name] || link.SizeBytes <= 0 || link.SizeBytes > maximumMarkerBytes {
			return fmt.Errorf("%w: retained activation marker manifest is invalid", ErrIntegrity)
		}
		if parsed, parseErr := parseMarkerID(link.MarkerID.String()); parseErr != nil || parsed != link.MarkerID {
			return fmt.Errorf("%w: retained activation marker identity is invalid", ErrIntegrity)
		}
		if index == 0 && (link.Name != intentFileName || link.MarkerID != marker.IntentID) {
			return fmt.Errorf("%w: retained activation marker manifest lacks its intent", ErrIntegrity)
		}
		if link.Name == recheckCompletionFileName && link.MarkerID != marker.RecheckCompletionID {
			return fmt.Errorf("%w: retained recheck completion manifest link disagrees", ErrIntegrity)
		}
		if link.Name == activationCompletionName && link.MarkerID != marker.ActivationCompletionID {
			return fmt.Errorf("%w: retained activation completion manifest link disagrees", ErrIntegrity)
		}
		seen[link.Name] = true
	}
	if !seen[recheckCompletionFileName] || marker.Intent.Plan.Action == ActionRecheckThenStart && !seen[activationCompletionName] {
		return fmt.Errorf("%w: retained activation terminal marker is absent", ErrIntegrity)
	}
	return nil
}

type RetentionComplete struct {
	Schema                string            `json:"schema"`
	OperationID           OperationID       `json:"operation_id"`
	OperationRootIdentity string            `json:"operation_root_identity"`
	TargetRootIdentity    string            `json:"target_root_identity"`
	PlanID                string            `json:"plan_id"`
	IntentMarkerID        RetentionMarkerID `json:"intent_marker_id"`
	TerminalMarkerID      MarkerID          `json:"terminal_marker_id"`
}

func (marker RetentionComplete) Validate() error {
	if marker.Schema != RetentionCompleteSchemaV1 || !canonicalPlanID(marker.PlanID) || OperationIDForPlan(marker.PlanID) != marker.OperationID {
		return fmt.Errorf("%w: client activation retention completion is invalid", ErrIntegrity)
	}
	if parsed, err := ParseRetentionMarkerID(marker.IntentMarkerID.String()); err != nil || parsed != marker.IntentMarkerID {
		return fmt.Errorf("%w: client activation retention intent link is invalid", ErrIntegrity)
	}
	if parsed, err := parseMarkerID(marker.TerminalMarkerID.String()); err != nil || parsed != marker.TerminalMarkerID {
		return fmt.Errorf("%w: client activation retention terminal link is invalid", ErrIntegrity)
	}
	operationRoot, operationErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
	targetRoot, targetErr := fsbind.ParseIdentity(marker.TargetRootIdentity)
	if operationErr != nil || targetErr != nil || operationRoot.IsZero() || targetRoot.IsZero() {
		return fmt.Errorf("%w: client activation retention completion filesystem identity is invalid", ErrIntegrity)
	}
	return nil
}

type RetentionLimits struct {
	MaxMarkerBytes int64 `json:"max_marker_bytes"`
	MaxObjects     int   `json:"max_objects"`
	MaxPathBytes   int64 `json:"max_path_bytes"`
}

func DefaultRetentionLimits() RetentionLimits {
	return RetentionLimits{MaxMarkerBytes: maximumRetentionBytes, MaxObjects: 16, MaxPathBytes: 1 << 20}
}

func (limits RetentionLimits) Validate() error {
	if limits.MaxMarkerBytes <= 0 || limits.MaxMarkerBytes > maximumRetentionBytes || limits.MaxObjects < 16 || limits.MaxObjects > 64 ||
		limits.MaxPathBytes <= 0 || limits.MaxPathBytes > 1<<20 {
		return fmt.Errorf("%w: client activation retention limits are invalid", ErrPolicy)
	}
	return nil
}

type PruneOptions struct {
	TargetRoot     string
	OperationID    OperationID
	ExpectedPlanID string
	Acknowledge    bool
	Limits         RetentionLimits
}

type RetentionOutcome string

const (
	RetentionOutcomePruned          RetentionOutcome = "pruned"
	RetentionOutcomeAlreadyPruned   RetentionOutcome = "already_pruned"
	RetentionOutcomeBlocked         RetentionOutcome = "blocked"
	RetentionOutcomeInterrupted     RetentionOutcome = "interrupted"
	RetentionOutcomeIntegrityFailed RetentionOutcome = "integrity_failed"
)

type RetentionTargetReport struct {
	ExpectedRootIdentity string `json:"expected_root_identity,omitempty"`
	ObservedRootIdentity string `json:"observed_root_identity,omitempty"`
	RootIdentityBound    bool   `json:"root_identity_bound"`
	StabilityAssurance   string `json:"stability_assurance"`
}

type RetentionProofReport struct {
	Basis               string `json:"basis"`
	IntentID            string `json:"intent_id,omitempty"`
	TerminalMarkerID    string `json:"terminal_marker_id,omitempty"`
	MarkersRecorded     int    `json:"markers_recorded"`
	HistoricalAuthority bool   `json:"historical_terminal_authority"`
	Assurance           string `json:"assurance"`
}

type RetentionMarkerReport struct {
	State             string `json:"state"`
	IntentMarkerID    string `json:"intent_marker_id,omitempty"`
	CompleteMarkerID  string `json:"complete_marker_id,omitempty"`
	IntentDurable     bool   `json:"intent_durable"`
	CompletionDurable bool   `json:"completion_durable"`
	ExactTombstone    bool   `json:"exact_tombstone"`
	PruneResumable    bool   `json:"prune_resumable"`
}

type RetentionWriteReport struct {
	ControlDirectoriesCreated int   `json:"control_directories_created"`
	MarkerTemporaryFiles      int   `json:"marker_temporary_files_created"`
	MarkerTemporaryBytes      int64 `json:"marker_temporary_bytes_written"`
	MarkerPublicationAttempts int   `json:"marker_publication_attempts"`
	MarkerPublications        int   `json:"marker_publications"`
	MarkerTemporaryRemovals   int   `json:"marker_temporary_removals"`
	RemovalAttempts           int   `json:"removal_attempts"`
	FilesRemoved              int   `json:"files_removed"`
	DirectoriesRemoved        int   `json:"directories_removed"`
	BytesRemoved              int64 `json:"bytes_removed"`
	AmbiguousRemovals         int   `json:"ambiguous_removals"`
}

type RetentionRemovalReport struct {
	Markers []fsbind.Removal `json:"markers"`
	Scratch fsbind.Removal   `json:"scratch"`
}

type RetentionUsage struct {
	ObjectsConsidered   int   `json:"objects_considered"`
	PathBytesConsidered int64 `json:"path_bytes_considered"`
	BytesConsidered     int64 `json:"bytes_considered"`
}

type RetentionReport struct {
	Outcome         RetentionOutcome       `json:"outcome"`
	Effect          []string               `json:"effect"`
	WritesPerformed int                    `json:"writes_performed"`
	WritesUncertain bool                   `json:"writes_uncertain"`
	Operation       OperationReport        `json:"operation"`
	Plan            PlanReport             `json:"plan"`
	Target          RetentionTargetReport  `json:"target"`
	Proof           RetentionProofReport   `json:"proof"`
	Markers         RetentionMarkerReport  `json:"markers"`
	Writes          RetentionWriteReport   `json:"writes"`
	Removals        RetentionRemovalReport `json:"removals"`
	Limits          RetentionLimits        `json:"limits"`
	Used            RetentionUsage         `json:"used"`
	Blockers        []Finding              `json:"blockers"`
	Issues          []Finding              `json:"issues"`
	Warnings        []string               `json:"warnings"`
}

type retentionMarkerReceipt struct {
	DirectoryCreated           bool
	DirectoryDurability        string
	TemporaryCreated           bool
	TemporaryCreationUncertain bool
	TemporaryStateUncertain    bool
	TemporaryBytesWritten      int64
	AlreadyPresent             bool
	Publication                fsbind.Publication
	PublicationUncertain       bool
	TemporaryRemoval           fsbind.Removal
	TemporaryRemovalUncertain  bool
}
