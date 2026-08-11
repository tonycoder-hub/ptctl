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
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/sitebinding"
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
	SiteBinding       SiteBindingSelection
	SiteDetail        SiteDetailSelection
	MaterializedFinal MaterializedFinalSelection
	ClientActivation  ClientActivationSelection
	ClientRemoval     ClientRemovalSelection
	SourceRetirement  SourceRetirementSelection
	PathMapping       *PathMappingOptions
	ShowAbsolutePaths bool
}

// ClientActivationCompletionProof is implemented only by a process-local
// activation completion authority. Its public view remains historical and
// cannot by itself establish a current downloader job.
type ClientActivationCompletionProof interface {
	ReconciliationCompletion() (ClientActivationCompletion, bool)
}

// ClientActivationCurrentUseProof is a process-local bridge from one terminal
// activation authority and one exact materialized final to the downloader
// snapshots already supplied to this reconciliation. It performs no request.
type ClientActivationCurrentUseProof interface {
	ReconcileCurrentUse(ClientBracket) (ClientActivationCurrentUse, bool)
}

// ClientActivationCurrentAbsenceProof binds the same terminal activation and
// exact final authority to a complete two-snapshot typed queue absence. It
// consumes the existing reconciliation bracket and performs no request.
type ClientActivationCurrentAbsenceProof interface {
	ReconcileCurrentAbsence(ClientBracket) (ClientActivationCurrentAbsence, bool)
}

type ClientActivationSelection struct {
	Requested      bool
	Completion     ClientActivationCompletionProof
	CurrentUse     ClientActivationCurrentUseProof
	CurrentAbsence ClientActivationCurrentAbsenceProof
	StopReason     string
}

// ClientRemovalCompletionProof is implemented only by a process-local read of
// one canonical terminal keep-data removal journal or retained tombstone.
type ClientRemovalCompletionProof interface {
	ReconciliationRemovalCompletion() (ClientRemovalCompletion, bool)
}

type ClientRemovalSelection struct {
	Requested           bool
	CompletionAttempted bool
	Completion          ClientRemovalCompletionProof
	StopReason          string
}

// SourceRetirementCompletionProof is implemented only by a process-local read
// of one canonical terminal retirement journal or retained tombstone. The
// public view is historical and cannot establish current namespace absence.
type SourceRetirementCompletionProof interface {
	ReconciliationRetirementCompletion() (SourceRetirementCompletion, bool)
}

// SourceRetirementAbsenceProof is process-local authority for a bounded
// current observation of the exact retired names under their original bound
// parents. It performs no downloader request.
type SourceRetirementAbsenceProof interface {
	ReconcileCurrentRetiredNameAbsence() (SourceRetirementCurrentAbsence, bool)
}

type SourceRetirementSelection struct {
	Requested           bool
	CompletionAttempted bool
	AbsenceAttempted    bool
	Completion          SourceRetirementCompletionProof
	CurrentAbsence      SourceRetirementAbsenceProof
	StopReason          string
}

// MaterializedFinalSelection is an explicit same-invocation read of one
// materialize operation plus the opaque bridge created only when its current
// exact final proof is immediately followed by the ordinary exact-source proof
// used by this reconciliation.
type MaterializedFinalSelection struct {
	Requested  bool
	Final      *materialize.VerifiedFinal
	Source     *materialize.VerifiedFinalSource
	StopReason string
}

// SiteBindingSelection is an explicit, same-invocation read of one sealed
// record ID. StopReason is a stable internal code; public reports never copy a
// store error, record payload, or path into it.
type SiteBindingSelection struct {
	Requested  bool
	RecordID   metastore.RecordID
	Verified   *sitebinding.VerifiedSiteBinding
	StopReason string
}

// SiteDetailSelection is an explicit same-invocation live observation. The
// public TorrentDetail DTO alone is never authority; Observed must retain the
// opaque value returned by the adapter in this invocation.
type SiteDetailSelection struct {
	Requested    bool
	Config       site.TorrentDetailConfig
	Observed     *site.ObservedTorrentDetail
	Receipt      site.TorrentDetailReceipt
	StopReason   string
	RequestsMade int
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
	MetafileVariantID          string `json:"metafile_variant_id"`
	SiteRequested              bool   `json:"site_requested"`
	SiteBindingRequested       bool   `json:"site_binding_requested"`
	SiteBindingSelector        string `json:"site_binding_selector"`
	SiteDetailRequested        bool   `json:"site_detail_requested"`
	ClientRequested            bool   `json:"client_requested"`
	PathMappingRequested       bool   `json:"path_mapping_requested"`
	PathMappingID              string `json:"path_mapping_id,omitempty"`
	ClientPathSemantics        string `json:"client_path_semantics"`
	ClientFileLayoutMode       string `json:"client_file_layout_mode"`
	MaterializedFinalRequested bool   `json:"materialized_final_requested"`
	ClientActivationRequested  bool   `json:"client_activation_requested"`
	ClientRemovalRequested     bool   `json:"client_removal_requested"`
	SourceRetirementRequested  bool   `json:"source_retirement_requested"`
	AbsolutePathsShown         bool   `json:"absolute_paths_shown"`
}

type ReportLedgers struct {
	Site       SiteLedger             `json:"site"`
	Metafile   MetafileLedger         `json:"metafile"`
	Storage    StorageLedger          `json:"storage"`
	Downloader DownloaderLedger       `json:"downloader"`
	Activation ClientActivationLedger `json:"client_activation"`
	Removal    ClientRemovalLedger    `json:"client_removal"`
	Retirement SourceRetirementLedger `json:"source_retirement"`
}

type ClientRemovalLedger struct {
	Status                      string                   `json:"status"`
	Completion                  *ClientRemovalCompletion `json:"completion,omitempty"`
	ProcessLocalCompletionProof bool                     `json:"process_local_completion_proof"`
	Historical                  bool                     `json:"historical"`
	StopReason                  string                   `json:"stop_reason,omitempty"`
}

type ClientRemovalCompletion struct {
	Driver                 string    `json:"driver"`
	OperationID            string    `json:"operation_id"`
	PlanID                 string    `json:"plan_id"`
	IntentID               string    `json:"intent_id"`
	CompletionID           string    `json:"completion_id"`
	CompletionBasis        string    `json:"completion_basis"`
	UseID                  string    `json:"use_id"`
	JobID                  string    `json:"job_id"`
	FileLayoutID           string    `json:"file_layout_id"`
	CompleteSnapshotID     string    `json:"complete_file_snapshot_id"`
	ClientConfigID         string    `json:"client_config_id"`
	PathMappingID          string    `json:"path_mapping_id"`
	ActivationOperationID  string    `json:"activation_operation_id"`
	ActivationPlanID       string    `json:"activation_plan_id"`
	ActivationTerminalID   string    `json:"activation_terminal_id"`
	MetafileVariantID      string    `json:"metafile_variant_id"`
	MaterializeOperationID string    `json:"materialize_operation_id"`
	MaterializePlanID      string    `json:"materialize_plan_id"`
	TargetRootIdentity     string    `json:"target_root_identity"`
	FinalObjectIdentity    string    `json:"final_object_identity"`
	MultiFile              bool      `json:"multi_file"`
	ManifestFiles          int       `json:"manifest_files"`
	ContentBytes           int64     `json:"content_bytes"`
	ObservedAtStart        time.Time `json:"observed_at_start"`
	ObservedAtEnd          time.Time `json:"observed_at_end"`
	JobsExamined           int       `json:"jobs_examined"`
	RetainedTombstone      bool      `json:"retained_tombstone"`
	Assurance              string    `json:"assurance"`
}

type SourceRetirementLedger struct {
	Status                      string                          `json:"status"`
	Completion                  *SourceRetirementCompletion     `json:"completion,omitempty"`
	CurrentAbsence              *SourceRetirementCurrentAbsence `json:"current_absence,omitempty"`
	ProcessLocalCompletionProof bool                            `json:"process_local_completion_proof"`
	ProcessLocalAbsenceProof    bool                            `json:"process_local_absence_proof"`
	Historical                  bool                            `json:"historical"`
	StopReason                  string                          `json:"stop_reason,omitempty"`
}

type SourceRetirementCompletion struct {
	OperationID            string `json:"operation_id"`
	PlanID                 string `json:"plan_id"`
	IntentID               string `json:"intent_id"`
	CompletionID           string `json:"completion_id"`
	SearchScopeID          string `json:"search_scope_id"`
	MetafileVariantID      string `json:"metafile_variant_id"`
	MaterializeOperationID string `json:"materialize_operation_id"`
	MaterializePlanID      string `json:"materialize_plan_id"`
	ActivationOperationID  string `json:"activation_operation_id"`
	ActivationPlanID       string `json:"activation_plan_id"`
	ClientCompletionID     string `json:"client_completion_id"`
	CurrentClientUseID     string `json:"current_client_use_id"`
	SourceSelectionID      string `json:"source_selection_id"`
	TargetRootIdentity     string `json:"target_root_identity"`
	FinalObjectIdentity    string `json:"final_object_identity"`
	ClientSnapshotID       string `json:"client_snapshot_id"`
	FilesRetired           int    `json:"files_retired"`
	BytesRetired           int64  `json:"bytes_retired"`
	RetainedTombstone      bool   `json:"retained_tombstone"`
	Assurance              string `json:"assurance"`
}

type SourceRetirementCurrentAbsence struct {
	OperationID              string    `json:"operation_id"`
	PlanID                   string    `json:"plan_id"`
	CompletionID             string    `json:"completion_id"`
	AbsenceID                string    `json:"absence_id"`
	SearchScopeID            string    `json:"search_scope_id"`
	FilesChecked             int       `json:"files_checked"`
	BytesRetired             int64     `json:"bytes_retired"`
	ParentDirectoriesChecked int       `json:"parent_directories_checked"`
	ObservedAtStart          time.Time `json:"observed_at_start"`
	ObservedAtEnd            time.Time `json:"observed_at_end"`
	Assurance                string    `json:"assurance"`
}

