package clientactivate

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
		return state, fmt.Errorf("%w: client activation retention authority is unavailable", ErrIntegrity)
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
		return state, fmt.Errorf("%w: client activation retention directory is unsafe", ErrIntegrity)
	}
	listing, err := handle.subtree.List(ctx, directoryPath, fsbind.ListLimits{MaxEntries: 5, MaxNameBytes: 1 << 20})
	if err != nil {
		return state, classifyJournalError(err)
	}
	if !listing.Complete || len(listing.Entries) > 4 {
		return state, fmt.Errorf("%w: client activation retention inventory is incomplete", ErrIntegrity)
	}
	state.Entries = append(state.Entries, listing.Entries...)
	seen := make(map[string]bool)
	for _, entry := range listing.Entries {
		if entry.Kind != string(fsbind.ObjectKindRegular) || seen[entry.Name] {
			return state, fmt.Errorf("%w: client activation retention control object is unsafe", ErrIntegrity)
		}
		seen[entry.Name] = true
		switch entry.Name {
		case retentionIntentName, retentionIntentPending, retentionCompleteName, retentionCompletePending:
		default:
			if !strings.HasPrefix(entry.Name, "forget-") || !strings.HasSuffix(entry.Name, ".pending") || state.ForgetName != "" {
				return state, fmt.Errorf("%w: client activation retention contains an unexpected object", ErrIntegrity)
			}
			state.ForgetName = entry.Name
		}
	}
	readIntent := func(name string) (RetentionIntent, RetentionMarkerID, error) {
		raw, readErr := handle.readNamedBytes(ctx, []string{retentionDirectoryName, name})
		if readErr != nil {
			return RetentionIntent{}, "", readErr
		}
		intent, id, decodeErr := DecodeRetentionIntent(bytes.NewReader(raw))
		if decodeErr != nil {
			return RetentionIntent{}, "", fmt.Errorf("%w: client activation retention intent is invalid", ErrIntegrity)
		}
		return intent, id, nil
	}
	if seen[retentionIntentName] {
		intent, id, decodeErr := readIntent(retentionIntentName)
		if decodeErr != nil {
			return state, decodeErr
		}
		if !retentionIntentMatchesAuthority(intent, operation, handle.subtree.Identity(), targetIdentity) {
			return state, fmt.Errorf("%w: client activation retention intent is bound to another operation", ErrIntegrity)
		}
		state.IntentPresent, state.Intent, state.IntentID = true, intent, id
	}
	if seen[retentionIntentPending] {
		intent, id, decodeErr := readIntent(retentionIntentPending)
		if decodeErr != nil {
			return state, decodeErr
		}
		if !retentionIntentMatchesAuthority(intent, operation, handle.subtree.Identity(), targetIdentity) {
			return state, fmt.Errorf("%w: pending client activation retention intent is invalid", ErrIntegrity)
		}
		if state.IntentPresent && (!sameRetentionIntent(state.Intent, intent) || state.IntentID != id) {
			return state, fmt.Errorf("%w: pending client activation retention intent disagrees", ErrIntegrity)
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
		complete, id, decodeErr := DecodeRetentionComplete(bytes.NewReader(raw))
		if decodeErr != nil {
			return RetentionComplete{}, "", fmt.Errorf("%w: client activation retention completion is invalid", ErrIntegrity)
		}
		return complete, id, nil
	}
	if seen[retentionCompleteName] || seen[retentionCompletePending] {
		if !state.IntentPresent {
			return state, fmt.Errorf("%w: client activation retention completion exists without a durable intent", ErrIntegrity)
		}
		if seen[retentionCompleteName] {
			complete, id, decodeErr := readComplete(retentionCompleteName)
			if decodeErr != nil {
				return state, decodeErr
			}
			if !retentionCompleteMatchesIntent(complete, state.Intent, state.IntentID) {
				return state, fmt.Errorf("%w: client activation retention completion disagrees", ErrIntegrity)
			}
			state.CompletePresent, state.Complete, state.CompleteID = true, complete, id
		}
		if seen[retentionCompletePending] {
			complete, id, decodeErr := readComplete(retentionCompletePending)
			if decodeErr != nil {
				return state, decodeErr
			}
			if !retentionCompleteMatchesIntent(complete, state.Intent, state.IntentID) {
				return state, fmt.Errorf("%w: pending client activation retention completion disagrees", ErrIntegrity)
			}
			if state.CompletePresent && (state.Complete != complete || state.CompleteID != id) {
				return state, fmt.Errorf("%w: pending client activation retention completion changed", ErrIntegrity)
			}
			state.CompletePending = true
			if !state.CompletePresent {
				state.Complete, state.CompleteID = complete, id
			}
		}
	}
	if state.ForgetName != "" {
		if !state.IntentPresent || !state.CompletePresent || state.IntentPending || state.CompletePending {
			return state, fmt.Errorf("%w: client activation forget pending marker lacks one exact tombstone", ErrIntegrity)
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
			return state, fmt.Errorf("%w: client activation forget pending marker is invalid", ErrIntegrity)
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
	if !fresh.Complete || !sameEntries(state.Entries, fresh.Entries) {
		return fmt.Errorf("%w: client activation retention namespace changed while read", ErrIntegrity)
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
	return classifyJournalError(handle.subtree.CheckPaths(directoryPath))
}

func retentionIntentMatchesAuthority(marker RetentionIntent, operation OperationID, operationIdentity, targetIdentity fsbind.Identity) bool {
	return marker.Validate() == nil && marker.OperationID == operation && marker.OperationRootIdentity == operationIdentity.String() &&
		marker.TargetRootIdentity == targetIdentity.String()
}

func sameRetentionIntent(left, right RetentionIntent) bool {
	leftRaw, leftID, leftErr := EncodeRetentionIntent(left)
	rightRaw, rightID, rightErr := EncodeRetentionIntent(right)
	return leftErr == nil && rightErr == nil && leftID == rightID && bytes.Equal(leftRaw, rightRaw)
}

func retentionCompleteMatchesIntent(marker RetentionComplete, intent RetentionIntent, intentID RetentionMarkerID) bool {
	terminalID := intent.RecheckCompletionID
	if intent.ActivationCompletion != nil {
		terminalID = intent.ActivationCompletionID
	}
	return marker.Validate() == nil && marker.OperationID == intent.OperationID && marker.PlanID == intent.PlanID &&
		marker.OperationRootIdentity == intent.OperationRootIdentity && marker.TargetRootIdentity == intent.TargetRootIdentity &&
		marker.IntentMarkerID == intentID && marker.TerminalMarkerID == terminalID
}

func markerIDForName(name string, raw []byte) (MarkerID, error) {
	switch {
	case name == intentFileName:
		_, id, err := decodeIntent(bytes.NewReader(raw))
		return id, err
	case name == recheckStartedFileName:
		_, id, err := decodeRecheckStarted(bytes.NewReader(raw))
		return id, err
	case name == recheckCompletionFileName:
		_, id, err := decodeRecheckCompletion(bytes.NewReader(raw))
		return id, err
	case name == activationCompletionName:
		_, id, err := decodeActivationCompletion(bytes.NewReader(raw))
		return id, err
	case knownMarkerName(name):
		value, id, err := decodeAttempt(bytes.NewReader(raw))
		if err != nil {
			return "", err
		}
		if attemptFileName(value.Action, value.Sequence) != name {
			return "", fmt.Errorf("%w: retained activation attempt name disagrees", ErrIntegrity)
		}
		return id, nil
	default:
		return "", fmt.Errorf("%w: retained activation marker name is invalid", ErrIntegrity)
	}
}

func (state retentionState) journalState() journalState {
	result := journalState{Retained: true, RetentionIntentDurable: state.IntentPresent,
		RetentionComplete: state.CompletePresent && !state.IntentPending && !state.CompletePending,
		RetentionIntentID: state.IntentID, RetentionCompleteID: state.CompleteID,
		RecheckAttempts: []Attempt{}, RecheckAttemptIDs: []MarkerID{}, StartAttempts: []Attempt{}, StartAttemptIDs: []MarkerID{}}
	if state.IntentID == "" {
		return result
	}
	result.Intent, result.IntentID = state.Intent.Intent, state.Intent.IntentID
	recheck := state.Intent.RecheckCompletion
	result.RecheckCompletion, result.RecheckCompletionID = &recheck, state.Intent.RecheckCompletionID
	if state.Intent.ActivationCompletion != nil {
		activation := *state.Intent.ActivationCompletion
		result.ActivationCompletion, result.ActivationCompletionID = &activation, state.Intent.ActivationCompletionID
	}
	result.Durable = result.RetentionComplete
	return result
}
