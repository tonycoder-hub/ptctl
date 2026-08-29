package qbittorrent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

const (
	hardMaxJobFileRowBytes   = 1 << 20
	hardMaxJobFilePathBytes  = 64 << 10
	hardMaxJobFilePathDepth  = 128
	hardMaxJobFileFields     = 64
	hardMaxJobFileJSONDepth  = 8
	hardMaxJSONFieldKeyBytes = 256
	hardMaxTorrentJSONDepth  = 8
)

const (
	jobFileHasIndex uint8 = 1 << iota
	jobFileHasName
	jobFileHasSize
	jobFileHasProgress
	jobFileHasPriority
	jobFileHasIsSeed
	jobFileRequiredFields = jobFileHasIndex | jobFileHasName | jobFileHasSize | jobFileHasProgress | jobFileHasPriority | jobFileHasIsSeed
)

type rawJobFile struct {
	ctx      context.Context
	present  uint8
	Index    int
	Name     string
	Size     int64
	Progress float64
	Priority int
	IsSeed   bool
}

func (item *rawJobFile) UnmarshalJSON(data []byte) error {
	if len(data) > hardMaxJobFileRowBytes {
		return fmt.Errorf("qBittorrent file object exceeds the row limit")
	}
	if err := validateStrictJSONStrings(data, hardMaxJobFileJSONDepth); err != nil {
		return fmt.Errorf("decode qBittorrent file object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return fmt.Errorf("decode qBittorrent file object")
	}
	seen := make(map[string]struct{})
	fields := 0
	for decoder.More() {
		if item.ctx != nil {
			if err := item.ctx.Err(); err != nil {
				return err
			}
		}
		fields++
		if fields > hardMaxJobFileFields {
			return fmt.Errorf("qBittorrent file object has too many fields")
		}
		token, err = decoder.Token()
		if err != nil {
			return fmt.Errorf("decode qBittorrent file object")
		}
		key, ok := token.(string)
		if !ok || !validDecodedJSONFieldKey(key) {
			return fmt.Errorf("decode qBittorrent file object")
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("qBittorrent file object contains a duplicate field")
		}
		seen[key] = struct{}{}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return fmt.Errorf("decode qBittorrent file object field")
		}
		switch key {
		case "index":
			if err := decodeJSONInt(raw, &item.Index); err != nil {
				return fmt.Errorf("decode qBittorrent file object field")
			}
			item.present |= jobFileHasIndex
		case "name":
			value, err := decodeStrictJSONStringValue(raw)
			if err != nil {
				return fmt.Errorf("decode qBittorrent file object field")
			}
			item.Name = value
			item.present |= jobFileHasName
		case "size":
			if err := decodeJSONInt64(raw, &item.Size); err != nil {
				return fmt.Errorf("decode qBittorrent file object field")
			}
			item.present |= jobFileHasSize
		case "progress":
			if err := decodeJSONFloat64(raw, &item.Progress); err != nil {
				return fmt.Errorf("decode qBittorrent file object field")
			}
			item.present |= jobFileHasProgress
		case "priority":
			if err := decodeJSONInt(raw, &item.Priority); err != nil {
				return fmt.Errorf("decode qBittorrent file object field")
			}
			item.present |= jobFileHasPriority
		case "is_seed":
			if err := decodeJSONBool(raw, &item.IsSeed); err != nil {
				return fmt.Errorf("decode qBittorrent file object field")
			}
			item.present |= jobFileHasIsSeed
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return fmt.Errorf("decode qBittorrent file object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("decode qBittorrent file object")
	}
	if item.present != jobFileRequiredFields {
		return fmt.Errorf("qBittorrent file object is missing required fields")
	}
	return nil
}

func (s *readSession) ReadJobFiles(ctx context.Context, opaqueJobKey string, limits downloader.JobFileLedgerLimits) (snapshot downloader.JobFileLedgerSnapshot, err error) {
	snapshot = downloader.JobFileLedgerSnapshot{
		Driver:          "qbittorrent",
		JobKey:          opaqueJobKey,
		ObservedAtStart: time.Now().UTC(),
		Limits:          limits,
		Files:           []downloader.JobFile{},
	}
	defer func() { snapshot.ObservedAtEnd = time.Now().UTC() }()
	if err := limits.Validate(); err != nil {
		return snapshot, err
	}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	if opaqueJobKey == "" || len(opaqueJobKey) > maxOpaqueJobKeyBytes || hasUnsafeJobKeyByte(opaqueJobKey) {
		return snapshot, fmt.Errorf("invalid qBittorrent opaque job key")
	}
	response, err := s.getResponse(ctx, "api/v2/torrents/files", url.Values{"hash": {opaqueJobKey}})
	if err != nil {
		return snapshot, err
	}
	limited := &io.LimitedReader{R: response.Body, N: limits.MaxResponseBytes + 1}
	counted := &countingReader{source: limited}
	files, decodeErr := decodeJobFileLedger(ctx, counted, limits, &snapshot.Used)
	snapshot.Used.ResponseBytes = counted.count
	closeErr := response.Body.Close()
	if counted.count > limits.MaxResponseBytes {
		return snapshot, fmt.Errorf("qBittorrent job-file response exceeded its byte limit")
	}
	if decodeErr != nil {
		return snapshot, decodeErr
	}
	if closeErr != nil {
		return snapshot, fmt.Errorf("close qBittorrent job-file response: %w", closeErr)
	}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	snapshot.Files = files
	snapshot.Complete = true
	return snapshot, nil
}

type countingReader struct {
	source io.Reader
	count  int64
}

func (reader *countingReader) Read(buffer []byte) (int, error) {
	n, err := reader.source.Read(buffer)
	reader.count += int64(n)
	return n, err
}

func decodeJobFileLedger(ctx context.Context, source io.Reader, limits downloader.JobFileLedgerLimits, used *downloader.JobFileLedgerUsage) ([]downloader.JobFile, error) {
	decoder := json.NewDecoder(source)
	token, err := decoder.Token()
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, fmt.Errorf("decode qBittorrent job-file list")
	}
	if token != json.Delim('[') {
		return nil, fmt.Errorf("decode qBittorrent job-file list")
	}
	initialCapacity := min(limits.MaxFiles, 1024)
	files := make([]downloader.JobFile, 0, initialCapacity)
	seenIndexes := make(map[int]struct{}, initialCapacity)
	seenPaths := make(map[string]struct{}, initialCapacity)
	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if used.FilesConsidered >= limits.MaxFiles {
			return nil, fmt.Errorf("qBittorrent returned too many job files")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}
			return nil, fmt.Errorf("decode qBittorrent job-file list")
		}
		used.FilesConsidered++
		if len(raw) > hardMaxJobFileRowBytes {
			return nil, fmt.Errorf("qBittorrent file object exceeds the row limit")
		}
		item := rawJobFile{ctx: ctx}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("decode qBittorrent job-file list: %w", err)
		}
		file, pathKey, pathBytes, err := normalizeJobFile(item)
		if err != nil {
			return nil, err
		}
		if pathBytes > limits.MaxPathBytes-used.PathBytes {
			return nil, fmt.Errorf("qBittorrent job-file paths exceeded their byte limit")
		}
		used.PathBytes += pathBytes
		if _, exists := seenIndexes[file.Index]; exists {
			return nil, fmt.Errorf("qBittorrent job-file list contains a duplicate index")
		}
		if _, exists := seenPaths[pathKey]; exists {
			return nil, fmt.Errorf("qBittorrent job-file list contains a duplicate effective relative path")
		}
		seenIndexes[file.Index] = struct{}{}
		seenPaths[pathKey] = struct{}{}
		files = append(files, file)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if token, err = decoder.Token(); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, fmt.Errorf("decode qBittorrent job-file list")
	}
	if token != json.Delim(']') {
		return nil, fmt.Errorf("decode qBittorrent job-file list")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, fmt.Errorf("decode qBittorrent job-file list")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Index < files[j].Index })
	return files, nil
}

