package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/storage"
	"github.com/tonycoder-hub/ptctl/internal/storageindex"
)

func TestStorageProfileRefreshAndSnapshotOnlyDiscoveryArePrivateAndFailClosed(t *testing.T) {
	stateRoot := filepath.Join(physicalCLITempDir(t), "private-state")
	if _, _, err := metastore.Init(stateRoot); err != nil {
		t.Fatal(err)
	}
	contentRoot := physicalCLITempDir(t)
	content := []byte("indexed-content")
	if err := os.WriteFile(filepath.Join(contentRoot, "renamed.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := Run([]string{
		"storage", "profile", "create", "--state-store", stateRoot, "--name", "media",
		"--search-root", contentRoot, "--output", "json",
	}, strings.NewReader(""), &out, &errOut)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("profile create: code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	assertJSONDoesNotContain(t, out.Bytes(), stateRoot, contentRoot)
	var profileEnvelope map[string]any
	if err := json.Unmarshal(out.Bytes(), &profileEnvelope); err != nil || profileEnvelope["kind"] != "storage.profile.create" {
		t.Fatalf("unexpected profile envelope: %#v err=%v", profileEnvelope, err)
	}

	out.Reset()
	errOut.Reset()
	code = Run([]string{
		"storage", "index", "refresh", "--state-store", stateRoot, "--profile", "media", "--output", "json",
	}, strings.NewReader(""), &out, &errOut)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("index refresh: code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	assertJSONDoesNotContain(t, out.Bytes(), stateRoot, contentRoot)
	var refreshEnvelope map[string]any
	if err := json.Unmarshal(out.Bytes(), &refreshEnvelope); err != nil || refreshEnvelope["kind"] != "storage.index.refresh" {
		t.Fatalf("unexpected refresh envelope: %#v err=%v", refreshEnvelope, err)
	}
	refreshData := refreshEnvelope["data"].(map[string]any)
	if refreshData["outcome"] != "stored" || refreshData["writes_performed"] != float64(2) {
		t.Fatalf("unexpected refresh result: %#v", refreshData)
	}

	torrentPath := filepath.Join(physicalCLITempDir(t), "source.torrent")
	if err := os.WriteFile(torrentPath, testV1Metafile("source.bin", content), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	code = Run([]string{
		"seed", "discover", "--torrent", torrentPath, "--state-store", stateRoot, "--storage-profile", "media",
		"--output", "json", "--require-verified",
	}, strings.NewReader(""), &out, &errOut)
	if code != 4 || !strings.Contains(errOut.String(), "not verified_unique") {
		t.Fatalf("snapshot discovery: code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	assertJSONDoesNotContain(t, out.Bytes(), stateRoot, contentRoot)
	var discoveryEnvelope map[string]any
	if err := json.Unmarshal(out.Bytes(), &discoveryEnvelope); err != nil {
		t.Fatal(err)
	}
	discovery := discoveryEnvelope["data"].(map[string]any)
	if discovery["source_outcome"] != "incomplete" || discovery["best_evidence"] != "verified" || discovery["writes_performed"] != float64(0) {
		t.Fatalf("snapshot-only semantics were overstated or erased: %#v", discovery)
	}
	if _, exists := discovery["plan"]; exists {
		t.Fatalf("snapshot-only discovery emitted a plan: %#v", discovery["plan"])
	}
}

func TestStorageIndexRefreshDiscoverProvesUniqueAndReportsPrivatePublication(t *testing.T) {
	ctx := t.Context()
	stateRoot := filepath.Join(physicalCLITempDir(t), "private-state")
	store, _, err := metastore.Init(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := storageindex.NewRepository(store, storageindex.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	contentRoot := physicalCLITempDir(t)
	content := []byte("refresh-discover-cli")
	sourcePath := filepath.Join(contentRoot, "renamed-private-source.bin")
	if err := os.WriteFile(sourcePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateProfile(ctx, "media", []string{contentRoot}, false, storageindex.DefaultScanLimits(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	torrentPath := filepath.Join(physicalCLITempDir(t), "source.torrent")
	if err := os.WriteFile(torrentPath, testV1Metafile("final.bin", content), 0o600); err != nil {
		t.Fatal(err)
	}
	targetRoot := physicalCLITempDir(t)

	var out, errOut bytes.Buffer
	code := Run([]string{
		"storage", "index", "refresh-discover", "--state-store", stateRoot, "--profile", "media",
		"--torrent", torrentPath, "--target", targetRoot, "--require-verified", "--output", "json",
	}, strings.NewReader(""), &out, &errOut)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("refresh-discover: code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	var envelope struct {
		Schema string               `json:"schema"`
		Kind   string               `json:"kind"`
		Data   seed.DiscoveryResult `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	result := envelope.Data
	if envelope.Schema != "ptctl.dev/v1" || envelope.Kind != "content.source_discovery" ||
		result.SourceOutcome != "verified_unique" || result.Selection.Status != "ready" || result.Plan == nil ||
		result.WritesPerformed != 2 || result.IndexRefresh == nil || result.IndexRefresh.Status != "stored" ||
		result.IndexRefresh.WritesPerformed != 2 || result.IndexRefresh.DataRecord.ID == "" ||
		result.IndexRefresh.DescriptorRecord.ID == "" || result.IndexRefresh.Assurance != "same_invocation_complete_generation_published_and_revalidated" ||
		result.IndexRefresh.CurrentSearchStatus != "complete" || result.IndexRefresh.CurrentSearchObservedAtEnd.IsZero() ||
		len(result.IndexRefresh.StopReasons) != 0 || len(result.IndexRefresh.CandidateStopReasons) != 0 {
		t.Fatalf("unexpected refresh-discover report: %s", out.String())
	}
	if strings.Contains(out.String(), `"stop_reasons": null`) || strings.Contains(out.String(), `"candidate_stop_reasons": null`) {
		t.Fatalf("empty refresh arrays encoded as null: %s", out.String())
	}
	assertJSONDoesNotContain(t, out.Bytes(), stateRoot, contentRoot, sourcePath, targetRoot, torrentPath)

	descriptors, err := store.ListRecords(ctx, metastore.RecordKindStorageIndexDescriptorV1, metastore.DefaultRecordLimits())
	if err != nil || len(descriptors.Records) != 1 || descriptors.Records[0].ID != result.IndexRefresh.DescriptorRecord.ID {
		t.Fatalf("descriptor publication mismatch: %#v err=%v", descriptors, err)
	}
	data, err := store.ListRecords(ctx, metastore.RecordKindStorageIndexDataV1, metastore.DefaultRecordLimits())
	if err != nil || len(data.Records) != 1 || data.Records[0].ID != result.IndexRefresh.DataRecord.ID {
		t.Fatalf("data publication mismatch: %#v err=%v", data, err)
	}
}

func TestStorageIndexRefreshDiscoverReportsNotFoundBeforeRequireExit(t *testing.T) {
	stateRoot := filepath.Join(physicalCLITempDir(t), "private-state")
	store, _, err := metastore.Init(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := storageindex.NewRepository(store, storageindex.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	contentRoot := physicalCLITempDir(t)
	if _, err := repository.CreateProfile(t.Context(), "empty", []string{contentRoot}, false, storageindex.DefaultScanLimits(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	torrentPath := filepath.Join(physicalCLITempDir(t), "absent.torrent")
	if err := os.WriteFile(torrentPath, testV1Metafile("absent.bin", []byte("not-present")), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{
		"storage", "index", "refresh-discover", "--state-store", stateRoot, "--profile", "empty",
		"--torrent", torrentPath, "--require-verified", "--output", "json",
	}, strings.NewReader(""), &out, &errOut)
	if code != 4 || !strings.Contains(errOut.String(), "not verified_unique") {
		t.Fatalf("not-found require exit: code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	var envelope struct {
		Data seed.DiscoveryResult `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.SourceOutcome != "not_found" || envelope.Data.WritesPerformed != 2 || envelope.Data.IndexRefresh == nil || envelope.Data.IndexRefresh.Status != "stored" {
		t.Fatalf("not-found report was lost before require exit: %s", out.String())
	}
}

func TestStorageIndexRefreshDiscoverManifestBudgetPerformsNoWrite(t *testing.T) {
	ctx := t.Context()
	stateRoot := filepath.Join(physicalCLITempDir(t), "private-state")
	store, _, err := metastore.Init(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := storageindex.NewRepository(store, storageindex.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	contentRoot := physicalCLITempDir(t)
	if _, err := repository.CreateProfile(ctx, "media", []string{contentRoot}, false, storageindex.DefaultScanLimits(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	torrentPath := filepath.Join(physicalCLITempDir(t), "bundle.torrent")
	if err := os.WriteFile(torrentPath, testV1MultiFileMetafile([]byte("abcdef")), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := store.ListRecords(ctx, metastore.RecordKindStorageIndexDescriptorV1, metastore.DefaultRecordLimits())
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{
		"storage", "index", "refresh-discover", "--state-store", stateRoot, "--profile", "media",
		"--torrent", torrentPath, "--max-states", "1", "--require-verified", "--output", "json",
	}, strings.NewReader(""), &out, &errOut)
	if code != 4 || !strings.Contains(errOut.String(), "not verified_unique") {
		t.Fatalf("manifest budget exit: code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	var envelope struct {
		Data seed.DiscoveryResult `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	result := envelope.Data
	if result.SourceOutcome != "incomplete" || result.WritesPerformed != 0 || result.IndexRefresh == nil ||
		result.IndexRefresh.Status != "not_started" || result.IndexRefresh.WritesPerformed != 0 ||
		result.Scan.PathConfinement != "not_started_manifest_state_budget" {
		t.Fatalf("manifest budget crossed write boundary: %s", out.String())
	}
	after, err := store.ListRecords(ctx, metastore.RecordKindStorageIndexDescriptorV1, metastore.DefaultRecordLimits())
	if err != nil || len(after.Records) != len(before.Records) {
		t.Fatalf("manifest budget published descriptor: before=%d after=%d err=%v", len(before.Records), len(after.Records), err)
	}
}

func TestSeedDiscoverStoredProfileUsageIsValidatedBeforeStoreRead(t *testing.T) {
	missing := filepath.Join(physicalCLITempDir(t), "missing-state")
	var out, errOut bytes.Buffer
	code := Run([]string{
		"seed", "discover", "--torrent", "missing.torrent", "--state-store", missing,
		"--storage-profile", "media", "--search-root", physicalCLITempDir(t),
	}, strings.NewReader(""), &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "mutually exclusive") || strings.Contains(errOut.String(), missing) {
		t.Fatalf("usage validation touched private state: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestIndexedExplicitSelectionPlansAndMaterializesWithoutClaimingUnique(t *testing.T) {
	ctx := t.Context()
	stateRoot := filepath.Join(physicalCLITempDir(t), "private-state")
	store, _, err := metastore.Init(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := storageindex.NewRepository(store, storageindex.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	contentRoot := physicalCLITempDir(t)
	content := []byte("indexed-explicit-materialize")
	selectedPath := filepath.Join(contentRoot, "renamed-selected.bin")
	if err := os.WriteFile(selectedPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	profileReceipt, err := repository.CreateProfile(ctx, "media", []string{contentRoot}, false, storageindex.DefaultScanLimits(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := repository.Refresh(ctx, profileReceipt.Profile, storageindex.RefreshOptions{})
	if err != nil || refresh.DescriptorRecord.ID == "" {
		t.Fatalf("refresh failed: %#v %v", refresh, err)
	}
	descriptorID := refresh.DescriptorRecord.ID.String()
	torrentPath := filepath.Join(physicalCLITempDir(t), "source.torrent")
	if err := os.WriteFile(torrentPath, testV1Metafile("selected-final.bin", content), 0o600); err != nil {
		t.Fatal(err)
	}
	targetRoot := physicalCLITempDir(t)

	var out, errOut bytes.Buffer
	base := []string{
		"--torrent", torrentPath, "--state-store", stateRoot, "--storage-profile", "media",
		"--snapshot-record", descriptorID, "--output", "json",
	}
	previewArgs := append([]string{"seed", "discover"}, base...)
	if code := Run(previewArgs, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("indexed preview code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var previewEnvelope struct {
		Data seed.DiscoveryResult `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &previewEnvelope); err != nil {
		t.Fatal(err)
	}
	if previewEnvelope.Data.SourceOutcome != "incomplete" || len(previewEnvelope.Data.Matches) != 1 || previewEnvelope.Data.Plan != nil {
		t.Fatalf("unselected snapshot gained authority: %s", out.String())
	}
	matchID := previewEnvelope.Data.Matches[0].ID

	out.Reset()
	errOut.Reset()
	selectedArgs := append([]string{"seed", "discover"}, base...)
	selectedArgs = append(selectedArgs, "--select-source-match", matchID, "--target", targetRoot, "--require-verified")
	if code := Run(selectedArgs, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("selected preview code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var selectedEnvelope struct {
		Data seed.DiscoveryResult `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &selectedEnvelope); err != nil {
		t.Fatal(err)
	}
	selected := selectedEnvelope.Data
	if selected.SourceOutcome != "verified_selected" || selected.Selection.Status != "ready_explicit" ||
		selected.Selection.ScopeID == "" || selected.Plan == nil || selected.Plan.SourceMode != "indexed_explicit_map" ||
		selected.Plan.SourceSelectionID != selected.Selection.ScopeID {
		t.Fatalf("selected preview did not bind plan: %s", out.String())
	}
	planID := selected.Plan.ID
	assertJSONDoesNotContain(t, out.Bytes(), stateRoot, contentRoot, selectedPath, targetRoot, torrentPath)

	wrongMatch := "sha256:" + strings.Repeat("0", 64)
	if wrongMatch == matchID {
		wrongMatch = "sha256:" + strings.Repeat("1", 64)
	}
	out.Reset()
	errOut.Reset()
	wrongArgs := []string{
		"seed", "materialize", "run", "--torrent", torrentPath,
		"--state-store", stateRoot, "--storage-profile", "media", "--snapshot-record", descriptorID,
		"--select-source-match", wrongMatch, "--target", targetRoot, "--expect-plan-id", planID,
		"--acknowledge-filesystem-write", "--output", "json",
	}
	if code := Run(wrongArgs, strings.NewReader(""), &out, &errOut); code != 4 {
		t.Fatalf("unknown selected match code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	blocked := decodeMaterializeReport(t, out.Bytes())
	if blocked.Data.Outcome != materialize.OutcomeBlocked || blocked.Data.WritesPerformed != 0 {
		t.Fatalf("unknown selected match crossed write boundary: %s", out.String())
	}
	entries, err := os.ReadDir(targetRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unknown selected match changed target: entries=%d err=%v", len(entries), err)
	}

	// A new exact alternative after snapshot capture proves why this mode must
	// remain explicit selection rather than a current-filesystem uniqueness
	// claim. It does not invalidate the reviewed exact selected source map.
	if err := os.WriteFile(filepath.Join(contentRoot, "new-unindexed-alternative.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	runArgs := []string{
		"seed", "materialize", "run", "--torrent", torrentPath,
		"--state-store", stateRoot, "--storage-profile", "media", "--snapshot-record", descriptorID,
		"--select-source-match", matchID, "--target", targetRoot, "--expect-plan-id", planID,
		"--acknowledge-filesystem-write", "--output", "json",
	}
	if code := Run(runArgs, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("indexed materialize code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	report := decodeMaterializeReport(t, out.Bytes())
	if report.Data.Outcome != materialize.OutcomeMaterializedVerified || report.Data.Source.Outcome != "verified_selected" ||
		report.Data.Source.Mode != "indexed_explicit_live_reverification" || !report.Data.Source.ContentVerified ||
		!report.Data.Plan.Matches || report.Data.Plan.ObservedID != planID {
		t.Fatalf("indexed materialize overstated or lost authority: %s", out.String())
	}
	final, err := os.ReadFile(filepath.Join(targetRoot, "selected-final.bin"))
	if err != nil || !bytes.Equal(final, content) {
		t.Fatalf("indexed selected materialization bytes=%q err=%v", final, err)
	}
	assertJSONDoesNotContain(t, out.Bytes(), stateRoot, contentRoot, selectedPath, targetRoot, torrentPath)
}

func TestMaterializeIndexedSelectorUsagePrecedesStateAndTargetIO(t *testing.T) {
	missingState := filepath.Join(physicalCLITempDir(t), "MUST-NOT-READ-STATE")
	targetRoot := physicalCLITempDir(t)
	before, err := os.ReadDir(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{
		"seed", "materialize", "run", "--torrent", "missing.torrent",
		"--state-store", missingState, "--storage-profile", "media",
		"--target", targetRoot, "--expect-plan-id", strings.Repeat("1", 24),
		"--acknowledge-filesystem-write",
	}, strings.NewReader(""), &out, &errOut)
	if code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), "snapshot-record") || strings.Contains(errOut.String(), missingState) {
		t.Fatalf("incomplete indexed selector crossed I/O boundary: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	after, err := os.ReadDir(targetRoot)
	if err != nil || len(after) != len(before) {
		t.Fatalf("bad indexed usage changed target namespace: before=%d after=%d err=%v", len(before), len(after), err)
	}
}

func TestReconcileStoredProfileCannotBecomeConsistentWithoutCurrentSearchCompleteness(t *testing.T) {
	stateRoot := filepath.Join(physicalCLITempDir(t), "private-state")
	if _, _, err := metastore.Init(stateRoot); err != nil {
		t.Fatal(err)
	}
	contentRoot := physicalCLITempDir(t)
	content := []byte("reconcile-index")
	if err := os.WriteFile(filepath.Join(contentRoot, "copy.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"storage", "profile", "create", "--state-store", stateRoot, "--name", "media", "--search-root", contentRoot}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("profile create failed: %d %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"storage", "index", "refresh", "--state-store", stateRoot, "--profile", "media"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("index refresh failed: %d %s", code, errOut.String())
	}
	torrentPath := filepath.Join(physicalCLITempDir(t), "source.torrent")
	if err := os.WriteFile(torrentPath, testV1Metafile("source.bin", content), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	code := Run([]string{
		"reconcile", "report", "--torrent", torrentPath, "--state-store", stateRoot, "--storage-profile", "media",
		"--output", "json", "--require-reconciled",
	}, strings.NewReader(""), &out, &errOut)
	if code != 4 || !strings.Contains(errOut.String(), "not consistent") {
		t.Fatalf("reconcile index outcome: code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	assertJSONDoesNotContain(t, out.Bytes(), stateRoot, contentRoot)
	var reportEnvelope map[string]any
	if err := json.Unmarshal(out.Bytes(), &reportEnvelope); err != nil {
		t.Fatal(err)
	}
	report := reportEnvelope["data"].(map[string]any)
	if report["outcome"] == "consistent" || report["writes_performed"] != float64(0) {
		t.Fatalf("historical index upgraded reconciliation: %#v", report)
	}
}

func TestReconcileStoredProfilePreflightsStateBeforePasswordRead(t *testing.T) {
	torrentPath := filepath.Join(physicalCLITempDir(t), "source.torrent")
	if err := os.WriteFile(torrentPath, testV1Metafile("source.bin", []byte("x")), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := &trackingReader{}
	var out, errOut bytes.Buffer
	code := Run([]string{
		"reconcile", "report", "--torrent", torrentPath,
		"--state-store", filepath.Join(physicalCLITempDir(t), "missing"), "--storage-profile", "media",
		"--driver", "qbittorrent", "--url", "https://seedbox.invalid", "--username", "alice", "--password-stdin",
	}, reader, &out, &errOut)
	if code == 0 || reader.read {
		t.Fatalf("private state failure consumed downloader credential: code=%d stdout=%q stderr=%q read=%t", code, out.String(), errOut.String(), reader.read)
	}
}

func TestReconcileRejectsInvalidLiveProfileBeforeCredentialOrClientRequest(t *testing.T) {
	stateRoot := filepath.Join(physicalCLITempDir(t), "private-state")
	store, _, err := metastore.Init(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := storageindex.NewProfile("relative", []string{physicalCLITempDir(t)}, false, storageindex.DefaultScanLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	profile.Roots[0].PathRawBase64 = base64.StdEncoding.EncodeToString([]byte("relative-root"))
	profile = rebindCLIStorageProfile(t, profile)
	raw, err := storageindex.EncodeProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ImportRecord(t.Context(), metastore.RecordKindStorageProfileV1, bytes.NewReader(raw), metastore.DefaultRecordLimits()); err != nil {
		t.Fatal(err)
	}

	torrentPath := filepath.Join(physicalCLITempDir(t), "source.torrent")
	if err := os.WriteFile(torrentPath, testV1Metafile("source.bin", []byte("x")), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	reader := &trackingReader{}
	var out, errOut bytes.Buffer
	code := Run([]string{
		"reconcile", "report", "--torrent", torrentPath,
		"--state-store", stateRoot, "--storage-profile", "relative",
		"--driver", "qbittorrent", "--url", server.URL, "--username", "alice", "--password-stdin",
	}, reader, &out, &errOut)
	if code == 0 || reader.read || requests.Load() != 0 {
		t.Fatalf("invalid live profile crossed credential/network preflight: code=%d read=%t requests=%d stdout=%q stderr=%q", code, reader.read, requests.Load(), out.String(), errOut.String())
	}
}

func TestStorageIndexRefreshPublicReportDropsRelativeInventoryPaths(t *testing.T) {
	const secret = "CANARY-private-relative-name.bin"
	input := storageindex.RefreshResult{
		Status: "incomplete",
		Scan: storage.FullInventoryResult{
			Roots:       []storage.FullInventoryRootObservation{},
			Issues:      []storage.ScanIssue{{Code: "scan.file_changed", RootID: "root:test", RelativePath: secret, Message: "regular file changed"}},
			LimitHits:   []string{},
			StopReasons: []string{},
			Warnings:    []string{},
		},
		StopReasons: []string{},
	}
	report := publicStorageIndexRefresh(input)
	if report.Scan.Issues[0].RelativePath != "" {
		t.Fatalf("public refresh retained private relative path: %#v", report.Scan.Issues[0])
	}
	if input.Scan.Issues[0].RelativePath != secret {
		t.Fatal("public refresh mutated the private internal result")
	}
	var output bytes.Buffer
	if err := writeStorageIndexReport(&output, "json", report); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), secret) {
		t.Fatalf("JSON leaked private relative path: %s", output.String())
	}
}

func TestStorageProfileCreateRejectsUnrepresentableScanPolicyBeforeStoreRead(t *testing.T) {
	for name, option := range map[string][]string{
		"files":      {"--max-files", "100001"},
		"path bytes": {"--max-path-bytes", "16777217"},
		"components": {"--max-depth", "64"},
	} {
		t.Run(name, func(t *testing.T) {
			missingStore := filepath.Join(physicalCLITempDir(t), "must-not-be-read")
			args := []string{"storage", "profile", "create", "--state-store", missingStore, "--name", "media", "--search-root", physicalCLITempDir(t)}
			args = append(args, option...)
			var out, errOut bytes.Buffer
			code := Run(args, strings.NewReader(""), &out, &errOut)
			if code != 2 || !strings.Contains(errOut.String(), "exceeds repository index limits") || strings.Contains(errOut.String(), missingStore) {
				t.Fatalf("incompatible profile policy crossed usage preflight: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
			}
		})
	}
}

func TestStorageProfileAndIndexSubcommandHelpIsDiscoverable(t *testing.T) {
	commands := [][]string{
		{"storage", "profile", "create", "--help"},
		{"storage", "profile", "inspect", "--help"},
		{"storage", "index", "refresh", "--help"},
		{"storage", "index", "refresh-discover", "--help"},
		{"storage", "index", "inspect", "--help"},
	}
	for _, command := range commands {
		var out, errOut bytes.Buffer
		code := Run(command, strings.NewReader(""), &out, &errOut)
		if code != 0 || errOut.Len() != 0 || !strings.Contains(out.String(), "Usage:") || !strings.Contains(out.String(), "Flags:") {
			t.Fatalf("help contract failed for %v: code=%d stdout=%q stderr=%q", command, code, out.String(), errOut.String())
		}
	}
}

func assertJSONDoesNotContain(t *testing.T, raw []byte, secrets ...string) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if decodedContainsString(decoded, secret) {
			t.Fatalf("JSON leaked private absolute path %q: %s", secret, raw)
		}
	}
}

func decodedContainsString(value any, secret string) bool {
	switch typed := value.(type) {
	case string:
		return strings.Contains(typed, secret)
	case []any:
		for _, item := range typed {
			if decodedContainsString(item, secret) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if decodedContainsString(item, secret) {
				return true
			}
		}
	}
	return false
}

func rebindCLIStorageProfile(t *testing.T, profile storageindex.Profile) storageindex.Profile {
	t.Helper()
	paths := make([]string, len(profile.Roots))
	for index := range profile.Roots {
		paths[index] = profile.Roots[index].PathRawBase64
	}
	sort.Strings(paths)
	limits := profile.ScanLimits
	declaration := strings.Join([]string{
		storageindex.ProfileFormat, runtime.GOOS, "native_components_base64", "one_filesystem", fmt.Sprint(profile.AllowNetwork),
		fmt.Sprint(limits.MaxDepth), fmt.Sprint(limits.MaxDirectories), fmt.Sprint(limits.MaxEntries),
		fmt.Sprint(limits.MaxEntriesPerDirectory), fmt.Sprint(limits.MaxFiles), fmt.Sprint(limits.MaxPathBytes), fmt.Sprint(limits.MaxIssues),
		strings.Join(paths, "\x00"),
	}, "\x00")
	profileDigest := sha256.Sum256([]byte("ptctl-storage-profile-declaration-v1\x00" + declaration))
	revisionDigest := sha256.Sum256([]byte("ptctl-storage-profile-revision-v1\x00" + declaration))
	profile.Platform = runtime.GOOS
	profile.ID = "profile:" + hex.EncodeToString(profileDigest[:])
	profile.Revision = "revision:" + hex.EncodeToString(revisionDigest[:])
	for index := range profile.Roots {
		digest := sha256.Sum256([]byte("ptctl-storage-profile-root-v1\x00" + profile.ID + "\x00" + profile.Roots[index].PathRawBase64))
		profile.Roots[index].ID = "root:" + hex.EncodeToString(digest[:])
	}
	sort.Slice(profile.Roots, func(i, j int) bool { return profile.Roots[i].ID < profile.Roots[j].ID })
	return profile
}
