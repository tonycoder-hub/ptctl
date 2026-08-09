package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

const maxRetainedClientFileFindings = 20

type ClientFileLayoutLedger struct {
	Status             string                         `json:"status"`
	JobID              string                         `json:"job_id,omitempty"`
	SnapshotID         string                         `json:"snapshot_id,omitempty"`
	StabilityAssurance string                         `json:"stability_assurance"`
	Limits             downloader.JobFileLedgerLimits `json:"limits"`
	UsedBefore         downloader.JobFileLedgerUsage  `json:"used_before"`
	UsedAfter          downloader.JobFileLedgerUsage  `json:"used_after"`
	RequestsMade       int                            `json:"requests_made"`
	FilesExpected      int                            `json:"files_expected"`
	FilesObserved      int                            `json:"files_observed"`
	FilesSelected      int                            `json:"files_selected"`
	FilesComplete      int                            `json:"files_complete"`
	StopReasons        []string                       `json:"stop_reasons"`
	Findings           []ClientFileFinding            `json:"findings"`
	FindingOverflow    int                            `json:"finding_overflow"`
}

type ClientFileFinding struct {
	ManifestIndex int    `json:"manifest_index"`
	TorrentPath   string `json:"torrent_path,omitempty"`
	Status        string `json:"status"`
	ExpectedSize  int64  `json:"expected_size_bytes,omitempty"`
	ClientSize    *int64 `json:"client_size_bytes,omitempty"`
	ClientPathRef string `json:"client_path_ref,omitempty"`
	ClientPath    string `json:"client_path,omitempty"`
}

type clientFileLayoutAssessment struct {
	ledger     ClientFileLayoutLedger
	paths      map[int]clientPath
	stable     bool
	conflict   bool
	incomplete bool
	unselected bool
	unfinished bool
	manifestOK bool
	snapshotID string
}

func normalizeClientFileLayoutMode(requested bool, value string) (string, error) {
	if !requested {
		if value != "" && value != "auto" && value != "off" {
			return "", fmt.Errorf("invalid client file layout mode")
		}
		return "not_requested", nil
	}
	if value == "" {
		value = "auto"
	}
	if value != "auto" && value != "off" {
		return "", fmt.Errorf("invalid client file layout mode")
	}
	return value, nil
}

// SelectExactJobForFileRead selects only a unique algorithm-tagged identity.
// The opaque key is a same-session locator, not BitTorrent identity, and Build
// independently revalidates both outer ledger snapshots.
func SelectExactJobForFileRead(meta *metafile.MetaInfo, snapshot downloader.LedgerSnapshot, limits downloader.JobFileLedgerLimits) (string, bool) {
	if meta == nil || !meta.MultiFile || limits.Validate() != nil || len(meta.Files) > limits.MaxFiles || !manifestSupportsClientFileLayout(meta) ||
		!snapshot.Complete || snapshot.Driver != "qbittorrent" || !snapshot.Capabilities.TypedInfoHashes || !snapshot.Capabilities.ContentPath || !snapshot.Capabilities.JobFiles ||
		snapshot.ObservedAtStart.IsZero() || snapshot.ObservedAtEnd.Before(snapshot.ObservedAtStart) || !validLedgerJobs(snapshot.Jobs) {
		return "", false
	}
	assessment := assessSnapshot(meta, snapshot)
	if assessment.status != "exact_unique" || len(assessment.exact) != 1 {
		return "", false
	}
	return assessment.exact[0].Hash, true
}

