package clientactivate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

func ensureForgetRootIntent(ctx context.Context, target *fsbind.Session, handle *journalHandle, marker ForgetIntent, maximumBytes int64) (ForgetMarkerID, forgetMarkerReceipt, fsbind.ObjectInfo, error) {
	receipt := forgetMarkerReceipt{}
	if err := ctx.Err(); err != nil {
		return "", receipt, fsbind.ObjectInfo{}, err
	}
	raw, id, err := EncodeForgetIntent(marker)
	if err != nil {
		return "", receipt, fsbind.ObjectInfo{}, err
	}
	if maximumBytes <= 0 || maximumBytes > maximumForgetMarkerBytes || int64(len(raw)) > maximumBytes {
		return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: client activation forget marker byte budget is insufficient", ErrPolicy)
	}
	rootName, err := ForgetRootName(marker.OperationID)
	if err != nil {
		return "", receipt, fsbind.ObjectInfo{}, err
	}
	pendingName := forgetPendingName(id)
	pendingPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, pendingName})
	existing, existingID, object, existingRaw, readErr := readForgetRootIntent(ctx, target, rootName, maximumBytes)
	if readErr == nil {
		if existingID != id || !sameForgetIntent(existing, marker) || !bytes.Equal(existingRaw, raw) {
			return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: client activation forget root intent disagrees", ErrIntegrity)
		}
		if err := target.SyncRoot(ctx); err != nil {
			return id, receipt, object, err
		}
		if handle != nil && handle.subtree != nil {
			pending, pendingErr := handle.subtree.Inspect(ctx, pendingPath)
			if pendingErr == nil {
				if pending.Kind != fsbind.ObjectKindRegular || pending.SizeBytes != int64(len(raw)) {
					return id, receipt, object, fmt.Errorf("%w: client activation forget pending marker is unsafe", ErrIntegrity)
				}
				if identity, verifyErr := handle.verifyNamedBytes(ctx, []string{retentionDirectoryName, pendingName}, raw); verifyErr != nil || !identity.Equal(pending.Identity) {
					if verifyErr != nil {
						return id, receipt, object, verifyErr
					}
					return id, receipt, object, fmt.Errorf("%w: client activation forget pending marker identity changed", ErrIntegrity)
				}
				removal, removeErr := handle.subtree.RemoveRegularExact(ctx, pendingPath, pending.Identity, pending.SizeBytes)
				receipt.TemporaryRemoval = removal
				if removeErr != nil {
					return id, receipt, object, removeErr
				}
			} else if !errors.Is(pendingErr, fsbind.ErrNotFound) {
				return id, receipt, object, classifyJournalError(pendingErr)
			}
		}
		receipt.AlreadyPresent = true
		return id, receipt, object, nil
	}
	if !errors.Is(readErr, fsbind.ErrNotFound) {
		return "", receipt, fsbind.ObjectInfo{}, readErr
	}
	if handle == nil || handle.subtree == nil {
		return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: client activation retention tombstone is unavailable before forget intent publication", ErrIntegrity)
	}
	pending, inspectErr := handle.subtree.Inspect(ctx, pendingPath)
	if errors.Is(inspectErr, fsbind.ErrNotFound) {
		if err := ctx.Err(); err != nil {
			return "", receipt, fsbind.ObjectInfo{}, err
		}
		file, createErr := handle.subtree.CreateRegular(ctx, pendingPath)
		if createErr != nil {
			return "", receipt, fsbind.ObjectInfo{}, classifyJournalError(createErr)
		}
		receipt.TemporaryCreated = true
		written, writeErr := writeAll(ctx, file, raw)
		receipt.TemporaryBytesWritten = written
		if writeErr == nil {
			writeErr = file.Sync()
		}
		info, infoErr := file.Info()
		closeErr := file.Close()
		if writeErr != nil {
			return "", receipt, fsbind.ObjectInfo{}, writeErr
		}
		if infoErr != nil {
			return "", receipt, fsbind.ObjectInfo{}, classifyJournalError(infoErr)
		}
		if closeErr != nil {
			return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("close client activation forget pending marker failed")
		}
		if info.SizeBytes != int64(len(raw)) || info.Identity.IsZero() {
			return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: client activation forget pending marker changed", ErrIntegrity)
		}
		pending = fsbind.ObjectInfo{Identity: info.Identity, Kind: fsbind.ObjectKindRegular, SizeBytes: info.SizeBytes}
	} else if inspectErr != nil {
		return "", receipt, fsbind.ObjectInfo{}, classifyJournalError(inspectErr)
	} else if pending.Kind != fsbind.ObjectKindRegular || pending.SizeBytes != int64(len(raw)) {
		return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: client activation forget pending marker is unsafe", ErrIntegrity)
	}
	if identity, verifyErr := handle.verifyNamedBytes(ctx, []string{retentionDirectoryName, pendingName}, raw); verifyErr != nil || !identity.Equal(pending.Identity) {
		if verifyErr != nil {
			return "", receipt, fsbind.ObjectInfo{}, verifyErr
		}
		return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: client activation forget pending marker identity changed", ErrIntegrity)
	}
	if forgetTransitionHook != nil {
		if hookErr := forgetTransitionHook("pending_staged"); hookErr != nil {
			return id, receipt, fsbind.ObjectInfo{}, hookErr
		}
	}
	if err := ctx.Err(); err != nil {
		return "", receipt, fsbind.ObjectInfo{}, err
	}
	publication, publishErr := target.PublishNoReplace(ctx, handle.subtree, pendingPath, rootName)
	receipt.Publication = publication
	if publishErr != nil {
		return id, receipt, fsbind.ObjectInfo{}, publishErr
	}
	if !publication.Published || publication.Durability != fsbind.DurabilityConfirmed ||
		!publication.SourceIdentity.Equal(pending.Identity) || !publication.FinalIdentity.Equal(pending.Identity) {
		return id, receipt, fsbind.ObjectInfo{}, fsbind.ErrPublicationAmbiguous
	}
	fresh, freshID, object, freshRaw, freshErr := readForgetRootIntent(ctx, target, rootName, maximumBytes)
	if freshErr != nil || freshID != id || !sameForgetIntent(fresh, marker) || !bytes.Equal(freshRaw, raw) || !object.Identity.Equal(pending.Identity) {
		if freshErr != nil {
			return id, receipt, object, classifyJournalError(freshErr)
		}
		return id, receipt, object, fmt.Errorf("%w: published client activation forget intent changed", ErrIntegrity)
	}
	if err := target.SyncRoot(ctx); err != nil {
		return id, receipt, object, err
	}
	return id, receipt, object, nil
}