func normalizeJobFile(item rawJobFile) (downloader.JobFile, string, int64, error) {
	if item.Index < 0 {
		return downloader.JobFile{}, "", 0, fmt.Errorf("qBittorrent file object has an invalid index")
	}
	if item.Size < 0 {
		return downloader.JobFile{}, "", 0, fmt.Errorf("qBittorrent file object has an invalid size")
	}
	if math.IsNaN(item.Progress) || math.IsInf(item.Progress, 0) || item.Progress < 0 || item.Progress > 1 {
		return downloader.JobFile{}, "", 0, fmt.Errorf("qBittorrent file object has invalid progress")
	}
	var selection downloader.JobFileSelection
	switch item.Priority {
	case 0:
		selection = downloader.JobFileSelectionSkipped
	case 1, 6, 7:
		selection = downloader.JobFileSelectionSelected
	default:
		return downloader.JobFile{}, "", 0, fmt.Errorf("qBittorrent file object has an invalid priority")
	}
	components, err := splitJobFileRelativePath(item.Name)
	if err != nil {
		return downloader.JobFile{}, "", 0, err
	}
	pathBytes := int64(0)
	for _, component := range components {
		pathBytes += int64(len(component))
	}
	return downloader.JobFile{
		Index:              item.Index,
		RelativeComponents: components,
		SizeBytes:          item.Size,
		Progress:           item.Progress,
		Selection:          selection,
		Complete:           item.IsSeed,
	}, strings.Join(components, "\x00"), pathBytes, nil
}

func splitJobFileRelativePath(value string) ([]string, error) {
	if value == "" || len(value) > hardMaxJobFilePathBytes || !utf8.ValidString(value) {
		return nil, fmt.Errorf("qBittorrent file object has an invalid relative path")
	}
	if strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || len(value) >= 3 && isASCIILetter(value[0]) && value[1] == ':' && value[2] == '/' {
		return nil, fmt.Errorf("qBittorrent file object has an invalid relative path")
	}
	components := strings.Split(value, "/")
	if len(components) > hardMaxJobFilePathDepth {
		return nil, fmt.Errorf("qBittorrent file object has an invalid relative path")
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return nil, fmt.Errorf("qBittorrent file object has an invalid relative path")
		}
		for _, r := range component {
			if r == 0 || r == 0x7f || r == utf8.RuneError || unicode.IsControl(r) {
				return nil, fmt.Errorf("qBittorrent file object has an invalid relative path")
			}
		}
	}
	return append([]string(nil), components...), nil
}

