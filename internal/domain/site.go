package domain

import "time"

// Capability is a stable, negotiated site feature. A missing capability must
// never be emulated by guessing a site's HTML.
type Capability string

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
	Capabilities []Capability `json:"capabilities"`
}

type SessionStatus struct {
	Authenticated bool      `json:"authenticated"`
	Username      string    `json:"username,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
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