type ClientActivationLedger struct {
	Status                          string                          `json:"status"`
	Completion                      *ClientActivationCompletion     `json:"completion,omitempty"`
	CurrentUse                      *ClientActivationCurrentUse     `json:"current_use,omitempty"`
	CurrentAbsence                  *ClientActivationCurrentAbsence `json:"current_absence,omitempty"`
	ProcessLocalCompletionProof     bool                            `json:"process_local_completion_proof"`
	ProcessLocalCurrentUseProof     bool                            `json:"process_local_current_use_proof"`
	ProcessLocalCurrentAbsenceProof bool                            `json:"process_local_current_absence_proof"`
	Historical                      bool                            `json:"historical"`
	StopReason                      string                          `json:"stop_reason,omitempty"`
}

type ClientActivationCompletion struct {
	Driver                 string    `json:"driver"`
	OperationID            string    `json:"operation_id"`
	PlanID                 string    `json:"plan_id"`
	TerminalMarkerID       string    `json:"terminal_marker_id"`
	Action                 string    `json:"action"`
	TerminalPhase          string    `json:"terminal_phase"`
	TerminalJobState       string    `json:"terminal_job_state"`
	MetafileVariantID      string    `json:"metafile_variant_id"`
	MaterializeOperationID string    `json:"materialize_operation_id"`
	MaterializePlanID      string    `json:"materialize_plan_id"`
	ClientConfigID         string    `json:"client_config_id"`
	PathMappingID          string    `json:"path_mapping_id"`
	JobID                  string    `json:"job_id"`
	FinalObjectIdentity    string    `json:"final_object_identity"`
	ObservedAtStart        time.Time `json:"observed_at_start"`
	ObservedAtEnd          time.Time `json:"observed_at_end"`
	Assurance              string    `json:"assurance"`
}

type ClientActivationCurrentUse struct {
	Driver              string    `json:"driver"`
	UseID               string    `json:"use_id"`
	JobID               string    `json:"job_id"`
	FileLayoutID        string    `json:"file_layout_id"`
	CompleteSnapshotID  string    `json:"complete_file_snapshot_id"`
	JobState            string    `json:"job_state"`
	JobProgress         float64   `json:"job_progress"`
	ObservedAtStart     time.Time `json:"observed_at_start"`
	ObservedAtEnd       time.Time `json:"observed_at_end"`
	FinalObjectIdentity string    `json:"final_object_identity"`
	Assurance           string    `json:"assurance"`
}

type ClientActivationCurrentAbsence struct {
	Driver              string    `json:"driver"`
	UseID               string    `json:"use_id"`
	JobID               string    `json:"job_id"`
	FileLayoutID        string    `json:"file_layout_id"`
	CompleteSnapshotID  string    `json:"complete_file_snapshot_id"`
	ObservedAtStart     time.Time `json:"observed_at_start"`
	ObservedAtEnd       time.Time `json:"observed_at_end"`
	RequestsMade        int       `json:"requests_made"`
	JobsExaminedBefore  int       `json:"jobs_examined_before"`
	JobsExaminedAfter   int       `json:"jobs_examined_after"`
	FinalObjectIdentity string    `json:"final_object_identity"`
	Assurance           string    `json:"assurance"`
}

type SiteLedger struct {
	Status            string             `json:"status"`
	Ref               *domain.TorrentRef `json:"ref,omitempty"`
	BindingRecordID   string             `json:"binding_record_id,omitempty"`
	StoreID           string             `json:"store_id,omitempty"`
	MetafileVariantID string             `json:"metafile_variant_id,omitempty"`
	Origin            string             `json:"origin,omitempty"`
	RouteID           string             `json:"route_id,omitempty"`
	ObservedAtStart   *time.Time         `json:"observed_at_start,omitempty"`
	ObservedAtEnd     *time.Time         `json:"observed_at_end,omitempty"`
	ProcessLocalProof bool               `json:"process_local_proof"`
	Historical        bool               `json:"historical"`
	StopReason        string             `json:"stop_reason,omitempty"`
	Detail            SiteDetailLedger   `json:"detail"`
}

type SiteDetailLedger struct {
	Status            string                   `json:"status"`
	Ref               *domain.TorrentRef       `json:"ref,omitempty"`
	Origin            string                   `json:"origin,omitempty"`
	RouteID           string                   `json:"route_id,omitempty"`
	ObservedAtStart   *time.Time               `json:"observed_at_start,omitempty"`
	ObservedAtEnd     *time.Time               `json:"observed_at_end,omitempty"`
	RequestsMade      int                      `json:"requests_made"`
	Limits            site.TorrentDetailLimits `json:"limits"`
	Used              site.TorrentDetailUsage  `json:"used"`
	Observation       *domain.TorrentDetail    `json:"observation,omitempty"`
	ProcessLocalProof bool                     `json:"process_local_proof"`
	StopReason        string                   `json:"stop_reason,omitempty"`
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
	Status            string                  `json:"status"`
	ProcessLocalProof bool                    `json:"process_local_proof"`
	SelectedSourceID  string                  `json:"selected_source_id,omitempty"`
	SourceSnapshotID  string                  `json:"source_snapshot_id,omitempty"`
	MaterializedFinal MaterializedFinalLedger `json:"materialized_final"`
	Discovery         seed.DiscoveryResult    `json:"discovery"`
}

