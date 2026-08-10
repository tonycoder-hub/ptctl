// Package sourceretire builds zero-write, same-invocation eligibility plans for
// retiring explicitly reverified source file names. It deliberately exposes no
// deletion primitive or durable deletion authority.
package sourceretire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

const (
	PlanSchemaV1 = "ptctl.source-retirement-plan/v1"
	PlanModeV1   = "review_only_explicit_named_regular_files"

	OutcomeEligible        = "eligible_for_separate_review"
	OutcomeBlocked         = "blocked"
	OutcomeIncomplete      = "incomplete"
	OutcomeIntegrityFailed = "integrity_failed"
)

var (
	ErrPolicy     = errors.New("source retirement planning is blocked by policy")
	ErrIntegrity  = errors.New("source retirement planning failed integrity validation")
	ErrIncomplete = errors.New("source retirement planning is incomplete")
)

type Finding struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type SourceFile struct {
	ManifestIndex  int       `json:"manifest_index"`
	SizeBytes      int64     `json:"size_bytes"`
	ModifiedAt     time.Time `json:"modified_at"`
	SourcePathRef  string    `json:"source_path_ref"`
	SourcePath     string    `json:"source_path,omitempty"`
	DistinctObject bool      `json:"distinct_from_materialized_final"`
}

type Plan struct {
	Schema                 string       `json:"schema"`
	ID                     string       `json:"id"`
	Mode                   string       `json:"mode"`
	DeletionAuthority      string       `json:"deletion_authority"`
	MetafileVariantID      string       `json:"metafile_variant_id"`
	InfoHashV1             string       `json:"info_hash_v1,omitempty"`
	InfoHashV2             string       `json:"info_hash_v2,omitempty"`
	MaterializeOperationID string       `json:"materialize_operation_id"`
	MaterializePlanID      string       `json:"materialize_plan_id"`
	ActivationOperationID  string       `json:"activation_operation_id"`
	ActivationPlanID       string       `json:"activation_plan_id"`
	ClientCompletionID     string       `json:"client_completion_id"`
	CurrentClientUseID     string       `json:"current_client_use_id"`
	SourceSelectionID      string       `json:"source_selection_id"`
	TargetRootIdentity     string       `json:"target_root_identity"`
	FinalObjectIdentity    string       `json:"final_object_identity"`
	ManifestFiles          int          `json:"manifest_files"`
	PhysicalSourceFiles    int          `json:"physical_source_files"`
	ContentBytes           int64        `json:"content_bytes"`
	AbsolutePathsShown     bool         `json:"absolute_paths_shown"`
	SourceFiles            []SourceFile `json:"source_files"`
	EvidenceBasis          []string     `json:"evidence_basis"`
}

