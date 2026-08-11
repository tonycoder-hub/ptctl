package seed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

type Operation struct {
	ManifestIndex      int                          `json:"manifest_index"`
	TorrentPath        string                       `json:"torrent_path"`
	Kind               string                       `json:"kind"`
	Source             string                       `json:"source,omitempty"`
	SourcePrecondition *metafile.SourcePrecondition `json:"source_precondition,omitempty"`
	Target             string                       `json:"target"`
	ClientTarget       string                       `json:"client_target,omitempty"`
	Bytes              int64                        `json:"bytes"`
}

type Plan struct {
	ID                 string                      `json:"id"`
	TorrentName        string                      `json:"torrent_name"`
	InfoHashV1         string                      `json:"info_hash_v1,omitempty"`
	InfoHashV2         string                      `json:"info_hash_v2,omitempty"`
	MetafileVariantID  string                      `json:"metafile_variant_id"`
	Evidence           string                      `json:"evidence"`
	Effect             string                      `json:"effect"`
	ReadyToApply       bool                        `json:"ready_to_apply"`
	Readiness          string                      `json:"readiness"`
	SourceMode         string                      `json:"source_mode"`
	SourceSelectionID  string                      `json:"source_selection_id,omitempty"`
	SourceRoot         string                      `json:"source_root,omitempty"`
	TargetRoot         string                      `json:"target_root"`
	TargetRootIdentity string                      `json:"target_root_identity,omitempty"`
	ClientMapping      string                      `json:"client_mapping"`
	Strategy           string                      `json:"strategy"`
	EstimatedRead      int64                       `json:"estimated_read_bytes"`
	EstimatedWrite     int64                       `json:"estimated_write_bytes"`
	Operations         []Operation                 `json:"operations"`
	Verification       metafile.VerificationResult `json:"verification"`
	Warnings           []string                    `json:"warnings,omitempty"`
	Blockers           []string                    `json:"blockers"`
}

var ErrSourceIntegrity = errors.New("source content failed exact torrent verification")

// BuildMaterializePlan is read-only. It performs the exact v1 and/or v2
// verification required by the metafile and produces a plan, but never creates
// directories, links, or files.
func BuildMaterializePlan(ctx context.Context, meta *metafile.MetaInfo, sourceRoot, targetRoot, strategy string) (Plan, error) {
	plan, _, err := BuildMaterializePlanWithExactSource(ctx, meta, sourceRoot, targetRoot, strategy)
	return plan, err
}

// BuildMaterializePlanWithExactSource verifies one caller-selected exact
// source root and returns both the read-only plan and the process-local source
// authority created by that same proof. The authority is intentionally not
// serializable: a later materialize invocation must call this function again
// and reproduce the reviewed plan ID before it may write anything. If source
// proof succeeds but target-plan construction fails, the verified authority is
// returned with the error so an auditing caller can attribute the failure to
// planning without treating the absent plan as usable.
func BuildMaterializePlanWithExactSource(ctx context.Context, meta *metafile.MetaInfo, sourceRoot, targetRoot, strategy string) (Plan, *metafile.VerifiedSource, error) {
	verified, err := metafile.VerifyContentSource(ctx, meta, sourceRoot)
	if err != nil {
		return Plan{}, nil, err
	}
	if !verified.Result().Verified {
		return Plan{}, nil, ErrSourceIntegrity
	}
	sourceRoot, err = filepath.Abs(sourceRoot)
	if err != nil {
		return Plan{}, verified, fmt.Errorf("resolve source root: %w", err)
	}
	sourceRoot = filepath.Clean(sourceRoot)
	plan, err := buildMaterializePlan(ctx, meta, verified, "exact_root", "", sourceRoot, targetRoot, strategy)
	if err != nil {
		return Plan{}, verified, err
	}
	return plan, verified, nil
}

// BuildMaterializePlanFromVerified consumes an opaque mapped verification
// observation. It supports sources scattered across multiple storage roots and
// remains strictly read-only.
func BuildMaterializePlanFromVerified(ctx context.Context, meta *metafile.MetaInfo, verified *metafile.VerifiedSource, targetRoot, strategy string) (Plan, error) {
	return buildMaterializePlan(ctx, meta, verified, "discovered_map", "", "", targetRoot, strategy)
}

