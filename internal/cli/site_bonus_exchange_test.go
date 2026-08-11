package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/bonusexchange"
	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

type fakeBonusExchangeAdapter struct {
	review             domain.BonusOfferReview
	reviewReceipt      site.BonusReviewReceipt
	submissionOutcome  string
	submissionStop     string
	submitErr          error
	configErr          error
	openErr            error
	closeErr           error
	invalidObservation bool
	afterReview        func(*site.ObservedBonusReview, site.BonusReviewReceipt)
	afterSubmit        func(*site.ObservedBonusExchange, site.BonusExchangeReceipt)
	opened             int
	reviews            int
	submissions        int
	credential         string
}

func (*fakeBonusExchangeAdapter) Descriptor() domain.SiteDescriptor {
	return domain.SiteDescriptor{
		ID: "fakept", Name: "Fake PT", BaseURL: "https://fake.invalid/", Stability: "test",
		AuthMethods:  []domain.AuthMethod{domain.AuthMethodCookieHeader},
		Capabilities: []domain.Capability{domain.CapabilityBonusReview, domain.CapabilityBonusExchange},
	}
}

func (adapter *fakeBonusExchangeAdapter) ValidateBonusExchangeSelector(selector string) error {
	if adapter.configErr != nil {
		return adapter.configErr
	}
	if selector != "1" {
		return fmt.Errorf("invalid selector")
	}
	return nil
}

func (adapter *fakeBonusExchangeAdapter) BonusExchangeConfig() (site.BonusExchangeConfig, error) {
	if adapter.configErr != nil {
		return site.BonusExchangeConfig{}, adapter.configErr
	}
	return site.NewBonusExchangeConfig("https://fake.invalid", "fake.bonus_review.v1", "fake.bonus_exchange.v1")
}

func (adapter *fakeBonusExchangeAdapter) OpenBonusExchangeSession(ctx context.Context, credential site.Credential) (site.BonusExchangeSession, error) {
	adapter.opened++
	adapter.credential = credential.SecretValue()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if adapter.openErr != nil {
		return nil, adapter.openErr
	}
	return &fakeBonusExchangeSession{adapter: adapter}, nil
}

type fakeBonusExchangeSession struct {
	adapter  *fakeBonusExchangeAdapter
	requests int
	closed   bool
	reviewed *site.ObservedBonusReview
}

func (session *fakeBonusExchangeSession) ReadFreshBonusReview(_ context.Context, selector string, limits site.BonusReviewLimits, exchangeLimits site.BonusExchangeLimits) (*site.ObservedBonusReview, site.BonusReviewReceipt, error) {
	session.requests++
	session.adapter.reviews++
	if selector != "1" || limits != site.DefaultBonusReviewLimits() || exchangeLimits != site.DefaultBonusExchangeLimits() {
		return nil, site.BonusReviewReceipt{}, fmt.Errorf("invalid review request")
	}
	observed, err := site.NewObservedBonusReview(session.adapter.review, session.adapter.reviewReceipt)
	if err != nil {
		return nil, session.adapter.reviewReceipt, err
	}
	session.reviewed = observed
	if session.adapter.afterReview != nil {
		session.adapter.afterReview(observed, session.adapter.reviewReceipt)
	}
	return observed, session.adapter.reviewReceipt, nil
}

