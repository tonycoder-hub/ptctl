package downloader

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestMarshalExistingJobStopRequestSelectsExactlyOneJob(t *testing.T) {
	const job = "1111111111111111111111111111111111111111"
	tests := []struct {
		name       string
		descriptor ExistingJobStopDescriptor
		requestID  int64
	}{
		{name: "qbittorrent v4", descriptor: ExistingJobStopDescriptor{Driver: DriverQBittorrent, Protocol: ControlProtocolQBittorrentV4, StopRouteID: stopRouteQBittorrentV4}},
		{name: "qbittorrent v5", descriptor: ExistingJobStopDescriptor{Driver: DriverQBittorrent, Protocol: ControlProtocolQBittorrentV5, StopRouteID: stopRouteQBittorrentV5}},
		{name: "transmission legacy", descriptor: ExistingJobStopDescriptor{Driver: DriverTransmission, Protocol: ControlProtocolTransmissionV5, StopRouteID: stopRouteTransmission}, requestID: 7},
		{name: "transmission json rpc", descriptor: ExistingJobStopDescriptor{Driver: DriverTransmission, Protocol: ControlProtocolTransmissionV6, StopRouteID: stopRouteTransmission}, requestID: 8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := MarshalExistingJobStopRequest(test.descriptor, job, test.requestID)
			if err != nil {
				t.Fatal(err)
			}
			if test.descriptor.Driver == DriverQBittorrent {
				values, err := url.ParseQuery(string(body))
				if err != nil || len(values) != 1 || values.Get("hashes") != job || len(values["hashes"]) != 1 {
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
				if value["method"] != "torrent-stop" || !containsAll(encoded, `"ids":["`+job+`"]`) {
					t.Fatalf("unexpected legacy request: %s", body)
				}
			} else if value["method"] != "torrent_stop" || !containsAll(encoded, `"ids":["`+job+`"]`) {
				t.Fatalf("unexpected JSON-RPC request: %s", body)
			}
		})
	}
}

func TestMarshalExistingJobStopRequestRejectsInvalidOrBroadSelectors(t *testing.T) {
	descriptor := ExistingJobStopDescriptor{Driver: DriverQBittorrent, Protocol: ControlProtocolQBittorrentV5, StopRouteID: stopRouteQBittorrentV5}
	for _, job := range []string{"", "all", "a|b", "a b", "a\x00b"} {
		if _, err := MarshalExistingJobStopRequest(descriptor, job, 0); err == nil {
			t.Fatalf("accepted job selector %q", job)
		}
	}
	if _, err := MarshalExistingJobStopRequest(descriptor, "opaque", 1); err == nil {
		t.Fatal("accepted qBittorrent request ID")
	}
	transmission := ExistingJobStopDescriptor{Driver: DriverTransmission, Protocol: ControlProtocolTransmissionV6, StopRouteID: stopRouteTransmission}
	if _, err := MarshalExistingJobStopRequest(transmission, "opaque", 1); err == nil {
		t.Fatal("accepted non-hash Transmission selector")
	}
}

func TestExistingJobStopIdentityPolicyIsDriverSpecific(t *testing.T) {
	v1 := TypedIdentity{InfoHashV1: strings.Repeat("1", 40)}
	v2 := TypedIdentity{InfoHashV2: strings.Repeat("2", 64)}
	hybrid := TypedIdentity{InfoHashV1: v1.InfoHashV1, InfoHashV2: v2.InfoHashV2}
	qbit, ok := DescribeExistingJobStopDriver(DriverQBittorrent)
	if !ok || !qbit.SupportsIdentity(v1) || !qbit.SupportsIdentity(v2) || !qbit.SupportsIdentity(hybrid) {
		t.Fatal("qBittorrent stop policy rejected a typed identity")
	}
	transmission, ok := DescribeExistingJobStopDriver(DriverTransmission)
	if !ok || !transmission.SupportsIdentity(v1) || transmission.SupportsIdentity(v2) || transmission.SupportsIdentity(hybrid) {
		t.Fatal("Transmission stop policy did not remain v1-only")
	}
}
