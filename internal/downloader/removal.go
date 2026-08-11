package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

const (
	RemoveEffectKeepData = "request_existing_job_remove_keep_data"

	removeRouteQBittorrent  = "qbittorrent.torrents.delete_keep_data.v1"
	removeRouteTransmission = "transmission.torrent.remove_keep_data.v1"
)

// ExistingJobRemovalDescriptor is a normalized, code-owned capability for
// removing exactly one already identified downloader job while explicitly
// retaining its local data. There is deliberately no caller-controlled data
// deletion option in this port.
type ExistingJobRemovalDescriptor struct {
	Driver        string `json:"driver"`
	Protocol      string `json:"protocol"`
	RemoveRouteID string `json:"remove_route_id"`
}

func (descriptor ExistingJobRemovalDescriptor) Validate() error {
	switch descriptor.Driver {
	case DriverQBittorrent:
		if descriptor.RemoveRouteID != removeRouteQBittorrent ||
			descriptor.Protocol != ControlProtocolQBittorrentV4 && descriptor.Protocol != ControlProtocolQBittorrentV5 {
			return fmt.Errorf("downloader existing-job removal descriptor is invalid")
		}
	case DriverTransmission:
		if descriptor.RemoveRouteID != removeRouteTransmission ||
			descriptor.Protocol != ControlProtocolTransmissionV5 && descriptor.Protocol != ControlProtocolTransmissionV6 {
			return fmt.Errorf("downloader existing-job removal descriptor is invalid")
		}
	default:
		return fmt.Errorf("downloader existing-job removal descriptor is invalid")
	}
	return nil
}

// ExistingJobRemovalPolicy is the built-in request and identity policy used
// before opening an effectful removal session.
type ExistingJobRemovalPolicy struct {
	Driver             string
	OpenRequests       int
	DescriptorRequests int
}

func DescribeExistingJobRemovalDriver(driver string) (ExistingJobRemovalPolicy, bool) {
	switch driver {
	case DriverQBittorrent:
		return ExistingJobRemovalPolicy{Driver: driver, OpenRequests: 1, DescriptorRequests: 1}, true
	case DriverTransmission:
		return ExistingJobRemovalPolicy{Driver: driver, OpenRequests: 2, DescriptorRequests: 0}, true
	default:
		return ExistingJobRemovalPolicy{}, false
	}
}

func (policy ExistingJobRemovalPolicy) SupportsIdentity(identity TypedIdentity) bool {
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

// ExistingJobRemovalSession has only the authority needed by the explicit
// remove-keep-data workflow. The unique JobKey must come from a complete ledger
// read in this same authenticated session.
type ExistingJobRemovalSession interface {
	LedgerSession
	ReadExistingJobRemovalDescriptor(context.Context) (ExistingJobRemovalDescriptor, error)
	RemoveKeepData(context.Context, ExistingJobMutationRequest) (ExistingJobMutationReceipt, error)
}

// ExistingJobRemovalDriver cannot add, start, recheck, move, or delete local
// data. Implementations must not retry, redirect, or fan out the one mutation.
type ExistingJobRemovalDriver interface {
	LedgerDriver
	OpenExistingJobRemovalSession(context.Context, Credential) (ExistingJobRemovalSession, error)
	ClientConfigID(string) (string, error)
}

// MarshalExistingJobRemovalRequest returns the exact one-job, keep-data wire
// body for an audited built-in descriptor.
func MarshalExistingJobRemovalRequest(descriptor ExistingJobRemovalDescriptor, jobKey string, requestID int64) ([]byte, error) {
	if descriptor.Validate() != nil || len(jobKey) == 0 || len(jobKey) > 256 {
		return nil, fmt.Errorf("downloader existing-job removal request is invalid")
	}
	for index := range jobKey {
		if jobKey[index] < 0x21 || jobKey[index] > 0x7e {
			return nil, fmt.Errorf("downloader existing-job removal request is invalid")
		}
	}
	if descriptor.Driver == DriverQBittorrent {
		if requestID != 0 || jobKey == "all" || strings.ContainsRune(jobKey, '|') {
			return nil, fmt.Errorf("downloader existing-job removal request ID is invalid")
		}
		return []byte(url.Values{"deleteFiles": {"false"}, "hashes": {jobKey}}.Encode()), nil
	}
	if !canonicalHex(jobKey, 40) || requestID <= 0 {
		return nil, fmt.Errorf("Transmission existing-job removal selector is invalid")
	}
	if descriptor.Protocol == ControlProtocolTransmissionV6 {
		return json.Marshal(struct {
			JSONRPC string `json:"jsonrpc"`
			Method  string `json:"method"`
			Params  struct {
				IDs             []string `json:"ids"`
				DeleteLocalData bool     `json:"delete_local_data"`
			} `json:"params"`
			ID int64 `json:"id"`
		}{
			JSONRPC: "2.0", Method: "torrent_remove",
			Params: struct {
				IDs             []string `json:"ids"`
				DeleteLocalData bool     `json:"delete_local_data"`
			}{IDs: []string{jobKey}, DeleteLocalData: false},
			ID: requestID,
		})
	}
	return json.Marshal(struct {
		Method    string `json:"method"`
		Arguments struct {
			IDs             []string `json:"ids"`
			DeleteLocalData bool     `json:"delete-local-data"`
		} `json:"arguments"`
		Tag int64 `json:"tag"`
	}{
		Method: "torrent-remove",
		Arguments: struct {
			IDs             []string `json:"ids"`
			DeleteLocalData bool     `json:"delete-local-data"`
		}{IDs: []string{jobKey}, DeleteLocalData: false},
		Tag: requestID,
	})
}
