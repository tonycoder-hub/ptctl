// Package fsbind provides a small, fail-closed filesystem boundary for
// journaled filesystem operations. It binds an existing reviewed filesystem
// root, permits private operation-state mutations beneath an exclusively
// locked subtree, publishes without replacement, and can remove one explicitly
// identity-bound regular-file name from a separately bound parent directory.
package fsbind

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	ErrUnsupported           = errors.New("bound filesystem platform is unsupported")
	ErrInvalidPath           = errors.New("bound filesystem path is invalid")
	ErrAlreadyExists         = errors.New("bound filesystem destination already exists")
	ErrNotFound              = errors.New("bound filesystem object was not found")
	ErrNotEmpty              = errors.New("bound filesystem directory is not empty")
	ErrUnsafeObject          = errors.New("bound filesystem object is unsafe")
	ErrCrossFilesystem       = errors.New("bound filesystem object crossed the reviewed filesystem")
	ErrBindingChanged        = errors.New("bound filesystem root identity changed")
	ErrDurabilityUnconfirmed = errors.New("bound filesystem publication durability is unconfirmed")
	ErrPublicationAmbiguous  = errors.New("bound filesystem publication result is ambiguous")
	ErrRemovalAmbiguous      = errors.New("bound filesystem removal result is ambiguous")
	ErrBusy                  = errors.New("bound filesystem operation subtree is busy")
)

const (
	identityPrefix         = "fsbind-v1:"
	identityDomain         = "ptctl-fsbind-identity-v1\x00"
	operationLockName      = ".fsbind-operation.lock"
	maxPathComponents      = 256
	maxPathComponentBytes  = 64 << 10
	maxPathBytes           = 1 << 20
	hardMaxListEntries     = 100_000
	hardMaxListNameBytes   = int64(64 << 20)
	DurabilityNotPublished = "not_published"
	DurabilityConfirmed    = "confirmed"
	DurabilityUnconfirmed  = "unconfirmed"
	durabilityNotPublished = DurabilityNotPublished
	durabilityConfirmed    = DurabilityConfirmed
	durabilityUnconfirmed  = DurabilityUnconfirmed
)

// Identity is a deterministic, path-free observation token. It can be stored
// in a journal and compared after reopening, but never grants filesystem
// authority by itself.
type Identity struct{ token string }

