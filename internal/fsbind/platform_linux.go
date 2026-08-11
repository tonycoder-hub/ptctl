//go:build linux

package fsbind

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type platformData struct {
	parent   *os.File
	rootName string
}

var linuxRenameat2 = unix.Renameat2

// linuxAbsoluteOpenHook is a deterministic package-test seam for replacing an
// absolute locator ancestor after it has been opened but before traversal has
// completed. The real ancestor-chain validation always runs afterward.
var linuxAbsoluteOpenHook func(componentIndex int)

func platformBindExisting(root string) (*Session, RootInfo, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, RootInfo{}, ErrInvalidPath
	}
	clean := filepath.Clean(root)
	if len(clean) > maxPathBytes {
		return nil, RootInfo{}, ErrInvalidPath
	}
	rootName := filepath.Base(clean)
	if clean == string(filepath.Separator) || validateSingleComponent(rootName) != nil {
		return nil, RootInfo{}, ErrInvalidPath
	}
	parent, err := openLinuxAbsoluteDirectory(filepath.Dir(clean))
	if err != nil {
		return nil, RootInfo{}, err
	}
	failed := true
	defer func() {
		if failed {
			_ = parent.Close()
		}
	}()
	rootDirectory, err := openLinuxDirectoryAt(parent, rootName)
	if err != nil {
		return nil, RootInfo{}, err
	}
	defer func() {
		if failed {
			_ = rootDirectory.Close()
		}
	}()
	raw, err := linuxRawIdentity(rootDirectory, true)
	if err != nil {
		return nil, RootInfo{}, err
	}
	filesystem, err := linuxReviewedFilesystem(rootDirectory)
	if err != nil {
		return nil, RootInfo{}, err
	}
	session := &Session{
		path:     clean,
		root:     &boundDirectory{file: rootDirectory, raw: raw, identity: identityFromRaw(raw)},
		platform: platformState{data: platformData{parent: parent, rootName: rootName}},
	}
	info := RootInfo{
		Identity: session.root.identity, Filesystem: filesystem,
		CommitAssurance: "same_mount_renameat2_noreplace_and_parent_directories_fsync",
	}
	failed = false
	return session, info, nil
}

func platformCheckBinding(session *Session) error {
	if session == nil || session.root == nil || session.root.file == nil || session.platform.data.parent == nil {
		return ErrBindingChanged
	}
	handleRaw, err := linuxRawIdentity(session.root.file, true)
	if err != nil || handleRaw != session.root.raw {
		return ErrBindingChanged
	}
	parentChild, err := openLinuxDirectoryAt(session.platform.data.parent, session.platform.data.rootName)
	if err != nil {
		return ErrBindingChanged
	}
	parentRaw, rawErr := linuxRawIdentity(parentChild, true)
	_ = parentChild.Close()
	if rawErr != nil || parentRaw != session.root.raw {
		return ErrBindingChanged
	}
	named, err := openLinuxAbsoluteDirectory(session.path)
	if err != nil {
		return ErrBindingChanged
	}
	namedRaw, rawErr := linuxRawIdentity(named, true)
	_ = named.Close()
	if rawErr != nil || namedRaw != session.root.raw {
		return ErrBindingChanged
	}
	return nil
}

func platformCloseSession(session *Session) error {
	if session == nil {
		return nil
	}
	var first error
	if session.root != nil && session.root.file != nil {
		first = session.root.file.Close()
		session.root.file = nil
	}
	if session.platform.data.parent != nil {
		if err := session.platform.data.parent.Close(); err != nil && first == nil {
			first = err
		}
		session.platform.data.parent = nil
	}
	return first
}

func platformValidateComponent(component string) error {
	if len(component) > 255 || strings.ContainsRune(component, filepath.Separator) {
		return ErrInvalidPath
	}
	for _, value := range component {
		if value < 0x20 || value == 0x7f {
			return ErrInvalidPath
		}
	}
	return nil
}

