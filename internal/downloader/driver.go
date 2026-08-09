package downloader

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Credential struct {
	username string
	password string
}

func NewCredential(username, password string) (Credential, error) {
	if password == "" {
		return Credential{}, fmt.Errorf("empty downloader password")
	}
	if len(password) > 64*1024 {
		return Credential{}, fmt.Errorf("downloader password exceeds 64 KiB")
	}
	return Credential{username: username, password: password}, nil
}

func (c Credential) UsernameValue() string { return c.username }
func (c Credential) PasswordValue() string { return c.password }
func (c Credential) String() string        { return "[REDACTED]" }
func (c Credential) GoString() string      { return "downloader.Credential{[REDACTED]}" }

type Status struct {
	Driver        string    `json:"driver"`
	Endpoint      string    `json:"endpoint"`
	Version       string    `json:"version"`
	WebAPIVersion string    `json:"web_api_version,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
}

// IdentityStatus describes whether a downloader job exposed a usable, typed
// BitTorrent identity. The opaque job key remains separate because downloader
// APIs do not give that field portable v1/v2 semantics.
type IdentityStatus string

const (
	IdentityStatusUnavailable IdentityStatus = "unavailable"
	IdentityStatusValid       IdentityStatus = "valid"
	IdentityStatusInvalid     IdentityStatus = "invalid"
)

type Torrent struct {
	Hash             string         `json:"hash"`
	InfoHashV1       string         `json:"info_hash_v1,omitempty"`
	InfoHashV2       string         `json:"info_hash_v2,omitempty"`
	IdentityStatus   IdentityStatus `json:"identity_status"`
	IdentityEvidence []string       `json:"identity_evidence"`
	IdentityIssues   []string       `json:"identity_issues"`
	Name             string         `json:"name"`
	SizeBytes        int64          `json:"size_bytes"`
	Progress         float64        `json:"progress"`
	State            string         `json:"state"`
	SavePath         string         `json:"save_path"`
	ContentPath      string         `json:"content_path,omitempty"`
	Downloaded       int64          `json:"downloaded_bytes"`
	Uploaded         int64          `json:"uploaded_bytes"`
}

// LedgerCapabilities declare which normalized facts a downloader read ledger
// can expose. A true value is a capability, not a claim that every job supplied
// that fact.
type LedgerCapabilities struct {
	TypedInfoHashes bool `json:"typed_info_hashes"`
	ContentPath     bool `json:"content_path"`
	RawMetafile     bool `json:"raw_metafile"`
	JobFiles        bool `json:"job_files"`
}

// LedgerSnapshot is one bounded observation of downloader jobs. Observation
// timestamps bracket the complete request and parse, rather than pretending
// that all jobs were sampled atomically.
type LedgerSnapshot struct {
	Driver          string             `json:"driver"`
	ObservedAtStart time.Time          `json:"observed_at_start"`
	ObservedAtEnd   time.Time          `json:"observed_at_end"`
	Complete        bool               `json:"complete"`
	Capabilities    LedgerCapabilities `json:"capabilities"`
	Jobs            []Torrent          `json:"jobs"`
}

type JobFileSelection string

const (
	JobFileSelectionSelected JobFileSelection = "selected"
	JobFileSelectionSkipped  JobFileSelection = "skipped"
)

type JobFile struct {
	Index              int              `json:"index"`
	RelativeComponents []string         `json:"relative_components"`
	SizeBytes          int64            `json:"size_bytes"`
	Progress           float64          `json:"progress"`
	Selection          JobFileSelection `json:"selection"`
	Complete           bool             `json:"complete"`
}

const (
	defaultJobFileLedgerMaxFiles         = 10_000
	defaultJobFileLedgerMaxPathBytes     = int64(16 << 20)
	defaultJobFileLedgerMaxResponseBytes = int64(8 << 20)

	hardJobFileLedgerMaxFiles         = 100_000
	hardJobFileLedgerMaxPathBytes     = int64(64 << 20)
	hardJobFileLedgerMaxResponseBytes = int64(32 << 20)
)

// JobFileLedgerLimits are mandatory work and retention budgets. Callers may
// tighten the defaults but cannot disable a limit or exceed its hard cap.
type JobFileLedgerLimits struct {
	MaxFiles         int   `json:"max_files"`
	MaxPathBytes     int64 `json:"max_path_bytes"`
	MaxResponseBytes int64 `json:"max_response_bytes"`
}

func DefaultJobFileLedgerLimits() JobFileLedgerLimits {
	return JobFileLedgerLimits{
		MaxFiles:         defaultJobFileLedgerMaxFiles,
		MaxPathBytes:     defaultJobFileLedgerMaxPathBytes,
		MaxResponseBytes: defaultJobFileLedgerMaxResponseBytes,
	}
}

func (limits JobFileLedgerLimits) Validate() error {
	if limits.MaxFiles <= 0 || limits.MaxFiles > hardJobFileLedgerMaxFiles {
		return fmt.Errorf("maximum job files must be in 1..%d", hardJobFileLedgerMaxFiles)
	}
	if limits.MaxPathBytes <= 0 || limits.MaxPathBytes > hardJobFileLedgerMaxPathBytes {
		return fmt.Errorf("maximum job-file path bytes must be in 1..%d", hardJobFileLedgerMaxPathBytes)
	}
	if limits.MaxResponseBytes <= 0 || limits.MaxResponseBytes > hardJobFileLedgerMaxResponseBytes {
		return fmt.Errorf("maximum job-file response bytes must be in 1..%d", hardJobFileLedgerMaxResponseBytes)
	}
	return nil
}

type JobFileLedgerUsage struct {
	FilesConsidered int   `json:"files_considered"`
	PathBytes       int64 `json:"path_bytes"`
	ResponseBytes   int64 `json:"response_bytes"`
}

// JobFileLedgerSnapshot is one bounded observation of a single downloader
// job's effective file paths and selection state. JobKey is process-local
// request authority and is intentionally omitted from serialized reports.
type JobFileLedgerSnapshot struct {
	Driver          string              `json:"driver"`
	JobKey          string              `json:"-"`
	ObservedAtStart time.Time           `json:"observed_at_start"`
	ObservedAtEnd   time.Time           `json:"observed_at_end"`
	Complete        bool                `json:"complete"`
	Limits          JobFileLedgerLimits `json:"limits"`
	Used            JobFileLedgerUsage  `json:"used"`
	Files           []JobFile           `json:"files"`
}

// LedgerSession reuses one authenticated read-only downloader session. The
// request count includes authentication. Close releases idle connections and
// must not perform an effectful logout request.
type LedgerSession interface {
	ReadLedger(context.Context) (LedgerSnapshot, error)
	ReadJobFiles(context.Context, string, JobFileLedgerLimits) (JobFileLedgerSnapshot, error)
	RequestsMade() int
	Close() error
}

// RequestCountError lets audit-oriented callers retain the exact number of
// attempted HTTP requests when opening a read session fails before a session
// value can be returned.
type RequestCountError interface {
	error
	RequestsMade() int
}

// RequestsMadeFromError extracts an adapter's request count without depending
// on its concrete error type. The boolean is false when the error carries no
// audited count.
func RequestsMadeFromError(err error) (int, bool) {
	var counted RequestCountError
	if !errors.As(err, &counted) {
		return 0, false
	}
	requests := counted.RequestsMade()
	if requests < 0 {
		return 0, false
	}
	return requests, true
}

type Driver interface {
	Status(context.Context, Credential) (Status, error)
	Torrents(context.Context, Credential) ([]Torrent, error)
}
