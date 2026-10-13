package security

import (
	"strings"
	"testing"
)

func TestRedactCanarySecrets(t *testing.T) {
	canary := "CANARY-PT-SECRET-7f19"
	inputs := []string{
		"Cookie: session=" + canary,
		"Authorization: Bearer " + canary,
		"https://tracker.invalid/announce?passkey=" + canary,
		"request failed: https://site.invalid/download.php?id=1&authkey=" + canary,
		"password=" + canary,
		"password: " + canary,
		`{"cookie":"` + canary + `"}`,
		"https://site.invalid/?auth%6bey=" + canary,
		"SID=" + canary,
		"x-api-key: " + canary,
		"X-Api-Key=" + canary,
		`{"x-api-key": "` + canary + `"}`,
		`{"apiKey":"` + canary + `"}`,
	}
	for _, input := range inputs {
		got := Redact(input)
		if strings.Contains(got, canary) {
			t.Fatalf("secret leaked from %q: %q", input, got)
		}
	}
}