func platformCreatePrivateDirectory(session *Session, parent *boundDirectory, name string) (*boundDirectory, bool, bool, error) {
	if err := verifyLinuxDirectory(session, parent); err != nil {
		return nil, false, false, err
	}
	if err := unix.Mkdirat(int(parent.file.Fd()), name, 0o700); err != nil {
		if errors.Is(err, syscall.EEXIST) {
			return nil, false, false, ErrAlreadyExists
		}
		return nil, false, false, fmt.Errorf("create private directory failed")
	}
	file, err := openLinuxDirectoryAt(parent.file, name)
	if err != nil {
		return nil, true, false, err
	}
	if err := unix.Fchmod(int(file.Fd()), 0o700); err != nil {
		_ = file.Close()
		return nil, true, false, fmt.Errorf("secure private directory failed")
	}
	raw, err := validateLinuxPrivate(file, true, session.root.raw)
	if err != nil {
		_ = file.Close()
		return nil, true, false, err
	}
	if err := unix.Fsync(int(file.Fd())); err != nil {
		_ = file.Close()
		return &boundDirectory{file: file, raw: raw, identity: identityFromRaw(raw)}, true, false, ErrDurabilityUnconfirmed
	}
	if err := unix.Fsync(int(parent.file.Fd())); err != nil {
		_ = file.Close()
		return &boundDirectory{file: file, raw: raw, identity: identityFromRaw(raw)}, true, false, ErrDurabilityUnconfirmed
	}
	return &boundDirectory{file: file, raw: raw, identity: identityFromRaw(raw)}, true, true, nil
}

func platformOpenPrivateDirectory(session *Session, parent *boundDirectory, name string) (*boundDirectory, error) {
	if err := verifyLinuxDirectory(session, parent); err != nil {
		return nil, err
	}
	file, err := openLinuxDirectoryAt(parent.file, name)
	if err != nil {
		return nil, err
	}
	raw, err := validateLinuxPrivate(file, true, session.root.raw)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &boundDirectory{file: file, raw: raw, identity: identityFromRaw(raw)}, nil
}

func platformCreatePrivateRegular(session *Session, parent *boundDirectory, name string) (*os.File, rawIdentity, error) {
	return openLinuxPrivateRegular(session, parent, name, true)
}

func platformOpenPrivateRegular(session *Session, parent *boundDirectory, name string) (*os.File, rawIdentity, error) {
	return openLinuxPrivateRegular(session, parent, name, false)
}

func platformOpenPrivateRegularReadOnly(session *Session, parent *boundDirectory, name string) (*os.File, rawIdentity, error) {
	if err := verifyLinuxDirectory(session, parent); err != nil {
		return nil, rawIdentity{}, err
	}
	fd, err := unix.Openat(int(parent.file.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, rawIdentity{}, ErrNotFound
		}
		return nil, rawIdentity{}, ErrUnsafeObject
	}
	file := os.NewFile(uintptr(fd), "fsbind-private-read-only")
	if file == nil {
		_ = unix.Close(fd)
		return nil, rawIdentity{}, ErrUnsafeObject
	}
	raw, err := validateLinuxPrivate(file, false, session.root.raw)
	if err != nil {
		_ = file.Close()
		return nil, rawIdentity{}, err
	}
	return file, raw, nil
}

func platformOpenAnySource(session *Session, parent *boundDirectory, name string, wantDirectory bool) (*os.File, rawIdentity, error) {
	if wantDirectory {
		directory, err := platformOpenPrivateDirectory(session, parent, name)
		if err != nil {
			return nil, rawIdentity{}, err
		}
		return directory.file, directory.raw, nil
	}
	return platformOpenPrivateRegular(session, parent, name)
}

func platformInspectObject(session *Session, parent *boundDirectory, name string) (*os.File, rawIdentity, ObjectKind, error) {
	return inspectLinuxObject(session, parent, name, true)
}

func platformInspectRootObject(session *Session, name string) (*os.File, rawIdentity, ObjectKind, error) {
	return inspectLinuxObject(session, session.root, name, false)
}

