package sourceretire

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const (
	parentCleanupJournalDirectory = "journal"
	parentCleanupScratchDirectory = "scratch"
	parentCleanupIntentFile       = "intent.json"
	parentCleanupCompleteFile     = "complete.json"
)

type parentCleanupJournalWrite struct {
	ScratchCreated      bool
	ScratchBytesWritten int64
	ScratchRemoved      bool
	Attempted           bool
	Published           bool
	Existing            bool
	Durability          string
	Ambiguous           bool
}

type parentCleanupJournalState struct {
	Intent          ParentCleanupIntent
	IntentID        string
	Attempts        []ParentCleanupAttempt
	AttemptIDs      []string
	AttemptPresent  []bool
	Removed         []ParentCleanupRemoved
	RemovedIDs      []string
	RemovedPresent  []bool
	Complete        ParentCleanupComplete
	CompleteID      string
	CompletePresent bool
}

type parentCleanupJournal struct {
	target  *fsbind.Session
	subtree *fsbind.Subtree
	limits  ParentCleanupExecutionLimits
	state   parentCleanupJournalState
}

func initializeParentCleanupJournal(ctx context.Context, target *fsbind.Session, intent ParentCleanupIntent) (*parentCleanupJournal, fsbind.Creation, fsbind.MkdirReceipt, parentCleanupJournalWrite, error) {
	var creation fsbind.Creation
	var directories fsbind.MkdirReceipt
	var marker parentCleanupJournalWrite
	if err := ctx.Err(); err != nil {
		return nil, creation, directories, marker, err
	}
	name, err := ParentCleanupOperationDirectoryName(intent.OperationID)
	if err != nil {
		return nil, creation, directories, marker, err
	}
	subtree, creation, err := target.CreatePrivateSubtreeWithReceipt(name)
	existing := false
	if errors.Is(err, fsbind.ErrAlreadyExists) {
		existing = true
		subtree, err = target.OpenPrivateSubtreeObserved(name)
	}
	if err != nil {
		return nil, creation, directories, marker, err
	}
	fail := func(err error) (*parentCleanupJournal, fsbind.Creation, fsbind.MkdirReceipt, parentCleanupJournalWrite, error) {
		_ = subtree.Close()
		return nil, creation, directories, marker, err
	}
	intent.OperationRootIdentity = subtree.Identity().String()
	if err := intent.Validate(); err != nil {
		return fail(err)
	}
	if existing {
		loaded, loadErr := loadParentCleanupJournalFromSubtree(ctx, target, subtree, intent.Limits)
		if loadErr != nil {
			return fail(loadErr)
		}
		_, expectedID, encodeErr := encodeParentCleanupIntent(intent)
		if encodeErr != nil || loaded.state.IntentID != expectedID {
			_ = loaded.subtree.Close()
			return nil, creation, directories, marker, fmt.Errorf("%w: existing parent-cleanup intent disagrees with the live review", ErrExecutionIntegrity)
		}
		return loaded, creation, directories, marker, nil
	}
	journalPath, _ := fsbind.PathFromComponents([]string{parentCleanupJournalDirectory})
	scratchPath, _ := fsbind.PathFromComponents([]string{parentCleanupScratchDirectory})
	for _, path := range []fsbind.Path{journalPath, scratchPath} {
		receipt, mkdirErr := subtree.MkdirAll(ctx, path)
		directories.DirectoriesCreated += receipt.DirectoriesCreated
		if receipt.Durability != "" {
			directories.Durability = receipt.Durability
		}
		if mkdirErr != nil && !errors.Is(mkdirErr, fsbind.ErrAlreadyExists) {
			return fail(mkdirErr)
		}
	}
	handle := &parentCleanupJournal{target: target, subtree: subtree, limits: intent.Limits}
	raw, expectedID, err := encodeParentCleanupIntent(intent)
	if err != nil {
		return fail(err)
	}
	intentPath, _ := fsbind.PathFromComponents([]string{parentCleanupJournalDirectory, parentCleanupIntentFile})
	marker, err = handle.commitMarker(ctx, "intent", intentPath, raw)
	if err != nil {
		return fail(err)
	}
	loaded, err := loadParentCleanupJournalFromSubtree(ctx, target, subtree, intent.Limits)
	if err != nil {
		return fail(err)
	}
	if loaded.state.IntentID != expectedID {
		_ = loaded.subtree.Close()
		return nil, creation, directories, marker, fmt.Errorf("%w: existing parent-cleanup intent disagrees with the live review", ErrExecutionIntegrity)
	}
	return loaded, creation, directories, marker, nil
}

