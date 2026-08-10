//go:build darwin

package metastore

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func platformFilesystemIdentity(file *os.File) (platformFilesystemID, error) {
	if file == nil {
		return platformFilesystemID{}, fmt.Errorf("filesystem handle is unavailable")
	}
	fd := int(file.Fd())
	var native unix.Stat_t
	var filesystem unix.Statfs_t
	if err := unix.Fstat(fd, &native); err != nil {
		return platformFilesystemID{}, fmt.Errorf("filesystem handle is unavailable")
	}
	if err := unix.Fstatfs(fd, &filesystem); err != nil {
		return platformFilesystemID{}, fmt.Errorf("filesystem mount identity is unavailable")
	}
	mount := uint64(uint32(filesystem.Fsid.Val[0]))<<32 | uint64(uint32(filesystem.Fsid.Val[1]))
	return platformFilesystemID{volume: uint64(native.Dev), mount: mount}, nil
}

func platformValidateLocalFilesystem(path string) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return fmt.Errorf("store filesystem type is unavailable")
	}
	var builder strings.Builder
	for _, value := range stat.Fstypename {
		if value == 0 {
			break
		}
		builder.WriteByte(byte(value))
	}
	switch builder.String() {
	case "apfs", "hfs":
		return nil
	default:
		return fmt.Errorf("private metafile store requires a reviewed local filesystem")
	}
}
