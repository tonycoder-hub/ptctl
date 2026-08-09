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
	ID                string                      `json:"id"`
	TorrentName       string                      `json:"torrent_name"`
	InfoHashV1        string                      `json:"info_hash_v1,omitempty"`
	InfoHashV2        string                      `json:"info_hash_v2,omitempty"`
	MetafileVariantID string                      `json:"metafile_variant_id"`
	Evidence          string                      `json:"evidence"`
	Effect            string                      `json:"effect"`
	ReadyToApply      bool                        `json:"ready_to_apply"`
	Readiness         string                      `json:"readiness"`
	SourceMode        string                      `json:"source_mode"`
	SourceRoot        string                      `json:"source_root,omitempty"`
	TargetRoot        string                      `json:"target_root"`
	ClientMapping     string                      `json:"client_mapping"`
	Strategy          string                      `json:"strategy"`
	EstimatedRead     int64                       `json:"estimated_read_bytes"`
	EstimatedWrite    int64                       `json:"estimated_write_bytes"`
	Operations        []Operation                 `json:"operations"`
	Verification      metafile.VerificationResult `json:"verification"`
	Warnings          []string                    `json:"warnings,omitempty"`
	Blockers          []string                    `json:"blockers"`
}

var ErrSourceIntegrity = errors.New("source content failed exact torrent verification")

// BuildMaterializePlan is read-only. It performs the exact v1 and/or v2
// verification required by the metafile and produces a plan, but never creates
// directories, links, or files.
func BuildMaterializePlan(ctx context.Context, meta *metafile.MetaInfo, sourceRoot, targetRoot, strategy string) (Plan, error) {
	verified, err := metafile.VerifyContentSource(ctx, meta, sourceRoot)
	if err != nil {
		return Plan{}, err
	}
	if !verified.Result().Verified {
		return Plan{}, ErrSourceIntegrity
	}
	sourceRoot, err = filepath.Abs(sourceRoot)
	if err != nil {
		return Plan{}, fmt.Errorf("resolve source root: %w", err)
	}
	sourceRoot = filepath.Clean(sourceRoot)
	return buildMaterializePlan(ctx, meta, verified, "exact_root", sourceRoot, targetRoot, strategy)
}

// BuildMaterializePlanFromVerified consumes an opaque mapped verification
// observation. It supports sources scattered across multiple storage roots and
// remains strictly read-only.
func BuildMaterializePlanFromVerified(ctx context.Context, meta *metafile.MetaInfo, verified *metafile.VerifiedSource, targetRoot, strategy string) (Plan, error) {
	return buildMaterializePlan(ctx, meta, verified, "discovered_map", "", targetRoot, strategy)
}

func buildMaterializePlan(ctx context.Context, meta *metafile.MetaInfo, verified *metafile.VerifiedSource, sourceMode, sourceRoot, targetRoot, strategy string) (Plan, error) {
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
	allTargetPaths := make([][][]byte, 0, len(meta.Files))
	for _, file := range meta.Files {
		components := targetComponents(meta, file)
		allTargetPaths = append(allTargetPaths, components)
	}
	if err := storage.ValidateManifestPaths(allTargetPaths, semantics); err != nil {
		return Plan{}, fmt.Errorf("unsafe target layout: %w", err)
	}

	plan := Plan{
		TorrentName:       meta.Name,
		InfoHashV1:        meta.InfoHashV1,
		InfoHashV2:        meta.InfoHashV2,
		MetafileVariantID: meta.MetafileVariantID,
		Evidence:          planEvidence(meta.Version),
		Effect:            "none",
		ReadyToApply:      false,
		Readiness:         "layout_only",
		SourceMode:        sourceMode,
		SourceRoot:        sourceRoot,
		TargetRoot:        targetProbe.ResolvedPath,
		ClientMapping:     "not_requested",
		Strategy:          strategy,
		Verification:      verification,
		Warnings: []string{
			"plan only: no filesystem changes were made",
			"apply is intentionally absent in this alpha; review paths and use a trusted copier or future journaled apply command",
		},
		Blockers: []string{
			"target filesystem semantics were inferred from the host OS, not measured for this storage root",
			"no host-to-downloader path mapping or downloader job was reconciled",
			"no site release identity was bound to the local metafile artifact",
			"any future apply must repeat exact piece verification immediately before copying",
		},
	}
	plan.Warnings = append(plan.Warnings, targetProbe.Warnings...)
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
	lines := []string{plan.InfoHashV1, plan.InfoHashV2, plan.MetafileVariantID, plan.Verification.SourceSnapshotID, plan.SourceMode, plan.SourceRoot, plan.TargetRoot, plan.Readiness, plan.ClientMapping}
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