func openParentCleanupJournal(ctx context.Context, target *fsbind.Session, operation ParentCleanupOperationID, limits ParentCleanupExecutionLimits) (*parentCleanupJournal, error) {
	name, err := ParentCleanupOperationDirectoryName(operation)
	if err != nil {
		return nil, err
	}
	subtree, err := target.OpenPrivateSubtreeObserved(name)
	if errors.Is(err, fsbind.ErrNotFound) {
		return nil, ErrOperationNotFound
	}
	if err != nil {
		return nil, err
	}
	journal, err := loadParentCleanupJournalFromSubtree(ctx, target, subtree, limits)
	if err != nil {
		_ = subtree.Close()
		return nil, err
	}
	if journal.state.Intent.OperationID != operation {
		_ = subtree.Close()
		return nil, fmt.Errorf("%w: parent-cleanup operation identity disagrees", ErrExecutionIntegrity)
	}
	return journal, nil
}

func loadParentCleanupJournalFromSubtree(ctx context.Context, target *fsbind.Session, subtree *fsbind.Subtree, limits ParentCleanupExecutionLimits) (*parentCleanupJournal, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	journalPath, _ := fsbind.PathFromComponents([]string{parentCleanupJournalDirectory})
	scratchPath, _ := fsbind.PathFromComponents([]string{parentCleanupScratchDirectory})
	root, err := subtree.List(ctx, fsbind.Path{}, fsbind.ListLimits{MaxEntries: 3, MaxNameBytes: 1 << 20})
	if err != nil {
		return nil, classifyExecutionBindingError(err)
	}
	allowed := map[string]string{".fsbind-operation.lock": "regular", parentCleanupJournalDirectory: "directory", parentCleanupScratchDirectory: "directory"}
	if !root.Complete || len(root.Entries) != len(allowed) {
		return nil, fmt.Errorf("%w: parent-cleanup operation namespace is incomplete", ErrExecutionIntegrity)
	}
	for _, entry := range root.Entries {
		if allowed[entry.Name] != entry.Kind {
			return nil, fmt.Errorf("%w: parent-cleanup operation namespace contains an unexpected object", ErrExecutionIntegrity)
		}
	}
	handle := &parentCleanupJournal{target: target, subtree: subtree, limits: limits}
	intentPath, _ := fsbind.PathFromComponents([]string{parentCleanupJournalDirectory, parentCleanupIntentFile})
	raw, _, err := handle.readRequiredMarker(ctx, intentPath, limits.MaxIntentBytes)
	if err != nil {
		return nil, err
	}
	intent, intentID, err := decodeParentCleanupIntent(bytes.NewReader(raw), limits)
	if err != nil || intent.OperationRootIdentity != subtree.Identity().String() || intent.TargetRootIdentity != target.Info().Identity.String() {
		return nil, fmt.Errorf("%w: parent-cleanup intent is bound to another filesystem object", ErrExecutionIntegrity)
	}
	handle.state = parentCleanupJournalState{Intent: intent, IntentID: intentID,
		Attempts: make([]ParentCleanupAttempt, len(intent.Directories)), AttemptIDs: make([]string, len(intent.Directories)), AttemptPresent: make([]bool, len(intent.Directories)),
		Removed: make([]ParentCleanupRemoved, len(intent.Directories)), RemovedIDs: make([]string, len(intent.Directories)), RemovedPresent: make([]bool, len(intent.Directories))}
	listing, err := subtree.List(ctx, journalPath, fsbind.ListLimits{MaxEntries: 2*limits.MaxParents + 2, MaxNameBytes: int64(2*limits.MaxParents+2) * 112})
	if err != nil {
		return nil, classifyExecutionBindingError(err)
	}
	if !listing.Complete {
		return nil, fmt.Errorf("%w: parent-cleanup journal inventory is incomplete", ErrExecutionIntegrity)
	}
	seen := make(map[string]struct{}, len(listing.Entries))
	for _, entry := range listing.Entries {
		if entry.Kind != string(fsbind.ObjectKindRegular) {
			return nil, fmt.Errorf("%w: parent-cleanup journal contains a non-regular marker", ErrExecutionIntegrity)
		}
		seen[entry.Name] = struct{}{}
	}
	delete(seen, parentCleanupIntentFile)
	for sequence, directory := range intent.Directories {
		attemptName := parentCleanupAttemptName(sequence)
		if _, present := seen[attemptName]; present {
			path, _ := fsbind.PathFromComponents([]string{parentCleanupJournalDirectory, attemptName})
			raw, _, readErr := handle.readRequiredMarker(ctx, path, limits.MaxMarkerBytes)
			if readErr != nil {
				return nil, readErr
			}
			attempt, id, decodeErr := decodeParentCleanupAttempt(bytes.NewReader(raw), limits)
			if decodeErr != nil || !parentCleanupAttemptMatches(attempt, intent, intentID, directory) {
				return nil, fmt.Errorf("%w: parent-cleanup attempt disagrees with its intent", ErrExecutionIntegrity)
			}
			handle.state.Attempts[sequence], handle.state.AttemptIDs[sequence], handle.state.AttemptPresent[sequence] = attempt, id, true
			delete(seen, attemptName)
		}
		removedName := parentCleanupRemovedName(sequence)
		if _, present := seen[removedName]; present {
			if !handle.state.AttemptPresent[sequence] {
				return nil, fmt.Errorf("%w: parent-cleanup removal exists without an attempt", ErrExecutionIntegrity)
			}
			path, _ := fsbind.PathFromComponents([]string{parentCleanupJournalDirectory, removedName})
			raw, _, readErr := handle.readRequiredMarker(ctx, path, limits.MaxMarkerBytes)
			if readErr != nil {
				return nil, readErr
			}
			removed, id, decodeErr := decodeParentCleanupRemoved(bytes.NewReader(raw), limits)
			if decodeErr != nil || !parentCleanupRemovedMatches(removed, intent, intentID, directory, handle.state.AttemptIDs[sequence]) {
				return nil, fmt.Errorf("%w: parent-cleanup removal disagrees with its attempt", ErrExecutionIntegrity)
			}
			handle.state.Removed[sequence], handle.state.RemovedIDs[sequence], handle.state.RemovedPresent[sequence] = removed, id, true
			delete(seen, removedName)
		}
	}
	if err := validateParentCleanupJournalOrder(handle.state); err != nil {
		return nil, err
	}
	if _, present := seen[parentCleanupCompleteFile]; present {
		for _, removed := range handle.state.RemovedPresent {
			if !removed {
				return nil, fmt.Errorf("%w: parent cleanup completed before every reviewed directory", ErrExecutionIntegrity)
			}
		}
		path, _ := fsbind.PathFromComponents([]string{parentCleanupJournalDirectory, parentCleanupCompleteFile})
		raw, _, readErr := handle.readRequiredMarker(ctx, path, limits.MaxMarkerBytes)
		if readErr != nil {
			return nil, readErr
		}
		complete, id, decodeErr := decodeParentCleanupComplete(bytes.NewReader(raw), limits)
		if decodeErr != nil || complete.OperationID != intent.OperationID || complete.IntentID != intentID || complete.ParentsRemoved != len(intent.Directories) {
			return nil, fmt.Errorf("%w: parent-cleanup completion disagrees with its intent", ErrExecutionIntegrity)
		}
		handle.state.Complete, handle.state.CompleteID, handle.state.CompletePresent = complete, id, true
		delete(seen, parentCleanupCompleteFile)
	}
	if len(seen) != 0 {
		return nil, fmt.Errorf("%w: parent-cleanup journal contains an unexpected marker", ErrExecutionIntegrity)
	}
	if err := handle.auditScratch(ctx); err != nil {
		return nil, err
	}
	if err := subtree.CheckPaths(journalPath, scratchPath); err != nil {
		return nil, classifyExecutionBindingError(err)
	}
	return handle, nil
}