func platformOpenRootRegular(session *Session, name string, _ bool) (*os.File, rawIdentity, error) {
	if err := verifyLinuxDirectory(session, session.root); err != nil {
		return nil, rawIdentity{}, err
	}
	fd, err := unix.Openat(int(session.root.file.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, rawIdentity{}, ErrNotFound
		}
		return nil, rawIdentity{}, ErrUnsafeObject
	}
	file := os.NewFile(uintptr(fd), "fsbind-root-regular")
	if file == nil {
		_ = unix.Close(fd)
		return nil, rawIdentity{}, ErrUnsafeObject
	}
	raw, err := linuxRawIdentity(file, false)
	if err != nil || raw.volume != session.root.raw.volume || raw.mount != session.root.raw.mount {
		_ = file.Close()
		if err == nil {
			err = ErrCrossFilesystem
		}
		return nil, rawIdentity{}, err
	}
	return file, raw, nil
}

func inspectLinuxObject(session *Session, parent *boundDirectory, name string, requirePrivate bool) (*os.File, rawIdentity, ObjectKind, error) {
	if err := verifyLinuxDirectory(session, parent); err != nil {
		return nil, rawIdentity{}, "", err
	}
	fd, err := unix.Openat(int(parent.file.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, rawIdentity{}, "", ErrNotFound
		}
		return nil, rawIdentity{}, "", ErrUnsafeObject
	}
	file := os.NewFile(uintptr(fd), "fsbind-inspected-object")
	if file == nil {
		_ = unix.Close(fd)
		return nil, rawIdentity{}, "", ErrUnsafeObject
	}
	info, err := file.Stat()
	if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
		_ = file.Close()
		return nil, rawIdentity{}, "", ErrUnsafeObject
	}
	kind := ObjectKindRegular
	if info.IsDir() {
		kind = ObjectKindDirectory
	}
	var raw rawIdentity
	if requirePrivate {
		raw, err = validateLinuxPrivate(file, kind == ObjectKindDirectory, session.root.raw)
	} else {
		raw, err = linuxRawIdentity(file, kind == ObjectKindDirectory)
		if err == nil && (raw.volume != session.root.raw.volume || raw.mount != session.root.raw.mount) {
			err = ErrCrossFilesystem
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, rawIdentity{}, "", err
	}
	return file, raw, kind, nil
}

func platformAcquireOperationLock(session *Session, directory *boundDirectory, create bool) (*os.File, rawIdentity, error) {
	if err := verifyLinuxDirectory(session, directory); err != nil {
		return nil, rawIdentity{}, err
	}
	if err := unix.Flock(int(directory.file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, rawIdentity{}, ErrBusy
		}
		return nil, rawIdentity{}, fmt.Errorf("acquire operation directory lock failed")
	}
	directoryLocked := true
	defer func() {
		if directoryLocked {
			_ = unix.Flock(int(directory.file.Fd()), unix.LOCK_UN)
		}
	}()
	created := false
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if create {
		flags |= unix.O_CREAT | unix.O_EXCL
		created = true
	}
	fd, err := unix.Openat(int(directory.file.Fd()), operationLockName, flags, 0o600)
	if create && errors.Is(err, syscall.EEXIST) {
		created = false
		fd, err = unix.Openat(int(directory.file.Fd()), operationLockName, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, rawIdentity{}, ErrNotFound
		}
		return nil, rawIdentity{}, fmt.Errorf("open operation lock failed")
	}
	file := os.NewFile(uintptr(fd), "fsbind-operation-lock")
	if file == nil {
		_ = unix.Close(fd)
		return nil, rawIdentity{}, fmt.Errorf("wrap operation lock failed")
	}
	if created && unix.Fchmod(fd, 0o600) != nil {
		_ = file.Close()
		_ = unix.Unlinkat(int(directory.file.Fd()), operationLockName, 0)
		return nil, rawIdentity{}, fmt.Errorf("secure operation lock failed")
	}
	raw, err := validateLinuxPrivate(file, false, session.root.raw)
	if err != nil {
		_ = file.Close()
		if created {
			_ = unix.Unlinkat(int(directory.file.Fd()), operationLockName, 0)
		}
		return nil, rawIdentity{}, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, rawIdentity{}, ErrBusy
		}
		return nil, rawIdentity{}, fmt.Errorf("acquire operation lock failed")
	}
	directoryLocked = false
	return file, raw, nil
}

