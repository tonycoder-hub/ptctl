package metastore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	// Record kinds are schema identifiers, not caller-defined labels. Adding a
	// kind is a store-format compatibility decision and must be explicit here.
	RecordKindStorageProfileV1         RecordKind = "storage.profile.v1"
	RecordKindStorageIndexDataV1       RecordKind = "storage.index.data.v1"
	RecordKindStorageIndexDescriptorV1 RecordKind = "storage.index.descriptor.v1"
	RecordKindSiteMetafileBindingV1    RecordKind = "site.metafile.binding.v1"

	defaultMaxRecordBytes = int64(64 << 20)
	hardMaxRecordBytes    = int64(64 << 20)
	defaultMaxEntries     = 25_000
	hardMaxEntries        = 100_000
	defaultMaxRecords     = 10_000
	hardMaxRecords        = 50_000
	defaultMaxPathBytes   = int64(16 << 20)
	hardMaxPathBytes      = int64(64 << 20)

	recordDigestDomain = "ptctl-sealed-record-v1\x00"
	recordPrefix       = "record-"
	recordSuffix       = ".sealed"

	recordImportEffect = "write_private_sealed_record"
	recordLoadEffect   = "read_private_sealed_record"
	recordVerifyEffect = "verify_private_sealed_record_set"
	hardMaxRecordSet   = 16
)

var (
	// ErrCorruptRecord identifies a named sealed record that is unsafe, changed
	// during use, or does not match its domain-separated content digest.
	ErrCorruptRecord = errors.New("sealed record is corrupt")

	// ErrRecordNotFound identifies an absent (kind, ID) pair without exposing
	// its private store path.
	ErrRecordNotFound = errors.New("sealed record was not found")

	// ErrRecordConsumerIncomplete means the callback returned without itself
	// observing EOF. The store may still drain and verify the record, but the
	// callback did not prove that it consumed the complete value.
	ErrRecordConsumerIncomplete = errors.New("sealed record consumer did not reach EOF")

	errRecordByteLimit = errors.New("sealed record byte limit exceeded")
)

// RecordKind is an allowlisted record schema. It is deliberately not an open
// string namespace: an unknown schema cannot be persisted accidentally.
type RecordKind string

func ParseRecordKind(value string) (RecordKind, error) {
	switch RecordKind(value) {
	case RecordKindStorageProfileV1,
		RecordKindStorageIndexDataV1,
		RecordKindStorageIndexDescriptorV1,
		RecordKindSiteMetafileBindingV1:
		return RecordKind(value), nil
	default:
		return "", fmt.Errorf("sealed record kind is invalid")
	}
}

// RecordID is domain-separated from ArtifactID. The digest covers the record
// domain, its allowlisted kind, and every raw payload byte.
type RecordID string

func ParseRecordID(value string) (RecordID, error) {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return "", fmt.Errorf("sealed record ID is invalid")
	}
	digest := value[len("sha256:"):]
	if strings.ToLower(digest) != digest {
		return "", fmt.Errorf("sealed record ID is invalid")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("sealed record ID is invalid")
	}
	return RecordID(value), nil
}

func (id RecordID) String() string { return string(id) }

// RecordRef is a sealed-record locator, not a torrent ArtifactRef. ListRecords
// returns observed locators; LoadRecord is the operation that verifies bytes.
type RecordRef struct {
	Kind      RecordKind `json:"kind"`
	ID        RecordID   `json:"record_id"`
	SizeBytes int64      `json:"size_bytes"`
}

type RecordLimits struct {
	MaxRecordBytes int64 `json:"max_record_bytes"`
	MaxEntries     int   `json:"max_entries"`
	MaxRecords     int   `json:"max_records"`
	MaxPathBytes   int64 `json:"max_path_bytes"`
}

func DefaultRecordLimits() RecordLimits {
	return RecordLimits{
		MaxRecordBytes: defaultMaxRecordBytes,
		MaxEntries:     defaultMaxEntries,
		MaxRecords:     defaultMaxRecords,
		MaxPathBytes:   defaultMaxPathBytes,
	}
}

