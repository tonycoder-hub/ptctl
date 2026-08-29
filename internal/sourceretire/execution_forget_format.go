package sourceretire

import (
	"fmt"
	"io"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

const (
	ExecutionForgetIntentSchemaV1 = "ptctl.source-retirement-forget-intent/v1"
	ExecutionForgetBasisExact     = "exact_retention_tombstone_selected_for_erasure"

	executionForgetIntentDomain = "ptctl-source-retirement-forget-intent-v1\x00"
	executionForgetRootSuffix   = ".json"
	maximumExecutionForgetBytes = int64(8 << 10)
)

type ExecutionForgetMarkerID string

func ParseExecutionForgetMarkerID(value string) (ExecutionForgetMarkerID, error) {
	if !canonicalSHA256ID(value) {
		return "", fmt.Errorf("invalid source retirement forget marker ID")
	}
	return ExecutionForgetMarkerID(value), nil
}

func (id ExecutionForgetMarkerID) String() string { return string(id) }

// ExecutionForgetIntent is a temporary root-level recovery authority. It is
// published before the retained tombstone is touched and removed only after
// the exact operation subtree is durably absent. Removing this last marker is
// deliberately the point after which historical attribution no longer exists.
type ExecutionForgetIntent struct {
	Schema                    string                     `json:"schema"`
	OperationID               OperationID                `json:"operation_id"`
	OperationRootIdentity     string                     `json:"operation_root_identity"`
	TargetRootIdentity        string                     `json:"target_root_identity"`
	PlanID                    string                     `json:"plan_id"`
	RetentionIntentMarkerID   ExecutionRetentionMarkerID `json:"retention_intent_marker_id"`
	RetentionCompleteMarkerID ExecutionRetentionMarkerID `json:"retention_complete_marker_id"`
	RetentionIntent           ExecutionRetentionIntent   `json:"retention_intent"`
	RetentionComplete         ExecutionRetentionComplete `json:"retention_complete"`
	Basis                     string                     `json:"basis"`
}

func (marker ExecutionForgetIntent) Validate() error {
	if marker.Schema != ExecutionForgetIntentSchemaV1 || marker.Basis != ExecutionForgetBasisExact ||
		!canonicalSHA256ID(marker.PlanID) {
		return fmt.Errorf("%w: source retirement forget intent is invalid", ErrExecutionIntegrity)
	}
	if parsed, err := ParseOperationID(marker.OperationID.String()); err != nil || parsed != marker.OperationID {
		return fmt.Errorf("%w: source retirement forget operation is invalid", ErrExecutionIntegrity)
	}
	if derived, err := executionOperationID(marker.PlanID); err != nil || derived != marker.OperationID {
		return fmt.Errorf("%w: source retirement forget operation disagrees with its plan", ErrExecutionIntegrity)
	}
	if parsed, err := ParseExecutionRetentionMarkerID(marker.RetentionIntentMarkerID.String()); err != nil || parsed != marker.RetentionIntentMarkerID {
		return fmt.Errorf("%w: source retirement forget retention intent link is invalid", ErrExecutionIntegrity)
	}
	if parsed, err := ParseExecutionRetentionMarkerID(marker.RetentionCompleteMarkerID.String()); err != nil || parsed != marker.RetentionCompleteMarkerID {
		return fmt.Errorf("%w: source retirement forget retention completion link is invalid", ErrExecutionIntegrity)
	}
	intentRaw, intentID, intentErr := EncodeExecutionRetentionIntent(marker.RetentionIntent)
	completeRaw, completeID, completeErr := EncodeExecutionRetentionComplete(marker.RetentionComplete)
	if intentErr != nil || completeErr != nil || len(intentRaw) == 0 || len(completeRaw) == 0 ||
		intentID != marker.RetentionIntentMarkerID || completeID != marker.RetentionCompleteMarkerID ||
		marker.RetentionIntent.OperationID != marker.OperationID || marker.RetentionComplete.OperationID != marker.OperationID ||
		marker.RetentionIntent.OperationRootIdentity != marker.OperationRootIdentity || marker.RetentionComplete.OperationRootIdentity != marker.OperationRootIdentity ||
		marker.RetentionIntent.TargetRootIdentity != marker.TargetRootIdentity || marker.RetentionComplete.TargetRootIdentity != marker.TargetRootIdentity ||
		marker.RetentionIntent.PlanID != marker.PlanID || marker.RetentionComplete.IntentMarkerID != marker.RetentionIntentMarkerID {
		return fmt.Errorf("%w: source retirement forget retained tombstone link is invalid", ErrExecutionIntegrity)
	}
	operationRoot, operationErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
	targetRoot, targetErr := fsbind.ParseIdentity(marker.TargetRootIdentity)
	if operationErr != nil || targetErr != nil || operationRoot.IsZero() || targetRoot.IsZero() {
		return fmt.Errorf("%w: source retirement forget filesystem identity is invalid", ErrExecutionIntegrity)
	}
	return nil
}

func EncodeExecutionForgetIntent(marker ExecutionForgetIntent) ([]byte, ExecutionForgetMarkerID, error) {
	if err := marker.Validate(); err != nil {
		return nil, "", err
	}
	raw, id, err := encodeExecutionMarker(executionForgetIntentDomain, marker, maximumExecutionForgetBytes)
	return raw, ExecutionForgetMarkerID(id), err
}

func DecodeExecutionForgetIntent(reader io.Reader) (ExecutionForgetIntent, ExecutionForgetMarkerID, error) {
	var marker ExecutionForgetIntent
	id, err := decodeExecutionMarker(reader, maximumExecutionForgetBytes, executionForgetIntentDomain, &marker, func() error {
		return marker.Validate()
	})
	if err != nil {
		return ExecutionForgetIntent{}, "", err
	}
	return marker, ExecutionForgetMarkerID(id), nil
}

func ExecutionForgetRootName(operation OperationID) (string, error) {
	parsed, err := ParseOperationID(operation.String())
	if err != nil || parsed != operation {
		return "", fmt.Errorf("invalid source retirement operation ID")
	}
	return materialize.SourceRetireForgetMarkerPrefix + strings.TrimPrefix(operation.String(), markerIDPrefix) + executionForgetRootSuffix, nil
}

func ParseExecutionForgetRootName(name string) (OperationID, error) {
	if !strings.HasPrefix(name, materialize.SourceRetireForgetMarkerPrefix) || !strings.HasSuffix(name, executionForgetRootSuffix) {
		return "", fmt.Errorf("invalid source retirement forget marker name")
	}
	hexDigest := strings.TrimSuffix(strings.TrimPrefix(name, materialize.SourceRetireForgetMarkerPrefix), executionForgetRootSuffix)
	if len(hexDigest) != 64 {
		return "", fmt.Errorf("invalid source retirement forget marker name")
	}
	return ParseOperationID(markerIDPrefix + hexDigest)
}

func executionForgetPendingName(id ExecutionForgetMarkerID) string {
	return "forget-" + strings.TrimPrefix(id.String(), markerIDPrefix) + ".pending"
}
