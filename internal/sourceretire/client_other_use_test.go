package sourceretire

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

// otherJob returns a non-plan downloader job whose content root is the client
// projection of hostPath under the fixture's /downloads mapping.
func otherJob(t *testing.T, fixture retireFixture, key, hostPath string) downloader.Torrent {
	t.Helper()
	relative, err := filepath.Rel(fixture.hostRoot, hostPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("host path is outside the fixture mapping: %v", err)
	}
	content := path.Join("/downloads", filepath.ToSlash(relative))
	digest := sha1.Sum([]byte(key))
	return downloader.Torrent{Hash: key, Name: key, InfoHashV1: hex.EncodeToString(digest[:]),
		IdentityStatus: downloader.IdentityStatusValid, IdentityEvidence: []string{"magnet_xt_btih_hex"}, IdentityIssues: []string{},
		SizeBytes: 1, State: "stalledUP", Progress: 1, SavePath: path.Dir(content), ContentPath: content}
}

// otherUseSession returns one plan-job ledger per entry; each entry lists the
// other jobs present in that ledger response.
func otherUseSession(fixture retireFixture, ledgers ...[]downloader.Torrent) *retireActivationSession {
	now := time.Now().UTC()
	snapshots := make([]downloader.LedgerSnapshot, len(ledgers))
	for index, others := range ledgers {
		job := fixture.currentJob
		snapshot := retireLedger(&job, now.Add(time.Duration(index)*time.Second))
		snapshot.Jobs = append(snapshot.Jobs, others...)
		snapshots[index] = snapshot
	}
	return &retireActivationSession{requests: 1, ledgers: snapshots}
}

func otherUseBuildOptions(fixture retireFixture, session downloader.LedgerSession) BuildOptions {
	return BuildOptions{Meta: fixture.meta, Discovery: &fixture.discovery, Final: fixture.final, Activation: fixture.activation,
		ClientUse: fixture.currentUse, ClientSession: session}
}

