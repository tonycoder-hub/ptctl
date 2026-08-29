package sourceretire

import (
	"fmt"
	"io"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

const (
	ParentCleanupForgetIntentSchemaV1 = "ptctl.source-retirement-parent-cleanup-forget-intent/v1"
	ParentCleanupForgetBasisExact     = "exact_retention_tombstone_selected_for_erasure"

	parentCleanupForgetIntentDomain = "ptctl-source-retirement-parent-cleanup-forget-intent-v1\x00"
	parentCleanupForgetRootSuffix   = ".json"
	maximumParentCleanupForgetBytes = int64(8 << 10)
)

type ParentCleanupForgetMarkerID string

func ParseParentCleanupForgetMarkerID(value string) (ParentCleanupForgetMarkerID, error) {
	if !canonicalSHA256ID(value) {
		return "", fmt.Errorf("invalid parent cleanup forget marker ID")
	}
	return ParentCleanupForgetMarkerID(value), nil
}

func (id ParentCleanupForgetMarkerID) String() string { return string(id) }

// ParentCleanupForgetIntent is a temporary root-level recovery authority. It is
// published before the retained tombstone is touched and removed only after
// the exact operation subtree is durably absent. Removing this last marker is
// deliberately the point after which historical attribution no longer exists.
type ParentCleanupForgetIntent struct {
	Schema                    string                         `json:"schema"`
	OperationID               ParentCleanupOperationID       `json:"operation_id"`
	OperationRootIdentity     string                         `json:"operation_root_identity"`
	TargetRootIdentity        string                         `json:"target_root_identity"`
	CleanupPlanID             string                         `json:"cleanup_plan_id"`
	RetentionIntentMarkerID   ParentCleanupRetentionMarkerID `json:"retention_intent_marker_id"`
	RetentionCompleteMarkerID ParentCleanupRetentionMarkerID `json:"retention_complete_marker_id"`
	RetentionIntent           ParentCleanupRetentionIntent   `json:"retention_intent"`
	RetentionComplete         ParentCleanupRetentionComplete `json:"retention_complete"`
	Basis                     string                         `json:"basis"`
}

func (marker ParentCleanupForgetIntent) Validate() error {
	if marker.Schema != ParentCleanupForgetIntentSchemaV1 || marker.Basis != ParentCleanupForgetBasisExact ||
		!canonicalSHA256ID(marker.CleanupPlanID) {
		return fmt.Errorf("%w: parent cleanup forget intent is invalid", ErrExecutionIntegrity)
	}
	if parsed, err := ParseParentCleanupOperationID(marker.OperationID.String()); err != nil || parsed != marker.OperationID {
		return fmt.Errorf("%w: parent cleanup forget operation is invalid", ErrExecutionIntegrity)
	}
	if derived, err := ParentCleanupOperationIDForPlanID(marker.CleanupPlanID); err != nil || derived != marker.OperationID {
		return fmt.Errorf("%w: parent cleanup forget operation disagrees with its plan", ErrExecutionIntegrity)
	}
	if parsed, err := ParseParentCleanupRetentionMarkerID(marker.RetentionIntentMarkerID.String()); err != nil || parsed != marker.RetentionIntentMarkerID {
		return fmt.Errorf("%w: parent cleanup forget retention intent link is invalid", ErrExecutionIntegrity)
	}
	if parsed, err := ParseParentCleanupRetentionMarkerID(marker.RetentionCompleteMarkerID.String()); err != nil || parsed != marker.RetentionCompleteMarkerID {
		return fmt.Errorf("%w: parent cleanup forget retention completion link is invalid", ErrExecutionIntegrity)
	}
	intentRaw, intentID, intentErr := EncodeParentCleanupRetentionIntent(marker.RetentionIntent)
	completeRaw, completeID, completeErr := EncodeParentCleanupRetentionComplete(marker.RetentionComplete)
	if intentErr != nil || completeErr != nil || len(intentRaw) == 0 || len(completeRaw) == 0 ||
		intentID != marker.RetentionIntentMarkerID || completeID != marker.RetentionCompleteMarkerID ||
		marker.RetentionIntent.OperationID != marker.OperationID || marker.RetentionComplete.OperationID != marker.OperationID ||
		marker.RetentionIntent.OperationRootIdentity != marker.OperationRootIdentity || marker.RetentionComplete.OperationRootIdentity != marker.OperationRootIdentity ||
		marker.RetentionIntent.TargetRootIdentity != marker.TargetRootIdentity || marker.RetentionComplete.TargetRootIdentity != marker.TargetRootIdentity ||
		marker.RetentionIntent.CleanupPlanID != marker.CleanupPlanID || marker.RetentionComplete.IntentMarkerID != marker.RetentionIntentMarkerID {
		return fmt.Errorf("%w: parent cleanup forget retained tombstone link is invalid", ErrExecutionIntegrity)
	}
	operationRoot, operationErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
	targetRoot, targetErr := fsbind.ParseIdentity(marker.TargetRootIdentity)
	if operationErr != nil || targetErr != nil || operationRoot.IsZero() || targetRoot.IsZero() {
		return fmt.Errorf("%w: parent cleanup forget filesystem identity is invalid", ErrExecutionIntegrity)
	}
	return nil
}

func EncodeParentCleanupForgetIntent(marker ParentCleanupForgetIntent) ([]byte, ParentCleanupForgetMarkerID, error) {
	if err := marker.Validate(); err != nil {
		return nil, "", err
	}
	raw, id, err := encodeExecutionMarker(parentCleanupForgetIntentDomain, marker, maximumParentCleanupForgetBytes)
	return raw, ParentCleanupForgetMarkerID(id), err
}

func DecodeParentCleanupForgetIntent(reader io.Reader) (ParentCleanupForgetIntent, ParentCleanupForgetMarkerID, error) {
	var marker ParentCleanupForgetIntent
	id, err := decodeExecutionMarker(reader, maximumParentCleanupForgetBytes, parentCleanupForgetIntentDomain, &marker, func() error {
		return marker.Validate()
	})
	if err != nil {
		return ParentCleanupForgetIntent{}, "", err
	}
	return marker, ParentCleanupForgetMarkerID(id), nil
}

func ParentCleanupForgetRootName(operation ParentCleanupOperationID) (string, error) {
	parsed, err := ParseParentCleanupOperationID(operation.String())
	if err != nil || parsed != operation {
		return "", fmt.Errorf("invalid parent cleanup operation ID")
	}
	return materialize.ParentCleanupForgetMarkerPrefix + strings.TrimPrefix(operation.String(), markerIDPrefix) + parentCleanupForgetRootSuffix, nil
}

func ParseParentCleanupForgetRootName(name string) (ParentCleanupOperationID, error) {
	if !strings.HasPrefix(name, materialize.ParentCleanupForgetMarkerPrefix) || !strings.HasSuffix(name, parentCleanupForgetRootSuffix) {
		return "", fmt.Errorf("invalid parent cleanup forget marker name")
	}
	hexDigest := strings.TrimSuffix(strings.TrimPrefix(name, materialize.ParentCleanupForgetMarkerPrefix), parentCleanupForgetRootSuffix)
	if len(hexDigest) != 64 {
		return "", fmt.Errorf("invalid parent cleanup forget marker name")
	}
	return ParseParentCleanupOperationID(markerIDPrefix + hexDigest)
}

func parentCleanupForgetPendingName(id ParentCleanupForgetMarkerID) string {
	return "forget-" + strings.TrimPrefix(id.String(), markerIDPrefix) + ".pending"
}