func (journal *parentCleanupJournal) close() error {
	if journal == nil || journal.subtree == nil {
		return nil
	}
	return journal.subtree.Close()
}

func (journal *parentCleanupJournal) auditScratch(ctx context.Context) error {
	path, _ := fsbind.PathFromComponents([]string{parentCleanupScratchDirectory})
	listing, err := journal.subtree.List(ctx, path, fsbind.ListLimits{MaxEntries: 1, MaxNameBytes: journal.limits.MaxPathBytes})
	if err != nil {
		return classifyExecutionBindingError(err)
	}
	if !listing.Complete || len(listing.Entries) > 1 {
		return fmt.Errorf("%w: parent-cleanup scratch inventory is incomplete", ErrExecutionIntegrity)
	}
	if len(listing.Entries) == 1 {
		entry := listing.Entries[0]
		if entry.Kind != string(fsbind.ObjectKindRegular) || !stringsHasSuffix(entry.Name, ".pending") || len(entry.Name) > 160 {
			return fmt.Errorf("%w: parent-cleanup scratch contains an unexpected object", ErrExecutionIntegrity)
		}
		allowed, allowErr := journal.allowedScratchNames()
		if allowErr != nil {
			return allowErr
		}
		if _, present := allowed[entry.Name]; !present {
			return fmt.Errorf("%w: parent-cleanup scratch does not belong to the next journal transition", ErrExecutionIntegrity)
		}
	}
	return nil
}

