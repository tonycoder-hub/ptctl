package materialize

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

func ensureForgetRootIntent(ctx context.Context, target *fsbind.Session, subtree *fsbind.Subtree, marker ForgetIntent, maximumBytes int64) (ForgetMarkerID, forgetMarkerReceipt, fsbind.ObjectInfo, error) {
	receipt := forgetMarkerReceipt{}
	if err := ctx.Err(); err != nil {
		return "", receipt, fsbind.ObjectInfo{}, err
	}
	raw, id, err := EncodeForgetIntent(marker)
	if err != nil {
		return "", receipt, fsbind.ObjectInfo{}, err
	}
	if maximumBytes <= 0 || maximumBytes > maxForgetMarkerBytes || int64(len(raw)) > maximumBytes {
		return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: materialize forget marker byte budget is insufficient", ErrPolicy)
	}
	rootName, err := ForgetRootName(marker.OperationID)
	if err != nil {
		return "", receipt, fsbind.ObjectInfo{}, err
	}
	pendingName := forgetPendingName(id)
	pendingPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, pendingName})
	existing, existingID, object, existingRaw, readErr := readForgetRootIntent(ctx, target, rootName, maximumBytes)
	if readErr == nil {
		if existingID != id || existing != marker || !bytes.Equal(existingRaw, raw) {
			return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: materialize forget root intent disagrees", ErrIntegrity)
		}
		if err := target.SyncRoot(ctx); err != nil {
			return id, receipt, object, err
		}
		if subtree != nil {
			pending, pendingErr := subtree.Inspect(ctx, pendingPath)
			if pendingErr == nil {
				if pending.Kind != fsbind.ObjectKindRegular || pending.SizeBytes != int64(len(raw)) {
					return id, receipt, object, fmt.Errorf("%w: materialize forget pending marker is unsafe", ErrIntegrity)
				}
				if identity, verifyErr := verifyRetentionNamedBytes(ctx, subtree, pendingPath, raw); verifyErr != nil || !identity.Equal(pending.Identity) {
					if verifyErr != nil {
						return id, receipt, object, verifyErr
					}
					return id, receipt, object, fmt.Errorf("%w: materialize forget pending marker identity changed", ErrIntegrity)
				}
				removal, removeErr := subtree.RemoveRegularExact(ctx, pendingPath, pending.Identity, pending.SizeBytes)
				receipt.TemporaryRemoval = removal
				if removeErr != nil {
					return id, receipt, object, removeErr
				}
			} else if !errors.Is(pendingErr, fsbind.ErrNotFound) {
				return id, receipt, object, classifyRetentionAuthorityError(pendingErr)
			}
		}
		receipt.AlreadyPresent = true
		return id, receipt, object, nil
	}
	if !errors.Is(readErr, fsbind.ErrNotFound) {
		return "", receipt, fsbind.ObjectInfo{}, readErr
	}
	if subtree == nil {
		return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: materialize retention tombstone is unavailable before forget intent publication", ErrIntegrity)
	}
	pending, inspectErr := subtree.Inspect(ctx, pendingPath)
	if errors.Is(inspectErr, fsbind.ErrNotFound) {
		if err := ctx.Err(); err != nil {
			return "", receipt, fsbind.ObjectInfo{}, err
		}
		file, createErr := subtree.CreateRegular(ctx, pendingPath)
		if createErr != nil {
			return "", receipt, fsbind.ObjectInfo{}, classifyRetentionAuthorityError(createErr)
		}
		receipt.TemporaryCreated = true
		written, writeErr := writeFullContext(ctx, file, raw)
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
			return "", receipt, fsbind.ObjectInfo{}, classifyRetentionAuthorityError(infoErr)
		}
		if closeErr != nil {
			return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("close materialize forget pending marker failed")
		}
		if info.SizeBytes != int64(len(raw)) || info.Identity.IsZero() {
			return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: materialize forget pending marker changed", ErrIntegrity)
		}
		pending = fsbind.ObjectInfo{Identity: info.Identity, Kind: fsbind.ObjectKindRegular, SizeBytes: info.SizeBytes}
	} else if inspectErr != nil {
		return "", receipt, fsbind.ObjectInfo{}, classifyRetentionAuthorityError(inspectErr)
	} else if pending.Kind != fsbind.ObjectKindRegular || pending.SizeBytes != int64(len(raw)) {
		return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: materialize forget pending marker is unsafe", ErrIntegrity)
	}
	if identity, verifyErr := verifyRetentionNamedBytes(ctx, subtree, pendingPath, raw); verifyErr != nil || !identity.Equal(pending.Identity) {
		if verifyErr != nil {
			return "", receipt, fsbind.ObjectInfo{}, verifyErr
		}
		return "", receipt, fsbind.ObjectInfo{}, fmt.Errorf("%w: materialize forget pending marker identity changed", ErrIntegrity)
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
	fresh, freshID, object, freshRaw, freshErr := readForgetRootIntent(ctx, target, rootName, maximumBytes)
	if freshErr != nil || freshID != id || fresh != marker || !bytes.Equal(freshRaw, raw) || !object.Identity.Equal(pending.Identity) {
		if freshErr != nil {
			return id, receipt, object, classifyRetentionAuthorityError(freshErr)
		}
		return id, receipt, object, fmt.Errorf("%w: published materialize forget intent changed", ErrIntegrity)
	}
	if err := target.SyncRoot(ctx); err != nil {
		return id, receipt, object, err
	}
	return id, receipt, object, nil
}

