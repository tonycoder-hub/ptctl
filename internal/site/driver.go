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

func NewAPIKeyCredential(key string) (Credential, error) {
	return NewCredential(domain.AuthMethodAPIKeyHeader, key)
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

// SearchOptions narrows one search beyond its query. The zero value asks for
// the site's default first page, the same search TorrentSearcher.Search runs.
type SearchOptions struct {
	// Page is 1-based; zero means the first page.
	Page int
	// Mode is a site-defined search area, such as M-Team's "adult"; empty
	// means the site's default area.
	Mode string
	// Categories keeps only torrents in these site category IDs.
	Categories []string
	// FreeOnly keeps only torrents whose download is not counted.
	FreeOnly bool
}

// IsZero reports whether the options ask for nothing beyond the default
// first page.
func (options SearchOptions) IsZero() bool {
	return options.Page == 0 && options.Mode == "" && len(options.Categories) == 0 && !options.FreeOnly
}

// OptionTorrentSearcher is a TorrentSearcher that also takes SearchOptions.
// ValidateSearchOptions runs before any credential is read, so an option the
// site cannot honor is refused without a request.
type OptionTorrentSearcher interface {
	TorrentSearcher
	ValidateSearchOptions(SearchOptions) error
	SearchWithOptions(context.Context, Credential, string, SearchOptions) ([]domain.TorrentSummary, error)
}

type BonusCatalogReader interface {
	BonusCatalog(context.Context, Credential) (domain.BonusCatalog, error)
}

// AccountTorrentQuery picks one page of one of the account's own torrent
// lists.
type AccountTorrentQuery struct {
	State domain.AccountTorrentState
	// Page is 1-based; zero means the first page.
	Page int
}

// AccountTorrentLister reads the account's own torrent lists.
// ValidateAccountTorrentQuery runs before any credential is read, so a state
// or page the site cannot serve is refused without a request.
type AccountTorrentLister interface {
	ValidateAccountTorrentQuery(AccountTorrentQuery) error
	AccountTorrents(context.Context, Credential, AccountTorrentQuery) (domain.AccountTorrentList, error)
}

type Registry struct {
	adapters map[string]Adapter
}

// NewRegistry panics on a nil adapter, an empty site ID, or a duplicate site
// ID. Registration is a programming decision, so a silent overwrite would hide
// a wiring bug behind whichever adapter happened to be listed last.
func NewRegistry(adapters ...Adapter) *Registry {
	r := &Registry{adapters: make(map[string]Adapter, len(adapters))}
	for _, adapter := range adapters {
		if adapter == nil {
			panic("site: nil adapter registered")
		}
		id := adapter.Descriptor().ID
		if id == "" {
			panic("site: adapter registered with an empty site ID")
		}
		if _, exists := r.adapters[id]; exists {
			panic(fmt.Sprintf("site: duplicate adapter registered for site ID %q", id))
		}
		r.adapters[id] = adapter
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
