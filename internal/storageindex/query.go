package storageindex

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"

	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

const queryEffect = "read_private_storage_index+read_live_storage_metadata"

type CandidateLimits struct {
	MaxCandidates int   `json:"max_candidates"`
	MaxPathBytes  int64 `json:"max_path_bytes"`
	MaxIssues     int   `json:"max_issues"`
}

func DefaultCandidateLimits() CandidateLimits {
	return CandidateLimits{MaxCandidates: 10_000, MaxPathBytes: 16 << 20, MaxIssues: 50}
}

func (limits CandidateLimits) Validate() error {
	if limits.MaxCandidates <= 0 || limits.MaxCandidates > 50_000 {
		return fmt.Errorf("maximum index candidates must be in 1..50000")
	}
	if limits.MaxPathBytes <= 0 || limits.MaxPathBytes > 64<<20 {
		return fmt.Errorf("maximum index candidate path bytes must be in 1..67108864")
	}
	if limits.MaxIssues <= 0 || limits.MaxIssues > 200 {
		return fmt.Errorf("maximum index candidate issues must be in 1..200")
	}
	return nil
}

type CandidateIssue struct {
	Code   string `json:"code"`
	RootID string `json:"root_id,omitempty"`
	Count  int    `json:"count,omitempty"`
}

type CandidateStats struct {
	SnapshotFilesConsidered int   `json:"snapshot_files_considered"`
	SizeMatchesConsidered   int   `json:"size_matches_considered"`
	CandidatesRetained      int   `json:"candidates_retained"`
	RetainedPathBytes       int64 `json:"retained_path_bytes"`
	StaleLocators           int   `json:"stale_locators"`
	ChangedHints            int   `json:"changed_hints"`
	StaleRoots              int   `json:"stale_roots"`
	IssueOverflow           int   `json:"issue_overflow"`
}

// IndexedCandidate contains fresh process-local FileObservation authority.
// SnapshotEntry is historical evidence only; all content proof must use the
// live observation and its identity-bound opener.
type IndexedCandidate struct {
	SnapshotEntry Entry                   `json:"snapshot_entry"`
	Observation   storage.FileObservation `json:"-"`
	ResolvedPath  string                  `json:"-"`
	HintsChanged  bool                    `json:"hints_changed"`
}

type CandidateResult struct {
	Effect                     string                      `json:"effect"`
	Complete                   bool                        `json:"complete"`
	HistoricalSnapshotVerified bool                        `json:"historical_snapshot_verified"`
	CurrentSearchComplete      bool                        `json:"current_search_complete"`
	ProfileID                  string                      `json:"profile_id"`
	SnapshotID                 string                      `json:"snapshot_id"`
	DescriptorRecordID         metastore.RecordID          `json:"descriptor_record_id"`
	DataRecordID               metastore.RecordID          `json:"data_record_id"`
	Limits                     CandidateLimits             `json:"limits"`
	Stats                      CandidateStats              `json:"stats"`
	StopReasons                []string                    `json:"stop_reasons"`
	Issues                     []CandidateIssue            `json:"issues"`
	Warnings                   []string                    `json:"warnings"`
	DataLoad                   metastore.RecordLoadReceipt `json:"data_load"`
	Candidates                 []IndexedCandidate          `json:"-"`
	authorityDigest            [sha256.Size]byte
	authoritySet               bool
}

// HasLiveAuthority proves that this value is the unchanged process-local
// result returned by LoadCandidates for the exact immutable profile. Public
// JSON deliberately loses this authority, and changing any profile/snapshot,
// accounting, diagnostic, locator, or observation field invalidates it.
func (result CandidateResult) HasLiveAuthority(profile Profile) bool {
	if !result.authoritySet || !result.HistoricalSnapshotVerified || result.CurrentSearchComplete ||
		result.ProfileID != profile.ID || profile.ID == "" || profile.Revision == "" {
		return false
	}
	digest := candidateAuthorityDigest(result, profile)
	return subtle.ConstantTimeCompare(result.authorityDigest[:], digest[:]) == 1
}