func ParseIdentity(value string) (Identity, error) {
	if len(value) != len(identityPrefix)+sha256.Size*2 || !strings.HasPrefix(value, identityPrefix) {
		return Identity{}, fmt.Errorf("filesystem identity is invalid")
	}
	digest := value[len(identityPrefix):]
	if strings.ToLower(digest) != digest {
		return Identity{}, fmt.Errorf("filesystem identity is invalid")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size {
		return Identity{}, fmt.Errorf("filesystem identity is invalid")
	}
	return Identity{token: value}, nil
}

func (identity Identity) String() string { return identity.token }
func (identity Identity) IsZero() bool   { return identity.token == "" }
func (identity Identity) Equal(other Identity) bool {
	return identity.token != "" && identity.token == other.token
}
func (identity Identity) MarshalText() ([]byte, error) {
	if _, err := ParseIdentity(identity.token); err != nil {
		return nil, err
	}
	return []byte(identity.token), nil
}
func (identity *Identity) UnmarshalText(raw []byte) error {
	parsed, err := ParseIdentity(string(raw))
	if err != nil {
		return err
	}
	*identity = parsed
	return nil
}

type rawIdentity struct {
	volume    uint64
	mount     uint64
	fileHigh  uint64
	fileLow   uint64
	directory bool
}

func identityFromRaw(raw rawIdentity) Identity {
	value := fmt.Sprintf("%s%d\x00%d\x00%d\x00%d\x00%t", identityDomain, raw.volume, raw.mount, raw.fileHigh, raw.fileLow, raw.directory)
	digest := sha256.Sum256([]byte(value))
	return Identity{token: identityPrefix + hex.EncodeToString(digest[:])}
}

type RootInfo struct {
	Identity        Identity `json:"identity"`
	Filesystem      string   `json:"filesystem"`
	CommitAssurance string   `json:"commit_assurance"`
}

// Path is a validated component path. Its String and JSON forms deliberately
// do not expose private path material.
type Path struct{ components []string }

func PathFromComponents(components []string) (Path, error) {
	if len(components) > maxPathComponents {
		return Path{}, ErrInvalidPath
	}
	result := make([]string, len(components))
	total := 0
	for index, component := range components {
		if component == "" || component == "." || component == ".." || component == operationLockName || len(component) > maxPathComponentBytes ||
			strings.IndexByte(component, 0) >= 0 || !utf8.ValidString(component) || platformValidateComponent(component) != nil {
			return Path{}, ErrInvalidPath
		}
		total += len(component)
		if total > maxPathBytes {
			return Path{}, ErrInvalidPath
		}
		result[index] = component
	}
	return Path{components: result}, nil
}

// ValidatePathComponents applies the exact platform and hard-budget rules used
// by all bound operations without retaining or exposing the components.
func ValidatePathComponents(components []string) error {
	_, err := PathFromComponents(components)
	return err
}

func (path Path) String() string   { return "[REDACTED_BOUND_PATH]" }
func (path Path) GoString() string { return "fsbind.Path{[REDACTED]}" }

func pathKey(components []string) string { return strings.Join(components, "\x00") }

type boundDirectory struct {
	file     *os.File
	raw      rawIdentity
	identity Identity
}

// platformState is implemented separately on supported operating systems.
// The empty declaration here is replaced through embedding platformData,
// whose definition is build-tagged.
type platformState struct{ data platformData }

type Session struct {
	mu        sync.Mutex
	path      string
	root      *boundDirectory
	info      RootInfo
	platform  platformState
	subtrees  map[*Subtree]struct{}
	published map[*Published]struct{}
	files     map[*File]struct{}
	closed    bool
}

type Subtree struct {
	session  *Session
	name     string
	base     *boundDirectory
	dirs     map[string]*boundDirectory
	lockFile *os.File
	lockRaw  rawIdentity
	closed   bool
}

// Published is a process-local, root-bound view of a published regular file or
// directory. Its identity can be journaled, but the value itself is authority
// and intentionally has no serialization surface.
type Published struct {
	session *Session
	name    string
	kind    ObjectKind
	raw     rawIdentity
	dirs    map[string]*boundDirectory
	closed  bool
}

type File struct {
	mu        sync.Mutex
	file      *os.File
	session   *Session
	subtree   *Subtree
	published *Published
	identity  Identity
	raw       rawIdentity
	closed    bool
}

type FileInfo struct {
	Identity  Identity  `json:"identity"`
	SizeBytes int64     `json:"size_bytes"`
	Modified  time.Time `json:"modified_at"`
}

type ObjectKind string

const (
	ObjectKindRegular   ObjectKind = "regular"
	ObjectKindDirectory ObjectKind = "directory"
)

type ObjectInfo struct {
	Identity  Identity   `json:"identity"`
	Kind      ObjectKind `json:"kind"`
	SizeBytes int64      `json:"size_bytes,omitempty"`
}

type MkdirReceipt struct {
	DirectoriesCreated int    `json:"directories_created"`
	Durability         string `json:"durability"`
}

type Creation struct {
	Created    bool     `json:"created"`
	Durability string   `json:"durability"`
	Identity   Identity `json:"identity,omitempty,omitzero"`
}

type ListLimits struct {
	MaxEntries   int   `json:"max_entries"`
	MaxNameBytes int64 `json:"max_name_bytes"`
}

func DefaultListLimits() ListLimits {
	return ListLimits{MaxEntries: 10_000, MaxNameBytes: 16 << 20}
}

// MaximumListLimits exposes the installed hard directory-inventory ceiling so
// higher-level fixed-budget protocols can reject an unrepresentable namespace
// before they mutate it. Callers should normally prefer DefaultListLimits.
func MaximumListLimits() ListLimits {
	return ListLimits{MaxEntries: hardMaxListEntries, MaxNameBytes: hardMaxListNameBytes}
}

func (limits ListLimits) Validate() error {
	if limits.MaxEntries <= 0 || limits.MaxEntries > hardMaxListEntries || limits.MaxNameBytes <= 0 || limits.MaxNameBytes > hardMaxListNameBytes {
		return fmt.Errorf("bound directory list limits are invalid")
	}
	return nil
}

type Entry struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type ListUsage struct {
	EntriesExamined int   `json:"entries_examined"`
	NameBytes       int64 `json:"name_bytes"`
}

type ListResult struct {
	Complete   bool       `json:"complete"`
	Limits     ListLimits `json:"limits"`
	Used       ListUsage  `json:"used"`
	Entries    []Entry    `json:"entries"`
	StopReason string     `json:"stop_reason,omitempty"`
}

type Publication struct {
	Attempted      bool     `json:"attempted"`
	Published      bool     `json:"published"`
	Durability     string   `json:"durability"`
	SourceIdentity Identity `json:"source_identity,omitempty,omitzero"`
	FinalIdentity  Identity `json:"final_identity,omitempty,omitzero"`
}

// Removal is an operation-scoped receipt for one identity-bound unlink or
// empty-directory removal. Removed means the reviewed name was observed
// absent after the attempt; Durability remains separate because an absent name
// does not prove that its parent-directory update reached stable storage.
type Removal struct {
	Attempted         bool       `json:"attempted"`
	Removed           bool       `json:"removed"`
	Durability        string     `json:"durability"`
	Identity          Identity   `json:"identity,omitempty,omitzero"`
	Kind              ObjectKind `json:"kind,omitempty"`
	SizeBytes         int64      `json:"size_bytes,omitempty"`
	NamespaceRead     bool       `json:"-"`
	EntriesExamined   int        `json:"-"`
	NameBytesExamined int64      `json:"-"`
}

// checkHook is a deterministic package test seam. It cannot replace the real
// named-root validation performed immediately afterward.
var checkHook func(stage string, session *Session) error

func BindExisting(root string) (*Session, RootInfo, error) {
	session, info, err := platformBindExisting(root)
	if err != nil {
		return nil, RootInfo{}, err
	}
	if session == nil || session.root == nil || info.Identity.IsZero() {
		if session != nil {
			_ = session.Close()
		}
		return nil, RootInfo{}, fmt.Errorf("bind existing target root failed")
	}
	session.info = info
	session.subtrees = make(map[*Subtree]struct{})
	session.published = make(map[*Published]struct{})
	session.files = make(map[*File]struct{})
	if err := session.check("session_bound"); err != nil {
		_ = session.Close()
		return nil, RootInfo{}, err
	}
	return session, info, nil
}

func (session *Session) Info() RootInfo {
	if session == nil {
		return RootInfo{}
	}
	return session.info
}

func (session *Session) Check() error { return session.check("explicit_check") }

// SyncRoot confirms the current root namespace after a crash-recovery identity
// match. It never turns a failed named-root check into a durability claim.
func (session *Session) SyncRoot(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := session.check("before_root_sync"); err != nil {
		return err
	}
	if err := platformSyncDirectory(session.root); err != nil {
		return ErrDurabilityUnconfirmed
	}
	return session.check("after_root_sync")
}

func (session *Session) check(stage string) error {
	if session == nil {
		return ErrBindingChanged
	}
	if checkHook != nil && checkHook(stage, session) != nil {
		return ErrBindingChanged
	}
	if err := platformCheckBinding(session); err != nil {
		return ErrBindingChanged
	}
	return nil
}

func (session *Session) CreatePrivateSubtree(name string) (*Subtree, error) {
	subtree, _, err := session.CreatePrivateSubtreeWithReceipt(name)
	return subtree, err
}

// CreatePrivateSubtreeWithReceipt preserves evidence when the new directory
// became visible but a later durability, lock, or binding check failed.
func (session *Session) CreatePrivateSubtreeWithReceipt(name string) (*Subtree, Creation, error) {
	receipt := Creation{Durability: durabilityNotPublished}
	if err := validateSingleComponent(name); err != nil {
		return nil, receipt, err
	}
	if err := session.check("before_subtree_create"); err != nil {
		return nil, receipt, err
	}
	directory, created, durable, err := platformCreatePrivateDirectory(session, session.root, name)
	if created {
		receipt.Created = true
		receipt.Durability = durabilityUnconfirmed
		if durable {
			receipt.Durability = durabilityConfirmed
		}
		if directory != nil {
			receipt.Identity = directory.identity
		}
	}
	if err != nil {
		if directory != nil {
			_ = directory.file.Close()
		}
		return nil, receipt, err
	}
	subtree := &Subtree{session: session, name: name, base: directory, dirs: map[string]*boundDirectory{"": directory}}
	lock, lockRaw, err := platformAcquireOperationLock(session, directory, true)
	if err != nil {
		_ = directory.file.Close()
		return nil, receipt, err
	}
	subtree.lockFile = lock
	subtree.lockRaw = lockRaw
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		_ = lock.Close()
		_ = directory.file.Close()
		return nil, receipt, ErrBindingChanged
	}
	session.subtrees[subtree] = struct{}{}
	session.mu.Unlock()
	if err := session.check("after_subtree_create"); err != nil {
		_ = subtree.Close()
		return nil, receipt, err
	}
	if err := subtree.checkBinding("after_subtree_create_binding"); err != nil {
		_ = subtree.Close()
		return nil, receipt, err
	}
	return subtree, receipt, nil
}

func (session *Session) OpenPrivateSubtree(name string, expected Identity) (*Subtree, error) {
	if expected.IsZero() {
		return nil, ErrInvalidPath
	}
	return session.openPrivateSubtree(name, expected, true)
}

// OpenPrivateSubtreeObserved acquires a named private subtree when its
// journaled identity is stored inside that subtree. The returned process-local
// handle is only an observation; callers must replay event zero and compare
// Identity before performing any further filesystem action.
func (session *Session) OpenPrivateSubtreeObserved(name string) (*Subtree, error) {
	return session.openPrivateSubtree(name, Identity{}, false)
}

