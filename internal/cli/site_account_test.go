package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

type accountReadAdapter struct {
	snapshot   domain.AccountSnapshot
	calls      int
	credential string
}

func (*accountReadAdapter) Descriptor() domain.SiteDescriptor {
	return domain.SiteDescriptor{
		ID: "account-test", Name: "Account test", BaseURL: "https://account.invalid/", Stability: "test",
		AuthMethods:  []domain.AuthMethod{domain.AuthMethodCookieHeader},
		Capabilities: []domain.Capability{domain.CapabilityAccountRead},
	}
}

func (adapter *accountReadAdapter) Account(ctx context.Context, credential site.Credential) (domain.AccountSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return domain.AccountSnapshot{}, err
	}
	adapter.calls++
	adapter.credential = credential.SecretValue()
	return adapter.snapshot, nil
}

func TestSiteAccountUsesTypedPortAndSafeEnvelope(t *testing.T) {
	const secret = "sid=COOKIE-CANARY"
	observed := time.Date(2026, 8, 12, 3, 4, 5, 0, time.UTC)
	adapter := &accountReadAdapter{snapshot: domain.AccountSnapshot{
		SiteID: "account-test", Username: "Alice", Bonus: "12345.67", ObservedAt: observed,
	}}
	var out, errOut bytes.Buffer
	application := &app{
		stdin: strings.NewReader(secret), stdout: &out, stderr: &errOut,
		registry: site.NewRegistry(adapter),
	}
	if err := application.siteRead("account", []string{"--cookie-stdin", "--output", "json", "account-test"}); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 1 || adapter.credential != secret || errOut.Len() != 0 || strings.Contains(out.String(), secret) {
		t.Fatalf("calls=%d credential=%q stdout=%q stderr=%q", adapter.calls, adapter.credential, out.String(), errOut.String())
	}
	var envelope struct {
		Schema string                 `json:"schema"`
		Kind   string                 `json:"kind"`
		Data   domain.AccountSnapshot `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Schema != "ptctl.dev/v1" || envelope.Kind != "site.account" || envelope.Data != adapter.snapshot {
		t.Fatalf("envelope=%#v", envelope)
	}
}
