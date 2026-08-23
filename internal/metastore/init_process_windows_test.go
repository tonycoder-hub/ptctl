//go:build windows

package metastore

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const (
	initProcessHelperEnv  = "PTCTL_TEST_METASTORE_INIT_PROCESS_HELPER"
	initProcessRootEnv    = "PTCTL_TEST_METASTORE_INIT_PROCESS_ROOT"
	initProcessPauseEnv   = "PTCTL_TEST_METASTORE_INIT_PROCESS_PAUSE"
	initProcessReleaseEnv = "PTCTL_TEST_METASTORE_INIT_PROCESS_RELEASE"
	initProcessResultEnv  = "PTCTL_TEST_METASTORE_INIT_PROCESS_RESULT"
)

type initProcessResult struct {
	Store   StoreInfo   `json:"store"`
	Receipt InitReceipt `json:"receipt"`
}

func TestWindowsInitPreparationMutexNameIsOpaqueAndCaseFolded(t *testing.T) {
	root := `C:\PRIVATE-ROOT-PATH-CANARY\MiXeD`
	first, err := platformInitPreparationMutexName(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := platformInitPreparationMutexName(strings.ToUpper(root))
	if err != nil {
		t.Fatal(err)
	}
	firstName := windows.UTF16PtrToString(first)
	if firstName != windows.UTF16PtrToString(second) || strings.Contains(strings.ToLower(firstName), "private-root-path-canary") {
		t.Fatalf("initializer mutex name was unstable or exposed its path: %q", firstName)
	}
}

func TestWindowsConcurrentInitAcrossProcesses(t *testing.T) {
	parent := physicalTempDir(t)
	root := filepath.Join(parent, "store")
	pause := filepath.Join(parent, "owner-assignment-paused")
	release := filepath.Join(parent, "owner-assignment-release")
	firstResult := filepath.Join(parent, "first.json")
	secondResult := filepath.Join(parent, "second.json")

	firstOutput := &bytes.Buffer{}
	first := initHelperCommand(root, firstResult, pause, release, firstOutput)
	if err := first.Start(); err != nil {
		t.Fatalf("start first initializer: %v", err)
	}
	t.Cleanup(func() { _ = first.Process.Kill() })
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Wait() }()

	deadline := time.NewTimer(10 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	waitingForPause := true
	for waitingForPause {
		select {
		case err := <-firstDone:
			t.Fatalf("first initializer exited before owner pause: %v\n%s", err, firstOutput.String())
		case <-deadline.C:
			_ = first.Process.Kill()
			err := <-firstDone
			t.Fatalf("first initializer did not enter owner pause: %v\n%s", err, firstOutput.String())
		case <-ticker.C:
			if _, err := os.Stat(pause); err == nil {
				waitingForPause = false
			}
		}
	}
	assertInitPreparationMutexHeld(t, root)

	secondOutput := &bytes.Buffer{}
	second := initHelperCommand(root, secondResult, "", "", secondOutput)
	if err := second.Start(); err != nil {
		_ = first.Process.Kill()
		t.Fatalf("start second initializer: %v", err)
	}
	t.Cleanup(func() { _ = second.Process.Kill() })
	secondDone := make(chan error, 1)
	go func() { secondDone <- second.Wait() }()
	select {
	case err := <-secondDone:
		_ = first.Process.Kill()
		t.Fatalf("second initializer crossed the first process preparation lock: %v\n%s", err, secondOutput.String())
	case <-time.After(150 * time.Millisecond):
	}
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		_ = first.Process.Kill()
		_ = second.Process.Kill()
		t.Fatalf("release first initializer: %v", err)
	}

	if err := <-secondDone; err != nil {
		t.Fatalf("second initializer failed: %v\n%s", err, secondOutput.String())
	}
	if err := <-firstDone; err != nil {
		t.Fatalf("first initializer failed: %v\n%s", err, firstOutput.String())
	}

	firstObserved := readInitProcessResult(t, firstResult)
	secondObserved := readInitProcessResult(t, secondResult)
	if firstObserved.Store != secondObserved.Store || firstObserved.Store.StoreID == "" {
		t.Fatalf("initializers selected different stores: first=%+v second=%+v", firstObserved.Store, secondObserved.Store)
	}
	if firstObserved.Receipt.WritesPerformed+secondObserved.Receipt.WritesPerformed != 1 ||
		firstObserved.Receipt.AlreadyInitialized == secondObserved.Receipt.AlreadyInitialized {
		t.Fatalf("unexpected cross-process receipts: first=%+v second=%+v", firstObserved.Receipt, secondObserved.Receipt)
	}
	opened, receipt, err := Init(root)
	if err != nil || opened == nil || opened.Info() != firstObserved.Store || !receipt.AlreadyInitialized || receipt.WritesPerformed != 0 {
		t.Fatalf("final Init returned store=%v receipt=%+v err=%v", opened, receipt, err)
	}
}