func (session *fakeBonusExchangeSession) SubmitFreshBonusExchange(_ context.Context, observed *site.ObservedBonusReview, expected string, limits site.BonusExchangeLimits) (*site.ObservedBonusExchange, site.BonusExchangeReceipt, error) {
	session.requests++
	session.adapter.submissions++
	start := time.Date(2026, 8, 12, 7, 8, 9, 0, time.UTC)
	receipt := site.BonusExchangeReceipt{
		Effect: site.BonusExchangeSubmitEffect, SiteID: "fakept", Selector: "1",
		ExpectedReviewID: expected, FreshReviewID: expected,
		Origin: "https://fake.invalid", ReviewRouteID: "fake.bonus_review.v1", ActionRouteID: "fake.bonus_exchange.v1",
		ObservedAtStart: start, ObservedAtEnd: start.Add(time.Second), Complete: true,
		RequestAttempted: true, ResponseComplete: true, Outcome: session.adapter.submissionOutcome, StopReason: session.adapter.submissionStop,
		Limits: limits,
		Used: site.BonusExchangeUsage{
			TotalRequestsAttempted: 2, ReviewRequestsAttempted: 1, SubmissionRequestsAttempted: 1,
			FormFieldsSubmitted: 2, FormBytesSubmitted: 24, ResponseBytesKnown: true,
		},
	}
	if receipt.Outcome == site.BonusExchangeOutcomeConfirmed {
		receipt.ConfirmationCode = "fake.bonus_exchange.confirmed.v1"
		receipt.StopReason = ""
	}
	if session.adapter.invalidObservation {
		receipt.SiteID = "COOKIE-BODY-URL-CANARY"
		return nil, receipt, errors.New("COOKIE-BODY-URL-CANARY")
	}
	authority, err := site.NewObservedBonusExchange(receipt)
	if err != nil || observed != session.reviewed {
		return nil, receipt, fmt.Errorf("invalid fake exchange authority")
	}
	if session.adapter.afterSubmit != nil {
		session.adapter.afterSubmit(authority, receipt)
	}
	return authority, receipt, session.adapter.submitErr
}

func (session *fakeBonusExchangeSession) RequestsMade() int { return session.requests }
func (session *fakeBonusExchangeSession) Close() error {
	if session.closed {
		return nil
	}
	session.closed = true
	return session.adapter.closeErr
}

