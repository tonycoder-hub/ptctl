package metastore

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
)

// ArtifactLink is an explicit reference from a sealed record to an exact
// metafile artifact in the same private store. It is not itself proof: only a
// successful linked import or load verifies both objects under one root
// session.
type ArtifactLink struct {
	ID             ArtifactID `json:"artifact_id"`
	SizeBytes      int64      `json:"size_bytes"`
	RequirePrivate bool       `json:"require_private,omitempty"`
}

// LinkedRecordDecoder must consume the record reader through EOF and return
// the artifact link encoded by that record. The link is not followed until the
// record digest, stable identity, and complete consumer read are verified.
type LinkedRecordDecoder func(io.Reader) (ArtifactLink, error)

// ImportRecordLinkedArtifact publishes a sealed record only while an exact
// linked metafile artifact is verified in the same operation-bound store root.
// A successful return includes post-publication verification of both objects.
func (s *Store) ImportRecordLinkedArtifact(ctx context.Context, kind RecordKind, reader io.Reader, recordLimits RecordLimits, link ArtifactLink, artifactLimits Limits) (RecordRef, RecordImportReceipt, error) {
	receipt := RecordImportReceipt{Effect: recordImportEffect, Store: s.Info()}
	parsedKind, kindErr := ParseRecordKind(string(kind))
	if s == nil || reader == nil {
		return RecordRef{}, receipt, fmt.Errorf("import linked sealed record: input is unavailable")
	}
	if kindErr != nil || parsedKind != kind {
		return RecordRef{}, receipt, fmt.Errorf("import linked sealed record: kind is invalid")
	}
	if err := recordLimits.Validate(); err != nil {
		return RecordRef{}, receipt, err
	}
	if err := artifactLimits.Validate(); err != nil {
		return RecordRef{}, receipt, err
	}
	if err := validateArtifactLink(link, artifactLimits); err != nil {
		return RecordRef{}, receipt, err
	}
	if err := ctx.Err(); err != nil {
		return RecordRef{}, receipt, err
	}
	session, err := s.validatedSession()
	if err != nil {
		return RecordRef{}, receipt, safeError("import linked sealed record", err)
	}
	defer session.Close()

	if _, _, err := verifyLinkedArtifact(ctx, session, link, artifactLimits); err != nil {
		return RecordRef{}, receipt, err
	}
	if err := session.check("linked_artifact_prechecked"); err != nil {
		return RecordRef{}, receipt, fmt.Errorf("import linked sealed record: bound identity changed")
	}

	ref, receipt, importErr := s.importRecordSession(ctx, session, kind, reader, recordLimits)
	if importErr != nil {
		if receipt.WritesPerformed > 0 {
			// Publication evidence and its durability classification remain
			// authoritative. A best-effort same-session artifact recheck must
			// never replace that post-commit error.
			_, _, _ = verifyLinkedArtifact(context.WithoutCancel(ctx), session, link, artifactLimits)
			_ = session.check("linked_record_imported")
		}
		return ref, receipt, importErr
	}
	if err := session.check("linked_record_imported"); err != nil {
		if receipt.WritesPerformed > 0 {
			return ref, receipt, fmt.Errorf("linked sealed record publication completed but bound identity changed")
		}
		return RecordRef{}, receipt, fmt.Errorf("import linked sealed record: bound identity changed")
	}
	if _, _, err := verifyLinkedArtifact(context.WithoutCancel(ctx), session, link, artifactLimits); err != nil {
		if receipt.WritesPerformed > 0 {
			return ref, receipt, fmt.Errorf("linked sealed record publication completed but artifact verification failed")
		}
		return RecordRef{}, receipt, err
	}
	if err := session.check("before_success"); err != nil {
		if receipt.WritesPerformed > 0 {
			return ref, receipt, fmt.Errorf("linked sealed record publication completed but bound identity changed")
		}
		return RecordRef{}, receipt, fmt.Errorf("import linked sealed record: bound identity changed")
	}
	return ref, receipt, nil
}

