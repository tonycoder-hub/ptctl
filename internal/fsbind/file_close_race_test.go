package fsbind

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Removal refuses while a handle with the same identity is open. That probe
// runs under the session lock and must observe File.Close without a data race
// (run with -race).
func TestFileCloseAndOpenIdentityProbeDoNotRace(t *testing.T) {
	root, session := newSupportedSession(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(root, "probe.bin"), []byte("probe"), 0o600); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 64; attempt++ {
		file, err := session.OpenRootRegular(ctx, "probe.bin")
		if err != nil {
			t.Fatal(err)
		}
		raw := file.raw
		if !session.hasOpenIdentity(raw) {
			t.Fatal("open handle was not reported")
		}
		var group sync.WaitGroup
		group.Add(2)
		go func() {
			defer group.Done()
			_ = file.Close()
		}()
		go func() {
			defer group.Done()
			_ = session.hasOpenIdentity(raw)
		}()
		group.Wait()
		if session.hasOpenIdentity(raw) {
			t.Fatal("closed handle was still reported open")
		}
	}
}
