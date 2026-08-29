package sourceretire

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const (
	executionJournalDirectory      = "journal"
	executionScratchDirectory      = "scratch"
	executionIntentFile            = "intent.json"
	executionCompleteFile          = "complete.json"
	executionRetentionDirectory    = "retention"
	executionRetentionIntentFile   = "intent.json"
	executionRetentionCompleteFile = "complete.json"
)

type executionJournalWrite struct {
	ScratchCreated      bool
	ScratchBytesWritten int64
	ScratchRemoved      bool
	Attempted           bool
	Published           bool
	Existing            bool
	Durability          string
	Ambiguous           bool
}

type executionJournalState struct {
	Intent          ExecutionIntent
	IntentID        string
	Attempts        []DeleteAttempt
	AttemptIDs      []string
	AttemptPresent  []bool
	Deleted         []DeleteComplete
	DeletedIDs      []string
	DeletedPresent  []bool
	Complete        ExecutionComplete
	CompleteID      string
	CompletePresent bool
}

type executionJournal struct {
	target  *fsbind.Session
	subtree *fsbind.Subtree
	limits  ExecutionLimits
	state   executionJournalState
}

func initializeExecutionJournal(ctx context.Context, target *fsbind.Session, intent ExecutionIntent) (*executionJournal, fsbind.Creation, fsbind.MkdirReceipt, executionJournalWrite, error) {
	var creation fsbind.Creation
	var directories fsbind.MkdirReceipt
	var marker executionJournalWrite
	if err := ctx.Err(); err != nil {
		return nil, creation, directories, marker, err
	}
	name, err := OperationDirectoryName(intent.OperationID)
	if err != nil {
		return nil, creation, directories, marker, err
	}
	subtree, creation, err := target.CreatePrivateSubtreeWithReceipt(name)
	if errors.Is(err, fsbind.ErrAlreadyExists) {
		subtree, err = target.OpenPrivateSubtreeObserved(name)
	}
	if err != nil {
		return nil, creation, directories, marker, err
	}
	fail := func(err error) (*executionJournal, fsbind.Creation, fsbind.MkdirReceipt, executionJournalWrite, error) {
		_ = subtree.Close()
		return nil, creation, directories, marker, err
	}
	intent.OperationRootIdentity = subtree.Identity().String()
	if err := intent.Validate(); err != nil {
		return fail(err)
	}
	journalPath, _ := fsbind.PathFromComponents([]string{executionJournalDirectory})
	scratchPath, _ := fsbind.PathFromComponents([]string{executionScratchDirectory})
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
	handle := &executionJournal{target: target, subtree: subtree, limits: intent.Limits}
	intentRaw, intentID, err := encodeExecutionIntent(intent)
	if err != nil {
		return fail(err)
	}
	intentPath, _ := fsbind.PathFromComponents([]string{executionJournalDirectory, executionIntentFile})
	marker, err = handle.commitMarker(ctx, "intent", intentPath, intentRaw)
	if err != nil {
		return fail(err)
	}
	loaded, err := loadExecutionJournalFromSubtree(ctx, target, subtree, intent.Limits)
	if err != nil {
		return fail(err)
	}
	if loaded.state.IntentID != intentID {
		_ = loaded.subtree.Close()
		return nil, creation, directories, marker, fmt.Errorf("%w: existing source retirement intent disagrees with the live review", ErrExecutionIntegrity)
	}
	return loaded, creation, directories, marker, nil
}

func openExecutionJournal(ctx context.Context, target *fsbind.Session, operation OperationID, limits ExecutionLimits) (*executionJournal, error) {
	return openExecutionJournalMode(ctx, target, operation, limits, false)
}

func openExecutionJournalForRetention(ctx context.Context, target *fsbind.Session, operation OperationID, limits ExecutionLimits) (*executionJournal, error) {
	return openExecutionJournalMode(ctx, target, operation, limits, true)
}