func (limits RecordLimits) Validate() error {
	if limits.MaxRecordBytes <= 0 || limits.MaxRecordBytes > hardMaxRecordBytes {
		return fmt.Errorf("maximum sealed record bytes must be in 1..%d", hardMaxRecordBytes)
	}
	if limits.MaxEntries <= 0 || limits.MaxEntries > hardMaxEntries {
		return fmt.Errorf("maximum sealed record entries must be in 1..%d", hardMaxEntries)
	}
	if limits.MaxRecords <= 0 || limits.MaxRecords > hardMaxRecords {
		return fmt.Errorf("maximum sealed records must be in 1..%d", hardMaxRecords)
	}
	if limits.MaxPathBytes <= 0 || limits.MaxPathBytes > hardMaxPathBytes {
		return fmt.Errorf("maximum sealed record path bytes must be in 1..%d", hardMaxPathBytes)
	}
	return nil
}

type RecordImportReceipt struct {
	Effect          string    `json:"effect"`
	WritesPerformed int       `json:"writes_performed"`
	AlreadyPresent  bool      `json:"already_present"`
	BytesConsumed   int64     `json:"bytes_consumed"`
	Store           StoreInfo `json:"store"`
}

type RecordLoadReceipt struct {
	Effect            string    `json:"effect"`
	Complete          bool      `json:"complete"`
	RecordBytesRead   int64     `json:"record_bytes_read"`
	ConsumerBytesRead int64     `json:"consumer_bytes_read"`
	RecordBytesKnown  bool      `json:"record_bytes_known"`
	Store             StoreInfo `json:"store"`
}

type RecordSetVerificationReceipt struct {
	Effect          string    `json:"effect"`
	Complete        bool      `json:"complete"`
	RecordsVerified int       `json:"records_verified"`
	BytesRead       int64     `json:"bytes_read"`
	Store           StoreInfo `json:"store"`
}

type RecordListUsage struct {
	EntriesConsidered int   `json:"entries_considered"`
	RecordsMatched    int   `json:"records_matched"`
	PathBytes         int64 `json:"path_bytes"`
}

type RecordListResult struct {
	Kind       RecordKind      `json:"kind"`
	Complete   bool            `json:"complete"`
	Limits     RecordLimits    `json:"limits"`
	Used       RecordListUsage `json:"used"`
	Records    []RecordRef     `json:"records"`
	StopReason string          `json:"stop_reason,omitempty"`
}

type RecordConsumer func(io.Reader) error

