package site

import (
	"context"
	"fmt"
	"sort"

	"github.com/tonycoder-hub/ptctl/internal/domain"
)

// Credential is an opaque, method-tagged secret. Adapters must reject auth
// methods they do not explicitly declare.
type Credential struct {
	method domain.AuthMethod
	secret string
}

func NewCookieCredential(cookie string) (Credential, error) {
	return NewCredential(domain.AuthMethodCookieHeader, cookie)
}

func NewCredential(method domain.AuthMethod, secret string) (Credential, error) {
	if method == "" {
		return Credential{}, fmt.Errorf("empty authentication method")
	}
	if secret == "" {
		return Credential{}, fmt.Errorf("empty session credential")
	}
	if len(secret) > 64*1024 {
		return Credential{}, fmt.Errorf("session credential exceeds 64 KiB")
	}
	for _, ch := range []byte(secret) {
		if ch < 0x20 || ch == 0x7f {
			return Credential{}, fmt.Errorf("session credential contains a control character")
		}
	}
	return Credential{method: method, secret: secret}, nil
}

func (c Credential) Method() domain.AuthMethod { return c.method }
func (c Credential) SecretValue() string       { return c.secret }
func (c Credential) String() string            { return "[REDACTED]" }
func (c Credential) GoString() string          { return "site.Credential{[REDACTED]}" }

type Adapter interface {
	Descriptor() domain.SiteDescriptor
}

type AuthChecker interface {
	CheckSession(context.Context, Credential) (domain.SessionStatus, error)
}

type AccountReader interface {
	Account(context.Context, Credential) (domain.AccountSnapshot, error)
}

type TorrentSearcher interface {
	Search(context.Context, Credential, string) ([]domain.TorrentSummary, error)
}

type BonusCatalogReader interface {
	BonusCatalog(context.Context, Credential) (domain.BonusCatalog, error)
}

type Registry struct {
	adapters map[string]Adapter
}

func NewRegistry(adapters ...Adapter) *Registry {
	r := &Registry{adapters: make(map[string]Adapter, len(adapters))}
	for _, adapter := range adapters {
		r.adapters[adapter.Descriptor().ID] = adapter
	}
	return r
}

func (r *Registry) Get(id string) (Adapter, bool) {
	d, ok := r.adapters[id]
	return d, ok
}

func (r *Registry) Descriptors() []domain.SiteDescriptor {
	items := make([]domain.SiteDescriptor, 0, len(r.adapters))
	for _, d := range r.adapters {
		items = append(items, d.Descriptor())
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}
