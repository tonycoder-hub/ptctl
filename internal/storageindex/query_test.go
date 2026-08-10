package storageindex

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadCandidatesReobservesLiveIdentityBoundFile(t *testing.T) {
	repository := testRepository(t)
	ctx := context.Background()
	root := t.TempDir()
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
	file, err := query.Candidates[0].Observation.OpenObservedRegularContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	value, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || string(value) != "payload" || query.Candidates[0].ResolvedPath != path {
		t.Fatalf("live candidate opener disagreed: value=%q read=%v close=%v path=%q", value, readErr, closeErr, query.Candidates[0].ResolvedPath)
	}
}

func TestLoadCandidatesRejectsReplacedProfileRoot(t *testing.T) {
	repository := testRepository(t)
	ctx := context.Background()
	parent := t.TempDir()
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
	root := t.TempDir()
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