type MaterializedFinalLedger struct {
	Status                   string                        `json:"status"`
	Observation              *materialize.FinalObservation `json:"observation,omitempty"`
	ProcessLocalFinalProof   bool                          `json:"process_local_final_proof"`
	ProcessLocalSourceBridge bool                          `json:"process_local_source_bridge"`
	StopReason               string                        `json:"stop_reason,omitempty"`
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
// opaque value returned by the same live/indexed discovery or exact-root
// observation, and Client.Before and Client.After must bracket that
// observation. A materialized-final request additionally requires the opaque
// bridge created by VerifyCurrentFinalSource. A JSON round-trip intentionally
// loses these storage capabilities and therefore cannot produce a verified
// relation.
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
			MetafileVariantID:          meta.MetafileVariantID,
			SiteRequested:              input.SiteRef != nil || input.SiteBinding.Requested || input.SiteDetail.Requested,
			SiteBindingRequested:       input.SiteBinding.Requested,
			SiteBindingSelector:        siteBindingSelector(input.SiteBinding.Requested),
			SiteDetailRequested:        input.SiteDetail.Requested,
			ClientRequested:            input.Client.Requested,
			PathMappingRequested:       input.PathMapping != nil,
			PathMappingID:              pathMappingIDValue,
			ClientPathSemantics:        pathSemantics,
			ClientFileLayoutMode:       fileLayoutMode,
			MaterializedFinalRequested: input.MaterializedFinal.Requested,
			ClientActivationRequested:  input.ClientActivation.Requested,
			ClientRemovalRequested:     input.ClientRemoval.Requested,
			SourceRetirementRequested:  input.SourceRetirement.Requested,
			AbsolutePathsShown:         input.ShowAbsolutePaths,
		},
		Relations: []Relation{},
		Blockers:  []ReportFinding{},
		Warnings:  []string{},
	}
	if input.Client.RequestsMade > 0 || input.Client.Before != nil || input.Client.After != nil || input.Client.FileAttempted {
		report.Effect = append(report.Effect, "read_downloader_state")
	}
	if input.MaterializedFinal.Requested {
		report.Effect = append(report.Effect, "read_materialize_operation_state", "read_materialized_final_content")
	}
	if input.ClientActivation.Requested {
		report.Effect = append(report.Effect, "read_client_activation_operation_state")
	}
	if input.ClientRemoval.CompletionAttempted {
		report.Effect = append(report.Effect, "read_client_removal_operation_state")
	}
	if input.SourceRetirement.CompletionAttempted {
		report.Effect = append(report.Effect, "read_source_retirement_operation_state")
	}
	if input.SourceRetirement.AbsenceAttempted {
		report.Effect = append(report.Effect, "read_retired_source_name_absence")
	}
	if input.SiteDetail.RequestsMade > 0 || input.SiteDetail.Observed != nil || input.SiteDetail.Receipt.Used.RequestsAttempted > 0 {
		report.Effect = append(report.Effect, site.TorrentDetailReadEffect)
	}
	if input.Client.FileAttempted {
		report.Effect = append(report.Effect, "read_downloader_file_layout")
	}
	report.Ledgers.Metafile = MetafileLedger{
		Status: "observed", VariantID: meta.MetafileVariantID, Version: meta.Version,
		InfoHashV1: meta.InfoHashV1, InfoHashV2: meta.InfoHashV2, Private: meta.Private,
		PhysicalBytes: physicalBytes(meta),
	}

	siteLedger, siteRelation, siteBlockers, siteWarnings := assessSiteBinding(meta, input.SiteRef, input.SiteBinding)
	siteDetailLedger, detailBlockers, detailWarnings := assessSiteDetail(input.SiteRef, input.SiteDetail)
	siteLedger.Detail = siteDetailLedger
	if siteDetailLedger.Status == "observed_current_ref" {
		siteRelation.EvidenceBasis = append(siteRelation.EvidenceBasis,
			"same_invocation_authenticated_site_detail",
			"exact_remote_id_detail_page",
			"single_bounded_no_redirect_site_read",
			"current_site_claim_not_metafile_variant_proof",
		)
		if siteRelation.EvidenceLevel == "declared" || siteRelation.EvidenceLevel == "none" {
			siteRelation.EvidenceLevel = "direct_site_claim"
		}
	}
	report.Ledgers.Site = siteLedger
	report.Blockers = append(report.Blockers, siteBlockers...)
	report.Blockers = append(report.Blockers, detailBlockers...)
	report.Warnings = append(report.Warnings, siteWarnings...)
	report.Warnings = append(report.Warnings, detailWarnings...)
	storageRelation := newRelation("storage_content_proof")
	storageRelation.Status = input.Discovery.SourceOutcome
	storageRelation.LeftIDs = append(storageRelation.LeftIDs, meta.MetafileVariantID)
	storageLedger := StorageLedger{
		Status:            input.Discovery.SourceOutcome,
		MaterializedFinal: MaterializedFinalLedger{Status: "not_requested"},
		Discovery:         sanitizedDiscovery(input.Discovery, input.ShowAbsolutePaths),
	}
	if verifiedStorageOutcome(input.Discovery.SourceOutcome) {
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
			report.Blockers = append(report.Blockers, ReportFinding{Code: "storage.process_local_proof_missing", Message: "the selected storage result is not backed by a same-invocation process-local proof"})
		}
	} else {
		storageRelation.EvidenceLevel = input.Discovery.BestEvidence
		for _, blocker := range input.Discovery.Blockers {
			storageRelation.BlockerCodes = append(storageRelation.BlockerCodes, blocker.Code)
			report.Blockers = append(report.Blockers, ReportFinding{Code: blocker.Code, Message: blocker.Message})
		}
	}
	materializedLedger, materializedOK, materializedBlockers, materializedWarnings := assessMaterializedFinal(
		meta, input.MaterializedFinal, &input.Discovery, input.VerifiedSource,
	)
	storageLedger.MaterializedFinal = materializedLedger
	report.Blockers = append(report.Blockers, materializedBlockers...)
	report.Warnings = append(report.Warnings, materializedWarnings...)
	if input.MaterializedFinal.Requested || materializedLedger.Status == "incomplete" {
		if materializedOK && storageLedger.ProcessLocalProof && verifiedStorageOutcome(storageRelation.Status) {
			observation := materializedLedger.Observation
			storageLedger.Status = "verified_materialized_final"
			storageRelation.Status = "verified_materialized_final"
			storageRelation.EvidenceLevel = "cryptographic"
			storageRelation.EvidenceBasis = append(storageRelation.EvidenceBasis,
				"current_materialized_final_exact_namespace",
				observation.AuthorityBasis,
				observation.Assurance,
				"opaque_materialized_final_source_bridge",
				"sequential_bracketed_non_atomic",
			)
		} else {
			storageLedger.Status = "incomplete"
			storageRelation.Status = "incomplete"
			if materializedLedger.StopReason == "materialized_final_integrity_failed" {
				storageLedger.Status = "integrity_failed"
				storageRelation.Status = "integrity_failed"
			}
			for _, blocker := range materializedBlockers {
				storageRelation.BlockerCodes = append(storageRelation.BlockerCodes, blocker.Code)
			}
		}
	}
	report.Ledgers.Storage = storageLedger

	client := assessClientBracket(meta, input.Client, input.ShowAbsolutePaths, clientWindows)
	clientEvidence, _ := downloader.DescribeLedgerDriver(client.ledger.Driver)
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
	case !verifiedStorageOutcome(storageRelation.Status) || !storageLedger.ProcessLocalProof:
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
				pathRelation.EvidenceBasis = append(pathRelation.EvidenceBasis, clientEvidence.FilePathBasis, clientEvidence.FileSelectionBasis, "bracketed_file_layout", "invocation_scoped_namespace_mapping", "lexical_comparison_only")
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
				pathRelation.EvidenceBasis = append(pathRelation.EvidenceBasis, "invocation_scoped_namespace_mapping", clientEvidence.ContentPathBasis, "lexical_comparison_only")
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
	activationLedger, activationBlockers, activationWarnings := assessClientActivation(
		meta, input.ClientActivation, input.MaterializedFinal, materializedLedger, input.Client,
		client, fileLayout, pathRelation, pathMappingIDValue,
	)
	report.Ledgers.Activation = activationLedger
	report.Blockers = append(report.Blockers, activationBlockers...)
	report.Warnings = append(report.Warnings, activationWarnings...)
	removalLedger, removalBlockers, removalWarnings := assessClientRemoval(
		meta, input.ClientRemoval, materializedLedger, activationLedger,
	)
	report.Ledgers.Removal = removalLedger
	report.Blockers = append(report.Blockers, removalBlockers...)
	report.Warnings = append(report.Warnings, removalWarnings...)
	retirementLedger, retirementBlockers, retirementWarnings := assessSourceRetirement(
		meta, input.SourceRetirement, materializedLedger, activationLedger,
	)
	report.Ledgers.Retirement = retirementLedger
	report.Blockers = append(report.Blockers, retirementBlockers...)
	report.Warnings = append(report.Warnings, retirementWarnings...)
	report.Relations = []Relation{siteRelation, variantRelation, client.relation, storageRelation, pathRelation}
	if removalLedger.Status == "historical_keep_data_removal_current_job_absent" {
		client.relation.BlockerCodes = withoutString(client.relation.BlockerCodes, "client.exact_job_absent")
		client.relation.EvidenceBasis = append(client.relation.EvidenceBasis, "terminal_keep_data_removal_expected_current_absence")
		pathRelation.BlockerCodes = withoutString(pathRelation.BlockerCodes, "path.client_identity_unavailable")
		report.Relations = []Relation{siteRelation, variantRelation, client.relation, storageRelation, pathRelation}
	}

	for _, code := range client.relation.BlockerCodes {
		report.Blockers = append(report.Blockers, findingForClientCode(code))
	}
	for _, code := range pathRelation.BlockerCodes {
		report.Blockers = append(report.Blockers, findingForPathCode(code))
	}
	if client.active {
		report.Warnings = append(report.Warnings, "the matching downloader job is active; lexical path agreement does not prove which bytes the client is currently reading")
	}
	report.Outcome = overallOutcome(siteRelation.Status, input.SiteBinding.Requested, siteDetailLedger.Status, input.SiteDetail.Requested,
		storageRelation.Status, storageLedger.ProcessLocalProof, client.relation.Status, pathRelation.Status, input.Client.Requested,
		activationLedger.Status, input.ClientActivation.Requested || activationLedger.Status != "not_requested",
		removalLedger.Status, input.ClientRemoval.Requested || removalLedger.Status != "not_requested",
		retirementLedger.Status, input.SourceRetirement.Requested || retirementLedger.Status != "not_requested")
	if report.Outcome == "consistent" {
		if removalLedger.Status == "historical_keep_data_removal_current_job_absent" {
			report.Assurance = "current_exact_local_content_plus_bracketed_typed_client_absence_and_canonical_historical_keep_data_removal_non_atomic"
		} else {
			report.Assurance = "local_content_proof_and_bracketed_typed_client_identity_with_lexical_path_agreement"
			if meta.MultiFile {
				report.Assurance = "local_content_proof_and_bracketed_typed_client_identity_with_bracketed_per_file_lexical_path_agreement"
			}
		}
		if input.SiteBinding.Requested && siteRelation.Status == "historical_observed_exact_variant" {
			report.Assurance += "_plus_sealed_historical_site_observation_current_site_mapping_unobservable"
		}
		if input.SiteDetail.Requested && siteDetailLedger.Status == "observed_current_ref" {
			report.Assurance += "_plus_same_invocation_current_site_ref_claim"
		}
		if materializedLedger.Status == "verified_current_final_source" {
			report.Assurance += "_plus_explicit_materialized_final_sequential_local_proof"
		}
		if activationLedger.Status == "historical_completion_current_job_bound" {
			report.Assurance += "_plus_canonical_historical_client_activation_bound_to_current_exact_job"
		}
		if activationLedger.Status == "historical_completion_current_job_absent" {
			report.Assurance += "_plus_canonical_historical_client_activation_bound_to_current_typed_job_absence"
		}
		if retirementLedger.Status == "historical_completion_current_absence_observed" {
			report.Assurance += "_plus_canonical_historical_source_retirement_with_current_bound_name_absence"
		}
	}
	report.Blockers = stableFindings(report.Blockers)
	report.Warnings = stableStrings(report.Warnings)
	for i := range report.Relations {
		report.Relations[i].BlockerCodes = stableStrings(report.Relations[i].BlockerCodes)
	}
	return report, nil
}

