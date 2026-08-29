package bonusexchange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

type PrepareInput struct {
	SiteID           string
	Selector         string
	ExpectedReviewID string
	Config           site.BonusExchangeConfig
	ReviewLimits     site.BonusReviewLimits
	ExchangeLimits   site.BonusExchangeLimits
}

func (input PrepareInput) Validate() error {
	if !safeText(input.SiteID, 128) || !safeText(input.Selector, 128) || site.ValidateBonusReviewID(input.ExpectedReviewID) != nil ||
		input.Config.Validate() != nil || input.ReviewLimits.Validate() != nil || input.ExchangeLimits.Validate() != nil {
		return fmt.Errorf("%w: prepare input is invalid", ErrInvalidExchange)
	}
	return nil
}

type Repository struct {
	store                      *metastore.Store
	limits                     Limits
	operationListBetweenPasses func()
	retentionTransitionHook    func(string) error
}

func NewRepository(store *metastore.Store, limits Limits) (*Repository, error) {
	if store == nil || store.Info().StoreID == "" {
		return nil, fmt.Errorf("bonus exchange store is unavailable")
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return &Repository{store: store, limits: limits}, nil
}

func (repository *Repository) Prepare(ctx context.Context, input PrepareInput) (IntentRecord, PrepareReceipt, error) {
	receipt := PrepareReceipt{Effect: PrepareEffect}
	if repository == nil || repository.store == nil {
		return IntentRecord{}, receipt, fmt.Errorf("prepare bonus exchange: repository is unavailable")
	}
	receipt.Store = repository.store.Info()
	if err := input.Validate(); err != nil {
		return IntentRecord{}, receipt, err
	}
	if err := ctx.Err(); err != nil {
		return IntentRecord{}, receipt, err
	}
	operationID, err := NewOperationID()
	if err != nil {
		return IntentRecord{}, receipt, err
	}
	record := IntentRecord{
		Schema: IntentSchemaV1, OperationID: operationID,
		SiteID: input.SiteID, Selector: input.Selector, ExpectedReviewID: input.ExpectedReviewID,
		Origin: input.Config.Origin, ReviewRouteID: input.Config.ReviewRouteID, ActionRouteID: input.Config.ActionRouteID,
		InputMode: site.BonusReviewInputNone, CreatedAt: time.Now().UTC(), ReviewLimits: input.ReviewLimits, ExchangeLimits: input.ExchangeLimits,
	}
	raw, err := EncodeIntent(record, repository.limits)
	if err != nil {
		return IntentRecord{}, receipt, err
	}
	ref, imported, err := repository.store.ImportRecord(ctx, metastore.RecordKindSiteBonusExchangeIntentV1, bytes.NewReader(raw), repository.recordLimits())
	receipt.WritesPerformed = imported.WritesPerformed
	receipt.Record = ref
	receipt.OperationID = operationID
	if err != nil {
		return record, receipt, err
	}
	want, err := metastore.ComputeRecordRef(metastore.RecordKindSiteBonusExchangeIntentV1, raw)
	if err != nil || ref != want || imported.AlreadyPresent || imported.WritesPerformed != 1 {
		return record, receipt, fmt.Errorf("prepare bonus exchange: intent publication is inconsistent")
	}
	return record, receipt, nil
}

func (repository *Repository) OpenSession(ctx context.Context) (*Session, error) {
	if repository == nil || repository.store == nil {
		return nil, fmt.Errorf("open bonus exchange state session: repository is unavailable")
	}
	bound, err := repository.store.OpenBoundRecordSession(ctx)
	if err != nil {
		return nil, err
	}
	return &Session{repository: repository, bound: bound}, nil
}

type Session struct {
	repository *Repository
	bound      *metastore.BoundRecordSession
	mu         sync.Mutex
	closed     bool
}

func (session *Session) LoadIntent(ctx context.Context, id metastore.RecordID) (*VerifiedIntent, LoadReceipt, error) {
	receipt := LoadReceipt{Effect: LoadEffect}
	if err := session.ready(ctx); err != nil {
		return nil, receipt, err
	}
	receipt.Store = session.bound.Info()
	parsedID, err := metastore.ParseRecordID(id.String())
	if err != nil || parsedID != id {
		return nil, receipt, fmt.Errorf("load bonus exchange intent: record identity is invalid")
	}
	var record IntentRecord
	var decodeErr error
	ref, loaded, err := session.bound.LoadRecord(ctx, metastore.RecordKindSiteBonusExchangeIntentV1, id, session.repository.recordLimits(), func(reader io.Reader) error {
		decoded, innerErr := DecodeIntent(reader, session.repository.limits)
		if innerErr != nil {
			decodeErr = innerErr
			return innerErr
		}
		record = decoded
		return nil
	})
	receipt.RecordBytesRead = loaded.RecordBytesRead
	receipt.ConsumerBytesRead = loaded.ConsumerBytesRead
	receipt.Record = ref
	if err != nil {
		if decodeErr != nil || errors.Is(err, metastore.ErrCorruptRecord) || errors.Is(err, metastore.ErrRecordConsumerIncomplete) {
			return nil, receipt, fmt.Errorf("%w: intent verification failed", ErrCorruptExchange)
		}
		return nil, receipt, err
	}
	if ref.ID != id || ref.Kind != metastore.RecordKindSiteBonusExchangeIntentV1 || record.Validate() != nil {
		return nil, receipt, fmt.Errorf("%w: intent identity disagrees", ErrCorruptExchange)
	}
	verified := &VerifiedIntent{record: record, ref: ref, store: session.bound.Info(), authority: &verifiedIntentAuthority{
		session: session, storeID: session.bound.Info().StoreID, intentID: ref.ID, operationID: record.OperationID,
	}}
	if !verified.validFor(session) {
		return nil, receipt, fmt.Errorf("%w: intent authority is invalid", ErrCorruptExchange)
	}
	receipt.Complete = true
	return verified, receipt, nil
}

// ReserveAttempt publishes the deterministic at-most-once marker only after
// a fresh process-local review reproduces the prepared intent. AlreadyPresent
// is terminal for this intent and never authorizes another POST.
func (session *Session) ReserveAttempt(ctx context.Context, intent *VerifiedIntent, observed *site.ObservedBonusReview, reviewReceipt site.BonusReviewReceipt) (AttemptRecord, *ReservedAttempt, ReserveReceipt, error) {
	receipt := ReserveReceipt{Effect: ReserveEffect}
	if err := session.ready(ctx); err != nil {
		return AttemptRecord{}, nil, receipt, err
	}
	receipt.Store = session.bound.Info()
	if !intent.validFor(session) || !freshReviewMatchesIntent(intent.record, observed, reviewReceipt) {
		return AttemptRecord{}, nil, receipt, fmt.Errorf("%w: fresh review authority does not match intent", ErrInvalidExchange)
	}
	attempt := attemptFromIntent(intent.record, intent.ref)
	raw, err := EncodeAttempt(attempt, session.repository.limits)
	if err != nil {
		return AttemptRecord{}, nil, receipt, err
	}
	want, err := metastore.ComputeRecordRef(metastore.RecordKindSiteBonusExchangeAttemptV1, raw)
	if err != nil {
		return AttemptRecord{}, nil, receipt, err
	}
	ref, imported, err := session.bound.ImportRecord(ctx, want.Kind, bytes.NewReader(raw), session.repository.recordLimits())
	receipt.WritesPerformed = imported.WritesPerformed
	receipt.AlreadyPresent = imported.AlreadyPresent
	receipt.Record = ref
	if err != nil {
		return attempt, nil, receipt, err
	}
	if ref != want || imported.WritesPerformed < 0 || imported.WritesPerformed > 1 || imported.AlreadyPresent == (imported.WritesPerformed == 1) {
		return attempt, nil, receipt, fmt.Errorf("%w: attempt publication is inconsistent", ErrCorruptExchange)
	}
	if imported.AlreadyPresent {
		return attempt, nil, receipt, ErrAttemptAlreadyReserved
	}
	if imported.WritesPerformed != 1 {
		return attempt, nil, receipt, fmt.Errorf("%w: attempt marker was not durably published", ErrCorruptExchange)
	}
	matchedOutcome, _, _, stopReason, scanErr := session.scanOutcomes(ctx, intent.record, intent.ref, attempt, ref)
	if scanErr != nil {
		if errors.Is(scanErr, ErrStatusIncomplete) {
			return attempt, nil, receipt, fmt.Errorf("%w: %s", ErrStatusIncomplete, stopReason)
		}
		return attempt, nil, receipt, scanErr
	}
	if matchedOutcome != nil {
		return attempt, nil, receipt, fmt.Errorf("%w: outcome exists without a prior observable attempt marker", ErrCorruptExchange)
	}
	verified, err := session.bound.VerifyRecordSet(ctx, []metastore.RecordRef{intent.ref, ref}, session.repository.recordLimits())
	if err != nil || !verified.Complete || verified.RecordsVerified != 2 {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return attempt, nil, receipt, err
		}
		return attempt, nil, receipt, fmt.Errorf("%w: attempt marker set could not be verified", ErrCorruptExchange)
	}
	receipt.Acquired = true
	reserved := &ReservedAttempt{record: attempt, ref: ref, store: session.bound.Info(), authority: &reservedAttemptAuthority{
		session: session, intent: intent.authority, storeID: session.bound.Info().StoreID, attemptID: ref.ID,
	}}
	if !reserved.validFor(session, intent) {
		return attempt, nil, receipt, fmt.Errorf("%w: attempt authority is invalid", ErrCorruptExchange)
	}
	return attempt, reserved, receipt, nil
}

