package clientadopt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

type retentionState struct {
	DirectoryPresent bool
	IntentPresent    bool
	IntentPending    bool
	Intent           RetentionIntent
	IntentID         RetentionMarkerID
	CompletePresent  bool
	CompletePending  bool
	Complete         RetentionComplete
	CompleteID       RetentionMarkerID
	ForgetPending    bool
	ForgetIntent     ForgetIntent
	ForgetID         ForgetMarkerID
	ForgetName       string
	Entries          []fsbind.Entry
}

func loadRetentionState(ctx context.Context, handle *journalHandle, operation OperationID, targetIdentity fsbind.Identity) (retentionState, error) {
	state := retentionState{Entries: []fsbind.Entry{}}
	if handle == nil || handle.subtree == nil || targetIdentity.IsZero() {
		return state, fmt.Errorf("%w: client adoption retention authority is unavailable", ErrIntegrity)
	}
	directoryPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	directory, err := handle.subtree.Inspect(ctx, directoryPath)
	if errors.Is(err, fsbind.ErrNotFound) {
		return state, nil
	}
	if err != nil {
		return state, classifyJournalError(err)
	}
	state.DirectoryPresent = true
	if directory.Kind != fsbind.ObjectKindDirectory || directory.Identity.IsZero() {
		return state, fmt.Errorf("%w: client adoption retention directory is unsafe", ErrIntegrity)
	}
	listing, err := handle.subtree.List(ctx, directoryPath, fsbind.ListLimits{MaxEntries: 5, MaxNameBytes: 1 << 20})
	if err != nil {
		return state, classifyJournalError(err)
	}
	if !listing.Complete || len(listing.Entries) > 4 {
		return state, fmt.Errorf("%w: client adoption retention inventory is incomplete", ErrIntegrity)
	}
	state.Entries = append(state.Entries, listing.Entries...)
	seen := make(map[string]bool)
	for _, entry := range listing.Entries {
		if entry.Kind != string(fsbind.ObjectKindRegular) || seen[entry.Name] {
			return state, fmt.Errorf("%w: client adoption retention control object is unsafe", ErrIntegrity)
		}
		seen[entry.Name] = true
		switch entry.Name {
		case retentionIntentName, retentionIntentPending, retentionCompleteName, retentionCompletePending:
		default:
			if !strings.HasPrefix(entry.Name, "forget-") || !strings.HasSuffix(entry.Name, ".pending") || state.ForgetName != "" {
				return state, fmt.Errorf("%w: client adoption retention contains an unexpected object", ErrIntegrity)
			}
			state.ForgetName = entry.Name
		}
	}
	readIntent := func(name string) (RetentionIntent, RetentionMarkerID, error) {
		raw, readErr := handle.readNamedBytes(ctx, []string{retentionDirectoryName, name})
		if readErr != nil {
			return RetentionIntent{}, "", readErr
		}
		return DecodeRetentionIntent(bytes.NewReader(raw))
	}
	if seen[retentionIntentName] {
		intent, id, decodeErr := readIntent(retentionIntentName)
		if decodeErr != nil || !retentionIntentMatchesAuthority(intent, operation, handle.subtree.Identity(), targetIdentity) {
			return state, fmt.Errorf("%w: client adoption retention intent is bound to another operation", ErrIntegrity)
		}
		state.IntentPresent, state.Intent, state.IntentID = true, intent, id
	}
	if seen[retentionIntentPending] {
		intent, id, decodeErr := readIntent(retentionIntentPending)
		if decodeErr != nil || !retentionIntentMatchesAuthority(intent, operation, handle.subtree.Identity(), targetIdentity) {
			return state, fmt.Errorf("%w: pending client adoption retention intent is invalid", ErrIntegrity)
		}
		if state.IntentPresent && (!sameRetentionIntent(state.Intent, intent) || state.IntentID != id) {
			return state, fmt.Errorf("%w: pending client adoption retention intent disagrees", ErrIntegrity)
		}
		state.IntentPending = true
		if !state.IntentPresent {
			state.Intent, state.IntentID = intent, id
		}
	}
	readComplete := func(name string) (RetentionComplete, RetentionMarkerID, error) {
		raw, readErr := handle.readNamedBytes(ctx, []string{retentionDirectoryName, name})
		if readErr != nil {
			return RetentionComplete{}, "", readErr
		}
		return DecodeRetentionComplete(bytes.NewReader(raw))
	}
	if seen[retentionCompleteName] || seen[retentionCompletePending] {
		if !state.IntentPresent {
			return state, fmt.Errorf("%w: client adoption retention completion exists without a durable intent", ErrIntegrity)
		}
		if seen[retentionCompleteName] {
			complete, id, decodeErr := readComplete(retentionCompleteName)
			if decodeErr != nil || !retentionCompleteMatchesIntent(complete, state.Intent, state.IntentID) {
				return state, fmt.Errorf("%w: client adoption retention completion disagrees", ErrIntegrity)
			}
			state.CompletePresent, state.Complete, state.CompleteID = true, complete, id
		}
		if seen[retentionCompletePending] {
			complete, id, decodeErr := readComplete(retentionCompletePending)
			if decodeErr != nil || !retentionCompleteMatchesIntent(complete, state.Intent, state.IntentID) {
				return state, fmt.Errorf("%w: pending client adoption retention completion disagrees", ErrIntegrity)
			}
			if state.CompletePresent && (state.Complete != complete || state.CompleteID != id) {
				return state, fmt.Errorf("%w: pending client adoption retention completion changed", ErrIntegrity)
			}
			state.CompletePending = true
			if !state.CompletePresent {
				state.Complete, state.CompleteID = complete, id
			}
		}
	}
	if state.ForgetName != "" {
		if !state.IntentPresent || !state.CompletePresent || state.IntentPending || state.CompletePending {
			return state, fmt.Errorf("%w: client adoption forget pending marker lacks one exact tombstone", ErrIntegrity)
		}
		raw, readErr := handle.readNamedBytes(ctx, []string{retentionDirectoryName, state.ForgetName})
		if readErr != nil {
			return state, readErr
		}
		forget, id, decodeErr := DecodeForgetIntent(bytes.NewReader(raw))
		if decodeErr != nil || forgetPendingName(id) != state.ForgetName || forget.OperationID != operation ||
			forget.OperationRootIdentity != handle.subtree.Identity().String() || forget.TargetRootIdentity != targetIdentity.String() ||
			forget.RetentionIntentMarkerID != state.IntentID || forget.RetentionCompleteMarkerID != state.CompleteID ||
			!sameRetentionIntent(forget.RetentionIntent, state.Intent) || forget.RetentionComplete != state.Complete {
			return state, fmt.Errorf("%w: client adoption forget pending marker is invalid", ErrIntegrity)
		}
		state.ForgetPending, state.ForgetIntent, state.ForgetID = true, forget, id
	}
	if err := confirmRetentionReadState(ctx, handle, state, seen); err != nil {
		return state, err
	}
	return state, nil
}

