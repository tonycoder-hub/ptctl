//go:build windows

package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func isReparsePoint(info os.FileInfo) bool {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && data.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func filesystemIdentity(path string, _ os.FileInfo) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	var data syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &data); err != nil {
		return "", false
	}
	return fmt.Sprintf("volume:%08x", data.VolumeSerialNumber), true
}

func fileIdentity(path string, _ os.FileInfo) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	return openedFileIdentity(file)
}

func openedFileIdentity(file *os.File) (string, bool) {
	var data syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &data); err != nil {
		return "", false
	}
	return fmt.Sprintf("win:%08x:%08x:%08x", data.VolumeSerialNumber, data.FileIndexHigh, data.FileIndexLow), true
}

func isNetworkPath(path string) bool {
	volume := filepath.VolumeName(path)
	return strings.HasPrefix(volume, `\\`)
}

// OpenRegular is os.Open on Windows, where an ordinary path cannot name a
// FIFO; callers still check that the opened object is a regular file.
func OpenRegular(path string) (*os.File, error) { return os.Open(path) }

func openDirectory(path string) (*os.File, error) { return os.Open(path) }
