package downloader

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"
)

// MetafilePayload is process-local access to exact raw metafile bytes. Its
// implementation must not serialize the bytes or derive them from an
// infohash. Open is invoked at most once per AddStopped call.
type MetafilePayload interface {
	VariantID() string
	SizeBytes() int64
	Open() (io.Reader, error)
}

type AddStoppedRequest struct {
	Metafile MetafilePayload
	SavePath string
	// Identity is the reviewed typed identity of Metafile. Implementations
	// whose add response exposes identity must compare it exactly.
	Identity TypedIdentity
}

type MutationReceipt struct {
	Effect              string    `json:"effect"`
	ObservedAtStart     time.Time `json:"observed_at_start"`
	ObservedAtEnd       time.Time `json:"observed_at_end"`
	Complete            bool      `json:"complete"`
	RequestsAttempted   int       `json:"requests_attempted"`
	AutomaticRetries    int       `json:"automatic_retries"`
	RedirectsFollowed   int       `json:"redirects_followed"`
	BytesSubmitted      int64     `json:"bytes_submitted"`
	BytesSubmittedKnown bool      `json:"bytes_submitted_known"`
	StopReason          string    `json:"stop_reason,omitempty"`
}

// MutationSession performs bounded, explicitly acknowledged downloader
// changes in the same authenticated session used for before/after ledgers.
// Implementations must never automatically retry an effectful request.
type MutationSession interface {
	LedgerSession
	AddStopped(context.Context, AddStoppedRequest) (MutationReceipt, error)
}

// StoppedAddDriver is the deliberately narrow mutation port used by client
// adoption. It cannot recheck, start, move, remove, or otherwise control an
// existing job.
type StoppedAddDriver interface {
	LedgerDriver
	OpenMutationSession(context.Context, Credential) (MutationSession, error)
	ClientConfigID(string) (string, error)
}

// StoppedAddDescriptor is code-owned policy for one audited built-in stopped
// add implementation. In particular, a read adapter does not gain mutation
// authority merely by returning a matching Driver string in a snapshot.
type StoppedAddDescriptor struct {
	Driver         string
	OpenRequests   int
	SupportsV1     bool
	SupportsV2     bool
	SupportsHybrid bool
}

func DescribeStoppedAddDriver(driver string) (StoppedAddDescriptor, bool) {
	switch driver {
	case DriverQBittorrent:
		return StoppedAddDescriptor{
			Driver: DriverQBittorrent, OpenRequests: 1,
			SupportsV1: true, SupportsV2: true, SupportsHybrid: true,
		}, true
	case DriverTransmission:
		return StoppedAddDescriptor{
			Driver: DriverTransmission, OpenRequests: 2,
			SupportsV1: true,
		}, true
	default:
		return StoppedAddDescriptor{}, false
	}
}

func (descriptor StoppedAddDescriptor) SupportsIdentity(identity TypedIdentity) bool {
	if identity.Validate() != nil {
		return false
	}
	switch {
	case identity.InfoHashV1 != "" && identity.InfoHashV2 != "":
		return descriptor.SupportsHybrid
	case identity.InfoHashV1 != "":
		return descriptor.SupportsV1
	case identity.InfoHashV2 != "":
		return descriptor.SupportsV2
	default:
		return false
	}
}

const (
	ControlProtocolQBittorrentV4  = "qbittorrent_webapi_v4"
	ControlProtocolQBittorrentV5  = "qbittorrent_webapi_v5"
	ControlProtocolTransmissionV5 = "transmission_rpc_v5_3"
	ControlProtocolTransmissionV6 = "transmission_rpc_v6"

	ControlEffectRecheck = "request_existing_job_recheck"
	ControlEffectStart   = "request_existing_job_start"
)

// ExistingJobControlDescriptor is a bounded, normalized capability result for
// mutations of an already identified downloader job. Protocol is deliberately
// explicit because each built-in adapter has version-bound method names.
type ExistingJobControlDescriptor struct {
	Driver         string `json:"driver"`
	Protocol       string `json:"protocol"`
	RecheckRouteID string `json:"recheck_route_id"`
	StartRouteID   string `json:"start_route_id"`
}

