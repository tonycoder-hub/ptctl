package bonusexchange

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/tonycoder-hub/ptctl/internal/metastore"
)

type retentionInventoryEntry struct {
	record RetentionRecord
	ref    metastore.RecordRef
}

type forgetInventoryEntry struct {
	record ForgetRecord
	ref    metastore.RecordRef
}

// Prune replaces one explicitly selected terminal operation's executable
// intent/attempt/outcome chain with a deterministic non-executable tombstone.
// It never deletes prepared or submission-unknown state.
func (session *Session) Prune(ctx context.Context, intentID metastore.RecordID, operationID OperationID) (RetentionReceipt, error) {
	receipt := RetentionReceipt{
		Effect: PruneEffect, State: StateInspectionIncomplete, OperationID: operationID, IntentRecordID: intentID,
		RemovalReceipts: []metastore.RecordRemovalReceipt{}, Store: sessionInfo(session),
	}
	if err := session.ready(ctx); err != nil {
		return receipt, err
	}
	parsedIntent, intentErr := metastore.ParseRecordID(intentID.String())
	parsedOperation, operationErr := ParseOperationID(operationID.String())
	if intentErr != nil || parsedIntent != intentID || operationErr != nil || parsedOperation != operationID {
		return receipt, fmt.Errorf("%w: prune selector is invalid", ErrInvalidExchange)
	}
	receipt.Store = session.bound.Info()

	forget, _, foundForget, err := session.findForgetByIntent(ctx, intentID, &receipt.Used)
	if err != nil {
		receipt.StopReason = retentionStopReason(err)
		return receipt, err
	}
	if foundForget {
		if forget.OperationID != operationID || forget.StoreID != receipt.Store.StoreID {
			return receipt, fmt.Errorf("%w: forget marker selector disagrees", ErrCorruptExchange)
		}
		receipt.State = StateForgetting
		receipt.RetentionRecord = forget.RetentionRef
		receipt.TerminalState = forget.Retention.TerminalState
		receipt.SiteID = forget.Retention.Intent.SiteID
		receipt.Selector = forget.Retention.Intent.Selector
		receipt.ExpectedReviewID = forget.Retention.Intent.ExpectedReviewID
		receipt.StopReason = "forget_in_progress"
		return receipt, fmt.Errorf("%w: forget has already crossed the retention boundary", ErrRetentionPolicy)
	}

	retention, retentionRef, foundRetention, err := session.findRetentionByIntent(ctx, intentID, &receipt.Used)
	if err != nil {
		receipt.StopReason = retentionStopReason(err)
		return receipt, err
	}
	if foundRetention {
		if retention.OperationID != operationID || retention.StoreID != receipt.Store.StoreID {
			return receipt, fmt.Errorf("%w: retention selector disagrees", ErrCorruptExchange)
		}
		return session.completePrune(ctx, retention, retentionRef, receipt)
	}

	status, err := session.Status(ctx, intentID)
	if err != nil {
		if errors.Is(err, metastore.ErrRecordNotFound) {
			receipt.StopReason = "operation_not_found"
			return receipt, ErrExchangeNotFound
		}
		receipt.StopReason = retentionStopReason(err)
		return receipt, err
	}
	receipt.State = status.State
	receipt.SiteID = status.Intent.SiteID
	receipt.Selector = status.Intent.Selector
	receipt.ExpectedReviewID = status.Intent.ExpectedReviewID
	if status.Intent.OperationID != operationID {
		return receipt, fmt.Errorf("%w: operation selector disagrees", ErrInvalidExchange)
	}
	if !status.Complete || !terminalRetentionState(status.State) || status.Attempt == nil || status.AttemptRef == nil ||
		status.Outcome == nil || status.OutcomeRef == nil {
		receipt.StopReason = "operation_not_terminal"
		return receipt, fmt.Errorf("%w: only a complete terminal operation may be pruned", ErrRetentionPolicy)
	}

	retention = RetentionRecord{
		Schema: RetentionSchemaV1, Basis: RetentionBasisExactTerminal, StoreID: receipt.Store.StoreID,
		OperationID: operationID, TerminalState: status.State,
		Intent: status.Intent, IntentRef: status.IntentRef,
		Attempt: *status.Attempt, AttemptRef: *status.AttemptRef,
		Outcome: *status.Outcome, OutcomeRef: *status.OutcomeRef,
	}
	raw, err := EncodeRetention(retention, session.repository.limits)
	if err != nil {
		return receipt, err
	}
	want, err := metastore.ComputeRecordRef(metastore.RecordKindSiteBonusExchangeRetentionV1, raw)
	if err != nil {
		return receipt, err
	}
	retentionRef, imported, err := session.bound.ImportRecord(ctx, want.Kind, bytes.NewReader(raw), session.repository.recordLimits())
	receipt.RetentionWrites = imported.WritesPerformed
	receipt.WritesPerformed += imported.WritesPerformed
	receipt.RetentionRecord = retentionRef
	receipt.TerminalState = retention.TerminalState
	receipt.SiteID = retention.Intent.SiteID
	receipt.Selector = retention.Intent.Selector
	receipt.ExpectedReviewID = retention.Intent.ExpectedReviewID
	receipt.State = StatePruning
	if err != nil {
		receipt.StopReason = retentionStopReason(err)
		return receipt, err
	}
	if retentionRef != want || imported.WritesPerformed < 0 || imported.WritesPerformed > 1 || imported.AlreadyPresent == (imported.WritesPerformed == 1) {
		return receipt, fmt.Errorf("%w: retention publication is inconsistent", ErrCorruptExchange)
	}
	confirmed, err := session.bound.ConfirmRecordSetDurability(ctx,
		[]metastore.RecordRef{retentionRef, retention.IntentRef, retention.AttemptRef, retention.OutcomeRef}, session.repository.recordLimits())
	if err != nil || !confirmed.Complete || !confirmed.DurabilityConfirmed {
		receipt.StopReason = retentionStopReason(err)
		if err == nil {
			err = ErrStatusIncomplete
		}
		return receipt, err
	}
	if err := session.retentionHook("retention_durable"); err != nil {
		receipt.StopReason = "transition_interrupted"
		return receipt, err
	}
	return session.completePrune(ctx, retention, retentionRef, receipt)
}

