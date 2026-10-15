package site

import (
	"fmt"
	"net/url"
	"regexp"

	"github.com/tonycoder-hub/ptctl/internal/domain"
)

// siteIDPattern fits inside every place a site ID is persisted or parsed:
// public refs (64 bytes), lowercase route-ID prefixes ([a-z0-9._-]), and the
// --site-ref flag ([A-Za-z0-9._:-], 128 bytes). New adapters must satisfy all
// of them at once.
var siteIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// DescriptorOrigin returns the canonical HTTPS origin of a descriptor's
// BaseURL. It is intentionally more permissive than the metafile config origin
// check: a single trailing "/" path is accepted because descriptors publish a
// browsable base URL, while fetch configs publish a bare origin.
func DescriptorOrigin(descriptor domain.SiteDescriptor) (string, error) {
	parsed, err := url.Parse(descriptor.BaseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("site descriptor origin is invalid")
	}
	return "https://" + parsed.Host, nil
}

// ValidateDescriptor checks the static shape every built-in adapter must
// publish. It is used by the adapter conformance suite rather than by
// NewRegistry, so narrow test doubles can still be registered.
func ValidateDescriptor(descriptor domain.SiteDescriptor) error {
	if !siteIDPattern.MatchString(descriptor.ID) {
		return fmt.Errorf("site descriptor ID %q must match %s", descriptor.ID, siteIDPattern)
	}
	if descriptor.Name == "" {
		return fmt.Errorf("site descriptor %q has an empty name", descriptor.ID)
	}
	if descriptor.Stability == "" {
		return fmt.Errorf("site descriptor %q has an empty stability", descriptor.ID)
	}
	if _, err := DescriptorOrigin(descriptor); err != nil {
		return fmt.Errorf("site descriptor %q: %w", descriptor.ID, err)
	}
	if len(descriptor.AuthMethods) == 0 {
		return fmt.Errorf("site descriptor %q declares no auth methods", descriptor.ID)
	}
	seenMethods := make(map[domain.AuthMethod]bool, len(descriptor.AuthMethods))
	for _, method := range descriptor.AuthMethods {
		if !knownAuthMethod(method) {
			return fmt.Errorf("site descriptor %q declares unknown auth method %q", descriptor.ID, method)
		}
		if seenMethods[method] {
			return fmt.Errorf("site descriptor %q declares auth method %q twice", descriptor.ID, method)
		}
		seenMethods[method] = true
	}
	seenCapabilities := make(map[domain.Capability]bool, len(descriptor.Capabilities))
	for _, capability := range descriptor.Capabilities {
		if _, known := capabilityPorts[capability]; !known {
			return fmt.Errorf("site descriptor %q declares unknown capability %q", descriptor.ID, capability)
		}
		if seenCapabilities[capability] {
			return fmt.Errorf("site descriptor %q declares capability %q twice", descriptor.ID, capability)
		}
		seenCapabilities[capability] = true
	}
	return nil
}

func knownAuthMethod(method domain.AuthMethod) bool {
	return method == domain.AuthMethodCookieHeader || method == domain.AuthMethodAPIKeyHeader
}

// capabilityPorts is the single mapping from a declared capability to the
// typed port an adapter must implement for it. Adding a capability means
// adding a domain constant, a port interface, and one entry here.
var capabilityPorts = map[domain.Capability]func(Adapter) bool{
	domain.CapabilityAuthCheck:     func(a Adapter) bool { _, ok := a.(AuthChecker); return ok },
	domain.CapabilityAccountRead:   func(a Adapter) bool { _, ok := a.(AccountReader); return ok },
	domain.CapabilitySearch:        func(a Adapter) bool { _, ok := a.(TorrentSearcher); return ok },
	domain.CapabilityBonusRead:     func(a Adapter) bool { _, ok := a.(BonusCatalogReader); return ok },
	domain.CapabilityDetail:        func(a Adapter) bool { _, ok := a.(TorrentDetailReader); return ok },
	domain.CapabilityMetafile:      func(a Adapter) bool { _, ok := a.(MetafileFetcher); return ok },
	domain.CapabilityBonusReview:   func(a Adapter) bool { _, ok := a.(BonusReviewReader); return ok },
	domain.CapabilityBonusExchange: func(a Adapter) bool { _, ok := a.(BonusExchangeWriter); return ok },
	domain.CapabilityLogin:         func(a Adapter) bool { _, ok := a.(SessionLoginer); return ok },
}

// KnownCapabilities returns every capability that has a typed port, in a
// stable order.
func KnownCapabilities() []domain.Capability {
	return []domain.Capability{
		domain.CapabilityAuthCheck,
		domain.CapabilityAccountRead,
		domain.CapabilitySearch,
		domain.CapabilityBonusRead,
		domain.CapabilityDetail,
		domain.CapabilityMetafile,
		domain.CapabilityBonusReview,
		domain.CapabilityBonusExchange,
		domain.CapabilityLogin,
	}
}

// ImplementsCapability reports whether adapter implements the typed port that
// backs capability. It does not consult the descriptor: a declared capability
// without its port, or a port without its declaration, are both adapter bugs
// that the conformance suite reports separately.
func ImplementsCapability(adapter Adapter, capability domain.Capability) bool {
	implements, known := capabilityPorts[capability]
	return known && adapter != nil && implements(adapter)
}

// ValidateOrigin reports whether origin is a canonical HTTPS origin suitable
// for a fetch, detail, or bonus config.
func ValidateOrigin(origin string) error { return validateCanonicalOrigin(origin) }

// ValidateRouteID reports whether routeID is a valid persisted route
// identifier.
func ValidateRouteID(routeID string) error { return validateRouteID(routeID) }
