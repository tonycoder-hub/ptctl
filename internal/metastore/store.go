package metastore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
)

const (
	markerName       = ".ptctl-metastore"
	objectsDir       = "objects"
	temporaryDir     = "tmp"
	maximumMarkerLen = 256
)

var (
	errArtifactNotFound              = errors.New("private metafile artifact was not found")
	errPrivateDirectoryAlreadyExists = errors.New("private directory already exists")
)

// publishNoReplace is a narrow test seam for the two post-publication failure
// states. Production always uses the platform primitive directly.
var publishNoReplace = platformSessionPublishNoReplace
var confirmPrivateStoreLayout = platformConfirmPrivateStoreLayout

type Store struct {
	root string
	info StoreInfo
}

func (s *Store) Info() StoreInfo {
	if s == nil {
		return StoreInfo{}
	}
	return s.info
}

func (s *Store) String() string {
	if s == nil {
		return "metastore.Store{nil}"
	}
	return "metastore.Store{" + s.info.StoreID + "}"
}

func (s *Store) GoString() string { return s.String() }

func Init(root string) (*Store, InitReceipt, error) {
	receipt := InitReceipt{Effect: initEffect}
	clean, err := normalizeStoreRoot(root, true)
	if err != nil {
		return nil, receipt, safeError("initialize private metafile store", err)
	}
	_, err = ensurePrivateDirectory(clean)
	if err != nil {
		return nil, receipt, safeError("initialize private metafile store", err)
	}
	session, err := openBoundRootOnlySession(clean)
	if err != nil {
		return nil, receipt, safeError("initialize private metafile store", err)
	}
	defer session.Close()
	// Everything below this point is inspected and mutated through one bound
	// root identity. Unknown root entries, an invalid marker, or a non-empty
	// object directory is rejected before either known subdirectory is added.
	// Existing staging contents remain opaque and are never read or overwritten;
	// random O_EXCL names isolate concurrent initialization.
	concurrentInfo, err := prepareInitSession(session)
	if err != nil {
		return nil, receipt, safeError("initialize private metafile store", err)
	}
	if concurrentInfo != nil {
		if err := session.check("before_success"); err != nil {
			return nil, receipt, fmt.Errorf("initialize private metafile store: bound identity changed")
		}
		existing := &Store{root: clean, info: *concurrentInfo}
		receipt.AlreadyInitialized = true
		receipt.Store = existing.Info()
		return existing, receipt, nil
	}
	if err := confirmPrivateStoreLayout(session); err != nil {
		return nil, receipt, fmt.Errorf("initialize private metafile store: layout durability precondition failed")
	}

	storeID, err := randomStoreID()
	if err != nil {
		return nil, receipt, fmt.Errorf("initialize private metafile store: random identifier unavailable")
	}
	marker := markerBytes(storeID)
	temporaryName, err := randomRelativeName(temporaryDir, ".init-", ".marker")
	if err != nil {
		return nil, receipt, fmt.Errorf("initialize private metafile store: random staging name unavailable")
	}
	file, err := session.createPrivateFile(temporaryName)
	if err != nil {
		return nil, receipt, fmt.Errorf("initialize private metafile store: create staging marker failed")
	}
	removeStaging := true
	defer func() {
		if removeStaging {
			_ = session.removePrivate(temporaryName)
		}
	}()
	if err := writeAndFlush(file, marker); err != nil {
		_ = file.Close()
		return nil, receipt, fmt.Errorf("initialize private metafile store: persist marker failed")
	}
	if err := file.Close(); err != nil {
		return nil, receipt, fmt.Errorf("initialize private metafile store: close marker failed")
	}

	published, publishErr := session.publishNoReplace(temporaryName, markerName)
	if published {
		receipt.WritesPerformed = 1
		removeStaging = false
	}
	if publishErr != nil {
		if published {
			info, inspectErr := inspectStoreSession(session)
			bindingErr := session.check("before_success")
			if inspectErr == nil && bindingErr == nil && info.StoreID == storeID {
				opened := &Store{root: clean, info: info}
				receipt.Store = opened.Info()
				return opened, receipt, publishedError(publishErr)
			}
			return nil, receipt, publishedError(publishErr)
		}
		return nil, receipt, fmt.Errorf("initialize private metafile store: atomic commit failed")
	}
	info, err := inspectStoreSession(session)
	if err != nil {
		return nil, receipt, fmt.Errorf("initialize private metafile store: committed store did not validate")
	}
	if err := session.check("before_success"); err != nil {
		return nil, receipt, fmt.Errorf("initialize private metafile store: bound identity changed after commit")
	}
	opened := &Store{root: clean, info: info}
	if !published {
		receipt.AlreadyInitialized = true
	} else if opened.info.StoreID != storeID {
		return nil, receipt, fmt.Errorf("initialize private metafile store: committed marker identity changed")
	}
	receipt.Store = opened.Info()
	return opened, receipt, nil
}