func (session *Session) openPrivateSubtree(name string, expected Identity, requireExpected bool) (*Subtree, error) {
	if err := validateSingleComponent(name); err != nil {
		return nil, err
	}
	if err := session.check("before_subtree_open"); err != nil {
		return nil, err
	}
	directory, err := platformOpenPrivateDirectory(session, session.root, name)
	if err != nil {
		return nil, err
	}
	if requireExpected && !directory.identity.Equal(expected) {
		_ = directory.file.Close()
		return nil, ErrUnsafeObject
	}
	lock, lockRaw, err := platformAcquireOperationLock(session, directory, false)
	if err != nil {
		_ = directory.file.Close()
		return nil, err
	}
	subtree := &Subtree{session: session, name: name, base: directory, dirs: map[string]*boundDirectory{"": directory}, lockFile: lock, lockRaw: lockRaw}
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		_ = lock.Close()
		_ = directory.file.Close()
		return nil, ErrBindingChanged
	}
	session.subtrees[subtree] = struct{}{}
	session.mu.Unlock()
	if err := session.check("after_subtree_open"); err != nil {
		_ = subtree.Close()
		return nil, err
	}
	if err := subtree.checkBinding("after_subtree_open_binding"); err != nil {
		_ = subtree.Close()
		return nil, err
	}
	return subtree, nil
}

// OpenPublishedRoot rebinds an explicitly named root publication to its
// journaled identity. No latest lookup or path-derived authority is accepted.
func (session *Session) OpenPublishedRoot(finalName string, expected Identity) (*Published, error) {
	if validateSingleComponent(finalName) != nil || expected.IsZero() {
		return nil, ErrInvalidPath
	}
	if err := session.check("before_published_open"); err != nil {
		return nil, err
	}
	native, raw, kind, err := platformInspectObject(session, session.root, finalName)
	if err != nil {
		return nil, err
	}
	if !identityFromRaw(raw).Equal(expected) {
		_ = native.Close()
		return nil, ErrUnsafeObject
	}
	published := &Published{session: session, name: finalName, kind: kind, raw: raw}
	if kind == ObjectKindDirectory {
		_ = native.Close()
		directory, openErr := platformOpenPrivateDirectory(session, session.root, finalName)
		if openErr != nil || directory.raw != raw {
			if directory != nil {
				_ = directory.file.Close()
			}
			return nil, ErrUnsafeObject
		}
		published.dirs = map[string]*boundDirectory{"": directory}
	} else {
		if err := native.Close(); err != nil {
			return nil, fmt.Errorf("close published object failed")
		}
	}
	if err := session.check("after_published_open"); err != nil {
		_ = published.Close()
		return nil, err
	}
	if err := published.checkBinding("after_published_open_binding"); err != nil {
		_ = published.Close()
		return nil, err
	}
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		_ = published.Close()
		return nil, ErrBindingChanged
	}
	session.published[published] = struct{}{}
	session.mu.Unlock()
	return published, nil
}

func (published *Published) Identity() Identity {
	if published == nil || published.closed {
		return Identity{}
	}
	return identityFromRaw(published.raw)
}

func (published *Published) Kind() ObjectKind {
	if published == nil || published.closed {
		return ""
	}
	return published.kind
}

// Check confirms that the root name still denotes this exact publication and
// revalidates every cached descendant directory. Callers performing a long
// verification must call it after consuming files before recording success.
func (published *Published) Check() error {
	if err := published.checkBinding("published_explicit_check"); err != nil {
		return err
	}
	return checkBoundDirectoryMap(published.session, published.dirs)
}

// OpenRegular opens a regular file relative to the published object. A zero
// Path addresses a single-file root publication; directory publications
// require a non-empty component path.
func (published *Published) OpenRegular(ctx context.Context, path Path) (*File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if published == nil || published.closed || published.session == nil {
		return nil, ErrBindingChanged
	}
	if err := published.checkBinding("before_published_regular_open"); err != nil {
		return nil, err
	}
	var parent *boundDirectory
	var name string
	if published.kind == ObjectKindRegular {
		if len(path.components) != 0 {
			return nil, ErrInvalidPath
		}
		parent, name = published.session.root, published.name
	} else {
		if len(path.components) == 0 {
			return nil, ErrInvalidPath
		}
		var err error
		parent, err = published.resolveDirectory(path.components[:len(path.components)-1])
		if err != nil {
			return nil, err
		}
		name = path.components[len(path.components)-1]
	}
	native, raw, err := platformOpenPrivateRegularReadOnly(published.session, parent, name)
	if err != nil {
		return nil, err
	}
	if published.kind == ObjectKindRegular && raw != published.raw {
		_ = native.Close()
		return nil, ErrUnsafeObject
	}
	file, err := published.session.wrapFile(native, raw, nil, published)
	if err != nil {
		return nil, err
	}
	if err := published.checkBinding("after_published_regular_open"); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// Inspect observes one named object through the published root without
// following links. A zero path observes the publication itself.
func (published *Published) Inspect(ctx context.Context, path Path) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}
	if err := published.checkBinding("before_published_inspect"); err != nil {
		return ObjectInfo{}, err
	}
	if len(path.components) == 0 {
		native, raw, kind, err := platformInspectObject(published.session, published.session.root, published.name)
		if err != nil || raw != published.raw || kind != published.kind {
			if native != nil {
				_ = native.Close()
			}
			return ObjectInfo{}, ErrBindingChanged
		}
		info, infoErr := native.Stat()
		closeErr := native.Close()
		if infoErr != nil || closeErr != nil {
			return ObjectInfo{}, ErrUnsafeObject
		}
		result := ObjectInfo{Identity: identityFromRaw(raw), Kind: kind}
		if kind == ObjectKindRegular {
			result.SizeBytes = info.Size()
		}
		if err := published.checkBinding("after_published_inspect"); err != nil {
			return ObjectInfo{}, err
		}
		return result, nil
	}
	if published.kind != ObjectKindDirectory {
		return ObjectInfo{}, ErrInvalidPath
	}
	parent, err := published.resolveDirectory(path.components[:len(path.components)-1])
	if err != nil {
		return ObjectInfo{}, err
	}
	native, raw, kind, err := platformInspectObject(published.session, parent, path.components[len(path.components)-1])
	if err != nil {
		return ObjectInfo{}, err
	}
	info, infoErr := native.Stat()
	closeErr := native.Close()
	if infoErr != nil || closeErr != nil {
		return ObjectInfo{}, ErrUnsafeObject
	}
	result := ObjectInfo{Identity: identityFromRaw(raw), Kind: kind}
	if kind == ObjectKindRegular {
		result.SizeBytes = info.Size()
	}
	if err := published.checkBinding("after_published_inspect"); err != nil {
		return ObjectInfo{}, err
	}
	return result, nil
}

