//go:build !windows && !linux

package fsbind

import (
	"os"
	"strings"
)

type platformData struct{}

func platformBindExisting(string) (*Session, RootInfo, error) {
	return nil, RootInfo{}, ErrUnsupported
}
func platformCheckBinding(*Session) error { return ErrUnsupported }
func platformCloseSession(*Session) error { return nil }
func platformValidateComponent(component string) error {
	if strings.ContainsAny(component, `/\`) {
		return ErrInvalidPath
	}
	return nil
}
func platformCreatePrivateDirectory(*Session, *boundDirectory, string) (*boundDirectory, bool, bool, error) {
	return nil, false, false, ErrUnsupported
}
func platformOpenPrivateDirectory(*Session, *boundDirectory, string) (*boundDirectory, error) {
	return nil, ErrUnsupported
}
func platformCreatePrivateRegular(*Session, *boundDirectory, string) (*os.File, rawIdentity, error) {
	return nil, rawIdentity{}, ErrUnsupported
}
func platformOpenPrivateRegular(*Session, *boundDirectory, string) (*os.File, rawIdentity, error) {
	return nil, rawIdentity{}, ErrUnsupported
}
func platformOpenPrivateRegularReadOnly(*Session, *boundDirectory, string) (*os.File, rawIdentity, error) {
	return nil, rawIdentity{}, ErrUnsupported
}
func platformOpenAnySource(*Session, *boundDirectory, string, bool) (*os.File, rawIdentity, error) {
	return nil, rawIdentity{}, ErrUnsupported
}
func platformInspectObject(*Session, *boundDirectory, string) (*os.File, rawIdentity, ObjectKind, error) {
	return nil, rawIdentity{}, "", ErrUnsupported
}
func platformInspectRootObject(*Session, string) (*os.File, rawIdentity, ObjectKind, error) {
	return nil, rawIdentity{}, "", ErrUnsupported
}
func platformAcquireOperationLock(*Session, *boundDirectory, bool) (*os.File, rawIdentity, error) {
	return nil, rawIdentity{}, ErrUnsupported
}
func platformCheckOperationLock(*Session, *boundDirectory, *os.File, rawIdentity) error {
	return ErrUnsupported
}
func platformReleaseOperationLock(*os.File, *boundDirectory) error { return nil }
func platformSyncFile(*os.File) error                              { return ErrUnsupported }
func platformSyncDirectory(*boundDirectory) error                  { return ErrUnsupported }
func platformReadDirectory(*boundDirectory, int) ([]os.DirEntry, error) {
	return nil, ErrUnsupported
}
func platformPublishNoReplace(*Session, *boundDirectory, string, *boundDirectory, string, bool, rawIdentity) (bool, bool, rawIdentity, error) {
	return false, false, rawIdentity{}, ErrUnsupported
}

func platformRemoveObject(*Session, *boundDirectory, string, bool, rawIdentity, int64) (bool, bool, rawIdentity, error) {
	return false, false, rawIdentity{}, ErrUnsupported
}
