package clientactivate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func encodeCanonical(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if int64(len(raw)) > maximumMarkerBytes {
		return nil, fmt.Errorf("client activation marker exceeds its byte limit")
	}
	return raw, nil
}

func markerIdentity(domain string, raw []byte, err error) ([]byte, MarkerID, error) {
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(append([]byte(domain), raw...))
	return raw, MarkerID("sha256:" + hex.EncodeToString(digest[:])), nil
}

func encodeIntent(value Intent) ([]byte, MarkerID, error) {
	if err := value.Validate(); err != nil {
		return nil, "", err
	}
	raw, err := encodeCanonical(value)
	return markerIdentity("ptctl-client-activation-intent-v1\x00", raw, err)
}

func encodeAttempt(value Attempt) ([]byte, MarkerID, error) {
	if err := value.Validate(); err != nil {
		return nil, "", err
	}
	raw, err := encodeCanonical(value)
	return markerIdentity("ptctl-client-activation-attempt-v1\x00", raw, err)
}

func encodeRecheckStarted(value RecheckStarted) ([]byte, MarkerID, error) {
	if err := value.Validate(); err != nil {
		return nil, "", err
	}
	raw, err := encodeCanonical(value)
	return markerIdentity("ptctl-client-recheck-started-v1\x00", raw, err)
}

func encodeRecheckCompletion(value RecheckCompletion) ([]byte, MarkerID, error) {
	if err := value.Validate(); err != nil {
		return nil, "", err
	}
	raw, err := encodeCanonical(value)
	return markerIdentity("ptctl-client-recheck-completion-v1\x00", raw, err)
}

func encodeActivationCompletion(value ActivationCompletion) ([]byte, MarkerID, error) {
	if err := value.Validate(); err != nil {
		return nil, "", err
	}
	raw, err := encodeCanonical(value)
	return markerIdentity("ptctl-client-activation-completion-v1\x00", raw, err)
}

func decodeIntent(reader io.Reader) (Intent, MarkerID, error) {
	var value Intent
	raw, err := readCanonical(reader, &value)
	if err != nil {
		return Intent{}, "", err
	}
	canonical, id, err := encodeIntent(value)
	if err != nil || !bytes.Equal(raw, canonical) {
		return Intent{}, "", fmt.Errorf("%w: activation intent is not canonical", ErrIntegrity)
	}
	return value, id, nil
}

func decodeAttempt(reader io.Reader) (Attempt, MarkerID, error) {
	var value Attempt
	raw, err := readCanonical(reader, &value)
	if err != nil {
		return Attempt{}, "", err
	}
	canonical, id, err := encodeAttempt(value)
	if err != nil || !bytes.Equal(raw, canonical) {
		return Attempt{}, "", fmt.Errorf("%w: activation attempt is not canonical", ErrIntegrity)
	}
	return value, id, nil
}

func decodeRecheckStarted(reader io.Reader) (RecheckStarted, MarkerID, error) {
	var value RecheckStarted
	raw, err := readCanonical(reader, &value)
	if err != nil {
		return RecheckStarted{}, "", err
	}
	canonical, id, err := encodeRecheckStarted(value)
	if err != nil || !bytes.Equal(raw, canonical) {
		return RecheckStarted{}, "", fmt.Errorf("%w: recheck-started marker is not canonical", ErrIntegrity)
	}
	return value, id, nil
}

func decodeRecheckCompletion(reader io.Reader) (RecheckCompletion, MarkerID, error) {
	var value RecheckCompletion
	raw, err := readCanonical(reader, &value)
	if err != nil {
		return RecheckCompletion{}, "", err
	}
	canonical, id, err := encodeRecheckCompletion(value)
	if err != nil || !bytes.Equal(raw, canonical) {
		return RecheckCompletion{}, "", fmt.Errorf("%w: recheck completion marker is not canonical", ErrIntegrity)
	}
	return value, id, nil
}

func decodeActivationCompletion(reader io.Reader) (ActivationCompletion, MarkerID, error) {
	var value ActivationCompletion
	raw, err := readCanonical(reader, &value)
	if err != nil {
		return ActivationCompletion{}, "", err
	}
	canonical, id, err := encodeActivationCompletion(value)
	if err != nil || !bytes.Equal(raw, canonical) {
		return ActivationCompletion{}, "", fmt.Errorf("%w: activation completion marker is not canonical", ErrIntegrity)
	}
	return value, id, nil
}

func readCanonical(reader io.Reader, destination any) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("%w: activation marker reader is unavailable", ErrIntegrity)
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maximumMarkerBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maximumMarkerBytes || len(raw) == 0 || raw[len(raw)-1] != '\n' || bytes.Count(raw, []byte{'\n'}) != 1 || !json.Valid(raw) {
		return nil, fmt.Errorf("%w: activation marker framing is invalid", ErrIntegrity)
	}
	duplicateDecoder := json.NewDecoder(bytes.NewReader(raw))
	jsonBudget := 768
	if err := rejectDuplicateKeys(duplicateDecoder, 0, &jsonBudget); err != nil {
		return nil, fmt.Errorf("%w: activation marker JSON is invalid", ErrIntegrity)
	}
	var trailing any
	if err := duplicateDecoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: activation marker has trailing data", ErrIntegrity)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return nil, fmt.Errorf("%w: activation marker schema is invalid", ErrIntegrity)
	}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: activation marker has trailing data", ErrIntegrity)
	}
	return raw, nil
}

func rejectDuplicateKeys(decoder *json.Decoder, depth int, budget *int) error {
	if decoder == nil || budget == nil || depth > 32 || *budget <= 0 {
		return fmt.Errorf("JSON structure exceeds its complexity limit")
	}
	*budget--
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object key is invalid")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("JSON object key is duplicated")
			}
			seen[key] = struct{}{}
			if err := rejectDuplicateKeys(decoder, depth+1, budget); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := rejectDuplicateKeys(decoder, depth+1, budget); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("JSON delimiter is invalid")
	}
}
