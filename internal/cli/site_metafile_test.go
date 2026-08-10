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
)

type fakeMetafileFetchAdapter struct {
	raw              []byte
	opened           int
	lastCredential   string
	forcedFetchError error
	configError      error
}

func (adapter *fakeMetafileFetchAdapter) Descriptor() domain.SiteDescriptor {
	return domain.SiteDescriptor{
		ID: "fakept", Name: "Fake PT", BaseURL: "https://fake.invalid/", Stability: "test",
		AuthMethods:  []domain.AuthMethod{domain.AuthMethodCookieHeader},
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
	return site.NewMetafileFetchConfig("https://fake.invalid", "fake.download_by_id.v1")
}

func (adapter *fakeMetafileFetchAdapter) OpenMetafileFetchSession(ctx context.Context, credential site.Credential) (site.MetafileFetchSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if credential.Method() != domain.AuthMethodCookieHeader {
		return nil, fmt.Errorf("wrong auth")
	}
	adapter.opened++
	adapter.lastCredential = credential.SecretValue()
	return &fakeMetafileFetchSession{raw: bytes.Clone(adapter.raw), forcedError: adapter.forcedFetchError}, nil
}

type fakeMetafileFetchSession struct {
	raw         []byte
	forcedError error
	requests    int
	closed      bool
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
	session.requests++
	receipt.Used.RequestsAttempted = 1
	if session.forcedError != nil {
		receipt.ObservedAtEnd = time.Now().UTC()
		receipt.StopReason = "site.synthetic_failure"
		return nil, receipt, session.forcedError
	}
	end := time.Now().UTC()
	fetched, err := site.NewFetchedMetafile(ref, receipt.Origin, receipt.RouteID, start, end, session.raw)
	if err != nil {
		return nil, receipt, err
	}
	receipt.ObservedAtEnd = end
	receipt.Complete = true
	receipt.Used.ResponseBytesRead = int64(len(session.raw))
	receipt.Used.ResponseBytesKnown = true
	return fetched, receipt, nil
}

func (session *fakeMetafileFetchSession) RequestsMade() int { return session.requests }
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
	if envelope.Kind != "site.metafile.fetch" || envelope.Data.Outcome != "stored" || envelope.Data.WritesPerformed != 1 || envelope.Data.Binding.Status != "observed_exact_variant" || envelope.Data.Binding.Observation.Origin != "https://fake.invalid" || envelope.Data.Binding.Observation.RouteID != "fake.download_by_id.v1" || !strings.HasPrefix(envelope.Data.Binding.Observation.MetafileVariantID, "sha256:") || envelope.Data.Request.RequestsMade != 1 || envelope.Data.Request.Retries != 0 || envelope.Data.Request.Redirects != 0 {
		t.Fatalf("unexpected report: %s", out.String())
	}
	assertJSONArray(t, "fetch blockers", envelope.Data.Blockers)
	assertJSONArray(t, "fetch issues", envelope.Data.Issues)
	assertJSONArray(t, "fetch warnings", envelope.Data.Warnings)
	assertJSONStringsExclude(t, out.Bytes(), storeRoot, name, passkey, cookie, "announce?passkey")

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
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &repeated); err != nil {
		t.Fatal(err)
	}
	if repeated.Data.Outcome != "already_present" || repeated.Data.WritesPerformed != 0 || repeated.Data.Binding.Status != "observed_exact_variant" {
		t.Fatalf("unexpected idempotent report: %s", out.String())
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
	if blockers < 0 || siteSection < blockers || storeSection < siteSection || !strings.Contains(text, "OBSERVED VARIANT") || !strings.Contains(text, "AUTOMATIC RETRIES") {
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