func validateParentCleanupJournalOrder(state parentCleanupJournalState) error {
	incompleteSeen := false
	for sequence := range state.Intent.Directories {
		if state.RemovedPresent[sequence] {
			if incompleteSeen {
				return fmt.Errorf("%w: parent-cleanup removal markers are out of order", ErrExecutionIntegrity)
			}
			continue
		}
		if state.AttemptPresent[sequence] {
			if incompleteSeen {
				return fmt.Errorf("%w: parent-cleanup attempt markers are out of order", ErrExecutionIntegrity)
			}
			incompleteSeen = true
			continue
		}
		incompleteSeen = true
	}
	return nil
}

func (journal *parentCleanupJournal) allowedScratchNames() (map[string]struct{}, error) {
	allowed := map[string]struct{}{}
	if journal.state.CompletePresent {
		return allowed, nil
	}
	for sequence, directory := range journal.state.Intent.Directories {
		if journal.state.RemovedPresent[sequence] {
			continue
		}
		if !journal.state.AttemptPresent[sequence] {
			marker := ParentCleanupAttempt{Schema: ParentCleanupAttemptSchemaV1, OperationID: journal.state.Intent.OperationID,
				IntentID: journal.state.IntentID, Sequence: sequence, ParentPathRef: directory.ParentPathRef, ParentIdentity: directory.ParentIdentity}
			raw, _, err := encodeParentCleanupAttempt(marker, journal.limits)
			if err != nil {
				return nil, err
			}
			allowed[fmt.Sprintf("attempt-%06d-%s.pending", sequence, sha256Hex(raw))] = struct{}{}
			return allowed, nil
		}
		for _, basis := range []string{ParentCleanupRemovalBasisConfirmed, ParentCleanupRemovalBasisRecovered} {
			marker := ParentCleanupRemoved{Schema: ParentCleanupRemovedSchemaV1, OperationID: journal.state.Intent.OperationID,
				IntentID: journal.state.IntentID, AttemptID: journal.state.AttemptIDs[sequence], Sequence: sequence,
				ParentPathRef: directory.ParentPathRef, ParentIdentity: directory.ParentIdentity, Basis: basis}
			raw, _, err := encodeParentCleanupRemoved(marker, journal.limits)
			if err != nil {
				return nil, err
			}
			allowed[fmt.Sprintf("removed-%06d-%s.pending", sequence, sha256Hex(raw))] = struct{}{}
		}
		return allowed, nil
	}
	marker := ParentCleanupComplete{Schema: ParentCleanupCompleteSchemaV1, OperationID: journal.state.Intent.OperationID,
		IntentID: journal.state.IntentID, ParentsRemoved: len(journal.state.Intent.Directories)}
	raw, _, err := encodeParentCleanupComplete(marker, journal.limits)
	if err != nil {
		return nil, err
	}
	allowed["complete-"+sha256Hex(raw)+".pending"] = struct{}{}
	return allowed, nil
}