func (session *Session) StoreOutcome(ctx context.Context, intent *VerifiedIntent, reserved *ReservedAttempt, observed *site.ObservedBonusExchange, exchange site.BonusExchangeReceipt) (OutcomeRecord, OutcomeReceipt, error) {
	receipt := OutcomeReceipt{Effect: OutcomeEffect}
	if err := session.ready(ctx); err != nil {
		return OutcomeRecord{}, receipt, err
	}
	receipt.Store = session.bound.Info()
	if !intent.validFor(session) || !reserved.validFor(session, intent) || observed == nil || !observed.MatchesReceipt(exchange) ||
		!observed.Matches(intent.record.SiteID, intent.record.Selector, intent.record.ExpectedReviewID, intent.record.Config()) {
		return OutcomeRecord{}, receipt, fmt.Errorf("%w: outcome authority is unavailable", ErrInvalidExchange)
	}
	if !reserved.bindOutcome(observed, exchange) {
		return OutcomeRecord{}, receipt, fmt.Errorf("%w: attempt is already bound to a different outcome", ErrCorruptExchange)
	}
	attempt := attemptFromIntent(intent.record, intent.ref)
	attemptRef := reserved.ref
	if !reserved.record.MatchesIntent(intent.record, intent.ref) || reserved.record != attempt {
		return OutcomeRecord{}, receipt, fmt.Errorf("%w: attempt identity is invalid", ErrInvalidExchange)
	}
	outcome := OutcomeRecord{
		Schema: OutcomeSchemaV1, OperationID: intent.record.OperationID,
		IntentRecordID: intent.ref.ID, AttemptRecordID: attemptRef.ID,
		RecordedAt: exchange.ObservedAtEnd, Receipt: exchange,
	}
	if !outcome.Matches(intent.record, intent.ref, attempt, attemptRef) {
		return OutcomeRecord{}, receipt, fmt.Errorf("%w: outcome does not match intent", ErrInvalidExchange)
	}
	raw, err := EncodeOutcome(outcome, session.repository.limits)
	if err != nil {
		return OutcomeRecord{}, receipt, err
	}
	want, err := metastore.ComputeRecordRef(metastore.RecordKindSiteBonusExchangeOutcomeV1, raw)
	if err != nil {
		return OutcomeRecord{}, receipt, err
	}
	ref, imported, err := session.bound.ImportRecord(ctx, want.Kind, bytes.NewReader(raw), session.repository.recordLimits())
	receipt.WritesPerformed = imported.WritesPerformed
	receipt.AlreadyPresent = imported.AlreadyPresent
	receipt.Record = ref
	if err != nil {
		return outcome, receipt, err
	}
	if ref != want || imported.WritesPerformed < 0 || imported.WritesPerformed > 1 || imported.AlreadyPresent == (imported.WritesPerformed == 1) {
		return outcome, receipt, fmt.Errorf("%w: outcome publication is inconsistent", ErrCorruptExchange)
	}
	verified, err := session.bound.VerifyRecordSet(ctx, []metastore.RecordRef{intent.ref, attemptRef, ref}, session.repository.recordLimits())
	if err != nil || !verified.Complete || verified.RecordsVerified != 3 {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return outcome, receipt, err
		}
		return outcome, receipt, fmt.Errorf("%w: outcome record set could not be verified", ErrCorruptExchange)
	}
	// Only a newly published object whose ImportRecord call completed can prove
	// publication durability in this invocation. Reopening an already-visible
	// object verifies its current bytes and identity, but cannot reconstruct the
	// historical directory-fsync/no-clobber receipt.
	receipt.DurabilityConfirmed = imported.WritesPerformed == 1 && !imported.AlreadyPresent
	return outcome, receipt, nil
}