func assessClientFileLayout(meta *metafile.MetaInfo, job *downloader.Torrent, bracket ClientBracket, windows, showAbsolute bool) clientFileLayoutAssessment {
	limits := bracket.FileLimits
	limitsValid := limits.Validate() == nil
	if !limitsValid {
		limits = downloader.DefaultJobFileLedgerLimits()
	}
	result := clientFileLayoutAssessment{
		ledger: ClientFileLayoutLedger{
			Status: "not_requested", StabilityAssurance: "not_requested", Limits: limits,
			StopReasons: []string{}, Findings: []ClientFileFinding{}, FilesExpected: len(meta.Files), RequestsMade: bracket.FileRequestsMade,
		},
		paths: map[int]clientPath{},
	}
	if bracket.FileRequestsMade < 0 || bracket.FileRequestsMade > 2 {
		result.ledger.Status = "incomplete"
		result.ledger.StopReasons = append(result.ledger.StopReasons, "client_file_request_count_invalid")
		result.incomplete = true
		return result
	}
	if bracket.FilesBefore != nil {
		result.ledger.UsedBefore = bracket.FilesBefore.Used
	}
	if bracket.FilesAfter != nil {
		result.ledger.UsedAfter = bracket.FilesAfter.Used
	}
	fileActivity := hasClientFileActivity(bracket)
	mode, err := normalizeClientFileLayoutMode(bracket.Requested, bracket.FileLayoutMode)
	if err != nil {
		result.ledger.Status = "incomplete"
		result.ledger.StopReasons = append(result.ledger.StopReasons, "invalid_file_layout_mode")
		result.incomplete = true
		return result
	}
	if !bracket.Requested || mode == "off" {
		if fileActivity {
			return unexpectedClientFileActivity(result)
		}
		return result
	}
	if !meta.MultiFile {
		if fileActivity {
			return unexpectedClientFileActivity(result)
		}
		result.ledger.Status = "not_applicable"
		return result
	}
	result.ledger.StabilityAssurance = "bracketed_non_atomic"
	if !limitsValid && bracket.FileAttempted {
		result.ledger.Status = "incomplete"
		result.ledger.StopReasons = append(result.ledger.StopReasons, "invalid_client_file_limits")
		result.incomplete = true
		return result
	}
	if limitsValid && len(meta.Files) > limits.MaxFiles {
		result.ledger.Status = "incomplete"
		result.ledger.StopReasons = append(result.ledger.StopReasons, "max_client_files")
		result.incomplete = true
		return result
	}
	if !manifestSupportsClientFileLayout(meta) {
		if fileActivity {
			return unexpectedClientFileActivity(result)
		}
		result.ledger.Status = "unsupported"
		result.ledger.StopReasons = append(result.ledger.StopReasons, "unsupported_manifest_layout")
		return result
	}
	if bracket.FileStopReason != "" {
		result.ledger.Status = "incomplete"
		result.ledger.StopReasons = append(result.ledger.StopReasons, safeFileStopReason(bracket.FileStopReason))
		result.incomplete = true
		return result
	}
	if job == nil {
		if bracket.FileAttempted {
			result.ledger.Status = "incomplete"
			result.ledger.StopReasons = append(result.ledger.StopReasons, "client_file_identity_unbound")
			result.incomplete = true
		} else {
			result.ledger.Status = "not_attempted"
			result.ledger.StopReasons = append(result.ledger.StopReasons, "exact_job_unavailable")
		}
		return result
	}
	result.ledger.JobID = jobID(job.Hash)
	if bracket.Before == nil || !bracket.Before.Capabilities.JobFiles {
		if fileActivity {
			return unexpectedClientFileActivity(result)
		}
		result.ledger.Status = "unsupported"
		result.ledger.StopReasons = append(result.ledger.StopReasons, "job_files_capability_missing")
		return result
	}
	if !bracket.FileAttempted || bracket.FilesBefore == nil || bracket.FilesAfter == nil {
		result.ledger.Status = "incomplete"
		result.ledger.StopReasons = append(result.ledger.StopReasons, "client_file_snapshot_incomplete")
		result.incomplete = true
		return result
	}
	if !validJobFileBracket(bracket, job.Hash, limits) {
		result.ledger.Status = "incomplete"
		result.ledger.StopReasons = append(result.ledger.StopReasons, "client_file_snapshot_incomplete")
		result.incomplete = true
		return result
	}
	beforeSignature := jobFileSignature(bracket.FilesBefore.Files)
	afterSignature := jobFileSignature(bracket.FilesAfter.Files)
	if beforeSignature != afterSignature {
		result.ledger.Status = "unstable"
		result.ledger.StopReasons = append(result.ledger.StopReasons, "client_file_snapshot_unstable")
		result.incomplete = true
		return result
	}
	result.stable = true
	result.snapshotID = fileSnapshotID(job.Hash, afterSignature)
	result.ledger.Status = "observed_stable"
	result.ledger.SnapshotID = result.snapshotID
	result.ledger.FilesObserved = len(bracket.FilesAfter.Files)
	result.compareManifest(meta, *job, bracket.FilesAfter.Files, windows, showAbsolute)
	result.ledger.StopReasons = stableStrings(result.ledger.StopReasons)
	return result
}