func assessMaterializedFinal(meta *metafile.MetaInfo, selection MaterializedFinalSelection, discovery *seed.DiscoveryResult, source *metafile.VerifiedSource) (MaterializedFinalLedger, bool, []ReportFinding, []string) {
	ledger := MaterializedFinalLedger{Status: "not_requested"}
	blockers := []ReportFinding{}
	warnings := []string{}
	hasActivity := selection.Final != nil || selection.Source != nil || selection.StopReason != ""
	if !selection.Requested {
		if !hasActivity {
			return ledger, false, blockers, warnings
		}
		ledger.Status = "incomplete"
		ledger.StopReason = "materialized_final_unexpected_activity"
		blockers = append(blockers, ReportFinding{
			Code: "storage.materialized_final_input_inconsistent", Message: "materialized-final proof values were supplied without an explicit materialized-final request",
		})
		return ledger, false, blockers, warnings
	}

	ledger.Status = "incomplete"
	ledger.StopReason = safeMaterializedFinalStopReason(selection.StopReason)
	if ledger.StopReason == "materialized_final_integrity_failed" {
		ledger.Status = "integrity_failed"
	}
	if selection.Final == nil || !selection.Final.Verified() {
		if ledger.StopReason == "" {
			ledger.StopReason = "materialized_final_verification_failed"
		}
		blockers = append(blockers, ReportFinding{
			Code: "storage.materialized_final_proof_unavailable", Message: "the explicit materialize operation did not establish a current exact final proof",
		})
		return ledger, false, blockers, warnings
	}

	observation := selection.Final.Observation()
	ledger.Observation = &observation
	if !materializedObservationMatchesMeta(observation, meta) {
		ledger.Status = "integrity_failed"
		ledger.StopReason = "materialized_final_integrity_failed"
		blockers = append(blockers, ReportFinding{
			Code: "storage.materialized_final_proof_mismatch", Message: "the current materialized-final proof does not match the requested metafile variant",
		})
		return ledger, false, blockers, warnings
	}
	ledger.ProcessLocalFinalProof = true
	if selection.StopReason != "" {
		blockers = append(blockers, ReportFinding{
			Code: "storage.materialized_final_source_bridge_unavailable", Message: "the current materialized final could not complete its same-invocation reconciliation source proof",
		})
		return ledger, false, blockers, warnings
	}
	if selection.Source == nil || discovery == nil || source == nil || !selection.Source.Matches(selection.Final, meta, discovery, source) {
		ledger.StopReason = "materialized_final_source_bridge_failed"
		blockers = append(blockers, ReportFinding{
			Code: "storage.materialized_final_source_bridge_unavailable", Message: "the current materialized final is not paired with the same-invocation reconciliation source proof",
		})
		return ledger, false, blockers, warnings
	}

	ledger.Status = "verified_current_final_source"
	ledger.ProcessLocalSourceBridge = true
	ledger.StopReason = ""
	warnings = append(warnings, "materialize attribution and reconciliation content proof are sequential bracketed observations, not an atomic filesystem snapshot")
	return ledger, true, blockers, warnings
}

func materializedObservationMatchesMeta(observation materialize.FinalObservation, meta *metafile.MetaInfo) bool {
	if meta == nil {
		return false
	}
	physical := physicalBytes(meta)
	return observation.OperationID != "" && observation.MaterializePlanID != "" &&
		observation.MetafileVariantID == meta.MetafileVariantID && observation.MetafileBytes == meta.MetafileBytes &&
		observation.InfoHashV1 == meta.InfoHashV1 && observation.InfoHashV2 == meta.InfoHashV2 &&
		observation.MultiFile == meta.MultiFile && observation.ManifestFiles == len(meta.Files) &&
		observation.ContentBytes == physical && observation.BytesVerified == physical &&
		observation.TargetRootIdentity != "" && observation.FinalObjectIdentity != "" &&
		(observation.AuthorityBasis == materialize.FinalAuthorityJournal || observation.AuthorityBasis == materialize.FinalAuthorityRetention) &&
		observation.Assurance == "same_invocation_bracketed_non_atomic_exact_content_and_namespace"
}

func safeMaterializedFinalStopReason(value string) string {
	switch value {
	case "materialized_final_context_cancelled", "materialized_final_integrity_failed", "materialized_final_policy_blocked",
		"materialized_final_verification_failed", "materialized_final_source_bridge_failed", "materialized_final_unexpected_activity":
		return value
	case "":
		return ""
	default:
		return "materialized_final_verification_failed"
	}
}

func assessClientActivation(meta *metafile.MetaInfo, selection ClientActivationSelection, materializedSelection MaterializedFinalSelection,
	materialized MaterializedFinalLedger, bracket ClientBracket, client clientAssessment, fileLayout clientFileLayoutAssessment,
	pathRelation Relation, currentPathMappingID string) (ClientActivationLedger, []ReportFinding, []string) {
	ledger := ClientActivationLedger{Status: "not_requested"}
	blockers := []ReportFinding{}
	warnings := []string{}
	hasActivity := selection.Completion != nil || selection.CurrentUse != nil || selection.CurrentAbsence != nil || selection.StopReason != ""
	if !selection.Requested {
		if !hasActivity {
			return ledger, blockers, warnings
		}
		ledger.Status = "incomplete"
		ledger.StopReason = "activation_unexpected_activity"
		blockers = append(blockers, ReportFinding{Code: "activation.input_inconsistent", Message: "client-activation proof values were supplied without an explicit activation request"})
		return ledger, blockers, warnings
	}

	ledger.Status = "incomplete"
	ledger.StopReason = safeClientActivationStopReason(selection.StopReason)
	if ledger.StopReason == "activation_completion_integrity_failed" {
		ledger.Status = "integrity_failed"
	}
	if ledger.StopReason == "activation_completion_selector_mismatch" {
		ledger.Status = "selected_activation_mismatch"
	}
	if selection.Completion == nil {
		blockers = append(blockers, ReportFinding{Code: "activation.completion_proof_unavailable", Message: "the explicit terminal client-activation journal could not be verified in this invocation"})
		return ledger, blockers, warnings
	}
	completion, ok := selection.Completion.ReconciliationCompletion()
	if !ok || !validClientActivationCompletion(completion) {
		ledger.Status = "incomplete"
		if ledger.StopReason == "" {
			ledger.StopReason = "activation_completion_load_failed"
		}
		blockers = append(blockers, ReportFinding{Code: "activation.completion_proof_unavailable", Message: "the terminal client-activation capability is unavailable or invalid"})
		return ledger, blockers, warnings
	}
	ledger.Completion = &completion
	ledger.ProcessLocalCompletionProof = true
	ledger.Historical = true
	warnings = append(warnings, "the terminal client-activation journal is historical evidence and does not by itself prove current downloader state")

	if meta == nil || completion.MetafileVariantID != meta.MetafileVariantID || !materializedSelection.Requested ||
		materialized.Observation == nil || !materialized.ProcessLocalFinalProof ||
		completion.MaterializeOperationID != materialized.Observation.OperationID ||
		completion.MaterializePlanID != materialized.Observation.MaterializePlanID ||
		completion.FinalObjectIdentity != materialized.Observation.FinalObjectIdentity ||
		currentPathMappingID == "" || completion.PathMappingID != currentPathMappingID {
		ledger.Status = "selected_activation_mismatch"
		ledger.StopReason = "activation_completion_selector_mismatch"
		blockers = append(blockers, ReportFinding{Code: "activation.selection_mismatch", Message: "the selected terminal activation does not belong to the requested metafile, materialized final, or client path mapping"})
		return ledger, blockers, warnings
	}
	if selection.CurrentUse != nil && selection.CurrentAbsence != nil {
		ledger.StopReason = "activation_unexpected_activity"
		blockers = append(blockers, ReportFinding{Code: "activation.input_inconsistent", Message: "current-use and current-absence activation bridges are mutually exclusive"})
		return ledger, blockers, warnings
	}
	if selection.CurrentAbsence != nil {
		ledger.Status = "historical_completion_current_job_absence_unbound"
		if selection.StopReason != "" || client.ledger.Status != "observed_stable" || client.relation.Status != "absent" || client.job != nil ||
			pathRelation.Status != "not_comparable" || materialized.Status != "verified_current_final_source" ||
			bracket.FileAttempted || bracket.FileRequestsMade != 0 || bracket.FilesBefore != nil || bracket.FilesAfter != nil {
			if ledger.StopReason == "" {
				ledger.StopReason = "activation_current_absence_bridge_failed"
			}
			blockers = append(blockers, ReportFinding{Code: "activation.current_absence_bridge_unavailable", Message: "the historical terminal activation could not be bound to complete current typed queue absence and the verified materialized final"})
			return ledger, blockers, warnings
		}
		current, ok := selection.CurrentAbsence.ReconcileCurrentAbsence(bracket)
		if !ok || !validClientActivationCurrentAbsence(current) || current.Driver != completion.Driver ||
			current.JobID != completion.JobID || current.FinalObjectIdentity != completion.FinalObjectIdentity ||
			bracket.Before == nil || bracket.After == nil || current.ObservedAtStart != bracket.Before.ObservedAtStart ||
			current.ObservedAtEnd != bracket.After.ObservedAtEnd || current.RequestsMade != bracket.RequestsMade {
			ledger.StopReason = "activation_current_absence_bridge_failed"
			blockers = append(blockers, ReportFinding{Code: "activation.current_absence_bridge_unavailable", Message: "the process-local activation absence bridge does not match this reconciliation's downloader bracket"})
			return ledger, blockers, warnings
		}
		ledger.Status = "historical_completion_current_job_absent"
		ledger.CurrentAbsence = &current
		ledger.ProcessLocalCurrentAbsenceProof = true
		ledger.StopReason = ""
		warnings = append(warnings, "activation attribution, current typed queue absence, and local content proof are separate sequential non-atomic observations")
		return ledger, blockers, warnings
	}
	ledger.Status = "historical_completion_current_job_unbound"
	if selection.StopReason != "" || selection.CurrentUse == nil || client.relation.Status != "exact_unique" || client.job == nil ||
		pathRelation.Status != "same_location" || !client.contentStable ||
		materialized.Status != "verified_current_final_source" ||
		(meta.MultiFile && (fileLayout.ledger.Status != "observed_stable" || !fileLayout.stable || !fileLayout.manifestOK)) {
		if ledger.StopReason == "" {
			ledger.StopReason = "activation_current_use_bridge_failed"
		}
		blockers = append(blockers, ReportFinding{Code: "activation.current_job_bridge_unavailable", Message: "the historical terminal activation could not be bound to the current exact downloader job and verified materialized layout"})
		return ledger, blockers, warnings
	}
	current, ok := selection.CurrentUse.ReconcileCurrentUse(bracket)
	if !ok || !validClientActivationCurrentUse(current) || current.Driver != completion.Driver ||
		current.JobID != completion.JobID || current.JobID != jobID(client.job.Hash) ||
		current.JobState != safeClientState(client.job.State) || current.JobProgress != client.job.Progress ||
		current.FinalObjectIdentity != completion.FinalObjectIdentity ||
		bracket.Before == nil || bracket.After == nil || current.ObservedAtStart != bracket.Before.ObservedAtStart ||
		current.ObservedAtEnd != bracket.After.ObservedAtEnd {
		ledger.StopReason = "activation_current_use_bridge_failed"
		blockers = append(blockers, ReportFinding{Code: "activation.current_job_bridge_unavailable", Message: "the process-local activation bridge does not match this reconciliation's downloader bracket"})
		return ledger, blockers, warnings
	}
	ledger.Status = "historical_completion_current_job_bound"
	ledger.CurrentUse = &current
	ledger.ProcessLocalCurrentUseProof = true
	ledger.StopReason = ""
	warnings = append(warnings, "activation attribution, current downloader claims, and local content proof are separate sequential non-atomic observations")
	return ledger, blockers, warnings
}