// ImportRecord stores the exact raw payload under its allowlisted schema. It
// streams through a private staging file and never buffers the complete value.
func (s *Store) ImportRecord(ctx context.Context, kind RecordKind, reader io.Reader, limits RecordLimits) (RecordRef, RecordImportReceipt, error) {
	receipt := RecordImportReceipt{Effect: recordImportEffect, Store: s.Info()}
	parsedKind, err := ParseRecordKind(string(kind))
	if s == nil || reader == nil {
		return RecordRef{}, receipt, fmt.Errorf("import sealed record: input is unavailable")
	}
	if err != nil || parsedKind != kind {
		return RecordRef{}, receipt, fmt.Errorf("import sealed record: kind is invalid")
	}
	if err := limits.Validate(); err != nil {
		return RecordRef{}, receipt, err
	}
	if err := ctx.Err(); err != nil {
		return RecordRef{}, receipt, err
	}
	session, err := s.validatedSession()
	if err != nil {
		return RecordRef{}, receipt, safeError("import sealed record", err)
	}
	defer session.Close()

	temporaryName, err := randomRelativeName(temporaryDir, ".record-import-", recordSuffix)
	if err != nil {
		return RecordRef{}, receipt, fmt.Errorf("import sealed record: random staging name unavailable")
	}
	file, err := session.createPrivateFile(temporaryName)
	if err != nil {
		return RecordRef{}, receipt, fmt.Errorf("import sealed record: create staging object failed")
	}
	stagingPresent := true
	defer func() {
		if stagingPresent {
			_ = session.removePrivate(temporaryName)
		}
	}()
	// This defer is registered after staging cleanup so it runs first if a
	// caller-provided Reader panics. That ordering also avoids retaining a
	// deletion-blocking Windows handle.
	defer file.Close()

	bytesConsumed, digest, err := copySealedRecord(ctx, file, reader, kind, limits.MaxRecordBytes)
	receipt.BytesConsumed = bytesConsumed
	if err != nil {
		_ = file.Close()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return RecordRef{}, receipt, err
		}
		if errors.Is(err, errRecordByteLimit) {
			return RecordRef{}, receipt, fmt.Errorf("import sealed record: byte limit exceeded")
		}
		return RecordRef{}, receipt, fmt.Errorf("import sealed record: read input failed")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return RecordRef{}, receipt, fmt.Errorf("import sealed record: flush staging object failed")
	}
	if err := file.Close(); err != nil {
		return RecordRef{}, receipt, fmt.Errorf("import sealed record: close staging object failed")
	}

	id := recordIDFromDigest(digest)
	ref := RecordRef{Kind: kind, ID: id, SizeBytes: bytesConsumed}
	verifiedSize, err := verifyRecordRelative(ctx, session, temporaryName, kind, id, limits.MaxRecordBytes)
	if err != nil || verifiedSize != bytesConsumed {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return RecordRef{}, receipt, err
		}
		return RecordRef{}, receipt, fmt.Errorf("import sealed record: staging verification failed")
	}

	objectName := recordRelativePath(kind, id)
	if existingSize, existingErr := verifyRecordRelative(ctx, session, objectName, kind, id, limits.MaxRecordBytes); existingErr == nil {
		if existingSize != bytesConsumed {
			return RecordRef{}, receipt, fmt.Errorf("%w: existing object size disagrees", ErrCorruptRecord)
		}
		if err := session.check("before_success"); err != nil {
			return RecordRef{}, receipt, fmt.Errorf("import sealed record: bound identity changed")
		}
		receipt.AlreadyPresent = true
		return ref, receipt, nil
	} else if !errors.Is(existingErr, ErrRecordNotFound) {
		if errors.Is(existingErr, context.Canceled) || errors.Is(existingErr, context.DeadlineExceeded) || errors.Is(existingErr, ErrCorruptRecord) {
			return RecordRef{}, receipt, existingErr
		}
		return RecordRef{}, receipt, fmt.Errorf("import sealed record: existing object verification failed")
	}

	published, publishErr := session.publishNoReplace(temporaryName, objectName)
	if published {
		receipt.WritesPerformed = 1
		stagingPresent = false
	}
	if publishErr != nil {
		if published {
			verificationContext := context.WithoutCancel(ctx)
			_, _ = verifyRecordRelative(verificationContext, session, objectName, kind, id, limits.MaxRecordBytes)
			_ = session.check("before_success")
			return ref, receipt, publishedError(publishErr)
		}
		return RecordRef{}, receipt, fmt.Errorf("import sealed record: atomic commit failed")
	}

	verificationContext := context.WithoutCancel(ctx)
	committedSize, err := verifyRecordRelative(verificationContext, session, objectName, kind, id, limits.MaxRecordBytes)
	if err != nil || committedSize != bytesConsumed {
		if published {
			return ref, receipt, fmt.Errorf("%w: committed object could not be verified", ErrCorruptRecord)
		}
		return RecordRef{}, receipt, fmt.Errorf("%w: concurrent object could not be verified", ErrCorruptRecord)
	}
	if err := session.check("before_success"); err != nil {
		if published {
			return ref, receipt, fmt.Errorf("private store publication completed but bound identity validation failed")
		}
		return RecordRef{}, receipt, fmt.Errorf("import sealed record: bound identity changed")
	}
	if !published {
		receipt.AlreadyPresent = true
	}
	return ref, receipt, nil
}

