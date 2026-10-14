//go:build !windows

package metafile

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestContentOpensRejectFIFOWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, open := range map[string]SourceOpener{
		"verify": func() (SourceFile, error) { return openFileSpec(fileSpec{path: fifo}) },
		"source": func() (SourceFile, error) { return openSourceBinding(SourceBinding{Path: fifo}) },
	} {
		done := make(chan error, 1)
		go func() {
			file, err := open()
			if err == nil {
				_ = file.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("%s opened a FIFO as content", name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s blocked opening a FIFO", name)
		}
	}
}
