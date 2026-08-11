package transmission

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

const (
	maxOpaqueKeyBytes = 40
	maxNameBytes      = 64 << 10
	maxPathBytes      = 64 << 10
	maxFilePathDepth  = 128
	maxJobFields      = 64
	maxFileFields     = 32
)

func (s *readSession) ReadLedger(ctx context.Context) (downloader.LedgerSnapshot, error) {
	started := time.Now().UTC()
	result, _, err := s.call(ctx, "torrent-get", "torrent_get", map[string]any{"fields": ledgerFields(s.protocol)}, maxLedgerBodyBytes)
	if err != nil {
		return downloader.LedgerSnapshot{}, err
	}
	jobs, err := decodeLedgerResult(ctx, result, s.protocol)
	if err != nil {
		return downloader.LedgerSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return downloader.LedgerSnapshot{}, err
	}
	return downloader.LedgerSnapshot{
		Driver:          downloader.DriverTransmission,
		ObservedAtStart: started,
		ObservedAtEnd:   time.Now().UTC(),
		Complete:        true,
		Capabilities: downloader.LedgerCapabilities{
			TypedInfoHashes: true,
			ContentPath:     true,
			RawMetafile:     false,
			JobFiles:        true,
		},
		Jobs: jobs,
	}, nil
}

func ledgerFields(value protocol) []string {
	if value == protocolJSONRPC {
		return []string{"hash_string", "name", "total_size", "percent_complete", "status", "download_dir", "downloaded_ever", "uploaded_ever"}
	}
	return []string{"hashString", "name", "totalSize", "percentComplete", "status", "downloadDir", "downloadedEver", "uploadedEver"}
}

func fileFields(value protocol) []string {
	if value == protocolJSONRPC {
		return []string{"hash_string", "name", "download_dir", "files", "file_stats"}
	}
	return []string{"hashString", "name", "downloadDir", "files", "fileStats"}
}

func decodeLedgerResult(ctx context.Context, result json.RawMessage, valueProtocol protocol) ([]downloader.Torrent, error) {
	fields, err := decodeObject(result, 16)
	if err != nil {
		return nil, fmt.Errorf("decode Transmission torrent result")
	}
	rawJobs, exists := fields["torrents"]
	if !exists {
		return nil, fmt.Errorf("Transmission torrent result is missing torrents")
	}
	jobs := make([]downloader.Torrent, 0, 256)
	seen := make(map[string]struct{})
	_, err = decodeRawArray(rawJobs, maxLedgerJobs, func(index int, raw json.RawMessage) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		job, err := decodeLedgerJob(raw, valueProtocol)
		if err != nil {
			return fmt.Errorf("decode Transmission torrent %d: %w", index, err)
		}
		if _, duplicate := seen[job.Hash]; duplicate {
			return fmt.Errorf("Transmission torrent list contains a duplicate opaque job key")
		}
		seen[job.Hash] = struct{}{}
		jobs = append(jobs, job)
		return nil
	})
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, err
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].Hash != jobs[j].Hash {
			return jobs[i].Hash < jobs[j].Hash
		}
		if jobs[i].ContentPath != jobs[j].ContentPath {
			return jobs[i].ContentPath < jobs[j].ContentPath
		}
		return jobs[i].Name < jobs[j].Name
	})
	return jobs, nil
}

