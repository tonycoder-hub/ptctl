// Package sites lists the built-in site adapters. It is the only place the
// CLI learns which sites exist; adding a site means adding its constructor to
// All and its synthetic samples to the conformance test. See
// docs/ADDING_A_SITE.md.
package sites

import (
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/tjupt"
)

// All returns a fresh instance of every built-in adapter, each bound to its
// production origin.
func All() []site.Adapter {
	return []site.Adapter{
		tjupt.New(""),
	}
}

// NewRegistry registers every built-in adapter.
func NewRegistry() *site.Registry {
	return site.NewRegistry(All()...)
}
