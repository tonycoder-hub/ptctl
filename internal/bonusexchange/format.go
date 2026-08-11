package bonusexchange

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func EncodeIntent(record IntentRecord, limits Limits) ([]byte, error) {
	return encodeRecord(record, limits, record.Validate)
}

func EncodeAttempt(record AttemptRecord, limits Limits) ([]byte, error) {
	return encodeRecord(record, limits, record.Validate)
}

func EncodeOutcome(record OutcomeRecord, limits Limits) ([]byte, error) {
	return encodeRecord(record, limits, record.Validate)
}

func DecodeIntent(reader io.Reader, limits Limits) (IntentRecord, error) {
	var record IntentRecord
	if err := decodeRecord(reader, limits, &record, func() error { return record.Validate() }); err != nil {
		return IntentRecord{}, err
	}
	return record, nil
}

func DecodeAttempt(reader io.Reader, limits Limits) (AttemptRecord, error) {
	var record AttemptRecord
	if err := decodeRecord(reader, limits, &record, func() error { return record.Validate() }); err != nil {
		return AttemptRecord{}, err
	}
	return record, nil
}

func DecodeOutcome(reader io.Reader, limits Limits) (OutcomeRecord, error) {
	var record OutcomeRecord
	if err := decodeRecord(reader, limits, &record, func() error { return record.Validate() }); err != nil {
		return OutcomeRecord{}, err
	}
	return record, nil
}

func encodeRecord(value any, limits Limits, validate func() error) ([]byte, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if err := validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: encode failed", ErrInvalidExchange)
	}
	raw = append(raw, '\n')
	if int64(len(raw)) > limits.MaxRecordBytes {
		return nil, fmt.Errorf("%w: record byte limit exceeded", ErrInvalidExchange)
	}
	return raw, nil
}

func decodeRecord(reader io.Reader, limits Limits, destination any, validate func() error) error {
	if reader == nil {
		return fmt.Errorf("%w: reader is unavailable", ErrCorruptExchange)
	}
	if err := limits.Validate(); err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, limits.MaxRecordBytes+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > limits.MaxRecordBytes {
		return fmt.Errorf("%w: record byte limit exceeded", ErrCorruptExchange)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' || bytes.Count(raw, []byte{'\n'}) != 1 || !json.Valid(raw) {
		return fmt.Errorf("%w: record framing is invalid", ErrCorruptExchange)
	}
	duplicateDecoder := json.NewDecoder(bytes.NewReader(raw))
	if err := rejectDuplicateKeys(duplicateDecoder); err != nil {
		return fmt.Errorf("%w: JSON object is invalid", ErrCorruptExchange)
	}
	var trailing any
	if err := duplicateDecoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: JSON has trailing data", ErrCorruptExchange)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%w: schema is invalid", ErrCorruptExchange)
	}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: JSON has trailing data", ErrCorruptExchange)
	}
	if err := validate(); err != nil {
		return fmt.Errorf("%w: value is invalid", ErrCorruptExchange)
	}
	canonical, err := json.Marshal(destination)
	if err != nil {
		return fmt.Errorf("%w: canonical encoding failed", ErrCorruptExchange)
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(raw, canonical) {
		return fmt.Errorf("%w: record is not canonical", ErrCorruptExchange)
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
