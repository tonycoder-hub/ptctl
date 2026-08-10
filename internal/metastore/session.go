package metastore

import (
	"errors"
	"fmt"
	"os"
)

// platformFilesystemID is populated from an already-open handle. volume is
// the POSIX device or Windows volume serial; mount distinguishes separate
// mounts of the same backing device where the platform exposes that identity.
type platformFilesystemID struct {
	volume uint64
	mount  uint64
}

type rootSession struct {
	path               string
	root               *os.Root
	rootDirectory      *os.File
	objectsDirectory   *os.File
	temporaryDirectory *os.File
	filesystem         platformFilesystemID
	platform           platformSessionState
}

// operationIdentityHook is a deterministic test seam. Production leaves it
// nil. It runs immediately before the real named-identity check, never instead
// of it.
var operationIdentityHook func(stage string, session *rootSession) error
var sessionFilesystemIdentity = platformFilesystemIdentity

func openBoundRootSession(path string) (*rootSession, error) {
	session, err := openBoundRootOnlySession(path)
	if err != nil {
		return nil, err
	}
	if err := session.attachSubdirectory(objectsDir); err != nil {
		_ = session.Close()
		return nil, err
	}
	if err := session.attachSubdirectory(temporaryDir); err != nil {
		_ = session.Close()
		return nil, err
	}
	if err := session.check("session_bound"); err != nil {
		_ = session.Close()
		return nil, err
	}
	return session, nil
}

func openBoundRootOnlySession(path string) (*rootSession, error) {
	state, err := platformOpenSessionState(path)
	if err != nil {
		return nil, err
	}
	session := &rootSession{path: path, platform: state}
	failed := true
	defer func() {
		if failed {
			_ = session.Close()
		}
	}()
	session.root, err = os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("store root handle is unavailable")
	}
	session.rootDirectory, _, err = openValidatedRelative(session.root, ".", true)
	if err != nil {
		return nil, fmt.Errorf("store root handle is unsafe")
	}
	session.filesystem, err = sessionFilesystemIdentity(session.rootDirectory)
	if err != nil {
		return nil, fmt.Errorf("store filesystem identity is unavailable")
	}
	if err := platformVerifySessionBinding(session); err != nil {
		return nil, err
	}
	failed = false
	return session, nil
}

func (session *rootSession) attachSubdirectory(name string) error {
	if session == nil || (name != objectsDir && name != temporaryDir) {
		return fmt.Errorf("store subdirectory name is invalid")
	}
	if (name == objectsDir && session.objectsDirectory != nil) || (name == temporaryDir && session.temporaryDirectory != nil) {
		return nil
	}
	if err := platformAttachSessionDirectoryGuard(session, name); err != nil {
		return err
	}
	file, err := platformSessionOpenRootDirectory(session, name)
	if err != nil {
		return err
	}
	if platformValidateOpenFile(file, true) != nil {
		_ = file.Close()
		return fmt.Errorf("store subdirectory is unsafe")
	}
	if err := session.validateFilesystem(file); err != nil {
		_ = file.Close()
		return err
	}
	if name == objectsDir {
		session.objectsDirectory = file
	} else {
		session.temporaryDirectory = file
	}
	if err := platformVerifySessionBinding(session); err != nil {
		return err
	}
	return nil
}

func (session *rootSession) createPrivateDirectory(name string) error {
	if session == nil || (name != objectsDir && name != temporaryDir) {
		return fmt.Errorf("store subdirectory name is invalid")
	}
	if err := session.check("before_directory_create"); err != nil {
		return err
	}
	if err := platformSessionCreatePrivateDirectory(session, name); err != nil && !errors.Is(err, errPrivateDirectoryAlreadyExists) {
		return err
	}
	return session.attachSubdirectory(name)
}

func (session *rootSession) Close() error {
	if session == nil {
		return nil
	}
	var first error
	for _, file := range []*os.File{session.temporaryDirectory, session.objectsDirectory, session.rootDirectory} {
		if file != nil {
			if err := file.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	if session.root != nil {
		if err := session.root.Close(); err != nil && first == nil {
			first = err
		}
	}
	if err := platformCloseSessionState(&session.platform); err != nil && first == nil {
		first = err
	}
	return first
}

func (session *rootSession) check(stage string) error {
	if session == nil {
		return fmt.Errorf("bound store session is unavailable")
	}
	hookFailed := false
	if operationIdentityHook != nil {
		if err := operationIdentityHook(stage, session); err != nil {
			hookFailed = true
		}
	}
	if err := platformVerifySessionBinding(session); err != nil || hookFailed {
		return fmt.Errorf("bound store identity changed")
	}
	return nil
}

func (session *rootSession) validateFilesystem(file *os.File) error {
	identity, err := sessionFilesystemIdentity(file)
	if err != nil || identity != session.filesystem {
		return fmt.Errorf("store object crossed its reviewed filesystem")
	}
	return nil
}

func (session *rootSession) openValidated(relative string, wantDirectory bool) (*os.File, os.FileInfo, error) {
	file, err := platformSessionOpen(session, relative, wantDirectory)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil || info.IsDir() != wantDirectory || (!wantDirectory && !info.Mode().IsRegular()) || platformValidateOpenFile(file, wantDirectory) != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("bound store object is unsafe")
	}
	if err := session.validateFilesystem(file); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func (session *rootSession) createPrivateFile(relative string) (*os.File, error) {
	if err := session.check("before_staging_create"); err != nil {
		return nil, err
	}
	file, err := platformSessionCreatePrivateFile(session, relative)
	if err != nil {
		return nil, err
	}
	if err := session.validateFilesystem(file); err != nil {
		_ = file.Close()
		_ = platformSessionRemovePrivate(session, relative)
		return nil, err
	}
	return file, nil
}

func (session *rootSession) removePrivate(relative string) error {
	return platformSessionRemovePrivate(session, relative)
}

func (session *rootSession) publishNoReplace(temporaryRelative, finalRelative string) (bool, error) {
	if err := session.check("before_publish"); err != nil {
		return false, err
	}
	published, err := publishNoReplace(session, temporaryRelative, finalRelative)
	if bindingErr := session.check("after_publish"); bindingErr != nil {
		if published {
			// Once the platform has proved publication, its post-commit error
			// carries the authoritative durability/cleanup classification. A
			// concurrent binding failure must still fail the operation, but must
			// not erase that stronger publication evidence.
			if err != nil {
				return true, err
			}
			return true, bindingErr
		}
		return false, bindingErr
	}
	return published, err
}
