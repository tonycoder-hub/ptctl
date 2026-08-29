package bonusexchange

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

func TestPruneRejectsPreparedAndSubmissionUnknownWithoutDeletingEvidence(t *testing.T) {
	fixture := newExchangeFixture(t)
	ctx := context.Background()

	prepared, err := fixture.session.Prune(ctx, fixture.intentRef.ID, fixture.intentRecord.OperationID)
	if !errors.Is(err, ErrRetentionPolicy) || prepared.WritesPerformed != 0 || prepared.State != StatePrepared || prepared.StopReason != "operation_not_terminal" {
		t.Fatalf("prepared=%#v err=%v", prepared, err)
	}
	if !recordStillPresent(t, fixture.session, fixture.intentRef) {
		t.Fatal("prepared intent was deleted")
	}

	_, _, attemptReceipt, err := fixture.session.ReserveAttempt(ctx, fixture.intent, fixture.review, fixture.reviewReceipt)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := fixture.session.Prune(ctx, fixture.intentRef.ID, fixture.intentRecord.OperationID)
	if !errors.Is(err, ErrRetentionPolicy) || unknown.WritesPerformed != 0 || unknown.State != StateAttemptReservedUnknown ||
		unknown.StopReason != "operation_not_terminal" {
		t.Fatalf("unknown=%#v err=%v", unknown, err)
	}
	if !recordStillPresent(t, fixture.session, fixture.intentRef) || !recordStillPresent(t, fixture.session, attemptReceipt.Record) {
		t.Fatal("submission-unknown evidence was deleted")
	}
	listed, err := fixture.session.bound.ListRecords(ctx, metastore.RecordKindSiteBonusExchangeRetentionV1, fixture.repository.recordLimits())
	if err != nil || !listed.Complete || len(listed.Records) != 0 {
		t.Fatalf("retention inventory=%#v err=%v", listed, err)
	}
}

