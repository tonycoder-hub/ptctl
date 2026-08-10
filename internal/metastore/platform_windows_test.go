//go:build windows

package metastore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsPrivateDirectoryPrimitiveUsesOwnerOnlyDACL(t *testing.T) {
	attributes, err := privateSecurityAttributes()
	if err != nil {
		t.Fatalf("privateSecurityAttributes: %v", err)
	}
	owner, _, ownerErr := attributes.SecurityDescriptor.Owner()
	if ownerErr != nil || owner != nil {
		t.Fatalf("creation descriptor unexpectedly supplied an owner: owner=%v err=%v", owner, ownerErr)
	}
	path := filepath.Join(t.TempDir(), "private")
	if err := platformCreatePrivateDirectory(path); err != nil {
		t.Fatalf("platformCreatePrivateDirectory: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := platformValidateOpenFile(file, true); err != nil {
		t.Fatalf("created directory did not validate: %v", err)
	}
}

func TestWindowsPrivateFilePrimitiveUsesOwnerOnlyDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.torrent")
	file, err := platformCreatePrivateFile(path)
	if err != nil {
		t.Fatalf("platformCreatePrivateFile: %v", err)
	}
	defer file.Close()
	if err := platformValidateOpenFile(file, false); err != nil {
		t.Fatalf("created file did not validate: %v", err)
	}
}

func TestWindowsOwnerAssignmentFailureCleansCreatedObjectWithoutLeaking(t *testing.T) {
	const canary = "OWNER-ASSIGNMENT-FAILURE-CANARY"
	tests := []struct {
		name   string
		create func(string) error
	}{
		{"directory", platformCreatePrivateDirectory},
		{"file", func(path string) error {
			file, err := platformCreatePrivateFile(path)
			if file != nil {
				_ = file.Close()
			}
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := setWindowsPrivateOwner
			setWindowsPrivateOwner = func(windows.Handle) error { return errors.New(canary) }
			t.Cleanup(func() { setWindowsPrivateOwner = original })

			path := filepath.Join(t.TempDir(), "private-"+test.name)
			err := test.create(path)
			if err == nil || strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), path) {
				t.Fatalf("create returned unsafe error: %v", err)
			}
			if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
				t.Fatalf("failed owner assignment left an object: %v", statErr)
			}
		})
	}
}

func TestWindowsStoreRejectsAdditionalDACLPrincipal(t *testing.T) {
	store := newTestStore(t)
	current, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + current.String() + ")(A;;GR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("read test DACL: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(
		store.root,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(store.root); err == nil || opened != nil || strings.Contains(err.Error(), store.root) {
		t.Fatalf("Open accepted broad DACL: store=%v err=%v", opened, err)
	}
}

func TestWindowsSessionGuardsBlockRenameAndCloseCleanly(t *testing.T) {
	store := newTestStore(t)
	session, err := store.validatedSession()
	if err != nil {
		t.Fatal(err)
	}
	moved := store.root + "-moved"
	if err := os.Rename(store.root, moved); err == nil {
		_ = os.Rename(moved, store.root)
		_ = session.Close()
		t.Fatal("store root was renamed while no-delete guards were held")
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close session guards: %v", err)
	}
	if err := os.Rename(store.root, moved); err != nil {
		t.Fatalf("rename remained blocked after guards closed: %v", err)
	}
	if err := os.Rename(moved, store.root); err != nil {
		t.Fatalf("restore store root: %v", err)
	}
}