func Open(root string) (*Store, error) {
	clean, err := normalizeStoreRoot(root, false)
	if err != nil {
		return nil, safeError("open private metafile store", err)
	}
	info, err := inspectStore(clean)
	if err != nil {
		return nil, safeError("open private metafile store", err)
	}
	return &Store{root: clean, info: info}, nil
}

func (s *Store) Import(ctx context.Context, reader io.Reader, limits Limits) (*metafile.MetaInfo, ArtifactRef, ImportReceipt, error) {
	return s.importStream(ctx, reader, limits, nil)
}

func (s *Store) ImportFile(ctx context.Context, path string, limits Limits) (*metafile.MetaInfo, ArtifactRef, ImportReceipt, error) {
	receipt := ImportReceipt{Effect: importEffect, Store: s.Info()}
	if s == nil {
		return nil, ArtifactRef{}, receipt, fmt.Errorf("import private metafile: store is unavailable")
	}
	if err := limits.Validate(); err != nil {
		return nil, ArtifactRef{}, receipt, err
	}
	if err := ctx.Err(); err != nil {
		return nil, ArtifactRef{}, receipt, err
	}
	clean, err := normalizeSourcePath(path)
	if err != nil {
		return nil, ArtifactRef{}, receipt, fmt.Errorf("import private metafile: source is unavailable")
	}
	if err := platformValidateSourcePath(clean); err != nil {
		return nil, ArtifactRef{}, receipt, fmt.Errorf("import private metafile: source is unsafe")
	}
	namedBefore, err := os.Lstat(clean)
	if err != nil || platformRejectNamedInfo(namedBefore) || !namedBefore.Mode().IsRegular() {
		return nil, ArtifactRef{}, receipt, fmt.Errorf("import private metafile: source is not a regular non-link file")
	}
	if namedBefore.Size() < 0 || namedBefore.Size() > limits.MaxArtifactBytes {
		return nil, ArtifactRef{}, receipt, fmt.Errorf("%w: artifact exceeds its byte limit", ErrInvalidMetafile)
	}
	file, err := os.Open(clean)
	if err != nil {
		return nil, ArtifactRef{}, receipt, fmt.Errorf("import private metafile: open source failed")
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	handleBefore, err := file.Stat()
	if err != nil || !os.SameFile(namedBefore, handleBefore) || platformValidateSourceFile(file) != nil {
		return nil, ArtifactRef{}, receipt, fmt.Errorf("import private metafile: source identity is unsafe")
	}
	postRead := func() error {
		handleAfter, statErr := file.Stat()
		namedAfter, pathErr := os.Lstat(clean)
		if statErr != nil || pathErr != nil || platformRejectNamedInfo(namedAfter) || !namedAfter.Mode().IsRegular() ||
			!os.SameFile(handleBefore, handleAfter) || !os.SameFile(handleAfter, namedAfter) ||
			handleAfter.Size() != handleBefore.Size() || !handleAfter.ModTime().Equal(handleBefore.ModTime()) {
			return fmt.Errorf("source changed during import")
		}
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("close source failed")
		}
		closed = true
		return nil
	}
	return s.importStream(ctx, file, limits, postRead)
}

func (s *Store) Load(ctx context.Context, id ArtifactID, limits Limits) (*metafile.MetaInfo, ArtifactRef, error) {
	if s == nil {
		return nil, ArtifactRef{}, fmt.Errorf("load private metafile artifact: store is unavailable")
	}
	parsedID, err := ParseArtifactID(id.String())
	if err != nil {
		return nil, ArtifactRef{}, err
	}
	if err := limits.Validate(); err != nil {
		return nil, ArtifactRef{}, err
	}
	session, err := s.validatedSession()
	if err != nil {
		return nil, ArtifactRef{}, safeError("load private metafile artifact", err)
	}
	defer session.Close()
	meta, raw, err := loadArtifact(ctx, session, parsedID, limits)
	if err != nil {
		if errors.Is(err, errArtifactNotFound) || errors.Is(err, ErrCorruptArtifact) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, ArtifactRef{}, err
		}
		return nil, ArtifactRef{}, fmt.Errorf("load private metafile artifact failed")
	}
	if err := session.check("before_success"); err != nil {
		return nil, ArtifactRef{}, fmt.Errorf("load private metafile artifact: bound identity changed")
	}
	return meta, makeArtifactRef(meta, int64(len(raw))), nil
}

