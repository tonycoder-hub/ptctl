package materialize

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const (
	RetentionIntentSchemaV1   = "ptctl.materialize-retention-intent/v1"
	RetentionCompleteSchemaV1 = "ptctl.materialize-retention-complete/v1"

	RetentionBasisCommitted = "exact_final_verified_before_retirement"
	RetentionBasisAbandoned = "publication_absent_before_retirement"

	maxRetentionMarkerBytes = int64(8 << 10)

	retentionIntentDigestDomain   = "ptctl-materialize-retention-intent-v1\x00"
	retentionCompleteDigestDomain = "ptctl-materialize-retention-complete-v1\x00"
)

type RetentionMarkerID string

func ParseRetentionMarkerID(value string) (RetentionMarkerID, error) {
	if !canonicalSHA256ID(value) {
		return "", fmt.Errorf("invalid materialize retention marker ID")
	}
	return RetentionMarkerID(value), nil
}

func (id RetentionMarkerID) String() string { return string(id) }

// RetentionIntent is a small private tombstone authority written before any
// operation-state deletion. It records only a historical proof basis; it does
// not claim that a final layout remains unchanged later.
type RetentionIntent struct {
	Schema                string      `json:"schema"`
	OperationID           OperationID `json:"operation_id"`
	OperationRootIdentity string      `json:"operation_root_identity"`
	TargetRootIdentity    string      `json:"target_root_identity"`
	IntentSHA256          string      `json:"intent_sha256"`
	TerminalEventID       EventID     `json:"terminal_event_id"`
	TerminalPhase         Phase       `json:"terminal_phase"`
	PlanID                string      `json:"plan_id"`
	MetafileVariantID     string      `json:"metafile_variant_id"`
	Basis                 string      `json:"basis"`
	FinalObjectIdentity   string      `json:"final_object_identity,omitempty"`
}

func (marker RetentionIntent) Validate() error {
	if marker.Schema != RetentionIntentSchemaV1 || marker.TerminalPhase != PhaseCommitted && marker.TerminalPhase != PhaseAbandoned ||
		!canonicalSHA256ID(marker.IntentSHA256) || !canonicalPlanID(marker.PlanID) || !canonicalSHA256ID(marker.MetafileVariantID) {
		return fmt.Errorf("materialize retention intent is invalid")
	}
	if parsed, err := ParseOperationID(marker.OperationID.String()); err != nil || parsed != marker.OperationID {
		return fmt.Errorf("materialize retention intent is invalid")
	}
	if parsed, err := ParseEventID(marker.TerminalEventID.String()); err != nil || parsed != marker.TerminalEventID {
		return fmt.Errorf("materialize retention intent is invalid")
	}
	operationIdentity, operationErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
	targetIdentity, targetErr := fsbind.ParseIdentity(marker.TargetRootIdentity)
	if operationErr != nil || targetErr != nil || operationIdentity.IsZero() || targetIdentity.IsZero() {
		return fmt.Errorf("materialize retention intent is invalid")
	}
	switch marker.TerminalPhase {
	case PhaseCommitted:
		final, err := fsbind.ParseIdentity(marker.FinalObjectIdentity)
		if marker.Basis != RetentionBasisCommitted || err != nil || final.IsZero() {
			return fmt.Errorf("materialize retention intent is invalid")
		}
	case PhaseAbandoned:
		if marker.Basis != RetentionBasisAbandoned || marker.FinalObjectIdentity != "" {
			return fmt.Errorf("materialize retention intent is invalid")
		}
	}
	return nil
}

type RetentionComplete struct {
	Schema                string            `json:"schema"`
	OperationID           OperationID       `json:"operation_id"`
	OperationRootIdentity string            `json:"operation_root_identity"`
	TargetRootIdentity    string            `json:"target_root_identity"`
	IntentMarkerID        RetentionMarkerID `json:"intent_marker_id"`
}

func (marker RetentionComplete) Validate() error {
	if marker.Schema != RetentionCompleteSchemaV1 {
		return fmt.Errorf("materialize retention completion is invalid")
	}
	if parsed, err := ParseOperationID(marker.OperationID.String()); err != nil || parsed != marker.OperationID {
		return fmt.Errorf("materialize retention completion is invalid")
	}
	if parsed, err := ParseRetentionMarkerID(marker.IntentMarkerID.String()); err != nil || parsed != marker.IntentMarkerID {
		return fmt.Errorf("materialize retention completion is invalid")
	}
	operationIdentity, operationErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
	targetIdentity, targetErr := fsbind.ParseIdentity(marker.TargetRootIdentity)
	if operationErr != nil || targetErr != nil || operationIdentity.IsZero() || targetIdentity.IsZero() {
		return fmt.Errorf("materialize retention completion is invalid")
	}
	return nil
}

func EncodeRetentionIntent(marker RetentionIntent) ([]byte, RetentionMarkerID, error) {
	if err := marker.Validate(); err != nil {
		return nil, "", err
	}
	return encodeRetentionMarker(marker, retentionIntentDigestDomain)
}

func DecodeRetentionIntent(reader io.Reader) (RetentionIntent, RetentionMarkerID, error) {
	var marker RetentionIntent
	raw, err := readRetentionMarker(reader)
	if err != nil {
		return marker, "", err
	}
	if err := decodeCanonicalJSON(raw, &marker); err != nil {
		return RetentionIntent{}, "", fmt.Errorf("materialize retention intent is not canonical")
	}
	canonical, id, err := EncodeRetentionIntent(marker)
	if err != nil || !bytes.Equal(raw, canonical) {
		return RetentionIntent{}, "", fmt.Errorf("materialize retention intent is not canonical")
	}
	return marker, id, nil
}

func EncodeRetentionComplete(marker RetentionComplete) ([]byte, RetentionMarkerID, error) {
	if err := marker.Validate(); err != nil {
		return nil, "", err
	}
	return encodeRetentionMarker(marker, retentionCompleteDigestDomain)
}

func DecodeRetentionComplete(reader io.Reader) (RetentionComplete, RetentionMarkerID, error) {
	var marker RetentionComplete
	raw, err := readRetentionMarker(reader)
	if err != nil {
		return marker, "", err
	}
	if err := decodeCanonicalJSON(raw, &marker); err != nil {
		return RetentionComplete{}, "", fmt.Errorf("materialize retention completion is not canonical")
	}
	canonical, id, err := EncodeRetentionComplete(marker)
	if err != nil || !bytes.Equal(raw, canonical) {
		return RetentionComplete{}, "", fmt.Errorf("materialize retention completion is not canonical")
	}
	return marker, id, nil
}

func IntentSHA256(intent Intent) (string, error) {
	raw, err := EncodeIntent(intent)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func encodeRetentionMarker(marker any, domain string) ([]byte, RetentionMarkerID, error) {
	raw, err := json.Marshal(marker)
	if err != nil {
		return nil, "", fmt.Errorf("encode materialize retention marker failed")
	}
	raw = append(raw, '\n')
	if int64(len(raw)) > maxRetentionMarkerBytes {
		return nil, "", fmt.Errorf("materialize retention marker byte limit exceeded")
	}
	digest := sha256.Sum256(append([]byte(domain), raw...))
	return raw, RetentionMarkerID("sha256:" + hex.EncodeToString(digest[:])), nil
}

func readRetentionMarker(reader io.Reader) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("materialize retention marker reader is unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxRetentionMarkerBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxRetentionMarkerBytes {
		return nil, fmt.Errorf("materialize retention marker byte limit exceeded")
	}
	return raw, nil
}
