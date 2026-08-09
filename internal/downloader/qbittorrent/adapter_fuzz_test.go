package qbittorrent

import (
	"encoding/hex"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

func FuzzParseMagnetIdentity(f *testing.F) {
	for _, seed := range []string{
		"",
		"magnet:?",
		"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
		"magnet:?xt=urn:btmh:12200123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"magnet:?xt=%20urn:btih:bad&tr=https%3A%2F%2Ftracker.invalid%2FCANARY",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		identity := parseMagnetIdentity(raw)
		if identity.status != downloader.IdentityStatusUnavailable && identity.status != downloader.IdentityStatusValid && identity.status != downloader.IdentityStatusInvalid {
			t.Fatalf("unknown identity status %q", identity.status)
		}
		if identity.status != downloader.IdentityStatusValid && (identity.v1 != "" || identity.v2 != "") {
			t.Fatal("non-valid identity retained a typed hash")
		}
		if identity.v1 != "" {
			decoded, err := hex.DecodeString(identity.v1)
			if err != nil || len(decoded) != 20 {
				t.Fatalf("non-canonical v1 identity %q", identity.v1)
			}
		}
		if identity.v2 != "" {
			decoded, err := hex.DecodeString(identity.v2)
			if err != nil || len(decoded) != 32 {
				t.Fatalf("non-canonical v2 identity %q", identity.v2)
			}
		}
	})
}