func (s *Store) importStream(ctx context.Context, reader io.Reader, limits Limits, postRead func() error) (*metafile.MetaInfo, ArtifactRef, ImportReceipt, error) {
	receipt := ImportReceipt{Effect: importEffect, Store: s.Info()}
	if s == nil || reader == nil {
		return nil, ArtifactRef{}, receipt, fmt.Errorf("import private metafile: input is unavailable")
	}
	if err := limits.Validate(); err != nil {
		return nil, ArtifactRef{}, receipt, err
	}
	if err := ctx.Err(); err != nil {
		return nil, ArtifactRef{}, receipt, err
	}
	session, err := s.validatedSession()
	if err != nil {
		return nil, ArtifactRef{}, receipt, safeError("import private metafile", err)
	}
	defer session.Close()

	temporaryName, err := randomRelativeName(temporaryDir, ".import-", ".torrent")
	if err != nil {
		return nil, ArtifactRef{}, receipt, fmt.Errorf("import private metafile: random staging name unavailable")
	}
	file, err := session.createPrivateFile(temporaryName)
	if err != nil {
		return nil, ArtifactRef{}, receipt, fmt.Errorf("import private metafile: create staging object failed")
	}
	stagingPresent := true
	defer func() {
		if stagingPresent {
			_ = session.removePrivate(temporaryName)
		}
	}()

	bytesConsumed, streamDigest, err := copyBounded(ctx, file, reader, limits.MaxArtifactBytes)
	receipt.BytesConsumed = bytesConsumed
	if err != nil {
		_ = file.Close()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, ArtifactRef{}, receipt, err
		}
		if errors.Is(err, ErrInvalidMetafile) {
			return nil, ArtifactRef{}, receipt, err
		}
		return nil, ArtifactRef{}, receipt, fmt.Errorf("import private metafile: read input failed")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, ArtifactRef{}, receipt, fmt.Errorf("import private metafile: flush staging object failed")
	}
	if postRead != nil {
		if err := postRead(); err != nil {
			_ = file.Close()
			return nil, ArtifactRef{}, receipt, fmt.Errorf("import private metafile: source changed during read")
		}
	}
	if err := file.Close(); err != nil {
		return nil, ArtifactRef{}, receipt, fmt.Errorf("import private metafile: close staging object failed")
	}

	raw, err := readStableRelative(ctx, session, temporaryName, limits.MaxArtifactBytes, false)
	if err != nil {
		return nil, ArtifactRef{}, receipt, fmt.Errorf("import private metafile: staging verification failed")
	}
	if int64(len(raw)) != bytesConsumed || sha256.Sum256(raw) != streamDigest {
		return nil, ArtifactRef{}, receipt, fmt.Errorf("import private metafile: staging object changed")
	}
	meta, err := metafile.Parse(raw)
	if err != nil {
		return nil, ArtifactRef{}, receipt, fmt.Errorf("%w: parse failed", ErrInvalidMetafile)
	}
	id, err := ParseArtifactID(meta.MetafileVariantID)
	if err != nil || id != artifactIDFor(raw) {
		return nil, ArtifactRef{}, receipt, fmt.Errorf("%w: variant identity is invalid", ErrInvalidMetafile)
	}
	ref := makeArtifactRef(meta, int64(len(raw)))

	if existingMeta, existingRaw, existingErr := loadArtifact(ctx, session, id, limits); existingErr == nil {
		if !bytes.Equal(existingRaw, raw) || existingMeta.MetafileVariantID != meta.MetafileVariantID {
			return nil, ArtifactRef{}, receipt, fmt.Errorf("%w: existing object disagrees with imported bytes", ErrCorruptArtifact)
		}
		if err := session.check("before_success"); err != nil {
			return nil, ArtifactRef{}, receipt, fmt.Errorf("import private metafile: bound identity changed")
		}
		receipt.AlreadyPresent = true
		return existingMeta, ref, receipt, nil
	} else if !errors.Is(existingErr, errArtifactNotFound) {
		return nil, ArtifactRef{}, receipt, existingErr
	}

	objectName := objectRelativePath(id)
	published, publishErr := session.publishNoReplace(temporaryName, objectName)
	if published {
		receipt.WritesPerformed = 1
		stagingPresent = false
	}
	if publishErr != nil {
		if published {
			verificationContext := context.WithoutCancel(ctx)
			committedMeta, committedRaw, verifyErr := loadArtifact(verificationContext, session, id, limits)
			bindingErr := session.check("before_success")
			if verifyErr == nil && bindingErr == nil && bytes.Equal(committedRaw, raw) && committedMeta.MetafileVariantID == meta.MetafileVariantID {
				return committedMeta, ref, receipt, publishedError(publishErr)
			}
			// The platform proved that this exact staging object crossed the
			// publication boundary. Preserve its parsed identity for audit even
			// when a subsequent store revalidation cannot be completed.
			return meta, ref, receipt, publishedError(publishErr)
		}
		return nil, ArtifactRef{}, receipt, fmt.Errorf("import private metafile: atomic commit failed")
	}
	verificationContext := context.WithoutCancel(ctx)
	committedMeta, committedRaw, err := loadArtifact(verificationContext, session, id, limits)
	if err != nil {
		return meta, ref, receipt, err
	}
	if !bytes.Equal(committedRaw, raw) || committedMeta.MetafileVariantID != meta.MetafileVariantID {
		return meta, ref, receipt, fmt.Errorf("%w: committed object disagrees with imported bytes", ErrCorruptArtifact)
	}
	if err := session.check("before_success"); err != nil {
		return meta, ref, receipt, fmt.Errorf("import private metafile: bound identity changed after commit")
	}
	if !published {
		receipt.AlreadyPresent = true
	}
	return committedMeta, ref, receipt, nil
}