func TestPruneReplacesTerminalChainWithExactTombstone(t *testing.T) {
	fixture := newExchangeFixture(t)
	_, _, outcomeRef := completeExchangeForRetention(t, &fixture, site.BonusExchangeOutcomeConfirmed)
	ctx := context.Background()

	receipt, err := fixture.session.Prune(ctx, fixture.intentRef.ID, fixture.intentRecord.OperationID)
	if err != nil || !receipt.Complete || receipt.State != StatePruned || receipt.TerminalState != StateConfirmed ||
		receipt.RetentionWrites != 1 || receipt.RecordsRemoved != 3 || receipt.RecordsAlreadyAbsent != 0 ||
		receipt.WritesPerformed < 4 || !receipt.DurabilityConfirmed || len(receipt.RemovalReceipts) != 3 {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	for _, ref := range []metastore.RecordRef{fixture.intentRef, receipt.RemovalReceipts[1].Record, outcomeRef} {
		if recordStillPresent(t, fixture.session, ref) {
			t.Fatalf("original record remained: %#v", ref)
		}
	}
	retention, ref, err := fixture.session.loadRetention(ctx, receipt.RetentionRecord.ID, &RetentionUsage{})
	if err != nil || ref != receipt.RetentionRecord || retention.Validate() != nil || retention.IntentRef != fixture.intentRef ||
		retention.OutcomeRef != outcomeRef || retention.TerminalState != StateConfirmed {
		t.Fatalf("retention=%#v ref=%#v err=%v", retention, ref, err)
	}

	repeated, err := fixture.session.Prune(ctx, fixture.intentRef.ID, fixture.intentRecord.OperationID)
	if err != nil || !repeated.Complete || repeated.State != StatePruned || repeated.RetentionWrites != 0 ||
		repeated.RecordsRemoved != 0 || repeated.RecordsAlreadyAbsent != 3 || repeated.WritesPerformed != 0 || !repeated.DurabilityConfirmed {
		t.Fatalf("repeated=%#v err=%v", repeated, err)
	}
}

func TestPruneRecoversEveryPostTombstoneCrashBoundary(t *testing.T) {
	for _, stop := range []string{"retention_durable", "original_0_removed", "original_1_removed", "original_2_removed"} {
		t.Run(stop, func(t *testing.T) {
			fixture := newExchangeFixture(t)
			completeExchangeForRetention(t, &fixture, site.BonusExchangeOutcomeUnknown)
			sentinel := errors.New("transition stopped")
			fixture.repository.retentionTransitionHook = func(stage string) error {
				if stage == stop {
					return sentinel
				}
				return nil
			}
			first, err := fixture.session.Prune(context.Background(), fixture.intentRef.ID, fixture.intentRecord.OperationID)
			if !errors.Is(err, sentinel) || first.Complete || first.State != StatePruning || first.RetentionRecord.ID == "" {
				t.Fatalf("first=%#v err=%v", first, err)
			}
			fixture.repository.retentionTransitionHook = nil
			if err := fixture.session.Close(); err != nil {
				t.Fatal(err)
			}
			recoveredSession, err := fixture.repository.OpenSession(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer recoveredSession.Close()
			recovered, err := recoveredSession.Prune(context.Background(), fixture.intentRef.ID, fixture.intentRecord.OperationID)
			if err != nil || !recovered.Complete || recovered.State != StatePruned || !recovered.DurabilityConfirmed {
				t.Fatalf("recovered=%#v err=%v", recovered, err)
			}
		})
	}
}

func TestForgetCrashBoundariesPreserveHonestEvidence(t *testing.T) {
	for _, stop := range []string{"forget_durable", "forget_0_removed"} {
		t.Run(stop, func(t *testing.T) {
			fixture := newExchangeFixture(t)
			completeExchangeForRetention(t, &fixture, site.BonusExchangeOutcomeConfirmed)
			pruned, err := fixture.session.Prune(context.Background(), fixture.intentRef.ID, fixture.intentRecord.OperationID)
			if err != nil || !pruned.Complete {
				t.Fatalf("pruned=%#v err=%v", pruned, err)
			}
			sentinel := errors.New("forget transition stopped")
			fixture.repository.retentionTransitionHook = func(stage string) error {
				if stage == stop {
					return sentinel
				}
				return nil
			}
			started, err := fixture.session.Forget(context.Background(), pruned.RetentionRecord.ID, fixture.intentRecord.OperationID)
			if !errors.Is(err, sentinel) || started.Complete || started.State != StateForgetting || started.ForgetRecord.ID == "" {
				t.Fatalf("started=%#v err=%v", started, err)
			}
			fixture.repository.retentionTransitionHook = nil
			if err := fixture.session.Close(); err != nil {
				t.Fatal(err)
			}
			recoveredSession, err := fixture.repository.OpenSession(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer recoveredSession.Close()
			recovered, err := recoveredSession.Forget(context.Background(), pruned.RetentionRecord.ID, fixture.intentRecord.OperationID)
			if err != nil || !recovered.Complete || recovered.State != "forgotten" || !recovered.DurabilityConfirmed {
				t.Fatalf("recovered=%#v err=%v", recovered, err)
			}
		})
	}

	t.Run("after_last_marker_removed", func(t *testing.T) {
		fixture := newExchangeFixture(t)
		completeExchangeForRetention(t, &fixture, site.BonusExchangeOutcomeConfirmed)
		pruned, err := fixture.session.Prune(context.Background(), fixture.intentRef.ID, fixture.intentRecord.OperationID)
		if err != nil || !pruned.Complete {
			t.Fatalf("pruned=%#v err=%v", pruned, err)
		}
		sentinel := errors.New("forget receipt lost")
		fixture.repository.retentionTransitionHook = func(stage string) error {
			if stage == "forget_1_removed" {
				return sentinel
			}
			return nil
		}
		started, err := fixture.session.Forget(context.Background(), pruned.RetentionRecord.ID, fixture.intentRecord.OperationID)
		if !errors.Is(err, sentinel) || started.Complete || started.RecordsRemoved != 2 || len(started.RemovalReceipts) != 2 {
			t.Fatalf("started=%#v err=%v", started, err)
		}
		fixture.repository.retentionTransitionHook = nil
		if err := fixture.session.Close(); err != nil {
			t.Fatal(err)
		}
		recoveredSession, err := fixture.repository.OpenSession(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer recoveredSession.Close()
		absent, err := recoveredSession.Forget(context.Background(), pruned.RetentionRecord.ID, fixture.intentRecord.OperationID)
		if !errors.Is(err, ErrExchangeNotFound) || absent.Complete || absent.WritesPerformed != 0 ||
			absent.StopReason != "retention_not_found_unattributed" {
			t.Fatalf("unattributed=%#v err=%v", absent, err)
		}
	})
}

func TestForgetRequiresCompletePruneAndErasesLastEvidence(t *testing.T) {
	fixture := newExchangeFixture(t)
	completeExchangeForRetention(t, &fixture, site.BonusExchangeOutcomeRejected)
	sentinel := errors.New("stop after retention")
	fixture.repository.retentionTransitionHook = func(stage string) error {
		if stage == "retention_durable" {
			return sentinel
		}
		return nil
	}
	partial, err := fixture.session.Prune(context.Background(), fixture.intentRef.ID, fixture.intentRecord.OperationID)
	if !errors.Is(err, sentinel) || partial.RetentionRecord.ID == "" {
		t.Fatalf("partial=%#v err=%v", partial, err)
	}
	fixture.repository.retentionTransitionHook = nil
	blocked, err := fixture.session.Forget(context.Background(), partial.RetentionRecord.ID, fixture.intentRecord.OperationID)
	if !errors.Is(err, ErrRetentionPolicy) || blocked.WritesPerformed != 0 || blocked.ForgetRecord.ID != "" || blocked.StopReason != "prune_incomplete" {
		t.Fatalf("blocked=%#v err=%v", blocked, err)
	}

	pruned, err := fixture.session.Prune(context.Background(), fixture.intentRef.ID, fixture.intentRecord.OperationID)
	if err != nil || !pruned.Complete {
		t.Fatalf("pruned=%#v err=%v", pruned, err)
	}
	fixture.repository.retentionTransitionHook = func(stage string) error {
		if stage == "forget_durable" {
			return sentinel
		}
		return nil
	}
	started, err := fixture.session.Forget(context.Background(), pruned.RetentionRecord.ID, fixture.intentRecord.OperationID)
	if !errors.Is(err, sentinel) || started.State != StateForgetting || started.ForgetWrites != 1 || started.ForgetRecord.ID == "" {
		t.Fatalf("started=%#v err=%v", started, err)
	}
	fixture.repository.retentionTransitionHook = nil
	if err := fixture.session.Close(); err != nil {
		t.Fatal(err)
	}
	recoveredSession, err := fixture.repository.OpenSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer recoveredSession.Close()
	forgotten, err := recoveredSession.Forget(context.Background(), pruned.RetentionRecord.ID, fixture.intentRecord.OperationID)
	if err != nil || !forgotten.Complete || forgotten.State != "forgotten" || forgotten.RecordsRemoved != 2 ||
		!forgotten.DurabilityConfirmed || forgotten.WritesPerformed == 0 {
		t.Fatalf("forgotten=%#v err=%v", forgotten, err)
	}
	repeated, err := recoveredSession.Forget(context.Background(), pruned.RetentionRecord.ID, fixture.intentRecord.OperationID)
	if !errors.Is(err, ErrExchangeNotFound) || repeated.Complete || repeated.WritesPerformed != 0 ||
		repeated.StopReason != "retention_not_found_unattributed" {
		t.Fatalf("repeated=%#v err=%v", repeated, err)
	}
}

func TestRetentionFormatsAreCanonicalAndKindSeparated(t *testing.T) {
	fixture := newExchangeFixture(t)
	attempt, attemptRef, outcomeRef := completeExchangeForRetention(t, &fixture, site.BonusExchangeOutcomeNotSubmitted)
	status, err := fixture.session.Status(context.Background(), fixture.intentRef.ID)
	if err != nil {
		t.Fatal(err)
	}
	retention := RetentionRecord{
		Schema: RetentionSchemaV1, Basis: RetentionBasisExactTerminal, StoreID: fixture.store.Info().StoreID,
		OperationID: fixture.intentRecord.OperationID, TerminalState: StateNotSubmitted,
		Intent: fixture.intentRecord, IntentRef: fixture.intentRef, Attempt: attempt, AttemptRef: attemptRef,
		Outcome: *status.Outcome, OutcomeRef: outcomeRef,
	}
	raw, err := EncodeRetention(retention, fixture.repository.limits)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRetention(bytes.NewReader(raw), fixture.repository.limits)
	if err != nil || decoded != retention {
		t.Fatalf("decoded=%#v err=%v", decoded, err)
	}
	retentionRef, err := metastore.ComputeRecordRef(metastore.RecordKindSiteBonusExchangeRetentionV1, raw)
	if err != nil {
		t.Fatal(err)
	}
	forget := ForgetRecord{
		Schema: ForgetSchemaV1, Basis: ForgetBasisExactRetention, StoreID: fixture.store.Info().StoreID,
		OperationID: fixture.intentRecord.OperationID, Retention: retention, RetentionRef: retentionRef,
	}
	forgetRaw, err := EncodeForget(forget, fixture.repository.limits)
	if err != nil {
		t.Fatal(err)
	}
	decodedForget, err := DecodeForget(bytes.NewReader(forgetRaw), fixture.repository.limits)
	if err != nil || decodedForget != forget {
		t.Fatalf("decoded forget=%#v err=%v", decodedForget, err)
	}
	forgetRef, err := metastore.ComputeRecordRef(metastore.RecordKindSiteBonusExchangeForgetV1, forgetRaw)
	if err != nil || forgetRef.ID == retentionRef.ID {
		t.Fatalf("retention=%#v forget=%#v err=%v", retentionRef, forgetRef, err)
	}
}

func TestStatusAndListExposePruningPrunedAndForgettingBoundaries(t *testing.T) {
	fixture := newExchangeFixture(t)
	completeExchangeForRetention(t, &fixture, site.BonusExchangeOutcomeConfirmed)
	sentinel := errors.New("transition stopped")
	fixture.repository.retentionTransitionHook = func(stage string) error {
		if stage == "retention_durable" {
			return sentinel
		}
		return nil
	}
	partial, err := fixture.session.Prune(context.Background(), fixture.intentRef.ID, fixture.intentRecord.OperationID)
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	status, err := fixture.session.Status(context.Background(), fixture.intentRef.ID)
	if err != nil || !status.Complete || status.State != StatePruning || status.RetentionRef == nil ||
		status.RetentionRef.ID != partial.RetentionRecord.ID || status.ForgetRef != nil {
		t.Fatalf("pruning status=%#v err=%v", status, err)
	}
	listed, err := fixture.session.ListOperations(context.Background())
	if err != nil || !listed.Complete || len(listed.Operations) != 1 || listed.Operations[0].Status != "pruning_not_inspected" ||
		listed.Operations[0].RetentionRecord == nil {
		t.Fatalf("pruning list=%#v err=%v", listed, err)
	}

	fixture.repository.retentionTransitionHook = nil
	pruned, err := fixture.session.Prune(context.Background(), fixture.intentRef.ID, fixture.intentRecord.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	status, err = fixture.session.Status(context.Background(), fixture.intentRef.ID)
	if err != nil || status.State != StatePruned || !status.Complete || status.RetentionRef == nil {
		t.Fatalf("pruned status=%#v err=%v", status, err)
	}
	listed, err = fixture.session.ListOperations(context.Background())
	if err != nil || len(listed.Operations) != 1 || listed.Operations[0].Status != "pruned_not_inspected" ||
		listed.Operations[0].IntentRecord != fixture.intentRef {
		t.Fatalf("pruned list=%#v err=%v", listed, err)
	}

	fixture.repository.retentionTransitionHook = func(stage string) error {
		if stage == "forget_durable" {
			return sentinel
		}
		return nil
	}
	started, err := fixture.session.Forget(context.Background(), pruned.RetentionRecord.ID, fixture.intentRecord.OperationID)
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	status, err = fixture.session.Status(context.Background(), fixture.intentRef.ID)
	if err != nil || status.State != StateForgetting || !status.Complete || status.ForgetRef == nil ||
		status.ForgetRef.ID != started.ForgetRecord.ID {
		t.Fatalf("forgetting status=%#v err=%v", status, err)
	}
	listed, err = fixture.session.ListOperations(context.Background())
	if err != nil || len(listed.Operations) != 1 || listed.Operations[0].Status != "forgetting_not_inspected" ||
		listed.Operations[0].ForgetRecord == nil {
		t.Fatalf("forgetting list=%#v err=%v", listed, err)
	}
}

func completeExchangeForRetention(t *testing.T, fixture *exchangeFixture, outcome string) (AttemptRecord, metastore.RecordRef, metastore.RecordRef) {
	t.Helper()
	attempt, reserved, reservedReceipt, err := fixture.session.ReserveAttempt(context.Background(), fixture.intent, fixture.review, fixture.reviewReceipt)
	if err != nil {
		t.Fatal(err)
	}
	exchange := fixture.exchangeReceipt(outcome)
	if outcome == site.BonusExchangeOutcomeNotSubmitted {
		exchange.RequestAttempted = false
		exchange.ResponseComplete = false
		exchange.Used.SubmissionRequestsAttempted = 0
		exchange.Used.TotalRequestsAttempted = 1
		exchange.Used.FormFieldsSubmitted = 0
		exchange.Used.FormBytesSubmitted = 0
		exchange.Used.ResponseBytesRead = 0
		exchange.Used.ResponseBytesKnown = false
		exchange.StopReason = "submission_not_sent"
	}
	observed, err := site.NewObservedBonusExchange(exchange)
	if err != nil {
		t.Fatal(err)
	}
	_, stored, err := fixture.session.StoreOutcome(context.Background(), fixture.intent, reserved, observed, exchange)
	if err != nil {
		t.Fatal(err)
	}
	return attempt, reservedReceipt.Record, stored.Record
}

func recordStillPresent(t *testing.T, session *Session, ref metastore.RecordRef) bool {
	t.Helper()
	loaded, _, err := session.bound.LoadRecord(context.Background(), ref.Kind, ref.ID, session.repository.recordLimits(), func(reader io.Reader) error {
		_, readErr := io.Copy(io.Discard, reader)
		return readErr
	})
	if errors.Is(err, metastore.ErrRecordNotFound) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	if loaded != ref {
		t.Fatalf("record identity changed: got=%#v want=%#v", loaded, ref)
	}
	return true
}
