package storageindex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const maxSmallRecordBytes = int64(256 << 10)

func EncodeProfile(profile Profile) ([]byte, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	return encodeCanonicalLine(profile, maxSmallRecordBytes)
}

func DecodeProfile(reader io.Reader) (Profile, error) {
	var profile Profile
	raw, err := readSmallRecord(reader)
	if err != nil {
		return profile, err
	}
	if err := strictUnmarshal(raw, &profile); err != nil {
		return Profile{}, fmt.Errorf("storage profile record is invalid")
	}
	if err := profile.Validate(); err != nil {
		return Profile{}, err
	}
	canonical, err := EncodeProfile(profile)
	if err != nil || !bytes.Equal(raw, canonical) {
		return Profile{}, fmt.Errorf("storage profile record is not canonical")
	}
	return profile, nil
}

func EncodeDescriptor(descriptor SnapshotDescriptor, limits Limits) ([]byte, error) {
	if err := descriptor.Validate(limits); err != nil {
		return nil, err
	}
	return encodeCanonicalLine(descriptor, maxSmallRecordBytes)
}

func DecodeDescriptor(reader io.Reader, limits Limits) (SnapshotDescriptor, error) {
	var descriptor SnapshotDescriptor
	raw, err := readSmallRecord(reader)
	if err != nil {
		return descriptor, err
	}
	if err := strictUnmarshal(raw, &descriptor); err != nil {
		return SnapshotDescriptor{}, fmt.Errorf("storage index descriptor is invalid")
	}
	if err := descriptor.Validate(limits); err != nil {
		return SnapshotDescriptor{}, err
	}
	canonical, err := EncodeDescriptor(descriptor, limits)
	if err != nil || !bytes.Equal(raw, canonical) {
		return SnapshotDescriptor{}, fmt.Errorf("storage index descriptor is not canonical")
	}
	return descriptor, nil
}

type SnapshotEncoder struct {
	writer      io.Writer
	header      SnapshotHeader
	limits      Limits
	bytes       int64
	files       int
	pathBytes   int64
	previousKey string
	closed      bool
}

func NewSnapshotEncoder(writer io.Writer, header SnapshotHeader, limits Limits) (*SnapshotEncoder, error) {
	if writer == nil {
		return nil, fmt.Errorf("storage index writer is unavailable")
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if err := header.Validate(); err != nil {
		return nil, err
	}
	encoder := &SnapshotEncoder{writer: writer, header: header, limits: limits}
	if err := encoder.write(header); err != nil {
		return nil, err
	}
	return encoder, nil
}

func (encoder *SnapshotEncoder) WriteEntry(entry Entry) error {
	if encoder == nil || encoder.closed {
		return fmt.Errorf("storage index encoder is closed")
	}
	pathBytes, key, err := validateEntry(entry, encoder.header, encoder.limits)
	if err != nil {
		return err
	}
	if encoder.files >= encoder.limits.MaxFiles {
		return fmt.Errorf("storage index file budget exceeded")
	}
	if pathBytes > encoder.limits.MaxPathBytes-encoder.pathBytes {
		return fmt.Errorf("storage index path budget exceeded")
	}
	if key <= encoder.previousKey {
		return fmt.Errorf("storage index entries are duplicated or unsorted")
	}
	if err := encoder.write(entry); err != nil {
		return err
	}
	encoder.previousKey = key
	encoder.files++
	encoder.pathBytes += pathBytes
	return nil
}

func (encoder *SnapshotEncoder) Close(observedAtEnd time.Time) (SnapshotFooter, error) {
	if encoder == nil || encoder.closed {
		return SnapshotFooter{}, fmt.Errorf("storage index encoder is closed")
	}
	encoder.closed = true
	end := canonicalTime(observedAtEnd)
	if !isCanonicalTime(end) || end.Before(encoder.header.ObservedAtStart) {
		return SnapshotFooter{}, fmt.Errorf("storage index observation interval is invalid")
	}
	footer := SnapshotFooter{
		Type: "footer", SnapshotID: encoder.header.SnapshotID, ObservedAtEnd: end,
		Complete: true, Files: encoder.files, PathBytes: encoder.pathBytes,
	}
	if err := encoder.write(footer); err != nil {
		return SnapshotFooter{}, err
	}
	return footer, nil
}

func (encoder *SnapshotEncoder) write(value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode storage index record failed")
	}
	raw = append(raw, '\n')
	if int64(len(raw)) > encoder.limits.MaxSnapshotBytes-encoder.bytes {
		return fmt.Errorf("storage index byte budget exceeded")
	}
	written, err := encoder.writer.Write(raw)
	encoder.bytes += int64(written)
	if err != nil || written != len(raw) {
		return fmt.Errorf("write storage index record failed")
	}
	return nil
}

type EntryVisitor func(Entry) error