func platformCheckOperationLock(session *Session, directory *boundDirectory, file *os.File, expected rawIdentity) error {
	if file == nil || verifyLinuxDirectory(session, directory) != nil {
		return ErrBindingChanged
	}
	held, err := validateLinuxPrivate(file, false, session.root.raw)
	if err != nil || held != expected {
		return ErrBindingChanged
	}
	named, namedRaw, err := platformOpenPrivateRegularReadOnly(session, directory, operationLockName)
	if err != nil {
		return ErrBindingChanged
	}
	closeErr := named.Close()
	if closeErr != nil || namedRaw != expected {
		return ErrBindingChanged
	}
	return nil
}

func platformReleaseOperationLock(file *os.File, directory *boundDirectory) error {
	var first error
	if file != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		first = file.Close()
	}
	if directory != nil && directory.file != nil {
		if err := unix.Flock(int(directory.file.Fd()), unix.LOCK_UN); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func platformRetirePrivateSubtree(session *Session, name string, directory *boundDirectory, lockFile *os.File, expectedDirectory, expectedLock rawIdentity) (attempted, lockRemoved, directoryAttempted, removed, durable bool, resultErr error) {
	if directory == nil || directory.file == nil || lockFile == nil || directory.raw != expectedDirectory ||
		verifyLinuxDirectory(session, directory) != nil || platformCheckOperationLock(session, directory, lockFile, expectedLock) != nil {
		return false, false, false, false, false, ErrBindingChanged
	}
	named, namedErr := platformOpenPrivateDirectory(session, session.root, name)
	if namedErr != nil {
		return false, false, false, false, false, namedErr
	}
	namedMatches := named.raw == expectedDirectory
	namedCloseErr := named.file.Close()
	if !namedMatches || namedCloseErr != nil {
		return false, false, false, false, false, ErrUnsafeObject
	}

	lockClosed, directoryClosed := false, false
	defer func() {
		if !lockClosed {
			_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
			_ = lockFile.Close()
		}
		if !directoryClosed {
			_ = unix.Flock(int(directory.file.Fd()), unix.LOCK_UN)
			_ = directory.file.Close()
		}
	}()

	attempted = true
	lockRemoveErr := unix.Unlinkat(int(directory.file.Fd()), operationLockName, 0)
	remainingLock, remainingLockRaw, remainingLockErr := platformOpenPrivateRegularReadOnly(session, directory, operationLockName)
	if remainingLock != nil {
		_ = remainingLock.Close()
	}
	lockRemoved = errors.Is(remainingLockErr, ErrNotFound)
	_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
	lockCloseErr := lockFile.Close()
	lockClosed = true
	if !lockRemoved {
		if remainingLockErr == nil && remainingLockRaw == expectedLock {
			return attempted, false, false, false, false, ErrUnsafeObject
		}
		return attempted, false, false, false, false, ErrRemovalAmbiguous
	}
	if lockRemoveErr != nil || lockCloseErr != nil {
		resultErr = ErrRemovalAmbiguous
	}

	directoryAttempted = true
	directoryRemoveErr := unix.Unlinkat(int(session.root.file.Fd()), name, unix.AT_REMOVEDIR)
	remainingDirectory, remainingDirectoryErr := platformOpenPrivateDirectory(session, session.root, name)
	if remainingDirectory != nil {
		_ = remainingDirectory.file.Close()
	}
	removed = errors.Is(remainingDirectoryErr, ErrNotFound)
	_ = unix.Flock(int(directory.file.Fd()), unix.LOCK_UN)
	directoryCloseErr := directory.file.Close()
	directoryClosed = true
	if !removed {
		if remainingDirectoryErr == nil {
			return attempted, lockRemoved, directoryAttempted, false, false, errors.Join(resultErr, ErrUnsafeObject)
		}
		return attempted, lockRemoved, directoryAttempted, false, false, errors.Join(resultErr, ErrRemovalAmbiguous)
	}
	if directoryRemoveErr != nil || directoryCloseErr != nil {
		resultErr = errors.Join(resultErr, ErrRemovalAmbiguous)
	}
	if unix.Fsync(int(session.root.file.Fd())) != nil {
		return attempted, lockRemoved, directoryAttempted, removed, false, errors.Join(resultErr, ErrDurabilityUnconfirmed)
	}
	durable = true
	return attempted, lockRemoved, directoryAttempted, removed, durable, resultErr
}

func platformSyncFile(file *os.File) error {
	if file == nil || unix.Fsync(int(file.Fd())) != nil {
		return fmt.Errorf("sync bound regular file failed")
	}
	return nil
}

func platformSyncDirectory(directory *boundDirectory) error {
	if directory == nil || directory.file == nil || unix.Fsync(int(directory.file.Fd())) != nil {
		return fmt.Errorf("sync bound directory failed")
	}
	return nil
}

func platformReadDirectory(directory *boundDirectory, maximum int) ([]os.DirEntry, error) {
	if directory == nil || directory.file == nil || maximum <= 0 {
		return nil, ErrUnsafeObject
	}
	fd, err := unix.Openat(int(directory.file.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("read bound directory failed")
	}
	file := os.NewFile(uintptr(fd), "fsbind-directory-list")
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("wrap bound directory failed")
	}
	entries, readErr := file.ReadDir(maximum)
	closeErr := file.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, fmt.Errorf("read bound directory failed")
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close bound directory failed")
	}
	return entries, nil
}

