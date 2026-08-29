package sourceretire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

func encodeParentCleanupIntent(intent ParentCleanupIntent) ([]byte, string, error) {
	if err := intent.Validate(); err != nil {
		return nil, "", err
	}
	return encodeExecutionMarker(parentCleanupIntentDomain, intent, intent.Limits.MaxIntentBytes)
}

func decodeParentCleanupIntent(reader io.Reader, limits ParentCleanupExecutionLimits) (ParentCleanupIntent, string, error) {
	var intent ParentCleanupIntent
	raw, err := readExecutionMarker(reader, limits.MaxIntentBytes)
	if err != nil {
		return intent, "", err
	}
	if err := strictUnmarshalExecutionJSON(raw, &intent); err != nil || intent.Limits != limits {
		return ParentCleanupIntent{}, "", fmt.Errorf("%w: parent-cleanup intent is invalid", ErrExecutionIntegrity)
	}
	canonical, id, err := encodeParentCleanupIntent(intent)
	if err != nil || !bytes.Equal(raw, canonical) {
		return ParentCleanupIntent{}, "", fmt.Errorf("%w: parent-cleanup intent is not canonical", ErrExecutionIntegrity)
	}
	return intent, id, nil
}

func encodeParentCleanupAttempt(marker ParentCleanupAttempt, limits ParentCleanupExecutionLimits) ([]byte, string, error) {
	operation, operationErr := ParseParentCleanupOperationID(marker.OperationID.String())
	if marker.Schema != ParentCleanupAttemptSchemaV1 || operationErr != nil || operation != marker.OperationID || marker.Sequence < 0 ||
		!canonicalSHA256ID(marker.IntentID) || !canonicalSHA256ID(marker.ParentPathRef) || !canonicalFSIdentity(marker.ParentIdentity) {
		return nil, "", fmt.Errorf("%w: parent-cleanup attempt is invalid", ErrExecutionIntegrity)
	}
	return encodeExecutionMarker(parentCleanupAttemptDomain, marker, limits.MaxMarkerBytes)
}

func decodeParentCleanupAttempt(reader io.Reader, limits ParentCleanupExecutionLimits) (ParentCleanupAttempt, string, error) {
	var marker ParentCleanupAttempt
	id, err := decodeExecutionMarker(reader, limits.MaxMarkerBytes, parentCleanupAttemptDomain, &marker, func() error {
		_, _, validateErr := encodeParentCleanupAttempt(marker, limits)
		return validateErr
	})
	return marker, id, err
}

func encodeParentCleanupRemoved(marker ParentCleanupRemoved, limits ParentCleanupExecutionLimits) ([]byte, string, error) {
	operation, operationErr := ParseParentCleanupOperationID(marker.OperationID.String())
	if marker.Schema != ParentCleanupRemovedSchemaV1 || operationErr != nil || operation != marker.OperationID || marker.Sequence < 0 ||
		!canonicalSHA256ID(marker.IntentID) || !canonicalSHA256ID(marker.AttemptID) || !canonicalSHA256ID(marker.ParentPathRef) ||
		!canonicalFSIdentity(marker.ParentIdentity) ||
		(marker.Basis != ParentCleanupRemovalBasisConfirmed && marker.Basis != ParentCleanupRemovalBasisRecovered) {
		return nil, "", fmt.Errorf("%w: parent-cleanup removal is invalid", ErrExecutionIntegrity)
	}
	return encodeExecutionMarker(parentCleanupRemovedDomain, marker, limits.MaxMarkerBytes)
}

func decodeParentCleanupRemoved(reader io.Reader, limits ParentCleanupExecutionLimits) (ParentCleanupRemoved, string, error) {
	var marker ParentCleanupRemoved
	id, err := decodeExecutionMarker(reader, limits.MaxMarkerBytes, parentCleanupRemovedDomain, &marker, func() error {
		_, _, validateErr := encodeParentCleanupRemoved(marker, limits)
		return validateErr
	})
	return marker, id, err
}

func encodeParentCleanupComplete(marker ParentCleanupComplete, limits ParentCleanupExecutionLimits) ([]byte, string, error) {
	operation, operationErr := ParseParentCleanupOperationID(marker.OperationID.String())
	if marker.Schema != ParentCleanupCompleteSchemaV1 || operationErr != nil || operation != marker.OperationID ||
		!canonicalSHA256ID(marker.IntentID) || marker.ParentsRemoved <= 0 || marker.ParentsRemoved > limits.MaxParents {
		return nil, "", fmt.Errorf("%w: parent-cleanup completion is invalid", ErrExecutionIntegrity)
	}
	return encodeExecutionMarker(parentCleanupCompleteDomain, marker, limits.MaxMarkerBytes)
}

func decodeParentCleanupComplete(reader io.Reader, limits ParentCleanupExecutionLimits) (ParentCleanupComplete, string, error) {
	var marker ParentCleanupComplete
	id, err := decodeExecutionMarker(reader, limits.MaxMarkerBytes, parentCleanupCompleteDomain, &marker, func() error {
		_, _, validateErr := encodeParentCleanupComplete(marker, limits)
		return validateErr
	})
	return marker, id, err
}

func preflightParentCleanupJournalEncoding(intent ParentCleanupIntent, placeholderOperationRoot string) error {
	intent.OperationRootIdentity = placeholderOperationRoot
	_, intentID, err := encodeParentCleanupIntent(intent)
	if err != nil {
		return err
	}
	for sequence, directory := range intent.Directories {
		attempt := ParentCleanupAttempt{Schema: ParentCleanupAttemptSchemaV1, OperationID: intent.OperationID,
			IntentID: intentID, Sequence: sequence, ParentPathRef: directory.ParentPathRef, ParentIdentity: directory.ParentIdentity}
		_, attemptID, attemptErr := encodeParentCleanupAttempt(attempt, intent.Limits)
		if attemptErr != nil {
			return attemptErr
		}
		for _, basis := range []string{ParentCleanupRemovalBasisConfirmed, ParentCleanupRemovalBasisRecovered} {
			removed := ParentCleanupRemoved{Schema: ParentCleanupRemovedSchemaV1, OperationID: intent.OperationID,
				IntentID: intentID, AttemptID: attemptID, Sequence: sequence, ParentPathRef: directory.ParentPathRef,
				ParentIdentity: directory.ParentIdentity, Basis: basis}
			if _, _, err := encodeParentCleanupRemoved(removed, intent.Limits); err != nil {
				return err
			}
		}
	}
	_, _, err = encodeParentCleanupComplete(ParentCleanupComplete{Schema: ParentCleanupCompleteSchemaV1,
		OperationID: intent.OperationID, IntentID: intentID, ParentsRemoved: len(intent.Directories)}, intent.Limits)
	return err
}

func strictUnmarshalExecutionJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}