func DecodeSnapshot(ctx context.Context, reader io.Reader, limits Limits, visit EntryVisitor) (SnapshotHeader, SnapshotFooter, error) {
	var header SnapshotHeader
	var footer SnapshotFooter
	if reader == nil || visit == nil {
		return header, footer, fmt.Errorf("storage index reader or visitor is unavailable")
	}
	if err := limits.Validate(); err != nil {
		return header, footer, err
	}
	if err := ctx.Err(); err != nil {
		return header, footer, err
	}
	bounded := &boundedLineReader{
		reader:  bufio.NewReaderSize(reader, 32<<10),
		maxLine: maxSnapshotLineBytes(limits), maxTotal: limits.MaxSnapshotBytes,
	}
	line, err := bounded.next()
	if err != nil {
		return header, footer, fmt.Errorf("storage index header is unavailable")
	}
	if err := strictUnmarshal(line, &header); err != nil || header.Validate() != nil || !canonicalJSONRow(line, header) {
		return SnapshotHeader{}, SnapshotFooter{}, fmt.Errorf("storage index header is invalid")
	}
	files := 0
	var pathBytes int64
	previousKey := ""
	for {
		if err := ctx.Err(); err != nil {
			return header, footer, err
		}
		line, err = bounded.next()
		if err != nil {
			return header, footer, fmt.Errorf("storage index footer is unavailable")
		}
		rowType, typeErr := jsonObjectType(line)
		if typeErr != nil {
			return header, footer, fmt.Errorf("storage index row is invalid")
		}
		switch rowType {
		case "file":
			if files >= limits.MaxFiles {
				return header, footer, fmt.Errorf("storage index file budget exceeded")
			}
			var entry Entry
			if err := strictUnmarshal(line, &entry); err != nil || !canonicalJSONRow(line, entry) {
				return header, footer, fmt.Errorf("storage index file row is invalid")
			}
			rowBytes, key, err := validateEntry(entry, header, limits)
			if err != nil || key <= previousKey || rowBytes > limits.MaxPathBytes-pathBytes {
				return header, footer, fmt.Errorf("storage index file row is invalid, duplicated, or unsorted")
			}
			if err := visit(entry); err != nil {
				return header, footer, err
			}
			files++
			pathBytes += rowBytes
			previousKey = key
		case "footer":
			if err := strictUnmarshal(line, &footer); err != nil || !canonicalJSONRow(line, footer) || footer.Type != "footer" || footer.SnapshotID != header.SnapshotID ||
				!footer.Complete || !isCanonicalTime(footer.ObservedAtEnd) || footer.ObservedAtEnd.Before(header.ObservedAtStart) ||
				footer.Files != files || footer.PathBytes != pathBytes {
				return header, SnapshotFooter{}, fmt.Errorf("storage index footer is invalid")
			}
			if trailing, trailingErr := bounded.next(); trailingErr == nil || len(trailing) != 0 || !errors.Is(trailingErr, io.EOF) {
				return header, footer, fmt.Errorf("storage index has trailing data")
			}
			return header, footer, nil
		default:
			return header, footer, fmt.Errorf("storage index row type is invalid")
		}
	}
}

func canonicalJSONRow(raw []byte, value any) bool {
	canonical, err := json.Marshal(value)
	return err == nil && bytes.Equal(raw, canonical)
}

type boundedLineReader struct {
	reader   *bufio.Reader
	maxLine  int
	maxTotal int64
	total    int64
}

func (reader *boundedLineReader) next() ([]byte, error) {
	if reader == nil || reader.reader == nil {
		return nil, io.EOF
	}
	var output []byte
	for {
		fragment, err := reader.reader.ReadSlice('\n')
		if len(fragment) > 0 {
			if int64(len(fragment)) > reader.maxTotal-reader.total || len(fragment) > reader.maxLine-len(output) {
				return nil, fmt.Errorf("storage index line or byte budget exceeded")
			}
			output = append(output, fragment...)
			reader.total += int64(len(fragment))
		}
		if err == nil {
			if len(output) == 0 || output[len(output)-1] != '\n' {
				return nil, fmt.Errorf("storage index line is incomplete")
			}
			return output[:len(output)-1], nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(output) == 0 {
			return nil, io.EOF
		}
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("storage index record is missing its final newline")
		}
		return nil, fmt.Errorf("read storage index record failed")
	}
}

func maxSnapshotLineBytes(limits Limits) int {
	value := 2_048 + limits.MaxPathComponents*((limits.MaxPathComponentBytes+2)/3*4+4)
	if int64(value) > limits.MaxSnapshotBytes {
		return int(limits.MaxSnapshotBytes)
	}
	return value
}

func readSmallRecord(reader io.Reader) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("private record reader is unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxSmallRecordBytes+1))
	if err != nil || int64(len(raw)) > maxSmallRecordBytes {
		return nil, fmt.Errorf("private record exceeds its byte limit")
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' || bytes.Count(raw, []byte{'\n'}) != 1 {
		return nil, fmt.Errorf("private record framing is invalid")
	}
	return raw, nil
}

func encodeCanonicalLine(value any, maximum int64) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode private record failed")
	}
	raw = append(raw, '\n')
	if int64(len(raw)) > maximum {
		return nil, fmt.Errorf("private record exceeds its byte limit")
	}
	return raw, nil
}

func strictUnmarshal(raw []byte, destination any) error {
	if len(raw) == 0 || !json.Valid(raw) {
		return fmt.Errorf("invalid JSON")
	}
	duplicateReader := json.NewDecoder(bytes.NewReader(raw))
	if err := rejectDuplicateObjectKeys(duplicateReader); err != nil {
		return err
	}
	var trailing any
	if err := duplicateReader.Decode(&trailing); !errors.Is(err, io.EOF) {
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

func jsonObjectType(raw []byte) (string, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return "", fmt.Errorf("invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := rejectDuplicateObjectKeys(decoder); err != nil {
		return "", err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("JSON has trailing data")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope) == 0 {
		return "", fmt.Errorf("JSON row is invalid")
	}
	value, ok := envelope["type"]
	if !ok {
		return "", fmt.Errorf("JSON row type is missing")
	}
	var rowType string
	if err := json.Unmarshal(value, &rowType); err != nil || rowType == "" {
		return "", fmt.Errorf("JSON row type is invalid")
	}
	return rowType, nil
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
		seen := map[string]struct{}{}
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
