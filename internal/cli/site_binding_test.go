package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

func TestSiteMetafileBindingListAndInspectKeepEvidenceLevelsSeparate(t *testing.T) {
	storeRoot := filepath.Join(physicalCLITempDir(t), "SITE-BINDING-STORE-PATH-CANARY")
	if _, _, err := metastore.Init(storeRoot); err != nil {
		t.Fatal(err)
	}
	const (
		name    = "SITE-BINDING-METAFILE-NAME-CANARY.bin"
		passkey = "SITE-BINDING-PASSKEY-CANARY"
		cookie  = "SID=SITE-BINDING-COOKIE-CANARY"
	)
	adapter := &fakeMetafileFetchAdapter{raw: sitePrivateTrackerMetafile(name, []byte("binding"), passkey)}
	recordID, variantID := fetchCLISiteBinding(t, storeRoot, adapter, cookie)

	var listOut bytes.Buffer
	a := &app{stdin: strings.NewReader("UNREAD-STDIN-CANARY"), stdout: &listOut, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	if err := a.site([]string{"metafile", "binding", "list", "--metafile-store", storeRoot, "--output", "json"}); err != nil {
		t.Fatalf("list failed: %v output=%s", err, listOut.String())
	}
	var listed struct {
		Kind string                `json:"kind"`
		Data siteBindingListReport `json:"data"`
	}
	if err := json.Unmarshal(listOut.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Kind != "site.metafile.binding.list" || listed.Data.Outcome != "complete" || !listed.Data.Complete ||
		listed.Data.WritesPerformed != 0 || len(listed.Data.Bindings) != 1 || listed.Data.Bindings[0].ID != recordID ||
		listed.Data.Assurance.Evidence != "bounded_name_only_locator_inventory" || listed.Data.Assurance.RecordsVerified ||
		listed.Data.Assurance.ArtifactsVerified || listed.Data.Assurance.Selection != "none" || listed.Data.Blockers == nil || listed.Data.Warnings == nil {
		t.Fatalf("unsafe list report: %s", listOut.String())
	}
	assertJSONStringsExclude(t, listOut.Bytes(), storeRoot, name, passkey, cookie, "fake.invalid", "fakept", variantID)
	if strings.Contains(listOut.String(), `"site_id"`) || strings.Contains(listOut.String(), `"remote_id"`) {
		t.Fatalf("name-only locator inventory exposed binding payload fields: %s", listOut.String())
	}

	var inspectOut bytes.Buffer
	a.stdout = &inspectOut
	if err := a.siteMetafileBinding([]string{"inspect", "--metafile-store", storeRoot, "--output", "json", recordID.String()}); err != nil {
		t.Fatalf("inspect failed: %v output=%s", err, inspectOut.String())
	}
	var inspected struct {
		Kind string                   `json:"kind"`
		Data siteBindingInspectReport `json:"data"`
	}
	if err := json.Unmarshal(inspectOut.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	assurance := inspected.Data.Assurance
	if inspected.Kind != "site.metafile.binding.inspect" || inspected.Data.Outcome != "verified" || !inspected.Data.Complete ||
		inspected.Data.WritesPerformed != 0 || inspected.Data.RecordID != recordID || inspected.Data.Binding == nil ||
		inspected.Data.Binding.Record.SiteID != "fakept" || inspected.Data.Binding.Record.RemoteID != "42" ||
		inspected.Data.Binding.Record.MetafileVariantID != variantID || !assurance.RecordDigestVerified ||
		!assurance.ArtifactDigestVerified || !assurance.ArtifactStrictlyParsed || !assurance.PrivateArtifactVerified ||
		!assurance.SameStoreOperationBound || !assurance.AdapterProvenanceAccepted || !assurance.ProcessLocalProofObserved ||
		inspected.Data.Blockers == nil || inspected.Data.Warnings == nil {
		t.Fatalf("unsafe inspect report: %s", inspectOut.String())
	}
	assertJSONStringsExclude(t, inspectOut.Bytes(), storeRoot, name, passkey, cookie, "announce?passkey")

	var human bytes.Buffer
	a.stdout = &human
	if err := a.siteMetafileBinding([]string{"inspect", "--metafile-store", storeRoot, recordID.String()}); err != nil {
		t.Fatalf("human inspect failed: %v", err)
	}
	if !strings.Contains(human.String(), "HISTORICAL SITE BINDING") || !strings.Contains(human.String(), "PROCESS-LOCAL PROOF OBSERVED") {
		t.Fatalf("human report omitted evidence sections: %s", human.String())
	}
	for _, secret := range []string{storeRoot, name, passkey, cookie, "announce?passkey"} {
		if strings.Contains(human.String(), secret) {
			t.Fatalf("human report leaked %q: %s", secret, human.String())
		}
	}
}

func TestSiteMetafileBindingListLimitIsReportFirstInconclusive(t *testing.T) {
	storeRoot := filepath.Join(physicalCLITempDir(t), "store")
	if _, _, err := metastore.Init(storeRoot); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeMetafileFetchAdapter{raw: sitePrivateTrackerMetafile("bounded.bin", []byte("x"), "PASSKEY-CANARY")}
	first, _ := fetchCLISiteBinding(t, storeRoot, adapter, "SID=first")
	second, _ := fetchCLISiteBinding(t, storeRoot, adapter, "SID=second")
	if first == second {
		t.Fatal("independent fetch observations produced the same binding record")
	}
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	err := a.siteMetafileBinding([]string{"list", "--metafile-store", storeRoot, "--max-bindings", "1", "--output", "json"})
	var inconclusive *inconclusiveErr
	if !errors.As(err, &inconclusive) {
		t.Fatalf("expected report-first inconclusive result, got %v output=%s", err, out.String())
	}
	var envelope struct {
		Data siteBindingListReport `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Outcome != "incomplete" || envelope.Data.Complete || envelope.Data.StopReason != "record_limit" ||
		len(envelope.Data.Bindings) != 1 || envelope.Data.Used.RecordsMatched != 2 ||
		!containsString(envelope.Data.Blockers, "binding.record_limit") {
		t.Fatalf("limit was not reported precisely: %s", out.String())
	}
}

func TestSiteMetafileBindingInspectIntegrityAndAdapterGates(t *testing.T) {
	storeRoot := filepath.Join(physicalCLITempDir(t), "store")
	if _, _, err := metastore.Init(storeRoot); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeMetafileFetchAdapter{raw: sitePrivateTrackerMetafile("integrity.bin", []byte("x"), "PASSKEY-CANARY")}
	recordID, variantID := fetchCLISiteBinding(t, storeRoot, adapter, "SID=value")

	var blockedOut bytes.Buffer
	blockedApp := &app{stdin: strings.NewReader(""), stdout: &blockedOut, stderr: ioDiscard{}, registry: site.NewRegistry()}
	err := blockedApp.siteMetafileBinding([]string{"inspect", "--metafile-store", storeRoot, "--output", "json", recordID.String()})
	var inconclusive *inconclusiveErr
	if !errors.As(err, &inconclusive) {
		t.Fatalf("missing adapter provenance was not blocked: %v output=%s", err, blockedOut.String())
	}
	var blocked struct {
		Data siteBindingInspectReport `json:"data"`
	}
	if err := json.Unmarshal(blockedOut.Bytes(), &blocked); err != nil {
		t.Fatal(err)
	}
	if blocked.Data.Outcome != "blocked" || !blocked.Data.Complete || blocked.Data.Binding == nil ||
		!blocked.Data.Assurance.RecordDigestVerified || !blocked.Data.Assurance.ProcessLocalProofObserved ||
		blocked.Data.Assurance.AdapterProvenanceAccepted || !containsString(blocked.Data.Blockers, "binding.adapter_provenance_mismatch") {
		t.Fatalf("adapter gate report is inconsistent: %s", blockedOut.String())
	}

	digest := strings.TrimPrefix(variantID, "sha256:")
	artifactPath := filepath.Join(storeRoot, "objects", digest+".torrent")
	if err := os.WriteFile(artifactPath, []byte("CORRUPT-ARTIFACT-CANARY"), 0o600); err != nil {
		t.Fatal(err)
	}
	var corruptOut bytes.Buffer
	corruptApp := &app{stdin: strings.NewReader(""), stdout: &corruptOut, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	err = corruptApp.siteMetafileBinding([]string{"inspect", "--metafile-store", storeRoot, "--output", "json", recordID.String()})
	var integrity *integrityErr
	if !errors.As(err, &integrity) {
		t.Fatalf("corrupt artifact was not integrity failure: %v output=%s", err, corruptOut.String())
	}
	var corrupt struct {
		Data siteBindingInspectReport `json:"data"`
	}
	if err := json.Unmarshal(corruptOut.Bytes(), &corrupt); err != nil {
		t.Fatal(err)
	}
	if corrupt.Data.Outcome != "integrity_failed" || corrupt.Data.Complete || corrupt.Data.Binding != nil ||
		!containsString(corrupt.Data.Blockers, "binding.integrity_failed") || corrupt.Data.Assurance.ArtifactDigestVerified {
		t.Fatalf("corrupt artifact report is unsafe: %s", corruptOut.String())
	}
	assertJSONStringsExclude(t, corruptOut.Bytes(), artifactPath, "CORRUPT-ARTIFACT-CANARY", "PASSKEY-CANARY")
}

func TestSiteMetafileBindingUsageRejectsBadIDBeforeStoreAccess(t *testing.T) {
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader("UNREAD-STDIN-CANARY"), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry()}
	err := a.siteMetafileBinding([]string{"inspect", "--metafile-store", filepath.Join(t.TempDir(), "MUST-NOT-OPEN"), "not-a-record-id"})
	var usage *usageErr
	if !errors.As(err, &usage) || out.Len() != 0 {
		t.Fatalf("invalid ID was not rejected as usage before reporting/store access: err=%v output=%s", err, out.String())
	}
}

func fetchCLISiteBinding(t *testing.T, storeRoot string, adapter *fakeMetafileFetchAdapter, cookie string) (metastore.RecordID, string) {
	t.Helper()
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader(cookie + "\n"), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	if err := a.siteMetafileFetch([]string{"--cookie-stdin", "--acknowledge-site-effect", "--metafile-store", storeRoot, "--output", "json", "fakept", "42"}); err != nil {
		t.Fatalf("fetch fixture failed: %v output=%s", err, out.String())
	}
	var envelope struct {
		Data siteMetafileFetchReport `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Persistent.Record == nil || envelope.Data.Binding.Observation == nil {
		t.Fatalf("fetch fixture did not publish binding: %s", out.String())
	}
	return envelope.Data.Persistent.Record.ID, envelope.Data.Binding.Observation.MetafileVariantID
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