// List returns a bounded deterministic inventory of one published directory.
func (published *Published) List(ctx context.Context, path Path, limits ListLimits) (ListResult, error) {
	result := ListResult{Limits: limits, Entries: []Entry{}}
	if err := limits.Validate(); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if published == nil || published.closed || published.kind != ObjectKindDirectory {
		return result, ErrInvalidPath
	}
	if err := published.checkBinding("before_published_list"); err != nil {
		return result, err
	}
	directory, err := published.resolveDirectory(path.components)
	if err != nil {
		return result, err
	}
	result, err = listBoundDirectory(ctx, published.session, directory, limits, "after_published_list")
	if err != nil {
		return result, err
	}
	if err := published.checkBinding("after_published_list_binding"); err != nil {
		result.Complete = false
		return result, err
	}
	return result, nil
}

func (published *Published) checkBinding(stage string) error {
	if published == nil || published.closed || published.session == nil {
		return ErrBindingChanged
	}
	if err := published.session.check(stage); err != nil {
		return err
	}
	native, raw, kind, err := platformInspectObject(published.session, published.session.root, published.name)
	if native != nil {
		_ = native.Close()
	}
	if err != nil || raw != published.raw || kind != published.kind {
		return ErrBindingChanged
	}
	return nil
}

func (published *Published) resolveDirectory(components []string) (*boundDirectory, error) {
	if published.kind != ObjectKindDirectory || published.dirs == nil {
		return nil, ErrUnsafeObject
	}
	for index, name := range components {
		key := pathKey(components[:index+1])
		if published.dirs[key] != nil {
			continue
		}
		parent := published.dirs[pathKey(components[:index])]
		if parent == nil {
			return nil, ErrUnsafeObject
		}
		directory, err := platformOpenPrivateDirectory(published.session, parent, name)
		if err != nil {
			return nil, err
		}
		published.dirs[key] = directory
	}
	return published.dirs[pathKey(components)], nil
}

func (published *Published) Close() error {
	if published == nil || published.closed {
		return nil
	}
	published.closed = true
	if published.session != nil {
		published.session.mu.Lock()
		delete(published.session.published, published)
		ownedFiles := make([]*File, 0)
		for file := range published.session.files {
			if file.published == published {
				ownedFiles = append(ownedFiles, file)
			}
		}
		published.session.mu.Unlock()
		for _, file := range ownedFiles {
			_ = file.Close()
		}
	}
	keys := make([]string, 0, len(published.dirs))
	for key := range published.dirs {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	var first error
	for _, key := range keys {
		if directory := published.dirs[key]; directory != nil && directory.file != nil {
			if err := directory.file.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	published.dirs = nil
	return first
}

func (subtree *Subtree) Identity() Identity {
	if subtree == nil || subtree.base == nil || subtree.closed {
		return Identity{}
	}
	return subtree.base.identity
}

// Check explicitly revalidates the named operation subtree and every cached
// descendant directory. Ordinary operations only revalidate the top-level
// binding so their cost does not grow with the number of previously visited
// directories; callers must use Check at integrity boundaries.
func (subtree *Subtree) Check() error {
	if err := subtree.checkBinding("subtree_explicit_check"); err != nil {
		return err
	}
	return checkBoundDirectoryMap(subtree.session, subtree.dirs)
}

// CheckPaths revalidates the named root, operation subtree, operation lock,
// and only the cached directory paths named by paths plus their ancestors. It
// is intended for narrow integrity boundaries; Check remains the full cached
// directory graph proof. Callers must explicitly pass an empty Path when only
// the operation subtree itself is relevant.
func (subtree *Subtree) CheckPaths(paths ...Path) error {
	if len(paths) == 0 || len(paths) > maxPathComponents {
		return ErrInvalidPath
	}
	if err := subtree.checkBinding("subtree_paths_check"); err != nil {
		return err
	}
	keys := make(map[string]struct{})
	for _, path := range paths {
		if len(path.components) > maxPathComponents {
			return ErrInvalidPath
		}
		for depth := 1; depth <= len(path.components); depth++ {
			key := pathKey(path.components[:depth])
			if subtree.dirs[key] == nil {
				return ErrNotFound
			}
			keys[key] = struct{}{}
		}
	}
	selected := make([]string, 0, len(keys))
	for key := range keys {
		selected = append(selected, key)
	}
	return checkBoundDirectoryKeys(subtree.session, subtree.dirs, selected)
}

func (subtree *Subtree) MkdirAll(ctx context.Context, path Path) (MkdirReceipt, error) {
	receipt := MkdirReceipt{Durability: durabilityConfirmed}
	if err := subtree.usable(ctx, "before_mkdir_all"); err != nil {
		return receipt, err
	}
	components := path.components
	for index, name := range components {
		if err := ctx.Err(); err != nil {
			return receipt, err
		}
		key := pathKey(components[:index+1])
		if subtree.dirs[key] != nil {
			continue
		}
		parent := subtree.dirs[pathKey(components[:index])]
		if parent == nil {
			return receipt, ErrUnsafeObject
		}
		directory, created, durable, err := platformCreatePrivateDirectory(subtree.session, parent, name)
		if created {
			receipt.DirectoriesCreated++
			if !durable {
				receipt.Durability = durabilityUnconfirmed
			}
		}
		if errors.Is(err, ErrAlreadyExists) {
			directory, err = platformOpenPrivateDirectory(subtree.session, parent, name)
		}
		if err != nil {
			if directory != nil {
				_ = directory.file.Close()
			}
			return receipt, err
		}
		subtree.dirs[key] = directory
	}
	if err := subtree.checkBinding("after_mkdir_all"); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func (subtree *Subtree) CreateRegular(ctx context.Context, path Path) (*File, error) {
	return subtree.openRegular(ctx, path, true)
}

func (subtree *Subtree) OpenRegular(ctx context.Context, path Path) (*File, error) {
	return subtree.openRegular(ctx, path, false)
}

// Inspect opens the named object without following links or crossing the
// reviewed filesystem and returns only a path-free observation.
func (subtree *Subtree) Inspect(ctx context.Context, path Path) (ObjectInfo, error) {
	if err := subtree.usable(ctx, "before_inspect"); err != nil {
		return ObjectInfo{}, err
	}
	parent, name, err := subtree.resolveTarget(path)
	if err != nil {
		return ObjectInfo{}, err
	}
	native, raw, kind, err := platformInspectObject(subtree.session, parent, name)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer native.Close()
	info, err := native.Stat()
	if err != nil {
		return ObjectInfo{}, ErrUnsafeObject
	}
	result := ObjectInfo{Identity: identityFromRaw(raw), Kind: kind}
	if kind == ObjectKindRegular {
		result.SizeBytes = info.Size()
	}
	if err := subtree.checkBinding("after_inspect"); err != nil {
		return ObjectInfo{}, err
	}
	return result, nil
}

func (subtree *Subtree) openRegular(ctx context.Context, path Path, create bool) (*File, error) {
	stage := "before_file_open"
	if create {
		stage = "before_file_create"
	}
	if err := subtree.usable(ctx, stage); err != nil {
		return nil, err
	}
	parent, name, err := subtree.resolveTarget(path)
	if err != nil {
		return nil, err
	}
	var native *os.File
	var raw rawIdentity
	if create {
		native, raw, err = platformCreatePrivateRegular(subtree.session, parent, name)
	} else {
		native, raw, err = platformOpenPrivateRegular(subtree.session, parent, name)
	}
	if err != nil {
		return nil, err
	}
	file, err := subtree.session.wrapFile(native, raw, subtree, nil)
	if err != nil {
		return nil, err
	}
	after := "after_file_open"
	if create {
		after = "after_file_create"
	}
	if err := subtree.checkBinding(after); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (session *Session) wrapFile(native *os.File, raw rawIdentity, subtree *Subtree, published *Published) (*File, error) {
	file := &File{file: native, session: session, subtree: subtree, published: published, identity: identityFromRaw(raw), raw: raw}
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		_ = native.Close()
		return nil, ErrBindingChanged
	}
	session.files[file] = struct{}{}
	session.mu.Unlock()
	return file, nil
}

func (subtree *Subtree) SyncDirectory(ctx context.Context, path Path) error {
	if err := subtree.usable(ctx, "before_directory_sync"); err != nil {
		return err
	}
	directory, err := subtree.resolveDirectory(path)
	if err != nil {
		return err
	}
	if err := platformSyncDirectory(directory); err != nil {
		return err
	}
	return subtree.checkBinding("after_directory_sync")
}

func (subtree *Subtree) List(ctx context.Context, path Path, limits ListLimits) (ListResult, error) {
	result := ListResult{Limits: limits, Entries: []Entry{}}
	if err := limits.Validate(); err != nil {
		return result, err
	}
	if err := subtree.usable(ctx, "before_directory_list"); err != nil {
		return result, err
	}
	directory, err := subtree.resolveDirectory(path)
	if err != nil {
		return result, err
	}
	result, err = listBoundDirectory(ctx, subtree.session, directory, limits, "after_directory_list")
	if err != nil {
		return result, err
	}
	if err := subtree.checkBinding("after_directory_list_binding"); err != nil {
		result.Complete = false
		return result, err
	}
	return result, nil
}

// ListRoot provides a bounded, deterministic root inventory for discovering
// explicitly named operation subtrees. It does not open or trust the entries.
func (session *Session) ListRoot(ctx context.Context, limits ListLimits) (ListResult, error) {
	result := ListResult{Limits: limits, Entries: []Entry{}}
	if err := limits.Validate(); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := session.check("before_root_list"); err != nil {
		return result, err
	}
	return listBoundDirectory(ctx, session, session.root, limits, "after_root_list")
}

// InspectRoot checks one explicit publication name without following links.
// Existing public target content need not use fsbind's private owner policy;
// only its type, identity, and reviewed filesystem are observed.
func (session *Session) InspectRoot(ctx context.Context, name string) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}
	if validateSingleComponent(name) != nil {
		return ObjectInfo{}, ErrInvalidPath
	}
	if err := session.check("before_root_inspect"); err != nil {
		return ObjectInfo{}, err
	}
	native, raw, kind, err := platformInspectRootObject(session, name)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer native.Close()
	info, err := native.Stat()
	if err != nil {
		return ObjectInfo{}, ErrUnsafeObject
	}
	result := ObjectInfo{Identity: identityFromRaw(raw), Kind: kind}
	if kind == ObjectKindRegular {
		result.SizeBytes = info.Size()
	}
	if err := session.check("after_root_inspect"); err != nil {
		return ObjectInfo{}, err
	}
	return result, nil
}

// InspectRootRegular observes one ordinary regular-file name immediately
// beneath the bound root. Unlike private operation-state methods, it does not
// require owner-only mode or a single link. The returned identity is only an
// observation; RemoveRootRegularExact must receive it again before unlinking.
func (session *Session) InspectRootRegular(ctx context.Context, name string) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}
	if validateSingleComponent(name) != nil {
		return ObjectInfo{}, ErrInvalidPath
	}
	if err := session.check("before_root_regular_inspect"); err != nil {
		return ObjectInfo{}, err
	}
	native, raw, err := platformOpenRootRegular(session, name, false)
	if err != nil {
		return ObjectInfo{}, err
	}
	info, infoErr := native.Stat()
	closeErr := native.Close()
	if infoErr != nil || closeErr != nil || !info.Mode().IsRegular() {
		return ObjectInfo{}, ErrUnsafeObject
	}
	if err := session.check("after_root_regular_inspect"); err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Identity: identityFromRaw(raw), Kind: ObjectKindRegular, SizeBytes: info.Size()}, nil
}

