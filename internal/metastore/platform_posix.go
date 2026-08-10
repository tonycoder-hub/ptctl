//go:build linux || darwin

package metastore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func platformCommitAssurance() string {
	return "same_filesystem_link_no_replace_file_final_and_parent_directories_fsync"
}

func platformValidateStoreLocation(path string, mayNotExist bool) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("store location is not absolute")
	}
	existing, err := validatePOSIXPrefixes(path, mayNotExist)
	if err != nil {
		return err
	}
	if err := platformValidateLocalFilesystem(existing); err != nil {
		return err
	}
	return nil
}

func platformValidateSourcePath(path string) error {
	_, err := validatePOSIXPrefixes(path, false)
	return err
}

func validatePOSIXPrefixes(path string, mayNotExist bool) (string, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("path is not absolute")
	}
	current := string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(clean, current), string(filepath.Separator))
	lastExisting := current
	for index, part := range parts {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if mayNotExist && os.IsNotExist(err) {
				for _, remaining := range parts[index:] {
					if remaining == "" || remaining == "." || remaining == ".." {
						return "", fmt.Errorf("path components are invalid")
					}
				}
				return lastExisting, nil
			}
			return "", fmt.Errorf("path prefix is unavailable")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("path prefix is a symbolic link")
		}
		if index < len(parts)-1 && !info.IsDir() {
			return "", fmt.Errorf("path prefix is not a directory")
		}
		lastExisting = current
	}
	return lastExisting, nil
}

func platformCreatePrivateDirectory(path string) error {
	parentPath := filepath.Dir(path)
	name := filepath.Base(path)
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("private directory name is invalid")
	}
	parentFD, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open private directory parent failed")
	}
	defer unix.Close(parentFD)
	if err := unix.Mkdirat(parentFD, name, 0o700); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%w", errPrivateDirectoryAlreadyExists)
		}
		return fmt.Errorf("create private directory failed")
	}
	directoryFD, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open private directory failed")
	}
	file := os.NewFile(uintptr(directoryFD), "private-metastore-created-directory")
	if file == nil {
		_ = unix.Close(directoryFD)
		return fmt.Errorf("wrap private directory failed")
	}
	if err := unix.Fchmod(directoryFD, 0o700); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure private directory failed")
	}
	if err := platformValidateOpenFile(file, true); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("flush private directory failed")
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close private directory failed")
	}
	if err := unix.Fsync(parentFD); err != nil {
		return fmt.Errorf("confirm directory durability failed")
	}
	return nil
}

func platformCreatePrivateFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create private file failed")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure private file failed")
	}
	if err := platformValidateOpenFile(file, false); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func platformRejectNamedInfo(info os.FileInfo) bool {
	return info == nil || info.Mode()&os.ModeSymlink != 0
}

func platformValidateSourceFile(file *os.File) error {
	if file == nil {
		return fmt.Errorf("source handle is unavailable")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("source handle is not a regular file")
	}
	return nil
}

func platformValidateOpenFile(file *os.File, wantDirectory bool) error {
	if file == nil {
		return fmt.Errorf("private store handle is unavailable")
	}
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect private store handle failed")
	}
	if info.IsDir() != wantDirectory || (!wantDirectory && !info.Mode().IsRegular()) {
		return fmt.Errorf("private store object type is invalid")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("private store object owner is invalid")
	}
	wanted := os.FileMode(0o600)
	if wantDirectory {
		wanted = 0o700
	}
	if info.Mode().Perm() != wanted || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("private store object permissions are invalid")
	}
	return nil
}
