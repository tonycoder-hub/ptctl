//go:build linux || darwin

package metastore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type platformSessionState struct {
	parentDirectory *os.File
	rootName        string
}

func platformOpenSessionState(path string) (platformSessionState, error) {
	parentPath := filepath.Dir(path)
	rootName := filepath.Base(path)
	if rootName == "" || rootName == "." || rootName == ".." {
		return platformSessionState{}, fmt.Errorf("store parent binding is unavailable")
	}
	fd, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return platformSessionState{}, fmt.Errorf("store parent binding is unavailable")
	}
	parent := os.NewFile(uintptr(fd), "private-metastore-parent")
	if parent == nil {
		_ = unix.Close(fd)
		return platformSessionState{}, fmt.Errorf("wrap store parent binding failed")
	}
	return platformSessionState{parentDirectory: parent, rootName: rootName}, nil
}

func platformCloseSessionState(state *platformSessionState) error {
	if state == nil || state.parentDirectory == nil {
		return nil
	}
	err := state.parentDirectory.Close()
	state.parentDirectory = nil
	return err
}

func platformAttachSessionDirectoryGuard(*rootSession, string) error { return nil }

func platformSessionOpenRootDirectory(session *rootSession, name string) (*os.File, error) {
	if session == nil || session.rootDirectory == nil || (name != objectsDir && name != temporaryDir) {
		return nil, fmt.Errorf("bound store subdirectory is unavailable")
	}
	fd, err := unix.Openat(int(session.rootDirectory.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, errArtifactNotFound
		}
		return nil, fmt.Errorf("open bound store subdirectory failed")
	}
	file := os.NewFile(uintptr(fd), "private-metastore-directory")
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("wrap bound store subdirectory failed")
	}
	return file, nil
}

func platformSessionCreatePrivateDirectory(session *rootSession, name string) error {
	if session == nil || session.rootDirectory == nil || (name != objectsDir && name != temporaryDir) {
		return fmt.Errorf("private directory name is invalid")
	}
	if err := unix.Mkdirat(int(session.rootDirectory.Fd()), name, 0o700); err != nil {
		if errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("%w", errPrivateDirectoryAlreadyExists)
		}
		return fmt.Errorf("create private directory failed")
	}
	file, err := platformSessionOpenRootDirectory(session, name)
	if err != nil {
		return fmt.Errorf("open created private directory failed")
	}
	if err := unix.Fchmod(int(file.Fd()), 0o700); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure private directory failed")
	}
	if err := platformValidateOpenFile(file, true); err != nil {
		_ = file.Close()
		return err
	}
	if err := session.validateFilesystem(file); err != nil {
		_ = file.Close()
		return err
	}
	if err := unix.Fsync(int(file.Fd())); err != nil {
		_ = file.Close()
		return fmt.Errorf("flush private directory failed")
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close private directory failed")
	}
	if err := unix.Fsync(int(session.rootDirectory.Fd())); err != nil {
		return fmt.Errorf("confirm private directory entry failed")
	}
	return nil
}

func platformVerifySessionBinding(session *rootSession) error {
	if session == nil || session.root == nil || session.rootDirectory == nil || session.platform.parentDirectory == nil {
		return fmt.Errorf("bound store session is incomplete")
	}
	if err := platformValidateStoreLocation(session.path, false); err != nil {
		return err
	}
	namedRoot, err := os.Lstat(session.path)
	handleRoot, handleErr := session.rootDirectory.Stat()
	if err != nil || handleErr != nil || platformRejectNamedInfo(namedRoot) || !namedRoot.IsDir() || !os.SameFile(namedRoot, handleRoot) || platformValidateOpenFile(session.rootDirectory, true) != nil {
		return fmt.Errorf("store root name no longer identifies its bound handle")
	}
	if err := session.validateFilesystem(session.rootDirectory); err != nil {
		return err
	}
	parentRootFD, parentErr := unix.Openat(int(session.platform.parentDirectory.Fd()), session.platform.rootName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if parentErr != nil {
		return fmt.Errorf("store root parent binding changed")
	}
	parentRoot := os.NewFile(uintptr(parentRootFD), "private-metastore-parent-child")
	if parentRoot == nil {
		_ = unix.Close(parentRootFD)
		return fmt.Errorf("wrap store root parent binding failed")
	}
	parentRootInfo, parentStatErr := parentRoot.Stat()
	parentCloseErr := parentRoot.Close()
	if parentStatErr != nil || parentCloseErr != nil || !os.SameFile(parentRootInfo, handleRoot) {
		return fmt.Errorf("store root parent binding changed")
	}
	items := []struct {
		name string
		file *os.File
	}{}
	if session.objectsDirectory != nil {
		items = append(items, struct {
			name string
			file *os.File
		}{objectsDir, session.objectsDirectory})
	}
	if session.temporaryDirectory != nil {
		items = append(items, struct {
			name string
			file *os.File
		}{temporaryDir, session.temporaryDirectory})
	}
	for _, item := range items {
		named, namedErr := session.root.Lstat(item.name)
		handle, statErr := item.file.Stat()
		if namedErr != nil || statErr != nil || platformRejectNamedInfo(named) || !named.IsDir() || !os.SameFile(named, handle) || platformValidateOpenFile(item.file, true) != nil {
			return fmt.Errorf("store subdirectory identity changed")
		}
		if err := session.validateFilesystem(item.file); err != nil {
			return err
		}
	}
	return nil
}

func platformSessionOpen(session *rootSession, relative string, wantDirectory bool) (*os.File, error) {
	directory, name, err := posixSessionTarget(session, relative)
	if err != nil {
		return nil, err
	}
	if directory == nil {
		return nil, fmt.Errorf("bound store directory is unavailable")
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if wantDirectory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(int(directory.Fd()), name, flags, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, errArtifactNotFound
		}
		return nil, fmt.Errorf("open bound store object failed")
	}
	file := os.NewFile(uintptr(fd), "private-metastore-object")
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("wrap bound store object failed")
	}
	return file, nil
}