// LoadRecord verifies the complete stored record while exposing it only to a
// synchronous reader callback. Returning from consume without observing EOF is
// an error even though the store drains the remainder to verify the digest.
func (s *Store) LoadRecord(ctx context.Context, kind RecordKind, id RecordID, limits RecordLimits, consume RecordConsumer) (RecordRef, RecordLoadReceipt, error) {
	receipt := RecordLoadReceipt{Effect: recordLoadEffect, Store: s.Info()}
	parsedKind, kindErr := ParseRecordKind(string(kind))
	parsedID, idErr := ParseRecordID(id.String())
	if s == nil || consume == nil {
		return RecordRef{}, receipt, fmt.Errorf("load sealed record: consumer is unavailable")
	}
	if kindErr != nil || parsedKind != kind || idErr != nil || parsedID != id {
		return RecordRef{}, receipt, fmt.Errorf("load sealed record: identity is invalid")
	}
	if err := limits.Validate(); err != nil {
		return RecordRef{}, receipt, err
	}
	if err := ctx.Err(); err != nil {
		return RecordRef{}, receipt, err
	}
	session, err := s.validatedSession()
	if err != nil {
		return RecordRef{}, receipt, safeError("load sealed record", err)
	}
	defer session.Close()

	relative := recordRelativePath(kind, id)
	file, before, err := session.openValidated(relative, false)
	if err != nil {
		if errors.Is(err, errArtifactNotFound) {
			return RecordRef{}, receipt, ErrRecordNotFound
		}
		return RecordRef{}, receipt, fmt.Errorf("%w: object is unsafe", ErrCorruptRecord)
	}
	defer file.Close()
	if before.Size() < 0 || before.Size() > limits.MaxRecordBytes {
		_ = file.Close()
		return RecordRef{}, receipt, fmt.Errorf("load sealed record: byte limit exceeded")
	}
	receipt.RecordBytesKnown = true
	digestReader := newRecordDigestReader(ctx, file, kind, limits.MaxRecordBytes)
	consumerReader := &recordConsumerReader{source: digestReader, active: true}
	consumerErr := consume(consumerReader)
	consumerBytes, consumerEOF := consumerReader.deactivate()
	receipt.ConsumerBytesRead = consumerBytes

	drainErr := drainRecordReader(digestReader)
	receipt.RecordBytesRead = digestReader.total
	after, statErr := file.Stat()
	closeErr := file.Close()
	reopened, namedAfter, pathErr := session.openValidated(relative, false)
	if reopened != nil {
		_ = reopened.Close()
	}
	stable := statErr == nil && closeErr == nil && pathErr == nil &&
		os.SameFile(before, after) && os.SameFile(after, namedAfter) &&
		after.Size() == before.Size() && namedAfter.Size() == after.Size() &&
		after.ModTime().Equal(before.ModTime()) && namedAfter.ModTime().Equal(after.ModTime())
	actualID := recordIDFromDigest(digestReader.sum())
	if drainErr != nil {
		if errors.Is(drainErr, context.Canceled) || errors.Is(drainErr, context.DeadlineExceeded) {
			return RecordRef{}, receipt, drainErr
		}
		return RecordRef{}, receipt, fmt.Errorf("%w: object read failed", ErrCorruptRecord)
	}
	if !stable || digestReader.total != before.Size() || actualID != id {
		return RecordRef{}, receipt, fmt.Errorf("%w: object identity disagrees", ErrCorruptRecord)
	}
	if err := session.check("before_success"); err != nil {
		return RecordRef{}, receipt, fmt.Errorf("%w: bound identity changed", ErrCorruptRecord)
	}
	ref := RecordRef{Kind: kind, ID: id, SizeBytes: before.Size()}
	if consumerErr != nil {
		return ref, receipt, fmt.Errorf("load sealed record: consumer failed")
	}
	if !consumerEOF {
		return ref, receipt, fmt.Errorf("%w", ErrRecordConsumerIncomplete)
	}
	receipt.Complete = true
	return ref, receipt, nil
}

// VerifyRecordSet proves that a small set of immutable records coexist under
// one operation-bound physical store root. It verifies every domain-separated
// digest and named identity without exposing payload bytes. This is used for
// descriptor/data publication protocols which must not combine observations
// from two replaceable store roots.
func (s *Store) VerifyRecordSet(ctx context.Context, records []RecordRef, limits RecordLimits) (RecordSetVerificationReceipt, error) {
	receipt := RecordSetVerificationReceipt{Effect: recordVerifyEffect, Store: s.Info()}
	if s == nil || len(records) == 0 || len(records) > hardMaxRecordSet {
		return receipt, fmt.Errorf("verify sealed record set: record count is invalid")
	}
	if err := limits.Validate(); err != nil {
		return receipt, err
	}
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		kind, kindErr := ParseRecordKind(string(record.Kind))
		id, idErr := ParseRecordID(record.ID.String())
		key := string(record.Kind) + "\x00" + record.ID.String()
		if kindErr != nil || kind != record.Kind || idErr != nil || id != record.ID || record.SizeBytes < 0 {
			return receipt, fmt.Errorf("verify sealed record set: record identity is invalid")
		}
		if _, exists := seen[key]; exists {
			return receipt, fmt.Errorf("verify sealed record set: record identity is duplicated")
		}
		seen[key] = struct{}{}
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	session, err := s.validatedSession()
	if err != nil {
		return receipt, safeError("verify sealed record set", err)
	}
	defer session.Close()
	for _, record := range records {
		size, verifyErr := verifyRecordRelative(ctx, session, recordRelativePath(record.Kind, record.ID), record.Kind, record.ID, limits.MaxRecordBytes)
		if verifyErr != nil || record.SizeBytes != 0 && size != record.SizeBytes {
			if errors.Is(verifyErr, context.Canceled) || errors.Is(verifyErr, context.DeadlineExceeded) {
				return receipt, verifyErr
			}
			return receipt, fmt.Errorf("verify sealed record set: record verification failed")
		}
		receipt.RecordsVerified++
		receipt.BytesRead += size
	}
	if err := session.check("before_success"); err != nil {
		return receipt, fmt.Errorf("verify sealed record set: bound identity changed")
	}
	receipt.Complete = true
	return receipt, nil
}

