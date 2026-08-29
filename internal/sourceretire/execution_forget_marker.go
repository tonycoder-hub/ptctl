package sourceretire

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

func ensureExecutionForgetRootIntent(ctx context.Context, target *fsbind.Session, subtree *fsbind.Subtree, marker ExecutionForgetIntent, maximumBytes int64) (ExecutionForgetMarkerID, executionForgetMarkerReceipt, fsbind.ObjectInfo, error) {
	receipt := executionForgetMarkerReceipt{}
	if err := ctx.Err(); err != nil {
		return "", receipt, fsbind.ObjectInfo{}, err
	}
	raw, id, err := EncodeExecutionForgetIntent(marker)
	if err != nil {
		return "", receipt, fsbind.ObjectInfo{}, err
	}
	if maximumBytes <= 0 || maximumBytes > maximumExecutionForgetBytes || int64(len(raw)) > maximumBytes {
		return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: source retirement forget marker byte budget is insufficient", ErrExecutionPolicy)
	}
	rootName, err := ExecutionForgetRootName(marker.OperationID)
	if err != nil {
		return "", receipt, fsbind.ObjectInfo{}, err
	}
	pendingName := executionForgetPendingName(id)
	pendingPath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory, pendingName})
	existing, existingID, object, existingRaw, readErr := readExecutionForgetRootIntent(ctx, target, rootName, maximumBytes)
	if readErr == nil {
		if existingID != id || existing != marker || !bytes.Equal(existingRaw, raw) {
			return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: source retirement forget root intent disagrees", ErrExecutionIntegrity)
		}
		if err := target.SyncRoot(ctx); err != nil {
			return id, receipt, object, err
		}
		if subtree != nil {
			pending, pendingErr := subtree.Inspect(ctx, pendingPath)
			if pendingErr == nil {
				if pending.Kind != fsbind.ObjectKindRegular || pending.SizeBytes != int64(len(raw)) {
					return id, receipt, object, fmt.Errorf("%w: source retirement forget pending marker is unsafe", ErrExecutionIntegrity)
				}
				if identity, verifyErr := verifyExecutionRetentionNamedBytes(ctx, subtree, pendingPath, raw); verifyErr != nil || !identity.Equal(pending.Identity) {
					if verifyErr != nil {
						return id, receipt, object, verifyErr
					}
					return id, receipt, object, fmt.Errorf("%w: source retirement forget pending marker identity changed", ErrExecutionIntegrity)
				}
				removal, removeErr := subtree.RemoveRegularExact(ctx, pendingPath, pending.Identity, pending.SizeBytes)
				receipt.TemporaryRemoval = removal
				if removeErr != nil {
					return id, receipt, object, removeErr
				}
			} else if !errors.Is(pendingErr, fsbind.ErrNotFound) {
				return id, receipt, object, pendingErr
			}
		}
		receipt.AlreadyPresent = true
		return id, receipt, object, nil
	}
	if !errors.Is(readErr, fsbind.ErrNotFound) {
		return "", receipt, fsbind.ObjectInfo{}, readErr
	}
	if subtree == nil {
		return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: source retirement tombstone is unavailable before forget intent publication", ErrExecutionIntegrity)
	}
	pending, inspectErr := subtree.Inspect(ctx, pendingPath)
	if errors.Is(inspectErr, fsbind.ErrNotFound) {
		if err := ctx.Err(); err != nil {
			return "", receipt, fsbind.ObjectInfo{}, err
		}
		file, createErr := subtree.CreateRegular(ctx, pendingPath)
		if createErr != nil {
			return "", receipt, fsbind.ObjectInfo{}, createErr
		}
		receipt.TemporaryCreated = true
		written, writeErr := writeExecutionBytes(ctx, file, raw)
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
			return "", receipt, fsbind.ObjectInfo{}, classifyExecutionBindingError(infoErr)
		}
		if closeErr != nil {
			return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("close source retirement forget pending marker failed")
		}
		if info.SizeBytes != int64(len(raw)) || info.Identity.IsZero() {
			return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: source retirement forget pending marker changed", ErrExecutionIntegrity)
		}
		pending = fsbind.ObjectInfo{Identity: info.Identity, Kind: fsbind.ObjectKindRegular, SizeBytes: info.SizeBytes}
	} else if inspectErr != nil {
		return "", receipt, fsbind.ObjectInfo{}, inspectErr
	} else if pending.Kind != fsbind.ObjectKindRegular || pending.SizeBytes != int64(len(raw)) {
		return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: source retirement forget pending marker is unsafe", ErrExecutionIntegrity)
	}
	if identity, verifyErr := verifyExecutionRetentionNamedBytes(ctx, subtree, pendingPath, raw); verifyErr != nil || !identity.Equal(pending.Identity) {
		if verifyErr != nil {
			return "", receipt, fsbind.ObjectInfo{}, verifyErr
		}
		return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: source retirement forget pending marker identity changed", ErrExecutionIntegrity)
	}
	if err := ctx.Err(); err != nil {
		return "", receipt, fsbind.ObjectInfo{}, err
	}
	publication, publishErr := target.PublishNoReplace(ctx, subtree, pendingPath, rootName)
	receipt.Publication = publication
	if publishErr != nil {
		return id, receipt, fsbind.ObjectInfo{}, publishErr
	}
	if !publication.Published || publication.Durability != fsbind.DurabilityConfirmed ||
		!publication.SourceIdentity.Equal(pending.Identity) || !publication.FinalIdentity.Equal(pending.Identity) {
		return id, receipt, fsbind.ObjectInfo{}, fsbind.ErrPublicationAmbiguous
	}
	fresh, freshID, object, freshRaw, freshErr := readExecutionForgetRootIntent(ctx, target, rootName, maximumBytes)
	if freshErr != nil || freshID != id || fresh != marker || !bytes.Equal(freshRaw, raw) || !object.Identity.Equal(pending.Identity) {
		if freshErr != nil {
			return id, receipt, object, freshErr
		}
		return id, receipt, object, fmt.Errorf("%w: published source retirement forget intent changed", ErrExecutionIntegrity)
	}
	if err := target.SyncRoot(ctx); err != nil {
		return id, receipt, object, err
	}
	return id, receipt, object, nil
}

