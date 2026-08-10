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

type fakeTorrentDetailAdapter struct {
	configErr error
	openErr   error
	detail    domain.TorrentDetail
	receipt   site.TorrentDetailReceipt
	readErr   error
	closeErr  error
	opened    int
}

func (*fakeTorrentDetailAdapter) Descriptor() domain.SiteDescriptor {
	return domain.SiteDescriptor{
		ID: "fakept", Name: "Fake PT", BaseURL: "https://fake.invalid/", Stability: "test",
		AuthMethods:  []domain.AuthMethod{domain.AuthMethodCookieHeader},
		Capabilities: []domain.Capability{domain.CapabilityDetail},
	}
}

func (adapter *fakeTorrentDetailAdapter) ValidateTorrentDetailRef(ref domain.TorrentRef) error {
	if adapter.configErr != nil {
		return adapter.configErr
	}
	if ref != (domain.TorrentRef{SiteID: "fakept", RemoteID: "42"}) {
		return fmt.Errorf("invalid fake detail ref")
	}
	return nil
}

func (adapter *fakeTorrentDetailAdapter) TorrentDetailConfig() (site.TorrentDetailConfig, error) {
	if adapter.configErr != nil {
		return site.TorrentDetailConfig{}, adapter.configErr
	}
	return site.NewTorrentDetailConfig("https://fake.invalid", "fake.details_by_id.v1")
}

func (adapter *fakeTorrentDetailAdapter) OpenTorrentDetailSession(ctx context.Context, credential site.Credential) (site.TorrentDetailSession, error) {
	adapter.opened++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if credential.Method() != domain.AuthMethodCookieHeader || credential.SecretValue() == "" {
		return nil, fmt.Errorf("invalid credential")
	}
	if adapter.openErr != nil {
		return nil, adapter.openErr
	}
	return &fakeTorrentDetailSession{adapter: adapter}, nil
}

type fakeTorrentDetailSession struct {
	adapter  *fakeTorrentDetailAdapter
	requests int
	closed   bool
}

func (session *fakeTorrentDetailSession) ReadTorrentDetail(context.Context, domain.TorrentRef, site.TorrentDetailLimits) (domain.TorrentDetail, site.TorrentDetailReceipt, error) {
	session.requests++
	return session.adapter.detail, session.adapter.receipt, session.adapter.readErr
}

func (session *fakeTorrentDetailSession) RequestsMade() int { return session.requests }

func (session *fakeTorrentDetailSession) Close() error {
	if session.closed {
		return nil
	}
	session.closed = true
	return session.adapter.closeErr
}

func successfulFakeTorrentDetail(t *testing.T) *fakeTorrentDetailAdapter {
	t.Helper()
	ref := domain.TorrentRef{SiteID: "fakept", RemoteID: "42"}
	config, err := site.NewTorrentDetailConfig("https://fake.invalid", "fake.details_by_id.v1")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 11, 2, 3, 4, 0, time.UTC)
	return &fakeTorrentDetailAdapter{
		detail: domain.TorrentDetail{
			Ref: ref, DisplayTitle: "Safe Release", DownloadReferenceObserved: true,
			EvidenceBasis: []string{site.DetailBasisAuthenticated, site.DetailBasisExactRef, site.DetailBasisOneRequest, site.DetailBasisSiteClaimOnly, site.DetailBasisDownloadRef},
		},
		receipt: site.TorrentDetailReceipt{
			Effect: site.TorrentDetailReadEffect, Ref: ref, Origin: config.Origin, RouteID: config.RouteID,
			ObservedAtStart: start, ObservedAtEnd: start.Add(time.Second), Complete: true,
			Limits: site.DefaultTorrentDetailLimits(),
			Used:   site.TorrentDetailUsage{RequestsAttempted: 1, ResponseBytesRead: 1234, ResponseBytesKnown: true},
		},
	}
}