// ListRecords enumerates observed record locators for one allowlisted kind.
// It never treats a filename as verified content; callers use LoadRecord for
// that. Every directory and result budget has an explicit N+1 stop.
func (s *Store) ListRecords(ctx context.Context, kind RecordKind, limits RecordLimits) (RecordListResult, error) {
	result := RecordListResult{Limits: limits, Records: []RecordRef{}}
	parsedKind, err := ParseRecordKind(string(kind))
	if s == nil {
		return result, fmt.Errorf("list sealed records: store is unavailable")
	}
	if err != nil || parsedKind != kind {
		return result, fmt.Errorf("list sealed records: kind is invalid")
	}
	result.Kind = kind
	if err := limits.Validate(); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		result.StopReason = "context_cancelled"
		return result, err
	}
	session, err := s.validatedSession()
	if err != nil {
		return result, safeError("list sealed records", err)
	}
	defer session.Close()

	directory, before, err := session.openValidated(objectsDir, true)
	if err != nil {
		return result, fmt.Errorf("list sealed records failed")
	}
	entries, overflow, readErr := readDirectoryPrefix(directory, limits.MaxEntries)
	after, statErr := directory.Stat()
	closeErr := directory.Close()
	reopened, namedAfter, pathErr := session.openValidated(objectsDir, true)
	if reopened != nil {
		_ = reopened.Close()
	}
	if readErr != nil || statErr != nil || closeErr != nil || pathErr != nil ||
		!os.SameFile(before, after) || !os.SameFile(after, namedAfter) ||
		after.Size() != before.Size() || namedAfter.Size() != after.Size() ||
		!after.ModTime().Equal(before.ModTime()) || !namedAfter.ModTime().Equal(after.ModTime()) {
		return result, fmt.Errorf("list sealed records failed")
	}
	if overflow {
		result.Used.EntriesConsidered = len(entries) + 1
		result.StopReason = "entry_limit"
		if err := verifyRecordListBinding(session, after); err != nil {
			return result, fmt.Errorf("list sealed records failed")
		}
		return result, nil
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			result.StopReason = "context_cancelled"
			return result, err
		}
		result.Used.EntriesConsidered++
		pathBytes := int64(len(objectsDir) + 1 + len(entry.Name()))
		if pathBytes > limits.MaxPathBytes-result.Used.PathBytes {
			result.StopReason = "path_limit"
			if err := verifyRecordListBinding(session, after); err != nil {
				return result, fmt.Errorf("list sealed records failed")
			}
			return result, nil
		}
		result.Used.PathBytes += pathBytes

		entryKind, entryID, isRecord, nameErr := classifyObjectName(entry.Name())
		if nameErr != nil {
			return result, fmt.Errorf("%w: objects directory contains an unknown entry", ErrCorruptRecord)
		}
		file, info, openErr := session.openValidated(filepath.Join(objectsDir, entry.Name()), false)
		if openErr != nil {
			return result, fmt.Errorf("%w: objects directory entry is unsafe", ErrCorruptRecord)
		}
		closeErr := file.Close()
		// ReadDir entries produced from the POSIX operation-bound directory
		// handle carry the handle's deliberately opaque os.File name. Calling
		// entry.Info would therefore perform a path lookup through that opaque
		// name rather than through the bound store root. openValidated already
		// performs the required no-follow named lookup, opens the same object,
		// and proves named/handle identity and type under the root session.
		if closeErr != nil {
			return result, fmt.Errorf("%w: objects directory entry is unsafe", ErrCorruptRecord)
		}
		if isRecord && (info.Size() < 0 || info.Size() > hardMaxRecordBytes) {
			return result, fmt.Errorf("%w: sealed record size is unsafe", ErrCorruptRecord)
		}
		if !isRecord || entryKind != kind {
			continue
		}
		result.Used.RecordsMatched++
		if result.Used.RecordsMatched > limits.MaxRecords {
			result.StopReason = "record_limit"
			if err := verifyRecordListBinding(session, after); err != nil {
				return result, fmt.Errorf("list sealed records failed")
			}
			return result, nil
		}
		result.Records = append(result.Records, RecordRef{Kind: entryKind, ID: entryID, SizeBytes: info.Size()})
	}
	if err := verifyRecordListBinding(session, after); err != nil {
		return result, fmt.Errorf("list sealed records failed")
	}
	sort.Slice(result.Records, func(i, j int) bool { return result.Records[i].ID < result.Records[j].ID })
	result.Complete = true
	return result, nil
}