func confirmRetentionReadState(ctx context.Context, handle *journalHandle, state retentionState, seen map[string]bool) error {
	directoryPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	fresh, err := handle.subtree.List(ctx, directoryPath, fsbind.ListLimits{MaxEntries: 5, MaxNameBytes: 1 << 20})
	if err != nil {
		return classifyJournalError(err)
	}
	if !fresh.Complete || !sameJournalEntries(state.Entries, fresh.Entries) {
		return fmt.Errorf("%w: client adoption retention namespace changed while read", ErrIntegrity)
	}
	if seen[retentionIntentName] || seen[retentionIntentPending] {
		raw, _, encodeErr := EncodeRetentionIntent(state.Intent)
		if encodeErr != nil {
			return encodeErr
		}
		for _, name := range []string{retentionIntentName, retentionIntentPending} {
			if seen[name] {
				if _, verifyErr := handle.verifyNamedBytes(ctx, []string{retentionDirectoryName, name}, raw); verifyErr != nil {
					return verifyErr
				}
			}
		}
	}
	if seen[retentionCompleteName] || seen[retentionCompletePending] {
		raw, _, encodeErr := EncodeRetentionComplete(state.Complete)
		if encodeErr != nil {
			return encodeErr
		}
		for _, name := range []string{retentionCompleteName, retentionCompletePending} {
			if seen[name] {
				if _, verifyErr := handle.verifyNamedBytes(ctx, []string{retentionDirectoryName, name}, raw); verifyErr != nil {
					return verifyErr
				}
			}
		}
	}
	if state.ForgetPending {
		raw, _, encodeErr := EncodeForgetIntent(state.ForgetIntent)
		if encodeErr != nil {
			return encodeErr
		}
		if _, verifyErr := handle.verifyNamedBytes(ctx, []string{retentionDirectoryName, state.ForgetName}, raw); verifyErr != nil {
			return verifyErr
		}
	}
	if err := handle.subtree.CheckPaths(directoryPath); err != nil {
		return classifyJournalError(err)
	}
	return nil
}