func assessClientRemoval(meta *metafile.MetaInfo, selection ClientRemovalSelection, materialized MaterializedFinalLedger,
	activation ClientActivationLedger) (ClientRemovalLedger, []ReportFinding, []string) {
	ledger := ClientRemovalLedger{Status: "not_requested"}
	blockers := []ReportFinding{}
	warnings := []string{}
	hasActivity := selection.CompletionAttempted || selection.Completion != nil || selection.StopReason != ""
	if !selection.Requested {
		if !hasActivity {
			return ledger, blockers, warnings
		}
		ledger.Status = "incomplete"
		ledger.StopReason = "removal_unexpected_activity"
		blockers = append(blockers, ReportFinding{Code: "removal.input_inconsistent", Message: "client-removal proof values were supplied without an explicit removal request"})
		return ledger, blockers, warnings
	}

	ledger.Status = "incomplete"
	ledger.StopReason = safeClientRemovalStopReason(selection.StopReason)
	if ledger.StopReason == "removal_completion_integrity_failed" {
		ledger.Status = "integrity_failed"
	}
	if ledger.StopReason == "removal_completion_selector_mismatch" {
		ledger.Status = "selected_removal_mismatch"
	}
	if !selection.CompletionAttempted || selection.Completion == nil {
		blockers = append(blockers, ReportFinding{Code: "removal.completion_proof_unavailable", Message: "the explicit terminal keep-data removal journal could not be verified in this invocation"})
		return ledger, blockers, warnings
	}
	completion, ok := selection.Completion.ReconciliationRemovalCompletion()
	if !ok || !validClientRemovalCompletion(completion) {
		if ledger.StopReason == "" {
			ledger.StopReason = "removal_completion_load_failed"
		}
		blockers = append(blockers, ReportFinding{Code: "removal.completion_proof_unavailable", Message: "the terminal keep-data removal capability is unavailable or invalid"})
		return ledger, blockers, warnings
	}
	ledger.Completion = &completion
	ledger.ProcessLocalCompletionProof = true
	ledger.Historical = true
	warnings = append(warnings, "the terminal keep-data removal record is historical evidence and does not by itself prove current queue absence")

	if meta == nil || materialized.Observation == nil || !materialized.ProcessLocalFinalProof ||
		activation.Completion == nil || !activation.ProcessLocalCompletionProof ||
		completion.MetafileVariantID != meta.MetafileVariantID ||
		completion.MaterializeOperationID != materialized.Observation.OperationID ||
		completion.MaterializePlanID != materialized.Observation.MaterializePlanID ||
		completion.TargetRootIdentity != materialized.Observation.TargetRootIdentity ||
		completion.FinalObjectIdentity != materialized.Observation.FinalObjectIdentity ||
		completion.MultiFile != materialized.Observation.MultiFile || completion.ManifestFiles != materialized.Observation.ManifestFiles ||
		completion.ContentBytes != materialized.Observation.ContentBytes ||
		completion.Driver != activation.Completion.Driver || completion.ClientConfigID != activation.Completion.ClientConfigID ||
		completion.PathMappingID != activation.Completion.PathMappingID ||
		completion.ActivationOperationID != activation.Completion.OperationID ||
		completion.ActivationPlanID != activation.Completion.PlanID ||
		completion.ActivationTerminalID != activation.Completion.TerminalMarkerID ||
		completion.JobID != activation.Completion.JobID || completion.ObservedAtStart.Before(activation.Completion.ObservedAtEnd) {
		ledger.Status = "selected_removal_mismatch"
		ledger.StopReason = "removal_completion_selector_mismatch"
		blockers = append(blockers, ReportFinding{Code: "removal.selection_mismatch", Message: "the selected terminal keep-data removal does not belong to the requested metafile, materialized final, or activation lineage"})
		return ledger, blockers, warnings
	}
	if completion.CompletionBasis != "accepted_response_then_exact_absence" {
		ledger.Status = "historical_absence_causality_unproven"
		ledger.StopReason = "removal_absence_causality_unproven"
		blockers = append(blockers, ReportFinding{Code: "removal.absence_causality_unproven", Message: "the historical exact absence followed an unknown request result and cannot be attributed to the reviewed keep-data removal"})
		return ledger, blockers, warnings
	}
	if selection.StopReason != "" || activation.Status != "historical_completion_current_job_absent" ||
		activation.CurrentAbsence == nil || !activation.ProcessLocalCurrentAbsenceProof {
		if ledger.StopReason == "" {
			ledger.StopReason = "removal_current_absence_unavailable"
		}
		blockers = append(blockers, ReportFinding{Code: "removal.current_absence_unavailable", Message: "the terminal keep-data removal could not be combined with current typed queue absence and exact final proof"})
		return ledger, blockers, warnings
	}
	current := activation.CurrentAbsence
	if completion.UseID != current.UseID || completion.JobID != current.JobID ||
		completion.FileLayoutID != current.FileLayoutID || completion.CompleteSnapshotID != current.CompleteSnapshotID ||
		completion.FinalObjectIdentity != current.FinalObjectIdentity || current.ObservedAtStart.Before(completion.ObservedAtEnd) {
		ledger.Status = "selected_removal_mismatch"
		ledger.StopReason = "removal_completion_selector_mismatch"
		blockers = append(blockers, ReportFinding{Code: "removal.selection_mismatch", Message: "the current typed queue absence does not match the completed keep-data removal lineage"})
		return ledger, blockers, warnings
	}
	ledger.Status = "historical_keep_data_removal_current_job_absent"
	ledger.StopReason = ""
	warnings = append(warnings, "removal attribution, current typed queue absence, and current exact local content are separate sequential non-atomic observations")
	return ledger, blockers, warnings
}

func assessSourceRetirement(meta *metafile.MetaInfo, selection SourceRetirementSelection, materialized MaterializedFinalLedger,
	activation ClientActivationLedger) (SourceRetirementLedger, []ReportFinding, []string) {
	ledger := SourceRetirementLedger{Status: "not_requested"}
	blockers := []ReportFinding{}
	warnings := []string{}
	hasActivity := selection.CompletionAttempted || selection.AbsenceAttempted || selection.Completion != nil || selection.CurrentAbsence != nil || selection.StopReason != ""
	if !selection.Requested {
		if !hasActivity {
			return ledger, blockers, warnings
		}
		ledger.Status = "incomplete"
		ledger.StopReason = "retirement_unexpected_activity"
		blockers = append(blockers, ReportFinding{Code: "retirement.input_inconsistent", Message: "source-retirement proof values were supplied without an explicit retirement request"})
		return ledger, blockers, warnings
	}

	ledger.Status = "incomplete"
	ledger.StopReason = safeSourceRetirementStopReason(selection.StopReason)
	if selection.AbsenceAttempted && !selection.CompletionAttempted || selection.Completion != nil && !selection.CompletionAttempted ||
		selection.CurrentAbsence != nil && !selection.AbsenceAttempted {
		ledger.StopReason = "retirement_unexpected_activity"
		blockers = append(blockers, ReportFinding{Code: "retirement.input_inconsistent", Message: "source-retirement proof activity is internally inconsistent"})
		return ledger, blockers, warnings
	}
	if ledger.StopReason == "retirement_completion_integrity_failed" {
		ledger.Status = "integrity_failed"
	}
	if ledger.StopReason == "retirement_completion_selector_mismatch" {
		ledger.Status = "selected_retirement_mismatch"
	}
	if selection.Completion == nil {
		blockers = append(blockers, ReportFinding{Code: "retirement.completion_proof_unavailable", Message: "the explicit terminal source-retirement journal could not be verified in this invocation"})
		return ledger, blockers, warnings
	}
	completion, ok := selection.Completion.ReconciliationRetirementCompletion()
	if !ok || !validSourceRetirementCompletion(completion) {
		ledger.Status = "incomplete"
		if ledger.StopReason == "" {
			ledger.StopReason = "retirement_completion_load_failed"
		}
		blockers = append(blockers, ReportFinding{Code: "retirement.completion_proof_unavailable", Message: "the terminal source-retirement capability is unavailable or invalid"})
		return ledger, blockers, warnings
	}
	ledger.Completion = &completion
	ledger.ProcessLocalCompletionProof = true
	ledger.Historical = true
	warnings = append(warnings, "the terminal source-retirement record is historical evidence and does not by itself prove that a retired name remains absent now")

	if meta == nil || materialized.Observation == nil || !materialized.ProcessLocalFinalProof ||
		activation.Completion == nil || !activation.ProcessLocalCompletionProof ||
		completion.MetafileVariantID != meta.MetafileVariantID ||
		completion.MaterializeOperationID != materialized.Observation.OperationID ||
		completion.MaterializePlanID != materialized.Observation.MaterializePlanID ||
		completion.TargetRootIdentity != materialized.Observation.TargetRootIdentity ||
		completion.FinalObjectIdentity != materialized.Observation.FinalObjectIdentity ||
		completion.ActivationOperationID != activation.Completion.OperationID ||
		completion.ActivationPlanID != activation.Completion.PlanID ||
		completion.ClientCompletionID != activation.Completion.TerminalMarkerID {
		ledger.Status = "selected_retirement_mismatch"
		ledger.StopReason = "retirement_completion_selector_mismatch"
		blockers = append(blockers, ReportFinding{Code: "retirement.selection_mismatch", Message: "the selected terminal source retirement does not belong to the requested metafile, materialized final, or activation lineage"})
		return ledger, blockers, warnings
	}

	ledger.Status = "historical_completion_current_absence_unobserved"
	if ledger.StopReason == "retirement_source_name_reappeared" {
		ledger.Status = "source_name_reappeared"
		blockers = append(blockers, ReportFinding{Code: "retirement.source_name_reappeared", Message: "at least one explicitly retired source name is present again in the bound original namespace"})
		return ledger, blockers, warnings
	}
	if completion.RetainedTombstone {
		ledger.StopReason = "retirement_current_absence_unavailable"
		blockers = append(blockers, ReportFinding{Code: "retirement.current_absence_unavailable", Message: "the retained retirement tombstone no longer contains authority to reobserve the original source names"})
		return ledger, blockers, warnings
	}
	if selection.StopReason != "" || selection.CurrentAbsence == nil || materialized.Status != "verified_current_final_source" ||
		activation.Status != "historical_completion_current_job_bound" || activation.CurrentUse == nil || !activation.ProcessLocalCurrentUseProof ||
		completion.CurrentClientUseID != activation.CurrentUse.UseID || completion.ClientSnapshotID != activation.CurrentUse.CompleteSnapshotID {
		if ledger.StopReason == "" {
			ledger.StopReason = "retirement_current_absence_unavailable"
		}
		blockers = append(blockers, ReportFinding{Code: "retirement.current_absence_unavailable", Message: "the historical source retirement could not be combined with current bound-name absence, final, and downloader-use proof"})
		return ledger, blockers, warnings
	}
	absence, ok := selection.CurrentAbsence.ReconcileCurrentRetiredNameAbsence()
	if !ok || !validSourceRetirementCurrentAbsence(absence) || absence.OperationID != completion.OperationID ||
		absence.PlanID != completion.PlanID || absence.CompletionID != completion.CompletionID ||
		absence.SearchScopeID != completion.SearchScopeID || absence.FilesChecked != completion.FilesRetired ||
		absence.BytesRetired != completion.BytesRetired {
		ledger.StopReason = "retirement_current_absence_unavailable"
		blockers = append(blockers, ReportFinding{Code: "retirement.current_absence_unavailable", Message: "the process-local retired-name absence proof does not match this terminal retirement"})
		return ledger, blockers, warnings
	}
	ledger.Status = "historical_completion_current_absence_observed"
	ledger.CurrentAbsence = &absence
	ledger.ProcessLocalAbsenceProof = true
	ledger.StopReason = ""
	warnings = append(warnings, "retirement completion, current source-name absence, current downloader use, and local content proof are separate sequential non-atomic observations")
	return ledger, blockers, warnings
}

