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

type fakeBonusReviewAdapter struct {
	configErr  error
	openErr    error
	nilSession bool
	review     domain.BonusOfferReview
	receipt    site.BonusReviewReceipt
	readErr    error
	closeErr   error
	opened     int
	credential string
}

func (*fakeBonusReviewAdapter) Descriptor() domain.SiteDescriptor {
	return domain.SiteDescriptor{
		ID: "fakept", Name: "Fake PT", BaseURL: "https://fake.invalid/", Stability: "test",
		AuthMethods: []domain.AuthMethod{domain.AuthMethodCookieHeader}, Capabilities: []domain.Capability{domain.CapabilityBonusReview},
	}
}

func (adapter *fakeBonusReviewAdapter) ValidateBonusReviewSelector(selector string) error {
	if adapter.configErr != nil {
		return adapter.configErr
	}
	if selector != "1" {
		return fmt.Errorf("invalid fake bonus selector")
	}
	return nil
}

func (adapter *fakeBonusReviewAdapter) BonusReviewConfig() (site.BonusReviewConfig, error) {
	if adapter.configErr != nil {
		return site.BonusReviewConfig{}, adapter.configErr
	}
	return site.NewBonusReviewConfig("https://fake.invalid", "fake.bonus_review.v1")
}

func (adapter *fakeBonusReviewAdapter) OpenBonusReviewSession(ctx context.Context, credential site.Credential) (site.BonusReviewSession, error) {
	adapter.opened++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	adapter.credential = credential.SecretValue()
	if adapter.openErr != nil {
		return nil, adapter.openErr
	}
	if adapter.nilSession {
		return nil, nil
	}
	return &fakeBonusReviewSession{adapter: adapter}, nil
}

type fakeBonusReviewSession struct {
	adapter  *fakeBonusReviewAdapter
	requests int
	closed   bool
}

func (session *fakeBonusReviewSession) ReadBonusReview(context.Context, string, site.BonusReviewLimits) (*site.ObservedBonusReview, site.BonusReviewReceipt, error) {
	session.requests++
	observed, _ := site.NewObservedBonusReview(session.adapter.review, session.adapter.receipt)
	return observed, session.adapter.receipt, session.adapter.readErr
}

func (session *fakeBonusReviewSession) RequestsMade() int { return session.requests }

func (session *fakeBonusReviewSession) Close() error {
	if session.closed {
		return nil
	}
	session.closed = true
	return session.adapter.closeErr
}

func successfulFakeBonusReview(t *testing.T) *fakeBonusReviewAdapter {
	t.Helper()
	config, err := site.NewBonusReviewConfig("https://fake.invalid", "fake.bonus_review.v1")
	if err != nil {
		t.Fatal(err)
	}
	review := domain.BonusOfferReview{
		SiteID: "fakept", Selector: "1", Balance: "12345.67",
		Columns:      []string{"Upload credit", "1000", "Exchange"},
		Availability: site.BonusReviewAvailabilityAvailable, InputMode: site.BonusReviewInputNone,
		ActionMethod: "post", ActionRouteID: "fake.bonus_exchange.v1",
		FormShapeID: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		EvidenceBasis: []string{
			site.BonusReviewBasisAuthenticated, site.BonusReviewBasisExactSelector, site.BonusReviewBasisExactForm,
			site.BonusReviewBasisOneRequest, site.BonusReviewBasisSiteClaimOnly, site.BonusReviewBasisNoSubmission,
		},
	}
	review.ReviewID, err = site.ComputeBonusReviewID(review)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 12, 3, 4, 5, 0, time.UTC)
	return &fakeBonusReviewAdapter{
		review: review,
		receipt: site.BonusReviewReceipt{
			Effect: site.BonusReviewReadEffect, SiteID: "fakept", Selector: "1", Origin: config.Origin, RouteID: config.RouteID,
			ObservedAtStart: start, ObservedAtEnd: start.Add(time.Second), Complete: true, Limits: site.DefaultBonusReviewLimits(),
			Used: site.BonusReviewUsage{
				RequestsAttempted: 1, ResponseBytesRead: 1234, ResponseBytesKnown: true,
				FormsExamined: 2, FieldsExamined: 4, TokensExamined: 80, VisibleTextBytes: 120,
			},
		},
	}
}