func (plan Plan) Validate() error {
	if plan.Schema != PlanSchemaV1 || plan.Mode != PlanModeV1 || plan.DeletionAuthority != "none" ||
		!canonicalSHA256ID(plan.ID) || !canonicalSHA256ID(plan.MetafileVariantID) ||
		!canonicalSHA256ID(plan.MaterializeOperationID) || !canonicalPlanID(plan.MaterializePlanID) ||
		!canonicalSHA256ID(plan.ActivationOperationID) || !canonicalPlanID(plan.ActivationPlanID) ||
		!canonicalSHA256ID(plan.ClientCompletionID) || !canonicalSHA256ID(plan.CurrentClientUseID) ||
		!canonicalSHA256ID(plan.SourceSelectionID) ||
		plan.TargetRootIdentity == "" || plan.FinalObjectIdentity == "" || plan.ManifestFiles <= 0 ||
		plan.PhysicalSourceFiles <= 0 || plan.PhysicalSourceFiles > plan.ManifestFiles || plan.ContentBytes <= 0 ||
		len(plan.SourceFiles) != plan.PhysicalSourceFiles {
		return fmt.Errorf("%w: source retirement plan is invalid", ErrIntegrity)
	}
	if (plan.InfoHashV1 == "" && plan.InfoHashV2 == "") ||
		(plan.InfoHashV1 != "" && !canonicalHex(plan.InfoHashV1, 40)) ||
		(plan.InfoHashV2 != "" && !canonicalHex(plan.InfoHashV2, 64)) {
		return fmt.Errorf("%w: source retirement typed identity is invalid", ErrIntegrity)
	}
	seen := make(map[string]struct{}, len(plan.SourceFiles))
	previous := -1
	var total int64
	for _, file := range plan.SourceFiles {
		if file.ManifestIndex <= previous || file.ManifestIndex >= plan.ManifestFiles || file.SizeBytes <= 0 || file.ModifiedAt.IsZero() ||
			!canonicalSHA256ID(file.SourcePathRef) || !file.DistinctObject ||
			(file.SourcePath != "" && !filepath.IsAbs(file.SourcePath)) {
			return fmt.Errorf("%w: source retirement file entry is invalid", ErrIntegrity)
		}
		if plan.AbsolutePathsShown != (file.SourcePath != "") ||
			file.SourcePath != "" && sourcePathRef(file.SourcePath) != file.SourcePathRef {
			return fmt.Errorf("%w: source retirement path disclosure disagrees with its reference", ErrIntegrity)
		}
		if _, duplicate := seen[file.SourcePathRef]; duplicate {
			return fmt.Errorf("%w: source retirement file path is duplicated", ErrIntegrity)
		}
		seen[file.SourcePathRef] = struct{}{}
		previous = file.ManifestIndex
		total += file.SizeBytes
		if total < 0 || total > plan.ContentBytes {
			return fmt.Errorf("%w: source retirement byte total is invalid", ErrIntegrity)
		}
	}
	if total != plan.ContentBytes || len(plan.EvidenceBasis) == 0 {
		return fmt.Errorf("%w: source retirement evidence is incomplete", ErrIntegrity)
	}
	if root, err := fsbind.ParseIdentity(plan.TargetRootIdentity); err != nil || root.IsZero() {
		return fmt.Errorf("%w: source retirement target identity is invalid", ErrIntegrity)
	}
	if final, err := fsbind.ParseIdentity(plan.FinalObjectIdentity); err != nil || final.IsZero() {
		return fmt.Errorf("%w: source retirement final identity is invalid", ErrIntegrity)
	}
	computed, err := planID(plan)
	if err != nil || computed != plan.ID {
		return fmt.Errorf("%w: source retirement plan ID disagrees", ErrIntegrity)
	}
	return nil
}

type SourceReport struct {
	Status        string `json:"status"`
	SelectionID   string `json:"selection_id,omitempty"`
	PhysicalFiles int    `json:"physical_files"`
	ContentBytes  int64  `json:"content_bytes"`
	Assurance     string `json:"assurance"`
}

type SourceScanReport struct {
	Complete             bool                       `json:"complete"`
	VerificationComplete bool                       `json:"verification_complete"`
	TimeBudgetMillis     int64                      `json:"time_budget_millis"`
	PathConfinement      string                     `json:"path_confinement"`
	InventoryLimits      storage.InventoryLimits    `json:"inventory_limits"`
	MatchLimits          metafile.SourceMatchLimits `json:"match_limits"`
	InventoryUsed        storage.InventoryStats     `json:"inventory_used"`
	MatchUsed            metafile.SourceMatchStats  `json:"match_used"`
	StopReasons          []string                   `json:"stop_reasons"`
	InventoryIssueCount  int                        `json:"inventory_issue_count"`
	MatchIssueCount      int                        `json:"match_issue_count"`
}

type ClientUseReport struct {
	Status       string                               `json:"status"`
	RequestsMade int                                  `json:"requests_made"`
	Stable       bool                                 `json:"stable"`
	Before       clientactivate.CurrentUseObservation `json:"before"`
	After        clientactivate.CurrentUseObservation `json:"after"`
	Assurance    string                               `json:"assurance"`
}

type Report struct {
	Outcome           string                               `json:"outcome"`
	Effect            []string                             `json:"effect"`
	WritesPerformed   int                                  `json:"writes_performed"`
	DeletionPerformed bool                                 `json:"deletion_performed"`
	Plan              Plan                                 `json:"plan"`
	Source            SourceReport                         `json:"source"`
	Scan              SourceScanReport                     `json:"source_scan"`
	Final             materialize.FinalObservation         `json:"materialized_final"`
	Activation        clientactivate.CompletionObservation `json:"client_completion"`
	ClientUse         ClientUseReport                      `json:"current_client_use"`
	Blockers          []Finding                            `json:"blockers"`
	Issues            []Finding                            `json:"issues"`
	Warnings          []string                             `json:"warnings"`
}

