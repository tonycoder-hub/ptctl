package site_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

type descriptorOnly struct{ descriptor domain.SiteDescriptor }

func (adapter descriptorOnly) Descriptor() domain.SiteDescriptor { return adapter.descriptor }

type authOnly struct{ descriptorOnly }

func (authOnly) CheckSession(context.Context, site.Credential) (domain.SessionStatus, error) {
	return domain.SessionStatus{}, nil
}

func validDescriptor() domain.SiteDescriptor {
	return domain.SiteDescriptor{
		ID:           "example",
		Name:         "Example / 示例",
		BaseURL:      "https://example.org/",
		Stability:    "experimental",
		AuthMethods:  []domain.AuthMethod{domain.AuthMethodCookieHeader},
		Capabilities: []domain.Capability{domain.CapabilityAuthCheck},
	}
}

func TestDescriptorOriginKeepsTheCLIAcceptanceRules(t *testing.T) {
	accepted := map[string]string{
		"https://www.tjupt.org/":      "https://www.tjupt.org",
		"https://www.tjupt.org":       "https://www.tjupt.org",
		"https://mirror.example:8443": "https://mirror.example:8443",
	}
	for baseURL, want := range accepted {
		got, err := site.DescriptorOrigin(domain.SiteDescriptor{BaseURL: baseURL})
		if err != nil || got != want {
			t.Fatalf("DescriptorOrigin(%q)=%q,%v want %q", baseURL, got, err, want)
		}
	}
	for _, baseURL := range []string{
		"", "http://example.org/", "https://", "https://user@example.org/", "https://example.org/path",
		"https://example.org/?q=1", "https://example.org/#top", "ftp://example.org/", "https://:443/",
	} {
		_, err := site.DescriptorOrigin(domain.SiteDescriptor{BaseURL: baseURL})
		if err == nil || err.Error() != "site descriptor origin is invalid" {
			t.Fatalf("DescriptorOrigin(%q) err=%v", baseURL, err)
		}
	}
}

func TestValidateDescriptorAcceptsCompleteDescriptors(t *testing.T) {
	if err := site.ValidateDescriptor(validDescriptor()); err != nil {
		t.Fatal(err)
	}
	all := validDescriptor()
	all.Capabilities = site.KnownCapabilities()
	if err := site.ValidateDescriptor(all); err != nil {
		t.Fatal(err)
	}
}

func TestValidateDescriptorRejectsMalformedDescriptors(t *testing.T) {
	tests := map[string]func(*domain.SiteDescriptor){
		"empty ID":              func(d *domain.SiteDescriptor) { d.ID = "" },
		"uppercase ID":          func(d *domain.SiteDescriptor) { d.ID = "Example" },
		"leading digit ID":      func(d *domain.SiteDescriptor) { d.ID = "1example" },
		"dotted ID":             func(d *domain.SiteDescriptor) { d.ID = "example.org" },
		"overlong ID":           func(d *domain.SiteDescriptor) { d.ID = "a" + strings.Repeat("b", 64) },
		"empty name":            func(d *domain.SiteDescriptor) { d.Name = "" },
		"empty stability":       func(d *domain.SiteDescriptor) { d.Stability = "" },
		"non-HTTPS base URL":    func(d *domain.SiteDescriptor) { d.BaseURL = "http://example.org/" },
		"base URL with path":    func(d *domain.SiteDescriptor) { d.BaseURL = "https://example.org/api" },
		"no auth methods":       func(d *domain.SiteDescriptor) { d.AuthMethods = nil },
		"unknown auth method":   func(d *domain.SiteDescriptor) { d.AuthMethods = []domain.AuthMethod{"password"} },
		"duplicate auth method": func(d *domain.SiteDescriptor) { d.AuthMethods = append(d.AuthMethods, domain.AuthMethodCookieHeader) },
		"unknown capability":    func(d *domain.SiteDescriptor) { d.Capabilities = []domain.Capability{"torrent.upload"} },
		"duplicate capability":  func(d *domain.SiteDescriptor) { d.Capabilities = append(d.Capabilities, domain.CapabilityAuthCheck) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			descriptor := validDescriptor()
			mutate(&descriptor)
			if err := site.ValidateDescriptor(descriptor); err == nil {
				t.Fatalf("accepted %+v", descriptor)
			}
		})
	}
	longest := validDescriptor()
	longest.ID = "a" + strings.Repeat("b", 63)
	if err := site.ValidateDescriptor(longest); err != nil {
		t.Fatalf("64-byte ID rejected: %v", err)
	}
}

func TestImplementsCapabilityChecksTheTypedPort(t *testing.T) {
	plain := descriptorOnly{validDescriptor()}
	checker := authOnly{plain}
	if !site.ImplementsCapability(checker, domain.CapabilityAuthCheck) {
		t.Fatal("auth checker not recognized")
	}
	if site.ImplementsCapability(plain, domain.CapabilityAuthCheck) {
		t.Fatal("descriptor-only adapter recognized as auth checker")
	}
	for _, capability := range site.KnownCapabilities() {
		if capability != domain.CapabilityAuthCheck && site.ImplementsCapability(checker, capability) {
			t.Fatalf("auth-only adapter recognized for %q", capability)
		}
	}
	if site.ImplementsCapability(checker, "torrent.upload") || site.ImplementsCapability(nil, domain.CapabilityAuthCheck) {
		t.Fatal("unknown capability or nil adapter recognized")
	}
}

func TestKnownCapabilitiesCoverEveryDomainCapabilityOnce(t *testing.T) {
	known := site.KnownCapabilities()
	seen := map[domain.Capability]bool{}
	for _, capability := range known {
		if seen[capability] {
			t.Fatalf("duplicate %q", capability)
		}
		seen[capability] = true
	}
	for _, capability := range []domain.Capability{
		domain.CapabilityAuthCheck, domain.CapabilityAccountRead, domain.CapabilitySearch, domain.CapabilityDetail,
		domain.CapabilityMetafile, domain.CapabilityBonusRead, domain.CapabilityBonusReview, domain.CapabilityBonusExchange,
	} {
		if !seen[capability] {
			t.Fatalf("missing %q", capability)
		}
	}
	known[0] = "mutated"
	if site.KnownCapabilities()[0] == "mutated" {
		t.Fatal("KnownCapabilities shares its backing array")
	}
}

func TestRegistryRejectsAmbiguousRegistration(t *testing.T) {
	if got := site.NewRegistry().Descriptors(); len(got) != 0 {
		t.Fatalf("empty registry descriptors=%v", got)
	}
	named := func(id string) site.Adapter {
		descriptor := validDescriptor()
		descriptor.ID = id
		return descriptorOnly{descriptor}
	}
	tests := map[string][]site.Adapter{
		"nil adapter":  {nil},
		"empty ID":     {named("")},
		"duplicate ID": {named("example"), named("other"), named("example")},
	}
	for name, adapters := range tests {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewRegistry did not panic")
				}
			}()
			site.NewRegistry(adapters...)
		})
	}
	registry := site.NewRegistry(named("zeta"), named("alpha"))
	descriptors := registry.Descriptors()
	if len(descriptors) != 2 || descriptors[0].ID != "alpha" || descriptors[1].ID != "zeta" {
		t.Fatalf("descriptors=%v", descriptors)
	}
	if _, ok := registry.Get("alpha"); !ok {
		t.Fatal("registered adapter not found")
	}
	if _, ok := registry.Get("missing"); ok {
		t.Fatal("unregistered adapter found")
	}
}
