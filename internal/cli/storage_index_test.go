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

	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/storage"
	"github.com/tonycoder-hub/ptctl/internal/storageindex"
)

func TestStorageProfileRefreshAndSnapshotOnlyDiscoveryArePrivateAndFailClosed(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "private-state")
	if _, _, err := metastore.Init(stateRoot); err != nil {
		t.Fatal(err)
	}
	contentRoot := t.TempDir()
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

	torrentPath := filepath.Join(t.TempDir(), "source.torrent")
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

func TestSeedDiscoverStoredProfileUsageIsValidatedBeforeStoreRead(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-state")
	var out, errOut bytes.Buffer
	code := Run([]string{
		"seed", "discover", "--torrent", "missing.torrent", "--state-store", missing,
		"--storage-profile", "media", "--search-root", t.TempDir(),
	}, strings.NewReader(""), &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "mutually exclusive") || strings.Contains(errOut.String(), missing) {
		t.Fatalf("usage validation touched private state: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestReconcileStoredProfileCannotBecomeConsistentWithoutCurrentSearchCompleteness(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "private-state")
	if _, _, err := metastore.Init(stateRoot); err != nil {
		t.Fatal(err)
	}
	contentRoot := t.TempDir()
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
	torrentPath := filepath.Join(t.TempDir(), "source.torrent")
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
	torrentPath := filepath.Join(t.TempDir(), "source.torrent")
	if err := os.WriteFile(torrentPath, testV1Metafile("source.bin", []byte("x")), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := &trackingReader{}
	var out, errOut bytes.Buffer
	code := Run([]string{
		"reconcile", "report", "--torrent", torrentPath,
		"--state-store", filepath.Join(t.TempDir(), "missing"), "--storage-profile", "media",
		"--driver", "qbittorrent", "--url", "https://seedbox.invalid", "--username", "alice", "--password-stdin",
	}, reader, &out, &errOut)
	if code == 0 || reader.read {
		t.Fatalf("private state failure consumed downloader credential: code=%d stdout=%q stderr=%q read=%t", code, out.String(), errOut.String(), reader.read)
	}
}

func TestReconcileRejectsInvalidLiveProfileBeforeCredentialOrClientRequest(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "private-state")
	store, _, err := metastore.Init(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := storageindex.NewProfile("relative", []string{t.TempDir()}, false, storageindex.DefaultScanLimits(), time.Now())
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

	torrentPath := filepath.Join(t.TempDir(), "source.torrent")
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
			missingStore := filepath.Join(t.TempDir(), "must-not-be-read")
			args := []string{"storage", "profile", "create", "--state-store", missingStore, "--name", "media", "--search-root", t.TempDir()}
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
