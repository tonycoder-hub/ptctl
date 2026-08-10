//go:build windows

package metastore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

type windowsDirectoryGuard struct {
	path string
	file *os.File
	info os.FileInfo
}

type platformSessionState struct {
	guards []windowsDirectoryGuard
}

func platformOpenSessionState(rootPath string) (platformSessionState, error) {
	var state platformSessionState
	failed := true
	defer func() {
		if failed {
			_ = platformCloseSessionState(&state)
		}
	}()
	paths, err := windowsGuardPaths(rootPath)
	if err != nil {
		return state, err
	}
	for _, path := range paths {
		private := sameWindowsPath(path, rootPath) || sameWindowsPath(path, filepath.Join(rootPath, objectsDir)) || sameWindowsPath(path, filepath.Join(rootPath, temporaryDir))
		guard, err := openWindowsDirectoryGuard(path, private)
		if err != nil {
			return state, err
		}
		state.guards = append(state.guards, guard)
	}
	if len(state.guards) == 0 {
		return state, fmt.Errorf("store directory guards are unavailable")
	}
	rootGuard := state.guard(rootPath)
	if rootGuard == nil {
		return state, fmt.Errorf("store root guard is unavailable")
	}
	rootVolume, err := sessionFilesystemIdentity(rootGuard.file)
	if err != nil {
		return state, err
	}
	for index := range state.guards {
		volume, volumeErr := sessionFilesystemIdentity(state.guards[index].file)
		if volumeErr != nil || volume != rootVolume {
			return state, fmt.Errorf("store path crossed its reviewed volume")
		}
	}
	failed = false
	return state, nil
}

func platformCloseSessionState(state *platformSessionState) error {
	if state == nil {
		return nil
	}
	var first error
	for index := len(state.guards) - 1; index >= 0; index-- {
		if state.guards[index].file != nil {
			if err := state.guards[index].file.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	state.guards = nil
	return first
}

func platformAttachSessionDirectoryGuard(session *rootSession, name string) error {
	if session == nil || (name != objectsDir && name != temporaryDir) {
		return fmt.Errorf("store directory guard target is invalid")
	}
	path := filepath.Join(session.path, name)
	if session.platform.guard(path) != nil {
		return nil
	}
	guard, err := openWindowsDirectoryGuard(path, true)
	if err != nil {
		return err
	}
	volume, volumeErr := sessionFilesystemIdentity(guard.file)
	if volumeErr != nil || volume != session.filesystem {
		_ = guard.file.Close()
		return fmt.Errorf("store subdirectory crossed its reviewed volume")
	}
	session.platform.guards = append(session.platform.guards, guard)
	return nil
}

func platformSessionOpenRootDirectory(session *rootSession, name string) (*os.File, error) {
	if session == nil || (name != objectsDir && name != temporaryDir) || session.platform.guard(filepath.Join(session.path, name)) == nil {
		return nil, fmt.Errorf("bound store subdirectory is unavailable")
	}
	return platformSessionOpen(session, name, true)
}

func platformSessionCreatePrivateDirectory(session *rootSession, name string) error {
	if session == nil || (name != objectsDir && name != temporaryDir) {
		return fmt.Errorf("private directory name is invalid")
	}
	return platformCreatePrivateDirectory(filepath.Join(session.path, name))
}

func platformFilesystemIdentity(file *os.File) (platformFilesystemID, error) {
	if file == nil {
		return platformFilesystemID{}, fmt.Errorf("filesystem handle is unavailable")
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &information); err != nil {
		return platformFilesystemID{}, fmt.Errorf("filesystem identity is unavailable")
	}
	return platformFilesystemID{volume: uint64(information.VolumeSerialNumber)}, nil
}

func platformVerifySessionBinding(session *rootSession) error {
	if session == nil || session.root == nil || session.rootDirectory == nil {
		return fmt.Errorf("bound store session is incomplete")
	}
	if err := platformValidateStoreLocation(session.path, false); err != nil {
		return err
	}
	for index := range session.platform.guards {
		guard := &session.platform.guards[index]
		named, namedErr := os.Lstat(guard.path)
		handle, handleErr := guard.file.Stat()
		if namedErr != nil || handleErr != nil || platformRejectNamedInfo(named) || !named.IsDir() || !os.SameFile(named, guard.info) || !os.SameFile(named, handle) {
			return fmt.Errorf("guarded store path identity changed")
		}
		volume, volumeErr := sessionFilesystemIdentity(guard.file)
		if volumeErr != nil || volume != session.filesystem {
			return fmt.Errorf("guarded store path crossed its reviewed volume")
		}
	}
	items := []struct {
		path string
		file *os.File
	}{{session.path, session.rootDirectory}}
	if session.objectsDirectory != nil {
		items = append(items, struct {
			path string
			file *os.File
		}{filepath.Join(session.path, objectsDir), session.objectsDirectory})
	}
	if session.temporaryDirectory != nil {
		items = append(items, struct {
			path string
			file *os.File
		}{filepath.Join(session.path, temporaryDir), session.temporaryDirectory})
	}
	for _, item := range items {
		guard := session.platform.guard(item.path)
		info, err := item.file.Stat()
		if guard == nil || err != nil || !os.SameFile(guard.info, info) || platformValidateOpenFile(item.file, true) != nil {
			return fmt.Errorf("bound private directory identity changed")
		}
		if err := session.validateFilesystem(item.file); err != nil {
			return err
		}
	}
	return nil
}

func platformSessionOpen(session *rootSession, relative string, wantDirectory bool) (*os.File, error) {
	if _, err := windowsSessionPath(session, relative); err != nil {
		return nil, err
	}
	named, err := session.root.Lstat(relative)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errArtifactNotFound
		}
		return nil, fmt.Errorf("named bound store object is unavailable or unsafe")
	}
	if platformRejectNamedInfo(named) || named.IsDir() != wantDirectory || (!wantDirectory && !named.Mode().IsRegular()) {
		return nil, fmt.Errorf("named bound store object is unavailable or unsafe")
	}
	file, err := session.root.Open(relative)
	if err != nil {
		return nil, fmt.Errorf("open bound store object failed")
	}
	handle, err := file.Stat()
	if err != nil || !os.SameFile(named, handle) {
		_ = file.Close()
		return nil, fmt.Errorf("bound store object identity changed")
	}
	return file, nil
}

