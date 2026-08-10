package downloader

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
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

const (
	ControlProtocolQBittorrentV4 = "qbittorrent_webapi_v4"
	ControlProtocolQBittorrentV5 = "qbittorrent_webapi_v5"

	ControlEffectRecheck = "request_existing_job_recheck"
	ControlEffectStart   = "request_existing_job_start"
)

// ExistingJobControlDescriptor is a bounded, normalized capability result for
// mutations of an already identified downloader job. Protocol is deliberately
// explicit because qBittorrent 4.x and 5.x use different start routes.
type ExistingJobControlDescriptor struct {
	Driver         string `json:"driver"`
	Protocol       string `json:"protocol"`
	RecheckRouteID string `json:"recheck_route_id"`
	StartRouteID   string `json:"start_route_id"`
}

func (descriptor ExistingJobControlDescriptor) Validate() error {
	if descriptor.Driver != "qbittorrent" || descriptor.RecheckRouteID != "qbittorrent.torrents.recheck.v1" {
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
	return nil
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