func hasClientFileActivity(bracket ClientBracket) bool {
	return bracket.FileAttempted ||
		bracket.FileRequestsMade != 0 ||
		bracket.FilesBefore != nil ||
		bracket.FilesAfter != nil ||
		bracket.FileStopReason != ""
}

func unexpectedClientFileActivity(result clientFileLayoutAssessment) clientFileLayoutAssessment {
	result.ledger.Status = "incomplete"
	result.ledger.StabilityAssurance = "bracketed_non_atomic"
	result.ledger.StopReasons = append(result.ledger.StopReasons, "unexpected_client_file_activity")
	result.incomplete = true
	return result
}

func (result *clientFileLayoutAssessment) compareManifest(meta *metafile.MetaInfo, job downloader.Torrent, files []downloader.JobFile, windows, showAbsolute bool) {
	byIndex := make(map[int]downloader.JobFile, len(files))
	for _, file := range files {
		byIndex[file.Index] = file
		if file.Index < 0 || file.Index >= len(meta.Files) {
			result.conflict = true
			result.addFinding(ClientFileFinding{ManifestIndex: file.Index, Status: "unexpected_index", ClientSize: int64Pointer(file.SizeBytes)})
		}
	}
	if len(files) != len(meta.Files) {
		result.conflict = true
	}
	savePath, saveErr := parseClientPath(job.SavePath, windows)
	contentRoot, contentErr := parseClientPath(job.ContentPath, windows)
	if saveErr != nil || contentErr != nil || !contentRoot.within(savePath) {
		result.incomplete = true
		result.ledger.StopReasons = append(result.ledger.StopReasons, "client_file_path_invalid")
	}
	for index, manifestFile := range meta.Files {
		clientFile, exists := byIndex[index]
		if !exists {
			result.conflict = true
			result.addFinding(ClientFileFinding{ManifestIndex: index, TorrentPath: torrentFilePath(meta, index), Status: "missing_index", ExpectedSize: manifestFile.Length})
			continue
		}
		if clientFile.SizeBytes != manifestFile.Length {
			result.conflict = true
			result.addFinding(ClientFileFinding{ManifestIndex: index, TorrentPath: torrentFilePath(meta, index), Status: "size_conflict", ExpectedSize: manifestFile.Length, ClientSize: int64Pointer(clientFile.SizeBytes)})
		}
		if saveErr == nil && contentErr == nil {
			components, err := parseClientRelativeComponents(clientFile.RelativeComponents, windows)
			if err != nil {
				result.incomplete = true
				result.addFinding(ClientFileFinding{ManifestIndex: index, TorrentPath: torrentFilePath(meta, index), Status: "invalid_client_path", ExpectedSize: manifestFile.Length, ClientSize: int64Pointer(clientFile.SizeBytes)})
			} else {
				fullPath := savePath.joinRelative(components)
				if len(fullPath.canonical()) > maxClientPathBytes {
					result.incomplete = true
					result.addFinding(ClientFileFinding{ManifestIndex: index, TorrentPath: torrentFilePath(meta, index), Status: "invalid_client_path", ExpectedSize: manifestFile.Length, ClientSize: int64Pointer(clientFile.SizeBytes)})
				} else if !fullPath.within(contentRoot) {
					result.incomplete = true
					result.addFinding(filePathFinding(meta, index, "outside_content_root", manifestFile.Length, clientFile.SizeBytes, fullPath, showAbsolute))
				} else {
					result.paths[index] = fullPath
				}
			}
		}
		if clientFile.Selection == downloader.JobFileSelectionSelected {
			result.ledger.FilesSelected++
		} else {
			result.unselected = true
			result.addFinding(ClientFileFinding{ManifestIndex: index, TorrentPath: torrentFilePath(meta, index), Status: "skipped", ExpectedSize: manifestFile.Length, ClientSize: int64Pointer(clientFile.SizeBytes)})
		}
		if clientFile.Progress == 1 && clientFile.Complete {
			result.ledger.FilesComplete++
		} else {
			result.unfinished = true
			result.addFinding(ClientFileFinding{ManifestIndex: index, TorrentPath: torrentFilePath(meta, index), Status: "incomplete", ExpectedSize: manifestFile.Length, ClientSize: int64Pointer(clientFile.SizeBytes)})
		}
	}
	if len(result.paths) == len(meta.Files) {
		if err := validateClientPathSet(result.paths); err != nil {
			result.incomplete = true
			result.ledger.StopReasons = append(result.ledger.StopReasons, "client_file_path_collision")
		}
	}
	result.manifestOK = !result.conflict && !result.incomplete && len(result.paths) == len(meta.Files)
}