func platformSessionCreatePrivateFile(session *rootSession, relative string) (*os.File, error) {
	absolute, err := windowsSessionPath(session, relative)
	if err != nil || filepath.Dir(relative) != temporaryDir {
		return nil, fmt.Errorf("private staging name is invalid")
	}
	file, err := platformCreatePrivateFile(absolute)
	if err != nil {
		return nil, err
	}
	bound, boundInfo, boundErr := session.openValidated(relative, false)
	createdInfo, statErr := file.Stat()
	if boundErr != nil || statErr != nil || !os.SameFile(boundInfo, createdInfo) {
		if bound != nil {
			_ = bound.Close()
		}
		_ = file.Close()
		_ = platformSessionRemovePrivate(session, relative)
		return nil, fmt.Errorf("created staging object escaped its bound root")
	}
	_ = bound.Close()
	return file, nil
}

func platformSessionPublishNoReplace(session *rootSession, temporaryRelative, finalRelative string) (bool, error) {
	fromPath, err := windowsSessionPath(session, temporaryRelative)
	if err != nil || filepath.Dir(temporaryRelative) != temporaryDir {
		return false, fmt.Errorf("private staging name is invalid")
	}
	toPath, err := windowsSessionPath(session, finalRelative)
	if err != nil || (filepath.Dir(finalRelative) != objectsDir && filepath.Dir(finalRelative) != ".") {
		return false, fmt.Errorf("private object name is invalid")
	}
	source, sourceInfo, err := session.openValidated(temporaryRelative, false)
	if err != nil {
		return false, fmt.Errorf("staging object identity is unsafe")
	}
	_ = source.Close()
	from, err := windows.UTF16PtrFromString(windowsAPIPath(fromPath))
	if err != nil {
		return false, fmt.Errorf("staging path encoding failed")
	}
	to, err := windows.UTF16PtrFromString(windowsAPIPath(toPath))
	if err != nil {
		return false, fmt.Errorf("object path encoding failed")
	}
	moveErr := windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
	if moveErr == nil {
		final, finalInfo, finalErr := session.openValidated(finalRelative, false)
		if final != nil {
			_ = final.Close()
		}
		if finalErr != nil || !os.SameFile(sourceInfo, finalInfo) {
			return true, fmt.Errorf("published object identity changed")
		}
		return true, nil
	}
	if errors.Is(moveErr, windows.ERROR_ALREADY_EXISTS) || errors.Is(moveErr, windows.ERROR_FILE_EXISTS) {
		return false, nil
	}
	remainingSource, _, sourceErr := session.openValidated(temporaryRelative, false)
	if remainingSource != nil {
		_ = remainingSource.Close()
	}
	final, finalInfo, finalErr := session.openValidated(finalRelative, false)
	if final != nil {
		_ = final.Close()
	}
	if errors.Is(sourceErr, errArtifactNotFound) && finalErr == nil && os.SameFile(sourceInfo, finalInfo) {
		return true, fmt.Errorf("%w", ErrDurabilityUnconfirmed)
	}
	return false, fmt.Errorf("move private object failed")
}