func TestBuildBlocksOtherClientJobsThatMayUseTheSource(t *testing.T) {
	fixture := makeRetireFixtureLayout(t, false, true)
	if err := ledgerJobValid(otherJob(t, fixture, "probe", fixture.sourcePath)); err != nil {
		t.Fatalf("test job shape is rejected by the ledger contract: %v", err)
	}
	otherDirectory := filepath.Join(fixture.hostRoot, "other")
	if err := os.Mkdir(otherDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	hardlink := filepath.Join(otherDirectory, "cross-seed-copy.bin")
	hardlinks := os.Link(fixture.sourcePath, hardlink) == nil
	symlinks := os.Symlink(fixture.sourceRoot, filepath.Join(fixture.hostRoot, "alias")) == nil
	unrelated := filepath.Join(otherDirectory, "unrelated.bin")
	if err := os.WriteFile(unrelated, []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	type testCase struct {
		name   string
		before []downloader.Torrent
		after  []downloader.Torrent
		code   string
		skip   bool
	}
	same := otherJob(t, fixture, "same-path", fixture.sourcePath)
	parent := otherJob(t, fixture, "parent-dir", fixture.sourceRoot)
	savePathOnly := otherJob(t, fixture, "save-only", filepath.Join(fixture.sourceRoot, "unknown-content"))
	savePathOnly.SavePath, savePathOnly.ContentPath = "/downloads/source", ""
	child := otherJob(t, fixture, "child", filepath.Join(fixture.sourcePath, "nested"))
	unmapped := otherJob(t, fixture, "unmapped", unrelated)
	unmapped.SavePath, unmapped.ContentPath = "/elsewhere", "/elsewhere/payload"
	cases := []testCase{
		{name: "same path", before: []downloader.Torrent{same}, after: []downloader.Torrent{same}, code: otherClientUseOverlapCode},
		{name: "parent directory", before: []downloader.Torrent{parent}, after: []downloader.Torrent{parent}, code: otherClientUseOverlapCode},
		{name: "save path without content root", before: []downloader.Torrent{savePathOnly}, after: []downloader.Torrent{savePathOnly}, code: otherClientUseOverlapCode},
		{name: "child path", before: []downloader.Torrent{child}, after: []downloader.Torrent{child}, code: otherClientUseOverlapCode},
		{name: "hardlinked copy", before: []downloader.Torrent{otherJob(t, fixture, "hardlink", hardlink)},
			after: []downloader.Torrent{otherJob(t, fixture, "hardlink", hardlink)}, code: otherClientUseOverlapCode, skip: !hardlinks},
		{name: "symlinked parent", before: []downloader.Torrent{otherJob(t, fixture, "alias", filepath.Join(fixture.hostRoot, "alias"))},
			after: []downloader.Torrent{otherJob(t, fixture, "alias", filepath.Join(fixture.hostRoot, "alias"))}, code: otherClientUseOverlapCode, skip: !symlinks},
		{name: "appears only in after ledger", before: nil, after: []downloader.Torrent{same}, code: otherClientUseOverlapCode},
		{name: "unmapped job", before: []downloader.Torrent{unmapped}, after: []downloader.Torrent{unmapped}, code: otherClientUseUnmappedCode},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if test.skip {
				t.Skip("filesystem links are unavailable")
			}
			session := otherUseSession(fixture, test.before, test.after)
			report, err := Build(context.Background(), otherUseBuildOptions(fixture, session))
			if err != nil || report.Outcome != OutcomeBlocked || !hasFinding(report.Blockers, test.code) || report.Plan.ID != "" ||
				report.WritesPerformed != 0 {
				t.Fatalf("report=%#v err=%v", report, err)
			}
			raw := fmt.Sprintf("%#v", report)
			if strings.Contains(raw, fixture.sourceRoot) || strings.Contains(raw, "/downloads/") {
				t.Fatal("other-use blocker disclosed a path")
			}
		})
	}

	t.Run("unrelated and absent jobs remain eligible", func(t *testing.T) {
		jobs := []downloader.Torrent{otherJob(t, fixture, "unrelated", unrelated),
			otherJob(t, fixture, "absent", filepath.Join(fixture.hostRoot, "absent", "payload")),
			otherJob(t, fixture, "through-file", filepath.Join(unrelated, "payload"))}
		session := otherUseSession(fixture, jobs, jobs)
		report, err := Build(context.Background(), otherUseBuildOptions(fixture, session))
		if err != nil || report.Outcome != OutcomeEligible || report.ClientUse.RequestsMade != 3 ||
			report.Plan.EvidenceBasis[len(report.Plan.EvidenceBasis)-2] != "same_session_complete_job_ledger_other_job_non_overlap" {
			t.Fatalf("report=%#v err=%v", report, err)
		}
	})

	t.Run("incomplete enumeration", func(t *testing.T) {
		session := otherUseSession(fixture, nil, []downloader.Torrent{otherJob(t, fixture, "unrelated", unrelated)})
		session.ledgers[1].Complete = false
		report, err := Build(context.Background(), otherUseBuildOptions(fixture, session))
		if err == nil || report.Outcome != OutcomeIncomplete || report.Plan.ID != "" {
			t.Fatalf("report=%#v err=%v", report, err)
		}
	})
}

func ledgerJobValid(job downloader.Torrent) error {
	snapshot := retireLedger(&job, time.Now().UTC())
	return downloader.ValidateLedgerDriverClaims(snapshot)
}

