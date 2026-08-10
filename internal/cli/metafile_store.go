package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
)

type metafileStoreReport struct {
	kind                  string
	Outcome               string                   `json:"outcome"`
	Effect                string                   `json:"effect"`
	WritesPerformed       int                      `json:"writes_performed"`
	SourceWritesPerformed int                      `json:"source_writes_performed"`
	Store                 metafileStoreSummary     `json:"store"`
	Artifact              *metafileArtifactSummary `json:"artifact,omitempty"`
	Metafile              *storedMetafileSummary   `json:"metafile,omitempty"`
	Assurance             metafileStoreAssurance   `json:"assurance"`
	Limits                metastore.Limits         `json:"limits"`
	Used                  metafileStoreUsage       `json:"used"`
	Paths                 metafileStorePaths       `json:"paths"`
	Blockers              []string                 `json:"blockers"`
	Issues                []string                 `json:"issues"`
	Warnings              []string                 `json:"warnings"`
}

type metafileStoreSummary struct {
	ID                 string `json:"id,omitempty"`
	Format             string `json:"format,omitempty"`
	Privacy            string `json:"privacy,omitempty"`
	CommitAssurance    string `json:"commit_assurance,omitempty"`
	AbsolutePathsShown bool   `json:"absolute_paths_shown"`
}

type metafileArtifactSummary struct {
	ID                 string `json:"id"`
	Status             string `json:"status"`
	MetafileVariantID  string `json:"metafile_variant_id"`
	Version            string `json:"version"`
	InfoHashV1         string `json:"info_hash_v1,omitempty"`
	InfoHashV2         string `json:"info_hash_v2,omitempty"`
	SizeBytes          int64  `json:"size_bytes"`
	ExactOriginalBytes bool   `json:"exact_original_bytes"`
}

type storedMetafileSummary struct {
	Version       string `json:"version"`
	InfoHashV1    string `json:"info_hash_v1,omitempty"`
	InfoHashV2    string `json:"info_hash_v2,omitempty"`
	Private       bool   `json:"private"`
	MultiFile     bool   `json:"multi_file"`
	MetafileBytes int64  `json:"metafile_bytes"`
	TotalLength   int64  `json:"total_length"`
	PieceLength   int64  `json:"piece_length"`
	FileCount     int    `json:"file_count"`
}

type metafileStoreAssurance struct {
	Publication         string `json:"publication"`
	DigestVerified      bool   `json:"digest_verified"`
	StrictlyParsed      bool   `json:"strictly_parsed"`
	AtomicNoClobber     bool   `json:"atomic_no_clobber"`
	DurabilityConfirmed bool   `json:"durability_confirmed"`
	PrivacyEnforced     bool   `json:"privacy_enforced"`
	Encryption          string `json:"encryption"`
}

type metafileStoreUsage struct {
	ArtifactBytes      int64 `json:"artifact_bytes"`
	ArtifactBytesKnown bool  `json:"artifact_bytes_known"`
}

type metafileStorePaths struct {
	Store  string `json:"store,omitempty"`
	Source string `json:"source,omitempty"`
}

func (a *app) metafileCommand(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		a.metafileStoreHelp()
		return nil
	}
	if args[0] != "store" {
		return usageError("unknown metafile subcommand %q", args[0])
	}
	if len(args) == 1 || args[1] == "help" || args[1] == "-h" || args[1] == "--help" {
		a.metafileStoreHelp()
		return nil
	}
	if len(args) == 3 && (args[2] == "-h" || args[2] == "--help") {
		a.metafileStoreHelp()
		return nil
	}
	switch args[1] {
	case "init":
		return a.metafileStoreInit(args[2:])
	case "import":
		return a.metafileStoreImport(args[2:])
	case "inspect":
		return a.metafileStoreInspect(args[2:])
	default:
		return usageError("unknown metafile store subcommand %q", args[1])
	}
}

func (a *app) metafileStoreHelp() {
	fmt.Fprint(a.stdout, `Usage:
  ptctl metafile store init --store DIR [--output table|json] [--show-absolute-paths]
  ptctl metafile store import --store DIR [--output table|json] [--show-absolute-paths] FILE.torrent
  ptctl metafile store inspect --store DIR [--output table|json] [--show-absolute-paths] METAFILE_VARIANT_ID

The store preserves exact private .torrent bytes under a whole-file SHA-256 ID.
Init and import are explicit private app-state writes. Inspect is read-only.
The store enforces owner-only filesystem access and atomic no-clobber publication;
it is not encrypted and does not protect against a privileged administrator.
`)
}