func platformPublishNoReplace(session *Session, sourceParent *boundDirectory, sourceName string, destinationParent *boundDirectory, destinationName string, wantDirectory bool, expected rawIdentity) (bool, bool, rawIdentity, error) {
	if verifyLinuxDirectory(session, sourceParent) != nil || verifyLinuxDirectory(session, destinationParent) != nil || sourceParent.raw.volume != destinationParent.raw.volume || sourceParent.raw.mount != destinationParent.raw.mount {
		return false, false, rawIdentity{}, ErrCrossFilesystem
	}
	existing, _, _, destinationErr := inspectLinuxObject(session, destinationParent, destinationName, false)
	if existing != nil {
		_ = existing.Close()
	}
	if destinationErr == nil || !errors.Is(destinationErr, ErrNotFound) {
		if destinationErr == nil || errors.Is(destinationErr, ErrUnsafeObject) {
			return false, false, rawIdentity{}, ErrAlreadyExists
		}
		return false, false, rawIdentity{}, destinationErr
	}
	source, actual, err := platformOpenAnySource(session, sourceParent, sourceName, wantDirectory)
	if err != nil || actual != expected {
		if source != nil {
			_ = source.Close()
		}
		return false, false, rawIdentity{}, ErrUnsafeObject
	}
	_ = source.Close()
	renameErr := linuxRenameat2(int(sourceParent.file.Fd()), sourceName, int(destinationParent.file.Fd()), destinationName, unix.RENAME_NOREPLACE)
	if renameErr == nil {
		final, finalRaw, finalErr := platformOpenAnySource(session, destinationParent, destinationName, wantDirectory)
		if final != nil {
			_ = final.Close()
		}
		if finalErr != nil || finalRaw != expected {
			return true, false, expected, ErrPublicationAmbiguous
		}
		if unix.Fsync(int(destinationParent.file.Fd())) != nil {
			return true, false, finalRaw, ErrDurabilityUnconfirmed
		}
		if sourceParent.raw != destinationParent.raw && unix.Fsync(int(sourceParent.file.Fd())) != nil {
			return true, false, finalRaw, ErrDurabilityUnconfirmed
		}
		return true, true, finalRaw, nil
	}
	if errors.Is(renameErr, syscall.EEXIST) {
		return false, false, rawIdentity{}, ErrAlreadyExists
	}
	remaining, remainingRaw, remainingErr := platformOpenAnySource(session, sourceParent, sourceName, wantDirectory)
	if remaining != nil {
		_ = remaining.Close()
	}
	final, finalRaw, finalErr := platformOpenAnySource(session, destinationParent, destinationName, wantDirectory)
	if final != nil {
		_ = final.Close()
	}
	if errors.Is(remainingErr, ErrNotFound) && finalErr == nil && finalRaw == expected {
		return true, false, finalRaw, ErrDurabilityUnconfirmed
	}
	if remainingErr == nil && remainingRaw == expected && errors.Is(finalErr, ErrNotFound) {
		if errors.Is(renameErr, syscall.ENOSYS) {
			return false, false, rawIdentity{}, ErrUnsupported
		}
		return false, false, rawIdentity{}, ErrPublicationAmbiguous
	}
	return false, false, rawIdentity{}, ErrPublicationAmbiguous
}