func (session *Session) Status(ctx context.Context, intentID metastore.RecordID) (Status, error) {
	status := Status{Effect: StatusEffect, State: StateInspectionIncomplete, Store: sessionInfo(session)}
	if err := session.ready(ctx); err != nil {
		return status, err
	}
	forget, forgetRef, foundForget, err := session.findForgetByIntent(ctx, intentID, &status.Used.Retention)
	if err != nil {
		status.StopReason = retentionStopReason(err)
		return status, err
	}
	if foundForget {
		if forget.StoreID != session.bound.Info().StoreID {
			return status, fmt.Errorf("%w: forget marker store identity disagrees", ErrCorruptExchange)
		}
		applyRetentionStatus(&status, forget.Retention, forget.RetentionRef)
		status.Forget = &forget
		status.ForgetRef = &forgetRef
		status.State = StateForgetting
		status.Complete = true
		return status, nil
	}
	retention, retentionRef, foundRetention, err := session.findRetentionByIntent(ctx, intentID, &status.Used.Retention)
	if err != nil {
		status.StopReason = retentionStopReason(err)
		return status, err
	}
	if foundRetention {
		if retention.StoreID != session.bound.Info().StoreID {
			return status, fmt.Errorf("%w: retention marker store identity disagrees", ErrCorruptExchange)
		}
		applyRetentionStatus(&status, retention, retentionRef)
		remaining, presenceErr := session.retainedOriginalsRemain(ctx, retention)
		if presenceErr != nil {
			status.StopReason = retentionStopReason(presenceErr)
			return status, presenceErr
		}
		status.State = StatePruned
		if remaining {
			status.State = StatePruning
		}
		status.Complete = true
		return status, nil
	}
	intent, _, err := session.LoadIntent(ctx, intentID)
	if err != nil {
		return status, err
	}
	status.Intent, status.IntentRef, status.Store = intent.PublicCopy()
	attempt := attemptFromIntent(intent.record, intent.ref)
	attemptRaw, err := EncodeAttempt(attempt, session.repository.limits)
	if err != nil {
		return status, err
	}
	attemptRef, err := metastore.ComputeRecordRef(metastore.RecordKindSiteBonusExchangeAttemptV1, attemptRaw)
	if err != nil {
		return status, err
	}
	matchedRecord, matchedRef, outcomeUsage, stopReason, outcomeErr := session.scanOutcomes(ctx, intent.record, intent.ref, attempt, attemptRef)
	status.Used.OutcomeEntriesConsidered = outcomeUsage.OutcomeEntriesConsidered
	status.Used.OutcomeRecordsRead = outcomeUsage.OutcomeRecordsRead
	status.Used.OutcomeBytesRead = outcomeUsage.OutcomeBytesRead
	if outcomeErr != nil {
		status.StopReason = stopReason
		return status, outcomeErr
	}
	var loadedAttempt AttemptRecord
	var attemptDecodeErr error
	_, _, attemptErr := session.bound.LoadRecord(ctx, attemptRef.Kind, attemptRef.ID, session.repository.recordLimits(), func(reader io.Reader) error {
		decoded, decodeErr := DecodeAttempt(reader, session.repository.limits)
		if decodeErr != nil {
			attemptDecodeErr = decodeErr
			return decodeErr
		}
		loadedAttempt = decoded
		return nil
	})
	if errors.Is(attemptErr, metastore.ErrRecordNotFound) {
		if matchedRecord != nil {
			return status, fmt.Errorf("%w: outcome exists without its attempt marker", ErrCorruptExchange)
		}
		verified, verifyErr := session.bound.VerifyRecordSet(ctx, []metastore.RecordRef{intent.ref}, session.repository.recordLimits())
		if verifyErr != nil || !verified.Complete {
			if errors.Is(verifyErr, context.Canceled) || errors.Is(verifyErr, context.DeadlineExceeded) {
				return status, verifyErr
			}
			return status, fmt.Errorf("%w: prepared intent could not be reverified", ErrCorruptExchange)
		}
		status.State = StatePrepared
		status.Complete = true
		return status, nil
	}
	if attemptErr != nil {
		if errors.Is(attemptErr, context.Canceled) || errors.Is(attemptErr, context.DeadlineExceeded) {
			return status, attemptErr
		}
		if attemptDecodeErr == nil && !errors.Is(attemptErr, metastore.ErrCorruptRecord) && !errors.Is(attemptErr, metastore.ErrRecordConsumerIncomplete) {
			return status, attemptErr
		}
		return status, fmt.Errorf("%w: attempt marker is invalid", ErrCorruptExchange)
	}
	if !loadedAttempt.MatchesIntent(intent.record, intent.ref) {
		return status, fmt.Errorf("%w: attempt marker is invalid", ErrCorruptExchange)
	}
	status.Attempt = &loadedAttempt
	status.AttemptRef = &attemptRef
	candidateState := StateAttemptReservedUnknown
	verifyRefs := []metastore.RecordRef{intent.ref, attemptRef}
	if matchedRecord != nil {
		status.Outcome = matchedRecord
		status.OutcomeRef = matchedRef
		verifyRefs = append(verifyRefs, *matchedRef)
		candidateState = stateFromOutcome(matchedRecord.Receipt.Outcome)
	}
	verified, err := session.bound.VerifyRecordSet(ctx, verifyRefs, session.repository.recordLimits())
	if err != nil || !verified.Complete || verified.RecordsVerified != len(verifyRefs) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return status, err
		}
		return status, fmt.Errorf("%w: status record set could not be verified", ErrCorruptExchange)
	}
	status.State = candidateState
	status.Complete = true
	return status, nil
}

