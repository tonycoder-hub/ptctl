package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

const (
	maxRetainedClientFindings = 20
	maxLedgerJobs             = 25_000
	maxLedgerNameBytes        = 64 << 10
	maxLedgerStateBytes       = 4 << 10
	maxLedgerPathBytes        = 64 << 10
	maxLedgerIdentityItems    = 32
	maxLedgerIdentityItemSize = 256
)

type ClientBracket struct {
	Requested        bool
	Before           *downloader.LedgerSnapshot
	After            *downloader.LedgerSnapshot
	StopReason       string
	RequestsMade     int
	FileLayoutMode   string
	FileLimits       downloader.JobFileLedgerLimits
	FileAttempted    bool
	FileRequestsMade int
	FilesBefore      *downloader.JobFileLedgerSnapshot
	FilesAfter       *downloader.JobFileLedgerSnapshot
	FileStopReason   string
}

type BuildInput struct {
	Meta              *metafile.MetaInfo
	Discovery         seed.DiscoveryResult
	VerifiedSource    *metafile.VerifiedSource
	Client            ClientBracket
	SiteRef           *domain.TorrentRef
	PathMapping       *PathMappingOptions
	ShowAbsolutePaths bool
}

type Report struct {
	Effect          []string        `json:"effect"`
	WritesPerformed int             `json:"writes_performed"`
	Outcome         string          `json:"outcome"`
	Assurance       string          `json:"assurance"`
	Scope           ReportScope     `json:"scope"`
	Ledgers         ReportLedgers   `json:"ledgers"`
	Relations       []Relation      `json:"relations"`
	Blockers        []ReportFinding `json:"blockers"`
	Warnings        []string        `json:"warnings"`
}

type ReportScope struct {
	MetafileVariantID    string `json:"metafile_variant_id"`
	SiteRequested        bool   `json:"site_requested"`
	ClientRequested      bool   `json:"client_requested"`
	PathMappingRequested bool   `json:"path_mapping_requested"`
	PathMappingID        string `json:"path_mapping_id,omitempty"`
	ClientPathSemantics  string `json:"client_path_semantics"`
	ClientFileLayoutMode string `json:"client_file_layout_mode"`
	AbsolutePathsShown   bool   `json:"absolute_paths_shown"`
}

type ReportLedgers struct {
	Site       SiteLedger       `json:"site"`
	Metafile   MetafileLedger   `json:"metafile"`
	Storage    StorageLedger    `json:"storage"`
	Downloader DownloaderLedger `json:"downloader"`
}

type SiteLedger struct {
	Status string             `json:"status"`
	Ref    *domain.TorrentRef `json:"ref,omitempty"`
}

type MetafileLedger struct {
	Status        string `json:"status"`
	VariantID     string `json:"variant_id"`
	Version       string `json:"version"`
	InfoHashV1    string `json:"info_hash_v1,omitempty"`
	InfoHashV2    string `json:"info_hash_v2,omitempty"`
	Private       bool   `json:"private"`
	PhysicalBytes int64  `json:"physical_bytes"`
}

type StorageLedger struct {
	Status            string               `json:"status"`
	ProcessLocalProof bool                 `json:"process_local_proof"`
	SelectedSourceID  string               `json:"selected_source_id,omitempty"`
	SourceSnapshotID  string               `json:"source_snapshot_id,omitempty"`
	Discovery         seed.DiscoveryResult `json:"discovery"`
}

type DownloaderLedger struct {
	Status              string                        `json:"status"`
	Driver              string                        `json:"driver,omitempty"`
	Capabilities        downloader.LedgerCapabilities `json:"capabilities"`
	ObservedAtStart     *time.Time                    `json:"observed_at_start,omitempty"`
	ObservedAtEnd       *time.Time                    `json:"observed_at_end,omitempty"`
	StabilityAssurance  string                        `json:"stability_assurance"`
	RequestsMade        int                           `json:"requests_made"`
	JobsExaminedBefore  int                           `json:"jobs_examined_before"`
	JobsExaminedAfter   int                           `json:"jobs_examined_after"`
	IdentityUnavailable int                           `json:"identity_unavailable"`
	IdentityInvalid     int                           `json:"identity_invalid"`
	Matches             []ClientJobClaim              `json:"matches"`
	FileLayout          ClientFileLayoutLedger        `json:"file_layout"`
	StopReason          string                        `json:"stop_reason,omitempty"`
}

type ClientJobClaim struct {
	ID               string   `json:"id"`
	Relation         string   `json:"relation"`
	InfoHashV1       string   `json:"info_hash_v1,omitempty"`
	InfoHashV2       string   `json:"info_hash_v2,omitempty"`
	IdentityEvidence []string `json:"identity_evidence"`
	State            string   `json:"state"`
	Progress         float64  `json:"progress"`
	SizeBytes        int64    `json:"size_bytes"`
	ContentPathRef   string   `json:"content_path_ref,omitempty"`
	ContentPath      string   `json:"content_path,omitempty"`
}

type Relation struct {
	Kind          string   `json:"kind"`
	Status        string   `json:"status"`
	EvidenceLevel string   `json:"evidence_level"`
	EvidenceBasis []string `json:"evidence_basis"`
	LeftIDs       []string `json:"left_ids"`
	RightIDs      []string `json:"right_ids"`
	BlockerCodes  []string `json:"blocker_codes"`
}