func candidateAuthorityDigest(result CandidateResult, profile Profile) [sha256.Size]byte {
	digest := sha256.New()
	_, _ = io.WriteString(digest, "ptctl-storage-index-live-candidates-v1\x00")
	writeCandidateAuthorityString(digest, result.Effect)
	writeCandidateAuthorityBool(digest, result.Complete)
	writeCandidateAuthorityBool(digest, result.HistoricalSnapshotVerified)
	writeCandidateAuthorityBool(digest, result.CurrentSearchComplete)
	writeCandidateAuthorityString(digest, result.ProfileID)
	writeCandidateAuthorityString(digest, profile.Revision)
	writeCandidateAuthorityString(digest, result.SnapshotID)
	writeCandidateAuthorityString(digest, result.DescriptorRecordID.String())
	writeCandidateAuthorityString(digest, result.DataRecordID.String())
	writeCandidateAuthorityInts(digest,
		int64(result.Limits.MaxCandidates), result.Limits.MaxPathBytes, int64(result.Limits.MaxIssues),
		int64(result.Stats.SnapshotFilesConsidered), int64(result.Stats.SizeMatchesConsidered),
		int64(result.Stats.CandidatesRetained), result.Stats.RetainedPathBytes,
		int64(result.Stats.StaleLocators), int64(result.Stats.ChangedHints), int64(result.Stats.StaleRoots), int64(result.Stats.IssueOverflow),
	)
	writeCandidateAuthorityStrings(digest, result.StopReasons)
	writeCandidateAuthorityInt(digest, int64(len(result.Issues)))
	for _, issue := range result.Issues {
		writeCandidateAuthorityString(digest, issue.Code)
		writeCandidateAuthorityString(digest, issue.RootID)
		writeCandidateAuthorityInt(digest, int64(issue.Count))
	}
	writeCandidateAuthorityStrings(digest, result.Warnings)
	writeCandidateAuthorityString(digest, result.DataLoad.Effect)
	writeCandidateAuthorityBool(digest, result.DataLoad.Complete)
	writeCandidateAuthorityInts(digest, result.DataLoad.RecordBytesRead, result.DataLoad.ConsumerBytesRead)
	writeCandidateAuthorityBool(digest, result.DataLoad.RecordBytesKnown)
	writeCandidateAuthorityString(digest, result.DataLoad.Store.StoreID)
	writeCandidateAuthorityString(digest, result.DataLoad.Store.Format)
	writeCandidateAuthorityString(digest, result.DataLoad.Store.Privacy)
	writeCandidateAuthorityString(digest, result.DataLoad.Store.CommitAssurance)
	writeCandidateAuthorityInt(digest, int64(len(result.Candidates)))
	for _, candidate := range result.Candidates {
		entry := candidate.SnapshotEntry
		writeCandidateAuthorityString(digest, entry.Type)
		writeCandidateAuthorityString(digest, entry.RootID)
		writeCandidateAuthorityStrings(digest, entry.RelativeComponentsRawBase64)
		writeCandidateAuthorityInts(digest, entry.SizeBytes, entry.ModifiedUnixNanos)
		writeCandidateAuthorityString(digest, entry.IdentityHint)
		observation := candidate.Observation
		writeCandidateAuthorityString(digest, observation.ObservationID)
		writeCandidateAuthorityString(digest, observation.RootID)
		writeCandidateAuthorityString(digest, observation.RelativePath)
		writeCandidateAuthorityStrings(digest, observation.RelativeComponentsRawBase64)
		writeCandidateAuthorityInts(digest, observation.SizeBytes, observation.ModifiedAt.UnixNano())
		writeCandidateAuthorityInt(digest, int64(len(observation.Aliases)))
		for _, alias := range observation.Aliases {
			writeCandidateAuthorityString(digest, alias.RootID)
			writeCandidateAuthorityString(digest, alias.RelativePath)
			writeCandidateAuthorityStrings(digest, alias.RelativeComponentsRawBase64)
		}
		writeCandidateAuthorityString(digest, candidate.ResolvedPath)
		writeCandidateAuthorityBool(digest, candidate.HintsChanged)
	}
	var resultDigest [sha256.Size]byte
	copy(resultDigest[:], digest.Sum(nil))
	return resultDigest
}

