package clientadopt

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const (
	RetentionIntentSchemaV1   = "ptctl.client-adopt-retention-intent/v1"
	RetentionCompleteSchemaV1 = "ptctl.client-adopt-retention-complete/v1"
	RetentionBasisTerminal    = "terminal_adoption_journal_exact"

	retentionDirectoryName   = "retention"
	retentionIntentName      = "intent.json"
	retentionCompleteName    = "complete.json"
	retentionIntentPending   = "intent.pending"
	retentionCompletePending = "complete.pending"

	retentionIntentDomain   = "ptctl-client-adopt-retention-intent-v1\x00"
	retentionCompleteDomain = "ptctl-client-adopt-retention-complete-v1\x00"
)

type RetentionMarkerID string

func ParseRetentionMarkerID(value string) (RetentionMarkerID, error) {
	if !canonicalSHA256ID(value) {
		return "", fmt.Errorf("invalid client adoption retention marker ID")
	}
	return RetentionMarkerID(value), nil
}

func (id RetentionMarkerID) String() string { return string(id) }

// RetentionIntent copies the exact bounded terminal journal into one durable
// deletion authority. It is historical evidence only; it does not claim that
// the downloader job or materialized final still has the recorded state.
type RetentionIntent struct {
	Schema                string      `json:"schema"`
	OperationID           OperationID `json:"operation_id"`
	OperationRootIdentity string      `json:"operation_root_identity"`
	TargetRootIdentity    string      `json:"target_root_identity"`
	PlanID                string      `json:"plan_id"`
	IntentID              MarkerID    `json:"intent_id"`
	Intent                Intent      `json:"intent"`
	AttemptIDs            []MarkerID  `json:"attempt_ids"`
	Attempts              []Attempt   `json:"attempts"`
	CompletionID          MarkerID    `json:"completion_id"`
	Completion            Completion  `json:"completion"`
	Basis                 string      `json:"basis"`
}