type ReportFinding struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type snapshotAssessment struct {
	status      string
	exact       []downloader.Torrent
	conflicts   []downloader.Torrent
	partial     []downloader.Torrent
	unavailable int
	invalid     int
	signature   string
}

type clientAssessment struct {
	ledger        DownloaderLedger
	relation      Relation
	job           *downloader.Torrent
	active        bool
	contentStable bool
}

// Build creates a read-only reconciliation report. VerifiedSource must be the
// opaque value returned by the same Discovery invocation, and Client.Before
// and Client.After must bracket that invocation. A JSON round-trip
// intentionally loses the storage proof and therefore cannot produce a
// verified relation.
func Build(input BuildInput) (Report, error) {
	if input.Meta == nil {
		return Report{}, fmt.Errorf("metafile is nil")
	}
	meta := input.Meta
	fileLayoutMode, err := normalizeClientFileLayoutMode(input.Client.Requested, input.Client.FileLayoutMode)
	if err != nil {
		return Report{}, err
	}
	clientWindows := false
	pathSemantics := "not_requested"
	pathMappingIDValue := ""
	if input.PathMapping != nil {
		clientWindows = input.PathMapping.ClientWindows
		pathSemantics = "posix_exact"
		if clientWindows {
			pathSemantics = "windows_exact"
		}
		pathMappingIDValue = pathMappingID(*input.PathMapping)
	}
	report := Report{
		Effect:          []string{"read_metafile", "read_storage_metadata", "read_storage_content"},
		WritesPerformed: 0,
		Outcome:         "partial",
		Assurance:       "axis_separated_non_atomic",
		Scope: ReportScope{
			MetafileVariantID:    meta.MetafileVariantID,
			SiteRequested:        input.SiteRef != nil,
			ClientRequested:      input.Client.Requested,
			PathMappingRequested: input.PathMapping != nil,
			PathMappingID:        pathMappingIDValue,
			ClientPathSemantics:  pathSemantics,
			ClientFileLayoutMode: fileLayoutMode,
			AbsolutePathsShown:   input.ShowAbsolutePaths,
		},
		Relations: []Relation{},
		Blockers:  []ReportFinding{},
		Warnings:  []string{},
	}
	if input.Client.Requested {
		report.Effect = append(report.Effect, "read_downloader_state")
	}
	if input.Client.FileAttempted {
		report.Effect = append(report.Effect, "read_downloader_file_layout")
	}
	report.Ledgers.Metafile = MetafileLedger{
		Status: "observed", VariantID: meta.MetafileVariantID, Version: meta.Version,
		InfoHashV1: meta.InfoHashV1, InfoHashV2: meta.InfoHashV2, Private: meta.Private,
		PhysicalBytes: physicalBytes(meta),
	}

	siteRelation := newRelation("site_metafile")
	if input.SiteRef == nil {
		report.Ledgers.Site = SiteLedger{Status: "not_supplied"}
		siteRelation.Status = "not_supplied"
	} else {
		ref := *input.SiteRef
		report.Ledgers.Site = SiteLedger{Status: "declared_unbound", Ref: &ref}
		siteRelation.Status = "declared_unbound"
		siteRelation.EvidenceLevel = "declared"
		siteRelation.EvidenceBasis = append(siteRelation.EvidenceBasis, "user_supplied_site_reference")
		siteRelation.LeftIDs = append(siteRelation.LeftIDs, ref.SiteID+"/"+ref.RemoteID)
		siteRelation.RightIDs = append(siteRelation.RightIDs, meta.MetafileVariantID)
		report.Warnings = append(report.Warnings, "the site reference is user-declared and is not bound to this exact metafile variant")
	}
	storageRelation := newRelation("storage_content_proof")
	storageRelation.Status = input.Discovery.SourceOutcome
	storageRelation.LeftIDs = append(storageRelation.LeftIDs, meta.MetafileVariantID)
	storageLedger := StorageLedger{Status: input.Discovery.SourceOutcome, Discovery: sanitizedDiscovery(input.Discovery, input.ShowAbsolutePaths)}
	if input.Discovery.SourceOutcome == "verified_unique" {
		storageLedger.SelectedSourceID = input.Discovery.Selection.SelectedID
		storageRelation.RightIDs = append(storageRelation.RightIDs, input.Discovery.Selection.SelectedID)
		retainedSource, retained := input.Discovery.VerifiedSource(meta)
		if retained && retainedSource == input.VerifiedSource {
			verification := input.VerifiedSource.Result()
			if verification.Verified {
				storageLedger.ProcessLocalProof = true
				storageLedger.SourceSnapshotID = verification.SourceSnapshotID
				storageRelation.EvidenceLevel = "cryptographic"
				storageRelation.EvidenceBasis = append(storageRelation.EvidenceBasis, verification.Evidence, verification.StabilityAssurance)
			} else {
				storageRelation.Status = "incomplete"
			}
		} else {
			storageRelation.Status = "incomplete"
			storageLedger.Status = "incomplete"
			storageRelation.BlockerCodes = append(storageRelation.BlockerCodes, "storage.process_local_proof_missing")
			report.Blockers = append(report.Blockers, ReportFinding{Code: "storage.process_local_proof_missing", Message: "the unique discovery result is not backed by a same-invocation process-local proof"})
		}
	} else {
		storageRelation.EvidenceLevel = input.Discovery.BestEvidence
		for _, blocker := range input.Discovery.Blockers {
			storageRelation.BlockerCodes = append(storageRelation.BlockerCodes, blocker.Code)
			report.Blockers = append(report.Blockers, ReportFinding{Code: blocker.Code, Message: blocker.Message})
		}
	}
	report.Ledgers.Storage = storageLedger

	client := assessClientBracket(meta, input.Client, input.ShowAbsolutePaths, clientWindows)
	fileLayout := assessClientFileLayout(meta, client.job, input.Client, clientWindows, input.ShowAbsolutePaths)
	client.ledger.FileLayout = fileLayout.ledger
	report.Ledgers.Downloader = client.ledger
	variantRelation := newRelation("metafile_variant_relation")
	variantRelation.LeftIDs = append(variantRelation.LeftIDs, meta.MetafileVariantID)
	if !input.Client.Requested {
		variantRelation.Status = "not_requested"
	} else {
		variantRelation.Status = "unobservable"
		variantRelation.BlockerCodes = append(variantRelation.BlockerCodes, "client.metafile_variant_unobservable")
		variantRelation.EvidenceBasis = append(variantRelation.EvidenceBasis, "downloader_does_not_expose_raw_metafile_bytes")
		report.Warnings = append(report.Warnings, "matching infohash claims cannot prove that the downloader holds the same private metafile variant")
	}
	pathRelation := newRelation("verified_source_vs_job_path")
	pathRelation.LeftIDs = append(pathRelation.LeftIDs, input.Discovery.Selection.SelectedID)
	switch {
	case !input.Client.Requested:
		pathRelation.Status = "not_requested"
	case input.PathMapping == nil:
		pathRelation.Status = "mapping_not_requested"
		pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.mapping_not_requested")
	case storageRelation.Status != "verified_unique" || !storageLedger.ProcessLocalProof:
		pathRelation.Status = relationDependencyStatus(storageRelation.Status)
		pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.storage_proof_unavailable")
	case client.relation.Status != "exact_unique" || client.job == nil:
		pathRelation.Status = relationDependencyStatus(client.relation.Status)
		pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.client_identity_unavailable")
	case !client.ledger.Capabilities.ContentPath:
		pathRelation.Status = "unsupported"
		pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.client_content_path_capability_missing")
	case !client.contentStable:
		pathRelation.Status = "client_content_unsettled"
		pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.client_content_unsettled")
	case !meta.MultiFile && fileLayout.incomplete:
		pathRelation.Status = "incomplete"
		pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.client_file_snapshot_incomplete")
	case meta.MultiFile:
		switch {
		case fileLayout.ledger.Status == "not_requested":
			pathRelation.Status = "client_file_layout_not_requested"
			pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.client_file_layout_not_requested")
		case fileLayout.ledger.Status == "unsupported":
			pathRelation.Status = "client_file_layout_unobservable"
			pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.client_file_layout_unobservable")
		case client.job.SizeBytes != physicalBytes(meta):
			pathRelation.Status = "client_size_conflict"
			pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.client_size_conflict")
		case fileLayout.ledger.Status == "not_attempted":
			pathRelation.Status = "incomplete"
			pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.client_file_snapshot_incomplete")
		case fileLayout.conflict:
			pathRelation.Status = "client_file_layout_conflict"
			pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.client_file_layout_conflict")
		case fileLayout.ledger.Status == "incomplete" || fileLayout.ledger.Status == "unstable" || fileLayout.incomplete:
			pathRelation.Status = "incomplete"
			pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.client_file_snapshot_incomplete")
			if fileLayout.ledger.Status == "unstable" {
				pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.client_file_snapshot_unstable")
			}
		case fileLayout.unselected:
			pathRelation.Status = "client_files_unselected"
			pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.client_files_unselected")
		case fileLayout.unfinished:
			pathRelation.Status = "client_files_incomplete"
			pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.client_files_incomplete")
		case !fileLayout.stable || !fileLayout.manifestOK:
			pathRelation.Status = "incomplete"
			pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.client_file_snapshot_incomplete")
		default:
			remainingFindings := maxRetainedClientFileFindings - len(fileLayout.ledger.Findings)
			expectedPaths, mismatches, mismatchOverflow, compareErr := compareVerifiedSourceFilePaths(meta, input.VerifiedSource, *input.PathMapping, fileLayout.paths, input.ShowAbsolutePaths, remainingFindings)
			if compareErr != nil {
				pathRelation.Status = "incomplete"
				pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.source_mapping_incomplete")
			} else {
				for _, finding := range mismatches {
					fileLayout.addFinding(finding)
				}
				fileLayout.ledger.FindingOverflow += mismatchOverflow
				pathRelation.EvidenceLevel = "lexical"
				pathRelation.EvidenceBasis = append(pathRelation.EvidenceBasis, "qbittorrent_effective_file_path_claims", "qbittorrent_selection_claims", "bracketed_file_layout", "invocation_scoped_namespace_mapping", "lexical_comparison_only")
				pathRelation.LeftIDs = []string{clientPathSetID("verified-source", expectedPaths)}
				pathRelation.RightIDs = []string{fileLayout.snapshotID}
				if len(mismatches) == 0 && mismatchOverflow == 0 {
					pathRelation.Status = "same_location"
				} else {
					pathRelation.Status = "different_location"
					pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.verified_source_differs_from_job")
					report.Blockers = append(report.Blockers, ReportFinding{Code: "path.verified_source_differs_from_job", Message: "verified reusable bytes are not at the downloader's declared effective file paths"})
				}
			}
		}
		client.ledger.FileLayout = fileLayout.ledger
		report.Ledgers.Downloader = client.ledger
	case client.job.SizeBytes != physicalBytes(meta):
		pathRelation.Status = "client_size_conflict"
		pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.client_size_conflict")
	default:
		expected, layout, err := expectedClientContentPath(meta, input.VerifiedSource, *input.PathMapping)
		if layout == "scattered_set" {
			pathRelation.Status = "not_comparable"
			pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.verified_source_scattered")
			report.Blockers = append(report.Blockers, ReportFinding{Code: "path.verified_source_scattered", Message: "verified reusable bytes are scattered and do not describe one downloader content root"})
		} else if err != nil {
			pathRelation.Status = "incomplete"
			pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.source_mapping_incomplete")
		} else {
			claimed, pathErr := parseClientPath(client.job.ContentPath, clientWindows)
			if pathErr != nil {
				pathRelation.Status = "incomplete"
				pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.client_content_path_invalid")
			} else {
				pathRelation.EvidenceLevel = "lexical"
				pathRelation.EvidenceBasis = append(pathRelation.EvidenceBasis, "invocation_scoped_namespace_mapping", "qbittorrent_content_path_claim", "lexical_comparison_only")
				pathRelation.LeftIDs = []string{expected.public(input.ShowAbsolutePaths)}
				pathRelation.RightIDs = []string{claimed.public(input.ShowAbsolutePaths)}
				if expected.equal(claimed) {
					pathRelation.Status = "same_location"
				} else {
					pathRelation.Status = "different_location"
					pathRelation.BlockerCodes = append(pathRelation.BlockerCodes, "path.verified_source_differs_from_job")
					report.Blockers = append(report.Blockers, ReportFinding{Code: "path.verified_source_differs_from_job", Message: "verified reusable bytes are not at the downloader's declared content path"})
				}
			}
		}
	}
	report.Relations = []Relation{siteRelation, variantRelation, client.relation, storageRelation, pathRelation}

	for _, code := range client.relation.BlockerCodes {
		report.Blockers = append(report.Blockers, findingForClientCode(code))
	}
	for _, code := range pathRelation.BlockerCodes {
		report.Blockers = append(report.Blockers, findingForPathCode(code))
	}
	if client.active {
		report.Warnings = append(report.Warnings, "the matching downloader job is active; lexical path agreement does not prove which bytes the client is currently reading")
	}
	report.Outcome = overallOutcome(storageRelation.Status, storageLedger.ProcessLocalProof, client.relation.Status, pathRelation.Status, input.Client.Requested)
	if report.Outcome == "consistent" {
		report.Assurance = "local_content_proof_and_bracketed_typed_client_identity_with_lexical_path_agreement"
		if meta.MultiFile {
			report.Assurance = "local_content_proof_and_bracketed_typed_client_identity_with_bracketed_per_file_lexical_path_agreement"
		}
	}
	report.Blockers = stableFindings(report.Blockers)
	report.Warnings = stableStrings(report.Warnings)
	for i := range report.Relations {
		report.Relations[i].BlockerCodes = stableStrings(report.Relations[i].BlockerCodes)
	}
	return report, nil
}

