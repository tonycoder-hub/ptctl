package storageindex

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/metastore"
)

func TestRepositoryCreatesIdempotentImmutableProfileAndSelectsAlias(t *testing.T) {
	repository := testRepository(t)
	ctx := context.Background()
	root := t.TempDir()
	first, err := repository.CreateProfile(ctx, "media", []string{root}, false, DefaultScanLimits(), time.Date(2026, 8, 10, 1, 0, 0, 0, time.UTC))
	if err != nil || first.WritesPerformed != 1 || first.AlreadyPresent || first.RecordID == "" {
		t.Fatalf("first profile import failed: receipt=%#v err=%v", first, err)
	}
	second, err := repository.CreateProfile(ctx, "media", []string{root}, false, DefaultScanLimits(), time.Date(2026, 8, 10, 2, 0, 0, 0, time.UTC))
	if err != nil || second.WritesPerformed != 0 || !second.AlreadyPresent || second.RecordID != first.RecordID {
		t.Fatalf("idempotent profile import changed state: receipt=%#v err=%v", second, err)
	}
	selected, err := repository.SelectProfile(ctx, "media")
	if err != nil || !selected.Complete || selected.Status != "selected" || selected.Profile.ID != first.Profile.ID {
		t.Fatalf("profile selection failed: selection=%#v err=%v", selected, err)
	}
	byID, err := repository.SelectProfile(ctx, first.Profile.ID)
	if err != nil || byID.Profile.ID != first.Profile.ID {
		t.Fatalf("profile ID selection failed: selection=%#v err=%v", byID, err)
	}
}

func TestRepositoryRejectsSameNameWithDifferentDeclaration(t *testing.T) {
	repository := testRepository(t)
	ctx := context.Background()
	if _, err := repository.CreateProfile(ctx, "media", []string{t.TempDir()}, false, DefaultScanLimits(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateProfile(ctx, "media", []string{t.TempDir()}, false, DefaultScanLimits(), time.Now()); !errors.Is(err, ErrProfileConflict) {
		t.Fatalf("immutable profile name conflict was not rejected: %v", err)
	}
}

func TestRepositorySelectsLatestGenerationAndRejectsTie(t *testing.T) {
	repository := testRepository(t)
	ctx := context.Background()
	profileReceipt, err := repository.CreateProfile(ctx, "media", []string{t.TempDir()}, false, DefaultScanLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	profile := profileReceipt.Profile
	first := importTestDescriptor(t, repository, profile, 1, "a")
	second := importTestDescriptor(t, repository, profile, 2, "b")
	selected, err := repository.SelectSnapshot(ctx, profile, "")
	if err != nil || selected.Status != "selected" || selected.DescriptorRecordID != second || selected.Descriptor.Generation != 2 {
		t.Fatalf("latest snapshot selection failed: selection=%#v err=%v", selected, err)
	}
	explicit, err := repository.SelectSnapshot(ctx, profile, first)
	if err != nil || explicit.Descriptor.Generation != 1 {
		t.Fatalf("explicit descriptor selection failed: selection=%#v err=%v", explicit, err)
	}
	_ = importTestDescriptor(t, repository, profile, 2, "c")
	ambiguous, err := repository.SelectSnapshot(ctx, profile, "")
	if !errors.Is(err, ErrSnapshotAmbiguous) || ambiguous.Status != "ambiguous" || !ambiguous.Complete {
		t.Fatalf("generation tie was not reported as ambiguous: selection=%#v err=%v", ambiguous, err)
	}
}

func TestProfileValidationRecomputesDeclarationIdentity(t *testing.T) {
	profile := testProfile(t)
	profile.ID = "profile:" + strings.Repeat("a", 64)
	if err := profile.Validate(); err == nil {
		t.Fatal("profile with forged declaration identity was accepted")
	}
}

func testRepository(t *testing.T) *Repository {
	t.Helper()
	store, _, err := metastore.Init(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func importTestDescriptor(t *testing.T, repository *Repository, profile Profile, generation uint64, salt string) metastore.RecordID {
	t.Helper()
	dataRef, _, err := repository.store.ImportRecord(context.Background(), metastore.RecordKindStorageIndexDataV1, bytes.NewBufferString("data-"+salt+"\n"), repository.recordLimits(repository.limits.MaxSnapshots))
	if err != nil {
		t.Fatal(err)
	}
	header, err := NewSnapshotHeader(profile, generation, time.Date(2026, 8, 10, int(generation), 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	descriptor := SnapshotDescriptor{
		Format: SnapshotDescriptorFormat, ID: header.SnapshotID, Generation: generation,
		ProfileID: profile.ID, ProfileRevision: profile.Revision, Platform: profile.Platform, PathEncoding: profile.PathEncoding,
		DataRecordID: dataRef.ID.String(), ObservedAtStart: header.ObservedAtStart, ObservedAtEnd: header.ObservedAtStart.Add(time.Second),
		Files: 0, PathBytes: 0, EnumerationScope: "complete_snapshot", LiveFreshness: "unproven_since_snapshot",
		Roots: []SnapshotRootObservation{{RootID: profile.Roots[0].ID, Status: "complete", FilesystemIdentityHint: "fs-" + salt, RootIdentityHint: "root-" + salt}},
	}
	raw, err := EncodeDescriptor(descriptor, repository.limits)
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err := repository.store.ImportRecord(context.Background(), metastore.RecordKindStorageIndexDescriptorV1, bytes.NewReader(raw), repository.recordLimits(repository.limits.MaxSnapshots))
	if err != nil {
		t.Fatal(err)
	}
	return ref.ID
}