func retentionIntentMatchesAuthority(marker RetentionIntent, operation OperationID, operationIdentity, targetIdentity fsbind.Identity) bool {
	return marker.Validate() == nil && marker.OperationID == operation &&
		marker.OperationRootIdentity == operationIdentity.String() && marker.TargetRootIdentity == targetIdentity.String()
}

func sameRetentionIntent(left, right RetentionIntent) bool {
	leftRaw, leftID, leftErr := EncodeRetentionIntent(left)
	rightRaw, rightID, rightErr := EncodeRetentionIntent(right)
	return leftErr == nil && rightErr == nil && leftID == rightID && bytes.Equal(leftRaw, rightRaw)
}

func retentionCompleteMatchesIntent(marker RetentionComplete, intent RetentionIntent, intentID RetentionMarkerID) bool {
	return marker.Validate() == nil && marker.OperationID == intent.OperationID && marker.PlanID == intent.PlanID &&
		marker.OperationRootIdentity == intent.OperationRootIdentity && marker.TargetRootIdentity == intent.TargetRootIdentity &&
		marker.IntentMarkerID == intentID && marker.CompletionID == intent.CompletionID
}

func (state retentionState) journalState() journalState {
	result := journalState{Retained: true, RetentionIntentDurable: state.IntentPresent,
		RetentionComplete: state.CompletePresent && !state.IntentPending && !state.CompletePending, RetentionIntentID: state.IntentID,
		RetentionCompleteID: state.CompleteID, Attempts: []Attempt{}, AttemptIDs: []MarkerID{}}
	if state.IntentID == "" {
		return result
	}
	result.Intent = state.Intent.Intent
	result.IntentID = state.Intent.IntentID
	result.Attempts = append(result.Attempts, state.Intent.Attempts...)
	result.AttemptIDs = append(result.AttemptIDs, state.Intent.AttemptIDs...)
	completion := state.Intent.Completion
	result.Completion = &completion
	result.CompletionID = state.Intent.CompletionID
	result.Durable = state.CompletePresent
	return result
}

func ensureRetentionDirectory(ctx context.Context, handle *journalHandle) (retentionMarkerReceipt, error) {
	receipt := retentionMarkerReceipt{}
	path, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	object, err := handle.subtree.Inspect(ctx, path)
	if err == nil {
		if object.Kind != fsbind.ObjectKindDirectory {
			return receipt, fmt.Errorf("%w: client adoption retention path is unsafe", ErrIntegrity)
		}
		return receipt, handle.subtree.CheckPaths(path)
	}
	if !errors.Is(err, fsbind.ErrNotFound) {
		return receipt, classifyJournalError(err)
	}
	mkdir, err := handle.subtree.MkdirAll(ctx, path)
	if mkdir.DirectoriesCreated > 0 {
		receipt.DirectoryCreated = true
		receipt.DirectoryDurability = mkdir.Durability
	}
	if err != nil {
		return receipt, err
	}
	if mkdir.DirectoriesCreated != 1 {
		return receipt, fmt.Errorf("%w: client adoption retention directory creation was not exact", ErrIntegrity)
	}
	if err := handle.subtree.SyncDirectory(ctx, fsbind.Path{}); err != nil {
		return receipt, err
	}
	return receipt, handle.subtree.CheckPaths(path)
}

