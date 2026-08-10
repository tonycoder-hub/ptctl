package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/storage"
	"github.com/tonycoder-hub/ptctl/internal/storageindex"
)

type storageProfileRootReport struct {
	ID   string `json:"root_id"`
	Path string `json:"absolute_path,omitempty"`
}

type storageProfileView struct {
	ID           string                     `json:"profile_id"`
	Revision     string                     `json:"profile_revision"`
	Name         string                     `json:"name"`
	CreatedAt    time.Time                  `json:"created_at"`
	Platform     string                     `json:"platform"`
	PathEncoding string                     `json:"path_encoding"`
	MountPolicy  string                     `json:"mount_policy"`
	AllowNetwork bool                       `json:"allow_network"`
	ScanLimits   storageindex.ScanLimits    `json:"scan_limits"`
	Roots        []storageProfileRootReport `json:"roots"`
}

type storageProfileReport struct {
	kind               string              `json:"-"`
	Outcome            string              `json:"outcome"`
	Effect             string              `json:"effect"`
	WritesPerformed    int                 `json:"writes_performed"`
	AlreadyPresent     bool                `json:"already_present"`
	AbsolutePathsShown bool                `json:"absolute_paths_shown"`
	RecordID           metastore.RecordID  `json:"record_id"`
	Store              metastore.StoreInfo `json:"store"`
	Profile            storageProfileView  `json:"profile"`
	Warnings           []string            `json:"warnings"`
}

type storageIndexReport struct {
	kind                  string                        `json:"-"`
	Outcome               string                        `json:"outcome"`
	Effect                string                        `json:"effect"`
	WritesPerformed       int                           `json:"writes_performed"`
	ProfileID             string                        `json:"profile_id"`
	ProfileRevision       string                        `json:"profile_revision"`
	Generation            uint64                        `json:"generation"`
	SnapshotID            string                        `json:"snapshot_id,omitempty"`
	DataRecord            metastore.RecordRef           `json:"data_record"`
	DescriptorRecord      metastore.RecordRef           `json:"descriptor_record"`
	DataPublication       metastore.RecordImportReceipt `json:"data_publication"`
	DescriptorPublication metastore.RecordImportReceipt `json:"descriptor_publication"`
	Scan                  storage.FullInventoryResult   `json:"scan"`
	StopReasons           []string                      `json:"stop_reasons"`
	Warnings              []string                      `json:"warnings"`
}

type storageIndexInspectReport struct {
	kind               string                          `json:"-"`
	Outcome            string                          `json:"outcome"`
	Effect             string                          `json:"effect"`
	WritesPerformed    int                             `json:"writes_performed"`
	SelectionComplete  bool                            `json:"selection_complete"`
	RecordsConsidered  int                             `json:"records_considered"`
	DescriptorsMatched int                             `json:"descriptors_matched"`
	DescriptorRecordID metastore.RecordID              `json:"descriptor_record_id"`
	Descriptor         storageindex.SnapshotDescriptor `json:"descriptor"`
	Store              metastore.StoreInfo             `json:"store"`
	Warnings           []string                        `json:"warnings"`
}

func (a *app) storageProfileCommand(args []string) error {
	if len(args) == 0 {
		return usageError("storage profile requires create or inspect")
	}
	switch args[0] {
	case "create":
		return a.storageProfileCreate(args[1:])
	case "inspect":
		return a.storageProfileInspect(args[1:])
	default:
		return usageError("unknown storage profile subcommand %q", args[0])
	}
}