func decodeLedgerJob(raw json.RawMessage, valueProtocol protocol) (downloader.Torrent, error) {
	fields, err := decodeObject(raw, maxJobFields)
	if err != nil {
		return downloader.Torrent{}, err
	}
	hashKey, sizeKey, progressKey, directoryKey, downloadedKey, uploadedKey := "hashString", "totalSize", "percentComplete", "downloadDir", "downloadedEver", "uploadedEver"
	if valueProtocol == protocolJSONRPC {
		hashKey, sizeKey, progressKey, directoryKey, downloadedKey, uploadedKey = "hash_string", "total_size", "percent_complete", "download_dir", "downloaded_ever", "uploaded_ever"
	}
	hashValue, err := requiredString(fields, hashKey)
	if err != nil {
		return downloader.Torrent{}, fmt.Errorf("missing typed hash")
	}
	hashValue, err = normalizeV1Hash(hashValue)
	if err != nil {
		return downloader.Torrent{}, err
	}
	name, err := requiredString(fields, "name")
	if err != nil || !validSimpleName(name) {
		return downloader.Torrent{}, fmt.Errorf("invalid torrent name")
	}
	directory, err := requiredString(fields, directoryKey)
	if err != nil || directory == "" || len(directory) > maxPathBytes || !validControlSafeUTF8(directory) {
		return downloader.Torrent{}, fmt.Errorf("invalid download directory")
	}
	contentPath, err := joinClaimedContentPath(directory, name)
	if err != nil {
		return downloader.Torrent{}, err
	}
	size, err := requiredInt64(fields, sizeKey)
	if err != nil || size < 0 {
		return downloader.Torrent{}, fmt.Errorf("invalid torrent size")
	}
	progress, err := requiredFloat64(fields, progressKey)
	if err != nil || progress < 0 || progress > 1 {
		return downloader.Torrent{}, fmt.Errorf("invalid torrent progress")
	}
	status, err := requiredInt64(fields, "status")
	if err != nil || status < 0 || status > 6 {
		return downloader.Torrent{}, fmt.Errorf("invalid torrent state")
	}
	downloaded, err := requiredInt64(fields, downloadedKey)
	if err != nil || downloaded < 0 {
		return downloader.Torrent{}, fmt.Errorf("invalid downloaded byte counter")
	}
	uploaded, err := requiredInt64(fields, uploadedKey)
	if err != nil || uploaded < 0 {
		return downloader.Torrent{}, fmt.Errorf("invalid uploaded byte counter")
	}
	return downloader.Torrent{
		Hash:             hashValue,
		InfoHashV1:       hashValue,
		IdentityStatus:   downloader.IdentityStatusValid,
		IdentityEvidence: []string{"transmission_hash_string_sha1"},
		IdentityIssues:   []string{},
		Name:             name,
		SizeBytes:        size,
		Progress:         progress,
		State:            normalizeState(status, progress),
		SavePath:         directory,
		ContentPath:      contentPath,
		Downloaded:       downloaded,
		Uploaded:         uploaded,
	}, nil
}