func inspectStore(rootPath string) (StoreInfo, error) {
	session, err := openBoundRootSession(rootPath)
	if err != nil {
		return StoreInfo{}, err
	}
	defer session.Close()
	info, err := inspectStoreSession(session)
	if err != nil {
		return StoreInfo{}, err
	}
	if err := session.check("before_success"); err != nil {
		return StoreInfo{}, err
	}
	return info, nil
}

func inspectStoreSession(session *rootSession) (StoreInfo, error) {
	rootDirectory, _, err := session.openValidated(".", true)
	if err != nil {
		return StoreInfo{}, fmt.Errorf("store root layout is unsafe")
	}
	entries, overflow, readErr := readDirectoryPrefix(rootDirectory, 3)
	closeErr := rootDirectory.Close()
	if readErr != nil || closeErr != nil || overflow || !isCompleteStoreLayout(entries) {
		return StoreInfo{}, fmt.Errorf("store root layout is unrecognized")
	}
	marker, err := readStableRelative(context.Background(), session, markerName, maximumMarkerLen, false)
	if err != nil {
		return StoreInfo{}, fmt.Errorf("store format marker is unavailable")
	}
	storeID, err := parseMarker(marker)
	if err != nil {
		return StoreInfo{}, fmt.Errorf("store format marker is invalid")
	}
	return StoreInfo{StoreID: storeID, Format: storeFormat, Privacy: privacyAssurance, CommitAssurance: platformCommitAssurance()}, nil
}

func (s *Store) validatedSession() (*rootSession, error) {
	if s == nil || s.root == "" || s.info.StoreID == "" {
		return nil, fmt.Errorf("store is unavailable")
	}
	session, err := openBoundRootSession(s.root)
	if err != nil {
		return nil, fmt.Errorf("store root unavailable")
	}
	current, err := inspectStoreSession(session)
	if err != nil || current != s.info {
		_ = session.Close()
		return nil, fmt.Errorf("store identity or security properties changed")
	}
	if err := session.check("session_opened"); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("store identity or security properties changed")
	}
	return session, nil
}

func ensurePrivateDirectory(path string) (bool, error) {
	named, err := os.Lstat(path)
	if err == nil {
		if platformRejectNamedInfo(named) || !named.IsDir() {
			return false, fmt.Errorf("store directory is unsafe")
		}
		return false, nil
	}
	if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect store directory failed")
	}
	if err := platformCreatePrivateDirectory(path); err != nil {
		if errors.Is(err, errPrivateDirectoryAlreadyExists) {
			if named, statErr := os.Lstat(path); statErr == nil && named.IsDir() && !platformRejectNamedInfo(named) {
				return false, nil
			}
		}
		return false, fmt.Errorf("create store directory failed")
	}
	return true, nil
}