// OpenRootRegular opens one ordinary regular-file name without following a
// link or reparse point. Callers must close the returned handle before any
// removal attempt; an open bound handle deliberately blocks removal.
func (session *Session) OpenRootRegular(ctx context.Context, name string) (*File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if validateSingleComponent(name) != nil {
		return nil, ErrInvalidPath
	}
	if err := session.check("before_root_regular_open"); err != nil {
		return nil, err
	}
	native, raw, err := platformOpenRootRegular(session, name, false)
	if err != nil {
		return nil, err
	}
	file, err := session.wrapFile(native, raw, nil, nil)
	if err != nil {
		return nil, err
	}
	if err := session.check("after_root_regular_open"); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// RemoveRootRegularExact removes exactly one ordinary regular-file name from
// the bound root. It verifies the previously observed identity and size, never
// follows links, never removes a directory, and fsyncs the parent after a
// visible unlink. An absent name is not treated as a successful first attempt;
// recovery policy belongs to the caller's durable journal.
func (session *Session) RemoveRootRegularExact(ctx context.Context, name string, expected Identity, expectedSize int64) (Removal, error) {
	receipt := Removal{Durability: durabilityNotPublished, Kind: ObjectKindRegular}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if validateSingleComponent(name) != nil || expected.IsZero() || expectedSize < 0 {
		return receipt, ErrInvalidPath
	}
	if err := session.check("before_root_regular_remove"); err != nil {
		return receipt, err
	}
	probe, raw, err := platformOpenRootRegular(session, name, true)
	if err != nil {
		return receipt, err
	}
	info, infoErr := probe.Stat()
	closeErr := probe.Close()
	if infoErr != nil || closeErr != nil || !info.Mode().IsRegular() || info.Size() != expectedSize ||
		!identityFromRaw(raw).Equal(expected) {
		return receipt, ErrUnsafeObject
	}
	receipt.Identity, receipt.SizeBytes = expected, expectedSize
	if session.hasOpenIdentity(raw) {
		return receipt, fmt.Errorf("remove bound root regular: handle remains open")
	}
	receipt.Attempted = true
	removed, durable, removedRaw, removeErr := platformRemoveRootRegular(session, name, raw, expectedSize)
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
	bindingErr := session.check("after_root_regular_remove")
	if removeErr != nil && bindingErr != nil {
		return receipt, errors.Join(removeErr, bindingErr)
	}
	if removeErr != nil {
		return receipt, removeErr
	}
	return receipt, bindingErr
}

// RemoveRootEmptyDirectoryExact removes exactly one ordinary empty directory
// directly beneath the bound root. It never follows links, requires the exact
// previously reviewed directory identity, performs a bounded empty-directory
// inventory immediately before the attempt, and fsyncs the parent after a
// visible removal. An absent name is not accepted as a successful first
// attempt; crash recovery policy belongs to the caller's durable journal.
func (session *Session) RemoveRootEmptyDirectoryExact(ctx context.Context, name string, expected Identity) (Removal, error) {
	receipt := Removal{Durability: durabilityNotPublished, Kind: ObjectKindDirectory}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if validateSingleComponent(name) != nil || expected.IsZero() {
		return receipt, ErrInvalidPath
	}
	if err := session.check("before_root_empty_directory_remove"); err != nil {
		return receipt, err
	}
	probe, raw, kind, err := platformInspectRootObject(session, name)
	if err != nil {
		return receipt, err
	}
	directory := &boundDirectory{file: probe, raw: raw, identity: identityFromRaw(raw)}
	if kind != ObjectKindDirectory || !directory.identity.Equal(expected) {
		_ = probe.Close()
		return receipt, ErrUnsafeObject
	}
	listing, listErr := listBoundDirectory(ctx, session, directory,
		ListLimits{MaxEntries: 1, MaxNameBytes: 64 << 10}, "after_root_empty_directory_list")
	receipt.NamespaceRead = true
	receipt.EntriesExamined = listing.Used.EntriesExamined
	receipt.NameBytesExamined = listing.Used.NameBytes
	closeErr := probe.Close()
	if listErr != nil {
		return receipt, listErr
	}
	if closeErr != nil {
		return receipt, fmt.Errorf("close bound root directory failed")
	}
	if !listing.Complete || len(listing.Entries) != 0 {
		return receipt, ErrNotEmpty
	}
	if session.hasAnyOpenFiles() {
		return receipt, fmt.Errorf("remove bound root directory: handle remains open")
	}
	receipt.Identity = expected
	receipt.Attempted = true
	removed, durable, removedRaw, removeErr := platformRemoveRootEmptyDirectory(session, name, raw)
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
	bindingErr := session.check("after_root_empty_directory_remove")
	if removeErr != nil && bindingErr != nil {
		return receipt, errors.Join(removeErr, bindingErr)
	}
	if removeErr != nil {
		return receipt, removeErr
	}
	return receipt, bindingErr
}

func listBoundDirectory(ctx context.Context, session *Session, directory *boundDirectory, limits ListLimits, afterStage string) (ListResult, error) {
	result := ListResult{Limits: limits, Entries: []Entry{}}
	entries, err := platformReadDirectory(directory, limits.MaxEntries+1)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			result.StopReason = "context_cancelled"
			return result, err
		}
		result.Used.EntriesExamined++
		name := entry.Name()
		result.Used.NameBytes += int64(len(name))
		if result.Used.EntriesExamined > limits.MaxEntries {
			result.StopReason = "max_entries"
			result.Entries = nil
			return result, nil
		}
		if result.Used.NameBytes > limits.MaxNameBytes {
			result.StopReason = "max_name_bytes"
			result.Entries = nil
			return result, nil
		}
		if name != operationLockName {
			if _, err := PathFromComponents([]string{name}); err != nil {
				result.StopReason = "unsafe_entry"
				result.Entries = nil
				return result, ErrUnsafeObject
			}
		}
		kind := "other"
		if entry.Type()&os.ModeSymlink != 0 {
			kind = "unsafe"
		} else if entry.IsDir() {
			kind = "directory"
		} else if entry.Type().IsRegular() {
			kind = "regular"
		}
		result.Entries = append(result.Entries, Entry{Name: name, Kind: kind})
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Name < result.Entries[j].Name })
	result.Complete = true
	if err := session.check(afterStage); err != nil {
		result.Complete = false
		return result, err
	}
	return result, nil
}

