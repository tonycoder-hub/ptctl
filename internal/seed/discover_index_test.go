package seed

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/storageindex"
)

func TestDiscoverFromIndexPreservesExactMatchButBlocksUniqueSelectionAndPlan(t *testing.T) {
	ctx := context.Background()
	content := []byte("indexed-proof")
	meta := discoverV1SingleMeta(t, "source.bin", content)
	root := physicalSeedIndexTempDir(t)
	writeSeedFile(t, filepath.Join(root, "renamed.bin"), content)
	repository, profile, descriptorID := indexedFixture(t, root)
	query, err := repository.LoadCandidates(ctx, profile, descriptorID, []int64{int64(len(content))}, storageindex.DefaultCandidateLimits())
	if err != nil {
		t.Fatal(err)
	}
	options := defaultDiscoverOptions(nil, physicalSeedIndexTempDir(t))
	result, err := DiscoverFromIndex(ctx, meta, profile, query, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceOutcome != "incomplete" || result.Selection.Status != "blocked" || result.Handoff.Status != "blocked" || result.Plan != nil || result.BestEvidence != "verified" || len(result.Matches) != 1 {
		t.Fatalf("snapshot-only match was overstated or erased: %#v", result)
	}
	if !hasDiscoveryBlocker(result.Blockers, "index.historical_scope") || !hasDiscoveryBlocker(result.Blockers, "handoff.current_uniqueness_required") {
		t.Fatalf("historical completeness blockers are missing: %#v", result.Blockers)
	}
	if _, err := result.Matches[0].Verification.MatchSourceSnapshot(filepath.Join(root, "renamed.bin")); err == nil {
		t.Fatal("snapshot-only public match retained a process-local path oracle")
	}
}

func TestDiscoverFromIndexZeroLiveCandidatesNeverClaimsNotFound(t *testing.T) {
	ctx := context.Background()
	content := []byte("gone")
	meta := discoverV1SingleMeta(t, "source.bin", content)
	root := physicalSeedIndexTempDir(t)
	path := filepath.Join(root, "old.bin")
	writeSeedFile(t, path, content)
	repository, profile, descriptorID := indexedFixture(t, root)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	query, err := repository.LoadCandidates(ctx, profile, descriptorID, []int64{int64(len(content))}, storageindex.DefaultCandidateLimits())
	if err != nil {
		t.Fatal(err)
	}
	result, err := DiscoverFromIndex(ctx, meta, profile, query, defaultDiscoverOptions(nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceOutcome != "incomplete" || result.BestEvidence != "none" || len(result.Matches) != 0 || !hasDiscoveryBlocker(result.Blockers, "source.no_verified_index_candidate") {
		t.Fatalf("stale empty index result became a not-found proof: %#v", result)
	}
}

func indexedFixture(t *testing.T, root string) (*storageindex.Repository, storageindex.Profile, metastore.RecordID) {
	t.Helper()
	store, _, err := metastore.Init(filepath.Join(physicalSeedIndexTempDir(t), "state"))
	if err != nil {
		t.Fatal(err)
	}
	repository, err := storageindex.NewRepository(store, storageindex.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	profileReceipt, err := repository.CreateProfile(context.Background(), "media", []string{root}, false, storageindex.DefaultScanLimits(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	refresh, err := repository.Refresh(context.Background(), profileReceipt.Profile, storageindex.RefreshOptions{Clock: func() time.Time {
		now = now.Add(time.Second)
		return now
	}})
	if err != nil || refresh.DescriptorRecord.ID == "" {
		t.Fatalf("index refresh failed: result=%#v err=%v", refresh, err)
	}
	return repository, profileReceipt.Profile, refresh.DescriptorRecord.ID
}

func physicalSeedIndexTempDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatalf("resolve temporary directory: %v", err)
	}
	return resolved
}