func (descriptor ExistingJobControlDescriptor) Validate() error {
	switch descriptor.Driver {
	case DriverQBittorrent:
		if descriptor.RecheckRouteID != "qbittorrent.torrents.recheck.v1" {
			return fmt.Errorf("downloader existing-job control descriptor is invalid")
		}
		switch descriptor.Protocol {
		case ControlProtocolQBittorrentV4:
			if descriptor.StartRouteID != "qbittorrent.torrents.resume.v1" {
				return fmt.Errorf("downloader existing-job control descriptor is invalid")
			}
		case ControlProtocolQBittorrentV5:
			if descriptor.StartRouteID != "qbittorrent.torrents.start.v1" {
				return fmt.Errorf("downloader existing-job control descriptor is invalid")
			}
		default:
			return fmt.Errorf("downloader existing-job control protocol is unsupported")
		}
	case DriverTransmission:
		if descriptor.RecheckRouteID != "transmission.torrent.verify.v1" || descriptor.StartRouteID != "transmission.torrent.start.v1" {
			return fmt.Errorf("downloader existing-job control descriptor is invalid")
		}
		if descriptor.Protocol != ControlProtocolTransmissionV5 && descriptor.Protocol != ControlProtocolTransmissionV6 {
			return fmt.Errorf("downloader existing-job control protocol is unsupported")
		}
	default:
		return fmt.Errorf("downloader existing-job control descriptor is invalid")
	}
	return nil
}

// ExistingJobControlPolicy is code-owned request-count and identity policy for
// one audited built-in control adapter.
type ExistingJobControlPolicy struct {
	Driver             string
	OpenRequests       int
	DescriptorRequests int
}

func DescribeExistingJobControlDriver(driver string) (ExistingJobControlPolicy, bool) {
	switch driver {
	case DriverQBittorrent:
		return ExistingJobControlPolicy{Driver: driver, OpenRequests: 1, DescriptorRequests: 1}, true
	case DriverTransmission:
		return ExistingJobControlPolicy{Driver: driver, OpenRequests: 2, DescriptorRequests: 0}, true
	default:
		return ExistingJobControlPolicy{}, false
	}
}

func (policy ExistingJobControlPolicy) SupportsIdentity(identity TypedIdentity) bool {
	descriptor, ok := DescribeStoppedAddDriver(policy.Driver)
	return ok && descriptor.SupportsIdentity(identity)
}

type ExistingJobMutationRequest struct {
	// JobKey is a process-local opaque locator obtained from a complete ledger
	// in the same authenticated session. It is never serialized as identity.
	JobKey string `json:"-"`
}

type ExistingJobMutationReceipt struct {
	Effect            string    `json:"effect"`
	ObservedAtStart   time.Time `json:"observed_at_start"`
	ObservedAtEnd     time.Time `json:"observed_at_end"`
	Complete          bool      `json:"complete"`
	RequestsAttempted int       `json:"requests_attempted"`
	AutomaticRetries  int       `json:"automatic_retries"`
	RedirectsFollowed int       `json:"redirects_followed"`
	RequestBytes      int64     `json:"request_bytes"`
	RequestBytesKnown bool      `json:"request_bytes_known"`
	RequestID         int64     `json:"request_id,omitempty"`
	StopReason        string    `json:"stop_reason,omitempty"`
}

// ExistingJobMutationSession performs only the two transitions used by the
// explicit activation workflow. A caller must first obtain the descriptor, then
// select one unique typed job from this same session. Implementations must not
// retry, redirect, fan out, or accept a caller-selected route.
type ExistingJobMutationSession interface {
	LedgerSession
	ReadExistingJobControlDescriptor(context.Context) (ExistingJobControlDescriptor, error)
	Recheck(context.Context, ExistingJobMutationRequest) (ExistingJobMutationReceipt, error)
	Start(context.Context, ExistingJobMutationRequest) (ExistingJobMutationReceipt, error)
}

// ExistingJobControlDriver is the deliberately narrow built-in port used by
// client activation. It cannot add, stop, move, remove, or delete a job.
type ExistingJobControlDriver interface {
	LedgerDriver
	OpenExistingJobMutationSession(context.Context, Credential) (ExistingJobMutationSession, error)
	ClientConfigID(string) (string, error)
}

// MarshalExistingJobMutationRequest returns the exact bounded wire body for a
// reviewed built-in descriptor. It centralizes request-size validation between
// adapters and the activation receipt verifier.
func MarshalExistingJobMutationRequest(descriptor ExistingJobControlDescriptor, effect, jobKey string, requestID int64) ([]byte, error) {
	if descriptor.Validate() != nil || len(jobKey) == 0 || len(jobKey) > 256 {
		return nil, fmt.Errorf("downloader existing-job request is invalid")
	}
	for index := range jobKey {
		if jobKey[index] < 0x21 || jobKey[index] > 0x7e {
			return nil, fmt.Errorf("downloader existing-job request is invalid")
		}
	}
	if effect != ControlEffectRecheck && effect != ControlEffectStart {
		return nil, fmt.Errorf("downloader existing-job action is invalid")
	}
	if descriptor.Driver == DriverQBittorrent {
		if requestID != 0 {
			return nil, fmt.Errorf("downloader existing-job request ID is invalid")
		}
		return []byte(url.Values{"hashes": {jobKey}}.Encode()), nil
	}
	if !canonicalHex(jobKey, 40) || requestID <= 0 {
		return nil, fmt.Errorf("Transmission existing-job selector is invalid")
	}
	method := "torrent-verify"
	if effect == ControlEffectStart {
		method = "torrent-start"
	}
	arguments := map[string]any{"ids": []string{jobKey}}
	if descriptor.Protocol == ControlProtocolTransmissionV6 {
		method = strings.ReplaceAll(method, "-", "_")
		return json.Marshal(struct {
			JSONRPC string         `json:"jsonrpc"`
			Method  string         `json:"method"`
			Params  map[string]any `json:"params"`
			ID      int64          `json:"id"`
		}{JSONRPC: "2.0", Method: method, Params: arguments, ID: requestID})
	}
	return json.Marshal(struct {
		Method    string         `json:"method"`
		Arguments map[string]any `json:"arguments"`
		Tag       int64          `json:"tag"`
	}{Method: method, Arguments: arguments, Tag: requestID})
}

