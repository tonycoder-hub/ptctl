package sourceretire

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

type executionRetentionState struct {
	DirectoryPresent bool
	IntentPresent    bool
	Intent           ExecutionRetentionIntent
	IntentID         ExecutionRetentionMarkerID
	CompletePresent  bool
	Complete         ExecutionRetentionComplete
	CompleteID       ExecutionRetentionMarkerID
	Entries          []fsbind.Entry
}

func loadExecutionRetentionState(ctx context.Context, subtree *fsbind.Subtree, operation OperationID, targetIdentity fsbind.Identity) (executionRetentionState, error) {
	state := executionRetentionState{Entries: []fsbind.Entry{}}
	if subtree == nil || targetIdentity.IsZero() {
		return state, fmt.Errorf("%w: source retirement retention authority is unavailable", ErrExecutionIntegrity)
	}
	directoryPath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory})
	directory, err := subtree.Inspect(ctx, directoryPath)
	if errors.Is(err, fsbind.ErrNotFound) {
		return state, nil
	}
	if err != nil {
		return state, classifyExecutionBindingError(err)
	}
	state.DirectoryPresent = true
	if directory.Kind != fsbind.ObjectKindDirectory || directory.Identity.IsZero() {
		return state, fmt.Errorf("%w: source retirement retention directory is unsafe", ErrExecutionIntegrity)
	}
	listing, err := subtree.List(ctx, directoryPath, fsbind.ListLimits{MaxEntries: 5, MaxNameBytes: 1 << 20})
	if err != nil {
		return state, classifyExecutionBindingError(err)
	}
	if !listing.Complete {
		return state, fmt.Errorf("%w: source retirement retention inventory is incomplete", ErrExecutionIntegrity)
	}
	state.Entries = append(state.Entries, listing.Entries...)
	for _, entry := range listing.Entries {
		if entry.Kind != string(fsbind.ObjectKindRegular) {
			return state, fmt.Errorf("%w: source retirement retention control object is unsafe", ErrExecutionIntegrity)
		}
	}
	intentPath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory, executionRetentionIntentFile})
	if executionRetentionEntryPresent(listing.Entries, executionRetentionIntentFile) {
		raw, _, readErr := readExecutionRetentionNamedBytes(ctx, subtree, intentPath, maximumExecutionRetentionMarker)
		if readErr != nil {
			return state, readErr
		}
		intent, id, decodeErr := DecodeExecutionRetentionIntent(bytes.NewReader(raw))
		if decodeErr != nil || intent.OperationID != operation || intent.OperationRootIdentity != subtree.Identity().String() ||
			intent.TargetRootIdentity != targetIdentity.String() {
			return state, fmt.Errorf("%w: source retirement retention intent is bound to another operation", ErrExecutionIntegrity)
		}
		state.IntentPresent, state.Intent, state.IntentID = true, intent, id
	}
	completePath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory, executionRetentionCompleteFile})
	if executionRetentionEntryPresent(listing.Entries, executionRetentionCompleteFile) {
		if !state.IntentPresent {
			return state, fmt.Errorf("%w: source retirement retention completion exists without an intent", ErrExecutionIntegrity)
		}
		raw, _, readErr := readExecutionRetentionNamedBytes(ctx, subtree, completePath, maximumExecutionRetentionMarker)
		if readErr != nil {
			return state, readErr
		}
		complete, id, decodeErr := DecodeExecutionRetentionComplete(bytes.NewReader(raw))
		if decodeErr != nil || complete.OperationID != operation || complete.OperationRootIdentity != subtree.Identity().String() ||
			complete.TargetRootIdentity != targetIdentity.String() || complete.IntentMarkerID != state.IntentID {
			return state, fmt.Errorf("%w: source retirement retention completion disagrees with its intent", ErrExecutionIntegrity)
		}
		state.CompletePresent, state.Complete, state.CompleteID = true, complete, id
	}
	if err := subtree.CheckPaths(directoryPath); err != nil {
		return state, classifyExecutionBindingError(err)
	}
	return state, nil
}

func executionRetentionEntryPresent(entries []fsbind.Entry, name string) bool {
	for _, entry := range entries {
		if entry.Name == name {
			return true
		}
	}
	return false
}

func ensureExecutionRetentionIntent(ctx context.Context, subtree *fsbind.Subtree, marker ExecutionRetentionIntent) (ExecutionRetentionMarkerID, executionRetentionMarkerReceipt, error) {
	raw, id, err := EncodeExecutionRetentionIntent(marker)
	if err != nil {
		return "", executionRetentionMarkerReceipt{}, err
	}
	return ensureExecutionRetentionMarker(ctx, subtree, executionRetentionIntentFile, "intent", raw, id)
}

func ensureExecutionRetentionComplete(ctx context.Context, subtree *fsbind.Subtree, marker ExecutionRetentionComplete) (ExecutionRetentionMarkerID, executionRetentionMarkerReceipt, error) {
	raw, id, err := EncodeExecutionRetentionComplete(marker)
	if err != nil {
		return "", executionRetentionMarkerReceipt{}, err
	}
	return ensureExecutionRetentionMarker(ctx, subtree, executionRetentionCompleteFile, "complete", raw, id)
}