func validJobFileBracket(bracket ClientBracket, jobKey string, limits downloader.JobFileLedgerLimits) bool {
	if bracket.Before == nil || bracket.After == nil || bracket.FilesBefore == nil || bracket.FilesAfter == nil || bracket.FileRequestsMade != 2 {
		return false
	}
	beforeFiles, afterFiles := *bracket.FilesBefore, *bracket.FilesAfter
	if !validJobFileSnapshot(beforeFiles, limits) || !validJobFileSnapshot(afterFiles, limits) || beforeFiles.Driver != bracket.Before.Driver || afterFiles.Driver != bracket.After.Driver ||
		beforeFiles.JobKey != jobKey || afterFiles.JobKey != jobKey {
		return false
	}
	return !beforeFiles.ObservedAtStart.Before(bracket.Before.ObservedAtEnd) &&
		!afterFiles.ObservedAtStart.Before(beforeFiles.ObservedAtEnd) &&
		!bracket.After.ObservedAtStart.Before(afterFiles.ObservedAtEnd)
}

func validJobFileSnapshot(snapshot downloader.JobFileLedgerSnapshot, limits downloader.JobFileLedgerLimits) bool {
	if !snapshot.Complete || snapshot.Driver != "qbittorrent" || !validOpaqueJobKey(snapshot.JobKey) || snapshot.Limits != limits || limits.Validate() != nil ||
		snapshot.ObservedAtStart.IsZero() || snapshot.ObservedAtEnd.Before(snapshot.ObservedAtStart) || len(snapshot.Files) > limits.MaxFiles ||
		snapshot.Used.ResponseBytes <= 0 || snapshot.Used.ResponseBytes > limits.MaxResponseBytes || snapshot.Used.PathBytes < 0 || snapshot.Used.PathBytes > limits.MaxPathBytes ||
		snapshot.Used.FilesConsidered != len(snapshot.Files) {
		return false
	}
	seenIndex := make(map[int]struct{}, len(snapshot.Files))
	seenPath := make(map[string]struct{}, len(snapshot.Files))
	pathBytes := int64(0)
	for _, file := range snapshot.Files {
		if file.Index < 0 || file.SizeBytes < 0 || math.IsNaN(file.Progress) || math.IsInf(file.Progress, 0) || file.Progress < 0 || file.Progress > 1 || len(file.RelativeComponents) == 0 || len(file.RelativeComponents) > 128 {
			return false
		}
		if file.Selection != downloader.JobFileSelectionSelected && file.Selection != downloader.JobFileSelectionSkipped {
			return false
		}
		if _, exists := seenIndex[file.Index]; exists {
			return false
		}
		seenIndex[file.Index] = struct{}{}
		for _, component := range file.RelativeComponents {
			if component == "" || !utf8.ValidString(component) || strings.ContainsRune(component, utf8.RuneError) || strings.ContainsAny(component, "/\\") {
				return false
			}
			for _, character := range component {
				if unicode.IsControl(character) {
					return false
				}
			}
			pathBytes += int64(len(component))
			if pathBytes < 0 || pathBytes > limits.MaxPathBytes {
				return false
			}
		}
		pathKey := strings.Join(file.RelativeComponents, "\x00")
		if _, exists := seenPath[pathKey]; exists {
			return false
		}
		seenPath[pathKey] = struct{}{}
	}
	return pathBytes == snapshot.Used.PathBytes
}