func assessClientBracket(meta *metafile.MetaInfo, bracket ClientBracket, showAbsolute, windows bool) clientAssessment {
	result := clientAssessment{
		ledger:   DownloaderLedger{Status: "not_requested", StabilityAssurance: "not_requested", RequestsMade: bracket.RequestsMade, Matches: []ClientJobClaim{}},
		relation: newRelation("client_infohash_relation"),
	}
	if !bracket.Requested {
		result.relation.Status = "not_requested"
		return result
	}
	result.ledger.Status = "incomplete"
	result.ledger.StabilityAssurance = "bracketed_non_atomic"
	if bracket.RequestsMade < 0 || bracket.FileRequestsMade < 0 || bracket.FileRequestsMade > 2 || !bracket.FileAttempted && bracket.FileRequestsMade != 0 {
		result.ledger.StopReason = "client_snapshot_incomplete"
		result.relation.Status = "incomplete"
		result.relation.BlockerCodes = append(result.relation.BlockerCodes, "client.snapshot_incomplete")
		return result
	}
	if bracket.StopReason != "" || bracket.Before == nil || bracket.After == nil {
		result.ledger.StopReason = safeStopReason(bracket.StopReason)
		if result.ledger.StopReason == "" {
			result.ledger.StopReason = "client_snapshot_incomplete"
		}
		result.relation.Status = "incomplete"
		result.relation.BlockerCodes = append(result.relation.BlockerCodes, "client.snapshot_incomplete")
		return result
	}
	before, after := bracket.Before, bracket.After
	if bracket.RequestsMade != 3+bracket.FileRequestsMade {
		result.ledger.StopReason = "client_snapshot_incomplete"
		result.relation.Status = "incomplete"
		result.relation.BlockerCodes = append(result.relation.BlockerCodes, "client.snapshot_incomplete")
		return result
	}
	if !validLedgerBracket(*before, *after) {
		result.ledger.StopReason = "client_snapshot_incomplete"
		result.relation.Status = "incomplete"
		result.relation.BlockerCodes = append(result.relation.BlockerCodes, "client.snapshot_incomplete")
		return result
	}
	result.ledger.Driver = before.Driver
	result.ledger.Capabilities = before.Capabilities
	start, end := before.ObservedAtStart, after.ObservedAtEnd
	result.ledger.ObservedAtStart, result.ledger.ObservedAtEnd = &start, &end
	result.ledger.JobsExaminedBefore, result.ledger.JobsExaminedAfter = len(before.Jobs), len(after.Jobs)
	if !before.Capabilities.TypedInfoHashes {
		result.ledger.Status = "unsupported"
		result.ledger.StopReason = "typed_infohash_capability_missing"
		result.relation.Status = "incomplete"
		result.relation.BlockerCodes = append(result.relation.BlockerCodes, "client.typed_infohash_capability_missing")
		return result
	}
	first, second := assessSnapshot(meta, *before), assessSnapshot(meta, *after)
	result.ledger.IdentityUnavailable = second.unavailable
	result.ledger.IdentityInvalid = second.invalid
	result.ledger.Matches = publicClientClaims(second, showAbsolute, windows)
	if first.status != second.status || first.signature != second.signature {
		result.ledger.Status = "unstable"
		result.ledger.StopReason = "client_identity_changed_during_storage_proof"
		result.relation.Status = "incomplete"
		result.relation.BlockerCodes = append(result.relation.BlockerCodes, "client.snapshot_unstable")
		return result
	}
	result.ledger.Status = "observed_stable"
	result.relation.Status = second.status
	result.relation.EvidenceLevel = "client_claim"
	result.relation.EvidenceBasis = append(result.relation.EvidenceBasis, "qbittorrent_magnet_uri_xt", "bracketed_before_and_after_storage_proof", "non_atomic_observation")
	for _, item := range second.exact {
		result.relation.RightIDs = append(result.relation.RightIDs, jobID(item.Hash))
	}
	result.relation.LeftIDs = appendHashIDs(result.relation.LeftIDs, meta)
	switch second.status {
	case "exact_unique":
		job := second.exact[0]
		result.job = &job
		result.active = activeState(job.State)
		result.contentStable = stableClientContentClaim(job)
	case "ambiguous":
		result.relation.BlockerCodes = append(result.relation.BlockerCodes, "client.multiple_exact_jobs")
	case "conflict":
		result.relation.BlockerCodes = append(result.relation.BlockerCodes, "client.typed_infohash_conflict")
	case "incomplete":
		result.relation.BlockerCodes = append(result.relation.BlockerCodes, "client.typed_identity_incomplete")
	case "absent":
		result.relation.BlockerCodes = append(result.relation.BlockerCodes, "client.exact_job_absent")
	}
	return result
}