func ensureRetentionMarker(ctx context.Context, handle *journalHandle, destination, pending string, raw []byte) (retentionMarkerReceipt, error) {
	receipt, err := ensureRetentionDirectory(ctx, handle)
	if err != nil {
		return receipt, err
	}
	if len(raw) == 0 || int64(len(raw)) > maximumMarkerBytes {
		return receipt, fmt.Errorf("%w: client adoption retention marker exceeds its budget", ErrPolicy)
	}
	destinationPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, destination})
	pendingPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, pending})
	if object, inspectErr := handle.subtree.Inspect(ctx, destinationPath); inspectErr == nil {
		if object.Kind != fsbind.ObjectKindRegular {
			return receipt, fmt.Errorf("%w: client adoption retention marker destination is unsafe", ErrIntegrity)
		}
		if _, verifyErr := handle.verifyNamedBytes(ctx, []string{retentionDirectoryName, destination}, raw); verifyErr != nil {
			return receipt, verifyErr
		}
		receipt.AlreadyPresent = true
		if pendingObject, pendingErr := handle.subtree.Inspect(ctx, pendingPath); pendingErr == nil {
			if _, verifyErr := handle.verifyNamedBytes(ctx, []string{retentionDirectoryName, pending}, raw); verifyErr != nil {
				return receipt, verifyErr
			}
			removal, removeErr := handle.subtree.RemoveRegularExact(ctx, pendingPath, pendingObject.Identity, pendingObject.SizeBytes)
			receipt.TemporaryRemoval = removal
			if removeErr != nil {
				receipt.TemporaryRemovalUncertain = true
				return receipt, removeErr
			}
		} else if !errors.Is(pendingErr, fsbind.ErrNotFound) {
			return receipt, classifyJournalError(pendingErr)
		}
		return receipt, confirmRetentionDurability(ctx, handle)
	} else if !errors.Is(inspectErr, fsbind.ErrNotFound) {
		return receipt, classifyJournalError(inspectErr)
	}
	pendingObject, pendingErr := handle.subtree.Inspect(ctx, pendingPath)
	if errors.Is(pendingErr, fsbind.ErrNotFound) {
		file, createErr := handle.subtree.CreateRegular(ctx, pendingPath)
		if createErr != nil {
			if object, inspectErr := handle.subtree.Inspect(ctx, pendingPath); inspectErr == nil && object.Kind == fsbind.ObjectKindRegular {
				receipt.TemporaryCreationUncertain = true
			}
			return receipt, createErr
		}
		receipt.TemporaryCreated = true
		written, writeErr := writeAll(ctx, file, raw)
		receipt.TemporaryBytesWritten = written
		syncErr := file.Sync()
		info, infoErr := file.Info()
		closeErr := file.Close()
		receipt.TemporaryStateUncertain = syncErr != nil || infoErr != nil || closeErr != nil
		if writeErr != nil {
			return receipt, writeErr
		}
		if syncErr != nil || infoErr != nil || closeErr != nil || written != int64(len(raw)) || info.SizeBytes != int64(len(raw)) {
			return receipt, fmt.Errorf("client adoption retention staging could not be confirmed")
		}
		pendingObject = fsbind.ObjectInfo{Identity: info.Identity, Kind: fsbind.ObjectKindRegular, SizeBytes: info.SizeBytes}
	} else if pendingErr != nil {
		return receipt, classifyJournalError(pendingErr)
	} else if pendingObject.Kind != fsbind.ObjectKindRegular {
		return receipt, fmt.Errorf("%w: client adoption retention staging object is unsafe", ErrIntegrity)
	}
	if _, err := handle.verifyNamedBytes(ctx, []string{retentionDirectoryName, pending}, raw); err != nil {
		return receipt, err
	}
	publication, publishErr := handle.subtree.CommitRegularNoReplace(ctx, pendingPath, destinationPath)
	receipt.Publication = publication
	if publishErr != nil {
		receipt.PublicationUncertain = publication.Attempted
		if errors.Is(publishErr, fsbind.ErrAlreadyExists) {
			if _, verifyErr := handle.verifyNamedBytes(ctx, []string{retentionDirectoryName, destination}, raw); verifyErr == nil {
				receipt.AlreadyPresent = true
				removal, removeErr := handle.subtree.RemoveRegularExact(ctx, pendingPath, pendingObject.Identity, pendingObject.SizeBytes)
				receipt.TemporaryRemoval = removal
				receipt.TemporaryRemovalUncertain = removeErr != nil
				if removeErr != nil {
					return receipt, removeErr
				}
				return receipt, confirmRetentionDurability(ctx, handle)
			}
		}
		return receipt, publishErr
	}
	if !publication.Published || !publication.SourceIdentity.Equal(pendingObject.Identity) || !publication.FinalIdentity.Equal(pendingObject.Identity) {
		receipt.PublicationUncertain = true
		return receipt, fsbind.ErrPublicationAmbiguous
	}
	if _, err := handle.verifyNamedBytes(ctx, []string{retentionDirectoryName, destination}, raw); err != nil {
		receipt.PublicationUncertain = true
		return receipt, err
	}
	directoryPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	if err := handle.subtree.SyncDirectory(ctx, directoryPath); err != nil {
		receipt.PublicationUncertain = true
		return receipt, err
	}
	if err := handle.subtree.SyncDirectory(ctx, fsbind.Path{}); err != nil {
		receipt.PublicationUncertain = true
		return receipt, err
	}
	if err := handle.subtree.CheckPaths(fsbind.Path{}, directoryPath); err != nil {
		receipt.PublicationUncertain = true
		return receipt, classifyJournalError(err)
	}
	return receipt, nil
}