func TestSiteBonusExchangePrepareSubmitStatusAndNoRetry(t *testing.T) {
	adapter := successfulFakeBonusExchange(t)
	stateRoot := initializedBonusExchangeStore(t)
	reviewID := adapter.review.ReviewID
	tracking := &trackingReader{}
	prepared := runBonusExchangeCLI(t, &app{stdin: tracking, registry: site.NewRegistry(adapter)}, []string{
		"prepare", "--state-store", stateRoot, "--expect-review-id", reviewID, "--output", "json", "fakept", "1",
	}, false)
	if tracking.read || prepared.Outcome != bonusexchange.StatePrepared || prepared.WritesPerformed != 1 || prepared.Operation.IntentRecord == nil || prepared.Operation.OperationID == "" || !prepared.Operation.Complete || prepared.Request.Status != "not_requested_local_prepare" {
		t.Fatalf("prepared=%#v credentialRead=%t", prepared, tracking.read)
	}

	const cookie = "sid=COOKIE-BONUS-EXCHANGE-CANARY"
	submitted := runBonusExchangeCLI(t, &app{stdin: strings.NewReader(cookie), registry: site.NewRegistry(adapter)}, []string{
		"submit", "--state-store", stateRoot, "--intent-record", prepared.Operation.IntentRecord.ID.String(),
		"--expect-review-id", reviewID, "--cookie-stdin", "--acknowledge-bonus-exchange", "--output", "json", "fakept", "1",
	}, false)
	if submitted.Outcome != bonusexchange.StateConfirmed || submitted.WritesPerformed != 2 || submitted.FormSubmissionAttempts != 1 ||
		submitted.Operation.AttemptRecord == nil || submitted.Operation.OutcomeRecord == nil || !submitted.Operation.Complete || submitted.Operation.StopReason != "" ||
		!submitted.Persistence.OutcomeDurable || submitted.Persistence.Status != "outcome_durable" || !submitted.Persistence.AttemptRecordVerified || !submitted.Persistence.OutcomeRecordVerified ||
		submitted.Persistence.AttemptWrites != 1 || submitted.Persistence.OutcomeWrites != 1 ||
		!submitted.Assurance.FreshReviewReproduced || !submitted.Assurance.ProcessLocalReviewAuthority ||
		!submitted.Assurance.AttemptMarkerDurable || !submitted.Assurance.ProcessLocalOutcomeAuthority ||
		!submitted.Assurance.AttemptMarkerBlocksFuture || !submitted.Assurance.SubmissionRequestBound || submitted.Assurance.AtMostOnceScope != "one_prepared_operation_in_one_uncloned_private_store_history" ||
		!submitted.Assurance.NoAutomaticRetry || adapter.opened != 1 || adapter.reviews != 1 || adapter.submissions != 1 {
		t.Fatalf("submitted=%#v adapter=%#v", submitted, adapter)
	}
	if adapter.credential != cookie {
		t.Fatalf("credential was not delivered exactly once: %q", adapter.credential)
	}
	assertDirectoryExcludesText(t, stateRoot, cookie)

	statusReader := &trackingReader{}
	status := runBonusExchangeCLI(t, &app{stdin: statusReader, registry: site.NewRegistry(adapter)}, []string{
		"status", "--state-store", stateRoot, "--intent-record", prepared.Operation.IntentRecord.ID.String(), "--output", "json",
	}, false)
	if statusReader.read || status.Outcome != bonusexchange.StateConfirmed || status.WritesPerformed != 0 || status.FormSubmissionAttempts != 0 ||
		!status.Operation.Complete || status.Operation.StopReason != "" || status.Operation.OutcomeRecord == nil || status.Persistence.OutcomeDurable ||
		status.Assurance.AttemptMarkerDurable || !status.Assurance.AttemptMarkerBlocksFuture || !status.Assurance.SubmissionRequestBound ||
		!status.Persistence.AttemptRecordVerified || !status.Persistence.OutcomeRecordVerified || status.Request.Status != "not_requested_read_only_status" ||
		status.Request.FreshReview.SiteID != "fakept" || status.Request.Submission.SiteID != "fakept" || status.Request.Submission.ExpectedReviewID != reviewID {
		t.Fatalf("status=%#v credentialRead=%t", status, statusReader.read)
	}

	retryReader := &trackingReader{}
	retry := runBonusExchangeCLI(t, &app{stdin: retryReader, registry: site.NewRegistry(adapter)}, []string{
		"submit", "--state-store", stateRoot, "--intent-record", prepared.Operation.IntentRecord.ID.String(),
		"--expect-review-id", reviewID, "--cookie-stdin", "--acknowledge-bonus-exchange", "--output", "json", "fakept", "1",
	}, true)
	if retryReader.read || retry.Operation.Status != bonusexchange.StateConfirmed || retry.FormSubmissionAttempts != 0 || adapter.opened != 1 || adapter.submissions != 1 || len(retry.Blockers) == 0 {
		t.Fatalf("retry=%#v credentialRead=%t adapter=%#v", retry, retryReader.read, adapter)
	}
}

func TestSiteBonusExchangeFreshMismatchNeverReservesOrSubmits(t *testing.T) {
	adapter := successfulFakeBonusExchange(t)
	stateRoot := initializedBonusExchangeStore(t)
	originalReviewID := adapter.review.ReviewID
	prepared := runBonusExchangeCLI(t, &app{stdin: &trackingReader{}, registry: site.NewRegistry(adapter)}, []string{
		"prepare", "--state-store", stateRoot, "--expect-review-id", originalReviewID, "--output", "json", "fakept", "1",
	}, false)
	adapter.review.Columns[0] = "changed offer"
	changedID, err := site.ComputeBonusReviewID(adapter.review)
	if err != nil {
		t.Fatal(err)
	}
	adapter.review.ReviewID = changedID

	report := runBonusExchangeCLI(t, &app{stdin: strings.NewReader("sid=secret"), registry: site.NewRegistry(adapter)}, []string{
		"submit", "--state-store", stateRoot, "--intent-record", prepared.Operation.IntentRecord.ID.String(),
		"--expect-review-id", originalReviewID, "--cookie-stdin", "--acknowledge-bonus-exchange", "--output", "json", "fakept", "1",
	}, true)
	if report.WritesPerformed != 0 || report.FormSubmissionAttempts != 0 || report.Assurance.AttemptMarkerDurable || adapter.submissions != 0 {
		t.Fatalf("mismatch report=%#v adapter=%#v", report, adapter)
	}
	status := runBonusExchangeCLI(t, &app{stdin: &trackingReader{}, registry: site.NewRegistry(adapter)}, []string{
		"status", "--state-store", stateRoot, "--intent-record", prepared.Operation.IntentRecord.ID.String(), "--output", "json",
	}, false)
	if status.Outcome != bonusexchange.StatePrepared || status.Operation.AttemptRecord != nil {
		t.Fatalf("mismatch changed durable state: %#v", status)
	}
}

