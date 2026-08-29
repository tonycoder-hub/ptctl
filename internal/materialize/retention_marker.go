package materialize

import (
	"context"
	"errors"
	"fmt"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

type retentionMarkerReceipt struct {
	DirectoryCreated      bool
	DirectoryDurability   string
	TemporaryCreated      bool
	TemporaryBytesWritten int64
	AlreadyPresent        bool
	Publication           fsbind.Publication
	TemporaryRemoval      fsbind.Removal
}

func ensureRetentionIntentMarker(ctx context.Context, subtree *fsbind.Subtree, marker RetentionIntent) (RetentionMarkerID, retentionMarkerReceipt, error) {
	raw, id, err := EncodeRetentionIntent(marker)
	if err != nil {
		return "", retentionMarkerReceipt{}, err
	}
	return ensureRetentionMarkerBytes(ctx, subtree, retentionIntentFileName, "intent", raw, id)
}

func ensureRetentionCompleteMarker(ctx context.Context, subtree *fsbind.Subtree, marker RetentionComplete) (RetentionMarkerID, retentionMarkerReceipt, error) {
	raw, id, err := EncodeRetentionComplete(marker)
	if err != nil {
		return "", retentionMarkerReceipt{}, err
	}
	return ensureRetentionMarkerBytes(ctx, subtree, retentionCompleteName, "complete", raw, id)
}

func ensureRetentionMarkerBytes(ctx context.Context, subtree *fsbind.Subtree, destinationName, purpose string, raw []byte, id RetentionMarkerID) (RetentionMarkerID, retentionMarkerReceipt, error) {
	receipt := retentionMarkerReceipt{}
	if subtree == nil || len(raw) == 0 || int64(len(raw)) > maxRetentionMarkerBytes {
		return "", receipt, fmt.Errorf("%w: retention marker input is unavailable", ErrPolicy)
	}
	if _, err := ParseRetentionMarkerID(id.String()); err != nil {
		return "", receipt, err
	}
	retentionPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	directory, inspectErr := subtree.Inspect(ctx, retentionPath)
	switch {
	case errors.Is(inspectErr, fsbind.ErrNotFound):
		mkdir, err := subtree.MkdirAll(ctx, retentionPath)
		receipt.DirectoryCreated = mkdir.DirectoriesCreated != 0
		receipt.DirectoryDurability = mkdir.Durability
		if err != nil {
			return "", receipt, err
		}
	case inspectErr != nil:
		return "", receipt, inspectErr
	case directory.Kind != fsbind.ObjectKindDirectory:
		return "", receipt, fmt.Errorf("%w: retention control directory is unsafe", ErrIntegrity)
	}
	destination, err := fsbind.PathFromComponents([]string{retentionDirectoryName, destinationName})
	if err != nil {
		return "", receipt, err
	}
	temporaryName := retentionTemporaryName(purpose, id)
	temporary, err := fsbind.PathFromComponents([]string{retentionDirectoryName, temporaryName})
	if err != nil {
		return "", receipt, err
	}
	if identity, err := verifyRetentionNamedBytes(ctx, subtree, destination, raw); err == nil {
		receipt.AlreadyPresent = true
		if tempInfo, tempErr := subtree.Inspect(ctx, temporary); tempErr == nil {
			if tempInfo.Kind != fsbind.ObjectKindRegular {
				return id, receipt, fmt.Errorf("%w: retention marker temporary object is unsafe", ErrIntegrity)
			}
			if _, verifyErr := verifyRetentionNamedBytes(ctx, subtree, temporary, raw); verifyErr != nil {
				return id, receipt, verifyErr
			}
			removal, removeErr := subtree.RemoveRegularExact(ctx, temporary, tempInfo.Identity, int64(len(raw)))
			receipt.TemporaryRemoval = removal
			if removeErr != nil {
				return id, receipt, removeErr
			}
		} else if !errors.Is(tempErr, fsbind.ErrNotFound) {
			return id, receipt, tempErr
		}
		if identity.IsZero() {
			return id, receipt, fmt.Errorf("%w: retention marker identity is unavailable", ErrIntegrity)
		}
		return id, receipt, nil
	} else if !errors.Is(err, fsbind.ErrNotFound) {
		return "", receipt, err
	}

	tempInfo, tempErr := subtree.Inspect(ctx, temporary)
	if errors.Is(tempErr, fsbind.ErrNotFound) {
		file, err := subtree.CreateRegular(ctx, temporary)
		if err != nil {
			return "", receipt, err
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
			return "", receipt, writeErr
		}
		if infoErr != nil {
			return "", receipt, infoErr
		}
		if closeErr != nil {
			return "", receipt, closeErr
		}
		if info.SizeBytes != int64(len(raw)) || info.Identity.IsZero() {
			return "", receipt, fmt.Errorf("%w: retention marker temporary object changed", ErrIntegrity)
		}
		tempInfo = fsbind.ObjectInfo{Identity: info.Identity, Kind: fsbind.ObjectKindRegular, SizeBytes: info.SizeBytes}
	} else if tempErr != nil {
		return "", receipt, tempErr
	} else if tempInfo.Kind != fsbind.ObjectKindRegular {
		return "", receipt, fmt.Errorf("%w: retention marker temporary object is unsafe", ErrIntegrity)
	}
	if _, err := verifyRetentionNamedBytes(ctx, subtree, temporary, raw); err != nil {
		return "", receipt, err
	}
	publication, publishErr := runPublication("retention", func() (fsbind.Publication, error) {
		return subtree.CommitRegularNoReplace(ctx, temporary, destination)
	})
	receipt.Publication = publication
	if publishErr != nil {
		return id, receipt, publishErr
	}
	if !publication.Published || publication.Durability != fsbind.DurabilityConfirmed ||
		!publication.SourceIdentity.Equal(tempInfo.Identity) || !publication.FinalIdentity.Equal(tempInfo.Identity) {
		return id, receipt, fsbind.ErrPublicationAmbiguous
	}
	if _, err := verifyRetentionNamedBytes(ctx, subtree, destination, raw); err != nil {
		return id, receipt, err
	}
	if err := subtree.CheckPaths(retentionPath); err != nil {
		return id, receipt, err
	}
	return id, receipt, nil
}

func verifyRetentionNamedBytes(ctx context.Context, subtree *fsbind.Subtree, path fsbind.Path, expected []byte) (fsbind.Identity, error) {
	object, err := subtree.Inspect(ctx, path)
	if err != nil {
		return fsbind.Identity{}, err
	}
	if object.Kind != fsbind.ObjectKindRegular || object.SizeBytes != int64(len(expected)) || object.Identity.IsZero() {
		return fsbind.Identity{}, fmt.Errorf("%w: retention marker object is unsafe", ErrIntegrity)
	}
	if err := verifySubtreeNamedBytes(ctx, subtree, path, expected, object.Identity); err != nil {
		if errors.Is(err, ErrCorruptJournal) {
			return fsbind.Identity{}, fmt.Errorf("%w: retention marker bytes disagree", ErrIntegrity)
		}
		return fsbind.Identity{}, err
	}
	return object.Identity, nil
}
