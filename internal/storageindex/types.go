package storageindex

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ProfileFormat            = "ptctl.storage-profile/v1"
	SnapshotDataFormat       = "ptctl.storage-index-data/v1"
	SnapshotDescriptorFormat = "ptctl.storage-index-descriptor/v1"

	ProfileRecordKind            = "storage.profile.v1"
	SnapshotDataRecordKind       = "storage.index.data.v1"
	SnapshotDescriptorRecordKind = "storage.index.descriptor.v1"

	DefaultMaxProfiles           = 64
	DefaultMaxSnapshots          = 256
	DefaultMaxSnapshotBytes      = int64(32 << 20)
	DefaultMaxIndexedFiles       = 100_000
	DefaultMaxIndexedPathBytes   = int64(16 << 20)
	DefaultMaxPathComponents     = 64
	DefaultMaxPathComponentBytes = 4 << 10

	hardMaxProfiles           = 256
	hardMaxSnapshots          = 1_024
	hardMaxSnapshotBytes      = int64(64 << 20)
	hardMaxIndexedFiles       = 500_000
	hardMaxIndexedPathBytes   = int64(48 << 20)
	hardMaxPathComponents     = 128
	hardMaxPathComponentBytes = 16 << 10
	hardMaxRoots              = 32
	hardMaxProfileNameBytes   = 64
	hardMaxRootPathBytes      = 32 << 10
)

// Limits bound both immutable snapshot parsing and the amount of private
// state inspected while selecting profiles and descriptors. They do not grant
// any content-proof authority to an index entry.
type Limits struct {
	MaxProfiles           int   `json:"max_profiles"`
	MaxSnapshots          int   `json:"max_snapshots"`
	MaxSnapshotBytes      int64 `json:"max_snapshot_bytes"`
	MaxFiles              int   `json:"max_files"`
	MaxPathBytes          int64 `json:"max_path_bytes"`
	MaxPathComponents     int   `json:"max_path_components"`
	MaxPathComponentBytes int   `json:"max_path_component_bytes"`
}

func DefaultLimits() Limits {
	return Limits{
		MaxProfiles:           DefaultMaxProfiles,
		MaxSnapshots:          DefaultMaxSnapshots,
		MaxSnapshotBytes:      DefaultMaxSnapshotBytes,
		MaxFiles:              DefaultMaxIndexedFiles,
		MaxPathBytes:          DefaultMaxIndexedPathBytes,
		MaxPathComponents:     DefaultMaxPathComponents,
		MaxPathComponentBytes: DefaultMaxPathComponentBytes,
	}
}

func (limits Limits) Validate() error {
	checks := []struct {
		name  string
		value int64
		hard  int64
	}{
		{"max profiles", int64(limits.MaxProfiles), hardMaxProfiles},
		{"max snapshots", int64(limits.MaxSnapshots), hardMaxSnapshots},
		{"max snapshot bytes", limits.MaxSnapshotBytes, hardMaxSnapshotBytes},
		{"max files", int64(limits.MaxFiles), hardMaxIndexedFiles},
		{"max path bytes", limits.MaxPathBytes, hardMaxIndexedPathBytes},
		{"max path components", int64(limits.MaxPathComponents), hardMaxPathComponents},
		{"max path component bytes", int64(limits.MaxPathComponentBytes), hardMaxPathComponentBytes},
	}
	for _, check := range checks {
		if check.value <= 0 || check.value > check.hard {
			return fmt.Errorf("%s must be in 1..%d", check.name, check.hard)
		}
	}
	return nil
}

// ScanLimits are persisted with a profile so an invocation may tighten but
// never silently expand the reviewed filesystem scope.
type ScanLimits struct {
	MaxDepth               int   `json:"max_depth"`
	MaxDirectories         int   `json:"max_directories"`
	MaxEntries             int   `json:"max_entries"`
	MaxEntriesPerDirectory int   `json:"max_entries_per_directory"`
	MaxFiles               int   `json:"max_files"`
	MaxPathBytes           int64 `json:"max_path_bytes"`
	MaxIssues              int   `json:"max_issues"`
}