func TestSiteBonusExchangeUnknownResultIsDurableAndSecretFree(t *testing.T) {
	adapter := successfulFakeBonusExchange(t)
	adapter.submissionOutcome = site.BonusExchangeOutcomeUnknown
	adapter.submissionStop = "submission_transport_uncertain"
	adapter.submitErr = errors.New("COOKIE-URL-BODY-CANARY")
	stateRoot := initializedBonusExchangeStore(t)
	prepared := runBonusExchangeCLI(t, &app{stdin: &trackingReader{}, registry: site.NewRegistry(adapter)}, []string{
		"prepare", "--state-store", stateRoot, "--expect-review-id", adapter.review.ReviewID, "--output", "json", "fakept", "1",
	}, false)

	var out bytes.Buffer
	a := &app{stdin: strings.NewReader("sid=COOKIE-URL-BODY-CANARY"), stdout: &out, registry: site.NewRegistry(adapter)}
	err := a.siteBonusExchange([]string{
		"submit", "--state-store", stateRoot, "--intent-record", prepared.Operation.IntentRecord.ID.String(),
		"--expect-review-id", adapter.review.ReviewID, "--cookie-stdin", "--acknowledge-bonus-exchange", "--output", "json", "fakept", "1",
	})
	if err == nil || strings.Contains(err.Error(), "CANARY") || strings.Contains(out.String(), "CANARY") || strings.Contains(out.String(), stateRoot) {
		t.Fatalf("err=%v output=%s", err, out.String())
	}
	var envelope struct {
		Kind string                  `json:"kind"`
		Data siteBonusExchangeReport `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Kind != "site.bonus.exchange.submit" || envelope.Data.Outcome != bonusexchange.StateUnknown ||
		!envelope.Data.Persistence.OutcomeDurable || envelope.Data.FormSubmissionAttempts != 1 || envelope.Data.Operation.OutcomeRecord == nil {
		t.Fatalf("report=%#v", envelope.Data)
	}

	retryReader := &trackingReader{}
	_ = runBonusExchangeCLI(t, &app{stdin: retryReader, registry: site.NewRegistry(adapter)}, []string{
		"submit", "--state-store", stateRoot, "--intent-record", prepared.Operation.IntentRecord.ID.String(),
		"--expect-review-id", adapter.review.ReviewID, "--cookie-stdin", "--acknowledge-bonus-exchange", "--output", "json", "fakept", "1",
	}, true)
	if retryReader.read || adapter.submissions != 1 {
		t.Fatalf("unknown outcome was retried: read=%t submissions=%d", retryReader.read, adapter.submissions)
	}
}

func TestSiteBonusExchangeCloseFailureDoesNotEraseDurableOutcome(t *testing.T) {
	adapter := successfulFakeBonusExchange(t)
	adapter.closeErr = errors.New("COOKIE-URL-BODY-CANARY")
	stateRoot := initializedBonusExchangeStore(t)
	prepared := runBonusExchangeCLI(t, &app{stdin: &trackingReader{}, registry: site.NewRegistry(adapter)}, []string{
		"prepare", "--state-store", stateRoot, "--expect-review-id", adapter.review.ReviewID, "--output", "json", "fakept", "1",
	}, false)
	report := runBonusExchangeCLI(t, &app{stdin: strings.NewReader("sid=secret"), registry: site.NewRegistry(adapter)}, []string{
		"submit", "--state-store", stateRoot, "--intent-record", prepared.Operation.IntentRecord.ID.String(),
		"--expect-review-id", adapter.review.ReviewID, "--cookie-stdin", "--acknowledge-bonus-exchange", "--output", "json", "fakept", "1",
	}, true)
	if report.Outcome != bonusexchange.StateConfirmed || !report.Persistence.OutcomeDurable || !report.Operation.Complete ||
		report.Operation.Status != bonusexchange.StateConfirmed || len(report.Blockers) != 1 || report.Blockers[0].Code != "session.close_failed" {
		t.Fatalf("report=%#v", report)
	}
}

func TestSiteBonusExchangeValidationPrecedesCredentialRead(t *testing.T) {
	adapter := successfulFakeBonusExchange(t)
	stateRoot := initializedBonusExchangeStore(t)
	prepared := runBonusExchangeCLI(t, &app{stdin: &trackingReader{}, registry: site.NewRegistry(adapter)}, []string{
		"prepare", "--state-store", stateRoot, "--expect-review-id", adapter.review.ReviewID, "--output", "json", "fakept", "1",
	}, false)
	tests := [][]string{
		{"submit", "--state-store", stateRoot, "--intent-record", prepared.Operation.IntentRecord.ID.String(), "--expect-review-id", adapter.review.ReviewID, "--cookie-stdin", "fakept", "1"},
		{"submit", "--state-store", stateRoot, "--intent-record", "bad", "--expect-review-id", adapter.review.ReviewID, "--cookie-stdin", "--acknowledge-bonus-exchange", "fakept", "1"},
		{"submit", "--state-store", stateRoot, "--intent-record", prepared.Operation.IntentRecord.ID.String(), "--expect-review-id", "bad", "--cookie-stdin", "--acknowledge-bonus-exchange", "fakept", "1"},
		{"submit", "--state-store", stateRoot, "--intent-record", prepared.Operation.IntentRecord.ID.String(), "--expect-review-id", adapter.review.ReviewID, "--cookie-stdin", "--acknowledge-bonus-exchange", "fakept", "2"},
	}
	for index, args := range tests {
		reader := &trackingReader{}
		var out bytes.Buffer
		a := &app{stdin: reader, stdout: &out, registry: site.NewRegistry(adapter)}
		if err := a.siteBonusExchange(args); err == nil || reader.read {
			t.Fatalf("case %d err=%v read=%t output=%q", index, err, reader.read, out.String())
		}
	}
}

func TestSiteBonusExchangeReservationRaceNeverPosts(t *testing.T) {
	adapter := successfulFakeBonusExchange(t)
	stateRoot := initializedBonusExchangeStore(t)
	prepared := runBonusExchangeCLI(t, &app{stdin: &trackingReader{}, registry: site.NewRegistry(adapter)}, []string{
		"prepare", "--state-store", stateRoot, "--expect-review-id", adapter.review.ReviewID, "--output", "json", "fakept", "1",
	}, false)
	adapter.afterReview = func(observed *site.ObservedBonusReview, receipt site.BonusReviewReceipt) {
		store, err := metastore.Open(stateRoot)
		if err != nil {
			t.Fatal(err)
		}
		repository, err := bonusexchange.NewRepository(store, bonusexchange.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		session, err := repository.OpenSession(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		intent, _, err := session.LoadIntent(context.Background(), prepared.Operation.IntentRecord.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, reserved, reservedReceipt, err := session.ReserveAttempt(context.Background(), intent, observed, receipt); err != nil || reserved == nil || !reservedReceipt.Acquired {
			t.Fatalf("racing reservation=%v receipt=%#v err=%v", reserved, reservedReceipt, err)
		}
	}

	report := runBonusExchangeCLI(t, &app{stdin: strings.NewReader("sid=secret"), registry: site.NewRegistry(adapter)}, []string{
		"submit", "--state-store", stateRoot, "--intent-record", prepared.Operation.IntentRecord.ID.String(),
		"--expect-review-id", adapter.review.ReviewID, "--cookie-stdin", "--acknowledge-bonus-exchange", "--output", "json", "fakept", "1",
	}, true)
	if adapter.submissions != 0 || report.FormSubmissionAttempts != 0 || report.Operation.Status != bonusexchange.StateAttemptReservedUnknown ||
		report.Operation.AttemptRecord == nil || !report.Assurance.AttemptMarkerBlocksFuture || report.Assurance.SubmissionRequestBound {
		t.Fatalf("race submitted form: report=%#v adapter=%#v", report, adapter)
	}
}

func TestSiteBonusExchangePostResultPersistenceFailureStaysUnknown(t *testing.T) {
	adapter := successfulFakeBonusExchange(t)
	stateRoot := initializedBonusExchangeStore(t)
	prepared := runBonusExchangeCLI(t, &app{stdin: &trackingReader{}, registry: site.NewRegistry(adapter)}, []string{
		"prepare", "--state-store", stateRoot, "--expect-review-id", adapter.review.ReviewID, "--output", "json", "fakept", "1",
	}, false)
	adapter.afterSubmit = func(*site.ObservedBonusExchange, site.BonusExchangeReceipt) {
		corruptSealedRecord(t, stateRoot, metastore.RecordKindSiteBonusExchangeIntentV1, prepared.Operation.IntentRecord.ID)
	}
	report := runBonusExchangeCLI(t, &app{stdin: strings.NewReader("sid=secret"), registry: site.NewRegistry(adapter)}, []string{
		"submit", "--state-store", stateRoot, "--intent-record", prepared.Operation.IntentRecord.ID.String(),
		"--expect-review-id", adapter.review.ReviewID, "--cookie-stdin", "--acknowledge-bonus-exchange", "--output", "json", "fakept", "1",
	}, true)
	if adapter.submissions != 1 || report.FormSubmissionAttempts != 1 || report.Outcome != "submission_outcome_not_durable" ||
		report.Persistence.OutcomeDurable || report.Persistence.Status != "outcome_publication_unverified" ||
		report.Request.Status != "complete" || !report.Assurance.SubmissionRequestBound ||
		!report.Assurance.AttemptMarkerDurable || report.Assurance.AttemptMarkerBlocksFuture || len(report.Blockers) == 0 {
		t.Fatalf("persistence failure report=%#v adapter=%#v", report, adapter)
	}

	retryReader := &trackingReader{}
	_ = runBonusExchangeCLI(t, &app{stdin: retryReader, registry: site.NewRegistry(adapter)}, []string{
		"submit", "--state-store", stateRoot, "--intent-record", prepared.Operation.IntentRecord.ID.String(),
		"--expect-review-id", adapter.review.ReviewID, "--cookie-stdin", "--acknowledge-bonus-exchange", "--output", "json", "fakept", "1",
	}, true)
	if retryReader.read || adapter.submissions != 1 {
		t.Fatalf("corrupt post-attempt state retried: read=%t submissions=%d", retryReader.read, adapter.submissions)
	}
}

func TestSiteBonusExchangeInvalidAdapterReceiptIsRedactedAndNotPersisted(t *testing.T) {
	adapter := successfulFakeBonusExchange(t)
	adapter.invalidObservation = true
	stateRoot := initializedBonusExchangeStore(t)
	prepared := runBonusExchangeCLI(t, &app{stdin: &trackingReader{}, registry: site.NewRegistry(adapter)}, []string{
		"prepare", "--state-store", stateRoot, "--expect-review-id", adapter.review.ReviewID, "--output", "json", "fakept", "1",
	}, false)
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader("sid=COOKIE-BODY-URL-CANARY"), stdout: &out, registry: site.NewRegistry(adapter)}
	err := a.siteBonusExchange([]string{
		"submit", "--state-store", stateRoot, "--intent-record", prepared.Operation.IntentRecord.ID.String(),
		"--expect-review-id", adapter.review.ReviewID, "--cookie-stdin", "--acknowledge-bonus-exchange", "--output", "json", "fakept", "1",
	})
	if err == nil || strings.Contains(err.Error(), "CANARY") || strings.Contains(out.String(), "CANARY") || strings.Contains(out.String(), stateRoot) {
		t.Fatalf("err=%v output=%s", err, out.String())
	}
	var envelope struct {
		Data siteBonusExchangeReport `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.FormSubmissionAttempts != 1 || envelope.Data.Persistence.OutcomeDurable || envelope.Data.Operation.OutcomeRecord != nil ||
		envelope.Data.Outcome != "submission_outcome_not_durable" || envelope.Data.Assurance.ProcessLocalOutcomeAuthority ||
		envelope.Data.Assurance.SubmissionRequestBound || !envelope.Data.Assurance.AttemptMarkerBlocksFuture ||
		envelope.Data.Assurance.NoAutomaticRetry || envelope.Data.Request.Status != "submission_receipt_invalid" || envelope.Data.Request.Submission.Complete ||
		envelope.Data.Request.Submission.Outcome != site.BonusExchangeOutcomeUnknown ||
		envelope.Data.Request.Submission.StopReason != "site.exchange_receipt_invalid" ||
		envelope.Data.Request.Submission.ConfirmationCode != "" {
		t.Fatalf("invalid adapter report=%#v", envelope.Data)
	}
}

func TestSiteBonusExchangeHelpAndDispatch(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"site", "bonus", "exchange", "help"}, strings.NewReader(""), &out, &errOut); code != 0 || !strings.Contains(out.String(), "deterministic at-most-once attempt marker") {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"site", "capabilities", "--output", "json", "tjupt"}, strings.NewReader(""), &out, &errOut); code != 0 || !strings.Contains(out.String(), string(domain.CapabilityBonusExchange)) {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
}

