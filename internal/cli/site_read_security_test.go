package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

type declaredOnlySiteAdapter struct {
	capability domain.Capability
}

func (adapter *declaredOnlySiteAdapter) Descriptor() domain.SiteDescriptor {
	return domain.SiteDescriptor{
		ID: "declared-only", Name: "Declared only", BaseURL: "https://declared.invalid/", Stability: "test",
		AuthMethods:  []domain.AuthMethod{domain.AuthMethodCookieHeader},
		Capabilities: []domain.Capability{adapter.capability},
	}
}

type failingStatusAdapter struct {
	calls int
}

func (*failingStatusAdapter) Descriptor() domain.SiteDescriptor {
	return domain.SiteDescriptor{
		ID: "failing-status", Name: "Failing status", BaseURL: "https://status.invalid/", Stability: "test",
		AuthMethods:  []domain.AuthMethod{domain.AuthMethodCookieHeader},
		Capabilities: []domain.Capability{domain.CapabilityAuthCheck},
	}
}

func (adapter *failingStatusAdapter) CheckSession(context.Context, site.Credential) (domain.SessionStatus, error) {
	adapter.calls++
	return domain.SessionStatus{}, errors.New("COOKIE-CANARY RESPONSE-CANARY")
}

func TestSiteReadChecksTypedPortBeforeCredentialInput(t *testing.T) {
	tests := []struct {
		command    string
		capability domain.Capability
		args       []string
	}{
		{command: "status", capability: domain.CapabilityAuthCheck, args: []string{"--cookie-stdin", "declared-only"}},
		{command: "account", capability: domain.CapabilityAccountRead, args: []string{"--cookie-stdin", "declared-only"}},
		{command: "search", capability: domain.CapabilitySearch, args: []string{"--cookie-stdin", "declared-only", "query"}},
		{command: "bonus-catalog", capability: domain.CapabilityBonusRead, args: []string{"--cookie-stdin", "declared-only"}},
	}
	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			secret := &trackingReader{}
			application := &app{
				stdin: secret, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
				registry: site.NewRegistry(&declaredOnlySiteAdapter{capability: test.capability}),
			}
			err := application.siteRead(test.command, test.args)
			if err == nil || !strings.Contains(err.Error(), "does not implement its typed port") || secret.read {
				t.Fatalf("err=%v secretRead=%t", err, secret.read)
			}
		})
	}
}

func TestSiteSearchRejectsBlankQueryBeforeCredentialInput(t *testing.T) {
	secret := &trackingReader{}
	application := &app{
		stdin: secret, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		registry: site.NewRegistry(&declaredOnlySiteAdapter{capability: domain.CapabilitySearch}),
	}
	err := application.siteRead("search", []string{"--cookie-stdin", "declared-only", "   "})
	if err == nil || !strings.Contains(err.Error(), "query is empty") || secret.read {
		t.Fatalf("err=%v secretRead=%t", err, secret.read)
	}
}

func TestSiteReadDoesNotPropagateAdapterSecretErrors(t *testing.T) {
	adapter := &failingStatusAdapter{}
	application := &app{
		stdin: strings.NewReader("sid=COOKIE-CANARY"), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		registry: site.NewRegistry(adapter),
	}
	err := application.siteRead("status", []string{"--cookie-stdin", "failing-status"})
	if err == nil || adapter.calls != 1 || strings.Contains(err.Error(), "CANARY") || err.Error() != "site status failed" {
		t.Fatalf("err=%v calls=%d", err, adapter.calls)
	}
}