func DefaultScanLimits() ScanLimits {
	return ScanLimits{
		MaxDepth:               32,
		MaxDirectories:         25_000,
		MaxEntries:             100_000,
		MaxEntriesPerDirectory: 50_000,
		MaxFiles:               DefaultMaxIndexedFiles,
		MaxPathBytes:           DefaultMaxIndexedPathBytes,
		MaxIssues:              50,
	}
}

func (limits ScanLimits) Validate() error {
	checks := []struct {
		name  string
		value int64
		hard  int64
	}{
		{"max depth", int64(limits.MaxDepth), 64},
		{"max directories", int64(limits.MaxDirectories), 100_000},
		{"max entries", int64(limits.MaxEntries), 500_000},
		{"max entries per directory", int64(limits.MaxEntriesPerDirectory), 200_000},
		{"max files", int64(limits.MaxFiles), hardMaxIndexedFiles},
		{"max path bytes", limits.MaxPathBytes, hardMaxIndexedPathBytes},
		{"max issues", int64(limits.MaxIssues), 200},
	}
	for _, check := range checks {
		if check.value <= 0 || check.value > check.hard {
			return fmt.Errorf("%s must be in 1..%d", check.name, check.hard)
		}
	}
	return nil
}

// ValidateScanLimitsForIndex ensures a persisted scan policy can always be
// represented by the repository which will seal its snapshots. This is
// checked both when a profile is created and again before every live use.
func ValidateScanLimitsForIndex(scan ScanLimits, index Limits) error {
	if err := scan.Validate(); err != nil {
		return err
	}
	if err := index.Validate(); err != nil {
		return err
	}
	if scan.MaxFiles > index.MaxFiles || scan.MaxPathBytes > index.MaxPathBytes || scan.MaxDepth+1 > index.MaxPathComponents {
		return ErrProfileExceedsIndexLimits
	}
	return nil
}

type Profile struct {
	Format       string        `json:"format"`
	ID           string        `json:"profile_id"`
	Revision     string        `json:"profile_revision"`
	Name         string        `json:"name"`
	CreatedAt    time.Time     `json:"created_at"`
	Platform     string        `json:"platform"`
	PathEncoding string        `json:"path_encoding"`
	MountPolicy  string        `json:"mount_policy"`
	AllowNetwork bool          `json:"allow_network"`
	ScanLimits   ScanLimits    `json:"scan_limits"`
	Roots        []ProfileRoot `json:"roots"`
}

// ProfileRoot keeps the path as canonical base64 bytes. A display path is
// presentation-only and never becomes reopen authority after serialization.
type ProfileRoot struct {
	ID            string `json:"root_id"`
	PathRawBase64 string `json:"path_raw_base64"`
}

func NewProfile(name string, roots []string, allowNetwork bool, scanLimits ScanLimits, now time.Time) (Profile, error) {
	if err := validateProfileName(name); err != nil {
		return Profile{}, err
	}
	if err := scanLimits.Validate(); err != nil {
		return Profile{}, err
	}
	if len(roots) == 0 || len(roots) > hardMaxRoots {
		return Profile{}, fmt.Errorf("profile roots must contain 1..%d paths", hardMaxRoots)
	}
	paths := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, path := range roots {
		if path == "" || strings.IndexByte(path, 0) >= 0 || len(path) > hardMaxRootPathBytes {
			return Profile{}, fmt.Errorf("profile root is empty or invalid")
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return Profile{}, fmt.Errorf("profile root cannot be resolved")
		}
		absolute = filepath.Clean(absolute)
		key := base64.StdEncoding.EncodeToString([]byte(absolute))
		if _, exists := seen[key]; exists {
			return Profile{}, fmt.Errorf("profile root is duplicated")
		}
		seen[key] = struct{}{}
		paths = append(paths, key)
	}
	sort.Strings(paths)
	profileID, revision := profileDeclarationIDs(runtime.GOOS, allowNetwork, scanLimits, paths)
	result := Profile{
		Format: ProfileFormat, ID: profileID, Revision: revision, Name: name,
		CreatedAt: canonicalTime(now), Platform: runtime.GOOS, PathEncoding: "native_components_base64", MountPolicy: "one_filesystem",
		AllowNetwork: allowNetwork, ScanLimits: scanLimits,
		Roots: make([]ProfileRoot, 0, len(paths)),
	}
	for _, path := range paths {
		rootDigest := sha256.Sum256([]byte("ptctl-storage-profile-root-v1\x00" + profileID + "\x00" + path))
		result.Roots = append(result.Roots, ProfileRoot{ID: "root:" + hex.EncodeToString(rootDigest[:]), PathRawBase64: path})
	}
	sort.Slice(result.Roots, func(i, j int) bool { return result.Roots[i].ID < result.Roots[j].ID })
	if err := result.Validate(); err != nil {
		return Profile{}, err
	}
	return result, nil
}