func (journal *parentCleanupJournal) commitMarker(ctx context.Context, purpose string, destination fsbind.Path, raw []byte) (parentCleanupJournalWrite, error) {
	receipt := parentCleanupJournalWrite{Durability: fsbind.DurabilityNotPublished}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if int64(len(raw)) <= 0 || int64(len(raw)) > journal.limits.MaxScratchBytes {
		return receipt, fmt.Errorf("%w: parent-cleanup marker exceeds its scratch budget", ErrExecutionPolicy)
	}
	if existing, _, err := journal.readNamedBytes(ctx, destination, int64(len(raw))); err == nil {
		if !bytes.Equal(existing, raw) {
			return receipt, fmt.Errorf("%w: existing parent-cleanup marker disagrees", ErrExecutionIntegrity)
		}
		if err := journal.confirmControlDurability(ctx); err != nil {
			return receipt, err
		}
		receipt.Existing, receipt.Durability = true, fsbind.DurabilityConfirmed
		return receipt, nil
	} else if !errors.Is(err, fsbind.ErrNotFound) {
		return receipt, err
	}
	digest := sha256Hex(raw)
	scratchName := purpose + "-" + digest + ".pending"
	scratch, err := fsbind.PathFromComponents([]string{parentCleanupScratchDirectory, scratchName})
	if err != nil {
		return receipt, err
	}
	scratchDirectory, _ := fsbind.PathFromComponents([]string{parentCleanupScratchDirectory})
	listing, err := journal.subtree.List(ctx, scratchDirectory, fsbind.ListLimits{MaxEntries: 1, MaxNameBytes: journal.limits.MaxPathBytes})
	if err != nil || !listing.Complete || len(listing.Entries) > 1 {
		if err == nil {
			err = fmt.Errorf("%w: parent-cleanup scratch inventory is incomplete", ErrExecutionIntegrity)
		}
		return receipt, err
	}
	if len(listing.Entries) == 1 {
		if listing.Entries[0].Name != scratchName || listing.Entries[0].Kind != string(fsbind.ObjectKindRegular) {
			return receipt, fmt.Errorf("%w: parent-cleanup scratch belongs to another marker", ErrExecutionIntegrity)
		}
		staged, object, readErr := journal.readNamedBytes(ctx, scratch, journal.limits.MaxScratchBytes)
		if readErr != nil {
			return receipt, readErr
		}
		if !bytes.Equal(staged, raw) {
			removal, removeErr := journal.subtree.RemoveRegularExact(ctx, scratch, object.Identity, object.SizeBytes)
			receipt.ScratchRemoved = removal.Removed
			if removeErr != nil || !removal.Removed || removal.Durability != fsbind.DurabilityConfirmed {
				if removeErr == nil {
					removeErr = fsbind.ErrRemovalAmbiguous
				}
				return receipt, removeErr
			}
			listing.Entries = nil
		}
	}
	if len(listing.Entries) == 0 {
		file, createErr := journal.subtree.CreateRegular(ctx, scratch)
		if createErr != nil {
			return receipt, createErr
		}
		receipt.ScratchCreated = true
		written, writeErr := writeExecutionBytes(ctx, file, raw)
		receipt.ScratchBytesWritten = written
		if writeErr == nil {
			writeErr = file.Sync()
		}
		info, infoErr := file.Info()
		closeErr := file.Close()
		if writeErr != nil {
			return receipt, writeErr
		}
		if infoErr != nil {
			return receipt, infoErr
		}
		if closeErr != nil {
			return receipt, closeErr
		}
		if info.SizeBytes != int64(len(raw)) || info.Identity.IsZero() {
			return receipt, fmt.Errorf("%w: parent-cleanup scratch changed", ErrExecutionIntegrity)
		}
	}
	publication, commitErr := journal.subtree.CommitRegularNoReplace(ctx, scratch, destination)
	receipt.Attempted, receipt.Published, receipt.Durability = publication.Attempted, publication.Published, publication.Durability
	receipt.Ambiguous = errors.Is(commitErr, fsbind.ErrPublicationAmbiguous) || publication.Attempted && !publication.Published && commitErr == nil
	if committed, _, verifyErr := journal.readNamedBytes(ctx, destination, int64(len(raw))); verifyErr == nil && bytes.Equal(committed, raw) {
		if syncErr := journal.confirmControlDurability(ctx); syncErr == nil {
			receipt.Published, receipt.Durability = true, fsbind.DurabilityConfirmed
			return receipt, nil
		}
	}
	if commitErr != nil {
		return receipt, commitErr
	}
	receipt.Ambiguous = true
	return receipt, fsbind.ErrPublicationAmbiguous
}