// ListOperations returns a detected-stable, bounded inventory of verified
// live intents and historical retention/forget markers. It never selects a
// newest record and deliberately leaves the full transition state
// uninspected; Status remains the explicit-ID state transition used after a
// caller chooses one operation.
func (session *Session) ListOperations(ctx context.Context) (OperationListResult, error) {
	result := OperationListResult{
		Effect: ListEffect, Limits: sessionLimits(session), Store: sessionInfo(session), Operations: []OperationSummary{},
	}
	if err := session.ready(ctx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			result.StopReason = "context_cancelled"
		}
		return result, err
	}
	result.Store = session.bound.Info()
	listLimits := session.repository.recordLimits()
	listLimits.MaxEntries = session.repository.limits.MaxStatusEntries
	listLimits.MaxRecords = session.repository.limits.MaxStatusRecords
	listLimits.MaxPathBytes = session.repository.limits.MaxStatusPathBytes

	before, err := session.bound.ListRecords(ctx, metastore.RecordKindSiteBonusExchangeIntentV1, listLimits)
	addOperationListInventoryUsage(&result.Used, before)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			result.StopReason = "context_cancelled"
		}
		return result, err
	}
	if !before.Complete {
		result.StopReason = "intent_inventory_" + before.StopReason
		return result, ErrStatusIncomplete
	}

	seenOperations := make(map[OperationID]struct{}, len(before.Records))
	result.Used.IntentVerificationPasses++
	for _, candidate := range before.Records {
		record, err := session.loadOperationIntent(ctx, candidate, &result)
		if err != nil {
			return result, err
		}
		if _, duplicate := seenOperations[record.OperationID]; duplicate {
			return result, fmt.Errorf("%w: multiple intents share one operation identity", ErrCorruptExchange)
		}
		seenOperations[record.OperationID] = struct{}{}
		result.Operations = append(result.Operations, OperationSummary{
			IntentRecord: candidate, OperationID: record.OperationID, SiteID: record.SiteID, Selector: record.Selector,
			ExpectedReviewID: record.ExpectedReviewID, CreatedAt: record.CreatedAt, Status: "not_inspected",
		})
	}
	if session.repository.operationListBetweenPasses != nil {
		session.repository.operationListBetweenPasses()
	}

	after, err := session.bound.ListRecords(ctx, metastore.RecordKindSiteBonusExchangeIntentV1, listLimits)
	addOperationListInventoryUsage(&result.Used, after)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			result.StopReason = "context_cancelled"
		}
		return result, err
	}
	if !after.Complete {
		result.StopReason = "intent_inventory_" + after.StopReason
		return result, ErrStatusIncomplete
	}
	if !sameRecordRefs(before.Records, after.Records) {
		result.StopReason = "intent_inventory_changed"
		return result, ErrStatusIncomplete
	}
	result.Used.IntentVerificationPasses++
	for _, candidate := range after.Records {
		if _, err := session.loadOperationIntent(ctx, candidate, &result); err != nil {
			return result, err
		}
	}
	retained, err := session.listAllRetentionStable(ctx, &result.Used.Retention)
	if err != nil {
		result.StopReason = retentionStopReason(err)
		return result, err
	}
	forgotten, err := session.listAllForgetStable(ctx, &result.Used.Retention)
	if err != nil {
		result.StopReason = retentionStopReason(err)
		return result, err
	}
	operationIndex := make(map[OperationID]int, len(result.Operations)+len(retained)+len(forgotten))
	for index := range result.Operations {
		operationIndex[result.Operations[index].OperationID] = index
	}
	for _, entry := range retained {
		index, exists := operationIndex[entry.record.OperationID]
		if exists {
			current := &result.Operations[index]
			if current.IntentRecord != entry.record.IntentRef || current.RetentionRecord != nil {
				return result, fmt.Errorf("%w: live and retained operation identities disagree", ErrCorruptExchange)
			}
			copyRef := entry.ref
			current.RetentionRecord = &copyRef
			current.Status = "pruning_not_inspected"
			continue
		}
		copyRef := entry.ref
		operationIndex[entry.record.OperationID] = len(result.Operations)
		result.Operations = append(result.Operations, OperationSummary{
			IntentRecord: entry.record.IntentRef, RetentionRecord: &copyRef, OperationID: entry.record.OperationID,
			SiteID: entry.record.Intent.SiteID, Selector: entry.record.Intent.Selector,
			ExpectedReviewID: entry.record.Intent.ExpectedReviewID, CreatedAt: entry.record.Intent.CreatedAt,
			Status: "pruned_not_inspected",
		})
	}
	for _, entry := range forgotten {
		index, exists := operationIndex[entry.record.OperationID]
		if exists {
			current := &result.Operations[index]
			if current.IntentRecord != entry.record.Retention.IntentRef || current.ForgetRecord != nil ||
				(current.RetentionRecord != nil && *current.RetentionRecord != entry.record.RetentionRef) {
				return result, fmt.Errorf("%w: forget operation identities disagree", ErrCorruptExchange)
			}
			copyRef := entry.ref
			current.ForgetRecord = &copyRef
			current.Status = "forgetting_not_inspected"
			continue
		}
		copyRef := entry.ref
		operationIndex[entry.record.OperationID] = len(result.Operations)
		result.Operations = append(result.Operations, OperationSummary{
			IntentRecord: entry.record.Retention.IntentRef, ForgetRecord: &copyRef, OperationID: entry.record.OperationID,
			SiteID: entry.record.Retention.Intent.SiteID, Selector: entry.record.Retention.Intent.Selector,
			ExpectedReviewID: entry.record.Retention.Intent.ExpectedReviewID, CreatedAt: entry.record.Retention.Intent.CreatedAt,
			Status: "forgetting_not_inspected",
		})
	}
	if len(result.Operations) > session.repository.limits.MaxStatusRecords {
		result.StopReason = "operation_limit"
		return result, ErrStatusIncomplete
	}
	sort.Slice(result.Operations, func(i, j int) bool {
		return result.Operations[i].IntentRecord.ID < result.Operations[j].IntentRecord.ID
	})
	if err := session.bound.Check(ctx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			result.StopReason = "context_cancelled"
		}
		return result, err
	}
	result.Complete = true
	return result, nil
}