func platformRemoveObject(session *Session, parent *boundDirectory, name string, wantDirectory bool, expected rawIdentity, expectedSize int64) (bool, bool, rawIdentity, error) {
	if verifyLinuxDirectory(session, parent) != nil {
		return false, false, rawIdentity{}, ErrCrossFilesystem
	}
	source, actual, err := platformOpenAnySource(session, parent, name, wantDirectory)
	if err != nil || actual != expected {
		if source != nil {
			_ = source.Close()
		}
		return false, false, rawIdentity{}, ErrUnsafeObject
	}
	if !wantDirectory && expectedSize >= 0 {
		info, statErr := source.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
			_ = source.Close()
			return false, false, rawIdentity{}, ErrUnsafeObject
		}
	}
	if closeErr := source.Close(); closeErr != nil {
		return false, false, rawIdentity{}, fmt.Errorf("close removal source failed")
	}
	flags := 0
	if wantDirectory {
		flags = unix.AT_REMOVEDIR
	}
	removeErr := unix.Unlinkat(int(parent.file.Fd()), name, flags)
	remaining, remainingRaw, remainingErr := platformOpenAnySource(session, parent, name, wantDirectory)
	if remaining != nil {
		_ = remaining.Close()
	}
	if removeErr == nil {
		if errors.Is(remainingErr, ErrNotFound) {
			if unix.Fsync(int(parent.file.Fd())) != nil {
				return true, false, expected, ErrDurabilityUnconfirmed
			}
			return true, true, expected, nil
		}
		return true, false, expected, ErrRemovalAmbiguous
	}
	if errors.Is(remainingErr, ErrNotFound) {
		return true, false, expected, ErrRemovalAmbiguous
	}
	if remainingErr == nil && remainingRaw == expected {
		return false, false, expected, ErrUnsafeObject
	}
	return false, false, rawIdentity{}, ErrRemovalAmbiguous
}

func platformRemoveRootRegular(session *Session, name string, expected rawIdentity, expectedSize int64) (bool, bool, rawIdentity, error) {
	if verifyLinuxDirectory(session, session.root) != nil {
		return false, false, rawIdentity{}, ErrCrossFilesystem
	}
	source, actual, err := platformOpenRootRegular(session, name, true)
	if err != nil || actual != expected {
		if source != nil {
			_ = source.Close()
		}
		return false, false, rawIdentity{}, ErrUnsafeObject
	}
	info, statErr := source.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
		_ = source.Close()
		return false, false, rawIdentity{}, ErrUnsafeObject
	}
	if closeErr := source.Close(); closeErr != nil {
		return false, false, rawIdentity{}, fmt.Errorf("close root removal source failed")
	}
	removeErr := unix.Unlinkat(int(session.root.file.Fd()), name, 0)
	remaining, remainingRaw, remainingErr := platformOpenRootRegular(session, name, false)
	if remaining != nil {
		_ = remaining.Close()
	}
	if removeErr == nil {
		if errors.Is(remainingErr, ErrNotFound) {
			if unix.Fsync(int(session.root.file.Fd())) != nil {
				return true, false, expected, ErrDurabilityUnconfirmed
			}
			return true, true, expected, nil
		}
		return true, false, expected, ErrRemovalAmbiguous
	}
	if errors.Is(remainingErr, ErrNotFound) {
		return true, false, expected, ErrRemovalAmbiguous
	}
	if remainingErr == nil && remainingRaw == expected {
		return false, false, expected, ErrUnsafeObject
	}
	return false, false, rawIdentity{}, ErrRemovalAmbiguous
}