func (session *Session) completePrune(ctx context.Context, retention RetentionRecord, retentionRef metastore.RecordRef, receipt RetentionReceipt) (RetentionReceipt, error) {
	receipt.State = StatePruning
	receipt.RetentionRecord = retentionRef
	receipt.TerminalState = retention.TerminalState
	if retention.Validate() != nil || retention.StoreID != session.bound.Info().StoreID || retention.IntentRef.ID != receipt.IntentRecordID ||
		retention.OperationID != receipt.OperationID || !exactRecordRef(metastore.RecordKindSiteBonusExchangeRetentionV1, retention, retentionRef) {
		return receipt, fmt.Errorf("%w: retention tombstone is invalid", ErrCorruptExchange)
	}
	confirmed, err := session.bound.ConfirmRecordSetDurability(ctx, []metastore.RecordRef{retentionRef}, session.repository.recordLimits())
	if err != nil || !confirmed.Complete || !confirmed.DurabilityConfirmed {
		receipt.StopReason = retentionStopReason(err)
		if err == nil {
			err = ErrStatusIncomplete
		}
		return receipt, err
	}
	for index, record := range []metastore.RecordRef{retention.OutcomeRef, retention.AttemptRef, retention.IntentRef} {
		removed, removeErr := session.bound.RemoveRecordExact(ctx, record, session.repository.recordLimits())
		receipt.RemovalReceipts = append(receipt.RemovalReceipts, removed)
		receipt.WritesPerformed += removed.WritesPerformed
		if removed.Removed {
			receipt.RecordsRemoved++
		}
		if removed.AlreadyAbsent {
			receipt.RecordsAlreadyAbsent++
		}
		if removeErr != nil {
			receipt.StopReason = retentionStopReason(removeErr)
			return receipt, removeErr
		}
		if err := session.retentionHook(fmt.Sprintf("original_%d_removed", index)); err != nil {
			receipt.StopReason = "transition_interrupted"
			return receipt, err
		}
	}
	confirmed, err = session.bound.ConfirmRecordSetDurability(ctx, []metastore.RecordRef{retentionRef}, session.repository.recordLimits())
	if err != nil || !confirmed.Complete || !confirmed.DurabilityConfirmed {
		receipt.StopReason = retentionStopReason(err)
		if err == nil {
			err = ErrStatusIncomplete
		}
		return receipt, err
	}
	receipt.Complete = true
	receipt.State = StatePruned
	receipt.DurabilityConfirmed = true
	return receipt, nil
}

