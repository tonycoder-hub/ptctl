// Package materialize turns a same-invocation, exactly verified source layout
// into a copy-only target layout. Its journal is recovery evidence, never
// content-proof authority; source, staging, and final bytes are reverified in
// the invocation that performs each transition.
package materialize

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const (
	IntentSchemaV1 = "ptctl.materialize-intent/v1"
	EventSchemaV1  = "ptctl.materialize-event/v1"

	StrategyCopy = "copy"

	ProofV1     = "v1_piece_verified"
	ProofV2     = "v2_merkle_verified"
	ProofHybrid = "single_pass_v1_piece_and_v2_merkle_verified"

	defaultMaxFiles          = 10_000
	defaultMaxContentBytes   = int64(1 << 40) // 1 TiB
	defaultMaxPathBytes      = int64(16 << 20)
	defaultMaxDirectories    = 128
	defaultMaxNamespaceObjs  = 50_000
	defaultMaxNamespaceBytes = int64(32 << 20)
	defaultCopyBufferBytes   = 1 << 20
	defaultMaxJournalEvents  = 10_032
	defaultMaxEventBytes     = int64(64 << 10)
	defaultMaxScratchEntries = 1_024
	defaultMaxScratchBytes   = defaultMaxContentBytes
	defaultMaxFindings       = 128
	hardMaxFiles             = 99_900
	hardMaxContentBytes      = int64(64 << 40)
	hardMaxPathBytes         = int64(64 << 20)
	hardMaxDirectories       = 256
	hardMaxNamespaceObjs     = 200_000
	hardMaxNamespaceBytes    = int64(64 << 20)
	hardMaxCopyBufferBytes   = 8 << 20
	hardMaxJournalEvents     = 100_000
	hardMaxEventBytes        = int64(256 << 10)
	hardMaxScratchEntries    = 4_096
	hardMaxScratchBytes      = hardMaxContentBytes
	hardMaxFindings          = 1_024
	maxRawPathComponents     = 128
	maxRawPathComponentBytes = 16 << 10
)

var (
	ErrInvalidIntent     = errors.New("materialize intent is invalid")
	ErrCorruptJournal    = errors.New("materialize journal is corrupt")
	ErrOperationNotFound = errors.New("materialize operation was not found")
	ErrIntegrity         = errors.New("materialized content failed exact verification")
	ErrPolicy            = errors.New("materialize operation is blocked by policy")
	errScratchCapacity   = errors.New("materialize scratch capacity is exhausted")
)

type OperationID string

func ParseOperationID(value string) (OperationID, error) {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return "", fmt.Errorf("invalid materialize operation ID")
	}
	digest := strings.TrimPrefix(value, "sha256:")
	if strings.ToLower(digest) != digest {
		return "", fmt.Errorf("invalid materialize operation ID")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 {
		return "", fmt.Errorf("invalid materialize operation ID")
	}
	return OperationID(value), nil
}

func (id OperationID) String() string { return string(id) }

type EventID string

func ParseEventID(value string) (EventID, error) {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return "", fmt.Errorf("invalid materialize journal event ID")
	}
	digest := strings.TrimPrefix(value, "sha256:")
	if strings.ToLower(digest) != digest {
		return "", fmt.Errorf("invalid materialize journal event ID")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 {
		return "", fmt.Errorf("invalid materialize journal event ID")
	}
	return EventID(value), nil
}

func (id EventID) String() string { return string(id) }

type Limits struct {
	MaxFiles            int   `json:"max_files"`
	MaxContentBytes     int64 `json:"max_content_bytes"`
	MaxPathBytes        int64 `json:"max_path_bytes"`
	MaxDirectories      int   `json:"max_directories"`
	MaxNamespaceObjects int   `json:"max_namespace_objects"`
	MaxNamespaceBytes   int64 `json:"max_namespace_bytes"`
	CopyBufferBytes     int   `json:"copy_buffer_bytes"`
	MaxJournalEvents    int   `json:"max_journal_events"`
	MaxEventBytes       int64 `json:"max_event_bytes"`
	MaxScratchEntries   int   `json:"max_scratch_entries"`
	MaxScratchBytes     int64 `json:"max_scratch_bytes"`
	MaxFindings         int   `json:"max_findings"`
}