func TestWindowsInitProcessHelper(t *testing.T) {
	if os.Getenv(initProcessHelperEnv) != "1" {
		t.Skip("subprocess helper")
	}
	root := os.Getenv(initProcessRootEnv)
	resultPath := os.Getenv(initProcessResultEnv)
	if root == "" || resultPath == "" {
		t.Fatal("subprocess helper selectors are unavailable")
	}
	if pause, release := os.Getenv(initProcessPauseEnv), os.Getenv(initProcessReleaseEnv); pause != "" || release != "" {
		if pause == "" || release == "" {
			t.Fatal("subprocess owner-pause selectors are incomplete")
		}
		original := setWindowsPrivateOwner
		var once sync.Once
		setWindowsPrivateOwner = func(handle windows.Handle) error {
			once.Do(func() {
				if err := os.WriteFile(pause, []byte("paused"), 0o600); err != nil {
					t.Fatalf("publish owner pause: %v", err)
				}
				deadline := time.Now().Add(10 * time.Second)
				for {
					if _, err := os.Stat(release); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("owner pause was not released")
					}
					time.Sleep(10 * time.Millisecond)
				}
			})
			return original(handle)
		}
	}
	store, receipt, err := Init(root)
	if err != nil || store == nil {
		t.Fatalf("Init returned store=%v receipt=%+v err=%v", store, receipt, err)
	}
	raw, err := json.Marshal(initProcessResult{Store: store.Info(), Receipt: receipt})
	if err != nil {
		t.Fatalf("encode subprocess result: %v", err)
	}
	if err := os.WriteFile(resultPath, raw, 0o600); err != nil {
		t.Fatalf("write subprocess result: %v", err)
	}
}

func initHelperCommand(root, result, pause, release string, output *bytes.Buffer) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=^TestWindowsInitProcessHelper$", "-test.count=1")
	command.Env = append(os.Environ(),
		initProcessHelperEnv+"=1",
		initProcessRootEnv+"="+root,
		initProcessResultEnv+"="+result,
		initProcessPauseEnv+"="+pause,
		initProcessReleaseEnv+"="+release,
	)
	command.Stdout = output
	command.Stderr = output
	return command
}

func readInitProcessResult(t *testing.T, path string) initProcessResult {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read subprocess result: %v", err)
	}
	var result initProcessResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode subprocess result: %v", err)
	}
	return result
}

func assertInitPreparationMutexHeld(t *testing.T, root string) {
	t.Helper()
	name, err := platformInitPreparationMutexName(root)
	if err != nil {
		t.Fatalf("derive initializer mutex name: %v", err)
	}
	handle, err := windows.OpenMutex(windows.SYNCHRONIZE|windows.MUTEX_MODIFY_STATE, false, name)
	if err != nil {
		t.Fatalf("open initializer mutex: %v", err)
	}
	runtime.LockOSThread()
	wait, waitErr := windows.WaitForSingleObject(handle, 0)
	if wait == windows.WAIT_OBJECT_0 || wait == windows.WAIT_ABANDONED {
		_ = windows.ReleaseMutex(handle)
	}
	runtime.UnlockOSThread()
	_ = windows.CloseHandle(handle)
	if waitErr != nil || wait != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("initializer mutex was not held by the first process: wait=%d err=%v", wait, waitErr)
	}
}