// PublishNoReplace moves one closed source file or directory from the owned
// subtree to a single-component destination in the bound root.
func (session *Session) PublishNoReplace(ctx context.Context, subtree *Subtree, source Path, finalName string) (Publication, error) {
	receipt := Publication{Durability: durabilityNotPublished}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if subtree == nil || subtree.session != session || subtree.closed || len(source.components) == 0 || validateSingleComponent(finalName) != nil {
		return receipt, ErrInvalidPath
	}
	if err := session.check("before_publish"); err != nil {
		return receipt, err
	}
	sourceParent, sourceName, err := subtree.resolveTarget(source)
	if err != nil {
		return receipt, err
	}
	probe, _, sourceKind, probeErr := platformInspectObject(session, sourceParent, sourceName)
	if probeErr != nil {
		return receipt, probeErr
	}
	if closeErr := probe.Close(); closeErr != nil {
		return receipt, fmt.Errorf("close publication source failed")
	}
	if sourceKind == ObjectKindDirectory {
		if err := subtree.releaseDirectoryTree(source); err != nil {
			return receipt, err
		}
	}
	receipt, publishErr := session.publishBoundNoReplace(
		receipt, sourceParent, sourceName, session.root, finalName,
		"after_publish",
	)
	bindingErr := subtree.checkBinding("after_publish_subtree_binding")
	if publishErr != nil && bindingErr != nil {
		return receipt, errors.Join(publishErr, bindingErr)
	}
	if publishErr != nil {
		return receipt, publishErr
	}
	return receipt, bindingErr
}