func verifyRecordListBinding(session *rootSession, baseline os.FileInfo) error {
	directory, current, err := session.openValidated(objectsDir, true)
	if err != nil {
		return fmt.Errorf("objects directory is unavailable")
	}
	closeErr := directory.Close()
	if closeErr != nil || !os.SameFile(baseline, current) || baseline.Size() != current.Size() || !baseline.ModTime().Equal(current.ModTime()) {
		return fmt.Errorf("objects directory changed during enumeration")
	}
	return session.check("before_success")
}

func recordRelativePath(kind RecordKind, id RecordID) string {
	digest := strings.TrimPrefix(id.String(), "sha256:")
	return filepath.Join(objectsDir, recordPrefix+string(kind)+"-"+digest+recordSuffix)
}

func classifyObjectName(name string) (RecordKind, RecordID, bool, error) {
	if isArtifactObjectName(name) {
		return "", "", false, nil
	}
	for _, kind := range allRecordKinds() {
		prefix := recordPrefix + string(kind) + "-"
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		digest := strings.TrimSuffix(strings.TrimPrefix(name, prefix), recordSuffix)
		if len(digest) != 64 || name != prefix+digest+recordSuffix {
			return "", "", false, fmt.Errorf("unknown sealed record object name")
		}
		id, err := ParseRecordID("sha256:" + digest)
		if err != nil {
			return "", "", false, fmt.Errorf("unknown sealed record object name")
		}
		return kind, id, true, nil
	}
	return "", "", false, fmt.Errorf("unknown private store object name")
}

func isArtifactObjectName(name string) bool {
	if len(name) != 64+len(".torrent") || !strings.HasSuffix(name, ".torrent") {
		return false
	}
	digest := strings.TrimSuffix(name, ".torrent")
	if strings.ToLower(digest) != digest {
		return false
	}
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size
}

func allRecordKinds() []RecordKind {
	return []RecordKind{
		RecordKindStorageProfileV1,
		RecordKindStorageIndexDataV1,
		RecordKindStorageIndexDescriptorV1,
		RecordKindSiteMetafileBindingV1,
	}
}

func newRecordHasher(kind RecordKind) hash.Hash {
	hasher := sha256.New()
	_, _ = io.WriteString(hasher, recordDigestDomain)
	_, _ = io.WriteString(hasher, string(kind))
	_, _ = io.WriteString(hasher, "\x00")
	return hasher
}

func recordIDFromDigest(digest [sha256.Size]byte) RecordID {
	return RecordID("sha256:" + hex.EncodeToString(digest[:]))
}

func copySealedRecord(ctx context.Context, destination io.Writer, source io.Reader, kind RecordKind, maximum int64) (int64, [sha256.Size]byte, error) {
	hasher := newRecordHasher(kind)
	buffer := make([]byte, 32<<10)
	var total int64
	zeroReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, [sha256.Size]byte{}, err
		}
		readLength := len(buffer)
		remaining := maximum - total
		if remaining < int64(readLength-1) {
			readLength = int(remaining + 1)
		}
		n, readErr := source.Read(buffer[:readLength])
		if n < 0 || n > readLength {
			return total, [sha256.Size]byte{}, fmt.Errorf("input reader returned an invalid byte count")
		}
		if n > 0 {
			zeroReads = 0
			if int64(n) > maximum-total {
				return total + int64(n), [sha256.Size]byte{}, errRecordByteLimit
			}
			written, writeErr := destination.Write(buffer[:n])
			if writeErr != nil || written != n {
				return total + int64(n), [sha256.Size]byte{}, fmt.Errorf("write staging object failed")
			}
			_, _ = hasher.Write(buffer[:n])
			total += int64(n)
		} else if readErr == nil {
			zeroReads++
			if zeroReads > 100 {
				return total, [sha256.Size]byte{}, fmt.Errorf("input reader made no progress")
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				var digest [sha256.Size]byte
				copy(digest[:], hasher.Sum(nil))
				return total, digest, nil
			}
			return total, [sha256.Size]byte{}, fmt.Errorf("read input failed")
		}
	}
}