func newReport() Report {
	return Report{
		Outcome: OutcomeIncomplete,
		Effect: []string{"read_source_metadata", "read_source_content", "read_exact_materialized_final",
			"read_private_client_activation_journal"},
		WritesPerformed: 0, DeletionPerformed: false,
		Plan: Plan{Schema: PlanSchemaV1, Mode: PlanModeV1, DeletionAuthority: "none",
			SourceFiles: []SourceFile{}, EvidenceBasis: []string{}},
		Source:    SourceReport{Status: "not_observed", Assurance: "not_observed"},
		Scan:      SourceScanReport{StopReasons: []string{}},
		ClientUse: ClientUseReport{Status: "not_observed", Assurance: "not_observed"},
		Blockers:  []Finding{}, Issues: []Finding{},
		Warnings: []string{
			"this report performs zero writes and grants no deletion authority",
			"only explicit content-bearing regular-file names are candidates; directories, empty files, and padding are not retired",
			"unselected hardlink or alias names remain untouched; the plan does not claim that underlying storage would be reclaimed",
			"live downloader state and effective paths are bounded self-reported lexical claims; they do not prove remote filesystem reachability or an open inode",
			"typed downloader identity does not make the downloader's raw private metafile variant observable",
			"source, final, and client observations are same-invocation or historical brackets, not one atomic snapshot",
			"a future deletion command must rebuild the same plan from live proof and require a separate acknowledgement and journal",
			"client, job, layout, mapping, and path references are stable pseudonyms and may be dictionary-guessable; they are not anonymization",
		},
	}
}

func (report *Report) addBlocker(code, message string) {
	report.Blockers = append(report.Blockers, Finding{Code: code, Message: message})
}

func (report *Report) addIssue(code, message string) {
	report.Issues = append(report.Issues, Finding{Code: code, Message: message})
}

func (report *Report) finalize() {
	if report.Effect == nil {
		report.Effect = []string{}
	}
	if report.Plan.SourceFiles == nil {
		report.Plan.SourceFiles = []SourceFile{}
	}
	if report.Plan.EvidenceBasis == nil {
		report.Plan.EvidenceBasis = []string{}
	}
	if report.Scan.StopReasons == nil {
		report.Scan.StopReasons = []string{}
	}
	if report.Blockers == nil {
		report.Blockers = []Finding{}
	}
	if report.Issues == nil {
		report.Issues = []Finding{}
	}
	if report.Warnings == nil {
		report.Warnings = []string{}
	}
	sort.Slice(report.Blockers, func(i, j int) bool {
		if report.Blockers[i].Code != report.Blockers[j].Code {
			return report.Blockers[i].Code < report.Blockers[j].Code
		}
		return report.Blockers[i].Message < report.Blockers[j].Message
	})
	sort.Slice(report.Issues, func(i, j int) bool {
		if report.Issues[i].Code != report.Issues[j].Code {
			return report.Issues[i].Code < report.Issues[j].Code
		}
		return report.Issues[i].Message < report.Issues[j].Message
	})
	sort.Strings(report.Warnings)
}

func planID(plan Plan) (string, error) {
	copyPlan := plan
	copyPlan.ID = ""
	copyPlan.AbsolutePathsShown = false
	copyPlan.SourceFiles = append([]SourceFile(nil), plan.SourceFiles...)
	for i := range copyPlan.SourceFiles {
		copyPlan.SourceFiles[i].SourcePath = ""
	}
	raw, err := json.Marshal(copyPlan)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte("ptctl-source-retirement-plan-v1\x00"), raw...))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func sourcePathRef(path string) string {
	digest := sha256.Sum256([]byte("ptctl-source-retirement-path-v1\x00" + filepath.Clean(path)))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func canonicalSHA256ID(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	digest := strings.TrimPrefix(value, "sha256:")
	if strings.ToLower(digest) != digest {
		return false
	}
	raw, err := hex.DecodeString(digest)
	return err == nil && len(raw) == sha256.Size
}

func canonicalPlanID(value string) bool {
	if len(value) != 24 || strings.ToLower(value) != value {
		return false
	}
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 12
}

func canonicalHex(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