func platformRemoveRootEmptyDirectory(session *Session, name string, expected rawIdentity) (bool, bool, rawIdentity, error) {
	if verifyLinuxDirectory(session, session.root) != nil {
		return false, false, rawIdentity{}, ErrCrossFilesystem
	}
	source, actual, kind, err := platformInspectRootObject(session, name)
	if err != nil {
		if source != nil {
			_ = source.Close()
		}
		return false, false, rawIdentity{}, err
	}
	if kind != ObjectKindDirectory || actual != expected {
		_ = source.Close()
		return false, false, rawIdentity{}, ErrUnsafeObject
	}
	if closeErr := source.Close(); closeErr != nil {
		return false, false, rawIdentity{}, fmt.Errorf("close root directory removal source failed")
	}
	removeErr := unix.Unlinkat(int(session.root.file.Fd()), name, unix.AT_REMOVEDIR)
	remaining, remainingRaw, _, remainingErr := platformInspectRootObject(session, name)
	if remaining != nil {
		_ = remaining.Close()
	}
	if removeErr == nil {
		if errors.Is(remainingErr, ErrNotFound) {
			if unix.Fsync(int(session.root.file.Fd())) != nil {
				return true, false, expected, ErrDurabilityUnconfirmed
			}
			return true, true, expected, nil
		}
		return true, false, expected, ErrRemovalAmbiguous
	}
	if errors.Is(removeErr, syscall.ENOTEMPTY) || errors.Is(removeErr, syscall.EEXIST) {
		return false, false, expected, ErrNotEmpty
	}
	if errors.Is(remainingErr, ErrNotFound) {
		return true, false, expected, ErrRemovalAmbiguous
	}
	if remainingErr == nil && remainingRaw == expected {
		return false, false, expected, fmt.Errorf("root directory removal failed")
	}
	return false, false, rawIdentity{}, ErrRemovalAmbiguous
}

func openLinuxPrivateRegular(session *Session, parent *boundDirectory, name string, create bool) (*os.File, rawIdentity, error) {
	if err := verifyLinuxDirectory(session, parent); err != nil {
		return nil, rawIdentity{}, err
	}
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if create {
		flags |= unix.O_CREAT | unix.O_EXCL
	}
	fd, err := unix.Openat(int(parent.file.Fd()), name, flags, 0o600)
	if err != nil {
		if errors.Is(err, syscall.EEXIST) {
			return nil, rawIdentity{}, ErrAlreadyExists
		}
		if errors.Is(err, syscall.ENOENT) {
			return nil, rawIdentity{}, ErrNotFound
		}
		return nil, rawIdentity{}, ErrUnsafeObject
	}
	file := os.NewFile(uintptr(fd), "fsbind-private-file")
	if file == nil {
		_ = unix.Close(fd)
		return nil, rawIdentity{}, ErrUnsafeObject
	}
	failed := true
	defer func() {
		if failed {
			_ = file.Close()
			if create {
				_ = unix.Unlinkat(int(parent.file.Fd()), name, 0)
			}
		}
	}()
	if create && unix.Fchmod(fd, 0o600) != nil {
		return nil, rawIdentity{}, fmt.Errorf("secure private regular file failed")
	}
	raw, err := validateLinuxPrivate(file, false, session.root.raw)
	if err != nil {
		return nil, rawIdentity{}, err
	}
	failed = false
	return file, raw, nil
}

func openLinuxAbsoluteDirectory(path string) (*os.File, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return nil, ErrInvalidPath
	}
	components := make([]string, 0, strings.Count(strings.TrimPrefix(clean, "/"), "/")+1)
	for _, component := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
		if component == "" {
			continue
		}
		if validateSingleComponent(component) != nil {
			return nil, ErrInvalidPath
		}
		components = append(components, component)
	}
	if len(components) > maxPathComponents {
		return nil, ErrInvalidPath
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrUnsafeObject
	}
	current := os.NewFile(uintptr(fd), "fsbind-absolute-directory")
	if current == nil {
		_ = unix.Close(fd)
		return nil, ErrUnsafeObject
	}
	handles := []*os.File{current}
	closeHandles := func() {
		for _, handle := range handles {
			if handle != nil {
				_ = handle.Close()
			}
		}
	}
	for index, component := range components {
		next, openErr := openLinuxDirectoryAt(current, component)
		if openErr != nil {
			closeHandles()
			return nil, openErr
		}
		current = next
		handles = append(handles, current)
		if linuxAbsoluteOpenHook != nil {
			linuxAbsoluteOpenHook(index)
		}
	}
	// Keep the entire no-follow chain alive until every retained parent still
	// names the child handle that was traversed. Revalidating leaf-to-root makes
	// an ancestor rename/replacement during traversal observable instead of
	// accepting a root reached through a now-detached parent handle.
	for index := len(components) - 1; index >= 0; index-- {
		expected, expectedErr := linuxRawIdentity(handles[index+1], true)
		reopened, reopenErr := openLinuxDirectoryAt(handles[index], components[index])
		if expectedErr != nil || reopenErr != nil {
			if reopened != nil {
				_ = reopened.Close()
			}
			closeHandles()
			return nil, ErrBindingChanged
		}
		actual, actualErr := linuxRawIdentity(reopened, true)
		closeErr := reopened.Close()
		if actualErr != nil || closeErr != nil || actual != expected {
			closeHandles()
			return nil, ErrBindingChanged
		}
	}
	for index := 0; index < len(handles)-1; index++ {
		if err := handles[index].Close(); err != nil {
			for remaining := index + 1; remaining < len(handles); remaining++ {
				_ = handles[remaining].Close()
			}
			return nil, ErrUnsafeObject
		}
		handles[index] = nil
	}
	return current, nil
}