func writeCandidateAuthorityString(digest hash.Hash, value string) {
	writeCandidateAuthorityInt(digest, int64(len(value)))
	_, _ = io.WriteString(digest, value)
}

func writeCandidateAuthorityStrings(digest hash.Hash, values []string) {
	writeCandidateAuthorityInt(digest, int64(len(values)))
	for _, value := range values {
		writeCandidateAuthorityString(digest, value)
	}
}

func writeCandidateAuthorityBool(digest hash.Hash, value bool) {
	if value {
		_, _ = digest.Write([]byte{1})
		return
	}
	_, _ = digest.Write([]byte{0})
}

func writeCandidateAuthorityInts(digest hash.Hash, values ...int64) {
	for _, value := range values {
		writeCandidateAuthorityInt(digest, value)
	}
}

func writeCandidateAuthorityInt(digest hash.Hash, value int64) {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], uint64(value))
	_, _ = digest.Write(raw[:])
}

// LoadCandidates verifies one sealed descriptor and its complete data stream,
// retains only requested sizes within hard budgets, then reobserves every
// retained locator beneath its immutable profile root. The result deliberately
// keeps CurrentSearchComplete=false: a historical snapshot cannot prove that
// no files were added, removed, or renamed since capture.
func (repository *Repository) LoadCandidates(ctx context.Context, profile Profile, descriptorRecordID metastore.RecordID, wantedSizes []int64, limits CandidateLimits) (CandidateResult, error) {
	result := CandidateResult{
		Effect: queryEffect, ProfileID: profile.ID, DescriptorRecordID: descriptorRecordID, Limits: limits,
		CurrentSearchComplete: false, StopReasons: []string{}, Issues: []CandidateIssue{}, Warnings: []string{
			"sealed snapshot completeness is historical and never proves the current filesystem search space",
			"index identity and modification-time fields are hints; every retained locator is reopened and content-verified live",
		}, Candidates: []IndexedCandidate{},
	}
	if repository == nil || repository.store == nil {
		return result, fmt.Errorf("storage index repository is unavailable")
	}
	if err := ValidateProfileForLiveUse(profile, repository.limits); err != nil {
		return result, err
	}
	if err := limits.Validate(); err != nil {
		return result, err
	}
	if descriptorRecordID == "" {
		return result, fmt.Errorf("storage index descriptor record ID is empty")
	}
	wanted, err := normalizeWantedSizes(wantedSizes)
	if err != nil {
		return result, err
	}
	descriptorRecord, err := repository.loadDescriptor(ctx, metastore.RecordRef{Kind: metastore.RecordKindStorageIndexDescriptorV1, ID: descriptorRecordID})
	if err != nil {
		return result, err
	}
	descriptor := descriptorRecord.descriptor
	if !descriptorMatchesProfile(descriptor, profile) {
		return result, ErrSnapshotSelectionIncomplete
	}
	result.SnapshotID = descriptor.ID
	dataID, err := metastore.ParseRecordID(descriptor.DataRecordID)
	if err != nil {
		return result, fmt.Errorf("storage index descriptor data identity is invalid")
	}
	result.DataRecordID = dataID

	provisional := make([]Entry, 0)
	var decodedHeader SnapshotHeader
	var decodedFooter SnapshotFooter
	dataRef, loadReceipt, loadErr := repository.store.LoadRecord(ctx, metastore.RecordKindStorageIndexDataV1, dataID, repository.recordLimits(repository.limits.MaxSnapshots), func(reader io.Reader) error {
		var decodeErr error
		decodedHeader, decodedFooter, decodeErr = DecodeSnapshot(ctx, reader, repository.limits, func(entry Entry) error {
			result.Stats.SnapshotFilesConsidered++
			if !wantedSize(wanted, entry.SizeBytes) {
				return nil
			}
			result.Stats.SizeMatchesConsidered++
			pathBytes, pathErr := encodedPathBytes(entry.RelativeComponentsRawBase64)
			if pathErr != nil {
				return pathErr
			}
			if len(provisional) >= limits.MaxCandidates {
				result.Complete = false
				result.StopReasons = appendUniqueString(result.StopReasons, "max_candidates")
				return nil
			}
			if pathBytes > limits.MaxPathBytes-result.Stats.RetainedPathBytes {
				result.Complete = false
				result.StopReasons = appendUniqueString(result.StopReasons, "max_path_bytes")
				return nil
			}
			provisional = append(provisional, cloneEntry(entry))
			result.Stats.RetainedPathBytes += pathBytes
			return nil
		})
		return decodeErr
	})
	result.DataLoad = loadReceipt
	if loadErr != nil || !loadReceipt.Complete {
		result.StopReasons = appendUniqueString(result.StopReasons, "snapshot_data_invalid")
		if loadErr != nil {
			return result, loadErr
		}
		return result, fmt.Errorf("storage index data load is incomplete")
	}
	if dataRef.ID != dataID || !snapshotBindsDescriptor(decodedHeader, decodedFooter, descriptor, profile) {
		result.StopReasons = appendUniqueString(result.StopReasons, "snapshot_descriptor_mismatch")
		return result, fmt.Errorf("storage index descriptor does not bind its data stream")
	}
	result.HistoricalSnapshotVerified = true
	if len(result.StopReasons) == 0 {
		result.Complete = true
	}

	rootByID, observationByID, err := candidateRoots(profile, descriptor)
	if err != nil {
		return result, err
	}
	grouped := make(map[string][]Entry)
	for _, entry := range provisional {
		grouped[entry.RootID] = append(grouped[entry.RootID], entry)
	}
	rootIDs := make([]string, 0, len(grouped))
	for rootID := range grouped {
		rootIDs = append(rootIDs, rootID)
	}
	sort.Strings(rootIDs)
	for _, rootID := range rootIDs {
		if err := ctx.Err(); err != nil {
			result.Complete = false
			result.StopReasons = appendUniqueString(result.StopReasons, "context_cancelled")
			return result, err
		}
		root := rootByID[rootID]
		expected := observationByID[rootID]
		group := make([]IndexedCandidate, 0, len(grouped[rootID]))
		rootStale := false
		staleLocators := 0
		for _, entry := range grouped[rootID] {
			live, observeErr := storage.ReobserveIndexedRegular(ctx, root, entry.RelativeComponentsRawBase64, profile.AllowNetwork)
			if observeErr != nil {
				if errors.Is(observeErr, context.Canceled) || errors.Is(observeErr, context.DeadlineExceeded) {
					result.Complete = false
					result.StopReasons = appendUniqueString(result.StopReasons, "context_cancelled")
					return result, observeErr
				}
				staleLocators++
				addCandidateIssue(&result, limits, CandidateIssue{Code: "locator_stale_or_unavailable", RootID: rootID, Count: 1})
				continue
			}
			if live.FilesystemIdentityHint != expected.FilesystemIdentityHint || live.RootIdentityHint != expected.RootIdentityHint {
				rootStale = true
				break
			}
			if live.Observation.SizeBytes != entry.SizeBytes {
				staleLocators++
				addCandidateIssue(&result, limits, CandidateIssue{Code: "locator_size_changed", RootID: rootID, Count: 1})
				continue
			}
			path, resolveErr := live.Observation.ResolveObservedRegularContext(ctx)
			if resolveErr != nil {
				staleLocators++
				addCandidateIssue(&result, limits, CandidateIssue{Code: "locator_changed_after_reobserve", RootID: rootID, Count: 1})
				continue
			}
			hintsChanged := live.Observation.ModifiedAt.UnixNano() != entry.ModifiedUnixNanos || (entry.IdentityHint != "" && live.FileIdentityHint != entry.IdentityHint)
			if hintsChanged {
				result.Stats.ChangedHints++
			}
			group = append(group, IndexedCandidate{SnapshotEntry: cloneEntry(entry), Observation: live.Observation, ResolvedPath: path, HintsChanged: hintsChanged})
		}
		if rootStale {
			result.Stats.StaleRoots++
			result.Stats.StaleLocators += len(grouped[rootID])
			addCandidateIssue(&result, limits, CandidateIssue{Code: "root_identity_changed", RootID: rootID, Count: len(grouped[rootID])})
			continue
		}
		result.Stats.StaleLocators += staleLocators
		result.Candidates = append(result.Candidates, group...)
	}
	sort.Slice(result.Candidates, func(i, j int) bool {
		return result.Candidates[i].Observation.SortKey() < result.Candidates[j].Observation.SortKey()
	})
	result.Stats.CandidatesRetained = len(result.Candidates)
	result.authorityDigest = candidateAuthorityDigest(result, profile)
	result.authoritySet = true
	return result, nil
}

