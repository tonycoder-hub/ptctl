//go:build !windows

package storage

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func isReparsePoint(os.FileInfo) bool { return false }

func filesystemIdentity(_ string, info os.FileInfo) (string, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("device:%d", uint64(stat.Dev)), true
}

func fileIdentity(_ string, info os.FileInfo) (string, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("unix:%d:%d", uint64(stat.Dev), uint64(stat.Ino)), true
}

func openedFileIdentity(file *os.File) (string, bool) {
	info, err := file.Stat()
	if err != nil {
		return "", false
	}
	return fileIdentity("", info)
}

func isNetworkPath(string) bool { return false }

// OpenRegular opens path read-only and returns it only if it is a regular
// file. A final symbolic link is refused, and O_NONBLOCK keeps a FIFO or
// device that replaced an inspected file from blocking the open.
func OpenRegular(path string) (*os.File, error) {
	flags := syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	fd, err := syscall.Open(path, flags, 0)
	for err == syscall.EINTR {
		fd, err = syscall.Open(path, flags, 0)
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var stat syscall.Stat_t
	if err = syscall.Fstat(fd, &stat); err == nil && stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		err = errors.New("not a regular file")
	}
	if err == nil {
		err = syscall.SetNonblock(fd, false)
	}
	if err != nil {
		_ = syscall.Close(fd)
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// openDirectory fails with ENOTDIR, instead of blocking, when a FIFO replaced
// the inspected directory.
func openDirectory(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
}
