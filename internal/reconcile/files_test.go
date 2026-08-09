package reconcile

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

func TestMultiFileLayoutClosesClientStorageRelation(t *testing.T) {
	meta, discovery, source, hostRoot := reconciledMultiFile(t)
	job, before, after, filesBefore, filesAfter, limits := stableMultiFileBracket(meta)
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client: ClientBracket{
			Requested: true, Before: &before, After: &after, RequestsMade: 5,
			FileLayoutMode: "auto", FileLimits: limits, FileAttempted: true, FileRequestsMade: 2,
			FilesBefore: &filesBefore, FilesAfter: &filesAfter,
		},
		PathMapping: &PathMappingOptions{HostRoot: hostRoot, ClientRoot: "/downloads"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "consistent" || relationStatus(report, "verified_source_vs_job_path") != "same_location" {
		t.Fatalf("ordinary multi-file layout did not reconcile: %#v", report)
	}
	ledger := report.Ledgers.Downloader.FileLayout
	if ledger.Status != "observed_stable" || ledger.RequestsMade != 2 || ledger.FilesExpected != 2 || ledger.FilesObserved != 2 || ledger.FilesSelected != 2 || ledger.FilesComplete != 2 {
		t.Fatalf("unexpected file ledger: %#v", ledger)
	}
	if report.Scope.ClientFileLayoutMode != "auto" || !containsString(report.Effect, "read_downloader_file_layout") || !strings.Contains(report.Assurance, "bracketed_per_file") {
		t.Fatalf("file-layout scope or assurance missing: %#v", report)
	}
	if job.Hash == "" {
		t.Fatal("test job unexpectedly has no opaque key")
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{hostRoot, "/downloads/bundle", job.Hash} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("private file-ledger value leaked: %s", encoded)
		}
	}
}

func TestMultiFileSnapshotMutationIsIncomplete(t *testing.T) {
	meta, discovery, source, hostRoot := reconciledMultiFile(t)
	_, before, after, filesBefore, filesAfter, limits := stableMultiFileBracket(meta)
	filesAfter.Files[1].RelativeComponents[1] = "shifted-two"
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client: ClientBracket{
			Requested: true, Before: &before, After: &after, RequestsMade: 5,
			FileLayoutMode: "auto", FileLimits: limits, FileAttempted: true, FileRequestsMade: 2,
			FilesBefore: &filesBefore, FilesAfter: &filesAfter,
		},
		PathMapping: &PathMappingOptions{HostRoot: hostRoot, ClientRoot: "/downloads"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "incomplete" || report.Ledgers.Downloader.FileLayout.Status != "unstable" || relationStatus(report, "verified_source_vs_job_path") != "incomplete" {
		t.Fatalf("file mutation was not fail-closed: %#v", report)
	}
	if !containsString(report.Ledgers.Downloader.FileLayout.StopReasons, "client_file_snapshot_unstable") {
		t.Fatalf("unstable reason missing: %#v", report.Ledgers.Downloader.FileLayout)
	}
}

func TestFileAttemptRemainsAuditableWhenOuterIdentityIsUnstable(t *testing.T) {
	meta, discovery, source, hostRoot := reconciledMultiFile(t)
	_, before, after, filesBefore, filesAfter, limits := stableMultiFileBracket(meta)
	after.Jobs[0].ContentPath = "/downloads/moved-bundle"
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client: ClientBracket{
			Requested: true, Before: &before, After: &after, RequestsMade: 5,
			FileLayoutMode: "auto", FileLimits: limits, FileAttempted: true, FileRequestsMade: 2,
			FilesBefore: &filesBefore, FilesAfter: &filesAfter,
		},
		PathMapping: &PathMappingOptions{HostRoot: hostRoot, ClientRoot: "/downloads"},
	})
	if err != nil {
		t.Fatal(err)
	}
	layout := report.Ledgers.Downloader.FileLayout
	if report.Outcome != "incomplete" || relationStatus(report, "client_infohash_relation") != "incomplete" || layout.Status != "incomplete" || layout.RequestsMade != 2 || layout.UsedBefore.FilesConsidered != 2 || layout.UsedAfter.FilesConsidered != 2 || !containsString(layout.StopReasons, "client_file_identity_unbound") {
		t.Fatalf("completed file reads disappeared behind an unstable outer bracket: %#v", report)
	}
}