func normalizeWantedSizes(values []int64) ([]int64, error) {
	result := append([]int64(nil), values...)
	for _, value := range result {
		if value < 0 {
			return nil, fmt.Errorf("wanted file size is negative")
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	compacted := result[:0]
	for _, value := range result {
		if len(compacted) == 0 || compacted[len(compacted)-1] != value {
			compacted = append(compacted, value)
		}
	}
	return compacted, nil
}

func wantedSize(sorted []int64, value int64) bool {
	index := sort.Search(len(sorted), func(index int) bool { return sorted[index] >= value })
	return index < len(sorted) && sorted[index] == value
}

func encodedPathBytes(components []string) (int64, error) {
	var result int64
	for _, component := range components {
		raw, err := base64.StdEncoding.Strict().DecodeString(component)
		if err != nil || base64.StdEncoding.EncodeToString(raw) != component {
			return 0, fmt.Errorf("storage index path component is invalid")
		}
		result += int64(len(raw))
	}
	return result, nil
}

func cloneEntry(entry Entry) Entry {
	entry.RelativeComponentsRawBase64 = append([]string(nil), entry.RelativeComponentsRawBase64...)
	return entry
}

func snapshotBindsDescriptor(header SnapshotHeader, footer SnapshotFooter, descriptor SnapshotDescriptor, profile Profile) bool {
	if header.SnapshotID != descriptor.ID || header.Generation != descriptor.Generation || header.ProfileID != descriptor.ProfileID ||
		header.ProfileRevision != descriptor.ProfileRevision || header.Platform != descriptor.Platform || header.PathEncoding != descriptor.PathEncoding ||
		!header.ObservedAtStart.Equal(descriptor.ObservedAtStart) || footer.SnapshotID != descriptor.ID || !footer.ObservedAtEnd.Equal(descriptor.ObservedAtEnd) ||
		footer.Files != descriptor.Files || footer.PathBytes != descriptor.PathBytes || !descriptorMatchesProfile(descriptor, profile) {
		return false
	}
	if len(header.RootIDs) != len(descriptor.Roots) || len(header.RootIDs) != len(profile.Roots) {
		return false
	}
	for index, rootID := range header.RootIDs {
		if descriptor.Roots[index].RootID != rootID || profile.Roots[index].ID != rootID {
			return false
		}
	}
	return true
}

func candidateRoots(profile Profile, descriptor SnapshotDescriptor) (map[string]storage.FullInventoryRoot, map[string]SnapshotRootObservation, error) {
	roots := make(map[string]storage.FullInventoryRoot, len(profile.Roots))
	observations := make(map[string]SnapshotRootObservation, len(descriptor.Roots))
	for _, profileRoot := range profile.Roots {
		path, err := profileRoot.Path()
		if err != nil {
			return nil, nil, err
		}
		roots[profileRoot.ID] = storage.FullInventoryRoot{ID: profileRoot.ID, Path: path}
	}
	for _, observation := range descriptor.Roots {
		observations[observation.RootID] = observation
	}
	if len(roots) != len(observations) {
		return nil, nil, fmt.Errorf("storage index root scope is inconsistent")
	}
	for rootID := range roots {
		if _, ok := observations[rootID]; !ok {
			return nil, nil, fmt.Errorf("storage index root scope is inconsistent")
		}
	}
	return roots, observations, nil
}

func addCandidateIssue(result *CandidateResult, limits CandidateLimits, issue CandidateIssue) {
	for index := range result.Issues {
		if result.Issues[index].Code == issue.Code && result.Issues[index].RootID == issue.RootID {
			result.Issues[index].Count += issue.Count
			return
		}
	}
	if len(result.Issues) < limits.MaxIssues {
		result.Issues = append(result.Issues, issue)
	} else {
		result.Stats.IssueOverflow += issue.Count
	}
}