func openExecutionJournalMode(ctx context.Context, target *fsbind.Session, operation OperationID, limits ExecutionLimits, allowRetention bool) (*executionJournal, error) {
	name, err := OperationDirectoryName(operation)
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
	journal, err := loadExecutionJournalFromSubtreeMode(ctx, target, subtree, limits, allowRetention)
	if err != nil {
		_ = subtree.Close()
		return nil, err
	}
	if journal.state.Intent.OperationID != operation {
		_ = subtree.Close()
		return nil, fmt.Errorf("%w: source retirement operation identity disagrees", ErrExecutionIntegrity)
	}
	return journal, nil
}

func loadExecutionJournalFromSubtree(ctx context.Context, target *fsbind.Session, subtree *fsbind.Subtree, limits ExecutionLimits) (*executionJournal, error) {
	return loadExecutionJournalFromSubtreeMode(ctx, target, subtree, limits, false)
}

func loadExecutionJournalFromSubtreeMode(ctx context.Context, target *fsbind.Session, subtree *fsbind.Subtree, limits ExecutionLimits, allowRetention bool) (*executionJournal, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	journalPath, _ := fsbind.PathFromComponents([]string{executionJournalDirectory})
	scratchPath, _ := fsbind.PathFromComponents([]string{executionScratchDirectory})
	root, err := subtree.List(ctx, fsbind.Path{}, fsbind.ListLimits{MaxEntries: 5, MaxNameBytes: 1 << 20})
	if err != nil {
		return nil, classifyExecutionBindingError(err)
	}
	allowedRoot := map[string]string{".fsbind-operation.lock": "regular", executionJournalDirectory: "directory", executionScratchDirectory: "directory"}
	if allowRetention {
		allowedRoot[executionRetentionDirectory] = "directory"
	}
	if !root.Complete || len(root.Entries) < 3 || len(root.Entries) > len(allowedRoot) {
		return nil, fmt.Errorf("%w: source retirement operation namespace is incomplete", ErrExecutionIntegrity)
	}
	seenRoot := make(map[string]bool, len(root.Entries))
	for _, entry := range root.Entries {
		if allowedRoot[entry.Name] != entry.Kind {
			return nil, fmt.Errorf("%w: source retirement operation namespace contains an unexpected object", ErrExecutionIntegrity)
		}
		seenRoot[entry.Name] = true
	}
	for _, required := range []string{".fsbind-operation.lock", executionJournalDirectory, executionScratchDirectory} {
		if !seenRoot[required] {
			return nil, fmt.Errorf("%w: source retirement operation namespace is incomplete", ErrExecutionIntegrity)
		}
	}
	handle := &executionJournal{target: target, subtree: subtree, limits: limits}
	intentPath, _ := fsbind.PathFromComponents([]string{executionJournalDirectory, executionIntentFile})
	raw, _, err := handle.readRequiredMarker(ctx, intentPath, limits.MaxIntentBytes)
	if err != nil {
		return nil, err
	}
	intent, intentID, err := decodeExecutionIntent(bytes.NewReader(raw), limits)
	if err != nil || intent.OperationRootIdentity != subtree.Identity().String() || intent.TargetRootIdentity != target.Info().Identity.String() {
		return nil, fmt.Errorf("%w: source retirement intent is bound to another filesystem object", ErrExecutionIntegrity)
	}
	handle.state = executionJournalState{Intent: intent, IntentID: intentID,
		Attempts: make([]DeleteAttempt, len(intent.Files)), AttemptIDs: make([]string, len(intent.Files)), AttemptPresent: make([]bool, len(intent.Files)),
		Deleted: make([]DeleteComplete, len(intent.Files)), DeletedIDs: make([]string, len(intent.Files)), DeletedPresent: make([]bool, len(intent.Files))}

	listing, err := subtree.List(ctx, journalPath, fsbind.ListLimits{MaxEntries: 2*limits.MaxFiles + 2, MaxNameBytes: int64(2*limits.MaxFiles+2) * 96})
	if err != nil {
		return nil, classifyExecutionBindingError(err)
	}
	if !listing.Complete {
		return nil, fmt.Errorf("%w: source retirement journal inventory is incomplete", ErrExecutionIntegrity)
	}
	seen := make(map[string]struct{}, len(listing.Entries))
	for _, entry := range listing.Entries {
		if entry.Kind != "regular" {
			return nil, fmt.Errorf("%w: source retirement journal contains a non-regular marker", ErrExecutionIntegrity)
		}
		seen[entry.Name] = struct{}{}
	}
	delete(seen, executionIntentFile)
	for sequence, file := range intent.Files {
		attemptName := executionAttemptName(sequence)
		if _, present := seen[attemptName]; present {
			path, _ := fsbind.PathFromComponents([]string{executionJournalDirectory, attemptName})
			raw, _, readErr := handle.readRequiredMarker(ctx, path, limits.MaxMarkerBytes)
			if readErr != nil {
				return nil, readErr
			}
			attempt, id, decodeErr := decodeDeleteAttempt(bytes.NewReader(raw), limits)
			if decodeErr != nil || !attemptMatchesIntent(attempt, intent, intentID, file) {
				return nil, fmt.Errorf("%w: source deletion attempt disagrees with its intent", ErrExecutionIntegrity)
			}
			handle.state.Attempts[sequence], handle.state.AttemptIDs[sequence], handle.state.AttemptPresent[sequence] = attempt, id, true
			delete(seen, attemptName)
		}
		completeName := executionDeletedName(sequence)
		if _, present := seen[completeName]; present {
			if !handle.state.AttemptPresent[sequence] {
				return nil, fmt.Errorf("%w: source deletion completion exists without an attempt", ErrExecutionIntegrity)
			}
			path, _ := fsbind.PathFromComponents([]string{executionJournalDirectory, completeName})
			raw, _, readErr := handle.readRequiredMarker(ctx, path, limits.MaxMarkerBytes)
			if readErr != nil {
				return nil, readErr
			}
			complete, id, decodeErr := decodeDeleteComplete(bytes.NewReader(raw), limits)
			if decodeErr != nil || !deleteCompleteMatchesIntent(complete, intent, intentID, file, handle.state.AttemptIDs[sequence]) {
				return nil, fmt.Errorf("%w: source deletion completion disagrees with its attempt", ErrExecutionIntegrity)
			}
			handle.state.Deleted[sequence], handle.state.DeletedIDs[sequence], handle.state.DeletedPresent[sequence] = complete, id, true
			delete(seen, completeName)
		}
	}
	if _, present := seen[executionCompleteFile]; present {
		for _, done := range handle.state.DeletedPresent {
			if !done {
				return nil, fmt.Errorf("%w: source retirement completed before every source name", ErrExecutionIntegrity)
			}
		}
		path, _ := fsbind.PathFromComponents([]string{executionJournalDirectory, executionCompleteFile})
		raw, _, readErr := handle.readRequiredMarker(ctx, path, limits.MaxMarkerBytes)
		if readErr != nil {
			return nil, readErr
		}
		complete, id, decodeErr := decodeExecutionComplete(bytes.NewReader(raw), limits)
		if decodeErr != nil || complete.OperationID != intent.OperationID || complete.IntentID != intentID || complete.FilesRetired != len(intent.Files) ||
			complete.BytesRetired != intent.ContentBytes || complete.FinalObjectIdentity != intent.FinalObjectIdentity || complete.CurrentClientUseID != intent.CurrentClientUseID {
			return nil, fmt.Errorf("%w: source retirement completion disagrees with its intent", ErrExecutionIntegrity)
		}
		handle.state.Complete, handle.state.CompleteID, handle.state.CompletePresent = complete, id, true
		delete(seen, executionCompleteFile)
	}
	if len(seen) != 0 {
		return nil, fmt.Errorf("%w: source retirement journal contains an unexpected marker", ErrExecutionIntegrity)
	}
	if err := handle.auditScratch(ctx); err != nil {
		return nil, err
	}
	if err := subtree.CheckPaths(journalPath, scratchPath); err != nil {
		return nil, classifyExecutionBindingError(err)
	}
	return handle, nil
}