// Forget irreversibly removes one explicitly selected complete retention
// tombstone. A deterministic forget marker is published first and removed
// last. After the last marker is absent, later invocations cannot infer that a
// prior forget succeeded.
func (session *Session) Forget(ctx context.Context, retentionID metastore.RecordID, operationID OperationID) (ForgetReceipt, error) {
	receipt := ForgetReceipt{
		Effect: ForgetEffect, State: StateInspectionIncomplete, OperationID: operationID,
		RetentionRecord: metastore.RecordRef{Kind: metastore.RecordKindSiteBonusExchangeRetentionV1, ID: retentionID},
		RemovalReceipts: []metastore.RecordRemovalReceipt{}, Store: sessionInfo(session),
	}
	if err := session.ready(ctx); err != nil {
		return receipt, err
	}
	parsedRetention, retentionErr := metastore.ParseRecordID(retentionID.String())
	parsedOperation, operationErr := ParseOperationID(operationID.String())
	if retentionErr != nil || parsedRetention != retentionID || operationErr != nil || parsedOperation != operationID {
		return receipt, fmt.Errorf("%w: forget selector is invalid", ErrInvalidExchange)
	}
	receipt.Store = session.bound.Info()

	forget, forgetRef, foundForget, err := session.findForgetByRetention(ctx, retentionID, &receipt.Used)
	if err != nil {
		receipt.StopReason = retentionStopReason(err)
		return receipt, err
	}
	var retention RetentionRecord
	var retentionRef metastore.RecordRef
	if foundForget {
		if forget.OperationID != operationID || forget.StoreID != receipt.Store.StoreID {
			return receipt, fmt.Errorf("%w: forget selector disagrees", ErrCorruptExchange)
		}
		retention, retentionRef = forget.Retention, forget.RetentionRef
		receipt.ForgetRecord = forgetRef
		receipt.State = StateForgetting
	} else {
		retention, retentionRef, err = session.loadRetention(ctx, retentionID, &receipt.Used)
		if err != nil {
			if errors.Is(err, metastore.ErrRecordNotFound) {
				receipt.StopReason = "retention_not_found_unattributed"
				return receipt, ErrExchangeNotFound
			}
			receipt.StopReason = retentionStopReason(err)
			return receipt, err
		}
		if retention.OperationID != operationID || retention.StoreID != receipt.Store.StoreID {
			return receipt, fmt.Errorf("%w: forget selector disagrees", ErrInvalidExchange)
		}
		receipt.RetentionRecord = retentionRef
		if err := session.ensurePrunedOriginalsAbsent(ctx, retention); err != nil {
			receipt.StopReason = "prune_incomplete"
			return receipt, err
		}
		forget = ForgetRecord{
			Schema: ForgetSchemaV1, Basis: ForgetBasisExactRetention, StoreID: receipt.Store.StoreID,
			OperationID: operationID, Retention: retention, RetentionRef: retentionRef,
		}
		raw, encodeErr := EncodeForget(forget, session.repository.limits)
		if encodeErr != nil {
			return receipt, encodeErr
		}
		want, computeErr := metastore.ComputeRecordRef(metastore.RecordKindSiteBonusExchangeForgetV1, raw)
		if computeErr != nil {
			return receipt, computeErr
		}
		var imported metastore.RecordImportReceipt
		forgetRef, imported, err = session.bound.ImportRecord(ctx, want.Kind, bytes.NewReader(raw), session.repository.recordLimits())
		receipt.ForgetRecord = forgetRef
		receipt.ForgetWrites = imported.WritesPerformed
		receipt.WritesPerformed += imported.WritesPerformed
		receipt.State = StateForgetting
		if err != nil {
			receipt.StopReason = retentionStopReason(err)
			return receipt, err
		}
		if forgetRef != want || imported.WritesPerformed < 0 || imported.WritesPerformed > 1 || imported.AlreadyPresent == (imported.WritesPerformed == 1) {
			return receipt, fmt.Errorf("%w: forget marker publication is inconsistent", ErrCorruptExchange)
		}
	}
	receipt.TerminalState = retention.TerminalState
	receipt.SiteID = retention.Intent.SiteID
	receipt.Selector = retention.Intent.Selector
	receipt.ExpectedReviewID = retention.Intent.ExpectedReviewID
	if err := session.ensurePrunedOriginalsAbsent(ctx, retention); err != nil {
		receipt.StopReason = "prune_incomplete"
		return receipt, err
	}
	confirmed, err := session.bound.ConfirmRecordSetDurability(ctx, []metastore.RecordRef{forgetRef}, session.repository.recordLimits())
	if err != nil || !confirmed.Complete || !confirmed.DurabilityConfirmed {
		receipt.StopReason = retentionStopReason(err)
		if err == nil {
			err = ErrStatusIncomplete
		}
		return receipt, err
	}
	if err := session.retentionHook("forget_durable"); err != nil {
		receipt.StopReason = "transition_interrupted"
		return receipt, err
	}
	for index, record := range []metastore.RecordRef{retentionRef, forgetRef} {
		removed, removeErr := session.bound.RemoveRecordExact(ctx, record, session.repository.recordLimits())
		receipt.RemovalReceipts = append(receipt.RemovalReceipts, removed)
		receipt.WritesPerformed += removed.WritesPerformed
		if removed.Removed {
			receipt.RecordsRemoved++
		}
		if removed.AlreadyAbsent {
			receipt.RecordsAlreadyAbsent++
		}
		if removeErr != nil {
			receipt.StopReason = retentionStopReason(removeErr)
			return receipt, removeErr
		}
		if err := session.retentionHook(fmt.Sprintf("forget_%d_removed", index)); err != nil {
			receipt.StopReason = "transition_interrupted"
			return receipt, err
		}
	}
	if err := session.bound.Check(ctx); err != nil {
		receipt.StopReason = retentionStopReason(err)
		return receipt, err
	}
	receipt.Complete = true
	receipt.State = "forgotten"
	receipt.DurabilityConfirmed = true
	return receipt, nil
}