func validSourceRetirementCompletion(value SourceRetirementCompletion) bool {
	if !validSHA256ID(value.OperationID) || !validSHA256ID(value.PlanID) || !validSHA256ID(value.IntentID) ||
		!validSHA256ID(value.CompletionID) || !validSHA256ID(value.SearchScopeID) || !validSHA256ID(value.MetafileVariantID) ||
		!validSHA256ID(value.MaterializeOperationID) || !validPlanID(value.MaterializePlanID) ||
		!validSHA256ID(value.ActivationOperationID) || !validPlanID(value.ActivationPlanID) ||
		!validSHA256ID(value.ClientCompletionID) || !validSHA256ID(value.CurrentClientUseID) ||
		!validSHA256ID(value.SourceSelectionID) || !validSHA256ID(value.ClientSnapshotID) ||
		value.TargetRootIdentity == "" || len(value.TargetRootIdentity) > 512 || value.FinalObjectIdentity == "" || len(value.FinalObjectIdentity) > 512 ||
		value.FilesRetired <= 0 || value.FilesRetired > 49_999 || value.BytesRetired <= 0 || value.BytesRetired > 1<<50 {
		return false
	}
	if value.RetainedTombstone {
		return value.Assurance == "same_invocation_bound_canonical_source_retirement_retention_tombstone_read_without_current_absence_inference"
	}
	return value.Assurance == "same_invocation_bound_canonical_terminal_source_retirement_journal_read_without_current_absence_inference"
}

func validSourceRetirementCurrentAbsence(value SourceRetirementCurrentAbsence) bool {
	return validSHA256ID(value.OperationID) && validSHA256ID(value.PlanID) && validSHA256ID(value.CompletionID) &&
		validSHA256ID(value.AbsenceID) && validSHA256ID(value.SearchScopeID) && value.FilesChecked > 0 && value.FilesChecked <= 49_999 &&
		value.BytesRetired > 0 && value.BytesRetired <= 1<<50 && value.ParentDirectoriesChecked > 0 &&
		value.ParentDirectoriesChecked <= value.FilesChecked && !value.ObservedAtStart.IsZero() &&
		!value.ObservedAtEnd.Before(value.ObservedAtStart) &&
		value.Assurance == "same_invocation_two_pass_identity_bound_retired_name_absence_bracketed_non_atomic"
}

func safeSourceRetirementStopReason(value string) string {
	switch value {
	case "retirement_context_cancelled", "retirement_completion_integrity_failed", "retirement_completion_load_failed",
		"retirement_completion_selector_mismatch", "retirement_current_absence_unavailable", "retirement_source_name_reappeared",
		"retirement_prerequisite_unavailable", "retirement_unexpected_activity":
		return value
	case "":
		return ""
	default:
		return "retirement_completion_load_failed"
	}
}

func validClientActivationCompletion(value ClientActivationCompletion) bool {
	if _, ok := downloader.DescribeLedgerDriver(value.Driver); !ok || !validSHA256ID(value.OperationID) || !validPlanID(value.PlanID) ||
		!validSHA256ID(value.TerminalMarkerID) || !validSHA256ID(value.MetafileVariantID) || !validSHA256ID(value.MaterializeOperationID) ||
		!validPlanID(value.MaterializePlanID) || !validSHA256ID(value.ClientConfigID) || !validSHA256ID(value.PathMappingID) ||
		!validSHA256ID(value.JobID) || value.FinalObjectIdentity == "" || len(value.FinalObjectIdentity) > 512 ||
		value.ObservedAtStart.IsZero() || value.ObservedAtEnd.Before(value.ObservedAtStart) || safeClientState(value.TerminalJobState) != value.TerminalJobState {
		return false
	}
	if value.Action == "recheck_only" {
		if value.TerminalPhase != "recheck_complete_stopped" || !activationStoppedCompleteState(value.TerminalJobState) {
			return false
		}
	} else if value.Action == "recheck_then_start" {
		if value.TerminalPhase != "started_client_claim_observed" || !activationStartedState(value.TerminalJobState) {
			return false
		}
	} else {
		return false
	}
	return value.Assurance == "same_invocation_bound_canonical_terminal_activation_read_without_durability_or_client_refresh" ||
		value.Assurance == "same_invocation_bound_canonical_activation_retention_tombstone_read_without_durability_or_client_refresh"
}

func validClientActivationCurrentUse(value ClientActivationCurrentUse) bool {
	if _, ok := downloader.DescribeLedgerDriver(value.Driver); !ok || !validSHA256ID(value.UseID) || !validSHA256ID(value.JobID) ||
		!validSHA256ID(value.FileLayoutID) || !validSHA256ID(value.CompleteSnapshotID) ||
		value.FinalObjectIdentity == "" || len(value.FinalObjectIdentity) > 512 ||
		value.JobProgress != 1 || value.ObservedAtStart.IsZero() || value.ObservedAtEnd.Before(value.ObservedAtStart) ||
		safeClientState(value.JobState) != value.JobState {
		return false
	}
	if !activationStoppedCompleteState(value.JobState) && !activationStartedState(value.JobState) {
		return false
	}
	return value.Assurance == "same_invocation_existing_reconciliation_bracket_bound_to_canonical_terminal_activation_and_exact_final_non_atomic"
}

func validClientActivationCurrentAbsence(value ClientActivationCurrentAbsence) bool {
	if _, ok := downloader.DescribeLedgerDriver(value.Driver); !ok || !validSHA256ID(value.UseID) || !validSHA256ID(value.JobID) ||
		!validSHA256ID(value.FileLayoutID) || !validSHA256ID(value.CompleteSnapshotID) ||
		value.FinalObjectIdentity == "" || len(value.FinalObjectIdentity) > 512 || value.ObservedAtStart.IsZero() ||
		value.ObservedAtEnd.Before(value.ObservedAtStart) || value.RequestsMade < 2 || value.RequestsMade > 4 ||
		value.JobsExaminedBefore < 0 || value.JobsExaminedBefore > maxLedgerJobs ||
		value.JobsExaminedAfter < 0 || value.JobsExaminedAfter > maxLedgerJobs {
		return false
	}
	return value.Assurance == "same_invocation_existing_reconciliation_bracket_bound_to_canonical_terminal_activation_and_exact_final_with_typed_job_absence_non_atomic_without_causality_attribution"
}

