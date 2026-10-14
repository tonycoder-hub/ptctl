package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

const apiKeyCanary = "API-KEY-CANARY-0123456789"

type apiKeySiteAdapter struct {
	status  domain.SessionStatus
	results []domain.TorrentSummary
	err     error
	calls   int
	method  domain.AuthMethod
	secret  string
}

func (*apiKeySiteAdapter) Descriptor() domain.SiteDescriptor {
	return domain.SiteDescriptor{
		ID: "api-key-test", Name: "API key test", BaseURL: "https://api.invalid/", Stability: "test",
		AuthMethods:  []domain.AuthMethod{domain.AuthMethodAPIKeyHeader},
		Capabilities: []domain.Capability{domain.CapabilityAuthCheck, domain.CapabilitySearch},
	}
}

func (adapter *apiKeySiteAdapter) record(credential site.Credential) {
	adapter.calls++
	adapter.method, adapter.secret = credential.Method(), credential.SecretValue()
}

func (adapter *apiKeySiteAdapter) CheckSession(_ context.Context, credential site.Credential) (domain.SessionStatus, error) {
	adapter.record(credential)
	return adapter.status, adapter.err
}

func (adapter *apiKeySiteAdapter) Search(_ context.Context, credential site.Credential, _ string) ([]domain.TorrentSummary, error) {
	adapter.record(credential)
	return adapter.results, adapter.err
}