func DefaultLimits() Limits {
	return Limits{
		MaxFiles: defaultMaxFiles, MaxContentBytes: defaultMaxContentBytes,
		MaxPathBytes: defaultMaxPathBytes, CopyBufferBytes: defaultCopyBufferBytes,
		MaxDirectories:      defaultMaxDirectories,
		MaxNamespaceObjects: defaultMaxNamespaceObjs, MaxNamespaceBytes: defaultMaxNamespaceBytes,
		MaxJournalEvents: defaultMaxJournalEvents, MaxEventBytes: defaultMaxEventBytes,
		MaxScratchEntries: defaultMaxScratchEntries, MaxScratchBytes: defaultMaxScratchBytes,
		MaxFindings: defaultMaxFindings,
	}
}

func (limits Limits) Validate() error {
	checks := []struct {
		name  string
		value int64
		hard  int64
	}{
		{"maximum files", int64(limits.MaxFiles), hardMaxFiles},
		{"maximum content bytes", limits.MaxContentBytes, hardMaxContentBytes},
		{"maximum path bytes", limits.MaxPathBytes, hardMaxPathBytes},
		{"maximum directories", int64(limits.MaxDirectories), hardMaxDirectories},
		{"maximum namespace objects", int64(limits.MaxNamespaceObjects), hardMaxNamespaceObjs},
		{"maximum namespace bytes", limits.MaxNamespaceBytes, hardMaxNamespaceBytes},
		{"copy buffer bytes", int64(limits.CopyBufferBytes), hardMaxCopyBufferBytes},
		{"maximum journal events", int64(limits.MaxJournalEvents), hardMaxJournalEvents},
		{"maximum event bytes", limits.MaxEventBytes, hardMaxEventBytes},
		{"maximum scratch entries", int64(limits.MaxScratchEntries), hardMaxScratchEntries},
		{"maximum scratch bytes", limits.MaxScratchBytes, hardMaxScratchBytes},
		{"maximum retained findings", int64(limits.MaxFindings), hardMaxFindings},
	}
	for _, check := range checks {
		if check.value <= 0 || check.value > check.hard {
			return fmt.Errorf("%s must be in 1..%d", check.name, check.hard)
		}
	}
	if limits.MaxJournalEvents < limits.MaxFiles+8 {
		return fmt.Errorf("maximum journal events must allow every file plus protocol transitions")
	}
	if limits.MaxScratchBytes < limits.MaxEventBytes {
		return fmt.Errorf("maximum scratch bytes must hold one maximum journal payload")
	}
	return nil
}

type Phase string

const (
	PhaseJournaled     Phase = "journaled"
	PhaseStageCreated  Phase = "stage_created"
	PhaseFileStaged    Phase = "file_staged"
	PhaseStageVerified Phase = "stage_verified"
	PhasePublishIntent Phase = "publish_intent"
	PhasePublished     Phase = "published"
	PhaseFinalVerified Phase = "final_verified"
	PhaseCommitted     Phase = "committed"
	PhaseAbandoned     Phase = "abandoned"
)

func validPhase(value Phase) bool {
	switch value {
	case PhaseJournaled, PhaseStageCreated, PhaseFileStaged, PhaseStageVerified,
		PhasePublishIntent, PhasePublished, PhaseFinalVerified, PhaseCommitted, PhaseAbandoned:
		return true
	default:
		return false
	}
}

