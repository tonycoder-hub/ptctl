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

func TestRedactTrackerAndBearerSecretsExactly(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "GET https://t.invalid/rss.php?rsskey=CANARY&x=1", want: "GET https://t.invalid/rss.php?rsskey=[REDACTED]&x=1"},
		{input: "query passkey=CANARY", want: "query passkey=[REDACTED]"},
		{input: "query authkey=CANARY&id=2", want: "query authkey=[REDACTED]&id=2"},
		{input: "torrent_pass=CANARY", want: "torrent_pass=[REDACTED]"},
		{input: "token=CANARY", want: "token=[REDACTED]"},
		{input: "next=https%3A%2F%2Ft.invalid%2Fx%3Fpasskey%3DCANARY%26a%3D1", want: "next=https%3A%2F%2Ft.invalid%2Fx%3Fpasskey%3D[REDACTED]"},
		{input: `{"passkey":"CANARY","id":1}`, want: `{"passkey":"[REDACTED]","id":1}`},
		{input: `{"torrent_pass": "CANARY", "rsskey":"CANARY"}`, want: `{"torrent_pass": "[REDACTED]", "rsskey":"[REDACTED]"}`},
		{input: `{"token": "CA\"NARY"}`, want: `{"token": "[REDACTED]"}`},
		{input: `{"Cookie":["a=CANARY","b=CANARY"]}`, want: `{"Cookie":"[REDACTED]"}`},
		{input: `{Passkey:CANARY Size:1}`, want: `{Passkey:[REDACTED] Size:1}`},
		{input: "Authorization: Bearer CANARY", want: "Authorization: [REDACTED]"},
		{input: "upstream said: bearer CANARY.TOKEN_123", want: "upstream said: bearer [REDACTED]"},
		{input: "Cookie: c_secure_uid=1; c_secure_pass=CANARY", want: "Cookie: [REDACTED]"},
		{input: "c_secure_pass=CANARY; c_secure_uid=1", want: "c_secure_pass=[REDACTED] c_secure_uid=1"},
		{input: "invalid token: expired", want: "invalid token: expired"},
		{input: "the passkey is required", want: "the passkey is required"},
		{input: "bearer of bad news", want: "bearer of bad news"},
		{input: "tokens=3; consider the session closed", want: "tokens=3; consider the session closed"},
		{input: `{"status":"blocked","secret_free":true,"tokens":2}`, want: `{"status":"blocked","secret_free":true,"tokens":2}`},
		{input: "the downloader answered without accepting the add", want: "the downloader answered without accepting the add"},
	}
	for _, test := range tests {
		if got := Redact(test.input); got != test.want {
			t.Fatalf("Redact(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}
