package storageindex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestProfileCanonicalRoundTripAndStableDeclarationIdentity(t *testing.T) {
	now := time.Date(2026, 8, 10, 1, 2, 3, 4, time.UTC)
	rootA := physicalIndexTempDir(t)
	rootB := physicalIndexTempDir(t)
	first, err := NewProfile("media", []string{rootB, rootA}, false, DefaultScanLimits(), now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewProfile("media", []string{rootA, rootB}, false, DefaultScanLimits(), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.Revision != second.Revision {
		t.Fatalf("equivalent declarations have unstable identity: %#v %#v", first, second)
	}
	alias, err := NewProfile("archive", []string{rootA, rootB}, false, DefaultScanLimits(), now)
	if err != nil {
		t.Fatal(err)
	}
	if alias.ID != first.ID || alias.Revision != first.Revision {
		t.Fatal("display name unexpectedly became profile authority")
	}
	if first.CreatedAt.Equal(second.CreatedAt) {
		t.Fatal("observation time unexpectedly became declaration identity")
	}
	raw, err := EncodeProfile(first)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeProfile(bytes.NewReader(raw))
	if err != nil || decoded.ID != first.ID || decoded.Revision != first.Revision || len(decoded.Roots) != 2 {
		t.Fatalf("profile round trip failed: decoded=%#v err=%v", decoded, err)
	}
	for _, root := range decoded.Roots {
		path, pathErr := root.Path()
		if pathErr != nil || (path != rootA && path != rootB) {
			t.Fatalf("profile root authority changed: path=%q err=%v", path, pathErr)
		}
	}
}

func TestProfileRejectsNoncanonicalAndDuplicateJSON(t *testing.T) {
	profile := testProfile(t)
	raw, err := EncodeProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	noncanonical := append([]byte(" "), raw...)
	if _, err := DecodeProfile(bytes.NewReader(noncanonical)); err == nil {
		t.Fatal("noncanonical profile JSON was accepted")
	}
	duplicate := bytes.Replace(raw, []byte(`"format":"`), []byte(`"format":"ptctl.storage-profile/v1","format":"`), 1)
	if _, err := DecodeProfile(bytes.NewReader(duplicate)); err == nil {
		t.Fatal("duplicate profile field was accepted")
	}
	unknown := bytes.Replace(raw, []byte(`{"format"`), []byte(`{"unknown":1,"format"`), 1)
	if _, err := DecodeProfile(bytes.NewReader(unknown)); err == nil {
		t.Fatal("unknown profile field was accepted")
	}
}

func TestSnapshotStreamingRoundTrip(t *testing.T) {
	profile := testProfile(t)
	start := time.Date(2026, 8, 10, 2, 0, 0, 0, time.UTC)
	header, err := NewSnapshotHeader(profile, 7, start)
	if err != nil {
		t.Fatal(err)
	}
	limits := DefaultLimits()
	var output bytes.Buffer
	encoder, err := NewSnapshotEncoder(&output, header, limits)
	if err != nil {
		t.Fatal(err)
	}
	entries := []Entry{
		indexEntry(header.RootIDs[0], "alpha", 1, 10, "same-physical-hint"),
		indexEntry(header.RootIDs[0], "beta", 2, 11, "same-physical-hint"),
	}
	for _, entry := range entries {
		if err := encoder.WriteEntry(entry); err != nil {
			t.Fatal(err)
		}
	}
	footer, err := encoder.Close(start.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var visited []Entry
	decodedHeader, decodedFooter, err := DecodeSnapshot(context.Background(), bytes.NewReader(output.Bytes()), limits, func(entry Entry) error {
		visited = append(visited, entry)
		return nil
	})
	if err != nil || decodedHeader.SnapshotID != header.SnapshotID || decodedHeader.Generation != 7 || decodedFooter != footer || len(visited) != 2 {
		t.Fatalf("snapshot round trip failed: header=%#v footer=%#v visited=%#v err=%v", decodedHeader, decodedFooter, visited, err)
	}
	if visited[0].IdentityHint != visited[1].IdentityHint {
		t.Fatal("hardlink-like aliases were collapsed by the snapshot format")
	}
}

func TestSnapshotRejectsMalformedRowsAndBudgets(t *testing.T) {
	profile := testProfile(t)
	start := time.Date(2026, 8, 10, 2, 0, 0, 0, time.UTC)
	header, err := NewSnapshotHeader(profile, 1, start)
	if err != nil {
		t.Fatal(err)
	}
	headerLine, _ := json.Marshal(header)
	entry := indexEntry(header.RootIDs[0], "alpha", 1, 10, "hint")
	entryLine, _ := json.Marshal(entry)
	footer := SnapshotFooter{Type: "footer", SnapshotID: header.SnapshotID, ObservedAtEnd: start.Add(time.Second), Complete: true, Files: 1, PathBytes: 5}
	footerLine, _ := json.Marshal(footer)
	valid := bytes.Join([][]byte{headerLine, entryLine, footerLine, nil}, []byte{'\n'})
	tests := []struct {
		name string
		raw  []byte
	}{
		{"missing final newline", bytes.TrimSuffix(valid, []byte{'\n'})},
		{"duplicate field", bytes.Replace(valid, []byte(`"size_bytes":1`), []byte(`"size_bytes":1,"size_bytes":1`), 1)},
		{"unknown field", bytes.Replace(valid, []byte(`"type":"file"`), []byte(`"type":"file","unknown":1`), 1)},
		{"invalid base64", bytes.Replace(valid, []byte(base64.StdEncoding.EncodeToString([]byte("alpha"))), []byte("***"), 1)},
		{"footer mismatch", bytes.Replace(valid, []byte(`"files":1`), []byte(`"files":2`), 1)},
		{"trailing row", append(append([]byte(nil), valid...), []byte("{}\n")...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := DecodeSnapshot(context.Background(), bytes.NewReader(test.raw), DefaultLimits(), func(Entry) error { return nil }); err == nil {
				t.Fatal("malformed snapshot was accepted")
			}
		})
	}

	limited := DefaultLimits()
	limited.MaxFiles = 1
	var output bytes.Buffer
	encoder, err := NewSnapshotEncoder(&output, header, limited)
	if err != nil {
		t.Fatal(err)
	}
	if err := encoder.WriteEntry(indexEntry(header.RootIDs[0], "a", 1, 1, "")); err != nil {
		t.Fatal(err)
	}
	if err := encoder.WriteEntry(indexEntry(header.RootIDs[0], "b", 1, 1, "")); err == nil {
		t.Fatal("N+1 file row was accepted")
	}
}

func TestSnapshotRejectsUnsortedAndVisitorFailure(t *testing.T) {
	profile := testProfile(t)
	header, err := NewSnapshotHeader(profile, 1, time.Date(2026, 8, 10, 2, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	encoder, _ := NewSnapshotEncoder(&output, header, DefaultLimits())
	if err := encoder.WriteEntry(indexEntry(header.RootIDs[0], "b", 1, 1, "")); err != nil {
		t.Fatal(err)
	}
	if err := encoder.WriteEntry(indexEntry(header.RootIDs[0], "a", 1, 1, "")); err == nil {
		t.Fatal("unsorted entry was accepted")
	}

	output.Reset()
	encoder, _ = NewSnapshotEncoder(&output, header, DefaultLimits())
	_ = encoder.WriteEntry(indexEntry(header.RootIDs[0], "a", 1, 1, ""))
	_, _ = encoder.Close(header.ObservedAtStart.Add(time.Second))
	wanted := fmt.Errorf("visitor stopped")
	if _, _, err := DecodeSnapshot(context.Background(), bytes.NewReader(output.Bytes()), DefaultLimits(), func(Entry) error { return wanted }); err != wanted {
		t.Fatalf("visitor failure was not preserved: %v", err)
	}
}

func TestDescriptorCanonicalRoundTrip(t *testing.T) {
	profile := testProfile(t)
	header, err := NewSnapshotHeader(profile, 3, time.Date(2026, 8, 10, 2, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	descriptor := SnapshotDescriptor{
		Format: SnapshotDescriptorFormat, ID: header.SnapshotID, Generation: header.Generation,
		ProfileID: profile.ID, ProfileRevision: profile.Revision, Platform: profile.Platform, PathEncoding: profile.PathEncoding,
		DataRecordID: "sha256:" + strings.Repeat("a", 64), ObservedAtStart: header.ObservedAtStart,
		ObservedAtEnd: header.ObservedAtStart.Add(time.Second), Files: 2, PathBytes: 9,
		EnumerationScope: "complete_snapshot", LiveFreshness: "unproven_since_snapshot",
		Roots: []SnapshotRootObservation{{
			RootID: profile.Roots[0].ID, Status: "complete",
			FilesystemIdentityHint: "filesystem-hint", RootIdentityHint: "root-hint",
		}},
	}
	raw, err := EncodeDescriptor(descriptor, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeDescriptor(bytes.NewReader(raw), DefaultLimits())
	if err != nil || !reflect.DeepEqual(decoded, descriptor) {
		t.Fatalf("descriptor round trip failed: %#v err=%v", decoded, err)
	}
}

func TestSnapshotRejectsMissingZeroValuedFields(t *testing.T) {
	profile := testProfile(t)
	header, err := NewSnapshotHeader(profile, 1, time.Date(2026, 8, 10, 2, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	headerLine, _ := json.Marshal(header)
	entry := indexEntry(header.RootIDs[0], "zero", 0, 0, "")
	entryLine, _ := json.Marshal(entry)
	entryLine = bytes.Replace(entryLine, []byte(`,"size_bytes":0`), nil, 1)
	footer := SnapshotFooter{Type: "footer", SnapshotID: header.SnapshotID, ObservedAtEnd: header.ObservedAtStart.Add(time.Second), Complete: true, Files: 1, PathBytes: 4}
	footerLine, _ := json.Marshal(footer)
	raw := bytes.Join([][]byte{headerLine, entryLine, footerLine, nil}, []byte{'\n'})
	if _, _, err := DecodeSnapshot(context.Background(), bytes.NewReader(raw), DefaultLimits(), func(Entry) error { return nil }); err == nil {
		t.Fatal("missing zero-valued required field was accepted")
	}
}

func testProfile(t *testing.T) Profile {
	t.Helper()
	profile, err := NewProfile("media", []string{physicalIndexTempDir(t)}, false, DefaultScanLimits(), time.Date(2026, 8, 10, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func indexEntry(rootID, name string, size, modified int64, hint string) Entry {
	return Entry{
		Type: "file", RootID: rootID,
		RelativeComponentsRawBase64: []string{base64.StdEncoding.EncodeToString([]byte(name))},
		SizeBytes:                   size, ModifiedUnixNanos: modified, IdentityHint: hint,
	}
}