func successfulFakeBonusExchange(t *testing.T) *fakeBonusExchangeAdapter {
	t.Helper()
	reviewAdapter := successfulFakeBonusReview(t)
	return &fakeBonusExchangeAdapter{
		review: reviewAdapter.review, reviewReceipt: reviewAdapter.receipt,
		submissionOutcome: site.BonusExchangeOutcomeConfirmed,
	}
}

func initializedBonusExchangeStore(t *testing.T) string {
	t.Helper()
	root := filepath.Join(physicalCLITempDir(t), "bonus-state")
	if _, _, err := metastore.Init(root); err != nil {
		t.Fatal(err)
	}
	return root
}

func corruptSealedRecord(t *testing.T, root string, kind metastore.RecordKind, id metastore.RecordID) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimPrefix(id.String(), "sha256:")
	for _, entry := range entries {
		if !entry.IsDir() && strings.Contains(entry.Name(), string(kind)) && strings.Contains(entry.Name(), digest) {
			if err := os.WriteFile(filepath.Join(root, "objects", entry.Name()), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("record %s/%s not found", kind, id)
}

func assertDirectoryExcludesText(t *testing.T, root, forbidden string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(raw, []byte(forbidden)) {
			return fmt.Errorf("forbidden text was persisted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func runBonusExchangeCLI(t *testing.T, a *app, args []string, wantErr bool) siteBonusExchangeReport {
	t.Helper()
	var out bytes.Buffer
	a.stdout = &out
	err := a.siteBonusExchange(args)
	if wantErr == (err == nil) {
		t.Fatalf("wantErr=%t err=%v output=%s", wantErr, err, out.String())
	}
	var envelope struct {
		Kind string                  `json:"kind"`
		Data siteBonusExchangeReport `json:"data"`
	}
	if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
		t.Fatalf("decode report: %v output=%s", decodeErr, out.String())
	}
	if envelope.Kind == "" || envelope.Data.Blockers == nil || envelope.Data.Warnings == nil || strings.Contains(out.String(), "COOKIE-") {
		t.Fatalf("unsafe or incomplete envelope: %s", out.String())
	}
	return envelope.Data
}