func verifyRecordRelative(ctx context.Context, session *rootSession, relative string, kind RecordKind, expected RecordID, maximum int64) (int64, error) {
	file, before, err := session.openValidated(relative, false)
	if err != nil {
		if errors.Is(err, errArtifactNotFound) {
			return 0, ErrRecordNotFound
		}
		return 0, fmt.Errorf("%w: object is unsafe", ErrCorruptRecord)
	}
	if before.Size() < 0 || before.Size() > maximum {
		_ = file.Close()
		return 0, fmt.Errorf("%w: object exceeds its byte limit", ErrCorruptRecord)
	}
	reader := newRecordDigestReader(ctx, file, kind, maximum)
	readErr := drainRecordReader(reader)
	after, statErr := file.Stat()
	closeErr := file.Close()
	reopened, namedAfter, pathErr := session.openValidated(relative, false)
	if reopened != nil {
		_ = reopened.Close()
	}
	if readErr != nil {
		if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
			return 0, readErr
		}
		return 0, fmt.Errorf("%w: object read failed", ErrCorruptRecord)
	}
	if statErr != nil || closeErr != nil || pathErr != nil ||
		!os.SameFile(before, after) || !os.SameFile(after, namedAfter) ||
		after.Size() != before.Size() || namedAfter.Size() != after.Size() ||
		!after.ModTime().Equal(before.ModTime()) || !namedAfter.ModTime().Equal(after.ModTime()) ||
		reader.total != before.Size() || recordIDFromDigest(reader.sum()) != expected {
		return 0, fmt.Errorf("%w: object identity disagrees", ErrCorruptRecord)
	}
	return reader.total, nil
}

type recordDigestReader struct {
	ctx     context.Context
	source  io.Reader
	hasher  hash.Hash
	maximum int64
	total   int64
}

func newRecordDigestReader(ctx context.Context, source io.Reader, kind RecordKind, maximum int64) *recordDigestReader {
	return &recordDigestReader{ctx: ctx, source: source, hasher: newRecordHasher(kind), maximum: maximum}
}

func (reader *recordDigestReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := reader.source.Read(buffer)
	if n < 0 || n > len(buffer) {
		return 0, fmt.Errorf("sealed record reader returned an invalid byte count")
	}
	if n > 0 {
		if int64(n) > reader.maximum-reader.total {
			return 0, errRecordByteLimit
		}
		_, _ = reader.hasher.Write(buffer[:n])
		reader.total += int64(n)
	}
	if err != nil && err != io.EOF {
		return n, fmt.Errorf("sealed record read failed")
	}
	return n, err
}

func (reader *recordDigestReader) sum() [sha256.Size]byte {
	var digest [sha256.Size]byte
	copy(digest[:], reader.hasher.Sum(nil))
	return digest
}

type recordConsumerReader struct {
	mu     sync.Mutex
	source io.Reader
	active bool
	bytes  int64
	sawEOF bool
}

func (reader *recordConsumerReader) Read(buffer []byte) (int, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if !reader.active {
		return 0, fmt.Errorf("sealed record reader is no longer available")
	}
	n, err := reader.source.Read(buffer)
	reader.bytes += int64(n)
	if err == io.EOF {
		reader.sawEOF = true
	}
	return n, err
}

func (reader *recordConsumerReader) deactivate() (int64, bool) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.active = false
	return reader.bytes, reader.sawEOF
}

func drainRecordReader(reader io.Reader) error {
	buffer := make([]byte, 32<<10)
	zeroReads := 0
	for {
		n, err := reader.Read(buffer)
		if n < 0 || n > len(buffer) {
			return fmt.Errorf("sealed record reader returned an invalid byte count")
		}
		if n > 0 {
			zeroReads = 0
		} else if err == nil {
			zeroReads++
			if zeroReads > 100 {
				return fmt.Errorf("sealed record reader made no progress")
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