func validatePartialStoreSession(session *rootSession) (*StoreInfo, error) {
	rootDirectory, _, err := session.openValidated(".", true)
	if err != nil {
		return nil, fmt.Errorf("partial store root is unsafe")
	}
	entries, overflow, readErr := readDirectoryPrefix(rootDirectory, 3)
	closeErr := rootDirectory.Close()
	if readErr != nil || closeErr != nil || overflow {
		return nil, fmt.Errorf("partial store layout is unrecognized")
	}
	for _, entry := range entries {
		switch entry.Name() {
		case objectsDir, temporaryDir:
		case markerName:
			info, inspectErr := inspectStoreSession(session)
			if inspectErr != nil {
				return nil, fmt.Errorf("partial store marker is invalid")
			}
			return &info, nil
		default:
			return nil, fmt.Errorf("partial store layout is unrecognized")
		}
	}
	objects, _, err := session.openValidated(objectsDir, true)
	if err != nil {
		return nil, fmt.Errorf("partial store directory is unsafe")
	}
	objectEntries, objectReadErr := objects.ReadDir(1)
	objectCloseErr := objects.Close()
	if objectReadErr != nil && !errors.Is(objectReadErr, io.EOF) || objectCloseErr != nil || len(objectEntries) != 0 {
		return nil, fmt.Errorf("uninitialized store contains objects")
	}
	staging, _, err := session.openValidated(temporaryDir, true)
	if err != nil {
		return nil, fmt.Errorf("partial store directory is unsafe")
	}
	if err := staging.Close(); err != nil {
		return nil, fmt.Errorf("partial store directory is unavailable")
	}
	if err := session.check("partial_layout_validated"); err != nil {
		return nil, err
	}
	return nil, nil
}

func prepareInitSession(session *rootSession) (*StoreInfo, error) {
	rootDirectory, _, err := session.openValidated(".", true)
	if err != nil {
		return nil, fmt.Errorf("partial store root is unsafe")
	}
	entries, overflow, readErr := readDirectoryPrefix(rootDirectory, 3)
	closeErr := rootDirectory.Close()
	if readErr != nil || closeErr != nil || overflow {
		return nil, fmt.Errorf("partial store layout is unrecognized")
	}
	present := make(map[string]bool, len(entries))
	for _, entry := range entries {
		switch entry.Name() {
		case markerName, objectsDir, temporaryDir:
			present[entry.Name()] = true
		default:
			return nil, fmt.Errorf("partial store layout is unrecognized")
		}
	}
	if present[markerName] && (!present[objectsDir] || !present[temporaryDir]) {
		return nil, fmt.Errorf("partial store marker is invalid")
	}
	if present[objectsDir] {
		if err := session.attachSubdirectory(objectsDir); err != nil {
			return nil, fmt.Errorf("partial store objects directory is unsafe")
		}
	}
	if present[temporaryDir] {
		if err := session.attachSubdirectory(temporaryDir); err != nil {
			return nil, fmt.Errorf("partial store staging directory is unsafe")
		}
	}
	if present[markerName] {
		info, err := inspectStoreSession(session)
		if err != nil {
			return nil, fmt.Errorf("partial store marker is invalid")
		}
		return &info, nil
	}
	if present[objectsDir] {
		objects, _, err := session.openValidated(objectsDir, true)
		if err != nil {
			return nil, fmt.Errorf("partial store objects directory is unsafe")
		}
		objectEntries, objectOverflow, objectReadErr := readDirectoryPrefix(objects, 0)
		objectCloseErr := objects.Close()
		if objectReadErr != nil || objectCloseErr != nil || objectOverflow || len(objectEntries) != 0 {
			return nil, fmt.Errorf("uninitialized store contains objects")
		}
	}
	if !present[objectsDir] {
		if err := session.createPrivateDirectory(objectsDir); err != nil {
			return nil, err
		}
	}
	if !present[temporaryDir] {
		if err := session.createPrivateDirectory(temporaryDir); err != nil {
			return nil, err
		}
	}
	return validatePartialStoreSession(session)
}

