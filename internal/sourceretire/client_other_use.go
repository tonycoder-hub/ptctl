package sourceretire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

const (
	otherClientUseOverlapCode    = "source.other_client_job_overlap"
	otherClientUseUnmappedCode   = "source.other_client_job_unmapped"
	otherClientUseIncompleteCode = "client.other_use_enumeration_incomplete"
)

var (
	// errOtherClientUseOverlap means another downloader job may still use a
	// selected source name. It is a policy block, never a retryable failure.
	errOtherClientUseOverlap = errors.New("another downloader job references a selected source name, one of its parent directories, or the same file object")
	// errOtherClientUseUnmapped means another downloader job cannot be
	// translated into the host namespace, so non-overlap cannot be proven.
	errOtherClientUseUnmapped = errors.New("another downloader job is outside the reviewed host/client path mapping, so it cannot be proven not to use the source")
	// errOtherClientUseIncomplete means the complete job ledger, or a host
	// observation needed to compare it, was unavailable.
	errOtherClientUseIncomplete = fmt.Errorf("%w: downloader jobs could not be completely enumerated and compared with the source", ErrIncomplete)
)

// recordingLedgerSession keeps the job ledger returned by the latest read so
// the same response that proved current use of the plan job can also prove
// that no other job references the source. It adds no request of its own.
type recordingLedgerSession struct {
	downloader.LedgerSession
	reads    int
	snapshot downloader.LedgerSnapshot
	err      error
}

func recordLedgerSession(session downloader.LedgerSession) *recordingLedgerSession {
	if recorded, ok := session.(*recordingLedgerSession); ok && recorded != nil {
		return recorded
	}
	return &recordingLedgerSession{LedgerSession: session}
}

func (session *recordingLedgerSession) ReadLedger(ctx context.Context) (downloader.LedgerSnapshot, error) {
	snapshot, err := session.LedgerSession.ReadLedger(ctx)
	session.reads++
	session.snapshot, session.err = snapshot, err
	return snapshot, err
}

// ledgerSince returns the one ledger read made after readsBefore. Anything
// other than exactly one successful read is an incomplete enumeration.
func (session *recordingLedgerSession) ledgerSince(readsBefore int) (downloader.LedgerSnapshot, error) {
	if session.reads != readsBefore+1 {
		return downloader.LedgerSnapshot{}, fmt.Errorf("%w: expected exactly one job-ledger read", errOtherClientUseIncomplete)
	}
	if session.err != nil {
		return downloader.LedgerSnapshot{}, fmt.Errorf("%w: %w", errOtherClientUseIncomplete, session.err)
	}
	return session.snapshot, nil
}

// otherClientUseGuard compares every downloader job except the reviewed plan
// job with the selected source names. Downloader paths are untrusted claims:
// they are translated only through the exact reviewed host/client mapping,
// confined below its host root, and inspected with metadata-only calls.
type otherClientUseGuard struct {
	final      *materialize.VerifiedFinal
	activation clientactivate.Plan
	jobKey     string
}

func newOtherClientUseGuard(final *materialize.VerifiedFinal, activation *clientactivate.VerifiedCompletion, use *clientactivate.VerifiedCurrentUse) (otherClientUseGuard, error) {
	if final == nil || !final.Verified() || activation == nil || !activation.Verified() {
		return otherClientUseGuard{}, fmt.Errorf("%w: final or activation authority is unavailable", errOtherClientUseIncomplete)
	}
	request, ok := use.MutationRequest()
	if !ok || request.JobKey == "" {
		return otherClientUseGuard{}, fmt.Errorf("%w: the plan job key is unavailable", errOtherClientUseIncomplete)
	}
	return otherClientUseGuard{final: final, activation: activation.Plan(), jobKey: request.JobKey}, nil
}

// checkRecorded checks the single ledger read recorded since readsBefore.
func (guard otherClientUseGuard) checkRecorded(ctx context.Context, session *recordingLedgerSession, readsBefore int, sourcePaths []string) error {
	snapshot, err := session.ledgerSince(readsBefore)
	if err != nil {
		return err
	}
	return guard.check(ctx, snapshot, sourcePaths)
}

func otherClientUseFinding(err error) (string, string, bool) {
	switch {
	case errors.Is(err, errOtherClientUseOverlap):
		return otherClientUseOverlapCode, errOtherClientUseOverlap.Error(), true
	case errors.Is(err, errOtherClientUseUnmapped):
		return otherClientUseUnmappedCode, errOtherClientUseUnmapped.Error(), true
	default:
		return otherClientUseIncompleteCode, "downloader jobs could not be completely enumerated and compared with the source names", false
	}
}

type otherUseSource struct {
	info os.FileInfo
}

