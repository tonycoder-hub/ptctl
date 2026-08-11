package metastore

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// BoundRecordSession keeps one verified physical private-store root bound
// across a sequence of record reads, marker publications, an external effect,
// and the final receipt publication. Methods are serialized deliberately: a
// protocol using this authority must have one unambiguous operation order.
//
// Close releases all filesystem authority. A serialized value or StoreInfo
// cannot recreate this session.
type BoundRecordSession struct {
	store   *Store
	session *rootSession
	mu      sync.Mutex
	closed  bool
}

// OpenBoundRecordSession validates the store before returning an
// operation-bound authority. It performs no record or artifact mutation.
func (s *Store) OpenBoundRecordSession(ctx context.Context) (*BoundRecordSession, error) {
	if s == nil {
		return nil, fmt.Errorf("open bound sealed record session: store is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	session, err := s.validatedSession()
	if err != nil {
		return nil, safeError("open bound sealed record session", err)
	}
	return &BoundRecordSession{store: s, session: session}, nil
}

func (bound *BoundRecordSession) Info() StoreInfo {
	if bound == nil || bound.store == nil {
		return StoreInfo{}
	}
	return bound.store.Info()
}

func (bound *BoundRecordSession) Check(ctx context.Context) error {
	if bound == nil {
		return fmt.Errorf("check bound sealed record session: session is unavailable")
	}
	bound.mu.Lock()
	defer bound.mu.Unlock()
	if err := bound.ready(ctx); err != nil {
		return err
	}
	if err := bound.session.check("bound_record_session_check"); err != nil {
		return fmt.Errorf("check bound sealed record session: store identity changed")
	}
	return nil
}

func (bound *BoundRecordSession) ImportRecord(ctx context.Context, kind RecordKind, reader io.Reader, limits RecordLimits) (RecordRef, RecordImportReceipt, error) {
	receipt := RecordImportReceipt{Effect: recordImportEffect, Store: bound.Info()}
	if bound == nil || reader == nil {
		return RecordRef{}, receipt, fmt.Errorf("import sealed record: input is unavailable")
	}
	bound.mu.Lock()
	defer bound.mu.Unlock()
	if err := bound.ready(ctx); err != nil {
		return RecordRef{}, receipt, err
	}
	parsedKind, err := ParseRecordKind(string(kind))
	if err != nil || parsedKind != kind {
		return RecordRef{}, receipt, fmt.Errorf("import sealed record: kind is invalid")
	}
	if err := limits.Validate(); err != nil {
		return RecordRef{}, receipt, err
	}
	return bound.store.importRecordSession(ctx, bound.session, kind, reader, limits)
}

func (bound *BoundRecordSession) LoadRecord(ctx context.Context, kind RecordKind, id RecordID, limits RecordLimits, consume RecordConsumer) (RecordRef, RecordLoadReceipt, error) {
	receipt := RecordLoadReceipt{Effect: recordLoadEffect, Store: bound.Info()}
	if bound == nil || consume == nil {
		return RecordRef{}, receipt, fmt.Errorf("load sealed record: consumer is unavailable")
	}
	bound.mu.Lock()
	defer bound.mu.Unlock()
	if err := bound.ready(ctx); err != nil {
		return RecordRef{}, receipt, err
	}
	parsedKind, kindErr := ParseRecordKind(string(kind))
	parsedID, idErr := ParseRecordID(id.String())
	if kindErr != nil || parsedKind != kind || idErr != nil || parsedID != id {
		return RecordRef{}, receipt, fmt.Errorf("load sealed record: identity is invalid")
	}
	if err := limits.Validate(); err != nil {
		return RecordRef{}, receipt, err
	}
	return bound.store.loadRecordSession(ctx, bound.session, kind, id, limits, consume)
}

func (bound *BoundRecordSession) VerifyRecordSet(ctx context.Context, records []RecordRef, limits RecordLimits) (RecordSetVerificationReceipt, error) {
	receipt := RecordSetVerificationReceipt{Effect: recordVerifyEffect, Store: bound.Info()}
	if bound == nil {
		return receipt, fmt.Errorf("verify sealed record set: session is unavailable")
	}
	bound.mu.Lock()
	defer bound.mu.Unlock()
	if err := bound.ready(ctx); err != nil {
		return receipt, err
	}
	if err := validateRecordSet(records, limits); err != nil {
		return receipt, err
	}
	return bound.store.verifyRecordSetSession(ctx, bound.session, records, limits)
}

func (bound *BoundRecordSession) ListRecords(ctx context.Context, kind RecordKind, limits RecordLimits) (RecordListResult, error) {
	result := RecordListResult{Kind: kind, Limits: limits, Records: []RecordRef{}}
	if bound == nil {
		return result, fmt.Errorf("list sealed records: session is unavailable")
	}
	bound.mu.Lock()
	defer bound.mu.Unlock()
	if err := bound.ready(ctx); err != nil {
		if ctx.Err() != nil {
			result.StopReason = "context_cancelled"
		}
		return result, err
	}
	if err := validateRecordList(kind, limits); err != nil {
		return result, err
	}
	return bound.store.listRecordsSession(ctx, bound.session, kind, limits)
}

func (bound *BoundRecordSession) Close() error {
	if bound == nil {
		return nil
	}
	bound.mu.Lock()
	defer bound.mu.Unlock()
	if bound.closed {
		return nil
	}
	bound.closed = true
	if bound.session == nil {
		return nil
	}
	err := bound.session.Close()
	bound.session = nil
	if err != nil {
		return fmt.Errorf("close bound sealed record session failed")
	}
	return nil
}

func (bound *BoundRecordSession) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if bound.closed || bound.store == nil || bound.session == nil {
		return fmt.Errorf("bound sealed record session is closed")
	}
	return nil
}
