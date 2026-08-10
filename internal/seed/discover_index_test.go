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

func TestDiscoverFromIndexRejectsTamperedCandidateAuthority(t *testing.T) {
	ctx := context.Background()
	content := []byte("authority")
	meta := discoverV1SingleMeta(t, "source.bin", content)
	root := physicalSeedIndexTempDir(t)
	writeSeedFile(t, filepath.Join(root, "source.bin"), content)
	repository, profile, descriptorID := indexedFixture(t, root)
	query, err := repository.LoadCandidates(ctx, profile, descriptorID, []int64{int64(len(content))}, storageindex.DefaultCandidateLimits())
	if err != nil {
		t.Fatal(err)
	}
	query.SnapshotID += "-forged"
	if _, err := DiscoverFromIndex(ctx, meta, profile, query, defaultDiscoverOptions(nil, "")); err == nil {
		t.Fatal("tampered candidate DTO retained sealed-index authority")
	}
}

func TestDiscoverFromIndexExplicitMatchProducesBoundPlanWithoutClaimingUnique(t *testing.T) {
	ctx := context.Background()
	content := []byte("explicit-indexed-proof")
	meta := discoverV1SingleMeta(t, "source.bin", content)
	root := physicalSeedIndexTempDir(t)
	writeSeedFile(t, filepath.Join(root, "first.bin"), content)
	writeSeedFile(t, filepath.Join(root, "second.bin"), content)
	repository, profile, descriptorID := indexedFixture(t, root)
	query, err := repository.LoadCandidates(ctx, profile, descriptorID, []int64{int64(len(content))}, storageindex.DefaultCandidateLimits())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := DiscoverFromIndex(ctx, meta, profile, query, defaultDiscoverOptions(nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Matches) != 2 || preview.SourceOutcome != "incomplete" {
		t.Fatalf("expected two historical exact matches without authority: %#v", preview)
	}
	target := physicalSeedIndexTempDir(t)
	options := defaultDiscoverOptions(nil, target)
	options.ExplicitSourceMatchID = preview.Matches[1].ID
	selected, err := DiscoverFromIndex(ctx, meta, profile, query, options)
	if err != nil {
		t.Fatal(err)
	}
	if selected.SourceOutcome != "verified_selected" || selected.Selection.Status != "ready_explicit" ||
		selected.Selection.SelectedID != preview.Matches[1].ID || selected.Selection.ScopeID == "" ||
		selected.Handoff.Status != "ready" || selected.Plan == nil || !selected.Handoff.PlanProduced {
		t.Fatalf("explicit indexed selection did not become ready: %#v", selected)
	}
	if selected.Plan.SourceMode != "indexed_explicit_map" || selected.Plan.SourceSelectionID != selected.Selection.ScopeID {
		t.Fatalf("plan did not bind explicit index selection: %#v", selected.Plan)
	}
	if selected.SourceOutcome == "verified_unique" || hasDiscoveryBlocker(selected.Blockers, "source.multiple_verified_matches") {
		t.Fatalf("explicit choice was confused with uniqueness: %#v", selected)
	}
	rebuilt, err := selected.BuildMaterializePlan(ctx, meta, target, "copy")
	if err != nil || rebuilt.ID != selected.Plan.ID || rebuilt.SourceSelectionID != selected.Selection.ScopeID {
		t.Fatalf("private indexed authority did not reproduce reviewed plan: %#v %v", rebuilt, err)
	}
	public := selected.PublicReportCopy()
	if _, ok := public.VerifiedSource(meta); ok {
		t.Fatal("public indexed discovery copy retained process-local source authority")
	}
	if _, err := public.BuildMaterializePlan(ctx, meta, target, "copy"); err == nil {
		t.Fatal("public indexed discovery copy rebuilt an executable plan")
	}
}

func TestDiscoverFromIndexExplicitMatchAppliesTargetClientMapping(t *testing.T) {
	ctx := context.Background()
	content := []byte("explicit-indexed-target-mapping")
	meta := discoverV1SingleMeta(t, "source.bin", content)
	root := physicalSeedIndexTempDir(t)
	writeSeedFile(t, filepath.Join(root, "renamed.bin"), content)
	repository, profile, descriptorID := indexedFixture(t, root)
	query, err := repository.LoadCandidates(ctx, profile, descriptorID, []int64{int64(len(content))}, storageindex.DefaultCandidateLimits())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := DiscoverFromIndex(ctx, meta, profile, query, defaultDiscoverOptions(nil, ""))
	if err != nil || len(preview.Matches) != 1 {
		t.Fatalf("indexed preview failed: %#v %v", preview, err)
	}
	target := physicalSeedIndexTempDir(t)
	options := defaultDiscoverOptions(nil, target)
	options.ExplicitSourceMatchID = preview.Matches[0].ID
	options.ClientMapping = &ClientMappingOptions{HostRoot: target, ClientRoot: "/downloads"}
	selected, err := DiscoverFromIndex(ctx, meta, profile, query, options)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Handoff.Status != "ready" || !selected.Handoff.PlanProduced || selected.Plan == nil ||
		selected.Plan.ClientMapping != "lexical_only" || len(selected.Plan.Operations) != 1 || selected.Plan.Operations[0].ClientTarget != "/downloads/source.bin" {
		t.Fatalf("explicit indexed target mapping was not applied: %#v", selected)
	}

	blockedOptions := options
	blockedOptions.ClientMapping = &ClientMappingOptions{HostRoot: physicalSeedIndexTempDir(t), ClientRoot: "/downloads"}
	blocked, err := DiscoverFromIndex(ctx, meta, profile, query, blockedOptions)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Handoff.Status != "blocked" || !blocked.Handoff.PlanProduced || blocked.Plan == nil ||
		blocked.Plan.ClientMapping != "failed_outside_host_root" || !hasDiscoveryBlocker(blocked.Blockers, "mapping.target_outside_host_root") {
		t.Fatalf("explicit indexed target mapping failure was overstated: %#v", blocked)
	}
}

func TestExplicitIndexedMatchRemainsAuthorizedWhenOnlyOtherAlternativesHitBudget(t *testing.T) {
	ctx := context.Background()
	content := []byte("bounded-explicit-choice")
	meta := discoverV1SingleMeta(t, "source.bin", content)
	root := physicalSeedIndexTempDir(t)
	writeSeedFile(t, filepath.Join(root, "a.bin"), content)
	writeSeedFile(t, filepath.Join(root, "b.bin"), content)
	repository, profile, descriptorID := indexedFixture(t, root)
	query, err := repository.LoadCandidates(ctx, profile, descriptorID, []int64{int64(len(content))}, storageindex.DefaultCandidateLimits())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := DiscoverFromIndex(ctx, meta, profile, query, defaultDiscoverOptions(nil, ""))
	if err != nil || len(preview.Matches) != 2 {
		t.Fatalf("full preview failed: %#v %v", preview, err)
	}
	limitedPreviewOptions := defaultDiscoverOptions(nil, "")
	limitedPreviewOptions.MatchLimits.MaxVerifiedLayouts = 1
	limitedPreview, err := DiscoverFromIndex(ctx, meta, profile, query, limitedPreviewOptions)
	if err != nil || len(limitedPreview.Matches) != 1 || limitedPreview.Scan.VerificationComplete {
		t.Fatalf("bounded preview did not retain one deterministic exact assignment: %#v %v", limitedPreview, err)
	}
	options := defaultDiscoverOptions(nil, physicalSeedIndexTempDir(t))
	options.ExplicitSourceMatchID = limitedPreview.Matches[0].ID
	options.MatchLimits.MaxVerifiedLayouts = 1
	selected, err := DiscoverFromIndex(ctx, meta, profile, query, options)
	if err != nil {
		t.Fatal(err)
	}
	if selected.SourceOutcome != "verified_selected" || selected.Plan == nil || selected.Scan.VerificationComplete ||
		selected.Selection.SelectedID != limitedPreview.Matches[0].ID || len(selected.Matches) != 1 {
		t.Fatalf("budget on unselected alternatives erased exact explicit authority: %#v", selected)
	}
	if _, ok := selected.VerifiedSource(meta); !ok {
		t.Fatal("selected exact source lost private authority when only other alternatives were truncated")
	}
}

func TestDiscoverFromIndexExplicitMatchFailsClosedAfterLocatorDrift(t *testing.T) {
	ctx := context.Background()
	content := []byte("selected-before-drift")
	meta := discoverV1SingleMeta(t, "source.bin", content)
	root := physicalSeedIndexTempDir(t)
	path := filepath.Join(root, "selected.bin")
	writeSeedFile(t, path, content)
	repository, profile, descriptorID := indexedFixture(t, root)
	query, err := repository.LoadCandidates(ctx, profile, descriptorID, []int64{int64(len(content))}, storageindex.DefaultCandidateLimits())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := DiscoverFromIndex(ctx, meta, profile, query, defaultDiscoverOptions(nil, ""))
	if err != nil || len(preview.Matches) != 1 {
		t.Fatalf("initial indexed preview failed: %#v %v", preview, err)
	}
	if err := os.WriteFile(path, []byte("changed-after-review"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh, err := repository.LoadCandidates(ctx, profile, descriptorID, []int64{int64(len(content))}, storageindex.DefaultCandidateLimits())
	if err != nil {
		t.Fatal(err)
	}
	options := defaultDiscoverOptions(nil, physicalSeedIndexTempDir(t))
	options.ExplicitSourceMatchID = preview.Matches[0].ID
	selected, err := DiscoverFromIndex(ctx, meta, profile, fresh, options)
	if err != nil {
		t.Fatal(err)
	}
	if selected.SourceOutcome != "incomplete" || selected.Selection.Status != "blocked" || selected.Plan != nil ||
		!hasDiscoveryBlocker(selected.Blockers, "source.selected_match_unavailable") {
		t.Fatalf("drifted explicit locator retained authority: %#v", selected)
	}
}

func TestIndexedSelectionScopeChangesAcrossImmutableSnapshotGenerations(t *testing.T) {
	ctx := context.Background()
	content := []byte("same-live-object-new-index-generation")
	meta := discoverV1SingleMeta(t, "source.bin", content)
	root := physicalSeedIndexTempDir(t)
	writeSeedFile(t, filepath.Join(root, "selected.bin"), content)
	repository, profile, firstDescriptor := indexedFixture(t, root)
	target := physicalSeedIndexTempDir(t)
	firstQuery, err := repository.LoadCandidates(ctx, profile, firstDescriptor, []int64{int64(len(content))}, storageindex.DefaultCandidateLimits())
	if err != nil {
		t.Fatal(err)
	}
	firstPreview, err := DiscoverFromIndex(ctx, meta, profile, firstQuery, defaultDiscoverOptions(nil, ""))
	if err != nil || len(firstPreview.Matches) != 1 {
		t.Fatalf("first preview failed: %#v %v", firstPreview, err)
	}
	firstOptions := defaultDiscoverOptions(nil, target)
	firstOptions.ExplicitSourceMatchID = firstPreview.Matches[0].ID
	first, err := DiscoverFromIndex(ctx, meta, profile, firstQuery, firstOptions)
	if err != nil || first.Plan == nil {
		t.Fatalf("first selection failed: %#v %v", first, err)
	}

	now := time.Now().UTC().Add(time.Hour)
	secondRefresh, err := repository.Refresh(ctx, profile, storageindex.RefreshOptions{Clock: func() time.Time {
		now = now.Add(time.Second)
		return now
	}})
	if err != nil || secondRefresh.DescriptorRecord.ID == "" || secondRefresh.DescriptorRecord.ID == firstDescriptor {
		t.Fatalf("second generation failed: %#v %v", secondRefresh, err)
	}
	secondQuery, err := repository.LoadCandidates(ctx, profile, secondRefresh.DescriptorRecord.ID, []int64{int64(len(content))}, storageindex.DefaultCandidateLimits())
	if err != nil {
		t.Fatal(err)
	}
	secondOptions := defaultDiscoverOptions(nil, target)
	secondOptions.ExplicitSourceMatchID = firstPreview.Matches[0].ID
	second, err := DiscoverFromIndex(ctx, meta, profile, secondQuery, secondOptions)
	if err != nil || second.Plan == nil {
		t.Fatalf("second selection failed: %#v %v", second, err)
	}
	if first.Selection.SelectedID != second.Selection.SelectedID || first.Selection.ScopeID == second.Selection.ScopeID || first.Plan.ID == second.Plan.ID {
		t.Fatalf("immutable snapshot generation was not bound into selection/plan: first=%#v second=%#v", first.Selection, second.Selection)
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
