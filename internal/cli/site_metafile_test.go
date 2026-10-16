package cli

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/sitebinding"
)

type fakeMetafileFetchAdapter struct {
	raw              []byte
	opened           int
	lastCredential   string
	forcedFetchError error
	configError      error
	// linked makes the fake an API-key site whose route spends linkRequests
	// before the metafile GET; requestSkew misreports the session total.
	linked       bool
	linkRequests int
	requestSkew  int
}

func (adapter *fakeMetafileFetchAdapter) authMethod() domain.AuthMethod {
	if adapter.linked {
		return domain.AuthMethodAPIKeyHeader
	}
	return domain.AuthMethodCookieHeader
}

func (adapter *fakeMetafileFetchAdapter) Descriptor() domain.SiteDescriptor {
	return domain.SiteDescriptor{
		ID: "fakept", Name: "Fake PT", BaseURL: "https://fake.invalid/", Stability: "test",
		AuthMethods:  []domain.AuthMethod{adapter.authMethod()},
		Capabilities: []domain.Capability{domain.CapabilityMetafile},
	}
}

func (adapter *fakeMetafileFetchAdapter) ValidateMetafileRef(ref domain.TorrentRef) error {
	if ref.SiteID != "fakept" || ref.RemoteID != "42" {
		return fmt.Errorf("invalid fake ref")
	}
	return nil
}

func (adapter *fakeMetafileFetchAdapter) MetafileFetchConfig() (site.MetafileFetchConfig, error) {
	if adapter.configError != nil {
		return site.MetafileFetchConfig{}, adapter.configError
	}
	if adapter.linked {
		return site.NewLinkedMetafileFetchConfig("https://fake.invalid", "fake.download_by_id.v1", 2)
	}
	return site.NewMetafileFetchConfig("https://fake.invalid", "fake.download_by_id.v1")
}

func (adapter *fakeMetafileFetchAdapter) OpenMetafileFetchSession(ctx context.Context, credential site.Credential) (site.MetafileFetchSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if credential.Method() != adapter.authMethod() {
		return nil, fmt.Errorf("wrong auth")
	}
	adapter.opened++
	adapter.lastCredential = credential.SecretValue()
	return &fakeMetafileFetchSession{raw: bytes.Clone(adapter.raw), forcedError: adapter.forcedFetchError, linkRequests: adapter.linkRequests, requestSkew: adapter.requestSkew}, nil
}

type fakeMetafileFetchSession struct {
	raw          []byte
	forcedError  error
	requests     int
	closed       bool
	linkRequests int
	requestSkew  int
}

func (session *fakeMetafileFetchSession) FetchMetafile(ctx context.Context, ref domain.TorrentRef, limits site.MetafileFetchLimits) (*site.FetchedMetafile, site.MetafileFetchReceipt, error) {
	start := time.Now().UTC()
	receipt := site.MetafileFetchReceipt{
		Effect: site.MetafileFetchEffect, Ref: ref, Origin: "https://fake.invalid", RouteID: "fake.download_by_id.v1",
		ObservedAtStart: start, Limits: limits,
	}
	if err := ctx.Err(); err != nil {
		receipt.ObservedAtEnd = time.Now().UTC()
		receipt.StopReason = "context_cancelled"
		return nil, receipt, err
	}
	if session.closed || session.requests != 0 {
		receipt.ObservedAtEnd = time.Now().UTC()
		receipt.StopReason = "request_budget_exhausted"
		return nil, receipt, fmt.Errorf("request budget exhausted")
	}
	session.requests += 1 + session.linkRequests
	receipt.Used.RequestsAttempted = 1
	receipt.Used.LinkRequestsAttempted = session.linkRequests
	if session.forcedError != nil {
		receipt.ObservedAtEnd = time.Now().UTC()
		receipt.StopReason = "site.synthetic_failure"
		return nil, receipt, session.forcedError
	}
	end := time.Now().UTC()
	fetched, err := site.NewLinkedFetchedMetafile(ref, receipt.Origin, receipt.RouteID, start, end, session.linkRequests, session.raw)
	if err != nil {
		return nil, receipt, err
	}
	receipt.ObservedAtEnd = end
	receipt.Complete = true
	receipt.Used.ResponseBytesRead = int64(len(session.raw))
	receipt.Used.ResponseBytesKnown = true
	return fetched, receipt, nil
}

