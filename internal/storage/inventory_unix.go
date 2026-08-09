//go:build !windows

package storage

import (
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
