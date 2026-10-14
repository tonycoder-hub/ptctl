//go:build !windows

package storage

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestOpenHelpersRejectSpecialFilesWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	fifo := filepath.Join(dir, "fifo")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(regular, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	file, err := OpenRegular(regular)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || string(data) != "content" {
		t.Fatalf("regular file read %q, %v", data, err)
	}
	for _, test := range []struct {
		open func(string) (*os.File, error)
		path string
	}{{OpenRegular, fifo}, {OpenRegular, link}, {OpenRegular, dir}, {openDirectory, fifo}, {openDirectory, regular}} {
		done := make(chan error, 1)
		go func() {
			file, err := test.open(test.path)
			if err == nil {
				_ = file.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("%s was opened as the wrong kind of object", test.path)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("opening %s blocked", test.path)
		}
	}
}