func (session *Session) loadOperationIntent(ctx context.Context, candidate metastore.RecordRef, result *OperationListResult) (IntentRecord, error) {
	if err := ctx.Err(); err != nil {
		result.StopReason = "context_cancelled"
		return IntentRecord{}, err
	}
	if candidate.SizeBytes < 0 || candidate.SizeBytes > session.repository.limits.MaxStatusBytes-result.Used.IntentBytesRead {
		result.StopReason = "intent_byte_limit"
		return IntentRecord{}, ErrStatusIncomplete
	}
	var record IntentRecord
	var decodeErr error
	ref, loaded, loadErr := session.bound.LoadRecord(ctx, candidate.Kind, candidate.ID, session.repository.recordLimits(), func(reader io.Reader) error {
		decoded, innerErr := DecodeIntent(reader, session.repository.limits)
		if innerErr != nil {
			decodeErr = innerErr
			return innerErr
		}
		record = decoded
		return nil
	})
	result.Used.IntentRecordsRead++
	result.Used.IntentBytesRead += loaded.RecordBytesRead
	if loadErr != nil {
		switch {
		case errors.Is(loadErr, context.Canceled), errors.Is(loadErr, context.DeadlineExceeded):
			result.StopReason = "context_cancelled"
			return IntentRecord{}, loadErr
		case errors.Is(loadErr, metastore.ErrRecordNotFound):
			result.StopReason = "intent_inventory_changed"
			return IntentRecord{}, ErrStatusIncomplete
		case errors.Is(decodeErr, ErrCorruptExchange), errors.Is(loadErr, metastore.ErrCorruptRecord), errors.Is(loadErr, metastore.ErrRecordConsumerIncomplete):
			return IntentRecord{}, fmt.Errorf("%w: intent record verification failed", ErrCorruptExchange)
		default:
			return IntentRecord{}, loadErr
		}
	}
	if ref != candidate || record.Validate() != nil {
		return IntentRecord{}, fmt.Errorf("%w: intent inventory identity disagrees", ErrCorruptExchange)
	}
	return record, nil
}