func (session *fakeMetafileFetchSession) RequestsMade() int {
	return session.requests + session.requestSkew
}
func (session *fakeMetafileFetchSession) Close() error {
	session.closed = true
	return nil
}

func TestSiteMetafileFetchStoresExactVariantAndHidesSecrets(t *testing.T) {
	storeRoot := filepath.Join(physicalCLITempDir(t), "FETCH-STORE-PATH-CANARY")
	store, _, err := metastore.Init(storeRoot)
	if err != nil || store == nil {
		t.Fatalf("initialize store: %v", err)
	}
	const (
		name    = "FETCH-METAFILE-NAME-CANARY.bin"
		passkey = "FETCH-PASSKEY-CANARY"
		cookie  = "SID=FETCH-COOKIE-CANARY"
	)
	adapter := &fakeMetafileFetchAdapter{raw: sitePrivateTrackerMetafile(name, []byte("x"), passkey)}
	var out, errOut bytes.Buffer
	a := &app{stdin: strings.NewReader(cookie + "\r\n"), stdout: &out, stderr: &errOut, registry: site.NewRegistry(adapter)}
	if err := a.siteMetafileFetch([]string{"--cookie-stdin", "--acknowledge-site-effect", "--metafile-store", storeRoot, "--output", "json", "fakept", "42"}); err != nil {
		t.Fatalf("fetch failed: %v stdout=%s", err, out.String())
	}
	if adapter.opened != 1 || adapter.lastCredential != cookie {
		t.Fatalf("unexpected session: opened=%d credential=%q", adapter.opened, adapter.lastCredential)
	}
	var envelope struct {
		Kind string `json:"kind"`
		Data struct {
			Outcome         string `json:"outcome"`
			WritesPerformed int    `json:"writes_performed"`
			Binding         struct {
				Status      string `json:"status"`
				Observation struct {
					Origin            string `json:"origin"`
					RouteID           string `json:"route_id"`
					MetafileVariantID string `json:"metafile_variant_id"`
				} `json:"observation"`
			} `json:"binding"`
			Persistent struct {
				Outcome         string `json:"outcome"`
				WritesPerformed int    `json:"writes_performed"`
				Record          struct {
					ID string `json:"record_id"`
				} `json:"record"`
				Assurance struct {
					RecordVerified   bool `json:"record_digest_verified"`
					ArtifactVerified bool `json:"referenced_artifact_verified"`
					SameStore        bool `json:"same_store_operation_bound"`
				} `json:"assurance"`
			} `json:"persistent_binding"`
			Request struct {
				RequestsMade int `json:"requests_made"`
				Retries      int `json:"retries"`
				Redirects    int `json:"redirects"`
			} `json:"request"`
			Blockers json.RawMessage `json:"blockers"`
			Issues   json.RawMessage `json:"issues"`
			Warnings json.RawMessage `json:"warnings"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Kind != "site.metafile.fetch" || envelope.Data.Outcome != "stored" || envelope.Data.WritesPerformed != 2 || envelope.Data.Binding.Status != "observed_exact_variant" || envelope.Data.Binding.Observation.Origin != "https://fake.invalid" || envelope.Data.Binding.Observation.RouteID != "fake.download_by_id.v1" || !strings.HasPrefix(envelope.Data.Binding.Observation.MetafileVariantID, "sha256:") || envelope.Data.Request.RequestsMade != 1 || envelope.Data.Request.Retries != 0 || envelope.Data.Request.Redirects != 0 || envelope.Data.Persistent.Outcome != "stored" || envelope.Data.Persistent.WritesPerformed != 1 || !strings.HasPrefix(envelope.Data.Persistent.Record.ID, "sha256:") || !envelope.Data.Persistent.Assurance.RecordVerified || !envelope.Data.Persistent.Assurance.ArtifactVerified || !envelope.Data.Persistent.Assurance.SameStore {
		t.Fatalf("unexpected report: %s", out.String())
	}
	assertJSONArray(t, "fetch blockers", envelope.Data.Blockers)
	assertJSONArray(t, "fetch issues", envelope.Data.Issues)
	assertJSONArray(t, "fetch warnings", envelope.Data.Warnings)
	assertJSONStringsExclude(t, out.Bytes(), storeRoot, name, passkey, cookie, "announce?passkey")
	if strings.Contains(out.String(), "link_request") || !strings.Contains(out.String(), `"scope": "one_remote_metafile_get"`) ||
		!strings.Contains(out.String(), `"effectful_same_origin_get_for_remote_id"`) || len(envelope.Data.Warnings) == 0 || strings.Contains(string(envelope.Data.Warnings), "link") {
		t.Fatalf("direct route report changed: %s", out.String())
	}

	out.Reset()
	a = &app{stdin: strings.NewReader(cookie + "\n"), stdout: &out, stderr: &errOut, registry: site.NewRegistry(adapter)}
	if err := a.siteMetafileFetch([]string{"--cookie-stdin", "--acknowledge-site-effect", "--metafile-store", storeRoot, "--output", "json", "fakept", "42"}); err != nil {
		t.Fatalf("repeat fetch failed: %v stdout=%s", err, out.String())
	}
	var repeated struct {
		Data struct {
			Outcome         string `json:"outcome"`
			WritesPerformed int    `json:"writes_performed"`
			Binding         struct {
				Status string `json:"status"`
			} `json:"binding"`
			Persistent struct {
				Record struct {
					ID string `json:"record_id"`
				} `json:"record"`
			} `json:"persistent_binding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &repeated); err != nil {
		t.Fatal(err)
	}
	if repeated.Data.Outcome != "stored" || repeated.Data.WritesPerformed != 1 || repeated.Data.Binding.Status != "observed_exact_variant" || repeated.Data.Persistent.Record.ID == envelope.Data.Persistent.Record.ID {
		t.Fatalf("a new historical observation was not sealed separately: %s", out.String())
	}
}

func TestSiteMetafileFetchRejectsNonPrivateResponseBeforeStoreWrite(t *testing.T) {
	storeRoot := filepath.Join(physicalCLITempDir(t), "store")
	if _, _, err := metastore.Init(storeRoot); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeMetafileFetchAdapter{raw: testV1Metafile("public.bin", []byte("x"))}
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader("SID=value\n"), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	err := a.siteMetafileFetch([]string{"--cookie-stdin", "--acknowledge-site-effect", "--metafile-store", storeRoot, "--output", "json", "fakept", "42"})
	var integrity *integrityErr
	if !errors.As(err, &integrity) {
		t.Fatalf("expected integrity error, got %v stdout=%s", err, out.String())
	}
	if !strings.Contains(out.String(), `"outcome": "integrity_failed"`) || !strings.Contains(out.String(), "artifact.not_private") || !strings.Contains(out.String(), `"writes_performed": 0`) || strings.Contains(out.String(), "observed_exact_variant") {
		t.Fatalf("unsafe non-private report: %s", out.String())
	}
	store, err := metastore.Open(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := store.ListRecords(context.Background(), metastore.RecordKindSiteMetafileBindingV1, metastore.DefaultRecordLimits())
	if err != nil || len(listed.Records) != 0 {
		t.Fatalf("invalid response left a persistent site binding: records=%v err=%v", listed.Records, err)
	}
}

func TestPersistentSiteBindingAssuranceIsOperationScoped(t *testing.T) {
	record := metastore.RecordRef{Kind: metastore.RecordKindSiteMetafileBindingV1, ID: metastore.RecordID("sha256:" + strings.Repeat("1", 64)), SizeBytes: 10}
	already, blocker := siteFetchPersistentBinding(record, sitebinding.SealReceipt{AlreadyPresent: true, Record: record}, nil)
	if blocker != "" || already.Outcome != "already_present" || !already.Assurance.RecordDigestVerified || !already.Assurance.ReferencedArtifactVerified || !already.Assurance.SameStoreOperationBound || already.Assurance.AtomicNoClobber || already.Assurance.DurabilityConfirmed {
		t.Fatalf("historical publication was overstated: %#v blocker=%q", already, blocker)
	}
	unconfirmed, blocker := siteFetchPersistentBinding(record, sitebinding.SealReceipt{WritesPerformed: 1, Record: record}, metastore.ErrDurabilityUnconfirmed)
	if blocker != "binding.durability_unconfirmed" || unconfirmed.Outcome != "published_durability_unconfirmed" || unconfirmed.Assurance.RecordDigestVerified || unconfirmed.Assurance.ReferencedArtifactVerified || unconfirmed.Assurance.DurabilityConfirmed {
		t.Fatalf("unconfirmed publication was overstated: %#v blocker=%q", unconfirmed, blocker)
	}
}

func TestSiteMetafileFetchPostImportIdentityFailuresDoNotContradictPersistenceJSON(t *testing.T) {
	config, err := site.NewMetafileFetchConfig("https://fake.invalid", "fake.download_by_id.v1")
	if err != nil {
		t.Fatal(err)
	}
	for _, cause := range []string{"binding.exact_variant_not_established", "binding.import_identity_disagrees"} {
		t.Run(cause, func(t *testing.T) {
			report := newSiteMetafileFetchReport(
				domain.TorrentRef{SiteID: "fakept", RemoteID: "42"}, config,
				site.DefaultMetafileFetchLimits(), metastore.DefaultLimits(), false, "PRIVATE-STORE-PATH-CANARY",
			)
			report.Outcome = "published_post_commit_failure"
			report.WritesPerformed = 1
			report.Blockers = []string{cause}
			report.StoreOperation.Outcome = "stored"
			report.StoreOperation.WritesPerformed = 1
			report.StoreOperation.Assurance.DurabilityConfirmed = true
			report.Persistent = siteFetchPersistenceNotAttempted(nil)

			var out bytes.Buffer
			a := &app{stdout: &out}
			if err := a.writeSiteMetafileFetchReport("json", report); err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Data struct {
					Blockers   []string `json:"blockers"`
					Persistent struct {
						Outcome      string   `json:"outcome"`
						Publication  string   `json:"publication"`
						BlockerCodes []string `json:"blocker_codes"`
					} `json:"persistent_binding"`
				} `json:"data"`
			}
			if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if len(envelope.Data.Blockers) != 1 || envelope.Data.Blockers[0] != cause ||
				envelope.Data.Persistent.Outcome != "blocked" || envelope.Data.Persistent.Publication != "not_attempted" ||
				len(envelope.Data.Persistent.BlockerCodes) != 1 || envelope.Data.Persistent.BlockerCodes[0] != "binding.persistence_not_attempted" {
				t.Fatalf("post-import failure was reported inconsistently: %s", out.String())
			}
			if strings.Contains(out.String(), "binding.artifact_not_durably_imported") {
				t.Fatalf("successful artifact import was contradicted: %s", out.String())
			}
		})
	}
}

func TestSiteMetafileFetchGatesBeforeCredentialRead(t *testing.T) {
	missingStore := filepath.Join(physicalCLITempDir(t), "missing-store")
	tests := [][]string{
		{"site", "metafile", "fetch", "--cookie-stdin", "--metafile-store", missingStore, "tjupt", "42"},
		{"site", "metafile", "fetch", "--cookie-stdin", "--acknowledge-site-effect", "--metafile-store", missingStore, "--output", "yaml", "tjupt", "42"},
		{"site", "metafile", "fetch", "--cookie-stdin", "--acknowledge-site-effect", "--metafile-store", missingStore, "tjupt", "not-decimal"},
		{"site", "metafile", "fetch", "--cookie-stdin", "--acknowledge-site-effect", "--metafile-store", missingStore, "tjupt", "42"},
	}
	for _, args := range tests {
		reader := &trackingReader{}
		var out, errOut bytes.Buffer
		if code := Run(args, reader, &out, &errOut); code == 0 {
			t.Fatalf("expected failure for %v", args)
		}
		if reader.read {
			t.Fatalf("credential was read before preflight for %v", args)
		}
	}
	reader := &trackingReader{}
	adapter := &fakeMetafileFetchAdapter{configError: fmt.Errorf("COOKIE-CONFIG-CANARY")}
	a := &app{stdin: reader, stdout: ioDiscard{}, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	err := a.siteMetafileFetch([]string{"--cookie-stdin", "--acknowledge-site-effect", "--metafile-store", missingStore, "fakept", "42"})
	if err == nil || reader.read || adapter.opened != 0 || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("config preflight escaped its boundary: err=%v read=%t opened=%d", err, reader.read, adapter.opened)
	}
}

func TestSiteMetafileFetchHelpIsDiscoverable(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"site", "metafile", "fetch", "--help"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
			t.Fatalf("args=%v code=%d stderr=%q", args, code, errOut.String())
		}
		for _, wanted := range []string{"site metafile fetch", "--acknowledge-site-effect", "--cookie-stdin", "--metafile-store"} {
			if !strings.Contains(out.String(), wanted) {
				t.Fatalf("args=%v missing %q: %s", args, wanted, out.String())
			}
		}
	}
}

