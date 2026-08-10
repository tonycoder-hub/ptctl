//go:build windows

package metastore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const forbiddenWindowsAttributes = windows.FILE_ATTRIBUTE_REPARSE_POINT |
	windows.FILE_ATTRIBUTE_OFFLINE |
	windows.FILE_ATTRIBUTE_RECALL_ON_OPEN |
	windows.FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS

// setWindowsPrivateOwner is a narrow test seam around the post-creation owner
// assignment. Supplying O: in the creation descriptor can require
// SeRestorePrivilege; assigning the caller's enabled user SID through a
// WRITE_OWNER handle does not.
var setWindowsPrivateOwner = setCurrentUserAsOwner

func platformCommitAssurance() string {
	return "same_fixed_drive_movefileex_no_replace_write_through_and_file_flush"
}

func platformValidateStoreLocation(path string, mayNotExist bool) error {
	if !filepath.IsAbs(path) || strings.HasPrefix(path, `\\`) {
		return fmt.Errorf("store location must use a local drive path")
	}
	volume := filepath.VolumeName(path)
	if len(volume) != 2 || volume[1] != ':' {
		return fmt.Errorf("store location must use a local drive path")
	}
	rootPointer, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil || windows.GetDriveType(rootPointer) != windows.DRIVE_FIXED {
		return fmt.Errorf("private metafile store requires a fixed local drive")
	}
	return validateWindowsPrefixes(path, mayNotExist)
}

func platformValidateSourcePath(path string) error {
	return validateWindowsPrefixes(path, false)
}