func TestInvalidFileLimitsCannotBorrowSnapshotDefaults(t *testing.T) {
	meta, discovery, source, hostRoot := reconciledMultiFile(t)
	_, before, after, filesBefore, filesAfter, _ := stableMultiFileBracket(meta)
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client: ClientBracket{
			Requested: true, Before: &before, After: &after, RequestsMade: 5,
			FileLayoutMode: "auto", FileAttempted: true, FileRequestsMade: 2,
			FilesBefore: &filesBefore, FilesAfter: &filesAfter,
		},
		PathMapping: &PathMappingOptions{HostRoot: hostRoot, ClientRoot: "/downloads"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "incomplete" || report.Ledgers.Downloader.FileLayout.Status != "incomplete" || !containsString(report.Ledgers.Downloader.FileLayout.StopReasons, "invalid_client_file_limits") {
		t.Fatalf("invalid invocation limits borrowed snapshot defaults: %#v", report)
	}
}

func TestMultiFileManifestConflictAndSelectionRemainDistinct(t *testing.T) {
	meta, discovery, source, hostRoot := reconciledMultiFile(t)
	_, before, after, filesBefore, filesAfter, limits := stableMultiFileBracket(meta)
	filesBefore.Files[0].SizeBytes = 4
	filesAfter.Files[0].SizeBytes = 4
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client: ClientBracket{
			Requested: true, Before: &before, After: &after, RequestsMade: 5,
			FileLayoutMode: "auto", FileLimits: limits, FileAttempted: true, FileRequestsMade: 2,
			FilesBefore: &filesBefore, FilesAfter: &filesAfter,
		},
		PathMapping: &PathMappingOptions{HostRoot: hostRoot, ClientRoot: "/downloads"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "conflict" || relationStatus(report, "verified_source_vs_job_path") != "client_file_layout_conflict" {
		t.Fatalf("stable size contradiction was not a conflict: %#v", report)
	}

	_, before, after, filesBefore, filesAfter, limits = stableMultiFileBracket(meta)
	filesBefore.Files[1].Selection = downloader.JobFileSelectionSkipped
	filesAfter.Files[1].Selection = downloader.JobFileSelectionSkipped
	report, err = Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client: ClientBracket{
			Requested: true, Before: &before, After: &after, RequestsMade: 5,
			FileLayoutMode: "auto", FileLimits: limits, FileAttempted: true, FileRequestsMade: 2,
			FilesBefore: &filesBefore, FilesAfter: &filesAfter,
		},
		PathMapping: &PathMappingOptions{HostRoot: hostRoot, ClientRoot: "/downloads"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "partial" || relationStatus(report, "verified_source_vs_job_path") != "client_files_unselected" {
		t.Fatalf("skipped file was overstated or misclassified: %#v", report)
	}

	_, before, after, filesBefore, filesAfter, limits = stableMultiFileBracket(meta)
	before.Jobs[0].SizeBytes = 5
	after.Jobs[0].SizeBytes = 5
	report, err = Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client: ClientBracket{
			Requested: true, Before: &before, After: &after, RequestsMade: 5,
			FileLayoutMode: "auto", FileLimits: limits, FileAttempted: true, FileRequestsMade: 2,
			FilesBefore: &filesBefore, FilesAfter: &filesAfter,
		},
		PathMapping: &PathMappingOptions{HostRoot: hostRoot, ClientRoot: "/downloads"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "conflict" || relationStatus(report, "verified_source_vs_job_path") != "client_size_conflict" {
		t.Fatalf("top-level multi-file size contradiction was accepted: %#v", report)
	}
}

func TestFileLayoutOffAndExactSelectorDoNotFanOut(t *testing.T) {
	meta, discovery, source, hostRoot := reconciledMultiFile(t)
	job, before, after, _, _, limits := stableMultiFileBracket(meta)
	key, ok := SelectExactJobForFileRead(meta, before, limits)
	if !ok || key != job.Hash {
		t.Fatalf("unique typed job was not selected: %q %v", key, ok)
	}
	duplicate := job
	duplicate.Hash = "opaque-job-two"
	before.Jobs = append(before.Jobs, duplicate)
	if key, ok := SelectExactJobForFileRead(meta, before, limits); ok || key != "" {
		t.Fatalf("ambiguous jobs selected a file endpoint: %q %v", key, ok)
	}

	before, after = ledgerPair(job)
	before.Capabilities.JobFiles, after.Capabilities.JobFiles = true, true
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client:      ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3, FileLayoutMode: "off", FileLimits: limits},
		PathMapping: &PathMappingOptions{HostRoot: hostRoot, ClientRoot: "/downloads"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "partial" || report.Ledgers.Downloader.FileLayout.Status != "not_requested" || relationStatus(report, "verified_source_vs_job_path") != "client_file_layout_not_requested" || containsString(report.Effect, "read_downloader_file_layout") {
		t.Fatalf("disabled file layout did not retain the read-only partial contract: %#v", report)
	}
}

func TestUnexpectedFileActivityCannotReconcileSingleFileOrDisabledMode(t *testing.T) {
	meta, discovery, source, hostRoot := reconciledSingleFile(t)
	job := matchingJob(meta, "/downloads/renamed.bin")
	before, after := ledgerPair(job)
	limits := downloader.DefaultJobFileLedgerLimits()

	for _, mode := range []string{"auto", "off"} {
		t.Run(mode, func(t *testing.T) {
			report, err := Build(BuildInput{
				Meta: meta, Discovery: discovery, VerifiedSource: source,
				Client: ClientBracket{
					Requested: true, Before: &before, After: &after, RequestsMade: 5,
					FileLayoutMode: mode, FileLimits: limits, FileAttempted: true, FileRequestsMade: 2,
				},
				PathMapping: &PathMappingOptions{HostRoot: hostRoot, ClientRoot: "/downloads"},
			})
			if err != nil {
				t.Fatal(err)
			}
			layout := report.Ledgers.Downloader.FileLayout
			if report.Outcome == "consistent" || layout.Status != "incomplete" || relationStatus(report, "verified_source_vs_job_path") != "incomplete" || !containsString(layout.StopReasons, "unexpected_client_file_activity") {
				t.Fatalf("contradictory %s file activity was accepted: %#v", mode, report)
			}
		})
	}
}

func TestMultiFileLayoutSupportsPureV2AndHybridProofs(t *testing.T) {
	for _, version := range []string{"v2", "hybrid"} {
		t.Run(version, func(t *testing.T) {
			meta, discovery, source, hostRoot := reconciledMultiFileVersion(t, version)
			_, before, after, filesBefore, filesAfter, limits := stableMultiFileBracket(meta)
			report, err := Build(BuildInput{
				Meta: meta, Discovery: discovery, VerifiedSource: source,
				Client: ClientBracket{
					Requested: true, Before: &before, After: &after, RequestsMade: 5,
					FileLayoutMode: "auto", FileLimits: limits, FileAttempted: true, FileRequestsMade: 2,
					FilesBefore: &filesBefore, FilesAfter: &filesAfter,
				},
				PathMapping: &PathMappingOptions{HostRoot: hostRoot, ClientRoot: "/downloads"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if report.Outcome != "consistent" || relationStatus(report, "verified_source_vs_job_path") != "same_location" || !report.Ledgers.Storage.ProcessLocalProof {
				t.Fatalf("%s multi-file proof did not reconcile: %#v", version, report)
			}
		})
	}
}

func TestFileLayoutRejectsUnsupportedManifestAndNamespaceAmbiguity(t *testing.T) {
	for _, file := range []metafile.File{
		{Path: []string{"padding"}, Length: 1, Attribute: "p"},
		{Path: []string{"link"}, Length: 1, Attribute: "l"},
		{Path: []string{"empty"}, Length: 0},
	} {
		if manifestSupportsClientFileLayout(&metafile.MetaInfo{MultiFile: true, Files: []metafile.File{file}}) {
			t.Fatalf("unsupported manifest file was accepted: %#v", file)
		}
	}
	if !manifestSupportsClientFileLayout(&metafile.MetaInfo{MultiFile: true, Files: []metafile.File{{Path: []string{"ordinary"}, Length: 1}}}) {
		t.Fatal("ordinary nonempty file layout was rejected")
	}

	if _, err := parseClientRelativeComponents([]string{"%2e%2e"}, false); err != nil {
		t.Fatalf("literal percent path was URL-decoded or rejected: %v", err)
	}
	for _, components := range [][]string{{".."}, {"a/b"}, {""}} {
		if _, err := parseClientRelativeComponents(components, false); err == nil {
			t.Fatalf("accepted unsafe relative components %#v", components)
		}
	}
	windowsA, err := parseClientPath(`C:\Downloads\Bundle\A.bin`, true)
	if err != nil {
		t.Fatal(err)
	}
	windowsCase, err := parseClientPath(`C:\Downloads\Bundle\a.bin`, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateClientPathSet(map[int]clientPath{0: windowsA, 1: windowsCase}); err == nil {
		t.Fatal("accepted a Windows case-fold path collision")
	}
	windowsSigma, err := parseClientPath(`C:\Downloads\Bundle\σ.bin`, true)
	if err != nil {
		t.Fatal(err)
	}
	windowsFinalSigma, err := parseClientPath(`C:\Downloads\Bundle\ς.bin`, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateClientPathSet(map[int]clientPath{0: windowsSigma, 1: windowsFinalSigma}); err == nil {
		t.Fatal("accepted a Windows Unicode simple-fold path collision")
	}
	parent, err := parseClientPath("/downloads/bundle/file", false)
	if err != nil {
		t.Fatal(err)
	}
	child, err := parseClientPath("/downloads/bundle/file/child", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateClientPathSet(map[int]clientPath{0: parent, 1: child}); err == nil {
		t.Fatal("accepted a file/directory prefix collision")
	}
	interposed, err := parseClientPath("/downloads/bundle/file-child", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateClientPathSet(map[int]clientPath{0: parent, 1: interposed, 2: child}); err == nil {
		t.Fatal("lexical interposition hid a file/directory prefix collision")
	}
}

func TestUnsupportedManifestWinsOverUninterpretableClientSize(t *testing.T) {
	proofBytes := []byte{'a', 0, 0, 0}
	piece := sha1.Sum(proofBytes)
	meta, err := metafile.Parse(testBencode(map[string]any{"info": map[string]any{
		"files": []any{
			map[string]any{"length": int64(1), "path": []any{"a.bin"}},
			map[string]any{"attr": "p", "length": int64(3), "path": []any{".pad", "3"}},
		},
		"name": "bundle", "piece length": int64(4), "pieces": piece[:],
	}}))
	if err != nil {
		t.Fatal(err)
	}
	hostRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostRoot, "renamed-a"), []byte{'a'}, 0o600); err != nil {
		t.Fatal(err)
	}
	discovery, err := seed.Discover(context.Background(), meta, seed.DiscoverOptions{
		SearchRoots: []string{hostRoot}, InventoryLimits: storage.DefaultInventoryLimits(), MatchLimits: metafile.DefaultSourceMatchLimits(), Strategy: "copy",
		ClientMapping: &seed.ClientMappingOptions{HostRoot: hostRoot, ClientRoot: "/downloads"},
	})
	if err != nil {
		t.Fatal(err)
	}
	source, ok := discovery.VerifiedSource(meta)
	if !ok {
		t.Fatalf("padding fixture did not verify: %#v", discovery)
	}
	job := matchingJob(meta, "/downloads/bundle")
	job.SizeBytes = 999
	before, after := ledgerPair(job)
	before.Capabilities.JobFiles, after.Capabilities.JobFiles = true, true
	limits := downloader.DefaultJobFileLedgerLimits()
	if key, selected := SelectExactJobForFileRead(meta, before, limits); selected || key != "" {
		t.Fatalf("unsupported manifest selected a file endpoint: %q %t", key, selected)
	}
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client:      ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3, FileLayoutMode: "auto", FileLimits: limits},
		PathMapping: &PathMappingOptions{HostRoot: hostRoot, ClientRoot: "/downloads"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "partial" || report.Ledgers.Downloader.FileLayout.Status != "unsupported" || relationStatus(report, "verified_source_vs_job_path") != "client_file_layout_unobservable" {
		t.Fatalf("unsupported padding semantics were mislabeled as a size conflict: %#v", report)
	}
}

func reconciledMultiFile(t *testing.T) (*metafile.MetaInfo, seed.DiscoveryResult, *metafile.VerifiedSource, string) {
	return reconciledMultiFileVersion(t, "v1")
}

func reconciledMultiFileVersion(t *testing.T, version string) (*metafile.MetaInfo, seed.DiscoveryResult, *metafile.VerifiedSource, string) {
	t.Helper()
	dataA, dataB := []byte("abc"), []byte("def")
	if version == "hybrid" {
		dataA = bytes.Repeat([]byte{'a'}, 16384)
	}
	data := append(append([]byte{}, dataA...), dataB...)
	files := []any{
		map[string]any{"length": int64(len(dataA)), "path": []any{"a.bin"}},
		map[string]any{"length": int64(len(dataB)), "path": []any{"b.bin"}},
	}
	info := map[string]any{
		"name": "bundle",
	}
	switch version {
	case "v1":
		piece0, piece1 := sha1.Sum(data[:4]), sha1.Sum(data[4:])
		info["files"] = files
		info["piece length"] = int64(4)
		info["pieces"] = append(append([]byte{}, piece0[:]...), piece1[:]...)
	case "v2", "hybrid":
		rootA, rootB := sha256.Sum256(dataA), sha256.Sum256(dataB)
		info["file tree"] = map[string]any{
			"a.bin": map[string]any{"": map[string]any{"length": int64(len(dataA)), "pieces root": rootA[:]}},
			"b.bin": map[string]any{"": map[string]any{"length": int64(len(dataB)), "pieces root": rootB[:]}},
		}
		info["meta version"] = int64(2)
		info["piece length"] = int64(16384)
		if version == "hybrid" {
			pieceA, pieceB := sha1.Sum(dataA), sha1.Sum(dataB)
			info["files"] = files
			info["pieces"] = append(append([]byte{}, pieceA[:]...), pieceB[:]...)
		}
	default:
		t.Fatalf("unsupported test metafile version %q", version)
	}
	meta, err := metafile.Parse(testBencode(map[string]any{"info": info}))
	if err != nil {
		t.Fatal(err)
	}
	hostRoot := t.TempDir()
	bundleRoot := filepath.Join(hostRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleRoot, "renamed-one"), dataA, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleRoot, "renamed-two"), dataB, 0o600); err != nil {
		t.Fatal(err)
	}
	discovery, err := seed.Discover(context.Background(), meta, seed.DiscoverOptions{
		SearchRoots: []string{bundleRoot}, InventoryLimits: storage.DefaultInventoryLimits(), MatchLimits: metafile.DefaultSourceMatchLimits(), Strategy: "copy",
		ClientMapping: &seed.ClientMappingOptions{HostRoot: hostRoot, ClientRoot: "/downloads"},
	})
	if err != nil {
		t.Fatal(err)
	}
	source, ok := discovery.VerifiedSource(meta)
	if !ok {
		t.Fatalf("multi-file discovery did not retain process-local proof: %#v", discovery)
	}
	return meta, discovery, source, hostRoot
}

func stableMultiFileBracket(meta *metafile.MetaInfo) (downloader.Torrent, downloader.LedgerSnapshot, downloader.LedgerSnapshot, downloader.JobFileLedgerSnapshot, downloader.JobFileLedgerSnapshot, downloader.JobFileLedgerLimits) {
	job := matchingJob(meta, "/downloads/bundle")
	job.SizeBytes = physicalBytes(meta)
	job.SavePath = "/downloads"
	before, after := ledgerPair(job)
	before.Capabilities.JobFiles, after.Capabilities.JobFiles = true, true
	limits := downloader.DefaultJobFileLedgerLimits()
	files := []downloader.JobFile{
		{Index: 0, RelativeComponents: []string{"bundle", "renamed-one"}, SizeBytes: meta.Files[0].Length, Progress: 1, Selection: downloader.JobFileSelectionSelected, Complete: true},
		{Index: 1, RelativeComponents: []string{"bundle", "renamed-two"}, SizeBytes: meta.Files[1].Length, Progress: 1, Selection: downloader.JobFileSelectionSelected, Complete: true},
	}
	filesBefore := downloader.JobFileLedgerSnapshot{
		Driver: "qbittorrent", JobKey: job.Hash,
		ObservedAtStart: before.ObservedAtEnd.Add(100 * time.Millisecond), ObservedAtEnd: before.ObservedAtEnd.Add(200 * time.Millisecond),
		Complete: true, Limits: limits, Used: downloader.JobFileLedgerUsage{FilesConsidered: 2, PathBytes: 34, ResponseBytes: 512}, Files: files,
	}
	filesAfter := filesBefore
	filesAfter.ObservedAtStart = filesBefore.ObservedAtEnd.Add(100 * time.Millisecond)
	filesAfter.ObservedAtEnd = filesBefore.ObservedAtEnd.Add(200 * time.Millisecond)
	filesAfter.Files = append([]downloader.JobFile(nil), files...)
	for index := range filesAfter.Files {
		filesAfter.Files[index].RelativeComponents = append([]string(nil), filesAfter.Files[index].RelativeComponents...)
	}
	return job, before, after, filesBefore, filesAfter, limits
}

func containsString(items []string, wanted string) bool {
	for _, item := range items {
		if item == wanted {
			return true
		}
	}
	return false
}