func readForgetRootIntent(ctx context.Context, target *fsbind.Session, name string, limit int64) (ForgetIntent, ForgetMarkerID, fsbind.ObjectInfo, []byte, error) {
	if target == nil || limit <= 0 || limit > maximumForgetMarkerBytes {
		return ForgetIntent{}, "", fsbind.ObjectInfo{}, nil, fmt.Errorf("%w: client activation forget marker read is invalid", ErrPolicy)
	}
	readOnce := func() ([]byte, fsbind.ObjectInfo, error) {
		file, err := target.OpenRootPrivateRegular(ctx, name)
		if err != nil {
			return nil, fsbind.ObjectInfo{}, err
		}
		before, beforeErr := file.Info()
		if beforeErr != nil {
			_ = file.Close()
			return nil, fsbind.ObjectInfo{}, classifyJournalError(beforeErr)
		}
		if before.SizeBytes < 0 || before.SizeBytes > maximumForgetMarkerBytes {
			_ = file.Close()
			return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: client activation forget marker size is invalid", ErrIntegrity)
		}
		if before.SizeBytes > limit {
			_ = file.Close()
			return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: client activation forget marker byte budget was reached", ErrPolicy)
		}
		raw, readErr := readAllContext(ctx, io.LimitReader(file, limit+1), limit+1)
		after, afterErr := file.Info()
		closeErr := file.Close()
		if readErr != nil {
			return nil, fsbind.ObjectInfo{}, readErr
		}
		if afterErr != nil {
			return nil, fsbind.ObjectInfo{}, classifyJournalError(afterErr)
		}
		if closeErr != nil {
			return nil, fsbind.ObjectInfo{}, fmt.Errorf("close client activation forget marker failed")
		}
		if int64(len(raw)) != before.SizeBytes || int64(len(raw)) > limit || !before.Identity.Equal(after.Identity) ||
			before.SizeBytes != after.SizeBytes || !before.Modified.Equal(after.Modified) {
			return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: client activation forget marker changed while read", ErrIntegrity)
		}
		return raw, fsbind.ObjectInfo{Identity: before.Identity, Kind: fsbind.ObjectKindRegular, SizeBytes: before.SizeBytes}, nil
	}
	raw, object, err := readOnce()
	if err != nil {
		return ForgetIntent{}, "", fsbind.ObjectInfo{}, nil, err
	}
	marker, id, err := DecodeForgetIntent(bytes.NewReader(raw))
	if err != nil {
		return ForgetIntent{}, "", fsbind.ObjectInfo{}, nil, err
	}
	freshRaw, freshObject, err := readOnce()
	if err != nil {
		return ForgetIntent{}, "", fsbind.ObjectInfo{}, nil, err
	}
	if !object.Identity.Equal(freshObject.Identity) || object.SizeBytes != freshObject.SizeBytes || !bytes.Equal(raw, freshRaw) {
		return ForgetIntent{}, "", fsbind.ObjectInfo{}, nil, fmt.Errorf("%w: client activation forget marker name changed", ErrIntegrity)
	}
	return marker, id, object, raw, nil
}

func readAllContext(ctx context.Context, reader io.Reader, limit int64) ([]byte, error) {
	buffer := bytes.NewBuffer(make([]byte, 0, minInt64(limit, 32<<10)))
	temporary := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, err := reader.Read(temporary)
		if count > 0 {
			_, _ = buffer.Write(temporary[:count])
			if int64(buffer.Len()) > limit {
				return nil, fmt.Errorf("%w: client activation forget marker byte budget was reached", ErrPolicy)
			}
		}
		if err == io.EOF {
			return buffer.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
		if count == 0 {
			return nil, io.ErrNoProgress
		}
	}
}

func minInt64(left, right int64) int {
	if left < right {
		return int(left)
	}
	return int(right)
}
