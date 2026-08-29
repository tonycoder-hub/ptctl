package sourceretire

import (
	"fmt"
	"io"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const (
	ExecutionRetentionIntentSchemaV1   = "ptctl.source-retirement-retention-intent/v1"
	ExecutionRetentionCompleteSchemaV1 = "ptctl.source-retirement-retention-complete/v1"

	ExecutionRetentionBasisComplete = "terminal_complete_journal_exact"

	executionRetentionIntentDomain   = "ptctl-source-retirement-retention-intent-v1\x00"
	executionRetentionCompleteDomain = "ptctl-source-retirement-retention-complete-v1\x00"
	maximumExecutionRetentionMarker  = int64(16 << 10)
)

type ExecutionRetentionMarkerID string

func ParseExecutionRetentionMarkerID(value string) (ExecutionRetentionMarkerID, error) {
	if !canonicalSHA256ID(value) {
		return "", fmt.Errorf("invalid source retirement retention marker ID")
	}
	return ExecutionRetentionMarkerID(value), nil
}

func (id ExecutionRetentionMarkerID) String() string { return string(id) }

// ExecutionRetentionIntent is the durable deletion boundary for private
// source-retirement operation state. It retains the exact historical links
// needed to distinguish a pruned operation from one that never existed, but
// does not claim that retired source names remain absent now.
type ExecutionRetentionIntent struct {
	Schema                 string      `json:"schema"`
	OperationID            OperationID `json:"operation_id"`
	OperationRootIdentity  string      `json:"operation_root_identity"`
	TargetRootIdentity     string      `json:"target_root_identity"`
	PlanID                 string      `json:"plan_id"`
	IntentID               string      `json:"intent_id"`
	CompletionID           string      `json:"completion_id"`
	Basis                  string      `json:"basis"`
	SearchScopeID          string      `json:"search_scope_id"`
	MetafileVariantID      string      `json:"metafile_variant_id"`
	MaterializeOperationID string      `json:"materialize_operation_id"`
	MaterializePlanID      string      `json:"materialize_plan_id"`
	ActivationOperationID  string      `json:"activation_operation_id"`
	ActivationPlanID       string      `json:"activation_plan_id"`
	ClientCompletionID     string      `json:"client_completion_id"`
	CurrentClientUseID     string      `json:"current_client_use_id"`
	SourceSelectionID      string      `json:"source_selection_id"`
	FilesRetired           int         `json:"files_retired"`
	BytesRetired           int64       `json:"bytes_retired"`
	FinalObjectIdentity    string      `json:"final_object_identity"`
	ClientSnapshotID       string      `json:"client_snapshot_id"`
}

func (marker ExecutionRetentionIntent) Validate() error {
	if marker.Schema != ExecutionRetentionIntentSchemaV1 || marker.Basis != ExecutionRetentionBasisComplete ||
		!canonicalSHA256ID(marker.PlanID) || !canonicalSHA256ID(marker.IntentID) || !canonicalSHA256ID(marker.CompletionID) ||
		!canonicalSHA256ID(marker.SearchScopeID) || !canonicalSHA256ID(marker.MetafileVariantID) ||
		!canonicalSHA256ID(marker.MaterializeOperationID) || !canonicalPlanID(marker.MaterializePlanID) ||
		!canonicalSHA256ID(marker.ActivationOperationID) || !canonicalPlanID(marker.ActivationPlanID) ||
		!canonicalSHA256ID(marker.ClientCompletionID) || !canonicalSHA256ID(marker.CurrentClientUseID) ||
		!canonicalSHA256ID(marker.SourceSelectionID) || !canonicalSHA256ID(marker.ClientSnapshotID) ||
		marker.FilesRetired <= 0 || marker.FilesRetired > hardExecutionMaxFiles || marker.BytesRetired <= 0 ||
		marker.BytesRetired > hardExecutionMaxContent {
		return fmt.Errorf("%w: source retirement retention intent is invalid", ErrExecutionIntegrity)
	}
	if parsed, err := ParseOperationID(marker.OperationID.String()); err != nil || parsed != marker.OperationID {
		return fmt.Errorf("%w: source retirement retention operation is invalid", ErrExecutionIntegrity)
	}
	if derived, err := executionOperationID(marker.PlanID); err != nil || derived != marker.OperationID {
		return fmt.Errorf("%w: source retirement retention operation disagrees with its plan", ErrExecutionIntegrity)
	}
	operationRoot, operationErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
	targetRoot, targetErr := fsbind.ParseIdentity(marker.TargetRootIdentity)
	final, finalErr := fsbind.ParseIdentity(marker.FinalObjectIdentity)
	if operationErr != nil || targetErr != nil || finalErr != nil || operationRoot.IsZero() || targetRoot.IsZero() || final.IsZero() {
		return fmt.Errorf("%w: source retirement retention filesystem identity is invalid", ErrExecutionIntegrity)
	}
	return nil
}

type ExecutionRetentionComplete struct {
	Schema                string                     `json:"schema"`
	OperationID           OperationID                `json:"operation_id"`
	OperationRootIdentity string                     `json:"operation_root_identity"`
	TargetRootIdentity    string                     `json:"target_root_identity"`
	IntentMarkerID        ExecutionRetentionMarkerID `json:"intent_marker_id"`
}

func (marker ExecutionRetentionComplete) Validate() error {
	if marker.Schema != ExecutionRetentionCompleteSchemaV1 {
		return fmt.Errorf("%w: source retirement retention completion is invalid", ErrExecutionIntegrity)
	}
	if parsed, err := ParseOperationID(marker.OperationID.String()); err != nil || parsed != marker.OperationID {
		return fmt.Errorf("%w: source retirement retention completion operation is invalid", ErrExecutionIntegrity)
	}
	if parsed, err := ParseExecutionRetentionMarkerID(marker.IntentMarkerID.String()); err != nil || parsed != marker.IntentMarkerID {
		return fmt.Errorf("%w: source retirement retention completion marker is invalid", ErrExecutionIntegrity)
	}
	operationRoot, operationErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
	targetRoot, targetErr := fsbind.ParseIdentity(marker.TargetRootIdentity)
	if operationErr != nil || targetErr != nil || operationRoot.IsZero() || targetRoot.IsZero() {
		return fmt.Errorf("%w: source retirement retention completion filesystem identity is invalid", ErrExecutionIntegrity)
	}
	return nil
}

func EncodeExecutionRetentionIntent(marker ExecutionRetentionIntent) ([]byte, ExecutionRetentionMarkerID, error) {
	if err := marker.Validate(); err != nil {
		return nil, "", err
	}
	raw, id, err := encodeExecutionMarker(executionRetentionIntentDomain, marker, maximumExecutionRetentionMarker)
	return raw, ExecutionRetentionMarkerID(id), err
}

func DecodeExecutionRetentionIntent(reader io.Reader) (ExecutionRetentionIntent, ExecutionRetentionMarkerID, error) {
	var marker ExecutionRetentionIntent
	id, err := decodeExecutionMarker(reader, maximumExecutionRetentionMarker, executionRetentionIntentDomain, &marker, func() error {
		return marker.Validate()
	})
	if err != nil {
		return ExecutionRetentionIntent{}, "", err
	}
	return marker, ExecutionRetentionMarkerID(id), nil
}

func EncodeExecutionRetentionComplete(marker ExecutionRetentionComplete) ([]byte, ExecutionRetentionMarkerID, error) {
	if err := marker.Validate(); err != nil {
		return nil, "", err
	}
	raw, id, err := encodeExecutionMarker(executionRetentionCompleteDomain, marker, maximumExecutionRetentionMarker)
	return raw, ExecutionRetentionMarkerID(id), err
}

func DecodeExecutionRetentionComplete(reader io.Reader) (ExecutionRetentionComplete, ExecutionRetentionMarkerID, error) {
	var marker ExecutionRetentionComplete
	id, err := decodeExecutionMarker(reader, maximumExecutionRetentionMarker, executionRetentionCompleteDomain, &marker, func() error {
		return marker.Validate()
	})
	if err != nil {
		return ExecutionRetentionComplete{}, "", err
	}
	return marker, ExecutionRetentionMarkerID(id), nil
}