func readDirectoryPrefix(directory *os.File, maximum int) ([]os.DirEntry, bool, error) {
	if directory == nil || maximum < 0 {
		return nil, false, fmt.Errorf("directory reader is unavailable")
	}
	entries, err := directory.ReadDir(maximum + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, fmt.Errorf("directory read failed")
	}
	if len(entries) > maximum {
		return entries[:maximum], true, nil
	}
	return entries, false, nil
}

func isCompleteStoreLayout(entries []os.DirEntry) bool {
	if len(entries) != 3 {
		return false
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		switch entry.Name() {
		case markerName, objectsDir, temporaryDir:
			seen[entry.Name()] = true
		default:
			return false
		}
	}
	return seen[markerName] && seen[objectsDir] && seen[temporaryDir]
}

func openValidatedRelative(root *os.Root, relative string, wantDirectory bool) (*os.File, os.FileInfo, error) {
	named, err := root.Lstat(relative)
	if err != nil || platformRejectNamedInfo(named) {
		return nil, nil, fmt.Errorf("named store object is unavailable or unsafe")
	}
	if named.IsDir() != wantDirectory || (!wantDirectory && !named.Mode().IsRegular()) {
		return nil, nil, fmt.Errorf("named store object has the wrong type")
	}
	file, err := root.Open(relative)
	if err != nil {
		return nil, nil, fmt.Errorf("open store object failed")
	}
	handle, err := file.Stat()
	if err != nil || !os.SameFile(named, handle) || platformValidateOpenFile(file, wantDirectory) != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("store object identity or privacy is unsafe")
	}
	return file, handle, nil
}

func readStableRelative(ctx context.Context, session *rootSession, relative string, maximum int64, wantDirectory bool) ([]byte, error) {
	file, before, err := session.openValidated(relative, wantDirectory)
	if err != nil {
		return nil, err
	}
	if wantDirectory {
		_ = file.Close()
		return nil, fmt.Errorf("cannot read a store directory as bytes")
	}
	if before.Size() < 0 || before.Size() > maximum {
		_ = file.Close()
		return nil, fmt.Errorf("store object exceeds its byte limit")
	}
	raw, err := readBounded(ctx, file, maximum)
	after, statErr := file.Stat()
	closeErr := file.Close()
	reopened, namedAfter, pathErr := session.openValidated(relative, wantDirectory)
	if reopened != nil {
		_ = reopened.Close()
	}
	if err != nil {
		return nil, err
	}
	if statErr != nil || closeErr != nil || pathErr != nil ||
		!os.SameFile(before, after) || !os.SameFile(after, namedAfter) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return nil, fmt.Errorf("store object changed while reading")
	}
	return raw, nil
}

