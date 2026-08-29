package cli

import (
	"strings"
	"testing"
)

func TestReconciliationCredentialBundleIsStrictAndBounded(t *testing.T) {
	raw := `{"schema":"ptctl.credentials/v1","site_cookie":"sid=site-secret","downloader_password":"client-secret"}`
	siteCredential, clientCredential, err := readReconciliationCredentialBundle(strings.NewReader(raw), "user")
	if err != nil || siteCredential.SecretValue() != "sid=site-secret" || clientCredential.UsernameValue() != "user" || clientCredential.PasswordValue() != "client-secret" {
		t.Fatalf("site=%#v client=%#v err=%v", siteCredential, clientCredential, err)
	}
	for _, invalid := range []string{
		`{}`,
		`{"schema":"ptctl.credentials/v2","site_cookie":"sid=x","downloader_password":"y"}`,
		`{"schema":"ptctl.credentials/v1","schema":"ptctl.credentials/v1","site_cookie":"sid=x","downloader_password":"y"}`,
		`{"schema":"ptctl.credentials/v1","site_cookie":"sid=x","downloader_password":"y","extra":"z"}`,
		`{"schema":"ptctl.credentials/v1","site_cookie":"sid=\ud800","downloader_password":"y"}`,
		`{"schema":"ptctl.credentials/v1","site_cookie":"sid=x","downloader_password":"y"} {}`,
	} {
		if _, _, err := readReconciliationCredentialBundle(strings.NewReader(invalid), "user"); err == nil || strings.Contains(err.Error(), "site-secret") || strings.Contains(err.Error(), "client-secret") {
			t.Fatalf("invalid bundle accepted or leaked: %q err=%v", invalid, err)
		}
	}
	oversized := strings.Repeat("x", maxCredentialBundleBytes+1)
	if _, _, err := readReconciliationCredentialBundle(strings.NewReader(oversized), "user"); err == nil {
		t.Fatal("oversized bundle accepted")
	}
}
