package site

import (
	"context"
	"fmt"
	"sort"

	"github.com/tonycoder-hub/ptctl/internal/domain"
)

// Credential is deliberately opaque to callers and must never be formatted.
// The first release accepts it only from stdin and keeps it in memory.
type Credential struct {
	cookie string
}

func NewCookieCredential(cookie string) (Credential, error) {
	if cookie == "" {
		return Credential{}, fmt.Errorf("empty session credential")
	}
	if len(cookie) > 64*1024 {
		return Credential{}, fmt.Errorf("session credential exceeds 64 KiB")
	}
	return Credential{cookie: cookie}, nil
}

func (c Credential) CookieValue() string { return c.cookie }

type Driver interface {
	Descriptor() domain.SiteDescriptor
	CheckSession(context.Context, Credential) (domain.SessionStatus, error)
	Account(context.Context, Credential) (domain.AccountSnapshot, error)
	Search(context.Context, Credential, string) ([]domain.TorrentSummary, error)
	BonusCatalog(context.Context, Credential) (domain.BonusCatalog, error)
}

type Registry struct {
	drivers map[string]Driver
}

func NewRegistry(drivers ...Driver) *Registry {
	r := &Registry{drivers: make(map[string]Driver, len(drivers))}
	for _, driver := range drivers {
		r.drivers[driver.Descriptor().ID] = driver
	}
	return r
}

func (r *Registry) Get(id string) (Driver, bool) {
	d, ok := r.drivers[id]
	return d, ok
}

func (r *Registry) Descriptors() []domain.SiteDescriptor {
	items := make([]domain.SiteDescriptor, 0, len(r.drivers))
	for _, d := range r.drivers {
		items = append(items, d.Descriptor())
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}