func (session *Session) ensurePrunedOriginalsAbsent(ctx context.Context, retention RetentionRecord) error {
	remaining, err := session.retainedOriginalsRemain(ctx, retention)
	if err != nil {
		return err
	}
	if remaining {
		return fmt.Errorf("%w: original terminal records remain", ErrRetentionPolicy)
	}
	return nil
}

func (session *Session) retainedOriginalsRemain(ctx context.Context, retention RetentionRecord) (bool, error) {
	for _, record := range []metastore.RecordRef{retention.IntentRef, retention.AttemptRef, retention.OutcomeRef} {
		present, err := session.recordPresent(ctx, record)
		if err != nil {
			return false, err
		}
		if present {
			return true, nil
		}
	}
	return false, nil
}

func applyRetentionStatus(status *Status, retention RetentionRecord, ref metastore.RecordRef) {
	if status == nil {
		return
	}
	status.Intent = retention.Intent
	status.IntentRef = retention.IntentRef
	status.Attempt = &retention.Attempt
	status.AttemptRef = &retention.AttemptRef
	status.Outcome = &retention.Outcome
	status.OutcomeRef = &retention.OutcomeRef
	status.Retention = &retention
	status.RetentionRef = &ref
	status.Store.StoreID = retention.StoreID
}

func (session *Session) recordPresent(ctx context.Context, record metastore.RecordRef) (bool, error) {
	ref, _, err := session.bound.LoadRecord(ctx, record.Kind, record.ID, session.repository.recordLimits(), func(reader io.Reader) error {
		_, readErr := io.Copy(io.Discard, reader)
		return readErr
	})
	if errors.Is(err, metastore.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if ref != record {
		return false, fmt.Errorf("%w: retained record identity disagrees", ErrCorruptExchange)
	}
	return true, nil
}

func (session *Session) findRetentionByIntent(ctx context.Context, intentID metastore.RecordID, usage *RetentionUsage) (RetentionRecord, metastore.RecordRef, bool, error) {
	return session.scanRetentionStable(ctx, usage, func(record RetentionRecord) bool { return record.IntentRef.ID == intentID })
}

func (session *Session) findForgetByIntent(ctx context.Context, intentID metastore.RecordID, usage *RetentionUsage) (ForgetRecord, metastore.RecordRef, bool, error) {
	return session.scanForgetStable(ctx, usage, func(record ForgetRecord) bool { return record.Retention.IntentRef.ID == intentID })
}

func (session *Session) findForgetByRetention(ctx context.Context, retentionID metastore.RecordID, usage *RetentionUsage) (ForgetRecord, metastore.RecordRef, bool, error) {
	return session.scanForgetStable(ctx, usage, func(record ForgetRecord) bool { return record.RetentionRef.ID == retentionID })
}

func (session *Session) scanRetentionStable(ctx context.Context, usage *RetentionUsage, match func(RetentionRecord) bool) (RetentionRecord, metastore.RecordRef, bool, error) {
	var selected RetentionRecord
	var selectedRef metastore.RecordRef
	var before []metastore.RecordRef
	for pass := 0; pass < 2; pass++ {
		listed, err := session.listRetentionKind(ctx, metastore.RecordKindSiteBonusExchangeRetentionV1, usage)
		if err != nil {
			return RetentionRecord{}, metastore.RecordRef{}, false, err
		}
		if pass == 0 {
			before = listed
		} else if !sameRecordRefs(before, listed) {
			return RetentionRecord{}, metastore.RecordRef{}, false, ErrStatusIncomplete
		}
		found := false
		for _, candidate := range listed {
			record, loadedRef, err := session.loadRetentionCandidate(ctx, candidate, usage)
			if err != nil {
				return RetentionRecord{}, metastore.RecordRef{}, false, err
			}
			if !match(record) {
				continue
			}
			if found {
				return RetentionRecord{}, metastore.RecordRef{}, false, fmt.Errorf("%w: multiple retention records match one selector", ErrCorruptExchange)
			}
			found, selected, selectedRef = true, record, loadedRef
		}
		if pass == 1 && !found {
			return RetentionRecord{}, metastore.RecordRef{}, false, nil
		}
	}
	return selected, selectedRef, true, nil
}

func (session *Session) scanForgetStable(ctx context.Context, usage *RetentionUsage, match func(ForgetRecord) bool) (ForgetRecord, metastore.RecordRef, bool, error) {
	var selected ForgetRecord
	var selectedRef metastore.RecordRef
	var before []metastore.RecordRef
	for pass := 0; pass < 2; pass++ {
		listed, err := session.listRetentionKind(ctx, metastore.RecordKindSiteBonusExchangeForgetV1, usage)
		if err != nil {
			return ForgetRecord{}, metastore.RecordRef{}, false, err
		}
		if pass == 0 {
			before = listed
		} else if !sameRecordRefs(before, listed) {
			return ForgetRecord{}, metastore.RecordRef{}, false, ErrStatusIncomplete
		}
		found := false
		for _, candidate := range listed {
			record, err := session.loadForgetCandidate(ctx, candidate, usage)
			if err != nil {
				return ForgetRecord{}, metastore.RecordRef{}, false, err
			}
			if !match(record) {
				continue
			}
			if found {
				return ForgetRecord{}, metastore.RecordRef{}, false, fmt.Errorf("%w: multiple forget records match one selector", ErrCorruptExchange)
			}
			found, selected, selectedRef = true, record, candidate
		}
		if pass == 1 && !found {
			return ForgetRecord{}, metastore.RecordRef{}, false, nil
		}
	}
	return selected, selectedRef, true, nil
}

func (session *Session) listAllRetentionStable(ctx context.Context, usage *RetentionUsage) ([]retentionInventoryEntry, error) {
	var before []metastore.RecordRef
	var result []retentionInventoryEntry
	for pass := 0; pass < 2; pass++ {
		listed, err := session.listRetentionKind(ctx, metastore.RecordKindSiteBonusExchangeRetentionV1, usage)
		if err != nil {
			return nil, err
		}
		if pass == 0 {
			before = listed
		} else if !sameRecordRefs(before, listed) {
			return nil, ErrStatusIncomplete
		}
		entries := make([]retentionInventoryEntry, 0, len(listed))
		for _, candidate := range listed {
			record, ref, err := session.loadRetentionCandidate(ctx, candidate, usage)
			if err != nil {
				return nil, err
			}
			entries = append(entries, retentionInventoryEntry{record: record, ref: ref})
		}
		if pass == 1 {
			result = entries
		}
	}
	return result, nil
}

func (session *Session) listAllForgetStable(ctx context.Context, usage *RetentionUsage) ([]forgetInventoryEntry, error) {
	var before []metastore.RecordRef
	var result []forgetInventoryEntry
	for pass := 0; pass < 2; pass++ {
		listed, err := session.listRetentionKind(ctx, metastore.RecordKindSiteBonusExchangeForgetV1, usage)
		if err != nil {
			return nil, err
		}
		if pass == 0 {
			before = listed
		} else if !sameRecordRefs(before, listed) {
			return nil, ErrStatusIncomplete
		}
		entries := make([]forgetInventoryEntry, 0, len(listed))
		for _, candidate := range listed {
			record, err := session.loadForgetCandidate(ctx, candidate, usage)
			if err != nil {
				return nil, err
			}
			entries = append(entries, forgetInventoryEntry{record: record, ref: candidate})
		}
		if pass == 1 {
			result = entries
		}
	}
	return result, nil
}

func (session *Session) listRetentionKind(ctx context.Context, kind metastore.RecordKind, usage *RetentionUsage) ([]metastore.RecordRef, error) {
	limits := session.repository.recordLimits()
	limits.MaxEntries = session.repository.limits.MaxStatusEntries
	limits.MaxRecords = session.repository.limits.MaxStatusRecords
	limits.MaxPathBytes = session.repository.limits.MaxStatusPathBytes
	listed, err := session.bound.ListRecords(ctx, kind, limits)
	usage.InventoryPasses++
	usage.EntriesConsidered += listed.Used.EntriesConsidered
	if err != nil {
		return nil, err
	}
	if !listed.Complete {
		return nil, ErrStatusIncomplete
	}
	return listed.Records, nil
}

func (session *Session) loadRetention(ctx context.Context, id metastore.RecordID, usage *RetentionUsage) (RetentionRecord, metastore.RecordRef, error) {
	candidate := metastore.RecordRef{Kind: metastore.RecordKindSiteBonusExchangeRetentionV1, ID: id}
	record, ref, err := session.loadRetentionCandidate(ctx, candidate, usage)
	return record, ref, err
}

func (session *Session) loadRetentionCandidate(ctx context.Context, candidate metastore.RecordRef, usage *RetentionUsage) (RetentionRecord, metastore.RecordRef, error) {
	if candidate.SizeBytes > 0 && candidate.SizeBytes > session.repository.limits.MaxStatusBytes-usage.BytesRead {
		return RetentionRecord{}, metastore.RecordRef{}, ErrStatusIncomplete
	}
	var record RetentionRecord
	var decodeErr error
	ref, loaded, err := session.bound.LoadRecord(ctx, candidate.Kind, candidate.ID, session.repository.recordLimits(), func(reader io.Reader) error {
		decoded, innerErr := DecodeRetention(reader, session.repository.limits)
		decodeErr = innerErr
		if innerErr == nil {
			record = decoded
		}
		return innerErr
	})
	usage.RecordsRead++
	usage.BytesRead += loaded.RecordBytesRead
	if err != nil {
		if decodeErr != nil || errors.Is(err, metastore.ErrCorruptRecord) || errors.Is(err, metastore.ErrRecordConsumerIncomplete) {
			return RetentionRecord{}, metastore.RecordRef{}, fmt.Errorf("%w: retention record verification failed", ErrCorruptExchange)
		}
		return RetentionRecord{}, metastore.RecordRef{}, err
	}
	if candidate.SizeBytes != 0 && ref != candidate || record.StoreID != session.bound.Info().StoreID {
		return RetentionRecord{}, metastore.RecordRef{}, fmt.Errorf("%w: retention record identity disagrees", ErrCorruptExchange)
	}
	return record, ref, nil
}

func (session *Session) loadForgetCandidate(ctx context.Context, candidate metastore.RecordRef, usage *RetentionUsage) (ForgetRecord, error) {
	if candidate.SizeBytes > 0 && candidate.SizeBytes > session.repository.limits.MaxStatusBytes-usage.BytesRead {
		return ForgetRecord{}, ErrStatusIncomplete
	}
	var record ForgetRecord
	var decodeErr error
	ref, loaded, err := session.bound.LoadRecord(ctx, candidate.Kind, candidate.ID, session.repository.recordLimits(), func(reader io.Reader) error {
		decoded, innerErr := DecodeForget(reader, session.repository.limits)
		decodeErr = innerErr
		if innerErr == nil {
			record = decoded
		}
		return innerErr
	})
	usage.RecordsRead++
	usage.BytesRead += loaded.RecordBytesRead
	if err != nil {
		if decodeErr != nil || errors.Is(err, metastore.ErrCorruptRecord) || errors.Is(err, metastore.ErrRecordConsumerIncomplete) {
			return ForgetRecord{}, fmt.Errorf("%w: forget record verification failed", ErrCorruptExchange)
		}
		return ForgetRecord{}, err
	}
	if candidate.SizeBytes != 0 && ref != candidate || record.StoreID != session.bound.Info().StoreID {
		return ForgetRecord{}, fmt.Errorf("%w: forget record identity disagrees", ErrCorruptExchange)
	}
	return record, nil
}

func (session *Session) retentionHook(stage string) error {
	if session != nil && session.repository != nil && session.repository.retentionTransitionHook != nil {
		return session.repository.retentionTransitionHook(stage)
	}
	return nil
}

func retentionStopReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "context_cancelled"
	case errors.Is(err, ErrRetentionPolicy):
		return "retention_policy_blocked"
	case errors.Is(err, ErrExchangeNotFound), errors.Is(err, metastore.ErrRecordNotFound):
		return "operation_not_found"
	case errors.Is(err, metastore.ErrRemovalDurabilityUnconfirmed), errors.Is(err, metastore.ErrDurabilityUnconfirmed):
		return "durability_unconfirmed"
	case errors.Is(err, metastore.ErrRemovalAmbiguous):
		return "removal_ambiguous"
	case errors.Is(err, ErrStatusIncomplete):
		return "inventory_incomplete"
	case errors.Is(err, ErrCorruptExchange), errors.Is(err, metastore.ErrCorruptRecord):
		return "integrity_failed"
	default:
		return "operation_interrupted"
	}
}