func platformSessionRemovePrivate(session *rootSession, relative string) error {
	absolute, err := windowsSessionPath(session, relative)
	if err != nil || filepath.Dir(relative) != temporaryDir {
		return fmt.Errorf("private staging name is invalid")
	}
	err = os.Remove(absolute)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("remove private staging file failed")
}

func platformConfirmPrivateStoreLayout(*rootSession) error {
	// MoveFileEx WRITE_THROUGH is the explicitly reported Windows publication
	// boundary; no POSIX-style directory fsync guarantee is claimed.
	return nil
}

func openWindowsDirectoryGuard(path string, private bool) (windowsDirectoryGuard, error) {
	pointer, err := windows.UTF16PtrFromString(windowsAPIPath(path))
	if err != nil {
		return windowsDirectoryGuard{}, fmt.Errorf("store guard path encoding failed")
	}
	desiredAccess := uint32(windows.FILE_READ_ATTRIBUTES)
	if private {
		desiredAccess |= windows.READ_CONTROL
	}
	handle, err := windows.CreateFile(
		pointer,
		desiredAccess,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return windowsDirectoryGuard{}, fmt.Errorf("open store directory guard failed")
	}
	file := os.NewFile(uintptr(handle), "private-metastore-directory-guard")
	if file == nil {
		_ = windows.CloseHandle(handle)
		return windowsDirectoryGuard{}, fmt.Errorf("wrap store directory guard failed")
	}
	named, namedErr := os.Lstat(path)
	info, statErr := file.Stat()
	if namedErr != nil || statErr != nil || platformRejectNamedInfo(named) || !named.IsDir() || !os.SameFile(named, info) || validateWindowsHandleAttributes(handle, true) != nil {
		_ = file.Close()
		return windowsDirectoryGuard{}, fmt.Errorf("store directory guard identity is unsafe")
	}
	if private && platformValidateOpenFile(file, true) != nil {
		_ = file.Close()
		return windowsDirectoryGuard{}, fmt.Errorf("private store directory guard is unsafe")
	}
	return windowsDirectoryGuard{path: path, file: file, info: info}, nil
}

func windowsGuardPaths(rootPath string) ([]string, error) {
	clean := filepath.Clean(rootPath)
	volume := filepath.VolumeName(clean)
	if volume == "" {
		return nil, fmt.Errorf("store guard volume is unavailable")
	}
	volumeRoot := volume + `\`
	relative, err := filepath.Rel(volumeRoot, clean)
	if err != nil || relative == "." || strings.HasPrefix(relative, `..\`) {
		return nil, fmt.Errorf("store guard path is invalid")
	}
	paths := []string{volumeRoot}
	current := volumeRoot
	for _, component := range strings.Split(relative, `\`) {
		if component == "" || component == "." || component == ".." {
			return nil, fmt.Errorf("store guard path is invalid")
		}
		current = filepath.Join(current, component)
		paths = append(paths, current)
	}
	return paths, nil
}

func windowsSessionPath(session *rootSession, relative string) (string, error) {
	if session == nil || relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) != relative {
		return "", fmt.Errorf("bound store relative name is invalid")
	}
	absolute := filepath.Join(session.path, relative)
	check, err := filepath.Rel(session.path, absolute)
	if err != nil || check != relative || check == ".." || strings.HasPrefix(check, `..\`) {
		return "", fmt.Errorf("bound store relative name is invalid")
	}
	return absolute, nil
}

func (state *platformSessionState) guard(path string) *windowsDirectoryGuard {
	if state == nil {
		return nil
	}
	for index := range state.guards {
		if sameWindowsPath(state.guards[index].path, path) {
			return &state.guards[index]
		}
	}
	return nil
}

func sameWindowsPath(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}
