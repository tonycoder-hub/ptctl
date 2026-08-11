package clientremove

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
		return nil, fmt.Errorf("client removal marker exceeds its byte limit")
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
	return markerIdentity("ptctl-client-removal-intent-v1\x00", raw, err)
}

func encodeAttempt(value Attempt) ([]byte, MarkerID, error) {
	if err := value.Validate(); err != nil {
		return nil, "", err
	}
	raw, err := encodeCanonical(value)
	return markerIdentity("ptctl-client-removal-attempt-v1\x00", raw, err)
}

func encodeResponse(value Response) ([]byte, MarkerID, error) {
	if err := value.Validate(); err != nil {
		return nil, "", err
	}
	raw, err := encodeCanonical(value)
	return markerIdentity("ptctl-client-removal-response-v1\x00", raw, err)
}

func encodeCompletion(value Completion) ([]byte, MarkerID, error) {
	if err := value.Validate(); err != nil {
		return nil, "", err
	}
	raw, err := encodeCanonical(value)
	return markerIdentity("ptctl-client-removal-completion-v1\x00", raw, err)
}

func decodeIntent(reader io.Reader) (Intent, MarkerID, error) {
	var value Intent
	raw, err := readCanonical(reader, &value)
	if err != nil {
		return Intent{}, "", err
	}
	canonical, id, err := encodeIntent(value)
	if err != nil || !bytes.Equal(raw, canonical) {
		return Intent{}, "", fmt.Errorf("%w: client removal intent is not canonical", ErrIntegrity)
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
		return Attempt{}, "", fmt.Errorf("%w: client removal attempt is not canonical", ErrIntegrity)
	}
	return value, id, nil
}

func decodeResponse(reader io.Reader) (Response, MarkerID, error) {
	var value Response
	raw, err := readCanonical(reader, &value)
	if err != nil {
		return Response{}, "", err
	}
	canonical, id, err := encodeResponse(value)
	if err != nil || !bytes.Equal(raw, canonical) {
		return Response{}, "", fmt.Errorf("%w: client removal response is not canonical", ErrIntegrity)
	}
	return value, id, nil
}

func decodeCompletion(reader io.Reader) (Completion, MarkerID, error) {
	var value Completion
	raw, err := readCanonical(reader, &value)
	if err != nil {
		return Completion{}, "", err
	}
	canonical, id, err := encodeCompletion(value)
	if err != nil || !bytes.Equal(raw, canonical) {
		return Completion{}, "", fmt.Errorf("%w: client removal completion is not canonical", ErrIntegrity)
	}
	return value, id, nil
}

func readCanonical(reader io.Reader, destination any) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("%w: client removal marker reader is unavailable", ErrIntegrity)
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maximumMarkerBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maximumMarkerBytes || len(raw) == 0 || raw[len(raw)-1] != '\n' || bytes.Count(raw, []byte{'\n'}) != 1 || !json.Valid(raw) {
		return nil, fmt.Errorf("%w: client removal marker framing is invalid", ErrIntegrity)
	}
	duplicateDecoder := json.NewDecoder(bytes.NewReader(raw))
	budget := 768
	if err := rejectDuplicateKeys(duplicateDecoder, 0, &budget); err != nil {
		return nil, fmt.Errorf("%w: client removal marker JSON is invalid", ErrIntegrity)
	}
	var trailing any
	if err := duplicateDecoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: client removal marker has trailing data", ErrIntegrity)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return nil, fmt.Errorf("%w: client removal marker schema is invalid", ErrIntegrity)
	}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: client removal marker has trailing data", ErrIntegrity)
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
				return fmt.Errorf("object key is invalid")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object key")
			}
			seen[key] = struct{}{}
			if err := rejectDuplicateKeys(decoder, depth+1, budget); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := rejectDuplicateKeys(decoder, depth+1, budget); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("JSON delimiter is invalid")
	}
	_, err = decoder.Token()
	return err
}