func (journal *parentCleanupJournal) readNamedBytes(ctx context.Context, path fsbind.Path, limit int64) ([]byte, fsbind.ObjectInfo, error) {
	object, err := journal.subtree.Inspect(ctx, path)
	if err != nil {
		if errors.Is(err, fsbind.ErrNotFound) {
			return nil, fsbind.ObjectInfo{}, err
		}
		return nil, fsbind.ObjectInfo{}, classifyExecutionBindingError(err)
	}
	if object.Kind != fsbind.ObjectKindRegular || object.Identity.IsZero() || object.SizeBytes <= 0 || object.SizeBytes > limit {
		return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: parent-cleanup marker object is unsafe", ErrExecutionIntegrity)
	}
	file, err := journal.subtree.OpenRegular(ctx, path)
	if err != nil {
		return nil, fsbind.ObjectInfo{}, classifyExecutionBindingError(err)
	}
	before, beforeErr := file.Info()
	raw, readErr := io.ReadAll(&executionExactReader{ctx: ctx, reader: file, remaining: limit + 1})
	after, afterErr := file.Info()
	closeErr := file.Close()
	if beforeErr != nil {
		return nil, fsbind.ObjectInfo{}, classifyExecutionBindingError(beforeErr)
	}
	if readErr != nil {
		return nil, fsbind.ObjectInfo{}, readErr
	}
	if afterErr != nil {
		return nil, fsbind.ObjectInfo{}, classifyExecutionBindingError(afterErr)
	}
	if closeErr != nil {
		return nil, fsbind.ObjectInfo{}, closeErr
	}
	if int64(len(raw)) != object.SizeBytes || int64(len(raw)) > limit || !before.Identity.Equal(object.Identity) || !after.Identity.Equal(object.Identity) ||
		before.SizeBytes != object.SizeBytes || after.SizeBytes != object.SizeBytes || !before.Modified.Equal(after.Modified) {
		return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: parent-cleanup marker changed during observation", ErrExecutionIntegrity)
	}
	reobserved, err := journal.subtree.Inspect(ctx, path)
	if err != nil {
		return nil, fsbind.ObjectInfo{}, classifyExecutionBindingError(err)
	}
	if !reobserved.Identity.Equal(object.Identity) || reobserved.SizeBytes != object.SizeBytes {
		return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: parent-cleanup marker name changed during observation", ErrExecutionIntegrity)
	}
	return raw, object, nil
}

func (journal *parentCleanupJournal) readRequiredMarker(ctx context.Context, path fsbind.Path, limit int64) ([]byte, fsbind.ObjectInfo, error) {
	raw, object, err := journal.readNamedBytes(ctx, path, limit)
	if errors.Is(err, fsbind.ErrNotFound) {
		return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: required parent-cleanup marker is missing", ErrExecutionIntegrity)
	}
	return raw, object, err
}

func (journal *parentCleanupJournal) confirmControlDurability(ctx context.Context) error {
	journalPath, _ := fsbind.PathFromComponents([]string{parentCleanupJournalDirectory})
	if err := journal.subtree.SyncDirectory(ctx, journalPath); err != nil {
		return classifyExecutionBindingError(err)
	}
	if err := journal.subtree.SyncDirectory(ctx, fsbind.Path{}); err != nil {
		return classifyExecutionBindingError(err)
	}
	return journal.subtree.CheckPaths(journalPath)
}

