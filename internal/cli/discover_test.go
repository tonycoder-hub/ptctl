package cli

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSeedDiscoverJSONUsesStableKindAndHidesAbsolutePaths(t *testing.T) {
	content := []byte("content")
	torrentPath := filepath.Join(t.TempDir(), "source.torrent")
	if err := os.WriteFile(torrentPath, testV1Metafile("source.bin", content), 0o600); err != nil {
		t.Fatal(err)
	}
	correctRoot := t.TempDir()
	decoyRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(correctRoot, "renamed.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoyRoot, "decoy.bin"), []byte("CONTENX"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{
		"seed", "discover", "--torrent", torrentPath,
		"--search-root", decoyRoot, "--search-root", correctRoot,
		"--timeout", "1m", "--output", "json",
	}, strings.NewReader(""), &out, &errOut)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var response struct {
		Schema string `json:"schema"`
		Kind   string `json:"kind"`
		Data   struct {
			Effect          string `json:"effect"`
			WritesPerformed int    `json:"writes_performed"`
			SourceOutcome   string `json:"source_outcome"`
			Selection       struct {
				Status string `json:"status"`
			} `json:"selection"`
			Handoff struct {
				Status       string `json:"status"`
				PlanProduced bool   `json:"plan_produced"`
			} `json:"handoff"`
			Scan struct {
				SearchRoots     []any           `json:"search_roots"`
				StopReasons     json.RawMessage `json:"stop_reasons"`
				InventoryIssues json.RawMessage `json:"inventory_issues"`
				MatchIssues     json.RawMessage `json:"match_issues"`
			} `json:"scan"`
			Matches []struct {
				Evidence string `json:"evidence_level"`
			} `json:"matches"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Schema != "ptctl.dev/v1" || response.Kind != "content.source_discovery" || response.Data.SourceOutcome != "verified_unique" || response.Data.Selection.Status != "ready" || response.Data.Handoff.Status != "not_requested" || response.Data.Handoff.PlanProduced || response.Data.Effect != "read_metadata+read_content" || response.Data.WritesPerformed != 0 || len(response.Data.Scan.SearchRoots) != 2 || len(response.Data.Matches) != 1 || response.Data.Matches[0].Evidence != "verified" {
		t.Fatalf("unexpected discovery contract: %s", out.String())
	}
	for name, encoded := range map[string]json.RawMessage{
		"scan.stop_reasons":     response.Data.Scan.StopReasons,
		"scan.inventory_issues": response.Data.Scan.InventoryIssues,
		"scan.match_issues":     response.Data.Scan.MatchIssues,
	} {
		assertJSONArray(t, name, encoded)
	}
	assertJSONStringsExclude(t, out.Bytes(), correctRoot, decoyRoot, torrentPath)
}

func TestSeedDiscoverRequireVerifiedUsesInconclusiveExitFourAfterReport(t *testing.T) {
	content := []byte("same")
	torrentPath := filepath.Join(t.TempDir(), "source.torrent")
	if err := os.WriteFile(torrentPath, testV1Metafile("source.bin", content), 0o600); err != nil {
		t.Fatal(err)
	}
	first := t.TempDir()
	second := t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "one"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "two"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{
		"seed", "discover", "--torrent", torrentPath,
		"--search-root", first, "--search-root", second,
		"--require-verified", "--output", "json",
	}, strings.NewReader(""), &out, &errOut)
	if code != 4 || !strings.Contains(out.String(), `"source_outcome": "verified_ambiguous"`) || !strings.Contains(out.String(), `"status": "blocked"`) || !strings.Contains(out.String(), "source.multiple_verified_matches") || !strings.Contains(errOut.String(), "source outcome is not verified_unique") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestSeedDiscoverRequireVerifiedIgnoresBlockedTargetHandoff(t *testing.T) {
	content := []byte("content")
	torrentPath := filepath.Join(t.TempDir(), "source.torrent")
	if err := os.WriteFile(torrentPath, testV1Metafile("source.bin", content), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "renamed.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "source.bin"), []byte("conflict"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{
		"seed", "discover", "--torrent", torrentPath,
		"--search-root", root, "--target", target,
		"--require-verified", "--output", "json",
	}, strings.NewReader(""), &out, &errOut)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var response struct {
		Data struct {
			SourceOutcome string `json:"source_outcome"`
			Selection     struct {
				Status     string `json:"status"`
				SelectedID string `json:"selected_id"`
			} `json:"selection"`
			Handoff struct {
				Status       string `json:"status"`
				PlanProduced bool   `json:"plan_produced"`
			} `json:"handoff"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.SourceOutcome != "verified_unique" || response.Data.Selection.Status != "ready" || response.Data.Selection.SelectedID == "" || response.Data.Handoff.Status != "blocked" || response.Data.Handoff.PlanProduced {
		t.Fatalf("target handoff changed the verified source predicate: %s", out.String())
	}
}

func TestSeedDiscoverValidatesBudgetsBeforeFilesystemReads(t *testing.T) {
	content := []byte("x")
	torrentPath := filepath.Join(t.TempDir(), "source.torrent")
	if err := os.WriteFile(torrentPath, testV1Metafile("x", content), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{
		"seed", "discover", "--torrent", torrentPath,
		"--search-root", filepath.Join(t.TempDir(), "does-not-exist"),
		"--max-entries", "0",
	}, strings.NewReader(""), &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "max entries") || strings.Contains(errOut.String(), "does not exist") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestSeedDiscoverRejectsUnsupportedStrategyBeforeFilesystemReads(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	var out, errOut bytes.Buffer
	code := Run([]string{
		"seed", "discover", "--torrent", filepath.Join(missing, "missing.torrent"),
		"--search-root", missing, "--strategy", "hardlink",
	}, strings.NewReader(""), &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "--strategy must be copy") || strings.Contains(errOut.String(), "does-not-exist") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestSeedDiscoverRejectsStrategyWithoutTargetBeforeFilesystemReads(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	var out, errOut bytes.Buffer
	code := Run([]string{
		"seed", "discover", "--torrent", filepath.Join(missing, "missing.torrent"),
		"--search-root", missing, "--strategy", "copy",
	}, strings.NewReader(""), &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "--strategy requires --target") || strings.Contains(errOut.String(), "does-not-exist") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestSeedDiscoverRejectsExplicitClientStyleWithoutMapping(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	var out, errOut bytes.Buffer
	code := Run([]string{
		"seed", "discover", "--torrent", filepath.Join(missing, "missing.torrent"),
		"--search-root", missing, "--client-style", "windows",
	}, strings.NewReader(""), &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "--client-style requires --host-root and --client-root") || strings.Contains(errOut.String(), "does-not-exist") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestSeedDiscoverValidatesMappingConfigBeforeTorrentRead(t *testing.T) {
	missingTorrent := filepath.Join(t.TempDir(), "missing.torrent")
	var out, errOut bytes.Buffer
	code := Run([]string{
		"seed", "discover", "--torrent", missingTorrent,
		"--search-root", t.TempDir(), "--host-root", t.TempDir(),
		"--client-root", "relative-client-root",
	}, strings.NewReader(""), &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "path mapping is invalid") || !strings.Contains(errOut.String(), "client namespace root") || strings.Contains(errOut.String(), missingTorrent) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestSeedDiscoverHelpShowsDiscoverFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run([]string{"seed", "discover", "--help"}, strings.NewReader(""), &out, &errOut)
	if code != 0 || errOut.Len() != 0 || !strings.Contains(out.String(), "Usage:") || !strings.Contains(out.String(), "max-proof-bytes") || !strings.Contains(out.String(), "Host/client mapping applies") || strings.Contains(out.String(), "accepts flags only") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestSeedDiscoverHumanShowsBlockersBeforeMatches(t *testing.T) {
	content := []byte("same")
	torrentPath := filepath.Join(t.TempDir(), "source.torrent")
	if err := os.WriteFile(torrentPath, testV1Metafile("source.bin", content), 0o600); err != nil {
		t.Fatal(err)
	}
	first := t.TempDir()
	second := t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "one"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "two"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{"seed", "discover", "--torrent", torrentPath, "--search-root", first, "--search-root", second}, strings.NewReader(""), &out, &errOut)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	blockers := strings.Index(out.String(), "BLOCKERS")
	candidates := strings.Index(out.String(), "CANDIDATES (EXACT SIZE ONLY; NOT VERIFIED)")
	matches := strings.Index(out.String(), "VERIFIED MATCHES")
	if blockers < 0 || candidates < 0 || matches < 0 || blockers > candidates || candidates > matches || !strings.Contains(out.String(), "SOURCE OUTCOME") || !strings.Contains(out.String(), "verified_ambiguous") || !strings.Contains(out.String(), "SELECTION") || !strings.Contains(out.String(), "blocked") || !strings.Contains(out.String(), "HANDOFF") {
		t.Fatalf("unclear discovery table: %q", out.String())
	}
}

func TestSeedDiscoverHumanShowsPlanEvidenceAndBlockers(t *testing.T) {
	content := []byte("content")
	torrentPath := filepath.Join(t.TempDir(), "source.torrent")
	if err := os.WriteFile(torrentPath, testV1Metafile("source.bin", content), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "renamed.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{
		"seed", "discover", "--torrent", torrentPath,
		"--search-root", root, "--target", t.TempDir(),
	}, strings.NewReader(""), &out, &errOut)
	if code != 0 || errOut.Len() != 0 || !strings.Contains(out.String(), "HANDOFF") || !strings.Contains(out.String(), "ready") || !strings.Contains(out.String(), "LAYOUT PLAN") || !strings.Contains(out.String(), "EFFECT") || !strings.Contains(out.String(), "EVIDENCE") || !strings.Contains(out.String(), "PLAN BLOCKERS") || !strings.Contains(out.String(), "PLAN WARNINGS") || !strings.Contains(out.String(), "PLANNED OPERATIONS") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func assertJSONArray(t *testing.T, name string, encoded json.RawMessage) {
	t.Helper()
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatalf("%s is not valid JSON: %v (%s)", name, err, encoded)
	}
	if _, ok := value.([]any); !ok {
		t.Fatalf("%s must be an array, got %s", name, encoded)
	}
}

func assertJSONStringsExclude(t *testing.T, encoded []byte, secrets ...string) {
	t.Helper()
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	var visit func(any, string)
	visit = func(current any, location string) {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				visit(child, location+"."+key)
			}
		case []any:
			for index, child := range typed {
				visit(child, location+"["+strconv.Itoa(index)+"]")
			}
		case string:
			for _, secret := range secrets {
				if strings.Contains(typed, secret) {
					t.Fatalf("absolute path %q leaked at %s without --show-absolute-paths", secret, location)
				}
			}
		}
	}
	visit(value, "$")
}

func testV1Metafile(name string, content []byte) []byte {
	piece := sha1.Sum(content)
	result := []byte("d4:infod6:lengthi" + strconv.Itoa(len(content)) + "e4:name" + strconv.Itoa(len(name)) + ":" + name + "12:piece lengthi" + strconv.Itoa(len(content)) + "e6:pieces20:")
	result = append(result, piece[:]...)
	return append(result, 'e', 'e')
}