func addOperationListInventoryUsage(usage *OperationListUsage, inventory metastore.RecordListResult) {
	if usage == nil {
		return
	}
	usage.InventoryPasses++
	usage.EntriesConsidered += inventory.Used.EntriesConsidered
	usage.IntentRecordsMatched += inventory.Used.RecordsMatched
}

func sameRecordRefs(left, right []metastore.RecordRef) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sessionLimits(session *Session) Limits {
	if session == nil || session.repository == nil {
		return Limits{}
	}
	return session.repository.limits
}

func (session *Session) scanOutcomes(ctx context.Context, intent IntentRecord, intentRef metastore.RecordRef, attempt AttemptRecord, attemptRef metastore.RecordRef) (*OutcomeRecord, *metastore.RecordRef, StatusUsage, string, error) {
	var usage StatusUsage
	listLimits := session.repository.recordLimits()
	listLimits.MaxEntries = session.repository.limits.MaxStatusEntries
	listLimits.MaxRecords = session.repository.limits.MaxStatusRecords
	listLimits.MaxPathBytes = session.repository.limits.MaxStatusPathBytes
	listed, err := session.bound.ListRecords(ctx, metastore.RecordKindSiteBonusExchangeOutcomeV1, listLimits)
	usage.OutcomeEntriesConsidered = listed.Used.EntriesConsidered
	if err != nil {
		return nil, nil, usage, "", err
	}
	if !listed.Complete {
		return nil, nil, usage, "outcome_inventory_" + listed.StopReason, ErrStatusIncomplete
	}
	var matchedRecord *OutcomeRecord
	var matchedRef *metastore.RecordRef
	for _, candidate := range listed.Records {
		if candidate.SizeBytes < 0 || candidate.SizeBytes > session.repository.limits.MaxStatusBytes-usage.OutcomeBytesRead {
			return nil, nil, usage, "outcome_byte_limit", ErrStatusIncomplete
		}
		var decoded OutcomeRecord
		var decodeErr error
		_, loaded, loadErr := session.bound.LoadRecord(ctx, candidate.Kind, candidate.ID, session.repository.recordLimits(), func(reader io.Reader) error {
			value, innerErr := DecodeOutcome(reader, session.repository.limits)
			if innerErr != nil {
				decodeErr = innerErr
				return innerErr
			}
			decoded = value
			return nil
		})
		usage.OutcomeRecordsRead++
		usage.OutcomeBytesRead += loaded.RecordBytesRead
		if loadErr != nil {
			if errors.Is(loadErr, context.Canceled) || errors.Is(loadErr, context.DeadlineExceeded) {
				return nil, nil, usage, "", loadErr
			}
			if decodeErr == nil && !errors.Is(loadErr, metastore.ErrCorruptRecord) && !errors.Is(loadErr, metastore.ErrRecordConsumerIncomplete) {
				return nil, nil, usage, "", loadErr
			}
			return nil, nil, usage, "", fmt.Errorf("%w: outcome record verification failed", ErrCorruptExchange)
		}
		if decoded.OperationID != intent.OperationID {
			continue
		}
		if !decoded.Matches(intent, intentRef, attempt, attemptRef) {
			return nil, nil, usage, "", fmt.Errorf("%w: operation outcome links disagree", ErrCorruptExchange)
		}
		if matchedRecord != nil {
			return nil, nil, usage, "", fmt.Errorf("%w: operation has multiple outcomes", ErrCorruptExchange)
		}
		copyRecord := decoded
		copyRef := candidate
		matchedRecord = &copyRecord
		matchedRef = &copyRef
	}
	return matchedRecord, matchedRef, usage, "", nil
}

