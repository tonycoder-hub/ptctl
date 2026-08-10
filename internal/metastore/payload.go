package metastore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
)

// ArtifactPayload is one-shot process-local access to exact bytes from a
// validated private store object. It deliberately has no useful string or JSON
// representation and never exposes a filesystem path.
type ArtifactPayload struct {
	mu     sync.Mutex
	meta   *metafile.MetaInfo
	ref    ArtifactRef
	raw    []byte
	opened bool
}

func (payload *ArtifactPayload) VariantID() string {
	if payload == nil {
		return ""
	}
	return payload.ref.MetafileVariantID
}

func (payload *ArtifactPayload) SizeBytes() int64 {
	if payload == nil {
		return 0
	}
	return int64(len(payload.raw))
}

func (payload *ArtifactPayload) Open() (io.Reader, error) {
	if payload == nil {
		return nil, fmt.Errorf("private metafile payload is unavailable")
	}
	payload.mu.Lock()
	defer payload.mu.Unlock()
	if payload.opened {
		return nil, fmt.Errorf("private metafile payload was already opened")
	}
	payload.opened = true
	return bytes.NewReader(payload.raw), nil
}

func (payload *ArtifactPayload) Matches(meta *metafile.MetaInfo) bool {
	return payload != nil && meta != nil && payload.meta != nil &&
		payload.ref.MetafileVariantID == meta.MetafileVariantID && payload.ref.SizeBytes == meta.MetafileBytes &&
		payload.meta.InfoHashV1 == meta.InfoHashV1 && payload.meta.InfoHashV2 == meta.InfoHashV2
}

func (payload *ArtifactPayload) String() string   { return "[REDACTED_PRIVATE_METAFILE_PAYLOAD]" }
func (payload *ArtifactPayload) GoString() string { return "metastore.ArtifactPayload{[REDACTED]}" }
func (payload *ArtifactPayload) MarshalJSON() ([]byte, error) {
	return json.Marshal("[REDACTED_PRIVATE_METAFILE_PAYLOAD]")
}

// LoadPayload performs the same bound digest and strict-parse checks as Load,
// then returns exact raw bytes as one-shot process-local authority.
func (s *Store) LoadPayload(ctx context.Context, id ArtifactID, limits Limits) (*ArtifactPayload, error) {
	if s == nil {
		return nil, fmt.Errorf("load private metafile payload: store is unavailable")
	}
	parsedID, err := ParseArtifactID(id.String())
	if err != nil {
		return nil, err
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	session, err := s.validatedSession()
	if err != nil {
		return nil, safeError("load private metafile payload", err)
	}
	defer session.Close()
	meta, raw, err := loadArtifact(ctx, session, parsedID, limits)
	if err != nil {
		if errors.Is(err, errArtifactNotFound) || errors.Is(err, ErrCorruptArtifact) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("load private metafile payload failed")
	}
	if err := session.check("before_payload_success"); err != nil {
		return nil, fmt.Errorf("load private metafile payload: bound identity changed")
	}
	return &ArtifactPayload{meta: meta.Clone(), ref: makeArtifactRef(meta, int64(len(raw))), raw: raw}, nil
}
