package sourceretire

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	intentMarkerDomain   = "ptctl-source-retirement-intent-v1\x00"
	attemptMarkerDomain  = "ptctl-source-retirement-delete-attempt-v1\x00"
	deleteMarkerDomain   = "ptctl-source-retirement-delete-complete-v1\x00"
	completeMarkerDomain = "ptctl-source-retirement-complete-v1\x00"
)

func encodeExecutionIntent(intent ExecutionIntent) ([]byte, string, error) {
	if err := intent.Validate(); err != nil {
		return nil, "", err
	}
	return encodeExecutionMarker(intentMarkerDomain, intent, intent.Limits.MaxIntentBytes)
}

func decodeExecutionIntent(reader io.Reader, limits ExecutionLimits) (ExecutionIntent, string, error) {
	var intent ExecutionIntent
	raw, err := readExecutionMarker(reader, limits.MaxIntentBytes)
	if err != nil {
		return intent, "", err
	}
	if err := json.Unmarshal(raw, &intent); err != nil || intent.Limits != limits {
		return ExecutionIntent{}, "", fmt.Errorf("%w: source retirement intent is invalid", ErrExecutionIntegrity)
	}
	canonical, id, err := encodeExecutionIntent(intent)
	if err != nil || !bytes.Equal(raw, canonical) {
		return ExecutionIntent{}, "", fmt.Errorf("%w: source retirement intent is not canonical", ErrExecutionIntegrity)
	}
	return intent, id, nil
}

func encodeDeleteAttempt(marker DeleteAttempt, limits ExecutionLimits) ([]byte, string, error) {
	if marker.Schema != DeleteAttemptSchemaV1 || marker.Sequence < 0 || marker.ManifestIndex < 0 || marker.SizeBytes <= 0 ||
		!canonicalSHA256ID(marker.IntentID) || !canonicalSHA256ID(marker.SourcePathRef) || !canonicalFSIdentity(marker.FileIdentity) {
		return nil, "", fmt.Errorf("%w: source deletion attempt is invalid", ErrExecutionIntegrity)
	}
	if parsed, err := ParseOperationID(marker.OperationID.String()); err != nil || parsed != marker.OperationID {
		return nil, "", fmt.Errorf("%w: source deletion attempt is invalid", ErrExecutionIntegrity)
	}
	return encodeExecutionMarker(attemptMarkerDomain, marker, limits.MaxMarkerBytes)
}

func decodeDeleteAttempt(reader io.Reader, limits ExecutionLimits) (DeleteAttempt, string, error) {
	var marker DeleteAttempt
	id, err := decodeExecutionMarker(reader, limits.MaxMarkerBytes, attemptMarkerDomain, &marker, func() error {
		_, _, err := encodeDeleteAttempt(marker, limits)
		return err
	})
	return marker, id, err
}

func encodeDeleteComplete(marker DeleteComplete, limits ExecutionLimits) ([]byte, string, error) {
	if marker.Schema != DeleteCompleteSchemaV1 || marker.Sequence < 0 || marker.ManifestIndex < 0 || marker.SizeBytes <= 0 ||
		!canonicalSHA256ID(marker.IntentID) || !canonicalSHA256ID(marker.AttemptID) || !canonicalSHA256ID(marker.SourcePathRef) ||
		!canonicalFSIdentity(marker.FileIdentity) || marker.Basis != DeleteBasisConfirmed && marker.Basis != DeleteBasisRecovered {
		return nil, "", fmt.Errorf("%w: source deletion completion is invalid", ErrExecutionIntegrity)
	}
	if parsed, err := ParseOperationID(marker.OperationID.String()); err != nil || parsed != marker.OperationID {
		return nil, "", fmt.Errorf("%w: source deletion completion is invalid", ErrExecutionIntegrity)
	}
	return encodeExecutionMarker(deleteMarkerDomain, marker, limits.MaxMarkerBytes)
}

func decodeDeleteComplete(reader io.Reader, limits ExecutionLimits) (DeleteComplete, string, error) {
	var marker DeleteComplete
	id, err := decodeExecutionMarker(reader, limits.MaxMarkerBytes, deleteMarkerDomain, &marker, func() error {
		_, _, err := encodeDeleteComplete(marker, limits)
		return err
	})
	return marker, id, err
}

func encodeExecutionComplete(marker ExecutionComplete, limits ExecutionLimits) ([]byte, string, error) {
	if marker.Schema != ExecutionCompleteSchemaV1 || marker.FilesRetired <= 0 || marker.BytesRetired <= 0 ||
		!canonicalSHA256ID(marker.IntentID) || !canonicalFSIdentity(marker.FinalObjectIdentity) ||
		!canonicalSHA256ID(marker.CurrentClientUseID) || !canonicalSHA256ID(marker.ClientSnapshotID) {
		return nil, "", fmt.Errorf("%w: source retirement completion is invalid", ErrExecutionIntegrity)
	}
	if parsed, err := ParseOperationID(marker.OperationID.String()); err != nil || parsed != marker.OperationID {
		return nil, "", fmt.Errorf("%w: source retirement completion is invalid", ErrExecutionIntegrity)
	}
	return encodeExecutionMarker(completeMarkerDomain, marker, limits.MaxMarkerBytes)
}