func TestOtherClientUseGuardFailsClosedOnIncompleteEnumeration(t *testing.T) {
	fixture := makeRetireFixtureLayout(t, false, true)
	session := otherUseSession(fixture, nil)
	planJob := fixture.currentJob
	recorder := recordLedgerSession(session)
	if recordLedgerSession(recorder) != recorder {
		t.Fatal("a recorded session was wrapped twice")
	}
	if _, err := recorder.ledgerSince(0); !errors.Is(err, errOtherClientUseIncomplete) {
		t.Fatalf("missing ledger read was accepted: %v", err)
	}
	if _, err := recorder.ReadLedger(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.ReadLedger(context.Background()); err == nil {
		t.Fatal("exhausted test session returned a ledger")
	}
	if _, err := recorder.ledgerSince(1); !errors.Is(err, errOtherClientUseIncomplete) || !errors.Is(err, ErrIncomplete) {
		t.Fatalf("ledger read failure was accepted: %v", err)
	}

	guard := otherClientUseGuard{final: fixture.final, activation: fixture.activation.Plan(), jobKey: planJob.Hash}
	other := otherJob(t, fixture, "other", filepath.Join(fixture.hostRoot, "absent"))
	valid := otherUseSession(fixture, []downloader.Torrent{other}).ledgers[0]
	if err := guard.check(context.Background(), valid, []string{fixture.sourcePath}); err != nil {
		t.Fatalf("valid complete ledger was rejected: %v", err)
	}
	mutations := map[string]func(*downloader.LedgerSnapshot){
		"truncated":         func(snapshot *downloader.LedgerSnapshot) { snapshot.Complete = false },
		"driver mismatch":   func(snapshot *downloader.LedgerSnapshot) { snapshot.Driver = "transmission" },
		"no content paths":  func(snapshot *downloader.LedgerSnapshot) { snapshot.Capabilities.ContentPath = false },
		"plan job missing":  func(snapshot *downloader.LedgerSnapshot) { snapshot.Jobs = snapshot.Jobs[1:] },
		"duplicate job key": func(snapshot *downloader.LedgerSnapshot) { snapshot.Jobs[1].Hash = snapshot.Jobs[0].Hash },
		"no path claims": func(snapshot *downloader.LedgerSnapshot) {
			snapshot.Jobs[1].SavePath, snapshot.Jobs[1].ContentPath = "", ""
		},
	}
	for name, mutate := range mutations {
		snapshot := valid
		snapshot.Jobs = append([]downloader.Torrent(nil), valid.Jobs...)
		mutate(&snapshot)
		if err := guard.check(context.Background(), snapshot, []string{fixture.sourcePath}); !errors.Is(err, errOtherClientUseIncomplete) {
			t.Fatalf("%s: incomplete enumeration was accepted: %v", name, err)
		}
	}
	if err := guard.check(context.Background(), valid, []string{filepath.Join(fixture.sourceRoot, "missing.bin")}); !errors.Is(err, errOtherClientUseIncomplete) {
		t.Fatalf("an unobservable source was accepted: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := guard.check(cancelled, valid, []string{fixture.sourcePath}); !errors.Is(err, errOtherClientUseIncomplete) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled comparison was accepted: %v", err)
	}
	moved := guard
	moved.activation.PathMappingID = "sha256:" + strings.Repeat("0", 64)
	if err := moved.check(context.Background(), valid, []string{fixture.sourcePath}); !errors.Is(err, errOtherClientUseUnmapped) {
		t.Fatalf("an unreproducible path mapping was accepted: %v", err)
	}
}

func TestOtherClientUseGuardResolvesWindowsClaimsLikeACaseInsensitiveShare(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "Show", "Episode.mkv")
	for _, dir := range []string{"Show", "Other"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(source, []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, filepath.Join(root, "Other", "Linked.mkv")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Other", "notes.txt"), []byte("unrelated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "Dup"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A case-insensitive host cannot hold both names; there the exact lookup
	// already succeeds.
	err = os.Mkdir(filepath.Join(root, "DUP"), 0o755)
	bothCases := err == nil
	if err != nil && !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	sources, err := observeOtherUseSources(context.Background(), []string{source})
	if err != nil {
		t.Fatal(err)
	}
	namespace := otherUseNamespace{hostRoot: root, clientVolume: "M:", clientParts: []string{"Movies"}, windows: true, listings: map[string][]string{}}
	// Samba also matches dotted İ and dotless ı with i, which EqualFold does not.
	for _, claim := range []string{`M:\movies\show`, `M:\MOVIES\SHOW\episode.MKV`, `M:\Movies\other\linked.mkv`,
		`M:\Movies\Show\EPİSODE.mkv`, `M:\Movies\show\epısode.mkv`, `M:\Movies\Show\Episode.mkv\extra`} {
		if err := namespace.compareClaim(sources, claim); !errors.Is(err, errOtherClientUseOverlap) {
			t.Fatalf("%s: a claim naming the source in another case was accepted: %v", claim, err)
		}
	}
	// A path below a non-directory names nothing; it must not block retirement.
	for _, claim := range []string{`M:\Movies\show\absent.mkv`, `M:\Movies\Other\notes.txt\payload`} {
		if err := namespace.compareClaim(sources, claim); err != nil {
			t.Fatalf("%s: a claim naming nothing on the host was blocked: %v", claim, err)
		}
	}
	if bothCases {
		if err := namespace.compareClaim(sources, `M:\Movies\dup\file.mkv`); !errors.Is(err, errOtherClientUseIncomplete) {
			t.Fatalf("a claim matching two host names by case was accepted: %v", err)
		}
	}
}

func TestRunRechecksOtherClientJobsImmediatelyBeforeSourceRemoval(t *testing.T) {
	fixture := makeRetireFixtureLayout(t, false, true)
	preview, err := Build(context.Background(), otherUseBuildOptions(fixture, otherUseSession(fixture, nil, nil)))
	if err != nil || preview.Outcome != OutcomeEligible {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	runOptions := func(session downloader.LedgerSession) RunOptions {
		return RunOptions{Review: otherUseBuildOptions(fixture, session), ExpectedPlanID: preview.Plan.ID,
			SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultExecutionLimits()}
	}
	seeding := otherJob(t, fixture, "late-cross-seed", fixture.sourcePath)
	session := otherUseSession(fixture, nil, nil, []downloader.Torrent{seeding})
	report, err := Run(context.Background(), runOptions(session))
	if err != nil || report.Outcome != ExecutionOutcomeBlocked || !hasFinding(report.Blockers, otherClientUseOverlapCode) ||
		report.DeletionPerformed || report.Writes.NamesRemoved != 0 || !report.Operation.Resumable || session.RequestsMade() != 4 {
		t.Fatalf("run=%#v requests=%d err=%v", report, session.RequestsMade(), err)
	}
	if _, err := os.Stat(fixture.sourcePath); err != nil {
		t.Fatalf("source used by another job was removed: %v", err)
	}
	operation, err := ParseOperationID(report.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	resumeOptions := func(session downloader.LedgerSession) ResumeOptions {
		return ResumeOptions{Meta: fixture.meta, Final: fixture.final, Activation: fixture.activation, ClientUse: fixture.currentUse,
			ClientSession: session, TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: preview.Plan.ID,
			SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultExecutionLimits()}
	}
	// A failed pre-removal enumeration leaves the name and the operation resumable.
	failing := otherUseSession(fixture, nil)
	report, err = Resume(context.Background(), resumeOptions(failing))
	if err == nil || errors.Is(err, ErrExecutionIntegrity) || report.Outcome != ExecutionOutcomePartial || report.DeletionPerformed ||
		!report.Operation.Resumable || !hasFinding(report.Blockers, otherClientUseIncompleteCode) {
		t.Fatalf("resume with failed enumeration=%#v err=%v", report, err)
	}
	if _, err := os.Stat(fixture.sourcePath); err != nil {
		t.Fatalf("source was removed without a complete enumeration: %v", err)
	}
	// Once the other job is gone, resume completes with one extra ledger read.
	clean := otherUseSession(fixture, nil, nil, nil)
	report, err = Resume(context.Background(), resumeOptions(clean))
	if err != nil || report.Outcome != ExecutionOutcomeRetired || report.Writes.NamesRemoved != 1 || clean.RequestsMade() != 4 {
		t.Fatalf("resume=%#v requests=%d err=%v", report, clean.RequestsMade(), err)
	}
	if _, err := os.Lstat(fixture.sourcePath); !os.IsNotExist(err) {
		t.Fatalf("source remains after resumed retirement: %v", err)
	}
}