type TypedIdentity struct {
	InfoHashV1 string `json:"info_hash_v1,omitempty"`
	InfoHashV2 string `json:"info_hash_v2,omitempty"`
}

func (identity TypedIdentity) Validate() error {
	if identity.InfoHashV1 == "" && identity.InfoHashV2 == "" {
		return fmt.Errorf("typed downloader identity is empty")
	}
	if identity.InfoHashV1 != "" && !canonicalHex(identity.InfoHashV1, 40) {
		return fmt.Errorf("typed v1 infohash is invalid")
	}
	if identity.InfoHashV2 != "" && !canonicalHex(identity.InfoHashV2, 64) {
		return fmt.Errorf("typed v2 infohash is invalid")
	}
	return nil
}

type LedgerIdentityStatus string

const (
	LedgerIdentityAbsent      LedgerIdentityStatus = "absent"
	LedgerIdentityExactUnique LedgerIdentityStatus = "exact_unique"
	LedgerIdentityAmbiguous   LedgerIdentityStatus = "ambiguous"
	LedgerIdentityConflict    LedgerIdentityStatus = "conflict"
	LedgerIdentityIncomplete  LedgerIdentityStatus = "incomplete"
)

type LedgerIdentityAssessment struct {
	Status              LedgerIdentityStatus `json:"status"`
	JobsExamined        int                  `json:"jobs_examined"`
	IdentityUnavailable int                  `json:"identity_unavailable"`
	IdentityInvalid     int                  `json:"identity_invalid"`
	ExactJob            *Torrent             `json:"-"`
	ExactJobCount       int                  `json:"exact_job_count"`
	ConflictCount       int                  `json:"conflict_count"`
	IncompleteCount     int                  `json:"incomplete_count"`
}

// AssessLedgerIdentity fail-closes absence when any queue row lacks a usable
// typed identity: an opaque or malformed row could otherwise be the target.
func AssessLedgerIdentity(snapshot LedgerSnapshot, identity TypedIdentity) (LedgerIdentityAssessment, error) {
	result := LedgerIdentityAssessment{Status: LedgerIdentityIncomplete, JobsExamined: len(snapshot.Jobs)}
	if err := identity.Validate(); err != nil {
		return result, err
	}
	if !snapshot.Complete || !snapshot.Capabilities.TypedInfoHashes || snapshot.Driver == "" || len(snapshot.Jobs) > 100_000 ||
		snapshot.ObservedAtStart.IsZero() || snapshot.ObservedAtEnd.IsZero() || snapshot.ObservedAtEnd.Before(snapshot.ObservedAtStart) {
		return result, fmt.Errorf("downloader ledger snapshot is incomplete or invalid")
	}
	seen := make(map[string]struct{}, len(snapshot.Jobs))
	exact := make([]Torrent, 0, 1)
	for _, job := range snapshot.Jobs {
		if job.Hash == "" || len(job.Hash) > 256 {
			return result, fmt.Errorf("downloader ledger contains an invalid opaque job key")
		}
		for index := range job.Hash {
			if job.Hash[index] < 0x21 || job.Hash[index] > 0x7e {
				return result, fmt.Errorf("downloader ledger contains an invalid opaque job key")
			}
		}
		if _, duplicate := seen[job.Hash]; duplicate {
			return result, fmt.Errorf("downloader ledger contains a duplicate opaque job key")
		}
		seen[job.Hash] = struct{}{}
		if job.InfoHashV1 != "" && !canonicalHex(job.InfoHashV1, 40) || job.InfoHashV2 != "" && !canonicalHex(job.InfoHashV2, 64) {
			return result, fmt.Errorf("downloader ledger contains an invalid typed infohash")
		}
		switch job.IdentityStatus {
		case IdentityStatusUnavailable:
			if job.InfoHashV1 != "" || job.InfoHashV2 != "" {
				return result, fmt.Errorf("downloader ledger identity status contradicts typed hashes")
			}
			result.IdentityUnavailable++
			continue
		case IdentityStatusInvalid:
			if job.InfoHashV1 != "" || job.InfoHashV2 != "" {
				return result, fmt.Errorf("downloader ledger identity status contradicts typed hashes")
			}
			result.IdentityInvalid++
			continue
		case IdentityStatusValid:
			if job.InfoHashV1 == "" && job.InfoHashV2 == "" {
				return result, fmt.Errorf("downloader ledger valid identity is empty")
			}
		default:
			return result, fmt.Errorf("downloader ledger identity status is unknown")
		}
		switch classifyTypedJob(identity, job) {
		case "exact":
			exact = append(exact, job)
		case "conflict":
			result.ConflictCount++
		case "incomplete":
			result.IncompleteCount++
		}
	}
	sort.Slice(exact, func(i, j int) bool { return exact[i].Hash < exact[j].Hash })
	result.ExactJobCount = len(exact)
	switch {
	case len(exact) >= 2:
		result.Status = LedgerIdentityAmbiguous
	case result.ConflictCount > 0:
		result.Status = LedgerIdentityConflict
	case result.IncompleteCount > 0 || result.IdentityUnavailable > 0 || result.IdentityInvalid > 0:
		result.Status = LedgerIdentityIncomplete
	case len(exact) == 1:
		result.Status = LedgerIdentityExactUnique
		job := exact[0]
		result.ExactJob = &job
	default:
		result.Status = LedgerIdentityAbsent
	}
	return result, nil
}