// Intent is the canonical, secret-free header for one target-root-local
// operation. Absolute source and target paths are deliberately absent.
type Intent struct {
	Schema                   string   `json:"schema"`
	MetafileVariantID        string   `json:"metafile_variant_id"`
	InfoHashV1               string   `json:"info_hash_v1,omitempty"`
	InfoHashV2               string   `json:"info_hash_v2,omitempty"`
	Nonce                    string   `json:"nonce"`
	PlanID                   string   `json:"plan_id"`
	Strategy                 string   `json:"strategy"`
	TargetRootIdentity       string   `json:"target_root_identity"`
	FinalRawComponentsBase64 []string `json:"final_raw_components_base64"`
	MultiFile                bool     `json:"multi_file"`
	ManifestFiles            int      `json:"manifest_files"`
	ContentBytes             int64    `json:"content_bytes"`
	ManifestPathBytes        int64    `json:"manifest_path_bytes"`
	NamespaceObjects         int      `json:"namespace_objects"`
	NamespaceBytes           int64    `json:"namespace_bytes"`
	Limits                   Limits   `json:"limits"`
}

func (intent Intent) Validate() error {
	if intent.Schema != IntentSchemaV1 || intent.Strategy != StrategyCopy {
		return fmt.Errorf("%w: schema or strategy is unsupported", ErrInvalidIntent)
	}
	if !canonicalSHA256ID(intent.MetafileVariantID) || !canonicalDigest(intent.Nonce) || !canonicalPlanID(intent.PlanID) {
		return fmt.Errorf("%w: identity is invalid", ErrInvalidIntent)
	}
	if identity, err := fsbind.ParseIdentity(intent.TargetRootIdentity); err != nil || identity.IsZero() {
		return fmt.Errorf("%w: target root identity is invalid", ErrInvalidIntent)
	}
	if (intent.InfoHashV1 == "" && intent.InfoHashV2 == "") ||
		(intent.InfoHashV1 != "" && !canonicalHexDigest(intent.InfoHashV1, 20)) ||
		(intent.InfoHashV2 != "" && !canonicalHexDigest(intent.InfoHashV2, 32)) {
		return fmt.Errorf("%w: typed infohash is invalid", ErrInvalidIntent)
	}
	if err := intent.Limits.Validate(); err != nil {
		return fmt.Errorf("%w: limits are invalid", ErrInvalidIntent)
	}
	if intent.ManifestFiles <= 0 || intent.ManifestFiles > intent.Limits.MaxFiles || intent.ContentBytes < 0 || intent.ContentBytes > intent.Limits.MaxContentBytes ||
		intent.ManifestPathBytes <= 0 || intent.ManifestPathBytes > intent.Limits.MaxPathBytes ||
		intent.NamespaceObjects <= 0 || intent.NamespaceObjects > intent.Limits.MaxNamespaceObjects ||
		intent.NamespaceBytes <= 0 || intent.NamespaceBytes > intent.Limits.MaxNamespaceBytes {
		return fmt.Errorf("%w: manifest budget is invalid", ErrInvalidIntent)
	}
	if len(intent.FinalRawComponentsBase64) != 1 {
		return fmt.Errorf("%w: final target must have one top-level component", ErrInvalidIntent)
	}
	pathBytes, err := validateRawComponents(intent.FinalRawComponentsBase64)
	if err != nil || pathBytes > intent.Limits.MaxPathBytes {
		return fmt.Errorf("%w: final target path is invalid", ErrInvalidIntent)
	}
	return nil
}

// Event is one immutable hash-chain transition. EventID is derived from the
// canonical encoding and is therefore not stored inside the value itself.
type Event struct {
	Schema         string      `json:"schema"`
	OperationID    OperationID `json:"operation_id"`
	Sequence       int         `json:"sequence"`
	Previous       string      `json:"previous"`
	Phase          Phase       `json:"phase"`
	ManifestIndex  *int        `json:"manifest_index,omitempty"`
	Bytes          int64       `json:"bytes"`
	ObjectIdentity string      `json:"object_identity,omitempty"`
	ContentSHA256  string      `json:"content_sha256,omitempty"`
	Proof          string      `json:"proof,omitempty"`
}

