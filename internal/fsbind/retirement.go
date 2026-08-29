package fsbind

import (
	"context"
	"errors"
	"fmt"
)

// PrivateSubtreeRemoval records the two visible namespace transitions needed
// to retire one operation subtree. LockRemoved and Removed mean the reviewed
// names were observed absent after their respective attempts; durability is
// the final bound-root directory durability and is meaningful only when
// Removed is true.
type PrivateSubtreeRemoval struct {
	Attempted            bool       `json:"attempted"`
	LockRemovalAttempted bool       `json:"lock_removal_attempted"`
	LockRemoved          bool       `json:"lock_removed"`
	LockAbsentBefore     bool       `json:"lock_absent_before"`
	DirectoryAttempted   bool       `json:"directory_removal_attempted"`
	Removed              bool       `json:"removed"`
	Durability           string     `json:"durability"`
	Identity             Identity   `json:"identity,omitempty,omitzero"`
	LockIdentity         Identity   `json:"lock_identity,omitempty,omitzero"`
	Kind                 ObjectKind `json:"kind,omitempty"`
}

// OpenRootPrivateRegular opens one owner-private, single-link regular file
// directly beneath the bound root. It is intended for durable operation
// transition markers that must outlive the operation subtree being removed.
func (session *Session) OpenRootPrivateRegular(ctx context.Context, name string) (*File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if validateSingleComponent(name) != nil {
		return nil, ErrInvalidPath
	}
	if err := session.check("before_root_private_regular_open"); err != nil {
		return nil, err
	}
	native, raw, err := platformOpenPrivateRegularReadOnly(session, session.root, name)
	if err != nil {
		return nil, err
	}
	file, err := session.wrapFile(native, raw, nil, nil)
	if err != nil {
		return nil, err
	}
	if err := session.check("after_root_private_regular_open"); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// RemoveRootPrivateRegularExact removes one closed owner-private, single-link
// root marker using an identity and size from a prior exact read. An absent
// marker is never accepted as a successful first attempt.
func (session *Session) RemoveRootPrivateRegularExact(ctx context.Context, name string, expected Identity, expectedSize int64) (Removal, error) {
	receipt := Removal{Durability: durabilityNotPublished, Kind: ObjectKindRegular}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if validateSingleComponent(name) != nil || expected.IsZero() || expectedSize < 0 {
		return receipt, ErrInvalidPath
	}
	if err := session.check("before_root_private_regular_remove"); err != nil {
		return receipt, err
	}
	probe, raw, err := platformOpenPrivateRegularReadOnly(session, session.root, name)
	if err != nil {
		return receipt, err
	}
	info, infoErr := probe.Stat()
	closeErr := probe.Close()
	if infoErr != nil {
		return receipt, fmt.Errorf("inspect bound root private regular failed")
	}
	if closeErr != nil {
		return receipt, fmt.Errorf("close bound root private regular failed")
	}
	if !info.Mode().IsRegular() || info.Size() != expectedSize || !identityFromRaw(raw).Equal(expected) {
		return receipt, ErrUnsafeObject
	}
	if session.hasOpenIdentity(raw) {
		return receipt, fmt.Errorf("remove bound root private regular: handle remains open")
	}
	receipt.Identity, receipt.SizeBytes = expected, expectedSize
	receipt.Attempted = true
	removed, durable, removedRaw, removeErr := platformRemoveObject(session, session.root, name, false, raw, expectedSize)
	if removed {
		receipt.Removed = true
		receipt.Durability = durabilityUnconfirmed
		if durable {
			receipt.Durability = durabilityConfirmed
		}
		if removedRaw != (rawIdentity{}) && removedRaw != raw {
			removeErr = errors.Join(removeErr, ErrRemovalAmbiguous)
		}
	}
	if removeErr == nil && !removed {
		removeErr = ErrRemovalAmbiguous
	}
	bindingErr := session.check("after_root_private_regular_remove")
	if removeErr != nil && bindingErr != nil {
		return receipt, errors.Join(removeErr, bindingErr)
	}
	if removeErr != nil {
		return receipt, removeErr
	}
	return receipt, bindingErr
}

// RemovePrivateSubtreeExact consumes a locked operation subtree whose exact
// root namespace contains only fsbind's operation lock. The platform keeps the
// cooperative lock authority until the lock name is removed and then removes
// the reviewed directory by identity. Once an attempt begins, subtree is
// consumed even on error; recovery must use RemovePrivateSubtreeResidueExact
// with authority retained outside the subtree.
func (session *Session) RemovePrivateSubtreeExact(ctx context.Context, subtree *Subtree) (PrivateSubtreeRemoval, error) {
	receipt := PrivateSubtreeRemoval{Durability: durabilityNotPublished, Kind: ObjectKindDirectory}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if subtree == nil || subtree.session != session || subtree.closed || subtree.base == nil || subtree.lockFile == nil {
		return receipt, ErrInvalidPath
	}
	if err := subtree.Check(); err != nil {
		return receipt, err
	}
	listing, err := subtree.List(ctx, Path{}, ListLimits{MaxEntries: 2, MaxNameBytes: 1 << 10})
	if err != nil {
		return receipt, err
	}
	if !listing.Complete || len(listing.Entries) != 1 || listing.Entries[0].Name != operationLockName ||
		listing.Entries[0].Kind != string(ObjectKindRegular) || len(subtree.dirs) != 1 || session.hasAnyOpenFiles() {
		return receipt, ErrUnsafeObject
	}
	if err := subtree.checkBinding("before_private_subtree_retire"); err != nil {
		return receipt, err
	}
	receipt.Identity = subtree.base.identity
	receipt.LockIdentity = identityFromRaw(subtree.lockRaw)
	attempted, lockRemoved, directoryAttempted, removed, durable, retireErr := platformRetirePrivateSubtree(
		session, subtree.name, subtree.base, subtree.lockFile, subtree.base.raw, subtree.lockRaw,
	)
	receipt.Attempted = attempted
	receipt.LockRemovalAttempted = attempted
	receipt.LockRemoved = lockRemoved
	receipt.DirectoryAttempted = directoryAttempted
	receipt.Removed = removed
	if removed {
		receipt.Durability = durabilityUnconfirmed
		if durable {
			receipt.Durability = durabilityConfirmed
		}
	}
	if attempted {
		detachConsumedSubtree(subtree)
	}
	if retireErr == nil && attempted && !removed {
		retireErr = ErrRemovalAmbiguous
	}
	bindingErr := session.check("after_private_subtree_retire")
	if retireErr != nil && bindingErr != nil {
		return receipt, errors.Join(retireErr, bindingErr)
	}
	if retireErr != nil {
		return receipt, retireErr
	}
	return receipt, bindingErr
}

// RemovePrivateSubtreeResidueExact removes an exact empty, owner-private
// operation-directory residue after a durable external marker proves that the
// lock was already removed by an earlier attempt. It never removes a directory
// containing any object and never accepts an unbound identity.
func (session *Session) RemovePrivateSubtreeResidueExact(ctx context.Context, name string, expected Identity) (PrivateSubtreeRemoval, error) {
	receipt := PrivateSubtreeRemoval{Durability: durabilityNotPublished, Kind: ObjectKindDirectory, LockAbsentBefore: true}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if validateSingleComponent(name) != nil || expected.IsZero() {
		return receipt, ErrInvalidPath
	}
	if err := session.check("before_private_subtree_residue_remove"); err != nil {
		return receipt, err
	}
	directory, err := platformOpenPrivateDirectory(session, session.root, name)
	if err != nil {
		return receipt, err
	}
	if !directory.identity.Equal(expected) {
		_ = directory.file.Close()
		return receipt, ErrUnsafeObject
	}
	listing, listErr := listBoundDirectory(ctx, session, directory, ListLimits{MaxEntries: 1, MaxNameBytes: 1 << 10}, "after_private_subtree_residue_list")
	closeErr := directory.file.Close()
	if listErr != nil {
		return receipt, listErr
	}
	if closeErr != nil {
		return receipt, fmt.Errorf("close private subtree residue failed")
	}
	if !listing.Complete || len(listing.Entries) != 0 || session.hasAnyOpenFiles() {
		return receipt, ErrUnsafeObject
	}
	receipt.Identity = expected
	receipt.Attempted = true
	receipt.DirectoryAttempted = true
	removed, durable, removedRaw, removeErr := platformRemoveObject(session, session.root, name, true, directory.raw, -1)
	if removed {
		receipt.Removed = true
		receipt.Durability = durabilityUnconfirmed
		if durable {
			receipt.Durability = durabilityConfirmed
		}
		if removedRaw != (rawIdentity{}) && removedRaw != directory.raw {
			removeErr = errors.Join(removeErr, ErrRemovalAmbiguous)
		}
	}
	if removeErr == nil && !removed {
		removeErr = ErrRemovalAmbiguous
	}
	bindingErr := session.check("after_private_subtree_residue_remove")
	if removeErr != nil && bindingErr != nil {
		return receipt, errors.Join(removeErr, bindingErr)
	}
	if removeErr != nil {
		return receipt, removeErr
	}
	return receipt, bindingErr
}

func detachConsumedSubtree(subtree *Subtree) {
	if subtree == nil {
		return
	}
	subtree.closed = true
	if subtree.session != nil {
		subtree.session.mu.Lock()
		delete(subtree.session.subtrees, subtree)
		subtree.session.mu.Unlock()
	}
	subtree.lockFile = nil
	subtree.lockRaw = rawIdentity{}
	subtree.base = nil
	subtree.dirs = nil
}