type otherUseSources struct {
	files     []otherUseSource
	names     map[string]struct{}
	ancestors map[string]struct{}
	parents   []os.FileInfo
}

// check proves, for one complete job ledger, that no job other than the plan
// job has a content root (or, when no content root is claimed, a save path)
// equal to, above, or below a remaining source name, and that no such root is
// the same file object as a source or the same directory as a source parent.
func (guard otherClientUseGuard) check(ctx context.Context, snapshot downloader.LedgerSnapshot, sourcePaths []string) error {
	if guard.final == nil || guard.jobKey == "" {
		return fmt.Errorf("%w: guard authority is unavailable", errOtherClientUseIncomplete)
	}
	if !snapshot.Complete || snapshot.Driver == "" || snapshot.Driver != guard.activation.Driver || !snapshot.Capabilities.ContentPath {
		return fmt.Errorf("%w: the job ledger is incomplete or lacks content-path claims", errOtherClientUseIncomplete)
	}
	if err := downloader.ValidateLedgerDriverClaims(snapshot); err != nil {
		return fmt.Errorf("%w: job-ledger claims are invalid", errOtherClientUseIncomplete)
	}
	var planJob *downloader.Torrent
	seen := make(map[string]struct{}, len(snapshot.Jobs))
	others := make([]downloader.Torrent, 0, len(snapshot.Jobs))
	for index := range snapshot.Jobs {
		job := snapshot.Jobs[index]
		if job.Hash == "" {
			return fmt.Errorf("%w: a job has no key", errOtherClientUseIncomplete)
		}
		if _, duplicate := seen[job.Hash]; duplicate {
			return fmt.Errorf("%w: the job ledger repeats a job key", errOtherClientUseIncomplete)
		}
		seen[job.Hash] = struct{}{}
		if job.Hash == guard.jobKey {
			planJob = &snapshot.Jobs[index]
			continue
		}
		others = append(others, job)
	}
	if planJob == nil {
		return fmt.Errorf("%w: the plan job is absent from the job ledger", errOtherClientUseIncomplete)
	}
	if len(others) == 0 {
		return nil
	}
	sources, err := observeOtherUseSources(ctx, sourcePaths)
	if err != nil {
		return err
	}
	namespace, err := guard.recoverNamespace(*planJob)
	if err != nil {
		return err
	}
	for _, job := range others {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: %w", errOtherClientUseIncomplete, err)
		}
		claim := job.ContentPath
		if claim == "" {
			// Without a content root, the whole save path may be referenced.
			claim = job.SavePath
		}
		if claim == "" {
			return fmt.Errorf("%w: a job has neither a content root nor a save path", errOtherClientUseIncomplete)
		}
		if err := namespace.compareClaim(sources, claim); err != nil {
			return err
		}
	}
	return nil
}

func observeOtherUseSources(ctx context.Context, paths []string) (otherUseSources, error) {
	result := otherUseSources{names: map[string]struct{}{}, ancestors: map[string]struct{}{}}
	if len(paths) == 0 {
		return result, fmt.Errorf("%w: no source names were supplied", errOtherClientUseIncomplete)
	}
	parentSeen := map[string]struct{}{}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("%w: %w", errOtherClientUseIncomplete, err)
		}
		if path == "" || !filepath.IsAbs(path) {
			return result, fmt.Errorf("%w: a source name is not absolute", errOtherClientUseIncomplete)
		}
		clean := filepath.Clean(path)
		resolved, err := filepath.EvalSymlinks(clean)
		if err != nil {
			return result, fmt.Errorf("%w: a source name could not be resolved", errOtherClientUseIncomplete)
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			return result, fmt.Errorf("%w: a source name could not be observed", errOtherClientUseIncomplete)
		}
		result.files = append(result.files, otherUseSource{info: info})
		for _, name := range []string{clean, resolved} {
			result.names[name] = struct{}{}
			for current := name; ; {
				result.ancestors[current] = struct{}{}
				parent := filepath.Dir(current)
				if parent == current {
					break
				}
				current = parent
			}
		}
		for current := filepath.Dir(resolved); ; {
			if _, done := parentSeen[current]; !done {
				parentSeen[current] = struct{}{}
				parentInfo, err := os.Stat(current)
				if err != nil || !parentInfo.IsDir() {
					return result, fmt.Errorf("%w: a source parent directory could not be observed", errOtherClientUseIncomplete)
				}
				result.parents = append(result.parents, parentInfo)
			}
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
			current = parent
		}
	}
	return result, nil
}