func (s *readSession) ReadJobFiles(ctx context.Context, opaqueJobKey string, limits downloader.JobFileLedgerLimits) (snapshot downloader.JobFileLedgerSnapshot, err error) {
	snapshot = downloader.JobFileLedgerSnapshot{
		Driver:          downloader.DriverTransmission,
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
	canonicalKey, err := normalizeV1Hash(opaqueJobKey)
	if err != nil || canonicalKey != opaqueJobKey {
		return snapshot, fmt.Errorf("invalid Transmission opaque job key")
	}
	result, responseBytes, err := s.call(ctx, "torrent-get", "torrent_get", map[string]any{
		"ids": []string{opaqueJobKey}, "fields": fileFields(s.protocol),
	}, limits.MaxResponseBytes)
	snapshot.Used.ResponseBytes = responseBytes
	if err != nil {
		return snapshot, err
	}
	files, used, savePath, contentPath, err := decodeFileResult(ctx, result, s.protocol, opaqueJobKey, limits)
	used.ResponseBytes = responseBytes
	snapshot.Used = used
	if err != nil {
		return snapshot, err
	}
	snapshot.Files = files
	snapshot.SavePath = savePath
	snapshot.ContentPath = contentPath
	snapshot.Complete = true
	return snapshot, nil
}

type fileConstant struct {
	name      string
	length    int64
	completed int64
}

type fileState struct {
	completed int64
	wanted    bool
	priority  int64
}

func decodeFileResult(ctx context.Context, result json.RawMessage, valueProtocol protocol, expectedKey string, limits downloader.JobFileLedgerLimits) ([]downloader.JobFile, downloader.JobFileLedgerUsage, string, string, error) {
	usage := downloader.JobFileLedgerUsage{}
	fields, err := decodeObject(result, 16)
	if err != nil {
		return nil, usage, "", "", fmt.Errorf("decode Transmission file result")
	}
	rawJobs, exists := fields["torrents"]
	if !exists {
		return nil, usage, "", "", fmt.Errorf("Transmission file result is missing torrents")
	}
	var rawJob json.RawMessage
	count, err := decodeRawArray(rawJobs, 2, func(_ int, raw json.RawMessage) error {
		if rawJob != nil {
			return fmt.Errorf("Transmission file result is ambiguous")
		}
		rawJob = append(json.RawMessage(nil), raw...)
		return nil
	})
	if err != nil || count != 1 {
		return nil, usage, "", "", fmt.Errorf("Transmission file result did not identify exactly one job")
	}
	job, err := decodeObject(rawJob, maxJobFields)
	if err != nil {
		return nil, usage, "", "", fmt.Errorf("decode Transmission file job")
	}
	hashKey, statsKey := "hashString", "fileStats"
	if valueProtocol == protocolJSONRPC {
		hashKey, statsKey = "hash_string", "file_stats"
	}
	hashValue, err := requiredString(job, hashKey)
	if err != nil {
		return nil, usage, "", "", fmt.Errorf("Transmission file job is missing its hash")
	}
	hashValue, err = normalizeV1Hash(hashValue)
	if err != nil || hashValue != expectedKey {
		return nil, usage, "", "", fmt.Errorf("Transmission file job identity changed")
	}
	jobName, err := requiredString(job, "name")
	if err != nil || !validSimpleName(jobName) {
		return nil, usage, "", "", fmt.Errorf("Transmission file job has an invalid name")
	}
	directoryKey := "downloadDir"
	if valueProtocol == protocolJSONRPC {
		directoryKey = "download_dir"
	}
	directory, err := requiredString(job, directoryKey)
	if err != nil || directory == "" || len(directory) > maxPathBytes || !validControlSafeUTF8(directory) {
		return nil, usage, "", "", fmt.Errorf("Transmission file job has an invalid download directory")
	}
	contentPath, err := joinClaimedContentPath(directory, jobName)
	if err != nil {
		return nil, usage, "", "", err
	}
	rawFiles, filesOK := job["files"]
	rawStats, statsOK := job[statsKey]
	if !filesOK || !statsOK {
		return nil, usage, "", "", fmt.Errorf("Transmission file job is missing file arrays")
	}
	constants := make([]fileConstant, 0, min(limits.MaxFiles, 1024))
	_, err = decodeRawArray(rawFiles, limits.MaxFiles, func(_ int, raw json.RawMessage) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		value, err := decodeFileConstant(raw, valueProtocol)
		if err != nil {
			return err
		}
		constants = append(constants, value)
		return nil
	})
	if err != nil {
		return nil, usage, "", "", fmt.Errorf("decode Transmission file constants: %w", err)
	}
	states := make([]fileState, 0, len(constants))
	_, err = decodeRawArray(rawStats, limits.MaxFiles, func(_ int, raw json.RawMessage) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		value, err := decodeFileState(raw, valueProtocol)
		if err != nil {
			return err
		}
		states = append(states, value)
		return nil
	})
	if err != nil {
		return nil, usage, "", "", fmt.Errorf("decode Transmission file state: %w", err)
	}
	if len(constants) != len(states) {
		return nil, usage, "", "", fmt.Errorf("Transmission file arrays have different lengths")
	}
	files := make([]downloader.JobFile, 0, len(constants))
	seenPaths := make(map[string]struct{}, len(constants))
	for index := range constants {
		if err := ctx.Err(); err != nil {
			return nil, usage, "", "", err
		}
		constant, state := constants[index], states[index]
		if constant.completed != state.completed || constant.completed > constant.length {
			return nil, usage, "", "", fmt.Errorf("Transmission file byte counters disagree")
		}
		components, pathBytes, err := splitRelativePath(constant.name)
		if err != nil {
			return nil, usage, "", "", err
		}
		if pathBytes > limits.MaxPathBytes-usage.PathBytes {
			return nil, usage, "", "", fmt.Errorf("Transmission file paths exceeded their byte limit")
		}
		usage.PathBytes += pathBytes
		pathKey := strings.Join(components, "\x00")
		if _, exists := seenPaths[pathKey]; exists {
			return nil, usage, "", "", fmt.Errorf("Transmission file result contains a duplicate effective path")
		}
		seenPaths[pathKey] = struct{}{}
		progress := float64(1)
		if constant.length > 0 {
			progress = float64(constant.completed) / float64(constant.length)
		}
		selection := downloader.JobFileSelectionSkipped
		if state.wanted {
			selection = downloader.JobFileSelectionSelected
		}
		files = append(files, downloader.JobFile{
			Index: index, RelativeComponents: components, SizeBytes: constant.length,
			Progress: progress, Selection: selection, Complete: constant.completed == constant.length,
		})
		usage.FilesConsidered++
	}
	return files, usage, directory, contentPath, nil
}

