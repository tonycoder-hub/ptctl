package metastore

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	storeFormat = "ptctl.metastore/v1"

	privacyAssurance = "current_user_owner_only_verified"

	defaultMaxArtifactBytes = int64(32 << 20)
	hardMaxArtifactBytes    = int64(32 << 20)

	initEffect   = "write_private_metafile_store_init"
	importEffect = "write_private_metafile_store"
)

var (
	// ErrInvalidMetafile identifies a bounded import that was not a valid,
	// canonical metafile. Its wrapping errors never include source paths or
	// parser-controlled data.
	ErrInvalidMetafile = errors.New("private metafile is invalid")

	// ErrCorruptArtifact identifies an existing object whose bytes, digest, or
	// parsed metafile identity no longer agree. The store never repairs or
	// overwrites such an object automatically.
	ErrCorruptArtifact = errors.New("private metafile artifact is corrupt")

	// ErrDurabilityUnconfirmed means the final no-replace publication became
	// visible and revalidated, but the platform could not confirm the final
	// directory durability boundary. Callers must report the committed write;
	// retrying is safe only through the ordinary idempotent import path.
	ErrDurabilityUnconfirmed = errors.New("private store publication durability is unconfirmed")

	// ErrPublishedCleanupIncomplete means the final object crossed its
	// durability boundary, but removal or directory flushing of its private
	// staging name failed. The final object must not be rolled back.
	ErrPublishedCleanupIncomplete = errors.New("private store publication staging cleanup is incomplete")
)

type ArtifactID string

func ParseArtifactID(value string) (ArtifactID, error) {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return "", fmt.Errorf("invalid private metafile artifact ID")
	}
	digest := value[len("sha256:"):]
	if strings.ToLower(digest) != digest {
		return "", fmt.Errorf("invalid private metafile artifact ID")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 {
		return "", fmt.Errorf("invalid private metafile artifact ID")
	}
	return ArtifactID(value), nil
}

func (id ArtifactID) String() string { return string(id) }

type Limits struct {
	MaxArtifactBytes int64 `json:"max_artifact_bytes"`
}

func DefaultLimits() Limits {
	return Limits{MaxArtifactBytes: defaultMaxArtifactBytes}
}

func (limits Limits) Validate() error {
	if limits.MaxArtifactBytes <= 0 || limits.MaxArtifactBytes > hardMaxArtifactBytes {
		return fmt.Errorf("maximum private metafile bytes must be in 1..%d", hardMaxArtifactBytes)
	}
	return nil
}

type StoreInfo struct {
	StoreID         string `json:"store_id"`
	Format          string `json:"format"`
	Privacy         string `json:"privacy"`
	CommitAssurance string `json:"commit_assurance"`
}

type ArtifactRef struct {
	ID                ArtifactID `json:"artifact_id"`
	MetafileVariantID string     `json:"metafile_variant_id"`
	Version           string     `json:"version"`
	InfoHashV1        string     `json:"info_hash_v1,omitempty"`
	InfoHashV2        string     `json:"info_hash_v2,omitempty"`
	SizeBytes         int64      `json:"size_bytes"`
}

type InitReceipt struct {
	Effect             string    `json:"effect"`
	WritesPerformed    int       `json:"writes_performed"`
	AlreadyInitialized bool      `json:"already_initialized"`
	Store              StoreInfo `json:"store"`
}

type ImportReceipt struct {
	Effect          string    `json:"effect"`
	WritesPerformed int       `json:"writes_performed"`
	AlreadyPresent  bool      `json:"already_present"`
	BytesConsumed   int64     `json:"bytes_consumed"`
	Store           StoreInfo `json:"store"`
}