func validateWindowsPrefixes(path string, mayNotExist bool) error {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	if len(volume) != 2 || volume[1] != ':' || strings.HasPrefix(clean, `\\`) {
		return fmt.Errorf("path is not on a local drive")
	}
	root := volume + `\`
	relative, err := filepath.Rel(root, clean)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, `..\`) {
		return fmt.Errorf("path components are invalid")
	}
	current := root
	parts := strings.Split(relative, `\`)
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("path components are invalid")
		}
		current = filepath.Join(current, part)
		pointer, pointerErr := windows.UTF16PtrFromString(windowsAPIPath(current))
		if pointerErr != nil {
			return fmt.Errorf("path encoding is invalid")
		}
		attributes, attributeErr := windows.GetFileAttributes(pointer)
		if attributeErr != nil {
			if mayNotExist && (errors.Is(attributeErr, windows.ERROR_FILE_NOT_FOUND) || errors.Is(attributeErr, windows.ERROR_PATH_NOT_FOUND)) {
				return nil
			}
			return fmt.Errorf("path prefix is unavailable")
		}
		if attributes&forbiddenWindowsAttributes != 0 {
			return fmt.Errorf("path prefix has unsafe file attributes")
		}
		if index < len(parts)-1 && attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return fmt.Errorf("path prefix is not a directory")
		}
	}
	return nil
}

func platformCreatePrivateDirectory(path string) error {
	securityAttributes, err := privateSecurityAttributes()
	if err != nil {
		return fmt.Errorf("build private directory security failed")
	}
	pointer, err := windows.UTF16PtrFromString(windowsAPIPath(path))
	if err != nil {
		return fmt.Errorf("private directory path encoding failed")
	}
	if err := windows.CreateDirectory(pointer, securityAttributes); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return fmt.Errorf("%w", errPrivateDirectoryAlreadyExists)
		}
		return fmt.Errorf("create private directory failed")
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = os.Remove(path)
		}
	}()
	file, err := openCreatedPrivateDirectory(path)
	if err != nil {
		return fmt.Errorf("open private directory failed")
	}
	if err := setWindowsPrivateOwner(windows.Handle(file.Fd())); err != nil {
		_ = file.Close()
		return fmt.Errorf("assign private directory owner failed")
	}
	if err := platformValidateOpenFile(file, true); err != nil {
		_ = file.Close()
		return fmt.Errorf("validate private directory security failed")
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close private directory failed")
	}
	succeeded = true
	return nil
}

func platformCreatePrivateFile(path string) (*os.File, error) {
	securityAttributes, err := privateSecurityAttributes()
	if err != nil {
		return nil, fmt.Errorf("build private file security failed")
	}
	pointer, err := windows.UTF16PtrFromString(windowsAPIPath(path))
	if err != nil {
		return nil, fmt.Errorf("private file path encoding failed")
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_OWNER,
		windows.FILE_SHARE_READ,
		securityAttributes,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("create private file failed")
	}
	file := os.NewFile(uintptr(handle), "private-metafile-staging")
	if file == nil {
		_ = windows.CloseHandle(handle)
		_ = os.Remove(path)
		return nil, fmt.Errorf("wrap private file handle failed")
	}
	if err := setWindowsPrivateOwner(handle); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("assign private file owner failed")
	}
	if err := platformValidateOpenFile(file, false); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return file, nil
}

func openCreatedPrivateDirectory(path string) (*os.File, error) {
	pointer, err := windows.UTF16PtrFromString(windowsAPIPath(path))
	if err != nil {
		return nil, fmt.Errorf("private directory path encoding failed")
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.WRITE_OWNER,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open private directory owner handle failed")
	}
	file := os.NewFile(uintptr(handle), "private-metastore-owner-assignment")
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("wrap private directory owner handle failed")
	}
	named, namedErr := os.Lstat(path)
	handleInfo, statErr := file.Stat()
	if namedErr != nil || statErr != nil || platformRejectNamedInfo(named) || !named.IsDir() || !os.SameFile(named, handleInfo) || validateWindowsHandleAttributes(handle, true) != nil {
		_ = file.Close()
		return nil, fmt.Errorf("private directory owner handle is unsafe")
	}
	return file, nil
}

func setCurrentUserAsOwner(handle windows.Handle) error {
	current, err := currentUserSID()
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
		current,
		nil,
		nil,
		nil,
	); err != nil {
		return err
	}
	return nil
}

func privateSecurityAttributes() (*windows.SecurityAttributes, error) {
	current, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	// Do not set OWNER_SECURITY_INFORMATION at creation time. Asking the
	// kernel to assign even the caller's SID explicitly can require
	// SeRestorePrivilege, and a token's default owner may be a group such as
	// Administrators. SDDL's D:P therefore supplies only a protected DACL;
	// creation is followed immediately by WRITE_OWNER assignment through the
	// new object's handle and then strict owner/DACL validation.
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + current.String() + ")")
	if err != nil {
		return nil, err
	}
	attributes := &windows.SecurityAttributes{SecurityDescriptor: descriptor}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	return attributes, nil
}

func currentUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return nil, fmt.Errorf("current user identity is unavailable")
	}
	return user.User.Sid.Copy()
}

func platformRejectNamedInfo(info os.FileInfo) bool {
	if info == nil || info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	if data, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return data.FileAttributes&forbiddenWindowsAttributes != 0
	}
	return true
}

func platformValidateSourceFile(file *os.File) error {
	if file == nil {
		return fmt.Errorf("source handle is unavailable")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("source handle is not a regular file")
	}
	return validateWindowsHandleAttributes(windows.Handle(file.Fd()), false)
}

func platformValidateOpenFile(file *os.File, wantDirectory bool) error {
	if file == nil {
		return fmt.Errorf("private store handle is unavailable")
	}
	info, err := file.Stat()
	if err != nil || info.IsDir() != wantDirectory || (!wantDirectory && !info.Mode().IsRegular()) {
		return fmt.Errorf("private store object type is invalid")
	}
	handle := windows.Handle(file.Fd())
	if err := validateWindowsHandleAttributes(handle, wantDirectory); err != nil {
		return err
	}
	securityDescriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("private store security descriptor is unavailable")
	}
	owner, _, err := securityDescriptor.Owner()
	if err != nil {
		return fmt.Errorf("private store owner is unavailable")
	}
	current, err := currentUserSID()
	if err != nil || owner == nil || !owner.Equals(current) {
		return fmt.Errorf("private store owner is not the current user")
	}
	control, _, err := securityDescriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("private store DACL is not protected")
	}
	dacl, _, err := securityDescriptor.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("private store DACL is unavailable")
	}
	var first *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &first); err != nil || first == nil || first.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || first.Header.AceFlags&windows.INHERITED_ACE != 0 || first.Mask == 0 {
		return fmt.Errorf("private store DACL does not grant one explicit user ACE")
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&first.SidStart))
	if !aceSID.IsValid() || !aceSID.Equals(current) {
		return fmt.Errorf("private store DACL grants a different principal")
	}
	var second *windows.ACCESS_ALLOWED_ACE
	if windows.GetAce(dacl, 1, &second) == nil {
		return fmt.Errorf("private store DACL grants more than one ACE")
	}
	return nil
}

func validateWindowsHandleAttributes(handle windows.Handle, wantDirectory bool) error {
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return fmt.Errorf("private store file attributes are unavailable")
	}
	if information.FileAttributes&forbiddenWindowsAttributes != 0 {
		return fmt.Errorf("private store object has unsafe file attributes")
	}
	isDirectory := information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if isDirectory != wantDirectory {
		return fmt.Errorf("private store object type is invalid")
	}
	return nil
}

func windowsAPIPath(path string) string {
	if strings.HasPrefix(path, `\\?\`) {
		return path
	}
	return `\\?\` + path
}