func (profile Profile) Validate() error {
	if profile.Format != ProfileFormat || validateOpaqueID(profile.ID, "profile") != nil || validateOpaqueID(profile.Revision, "revision") != nil {
		return fmt.Errorf("storage profile identity is invalid")
	}
	if err := validateProfileName(profile.Name); err != nil {
		return err
	}
	if !isCanonicalTime(profile.CreatedAt) {
		return fmt.Errorf("storage profile time is invalid")
	}
	if (profile.Platform != "linux" && profile.Platform != "darwin" && profile.Platform != "windows") ||
		profile.PathEncoding != "native_components_base64" || profile.MountPolicy != "one_filesystem" {
		return fmt.Errorf("storage profile filesystem semantics are invalid")
	}
	if err := profile.ScanLimits.Validate(); err != nil {
		return err
	}
	if len(profile.Roots) == 0 || len(profile.Roots) > hardMaxRoots {
		return fmt.Errorf("storage profile root count is invalid")
	}
	seenIDs := make(map[string]struct{}, len(profile.Roots))
	seenPaths := make(map[string]struct{}, len(profile.Roots))
	paths := make([]string, 0, len(profile.Roots))
	previous := ""
	for _, root := range profile.Roots {
		if validateOpaqueID(root.ID, "root") != nil || root.ID <= previous {
			return fmt.Errorf("storage profile root identity is invalid or unsorted")
		}
		previous = root.ID
		if _, exists := seenIDs[root.ID]; exists {
			return fmt.Errorf("storage profile root identity is duplicated")
		}
		seenIDs[root.ID] = struct{}{}
		raw, err := base64.StdEncoding.Strict().DecodeString(root.PathRawBase64)
		if err != nil || len(raw) == 0 || len(raw) > hardMaxRootPathBytes || strings.IndexByte(string(raw), 0) >= 0 || base64.StdEncoding.EncodeToString(raw) != root.PathRawBase64 {
			return fmt.Errorf("storage profile root path is invalid")
		}
		if _, exists := seenPaths[root.PathRawBase64]; exists {
			return fmt.Errorf("storage profile root path is duplicated")
		}
		seenPaths[root.PathRawBase64] = struct{}{}
		paths = append(paths, root.PathRawBase64)
		rootDigest := sha256.Sum256([]byte("ptctl-storage-profile-root-v1\x00" + profile.ID + "\x00" + root.PathRawBase64))
		if root.ID != "root:"+hex.EncodeToString(rootDigest[:]) {
			return fmt.Errorf("storage profile root identity does not match its declaration")
		}
	}
	sort.Strings(paths)
	expectedID, expectedRevision := profileDeclarationIDs(profile.Platform, profile.AllowNetwork, profile.ScanLimits, paths)
	if profile.ID != expectedID || profile.Revision != expectedRevision {
		return fmt.Errorf("storage profile identity does not match its declaration")
	}
	return nil
}

// validateLivePlatform is deliberately separate from Validate: stored
// profiles remain inspectable on another operating system, but their native
// path bytes must never be interpreted or opened there.
func validateLivePlatform(profile Profile) error {
	if profile.Platform != runtime.GOOS {
		return ErrProfilePlatformMismatch
	}
	for _, root := range profile.Roots {
		path, err := root.Path()
		if err != nil || !filepath.IsAbs(path) || filepath.Clean(path) != path || (runtime.GOOS == "windows" && !utf8.ValidString(path)) {
			return ErrProfileLivePathInvalid
		}
	}
	return nil
}

