package bonusexchange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

func TestRepositoryEnforcesAtMostOnceAuthorityChain(t *testing.T) {
	fixture := newExchangeFixture(t)
	ctx := context.Background()

	status, err := fixture.session.Status(ctx, fixture.intentRef.ID)
	if err != nil || !status.Complete || status.State != StatePrepared || status.Attempt != nil || status.Outcome != nil {
		t.Fatalf("prepared status=%#v err=%v", status, err)
	}

	rawIntent, err := json.Marshal(fixture.intent)
	if err != nil {
		t.Fatal(err)
	}
	var replayedIntent VerifiedIntent
	if err := json.Unmarshal(rawIntent, &replayedIntent); err != nil {
		t.Fatal(err)
	}
	if replayedIntent.Verified() {
		t.Fatal("serialized intent regained authority")
	}
	if _, replayed, _, err := fixture.session.ReserveAttempt(ctx, &replayedIntent, fixture.review, fixture.reviewReceipt); err == nil || replayed != nil {
		t.Fatalf("serialized intent reserved attempt: reserved=%v err=%v", replayed, err)
	}

	attempt, reserved, reserveReceipt, err := fixture.session.ReserveAttempt(ctx, fixture.intent, fixture.review, fixture.reviewReceipt)
	if err != nil || reserved == nil || !reserved.Verified() || !reserveReceipt.Acquired || reserveReceipt.WritesPerformed != 1 || reserveReceipt.AlreadyPresent {
		t.Fatalf("attempt=%#v reserved=%v receipt=%#v err=%v", attempt, reserved, reserveReceipt, err)
	}
	status, err = fixture.session.Status(ctx, fixture.intentRef.ID)
	if err != nil || !status.Complete || status.State != StateAttemptReservedUnknown || status.Attempt == nil || status.Outcome != nil {
		t.Fatalf("reserved status=%#v err=%v", status, err)
	}

	second, err := fixture.repository.OpenSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	secondIntent, _, err := second.LoadIntent(ctx, fixture.intentRef.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, duplicate, duplicateReceipt, err := second.ReserveAttempt(ctx, secondIntent, fixture.review, fixture.reviewReceipt); !errors.Is(err, ErrAttemptAlreadyReserved) || duplicate != nil || !duplicateReceipt.AlreadyPresent || duplicateReceipt.Acquired || duplicateReceipt.WritesPerformed != 0 {
		t.Fatalf("duplicate=%v receipt=%#v err=%v", duplicate, duplicateReceipt, err)
	}

	exchange := fixture.exchangeReceipt(site.BonusExchangeOutcomeConfirmed)
	observedExchange, err := site.NewObservedBonusExchange(exchange)
	if err != nil {
		t.Fatal(err)
	}
	rawAttempt, err := json.Marshal(reserved)
	if err != nil {
		t.Fatal(err)
	}
	var replayedAttempt ReservedAttempt
	if err := json.Unmarshal(rawAttempt, &replayedAttempt); err != nil {
		t.Fatal(err)
	}
	if replayedAttempt.Verified() {
		t.Fatal("serialized attempt regained authority")
	}
	if _, _, err := fixture.session.StoreOutcome(ctx, fixture.intent, &replayedAttempt, observedExchange, exchange); err == nil {
		t.Fatal("serialized attempt authorized an outcome")
	}
	rawObserved, err := json.Marshal(observedExchange)
	if err != nil {
		t.Fatal(err)
	}
	var replayedObserved site.ObservedBonusExchange
	if err := json.Unmarshal(rawObserved, &replayedObserved); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.session.StoreOutcome(ctx, fixture.intent, reserved, &replayedObserved, exchange); err == nil {
		t.Fatal("serialized site receipt authorized an outcome")
	}

	outcome, stored, err := fixture.session.StoreOutcome(ctx, fixture.intent, reserved, observedExchange, exchange)
	if err != nil || stored.WritesPerformed != 1 || stored.AlreadyPresent || !stored.DurabilityConfirmed || !outcome.Matches(fixture.intentRecord, fixture.intentRef, attempt, reserveReceipt.Record) {
		t.Fatalf("outcome=%#v stored=%#v err=%v", outcome, stored, err)
	}
	replayedOutcome, replayedStore, err := fixture.session.StoreOutcome(ctx, fixture.intent, reserved, observedExchange, exchange)
	if err != nil || replayedStore.WritesPerformed != 0 || !replayedStore.AlreadyPresent || replayedStore.DurabilityConfirmed || replayedOutcome != outcome || replayedStore.Record != stored.Record {
		t.Fatalf("replayed outcome=%#v stored=%#v err=%v", replayedOutcome, replayedStore, err)
	}
	status, err = fixture.session.Status(ctx, fixture.intentRef.ID)
	if err != nil || !status.Complete || status.State != StateConfirmed || status.Outcome == nil || status.OutcomeRef == nil || *status.Outcome != outcome {
		t.Fatalf("confirmed status=%#v err=%v", status, err)
	}

	changed := fixture.exchangeReceipt(site.BonusExchangeOutcomeUnknown)
	changedObserved, err := site.NewObservedBonusExchange(changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.session.StoreOutcome(ctx, fixture.intent, reserved, changedObserved, changed); !errors.Is(err, ErrCorruptExchange) {
		t.Fatalf("different outcome reused reservation: %v", err)
	}
}

func TestConcurrentAttemptReservationGrantsExactlyOneAuthority(t *testing.T) {
	fixture := newExchangeFixture(t)
	ctx := context.Background()
	second, err := fixture.repository.OpenSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	secondIntent, _, err := second.LoadIntent(ctx, fixture.intentRef.ID)
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		reserved *ReservedAttempt
		receipt  ReserveReceipt
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var group sync.WaitGroup
	for _, candidate := range []struct {
		session *Session
		intent  *VerifiedIntent
	}{{fixture.session, fixture.intent}, {second, secondIntent}} {
		candidate := candidate
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, reserved, receipt, reserveErr := candidate.session.ReserveAttempt(ctx, candidate.intent, fixture.review, fixture.reviewReceipt)
			results <- result{reserved: reserved, receipt: receipt, err: reserveErr}
		}()
	}
	close(start)
	group.Wait()
	close(results)

	acquired, blocked := 0, 0
	for result := range results {
		switch {
		case result.err == nil && result.reserved != nil && result.reserved.Verified() && result.receipt.Acquired &&
			result.receipt.WritesPerformed == 1 && !result.receipt.AlreadyPresent:
			acquired++
		case errors.Is(result.err, ErrAttemptAlreadyReserved) && result.reserved == nil && !result.receipt.Acquired &&
			result.receipt.WritesPerformed == 0 && result.receipt.AlreadyPresent:
			blocked++
		default:
			t.Fatalf("unexpected reservation result: %#v", result)
		}
	}
	if acquired != 1 || blocked != 1 {
		t.Fatalf("acquired=%d blocked=%d", acquired, blocked)
	}
}

func TestStatusFailsClosedForMultipleOutcomesAndBudgetOverflow(t *testing.T) {
	fixture := newExchangeFixture(t)
	ctx := context.Background()
	attempt, reserved, reserveReceipt, err := fixture.session.ReserveAttempt(ctx, fixture.intent, fixture.review, fixture.reviewReceipt)
	if err != nil {
		t.Fatal(err)
	}
	confirmed := fixture.exchangeReceipt(site.BonusExchangeOutcomeConfirmed)
	observed, err := site.NewObservedBonusExchange(confirmed)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.session.StoreOutcome(ctx, fixture.intent, reserved, observed, confirmed); err != nil {
		t.Fatal(err)
	}

	unknown := fixture.exchangeReceipt(site.BonusExchangeOutcomeUnknown)
	second := OutcomeRecord{
		Schema: OutcomeSchemaV1, OperationID: fixture.intentRecord.OperationID,
		IntentRecordID: fixture.intentRef.ID, AttemptRecordID: reserveReceipt.Record.ID,
		RecordedAt: unknown.ObservedAtEnd, Receipt: unknown,
	}
	if !second.Matches(fixture.intentRecord, fixture.intentRef, attempt, reserveReceipt.Record) {
		t.Fatal("second outcome fixture is invalid")
	}
	raw, err := EncodeOutcome(second, fixture.repository.limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.ImportRecord(ctx, metastore.RecordKindSiteBonusExchangeOutcomeV1, bytes.NewReader(raw), fixture.repository.recordLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.session.Status(ctx, fixture.intentRef.ID); !errors.Is(err, ErrCorruptExchange) {
		t.Fatalf("multiple outcomes were not corrupt: %v", err)
	}

	limited := fixture.repository.limits
	limited.MaxStatusRecords = 1
	limitedRepository, err := NewRepository(fixture.store, limited)
	if err != nil {
		t.Fatal(err)
	}
	limitedSession, err := limitedRepository.OpenSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer limitedSession.Close()
	status, err := limitedSession.Status(ctx, fixture.intentRef.ID)
	if !errors.Is(err, ErrStatusIncomplete) || status.Complete || status.State != StateInspectionIncomplete || status.StopReason == "" || status.Used.OutcomeEntriesConsidered < 2 {
		t.Fatalf("limited status=%#v err=%v", status, err)
	}
}

func TestStatusNeverReportsPreparedBeforeInspectionCompletes(t *testing.T) {
	fixture := newExchangeFixture(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	status, err := fixture.session.Status(canceled, fixture.intentRef.ID)
	if !errors.Is(err, context.Canceled) || status.Complete || status.State != StateInspectionIncomplete {
		t.Fatalf("canceled status=%#v err=%v", status, err)
	}

	corrupt := metastore.RecordID("sha256:" + strings.Repeat("f", 64))
	status, err = fixture.session.Status(context.Background(), corrupt)
	if err == nil || status.Complete || status.State != StateInspectionIncomplete {
		t.Fatalf("missing status=%#v err=%v", status, err)
	}
}

func TestOrphanOutcomeCannotReopenPreparedIntent(t *testing.T) {
	fixture := newExchangeFixture(t)
	ctx := context.Background()
	attempt := attemptFromIntent(fixture.intentRecord, fixture.intentRef)
	attemptRaw, err := EncodeAttempt(attempt, fixture.repository.limits)
	if err != nil {
		t.Fatal(err)
	}
	attemptRef, err := metastore.ComputeRecordRef(metastore.RecordKindSiteBonusExchangeAttemptV1, attemptRaw)
	if err != nil {
		t.Fatal(err)
	}
	exchange := fixture.exchangeReceipt(site.BonusExchangeOutcomeConfirmed)
	orphan := OutcomeRecord{
		Schema: OutcomeSchemaV1, OperationID: fixture.intentRecord.OperationID,
		IntentRecordID: fixture.intentRef.ID, AttemptRecordID: attemptRef.ID,
		RecordedAt: exchange.ObservedAtEnd, Receipt: exchange,
	}
	if !orphan.Matches(fixture.intentRecord, fixture.intentRef, attempt, attemptRef) {
		t.Fatal("orphan outcome fixture is invalid")
	}
	raw, err := EncodeOutcome(orphan, fixture.repository.limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.ImportRecord(ctx, metastore.RecordKindSiteBonusExchangeOutcomeV1, bytes.NewReader(raw), fixture.repository.recordLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.session.Status(ctx, fixture.intentRef.ID); !errors.Is(err, ErrCorruptExchange) {
		t.Fatalf("orphan outcome was reported prepared: %v", err)
	}
	_, reserved, receipt, err := fixture.session.ReserveAttempt(ctx, fixture.intent, fixture.review, fixture.reviewReceipt)
	if !errors.Is(err, ErrCorruptExchange) || reserved != nil || receipt.Acquired || receipt.WritesPerformed != 1 {
		t.Fatalf("orphan outcome authorized reservation: reserved=%v receipt=%#v err=%v", reserved, receipt, err)
	}
}

func TestCanonicalRecordDecodersRejectAmbiguityAndPreserveReadErrors(t *testing.T) {
	fixture := newExchangeFixture(t)
	raw, err := EncodeIntent(fixture.intentRecord, fixture.repository.limits)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeIntent(bytes.NewReader(raw), fixture.repository.limits)
	if err != nil || decoded != fixture.intentRecord {
		t.Fatalf("decoded=%#v err=%v", decoded, err)
	}

	base := strings.TrimSuffix(string(raw), "}\n")
	mutations := map[string]string{
		"duplicate key":      base + `,"schema":"` + IntentSchemaV1 + `"}` + "\n",
		"unknown key":        base + `,"unknown":true}` + "\n",
		"trailing JSON":      string(raw) + `{}`,
		"unpaired surrogate": strings.Replace(string(raw), `"site_id":"tjupt"`, `"site_id":"\ud800"`, 1),
	}
	for name, mutation := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeIntent(strings.NewReader(mutation), fixture.repository.limits); !errors.Is(err, ErrCorruptExchange) {
				t.Fatalf("err=%v", err)
			}
		})
	}

	sentinel := errors.New("read sentinel")
	if _, err := DecodeIntent(errorReader{err: sentinel}, fixture.repository.limits); !errors.Is(err, sentinel) || errors.Is(err, ErrCorruptExchange) {
		t.Fatalf("read error classification=%v", err)
	}
}

type exchangeFixture struct {
	store         *metastore.Store
	repository    *Repository
	session       *Session
	intent        *VerifiedIntent
	intentRecord  IntentRecord
	intentRef     metastore.RecordRef
	review        *site.ObservedBonusReview
	reviewReceipt site.BonusReviewReceipt
	exchangeStart time.Time
}

func newExchangeFixture(t *testing.T) exchangeFixture {
	t.Helper()
	physical, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(physical, "state")
	store, _, err := metastore.Init(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("state store retained a resource handle: %v", err)
		}
	})
	repository, err := NewRepository(store, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	config, err := site.NewBonusExchangeConfig("https://www.tjupt.org", "tjupt.bonus.review.v1", "tjupt.bonus.exchange.v1")
	if err != nil {
		t.Fatal(err)
	}
	review, reviewReceipt := newObservedReview(t, config)
	intentRecord, prepared, err := repository.Prepare(context.Background(), PrepareInput{
		SiteID: "tjupt", Selector: "1", ExpectedReviewID: review.PublicCopy().ReviewID, Config: config,
		ReviewLimits: site.DefaultBonusReviewLimits(), ExchangeLimits: site.DefaultBonusExchangeLimits(),
	})
	if err != nil || prepared.WritesPerformed != 1 || prepared.Record.Kind != metastore.RecordKindSiteBonusExchangeIntentV1 {
		t.Fatalf("prepare=%#v err=%v", prepared, err)
	}
	session, err := repository.OpenSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	verified, loaded, err := session.LoadIntent(context.Background(), prepared.Record.ID)
	if err != nil || !verified.Verified() || !loaded.Complete {
		t.Fatalf("loaded=%#v verified=%v err=%v", loaded, verified, err)
	}
	return exchangeFixture{
		store: store, repository: repository, session: session, intent: verified,
		intentRecord: intentRecord, intentRef: prepared.Record, review: review, reviewReceipt: reviewReceipt,
		exchangeStart: time.Date(2026, 8, 12, 5, 6, 7, 0, time.UTC),
	}
}

func newObservedReview(t *testing.T, config site.BonusExchangeConfig) (*site.ObservedBonusReview, site.BonusReviewReceipt) {
	t.Helper()
	review := domain.BonusOfferReview{
		SiteID: "tjupt", Selector: "1", Availability: site.BonusReviewAvailabilityAvailable,
		InputMode: site.BonusReviewInputNone, ActionMethod: "post", ActionRouteID: config.ActionRouteID,
		FormShapeID: "sha256:" + strings.Repeat("b", 64), Columns: []string{"Gift", "100"}, Balance: "12345.67",
		EvidenceBasis: []string{
			site.BonusReviewBasisAuthenticated, site.BonusReviewBasisExactSelector, site.BonusReviewBasisExactForm,
			site.BonusReviewBasisOneRequest, site.BonusReviewBasisSiteClaimOnly, site.BonusReviewBasisNoSubmission,
		},
	}
	reviewID, err := site.ComputeBonusReviewID(review)
	if err != nil {
		t.Fatal(err)
	}
	review.ReviewID = reviewID
	start := time.Date(2026, 8, 12, 5, 0, 0, 0, time.UTC)
	limits := site.DefaultBonusReviewLimits()
	receipt := site.BonusReviewReceipt{
		Effect: site.BonusReviewReadEffect, SiteID: review.SiteID, Selector: review.Selector,
		Origin: config.Origin, RouteID: config.ReviewRouteID, ObservedAtStart: start, ObservedAtEnd: start.Add(time.Second),
		Complete: true, Limits: limits,
		Used: site.BonusReviewUsage{
			RequestsAttempted: 1, ResponseBytesRead: 512, ResponseBytesKnown: true,
			FormsExamined: 1, FieldsExamined: 2, TokensExamined: 10, VisibleTextBytes: 32,
		},
	}
	observed, err := site.NewObservedBonusReview(review, receipt)
	if err != nil {
		t.Fatal(err)
	}
	return observed, receipt
}

func (fixture exchangeFixture) exchangeReceipt(outcome string) site.BonusExchangeReceipt {
	receipt := site.BonusExchangeReceipt{
		Effect: site.BonusExchangeSubmitEffect, SiteID: fixture.intentRecord.SiteID, Selector: fixture.intentRecord.Selector,
		ExpectedReviewID: fixture.intentRecord.ExpectedReviewID, FreshReviewID: fixture.intentRecord.ExpectedReviewID,
		Origin: fixture.intentRecord.Origin, ReviewRouteID: fixture.intentRecord.ReviewRouteID, ActionRouteID: fixture.intentRecord.ActionRouteID,
		ObservedAtStart: fixture.exchangeStart, ObservedAtEnd: fixture.exchangeStart.Add(time.Second), Complete: true,
		RequestAttempted: true, ResponseComplete: true, Outcome: outcome, Limits: fixture.intentRecord.ExchangeLimits,
		Used: site.BonusExchangeUsage{
			TotalRequestsAttempted: 2, ReviewRequestsAttempted: 1, SubmissionRequestsAttempted: 1,
			FormFieldsSubmitted: 2, FormBytesSubmitted: 24, ResponseBytesKnown: true,
		},
	}
	if outcome == site.BonusExchangeOutcomeConfirmed {
		receipt.ConfirmationCode = "tjupt.bonus_exchange.confirm.upload.v1"
	} else {
		receipt.StopReason = "submission_response_unrecognized"
	}
	return receipt
}

type errorReader struct{ err error }

func (reader errorReader) Read([]byte) (int, error) { return 0, reader.err }