func openLinuxDirectoryAt(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, ErrNotFound
		}
		return nil, ErrUnsafeObject
	}
	file := os.NewFile(uintptr(fd), "fsbind-directory")
	if file == nil {
		_ = unix.Close(fd)
		return nil, ErrUnsafeObject
	}
	return file, nil
}

func verifyLinuxDirectory(session *Session, directory *boundDirectory) error {
	if session == nil || session.root == nil || directory == nil || directory.file == nil {
		return ErrUnsafeObject
	}
	raw, err := linuxRawIdentity(directory.file, true)
	if err != nil || raw != directory.raw {
		return ErrUnsafeObject
	}
	if raw.volume != session.root.raw.volume || raw.mount != session.root.raw.mount {
		return ErrCrossFilesystem
	}
	return nil
}

func validateLinuxPrivate(file *os.File, wantDirectory bool, root rawIdentity) (rawIdentity, error) {
	info, err := file.Stat()
	if err != nil || info.IsDir() != wantDirectory || (!wantDirectory && !info.Mode().IsRegular()) {
		return rawIdentity{}, ErrUnsafeObject
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	wanted := os.FileMode(0o600)
	if wantDirectory {
		wanted = 0o700
	}
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != wanted || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 ||
		(!wantDirectory && stat.Nlink != 1) {
		return rawIdentity{}, ErrUnsafeObject
	}
	raw, err := linuxRawIdentity(file, wantDirectory)
	if err != nil {
		return rawIdentity{}, err
	}
	if raw.volume != root.volume || raw.mount != root.mount {
		return rawIdentity{}, ErrCrossFilesystem
	}
	return raw, nil
}

func linuxRawIdentity(file *os.File, wantDirectory bool) (rawIdentity, error) {
	if file == nil {
		return rawIdentity{}, ErrUnsafeObject
	}
	fd := int(file.Fd())
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return rawIdentity{}, ErrUnsafeObject
	}
	if (stat.Mode&unix.S_IFMT == unix.S_IFDIR) != wantDirectory || (!wantDirectory && stat.Mode&unix.S_IFMT != unix.S_IFREG) {
		return rawIdentity{}, ErrUnsafeObject
	}
	var extended unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_STATX_SYNC_AS_STAT, unix.STATX_MNT_ID, &extended); err != nil || extended.Mask&unix.STATX_MNT_ID == 0 {
		return rawIdentity{}, ErrUnsupported
	}
	return rawIdentity{volume: uint64(stat.Dev), mount: extended.Mnt_id, fileLow: stat.Ino, directory: wantDirectory}, nil
}

func linuxReviewedFilesystem(file *os.File) (string, error) {
	var stat unix.Statfs_t
	if file == nil || unix.Fstatfs(int(file.Fd()), &stat) != nil {
		return "", ErrUnsupported
	}
	switch uint64(stat.Type) {
	case uint64(unix.EXT4_SUPER_MAGIC):
		return "ext4", nil
	case uint64(unix.XFS_SUPER_MAGIC):
		return "xfs", nil
	default:
		return "", ErrUnsupported
	}
}