func TestSiteMetafileFetchHumanReportOrdersBlockersBeforeEvidence(t *testing.T) {
	storeRoot := filepath.Join(physicalCLITempDir(t), "HUMAN-STORE-PATH-CANARY")
	if _, _, err := metastore.Init(storeRoot); err != nil {
		t.Fatal(err)
	}
	const (
		name    = "HUMAN-NAME-CANARY.bin"
		passkey = "HUMAN-PASSKEY-CANARY"
		cookie  = "SID=HUMAN-COOKIE-CANARY"
	)
	adapter := &fakeMetafileFetchAdapter{raw: sitePrivateTrackerMetafile(name, []byte("x"), passkey)}
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader(cookie + "\n"), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	if err := a.siteMetafileFetch([]string{"--cookie-stdin", "--acknowledge-site-effect", "--metafile-store", storeRoot, "fakept", "42"}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	blockers := strings.Index(text, "BLOCKERS")
	siteSection := strings.Index(text, "REMOTE ID")
	storeSection := strings.Index(text, "STORE OUTCOME")
	if blockers < 0 || siteSection < blockers || storeSection < siteSection || !strings.Contains(text, "OBSERVED VARIANT") || !strings.Contains(text, "AUTOMATIC RETRIES") || !strings.Contains(text, "PERSISTENT BINDING") || !strings.Contains(text, "RECORD ID") || strings.Contains(text, "LINK REQUEST") {
		t.Fatalf("unclear human report: %s", text)
	}
	for _, secret := range []string{storeRoot, name, passkey, cookie} {
		if strings.Contains(text, secret) {
			t.Fatalf("human report leaked %q: %s", secret, text)
		}
	}
}

func TestEffectfulSiteCredentialIsCanonicalASCII(t *testing.T) {
	credential, err := readEffectfulSiteCredential(strings.NewReader("a=1; b=2\r\n"))
	if err != nil || credential.SecretValue() != "a=1; b=2" {
		t.Fatalf("canonical credential rejected: credential=%v err=%v", credential, err)
	}
	for _, value := range []string{" a=1\n", "a=1 \n", "a=1\n\n", "a=\u00a0\n", "\n"} {
		if _, err := readEffectfulSiteCredential(strings.NewReader(value)); err == nil {
			t.Fatalf("unsafe credential accepted: %q", value)
		}
	}
}

func TestEffectfulAPIKeyCredentialIsCanonicalASCII(t *testing.T) {
	for _, value := range []string{"KEY-CANARY-1", "KEY-CANARY-1\n", "KEY-CANARY-1\r\n"} {
		credential, err := readEffectfulAPIKeyCredential(strings.NewReader(value))
		if err != nil || credential.Method() != domain.AuthMethodAPIKeyHeader || credential.SecretValue() != "KEY-CANARY-1" {
			t.Fatalf("canonical API key %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{"", "\n", " KEY\n", "KEY \n", "K EY\n", "KEY\t\n", "KEY\n\n", "KEY \n"} {
		if _, err := readEffectfulAPIKeyCredential(strings.NewReader(value)); err == nil {
			t.Fatalf("unsafe API key accepted: %q", value)
		}
	}
}

func TestSiteMetafileFetchLinkedRouteAccountsForLinkRequests(t *testing.T) {
	const (
		name    = "LINKED-NAME-CANARY.bin"
		passkey = "LINKED-PASSKEY-CANARY"
		key     = "LINKED-API-KEY-CANARY"
	)
	for _, links := range []int{1, 2} {
		t.Run(strconv.Itoa(links), func(t *testing.T) {
			storeRoot := filepath.Join(physicalCLITempDir(t), "LINKED-STORE-PATH-CANARY")
			if _, _, err := metastore.Init(storeRoot); err != nil {
				t.Fatal(err)
			}
			adapter := &fakeMetafileFetchAdapter{raw: sitePrivateTrackerMetafile(name, []byte("x"), passkey), linked: true, linkRequests: links}
			var out bytes.Buffer
			a := &app{stdin: strings.NewReader(key + "\n"), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
			if err := a.siteMetafileFetch([]string{"--api-key-stdin", "--acknowledge-site-effect", "--metafile-store", storeRoot, "--output", "json", "fakept", "42"}); err != nil {
				t.Fatalf("linked fetch failed: %v stdout=%s", err, out.String())
			}
			if adapter.opened != 1 || adapter.lastCredential != key {
				t.Fatalf("unexpected session: opened=%d", adapter.opened)
			}
			var envelope struct {
				Data struct {
					Outcome         string `json:"outcome"`
					Acknowledgement struct {
						Scope string `json:"scope"`
					} `json:"acknowledgement"`
					Binding struct {
						Status        string   `json:"status"`
						EvidenceBasis []string `json:"evidence_basis"`
					} `json:"binding"`
					Persistent struct {
						Outcome string `json:"outcome"`
					} `json:"persistent_binding"`
					Request struct {
						RequestsMade int                       `json:"requests_made"`
						Redirects    int                       `json:"redirects"`
						Receipt      site.MetafileFetchReceipt `json:"receipt"`
					} `json:"request"`
					Warnings []string `json:"warnings"`
				} `json:"data"`
			}
			if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			data := envelope.Data
			if data.Outcome != "stored" || data.Persistent.Outcome != "stored" || data.Binding.Status != "observed_exact_variant" ||
				data.Acknowledgement.Scope != "site_link_requests_then_one_remote_metafile_get" ||
				len(data.Binding.EvidenceBasis) == 0 || data.Binding.EvidenceBasis[0] != "effectful_site_issued_link_get_for_remote_id" ||
				data.Request.RequestsMade != 1+links || data.Request.Redirects != 0 ||
				data.Request.Receipt.Limits.MaxLinkRequests != 2 || data.Request.Receipt.Used.LinkRequestsAttempted != links ||
				data.Request.Receipt.Used.RequestsAttempted != 1 || !strings.Contains(strings.Join(data.Warnings, "\n"), "download link is a credential") {
				t.Fatalf("unexpected linked report: %s", out.String())
			}
			assertJSONStringsExclude(t, out.Bytes(), storeRoot, name, passkey, key, "announce?passkey")

			out.Reset()
			a = &app{stdin: strings.NewReader(key + "\n"), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
			if err := a.siteMetafileFetch([]string{"--api-key-stdin", "--acknowledge-site-effect", "--metafile-store", storeRoot, "fakept", "42"}); err != nil {
				t.Fatal(err)
			}
			if text := out.String(); !strings.Contains(text, "LINK REQUEST LIMIT  2\n") || !strings.Contains(text, "LINK REQUESTS USED  "+strconv.Itoa(links)+"\n") ||
				strings.Contains(text, key) || strings.Contains(text, passkey) {
				t.Fatalf("unexpected linked human report: %s", text)
			}
		})
	}
}

func TestSiteMetafileFetchLinkedRouteRejectsInconsistentAccounts(t *testing.T) {
	for _, test := range []struct {
		name         string
		linkRequests int
		skew         int
	}{
		{name: "uncounted link request", linkRequests: 1, skew: -1},
		{name: "unreported request", linkRequests: 1, skew: 1},
		{name: "link request over budget", linkRequests: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			storeRoot := filepath.Join(physicalCLITempDir(t), "store")
			if _, _, err := metastore.Init(storeRoot); err != nil {
				t.Fatal(err)
			}
			adapter := &fakeMetafileFetchAdapter{raw: sitePrivateTrackerMetafile("x.bin", []byte("x"), "INCONSISTENT-CANARY"), linked: true, linkRequests: test.linkRequests, requestSkew: test.skew}
			var out bytes.Buffer
			a := &app{stdin: strings.NewReader("KEY\n"), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
			if err := a.siteMetafileFetch([]string{"--api-key-stdin", "--acknowledge-site-effect", "--metafile-store", storeRoot, "--output", "json", "fakept", "42"}); err == nil {
				t.Fatalf("inconsistent account accepted: %s", out.String())
			}
			if !strings.Contains(out.String(), `"outcome": "blocked"`) || strings.Contains(out.String(), "observed_exact_variant") || strings.Contains(out.String(), `"writes_performed": 1`) {
				t.Fatalf("inconsistent account was published: %s", out.String())
			}
		})
	}
}

func TestSiteMetafileFetchNamesAndChecksTheCredentialFlagBeforeReadingIt(t *testing.T) {
	missingStore := filepath.Join(physicalCLITempDir(t), "missing-store")
	flags := []string{"--acknowledge-site-effect", "--metafile-store", missingStore}
	for _, test := range []struct {
		adapter *fakeMetafileFetchAdapter
		args    []string
		want    string
	}{
		{adapter: &fakeMetafileFetchAdapter{linked: true}, args: []string{"fakept", "42"}, want: "--api-key-stdin is required; credentials are never accepted in argv"},
		{adapter: &fakeMetafileFetchAdapter{linked: true}, args: []string{"--cookie-stdin", "fakept", "42"}, want: `site "fakept" does not support cookie_header authentication`},
		{adapter: &fakeMetafileFetchAdapter{}, args: []string{"fakept", "42"}, want: "--cookie-stdin is required; credentials are never accepted in argv"},
		{adapter: &fakeMetafileFetchAdapter{}, args: []string{"--api-key-stdin", "fakept", "42"}, want: `site "fakept" does not support api_key_header authentication`},
		{adapter: &fakeMetafileFetchAdapter{}, args: []string{"--cookie-stdin", "--api-key-stdin", "fakept", "42"}, want: "--cookie-stdin and --api-key-stdin are mutually exclusive"},
	} {
		secret := &trackingReader{}
		application := &app{stdin: secret, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, registry: site.NewRegistry(test.adapter)}
		if err := application.siteMetafileFetch(append(append([]string{}, flags...), test.args...)); err == nil || err.Error() != test.want || secret.read || test.adapter.opened != 0 {
			t.Fatalf("args=%q err=%v secretRead=%t", test.args, err, secret.read)
		}
	}
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"mteam", "1"}, want: "--api-key-stdin is required; credentials are never accepted in argv"},
		{args: []string{"tjupt", "1"}, want: "--cookie-stdin is required; credentials are never accepted in argv"},
		{args: []string{"--api-key-stdin", "tjupt", "1"}, want: `site "tjupt" does not support api_key_header authentication`},
		{args: []string{"--cookie-stdin", "mteam", "1"}, want: `site "mteam" does not support cookie_header authentication`},
	} {
		secret := &trackingReader{}
		var out, errOut bytes.Buffer
		args := append(append([]string{"site", "metafile", "fetch"}, flags...), test.args...)
		if code := Run(args, secret, &out, &errOut); code != 2 || out.Len() != 0 || secret.read || !strings.Contains(errOut.String(), test.want) {
			t.Fatalf("args=%q code=%d stdout=%q stderr=%q secretRead=%t", test.args, code, out.String(), errOut.String(), secret.read)
		}
	}
}

func TestSiteMetafileFetchRedactsAdapterErrorsAndReceiptText(t *testing.T) {
	storeRoot := filepath.Join(physicalCLITempDir(t), "store")
	if _, _, err := metastore.Init(storeRoot); err != nil {
		t.Fatal(err)
	}
	const canary = "COOKIE-URL-BODY-PASSKEY-CANARY"
	adapter := &fakeMetafileFetchAdapter{raw: []byte("d"), forcedFetchError: fmt.Errorf("https://fake.invalid/download.php?id=%s Cookie=%s", canary, canary)}
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader("SID=" + canary + "\n"), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	err := a.siteMetafileFetch([]string{"--cookie-stdin", "--acknowledge-site-effect", "--metafile-store", storeRoot, "--output", "json", "fakept", "42"})
	if err == nil || strings.Contains(err.Error(), canary) || strings.Contains(out.String(), canary) {
		t.Fatalf("adapter secret escaped: err=%v stdout=%s", err, out.String())
	}
	if !strings.Contains(out.String(), `"stop_reason": "site.fetch_failed"`) || !strings.Contains(out.String(), `"requests_made": 1`) || !strings.Contains(out.String(), `"writes_performed": 0`) {
		t.Fatalf("unsafe or unauditable failure report: %s", out.String())
	}
	limits := site.DefaultMetafileFetchLimits()
	config, err := site.NewMetafileFetchConfig("https://fake.invalid", "fake.download_by_id.v1")
	if err != nil {
		t.Fatal(err)
	}
	poisoned := publicMetafileFetchReceipt(
		domain.TorrentRef{SiteID: "fakept", RemoteID: "42"}, config, limits,
		site.MetafileFetchReceipt{
			Ref: domain.TorrentRef{SiteID: canary, RemoteID: canary}, Origin: "https://" + canary, RouteID: canary,
			Limits: limits, StopReason: canary,
		},
		false,
	)
	encoded, err := json.Marshal(poisoned)
	if err != nil || strings.Contains(string(encoded), canary) || poisoned.StopReason != "site.fetch_failed" || poisoned.Ref.SiteID != "fakept" || poisoned.RouteID != config.RouteID {
		t.Fatalf("poisoned receipt escaped: %s err=%v", encoded, err)
	}
}

func TestSuccessfulMetafileFetchReceiptMustMatchAuthority(t *testing.T) {
	limits := site.DefaultMetafileFetchLimits()
	ref := domain.TorrentRef{SiteID: "fakept", RemoteID: "42"}
	config, err := site.NewMetafileFetchConfig("https://fake.invalid", "fake.download_by_id.v1")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC()
	end := start.Add(time.Millisecond)
	fetched, err := site.NewFetchedMetafile(ref, config.Origin, config.RouteID, start, end, []byte("d"))
	if err != nil {
		t.Fatal(err)
	}
	receipt := site.MetafileFetchReceipt{
		Effect: site.MetafileFetchEffect, Ref: ref, Origin: "https://fake.invalid", RouteID: "fake.download_by_id.v1",
		ObservedAtStart: start, ObservedAtEnd: end, Complete: true, Limits: limits,
		Used: site.MetafileFetchUsage{RequestsAttempted: 1, ResponseBytesRead: 1, ResponseBytesKnown: true},
	}
	if err := validateSuccessfulMetafileFetch(ref, config, limits, fetched, receipt, 1); err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*site.MetafileFetchReceipt)
	}{
		{"ref", func(item *site.MetafileFetchReceipt) { item.Ref.RemoteID = "43" }},
		{"origin", func(item *site.MetafileFetchReceipt) { item.Origin = "https://other.invalid" }},
		{"route", func(item *site.MetafileFetchReceipt) { item.RouteID = "other.download.v1" }},
		{"time", func(item *site.MetafileFetchReceipt) { item.ObservedAtEnd = item.ObservedAtEnd.Add(time.Second) }},
		{"bytes", func(item *site.MetafileFetchReceipt) { item.Used.ResponseBytesRead = 2 }},
		{"retry", func(item *site.MetafileFetchReceipt) { item.Used.AutomaticRetries = 1 }},
		{"redirect", func(item *site.MetafileFetchReceipt) { item.Used.RedirectsFollowed = 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := receipt
			test.mutate(&changed)
			if err := validateSuccessfulMetafileFetch(ref, config, limits, fetched, changed, 1); err == nil {
				t.Fatalf("changed %s receipt was accepted", test.name)
			}
		})
	}
}

func sitePrivateTrackerMetafile(name string, content []byte, passkey string) []byte {
	announce := "https://tracker.invalid/announce?passkey=" + passkey
	piece := sha1.Sum(content)
	result := []byte("d8:announce" + strconv.Itoa(len(announce)) + ":" + announce + "4:infod6:lengthi" + strconv.Itoa(len(content)) + "e4:name" + strconv.Itoa(len(name)) + ":" + name + "12:piece lengthi" + strconv.Itoa(max(1, len(content))) + "e6:pieces20:")
	result = append(result, piece[:]...)
	return append(result, []byte("7:privatei1eee")...)
}

// ioDiscard avoids sharing stderr buffers with assertions that intentionally
// exercise report-first error paths.
type ioDiscard struct{}

func (ioDiscard) Write(value []byte) (int, error) { return len(value), nil }
