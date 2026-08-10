package sitebinding

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func EncodeRecord(record Record, limits Limits) ([]byte, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if err := record.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("%w: encode failed", ErrInvalidBinding)
	}
	raw = append(raw, '\n')
	if int64(len(raw)) > limits.MaxRecordBytes {
		return nil, fmt.Errorf("%w: record byte limit exceeded", ErrInvalidBinding)
	}
	return raw, nil
}

func DecodeRecord(reader io.Reader, limits Limits) (Record, error) {
	if reader == nil {
		return Record{}, fmt.Errorf("%w: reader is unavailable", ErrCorruptBinding)
	}
	if err := limits.Validate(); err != nil {
		return Record{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, limits.MaxRecordBytes+1))
	if err != nil || int64(len(raw)) > limits.MaxRecordBytes {
		return Record{}, fmt.Errorf("%w: record byte limit exceeded", ErrCorruptBinding)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' || bytes.Count(raw, []byte{'\n'}) != 1 {
		return Record{}, fmt.Errorf("%w: record framing is invalid", ErrCorruptBinding)
	}
	if !json.Valid(raw) {
		return Record{}, fmt.Errorf("%w: JSON is invalid", ErrCorruptBinding)
	}
	duplicateDecoder := json.NewDecoder(bytes.NewReader(raw))
	if err := rejectDuplicateObjectKeys(duplicateDecoder); err != nil {
		return Record{}, fmt.Errorf("%w: JSON object is invalid", ErrCorruptBinding)
	}
	var trailing any
	if err := duplicateDecoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Record{}, fmt.Errorf("%w: JSON has trailing data", ErrCorruptBinding)
	}

	var record Record
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return Record{}, fmt.Errorf("%w: schema is invalid", ErrCorruptBinding)
	}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Record{}, fmt.Errorf("%w: JSON has trailing data", ErrCorruptBinding)
	}
	if err := record.Validate(); err != nil {
		return Record{}, fmt.Errorf("%w: value is invalid", ErrCorruptBinding)
	}
	canonical, err := EncodeRecord(record, limits)
	if err != nil || !bytes.Equal(raw, canonical) {
		return Record{}, fmt.Errorf("%w: record is not canonical", ErrCorruptBinding)
	}
	return record, nil
}

func rejectDuplicateObjectKeys(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
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
			if err := rejectDuplicateObjectKeys(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := rejectDuplicateObjectKeys(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("JSON delimiter is invalid")
	}
}