// LoadRecordLinkedArtifact loads one explicit record ID, lets the consumer
// decode its artifact link, then verifies that artifact under the very same
// root session. It never enumerates records or selects a latest value.
func (s *Store) LoadRecordLinkedArtifact(ctx context.Context, kind RecordKind, id RecordID, recordLimits RecordLimits, artifactLimits Limits, decode LinkedRecordDecoder) (RecordRef, RecordLoadReceipt, *metafile.MetaInfo, ArtifactRef, error) {
	receipt := RecordLoadReceipt{Effect: recordLoadEffect, Store: s.Info()}
	parsedKind, kindErr := ParseRecordKind(string(kind))
	parsedID, idErr := ParseRecordID(id.String())
	if s == nil || decode == nil {
		return RecordRef{}, receipt, nil, ArtifactRef{}, fmt.Errorf("load linked sealed record: decoder is unavailable")
	}
	if kindErr != nil || parsedKind != kind || idErr != nil || parsedID != id {
		return RecordRef{}, receipt, nil, ArtifactRef{}, fmt.Errorf("load linked sealed record: identity is invalid")
	}
	if err := recordLimits.Validate(); err != nil {
		return RecordRef{}, receipt, nil, ArtifactRef{}, err
	}
	if err := artifactLimits.Validate(); err != nil {
		return RecordRef{}, receipt, nil, ArtifactRef{}, err
	}
	if err := ctx.Err(); err != nil {
		return RecordRef{}, receipt, nil, ArtifactRef{}, err
	}
	session, err := s.validatedSession()
	if err != nil {
		return RecordRef{}, receipt, nil, ArtifactRef{}, safeError("load linked sealed record", err)
	}
	defer session.Close()

	var link ArtifactLink
	recordRef, receipt, recordErr := s.loadRecordSession(ctx, session, kind, id, recordLimits, func(reader io.Reader) error {
		var decodeErr error
		link, decodeErr = decode(reader)
		return decodeErr
	})
	if recordErr != nil {
		return recordRef, receipt, nil, ArtifactRef{}, recordErr
	}
	if err := validateArtifactLink(link, artifactLimits); err != nil {
		receipt.Complete = false
		return recordRef, receipt, nil, ArtifactRef{}, err
	}
	if err := session.check("linked_record_loaded"); err != nil {
		receipt.Complete = false
		return recordRef, receipt, nil, ArtifactRef{}, fmt.Errorf("load linked sealed record: bound identity changed")
	}
	meta, artifactRef, err := verifyLinkedArtifact(ctx, session, link, artifactLimits)
	if err != nil {
		receipt.Complete = false
		return recordRef, receipt, nil, ArtifactRef{}, err
	}
	if err := session.check("before_success"); err != nil {
		receipt.Complete = false
		return recordRef, receipt, nil, ArtifactRef{}, fmt.Errorf("load linked sealed record: bound identity changed")
	}
	return recordRef, receipt, meta, artifactRef, nil
}

func validateArtifactLink(link ArtifactLink, limits Limits) error {
	parsed, err := ParseArtifactID(link.ID.String())
	if err != nil || parsed != link.ID || link.SizeBytes <= 0 || link.SizeBytes > limits.MaxArtifactBytes {
		return fmt.Errorf("linked metafile artifact identity is invalid")
	}
	return nil
}

func verifyLinkedArtifact(ctx context.Context, session *rootSession, link ArtifactLink, limits Limits) (*metafile.MetaInfo, ArtifactRef, error) {
	meta, raw, err := loadArtifact(ctx, session, link.ID, limits)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, ArtifactRef{}, err
		}
		if errors.Is(err, ErrCorruptArtifact) {
			return nil, ArtifactRef{}, err
		}
		return nil, ArtifactRef{}, fmt.Errorf("linked metafile artifact is unavailable")
	}
	ref := makeArtifactRef(meta, int64(len(raw)))
	if ref.ID != link.ID || ref.SizeBytes != link.SizeBytes {
		return nil, ArtifactRef{}, fmt.Errorf("%w: linked artifact identity disagrees", ErrCorruptArtifact)
	}
	if link.RequirePrivate && !meta.Private {
		return nil, ArtifactRef{}, fmt.Errorf("%w: linked artifact privacy requirement failed", ErrCorruptArtifact)
	}
	return meta, ref, nil
}
