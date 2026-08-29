package clientadopt

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

const (
	ForgetIntentSchemaV1 = "ptctl.client-adoption-forget-intent/v1"
	ForgetBasisExact     = "exact_adoption_retention_tombstone_selected_for_erasure"

	forgetIntentDigestDomain = "ptctl-client-adoption-forget-intent-v1\x00"
	forgetRootSuffix         = ".json"
	maximumForgetMarkerBytes = maximumMarkerBytes
)

type ForgetMarkerID string

func ParseForgetMarkerID(value string) (ForgetMarkerID, error) {
	if !canonicalSHA256ID(value) {
		return "", fmt.Errorf("invalid client adoption forget marker ID")
	}
	return ForgetMarkerID(value), nil
}

func (id ForgetMarkerID) String() string { return string(id) }

// ForgetIntent is the temporary root-level recovery authority for erasing one
// exact client-adoption retention tombstone. It is published before the
// operation subtree is touched and removed only after that subtree is durably
// absent.
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
	if marker.Schema != ForgetIntentSchemaV1 || marker.Basis != ForgetBasisExact || !canonicalPlanID(marker.PlanID) ||
		OperationIDForPlan(marker.PlanID) != marker.OperationID {
		return fmt.Errorf("%w: client adoption forget intent is invalid", ErrIntegrity)
	}
	if parsed, err := ParseOperationID(marker.OperationID.String()); err != nil || parsed != marker.OperationID {
		return fmt.Errorf("%w: client adoption forget operation is invalid", ErrIntegrity)
	}
	if parsed, err := ParseRetentionMarkerID(marker.RetentionIntentMarkerID.String()); err != nil || parsed != marker.RetentionIntentMarkerID {
		return fmt.Errorf("%w: client adoption forget retention intent link is invalid", ErrIntegrity)
	}
	if parsed, err := ParseRetentionMarkerID(marker.RetentionCompleteMarkerID.String()); err != nil || parsed != marker.RetentionCompleteMarkerID {
		return fmt.Errorf("%w: client adoption forget retention completion link is invalid", ErrIntegrity)
	}
	intentRaw, intentID, intentErr := EncodeRetentionIntent(marker.RetentionIntent)
	completeRaw, completeID, completeErr := EncodeRetentionComplete(marker.RetentionComplete)
	if intentErr != nil || completeErr != nil || len(intentRaw) == 0 || len(completeRaw) == 0 ||
		intentID != marker.RetentionIntentMarkerID || completeID != marker.RetentionCompleteMarkerID ||
		marker.RetentionIntent.OperationID != marker.OperationID || marker.RetentionComplete.OperationID != marker.OperationID ||
		marker.RetentionIntent.OperationRootIdentity != marker.OperationRootIdentity || marker.RetentionComplete.OperationRootIdentity != marker.OperationRootIdentity ||
		marker.RetentionIntent.TargetRootIdentity != marker.TargetRootIdentity || marker.RetentionComplete.TargetRootIdentity != marker.TargetRootIdentity ||
		marker.RetentionIntent.PlanID != marker.PlanID || marker.RetentionComplete.PlanID != marker.PlanID ||
		marker.RetentionComplete.IntentMarkerID != marker.RetentionIntentMarkerID ||
		!retentionCompleteMatchesIntent(marker.RetentionComplete, marker.RetentionIntent, marker.RetentionIntentMarkerID) {
		return fmt.Errorf("%w: client adoption forget retained tombstone link is invalid", ErrIntegrity)
	}
	operationRoot, operationErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
	targetRoot, targetErr := fsbind.ParseIdentity(marker.TargetRootIdentity)
	if operationErr != nil || targetErr != nil || operationRoot.IsZero() || targetRoot.IsZero() {
		return fmt.Errorf("%w: client adoption forget filesystem identity is invalid", ErrIntegrity)
	}
	return nil
}

func EncodeForgetIntent(marker ForgetIntent) ([]byte, ForgetMarkerID, error) {
	if err := marker.Validate(); err != nil {
		return nil, "", err
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		return nil, "", fmt.Errorf("encode client adoption forget marker failed")
	}
	raw = append(raw, '\n')
	if int64(len(raw)) > maximumForgetMarkerBytes {
		return nil, "", fmt.Errorf("%w: client adoption forget marker byte limit exceeded", ErrPolicy)
	}
	digest := sha256.Sum256(append([]byte(forgetIntentDigestDomain), raw...))
	return raw, ForgetMarkerID("sha256:" + hex.EncodeToString(digest[:])), nil
}

func DecodeForgetIntent(reader io.Reader) (ForgetIntent, ForgetMarkerID, error) {
	var marker ForgetIntent
	if reader == nil {
		return marker, "", fmt.Errorf("%w: client adoption forget marker reader is unavailable", ErrIntegrity)
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maximumForgetMarkerBytes+1))
	if err != nil {
		return marker, "", err
	}
	if int64(len(raw)) > maximumForgetMarkerBytes || len(raw) == 0 || raw[len(raw)-1] != '\n' || !json.Valid(raw) {
		return marker, "", fmt.Errorf("%w: client adoption forget marker is not canonical", ErrIntegrity)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return ForgetIntent{}, "", fmt.Errorf("%w: client adoption forget marker is not canonical", ErrIntegrity)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ForgetIntent{}, "", fmt.Errorf("%w: client adoption forget marker is not canonical", ErrIntegrity)
	}
	canonical, id, err := EncodeForgetIntent(marker)
	if err != nil || !bytes.Equal(raw, canonical) {
		return ForgetIntent{}, "", fmt.Errorf("%w: client adoption forget marker is not canonical", ErrIntegrity)
	}
	return marker, id, nil
}

func ForgetRootName(operation OperationID) (string, error) {
	parsed, err := ParseOperationID(operation.String())
	if err != nil || parsed != operation {
		return "", fmt.Errorf("invalid client adoption operation ID")
	}
	return materialize.ClientAdoptForgetMarkerPrefix + strings.TrimPrefix(operation.String(), "sha256:") + forgetRootSuffix, nil
}

func ParseForgetRootName(name string) (OperationID, error) {
	if !strings.HasPrefix(name, materialize.ClientAdoptForgetMarkerPrefix) || !strings.HasSuffix(name, forgetRootSuffix) {
		return "", fmt.Errorf("invalid client adoption forget marker name")
	}
	hexDigest := strings.TrimSuffix(strings.TrimPrefix(name, materialize.ClientAdoptForgetMarkerPrefix), forgetRootSuffix)
	if len(hexDigest) != 64 {
		return "", fmt.Errorf("invalid client adoption forget marker name")
	}
	return ParseOperationID("sha256:" + hexDigest)
}

func forgetPendingName(id ForgetMarkerID) string {
	return "forget-" + strings.TrimPrefix(id.String(), "sha256:") + ".pending"
}
