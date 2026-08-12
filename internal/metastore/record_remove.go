package metastore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

const recordRemovalEffect = "remove_private_sealed_record"

var (
	stageRecordRemoval = platformSessionStageRecordRemoval
	removeRecordName   = platformSessionRemoveRecordName
	syncRecordRemoval  = platformSessionSyncRecordRemoval
)

// RecordRemovalReceipt describes one exact sealed-record retirement. The
// record is first rebound by its domain-separated digest. A deterministic
// private staging name makes a crash between namespace transitions
// recoverable by the same explicit caller.
type RecordRemovalReceipt struct {
	Effect              string    `json:"effect"`
	Attempted           bool      `json:"attempted"`
	Removed             bool      `json:"removed"`
	AlreadyAbsent       bool      `json:"already_absent"`
	ResidueRecovered    bool      `json:"residue_recovered"`
	WritesPerformed     int       `json:"writes_performed"`
	DurabilityConfirmed bool      `json:"durability_confirmed"`
	Record              RecordRef `json:"record"`
	Store               StoreInfo `json:"store"`
}

// RemoveRecordExact removes one explicitly selected immutable sealed record.
// An already-absent record is only a current observation; callers must possess
// their own durable protocol authority before treating that as recovery.
func (s *Store) RemoveRecordExact(ctx context.Context, record RecordRef, limits RecordLimits) (RecordRemovalReceipt, error) {
	receipt := RecordRemovalReceipt{Effect: recordRemovalEffect, Record: record, Store: s.Info()}
	if s == nil {
		return receipt, fmt.Errorf("remove sealed record: store is unavailable")
	}
	if err := validateRecordRemoval(record, limits); err != nil {
		return receipt, err
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	session, err := s.validatedSession()
	if err != nil {
		return receipt, safeError("remove sealed record", err)
	}
	defer session.Close()
	return s.removeRecordSession(ctx, session, record, limits)
}

func validateRecordRemoval(record RecordRef, limits RecordLimits) error {
	kind, kindErr := ParseRecordKind(string(record.Kind))
	id, idErr := ParseRecordID(record.ID.String())
	if kindErr != nil || kind != record.Kind || idErr != nil || id != record.ID || record.SizeBytes < 0 {
		return fmt.Errorf("remove sealed record: record identity is invalid")
	}
	if err := limits.Validate(); err != nil {
		return err
	}
	if record.SizeBytes > limits.MaxRecordBytes {
		return fmt.Errorf("remove sealed record: record exceeds its byte limit")
	}
	return nil
}

func (s *Store) removeRecordSession(ctx context.Context, session *rootSession, record RecordRef, limits RecordLimits) (RecordRemovalReceipt, error) {
	receipt := RecordRemovalReceipt{Effect: recordRemovalEffect, Record: record, Store: s.Info()}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if err := session.check("before_record_remove"); err != nil {
		return receipt, fmt.Errorf("remove sealed record: bound identity changed")
	}

	object := recordRelativePath(record.Kind, record.ID)
	staging := recordRemovalStagingPath(record)
	objectPresent, err := verifyRemovalRecord(ctx, session, object, record, limits)
	if err != nil {
		return receipt, err
	}
	stagingPresent, err := verifyRemovalRecord(ctx, session, staging, record, limits)
	if err != nil {
		return receipt, err
	}
	receipt.ResidueRecovered = stagingPresent
	hadRecordState := objectPresent || stagingPresent

	if objectPresent && !stagingPresent {
		attempted, staged, stageErr := stageRecordRemoval(session, object, staging)
		if attempted {
			receipt.Attempted = true
			receipt.WritesPerformed++
		}
		if stageErr != nil {
			return receipt, stageErr
		}
		if !staged {
			return receipt, ErrRemovalAmbiguous
		}
		objectPresent, err = verifyRemovalRecord(ctx, session, object, record, limits)
		if err != nil {
			return receipt, err
		}
		stagingPresent, err = verifyRemovalRecord(ctx, session, staging, record, limits)
		if err != nil {
			return receipt, err
		}
		if !stagingPresent {
			return receipt, ErrRemovalAmbiguous
		}
	}

	if objectPresent {
		attempted, removed, removeErr := removeRecordName(session, object)
		if attempted {
			receipt.Attempted = true
			receipt.WritesPerformed++
		}
		if removeErr != nil {
			return receipt, removeErr
		}
		if !removed {
			return receipt, ErrRemovalAmbiguous
		}
		present, verifyErr := verifyRemovalRecord(ctx, session, object, record, limits)
		if verifyErr != nil {
			return receipt, verifyErr
		}
		if present {
			return receipt, ErrRemovalAmbiguous
		}
	}

	if stagingPresent {
		attempted, removed, removeErr := removeRecordName(session, staging)
		if attempted {
			receipt.Attempted = true
			receipt.WritesPerformed++
		}
		if removeErr != nil {
			return receipt, removeErr
		}
		if !removed {
			return receipt, ErrRemovalAmbiguous
		}
		present, verifyErr := verifyRemovalRecord(ctx, session, staging, record, limits)
		if verifyErr != nil {
			return receipt, verifyErr
		}
		if present {
			return receipt, ErrRemovalAmbiguous
		}
	}

	if err := syncRecordRemoval(session); err != nil {
		receipt.Removed = hadRecordState
		receipt.AlreadyAbsent = !hadRecordState
		return receipt, ErrRemovalDurabilityUnconfirmed
	}
	if err := session.check("after_record_remove"); err != nil {
		receipt.Removed = hadRecordState
		receipt.AlreadyAbsent = !hadRecordState
		return receipt, ErrRemovalAmbiguous
	}
	receipt.Removed = hadRecordState
	receipt.AlreadyAbsent = !hadRecordState
	receipt.DurabilityConfirmed = true
	return receipt, nil
}

func verifyRemovalRecord(ctx context.Context, session *rootSession, relative string, record RecordRef, limits RecordLimits) (bool, error) {
	size, err := verifyRecordRelative(ctx, session, relative, record.Kind, record.ID, limits.MaxRecordBytes)
	if errors.Is(err, ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, err
		}
		return false, fmt.Errorf("%w: exact removal record verification failed", ErrCorruptRecord)
	}
	if size != record.SizeBytes {
		return false, fmt.Errorf("%w: exact removal record size disagrees", ErrCorruptRecord)
	}
	return true, nil
}

func recordRemovalStagingPath(record RecordRef) string {
	digest := strings.TrimPrefix(record.ID.String(), "sha256:")
	return filepath.Join(temporaryDir, ".record-remove-"+string(record.Kind)+"-"+digest+".pending")
}