func validClientRemovalCompletion(value ClientRemovalCompletion) bool {
	if _, ok := downloader.DescribeLedgerDriver(value.Driver); !ok || !validSHA256ID(value.OperationID) || !validPlanID(value.PlanID) ||
		!validSHA256ID(value.IntentID) || !validSHA256ID(value.CompletionID) || !validSHA256ID(value.UseID) ||
		!validSHA256ID(value.JobID) || !validSHA256ID(value.FileLayoutID) || !validSHA256ID(value.CompleteSnapshotID) ||
		!validSHA256ID(value.ClientConfigID) || !validSHA256ID(value.PathMappingID) ||
		!validSHA256ID(value.ActivationOperationID) || !validPlanID(value.ActivationPlanID) ||
		!validSHA256ID(value.ActivationTerminalID) || !validSHA256ID(value.MetafileVariantID) ||
		!validSHA256ID(value.MaterializeOperationID) || !validPlanID(value.MaterializePlanID) ||
		value.TargetRootIdentity == "" || len(value.TargetRootIdentity) > 512 ||
		value.FinalObjectIdentity == "" || len(value.FinalObjectIdentity) > 512 || value.ManifestFiles <= 0 ||
		value.ManifestFiles > 49_999 || value.ContentBytes < 0 || value.ContentBytes > 1<<50 ||
		value.ObservedAtStart.IsZero() || value.ObservedAtEnd.Before(value.ObservedAtStart) ||
		value.JobsExamined < 0 || value.JobsExamined > maxLedgerJobs {
		return false
	}
	if value.CompletionBasis != "accepted_response_then_exact_absence" && value.CompletionBasis != "exact_absence_after_unknown_attempt_causality_unproven" {
		return false
	}
	if value.RetainedTombstone {
		return value.Assurance == "same_invocation_bound_canonical_client_removal_retention_tombstone_read_without_current_queue_inference"
	}
	return value.Assurance == "same_invocation_bound_canonical_terminal_client_removal_journal_read_without_current_queue_inference"
}

func activationStoppedCompleteState(value string) bool {
	return value == "pausedUP" || value == "stoppedUP"
}

func activationStartedState(value string) bool {
	switch value {
	case "uploading", "queuedUP", "stalledUP", "forcedUP":
		return true
	default:
		return false
	}
}

func safeClientActivationStopReason(value string) string {
	switch value {
	case "activation_context_cancelled", "activation_completion_integrity_failed", "activation_completion_load_failed",
		"activation_completion_selector_mismatch", "activation_current_use_bridge_failed", "activation_current_absence_bridge_failed", "activation_unexpected_activity":
		return value
	case "":
		return ""
	default:
		return "activation_completion_load_failed"
	}
}

func safeClientRemovalStopReason(value string) string {
	switch value {
	case "removal_context_cancelled", "removal_completion_integrity_failed", "removal_completion_load_failed",
		"removal_completion_selector_mismatch", "removal_absence_causality_unproven",
		"removal_current_absence_unavailable", "removal_prerequisite_unavailable", "removal_unexpected_activity":
		return value
	case "":
		return ""
	default:
		return "removal_completion_load_failed"
	}
}

func validSHA256ID(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	digest := strings.TrimPrefix(value, "sha256:")
	if strings.ToLower(digest) != digest {
		return false
	}
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size
}

func validPlanID(value string) bool {
	if len(value) != 24 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 12
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
	descriptor, knownDriver := downloader.DescribeLedgerDriver(before.Driver)
	if !knownDriver || bracket.RequestsMade != descriptor.OpenRequests+2+bracket.FileRequestsMade {
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
	result.relation.EvidenceBasis = append(result.relation.EvidenceBasis, descriptor.IdentityBasis, "bracketed_before_and_after_storage_proof", "non_atomic_observation")
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
	if _, ok := downloader.DescribeLedgerDriver(before.Driver); !ok || !before.Complete || !after.Complete || after.Driver != before.Driver || before.Capabilities != after.Capabilities {
		return false
	}
	if before.ObservedAtStart.IsZero() || before.ObservedAtEnd.IsZero() || after.ObservedAtStart.IsZero() || after.ObservedAtEnd.IsZero() ||
		before.ObservedAtEnd.Before(before.ObservedAtStart) || after.ObservedAtStart.Before(before.ObservedAtEnd) || after.ObservedAtEnd.Before(after.ObservedAtStart) {
		return false
	}
	return validLedgerJobs(before.Driver, before.Jobs) && validLedgerJobs(after.Driver, after.Jobs)
}

func validLedgerJobs(driver string, jobs []downloader.Torrent) bool {
	if _, ok := downloader.DescribeLedgerDriver(driver); !ok {
		return false
	}
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
		if !validDriverIdentityClaim(driver, job) {
			return false
		}
	}
	return true
}