// buildMaterializePlanFromIndexedSelection consumes one explicitly selected,
// live-reverified source assignment whose immutable profile/snapshot/match
// scope is bound by sourceSelectionID. It does not claim current-filesystem
// uniqueness or absence.
func buildMaterializePlanFromIndexedSelection(ctx context.Context, meta *metafile.MetaInfo, verified *metafile.VerifiedSource, targetRoot, strategy, sourceSelectionID string) (Plan, error) {
	if !canonicalIndexedSourceSelectionID(sourceSelectionID) {
		return Plan{}, fmt.Errorf("indexed source selection identity is invalid")
	}
	return buildMaterializePlan(ctx, meta, verified, "indexed_explicit_map", sourceSelectionID, "", targetRoot, strategy)
}

func buildMaterializePlan(ctx context.Context, meta *metafile.MetaInfo, verified *metafile.VerifiedSource, sourceMode, sourceSelectionID, sourceRoot, targetRoot, strategy string) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	if strategy == "" {
		strategy = "copy"
	}
	if strategy != "copy" {
		return Plan{}, fmt.Errorf("the alpha supports only the safe copy strategy; hardlink and symlink remain opt-in future capabilities")
	}
	if verified == nil || !verified.Matches(meta) {
		return Plan{}, fmt.Errorf("verified source observation does not match this metafile variant")
	}
	verification := verified.Result()
	if !verification.Verified {
		return Plan{}, ErrSourceIntegrity
	}
	targetProbe, err := storage.ProbeReadOnly(targetRoot)
	if err != nil {
		return Plan{}, err
	}
	semantics := targetProbe.Semantics
	targetRootIdentity := ""
	if targetSession, targetInfo, bindErr := fsbind.BindExisting(targetProbe.ResolvedPath); bindErr == nil {
		targetRootIdentity = targetInfo.Identity.String()
		_ = targetSession.Close()
	}
	allTargetPaths := make([][][]byte, 0, len(meta.Files))
	for _, file := range meta.Files {
		components := targetComponents(meta, file)
		allTargetPaths = append(allTargetPaths, components)
	}
	if err := storage.ValidateManifestPaths(allTargetPaths, semantics); err != nil {
		return Plan{}, fmt.Errorf("unsafe target layout: %w", err)
	}

	plan := Plan{
		TorrentName:        meta.Name,
		InfoHashV1:         meta.InfoHashV1,
		InfoHashV2:         meta.InfoHashV2,
		MetafileVariantID:  meta.MetafileVariantID,
		Evidence:           planEvidence(meta.Version),
		Effect:             "none",
		ReadyToApply:       false,
		Readiness:          "layout_only",
		SourceMode:         sourceMode,
		SourceSelectionID:  sourceSelectionID,
		SourceRoot:         sourceRoot,
		TargetRoot:         targetProbe.ResolvedPath,
		TargetRootIdentity: targetRootIdentity,
		ClientMapping:      "not_requested",
		Strategy:           strategy,
		Verification:       verification,
		Warnings: []string{
			"plan only: no filesystem changes were made",
			"journaled materialize is a separate explicit write command and never treats this serialized plan as source-proof authority",
		},
		Blockers: []string{
			"target filesystem semantics were inferred from the host OS, not measured for this storage root",
			"no host-to-downloader path mapping or downloader job was reconciled",
			"no site release identity was bound to the local metafile artifact",
			"journaled materialize requires a separately reviewed matching plan ID: seed plan for exact-root mode or seed discover --target for discovered/indexed mode; the writing invocation repeats exact source verification",
		},
	}
	if sourceMode == "exact_root" {
		plan.Warnings = append(plan.Warnings, "this plan ID can select seed materialize --source only when the writing invocation reopens the same exact source root and reproduces the plan")
		plan.Blockers = append(plan.Blockers, "the serialized plan retains no process-local source capability and cannot authorize a write by itself")
	}
	plan.Warnings = append(plan.Warnings, targetProbe.Warnings...)
	if targetRootIdentity == "" {
		plan.Warnings = append(plan.Warnings, "the target root could not provide an opaque bound filesystem identity; journaled materialize will remain blocked")
		plan.Blockers = append(plan.Blockers, "the target root lacks the bound identity required by journaled materialize")
	} else {
		plan.Warnings = append(plan.Warnings, "the plan ID includes an opaque read-only observation of the target-root filesystem identity")
	}
	for fileIndex, file := range meta.Files {
		if err := ctx.Err(); err != nil {
			return Plan{}, err
		}
		target, err := storage.PlannedJoin(targetProbe.ResolvedPath, targetComponents(meta, file), semantics)
		if err != nil {
			return Plan{}, err
		}
		if err := rejectSymlinkPrefix(targetProbe.ResolvedPath, target); err != nil {
			return Plan{}, err
		}
		if _, err := os.Lstat(target); err == nil {
			return Plan{}, fmt.Errorf("target already exists and conflict policy is fail: %q", target)
		} else if !os.IsNotExist(err) {
			return Plan{}, fmt.Errorf("inspect target %q: %w", target, err)
		}
		operation := Operation{ManifestIndex: fileIndex, TorrentPath: strings.Join(file.Path, "/"), Kind: strategy, Target: target, Bytes: file.Length}
		if strings.Contains(file.Attribute, "p") {
			operation.Kind = "padding"
		} else if file.Length == 0 {
			operation.Kind = "empty"
		} else {
			source, ok := verified.Path(fileIndex)
			if !ok {
				return Plan{}, fmt.Errorf("verified source has no binding for manifest file %d", fileIndex)
			}
			operation.Source = source
			precondition, err := verified.SourcePrecondition(fileIndex)
			if err != nil {
				return Plan{}, err
			}
			operation.SourcePrecondition = &precondition
			plan.EstimatedRead += file.Length
		}
		plan.EstimatedWrite += file.Length
		plan.Operations = append(plan.Operations, operation)
	}
	plan.ID = planID(plan)
	return plan, nil
}

