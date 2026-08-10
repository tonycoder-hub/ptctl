package materialize

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const (
	ForgetIntentSchemaV1 = "ptctl.materialize-forget-intent/v1"
	ForgetBasisExact     = "exact_retention_tombstone_selected_for_erasure"

	forgetIntentDigestDomain = "ptctl-materialize-forget-intent-v1\x00"
	forgetRootSuffix         = ".json"
	maxForgetMarkerBytes     = int64(8 << 10)
)

type ForgetMarkerID string

func ParseForgetMarkerID(value string) (ForgetMarkerID, error) {
	if !canonicalSHA256ID(value) {
		return "", fmt.Errorf("invalid materialize forget marker ID")
	}
	return ForgetMarkerID(value), nil
}

func (id ForgetMarkerID) String() string { return string(id) }

// ForgetIntent is the temporary root-level recovery authority for erasing one
// exact materialize retention tombstone. It is published before the operation
// subtree is touched and removed only after that subtree is durably absent.
type ForgetIntent struct {
	Schema                    string            `json:"schema"`
	OperationID               OperationID       `json:"operation_id"`
	OperationRootIdentity     string            `json:"operation_root_identity"`
	TargetRootIdentity        string            `json:"target_root_identity"`
	PlanID                    string            `json:"plan_id"`
	RetentionIntentMarkerID   RetentionMarkerID `json:"retention_intent_marker_id"`
	RetentionCompleteMarkerID RetentionMarkerID `json:"retention_complete_marker_id"`
	RetentionIntent           RetentionIntent   `json:"retention_intent"`
	RetentionComplete         RetentionComplete `json:"retention_complete"`
	Basis                     string            `json:"basis"`
}

func (marker ForgetIntent) Validate() error {
	if marker.Schema != ForgetIntentSchemaV1 || marker.Basis != ForgetBasisExact || !canonicalPlanID(marker.PlanID) {
		return fmt.Errorf("%w: materialize forget intent is invalid", ErrIntegrity)
	}
	if parsed, err := ParseOperationID(marker.OperationID.String()); err != nil || parsed != marker.OperationID {
		return fmt.Errorf("%w: materialize forget operation is invalid", ErrIntegrity)
	}
	if parsed, err := ParseRetentionMarkerID(marker.RetentionIntentMarkerID.String()); err != nil || parsed != marker.RetentionIntentMarkerID {
		return fmt.Errorf("%w: materialize forget retention intent link is invalid", ErrIntegrity)
	}
	if parsed, err := ParseRetentionMarkerID(marker.RetentionCompleteMarkerID.String()); err != nil || parsed != marker.RetentionCompleteMarkerID {
		return fmt.Errorf("%w: materialize forget retention completion link is invalid", ErrIntegrity)
	}
	intentRaw, intentID, intentErr := EncodeRetentionIntent(marker.RetentionIntent)
	completeRaw, completeID, completeErr := EncodeRetentionComplete(marker.RetentionComplete)
	if intentErr != nil || completeErr != nil || len(intentRaw) == 0 || len(completeRaw) == 0 ||
		intentID != marker.RetentionIntentMarkerID || completeID != marker.RetentionCompleteMarkerID ||
		marker.RetentionIntent.OperationID != marker.OperationID || marker.RetentionComplete.OperationID != marker.OperationID ||
		marker.RetentionIntent.OperationRootIdentity != marker.OperationRootIdentity || marker.RetentionComplete.OperationRootIdentity != marker.OperationRootIdentity ||
		marker.RetentionIntent.TargetRootIdentity != marker.TargetRootIdentity || marker.RetentionComplete.TargetRootIdentity != marker.TargetRootIdentity ||
		marker.RetentionIntent.PlanID != marker.PlanID || marker.RetentionComplete.IntentMarkerID != marker.RetentionIntentMarkerID {
		return fmt.Errorf("%w: materialize forget retained tombstone link is invalid", ErrIntegrity)
	}
	operationRoot, operationErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
	targetRoot, targetErr := fsbind.ParseIdentity(marker.TargetRootIdentity)
	if operationErr != nil || targetErr != nil || operationRoot.IsZero() || targetRoot.IsZero() {
		return fmt.Errorf("%w: materialize forget filesystem identity is invalid", ErrIntegrity)
	}
	return nil
}

func EncodeForgetIntent(marker ForgetIntent) ([]byte, ForgetMarkerID, error) {
	if err := marker.Validate(); err != nil {
		return nil, "", err
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		return nil, "", fmt.Errorf("encode materialize forget marker failed")
	}
	raw = append(raw, '\n')
	if int64(len(raw)) > maxForgetMarkerBytes {
		return nil, "", fmt.Errorf("%w: materialize forget marker byte limit exceeded", ErrPolicy)
	}
	digest := sha256.Sum256(append([]byte(forgetIntentDigestDomain), raw...))
	return raw, ForgetMarkerID("sha256:" + hex.EncodeToString(digest[:])), nil
}

func DecodeForgetIntent(reader io.Reader) (ForgetIntent, ForgetMarkerID, error) {
	var marker ForgetIntent
	if reader == nil {
		return marker, "", fmt.Errorf("%w: materialize forget marker reader is unavailable", ErrIntegrity)
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxForgetMarkerBytes+1))
	if err != nil {
		return marker, "", err
	}
	if int64(len(raw)) > maxForgetMarkerBytes {
		return marker, "", fmt.Errorf("%w: materialize forget marker byte limit exceeded", ErrIntegrity)
	}
	if err := decodeCanonicalJSON(raw, &marker); err != nil {
		return ForgetIntent{}, "", fmt.Errorf("%w: materialize forget marker is not canonical", ErrIntegrity)
	}
	canonical, id, err := EncodeForgetIntent(marker)
	if err != nil || !bytes.Equal(raw, canonical) {
		return ForgetIntent{}, "", fmt.Errorf("%w: materialize forget marker is not canonical", ErrIntegrity)
	}
	return marker, id, nil
}

func ForgetRootName(operation OperationID) (string, error) {
	parsed, err := ParseOperationID(operation.String())
	if err != nil || parsed != operation {
		return "", fmt.Errorf("invalid materialize operation ID")
	}
	return materializeForgetMarkerPrefix + strings.TrimPrefix(operation.String(), "sha256:") + forgetRootSuffix, nil
}

func ParseForgetRootName(name string) (OperationID, error) {
	if !strings.HasPrefix(name, materializeForgetMarkerPrefix) || !strings.HasSuffix(name, forgetRootSuffix) {
		return "", fmt.Errorf("invalid materialize forget marker name")
	}
	hexDigest := strings.TrimSuffix(strings.TrimPrefix(name, materializeForgetMarkerPrefix), forgetRootSuffix)
	if len(hexDigest) != 64 {
		return "", fmt.Errorf("invalid materialize forget marker name")
	}
	return ParseOperationID("sha256:" + hexDigest)
}

func forgetPendingName(id ForgetMarkerID) string {
	return "forget-" + strings.TrimPrefix(id.String(), "sha256:") + ".pending"
}