func confirmRetentionDurability(ctx context.Context, handle *journalHandle) error {
	if handle == nil || handle.subtree == nil {
		return fmt.Errorf("%w: client adoption retention authority is unavailable", ErrIntegrity)
	}
	directoryPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	if err := handle.subtree.SyncDirectory(ctx, directoryPath); err != nil {
		return err
	}
	if err := handle.subtree.SyncDirectory(ctx, fsbind.Path{}); err != nil {
		return err
	}
	if err := handle.subtree.CheckPaths(fsbind.Path{}, directoryPath); err != nil {
		return classifyJournalError(err)
	}
	return nil
}

func removeExpectedRetentionFile(ctx context.Context, handle *journalHandle, components []string, expected []byte) (fsbind.Removal, bool, error) {
	path, err := fsbind.PathFromComponents(components)
	if err != nil {
		return fsbind.Removal{}, false, fmt.Errorf("%w: client adoption retained marker path is invalid", ErrIntegrity)
	}
	object, inspectErr := handle.subtree.Inspect(ctx, path)
	if errors.Is(inspectErr, fsbind.ErrNotFound) {
		return fsbind.Removal{}, true, nil
	}
	if inspectErr != nil {
		return fsbind.Removal{}, false, classifyJournalError(inspectErr)
	}
	if object.Kind != fsbind.ObjectKindRegular || object.SizeBytes != int64(len(expected)) {
		return fsbind.Removal{}, false, fmt.Errorf("%w: client adoption retained marker object is unsafe", ErrIntegrity)
	}
	raw, readErr := handle.readNamedBytes(ctx, components)
	if readErr != nil {
		return fsbind.Removal{}, false, readErr
	}
	if !bytes.Equal(raw, expected) {
		return fsbind.Removal{}, false, fmt.Errorf("%w: client adoption retained marker bytes disagree", ErrIntegrity)
	}
	removal, removeErr := handle.subtree.RemoveRegularExact(ctx, path, object.Identity, object.SizeBytes)
	return removal, false, removeErr
}