// ValidateProfileForLiveUse performs every preflight which must precede
// interpreting persisted native path bytes. Cross-platform profiles remain
// inspectable, but refresh/query callers must pass this gate before any live
// filesystem or downloader work.
func ValidateProfileForLiveUse(profile Profile, index Limits) error {
	if err := profile.Validate(); err != nil {
		return err
	}
	if err := validateLivePlatform(profile); err != nil {
		return err
	}
	return ValidateScanLimitsForIndex(profile.ScanLimits, index)
}

func (root ProfileRoot) Path() (string, error) {
	raw, err := base64.StdEncoding.Strict().DecodeString(root.PathRawBase64)
	if err != nil || len(raw) == 0 || strings.IndexByte(string(raw), 0) >= 0 || base64.StdEncoding.EncodeToString(raw) != root.PathRawBase64 {
		return "", fmt.Errorf("storage profile root path is invalid")
	}
	return string(raw), nil
}

type SnapshotHeader struct {
	Type            string    `json:"type"`
	Format          string    `json:"format"`
	SnapshotID      string    `json:"snapshot_id"`
	Generation      uint64    `json:"generation"`
	ProfileID       string    `json:"profile_id"`
	ProfileRevision string    `json:"profile_revision"`
	Platform        string    `json:"platform"`
	PathEncoding    string    `json:"path_encoding"`
	ObservedAtStart time.Time `json:"observed_at_start"`
	RootIDs         []string  `json:"root_ids"`
}

type Entry struct {
	Type                        string   `json:"type"`
	RootID                      string   `json:"root_id"`
	RelativeComponentsRawBase64 []string `json:"relative_components_raw_base64"`
	SizeBytes                   int64    `json:"size_bytes"`
	ModifiedUnixNanos           int64    `json:"modified_unix_nanos"`
	IdentityHint                string   `json:"identity_hint,omitempty"`
}

type SnapshotFooter struct {
	Type          string    `json:"type"`
	SnapshotID    string    `json:"snapshot_id"`
	ObservedAtEnd time.Time `json:"observed_at_end"`
	Complete      bool      `json:"complete"`
	Files         int       `json:"files"`
	PathBytes     int64     `json:"path_bytes"`
}

type SnapshotDescriptor struct {
	Format           string                    `json:"format"`
	ID               string                    `json:"snapshot_id"`
	Generation       uint64                    `json:"generation"`
	ProfileID        string                    `json:"profile_id"`
	ProfileRevision  string                    `json:"profile_revision"`
	Platform         string                    `json:"platform"`
	PathEncoding     string                    `json:"path_encoding"`
	DataRecordID     string                    `json:"data_record_id"`
	ObservedAtStart  time.Time                 `json:"observed_at_start"`
	ObservedAtEnd    time.Time                 `json:"observed_at_end"`
	Files            int                       `json:"files"`
	PathBytes        int64                     `json:"path_bytes"`
	EnumerationScope string                    `json:"enumeration_scope"`
	LiveFreshness    string                    `json:"live_freshness"`
	Roots            []SnapshotRootObservation `json:"roots"`
}

type SnapshotRootObservation struct {
	RootID                 string `json:"root_id"`
	Status                 string `json:"status"`
	FilesystemIdentityHint string `json:"filesystem_identity_hint"`
	RootIdentityHint       string `json:"root_identity_hint"`
}