func TestSiteReadSendsAPIKeyFromStdin(t *testing.T) {
	adapter := &apiKeySiteAdapter{status: domain.SessionStatus{
		State: domain.AuthenticationAuthenticated, Username: "Alice", ObservedAt: time.Date(2026, 10, 14, 1, 2, 3, 0, time.UTC),
	}}
	var out, errOut bytes.Buffer
	application := &app{stdin: strings.NewReader(apiKeyCanary + "\n"), stdout: &out, stderr: &errOut, registry: site.NewRegistry(adapter)}
	if err := application.siteRead("status", []string{"--api-key-stdin", "--output", "json", "api-key-test"}); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 1 || adapter.method != domain.AuthMethodAPIKeyHeader || adapter.secret != apiKeyCanary ||
		errOut.Len() != 0 || strings.Contains(out.String(), "CANARY") {
		t.Fatalf("calls=%d method=%q secret=%q stdout=%q stderr=%q", adapter.calls, adapter.method, adapter.secret, out.String(), errOut.String())
	}
	var envelope struct {
		Kind string               `json:"kind"`
		Data domain.SessionStatus `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil || envelope.Kind != "site.session" || envelope.Data != adapter.status {
		t.Fatalf("envelope=%#v err=%v", envelope, err)
	}
}

func TestSiteReadNamesTheCredentialFlagASiteNeeds(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"site", "status", "mteam"}, want: "--api-key-stdin is required; credentials are never accepted in argv"},
		{args: []string{"site", "search", "mteam", "query"}, want: "--api-key-stdin is required; credentials are never accepted in argv"},
		{args: []string{"site", "status", "tjupt"}, want: "--cookie-stdin is required; credentials are never accepted in argv"},
		{args: []string{"site", "account", "unknown-site"}, want: "--cookie-stdin is required; credentials are never accepted in argv"},
		{args: []string{"site", "status", "--cookie-stdin", "--api-key-stdin", "mteam"}, want: "--cookie-stdin and --api-key-stdin are mutually exclusive"},
	}
	for _, test := range tests {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			secret := &trackingReader{}
			var out, errOut bytes.Buffer
			if code := Run(test.args, secret, &out, &errOut); code != 2 || out.Len() != 0 || secret.read || !strings.Contains(errOut.String(), test.want) {
				t.Fatalf("code=%d stdout=%q stderr=%q secretRead=%t", code, out.String(), errOut.String(), secret.read)
			}
		})
	}
}

func TestSiteReadRejectsAnUnsupportedCredentialKindBeforeReadingIt(t *testing.T) {
	tests := []struct {
		adapter site.Adapter
		args    []string
		want    string
	}{
		{adapter: &apiKeySiteAdapter{}, args: []string{"--cookie-stdin", "api-key-test"}, want: `site "api-key-test" does not support cookie_header authentication`},
		{adapter: &failingStatusAdapter{}, args: []string{"--api-key-stdin", "failing-status"}, want: `site "failing-status" does not support api_key_header authentication`},
	}
	for _, test := range tests {
		secret := &trackingReader{}
		application := &app{stdin: secret, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, registry: site.NewRegistry(test.adapter)}
		if err := application.siteRead("status", test.args); err == nil || err.Error() != test.want || secret.read {
			t.Fatalf("err=%v secretRead=%t", err, secret.read)
		}
	}
}

func TestSiteCommandsReportUnknownSitesAndUndeclaredCapabilitiesAsUsage(t *testing.T) {
	store := t.TempDir()
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"site", "capabilities", "no-such-site"}, want: `unknown site "no-such-site"`},
		{args: []string{"site", "status", "--cookie-stdin", "no-such-site"}, want: `unknown site "no-such-site"`},
		{args: []string{"site", "search", "--cookie-stdin", "no-such-site", "query"}, want: `unknown site "no-such-site"`},
		{args: []string{"site", "detail", "--cookie-stdin", "no-such-site", "1"}, want: `unknown site "no-such-site"`},
		{args: []string{"site", "bonus", "review", "--cookie-stdin", "no-such-site", "option"}, want: `unknown site "no-such-site"`},
		{args: []string{"site", "metafile", "fetch", "--cookie-stdin", "--acknowledge-site-effect", "--metafile-store", store, "no-such-site", "1"}, want: `unknown site "no-such-site"`},
		{args: []string{"site", "account", "--api-key-stdin", "api-key-test"}, want: `site "api-key-test" does not declare capability "account.read"`},
		{args: []string{"site", "status", "--cookie-stdin", "api-key-test"}, want: `site "api-key-test" does not support cookie_header authentication`},
		{args: []string{"site", "detail", "--cookie-stdin", "api-key-test", "1"}, want: `site "api-key-test" does not declare capability "torrent.detail"`},
		{args: []string{"site", "metafile", "fetch", "--cookie-stdin", "--acknowledge-site-effect", "--metafile-store", store, "api-key-test", "1"}, want: `site "api-key-test" does not declare capability "torrent.metafile.read_effectful"`},
	}
	for _, test := range tests {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			secret := &trackingReader{}
			var out, errOut bytes.Buffer
			application := &app{stdin: secret, stdout: &out, stderr: &errOut, registry: site.NewRegistry(&apiKeySiteAdapter{})}
			if code := runWithApp(test.args, application); code != 2 || out.Len() != 0 || secret.read || !strings.Contains(errOut.String(), test.want) || !strings.Contains(errOut.String(), "run 'pt help' for usage") {
				t.Fatalf("code=%d stdout=%q stderr=%q secretRead=%t", code, out.String(), errOut.String(), secret.read)
			}
		})
	}
}

func TestSiteReadRejectsAnEmptyAPIKey(t *testing.T) {
	adapter := &apiKeySiteAdapter{}
	application := &app{stdin: strings.NewReader(" \n"), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, registry: site.NewRegistry(adapter)}
	if err := application.siteRead("status", []string{"--api-key-stdin", "api-key-test"}); err == nil || err.Error() != "API key read from stdin is empty" || adapter.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, adapter.calls)
	}
}

func TestSiteReadNamesSecretFreeFailureCauses(t *testing.T) {
	for _, cause := range []error{site.ErrCredentialMalformed, site.ErrCredentialRejected, site.ErrRateLimited, site.ErrAddressBlocked} {
		adapter := &apiKeySiteAdapter{err: fmt.Errorf("RESPONSE-CANARY %s: %w", apiKeyCanary, cause)}
		application := &app{stdin: strings.NewReader(apiKeyCanary), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, registry: site.NewRegistry(adapter)}
		err := application.siteRead("status", []string{"--api-key-stdin", "api-key-test"})
		if err == nil || err.Error() != "site status failed: "+cause.Error() || strings.Contains(err.Error(), "CANARY") {
			t.Fatalf("err=%v", err)
		}
	}
	adapter := &apiKeySiteAdapter{err: errors.New("RESPONSE-CANARY")}
	application := &app{stdin: strings.NewReader(apiKeyCanary), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, registry: site.NewRegistry(adapter)}
	if err := application.siteRead("status", []string{"--api-key-stdin", "api-key-test"}); err == nil || err.Error() != "site status failed" {
		t.Fatalf("err=%v", err)
	}
}

func TestSiteSearchTableAddsSwarmAndPromotionColumnsOnlyWhenReported(t *testing.T) {
	size := int64(3 << 30)
	seeders, leechers := 12, 3
	ends := time.Date(2026, 10, 15, 4, 0, 0, 0, time.UTC)
	extended := []domain.TorrentSummary{
		{Ref: domain.TorrentRef{SiteID: "api-key-test", RemoteID: "1"}, Name: "Free", SizeBytes: &size, Seeders: &seeders, Leechers: &leechers,
			Promotion: &domain.Promotion{UploadFactor: "1", DownloadFactor: "0", EndsAt: &ends}},
		{Ref: domain.TorrentRef{SiteID: "api-key-test", RemoteID: "2"}, Name: "Double half",
			Promotion: &domain.Promotion{UploadFactor: "2", DownloadFactor: "0.5"}},
		{Ref: domain.TorrentRef{SiteID: "api-key-test", RemoteID: "3"}, Name: "Normal", Promotion: &domain.Promotion{UploadFactor: "1", DownloadFactor: "1"}},
		{Ref: domain.TorrentRef{SiteID: "api-key-test", RemoteID: "4"}, Name: "Unrecognized"},
	}
	plain := []domain.TorrentSummary{{Ref: domain.TorrentRef{SiteID: "api-key-test", RemoteID: "1"}, Name: "Plain", SizeBytes: &size}}
	tests := []struct {
		results []domain.TorrentSummary
		want    string
	}{
		{results: extended, want: "" +
			"REF             NAME          SIZE      SEEDERS  LEECHERS  PROMOTION\n" +
			"api-key-test/1  Free          3.00 GiB  12       3         free until 2026-10-15T04:00:00Z\n" +
			"api-key-test/2  Double half   unknown   -        -         up 2x, down 0.5x\n" +
			"api-key-test/3  Normal        unknown   -        -         -\n" +
			"api-key-test/4  Unrecognized  unknown   -        -         unknown\n"},
		{results: plain, want: "" +
			"REF             NAME   SIZE\n" +
			"api-key-test/1  Plain  3.00 GiB\n"},
	}
	for _, test := range tests {
		var out bytes.Buffer
		application := &app{stdin: strings.NewReader(apiKeyCanary), stdout: &out, stderr: &bytes.Buffer{}, registry: site.NewRegistry(&apiKeySiteAdapter{results: test.results})}
		if err := application.siteRead("search", []string{"--api-key-stdin", "api-key-test", "query"}); err != nil {
			t.Fatal(err)
		}
		if out.String() != test.want {
			t.Fatalf("stdout:\n%s\nwant:\n%s", out.String(), test.want)
		}
	}
}