func TestSiteDetailSuccessJSONIsAuditableAndSecretFree(t *testing.T) {
	const cookie = "sid=COOKIE-DETAIL-CANARY"
	adapter := successfulFakeTorrentDetail(t)
	var out, errOut bytes.Buffer
	a := &app{stdin: strings.NewReader(cookie), stdout: &out, stderr: &errOut, registry: site.NewRegistry(adapter)}
	if err := a.siteDetail([]string{"--cookie-stdin", "--output", "json", "fakept", "42"}); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Kind string           `json:"kind"`
		Data siteDetailReport `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Kind != "site.torrent.detail" || envelope.Data.Outcome != "observed" || envelope.Data.Effect != site.TorrentDetailReadEffect ||
		envelope.Data.WritesPerformed != 0 || envelope.Data.Observation == nil || envelope.Data.Observation.DisplayTitle != "Safe Release" ||
		envelope.Data.Request.Status != "complete" || envelope.Data.Request.RequestsMade != 1 || !envelope.Data.Request.Receipt.Complete ||
		len(envelope.Data.Blockers) != 0 || len(envelope.Data.Warnings) != 2 || adapter.opened != 1 {
		t.Fatalf("envelope=%#v", envelope)
	}
	if strings.Contains(out.String(), cookie) || strings.Contains(out.String(), "details.php") {
		t.Fatalf("secret or request path leaked: %s", out.String())
	}
}

func TestSiteDetailUsageAndPreflightRejectBeforeCredentialRead(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		adapter site.Adapter
	}{
		{name: "missing cookie flag", args: []string{"fakept", "42"}, adapter: successfulFakeTorrentDetail(t)},
		{name: "invalid output", args: []string{"--cookie-stdin", "--output", "yaml", "fakept", "42"}, adapter: successfulFakeTorrentDetail(t)},
		{name: "invalid ref", args: []string{"--cookie-stdin", "fakept", "043"}, adapter: successfulFakeTorrentDetail(t)},
		{name: "config unavailable", args: []string{"--cookie-stdin", "fakept", "42"}, adapter: &fakeTorrentDetailAdapter{configErr: fmt.Errorf("COOKIE-CONFIG-CANARY")}},
		{name: "missing typed port", args: []string{"--cookie-stdin", "fakept", "42"}, adapter: fakeDetailDescriptorOnly{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &trackingReader{}
			var out bytes.Buffer
			a := &app{stdin: reader, stdout: &out, registry: site.NewRegistry(test.adapter)}
			if err := a.siteDetail(test.args); err == nil || reader.read || strings.Contains(err.Error(), "CANARY") || out.Len() != 0 {
				t.Fatalf("err=%v read=%t output=%q", err, reader.read, out.String())
			}
		})
	}
}

type fakeDetailDescriptorOnly struct{}

func (fakeDetailDescriptorOnly) Descriptor() domain.SiteDescriptor {
	return domain.SiteDescriptor{
		ID: "fakept", AuthMethods: []domain.AuthMethod{domain.AuthMethodCookieHeader},
		Capabilities: []domain.Capability{domain.CapabilityDetail},
	}
}

func TestSiteDetailFailureReportRedactsAdapterReceiptAndErrors(t *testing.T) {
	const canary = "COOKIE-URL-BODY-CANARY"
	adapter := successfulFakeTorrentDetail(t)
	adapter.detail = domain.TorrentDetail{}
	adapter.receipt.Ref = domain.TorrentRef{SiteID: canary, RemoteID: canary}
	adapter.receipt.Origin = "https://" + canary
	adapter.receipt.RouteID = canary
	adapter.receipt.StopReason = canary
	adapter.receipt.Complete = false
	adapter.readErr = fmt.Errorf("https://fake.invalid/details.php?id=%s Cookie=%s", canary, canary)
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader("sid=" + canary), stdout: &out, registry: site.NewRegistry(adapter)}
	err := a.siteDetail([]string{"--cookie-stdin", "--output", "json", "fakept", "42"})
	if err == nil || strings.Contains(err.Error(), canary) || strings.Contains(out.String(), canary) {
		t.Fatalf("err=%v output=%s", err, out.String())
	}
	var envelope struct {
		Data siteDetailReport `json:"data"`
	}
	if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if envelope.Data.Outcome != "incomplete" || envelope.Data.Observation != nil || envelope.Data.Request.Status != "incomplete" ||
		envelope.Data.Request.Receipt.Ref != (domain.TorrentRef{SiteID: "fakept", RemoteID: "42"}) ||
		envelope.Data.Request.Receipt.Origin != "https://fake.invalid" || envelope.Data.Request.Receipt.RouteID != "fake.details_by_id.v1" ||
		envelope.Data.Request.Receipt.StopReason != "site.detail_failed" || len(envelope.Data.Blockers) != 1 {
		t.Fatalf("report=%#v", envelope.Data)
	}
}

func TestSiteDetailInconsistentReceiptAndCloseFailureFailClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*fakeTorrentDetailAdapter)
		stop   string
	}{
		{name: "receipt", mutate: func(adapter *fakeTorrentDetailAdapter) { adapter.receipt.Used.AutomaticRetries = 1 }, stop: "receipt_inconsistent"},
		{name: "detail evidence", mutate: func(adapter *fakeTorrentDetailAdapter) { adapter.detail.EvidenceBasis[0] = "untrusted" }, stop: "receipt_inconsistent"},
		{name: "close", mutate: func(adapter *fakeTorrentDetailAdapter) { adapter.closeErr = errors.New("CLOSE-CANARY") }, stop: "session_close_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter := successfulFakeTorrentDetail(t)
			test.mutate(adapter)
			var out bytes.Buffer
			a := &app{stdin: strings.NewReader("sid=secret"), stdout: &out, registry: site.NewRegistry(adapter)}
			err := a.siteDetail([]string{"--cookie-stdin", "--output", "json", "fakept", "42"})
			if err == nil || strings.Contains(err.Error(), "CANARY") {
				t.Fatalf("err=%v", err)
			}
			var envelope struct {
				Data siteDetailReport `json:"data"`
			}
			if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if envelope.Data.Observation != nil || envelope.Data.Request.Receipt.StopReason != test.stop || envelope.Data.Outcome != "incomplete" {
				t.Fatalf("report=%#v", envelope.Data)
			}
		})
	}
}

func TestSiteDetailHumanOrdersBlockersBeforeEvidence(t *testing.T) {
	adapter := successfulFakeTorrentDetail(t)
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader("sid=secret"), stdout: &out, registry: site.NewRegistry(adapter)}
	if err := a.siteDetail([]string{"--cookie-stdin", "fakept", "42"}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Index(text, "BLOCKERS") < 0 || strings.Index(text, "SITE") <= strings.Index(text, "BLOCKERS") || strings.Index(text, "DETAIL STATUS") <= strings.Index(text, "SITE") ||
		strings.Index(text, "REQUEST STATUS") <= strings.Index(text, "DETAIL STATUS") || !strings.Contains(text, "site claims") {
		t.Fatalf("human output order is unclear: %q", text)
	}
}

func TestSiteDetailRunDispatchAndCapabilities(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"site", "capabilities", "--output", "json", "tjupt"}, strings.NewReader(""), &out, &errOut); code != 0 || !strings.Contains(out.String(), `"torrent.detail"`) {
		t.Fatalf("code=%d output=%q stderr=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"site", "detail", "tjupt", "42"}, strings.NewReader(""), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "--cookie-stdin") {
		t.Fatalf("code=%d output=%q stderr=%q", code, out.String(), errOut.String())
	}
}
