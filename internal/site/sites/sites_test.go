package sites_test

import (
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/sites"
	"github.com/tonycoder-hub/ptctl/internal/site/sitetest"
	"github.com/tonycoder-hub/ptctl/internal/site/tjupt"
)

// samples holds synthetic conformance inputs for every built-in adapter. A new
// adapter in sites.All fails TestBuiltinAdaptersConform until it is listed here.
func samples(t *testing.T) map[string]sitetest.Samples {
	t.Helper()
	cookie, err := site.NewCookieCredential("sid=SITETEST-CANARY")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]sitetest.Samples{
		"tjupt": {Credential: cookie, RemoteID: "1", BonusSelector: "1"},
	}
}

func TestBuiltinAdaptersConform(t *testing.T) {
	all := samples(t)
	for _, adapter := range sites.All() {
		id := adapter.Descriptor().ID
		t.Run(id, func(t *testing.T) {
			input, ok := all[id]
			if !ok {
				t.Fatalf("add synthetic sitetest samples for %q", id)
			}
			sitetest.TestAdapter(t, adapter, input)
		})
	}
}

// A TJUPT adapter bound to any other base URL must still conform: it keeps the
// direct reads and refuses every origin-pinned port instead of retargeting it.
func TestTJUPTMirrorConformsWithOnlyDirectReads(t *testing.T) {
	adapter := tjupt.New("https://mirror.example/")
	want := []domain.Capability{domain.CapabilityAuthCheck, domain.CapabilityAccountRead, domain.CapabilitySearch, domain.CapabilityBonusRead}
	got := adapter.Descriptor().Capabilities
	if len(got) != len(want) {
		t.Fatalf("capabilities=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("capabilities=%v want %v", got, want)
		}
	}
	sitetest.TestAdapter(t, adapter, samples(t)["tjupt"])
}

func TestNewRegistryRegistersEveryBuiltinAdapter(t *testing.T) {
	registry := sites.NewRegistry()
	all := sites.All()
	if len(registry.Descriptors()) != len(all) {
		t.Fatalf("registry has %d adapters, want %d", len(registry.Descriptors()), len(all))
	}
	for _, adapter := range all {
		if _, ok := registry.Get(adapter.Descriptor().ID); !ok {
			t.Fatalf("%q is not registered", adapter.Descriptor().ID)
		}
	}
}