func platformSessionCreatePrivateFile(session *rootSession, relative string) (*os.File, error) {
	directory, name, err := posixSessionTarget(session, relative)
	if err != nil || directory != session.temporaryDirectory || name == "." {
		return nil, fmt.Errorf("private staging name is invalid")
	}
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create private file failed")
	}
	file := os.NewFile(uintptr(fd), "private-metafile-staging")
	if file == nil {
		_ = unix.Close(fd)
		_ = unix.Unlinkat(int(directory.Fd()), name, 0)
		return nil, fmt.Errorf("wrap private file handle failed")
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		_ = file.Close()
		_ = unix.Unlinkat(int(directory.Fd()), name, 0)
		return nil, fmt.Errorf("secure private file failed")
	}
	if err := platformValidateOpenFile(file, false); err != nil {
		_ = file.Close()
		_ = unix.Unlinkat(int(directory.Fd()), name, 0)
		return nil, err
	}
	return file, nil
}

func platformSessionPublishNoReplace(session *rootSession, temporaryRelative, finalRelative string) (bool, error) {
	temporaryDirectory, temporaryName, err := posixSessionTarget(session, temporaryRelative)
	if err != nil || temporaryDirectory != session.temporaryDirectory || temporaryName == "." {
		return false, fmt.Errorf("private staging name is invalid")
	}
	finalDirectory, finalName, err := posixSessionTarget(session, finalRelative)
	if err != nil || finalName == "." || (finalDirectory != session.rootDirectory && finalDirectory != session.objectsDirectory) {
		return false, fmt.Errorf("private object name is invalid")
	}
	if err := unix.Linkat(int(temporaryDirectory.Fd()), temporaryName, int(finalDirectory.Fd()), finalName, 0); err != nil {
		if errors.Is(err, syscall.EEXIST) {
			return false, nil
		}
		return false, fmt.Errorf("link private object failed")
	}
	if err := unix.Fsync(int(finalDirectory.Fd())); err != nil {
		return true, fmt.Errorf("%w", ErrDurabilityUnconfirmed)
	}
	if err := unix.Unlinkat(int(temporaryDirectory.Fd()), temporaryName, 0); err != nil && !errors.Is(err, syscall.ENOENT) {
		return true, fmt.Errorf("%w", ErrPublishedCleanupIncomplete)
	}
	if err := unix.Fsync(int(temporaryDirectory.Fd())); err != nil {
		return true, fmt.Errorf("%w", ErrPublishedCleanupIncomplete)
	}
	return true, nil
}

func platformSessionRemovePrivate(session *rootSession, relative string) error {
	directory, name, err := posixSessionTarget(session, relative)
	if err != nil || directory != session.temporaryDirectory || name == "." {
		return fmt.Errorf("private staging name is invalid")
	}
	if err := unix.Unlinkat(int(directory.Fd()), name, 0); err != nil && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("remove private staging file failed")
	}
	if err := unix.Fsync(int(directory.Fd())); err != nil {
		return fmt.Errorf("flush staging directory failed")
	}
	return nil
}

func platformConfirmPrivateStoreLayout(session *rootSession) error {
	if session == nil {
		return fmt.Errorf("bound store session is unavailable")
	}
	for _, directory := range []*os.File{session.objectsDirectory, session.temporaryDirectory, session.rootDirectory} {
		if err := unix.Fsync(int(directory.Fd())); err != nil {
			return fmt.Errorf("confirm directory durability failed")
		}
	}
	if session.platform.parentDirectory == nil {
		return fmt.Errorf("bound store parent is unavailable")
	}
	if err := unix.Fsync(int(session.platform.parentDirectory.Fd())); err != nil {
		return fmt.Errorf("confirm store parent durability failed")
	}
	return nil
}

func posixSessionTarget(session *rootSession, relative string) (*os.File, string, error) {
	if session == nil || relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) != relative {
		return nil, "", fmt.Errorf("bound store relative name is invalid")
	}
	components := strings.Split(relative, string(filepath.Separator))
	switch len(components) {
	case 1:
		if components[0] == "." {
			return session.rootDirectory, ".", nil
		}
		if components[0] == objectsDir {
			return session.objectsDirectory, ".", nil
		}
		if components[0] == temporaryDir {
			return session.temporaryDirectory, ".", nil
		}
		if components[0] == markerName {
			return session.rootDirectory, markerName, nil
		}
	case 2:
		if components[1] == "" || components[1] == "." || components[1] == ".." {
			break
		}
		if components[0] == objectsDir {
			return session.objectsDirectory, components[1], nil
		}
		if components[0] == temporaryDir {
			return session.temporaryDirectory, components[1], nil
		}
	}
	return nil, "", fmt.Errorf("bound store relative name is invalid")
}