// compare checks one confined host path claimed by another job. Lexical
// overlap is checked before and after symlink resolution; object identity
// catches hard links and bind-mount or symlink aliases of the job root.
func (sources otherUseSources) compare(hostPath string) error {
	if sources.lexicalOverlap(hostPath) {
		return errOtherClientUseOverlap
	}
	resolved, err := filepath.EvalSymlinks(hostPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: another job root could not be resolved on the host", errOtherClientUseIncomplete)
	}
	if sources.lexicalOverlap(resolved) {
		return errOtherClientUseOverlap
	}
	info, err := os.Stat(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: another job root could not be observed on the host", errOtherClientUseIncomplete)
	}
	switch {
	case info.Mode().IsRegular():
		for _, source := range sources.files {
			if os.SameFile(info, source.info) {
				return errOtherClientUseOverlap
			}
		}
	case info.IsDir():
		for _, parent := range sources.parents {
			if os.SameFile(info, parent) {
				return errOtherClientUseOverlap
			}
		}
	}
	return nil
}

func (sources otherUseSources) lexicalOverlap(path string) bool {
	path = filepath.Clean(path)
	if _, ok := sources.ancestors[path]; ok {
		return true
	}
	for current := path; ; {
		if _, ok := sources.names[current]; ok {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}

type otherUseNamespace struct {
	hostRoot     string
	clientVolume string
	clientParts  []string
	windows      bool
}

// recoverNamespace finds the unique host/client root pair whose projection of
// the exact final reproduces the reviewed path-mapping ID and save-path claim.
// Candidates are only suffix-preserving prefixes of the resolved target root
// and the plan job's verified save path, which is how the mapping was built.
func (guard otherClientUseGuard) recoverNamespace(planJob downloader.Torrent) (otherUseNamespace, error) {
	unmapped := fmt.Errorf("%w: the reviewed host/client path mapping could not be reproduced", errOtherClientUseUnmapped)
	var windows bool
	switch guard.activation.ClientPathSemantics {
	case "posix_exact":
	case "windows_exact":
		windows = true
	default:
		return otherUseNamespace{}, unmapped
	}
	targetRoot, ok := guard.final.ProcessTargetRoot()
	if !ok || targetRoot == "" {
		return otherUseNamespace{}, unmapped
	}
	absolute, err := filepath.Abs(targetRoot)
	if err != nil {
		return otherUseNamespace{}, unmapped
	}
	host, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return otherUseNamespace{}, fmt.Errorf("%w: the target root could not be resolved", errOtherClientUseIncomplete)
	}
	volume, parts, err := splitClientPath(planJob.SavePath, windows)
	if err != nil {
		return otherUseNamespace{}, unmapped
	}
	for suffix := 0; suffix <= len(parts); suffix++ {
		if suffix > 0 {
			parent := filepath.Dir(host)
			if parent == host || filepath.Base(host) != parts[len(parts)-suffix] {
				break
			}
			host = parent
		}
		prefix := parts[:len(parts)-suffix]
		for _, candidateVolume := range clientVolumeSpellings(volume, windows) {
			clientRoot := joinClientRoot(candidateVolume, prefix, windows)
			projection, projectErr := guard.final.ProjectClientPaths(host, clientRoot, windows)
			if projectErr == nil && projection.PathMappingID == guard.activation.PathMappingID &&
				projection.PathSemantics == guard.activation.ClientPathSemantics && projection.SavePathRef == guard.activation.ExpectedSavePathRef {
				return otherUseNamespace{hostRoot: host, clientVolume: volume, clientParts: append([]string(nil), prefix...), windows: windows}, nil
			}
		}
	}
	return otherUseNamespace{}, unmapped
}

// compareClaim compares one job's path claim with the sources. A Windows
// client can reach a case-sensitive host through a case-insensitive share, so
// its claim is first spelled the way that share resolves it on the host.
func (namespace otherUseNamespace) compareClaim(sources otherUseSources, claim string) error {
	hostPath, err := namespace.hostPath(claim)
	if err != nil {
		return err
	}
	if namespace.windows {
		if hostPath, err = foldExistingHostPath(namespace.hostRoot, hostPath); err != nil {
			return err
		}
	}
	return sources.compare(hostPath)
}

// foldExistingHostPath resolves each component of hostPath below root the way
// a case-insensitive share resolves a Windows client path on a case-sensitive
// host: an existing exact name wins, otherwise the single directory entry equal
// to the component under case folding is used. Without this, another job could
// hide a source behind a spelling that differs only in case. More than one
// folded match cannot be attributed and fails closed; no match leaves the
// remaining components as claimed, which names nothing on the host.
func foldExistingHostPath(root, hostPath string) (string, error) {
	relative, err := filepath.Rel(root, hostPath)
	if err != nil {
		return "", fmt.Errorf("%w: a job path could not be placed under the mapped host root", errOtherClientUseIncomplete)
	}
	if relative == "." {
		return hostPath, nil
	}
	parts := strings.Split(relative, string(filepath.Separator))
	current := root
	for index, part := range parts {
		exact := filepath.Join(current, part)
		_, err := os.Lstat(exact)
		if err == nil {
			current = exact
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: another job root could not be observed on the host", errOtherClientUseIncomplete)
		}
		entries, err := os.ReadDir(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: a directory above another job root could not be listed on the host", errOtherClientUseIncomplete)
		}
		match := ""
		for _, entry := range entries {
			if !strings.EqualFold(entry.Name(), part) {
				continue
			}
			if match != "" {
				return "", fmt.Errorf("%w: another job root matches more than one host name that differs only in case", errOtherClientUseIncomplete)
			}
			match = entry.Name()
		}
		if match == "" {
			return filepath.Join(append([]string{current}, parts[index:]...)...), nil
		}
		current = filepath.Join(current, match)
	}
	return current, nil
}

// hostPath translates one downloader path claim into the reviewed host
// namespace. A claim outside the mapped client root, or with a component that
// cannot name one host path component, fails closed as unmapped.
func (namespace otherUseNamespace) hostPath(claim string) (string, error) {
	volume, parts, err := splitClientPath(claim, namespace.windows)
	if err != nil {
		return "", fmt.Errorf("%w: a job path claim is malformed", errOtherClientUseUnmapped)
	}
	if !sameClientComponent(volume, namespace.clientVolume, namespace.windows) || len(parts) < len(namespace.clientParts) {
		return "", errOtherClientUseUnmapped
	}
	for index, part := range namespace.clientParts {
		if !sameClientComponent(parts[index], part, namespace.windows) {
			return "", errOtherClientUseUnmapped
		}
	}
	relative := parts[len(namespace.clientParts):]
	if len(relative) == 0 {
		return namespace.hostRoot, nil
	}
	raw := make([][]byte, len(relative))
	for index, part := range relative {
		raw[index] = []byte(part)
	}
	if err := storage.ValidateComponents(raw, storage.PathSemantics{Windows: namespace.windows || runtime.GOOS == "windows", CaseSensitive: true}); err != nil {
		return "", fmt.Errorf("%w: a job path component cannot be represented on the host", errOtherClientUseUnmapped)
	}
	joined := filepath.Join(append([]string{namespace.hostRoot}, relative...)...)
	if relativeHost, err := filepath.Rel(namespace.hostRoot, joined); err != nil || relativeHost == ".." ||
		strings.HasPrefix(relativeHost, ".."+string(filepath.Separator)) || filepath.IsAbs(relativeHost) {
		return "", fmt.Errorf("%w: a job path escapes the mapped host root", errOtherClientUseUnmapped)
	}
	return joined, nil
}

func splitClientPath(value string, windows bool) (string, []string, error) {
	canonical, err := reconcile.NormalizeClientPath(value, windows)
	if err != nil {
		return "", nil, err
	}
	if !windows {
		trimmed := strings.TrimPrefix(canonical, "/")
		if trimmed == "" {
			return "", []string{}, nil
		}
		return "", strings.Split(trimmed, "/"), nil
	}
	if strings.HasPrefix(canonical, `\\`) {
		segments := strings.Split(strings.TrimPrefix(canonical, `\\`), `\`)
		if len(segments) < 2 {
			return "", nil, fmt.Errorf("UNC client path is invalid")
		}
		return `\\` + segments[0] + `\` + segments[1], segments[2:], nil
	}
	if len(canonical) < 3 || canonical[1] != ':' || canonical[2] != '\\' {
		return "", nil, fmt.Errorf("Windows client path is invalid")
	}
	rest := canonical[3:]
	if rest == "" {
		return canonical[:2], []string{}, nil
	}
	return canonical[:2], strings.Split(rest, `\`), nil
}

func clientVolumeSpellings(volume string, windows bool) []string {
	if windows && len(volume) == 2 && volume[1] == ':' {
		lower := strings.ToLower(volume)
		if lower != volume {
			return []string{volume, lower}
		}
	}
	return []string{volume}
}

func joinClientRoot(volume string, parts []string, windows bool) string {
	if !windows {
		return "/" + strings.Join(parts, "/")
	}
	if len(parts) == 0 {
		if strings.HasPrefix(volume, `\\`) {
			return volume
		}
		return volume + `\`
	}
	return volume + `\` + strings.Join(parts, `\`)
}

func sameClientComponent(left, right string, windows bool) bool {
	if windows {
		return strings.EqualFold(left, right)
	}
	return left == right
}