func NewSnapshotHeader(profile Profile, generation uint64, now time.Time) (SnapshotHeader, error) {
	if err := profile.Validate(); err != nil {
		return SnapshotHeader{}, err
	}
	if generation == 0 {
		return SnapshotHeader{}, fmt.Errorf("snapshot generation must be positive")
	}
	id, err := newOpaqueID("snapshot")
	if err != nil {
		return SnapshotHeader{}, fmt.Errorf("snapshot identifier unavailable")
	}
	roots := make([]string, len(profile.Roots))
	for i := range profile.Roots {
		roots[i] = profile.Roots[i].ID
	}
	sort.Strings(roots)
	return SnapshotHeader{
		Type: "header", Format: SnapshotDataFormat, SnapshotID: id, Generation: generation,
		ProfileID: profile.ID, ProfileRevision: profile.Revision,
		Platform: profile.Platform, PathEncoding: profile.PathEncoding,
		ObservedAtStart: canonicalTime(now), RootIDs: roots,
	}, nil
}

func (header SnapshotHeader) Validate() error {
	if header.Type != "header" || header.Format != SnapshotDataFormat || header.Generation == 0 || validateOpaqueID(header.SnapshotID, "snapshot") != nil ||
		validateOpaqueID(header.ProfileID, "profile") != nil || validateOpaqueID(header.ProfileRevision, "revision") != nil ||
		(header.Platform != "linux" && header.Platform != "darwin" && header.Platform != "windows") || header.PathEncoding != "native_components_base64" ||
		!isCanonicalTime(header.ObservedAtStart) {
		return fmt.Errorf("storage index header is invalid")
	}
	if len(header.RootIDs) == 0 || len(header.RootIDs) > hardMaxRoots {
		return fmt.Errorf("storage index root scope is invalid")
	}
	previous := ""
	for _, id := range header.RootIDs {
		if validateOpaqueID(id, "root") != nil || id <= previous {
			return fmt.Errorf("storage index root scope is invalid or unsorted")
		}
		previous = id
	}
	return nil
}

func (descriptor SnapshotDescriptor) Validate(limits Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	if descriptor.Format != SnapshotDescriptorFormat || descriptor.Generation == 0 || validateOpaqueID(descriptor.ID, "snapshot") != nil ||
		validateOpaqueID(descriptor.ProfileID, "profile") != nil || validateOpaqueID(descriptor.ProfileRevision, "revision") != nil ||
		(descriptor.Platform != "linux" && descriptor.Platform != "darwin" && descriptor.Platform != "windows") || descriptor.PathEncoding != "native_components_base64" ||
		validateSHA256ID(descriptor.DataRecordID) != nil || !isCanonicalTime(descriptor.ObservedAtStart) || !isCanonicalTime(descriptor.ObservedAtEnd) ||
		descriptor.ObservedAtEnd.Before(descriptor.ObservedAtStart) || descriptor.Files < 0 || descriptor.Files > limits.MaxFiles ||
		descriptor.PathBytes < 0 || descriptor.PathBytes > limits.MaxPathBytes || descriptor.EnumerationScope != "complete_snapshot" ||
		descriptor.LiveFreshness != "unproven_since_snapshot" {
		return fmt.Errorf("storage index descriptor is invalid")
	}
	if len(descriptor.Roots) == 0 || len(descriptor.Roots) > hardMaxRoots {
		return fmt.Errorf("storage index descriptor root observations are invalid")
	}
	previous := ""
	for _, root := range descriptor.Roots {
		if validateOpaqueID(root.RootID, "root") != nil || root.RootID <= previous || root.Status != "complete" ||
			!validIdentityHint(root.FilesystemIdentityHint) || !validIdentityHint(root.RootIdentityHint) {
			return fmt.Errorf("storage index descriptor root observation is invalid or unsorted")
		}
		previous = root.RootID
	}
	return nil
}

