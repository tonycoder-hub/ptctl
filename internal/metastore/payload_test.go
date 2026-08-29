package metastore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPayloadIsExactOneShotAndRedacted(t *testing.T) {
	const canary = "PRIVATE-METAFILE-PAYLOAD-CANARY"
	raw := testMetafile("https://tracker.invalid/announce?passkey="+canary, "payload.bin", []byte("payload content"))
	root := filepath.Join(physicalTempDir(t), "payload-store-path-canary")
	store, _, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	meta, ref, _, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := store.LoadPayload(context.Background(), ref.ID, DefaultLimits())
	if err != nil || !payload.Matches(meta) || payload.VariantID() != ref.MetafileVariantID || payload.SizeBytes() != int64(len(raw)) {
		t.Fatalf("payload=%#v err=%v", payload, err)
	}
	reader, err := payload.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("payload changed: %q err=%v", got, err)
	}
	if _, err := payload.Open(); err == nil {
		t.Fatal("one-shot payload opened twice")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	public := string(encoded) + fmt.Sprintf("%s %#v", payload, payload)
	for _, secret := range []string{canary, root, string(raw)} {
		if strings.Contains(public, secret) {
			t.Fatalf("payload public representation leaked %q: %s", secret, public)
		}
	}
}