func readExecutionForgetRootIntent(ctx context.Context, target *fsbind.Session, name string, limit int64) (ExecutionForgetIntent, ExecutionForgetMarkerID, fsbind.ObjectInfo, []byte, error) {
	if target == nil || limit <= 0 || limit > maximumExecutionForgetBytes {
		return ExecutionForgetIntent{}, "", fsbind.ObjectInfo{}, nil, fmt.Errorf("%w: source retirement forget marker read is invalid", ErrExecutionPolicy)
	}
	readOnce := func() ([]byte, fsbind.ObjectInfo, error) {
		file, err := target.OpenRootPrivateRegular(ctx, name)
		if err != nil {
			return nil, fsbind.ObjectInfo{}, err
		}
		before, beforeErr := file.Info()
		if beforeErr != nil {
			_ = file.Close()
			return nil, fsbind.ObjectInfo{}, classifyExecutionBindingError(beforeErr)
		}
		if before.SizeBytes < 0 || before.SizeBytes > maximumExecutionForgetBytes {
			_ = file.Close()
			return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: source retirement forget marker size is invalid", ErrExecutionIntegrity)
		}
		if before.SizeBytes > limit {
			_ = file.Close()
			return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: source retirement forget marker byte budget was reached", ErrExecutionPolicy)
		}
		raw, readErr := io.ReadAll(&executionForgetContextReader{ctx: ctx, reader: io.LimitReader(file, limit+1)})
		after, afterErr := file.Info()
		closeErr := file.Close()
		if readErr != nil {
			return nil, fsbind.ObjectInfo{}, readErr
		}
		if afterErr != nil {
			return nil, fsbind.ObjectInfo{}, classifyExecutionBindingError(afterErr)
		}
		if closeErr != nil {
			return nil, fsbind.ObjectInfo{}, fmt.Errorf("close source retirement forget marker failed")
		}
		if int64(len(raw)) != before.SizeBytes || len(raw) > int(limit) ||
			!before.Identity.Equal(after.Identity) || before.SizeBytes != after.SizeBytes || !before.Modified.Equal(after.Modified) {
			return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: source retirement forget marker changed while read", ErrExecutionIntegrity)
		}
		return raw, fsbind.ObjectInfo{Identity: before.Identity, Kind: fsbind.ObjectKindRegular, SizeBytes: before.SizeBytes}, nil
	}
	raw, object, err := readOnce()
	if err != nil {
		return ExecutionForgetIntent{}, "", fsbind.ObjectInfo{}, nil, err
	}
	marker, id, err := DecodeExecutionForgetIntent(bytes.NewReader(raw))
	if err != nil {
		return ExecutionForgetIntent{}, "", fsbind.ObjectInfo{}, nil, err
	}
	freshRaw, freshObject, err := readOnce()
	if err != nil {
		return ExecutionForgetIntent{}, "", fsbind.ObjectInfo{}, nil, err
	}
	if !object.Identity.Equal(freshObject.Identity) || object.SizeBytes != freshObject.SizeBytes || !bytes.Equal(raw, freshRaw) {
		return ExecutionForgetIntent{}, "", fsbind.ObjectInfo{}, nil, fmt.Errorf("%w: source retirement forget marker name changed", ErrExecutionIntegrity)
	}
	return marker, id, object, raw, nil
}

type executionForgetContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *executionForgetContextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