func validateStrictJSONStrings(data []byte, maxDepth int) error {
	if maxDepth <= 0 || !utf8.Valid(data) {
		return fmt.Errorf("invalid JSON text")
	}
	depth := 0
	for index := 0; index < len(data); index++ {
		switch data[index] {
		case '"':
			end, err := scanStrictJSONString(data, index)
			if err != nil {
				return err
			}
			index = end - 1
		case '{', '[':
			depth++
			if depth > maxDepth {
				return fmt.Errorf("JSON nesting exceeds its limit")
			}
		case '}', ']':
			depth--
		}
	}
	return nil
}

func scanStrictJSONString(data []byte, start int) (int, error) {
	if start >= len(data) || data[start] != '"' {
		return 0, fmt.Errorf("invalid JSON string")
	}
	for index := start + 1; index < len(data); index++ {
		value := data[index]
		switch {
		case value == '"':
			return index + 1, nil
		case value == '\\':
			if index+1 >= len(data) {
				return 0, fmt.Errorf("invalid JSON string escape")
			}
			escape := data[index+1]
			switch escape {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				index++
			case 'u':
				code, ok := jsonHexQuad(data, index+2)
				if !ok {
					return 0, fmt.Errorf("invalid JSON Unicode escape")
				}
				if code >= 0xd800 && code <= 0xdbff {
					if index+11 >= len(data) || data[index+6] != '\\' || data[index+7] != 'u' {
						return 0, fmt.Errorf("unpaired JSON Unicode surrogate")
					}
					low, ok := jsonHexQuad(data, index+8)
					if !ok || low < 0xdc00 || low > 0xdfff {
						return 0, fmt.Errorf("unpaired JSON Unicode surrogate")
					}
					index += 11
				} else if code >= 0xdc00 && code <= 0xdfff {
					return 0, fmt.Errorf("unpaired JSON Unicode surrogate")
				} else {
					index += 5
				}
			default:
				return 0, fmt.Errorf("invalid JSON string escape")
			}
		case value < 0x20:
			return 0, fmt.Errorf("invalid JSON string control character")
		case value >= utf8.RuneSelf:
			_, size := utf8.DecodeRune(data[index:])
			if size == 1 {
				return 0, fmt.Errorf("invalid JSON string encoding")
			}
			index += size - 1
		}
	}
	return 0, fmt.Errorf("unterminated JSON string")
}

func jsonHexQuad(data []byte, start int) (uint16, bool) {
	if start < 0 || start+4 > len(data) {
		return 0, false
	}
	var result uint16
	for index := start; index < start+4; index++ {
		value := data[index]
		result <<= 4
		switch {
		case value >= '0' && value <= '9':
			result |= uint16(value - '0')
		case value >= 'a' && value <= 'f':
			result |= uint16(value-'a') + 10
		case value >= 'A' && value <= 'F':
			result |= uint16(value-'A') + 10
		default:
			return 0, false
		}
	}
	return result, true
}

func decodeStrictJSONStringValue(raw []byte) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '"' {
		return "", fmt.Errorf("JSON value is not a string")
	}
	end, err := scanStrictJSONString(trimmed, 0)
	if err != nil || end != len(trimmed) {
		return "", fmt.Errorf("invalid JSON string")
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil || !utf8.ValidString(value) {
		return "", fmt.Errorf("invalid JSON string")
	}
	return value, nil
}

func validDecodedJSONFieldKey(value string) bool {
	return value != "" && len(value) <= hardMaxJSONFieldKeyBytes && validControlSafeUTF8(value)
}

func validControlSafeUTF8(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r == 0 || r == 0x7f || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func decodeJSONInt(raw []byte, target *int) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '-' && (trimmed[0] < '0' || trimmed[0] > '9') {
		return fmt.Errorf("JSON value is not an integer")
	}
	return json.Unmarshal(trimmed, target)
}

func decodeJSONInt64(raw []byte, target *int64) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '-' && (trimmed[0] < '0' || trimmed[0] > '9') {
		return fmt.Errorf("JSON value is not an integer")
	}
	return json.Unmarshal(trimmed, target)
}

func decodeJSONFloat64(raw []byte, target *float64) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '-' && (trimmed[0] < '0' || trimmed[0] > '9') {
		return fmt.Errorf("JSON value is not a number")
	}
	return json.Unmarshal(trimmed, target)
}

func decodeJSONBool(raw []byte, target *bool) error {
	trimmed := bytes.TrimSpace(raw)
	if !bytes.Equal(trimmed, []byte("true")) && !bytes.Equal(trimmed, []byte("false")) {
		return fmt.Errorf("JSON value is not a boolean")
	}
	return json.Unmarshal(trimmed, target)
}

func isASCIILetter(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}
