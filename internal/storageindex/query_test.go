package storageindex

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadCandidatesReobservesLiveIdentityBoundFile(t *testing.T) {
	repository := testRepository(t)
	ctx := context.Background()
	root := physicalIndexTempDir(t)
	path := filepath.Join(root, "renamed.bin")
	writeTestFile(t, path, []byte("payload"))
	profileReceipt, err := repository.CreateProfile(ctx, "media", []string{root}, false, DefaultScanLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := repository.Refresh(ctx, profileReceipt.Profile, RefreshOptions{Clock: deterministicClock(time.Now().UTC())})
	if err != nil {
		t.Fatal(err)
	}
	query, err := repository.LoadCandidates(ctx, profileReceipt.Profile, refresh.DescriptorRecord.ID, []int64{7}, DefaultCandidateLimits())
	if err != nil || !query.Complete || !query.HistoricalSnapshotVerified || query.CurrentSearchComplete || len(query.Candidates) != 1 {
		t.Fatalf("candidate query failed: query=%#v err=%v", query, err)
	}
	if !query.HasLiveAuthority(profileReceipt.Profile) {
		t.Fatal("fresh candidate query did not retain process-local authority")
	}
	file, err := query.Candidates[0].Observation.OpenObservedRegularContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	value, readErr := io.ReadAll(file)
	closeErr := file.Close()
	expectedInfo, expectedErr := os.Stat(path)
	resolvedInfo, resolvedErr := os.Stat(query.Candidates[0].ResolvedPath)
	if readErr != nil || closeErr != nil || string(value) != "payload" || expectedErr != nil || resolvedErr != nil || !os.SameFile(expectedInfo, resolvedInfo) {
		t.Fatalf("live candidate opener disagreed: value=%q read=%v close=%v path=%q", value, readErr, closeErr, query.Candidates[0].ResolvedPath)
	}
}

func TestCandidateResultAuthorityRejectsDTOReplayAndMutation(t *testing.T) {
	repository := testRepository(t)
	ctx := context.Background()
	root := physicalIndexTempDir(t)
	writeTestFile(t, filepath.Join(root, "selected.bin"), []byte("payload"))
	profileReceipt, err := repository.CreateProfile(ctx, "media", []string{root}, false, DefaultScanLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := repository.Refresh(ctx, profileReceipt.Profile, RefreshOptions{Clock: deterministicClock(time.Now().UTC())})
	if err != nil {
		t.Fatal(err)
	}
	query, err := repository.LoadCandidates(ctx, profileReceipt.Profile, refresh.DescriptorRecord.ID, []int64{7}, DefaultCandidateLimits())
	if err != nil || !query.HasLiveAuthority(profileReceipt.Profile) {
		t.Fatalf("candidate query authority failed: %#v %v", query, err)
	}

	mutations := map[string]func(*CandidateResult){
		"profile":    func(value *CandidateResult) { value.ProfileID += "-changed" },
		"snapshot":   func(value *CandidateResult) { value.SnapshotID += "-changed" },
		"descriptor": func(value *CandidateResult) { value.DescriptorRecordID = value.DataRecordID },
		"accounting": func(value *CandidateResult) { value.Stats.CandidatesRetained++ },
		"warning": func(value *CandidateResult) {
			value.Warnings = append(append([]string(nil), value.Warnings...), "injected")
		},
		"locator": func(value *CandidateResult) {
			value.Candidates = append([]IndexedCandidate(nil), value.Candidates...)
			value.Candidates[0].ResolvedPath += "-changed"
		},
		"observation": func(value *CandidateResult) {
			value.Candidates = append([]IndexedCandidate(nil), value.Candidates...)
			value.Candidates[0].Observation.ObservationID += "-changed"
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			tampered := query
			mutate(&tampered)
			if tampered.HasLiveAuthority(profileReceipt.Profile) {
				t.Fatal("mutated candidate result retained authority")
			}
		})
	}

	raw, err := json.Marshal(query)
	if err != nil {
		t.Fatal(err)
	}
	var replayed CandidateResult
	if err := json.Unmarshal(raw, &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.HasLiveAuthority(profileReceipt.Profile) {
		t.Fatal("serialized candidate result recovered process-local authority")
	}
	changedProfile := profileReceipt.Profile
	changedProfile.Revision += "-changed"
	if query.HasLiveAuthority(changedProfile) {
		t.Fatal("candidate result crossed immutable profile revision")
	}
}

func TestLoadCandidatesRejectsReplacedProfileRoot(t *testing.T) {
	repository := testRepository(t)
	ctx := context.Background()
	parent := physicalIndexTempDir(t)
	root := filepath.Join(parent, "root")
	writeTestFile(t, filepath.Join(root, "file.bin"), []byte("same"))
	profileReceipt, err := repository.CreateProfile(ctx, "media", []string{root}, false, DefaultScanLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := repository.Refresh(ctx, profileReceipt.Profile, RefreshOptions{Clock: deterministicClock(time.Now().UTC())})
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(parent, "old-root")
	if err := os.Rename(root, old); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "file.bin"), []byte("same"))
	query, err := repository.LoadCandidates(ctx, profileReceipt.Profile, refresh.DescriptorRecord.ID, []int64{4}, DefaultCandidateLimits())
	if err != nil || !query.HistoricalSnapshotVerified || len(query.Candidates) != 0 || query.Stats.StaleRoots != 1 {
		t.Fatalf("replaced root was retained: query=%#v err=%v", query, err)
	}
}

func TestLoadCandidatesCandidateBudgetIsNPlusOneAndStillVerifiesSnapshot(t *testing.T) {
	repository := testRepository(t)
	ctx := context.Background()
	root := physicalIndexTempDir(t)
	writeTestFile(t, filepath.Join(root, "a"), []byte("x"))
	writeTestFile(t, filepath.Join(root, "b"), []byte("y"))
	profileReceipt, err := repository.CreateProfile(ctx, "media", []string{root}, false, DefaultScanLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := repository.Refresh(ctx, profileReceipt.Profile, RefreshOptions{Clock: deterministicClock(time.Now().UTC())})
	if err != nil {
		t.Fatal(err)
	}
	limits := DefaultCandidateLimits()
	limits.MaxCandidates = 1
	query, err := repository.LoadCandidates(ctx, profileReceipt.Profile, refresh.DescriptorRecord.ID, []int64{1}, limits)
	if err != nil || query.Complete || !query.HistoricalSnapshotVerified || query.Stats.SizeMatchesConsidered != 2 || len(query.Candidates) != 1 || !containsString(query.StopReasons, "max_candidates") {
		t.Fatalf("candidate budget contract failed: query=%#v err=%v", query, err)
	}
}

func TestRefreshAndLoadCandidatesBindsOnlySameInvocationCompleteGeneration(t *testing.T) {
	repository := testRepository(t)
	ctx := context.Background()
	root := physicalIndexTempDir(t)
	writeTestFile(t, filepath.Join(root, "wanted.bin"), []byte("payload"))
	writeTestFile(t, filepath.Join(root, "other.bin"), []byte("other"))
	profileReceipt, err := repository.CreateProfile(ctx, "media", []string{root}, false, DefaultScanLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	refresh, candidates, err := repository.RefreshAndLoadCandidates(ctx, profileReceipt.Profile, []int64{7}, DefaultCandidateLimits(), RefreshOptions{Clock: deterministicClock(time.Now().UTC())})
	if err != nil || !refresh.HasLiveAuthority(profileReceipt.Profile) || !candidates.HasLiveAuthority(profileReceipt.Profile) ||
		!candidates.CurrentSearchComplete || candidates.CurrentSearch == nil || candidates.CurrentSearch.Status != "complete" || len(candidates.Candidates) != 1 ||
		candidates.DescriptorRecordID != refresh.DescriptorRecord.ID || candidates.DataRecordID != refresh.DataRecord.ID {
		t.Fatalf("same-invocation generation was not bound: refresh=%#v candidates=%#v err=%v", refresh, candidates, err)
	}

	refreshRaw, err := json.Marshal(refresh)
	if err != nil {
		t.Fatal(err)
	}
	var replayedRefresh RefreshResult
	if err := json.Unmarshal(refreshRaw, &replayedRefresh); err != nil {
		t.Fatal(err)
	}
	if replayedRefresh.HasLiveAuthority(profileReceipt.Profile) {
		t.Fatal("serialized refresh recovered process-local authority")
	}
	candidateRaw, err := json.Marshal(candidates)
	if err != nil {
		t.Fatal(err)
	}
	var replayedCandidates CandidateResult
	if err := json.Unmarshal(candidateRaw, &replayedCandidates); err != nil {
		t.Fatal(err)
	}
	if replayedCandidates.HasLiveAuthority(profileReceipt.Profile) {
		t.Fatal("serialized current candidates recovered process-local authority")
	}

	historical, err := repository.LoadCandidates(ctx, profileReceipt.Profile, refresh.DescriptorRecord.ID, []int64{7}, DefaultCandidateLimits())
	if err != nil || !historical.HasLiveAuthority(profileReceipt.Profile) || historical.CurrentSearchComplete || historical.CurrentSearch != nil {
		t.Fatalf("later sealed read did not lose current completeness: %#v err=%v", historical, err)
	}
}

func TestRefreshAndLoadCandidatesBudgetNeverClaimsCurrentCompleteness(t *testing.T) {
	repository := testRepository(t)
	ctx := context.Background()
	root := physicalIndexTempDir(t)
	writeTestFile(t, filepath.Join(root, "a.bin"), []byte("x"))
	writeTestFile(t, filepath.Join(root, "b.bin"), []byte("y"))
	profileReceipt, err := repository.CreateProfile(ctx, "media", []string{root}, false, DefaultScanLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	limits := DefaultCandidateLimits()
	limits.MaxCandidates = 1
	refresh, candidates, err := repository.RefreshAndLoadCandidates(ctx, profileReceipt.Profile, []int64{1}, limits, RefreshOptions{Clock: deterministicClock(time.Now().UTC())})
	if err != nil || !refresh.HasLiveAuthority(profileReceipt.Profile) || !candidates.HasLiveAuthority(profileReceipt.Profile) ||
		candidates.CurrentSearchComplete || candidates.CurrentSearch == nil || candidates.CurrentSearch.Status != "incomplete" ||
		!containsString(candidates.StopReasons, "max_candidates") || !containsString(candidates.StopReasons, "current_refresh_changed_before_candidate_verification") {
		t.Fatalf("truncated candidates gained current completeness: refresh=%#v candidates=%#v err=%v", refresh, candidates, err)
	}
}
