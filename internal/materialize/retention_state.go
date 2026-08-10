package materialize

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

type retentionMarkerState struct {
	DirectoryPresent bool
	IntentPresent    bool
	Intent           RetentionIntent
	IntentID         RetentionMarkerID
	CompletePresent  bool
	Complete         RetentionComplete
	CompleteID       RetentionMarkerID
}

func loadRetentionMarkerState(ctx context.Context, subtree *fsbind.Subtree, operationID OperationID, targetIdentity fsbind.Identity) (retentionMarkerState, error) {
	state := retentionMarkerState{}
	if subtree == nil || targetIdentity.IsZero() {
		return state, fmt.Errorf("%w: retention authority is unavailable", ErrPolicy)
	}
	retentionPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	directory, err := subtree.Inspect(ctx, retentionPath)
	if errors.Is(err, fsbind.ErrNotFound) {
		return state, nil
	}
	if err != nil {
		return state, classifyRetentionAuthorityError(err)
	}
	if directory.Kind != fsbind.ObjectKindDirectory {
		return state, fmt.Errorf("%w: retention control directory is unsafe", ErrIntegrity)
	}
	state.DirectoryPresent = true

	intentPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, retentionIntentFileName})
	intentRaw, intentErr := readRetentionNamedBytes(ctx, subtree, intentPath)
	if errors.Is(intentErr, fsbind.ErrNotFound) {
		completePath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, retentionCompleteName})
		if _, completeErr := subtree.Inspect(ctx, completePath); completeErr == nil {
			return state, fmt.Errorf("%w: retention completion exists without an intent", ErrIntegrity)
		} else if !errors.Is(completeErr, fsbind.ErrNotFound) {
			return state, classifyRetentionAuthorityError(completeErr)
		}
		return state, nil
	}
	if intentErr != nil {
		return state, intentErr
	}
	intent, intentID, err := DecodeRetentionIntent(bytes.NewReader(intentRaw))
	if err != nil {
		return state, fmt.Errorf("%w: retention intent is invalid", ErrIntegrity)
	}
	if intent.OperationID != operationID || intent.OperationRootIdentity != subtree.Identity().String() ||
		intent.TargetRootIdentity != targetIdentity.String() {
		return state, fmt.Errorf("%w: retention intent is bound to another filesystem object", ErrIntegrity)
	}
	state.IntentPresent, state.Intent, state.IntentID = true, intent, intentID

	completePath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, retentionCompleteName})
	completeRaw, completeErr := readRetentionNamedBytes(ctx, subtree, completePath)
	if completeErr == nil {
		complete, completeID, decodeErr := DecodeRetentionComplete(bytes.NewReader(completeRaw))
		if decodeErr != nil {
			return state, fmt.Errorf("%w: retention completion is invalid", ErrIntegrity)
		}
		if complete.OperationID != operationID || complete.OperationRootIdentity != subtree.Identity().String() ||
			complete.TargetRootIdentity != targetIdentity.String() || complete.IntentMarkerID != intentID {
			return state, fmt.Errorf("%w: retention completion is bound to another operation", ErrIntegrity)
		}
		state.CompletePresent, state.Complete, state.CompleteID = true, complete, completeID
	} else if !errors.Is(completeErr, fsbind.ErrNotFound) {
		return state, completeErr
	}

	if err := auditRetentionControlDirectory(ctx, subtree, state, true); err != nil {
		return state, err
	}
	return state, nil
}

