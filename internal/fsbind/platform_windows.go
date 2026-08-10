//go:build windows

package fsbind

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const forbiddenWindowsAttributes = windows.FILE_ATTRIBUTE_REPARSE_POINT |
	windows.FILE_ATTRIBUTE_OFFLINE |
	windows.FILE_ATTRIBUTE_RECALL_ON_OPEN |
	windows.FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS

type windowsGuard struct {
	path    string
	file    *os.File
	raw     rawIdentity
	private bool
}

type platformData struct{ guards []windowsGuard }

type windowsFileRenameInformation struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

type windowsFileDispositionInformation struct{ DeleteFile byte }

var (
	windowsRenameRelative   = renameWindowsRelative
	windowsFlushFileBuffers = windows.FlushFileBuffers
	windowsSetPrivateOwner  = setCurrentUserAsOwner
	windowsNtCreateFile     = windows.NtCreateFile
	windowsNtSetInformation = windows.NtSetInformationFile
)

func platformBindExisting(root string) (*Session, RootInfo, error) {
	if root == "" {
		return nil, RootInfo{}, ErrInvalidPath
	}
	clean, err := filepath.Abs(root)
	if err != nil || !filepath.IsAbs(clean) || strings.HasPrefix(clean, `\\`) {
		return nil, RootInfo{}, ErrInvalidPath
	}
	clean = filepath.Clean(clean)
	volume := filepath.VolumeName(clean)
	if len(volume) != 2 || volume[1] != ':' {
		return nil, RootInfo{}, ErrUnsupported
	}
	volumeRoot := volume + `\`
	rootPointer, pointerErr := windows.UTF16PtrFromString(volumeRoot)
	if pointerErr != nil || windows.GetDriveType(rootPointer) != windows.DRIVE_FIXED {
		return nil, RootInfo{}, ErrUnsupported
	}
	filesystem, serial, err := windowsFilesystem(volumeRoot)
	if err != nil || filesystem != "NTFS" {
		return nil, RootInfo{}, ErrUnsupported
	}
	paths, err := windowsPrefixPaths(clean)
	if err != nil {
		return nil, RootInfo{}, err
	}
	if len(paths) > maxPathComponents+1 || len(clean) > maxPathBytes {
		return nil, RootInfo{}, ErrInvalidPath
	}
	state := platformState{}
	failed := true
	defer func() {
		if failed {
			for index := len(state.data.guards) - 1; index >= 0; index-- {
				_ = state.data.guards[index].file.Close()
			}
		}
	}()
	for index, path := range paths {
		desired := uint32(windows.FILE_READ_ATTRIBUTES)
		if index == len(paths)-1 {
			desired = windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE
		}
		guard, guardErr := openWindowsAbsoluteDirectory(path, desired)
		if guardErr != nil || guard.raw.volume != uint64(serial) {
			return nil, RootInfo{}, ErrUnsafeObject
		}
		state.data.guards = append(state.data.guards, guard)
	}
	rootGuard := &state.data.guards[len(state.data.guards)-1]
	session := &Session{
		path:     clean,
		root:     &boundDirectory{file: rootGuard.file, raw: rootGuard.raw, identity: identityFromRaw(rootGuard.raw)},
		platform: state,
	}
	info := RootInfo{
		Identity: session.root.identity, Filesystem: "ntfs",
		CommitAssurance: "same_ntfs_volume_handle_relative_nt_rename_no_replace_and_directory_flush",
	}
	failed = false
	return session, info, nil
}

func platformCheckBinding(session *Session) error {
	if session == nil || session.root == nil || session.root.file == nil || len(session.platform.data.guards) == 0 {
		return ErrBindingChanged
	}
	for index := range session.platform.data.guards {
		guard := &session.platform.data.guards[index]
		named, namedErr := os.Lstat(guard.path)
		handle, handleErr := guard.file.Stat()
		raw, rawErr := windowsRawIdentity(guard.file, true)
		if namedErr != nil || handleErr != nil || rawErr != nil || windowsNamedUnsafe(named, true) ||
			!os.SameFile(named, handle) || raw != guard.raw || raw.volume != session.root.raw.volume {
			return ErrBindingChanged
		}
	}
	return nil
}

func platformCloseSession(session *Session) error {
	if session == nil {
		return nil
	}
	var first error
	for index := len(session.platform.data.guards) - 1; index >= 0; index-- {
		if file := session.platform.data.guards[index].file; file != nil {
			if err := file.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	session.platform.data.guards = nil
	if session.root != nil {
		session.root.file = nil
	}
	return first
}

func platformValidateComponent(component string) error {
	if strings.ContainsAny(component, `/\:`) || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
		return ErrInvalidPath
	}
	encoded, err := windows.UTF16FromString(component)
	if err != nil || len(encoded)-1 > 255 {
		return ErrInvalidPath
	}
	for _, value := range component {
		if value < 0x20 || value == 0x7f {
			return ErrInvalidPath
		}
	}
	base := strings.ToUpper(strings.TrimSuffix(component, filepath.Ext(component)))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$":
		return ErrInvalidPath
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return ErrInvalidPath
	}
	return nil
}

func platformCreatePrivateDirectory(session *Session, parent *boundDirectory, name string) (*boundDirectory, bool, bool, error) {
	if err := verifyWindowsParent(session, parent); err != nil {
		return nil, false, false, err
	}
	descriptor, err := windowsPrivateSecurityDescriptor()
	if err != nil {
		return nil, false, false, fmt.Errorf("create private directory failed")
	}
	handle, err := ntOpenWindowsRelative(windows.Handle(parent.file.Fd()), name,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.DELETE|windows.READ_CONTROL|windows.WRITE_OWNER,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_CREATE, windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_WRITE_THROUGH, descriptor)
	if err != nil {
		if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
			return nil, false, false, ErrAlreadyExists
		}
		return nil, false, false, fmt.Errorf("create private directory failed")
	}
	file := os.NewFile(uintptr(handle), "fsbind-private-directory")
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, true, false, fmt.Errorf("wrap private directory failed")
	}
	if windowsSetPrivateOwner(handle) != nil || validateWindowsPrivateHandle(handle, true) != nil {
		_ = file.Close()
		return nil, true, false, ErrUnsafeObject
	}
	raw, err := windowsRawHandleIdentity(handle, true)
	if err != nil || raw.volume != session.root.raw.volume {
		_ = file.Close()
		return nil, true, false, ErrCrossFilesystem
	}
	directory := &boundDirectory{file: file, raw: raw, identity: identityFromRaw(raw)}
	if platformSyncDirectory(directory) != nil || platformSyncDirectory(parent) != nil {
		_ = file.Close()
		return directory, true, false, ErrDurabilityUnconfirmed
	}
	return directory, true, true, nil
}

func platformOpenPrivateDirectory(session *Session, parent *boundDirectory, name string) (*boundDirectory, error) {
	if err := verifyWindowsParent(session, parent); err != nil {
		return nil, err
	}
	handle, err := ntOpenWindowsRelative(windows.Handle(parent.file.Fd()), name,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.DELETE|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT, nil)
	if err != nil {
		if windowsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, ErrUnsafeObject
	}
	file := os.NewFile(uintptr(handle), "fsbind-private-directory")
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, ErrUnsafeObject
	}
	if validateWindowsPrivateHandle(handle, true) != nil {
		_ = file.Close()
		return nil, ErrUnsafeObject
	}
	raw, rawErr := windowsRawHandleIdentity(handle, true)
	if rawErr != nil || raw.volume != session.root.raw.volume {
		_ = file.Close()
		return nil, ErrCrossFilesystem
	}
	return &boundDirectory{file: file, raw: raw, identity: identityFromRaw(raw)}, nil
}

func platformCreatePrivateRegular(session *Session, parent *boundDirectory, name string) (*os.File, rawIdentity, error) {
	return openWindowsPrivateRegular(session, parent, name, true, false)
}

func platformOpenPrivateRegular(session *Session, parent *boundDirectory, name string) (*os.File, rawIdentity, error) {
	return openWindowsPrivateRegular(session, parent, name, false, false)
}

func platformOpenPrivateRegularReadOnly(session *Session, parent *boundDirectory, name string) (*os.File, rawIdentity, error) {
	return openWindowsRelativeTyped(session, parent, name, false, windows.FILE_GENERIC_READ|windows.READ_CONTROL)
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
	return inspectWindowsObject(session, parent, name, true)
}

func platformInspectRootObject(session *Session, name string) (*os.File, rawIdentity, ObjectKind, error) {
	return inspectWindowsObject(session, session.root, name, false)
}

func inspectWindowsObject(session *Session, parent *boundDirectory, name string, requirePrivate bool) (*os.File, rawIdentity, ObjectKind, error) {
	if err := verifyWindowsParent(session, parent); err != nil {
		return nil, rawIdentity{}, "", err
	}
	access := uint32(windows.FILE_READ_ATTRIBUTES)
	if requirePrivate {
		access |= windows.READ_CONTROL
	}
	handle, err := ntOpenWindowsRelative(windows.Handle(parent.file.Fd()), name, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN, windows.FILE_OPEN_REPARSE_POINT, nil)
	if err != nil {
		if windowsNotFound(err) {
			return nil, rawIdentity{}, "", ErrNotFound
		}
		return nil, rawIdentity{}, "", ErrUnsafeObject
	}
	file := os.NewFile(uintptr(handle), "fsbind-inspected-object")
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, rawIdentity{}, "", ErrUnsafeObject
	}
	var information windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(handle, &information) != nil || information.FileAttributes&forbiddenWindowsAttributes != 0 {
		_ = file.Close()
		return nil, rawIdentity{}, "", ErrUnsafeObject
	}
	wantDirectory := information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	raw, rawErr := windowsRawHandleIdentity(handle, wantDirectory)
	if rawErr != nil || raw.volume != session.root.raw.volume || (requirePrivate && validateWindowsPrivateHandle(handle, wantDirectory) != nil) {
		_ = file.Close()
		return nil, rawIdentity{}, "", ErrUnsafeObject
	}
	kind := ObjectKindRegular
	if wantDirectory {
		kind = ObjectKindDirectory
	}
	return file, raw, kind, nil
}

func platformAcquireOperationLock(session *Session, directory *boundDirectory, create bool) (*os.File, rawIdentity, error) {
	if err := verifyWindowsParent(session, directory); err != nil {
		return nil, rawIdentity{}, err
	}
	file, raw, err := openWindowsPrivateRegular(session, directory, operationLockName, create, true)
	if create && errors.Is(err, ErrAlreadyExists) {
		file, raw, err = openWindowsPrivateRegular(session, directory, operationLockName, false, true)
	}
	return file, raw, err
}

func platformCheckOperationLock(session *Session, _ *boundDirectory, file *os.File, expected rawIdentity) error {
	if file == nil || validateWindowsPrivateHandle(windows.Handle(file.Fd()), false) != nil {
		return ErrBindingChanged
	}
	raw, err := windowsRawHandleIdentity(windows.Handle(file.Fd()), false)
	if err != nil || raw != expected || raw.volume != session.root.raw.volume {
		return ErrBindingChanged
	}
	// The lock handle is opened with share mode zero, so Windows itself keeps
	// this exact named file from being renamed, deleted, or reopened meanwhile.
	return nil
}

func platformReleaseOperationLock(file *os.File, _ *boundDirectory) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func platformSyncFile(file *os.File) error {
	if file == nil || file.Sync() != nil {
		return fmt.Errorf("sync bound regular file failed")
	}
	return nil
}

func platformSyncDirectory(directory *boundDirectory) error {
	if directory == nil || directory.file == nil || windowsFlushFileBuffers(windows.Handle(directory.file.Fd())) != nil {
		return fmt.Errorf("sync bound directory failed")
	}
	return nil
}

func platformReadDirectory(directory *boundDirectory, maximum int) ([]os.DirEntry, error) {
	if directory == nil || directory.file == nil || maximum <= 0 {
		return nil, ErrUnsafeObject
	}
	handle, err := ntOpenWindowsRelative(windows.Handle(directory.file.Fd()), "", windows.FILE_GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT, nil)
	if err != nil {
		return nil, fmt.Errorf("read bound directory failed")
	}
	file := os.NewFile(uintptr(handle), "fsbind-directory-list")
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("wrap bound directory failed")
	}
	raw, rawErr := windowsRawHandleIdentity(handle, true)
	if rawErr != nil || raw != directory.raw {
		_ = file.Close()
		return nil, ErrUnsafeObject
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
	if verifyWindowsParent(session, sourceParent) != nil || verifyWindowsParent(session, destinationParent) != nil || sourceParent.raw.volume != destinationParent.raw.volume {
		return false, false, rawIdentity{}, ErrCrossFilesystem
	}
	existing, _, _, destinationErr := inspectWindowsObject(session, destinationParent, destinationName, false)
	if existing != nil {
		_ = existing.Close()
	}
	if destinationErr == nil || !errors.Is(destinationErr, ErrNotFound) {
		if destinationErr == nil || errors.Is(destinationErr, ErrUnsafeObject) {
			return false, false, rawIdentity{}, ErrAlreadyExists
		}
		return false, false, rawIdentity{}, destinationErr
	}
	source, actual, err := openWindowsRelativeTyped(session, sourceParent, sourceName, wantDirectory,
		windows.FILE_GENERIC_READ|windows.DELETE|windows.READ_CONTROL)
	if err != nil || actual != expected {
		if source != nil {
			_ = source.Close()
		}
		return false, false, rawIdentity{}, ErrUnsafeObject
	}
	renameErr := windowsRenameRelative(windows.Handle(source.Fd()), windows.Handle(destinationParent.file.Fd()), destinationName)
	_ = source.Close()
	if renameErr == nil {
		final, finalRaw, finalErr := platformOpenAnySource(session, destinationParent, destinationName, wantDirectory)
		if final != nil {
			_ = final.Close()
		}
		if finalErr != nil || finalRaw != expected {
			return true, false, expected, ErrPublicationAmbiguous
		}
		if platformSyncDirectory(destinationParent) != nil {
			return true, false, finalRaw, ErrDurabilityUnconfirmed
		}
		if sourceParent.raw != destinationParent.raw && platformSyncDirectory(sourceParent) != nil {
			return true, false, finalRaw, ErrDurabilityUnconfirmed
		}
		return true, true, finalRaw, nil
	}
	if errors.Is(renameErr, windows.STATUS_OBJECT_NAME_COLLISION) || errors.Is(renameErr, windows.ERROR_ALREADY_EXISTS) || errors.Is(renameErr, windows.ERROR_FILE_EXISTS) {
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
		return false, false, rawIdentity{}, ErrPublicationAmbiguous
	}
	return false, false, rawIdentity{}, ErrPublicationAmbiguous
}

func platformRemoveObject(session *Session, parent *boundDirectory, name string, wantDirectory bool, expected rawIdentity, expectedSize int64) (bool, bool, rawIdentity, error) {
	if verifyWindowsParent(session, parent) != nil {
		return false, false, rawIdentity{}, ErrCrossFilesystem
	}
	source, actual, err := openWindowsRelativeTyped(session, parent, name, wantDirectory,
		windows.FILE_GENERIC_READ|windows.DELETE|windows.READ_CONTROL)
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
	removeErr := markWindowsCreatedForDeletion(windows.Handle(source.Fd()))
	closeErr := source.Close()
	remaining, remainingRaw, remainingErr := platformOpenAnySource(session, parent, name, wantDirectory)
	if remaining != nil {
		_ = remaining.Close()
	}
	if removeErr == nil && closeErr == nil {
		if errors.Is(remainingErr, ErrNotFound) {
			if platformSyncDirectory(parent) != nil {
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

func openWindowsPrivateRegular(session *Session, parent *boundDirectory, name string, create, exclusiveLock bool) (*os.File, rawIdentity, error) {
	if err := verifyWindowsParent(session, parent); err != nil {
		return nil, rawIdentity{}, err
	}
	access := uint32(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.DELETE | windows.READ_CONTROL)
	disposition := uint32(windows.FILE_OPEN)
	var descriptor *windows.SECURITY_DESCRIPTOR
	if create {
		disposition = windows.FILE_CREATE
		access |= windows.WRITE_OWNER
		var err error
		descriptor, err = windowsPrivateSecurityDescriptor()
		if err != nil {
			return nil, rawIdentity{}, fmt.Errorf("build private file security failed")
		}
	}
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_DELETE)
	if exclusiveLock {
		share = 0
	}
	handle, err := ntOpenWindowsRelative(windows.Handle(parent.file.Fd()), name, access, share, disposition,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_WRITE_THROUGH, descriptor)
	if err != nil {
		if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
			return nil, rawIdentity{}, ErrAlreadyExists
		}
		if windowsNotFound(err) {
			return nil, rawIdentity{}, ErrNotFound
		}
		if errors.Is(err, windows.STATUS_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return nil, rawIdentity{}, ErrBusy
		}
		return nil, rawIdentity{}, ErrUnsafeObject
	}
	file := os.NewFile(uintptr(handle), "fsbind-private-file")
	if file == nil {
		if create {
			_ = markWindowsCreatedForDeletion(handle)
		}
		_ = windows.CloseHandle(handle)
		return nil, rawIdentity{}, ErrUnsafeObject
	}
	if create && windowsSetPrivateOwner(handle) != nil {
		_ = markWindowsCreatedForDeletion(handle)
		_ = file.Close()
		return nil, rawIdentity{}, fmt.Errorf("assign private file owner failed")
	}
	if validateWindowsPrivateHandle(handle, false) != nil {
		if create {
			_ = markWindowsCreatedForDeletion(handle)
		}
		_ = file.Close()
		return nil, rawIdentity{}, ErrUnsafeObject
	}
	raw, rawErr := windowsRawHandleIdentity(handle, false)
	if rawErr != nil || raw.volume != session.root.raw.volume {
		if create {
			_ = markWindowsCreatedForDeletion(handle)
		}
		_ = file.Close()
		return nil, rawIdentity{}, ErrCrossFilesystem
	}
	return file, raw, nil
}

func markWindowsCreatedForDeletion(handle windows.Handle) error {
	information := windowsFileDispositionInformation{DeleteFile: 1}
	return windows.SetFileInformationByHandle(handle, windows.FileDispositionInfo,
		(*byte)(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information)))
}

func openWindowsRelativeTyped(session *Session, parent *boundDirectory, name string, wantDirectory bool, access uint32) (*os.File, rawIdentity, error) {
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_WRITE_THROUGH)
	if wantDirectory {
		options |= windows.FILE_DIRECTORY_FILE
	} else {
		options |= windows.FILE_NON_DIRECTORY_FILE
	}
	handle, err := ntOpenWindowsRelative(windows.Handle(parent.file.Fd()), name, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN, options, nil)
	if err != nil {
		if windowsNotFound(err) {
			return nil, rawIdentity{}, ErrNotFound
		}
		return nil, rawIdentity{}, ErrUnsafeObject
	}
	file := os.NewFile(uintptr(handle), "fsbind-publication-source")
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, rawIdentity{}, ErrUnsafeObject
	}
	if validateWindowsPrivateHandle(handle, wantDirectory) != nil {
		_ = file.Close()
		return nil, rawIdentity{}, ErrUnsafeObject
	}
	raw, rawErr := windowsRawHandleIdentity(handle, wantDirectory)
	if rawErr != nil || raw.volume != session.root.raw.volume {
		_ = file.Close()
		return nil, rawIdentity{}, ErrCrossFilesystem
	}
	return file, raw, nil
}

func verifyWindowsParent(session *Session, parent *boundDirectory) error {
	if session == nil || parent == nil || parent.file == nil || parent.raw.volume != session.root.raw.volume {
		return ErrCrossFilesystem
	}
	raw, rawErr := windowsRawIdentity(parent.file, true)
	if rawErr != nil || raw != parent.raw {
		return ErrUnsafeObject
	}
	return nil
}

func openWindowsAbsoluteDirectory(path string, desired uint32) (windowsGuard, error) {
	pointer, err := windows.UTF16PtrFromString(windowsAPIPath(path))
	if err != nil {
		return windowsGuard{}, ErrInvalidPath
	}
	handle, err := windows.CreateFile(pointer, desired,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return windowsGuard{}, ErrNotFound
		}
		return windowsGuard{}, fmt.Errorf("open bound directory failed")
	}
	file := os.NewFile(uintptr(handle), "fsbind-directory")
	if file == nil {
		_ = windows.CloseHandle(handle)
		return windowsGuard{}, fmt.Errorf("wrap bound directory failed")
	}
	named, namedErr := os.Lstat(path)
	info, statErr := file.Stat()
	raw, rawErr := windowsRawIdentity(file, true)
	if namedErr != nil || statErr != nil || rawErr != nil || windowsNamedUnsafe(named, true) || !os.SameFile(named, info) {
		_ = file.Close()
		return windowsGuard{}, ErrUnsafeObject
	}
	return windowsGuard{path: path, file: file, raw: raw}, nil
}

func ntOpenWindowsRelative(parent windows.Handle, name string, access, share, disposition, options uint32, descriptor *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, ErrInvalidPath
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: parent,
		ObjectName: objectName, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		SecurityDescriptor: descriptor,
	}
	var status windows.IO_STATUS_BLOCK
	var handle windows.Handle
	err = windowsNtCreateFile(&handle, access|windows.SYNCHRONIZE, &attributes, &status, nil, windows.FILE_ATTRIBUTE_NORMAL,
		share, disposition, options|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	runtime.KeepAlive(objectName)
	runtime.KeepAlive(descriptor)
	return handle, err
}

func renameWindowsRelative(source, destinationDirectory windows.Handle, destinationName string) error {
	utf16Name, err := windows.UTF16FromString(destinationName)
	if err != nil {
		return ErrInvalidPath
	}
	nameLength := (len(utf16Name) - 1) * 2
	var dummy windowsFileRenameInformation
	bufferSize := int(unsafe.Offsetof(dummy.FileName)) + nameLength
	if minimum := int(unsafe.Sizeof(dummy)); bufferSize < minimum {
		bufferSize = minimum
	}
	buffer := make([]byte, bufferSize)
	information := (*windowsFileRenameInformation)(unsafe.Pointer(&buffer[0]))
	information.RootDirectory = destinationDirectory
	information.FileNameLength = uint32(nameLength)
	copy((*[windows.MAX_LONG_PATH]uint16)(unsafe.Pointer(&information.FileName[0]))[:nameLength/2:nameLength/2], utf16Name[:len(utf16Name)-1])
	var status windows.IO_STATUS_BLOCK
	err = windowsNtSetInformation(source, &status, &buffer[0], uint32(len(buffer)), windows.FileRenameInformation)
	runtime.KeepAlive(buffer)
	return err
}

func windowsNotFound(err error) bool {
	return errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) || errors.Is(err, windows.STATUS_OBJECT_PATH_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

func windowsRawIdentity(file *os.File, wantDirectory bool) (rawIdentity, error) {
	if file == nil {
		return rawIdentity{}, ErrUnsafeObject
	}
	return windowsRawHandleIdentity(windows.Handle(file.Fd()), wantDirectory)
}

func windowsRawHandleIdentity(handle windows.Handle, wantDirectory bool) (rawIdentity, error) {
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return rawIdentity{}, ErrUnsafeObject
	}
	if information.FileAttributes&forbiddenWindowsAttributes != 0 || (information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != wantDirectory {
		return rawIdentity{}, ErrUnsafeObject
	}
	if !wantDirectory && information.NumberOfLinks != 1 {
		return rawIdentity{}, ErrUnsafeObject
	}
	return rawIdentity{
		volume: uint64(information.VolumeSerialNumber), fileHigh: uint64(information.FileIndexHigh),
		fileLow: uint64(information.FileIndexLow), directory: wantDirectory,
	}, nil
}

func windowsNamedUnsafe(info os.FileInfo, wantDirectory bool) bool {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || info.IsDir() != wantDirectory || (!wantDirectory && !info.Mode().IsRegular()) {
		return true
	}
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return !ok || data.FileAttributes&forbiddenWindowsAttributes != 0
}

func validateWindowsPrivateHandle(handle windows.Handle, wantDirectory bool) error {
	if _, err := windowsRawHandleIdentity(handle, wantDirectory); err != nil {
		return err
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return ErrUnsafeObject
	}
	owner, _, err := descriptor.Owner()
	current, currentErr := currentUserSID()
	if err != nil || currentErr != nil || owner == nil || !owner.Equals(current) {
		return ErrUnsafeObject
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return ErrUnsafeObject
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return ErrUnsafeObject
	}
	var first *windows.ACCESS_ALLOWED_ACE
	if windows.GetAce(dacl, 0, &first) != nil || first == nil || first.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || first.Header.AceFlags&windows.INHERITED_ACE != 0 || first.Mask == 0 {
		return ErrUnsafeObject
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&first.SidStart))
	if !aceSID.IsValid() || !aceSID.Equals(current) {
		return ErrUnsafeObject
	}
	var second *windows.ACCESS_ALLOWED_ACE
	if windows.GetAce(dacl, 1, &second) == nil {
		return ErrUnsafeObject
	}
	return nil
}

func windowsPrivateSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	current, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + current.String() + ")")
}

func setCurrentUserAsOwner(handle windows.Handle) error {
	current, err := currentUserSID()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, current, nil, nil, nil)
}

func currentUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return nil, ErrUnsafeObject
	}
	return user.User.Sid.Copy()
}

func windowsFilesystem(root string) (string, uint32, error) {
	pointer, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return "", 0, err
	}
	filesystem := make([]uint16, 32)
	var serial uint32
	if err := windows.GetVolumeInformation(pointer, nil, 0, &serial, nil, nil, &filesystem[0], uint32(len(filesystem))); err != nil {
		return "", 0, err
	}
	return windows.UTF16ToString(filesystem), serial, nil
}

func windowsPrefixPaths(root string) ([]string, error) {
	volume := filepath.VolumeName(root)
	volumeRoot := volume + `\`
	relative, err := filepath.Rel(volumeRoot, root)
	if err != nil || relative == ".." || strings.HasPrefix(relative, `..\`) {
		return nil, ErrInvalidPath
	}
	paths := []string{volumeRoot}
	if relative == "." {
		return paths, nil
	}
	current := volumeRoot
	for _, component := range strings.Split(relative, `\`) {
		if err := validateSingleComponent(component); err != nil {
			return nil, err
		}
		current = filepath.Join(current, component)
		paths = append(paths, current)
	}
	return paths, nil
}

func windowsAPIPath(path string) string {
	if strings.HasPrefix(path, `\\?\`) {
		return path
	}
	return `\\?\` + path
}