func decodeFileConstant(raw json.RawMessage, valueProtocol protocol) (fileConstant, error) {
	fields, err := decodeObject(raw, maxFileFields)
	if err != nil {
		return fileConstant{}, err
	}
	completedKey := "bytesCompleted"
	if valueProtocol == protocolJSONRPC {
		completedKey = "bytes_completed"
	}
	name, err := requiredString(fields, "name")
	if err != nil {
		return fileConstant{}, fmt.Errorf("Transmission file is missing its name")
	}
	length, err := requiredInt64(fields, "length")
	if err != nil || length < 0 {
		return fileConstant{}, fmt.Errorf("Transmission file has an invalid length")
	}
	completed, err := requiredInt64(fields, completedKey)
	if err != nil || completed < 0 {
		return fileConstant{}, fmt.Errorf("Transmission file has invalid completed bytes")
	}
	return fileConstant{name: name, length: length, completed: completed}, nil
}

func decodeFileState(raw json.RawMessage, valueProtocol protocol) (fileState, error) {
	fields, err := decodeObject(raw, maxFileFields)
	if err != nil {
		return fileState{}, err
	}
	completedKey := "bytesCompleted"
	if valueProtocol == protocolJSONRPC {
		completedKey = "bytes_completed"
	}
	completed, err := requiredInt64(fields, completedKey)
	if err != nil || completed < 0 {
		return fileState{}, fmt.Errorf("Transmission file state has invalid completed bytes")
	}
	var wanted bool
	if valueProtocol == protocolJSONRPC {
		wanted, err = requiredBool(fields, "wanted")
	} else {
		wanted, err = requiredLegacyWanted(fields, "wanted")
	}
	if err != nil {
		return fileState{}, err
	}
	priority, err := requiredInt64(fields, "priority")
	if err != nil || priority < -1 || priority > 1 {
		return fileState{}, fmt.Errorf("Transmission file state has invalid priority")
	}
	return fileState{completed: completed, wanted: wanted, priority: priority}, nil
}

func normalizeV1Hash(value string) (string, error) {
	if len(value) != maxOpaqueKeyBytes {
		return "", fmt.Errorf("Transmission hash_string is not a full SHA-1 infohash")
	}
	digest, err := hex.DecodeString(value)
	if err != nil || len(digest) != 20 {
		return "", fmt.Errorf("Transmission hash_string is not a full SHA-1 infohash")
	}
	return hex.EncodeToString(digest), nil
}

func normalizeState(value int64, progress float64) string {
	switch value {
	case 0:
		if progress == 1 {
			return "stoppedUP"
		}
		return "stoppedDL"
	case 1:
		return "checkingResumeData"
	case 2:
		return "checkingDL"
	case 3:
		return "queuedDL"
	case 4:
		return "downloading"
	case 5:
		return "queuedUP"
	case 6:
		return "uploading"
	default:
		return "unknown"
	}
}

func joinClaimedContentPath(directory, name string) (string, error) {
	if !validSimpleName(name) || directory == "" || len(directory)+1+len(name) > maxPathBytes {
		return "", fmt.Errorf("Transmission content path is invalid")
	}
	if strings.HasSuffix(directory, "/") || strings.HasSuffix(directory, "\\") {
		return directory + name, nil
	}
	separator := "/"
	if strings.Contains(directory, "\\") && !strings.Contains(directory, "/") {
		separator = "\\"
	}
	return directory + separator + name, nil
}

func validSimpleName(value string) bool {
	if value == "" || value == "." || value == ".." || len(value) > maxNameBytes || strings.ContainsAny(value, "/\\") || !validControlSafeUTF8(value) {
		return false
	}
	return true
}

func splitRelativePath(value string) ([]string, int64, error) {
	if value == "" || len(value) > maxPathBytes || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || !utf8.ValidString(value) {
		return nil, 0, fmt.Errorf("Transmission file has an invalid relative path")
	}
	components := strings.Split(value, "/")
	if len(components) > maxFilePathDepth {
		return nil, 0, fmt.Errorf("Transmission file path is too deep")
	}
	pathBytes := int64(0)
	for _, component := range components {
		if component == "" || component == "." || component == ".." || !validControlSafeUTF8(component) {
			return nil, 0, fmt.Errorf("Transmission file has an invalid relative path")
		}
		pathBytes += int64(len(component))
	}
	return append([]string(nil), components...), pathBytes, nil
}

func validControlSafeUTF8(value string) bool {
	if !utf8.ValidString(value) || strings.ContainsRune(value, utf8.RuneError) {
		return false
	}
	for _, character := range value {
		if character == 0 || character == 0x7f || unicode.IsControl(character) {
			return false
		}
	}
	return true
}