func targetComponents(meta *metafile.MetaInfo, file metafile.File) [][]byte {
	if meta.MultiFile {
		components := make([][]byte, 0, len(file.RawPath)+1)
		components = append(components, append([]byte(nil), meta.NameRaw...))
		for _, component := range file.RawPath {
			components = append(components, append([]byte(nil), component...))
		}
		return components
	}
	return [][]byte{append([]byte(nil), meta.NameRaw...)}
}

func rejectSymlinkPrefix(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	current := root
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts[:max(0, len(parts)-1)] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect target prefix %q: %w", current, err)
		}
		if storage.IsLinkLike(info) {
			return fmt.Errorf("target prefix is a symlink or reparse point: %q", current)
		}
	}
	return nil
}

func planID(plan Plan) string {
	lines := []string{plan.InfoHashV1, plan.InfoHashV2, plan.MetafileVariantID, plan.Verification.SourceSnapshotID, plan.SourceMode, plan.SourceRoot, plan.TargetRoot, plan.TargetRootIdentity, plan.Readiness, plan.ClientMapping}
	if plan.SourceSelectionID != "" {
		lines = append(lines, "source-selection\x00"+plan.SourceSelectionID)
	}
	operations := make([]string, 0, len(plan.Operations))
	for _, operation := range plan.Operations {
		operations = append(operations, fmt.Sprint(operation.ManifestIndex)+"\x00"+operation.Kind+"\x00"+operation.Source+"\x00"+operation.Target+"\x00"+operation.ClientTarget+"\x00"+fmt.Sprint(operation.Bytes))
	}
	sort.Strings(operations)
	lines = append(lines, operations...)
	digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(digest[:12])
}

func planEvidence(version string) string {
	switch version {
	case "v1":
		return "source_observation:v1_piece_verified"
	case "v2":
		return "source_observation:v2_merkle_verified"
	case "hybrid":
		return "source_observation:single_pass_v1_piece_and_v2_merkle_verified"
	default:
		return "source_observation:unsupported"
	}
}

// MapPlanTargets adds a lexical host-to-client namespace projection. It does
// not contact the downloader and therefore cannot prove reachability.
func MapPlanTargets(plan Plan, hostRoot, clientRoot string, clientWindows bool) (Plan, error) {
	operations := append([]Operation(nil), plan.Operations...)
	for i := range operations {
		mapping, err := storage.MapHostToClient(hostRoot, operations[i].Target, clientRoot, clientWindows)
		if err != nil {
			return plan, fmt.Errorf("map target for manifest file %d: %w", operations[i].ManifestIndex, err)
		}
		operations[i].ClientTarget = mapping.ClientPath
	}
	plan.Operations = operations
	plan.ClientMapping = "lexical_only"
	plan.Blockers = removeString(plan.Blockers, "no host-to-downloader path mapping or downloader job was reconciled")
	plan.Blockers = append(plan.Blockers, "client paths were mapped lexically; downloader reachability and job state remain unknown")
	plan.Warnings = append(plan.Warnings, "host-to-client mapping is lexical evidence only and did not contact a downloader")
	plan.ID = planID(plan)
	return plan, nil
}

func removeString(items []string, target string) []string {
	result := make([]string, 0, len(items))
	for _, item := range items {
		if item != target {
			result = append(result, item)
		}
	}
	return result
}