func decodeExecutionComplete(reader io.Reader, limits ExecutionLimits) (ExecutionComplete, string, error) {
	var marker ExecutionComplete
	id, err := decodeExecutionMarker(reader, limits.MaxMarkerBytes, completeMarkerDomain, &marker, func() error {
		_, _, err := encodeExecutionComplete(marker, limits)
		return err
	})
	return marker, id, err
}

// preflightExecutionJournalEncoding proves that every fixed protocol object
// fits before the operation subtree is created. The real operation-root
// identity changes the digest but not the canonical encoded length.
func preflightExecutionJournalEncoding(intent ExecutionIntent, placeholderOperationRoot string) error {
	intent.OperationRootIdentity = placeholderOperationRoot
	_, intentID, err := encodeExecutionIntent(intent)
	if err != nil {
		return err
	}
	attemptIDs := make([]string, len(intent.Files))
	for sequence, file := range intent.Files {
		attempt := DeleteAttempt{Schema: DeleteAttemptSchemaV1, OperationID: intent.OperationID, IntentID: intentID,
			Sequence: sequence, ManifestIndex: file.ManifestIndex, SourcePathRef: file.SourcePathRef,
			FileIdentity: file.SourceObjectIdentity, SizeBytes: file.SizeBytes}
		if _, attemptIDs[sequence], err = encodeDeleteAttempt(attempt, intent.Limits); err != nil {
			return err
		}
		for _, basis := range []string{DeleteBasisConfirmed, DeleteBasisRecovered} {
			complete := DeleteComplete{Schema: DeleteCompleteSchemaV1, OperationID: intent.OperationID, IntentID: intentID,
				AttemptID: attemptIDs[sequence], Sequence: sequence, ManifestIndex: file.ManifestIndex,
				SourcePathRef: file.SourcePathRef, FileIdentity: file.SourceObjectIdentity, SizeBytes: file.SizeBytes,
				Basis: basis}
			if _, _, err := encodeDeleteComplete(complete, intent.Limits); err != nil {
				return err
			}
		}
	}
	complete := ExecutionComplete{Schema: ExecutionCompleteSchemaV1, OperationID: intent.OperationID, IntentID: intentID,
		FilesRetired: len(intent.Files), BytesRetired: intent.ContentBytes, FinalObjectIdentity: intent.FinalObjectIdentity,
		CurrentClientUseID: intent.CurrentClientUseID, ClientSnapshotID: intent.CurrentClientUseID}
	_, _, err = encodeExecutionComplete(complete, intent.Limits)
	return err
}

func encodeExecutionMarker(domain string, value any, limit int64) ([]byte, string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, "", fmt.Errorf("encode source retirement marker failed")
	}
	raw = append(raw, '\n')
	if int64(len(raw)) > limit {
		return nil, "", fmt.Errorf("%w: source retirement marker byte limit exceeded", ErrExecutionPolicy)
	}
	digest := sha256.Sum256(append([]byte(domain), raw...))
	return raw, markerIDPrefix + hex.EncodeToString(digest[:]), nil
}

func readExecutionMarker(reader io.Reader, limit int64) ([]byte, error) {
	if reader == nil || limit <= 0 {
		return nil, fmt.Errorf("source retirement marker reader is unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%w: source retirement marker byte limit exceeded", ErrExecutionIntegrity)
	}
	if !utf8.Valid(raw) || !executionJSONDepthBounded(raw, 8) {
		return nil, fmt.Errorf("%w: source retirement marker JSON shape is unsafe", ErrExecutionIntegrity)
	}
	return raw, nil
}

func executionJSONDepthBounded(raw []byte, maximum int) bool {
	depth := 0
	inString, escaped := false, false
	for _, value := range raw {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if value == '\\' {
				escaped = true
			} else if value == '"' {
				inString = false
			}
			continue
		}
		switch value {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maximum {
				return false
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return !inString && !escaped && depth == 0
}

func decodeExecutionMarker(reader io.Reader, limit int64, domain string, target any, validate func() error) (string, error) {
	raw, err := readExecutionMarker(reader, limit)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(raw, target); err != nil || validate() != nil {
		return "", fmt.Errorf("%w: source retirement marker is invalid", ErrExecutionIntegrity)
	}
	canonical, id, err := encodeExecutionMarker(domain, target, limit)
	if err != nil || !bytes.Equal(raw, canonical) {
		return "", fmt.Errorf("%w: source retirement marker is not canonical", ErrExecutionIntegrity)
	}
	return id, nil
}
