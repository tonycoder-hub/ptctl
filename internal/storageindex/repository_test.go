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
	root := physicalIndexTempDir(t)
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
	if _, err := repository.CreateProfile(ctx, "media", []string{physicalIndexTempDir(t)}, false, DefaultScanLimits(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateProfile(ctx, "media", []string{physicalIndexTempDir(t)}, false, DefaultScanLimits(), time.Now()); !errors.Is(err, ErrProfileConflict) {
		t.Fatalf("immutable profile name conflict was not rejected: %v", err)
	}
}

func TestRepositorySelectsLatestGenerationAndRejectsTie(t *testing.T) {
	repository := testRepository(t)
	ctx := context.Background()
	profileReceipt, err := repository.CreateProfile(ctx, "media", []string{physicalIndexTempDir(t)}, false, DefaultScanLimits(), time.Now())
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

func TestRepositoryClassifiesInvalidIndexRecordsAndBindingsAsIntegrity(t *testing.T) {
	ctx := context.Background()
	t.Run("descriptor", func(t *testing.T) {
		repository := testRepository(t)
		profile, err := repository.CreateProfile(ctx, "media", []string{physicalIndexTempDir(t)}, false, DefaultScanLimits(), time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		ref, _, err := repository.store.ImportRecord(ctx, metastore.RecordKindStorageIndexDescriptorV1, bytes.NewBufferString("{}\n"), repository.recordLimits(repository.limits.MaxSnapshots))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repository.SelectSnapshot(ctx, profile.Profile, ref.ID); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("non-canonical descriptor was not classified as integrity failure: %v", err)
		}
	})

	t.Run("data", func(t *testing.T) {
		repository := testRepository(t)
		profile, err := repository.CreateProfile(ctx, "media", []string{physicalIndexTempDir(t)}, false, DefaultScanLimits(), time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		descriptorID := importTestDescriptor(t, repository, profile.Profile, 1, "invalid-data")
		if _, err := repository.LoadCandidates(ctx, profile.Profile, descriptorID, []int64{1}, DefaultCandidateLimits()); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("invalid data stream was not classified as integrity failure: %v", err)
		}
	})

	t.Run("descriptor_data_binding", func(t *testing.T) {
		repository := testRepository(t)
		root := physicalIndexTempDir(t)
		writeTestFile(t, filepath.Join(root, "payload.bin"), []byte("payload"))
		profile, err := repository.CreateProfile(ctx, "media", []string{root}, false, DefaultScanLimits(), time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		refresh, err := repository.Refresh(ctx, profile.Profile, RefreshOptions{Clock: deterministicClock(time.Now().UTC())})
		if err != nil {
			t.Fatal(err)
		}
		selected, err := repository.SelectSnapshot(ctx, profile.Profile, refresh.DescriptorRecord.ID)
		if err != nil {
			t.Fatal(err)
		}
		descriptor := selected.Descriptor
		descriptor.Files++
		raw, err := EncodeDescriptor(descriptor, repository.limits)
		if err != nil {
			t.Fatal(err)
		}
		ref, _, err := repository.store.ImportRecord(ctx, metastore.RecordKindStorageIndexDescriptorV1, bytes.NewReader(raw), repository.recordLimits(repository.limits.MaxSnapshots))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repository.LoadCandidates(ctx, profile.Profile, ref.ID, []int64{7}, DefaultCandidateLimits()); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("descriptor/data mismatch was not classified as integrity failure: %v", err)
		}
	})
}

func TestInvalidDescriptorOnlyBlocksTheProfileItNames(t *testing.T) {
	ctx := context.Background()
	importRaw := func(t *testing.T, repository *Repository, raw string) metastore.RecordID {
		t.Helper()
		ref, _, err := repository.store.ImportRecord(ctx, metastore.RecordKindStorageIndexDescriptorV1, bytes.NewBufferString(raw), repository.recordLimits(repository.limits.MaxSnapshots))
		if err != nil {
			t.Fatal(err)
		}
		return ref.ID
	}
	setup := func(t *testing.T) (*Repository, Profile, Profile, metastore.RecordID) {
		t.Helper()
		repository := testRepository(t)
		healthy := testIndexProfile(t, repository, "healthy")
		damaged := testIndexProfile(t, repository, "damaged")
		latest := importTestDescriptor(t, repository, healthy, 1, "healthy")
		return repository, healthy, damaged, latest
	}

	t.Run("attributed_to_other_profile", func(t *testing.T) {
		repository, healthy, damaged, latest := setup(t)
		importRaw(t, repository, `{"profile_id":"`+damaged.ID+`","generation":"bad"}`+"\n")
		selected, err := repository.SelectSnapshot(ctx, healthy, "")
		if err != nil || selected.DescriptorRecordID != latest || selected.ForeignInvalidDescriptors != 1 {
			t.Fatalf("another profile's invalid descriptor blocked selection: selection=%#v err=%v", selected, err)
		}
		if _, err := repository.SelectSnapshot(ctx, damaged, ""); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("invalid descriptor did not fail closed for the profile it names: %v", err)
		}
		if _, err := repository.Refresh(ctx, damaged, RefreshOptions{}); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("refresh of the damaged profile did not fail closed: %v", err)
		}
		if _, err := repository.PruneSnapshots(ctx, healthy, PruneOptions{RetainGenerations: 1}); err != nil {
			t.Fatalf("healthy profile retention was blocked: %v", err)
		}
	})

	for name, raw := range map[string]string{
		"unattributable":     "{}\n",
		"duplicate_key":      `{"profile_id":"PLACEHOLDER","profile_id":"PLACEHOLDER"}` + "\n",
		"non_string_profile": `{"profile_id":["PLACEHOLDER"]}` + "\n",
		"same_profile":       `{"profile_id":"SELF","generation":"bad"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			repository, healthy, damaged, _ := setup(t)
			raw = strings.ReplaceAll(strings.ReplaceAll(raw, "SELF", healthy.ID), "PLACEHOLDER", damaged.ID)
			if name == "duplicate_key" {
				raw = strings.Replace(raw, damaged.ID, healthy.ID, 1)
			}
			importRaw(t, repository, raw)
			if selected, err := repository.SelectSnapshot(ctx, healthy, ""); !errors.Is(err, ErrIntegrity) || selected.Status == "selected" {
				t.Fatalf("unsafe invalid descriptor did not fail closed: selection=%#v err=%v", selected, err)
			}
		})
	}
}

func testRepository(t *testing.T) *Repository {
	t.Helper()
	store, _, err := metastore.Init(filepath.Join(physicalIndexTempDir(t), "state"))
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func physicalIndexTempDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatalf("resolve temporary directory: %v", err)
	}
	return resolved
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