func validateEntry(entry Entry, header SnapshotHeader, limits Limits) (int64, string, error) {
	if entry.Type != "file" || validateOpaqueID(entry.RootID, "root") != nil || entry.SizeBytes < 0 || entry.ModifiedUnixNanos < 0 ||
		len(entry.RelativeComponentsRawBase64) == 0 || len(entry.RelativeComponentsRawBase64) > limits.MaxPathComponents {
		return 0, "", fmt.Errorf("storage index entry is invalid")
	}
	foundRoot := false
	for _, id := range header.RootIDs {
		if id == entry.RootID {
			foundRoot = true
			break
		}
	}
	if !foundRoot {
		return 0, "", fmt.Errorf("storage index entry is outside its root scope")
	}
	var pathBytes int64
	var key strings.Builder
	key.WriteString(entry.RootID)
	for _, encoded := range entry.RelativeComponentsRawBase64 {
		raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
		component := string(raw)
		invalidSeparator := strings.Contains(component, "/")
		if header.Platform == "windows" {
			invalidSeparator = strings.ContainsAny(component, "/\\") || !utf8.Valid(raw)
		}
		if err != nil || len(raw) == 0 || len(raw) > limits.MaxPathComponentBytes || base64.StdEncoding.EncodeToString(raw) != encoded ||
			strings.IndexByte(component, 0) >= 0 || component == "." || component == ".." || invalidSeparator {
			return 0, "", fmt.Errorf("storage index path component is invalid")
		}
		pathBytes += int64(len(raw))
		if pathBytes > limits.MaxPathBytes {
			return 0, "", fmt.Errorf("storage index path budget exceeded")
		}
		key.WriteByte(0)
		key.Write(raw)
	}
	if entry.IdentityHint != "" && !validIdentityHint(entry.IdentityHint) {
		return 0, "", fmt.Errorf("storage index identity hint is invalid")
	}
	return pathBytes, key.String(), nil
}

func validateProfileName(value string) error {
	if len(value) == 0 || len(value) > hardMaxProfileNameBytes {
		return fmt.Errorf("storage profile name must contain 1..%d bytes", hardMaxProfileNameBytes)
	}
	for _, ch := range []byte(value) {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '.' || ch == '_' || ch == '-') {
			return fmt.Errorf("storage profile name contains an unsupported character")
		}
	}
	return nil
}

// profileDeclarationIDs intentionally excludes the display name and creation
// time. The authority is the exact filesystem scope and policy declaration;
// names are selectors which may become ambiguous, never proof.
func profileDeclarationIDs(platform string, allowNetwork bool, scanLimits ScanLimits, sortedPaths []string) (string, string) {
	declaration := strings.Join([]string{
		ProfileFormat, platform, "native_components_base64", "one_filesystem", fmt.Sprint(allowNetwork),
		fmt.Sprint(scanLimits.MaxDepth), fmt.Sprint(scanLimits.MaxDirectories), fmt.Sprint(scanLimits.MaxEntries),
		fmt.Sprint(scanLimits.MaxEntriesPerDirectory), fmt.Sprint(scanLimits.MaxFiles), fmt.Sprint(scanLimits.MaxPathBytes), fmt.Sprint(scanLimits.MaxIssues),
		strings.Join(sortedPaths, "\x00"),
	}, "\x00")
	profileDigest := sha256.Sum256([]byte("ptctl-storage-profile-declaration-v1\x00" + declaration))
	revisionDigest := sha256.Sum256([]byte("ptctl-storage-profile-revision-v1\x00" + declaration))
	return "profile:" + hex.EncodeToString(profileDigest[:]), "revision:" + hex.EncodeToString(revisionDigest[:])
}

func validIdentityHint(value string) bool {
	if len(value) == 0 || len(value) > 512 || strings.IndexByte(value, 0) >= 0 {
		return false
	}
	for _, ch := range []byte(value) {
		if ch < 0x20 || ch == 0x7f {
			return false
		}
	}
	return true
}

func newOpaqueID(prefix string) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + ":" + hex.EncodeToString(raw[:]), nil
}

func validateOpaqueID(value, prefix string) error {
	wanted := prefix + ":"
	if !strings.HasPrefix(value, wanted) || len(value) != len(wanted)+64 {
		return fmt.Errorf("opaque identifier is invalid")
	}
	digest := value[len(wanted):]
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 || strings.ToLower(digest) != digest {
		return fmt.Errorf("opaque identifier is invalid")
	}
	return nil
}

func validateSHA256ID(value string) error { return validateOpaqueID(value, "sha256") }

func canonicalTime(value time.Time) time.Time {
	return value.UTC().Round(0)
}

func isCanonicalTime(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC && value.Equal(value.Round(0))
}
