package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

// Saved site credentials. 'site login' saves one credential per site under
// the user config directory, and every site command uses it when no
// --cookie-stdin or --api-key-stdin flag is given. An explicit flag always
// wins, so scripts keep full control over what is sent.

const maxSavedCredentialBytes = 64 << 10

// siteCredentialSource says where one site command reads its credential.
type siteCredentialSource struct {
	method domain.AuthMethod
	saved  bool
	// savedPath is the file 'site login' saved, set by locateSaved.
	savedPath string
}

// defaultSiteAuthMethod is the method a site signs in with when no flag names
// one: the API key for a site that only takes API keys, else the cookie.
func defaultSiteAuthMethod(descriptor domain.SiteDescriptor) domain.AuthMethod {
	if descriptor.SupportsAuth(domain.AuthMethodAPIKeyHeader) && !descriptor.SupportsAuth(domain.AuthMethodCookieHeader) {
		return domain.AuthMethodAPIKeyHeader
	}
	return domain.AuthMethodCookieHeader
}

func credentialStdinFlag(method domain.AuthMethod) string {
	if method == domain.AuthMethodAPIKeyHeader {
		return "--api-key-stdin"
	}
	return "--cookie-stdin"
}

func savedCredentialLabel(method domain.AuthMethod) string {
	if method == domain.AuthMethodAPIKeyHeader {
		return "API key"
	}
	return "session cookie"
}

// savedCredentialPath is where 'site login' keeps the credential of one site:
// <user config dir>/ptctl/sessions/SITE.cookie or SITE.api-key.
func savedCredentialPath(siteID string, method domain.AuthMethod) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	name := siteID + ".cookie"
	if method == domain.AuthMethodAPIKeyHeader {
		name = siteID + ".api-key"
	}
	return filepath.Join(dir, "ptctl", "sessions", name), nil
}

// siteCredentialSourceFor picks the credential method of one site command and
// checks the site accepts it. An explicit flag selects stdin; with neither
// flag the command uses the saved credential for the site's default method.
func siteCredentialSourceFor(descriptor domain.SiteDescriptor, cookieStdin, apiKeyStdin bool) (siteCredentialSource, error) {
	method := defaultSiteAuthMethod(descriptor)
	switch {
	case cookieStdin:
		method = domain.AuthMethodCookieHeader
	case apiKeyStdin:
		method = domain.AuthMethodAPIKeyHeader
	}
	if !descriptor.SupportsAuth(method) {
		return siteCredentialSource{}, usageError("site %q does not support %s authentication", descriptor.ID, method)
	}
	return siteCredentialSource{method: method, saved: !cookieStdin && !apiKeyStdin}, nil
}

// locateSaved finds the saved credential once every other usage check has
// passed, so a missing file is a usage error and nothing is read or sent.
func (source *siteCredentialSource) locateSaved(siteID string) error {
	if !source.saved {
		return nil
	}
	label, flag := savedCredentialLabel(source.method), credentialStdinFlag(source.method)
	path, err := savedCredentialPath(siteID, source.method)
	if err != nil {
		return usageError("no user config directory for a saved %s; pipe one with %s", label, flag)
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return usageError("no saved %s for site %q; run 'ptctl site login %s' first, or pipe one with %s", label, siteID, siteID, flag)
	}
	source.savedPath = path
	return nil
}

// readSiteCredential reads the credential from its source. Stdin keeps the
// command's own reader; a saved file is always parsed canonically.
func (a *app) readSiteCredential(source siteCredentialSource, fromStdin func(io.Reader) (site.Credential, error)) (site.Credential, error) {
	if !source.saved {
		return fromStdin(a.stdin)
	}
	if source.savedPath == "" {
		return site.Credential{}, fmt.Errorf("saved %s was not located", savedCredentialLabel(source.method))
	}
	return readSavedCredential(source.savedPath, source.method)
}

// siteCredentialReadError is what a report-writing command returns when its
// credential cannot be read. Stdin keeps the generic line; a saved file's
// error names the file and what is wrong with it, never its content.
func siteCredentialReadError(source siteCredentialSource, err error) error {
	if source.saved {
		return err
	}
	return fmt.Errorf("read site credential failed")
}

// readSavedCredential reads one saved credential. It refuses a symlink or
// other non-regular file, and on systems with Unix permissions a file that
// group or other users can access. The error texts name the file, never its
// content.
func readSavedCredential(path string, method domain.AuthMethod) (site.Credential, error) {
	label := savedCredentialLabel(method)
	info, err := os.Lstat(path)
	if err != nil {
		return site.Credential{}, fmt.Errorf("read saved %s %s failed", label, path)
	}
	if !info.Mode().IsRegular() {
		return site.Credential{}, fmt.Errorf("refusing to use saved %s %s because it is not a regular file", label, path)
	}
	file, err := os.Open(path)
	if err != nil {
		return site.Credential{}, fmt.Errorf("read saved %s %s failed", label, path)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return site.Credential{}, fmt.Errorf("saved %s %s changed while it was opened", label, path)
	}
	if runtime.GOOS != "windows" && opened.Mode().Perm()&0o077 != 0 {
		return site.Credential{}, fmt.Errorf("refusing to use saved %s %s because other users can access it; make it owner-only (chmod 600) or sign in again", label, path)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxSavedCredentialBytes+3))
	if err != nil {
		return site.Credential{}, fmt.Errorf("read saved %s %s failed", label, path)
	}
	if bytes.HasSuffix(raw, []byte("\r\n")) {
		raw = raw[:len(raw)-2]
	} else if bytes.HasSuffix(raw, []byte("\n")) {
		raw = raw[:len(raw)-1]
	}
	var credential site.Credential
	if method == domain.AuthMethodAPIKeyHeader {
		credential, err = canonicalAPIKeyCredential(raw)
	} else {
		credential, err = canonicalCookieCredential(raw)
	}
	if err != nil {
		return site.Credential{}, fmt.Errorf("saved %s %s is invalid; sign in again with 'ptctl site login'", label, path)
	}
	return credential, nil
}

// askBonusExchangePassword asks for the account password with echo off. The
// password confirms this one exchange and is never saved.
func (a *app) askBonusExchangePassword(terminal promptTerminal, descriptor domain.SiteDescriptor) (site.AccountPassword, error) {
	value, err := terminal.ReadSecret(func() {
		fmt.Fprintf(a.stderr, "%s confirms this exchange with the account password (hidden, not saved): ", terminalSafe(descriptor.Name))
	})
	fmt.Fprintln(a.stderr)
	if err != nil {
		return site.AccountPassword{}, fmt.Errorf("read the account password from the terminal failed; nothing was submitted")
	}
	password, err := site.NewAccountPassword(value)
	if err != nil {
		return site.AccountPassword{}, fmt.Errorf("bonus exchange account password is invalid; nothing was submitted")
	}
	return password, nil
}