func (journal *executionJournal) close() error {
	if journal == nil || journal.subtree == nil {
		return nil
	}
	return journal.subtree.Close()
}

func (journal *executionJournal) auditScratch(ctx context.Context) error {
	scratchPath, _ := fsbind.PathFromComponents([]string{executionScratchDirectory})
	listing, err := journal.subtree.List(ctx, scratchPath, fsbind.ListLimits{MaxEntries: 2, MaxNameBytes: journal.limits.MaxPathBytes})
	if err != nil {
		return classifyExecutionBindingError(err)
	}
	if !listing.Complete || len(listing.Entries) > 1 {
		return fmt.Errorf("%w: source retirement scratch inventory is incomplete", ErrExecutionIntegrity)
	}
	if len(listing.Entries) == 1 {
		entry := listing.Entries[0]
		if entry.Kind != "regular" || !strings.HasSuffix(entry.Name, ".pending") || len(entry.Name) > 160 {
			return fmt.Errorf("%w: source retirement scratch contains an unexpected object", ErrExecutionIntegrity)
		}
	}
	return nil
}

func (journal *executionJournal) commitMarker(ctx context.Context, purpose string, destination fsbind.Path, raw []byte) (executionJournalWrite, error) {
	receipt := executionJournalWrite{Durability: fsbind.DurabilityNotPublished}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if int64(len(raw)) <= 0 || int64(len(raw)) > max64(journal.limits.MaxIntentBytes, journal.limits.MaxMarkerBytes) {
		return receipt, fmt.Errorf("%w: source retirement marker exceeds its byte limit", ErrExecutionPolicy)
	}
	if existing, _, err := journal.readNamedBytes(ctx, destination, int64(len(raw))); err == nil {
		if !bytes.Equal(existing, raw) {
			return receipt, fmt.Errorf("%w: existing source retirement marker disagrees", ErrExecutionIntegrity)
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
	scratch, err := fsbind.PathFromComponents([]string{executionScratchDirectory, scratchName})
	if err != nil {
		return receipt, err
	}
	listingPath, _ := fsbind.PathFromComponents([]string{executionScratchDirectory})
	listing, err := journal.subtree.List(ctx, listingPath, fsbind.ListLimits{MaxEntries: 2, MaxNameBytes: journal.limits.MaxPathBytes})
	if err != nil || !listing.Complete || len(listing.Entries) > 1 {
		if err == nil {
			err = fmt.Errorf("%w: source retirement scratch inventory is incomplete", ErrExecutionIntegrity)
		}
		return receipt, err
	}
	if len(listing.Entries) == 1 {
		if listing.Entries[0].Name != scratchName || listing.Entries[0].Kind != "regular" {
			return receipt, fmt.Errorf("%w: source retirement scratch belongs to another marker", ErrExecutionIntegrity)
		}
		staged, object, readErr := journal.readNamedBytes(ctx, scratch, max64(journal.limits.MaxIntentBytes, journal.limits.MaxMarkerBytes))
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
			return receipt, fmt.Errorf("%w: source retirement scratch changed", ErrExecutionIntegrity)
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
	// A defensive no-error/no-proof result is still an uncertain publication;
	// the public write receipt must not describe it as a known zero/known state.
	receipt.Ambiguous = true
	return receipt, fsbind.ErrPublicationAmbiguous
}

func (journal *executionJournal) readNamedBytes(ctx context.Context, path fsbind.Path, limit int64) ([]byte, fsbind.ObjectInfo, error) {
	object, err := journal.subtree.Inspect(ctx, path)
	if err != nil {
		if errors.Is(err, fsbind.ErrNotFound) {
			return nil, fsbind.ObjectInfo{}, err
		}
		return nil, fsbind.ObjectInfo{}, classifyExecutionBindingError(err)
	}
	if object.Kind != fsbind.ObjectKindRegular || object.Identity.IsZero() || object.SizeBytes <= 0 || object.SizeBytes > limit {
		return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: source retirement marker object is unsafe", ErrExecutionIntegrity)
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
		return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: source retirement marker changed during observation", ErrExecutionIntegrity)
	}
	reobserved, err := journal.subtree.Inspect(ctx, path)
	if err != nil || !reobserved.Identity.Equal(object.Identity) || reobserved.SizeBytes != object.SizeBytes {
		return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: source retirement marker name changed during observation", ErrExecutionIntegrity)
	}
	return raw, object, nil
}

func (journal *executionJournal) readRequiredMarker(ctx context.Context, path fsbind.Path, limit int64) ([]byte, fsbind.ObjectInfo, error) {
	raw, object, err := journal.readNamedBytes(ctx, path, limit)
	if errors.Is(err, fsbind.ErrNotFound) {
		return nil, fsbind.ObjectInfo{}, fmt.Errorf("%w: required source retirement marker is missing", ErrExecutionIntegrity)
	}
	return raw, object, err
}

func (journal *executionJournal) confirmControlDurability(ctx context.Context) error {
	journalPath, _ := fsbind.PathFromComponents([]string{executionJournalDirectory})
	rootPath, _ := fsbind.PathFromComponents(nil)
	if err := journal.subtree.SyncDirectory(ctx, journalPath); err != nil {
		return classifyExecutionBindingError(err)
	}
	if err := journal.subtree.SyncDirectory(ctx, rootPath); err != nil {
		return classifyExecutionBindingError(err)
	}
	return journal.subtree.CheckPaths(journalPath)
}

func (journal *executionJournal) publishAttempt(ctx context.Context, sequence int) (string, executionJournalWrite, error) {
	file := journal.state.Intent.Files[sequence]
	marker := DeleteAttempt{Schema: DeleteAttemptSchemaV1, OperationID: journal.state.Intent.OperationID, IntentID: journal.state.IntentID,
		Sequence: sequence, ManifestIndex: file.ManifestIndex, SourcePathRef: file.SourcePathRef, FileIdentity: file.SourceObjectIdentity, SizeBytes: file.SizeBytes}
	raw, id, err := encodeDeleteAttempt(marker, journal.limits)
	if err != nil {
		return "", executionJournalWrite{}, err
	}
	path, _ := fsbind.PathFromComponents([]string{executionJournalDirectory, executionAttemptName(sequence)})
	receipt, err := journal.commitMarker(ctx, fmt.Sprintf("attempt-%06d", sequence), path, raw)
	if err == nil {
		journal.state.Attempts[sequence], journal.state.AttemptIDs[sequence], journal.state.AttemptPresent[sequence] = marker, id, true
	}
	return id, receipt, err
}

func (journal *executionJournal) publishDeleted(ctx context.Context, sequence int, basis string) (string, executionJournalWrite, error) {
	file := journal.state.Intent.Files[sequence]
	marker := DeleteComplete{Schema: DeleteCompleteSchemaV1, OperationID: journal.state.Intent.OperationID, IntentID: journal.state.IntentID,
		AttemptID: journal.state.AttemptIDs[sequence], Sequence: sequence, ManifestIndex: file.ManifestIndex, SourcePathRef: file.SourcePathRef,
		FileIdentity: file.SourceObjectIdentity, SizeBytes: file.SizeBytes, Basis: basis}
	raw, id, err := encodeDeleteComplete(marker, journal.limits)
	if err != nil {
		return "", executionJournalWrite{}, err
	}
	path, _ := fsbind.PathFromComponents([]string{executionJournalDirectory, executionDeletedName(sequence)})
	receipt, err := journal.commitMarker(ctx, fmt.Sprintf("deleted-%06d", sequence), path, raw)
	if err == nil {
		journal.state.Deleted[sequence], journal.state.DeletedIDs[sequence], journal.state.DeletedPresent[sequence] = marker, id, true
	}
	return id, receipt, err
}

func (journal *executionJournal) publishComplete(ctx context.Context, snapshotID string) (string, executionJournalWrite, error) {
	marker := ExecutionComplete{Schema: ExecutionCompleteSchemaV1, OperationID: journal.state.Intent.OperationID, IntentID: journal.state.IntentID,
		FilesRetired: len(journal.state.Intent.Files), BytesRetired: journal.state.Intent.ContentBytes,
		FinalObjectIdentity: journal.state.Intent.FinalObjectIdentity, CurrentClientUseID: journal.state.Intent.CurrentClientUseID, ClientSnapshotID: snapshotID}
	raw, id, err := encodeExecutionComplete(marker, journal.limits)
	if err != nil {
		return "", executionJournalWrite{}, err
	}
	path, _ := fsbind.PathFromComponents([]string{executionJournalDirectory, executionCompleteFile})
	receipt, err := journal.commitMarker(ctx, "complete", path, raw)
	if err == nil {
		journal.state.Complete, journal.state.CompleteID, journal.state.CompletePresent = marker, id, true
	}
	return id, receipt, err
}

func attemptMatchesIntent(marker DeleteAttempt, intent ExecutionIntent, intentID string, file IntentFile) bool {
	return marker.OperationID == intent.OperationID && marker.IntentID == intentID && marker.Sequence == file.Sequence && marker.ManifestIndex == file.ManifestIndex &&
		marker.SourcePathRef == file.SourcePathRef && marker.FileIdentity == file.SourceObjectIdentity && marker.SizeBytes == file.SizeBytes
}

func deleteCompleteMatchesIntent(marker DeleteComplete, intent ExecutionIntent, intentID string, file IntentFile, attemptID string) bool {
	return marker.OperationID == intent.OperationID && marker.IntentID == intentID && marker.AttemptID == attemptID && marker.Sequence == file.Sequence &&
		marker.ManifestIndex == file.ManifestIndex && marker.SourcePathRef == file.SourcePathRef && marker.FileIdentity == file.SourceObjectIdentity && marker.SizeBytes == file.SizeBytes
}

func executionAttemptName(sequence int) string { return fmt.Sprintf("attempt-%06d.json", sequence) }
func executionDeletedName(sequence int) string { return fmt.Sprintf("deleted-%06d.json", sequence) }

func sha256Hex(raw []byte) string {
	digest := sha256.Sum256(raw)
	return fmt.Sprintf("%x", digest[:])
}

func writeExecutionBytes(ctx context.Context, writer io.Writer, raw []byte) (int64, error) {
	var written int64
	for len(raw) > 0 {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		count, err := writer.Write(raw)
		written += int64(count)
		raw = raw[count:]
		if err != nil {
			return written, err
		}
		if count == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

type executionExactReader struct {
	ctx       context.Context
	reader    io.Reader
	remaining int64
}

func (reader *executionExactReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(buffer)) > reader.remaining {
		buffer = buffer[:reader.remaining]
	}
	if len(buffer) == 0 {
		return 0, io.EOF
	}
	count, err := reader.reader.Read(buffer)
	reader.remaining -= int64(count)
	return count, err
}

func classifyExecutionBindingError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, fsbind.ErrBusy) || errors.Is(err, ErrOperationNotFound) {
		return err
	}
	if errors.Is(err, fsbind.ErrNotFound) || errors.Is(err, fsbind.ErrUnsafeObject) || errors.Is(err, fsbind.ErrBindingChanged) || errors.Is(err, fsbind.ErrCrossFilesystem) {
		return fmt.Errorf("%w: source retirement filesystem authority changed", ErrExecutionIntegrity)
	}
	return err
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func sortedExecutionFileReports(state executionJournalState) []ExecutionFileReport {
	result := make([]ExecutionFileReport, len(state.Intent.Files))
	for index, file := range state.Intent.Files {
		status := "pending"
		if state.AttemptPresent[index] {
			status = "attempt_recorded"
		}
		if state.DeletedPresent[index] {
			status = "retired"
		}
		result[index] = ExecutionFileReport{Sequence: index, ManifestIndex: file.ManifestIndex, SourcePathRef: file.SourcePathRef,
			SizeBytes: file.SizeBytes, Status: status, AttemptID: state.AttemptIDs[index], CompletionID: state.DeletedIDs[index]}
		if state.DeletedPresent[index] {
			result[index].Basis = state.Deleted[index].Basis
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Sequence < result[j].Sequence })
	return result
}