func (a *app) metafileStoreInit(args []string) error {
	fs := newFlagSet("metafile store init")
	output := fs.String("output", "table", "table or json")
	root := fs.String("store", "", "new private metafile store root")
	showAbsolute := fs.Bool("show-absolute-paths", false, "include the absolute store path in output")
	if err := fs.Parse(args); err != nil {
		return usageError("metafile store init: %v", err)
	}
	if fs.NArg() != 0 || *root == "" {
		return usageError("metafile store init requires --store DIR and no positional arguments")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	_, receipt, err := metastore.Init(*root)
	if err != nil {
		if errors.Is(err, metastore.ErrDurabilityUnconfirmed) {
			report := successfulMetafileStoreReport("metafile.store.init", "published_durability_unconfirmed", receipt.Effect, receipt.WritesPerformed, receipt.Store, nil, nil, "published_durability_unconfirmed", metastore.DefaultLimits(), 0, *showAbsolute, *root, "")
			report.Assurance.PrivacyEnforced = false
			report.Blockers = append(report.Blockers, "store.durability_unconfirmed")
			report.Issues = append(report.Issues, "store.durability_unconfirmed")
			if writeErr := a.writeMetafileStoreReport(*output, report); writeErr != nil {
				return writeErr
			}
			return err
		}
		if receipt.WritesPerformed > 0 {
			report := successfulMetafileStoreReport("metafile.store.init", "published_post_commit_failure", receipt.Effect, receipt.WritesPerformed, receipt.Store, nil, nil, "published_post_commit_failure", metastore.DefaultLimits(), 0, *showAbsolute, *root, "")
			report.Assurance.PrivacyEnforced = false
			report.Blockers = append(report.Blockers, "store.post_commit_failure")
			report.Issues = append(report.Issues, "store.post_commit_failure")
			if writeErr := a.writeMetafileStoreReport(*output, report); writeErr != nil {
				return writeErr
			}
			return err
		}
		report := failedMetafileStoreReport("metafile.store.init", "write_private_metafile_store_init", metastore.DefaultLimits(), "store.init_failed", *showAbsolute, *root, "")
		if writeErr := a.writeMetafileStoreReport(*output, report); writeErr != nil {
			return writeErr
		}
		return err
	}
	outcome := "initialized"
	publication := "confirmed_this_invocation"
	if receipt.AlreadyInitialized {
		outcome = "already_initialized"
		publication = "historical_publication_unobservable"
	}
	report := successfulMetafileStoreReport("metafile.store.init", outcome, receipt.Effect, receipt.WritesPerformed, receipt.Store, nil, nil, publication, metastore.DefaultLimits(), 0, *showAbsolute, *root, "")
	return a.writeMetafileStoreReport(*output, report)
}

func (a *app) metafileStoreImport(args []string) error {
	fs := newFlagSet("metafile store import")
	output := fs.String("output", "table", "table or json")
	root := fs.String("store", "", "initialized private metafile store root")
	showAbsolute := fs.Bool("show-absolute-paths", false, "include absolute store and source paths in output")
	if err := fs.Parse(args); err != nil {
		return usageError("metafile store import: %v", err)
	}
	if fs.NArg() != 1 || *root == "" || fs.Arg(0) == "" {
		return usageError("metafile store import requires --store DIR and one FILE.torrent")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	limits := metastore.DefaultLimits()
	store, err := metastore.Open(*root)
	if err != nil {
		report := failedMetafileStoreReport("metafile.store.import", "write_private_metafile_store", limits, "store.open_failed", *showAbsolute, *root, fs.Arg(0))
		if writeErr := a.writeMetafileStoreReport(*output, report); writeErr != nil {
			return writeErr
		}
		return err
	}
	meta, ref, receipt, err := store.ImportFile(context.Background(), fs.Arg(0), limits)
	if err != nil {
		if errors.Is(err, metastore.ErrDurabilityUnconfirmed) {
			artifact := artifactSummary(ref, "published_durability_unconfirmed")
			artifact.ExactOriginalBytes = false
			report := successfulMetafileStoreReport("metafile.store.import", "published_durability_unconfirmed", receipt.Effect, receipt.WritesPerformed, receipt.Store, &artifact, meta, "published_durability_unconfirmed", limits, receipt.BytesConsumed, *showAbsolute, *root, fs.Arg(0))
			report.Assurance.DigestVerified = false
			report.Assurance.PrivacyEnforced = false
			report.Blockers = append(report.Blockers, "store.durability_unconfirmed")
			report.Issues = append(report.Issues, "store.durability_unconfirmed")
			if writeErr := a.writeMetafileStoreReport(*output, report); writeErr != nil {
				return writeErr
			}
			return err
		}
		if receipt.WritesPerformed > 0 && ref.ID != "" {
			status := "published_post_commit_failure"
			outcome := "published_post_commit_failure"
			code := "store.post_commit_failure"
			if errors.Is(err, metastore.ErrCorruptArtifact) {
				status = "published_integrity_failed"
				outcome = "integrity_failed"
				code = "artifact.corrupt"
			}
			artifact := artifactSummary(ref, status)
			artifact.ExactOriginalBytes = false
			report := successfulMetafileStoreReport("metafile.store.import", outcome, receipt.Effect, receipt.WritesPerformed, receipt.Store, &artifact, meta, "published_post_commit_failure", limits, receipt.BytesConsumed, *showAbsolute, *root, fs.Arg(0))
			report.Assurance.DigestVerified = false
			report.Assurance.PrivacyEnforced = false
			report.Blockers = append(report.Blockers, code)
			report.Issues = append(report.Issues, code)
			if writeErr := a.writeMetafileStoreReport(*output, report); writeErr != nil {
				return writeErr
			}
			if errors.Is(err, metastore.ErrCorruptArtifact) {
				return &integrityErr{message: "published metafile failed final digest or parse validation"}
			}
			return err
		}
		issue := "store.import_failed"
		outcome := "blocked"
		if errors.Is(err, metastore.ErrInvalidMetafile) {
			issue = "artifact.invalid_metafile"
			outcome = "integrity_failed"
		} else if errors.Is(err, metastore.ErrCorruptArtifact) {
			issue = "artifact.corrupt"
			outcome = "integrity_failed"
		}
		report := failedMetafileStoreReport("metafile.store.import", "write_private_metafile_store", limits, issue, *showAbsolute, *root, fs.Arg(0))
		report.Outcome = outcome
		report.Store = storeSummary(store.Info(), *showAbsolute)
		report.Used.ArtifactBytes = receipt.BytesConsumed
		if writeErr := a.writeMetafileStoreReport(*output, report); writeErr != nil {
			return writeErr
		}
		if errors.Is(err, metastore.ErrInvalidMetafile) || errors.Is(err, metastore.ErrCorruptArtifact) {
			return &integrityErr{message: "metafile import failed strict validation or found a corrupt stored artifact"}
		}
		return err
	}
	status := "stored"
	outcome := "stored"
	publication := "confirmed_this_invocation"
	if receipt.AlreadyPresent {
		status = "already_present"
		outcome = "already_present"
		publication = "historical_publication_unobservable"
	}
	artifact := artifactSummary(ref, status)
	report := successfulMetafileStoreReport("metafile.store.import", outcome, receipt.Effect, receipt.WritesPerformed, receipt.Store, &artifact, meta, publication, limits, receipt.BytesConsumed, *showAbsolute, *root, fs.Arg(0))
	return a.writeMetafileStoreReport(*output, report)
}

func (a *app) metafileStoreInspect(args []string) error {
	fs := newFlagSet("metafile store inspect")
	output := fs.String("output", "table", "table or json")
	root := fs.String("store", "", "initialized private metafile store root")
	showAbsolute := fs.Bool("show-absolute-paths", false, "include the absolute store path in output")
	if err := fs.Parse(args); err != nil {
		return usageError("metafile store inspect: %v", err)
	}
	if fs.NArg() != 1 || *root == "" {
		return usageError("metafile store inspect requires --store DIR and one METAFILE_VARIANT_ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	id, err := metastore.ParseArtifactID(fs.Arg(0))
	if err != nil {
		return usageError("metafile store inspect: invalid metafile variant ID")
	}
	limits := metastore.DefaultLimits()
	store, err := metastore.Open(*root)
	if err != nil {
		report := failedMetafileStoreReport("metafile.store.inspect", "read_private_metafile_store", limits, "store.open_failed", *showAbsolute, *root, "")
		if writeErr := a.writeMetafileStoreReport(*output, report); writeErr != nil {
			return writeErr
		}
		return err
	}
	meta, ref, err := store.Load(context.Background(), id, limits)
	if err != nil {
		issue := "artifact.read_failed"
		outcome := "blocked"
		if errors.Is(err, metastore.ErrCorruptArtifact) {
			issue = "artifact.corrupt"
			outcome = "integrity_failed"
		}
		report := failedMetafileStoreReport("metafile.store.inspect", "read_private_metafile_store", limits, issue, *showAbsolute, *root, "")
		report.Outcome = outcome
		report.Store = storeSummary(store.Info(), *showAbsolute)
		report.Used.ArtifactBytesKnown = false
		if writeErr := a.writeMetafileStoreReport(*output, report); writeErr != nil {
			return writeErr
		}
		if errors.Is(err, metastore.ErrCorruptArtifact) {
			return &integrityErr{message: "stored metafile failed digest or parse validation"}
		}
		return err
	}
	artifact := artifactSummary(ref, "verified")
	report := successfulMetafileStoreReport("metafile.store.inspect", "verified", "read_private_metafile_store", 0, store.Info(), &artifact, meta, "historical_publication_unobservable", limits, ref.SizeBytes, *showAbsolute, *root, "")
	return a.writeMetafileStoreReport(*output, report)
}

func successfulMetafileStoreReport(kind, outcome, effect string, writes int, info metastore.StoreInfo, artifact *metafileArtifactSummary, meta *metafile.MetaInfo, publication string, limits metastore.Limits, used int64, showAbsolute bool, root, source string) metafileStoreReport {
	atomicNoClobber := publication == "confirmed_this_invocation" || publication == "published_durability_unconfirmed" || publication == "published_post_commit_failure"
	durabilityConfirmed := publication == "confirmed_this_invocation" || publication == "published_post_commit_failure"
	report := metafileStoreReport{
		kind: kind, Outcome: outcome, Effect: effect, WritesPerformed: writes,
		Store: storeSummary(info, showAbsolute), Artifact: artifact,
		Assurance: metafileStoreAssurance{Publication: publication, DigestVerified: artifact != nil, StrictlyParsed: artifact != nil, AtomicNoClobber: atomicNoClobber, DurabilityConfirmed: durabilityConfirmed, PrivacyEnforced: true, Encryption: "none"},
		Limits:    limits, Used: metafileStoreUsage{ArtifactBytes: used, ArtifactBytesKnown: true},
		Blockers: []string{}, Issues: []string{},
		Warnings: []string{"the store is permission-isolated, not encrypted"},
	}
	if meta != nil {
		report.Metafile = sanitizedStoredMetafile(meta)
	}
	if showAbsolute {
		report.Paths.Store = displayedAbsolutePath(root)
		report.Paths.Source = displayedAbsolutePath(source)
	}
	return report
}

func failedMetafileStoreReport(kind, effect string, limits metastore.Limits, code string, showAbsolute bool, root, source string) metafileStoreReport {
	report := metafileStoreReport{
		kind: kind, Outcome: "blocked", Effect: effect,
		Assurance: metafileStoreAssurance{Publication: "not_established", Encryption: "none"},
		Limits:    limits, Used: metafileStoreUsage{ArtifactBytesKnown: true}, Blockers: []string{code}, Issues: []string{},
		Warnings: []string{"the store is permission-isolated, not encrypted"},
		Store:    metafileStoreSummary{AbsolutePathsShown: showAbsolute},
	}
	if strings.HasPrefix(code, "artifact.") {
		report.Issues = append(report.Issues, code)
	}
	if showAbsolute {
		report.Paths.Store = displayedAbsolutePath(root)
		report.Paths.Source = displayedAbsolutePath(source)
	}
	return report
}

func storeSummary(info metastore.StoreInfo, showAbsolute bool) metafileStoreSummary {
	return metafileStoreSummary{ID: info.StoreID, Format: info.Format, Privacy: info.Privacy, CommitAssurance: info.CommitAssurance, AbsolutePathsShown: showAbsolute}
}

func artifactSummary(ref metastore.ArtifactRef, status string) metafileArtifactSummary {
	return metafileArtifactSummary{ID: ref.ID.String(), Status: status, MetafileVariantID: ref.MetafileVariantID, Version: ref.Version, InfoHashV1: ref.InfoHashV1, InfoHashV2: ref.InfoHashV2, SizeBytes: ref.SizeBytes, ExactOriginalBytes: true}
}

func sanitizedStoredMetafile(meta *metafile.MetaInfo) *storedMetafileSummary {
	return &storedMetafileSummary{Version: meta.Version, InfoHashV1: meta.InfoHashV1, InfoHashV2: meta.InfoHashV2, Private: meta.Private, MultiFile: meta.MultiFile, MetafileBytes: meta.MetafileBytes, TotalLength: meta.TotalLength, PieceLength: meta.PieceLength, FileCount: len(meta.Files)}
}

func displayedAbsolutePath(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	return abs
}

func (a *app) writeMetafileStoreReport(output string, report metafileStoreReport) error {
	if output == "json" {
		return writeJSON(a.stdout, report, nil)
	}
	return writeMetafileStoreHuman(a.stdout, report)
}

func writeMetafileStoreHuman(out io.Writer, report metafileStoreReport) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nSOURCE WRITES PERFORMED\t%d\n", terminalSafe(report.Outcome), terminalSafe(report.Effect), report.WritesPerformed, report.SourceWritesPerformed)
	fmt.Fprintf(w, "PUBLICATION ASSURANCE\t%s\nDIGEST VERIFIED\t%t\nSTRICTLY PARSED\t%t\nATOMIC NO-CLOBBER\t%t\nDURABILITY CONFIRMED\t%t\nPRIVACY ENFORCED\t%t\nENCRYPTION\t%s\n", terminalSafe(report.Assurance.Publication), report.Assurance.DigestVerified, report.Assurance.StrictlyParsed, report.Assurance.AtomicNoClobber, report.Assurance.DurabilityConfirmed, report.Assurance.PrivacyEnforced, terminalSafe(report.Assurance.Encryption))
	fmt.Fprintln(w, "\nBLOCKERS")
	if len(report.Blockers) == 0 {
		fmt.Fprintln(w, "-\tnone")
	}
	for _, blocker := range report.Blockers {
		fmt.Fprintf(w, "-\t%s\n", terminalSafe(blocker))
	}
	fmt.Fprintf(w, "\nSTORE ID\t%s\nSTORE FORMAT\t%s\nPRIVACY\t%s\nCOMMIT ASSURANCE\t%s\n", terminalSafe(report.Store.ID), terminalSafe(report.Store.Format), terminalSafe(report.Store.Privacy), terminalSafe(report.Store.CommitAssurance))
	if report.Paths.Store != "" {
		fmt.Fprintf(w, "STORE PATH\t%s\n", terminalSafe(report.Paths.Store))
	}
	if report.Paths.Source != "" {
		fmt.Fprintf(w, "SOURCE PATH\t%s\n", terminalSafe(report.Paths.Source))
	}
	if report.Artifact != nil {
		fmt.Fprintf(w, "\nARTIFACT\t%s\nARTIFACT STATUS\t%s\nMETAFILE VARIANT\t%s\nVERSION\t%s\nSIZE\t%d bytes\n", terminalSafe(report.Artifact.ID), terminalSafe(report.Artifact.Status), terminalSafe(report.Artifact.MetafileVariantID), terminalSafe(report.Artifact.Version), report.Artifact.SizeBytes)
		if report.Artifact.InfoHashV1 != "" {
			fmt.Fprintf(w, "INFOHASH V1\t%s\n", terminalSafe(report.Artifact.InfoHashV1))
		}
		if report.Artifact.InfoHashV2 != "" {
			fmt.Fprintf(w, "INFOHASH V2\t%s\n", terminalSafe(report.Artifact.InfoHashV2))
		}
	}
	if report.Metafile != nil {
		fmt.Fprintf(w, "\nMETAFILE VERSION\t%s\nPRIVATE\t%t\nMULTI-FILE\t%t\nMETAFILE BYTES\t%d\nCONTENT BYTES\t%d\nPIECE LENGTH\t%d\nFILES\t%d\n", terminalSafe(report.Metafile.Version), report.Metafile.Private, report.Metafile.MultiFile, report.Metafile.MetafileBytes, report.Metafile.TotalLength, report.Metafile.PieceLength, report.Metafile.FileCount)
	}
	fmt.Fprintf(w, "\nLIMIT\t%d bytes\n", report.Limits.MaxArtifactBytes)
	if report.Used.ArtifactBytesKnown {
		fmt.Fprintf(w, "USED\t%d bytes\n", report.Used.ArtifactBytes)
	} else {
		fmt.Fprintln(w, "USED\tunknown")
	}
	fmt.Fprintln(w, "\nISSUES")
	if len(report.Issues) == 0 {
		fmt.Fprintln(w, "-\tnone")
	}
	for _, issue := range report.Issues {
		fmt.Fprintf(w, "-\t%s\n", terminalSafe(issue))
	}
	fmt.Fprintln(w, "\nWARNINGS")
	for _, warning := range report.Warnings {
		fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
	}
	return w.Flush()
}