func (session *Session) Check(ctx context.Context) error {
	if err := session.ready(ctx); err != nil {
		return err
	}
	return session.bound.Check(ctx)
}

func (session *Session) Close() error {
	if session == nil {
		return nil
	}
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return nil
	}
	session.closed = true
	bound := session.bound
	session.mu.Unlock()
	if bound != nil {
		return bound.Close()
	}
	return nil
}

func (session *Session) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if session == nil {
		return fmt.Errorf("bonus exchange state session is unavailable")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || session.repository == nil || session.bound == nil {
		return fmt.Errorf("bonus exchange state session is closed")
	}
	return nil
}

func (intent *VerifiedIntent) validFor(session *Session) bool {
	return intent != nil && intent.Verified() && session != nil && intent.authority.session == session && session.isOpen()
}

func (attempt *ReservedAttempt) validFor(session *Session, intent *VerifiedIntent) bool {
	return attempt != nil && attempt.Verified() && intent != nil && intent.validFor(session) && attempt.authority.session == session &&
		attempt.authority.intent == intent.authority && attempt.record.MatchesIntent(intent.record, intent.ref) && session.isOpen()
}

func (attempt *ReservedAttempt) bindOutcome(observed *site.ObservedBonusExchange, receipt site.BonusExchangeReceipt) bool {
	if attempt == nil || attempt.authority == nil || observed == nil || !observed.MatchesReceipt(receipt) {
		return false
	}
	attempt.authority.mu.Lock()
	defer attempt.authority.mu.Unlock()
	if attempt.authority.observedOutcome == nil {
		attempt.authority.observedOutcome = observed
		attempt.authority.receipt = receipt
		return true
	}
	return attempt.authority.observedOutcome == observed && attempt.authority.receipt == receipt
}

