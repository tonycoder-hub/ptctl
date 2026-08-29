package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMetafileStoreWorkflowAndStoredConsumers(t *testing.T) {
	storeRoot := filepath.Join(physicalCLITempDir(t), "PTCTL-STORE-PATH-CANARY")
	content := []byte("private-store-content")
	const privateName = "PTCTL-METAFILE-NAME-CANARY.bin"
	const passkey = "PTCTL-PASSKEY-CANARY"
	sourcePath := filepath.Join(physicalCLITempDir(t), "PTCTL-SOURCE-PATH-CANARY.torrent")
	if err := os.WriteFile(sourcePath, privateTrackerMetafile(privateName, content, passkey), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := Run([]string{"metafile", "store", "init", "--store", storeRoot, "--output", "json"}, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("init code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), storeRoot, sourcePath, privateName, passkey)
	var initEnvelope struct {
		Kind string `json:"kind"`
		Data struct {
			Outcome   string          `json:"outcome"`
			Blockers  json.RawMessage `json:"blockers"`
			Issues    json.RawMessage `json:"issues"`
			Warnings  json.RawMessage `json:"warnings"`
			Assurance struct {
				Publication         string `json:"publication"`
				AtomicNoClobber     bool   `json:"atomic_no_clobber"`
				DurabilityConfirmed bool   `json:"durability_confirmed"`
			} `json:"assurance"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &initEnvelope); err != nil {
		t.Fatal(err)
	}
	if initEnvelope.Kind != "metafile.store.init" || initEnvelope.Data.Outcome != "initialized" || initEnvelope.Data.Assurance.Publication != "confirmed_this_invocation" || !initEnvelope.Data.Assurance.AtomicNoClobber || !initEnvelope.Data.Assurance.DurabilityConfirmed {
		t.Fatalf("unexpected init report: %s", out.String())
	}
	assertJSONArray(t, "init blockers", initEnvelope.Data.Blockers)
	assertJSONArray(t, "init issues", initEnvelope.Data.Issues)
	assertJSONArray(t, "init warnings", initEnvelope.Data.Warnings)

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"metafile", "store", "import", "--store", storeRoot, "--output", "json", sourcePath}, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("import code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), storeRoot, sourcePath, privateName, passkey)
	var importEnvelope struct {
		Kind string `json:"kind"`
		Data struct {
			Outcome         string `json:"outcome"`
			WritesPerformed int    `json:"writes_performed"`
			Assurance       struct {
				Publication         string `json:"publication"`
				DigestVerified      bool   `json:"digest_verified"`
				AtomicNoClobber     bool   `json:"atomic_no_clobber"`
				DurabilityConfirmed bool   `json:"durability_confirmed"`
			} `json:"assurance"`
			Artifact struct {
				VariantID string `json:"metafile_variant_id"`
				Status    string `json:"status"`
			} `json:"artifact"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &importEnvelope); err != nil {
		t.Fatal(err)
	}
	variantID := importEnvelope.Data.Artifact.VariantID
	if importEnvelope.Kind != "metafile.store.import" || importEnvelope.Data.Outcome != "stored" || importEnvelope.Data.WritesPerformed != 1 || !strings.HasPrefix(variantID, "sha256:") || importEnvelope.Data.Assurance.Publication != "confirmed_this_invocation" || !importEnvelope.Data.Assurance.AtomicNoClobber || !importEnvelope.Data.Assurance.DurabilityConfirmed {
		t.Fatalf("unexpected import report: %s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"metafile", "store", "import", "--store", storeRoot, "--output", "json", sourcePath}, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("repeat import code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if err := json.Unmarshal(out.Bytes(), &importEnvelope); err != nil {
		t.Fatal(err)
	}
	if importEnvelope.Data.Outcome != "already_present" || importEnvelope.Data.WritesPerformed != 0 || importEnvelope.Data.Artifact.VariantID != variantID || importEnvelope.Data.Assurance.Publication != "historical_publication_unobservable" || !importEnvelope.Data.Assurance.DigestVerified || importEnvelope.Data.Assurance.AtomicNoClobber || importEnvelope.Data.Assurance.DurabilityConfirmed {
		t.Fatalf("import was not idempotent: %s", out.String())
	}

	for _, args := range [][]string{
		{"metafile", "store", "inspect", "--store", storeRoot, "--output", "json", variantID},
		{"torrent", "inspect", "--metafile-store", storeRoot, "--metafile-variant", variantID, "--output", "json"},
	} {
		out.Reset()
		errOut.Reset()
		if code := Run(args, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 || !strings.Contains(out.String(), variantID) {
			t.Fatalf("args=%v code=%d stdout=%q stderr=%q", args, code, out.String(), errOut.String())
		}
		if args[0] == "metafile" {
			assertHistoricalStorePublication(t, out.Bytes())
		}
	}

	contentPath := filepath.Join(physicalCLITempDir(t), "renamed-content.bin")
	if err := os.WriteFile(contentPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"torrent", "verify", "--metafile-store", storeRoot, "--metafile-variant", variantID, "--content", contentPath, "--output", "json"}, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 || !strings.Contains(out.String(), `"verified": true`) {
		t.Fatalf("stored verify code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}

	targetRoot := physicalCLITempDir(t)
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"seed", "plan", "--metafile-store", storeRoot, "--metafile-variant", variantID, "--source", contentPath, "--target", targetRoot, "--output", "json"}, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 || !strings.Contains(out.String(), `"kind": "content.layout_plan"`) || !strings.Contains(out.String(), `"effect": "none"`) || strings.Contains(out.String(), storeRoot) {
		t.Fatalf("stored seed plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}

	searchRoot := physicalCLITempDir(t)
	if err := os.WriteFile(filepath.Join(searchRoot, "different-name.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"seed", "discover", "--metafile-store", storeRoot, "--metafile-variant", variantID, "--search-root", searchRoot, "--output", "json"},
		{"reconcile", "report", "--metafile-store", storeRoot, "--metafile-variant", variantID, "--search-root", searchRoot, "--output", "json"},
	} {
		out.Reset()
		errOut.Reset()
		if code := Run(args, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 || !strings.Contains(out.String(), variantID) {
			t.Fatalf("args=%v code=%d stdout=%q stderr=%q", args, code, out.String(), errOut.String())
		}
	}
}

func TestMetafileStoreInvalidImportAndCorruptConsumerUseIntegrityExit(t *testing.T) {
	storeRoot := filepath.Join(physicalCLITempDir(t), "store")
	var out, errOut bytes.Buffer
	if code := Run([]string{"metafile", "store", "init", "--store", storeRoot}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("init code=%d stderr=%q", code, errOut.String())
	}
	invalidPath := filepath.Join(physicalCLITempDir(t), "PTCTL-INVALID-PATH-CANARY.torrent")
	if err := os.WriteFile(invalidPath, []byte("PTCTL-INVALID-BODY-CANARY"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	code := Run([]string{"metafile", "store", "import", "--store", storeRoot, "--output", "json", invalidPath}, strings.NewReader(""), &out, &errOut)
	if code != 3 || !strings.Contains(out.String(), `"outcome": "integrity_failed"`) || strings.Contains(out.String(), invalidPath) || strings.Contains(out.String(), "PTCTL-INVALID-BODY-CANARY") || strings.Contains(errOut.String(), invalidPath) {
		t.Fatalf("invalid import code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var invalidReport struct {
		Data struct {
			Used metafileStoreUsage `json:"used"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &invalidReport); err != nil || !invalidReport.Data.Used.ArtifactBytesKnown || invalidReport.Data.Used.ArtifactBytes != int64(len("PTCTL-INVALID-BODY-CANARY")) {
		t.Fatalf("invalid import usage was inaccurate: report=%+v err=%v", invalidReport, err)
	}

	content := []byte("valid")
	validPath := filepath.Join(physicalCLITempDir(t), "valid.torrent")
	if err := os.WriteFile(validPath, testV1Metafile("valid.bin", content), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"metafile", "store", "import", "--store", storeRoot, "--output", "json", validPath}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("valid import code=%d stderr=%q", code, errOut.String())
	}
	var imported struct {
		Data struct {
			Artifact struct {
				VariantID string `json:"metafile_variant_id"`
			} `json:"artifact"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &imported); err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(storeRoot, "objects", strings.TrimPrefix(imported.Data.Artifact.VariantID, "sha256:")+".torrent")
	if err := os.WriteFile(objectPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	code = Run([]string{"metafile", "store", "inspect", "--store", storeRoot, "--output", "json", imported.Data.Artifact.VariantID}, strings.NewReader(""), &out, &errOut)
	if code != 3 || !strings.Contains(out.String(), `"outcome": "integrity_failed"`) || strings.Contains(out.String(), storeRoot) || strings.Contains(errOut.String(), storeRoot) {
		t.Fatalf("corrupt store inspect code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var corruptReport struct {
		Data struct {
			Used metafileStoreUsage `json:"used"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &corruptReport); err != nil || corruptReport.Data.Used.ArtifactBytesKnown {
		t.Fatalf("corrupt inspect should report unknown read usage: report=%+v err=%v", corruptReport, err)
	}

	out.Reset()
	errOut.Reset()
	code = Run([]string{"torrent", "inspect", "--metafile-store", storeRoot, "--metafile-variant", imported.Data.Artifact.VariantID, "--output", "json"}, strings.NewReader(""), &out, &errOut)
	if code != 3 || out.Len() != 0 || !strings.Contains(errOut.String(), "stored metafile failed") || strings.Contains(errOut.String(), storeRoot) {
		t.Fatalf("corrupt consumer code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func privateTrackerMetafile(name string, content []byte, passkey string) []byte {
	base := testV1Metafile(name, content)
	announce := "https://tracker.invalid/announce?passkey=" + passkey
	wrapped := []byte("d8:announce" + strconv.Itoa(len(announce)) + ":" + announce)
	return append(wrapped, base[1:]...)
}

func TestMetafileSelectorsRejectPartialAndMixedInputs(t *testing.T) {
	storeRoot := filepath.Join(physicalCLITempDir(t), "store")
	variant := "sha256:" + strings.Repeat("a", 64)
	tests := [][]string{
		{"torrent", "inspect", "--metafile-store", storeRoot},
		{"torrent", "inspect", "--metafile-store=", "release.torrent"},
		{"torrent", "inspect", "--metafile-variant", variant},
		{"torrent", "inspect", "--metafile-store", storeRoot, "--metafile-variant", "sha256:not-canonical"},
		{"torrent", "inspect", "--metafile-store", storeRoot, "--metafile-variant", variant, "release.torrent"},
		{"metafile", "store", "import", "--store", storeRoot, ""},
		{"seed", "plan", "--torrent", "release.torrent", "--metafile-store", storeRoot, "--metafile-variant", variant, "--source", "source", "--target", "target"},
		{"seed", "plan", "--torrent=", "--source", "source", "--target", "target"},
	}
	for _, args := range tests {
		var out, errOut bytes.Buffer
		if code := Run(args, strings.NewReader(""), &out, &errOut); code != 2 {
			t.Fatalf("args=%v code=%d stdout=%q stderr=%q", args, code, out.String(), errOut.String())
		}
	}
}

func TestReconcileMetafileSelectorUsageDoesNotReadPassword(t *testing.T) {
	reader := &trackingReader{}
	var out, errOut bytes.Buffer
	code := Run([]string{
		"reconcile", "report",
		"--metafile-store", filepath.Join(physicalCLITempDir(t), "store"),
		"--search-root", physicalCLITempDir(t),
		"--driver", "qbittorrent", "--url", "https://seedbox.invalid",
		"--username", "user", "--password-stdin",
	}, reader, &out, &errOut)
	if code != 2 || reader.read || !strings.Contains(errOut.String(), "--metafile-store and --metafile-variant together") {
		t.Fatalf("code=%d secret_read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
}

func TestSeedPlanRejectsStrategyBeforeMetafileOrSourceRead(t *testing.T) {
	missingRoot := filepath.Join(physicalCLITempDir(t), "PTCTL-NOT-READ")
	var out, errOut bytes.Buffer
	code := Run([]string{
		"seed", "plan", "--torrent", filepath.Join(missingRoot, "secret.torrent"),
		"--source", filepath.Join(missingRoot, "content"), "--target", filepath.Join(missingRoot, "target"),
		"--strategy", "hardlink",
	}, strings.NewReader(""), &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "--strategy must be copy") || strings.Contains(errOut.String(), missingRoot) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestHelpShowsMetafileStoreAndStoredSelectors(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"help"}, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	for _, required := range []string{"metafile store init", "metafile store import", "metafile store inspect", "--metafile-store", "--metafile-variant"} {
		if !strings.Contains(out.String(), required) {
			t.Fatalf("help is missing %q: %q", required, out.String())
		}
	}
}

func physicalCLITempDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatalf("resolve temporary directory: %v", err)
	}
	return resolved
}

func assertHistoricalStorePublication(t *testing.T, raw []byte) {
	t.Helper()
	var envelope struct {
		Data struct {
			Assurance struct {
				Publication         string `json:"publication"`
				DigestVerified      bool   `json:"digest_verified"`
				AtomicNoClobber     bool   `json:"atomic_no_clobber"`
				DurabilityConfirmed bool   `json:"durability_confirmed"`
			} `json:"assurance"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	assurance := envelope.Data.Assurance
	if assurance.Publication != "historical_publication_unobservable" || !assurance.DigestVerified || assurance.AtomicNoClobber || assurance.DurabilityConfirmed {
		t.Fatalf("read-only report overstated historical publication: %s", raw)
	}
}