func (a *app) storageProfileCreate(args []string) error {
	fs := newFlagSet("storage profile create")
	output := fs.String("output", "table", "table or json")
	stateStore := fs.String("state-store", "", "initialized private state/metafile store root")
	name := fs.String("name", "", "immutable profile display selector")
	var roots stringListFlag
	fs.Var(&roots, "search-root", "profile filesystem root; repeatable")
	allowNetwork := fs.Bool("allow-network", false, "allow explicit network/UNC roots in this immutable profile")
	showAbsolute := fs.Bool("show-absolute-paths", false, "include profile root paths in output")
	defaults := storageindex.DefaultScanLimits()
	maxDepth := fs.Int("max-depth", defaults.MaxDepth, "maximum directory depth per refresh")
	maxDirectories := fs.Int("max-directories", defaults.MaxDirectories, "maximum directories opened per refresh")
	maxEntries := fs.Int("max-entries", defaults.MaxEntries, "maximum directory entries examined per refresh")
	maxDirectoryEntries := fs.Int("max-directory-entries", defaults.MaxEntriesPerDirectory, "maximum entries accepted from one directory")
	maxFiles := fs.Int("max-files", defaults.MaxFiles, "maximum regular-file records in a snapshot")
	maxPathBytes := fs.Int64("max-path-bytes", defaults.MaxPathBytes, "maximum raw relative-path bytes in a snapshot")
	if handled, err := parseStorageFlags(a, fs, args,
		"ptctl storage profile create --state-store DIR --name NAME --search-root PATH [--search-root PATH...] [flags]",
		"Creates one immutable private profile declaration. It validates configuration but does not scan the roots."); handled || err != nil {
		return err
	}
	if fs.NArg() != 0 || *stateStore == "" || *name == "" || len(roots) == 0 {
		return usageError("storage profile create requires --state-store, --name, one or more --search-root values, and flags only")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	limits := defaults
	limits.MaxDepth = *maxDepth
	limits.MaxDirectories = *maxDirectories
	limits.MaxEntries = *maxEntries
	limits.MaxEntriesPerDirectory = *maxDirectoryEntries
	limits.MaxFiles = *maxFiles
	limits.MaxPathBytes = *maxPathBytes
	if err := limits.Validate(); err != nil {
		return usageError("storage profile create: %v", err)
	}
	indexLimits := storageindex.DefaultLimits()
	if err := storageindex.ValidateScanLimitsForIndex(limits, indexLimits); err != nil {
		return usageError("storage profile create: %v", err)
	}
	store, err := metastore.Open(*stateStore)
	if err != nil {
		return err
	}
	repository, err := storageindex.NewRepository(store, indexLimits)
	if err != nil {
		return err
	}
	receipt, err := repository.CreateProfile(context.Background(), *name, append([]string(nil), roots...), *allowNetwork, limits, time.Now().UTC())
	if err != nil {
		if receipt.WritesPerformed > 0 {
			report := storageProfileReport{
				kind: "storage.profile.create", Outcome: "published_post_commit_failure", Effect: receipt.Effect,
				WritesPerformed: receipt.WritesPerformed, AbsolutePathsShown: *showAbsolute,
				RecordID: receipt.RecordID, Store: receipt.Store, Profile: publicStorageProfile(receipt.Profile, *showAbsolute),
				Warnings: []string{"a sealed profile record became visible, but final publication assurance was not confirmed; retry is idempotent"},
			}
			if errors.Is(err, metastore.ErrDurabilityUnconfirmed) {
				report.Outcome = "published_durability_unconfirmed"
			}
			if writeErr := writeStorageProfileReport(a.stdout, *output, report); writeErr != nil {
				return writeErr
			}
		}
		return err
	}
	report := storageProfileReport{
		kind: "storage.profile.create", Outcome: "created", Effect: receipt.Effect,
		WritesPerformed: receipt.WritesPerformed, AlreadyPresent: receipt.AlreadyPresent, AbsolutePathsShown: *showAbsolute,
		RecordID: receipt.RecordID, Store: receipt.Store, Profile: publicStorageProfile(receipt.Profile, *showAbsolute),
		Warnings: []string{"profile identity binds roots and scan policy; changing either requires a new immutable profile declaration"},
	}
	if receipt.AlreadyPresent {
		report.Outcome = "already_present"
	}
	return writeStorageProfileReport(a.stdout, *output, report)
}

func (a *app) storageProfileInspect(args []string) error {
	fs := newFlagSet("storage profile inspect")
	output := fs.String("output", "table", "table or json")
	stateStore := fs.String("state-store", "", "initialized private state/metafile store root")
	showAbsolute := fs.Bool("show-absolute-paths", false, "include profile root paths in output")
	if handled, err := parseStorageFlags(a, fs, args,
		"ptctl storage profile inspect --state-store DIR [flags] PROFILE",
		"Reads and validates one sealed profile. Foreign-platform profiles are inspectable but cannot be used live."); handled || err != nil {
		return err
	}
	if fs.NArg() != 1 || *stateStore == "" {
		return usageError("storage profile inspect requires --state-store and one PROFILE name or ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	store, err := metastore.Open(*stateStore)
	if err != nil {
		return err
	}
	repository, err := storageindex.NewRepository(store, storageindex.DefaultLimits())
	if err != nil {
		return err
	}
	selection, err := repository.SelectProfile(context.Background(), fs.Arg(0))
	if err != nil {
		return err
	}
	report := storageProfileReport{
		kind: "storage.profile.inspect", Outcome: "selected", Effect: selection.Effect, WritesPerformed: 0,
		AbsolutePathsShown: *showAbsolute, RecordID: selection.RecordID, Store: selection.Store,
		Profile: publicStorageProfile(selection.Profile, *showAbsolute), Warnings: []string{},
	}
	return writeStorageProfileReport(a.stdout, *output, report)
}

func (a *app) storageIndexCommand(args []string) error {
	if len(args) == 0 {
		return usageError("storage index requires refresh or inspect")
	}
	switch args[0] {
	case "refresh":
		return a.storageIndexRefresh(args[1:])
	case "inspect":
		return a.storageIndexInspect(args[1:])
	default:
		return usageError("unknown storage index subcommand %q", args[0])
	}
}

func (a *app) storageIndexRefresh(args []string) error {
	fs := newFlagSet("storage index refresh")
	output := fs.String("output", "table", "table or json")
	stateStore := fs.String("state-store", "", "initialized private state/metafile store root")
	profileSelector := fs.String("profile", "", "storage profile name or immutable ID")
	timeout := fs.Duration("timeout", time.Hour, "shared inventory and publication wall-clock budget")
	if handled, err := parseStorageFlags(a, fs, args,
		"ptctl storage index refresh --state-store DIR --profile PROFILE [flags]",
		"Performs a bounded metadata scan and explicitly writes an immutable data record followed by its descriptor."); handled || err != nil {
		return err
	}
	if fs.NArg() != 0 || *stateStore == "" || *profileSelector == "" {
		return usageError("storage index refresh requires --state-store, --profile, and flags only")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > 7*24*time.Hour {
		return usageError("--timeout must be greater than zero and no more than 168h")
	}
	store, err := metastore.Open(*stateStore)
	if err != nil {
		return err
	}
	repository, err := storageindex.NewRepository(store, storageindex.DefaultLimits())
	if err != nil {
		return err
	}
	selection, err := repository.SelectProfile(context.Background(), *profileSelector)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	refresh, refreshErr := repository.Refresh(ctx, selection.Profile, storageindex.RefreshOptions{})
	report := publicStorageIndexRefresh(refresh)
	writeErr := writeStorageIndexReport(a.stdout, *output, report)
	if writeErr != nil {
		return writeErr
	}
	if refreshErr != nil {
		return refreshErr
	}
	if refresh.Status != "stored" {
		return &inconclusiveErr{message: "storage index refresh did not publish a complete descriptor"}
	}
	return nil
}

func (a *app) storageIndexInspect(args []string) error {
	fs := newFlagSet("storage index inspect")
	output := fs.String("output", "table", "table or json")
	stateStore := fs.String("state-store", "", "initialized private state/metafile store root")
	profileSelector := fs.String("profile", "", "storage profile name or immutable ID")
	explicitRecord := fs.String("snapshot-record", "", "explicit sealed descriptor record ID; bypasses latest listing")
	if handled, err := parseStorageFlags(a, fs, args,
		"ptctl storage index inspect --state-store DIR --profile PROFILE [--snapshot-record ID] [flags]",
		"Reads and validates a sealed descriptor. Historical completeness is never current-filesystem proof."); handled || err != nil {
		return err
	}
	if fs.NArg() != 0 || *stateStore == "" || *profileSelector == "" {
		return usageError("storage index inspect requires --state-store, --profile, and flags only")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var descriptorID metastore.RecordID
	var err error
	if flagWasSet(fs, "snapshot-record") {
		descriptorID, err = metastore.ParseRecordID(*explicitRecord)
		if err != nil {
			return usageError("--snapshot-record is invalid")
		}
	}
	store, err := metastore.Open(*stateStore)
	if err != nil {
		return err
	}
	repository, err := storageindex.NewRepository(store, storageindex.DefaultLimits())
	if err != nil {
		return err
	}
	profile, err := repository.SelectProfile(context.Background(), *profileSelector)
	if err != nil {
		return err
	}
	selection, err := repository.SelectSnapshot(context.Background(), profile.Profile, descriptorID)
	if err != nil {
		return err
	}
	descriptor := selection.Descriptor
	for index := range descriptor.Roots {
		descriptor.Roots[index].FilesystemIdentityHint = ""
		descriptor.Roots[index].RootIdentityHint = ""
	}
	report := storageIndexInspectReport{
		kind: "storage.index.inspect", Outcome: selection.Status, Effect: selection.Effect, WritesPerformed: 0,
		SelectionComplete: selection.Complete, RecordsConsidered: selection.RecordsConsidered, DescriptorsMatched: selection.DescriptorsMatched,
		DescriptorRecordID: selection.DescriptorRecordID, Descriptor: descriptor, Store: selection.Store,
		Warnings: []string{"complete_snapshot describes a bounded historical enumeration, not current filesystem completeness"},
	}
	return writeStorageIndexInspectReport(a.stdout, *output, report)
}

func publicStorageProfile(profile storageindex.Profile, showAbsolute bool) storageProfileView {
	result := storageProfileView{
		ID: profile.ID, Revision: profile.Revision, Name: profile.Name, CreatedAt: profile.CreatedAt,
		Platform: profile.Platform, PathEncoding: profile.PathEncoding, MountPolicy: profile.MountPolicy,
		AllowNetwork: profile.AllowNetwork, ScanLimits: profile.ScanLimits, Roots: []storageProfileRootReport{},
	}
	for _, root := range profile.Roots {
		item := storageProfileRootReport{ID: root.ID}
		if showAbsolute {
			item.Path, _ = root.Path()
		}
		result.Roots = append(result.Roots, item)
	}
	return result
}

func parseStorageFlags(a *app, fs *flag.FlagSet, args []string, usageLine, summary string) (bool, error) {
	var output strings.Builder
	fs.SetOutput(&output)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintf(fs.Output(), "  %s\n\n", usageLine)
		fmt.Fprintln(fs.Output(), summary)
		fmt.Fprintln(fs.Output(), "\nFlags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, writeErr := fmt.Fprint(a.stdout, output.String())
			return true, writeErr
		}
		detail := strings.TrimSpace(output.String())
		if detail == "" {
			detail = err.Error()
		}
		return false, usageError("%s: %s", fs.Name(), detail)
	}
	return false, nil
}

func publicStorageIndexRefresh(value storageindex.RefreshResult) storageIndexReport {
	scan := value.Scan
	scan.Roots = append([]storage.FullInventoryRootObservation(nil), scan.Roots...)
	for index := range scan.Roots {
		scan.Roots[index].FilesystemIdentityHint = ""
		scan.Roots[index].RootIdentityHint = ""
	}
	scan.Issues = append([]storage.ScanIssue(nil), scan.Issues...)
	for index := range scan.Issues {
		// Relative names are private inventory data. Refresh has no path
		// disclosure mode, so public reports retain only root IDs and stable
		// issue codes/counts.
		scan.Issues[index].RelativePath = ""
	}
	return storageIndexReport{
		kind: "storage.index.refresh", Outcome: value.Status, Effect: value.Effect, WritesPerformed: value.WritesPerformed,
		ProfileID: value.ProfileID, ProfileRevision: value.ProfileRevision, Generation: value.Generation, SnapshotID: value.SnapshotID,
		DataRecord: value.DataRecord, DescriptorRecord: value.DescriptorRecord,
		DataPublication: value.DataPublication, DescriptorPublication: value.DescriptorPublication,
		Scan: scan, StopReasons: append([]string{}, value.StopReasons...),
		Warnings: []string{"the descriptor is published only after a complete sealed inventory; historical completeness never proves current uniqueness"},
	}
}

func writeStorageProfileReport(out io.Writer, output string, report storageProfileReport) error {
	if output == "json" {
		return writeJSON(out, report, nil)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES\t%d\nPROFILE\t%s\nREVISION\t%s\nNAME\t%s\nPLATFORM\t%s\nNETWORK ROOTS\t%t\nRECORD\t%s\n",
		terminalSafe(report.Outcome), terminalSafe(report.Effect), report.WritesPerformed, terminalSafe(report.Profile.ID), terminalSafe(report.Profile.Revision), terminalSafe(report.Profile.Name), terminalSafe(report.Profile.Platform), report.Profile.AllowNetwork, terminalSafe(report.RecordID.String()))
	fmt.Fprintln(w, "ROOT ID\tPATH")
	for _, root := range report.Profile.Roots {
		path := "hidden"
		if root.Path != "" {
			path = root.Path
		}
		fmt.Fprintf(w, "%s\t%s\n", terminalSafe(root.ID), terminalSafe(path))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(out, "warning: %s\n", terminalSafe(warning))
	}
	return nil
}

func writeStorageIndexReport(out io.Writer, output string, report storageIndexReport) error {
	if output == "json" {
		return writeJSON(out, report, nil)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES\t%d\nPROFILE\t%s\nGENERATION\t%d\nSNAPSHOT\t%s\nFILES\t%d\nPATH BYTES\t%d\nDATA RECORD\t%s\nDESCRIPTOR RECORD\t%s\n",
		terminalSafe(report.Outcome), terminalSafe(report.Effect), report.WritesPerformed, terminalSafe(report.ProfileID), report.Generation, terminalSafe(report.SnapshotID), report.Scan.Stats.FilesEmitted, report.Scan.Stats.EmittedPathBytes,
		terminalSafe(report.DataRecord.ID.String()), terminalSafe(report.DescriptorRecord.ID.String()))
	if len(report.StopReasons) > 0 {
		fmt.Fprintf(w, "STOP REASONS\t%s\n", terminalSafe(strings.Join(report.StopReasons, ",")))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(out, "warning: %s\n", terminalSafe(warning))
	}
	return nil
}

func writeStorageIndexInspectReport(out io.Writer, output string, report storageIndexInspectReport) error {
	if output == "json" {
		return writeJSON(out, report, nil)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES\t%d\nPROFILE\t%s\nGENERATION\t%d\nSNAPSHOT\t%s\nDESCRIPTOR RECORD\t%s\nDATA RECORD\t%s\nFILES\t%d\nPATH BYTES\t%d\nOBSERVED START\t%s\nOBSERVED END\t%s\nFRESHNESS\t%s\n",
		terminalSafe(report.Outcome), terminalSafe(report.Effect), report.WritesPerformed, terminalSafe(report.Descriptor.ProfileID), report.Descriptor.Generation,
		terminalSafe(report.Descriptor.ID), terminalSafe(report.DescriptorRecordID.String()), terminalSafe(report.Descriptor.DataRecordID), report.Descriptor.Files, report.Descriptor.PathBytes,
		report.Descriptor.ObservedAtStart.Format(time.RFC3339), report.Descriptor.ObservedAtEnd.Format(time.RFC3339), terminalSafe(report.Descriptor.LiveFreshness))
	if err := w.Flush(); err != nil {
		return err
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(out, "warning: %s\n", terminalSafe(warning))
	}
	return nil
}
