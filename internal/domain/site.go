package domain

import "time"

// Capability is a stable, negotiated site feature. A missing capability must
// never be emulated by guessing a site's HTML.
type Capability string

type AuthMethod string

const (
	AuthMethodCookieHeader AuthMethod = "cookie_header"
)

const (
	CapabilityAuthCheck   Capability = "auth.check"
	CapabilityAccountRead Capability = "account.read"
	CapabilitySearch      Capability = "torrent.search"
	CapabilityDetail      Capability = "torrent.detail"
	CapabilityMetafile    Capability = "torrent.metafile.read_effectful"
	CapabilityBonusRead   Capability = "bonus.catalog.read"
)

type SiteDescriptor struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	BaseURL      string       `json:"base_url"`
	Stability    string       `json:"stability"`
	AuthMethods  []AuthMethod `json:"auth_methods"`
	Capabilities []Capability `json:"capabilities"`
}

func (d SiteDescriptor) Supports(capability Capability) bool {
	for _, candidate := range d.Capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

func (d SiteDescriptor) SupportsAuth(method AuthMethod) bool {
	for _, candidate := range d.AuthMethods {
		if candidate == method {
			return true
		}
	}
	return false
}

type AuthenticationState string

const (
	AuthenticationAuthenticated   AuthenticationState = "authenticated"
	AuthenticationUnauthenticated AuthenticationState = "unauthenticated"
	AuthenticationIndeterminate   AuthenticationState = "indeterminate"
)

type SessionStatus struct {
	State      AuthenticationState `json:"state"`
	Username   string              `json:"username,omitempty"`
	ObservedAt time.Time           `json:"observed_at"`
}

type AccountSnapshot struct {
	SiteID          string    `json:"site_id"`
	Username        string    `json:"username,omitempty"`
	UploadedBytes   *int64    `json:"uploaded_bytes,omitempty"`
	DownloadedBytes *int64    `json:"downloaded_bytes,omitempty"`
	Ratio           string    `json:"ratio,omitempty"`
	Bonus           string    `json:"bonus,omitempty"`
	Seeding         *int      `json:"seeding,omitempty"`
	Leeching        *int      `json:"leeching,omitempty"`
	ObservedAt      time.Time `json:"observed_at"`
}

type TorrentRef struct {
	SiteID   string `json:"site_id"`
	RemoteID string `json:"remote_id"`
}

// SiteMetafileObservation is the public, content-free proof produced when a
// site adapter's one effectful fetch is imported as an exact metafile variant.
// Origin and RouteID identify the adapter-controlled route that was actually
// observed; neither value is inferred from TorrentRef or an info hash.
type SiteMetafileObservation struct {
	Ref               TorrentRef `json:"ref"`
	Origin            string     `json:"origin"`
	RouteID           string     `json:"route_id"`
	MetafileVariantID string     `json:"metafile_variant_id"`
	Basis             string     `json:"basis"`
	ObservedAtStart   time.Time  `json:"observed_at_start"`
	ObservedAtEnd     time.Time  `json:"observed_at_end"`
	ResponseBytes     int64      `json:"response_bytes"`
}

type TorrentSummary struct {
	Ref         TorrentRef `json:"ref"`
	Name        string     `json:"name"`
	SizeBytes   *int64     `json:"size_bytes,omitempty"`
	Seeders     *int       `json:"seeders,omitempty"`
	Leechers    *int       `json:"leechers,omitempty"`
	Snatches    *int       `json:"snatches,omitempty"`
	Category    string     `json:"category,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	Promotion   *Promotion `json:"promotion,omitempty"`
}

type Promotion struct {
	UploadFactor   string     `json:"upload_factor"`
	DownloadFactor string     `json:"download_factor"`
	EndsAt         *time.Time `json:"ends_at,omitempty"`
}

// BonusCatalogRow intentionally preserves site-defined columns. Bonus shop
// semantics are not portable enough to force into a universal purchase model.
type BonusCatalogRow struct {
	Columns []string `json:"columns"`
}

type BonusCatalog struct {
	SiteID     string            `json:"site_id"`
	Balance    string            `json:"balance,omitempty"`
	Rows       []BonusCatalogRow `json:"rows"`
	ObservedAt time.Time         `json:"observed_at"`
}
