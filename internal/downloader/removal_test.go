package downloader

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestMarshalExistingJobRemovalRequestKeepsDataAndSelectsOneJob(t *testing.T) {
	const job = "1111111111111111111111111111111111111111"
	tests := []struct {
		name       string
		descriptor ExistingJobRemovalDescriptor
		requestID  int64
	}{
		{name: "qbittorrent v4", descriptor: ExistingJobRemovalDescriptor{Driver: DriverQBittorrent, Protocol: ControlProtocolQBittorrentV4, RemoveRouteID: removeRouteQBittorrent}},
		{name: "qbittorrent v5", descriptor: ExistingJobRemovalDescriptor{Driver: DriverQBittorrent, Protocol: ControlProtocolQBittorrentV5, RemoveRouteID: removeRouteQBittorrent}},
		{name: "transmission legacy", descriptor: ExistingJobRemovalDescriptor{Driver: DriverTransmission, Protocol: ControlProtocolTransmissionV5, RemoveRouteID: removeRouteTransmission}, requestID: 7},
		{name: "transmission json rpc", descriptor: ExistingJobRemovalDescriptor{Driver: DriverTransmission, Protocol: ControlProtocolTransmissionV6, RemoveRouteID: removeRouteTransmission}, requestID: 8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := MarshalExistingJobRemovalRequest(test.descriptor, job, test.requestID)
			if err != nil {
				t.Fatal(err)
			}
			if test.descriptor.Driver == DriverQBittorrent {
				values, err := url.ParseQuery(string(body))
				if err != nil || len(values) != 2 || values.Get("hashes") != job || values.Get("deleteFiles") != "false" || len(values["hashes"]) != 1 {
					t.Fatalf("body=%q values=%v err=%v", body, values, err)
				}
				return
			}
			var value map[string]any
			if err := json.Unmarshal(body, &value); err != nil {
				t.Fatal(err)
			}
			encoded := string(body)
			if test.descriptor.Protocol == ControlProtocolTransmissionV5 {
				if value["method"] != "torrent-remove" || !containsAll(encoded, `"delete-local-data":false`, `"ids":["`+job+`"]`) {
					t.Fatalf("unexpected legacy request: %s", body)
				}
			} else if value["method"] != "torrent_remove" || !containsAll(encoded, `"delete_local_data":false`, `"ids":["`+job+`"]`) {
				t.Fatalf("unexpected JSON-RPC request: %s", body)
			}
		})
	}
}

func TestMarshalExistingJobRemovalRequestRejectsInvalidOrBroadSelectors(t *testing.T) {
	descriptor := ExistingJobRemovalDescriptor{Driver: DriverQBittorrent, Protocol: ControlProtocolQBittorrentV5, RemoveRouteID: removeRouteQBittorrent}
	for _, job := range []string{"", "all", "a|b", "a b", "a\x00b"} {
		if _, err := MarshalExistingJobRemovalRequest(descriptor, job, 0); err == nil {
			t.Fatalf("accepted job selector %q", job)
		}
	}
	if _, err := MarshalExistingJobRemovalRequest(descriptor, "opaque", 1); err == nil {
		t.Fatal("accepted qBittorrent request ID")
	}
	transmission := ExistingJobRemovalDescriptor{Driver: DriverTransmission, Protocol: ControlProtocolTransmissionV6, RemoveRouteID: removeRouteTransmission}
	if _, err := MarshalExistingJobRemovalRequest(transmission, "opaque", 1); err == nil {
		t.Fatal("accepted non-hash Transmission selector")
	}
}

func TestExistingJobRemovalIdentityPolicyIsDriverSpecific(t *testing.T) {
	v1 := TypedIdentity{InfoHashV1: strings.Repeat("1", 40)}
	v2 := TypedIdentity{InfoHashV2: strings.Repeat("2", 64)}
	hybrid := TypedIdentity{InfoHashV1: v1.InfoHashV1, InfoHashV2: v2.InfoHashV2}
	qbit, ok := DescribeExistingJobRemovalDriver(DriverQBittorrent)
	if !ok || !qbit.SupportsIdentity(v1) || !qbit.SupportsIdentity(v2) || !qbit.SupportsIdentity(hybrid) {
		t.Fatal("qBittorrent removal policy rejected a typed identity")
	}
	transmission, ok := DescribeExistingJobRemovalDriver(DriverTransmission)
	if !ok || !transmission.SupportsIdentity(v1) || transmission.SupportsIdentity(v2) || transmission.SupportsIdentity(hybrid) {
		t.Fatal("Transmission removal policy did not remain v1-only")
	}
}

func containsAll(value string, substrings ...string) bool {
	for _, substring := range substrings {
		if !strings.Contains(value, substring) {
			return false
		}
	}
	return true
}
