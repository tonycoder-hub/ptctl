//go:build linux || darwin

package metastore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// returnsWithoutBlocking runs operation and fails the test if it is still
// blocked after a generous deadline. A reader blocked in open(2) on the FIFO is
// then released by opening the write side, so the goroutine cannot leak.
func returnsWithoutBlocking(t *testing.T, fifo string, operation func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- operation() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
	}
	if writer, err := os.OpenFile(fifo, os.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
		_ = writer.Close()
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	t.Fatal("operation blocked opening a FIFO planted at a store name")
	return nil
}

func plantFIFO(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBoundOpenRejectsFIFOWithoutBlocking(t *testing.T) {
	t.Run("staging name", func(t *testing.T) {
		store := newTestStore(t)
		fifo := filepath.Join(store.root, temporaryDir, "planted")
		plantFIFO(t, fifo)
		session, err := store.validatedSession()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		err = returnsWithoutBlocking(t, fifo, func() error {
			file, _, openErr := session.openValidated(filepath.Join(temporaryDir, "planted"), false)
			if file != nil {
				_ = file.Close()
			}
			return openErr
		})
		if err == nil {
			t.Fatal("bound open accepted a FIFO")
		}
	})
	t.Run("store marker", func(t *testing.T) {
		store := newTestStore(t)
		fifo := filepath.Join(store.root, markerName)
		plantFIFO(t, fifo)
		err := returnsWithoutBlocking(t, fifo, func() error {
			_, openErr := Open(store.root)
			return openErr
		})
		if err == nil {
			t.Fatal("Open accepted a FIFO store marker")
		}
	})
	t.Run("artifact object", func(t *testing.T) {
		store := newTestStore(t)
		raw := testMetafile("https://tracker.invalid/announce", "volume.bin", []byte("volume"))
		_, ref, _, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		fifo := filepath.Join(store.root, objectRelativePath(ref.ID))
		plantFIFO(t, fifo)
		err = returnsWithoutBlocking(t, fifo, func() error {
			_, _, loadErr := store.Load(context.Background(), ref.ID, DefaultLimits())
			return loadErr
		})
		if err == nil {
			t.Fatal("Load accepted a FIFO artifact object")
		}
	})
	t.Run("regular object stays blocking", func(t *testing.T) {
		store := newTestStore(t)
		session, err := store.validatedSession()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		file, _, err := session.openValidated(markerName, false)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
		if err != nil || flags&unix.O_NONBLOCK != 0 {
			t.Fatalf("validated handle flags=%#x err=%v", flags, err)
		}
	})
}

func TestImportSourceOpenRejectsFIFOWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(physicalTempDir(t), "source.torrent")
	plantFIFO(t, fifo)
	err := returnsWithoutBlocking(t, fifo, func() error {
		file, openErr := platformOpenSourceFile(fifo)
		if file != nil {
			_ = file.Close()
		}
		return openErr
	})
	if err == nil {
		t.Fatal("source open accepted a FIFO")
	}

	regular := filepath.Join(physicalTempDir(t), "regular.torrent")
	raw := testMetafile("https://tracker.invalid/announce", "volume.bin", []byte("volume"))
	if err := os.WriteFile(regular, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := platformOpenSourceFile(regular)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_NONBLOCK != 0 {
		t.Fatalf("source handle flags=%#x err=%v", flags, err)
	}
	store := newTestStore(t)
	if _, _, receipt, err := store.ImportFile(context.Background(), regular, DefaultLimits()); err != nil || receipt.WritesPerformed != 1 {
		t.Fatalf("ImportFile receipt=%+v err=%v", receipt, err)
	}
}