func (event Event) Validate() error {
	if event.Schema != EventSchemaV1 || !validPhase(event.Phase) || event.Sequence < 0 || event.Bytes < 0 {
		return fmt.Errorf("%w: event shape is invalid", ErrCorruptJournal)
	}
	if parsed, err := ParseOperationID(event.OperationID.String()); err != nil || parsed != event.OperationID {
		return fmt.Errorf("%w: operation identity is invalid", ErrCorruptJournal)
	}
	if event.Sequence == 0 {
		if event.Phase != PhaseJournaled || event.Previous != event.OperationID.String() || event.ObjectIdentity == "" {
			return fmt.Errorf("%w: initial event is invalid", ErrCorruptJournal)
		}
	} else if parsed, err := ParseEventID(event.Previous); err != nil || parsed.String() != event.Previous {
		return fmt.Errorf("%w: previous event identity is invalid", ErrCorruptJournal)
	}
	if event.Phase == PhaseFileStaged {
		if event.ManifestIndex == nil || *event.ManifestIndex < 0 || event.ObjectIdentity == "" || !canonicalDigest(event.ContentSHA256) {
			return fmt.Errorf("%w: staged-file event is invalid", ErrCorruptJournal)
		}
	} else if event.ManifestIndex != nil || event.ContentSHA256 != "" {
		return fmt.Errorf("%w: event carries unexpected file fields", ErrCorruptJournal)
	}
	if phaseRequiresIdentity(event.Phase) && event.ObjectIdentity == "" {
		return fmt.Errorf("%w: transition object identity is missing", ErrCorruptJournal)
	}
	if event.ObjectIdentity != "" {
		if identity, err := fsbind.ParseIdentity(event.ObjectIdentity); err != nil || identity.IsZero() {
			return fmt.Errorf("%w: transition object identity is invalid", ErrCorruptJournal)
		}
	}
	if !phaseAllowsIdentity(event.Phase) && event.ObjectIdentity != "" {
		return fmt.Errorf("%w: transition object identity is unexpected", ErrCorruptJournal)
	}
	if event.Phase == PhaseStageVerified || event.Phase == PhaseFinalVerified {
		if !validProof(event.Proof) {
			return fmt.Errorf("%w: exact verification proof is missing", ErrCorruptJournal)
		}
	} else if event.Proof != "" {
		return fmt.Errorf("%w: verification proof is unexpected", ErrCorruptJournal)
	}
	if (event.Phase == PhaseJournaled || event.Phase == PhaseStageCreated) && event.Bytes != 0 {
		return fmt.Errorf("%w: transition byte count is invalid", ErrCorruptJournal)
	}
	return nil
}

func phaseRequiresIdentity(phase Phase) bool {
	switch phase {
	case PhaseJournaled, PhaseStageCreated, PhaseFileStaged, PhaseStageVerified, PhasePublishIntent,
		PhasePublished, PhaseFinalVerified, PhaseCommitted:
		return true
	default:
		return false
	}
}

func phaseAllowsIdentity(phase Phase) bool {
	return phaseRequiresIdentity(phase)
}

func validProof(value string) bool {
	switch value {
	case ProofV1, ProofV2, ProofHybrid:
		return true
	default:
		return false
	}
}

func canonicalSHA256ID(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	return canonicalDigest(strings.TrimPrefix(value, "sha256:"))
}

func canonicalPlanID(value string) bool {
	if len(value) != 24 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 12
}

func canonicalDigest(value string) bool {
	return canonicalHexDigest(value, 32)
}

func canonicalHexDigest(value string, bytes int) bool {
	if len(value) != bytes*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == bytes
}

func validateRawComponents(values []string) (int64, error) {
	if len(values) == 0 || len(values) > maxRawPathComponents {
		return 0, fmt.Errorf("raw path component count is invalid")
	}
	var total int64
	for _, value := range values {
		raw, err := base64.StdEncoding.Strict().DecodeString(value)
		if err != nil || len(raw) == 0 || len(raw) > maxRawPathComponentBytes || base64.StdEncoding.EncodeToString(raw) != value {
			return 0, fmt.Errorf("raw path component is invalid")
		}
		if string(raw) == "." || string(raw) == ".." {
			return 0, fmt.Errorf("raw path component is traversal")
		}
		for _, ch := range raw {
			if ch == 0 || ch == '/' || ch == '\\' {
				return 0, fmt.Errorf("raw path component contains a separator or NUL")
			}
		}
		total += int64(len(raw))
	}
	return total, nil
}