func (subtree *Subtree) releaseDirectoryTree(path Path) error {
	prefix := pathKey(path.components)
	keys := make([]string, 0)
	for key := range subtree.dirs {
		if key == prefix || strings.HasPrefix(key, prefix+"\x00") {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, key := range keys {
		directory := subtree.dirs[key]
		if directory != nil && directory.file != nil {
			if err := directory.file.Close(); err != nil {
				return fmt.Errorf("close publication directory failed")
			}
		}
		delete(subtree.dirs, key)
	}
	return nil
}

// CommitRegularNoReplace atomically renames a closed scratch regular file to a
// final name in the same operation subtree. It never replaces an existing
// object and reports a visible publication even when durability confirmation
// or the final root-binding check fails.
func (subtree *Subtree) CommitRegularNoReplace(ctx context.Context, source, destination Path) (Publication, error) {
	receipt := Publication{Durability: durabilityNotPublished}
	if err := subtree.usable(ctx, "before_subtree_commit"); err != nil {
		return receipt, err
	}
	if pathKey(source.components) == pathKey(destination.components) {
		return receipt, ErrAlreadyExists
	}
	sourceParent, sourceName, err := subtree.resolveTarget(source)
	if err != nil {
		return receipt, err
	}
	destinationParent, destinationName, err := subtree.resolveTarget(destination)
	if err != nil {
		return receipt, err
	}
	receipt, publishErr := subtree.session.publishBoundNoReplace(
		receipt, sourceParent, sourceName, destinationParent, destinationName,
		"after_subtree_commit",
	)
	bindingErr := subtree.checkBinding("after_subtree_commit_binding")
	if publishErr != nil && bindingErr != nil {
		return receipt, errors.Join(publishErr, bindingErr)
	}
	if publishErr != nil {
		return receipt, publishErr
	}
	return receipt, bindingErr
}

// RemoveRegular removes one closed, owner-private, single-link regular file.
// The caller must supply the identity obtained by a prior bounded namespace
// audit. The method never follows links and never accepts an absent name as a
// successful first attempt.
func (subtree *Subtree) RemoveRegular(ctx context.Context, path Path, expected Identity) (Removal, error) {
	return subtree.removeBoundObject(ctx, path, expected, ObjectKindRegular, -1)
}

// RemoveRegularExact additionally requires the regular-file size observed by
// the caller's complete preflight. A size change fails before the unlink
// attempt, so a stale byte-budget observation cannot silently authorize a
// larger removal.
func (subtree *Subtree) RemoveRegularExact(ctx context.Context, path Path, expected Identity, expectedSize int64) (Removal, error) {
	if expectedSize < 0 {
		return Removal{Durability: durabilityNotPublished, Kind: ObjectKindRegular}, ErrInvalidPath
	}
	return subtree.removeBoundObject(ctx, path, expected, ObjectKindRegular, expectedSize)
}

// RemoveEmptyDirectory removes one closed, owner-private empty directory. The
// caller is responsible for a complete bounded child inventory and post-order
// traversal before invoking this method.
func (subtree *Subtree) RemoveEmptyDirectory(ctx context.Context, path Path, expected Identity) (Removal, error) {
	return subtree.removeBoundObject(ctx, path, expected, ObjectKindDirectory, -1)
}

func (subtree *Subtree) removeBoundObject(ctx context.Context, path Path, expected Identity, wanted ObjectKind, expectedSize int64) (Removal, error) {
	receipt := Removal{Durability: durabilityNotPublished, Kind: wanted}
	if len(path.components) == 0 || expected.IsZero() || (wanted != ObjectKindRegular && wanted != ObjectKindDirectory) {
		return receipt, ErrInvalidPath
	}
	if err := subtree.usable(ctx, "before_subtree_remove"); err != nil {
		return receipt, err
	}
	parent, name, err := subtree.resolveTarget(path)
	if err != nil {
		return receipt, err
	}
	probe, actual, kind, inspectErr := platformInspectObject(subtree.session, parent, name)
	if inspectErr != nil {
		return receipt, inspectErr
	}
	info, statErr := probe.Stat()
	closeErr := probe.Close()
	if statErr != nil {
		return receipt, fmt.Errorf("inspect bound removal object failed")
	}
	if closeErr != nil {
		return receipt, fmt.Errorf("close bound removal object failed")
	}
	if kind != wanted || !identityFromRaw(actual).Equal(expected) {
		return receipt, ErrUnsafeObject
	}
	receipt.Identity = expected
	if wanted == ObjectKindRegular {
		receipt.SizeBytes = info.Size()
		if expectedSize >= 0 && receipt.SizeBytes != expectedSize {
			return receipt, ErrUnsafeObject
		}
	}
	if subtree.session.hasOpenIdentity(actual) || (wanted == ObjectKindDirectory && subtree.session.hasAnyOpenFiles()) {
		return receipt, fmt.Errorf("remove bound object: handle remains open")
	}
	if wanted == ObjectKindDirectory {
		if err := subtree.releaseDirectoryTree(path); err != nil {
			return receipt, err
		}
	}
	receipt.Attempted = true
	removed, durable, removedRaw, removeErr := platformRemoveObject(subtree.session, parent, name, wanted == ObjectKindDirectory, actual, expectedSize)
	if removed {
		receipt.Removed = true
		receipt.Durability = durabilityUnconfirmed
		if durable {
			receipt.Durability = durabilityConfirmed
		}
		if removedRaw != (rawIdentity{}) && removedRaw != actual {
			removeErr = errors.Join(removeErr, ErrRemovalAmbiguous)
		}
	}
	if removeErr == nil && !removed {
		removeErr = ErrRemovalAmbiguous
	}
	bindingErr := subtree.checkBinding("after_subtree_remove_binding")
	if removeErr != nil && bindingErr != nil {
		return receipt, errors.Join(removeErr, bindingErr)
	}
	if removeErr != nil {
		return receipt, removeErr
	}
	return receipt, bindingErr
}

func (session *Session) publishBoundNoReplace(receipt Publication, sourceParent *boundDirectory, sourceName string, destinationParent *boundDirectory, destinationName, afterStage string) (Publication, error) {
	native, expected, kind, err := platformInspectObject(session, sourceParent, sourceName)
	if err != nil {
		return receipt, err
	}
	if closeErr := native.Close(); closeErr != nil {
		return receipt, fmt.Errorf("close publication source failed")
	}
	if afterStage == "after_subtree_commit" && kind != ObjectKindRegular {
		return receipt, ErrUnsafeObject
	}
	receipt.SourceIdentity = identityFromRaw(expected)
	if session.hasOpenIdentity(expected) || (kind == ObjectKindDirectory && session.hasAnyOpenFiles()) {
		return receipt, fmt.Errorf("publish bound object: source handle remains open")
	}
	receipt.Attempted = true
	published, durable, finalRaw, publishErr := platformPublishNoReplace(
		session, sourceParent, sourceName, destinationParent, destinationName,
		kind == ObjectKindDirectory, expected,
	)
	if published {
		receipt.Published = true
		receipt.FinalIdentity = identityFromRaw(finalRaw)
		if durable {
			receipt.Durability = durabilityConfirmed
		} else {
			receipt.Durability = durabilityUnconfirmed
		}
	}
	bindingErr := session.check(afterStage)
	if publishErr != nil {
		if bindingErr != nil {
			return receipt, errors.Join(publishErr, bindingErr)
		}
		return receipt, publishErr
	}
	if bindingErr != nil {
		return receipt, bindingErr
	}
	return receipt, nil
}

func (session *Session) hasAnyOpenFiles() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return len(session.files) != 0
}

func (subtree *Subtree) usable(ctx context.Context, stage string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if subtree == nil || subtree.session == nil || subtree.closed || subtree.lockFile == nil {
		return ErrBindingChanged
	}
	return subtree.checkBinding(stage)
}

func (subtree *Subtree) checkBinding(stage string) error {
	if subtree == nil || subtree.session == nil || subtree.closed || subtree.base == nil || subtree.lockFile == nil {
		return ErrBindingChanged
	}
	if err := subtree.session.check(stage); err != nil {
		return err
	}
	native, raw, kind, err := platformInspectObject(subtree.session, subtree.session.root, subtree.name)
	if native != nil {
		_ = native.Close()
	}
	if err != nil || kind != ObjectKindDirectory || raw != subtree.base.raw {
		return ErrBindingChanged
	}
	if err := platformCheckOperationLock(subtree.session, subtree.base, subtree.lockFile, subtree.lockRaw); err != nil {
		return ErrBindingChanged
	}
	return nil
}

// boundDirectoryCheckHook is a deterministic package test seam for proving
// that full cached-directory scans occur only on explicit Check calls.
var boundDirectoryCheckHook func(key string)

func checkBoundDirectoryMap(session *Session, directories map[string]*boundDirectory) error {
	if len(directories) <= 1 {
		return nil
	}
	keys := make([]string, 0, len(directories)-1)
	for key := range directories {
		if key != "" {
			keys = append(keys, key)
		}
	}
	return checkBoundDirectoryKeys(session, directories, keys)
}

func checkBoundDirectoryKeys(session *Session, directories map[string]*boundDirectory, keys []string) error {
	sort.Slice(keys, func(i, j int) bool {
		leftDepth, rightDepth := strings.Count(keys[i], "\x00"), strings.Count(keys[j], "\x00")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return keys[i] < keys[j]
	})
	for _, key := range keys {
		if boundDirectoryCheckHook != nil {
			boundDirectoryCheckHook(key)
		}
		components := strings.Split(key, "\x00")
		parent := directories[pathKey(components[:len(components)-1])]
		expected := directories[key]
		if parent == nil || expected == nil {
			return ErrBindingChanged
		}
		current, err := platformOpenPrivateDirectory(session, parent, components[len(components)-1])
		if err != nil {
			return ErrBindingChanged
		}
		matches := current.raw == expected.raw
		closeErr := current.file.Close()
		if !matches || closeErr != nil {
			return ErrBindingChanged
		}
	}
	return nil
}

func (subtree *Subtree) resolveTarget(path Path) (*boundDirectory, string, error) {
	if len(path.components) == 0 {
		return nil, "", ErrInvalidPath
	}
	parentPath := Path{components: path.components[:len(path.components)-1]}
	parent, err := subtree.resolveDirectory(parentPath)
	if err != nil {
		return nil, "", err
	}
	return parent, path.components[len(path.components)-1], nil
}

func (subtree *Subtree) resolveDirectory(path Path) (*boundDirectory, error) {
	if subtree == nil || subtree.session == nil {
		return nil, ErrBindingChanged
	}
	if directory := subtree.dirs[pathKey(path.components)]; directory != nil {
		return directory, nil
	}
	for index, name := range path.components {
		key := pathKey(path.components[:index+1])
		if subtree.dirs[key] != nil {
			continue
		}
		parent := subtree.dirs[pathKey(path.components[:index])]
		if parent == nil {
			return nil, ErrUnsafeObject
		}
		directory, err := platformOpenPrivateDirectory(subtree.session, parent, name)
		if err != nil {
			return nil, err
		}
		subtree.dirs[key] = directory
	}
	directory := subtree.dirs[pathKey(path.components)]
	if directory == nil {
		return nil, ErrNotFound
	}
	return directory, nil
}

func validateSingleComponent(value string) error {
	path, err := PathFromComponents([]string{value})
	if err != nil || len(path.components) != 1 || value == operationLockName {
		return ErrInvalidPath
	}
	return nil
}

func (session *Session) hasOpenIdentity(raw rawIdentity) bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	for file := range session.files {
		if file != nil && !file.closed && file.raw == raw {
			return true
		}
	}
	return false
}