func ensureExecutionRetentionMarker(ctx context.Context, subtree *fsbind.Subtree, destinationName, purpose string, raw []byte, id ExecutionRetentionMarkerID) (ExecutionRetentionMarkerID, executionRetentionMarkerReceipt, error) {
	receipt := executionRetentionMarkerReceipt{}
	if subtree == nil || len(raw) == 0 || int64(len(raw)) > maximumExecutionRetentionMarker {
		return "", receipt, fmt.Errorf("%w: source retirement retention marker input is unavailable", ErrExecutionPolicy)
	}
	if _, err := ParseExecutionRetentionMarkerID(id.String()); err != nil {
		return "", receipt, err
	}
	directoryPath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory})
	directory, inspectErr := subtree.Inspect(ctx, directoryPath)
	switch {
	case errors.Is(inspectErr, fsbind.ErrNotFound):
		mkdir, err := subtree.MkdirAll(ctx, directoryPath)
		receipt.DirectoryCreated = mkdir.DirectoriesCreated != 0
		receipt.DirectoryDurability = mkdir.Durability
		if err != nil {
			return "", receipt, err
		}
	case inspectErr != nil:
		return "", receipt, inspectErr
	case directory.Kind != fsbind.ObjectKindDirectory:
		return "", receipt, fmt.Errorf("%w: source retirement retention directory is unsafe", ErrExecutionIntegrity)
	}
	destination, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory, destinationName})
	temporaryName := executionRetentionTemporaryName(purpose, id)
	temporary, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory, temporaryName})
	if identity, err := verifyExecutionRetentionNamedBytes(ctx, subtree, destination, raw); err == nil {
		receipt.AlreadyPresent = true
		if tempInfo, tempErr := subtree.Inspect(ctx, temporary); tempErr == nil {
			if tempInfo.Kind != fsbind.ObjectKindRegular {
				return id, receipt, fmt.Errorf("%w: source retirement retention temporary object is unsafe", ErrExecutionIntegrity)
			}
			if _, verifyErr := verifyExecutionRetentionNamedBytes(ctx, subtree, temporary, raw); verifyErr != nil {
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
			return id, receipt, fmt.Errorf("%w: source retirement retention marker identity is unavailable", ErrExecutionIntegrity)
		}
		if err := confirmExecutionRetentionDurability(ctx, subtree); err != nil {
			return id, receipt, err
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
		written, writeErr := writeExecutionBytes(ctx, file, raw)
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
			return "", receipt, fmt.Errorf("%w: source retirement retention temporary object changed", ErrExecutionIntegrity)
		}
		tempInfo = fsbind.ObjectInfo{Identity: info.Identity, Kind: fsbind.ObjectKindRegular, SizeBytes: info.SizeBytes}
	} else if tempErr != nil {
		return "", receipt, tempErr
	} else if tempInfo.Kind != fsbind.ObjectKindRegular {
		return "", receipt, fmt.Errorf("%w: source retirement retention temporary object is unsafe", ErrExecutionIntegrity)
	}
	if _, err := verifyExecutionRetentionNamedBytes(ctx, subtree, temporary, raw); err != nil {
		return "", receipt, err
	}
	publication, publishErr := subtree.CommitRegularNoReplace(ctx, temporary, destination)
	receipt.Publication = publication
	if publishErr != nil {
		return id, receipt, publishErr
	}
	if !publication.Published || publication.Durability != fsbind.DurabilityConfirmed ||
		!publication.SourceIdentity.Equal(tempInfo.Identity) || !publication.FinalIdentity.Equal(tempInfo.Identity) {
		return id, receipt, fsbind.ErrPublicationAmbiguous
	}
	if _, err := verifyExecutionRetentionNamedBytes(ctx, subtree, destination, raw); err != nil {
		return id, receipt, err
	}
	if err := confirmExecutionRetentionDurability(ctx, subtree); err != nil {
		return id, receipt, err
	}
	return id, receipt, nil
}

func verifyExecutionRetentionNamedBytes(ctx context.Context, subtree *fsbind.Subtree, path fsbind.Path, expected []byte) (fsbind.Identity, error) {
	raw, object, err := readExecutionRetentionNamedBytes(ctx, subtree, path, int64(len(expected)))
	if err != nil {
		return fsbind.Identity{}, err
	}
	if !bytes.Equal(raw, expected) || object.Identity.IsZero() {
		return fsbind.Identity{}, fmt.Errorf("%w: source retirement retention marker bytes disagree", ErrExecutionIntegrity)
	}
	return object.Identity, nil
}