func auditRetentionControlDirectory(ctx context.Context, subtree *fsbind.Subtree, state retentionMarkerState, allowPending bool) error {
	if subtree == nil || !state.DirectoryPresent || !state.IntentPresent {
		return fmt.Errorf("%w: retention intent authority is unavailable", ErrIntegrity)
	}
	intentRaw, intentID, err := EncodeRetentionIntent(state.Intent)
	if err != nil || intentID != state.IntentID {
		return fmt.Errorf("%w: retention intent cannot be re-encoded", ErrIntegrity)
	}
	complete := RetentionComplete{
		Schema: RetentionCompleteSchemaV1, OperationID: state.Intent.OperationID,
		OperationRootIdentity: state.Intent.OperationRootIdentity,
		TargetRootIdentity:    state.Intent.TargetRootIdentity, IntentMarkerID: state.IntentID,
	}
	completeRaw, completeID, err := EncodeRetentionComplete(complete)
	if err != nil {
		return fmt.Errorf("%w: retention completion cannot be constructed", ErrIntegrity)
	}
	if state.CompletePresent && (state.Complete != complete || state.CompleteID != completeID) {
		return fmt.Errorf("%w: retention completion disagrees with its intent", ErrIntegrity)
	}

	retentionPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	listing, err := subtree.List(ctx, retentionPath, fsbind.ListLimits{MaxEntries: 8, MaxNameBytes: 1 << 20})
	if err != nil {
		return classifyRetentionAuthorityError(err)
	}
	if !listing.Complete {
		return fmt.Errorf("%w: retention control inventory is incomplete", ErrIntegrity)
	}
	intentPending := retentionTemporaryName("intent", intentID)
	completePending := retentionTemporaryName("complete", completeID)
	allowed := map[string]string{retentionIntentFileName: "regular"}
	if state.CompletePresent {
		allowed[retentionCompleteName] = "regular"
	}
	if allowPending {
		allowed[intentPending] = "regular"
		allowed[completePending] = "regular"
	}
	seen := make(map[string]bool, len(allowed))
	for _, entry := range listing.Entries {
		kind, ok := allowed[entry.Name]
		if !ok || entry.Kind != kind {
			return fmt.Errorf("%w: retention control directory contains an unexpected object", ErrIntegrity)
		}
		seen[entry.Name] = true
		switch entry.Name {
		case retentionIntentFileName, retentionCompleteName:
			// The named marker was already read and rebound above.
		case intentPending:
			path, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, intentPending})
			if _, verifyErr := verifyRetentionNamedBytes(ctx, subtree, path, intentRaw); verifyErr != nil {
				return verifyErr
			}
		case completePending:
			path, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, completePending})
			if _, verifyErr := verifyRetentionNamedBytes(ctx, subtree, path, completeRaw); verifyErr != nil {
				return verifyErr
			}
		}
	}
	if !seen[retentionIntentFileName] || state.CompletePresent && !seen[retentionCompleteName] {
		return fmt.Errorf("%w: retention control directory is incomplete", ErrIntegrity)
	}
	intentPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, retentionIntentFileName})
	if _, err := verifyRetentionNamedBytes(ctx, subtree, intentPath, intentRaw); err != nil {
		return err
	}
	if state.CompletePresent {
		completePath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, retentionCompleteName})
		if _, err := verifyRetentionNamedBytes(ctx, subtree, completePath, completeRaw); err != nil {
			return err
		}
	}
	if err := subtree.CheckPaths(retentionPath); err != nil {
		return classifyRetentionAuthorityError(err)
	}
	return nil
}

func retentionTemporaryName(purpose string, id RetentionMarkerID) string {
	return purpose + "-" + id.String()[len("sha256:"):] + ".pending"
}

func readRetentionNamedBytes(ctx context.Context, subtree *fsbind.Subtree, path fsbind.Path) ([]byte, error) {
	object, err := subtree.Inspect(ctx, path)
	if err != nil {
		return nil, err
	}
	if object.Kind != fsbind.ObjectKindRegular || object.Identity.IsZero() || object.SizeBytes <= 0 || object.SizeBytes > maxRetentionMarkerBytes {
		return nil, fmt.Errorf("%w: retention marker object is unsafe", ErrIntegrity)
	}
	file, err := subtree.OpenRegular(ctx, path)
	if err != nil {
		return nil, classifyRetentionAuthorityError(err)
	}
	before, beforeErr := file.Info()
	raw, readErr := io.ReadAll(&contextExactReader{ctx: ctx, reader: file, remaining: maxRetentionMarkerBytes + 1})
	after, afterErr := file.Info()
	closeErr := file.Close()
	if beforeErr != nil {
		return nil, classifyRetentionAuthorityError(beforeErr)
	}
	if readErr != nil {
		return nil, readErr
	}
	if afterErr != nil {
		return nil, classifyRetentionAuthorityError(afterErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(raw)) != object.SizeBytes || int64(len(raw)) > maxRetentionMarkerBytes ||
		!before.Identity.Equal(object.Identity) || !after.Identity.Equal(object.Identity) ||
		before.SizeBytes != object.SizeBytes || after.SizeBytes != object.SizeBytes || !before.Modified.Equal(after.Modified) {
		return nil, fmt.Errorf("%w: retention marker changed during observation", ErrIntegrity)
	}
	if _, err := verifyRetentionNamedBytes(ctx, subtree, path, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func classifyRetentionAuthorityError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, fsbind.ErrBusy) || errors.Is(err, fsbind.ErrNotFound) {
		return err
	}
	if errors.Is(err, fsbind.ErrUnsafeObject) || errors.Is(err, fsbind.ErrBindingChanged) || errors.Is(err, fsbind.ErrCrossFilesystem) {
		return fmt.Errorf("%w: retention authority changed", ErrIntegrity)
	}
	return err
}