func (marker RetentionIntent) Validate() error {
	if marker.Schema != RetentionIntentSchemaV1 || marker.Basis != RetentionBasisTerminal ||
		marker.Intent.Validate() != nil || marker.Completion.Validate() != nil ||
		marker.OperationID != marker.Intent.OperationID || marker.PlanID != marker.Intent.PlanID ||
		marker.Completion.OperationID != marker.OperationID || marker.Completion.PlanID != marker.PlanID ||
		marker.OperationRootIdentity != marker.Intent.OperationRootIdentity ||
		marker.Intent.Plan.TargetRootIdentity != marker.TargetRootIdentity ||
		len(marker.Attempts) == 0 || len(marker.Attempts) > maximumAttempts || len(marker.Attempts) != len(marker.AttemptIDs) {
		return fmt.Errorf("%w: client adoption retention intent is invalid", ErrIntegrity)
	}
	operationRoot, operationErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
	targetRoot, targetErr := fsbind.ParseIdentity(marker.TargetRootIdentity)
	if operationErr != nil || targetErr != nil || operationRoot.IsZero() || targetRoot.IsZero() {
		return fmt.Errorf("%w: client adoption retention filesystem identity is invalid", ErrIntegrity)
	}
	intentRaw, intentID, err := encodeIntent(marker.Intent)
	if err != nil || len(intentRaw) == 0 || marker.IntentID != intentID {
		return fmt.Errorf("%w: client adoption retention intent link is invalid", ErrIntegrity)
	}
	for index, attempt := range marker.Attempts {
		_, id, encodeErr := encodeAttempt(attempt)
		if encodeErr != nil || id != marker.AttemptIDs[index] || attempt.OperationID != marker.OperationID ||
			attempt.PlanID != marker.PlanID || attempt.Sequence != index+1 ||
			attempt.MetafileVariantID != marker.Intent.Plan.MetafileVariantID ||
			attempt.MetafileBytes != marker.Intent.Plan.MetafileBytes ||
			!attemptMatchesPlan(attempt, marker.Intent.Plan) {
			return fmt.Errorf("%w: client adoption retention attempt chain is invalid", ErrIntegrity)
		}
		if index == 0 && attempt.PreviousAttemptID != "" {
			return fmt.Errorf("%w: client adoption retention attempt predecessor is invalid", ErrIntegrity)
		}
		if index > 0 && attempt.PreviousAttemptID != marker.AttemptIDs[index-1].String() {
			return fmt.Errorf("%w: client adoption retention attempt predecessor is invalid", ErrIntegrity)
		}
	}
	_, completionID, err := encodeCompletion(marker.Completion)
	if err != nil || completionID != marker.CompletionID || marker.Completion.AttemptID != marker.AttemptIDs[len(marker.AttemptIDs)-1] ||
		marker.Completion.ContentPathRef != marker.Intent.Plan.ExpectedContentPathRef ||
		marker.Completion.FinalObjectIdentity != marker.Intent.Plan.FinalObjectIdentity ||
		!completionMatchesPlan(marker.Completion, marker.Intent.Plan, marker.Attempts[len(marker.Attempts)-1]) {
		return fmt.Errorf("%w: client adoption retention completion link is invalid", ErrIntegrity)
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
	CompletionID          MarkerID          `json:"completion_id"`
}

func (marker RetentionComplete) Validate() error {
	if marker.Schema != RetentionCompleteSchemaV1 || !canonicalPlanID(marker.PlanID) {
		return fmt.Errorf("%w: client adoption retention completion is invalid", ErrIntegrity)
	}
	if parsed, err := ParseOperationID(marker.OperationID.String()); err != nil || parsed != marker.OperationID {
		return fmt.Errorf("%w: client adoption retention completion operation is invalid", ErrIntegrity)
	}
	if OperationIDForPlan(marker.PlanID) != marker.OperationID {
		return fmt.Errorf("%w: client adoption retention completion operation disagrees with its plan", ErrIntegrity)
	}
	if parsed, err := ParseRetentionMarkerID(marker.IntentMarkerID.String()); err != nil || parsed != marker.IntentMarkerID {
		return fmt.Errorf("%w: client adoption retention completion intent link is invalid", ErrIntegrity)
	}
	if parsed, err := parseMarkerID(marker.CompletionID.String()); err != nil || parsed != marker.CompletionID {
		return fmt.Errorf("%w: client adoption retention completion marker link is invalid", ErrIntegrity)
	}
	operationRoot, operationErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
	targetRoot, targetErr := fsbind.ParseIdentity(marker.TargetRootIdentity)
	if operationErr != nil || targetErr != nil || operationRoot.IsZero() || targetRoot.IsZero() {
		return fmt.Errorf("%w: client adoption retention completion filesystem identity is invalid", ErrIntegrity)
	}
	return nil
}

func EncodeRetentionIntent(marker RetentionIntent) ([]byte, RetentionMarkerID, error) {
	if err := marker.Validate(); err != nil {
		return nil, "", err
	}
	raw, err := encodeCanonical(marker)
	return retentionIdentity(retentionIntentDomain, raw, err)
}

func DecodeRetentionIntent(reader io.Reader) (RetentionIntent, RetentionMarkerID, error) {
	var marker RetentionIntent
	raw, err := readCanonical(reader, &marker)
	if err != nil {
		return RetentionIntent{}, "", err
	}
	canonical, id, err := EncodeRetentionIntent(marker)
	if err != nil || !bytes.Equal(raw, canonical) {
		return RetentionIntent{}, "", fmt.Errorf("%w: client adoption retention intent is not canonical", ErrIntegrity)
	}
	return marker, id, nil
}

func EncodeRetentionComplete(marker RetentionComplete) ([]byte, RetentionMarkerID, error) {
	if err := marker.Validate(); err != nil {
		return nil, "", err
	}
	raw, err := encodeCanonical(marker)
	return retentionIdentity(retentionCompleteDomain, raw, err)
}

func DecodeRetentionComplete(reader io.Reader) (RetentionComplete, RetentionMarkerID, error) {
	var marker RetentionComplete
	raw, err := readCanonical(reader, &marker)
	if err != nil {
		return RetentionComplete{}, "", err
	}
	canonical, id, err := EncodeRetentionComplete(marker)
	if err != nil || !bytes.Equal(raw, canonical) {
		return RetentionComplete{}, "", fmt.Errorf("%w: client adoption retention completion is not canonical", ErrIntegrity)
	}
	return marker, id, nil
}

func retentionIdentity(domain string, raw []byte, err error) ([]byte, RetentionMarkerID, error) {
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(append([]byte(domain), raw...))
	return raw, RetentionMarkerID("sha256:" + hex.EncodeToString(digest[:])), nil
}