func readExecutionRetentionNamedBytes(ctx context.Context, subtree *fsbind.Subtree, path fsbind.Path, limit int64) ([]byte, fsbind.ObjectInfo, error) {
	if subtree == nil {
		return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: source retirement retention authority is unavailable", ErrExecutionIntegrity)
	}
	journal := &executionJournal{subtree: subtree}
	return journal.readNamedBytes(ctx, path, limit)
}

func executionRetentionTemporaryName(purpose string, id ExecutionRetentionMarkerID) string {
	return purpose + "-" + strings.TrimPrefix(id.String(), markerIDPrefix) + ".pending"
}

func confirmExecutionRetentionDurability(ctx context.Context, subtree *fsbind.Subtree) error {
	directoryPath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory})
	rootPath, _ := fsbind.PathFromComponents(nil)
	if err := subtree.SyncDirectory(ctx, directoryPath); err != nil {
		return err
	}
	if err := subtree.SyncDirectory(ctx, rootPath); err != nil {
		return err
	}
	return subtree.CheckPaths(rootPath, directoryPath)
}

func auditExecutionRetentionControl(ctx context.Context, subtree *fsbind.Subtree, state executionRetentionState, allowPending bool) error {
	if !state.DirectoryPresent {
		return nil
	}
	directoryPath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory})
	listing, err := subtree.List(ctx, directoryPath, fsbind.ListLimits{MaxEntries: 5, MaxNameBytes: 1 << 20})
	if err != nil {
		return classifyExecutionBindingError(err)
	}
	if !listing.Complete {
		return fmt.Errorf("%w: source retirement retention inventory is incomplete", ErrExecutionIntegrity)
	}
	entries := listing.Entries
	allowed := make(map[string]bool, 4)
	if state.IntentPresent {
		allowed[executionRetentionIntentFile] = true
		allowed[executionRetentionTemporaryName("intent", state.IntentID)] = allowPending
	}
	if state.CompletePresent {
		allowed[executionRetentionCompleteFile] = true
		allowed[executionRetentionTemporaryName("complete", state.CompleteID)] = allowPending
	}
	for _, entry := range entries {
		if !allowed[entry.Name] {
			// Before the intent is durable, exactly one canonical intent pending
			// name may remain from an interrupted first publication.
			if !state.IntentPresent && allowPending && strings.HasPrefix(entry.Name, "intent-") && strings.HasSuffix(entry.Name, ".pending") && len(entry.Name) == len("intent-")+64+len(".pending") {
				continue
			}
			return fmt.Errorf("%w: source retirement retention namespace contains an unexpected object", ErrExecutionIntegrity)
		}
	}
	if state.IntentPresent && !executionRetentionEntryPresent(entries, executionRetentionIntentFile) ||
		state.CompletePresent && !executionRetentionEntryPresent(entries, executionRetentionCompleteFile) {
		return fmt.Errorf("%w: source retirement retention namespace is incomplete", ErrExecutionIntegrity)
	}
	if state.IntentPresent {
		raw, id, err := EncodeExecutionRetentionIntent(state.Intent)
		if err != nil || id != state.IntentID {
			return fmt.Errorf("%w: source retirement retention intent identity changed", ErrExecutionIntegrity)
		}
		intentPath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory, executionRetentionIntentFile})
		if _, err := verifyExecutionRetentionNamedBytes(ctx, subtree, intentPath, raw); err != nil {
			return err
		}
		pendingName := executionRetentionTemporaryName("intent", id)
		if allowPending && executionRetentionEntryPresent(entries, pendingName) {
			pendingPath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory, pendingName})
			if _, err := verifyExecutionRetentionNamedBytes(ctx, subtree, pendingPath, raw); err != nil {
				return err
			}
		}
	}
	if state.CompletePresent {
		raw, id, err := EncodeExecutionRetentionComplete(state.Complete)
		if err != nil || id != state.CompleteID {
			return fmt.Errorf("%w: source retirement retention completion identity changed", ErrExecutionIntegrity)
		}
		completePath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory, executionRetentionCompleteFile})
		if _, err := verifyExecutionRetentionNamedBytes(ctx, subtree, completePath, raw); err != nil {
			return err
		}
		pendingName := executionRetentionTemporaryName("complete", id)
		if allowPending && executionRetentionEntryPresent(entries, pendingName) {
			pendingPath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory, pendingName})
			if _, err := verifyExecutionRetentionNamedBytes(ctx, subtree, pendingPath, raw); err != nil {
				return err
			}
		}
	}
	after, err := subtree.List(ctx, directoryPath, fsbind.ListLimits{MaxEntries: 5, MaxNameBytes: 1 << 20})
	if err != nil {
		return classifyExecutionBindingError(err)
	}
	if !after.Complete || !sameExecutionRetentionEntries(entries, after.Entries) {
		return fmt.Errorf("%w: source retirement retention namespace changed during observation", ErrExecutionIntegrity)
	}
	return subtree.CheckPaths(directoryPath)
}

func sameExecutionRetentionEntries(left, right []fsbind.Entry) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Name != right[index].Name || left[index].Kind != right[index].Kind {
			return false
		}
	}
	return true
}
