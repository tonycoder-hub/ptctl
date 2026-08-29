package clientactivate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

const (
	RetentionIntentSchemaV1   = "ptctl.client-activation-retention-intent/v1"
	RetentionCompleteSchemaV1 = "ptctl.client-activation-retention-complete/v1"
	RetentionBasisTerminal    = "terminal_activation_journal_exact"

	retentionDirectoryName   = "retention"
	retentionIntentName      = "intent.json"
	retentionCompleteName    = "complete.json"
	retentionIntentPending   = "intent.pending"
	retentionCompletePending = "complete.pending"

	retentionIntentDomain   = "ptctl-client-activation-retention-intent-v1\x00"
	retentionCompleteDomain = "ptctl-client-activation-retention-complete-v1\x00"
	maximumRetentionBytes   = maximumMarkerBytes
)

type RetentionMarkerID string

func ParseRetentionMarkerID(value string) (RetentionMarkerID, error) {
	if !canonicalSHA256ID(value) {
		return "", fmt.Errorf("invalid client activation retention marker ID")
	}
	return RetentionMarkerID(value), nil
}

func (id RetentionMarkerID) String() string { return string(id) }

func encodeRetentionCanonical(value any) ([]byte, error) {
	raw, err := encodeCanonical(value)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maximumRetentionBytes {
		return nil, fmt.Errorf("client activation retention marker exceeds its byte limit")
	}
	return raw, nil
}

func readRetentionCanonical(reader io.Reader, destination any) ([]byte, error) {
	// The existing strict decoder provides duplicate-key, unknown-field,
	// framing, depth, and trailing-data rejection. Retention records are built
	// from fixed-size canonical activation fields and remain below the ordinary
	// marker hard limit in v1.
	return readCanonical(reader, destination)
}

func EncodeRetentionIntent(marker RetentionIntent) ([]byte, RetentionMarkerID, error) {
	if err := marker.Validate(); err != nil {
		return nil, "", err
	}
	raw, err := encodeRetentionCanonical(marker)
	return retentionIdentity(retentionIntentDomain, raw, err)
}

func DecodeRetentionIntent(reader io.Reader) (RetentionIntent, RetentionMarkerID, error) {
	var marker RetentionIntent
	raw, err := readRetentionCanonical(reader, &marker)
	if err != nil {
		return RetentionIntent{}, "", err
	}
	canonical, id, err := EncodeRetentionIntent(marker)
	if err != nil || !bytes.Equal(raw, canonical) {
		return RetentionIntent{}, "", fmt.Errorf("%w: client activation retention intent is not canonical", ErrIntegrity)
	}
	return marker, id, nil
}

func EncodeRetentionComplete(marker RetentionComplete) ([]byte, RetentionMarkerID, error) {
	if err := marker.Validate(); err != nil {
		return nil, "", err
	}
	raw, err := encodeRetentionCanonical(marker)
	return retentionIdentity(retentionCompleteDomain, raw, err)
}

func DecodeRetentionComplete(reader io.Reader) (RetentionComplete, RetentionMarkerID, error) {
	var marker RetentionComplete
	raw, err := readRetentionCanonical(reader, &marker)
	if err != nil {
		return RetentionComplete{}, "", err
	}
	canonical, id, err := EncodeRetentionComplete(marker)
	if err != nil || !bytes.Equal(raw, canonical) {
		return RetentionComplete{}, "", fmt.Errorf("%w: client activation retention completion is not canonical", ErrIntegrity)
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