func (session *Session) isOpen() bool {
	if session == nil {
		return false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return !session.closed && session.bound != nil
}

func (repository *Repository) recordLimits() metastore.RecordLimits {
	limits := metastore.DefaultRecordLimits()
	limits.MaxRecordBytes = repository.limits.MaxRecordBytes
	return limits
}

func attemptFromIntent(intent IntentRecord, ref metastore.RecordRef) AttemptRecord {
	return AttemptRecord{
		Schema: AttemptSchemaV1, OperationID: intent.OperationID, IntentRecordID: ref.ID,
		SiteID: intent.SiteID, Selector: intent.Selector, ExpectedReviewID: intent.ExpectedReviewID,
		Origin: intent.Origin, ReviewRouteID: intent.ReviewRouteID, ActionRouteID: intent.ActionRouteID,
	}
}

func freshReviewMatchesIntent(intent IntentRecord, observed *site.ObservedBonusReview, receipt site.BonusReviewReceipt) bool {
	if observed == nil || !observed.MatchesReceipt(receipt) || !observed.Matches(intent.SiteID, intent.Selector, intent.Origin, intent.ReviewRouteID) ||
		receipt.Limits != intent.ReviewLimits {
		return false
	}
	review := observed.PublicCopy()
	return review.ReviewID == intent.ExpectedReviewID && review.Availability == site.BonusReviewAvailabilityAvailable &&
		review.InputMode == site.BonusReviewInputNone && review.ActionMethod == "post" && review.ActionRouteID == intent.ActionRouteID
}

func stateFromOutcome(outcome string) string {
	switch outcome {
	case site.BonusExchangeOutcomeConfirmed:
		return StateConfirmed
	case site.BonusExchangeOutcomeRejected:
		return StateRejected
	case site.BonusExchangeOutcomeNotSubmitted:
		return StateNotSubmitted
	default:
		return StateUnknown
	}
}

func sessionInfo(session *Session) metastore.StoreInfo {
	if session == nil || session.bound == nil {
		return metastore.StoreInfo{}
	}
	return session.bound.Info()
}

type publicVerifiedIntent struct {
	Intent IntentRecord        `json:"intent"`
	Record metastore.RecordRef `json:"record"`
	Store  metastore.StoreInfo `json:"store"`
}

type publicReservedAttempt struct {
	Attempt AttemptRecord       `json:"attempt"`
	Record  metastore.RecordRef `json:"record"`
	Store   metastore.StoreInfo `json:"store"`
}

func (intent VerifiedIntent) MarshalJSON() ([]byte, error) {
	return json.Marshal(publicVerifiedIntent{Intent: intent.record, Record: intent.ref, Store: intent.store})
}

func (intent *VerifiedIntent) UnmarshalJSON(raw []byte) error {
	if intent == nil || len(raw) == 0 || int64(len(raw)) > hardMaxRecordBytes*2 {
		return fmt.Errorf("verified bonus exchange intent report is invalid")
	}
	var public publicVerifiedIntent
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&public); err != nil {
		return fmt.Errorf("verified bonus exchange intent report is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("verified bonus exchange intent report is invalid")
	}
	intent.record = public.Intent
	intent.ref = public.Record
	intent.store = public.Store
	intent.authority = nil
	return nil
}

func (attempt ReservedAttempt) MarshalJSON() ([]byte, error) {
	return json.Marshal(publicReservedAttempt{Attempt: attempt.record, Record: attempt.ref, Store: attempt.store})
}

func (attempt *ReservedAttempt) UnmarshalJSON(raw []byte) error {
	if attempt == nil || len(raw) == 0 || int64(len(raw)) > hardMaxRecordBytes*2 {
		return fmt.Errorf("reserved bonus exchange attempt report is invalid")
	}
	var public publicReservedAttempt
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&public); err != nil {
		return fmt.Errorf("reserved bonus exchange attempt report is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("reserved bonus exchange attempt report is invalid")
	}
	attempt.record = public.Attempt
	attempt.ref = public.Record
	attempt.store = public.Store
	attempt.authority = nil
	return nil
}
