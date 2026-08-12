package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

const (
	StopEffect = "request_existing_job_stop"

	stopRouteQBittorrentV4 = "qbittorrent.torrents.pause.v1"
	stopRouteQBittorrentV5 = "qbittorrent.torrents.stop.v1"
	stopRouteTransmission  = "transmission.torrent.stop.v1"
)

// ExistingJobStopDescriptor is a normalized, code-owned capability for
// stopping exactly one already identified downloader job. It is deliberately
// separate from recheck/start and removal so a stop workflow cannot acquire
// broader mutation authority through the same interface.
type ExistingJobStopDescriptor struct {
	Driver      string `json:"driver"`
	Protocol    string `json:"protocol"`
	StopRouteID string `json:"stop_route_id"`
}

func (descriptor ExistingJobStopDescriptor) Validate() error {
	switch descriptor.Driver {
	case DriverQBittorrent:
		switch descriptor.Protocol {
		case ControlProtocolQBittorrentV4:
			if descriptor.StopRouteID != stopRouteQBittorrentV4 {
				return fmt.Errorf("downloader existing-job stop descriptor is invalid")
			}
		case ControlProtocolQBittorrentV5:
			if descriptor.StopRouteID != stopRouteQBittorrentV5 {
				return fmt.Errorf("downloader existing-job stop descriptor is invalid")
			}
		default:
			return fmt.Errorf("downloader existing-job stop descriptor is invalid")
		}
	case DriverTransmission:
		if descriptor.StopRouteID != stopRouteTransmission ||
			descriptor.Protocol != ControlProtocolTransmissionV5 && descriptor.Protocol != ControlProtocolTransmissionV6 {
			return fmt.Errorf("downloader existing-job stop descriptor is invalid")
		}
	default:
		return fmt.Errorf("downloader existing-job stop descriptor is invalid")
	}
	return nil
}

// ExistingJobStopPolicy is the built-in request and typed-identity policy
// checked before opening an effectful stop session.
type ExistingJobStopPolicy struct {
	Driver             string
	OpenRequests       int
	DescriptorRequests int
}

func DescribeExistingJobStopDriver(driver string) (ExistingJobStopPolicy, bool) {
	switch driver {
	case DriverQBittorrent:
		return ExistingJobStopPolicy{Driver: driver, OpenRequests: 1, DescriptorRequests: 1}, true
	case DriverTransmission:
		return ExistingJobStopPolicy{Driver: driver, OpenRequests: 2, DescriptorRequests: 0}, true
	default:
		return ExistingJobStopPolicy{}, false
	}
}

func (policy ExistingJobStopPolicy) SupportsIdentity(identity TypedIdentity) bool {
	if identity.Validate() != nil {
		return false
	}
	switch policy.Driver {
	case DriverQBittorrent:
		return identity.InfoHashV1 != "" || identity.InfoHashV2 != ""
	case DriverTransmission:
		return identity.InfoHashV1 != "" && identity.InfoHashV2 == ""
	default:
		return false
	}
}

// ExistingJobStopSession has only the authority needed to stop one unique job
// selected from a complete ledger in this same authenticated session.
type ExistingJobStopSession interface {
	LedgerSession
	ReadExistingJobStopDescriptor(context.Context) (ExistingJobStopDescriptor, error)
	Stop(context.Context, ExistingJobMutationRequest) (ExistingJobMutationReceipt, error)
}

// ExistingJobStopDriver cannot add, recheck, start, relocate, remove, or
// delete data. Implementations must not retry, redirect, or fan out the one
// effectful request.
type ExistingJobStopDriver interface {
	LedgerDriver
	OpenExistingJobStopSession(context.Context, Credential) (ExistingJobStopSession, error)
	ClientConfigID(string) (string, error)
}

// MarshalExistingJobStopRequest returns the exact one-job wire body for one
// audited built-in stop descriptor.
func MarshalExistingJobStopRequest(descriptor ExistingJobStopDescriptor, jobKey string, requestID int64) ([]byte, error) {
	if descriptor.Validate() != nil || len(jobKey) == 0 || len(jobKey) > 256 {
		return nil, fmt.Errorf("downloader existing-job stop request is invalid")
	}
	for index := range jobKey {
		if jobKey[index] < 0x21 || jobKey[index] > 0x7e {
			return nil, fmt.Errorf("downloader existing-job stop request is invalid")
		}
	}
	if descriptor.Driver == DriverQBittorrent {
		if requestID != 0 || jobKey == "all" || strings.ContainsRune(jobKey, '|') {
			return nil, fmt.Errorf("downloader existing-job stop selector is invalid")
		}
		return []byte(url.Values{"hashes": {jobKey}}.Encode()), nil
	}
	if !canonicalHex(jobKey, 40) || requestID <= 0 {
		return nil, fmt.Errorf("Transmission existing-job stop selector is invalid")
	}
	if descriptor.Protocol == ControlProtocolTransmissionV6 {
		return json.Marshal(struct {
			JSONRPC string `json:"jsonrpc"`
			Method  string `json:"method"`
			Params  struct {
				IDs []string `json:"ids"`
			} `json:"params"`
			ID int64 `json:"id"`
		}{
			JSONRPC: "2.0", Method: "torrent_stop",
			Params: struct {
				IDs []string `json:"ids"`
			}{IDs: []string{jobKey}},
			ID: requestID,
		})
	}
	return json.Marshal(struct {
		Method    string `json:"method"`
		Arguments struct {
			IDs []string `json:"ids"`
		} `json:"arguments"`
		Tag int64 `json:"tag"`
	}{
		Method: "torrent-stop",
		Arguments: struct {
			IDs []string `json:"ids"`
		}{IDs: []string{jobKey}},
		Tag: requestID,
	})
}