func (journal *parentCleanupJournal) publishAttempt(ctx context.Context, sequence int) (string, parentCleanupJournalWrite, error) {
	directory := journal.state.Intent.Directories[sequence]
	marker := ParentCleanupAttempt{Schema: ParentCleanupAttemptSchemaV1, OperationID: journal.state.Intent.OperationID,
		IntentID: journal.state.IntentID, Sequence: sequence, ParentPathRef: directory.ParentPathRef, ParentIdentity: directory.ParentIdentity}
	raw, id, err := encodeParentCleanupAttempt(marker, journal.limits)
	if err != nil {
		return "", parentCleanupJournalWrite{}, err
	}
	path, _ := fsbind.PathFromComponents([]string{parentCleanupJournalDirectory, parentCleanupAttemptName(sequence)})
	receipt, err := journal.commitMarker(ctx, fmt.Sprintf("attempt-%06d", sequence), path, raw)
	if err == nil {
		journal.state.Attempts[sequence], journal.state.AttemptIDs[sequence], journal.state.AttemptPresent[sequence] = marker, id, true
	}
	return id, receipt, err
}

func (journal *parentCleanupJournal) publishRemoved(ctx context.Context, sequence int, basis string) (string, parentCleanupJournalWrite, error) {
	directory := journal.state.Intent.Directories[sequence]
	marker := ParentCleanupRemoved{Schema: ParentCleanupRemovedSchemaV1, OperationID: journal.state.Intent.OperationID,
		IntentID: journal.state.IntentID, AttemptID: journal.state.AttemptIDs[sequence], Sequence: sequence,
		ParentPathRef: directory.ParentPathRef, ParentIdentity: directory.ParentIdentity, Basis: basis}
	raw, id, err := encodeParentCleanupRemoved(marker, journal.limits)
	if err != nil {
		return "", parentCleanupJournalWrite{}, err
	}
	path, _ := fsbind.PathFromComponents([]string{parentCleanupJournalDirectory, parentCleanupRemovedName(sequence)})
	receipt, err := journal.commitMarker(ctx, fmt.Sprintf("removed-%06d", sequence), path, raw)
	if err == nil {
		journal.state.Removed[sequence], journal.state.RemovedIDs[sequence], journal.state.RemovedPresent[sequence] = marker, id, true
	}
	return id, receipt, err
}

func (journal *parentCleanupJournal) publishComplete(ctx context.Context) (string, parentCleanupJournalWrite, error) {
	marker := ParentCleanupComplete{Schema: ParentCleanupCompleteSchemaV1, OperationID: journal.state.Intent.OperationID,
		IntentID: journal.state.IntentID, ParentsRemoved: len(journal.state.Intent.Directories)}
	raw, id, err := encodeParentCleanupComplete(marker, journal.limits)
	if err != nil {
		return "", parentCleanupJournalWrite{}, err
	}
	path, _ := fsbind.PathFromComponents([]string{parentCleanupJournalDirectory, parentCleanupCompleteFile})
	receipt, err := journal.commitMarker(ctx, "complete", path, raw)
	if err == nil {
		journal.state.Complete, journal.state.CompleteID, journal.state.CompletePresent = marker, id, true
	}
	return id, receipt, err
}

func parentCleanupAttemptMatches(marker ParentCleanupAttempt, intent ParentCleanupIntent, intentID string, directory ParentCleanupIntentDirectory) bool {
	return marker.OperationID == intent.OperationID && marker.IntentID == intentID && marker.Sequence == directory.Sequence &&
		marker.ParentPathRef == directory.ParentPathRef && marker.ParentIdentity == directory.ParentIdentity
}

func parentCleanupRemovedMatches(marker ParentCleanupRemoved, intent ParentCleanupIntent, intentID string, directory ParentCleanupIntentDirectory, attemptID string) bool {
	return marker.OperationID == intent.OperationID && marker.IntentID == intentID && marker.AttemptID == attemptID && marker.Sequence == directory.Sequence &&
		marker.ParentPathRef == directory.ParentPathRef && marker.ParentIdentity == directory.ParentIdentity
}

func parentCleanupAttemptName(sequence int) string { return fmt.Sprintf("attempt-%06d.json", sequence) }
func parentCleanupRemovedName(sequence int) string { return fmt.Sprintf("removed-%06d.json", sequence) }

// Kept local so journal parsing does not rely on platform path rules for a
// fixed ASCII protocol suffix.
func stringsHasSuffix(value, suffix string) bool {
	return len(value) >= len(suffix) && value[len(value)-len(suffix):] == suffix
}