func validDriverIdentityClaim(driver string, job downloader.Torrent) bool {
	switch driver {
	case downloader.DriverQBittorrent:
		if job.IdentityStatus != downloader.IdentityStatusValid {
			return true
		}
		var hasV1Evidence, hasV2Evidence bool
		for _, evidence := range job.IdentityEvidence {
			switch evidence {
			case "magnet_xt_btih_hex", "magnet_xt_btih_base32":
				hasV1Evidence = true
			case "magnet_xt_btmh_sha256":
				hasV2Evidence = true
			}
		}
		return (job.InfoHashV1 == "" || hasV1Evidence) && (job.InfoHashV2 == "" || hasV2Evidence)
	case downloader.DriverTransmission:
		if job.IdentityStatus != downloader.IdentityStatusValid || job.InfoHashV1 == "" || job.InfoHashV2 != "" || len(job.IdentityEvidence) != 1 ||
			job.IdentityEvidence[0] != "transmission_hash_string_sha1" || len(job.IdentityIssues) != 0 {
			return false
		}
		return true
	default:
		return false
	}
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

func assessSiteBinding(meta *metafile.MetaInfo, declared *domain.TorrentRef, selection SiteBindingSelection) (SiteLedger, Relation, []ReportFinding, []string) {
	relation := newRelation("site_metafile")
	blockers := []ReportFinding{}
	warnings := []string{}
	if !selection.Requested {
		if declared == nil {
			return SiteLedger{Status: "not_supplied"}, relationWithStatus(relation, "not_supplied"), blockers, warnings
		}
		ref := *declared
		ledger := SiteLedger{Status: "declared_unbound", Ref: &ref}
		relation.Status = "declared_unbound"
		relation.EvidenceLevel = "declared"
		relation.EvidenceBasis = append(relation.EvidenceBasis, "user_supplied_site_reference")
		relation.LeftIDs = append(relation.LeftIDs, ref.SiteID+"/"+ref.RemoteID)
		relation.RightIDs = append(relation.RightIDs, meta.MetafileVariantID)
		warnings = append(warnings, "the site reference is user-declared and is not bound to this exact metafile variant")
		return ledger, relation, blockers, warnings
	}

	ledger := SiteLedger{Status: "incomplete", BindingRecordID: selection.RecordID.String(), StopReason: safeSiteBindingStopReason(selection.StopReason)}
	relation.LeftIDs = append(relation.LeftIDs, selection.RecordID.String())
	relation.RightIDs = append(relation.RightIDs, meta.MetafileVariantID)
	if parsed, err := metastore.ParseRecordID(selection.RecordID.String()); err != nil || parsed != selection.RecordID {
		ledger.StopReason = "site_binding_identity_invalid"
		relation.Status = "incomplete"
		relation.BlockerCodes = append(relation.BlockerCodes, "site.binding_identity_invalid")
		blockers = append(blockers, ReportFinding{Code: "site.binding_identity_invalid", Message: "the explicit site binding record identity is invalid"})
		return ledger, relation, blockers, warnings
	}
	if selection.Verified == nil {
		switch ledger.StopReason {
		case "site_binding_mismatch":
			ledger.Status = "selected_binding_mismatch"
			relation.Status = "selected_binding_mismatch"
			relation.BlockerCodes = append(relation.BlockerCodes, "site.selected_binding_mismatch")
			blockers = append(blockers, ReportFinding{Code: "site.selected_binding_mismatch", Message: "the explicit historical site binding does not match the requested reference or metafile variant"})
		case "site_binding_integrity_failed":
			ledger.Status = "integrity_failed"
			relation.Status = "integrity_failed"
			relation.BlockerCodes = append(relation.BlockerCodes, "site.binding_integrity_failed")
			blockers = append(blockers, ReportFinding{Code: "site.binding_integrity_failed", Message: "the explicit site binding record or its linked metafile artifact failed integrity verification"})
		default:
			ledger.Status = "incomplete"
			relation.Status = "incomplete"
			relation.BlockerCodes = append(relation.BlockerCodes, "site.binding_proof_unavailable")
			blockers = append(blockers, ReportFinding{Code: "site.binding_proof_unavailable", Message: "the explicit site binding could not be verified in the current invocation"})
		}
		return ledger, relation, blockers, warnings
	}
	if !selection.Verified.Verified() {
		ledger.Status = "incomplete"
		ledger.StopReason = "site_binding_load_failed"
		relation.Status = "incomplete"
		relation.BlockerCodes = append(relation.BlockerCodes, "site.binding_proof_unavailable")
		blockers = append(blockers, ReportFinding{Code: "site.binding_proof_unavailable", Message: "the explicit site binding could not be verified in the current invocation"})
		return ledger, relation, blockers, warnings
	}

	public := selection.Verified.PublicCopy()
	record := public.Record
	ref := domain.TorrentRef{SiteID: record.SiteID, RemoteID: record.RemoteID}
	ledger.Ref = &ref
	ledger.StoreID = public.Store.StoreID
	ledger.MetafileVariantID = record.MetafileVariantID
	ledger.Origin = record.Origin
	ledger.RouteID = record.RouteID
	start, end := record.ObservedAtStart, record.ObservedAtEnd
	ledger.ObservedAtStart, ledger.ObservedAtEnd = &start, &end
	ledger.Historical = true
	requestedRef := ref
	if declared != nil {
		requestedRef = *declared
	}
	if !selection.Verified.Matches(selection.RecordID, ref, record.MetafileVariantID) ||
		public.RecordRef.ID != selection.RecordID || record.MetafileVariantID != meta.MetafileVariantID || requestedRef != ref {
		ledger.Status = "selected_binding_mismatch"
		ledger.StopReason = "site_binding_mismatch"
		relation.Status = "selected_binding_mismatch"
		relation.BlockerCodes = append(relation.BlockerCodes, "site.selected_binding_mismatch")
		relation.LeftIDs = append(relation.LeftIDs, ref.SiteID+"/"+ref.RemoteID)
		blockers = append(blockers, ReportFinding{Code: "site.selected_binding_mismatch", Message: "the explicit historical site binding does not match the requested reference or metafile variant"})
		return ledger, relation, blockers, warnings
	}
	ledger.Status = "historical_observed_exact_variant"
	ledger.ProcessLocalProof = true
	ledger.StopReason = ""
	relation.Status = "historical_observed_exact_variant"
	relation.EvidenceLevel = "persistent_direct_observation"
	relation.EvidenceBasis = append(relation.EvidenceBasis,
		"effectful_fetch_exact_response",
		"sealed_binding_record_digest_verified",
		"referenced_whole_raw_artifact_verified",
		"same_store_operation_bound_pair",
		"historical_non_atomic_site_observation",
	)
	relation.LeftIDs = append(relation.LeftIDs, ref.SiteID+"/"+ref.RemoteID)
	warnings = append(warnings, "the sealed site binding is a historical exact-response observation, not proof of the site's current mapping or a site signature")
	return ledger, relation, blockers, warnings
}

func assessSiteDetail(declared *domain.TorrentRef, selection SiteDetailSelection) (SiteDetailLedger, []ReportFinding, []string) {
	ledger := SiteDetailLedger{
		Status: "not_requested",
		Limits: site.DefaultTorrentDetailLimits(),
		Used:   site.TorrentDetailUsage{},
	}
	blockers := []ReportFinding{}
	warnings := []string{}
	if !selection.Requested {
		return ledger, blockers, warnings
	}
	ledger.Status = "incomplete"
	ledger.RequestsMade = boundedSiteDetailCount(selection.RequestsMade)
	ledger.StopReason = safeSiteDetailStopReason(selection.StopReason)
	if ledger.RequestsMade > 0 {
		warnings = append(warnings, "the live site read was remote-visible even though reconciliation performed zero local writes")
	}
	receipt := selection.Receipt
	if receipt.Limits.Validate() == nil {
		ledger.Limits = receipt.Limits
	}
	ledger.Used = site.TorrentDetailUsage{
		RequestsAttempted:  boundedSiteDetailCount(receipt.Used.RequestsAttempted),
		AutomaticRetries:   boundedSiteDetailCount(receipt.Used.AutomaticRetries),
		RedirectsFollowed:  boundedSiteDetailCount(receipt.Used.RedirectsFollowed),
		ResponseBytesRead:  boundedSiteDetailBytes(receipt.Used.ResponseBytesRead, ledger.Limits.MaxResponseBytes),
		ResponseBytesKnown: receipt.Used.ResponseBytesKnown,
	}
	if declared == nil {
		ledger.StopReason = "site_detail_reference_missing"
		blockers = append(blockers, ReportFinding{Code: "site.detail_reference_missing", Message: "a live site detail observation requires an explicit site reference"})
		return ledger, blockers, warnings
	}
	ref := *declared
	ledger.Ref = &ref
	config := selection.Config
	configErr := config.Validate()
	if configErr == nil {
		ledger.Origin = config.Origin
		ledger.RouteID = config.RouteID
	}
	if selection.Observed == nil {
		if ledger.StopReason == "" {
			ledger.StopReason = "site_detail_observation_incomplete"
		}
		blockers = append(blockers, ReportFinding{Code: "site.detail_observation_incomplete", Message: "the requested live site detail observation did not complete"})
		return ledger, blockers, warnings
	}
	if configErr != nil || selection.StopReason != "" || receipt.Ref != ref || receipt.Origin != config.Origin || receipt.RouteID != config.RouteID ||
		receipt.Limits != ledger.Limits || selection.RequestsMade != 1 || !selection.Observed.MatchesReceipt(receipt) ||
		!selection.Observed.Matches(ref, config.Origin, config.RouteID) {
		ledger.StopReason = "site_detail_receipt_inconsistent"
		blockers = append(blockers, ReportFinding{Code: "site.detail_receipt_inconsistent", Message: "the live site detail authority and bounded request receipt did not agree"})
		return ledger, blockers, warnings
	}
	public := selection.Observed.PublicCopy()
	start, end := receipt.ObservedAtStart.UTC(), receipt.ObservedAtEnd.UTC()
	ledger.Status = "observed_current_ref"
	ledger.ObservedAtStart = &start
	ledger.ObservedAtEnd = &end
	ledger.Observation = &public
	ledger.ProcessLocalProof = true
	ledger.StopReason = ""
	warnings = append(warnings,
		"the live site detail is a same-invocation site claim for the remote ID, not proof that the site currently serves this exact metafile variant",
		"the live site request and the storage/downloader observations are non-atomic",
	)
	return ledger, blockers, warnings
}

func relationWithStatus(relation Relation, status string) Relation {
	relation.Status = status
	return relation
}

func siteBindingSelector(requested bool) string {
	if requested {
		return "explicit_record"
	}
	return "not_requested"
}

func safeSiteBindingStopReason(value string) string {
	switch value {
	case "site_binding_load_failed", "site_binding_integrity_failed", "site_binding_adapter_mismatch", "site_binding_mismatch":
		return value
	case "":
		return ""
	default:
		return "site_binding_load_failed"
	}
}

func safeSiteDetailStopReason(value string) string {
	switch value {
	case "invalid_limits", "invalid_reference", "context_done", "session_closed", "request_budget_exhausted", "request_accounting_invalid", "site_request_failed", "authentication_required", "not_found", "rate_limited", "redirect_rejected", "http_status_rejected", "empty_response", "challenge_response", "unrecognized_response", "content_type_rejected", "invalid_text_encoding", "response_accounting_invalid", "session_open_failed", "session_close_failed", "receipt_inconsistent", "site_detail_observation_incomplete", "site_detail_receipt_inconsistent", "site_detail_reference_missing", "site_detail_skipped_by_binding_gate", "site_detail_skipped_by_prerequisite_gate":
		return value
	case "":
		return ""
	default:
		return "site_detail_observation_incomplete"
	}
}

func boundedSiteDetailCount(value int) int {
	if value < 0 {
		return 0
	}
	if value > 2 {
		return 2
	}
	return value
}

func boundedSiteDetailBytes(value, maximum int64) int64 {
	if value < 0 {
		return 0
	}
	if maximum <= 0 {
		maximum = site.DefaultTorrentDetailLimits().MaxResponseBytes
	}
	if value > maximum+1 {
		return maximum + 1
	}
	return value
}

func overallOutcome(siteStatus string, siteBindingRequested bool, siteDetailStatus string, siteDetailRequested bool, storageStatus string, processProof bool,
	clientStatus, pathStatus string, clientRequested bool, activationStatus string, activationRequested bool,
	removalStatus string, removalRequested bool,
	retirementStatus string, retirementRequested bool) string {
	if (siteBindingRequested && siteStatus == "integrity_failed") || storageStatus == "integrity_failed" ||
		(activationRequested && activationStatus == "integrity_failed") ||
		(removalRequested && removalStatus == "integrity_failed") ||
		(retirementRequested && retirementStatus == "integrity_failed") {
		return "integrity_failed"
	}
	if (siteBindingRequested && siteStatus == "selected_binding_mismatch") ||
		(activationRequested && activationStatus == "selected_activation_mismatch") ||
		(removalRequested && removalStatus == "selected_removal_mismatch") ||
		(retirementRequested && (retirementStatus == "selected_retirement_mismatch" || retirementStatus == "source_name_reappeared")) ||
		clientStatus == "conflict" || pathStatus == "client_size_conflict" || pathStatus == "client_file_layout_conflict" {
		return "conflict"
	}
	if storageStatus == "verified_ambiguous" || clientStatus == "ambiguous" {
		return "ambiguous"
	}
	if (siteBindingRequested && siteStatus != "historical_observed_exact_variant") ||
		(siteDetailRequested && siteDetailStatus != "observed_current_ref") ||
		(activationRequested && activationStatus != "historical_completion_current_job_bound" && activationStatus != "historical_completion_current_job_absent") ||
		(removalRequested && removalStatus != "historical_keep_data_removal_current_job_absent") ||
		(retirementRequested && retirementStatus != "historical_completion_current_absence_observed") ||
		storageStatus == "incomplete" || (verifiedStorageOutcome(storageStatus) && !processProof) ||
		(clientRequested && clientStatus == "incomplete") || pathStatus == "incomplete" {
		return "incomplete"
	}
	if processProof && (clientStatus == "exact_unique" && pathStatus == "same_location" ||
		removalRequested && removalStatus == "historical_keep_data_removal_current_job_absent" &&
			clientStatus == "absent" && activationStatus == "historical_completion_current_job_absent" && pathStatus == "not_comparable") {
		return "consistent"
	}
	return "partial"
}

func verifiedStorageOutcome(status string) bool {
	return status == "verified_unique" || status == "verified_exact_root" || status == "verified_materialized_final"
}

func relationDependencyStatus(status string) string {
	switch status {
	case "verified_ambiguous", "ambiguous":
		return "ambiguous"
	case "incomplete", "conflict":
		return status
	case "integrity_failed":
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

// PathMappingFingerprint returns the same invocation-scoped lexical mapping
// identity recorded by Build. It contains no raw host or client path.
func PathMappingFingerprint(value PathMappingOptions) string { return pathMappingID(value) }

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
		case "magnet_xt_btih_hex", "magnet_xt_btih_base32", "magnet_xt_btmh_sha256", "transmission_hash_string_sha1":
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

func withoutString(items []string, removed string) []string {
	result := make([]string, 0, len(items))
	for _, item := range items {
		if item != removed {
			result = append(result, item)
		}
	}
	return result
}