func loadArtifact(ctx context.Context, session *rootSession, id ArtifactID, limits Limits) (*metafile.MetaInfo, []byte, error) {
	relative := objectRelativePath(id)
	raw, err := readStableRelative(ctx, session, relative, limits.MaxArtifactBytes, false)
	if err != nil {
		if errors.Is(err, errArtifactNotFound) {
			return nil, nil, errArtifactNotFound
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%w: object could not be read safely", ErrCorruptArtifact)
	}
	if artifactIDFor(raw) != id {
		return nil, nil, fmt.Errorf("%w: digest mismatch", ErrCorruptArtifact)
	}
	meta, err := metafile.Parse(raw)
	if err != nil || meta.MetafileVariantID != id.String() {
		return nil, nil, fmt.Errorf("%w: parse or variant mismatch", ErrCorruptArtifact)
	}
	return meta, raw, nil
}

func copyBounded(ctx context.Context, destination io.Writer, source io.Reader, maximum int64) (int64, [sha256.Size]byte, error) {
	hasher := sha256.New()
	w := io.MultiWriter(destination, hasher)
	buffer := make([]byte, 32<<10)
	var total int64
	zeroReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, [sha256.Size]byte{}, err
		}
		n, readErr := source.Read(buffer)
		if n < 0 || n > len(buffer) {
			return total, [sha256.Size]byte{}, fmt.Errorf("input reader returned an invalid byte count")
		}
		if n > 0 {
			zeroReads = 0
			if int64(n) > maximum-total {
				return total + int64(n), [sha256.Size]byte{}, fmt.Errorf("%w: artifact exceeds its byte limit", ErrInvalidMetafile)
			}
			written, writeErr := w.Write(buffer[:n])
			total += int64(written)
			if writeErr != nil || written != n {
				return total, [sha256.Size]byte{}, fmt.Errorf("write staging object failed")
			}
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

func readBounded(ctx context.Context, reader io.Reader, maximum int64) ([]byte, error) {
	var output bytes.Buffer
	if maximum < 64<<10 {
		output.Grow(int(maximum))
	} else {
		output.Grow(64 << 10)
	}
	buffer := make([]byte, 32<<10)
	zeroReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, readErr := reader.Read(buffer)
		if n < 0 || n > len(buffer) {
			return nil, fmt.Errorf("reader returned an invalid byte count")
		}
		if n > 0 {
			zeroReads = 0
			if int64(output.Len())+int64(n) > maximum {
				return nil, fmt.Errorf("byte limit exceeded")
			}
			_, _ = output.Write(buffer[:n])
		} else if readErr == nil {
			zeroReads++
			if zeroReads > 100 {
				return nil, fmt.Errorf("reader made no progress")
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return output.Bytes(), nil
			}
			return nil, fmt.Errorf("read failed")
		}
	}
}

func writeAndFlush(file *os.File, value []byte) error {
	if _, err := file.Write(value); err != nil {
		return err
	}
	return file.Sync()
}

func normalizeStoreRoot(value string, mayNotExist bool) (string, error) {
	if value == "" || strings.IndexByte(value, 0) >= 0 {
		return "", fmt.Errorf("store root is empty or invalid")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("store root cannot be resolved")
	}
	absolute = filepath.Clean(absolute)
	volumeRoot := filepath.VolumeName(absolute) + string(filepath.Separator)
	if absolute == volumeRoot || absolute == string(filepath.Separator) {
		return "", fmt.Errorf("filesystem root cannot be a private metafile store")
	}
	if err := platformValidateStoreLocation(absolute, mayNotExist); err != nil {
		return "", err
	}
	return absolute, nil
}

func normalizeSourcePath(value string) (string, error) {
	if value == "" || strings.IndexByte(value, 0) >= 0 {
		return "", fmt.Errorf("source path is invalid")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("source path cannot be resolved")
	}
	return filepath.Clean(absolute), nil
}

func randomStoreID() (string, error) {
	var value [32]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", err
	}
	return "store:" + hex.EncodeToString(value[:]), nil
}

func randomRelativeName(directory, prefix, suffix string) (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", err
	}
	return filepath.Join(directory, prefix+hex.EncodeToString(value[:])+suffix), nil
}

func markerBytes(storeID string) []byte {
	return []byte(storeFormat + "\nstore_id=" + storeID + "\n")
}

func parseMarker(raw []byte) (string, error) {
	text := string(raw)
	prefix := storeFormat + "\nstore_id=store:"
	if !strings.HasPrefix(text, prefix) || !strings.HasSuffix(text, "\n") || len(text) != len(prefix)+64+1 {
		return "", fmt.Errorf("unknown store marker format")
	}
	digest := text[len(prefix) : len(prefix)+64]
	if strings.ToLower(digest) != digest {
		return "", fmt.Errorf("invalid store identifier")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 {
		return "", fmt.Errorf("invalid store identifier")
	}
	return "store:" + digest, nil
}

func artifactIDFor(raw []byte) ArtifactID {
	digest := sha256.Sum256(raw)
	return ArtifactID("sha256:" + hex.EncodeToString(digest[:]))
}

func objectRelativePath(id ArtifactID) string {
	return filepath.Join(objectsDir, strings.TrimPrefix(id.String(), "sha256:")+".torrent")
}

func makeArtifactRef(meta *metafile.MetaInfo, size int64) ArtifactRef {
	id, _ := ParseArtifactID(meta.MetafileVariantID)
	return ArtifactRef{
		ID:                id,
		MetafileVariantID: meta.MetafileVariantID,
		Version:           meta.Version,
		InfoHashV1:        meta.InfoHashV1,
		InfoHashV2:        meta.InfoHashV2,
		SizeBytes:         size,
	}
}

func safeError(operation string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%s failed", operation)
}

func publishedError(err error) error {
	if errors.Is(err, ErrPublishedCleanupIncomplete) {
		return fmt.Errorf("%w", ErrPublishedCleanupIncomplete)
	}
	if errors.Is(err, ErrDurabilityUnconfirmed) {
		return fmt.Errorf("%w", ErrDurabilityUnconfirmed)
	}
	return fmt.Errorf("private metafile publication completed but bound identity validation failed")
}