func readForgetRootIntent(ctx context.Context, target *fsbind.Session, name string, limit int64) (ForgetIntent, ForgetMarkerID, fsbind.ObjectInfo, []byte, error) {
	if target == nil || limit <= 0 || limit > maxForgetMarkerBytes {
		return ForgetIntent{}, "", fsbind.ObjectInfo{}, nil, fmt.Errorf("%w: materialize forget marker read is invalid", ErrPolicy)
	}
	readOnce := func() ([]byte, fsbind.ObjectInfo, error) {
		file, err := target.OpenRootPrivateRegular(ctx, name)
		if err != nil {
			return nil, fsbind.ObjectInfo{}, err
		}
		before, beforeErr := file.Info()
		if beforeErr != nil {
			_ = file.Close()
			return nil, fsbind.ObjectInfo{}, classifyRetentionAuthorityError(beforeErr)
		}
		if before.SizeBytes < 0 || before.SizeBytes > maxForgetMarkerBytes {
			_ = file.Close()
			return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: materialize forget marker size is invalid", ErrIntegrity)
		}
		if before.SizeBytes > limit {
			_ = file.Close()
			return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: materialize forget marker byte budget was reached", ErrPolicy)
		}
		raw, readErr := io.ReadAll(&contextExactReader{ctx: ctx, reader: io.LimitReader(file, limit+1), remaining: limit + 1})
		after, afterErr := file.Info()
		closeErr := file.Close()
		if readErr != nil {
			return nil, fsbind.ObjectInfo{}, readErr
		}
		if afterErr != nil {
			return nil, fsbind.ObjectInfo{}, classifyRetentionAuthorityError(afterErr)
		}
		if closeErr != nil {
			return nil, fsbind.ObjectInfo{}, fmt.Errorf("close materialize forget marker failed")
		}
		if int64(len(raw)) != before.SizeBytes || len(raw) > int(limit) ||
			!before.Identity.Equal(after.Identity) || before.SizeBytes != after.SizeBytes || !before.Modified.Equal(after.Modified) {
			return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: materialize forget marker changed while read", ErrIntegrity)
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
		return ForgetIntent{}, "", fsbind.ObjectInfo{}, nil, fmt.Errorf("%w: materialize forget marker name changed", ErrIntegrity)
	}
	return marker, id, object, raw, nil
}