func (file *File) Identity() Identity {
	if file == nil {
		return Identity{}
	}
	return file.identity
}

func (file *File) Read(buffer []byte) (int, error) {
	if file == nil {
		return 0, os.ErrClosed
	}
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.file == nil || file.closed {
		return 0, os.ErrClosed
	}
	return file.file.Read(buffer)
}
func (file *File) Write(buffer []byte) (int, error) {
	if file == nil {
		return 0, os.ErrClosed
	}
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.file == nil || file.closed {
		return 0, os.ErrClosed
	}
	return file.file.Write(buffer)
}
func (file *File) Seek(offset int64, whence int) (int64, error) {
	if file == nil {
		return 0, os.ErrClosed
	}
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.file == nil || file.closed {
		return 0, os.ErrClosed
	}
	return file.file.Seek(offset, whence)
}
func (file *File) Sync() error {
	if file == nil {
		return os.ErrClosed
	}
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.file == nil || file.closed {
		return os.ErrClosed
	}
	return platformSyncFile(file.file)
}
func (file *File) Info() (FileInfo, error) {
	if file == nil {
		return FileInfo{}, os.ErrClosed
	}
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.file == nil || file.closed {
		return FileInfo{}, os.ErrClosed
	}
	info, err := file.file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return FileInfo{}, ErrUnsafeObject
	}
	return FileInfo{Identity: file.identity, SizeBytes: info.Size(), Modified: info.ModTime().UTC()}, nil
}

// Stat returns metadata from the already-bound native handle. It performs no
// pathname lookup and therefore preserves the caller's no-follow authority.
func (file *File) Stat() (os.FileInfo, error) {
	if file == nil {
		return nil, os.ErrClosed
	}
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.file == nil || file.closed {
		return nil, os.ErrClosed
	}
	return file.file.Stat()
}
func (file *File) Close() error {
	if file == nil {
		return nil
	}
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.closed {
		return nil
	}
	file.closed = true
	if file.session != nil {
		file.session.mu.Lock()
		delete(file.session.files, file)
		file.session.mu.Unlock()
	}
	return file.file.Close()
}

var _ io.ReadWriteSeeker = (*File)(nil)

func (subtree *Subtree) Close() error {
	if subtree == nil || subtree.closed {
		return nil
	}
	subtree.closed = true
	if subtree.session != nil {
		subtree.session.mu.Lock()
		delete(subtree.session.subtrees, subtree)
		ownedFiles := make([]*File, 0)
		for file := range subtree.session.files {
			if file.subtree == subtree {
				ownedFiles = append(ownedFiles, file)
			}
		}
		subtree.session.mu.Unlock()
		for _, file := range ownedFiles {
			_ = file.Close()
		}
	}
	var first error
	if subtree.lockFile != nil {
		if err := platformReleaseOperationLock(subtree.lockFile, subtree.base); err != nil {
			first = err
		}
		subtree.lockFile = nil
		subtree.lockRaw = rawIdentity{}
	}
	keys := make([]string, 0, len(subtree.dirs))
	for key := range subtree.dirs {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, key := range keys {
		if directory := subtree.dirs[key]; directory != nil && directory.file != nil {
			if err := directory.file.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	subtree.dirs = nil
	return first
}

func (session *Session) Close() error {
	if session == nil {
		return nil
	}
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return nil
	}
	session.closed = true
	files := make([]*File, 0, len(session.files))
	for file := range session.files {
		files = append(files, file)
	}
	subtrees := make([]*Subtree, 0, len(session.subtrees))
	for subtree := range session.subtrees {
		subtrees = append(subtrees, subtree)
	}
	published := make([]*Published, 0, len(session.published))
	for item := range session.published {
		published = append(published, item)
	}
	session.mu.Unlock()
	var first error
	for _, file := range files {
		if err := file.Close(); err != nil && first == nil {
			first = err
		}
	}
	for _, subtree := range subtrees {
		if err := subtree.Close(); err != nil && first == nil {
			first = err
		}
	}
	for _, item := range published {
		if err := item.Close(); err != nil && first == nil {
			first = err
		}
	}
	if err := platformCloseSession(session); err != nil && first == nil {
		first = err
	}
	return first
}
