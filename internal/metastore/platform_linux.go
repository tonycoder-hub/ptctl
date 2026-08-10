//go:build linux

package metastore

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func platformFilesystemIdentity(file *os.File) (platformFilesystemID, error) {
	if file == nil {
		return platformFilesystemID{}, fmt.Errorf("filesystem handle is unavailable")
	}
	fd := int(file.Fd())
	var native unix.Stat_t
	if err := unix.Fstat(fd, &native); err != nil {
		return platformFilesystemID{}, fmt.Errorf("filesystem handle is unavailable")
	}
	var extended unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_STATX_SYNC_AS_STAT, unix.STATX_MNT_ID, &extended); err != nil || extended.Mask&unix.STATX_MNT_ID == 0 {
		return platformFilesystemID{}, fmt.Errorf("filesystem mount identity is unavailable")
	}
	return platformFilesystemID{volume: uint64(native.Dev), mount: extended.Mnt_id}, nil
}

func platformValidateLocalFilesystem(path string) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return fmt.Errorf("store filesystem type is unavailable")
	}
	// This is deliberately a positive allowlist. Unknown filesystems fail
	// closed instead of being guessed local; new local backends require an
	// explicit review of their atomic-link and durability semantics.
	switch uint64(stat.Type) {
	case uint64(unix.EXT4_SUPER_MAGIC),
		uint64(unix.XFS_SUPER_MAGIC),
		uint64(unix.BTRFS_SUPER_MAGIC),
		uint64(unix.OVERLAYFS_SUPER_MAGIC),
		0xf2f52010, // F2FS_SUPER_MAGIC
		0x2fc12fc1: // ZFS_SUPER_MAGIC
		return nil
	default:
		return fmt.Errorf("private metafile store requires a reviewed local filesystem")
	}
}
