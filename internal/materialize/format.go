package materialize

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	operationDigestDomain = "ptctl-materialize-operation-v1\x00"
	eventDigestDomain     = "ptctl-materialize-event-v1\x00"
)

func EncodeIntent(intent Intent) ([]byte, error) {
	if err := intent.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(intent)
	if err != nil {
		return nil, fmt.Errorf("%w: encode failed", ErrInvalidIntent)
	}
	raw = append(raw, '\n')
	if int64(len(raw)) > intent.Limits.MaxEventBytes {
		return nil, fmt.Errorf("%w: intent byte limit exceeded", ErrInvalidIntent)
	}
	return raw, nil
}

func DecodeIntent(reader io.Reader, limits Limits) (Intent, error) {
	if reader == nil {
		return Intent{}, fmt.Errorf("%w: reader is unavailable", ErrCorruptJournal)
	}
	if err := limits.Validate(); err != nil {
		return Intent{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, limits.MaxEventBytes+1))
	if err != nil {
		return Intent{}, err
	}
	if int64(len(raw)) > limits.MaxEventBytes {
		return Intent{}, fmt.Errorf("%w: intent byte limit exceeded", ErrCorruptJournal)
	}
	var intent Intent
	if err := decodeCanonicalJSON(raw, &intent); err != nil {
		return Intent{}, fmt.Errorf("%w: intent is not canonical", ErrCorruptJournal)
	}
	if intent.Limits != limits {
		return Intent{}, fmt.Errorf("%w: caller and journal limits disagree", ErrCorruptJournal)
	}
	canonical, err := EncodeIntent(intent)
	if err != nil || !bytes.Equal(raw, canonical) {
		return Intent{}, fmt.Errorf("%w: intent is not canonical", ErrCorruptJournal)
	}
	return intent, nil
}

func OperationIDFor(intent Intent) (OperationID, error) {
	raw, err := EncodeIntent(intent)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(operationDigestDomain), raw...))
	return OperationID("sha256:" + hex.EncodeToString(digest[:])), nil
}

func EncodeEvent(event Event, limits Limits) ([]byte, EventID, error) {
	if err := limits.Validate(); err != nil {
		return nil, "", err
	}
	if err := event.Validate(); err != nil {
		return nil, "", err
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return nil, "", fmt.Errorf("%w: encode event failed", ErrCorruptJournal)
	}
	raw = append(raw, '\n')
	if int64(len(raw)) > limits.MaxEventBytes {
		return nil, "", fmt.Errorf("%w: event byte limit exceeded", ErrCorruptJournal)
	}
	digest := sha256.Sum256(append([]byte(eventDigestDomain), raw...))
	return raw, EventID("sha256:" + hex.EncodeToString(digest[:])), nil
}

func DecodeEvent(reader io.Reader, limits Limits) (Event, EventID, error) {
	if reader == nil {
		return Event{}, "", fmt.Errorf("%w: reader is unavailable", ErrCorruptJournal)
	}
	if err := limits.Validate(); err != nil {
		return Event{}, "", err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, limits.MaxEventBytes+1))
	if err != nil {
		return Event{}, "", err
	}
	if int64(len(raw)) > limits.MaxEventBytes {
		return Event{}, "", fmt.Errorf("%w: event byte limit exceeded", ErrCorruptJournal)
	}
	var event Event
	if err := decodeCanonicalJSON(raw, &event); err != nil {
		return Event{}, "", fmt.Errorf("%w: event is not canonical", ErrCorruptJournal)
	}
	canonical, id, err := EncodeEvent(event, limits)
	if err != nil || !bytes.Equal(raw, canonical) {
		return Event{}, "", fmt.Errorf("%w: event is not canonical", ErrCorruptJournal)
	}
	return event, id, nil
}

func decodeCanonicalJSON(raw []byte, destination any) error {
	if len(raw) == 0 || raw[len(raw)-1] != '\n' || bytes.Count(raw, []byte{'\n'}) != 1 || !json.Valid(raw) {
		return fmt.Errorf("JSON framing is invalid")
	}
	duplicateDecoder := json.NewDecoder(bytes.NewReader(raw))
	if err := rejectDuplicateKeys(duplicateDecoder); err != nil {
		return err
	}
	var trailing any
	if err := duplicateDecoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("JSON has trailing data")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("JSON has trailing data")
	}
	return nil
}

func rejectDuplicateKeys(decoder *json.Decoder) error {
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
			if err := rejectDuplicateKeys(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := rejectDuplicateKeys(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("JSON delimiter is invalid")
	}
}