func validLedgerBracket(before, after downloader.LedgerSnapshot) bool {
	if !before.Complete || !after.Complete || before.Driver != "qbittorrent" || after.Driver != before.Driver || before.Capabilities != after.Capabilities {
		return false
	}
	if before.ObservedAtStart.IsZero() || before.ObservedAtEnd.IsZero() || after.ObservedAtStart.IsZero() || after.ObservedAtEnd.IsZero() ||
		before.ObservedAtEnd.Before(before.ObservedAtStart) || after.ObservedAtStart.Before(before.ObservedAtEnd) || after.ObservedAtEnd.Before(after.ObservedAtStart) {
		return false
	}
	return validLedgerJobs(before.Jobs) && validLedgerJobs(after.Jobs)
}

func validLedgerJobs(jobs []downloader.Torrent) bool {
	if len(jobs) > maxLedgerJobs {
		return false
	}
	seen := make(map[string]struct{}, len(jobs))
	for _, job := range jobs {
		if !validOpaqueJobKey(job.Hash) || job.SizeBytes < 0 || job.Downloaded < 0 || job.Uploaded < 0 ||
			math.IsNaN(job.Progress) || math.IsInf(job.Progress, 0) || job.Progress < 0 || job.Progress > 1 ||
			!validTypedHash(job.InfoHashV1, 40) || !validTypedHash(job.InfoHashV2, 64) ||
			len(job.Name) > maxLedgerNameBytes || len(job.State) > maxLedgerStateBytes ||
			len(job.SavePath) > maxLedgerPathBytes || len(job.ContentPath) > maxLedgerPathBytes ||
			!validLedgerIdentityItems(job.IdentityEvidence) || !validLedgerIdentityItems(job.IdentityIssues) {
			return false
		}
		if _, exists := seen[job.Hash]; exists {
			return false
		}
		seen[job.Hash] = struct{}{}
		switch job.IdentityStatus {
		case downloader.IdentityStatusValid:
			if job.InfoHashV1 == "" && job.InfoHashV2 == "" {
				return false
			}
		case downloader.IdentityStatusUnavailable, downloader.IdentityStatusInvalid:
			if job.InfoHashV1 != "" || job.InfoHashV2 != "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validLedgerIdentityItems(values []string) bool {
	if len(values) > maxLedgerIdentityItems {
		return false
	}
	for _, value := range values {
		if value == "" || len(value) > maxLedgerIdentityItemSize {
			return false
		}
	}
	return true
}

func validOpaqueJobKey(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 0x21 || value[index] > 0x7e {
			return false
		}
	}
	return true
}

func validTypedHash(value string, length int) bool {
	if value == "" {
		return true
	}
	if len(value) != length {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func assessSnapshot(meta *metafile.MetaInfo, snapshot downloader.LedgerSnapshot) snapshotAssessment {
	result := snapshotAssessment{}
	for _, job := range snapshot.Jobs {
		switch string(job.IdentityStatus) {
		case "unavailable":
			result.unavailable++
			continue
		case "invalid":
			result.invalid++
			continue
		case "valid":
		default:
			result.invalid++
			continue
		}
		switch classifyJob(meta, job) {
		case "exact":
			result.exact = append(result.exact, job)
		case "conflict":
			result.conflicts = append(result.conflicts, job)
		case "incomplete":
			result.partial = append(result.partial, job)
		}
	}
	sortJobs(result.exact)
	sortJobs(result.conflicts)
	sortJobs(result.partial)
	switch {
	case len(result.exact) >= 2:
		result.status = "ambiguous"
	case len(result.conflicts) > 0:
		result.status = "conflict"
	case len(result.partial) > 0 || result.unavailable > 0 || result.invalid > 0:
		result.status = "incomplete"
	case len(result.exact) == 1:
		result.status = "exact_unique"
	default:
		result.status = "absent"
	}
	result.signature = assessmentSignature(result)
	return result
}

func classifyJob(meta *metafile.MetaInfo, job downloader.Torrent) string {
	hasV1, hasV2 := meta.InfoHashV1 != "", meta.InfoHashV2 != ""
	v1Match := hasV1 && strings.EqualFold(job.InfoHashV1, meta.InfoHashV1)
	v2Match := hasV2 && strings.EqualFold(job.InfoHashV2, meta.InfoHashV2)
	switch {
	case hasV1 && !hasV2:
		if !v1Match {
			return "unrelated"
		}
		if job.InfoHashV2 != "" {
			return "conflict"
		}
		return "exact"
	case hasV2 && !hasV1:
		if !v2Match {
			return "unrelated"
		}
		if job.InfoHashV1 != "" {
			return "conflict"
		}
		return "exact"
	case hasV1 && hasV2:
		if v1Match && v2Match {
			return "exact"
		}
		if v1Match || v2Match {
			if job.InfoHashV1 == "" || job.InfoHashV2 == "" {
				return "incomplete"
			}
			return "conflict"
		}
	}
	return "unrelated"
}

func assessmentSignature(value snapshotAssessment) string {
	hasher := sha256.New()
	_, _ = io.WriteString(hasher, strings.Join([]string{value.status, fmt.Sprint(value.unavailable), fmt.Sprint(value.invalid)}, "\x00"))
	appendJobs := func(kind string, jobs []downloader.Torrent) {
		for _, job := range jobs {
			_, _ = io.WriteString(hasher, "\n"+strings.Join([]string{kind, job.Hash, job.InfoHashV1, job.InfoHashV2, job.ContentPath, job.SavePath, fmt.Sprint(job.SizeBytes), job.State, fmt.Sprint(job.Progress), fmt.Sprint(job.Downloaded)}, "\x00"))
		}
	}
	appendJobs("exact", value.exact)
	appendJobs("conflict", value.conflicts)
	appendJobs("partial", value.partial)
	return hex.EncodeToString(hasher.Sum(nil))
}

func sortJobs(items []downloader.Torrent) {
	sort.Slice(items, func(i, j int) bool {
		left, right := items[i], items[j]
		if left.Hash != right.Hash {
			return left.Hash < right.Hash
		}
		if left.InfoHashV1 != right.InfoHashV1 {
			return left.InfoHashV1 < right.InfoHashV1
		}
		if left.InfoHashV2 != right.InfoHashV2 {
			return left.InfoHashV2 < right.InfoHashV2
		}
		if left.ContentPath != right.ContentPath {
			return left.ContentPath < right.ContentPath
		}
		if left.SavePath != right.SavePath {
			return left.SavePath < right.SavePath
		}
		if left.SizeBytes != right.SizeBytes {
			return left.SizeBytes < right.SizeBytes
		}
		if left.State != right.State {
			return left.State < right.State
		}
		if left.Progress != right.Progress {
			return left.Progress < right.Progress
		}
		return left.Downloaded < right.Downloaded
	})
}

func publicClientClaims(value snapshotAssessment, showAbsolute, windows bool) []ClientJobClaim {
	claims := make([]ClientJobClaim, 0, min(maxRetainedClientFindings, len(value.exact)+len(value.conflicts)+len(value.partial)))
	appendClaims := func(relation string, jobs []downloader.Torrent) {
		for _, job := range jobs {
			if len(claims) >= maxRetainedClientFindings {
				return
			}
			claim := ClientJobClaim{
				ID: jobID(job.Hash), Relation: relation, InfoHashV1: job.InfoHashV1, InfoHashV2: job.InfoHashV2,
				IdentityEvidence: safeIdentityEvidence(job.IdentityEvidence), State: safeClientState(job.State), Progress: job.Progress, SizeBytes: job.SizeBytes,
			}
			if parsed, err := parseClientPath(job.ContentPath, windows); err == nil {
				claim.ContentPathRef = parsed.public(false)
				if showAbsolute {
					claim.ContentPath = parsed.public(true)
				}
			}
			claims = append(claims, claim)
		}
	}
	appendClaims("exact", value.exact)
	appendClaims("conflict", value.conflicts)
	appendClaims("incomplete", value.partial)
	sort.Slice(claims, func(i, j int) bool { return claims[i].ID < claims[j].ID })
	return claims
}

func overallOutcome(storageStatus string, processProof bool, clientStatus, pathStatus string, clientRequested bool) string {
	if storageStatus == "verified_ambiguous" || clientStatus == "ambiguous" {
		return "ambiguous"
	}
	if clientStatus == "conflict" || pathStatus == "client_size_conflict" || pathStatus == "client_file_layout_conflict" {
		return "conflict"
	}
	if storageStatus == "incomplete" || storageStatus == "verified_unique" && !processProof || clientRequested && clientStatus == "incomplete" || pathStatus == "incomplete" {
		return "incomplete"
	}
	if processProof && clientStatus == "exact_unique" && pathStatus == "same_location" {
		return "consistent"
	}
	return "partial"
}

func relationDependencyStatus(status string) string {
	switch status {
	case "verified_ambiguous", "ambiguous":
		return "ambiguous"
	case "incomplete", "conflict":
		return status
	default:
		return "not_comparable"
	}
}

func newRelation(kind string) Relation {
	return Relation{Kind: kind, Status: "unknown", EvidenceLevel: "none", EvidenceBasis: []string{}, LeftIDs: []string{}, RightIDs: []string{}, BlockerCodes: []string{}}
}

func appendHashIDs(items []string, meta *metafile.MetaInfo) []string {
	if meta.InfoHashV1 != "" {
		items = append(items, "btih:"+meta.InfoHashV1)
	}
	if meta.InfoHashV2 != "" {
		items = append(items, "btmh:1220"+meta.InfoHashV2)
	}
	return items
}

func jobID(native string) string {
	digest := sha256.Sum256([]byte("ptctl-downloader-job-v1\x00" + native))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func pathMappingID(value PathMappingOptions) string {
	style := "posix_exact"
	if value.ClientWindows {
		style = "windows_exact"
	}
	hostRoot, clientRoot := value.HostRoot, value.ClientRoot
	if normalized, err := storage.MapHostToClient(value.HostRoot, value.HostRoot, value.ClientRoot, value.ClientWindows); err == nil {
		hostRoot, clientRoot = normalized.HostRoot, normalized.ClientRoot
	}
	if parsed, err := parseClientPath(clientRoot, value.ClientWindows); err == nil {
		clientRoot = parsed.canonical()
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{"ptctl-path-mapping-v1", style, hostRoot, clientRoot}, "\x00")))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func physicalBytes(meta *metafile.MetaInfo) int64 {
	var total int64
	for _, file := range meta.Files {
		if !strings.Contains(file.Attribute, "p") {
			total += file.Length
		}
	}
	return total
}

func sanitizedDiscovery(value seed.DiscoveryResult, showAbsolute bool) seed.DiscoveryResult {
	value = value.PublicReportCopy()
	value.AbsolutePathsShown = showAbsolute
	value.Scan.SearchRoots = append([]seed.DiscoveryRoot{}, value.Scan.SearchRoots...)
	if !showAbsolute {
		for i := range value.Scan.SearchRoots {
			value.Scan.SearchRoots[i].InputPath = ""
			value.Scan.SearchRoots[i].ResolvedPath = ""
		}
	}
	value.Files = append([]seed.DiscoveryFile{}, value.Files...)
	for i := range value.Files {
		value.Files[i].Candidates = append([]seed.DiscoveryCandidate{}, value.Files[i].Candidates...)
		if !showAbsolute {
			for j := range value.Files[i].Candidates {
				value.Files[i].Candidates[j].AbsolutePath = ""
			}
		}
	}
	value.Matches = append([]seed.DiscoveryMatch{}, value.Matches...)
	for i := range value.Matches {
		value.Matches[i].Bindings = append([]seed.DiscoveryBinding{}, value.Matches[i].Bindings...)
		if !showAbsolute {
			for j := range value.Matches[i].Bindings {
				value.Matches[i].Bindings[j].AbsolutePath = ""
				value.Matches[i].Bindings[j].ClientPath = ""
			}
		}
	}
	if value.Plan != nil {
		plan := *value.Plan
		plan.Operations = append([]seed.DiscoveryPlanOperation{}, value.Plan.Operations...)
		if !showAbsolute {
			for i := range plan.Operations {
				if plan.Operations[i].Source != "" {
					plan.Operations[i].Source = fmt.Sprintf("verified-source:%d", plan.Operations[i].ManifestIndex)
				}
				plan.Operations[i].Target = plan.Operations[i].TorrentPath
				plan.Operations[i].ClientTarget = ""
			}
		}
		value.Plan = &plan
	}
	return value
}

func activeState(state string) bool {
	switch state {
	case "uploading", "forcedUP", "allocating", "downloading", "metaDL", "forcedMetaDL", "pausedDL", "queuedDL", "stalledDL", "checkingDL", "forcedDL", "stoppedDL",
		"checkingResumeData", "moving", "checkingUP":
		return true
	default:
		return false
	}
}

func stableClientContentClaim(job downloader.Torrent) bool {
	if job.Progress != 1 {
		return false
	}
	switch job.State {
	case "uploading", "stalledUP", "pausedUP", "queuedUP", "forcedUP", "stoppedUP":
		return true
	default:
		return false
	}
}

func safeClientState(value string) string {
	switch value {
	case "error", "missingFiles", "uploading", "pausedUP", "queuedUP", "stalledUP", "checkingUP", "forcedUP", "stoppedUP",
		"allocating", "downloading", "metaDL", "forcedMetaDL", "pausedDL", "queuedDL", "stalledDL", "checkingDL", "forcedDL", "stoppedDL",
		"checkingResumeData", "moving", "unknown":
		return value
	default:
		return "unknown"
	}
}

func safeIdentityEvidence(values []string) []string {
	result := make([]string, 0, 3)
	for _, value := range values {
		switch value {
		case "magnet_xt_btih_hex", "magnet_xt_btih_base32", "magnet_xt_btmh_sha256":
			result = append(result, value)
		}
	}
	return stableStrings(result)
}

func safeStopReason(reason string) string {
	switch reason {
	case "client_session_failed", "client_snapshot_before_failed", "client_snapshot_after_failed", "context_cancelled", "client_snapshot_incomplete":
		return reason
	default:
		if reason != "" {
			return "client_read_failed"
		}
		return ""
	}
}

func findingForClientCode(code string) ReportFinding {
	switch code {
	case "client.multiple_exact_jobs":
		return ReportFinding{Code: code, Message: "multiple downloader jobs claim the exact required typed infohash identity"}
	case "client.typed_infohash_conflict":
		return ReportFinding{Code: code, Message: "a downloader job matches one required infohash family and conflicts on another"}
	case "client.typed_identity_incomplete":
		return ReportFinding{Code: code, Message: "the downloader ledger contains identities that cannot be safely classified"}
	case "client.exact_job_absent":
		return ReportFinding{Code: code, Message: "no downloader job claimed the exact required typed infohash identity"}
	case "client.snapshot_unstable":
		return ReportFinding{Code: code, Message: "identity-critical downloader fields changed while storage proof was running"}
	case "client.typed_infohash_capability_missing":
		return ReportFinding{Code: code, Message: "the downloader session did not declare algorithm-tagged infohash capability"}
	default:
		return ReportFinding{Code: code, Message: "the downloader ledger could not be reconciled completely"}
	}
}

func findingForPathCode(code string) ReportFinding {
	switch code {
	case "path.mapping_not_requested":
		return ReportFinding{Code: code, Message: "host-to-client namespace mapping was not requested, so downloader path alignment was not evaluated"}
	case "path.storage_proof_unavailable":
		return ReportFinding{Code: code, Message: "downloader path alignment requires one uniquely verified storage source"}
	case "path.client_identity_unavailable":
		return ReportFinding{Code: code, Message: "downloader path alignment requires one exact, stable typed-infohash job"}
	case "path.source_mapping_incomplete":
		return ReportFinding{Code: code, Message: "the verified source could not be represented as one safe client content path"}
	case "path.client_content_path_invalid":
		return ReportFinding{Code: code, Message: "the downloader returned an invalid or unsupported content path"}
	case "path.client_content_path_capability_missing":
		return ReportFinding{Code: code, Message: "the downloader did not declare content-path capability"}
	case "path.client_content_unsettled":
		return ReportFinding{Code: code, Message: "the matching downloader job is not in a stable, complete seeding state"}
	case "path.client_size_conflict":
		return ReportFinding{Code: code, Message: "the matching downloader job reports a total size that conflicts with the metafile"}
	case "path.client_file_layout_unobservable":
		return ReportFinding{Code: code, Message: "the downloader cannot safely expose the ordinary multi-file paths, sizes, selection, and completion state required for alignment"}
	case "path.client_file_layout_not_requested":
		return ReportFinding{Code: code, Message: "downloader per-file layout observation was disabled, so multi-file path alignment was not evaluated"}
	case "path.client_file_snapshot_incomplete":
		return ReportFinding{Code: code, Message: "the bounded downloader per-file observations were unavailable, invalid, or incomplete"}
	case "path.client_file_snapshot_unstable":
		return ReportFinding{Code: code, Message: "identity-critical downloader file fields changed while storage proof was running"}
	case "path.client_file_layout_conflict":
		return ReportFinding{Code: code, Message: "the downloader file ledger conflicts with the metafile index or file sizes"}
	case "path.client_files_unselected":
		return ReportFinding{Code: code, Message: "one or more downloader files are explicitly skipped"}
	case "path.client_files_incomplete":
		return ReportFinding{Code: code, Message: "one or more downloader files are not completely available according to the client"}
	case "path.verified_source_scattered":
		return ReportFinding{Code: code, Message: "verified reusable bytes are scattered and do not describe one downloader content root"}
	case "path.verified_source_differs_from_job":
		return ReportFinding{Code: code, Message: "verified reusable bytes are not at the downloader's declared content path"}
	default:
		return ReportFinding{Code: code, Message: "the verified source and downloader content path could not be reconciled"}
	}
}

func stableFindings(items []ReportFinding) []ReportFinding {
	byCode := make(map[string]ReportFinding, len(items))
	for _, item := range items {
		if item.Code != "" {
			byCode[item.Code] = item
		}
	}
	result := make([]ReportFinding, 0, len(byCode))
	for _, item := range byCode {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Code < result[j].Code })
	return result
}

func stableStrings(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	result := make([]string, 0, len(items))
	for _, item := range items {
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	sort.Strings(result)
	return result
}