// ValidateLedgerDriverClaims checks the code-owned identity semantics of an
// audited built-in adapter. It is intentionally separate from the generic
// typed-identity matcher so tests and internal algorithms can still exercise
// the matcher with synthetic driver names without granting those names client
// adoption authority.
func ValidateLedgerDriverClaims(snapshot LedgerSnapshot) error {
	if _, recognized := DescribeLedgerDriver(snapshot.Driver); !recognized {
		return fmt.Errorf("downloader ledger driver is unsupported")
	}
	for _, job := range snapshot.Jobs {
		if !validDriverIdentityEvidence(snapshot.Driver, job) {
			return fmt.Errorf("downloader ledger identity evidence is invalid")
		}
	}
	return nil
}

func validDriverIdentityEvidence(driver string, job Torrent) bool {
	if len(job.IdentityEvidence) > 8 || len(job.IdentityIssues) > 8 {
		return false
	}
	for _, values := range [][]string{job.IdentityEvidence, job.IdentityIssues} {
		for _, value := range values {
			if value == "" || len(value) > 128 {
				return false
			}
		}
	}
	if job.IdentityStatus != IdentityStatusValid {
		return true
	}
	switch driver {
	case DriverQBittorrent:
		var v1, v2 bool
		for _, evidence := range job.IdentityEvidence {
			switch evidence {
			case "magnet_xt_btih_hex", "magnet_xt_btih_base32":
				v1 = true
			case "magnet_xt_btmh_sha256":
				v2 = true
			default:
				return false
			}
		}
		return (job.InfoHashV1 == "" || v1) && (job.InfoHashV2 == "" || v2)
	case DriverTransmission:
		return job.InfoHashV1 != "" && job.InfoHashV2 == "" && len(job.IdentityEvidence) == 1 &&
			job.IdentityEvidence[0] == "transmission_hash_string_sha1" && len(job.IdentityIssues) == 0
	default:
		return false
	}
}

func classifyTypedJob(identity TypedIdentity, job Torrent) string {
	hasV1, hasV2 := identity.InfoHashV1 != "", identity.InfoHashV2 != ""
	v1Match := hasV1 && job.InfoHashV1 == identity.InfoHashV1
	v2Match := hasV2 && job.InfoHashV2 == identity.InfoHashV2
	switch {
	case hasV1 && !hasV2:
		if !v1Match {
			return "unrelated"
		}
		if job.InfoHashV2 != "" {
			return "conflict"
		}
		return "exact"
	case hasV2 && !hasV1:
		if !v2Match {
			return "unrelated"
		}
		if job.InfoHashV1 != "" {
			return "conflict"
		}
		return "exact"
	case hasV1 && hasV2:
		if v1Match && v2Match {
			return "exact"
		}
		if v1Match || v2Match {
			if job.InfoHashV1 == "" || job.InfoHashV2 == "" {
				return "incomplete"
			}
			return "conflict"
		}
	}
	return "unrelated"
}

func canonicalHex(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded)*2 == length
}