func jobFileSignature(files []downloader.JobFile) string {
	ordered := append([]downloader.JobFile(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })
	hasher := sha256.New()
	writeHashField(hasher, "ptctl-job-file-ledger-v1")
	for _, file := range ordered {
		writeHashField(hasher, strconv.Itoa(file.Index))
		writeHashField(hasher, strconv.FormatInt(file.SizeBytes, 10))
		writeHashField(hasher, strconv.FormatFloat(file.Progress, 'g', -1, 64))
		writeHashField(hasher, string(file.Selection))
		writeHashField(hasher, strconv.FormatBool(file.Complete))
		for _, component := range file.RelativeComponents {
			writeHashField(hasher, component)
		}
		writeHashField(hasher, "")
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func writeHashField(hasher hash.Hash, value string) {
	_, _ = hasher.Write([]byte(strconv.Itoa(len(value))))
	_, _ = hasher.Write([]byte{':'})
	_, _ = hasher.Write([]byte(value))
}

func fileSnapshotID(jobKey, signature string) string {
	digest := sha256.Sum256([]byte("ptctl-client-file-snapshot-v1\x00" + jobID(jobKey) + "\x00" + signature))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func clientPathSetID(kind string, paths map[int]clientPath) string {
	indices := make([]int, 0, len(paths))
	for index := range paths {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	hasher := sha256.New()
	writeHashField(hasher, "ptctl-client-path-set-v1")
	writeHashField(hasher, kind)
	for _, index := range indices {
		writeHashField(hasher, strconv.Itoa(index))
		writeHashField(hasher, paths[index].canonical())
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil))
}

func manifestSupportsClientFileLayout(meta *metafile.MetaInfo) bool {
	if meta == nil || !meta.MultiFile || len(meta.Files) == 0 {
		return false
	}
	for _, file := range meta.Files {
		if file.Attribute != "" || file.Length == 0 {
			return false
		}
	}
	return true
}

func compareVerifiedSourceFilePaths(meta *metafile.MetaInfo, source *metafile.VerifiedSource, mapping PathMappingOptions, claimed map[int]clientPath, showAbsolute bool, maxFindings int) (map[int]clientPath, []ClientFileFinding, int, error) {
	if meta == nil || source == nil || !source.Matches(meta) {
		return nil, nil, 0, fmt.Errorf("there is no matching process-local verified source")
	}
	if maxFindings < 0 {
		maxFindings = 0
	}
	expected := make(map[int]clientPath, len(meta.Files))
	findings := make([]ClientFileFinding, 0, min(maxFindings, len(meta.Files)))
	findingOverflow := 0
	for index, manifestFile := range meta.Files {
		hostPath, ok := source.Path(index)
		if !ok {
			return nil, nil, 0, fmt.Errorf("verified source has no physical binding for manifest file %d", index)
		}
		mapped, err := storage.MapHostToClient(mapping.HostRoot, hostPath, mapping.ClientRoot, mapping.ClientWindows)
		if err != nil {
			return nil, nil, 0, err
		}
		parsed, err := parseClientPath(mapped.ClientPath, mapping.ClientWindows)
		if err != nil {
			return nil, nil, 0, err
		}
		expected[index] = parsed
		clientPath, exists := claimed[index]
		if !exists {
			return nil, nil, 0, fmt.Errorf("client file ledger has no effective path for manifest file %d", index)
		}
		if !parsed.equal(clientPath) {
			finding := ClientFileFinding{
				ManifestIndex: index, TorrentPath: torrentFilePath(meta, index), Status: "different_location", ExpectedSize: manifestFile.Length,
				ClientPathRef: clientPath.public(false),
			}
			if showAbsolute {
				finding.ClientPath = clientPath.public(true)
			}
			if len(findings) < maxFindings {
				findings = append(findings, finding)
			} else {
				findingOverflow++
			}
		}
	}
	if err := validateClientPathSet(expected); err != nil {
		return nil, nil, 0, err
	}
	return expected, findings, findingOverflow, nil
}

func (result *clientFileLayoutAssessment) addFinding(value ClientFileFinding) {
	if len(result.ledger.Findings) < maxRetainedClientFileFindings {
		result.ledger.Findings = append(result.ledger.Findings, value)
	} else {
		result.ledger.FindingOverflow++
	}
}

func filePathFinding(meta *metafile.MetaInfo, index int, status string, expected, actual int64, path clientPath, show bool) ClientFileFinding {
	result := ClientFileFinding{
		ManifestIndex: index, TorrentPath: torrentFilePath(meta, index), Status: status, ExpectedSize: expected, ClientSize: int64Pointer(actual),
		ClientPathRef: path.public(false),
	}
	if show {
		result.ClientPath = path.public(true)
	}
	return result
}

func torrentFilePath(meta *metafile.MetaInfo, index int) string {
	if meta == nil || index < 0 || index >= len(meta.Files) {
		return ""
	}
	return strings.Join(meta.Files[index].Path, "/")
}

func int64Pointer(value int64) *int64 { return &value }

func safeFileStopReason(value string) string {
	switch value {
	case "client_file_snapshot_before_failed", "client_file_snapshot_after_failed", "client_file_snapshot_incomplete", "context_cancelled", "max_client_files", "unsupported_manifest_layout":
		return value
	default:
		return "client_file_read_failed"
	}
}