func TestSiteBonusReviewSuccessJSONIsAuditableAndSecretFree(t *testing.T) {
	const cookie = "sid=COOKIE-BONUS-CANARY"
	adapter := successfulFakeBonusReview(t)
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader(cookie), stdout: &out, registry: site.NewRegistry(adapter)}
	if err := a.siteBonusReview([]string{"--cookie-stdin", "--output", "json", "fakept", "1"}); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Kind string                `json:"kind"`
		Data siteBonusReviewReport `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Kind != "site.bonus.offer_review" || envelope.Data.Outcome != "reviewed" || envelope.Data.Effect != site.BonusReviewReadEffect ||
		envelope.Data.WritesPerformed != 0 || envelope.Data.FormSubmissions != 0 || envelope.Data.Review == nil || envelope.Data.Review.Selector != "1" ||
		envelope.Data.Request.Status != "complete" || envelope.Data.Request.RequestsMade != 1 || !envelope.Data.Request.Receipt.Complete ||
		!envelope.Data.Assurance.ProcessLocalAuthority || envelope.Data.Assurance.SerializedAuthority || envelope.Data.Assurance.FormSubmitted ||
		len(envelope.Data.Blockers) != 0 || len(envelope.Data.Warnings) != 4 || !strings.Contains(strings.Join(envelope.Data.Warnings, " "), "JavaScript") || adapter.opened != 1 {
		t.Fatalf("envelope=%#v", envelope)
	}
	if strings.Contains(out.String(), cookie) || strings.Contains(out.String(), "mybonusapps.php") {
		t.Fatalf("secret or request path leaked: %s", out.String())
	}
}

func TestSiteBonusReviewUsageRejectsBeforeCredentialRead(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		adapter site.Adapter
	}{
		{name: "missing cookie flag", args: []string{"fakept", "1"}, adapter: successfulFakeBonusReview(t)},
		{name: "invalid output", args: []string{"--cookie-stdin", "--output", "yaml", "fakept", "1"}, adapter: successfulFakeBonusReview(t)},
		{name: "invalid option", args: []string{"--cookie-stdin", "fakept", "01"}, adapter: successfulFakeBonusReview(t)},
		{name: "config unavailable", args: []string{"--cookie-stdin", "fakept", "1"}, adapter: &fakeBonusReviewAdapter{configErr: fmt.Errorf("COOKIE-CONFIG-CANARY")}},
		{name: "missing typed port", args: []string{"--cookie-stdin", "fakept", "1"}, adapter: fakeBonusDescriptorOnly{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &trackingReader{}
			var out bytes.Buffer
			a := &app{stdin: reader, stdout: &out, registry: site.NewRegistry(test.adapter)}
			if err := a.siteBonusReview(test.args); err == nil || reader.read || strings.Contains(err.Error(), "CANARY") || out.Len() != 0 {
				t.Fatalf("err=%v read=%t output=%q", err, reader.read, out.String())
			}
		})
	}
}

type fakeBonusDescriptorOnly struct{}

func (fakeBonusDescriptorOnly) Descriptor() domain.SiteDescriptor {
	return domain.SiteDescriptor{
		ID: "fakept", AuthMethods: []domain.AuthMethod{domain.AuthMethodCookieHeader},
		Capabilities: []domain.Capability{domain.CapabilityBonusReview},
	}
}

func TestSiteBonusReviewFailureRedactsAdapterDataAndFailsClosed(t *testing.T) {
	const canary = "COOKIE-URL-BODY-CANARY"
	adapter := successfulFakeBonusReview(t)
	adapter.receipt.SiteID = canary
	adapter.receipt.Selector = canary
	adapter.receipt.Origin = "https://" + canary
	adapter.receipt.RouteID = canary
	adapter.receipt.StopReason = canary
	adapter.receipt.Complete = false
	adapter.readErr = fmt.Errorf("https://fake.invalid/mybonusapps.php Cookie=%s", canary)
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader("sid=" + canary), stdout: &out, registry: site.NewRegistry(adapter)}
	err := a.siteBonusReview([]string{"--cookie-stdin", "--output", "json", "fakept", "1"})
	if err == nil || strings.Contains(err.Error(), canary) || strings.Contains(out.String(), canary) {
		t.Fatalf("err=%v output=%s", err, out.String())
	}
	var envelope struct {
		Data siteBonusReviewReport `json:"data"`
	}
	if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if envelope.Data.Outcome != "incomplete" || envelope.Data.Review != nil || envelope.Data.Request.Status != "incomplete" ||
		envelope.Data.Request.Receipt.SiteID != "fakept" || envelope.Data.Request.Receipt.Selector != "1" ||
		envelope.Data.Request.Receipt.Origin != "https://fake.invalid" || envelope.Data.Request.Receipt.RouteID != "fake.bonus_review.v1" ||
		envelope.Data.Request.Receipt.StopReason != "site.bonus_review_failed" || len(envelope.Data.Blockers) != 1 {
		t.Fatalf("report=%#v", envelope.Data)
	}
}

func TestSiteBonusReviewNilSessionFailsClosed(t *testing.T) {
	adapter := successfulFakeBonusReview(t)
	adapter.nilSession = true
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader("sid=secret"), stdout: &out, registry: site.NewRegistry(adapter)}
	err := a.siteBonusReview([]string{"--cookie-stdin", "--output", "json", "fakept", "1"})
	if err == nil {
		t.Fatal("nil session was accepted")
	}
	var envelope struct {
		Data siteBonusReviewReport `json:"data"`
	}
	if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if envelope.Data.Outcome != "incomplete" || envelope.Data.Request.Status != "incomplete" || envelope.Data.Request.Receipt.StopReason != "session_open_failed" {
		t.Fatalf("report=%#v", envelope.Data)
	}
}

func TestSiteBonusReviewInconsistentReceiptAndCloseFailureFailClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*fakeBonusReviewAdapter)
		stop   string
	}{
		{name: "receipt", mutate: func(adapter *fakeBonusReviewAdapter) { adapter.receipt.Used.AutomaticRetries = 1 }, stop: "receipt_inconsistent"},
		{name: "review", mutate: func(adapter *fakeBonusReviewAdapter) { adapter.review.Columns[0] = "changed" }, stop: "receipt_inconsistent"},
		{name: "close", mutate: func(adapter *fakeBonusReviewAdapter) { adapter.closeErr = errors.New("CLOSE-CANARY") }, stop: "session_close_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter := successfulFakeBonusReview(t)
			test.mutate(adapter)
			var out bytes.Buffer
			a := &app{stdin: strings.NewReader("sid=secret"), stdout: &out, registry: site.NewRegistry(adapter)}
			err := a.siteBonusReview([]string{"--cookie-stdin", "--output", "json", "fakept", "1"})
			if err == nil || strings.Contains(err.Error(), "CANARY") {
				t.Fatalf("err=%v", err)
			}
			var envelope struct {
				Data siteBonusReviewReport `json:"data"`
			}
			if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if envelope.Data.Review != nil || envelope.Data.Request.Receipt.StopReason != test.stop || envelope.Data.Outcome != "incomplete" {
				t.Fatalf("report=%#v", envelope.Data)
			}
		})
	}
}

func TestSiteBonusReviewHumanAndDispatch(t *testing.T) {
	adapter := successfulFakeBonusReview(t)
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader("sid=secret"), stdout: &out, registry: site.NewRegistry(adapter)}
	if err := a.siteBonusReview([]string{"--cookie-stdin", "fakept", "1"}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Index(text, "BLOCKERS") < 0 || strings.Index(text, "SITE") <= strings.Index(text, "BLOCKERS") || strings.Index(text, "REVIEW ID") <= strings.Index(text, "SITE") || strings.Index(text, "REQUEST STATUS") <= strings.Index(text, "REVIEW ID") || !strings.Contains(text, "FORM SUBMISSIONS  0") {
		t.Fatalf("human output order is unclear: %q", text)
	}

	out.Reset()
	var errOut bytes.Buffer
	if code := Run([]string{"site", "capabilities", "--output", "json", "tjupt"}, strings.NewReader(""), &out, &errOut); code != 0 || !strings.Contains(out.String(), `"bonus.offer.review"`) {
		t.Fatalf("code=%d output=%q stderr=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"site", "bonus", "review", "tjupt", "1"}, strings.NewReader(""), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "--cookie-stdin") {
		t.Fatalf("code=%d output=%q stderr=%q", code, out.String(), errOut.String())
	}
}
