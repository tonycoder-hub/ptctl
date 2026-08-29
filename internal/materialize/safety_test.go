package materialize

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/seed"
)

func TestRunBlocksTargetRootSwapAfterReviewedPlanWithoutWritingReplacement(t *testing.T) {
	ctx := context.Background()
	meta, discovery, _, targetRoot := singleMaterializeFixture(t, ctx, "final.bin", []byte("root binding"))
	parent := filepath.Dir(targetRoot)
	detached := filepath.Join(parent, filepath.Base(targetRoot)+"-reviewed")
	targetBindHook = func() error {
		if err := os.Rename(targetRoot, detached); err != nil {
			return err
		}
		return os.Mkdir(targetRoot, 0o700)
	}
	t.Cleanup(func() { targetBindHook = nil })
	report, err := Run(ctx, RunOptions{
		Meta: meta, Discovery: &discovery, TargetRoot: targetRoot,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	targetBindHook = nil
	if err == nil || report.Outcome != OutcomeBlocked || report.WritesPerformed != 0 {
		t.Fatalf("reviewed root replacement was not a zero-write block: %#v %v", report, err)
	}
	entries, readErr := os.ReadDir(targetRoot)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("replacement root was modified: %#v %v", entries, readErr)
	}
}

func TestStageIsReverifiedAfterStageVerifiedAndPublishIntentHooks(t *testing.T) {
	for _, phase := range []Phase{PhaseStageVerified, PhasePublishIntent} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			content := []byte("stage mutation")
			meta, discovery, _, targetRoot := singleMaterializeFixture(t, ctx, "final.bin", content)
			transitionHook = func(current Phase) error {
				if current != phase {
					return nil
				}
				operation := onlyOperationDirectory(t, targetRoot)
				return os.WriteFile(filepath.Join(operation, stageDirectoryName, "final.bin"), bytes.Repeat([]byte{'x'}, len(content)), 0o600)
			}
			t.Cleanup(func() { transitionHook = nil })
			report, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
			transitionHook = nil
			if err == nil || !errors.Is(err, ErrIntegrity) || report.Operation.PhaseAfter != string(phase) {
				t.Fatalf("stage mutation after %s was accepted: %#v %v", phase, report, err)
			}
			if _, statErr := os.Lstat(filepath.Join(targetRoot, "final.bin")); !os.IsNotExist(statErr) {
				t.Fatal("a changed stage became publicly visible")
			}
		})
	}
}

func TestFinalIsReverifiedAfterFinalVerifiedHook(t *testing.T) {
	ctx := context.Background()
	content := []byte("final mutation")
	meta, discovery, _, targetRoot := singleMaterializeFixture(t, ctx, "final.bin", content)
	transitionHook = func(phase Phase) error {
		if phase != PhaseFinalVerified {
			return nil
		}
		return os.WriteFile(filepath.Join(targetRoot, "final.bin"), bytes.Repeat([]byte{'z'}, len(content)), 0o600)
	}
	t.Cleanup(func() { transitionHook = nil })
	report, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
	transitionHook = nil
	if err == nil || !errors.Is(err, ErrIntegrity) || report.Operation.PhaseAfter != string(PhaseFinalVerified) ||
		report.Outcome != OutcomePublishedIntegrityFailed {
		t.Fatalf("final mutation was committed: %#v %v", report, err)
	}
}

func TestExactNamespaceRejectsUnexpectedStageAndFinalObjects(t *testing.T) {
	for _, phase := range []Phase{PhaseStageVerified, PhaseFinalVerified} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			meta, discovery, targetRoot := multiMaterializeFixture(t, ctx)
			transitionHook = func(current Phase) error {
				if current != phase {
					return nil
				}
				root := filepath.Join(targetRoot, "bundle")
				if phase == PhaseStageVerified {
					root = filepath.Join(onlyOperationDirectory(t, targetRoot), stageDirectoryName, "bundle")
				}
				return os.WriteFile(filepath.Join(root, "unexpected"), []byte("not in torrent"), 0o600)
			}
			t.Cleanup(func() { transitionHook = nil })
			report, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
			transitionHook = nil
			if err == nil || !errors.Is(err, ErrIntegrity) {
				t.Fatalf("unexpected namespace object was accepted: %#v %v", report, err)
			}
			if phase == PhaseStageVerified {
				if _, statErr := os.Lstat(filepath.Join(targetRoot, "bundle")); !os.IsNotExist(statErr) {
					t.Fatal("stage with an extra object was published")
				}
			} else if report.Operation.PhaseAfter == string(PhaseCommitted) {
				t.Fatal("final namespace with an extra object was committed")
			}
		})
	}
}

func TestCancellationDuringStageReverificationIsInterruptedNotIntegrity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	meta, discovery, _, targetRoot := singleMaterializeFixture(t, ctx, "final.bin", []byte("cancel stage"))
	transitionHook = func(phase Phase) error {
		if phase == PhaseStageVerified {
			cancel()
		}
		return nil
	}
	t.Cleanup(func() { transitionHook = nil })
	report, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
	transitionHook = nil
	if !errors.Is(err, context.Canceled) || report.Outcome != OutcomeInterrupted || !report.Operation.Resumable {
		t.Fatalf("cancellation was misclassified: %#v %v", report, err)
	}
	for _, issue := range report.Issues {
		if strings.Contains(issue.Code, "verification_failed") {
			t.Fatalf("cancellation emitted an integrity finding: %#v", report.Issues)
		}
	}
}

func TestPostCommitCancellationCannotUndoTerminalReport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	meta, discovery, _, targetRoot := singleMaterializeFixture(t, ctx, "final.bin", []byte("commit survives"))
	transitionHook = func(phase Phase) error {
		if phase == PhaseCommitted {
			cancel()
			return context.Canceled
		}
		return nil
	}
	t.Cleanup(func() { transitionHook = nil })
	report, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
	transitionHook = nil
	if !errors.Is(err, context.Canceled) || report.Outcome != OutcomeAlreadyCommitted || report.Operation.Resumable ||
		report.Operation.PhaseAfter != string(PhaseCommitted) || report.Target.Publication != "committed" {
		t.Fatalf("durable commit was denied by a post-commit cancellation: %#v %v", report, err)
	}
}

func TestStatusIsHistoricalAndCommittedResumeReverifiesCurrentBytes(t *testing.T) {
	ctx := context.Background()
	content := []byte("historical commit")
	meta, discovery, _, targetRoot := singleMaterializeFixture(t, ctx, "final.bin", content)
	run, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
	if err != nil {
		t.Fatal(err)
	}
	operationID, _ := ParseOperationID(run.Operation.ID)
	status, err := Status(ctx, ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if status.Plan.Matches || status.Plan.ExpectedID != "" || status.Target.FinalContentVerified ||
		status.Target.Publication != "historical_commit_only" || status.Target.StabilityAssurance != "historical_journal_only" {
		t.Fatalf("status promoted historical journal data into live proof: %#v", status)
	}
	if err := os.WriteFile(filepath.Join(targetRoot, "final.bin"), bytes.Repeat([]byte{'q'}, len(content)), 0o600); err != nil {
		t.Fatal(err)
	}
	resumed, err := Resume(ctx, ResumeOptions{
		Meta: meta, TargetRoot: targetRoot, OperationID: operationID,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	if err == nil || !errors.Is(err, ErrIntegrity) || resumed.Outcome != OutcomePublishedIntegrityFailed || resumed.Target.FinalContentVerified {
		t.Fatalf("committed resume replayed history without live verification: %#v %v", resumed, err)
	}
}

func TestStatusNeverRecreatesMissingOperationLock(t *testing.T) {
	_, _, _, targetRoot, operationID := interruptedMaterialize(t, PhaseFileStaged)
	operation, _ := OperationDirectoryName(operationID)
	lockPath := filepath.Join(targetRoot, operation, operationLockEntryName)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	report, err := Status(context.Background(), ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: DefaultLimits()})
	if err == nil || report.Outcome != OutcomeIntegrityFailed || report.WritesPerformed != 0 {
		t.Fatalf("missing lock was repaired or accepted: %#v %v", report, err)
	}
	if _, statErr := os.Lstat(lockPath); !os.IsNotExist(statErr) {
		t.Fatal("read-only status recreated the missing lock")
	}
}

func TestResumeRejectsReplacedStageContainerBeforeFurtherWrites(t *testing.T) {
	ctx := context.Background()
	meta, discovery, searchRoot, targetRoot, operationID := interruptedMaterialize(t, PhaseStageCreated)
	operation, _ := OperationDirectoryName(operationID)
	stage := filepath.Join(targetRoot, operation, stageDirectoryName)
	detached := filepath.Join(targetRoot, "detached-stage")
	if err := os.Rename(stage, detached); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	fresh := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
	report, err := Resume(ctx, ResumeOptions{
		Meta: meta, Discovery: &fresh, TargetRoot: targetRoot, OperationID: operationID,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	if err == nil || !errors.Is(err, ErrIntegrity) || report.WritesPerformed != 0 {
		t.Fatalf("replaced stage container received further writes: %#v %v", report, err)
	}
}

func TestResumeRejectsHardlinkedStageFileForCopyStrategy(t *testing.T) {
	ctx := context.Background()
	meta, discovery, searchRoot, targetRoot, operationID := interruptedMaterialize(t, PhaseStageCreated)
	operation, _ := OperationDirectoryName(operationID)
	stageFile := filepath.Join(targetRoot, operation, stageDirectoryName, "final.bin")
	sourceFile := filepath.Join(searchRoot, "source")
	if err := os.Link(sourceFile, stageFile); err != nil {
		t.Skipf("hardlinks are unavailable on the test filesystem: %v", err)
	}
	fresh := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
	report, err := Resume(ctx, ResumeOptions{
		Meta: meta, Discovery: &fresh, TargetRoot: targetRoot, OperationID: operationID,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	if err == nil || !errors.Is(err, ErrIntegrity) || report.Operation.PhaseAfter != string(PhaseStageCreated) {
		t.Fatalf("hardlinked stage object was accepted as copy-only: %#v %v", report, err)
	}
	if _, statErr := os.Lstat(filepath.Join(targetRoot, "final.bin")); !os.IsNotExist(statErr) {
		t.Fatal("hardlinked stage object was published")
	}
}

func TestRecoveredManualRenameDoesNotClaimHistoricalNoClobber(t *testing.T) {
	ctx := context.Background()
	meta, discovery, _, targetRoot, operationID := interruptedMaterialize(t, PhasePublishIntent)
	operation, _ := OperationDirectoryName(operationID)
	stageTop := filepath.Join(targetRoot, operation, stageDirectoryName, "final.bin")
	if err := os.Rename(stageTop, filepath.Join(targetRoot, "final.bin")); err != nil {
		t.Fatal(err)
	}
	report, err := Resume(ctx, ResumeOptions{
		Meta: meta, TargetRoot: targetRoot, OperationID: operationID,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	if err != nil || report.Outcome != OutcomeMaterializedVerified || report.Target.NoClobber {
		t.Fatalf("manual recovery was promoted into observed no-clobber publication: %#v %v", report, err)
	}
}

func TestPublishIntentRejectsFinalAndUnsafeStageAsPublishedIntegrityConflict(t *testing.T) {
	ctx := context.Background()
	meta, discovery, _, targetRoot, operationID := interruptedMaterialize(t, PhasePublishIntent)
	operation, _ := OperationDirectoryName(operationID)
	stageTop := filepath.Join(targetRoot, operation, stageDirectoryName, "final.bin")
	if err := os.Link(stageTop, filepath.Join(targetRoot, "final.bin")); err != nil {
		t.Skipf("hardlinks are unavailable on the test filesystem: %v", err)
	}
	report, err := Resume(ctx, ResumeOptions{
		Meta: meta, TargetRoot: targetRoot, OperationID: operationID,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	if err == nil || !errors.Is(err, ErrIntegrity) || report.Outcome != OutcomePublishedIntegrityFailed ||
		report.Target.Publication != "publication_conflict_observed" || report.Operation.Resumable {
		t.Fatalf("unsafe stage beside an observed final was reported as an ordinary interruption: %#v %v", report, err)
	}
}

func TestPublishIntentObservationSafetyErrorsAreIntegrityConflicts(t *testing.T) {
	for _, err := range []error{fsbind.ErrUnsafeObject, fsbind.ErrBindingChanged, fsbind.ErrCrossFilesystem} {
		report := newReport("", "", DefaultLimits())
		classified := classifyPublishIntentObservationError(&report, err)
		if !errors.Is(classified, ErrIntegrity) || report.Target.Publication != "publication_conflict_observed" {
			t.Fatalf("publish-intent observation error was not classified as a visible integrity conflict: %#v %v", report, classified)
		}
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, errors.New("ordinary inspection failure")} {
		report := newReport("", "", DefaultLimits())
		classified := classifyPublishIntentObservationError(&report, err)
		if !errors.Is(classified, err) || report.Target.Publication == "publication_conflict_observed" {
			t.Fatalf("non-integrity observation error was overclassified: %#v %v", report, classified)
		}
	}
}

func TestJournalScratchFailureIsReportedAsAWrite(t *testing.T) {
	ctx := context.Background()
	meta, discovery, _, targetRoot := singleMaterializeFixture(t, ctx, "final.bin", []byte("scratch receipt"))
	injected := errors.New("fail after scratch creation")
	journalScratchCreatedHook = func(purpose string) error {
		if purpose == "intent" {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { journalScratchCreatedHook = nil })
	report, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
	journalScratchCreatedHook = nil
	if !errors.Is(err, injected) || report.Writes.ScratchFilesCreated != 1 || report.WritesPerformed == 0 {
		t.Fatalf("retained journal scratch was hidden from the receipt: %#v %v", report, err)
	}
}

func TestJournalScratchReplacementCannotAdvanceReplay(t *testing.T) {
	ctx := context.Background()
	meta, discovery, _, targetRoot := singleMaterializeFixture(t, ctx, "final.bin", []byte("replace pending"))
	replaced := false
	journalScratchMutationHook = func(ctx context.Context, journal *journal, purpose string, pending fsbind.Path, raw []byte) error {
		if purpose != "intent" || replaced {
			return nil
		}
		replaced = true
		backup, _ := fsbind.PathFromComponents([]string{scratchDirectoryName, "intent-" + strings.Repeat("f", 32) + ".pending"})
		publication, err := journal.subtree.CommitRegularNoReplace(ctx, pending, backup)
		if err != nil || !publication.Published {
			return fmt.Errorf("detach scratch: %w", err)
		}
		file, err := journal.subtree.CreateRegular(ctx, pending)
		if err != nil {
			return err
		}
		if _, err := writeFullContext(ctx, file, raw); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
		return file.Close()
	}
	t.Cleanup(func() { journalScratchMutationHook = nil })
	report, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
	journalScratchMutationHook = nil
	if err == nil || !errors.Is(err, ErrCorruptJournal) || report.Outcome != OutcomeIntegrityFailed || report.Operation.Resumable ||
		report.Target.Publication != "not_attempted" || report.Writes.ScratchFilesCreated != 1 || report.Writes.JournalEvents != 0 {
		t.Fatalf("replaced journal scratch advanced the replay state: %#v %v", report, err)
	}
}

func TestCanceledControlAndResumeReadsAreNotIntegrityFailures(t *testing.T) {
	meta, discovery, _, targetRoot, operationID := interruptedMaterialize(t, PhaseStageVerified)
	tests := []struct {
		name string
		call func(context.Context) (Report, error)
	}{
		{name: "status", call: func(ctx context.Context) (Report, error) {
			return Status(ctx, ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: DefaultLimits()})
		}},
		{name: "resume", call: func(ctx context.Context) (Report, error) {
			return Resume(ctx, ResumeOptions{
				Meta: meta, TargetRoot: targetRoot, OperationID: operationID,
				ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
			})
		}},
		{name: "abandon", call: func(ctx context.Context) (Report, error) {
			return Abandon(ctx, ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: DefaultLimits()})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			report, err := test.call(ctx)
			if !errors.Is(err, context.Canceled) || report.Outcome != OutcomeInterrupted || report.Outcome == OutcomeIntegrityFailed ||
				report.Operation.ID != operationID.String() || report.Operation.Status != "inspection_incomplete" ||
				report.Operation.PhaseAfter != "unknown" {
				t.Fatalf("pre-canceled %s was misclassified: %#v %v", test.name, report, err)
			}
		})
	}
}

func TestFinalPublicationAmbiguityIsExplicit(t *testing.T) {
	tests := []struct {
		name        string
		publication fsbind.Publication
		err         error
		status      string
	}{
		{name: "nil_error", publication: fsbind.Publication{Attempted: true, Durability: fsbind.DurabilityNotPublished}, status: "publication_ambiguous"},
		{name: "explicit_error", publication: fsbind.Publication{Attempted: true, Durability: fsbind.DurabilityNotPublished}, err: fsbind.ErrPublicationAmbiguous, status: "publication_ambiguous"},
		{name: "published_explicit_error", publication: fsbind.Publication{Attempted: true, Published: true, Durability: fsbind.DurabilityUnconfirmed}, err: fsbind.ErrPublicationAmbiguous, status: "published_ambiguous"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			meta, discovery, _, targetRoot := singleMaterializeFixture(t, ctx, "final.bin", []byte("ambiguous publish"))
			publicationTestHook = func(kind string) (fsbind.Publication, error, bool) {
				if kind != "final" {
					return fsbind.Publication{}, nil, false
				}
				return test.publication, test.err, true
			}
			t.Cleanup(func() { publicationTestHook = nil })
			report, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
			publicationTestHook = nil
			if err == nil || !errors.Is(err, fsbind.ErrPublicationAmbiguous) || report.Outcome != OutcomePublicationAmbiguous ||
				report.Target.Publication != test.status || !report.WritesUncertain ||
				report.Writes.FinalPublicationAttempts != 1 || report.Writes.AmbiguousPublications != 1 {
				t.Fatalf("ambiguous final publication was hidden: %#v %v", report, err)
			}
		})
	}
}

func TestPublishedPrivateAmbiguityIsCountedWithoutClaimingFinalVisibility(t *testing.T) {
	for _, kind := range []string{"journal", "stage"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			meta, discovery, _, targetRoot := singleMaterializeFixture(t, ctx, "final.bin", []byte("private published ambiguity"))
			publicationTestHook = func(current string) (fsbind.Publication, error, bool) {
				if current != kind {
					return fsbind.Publication{}, nil, false
				}
				return fsbind.Publication{
					Attempted: true, Published: true, Durability: fsbind.DurabilityUnconfirmed,
				}, fsbind.ErrPublicationAmbiguous, true
			}
			t.Cleanup(func() { publicationTestHook = nil })
			report, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
			publicationTestHook = nil
			if err == nil || !errors.Is(err, fsbind.ErrPublicationAmbiguous) || !report.WritesUncertain ||
				report.Writes.AmbiguousPublications != 1 || report.Target.Publication != "not_attempted" ||
				report.Outcome == OutcomePublishedDurabilityUnconfirmed || report.Outcome == OutcomePublicationAmbiguous {
				t.Fatalf("published private ambiguity was hidden or promoted to final: %#v %v", report, err)
			}
		})
	}
}

func TestCreationPhaseAdvancesOnlyAfterJournalReplayIsVerified(t *testing.T) {
	ctx := context.Background()
	meta, discovery, _, targetRoot := singleMaterializeFixture(t, ctx, "final.bin", []byte("creation phase"))
	journalCalls := 0
	publicationTestHook = func(kind string) (fsbind.Publication, error, bool) {
		if kind != "journal" {
			return fsbind.Publication{}, nil, false
		}
		journalCalls++
		if journalCalls != 2 {
			return fsbind.Publication{}, nil, false
		}
		return fsbind.Publication{
			Attempted: true, Published: true, Durability: fsbind.DurabilityUnconfirmed,
		}, fsbind.ErrPublicationAmbiguous, true
	}
	t.Cleanup(func() { publicationTestHook = nil })
	report, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
	publicationTestHook = nil
	if err == nil || !errors.Is(err, fsbind.ErrPublicationAmbiguous) || report.Operation.PhaseAfter == string(PhaseJournaled) ||
		report.Operation.PhaseAfter == string(PhaseStageCreated) || report.Operation.Resumable ||
		report.Writes.JournalEvents != 1 || !report.WritesUncertain {
		t.Fatalf("physical event publication was promoted into replayed phase authority: %#v %v", report, err)
	}
}

func TestPrivatePublicationAmbiguityNeverClaimsFinalPublication(t *testing.T) {
	ctx := context.Background()
	meta, discovery, _, targetRoot := singleMaterializeFixture(t, ctx, "final.bin", []byte("private ambiguity"))
	publicationTestHook = func(kind string) (fsbind.Publication, error, bool) {
		if kind != "journal" {
			return fsbind.Publication{}, nil, false
		}
		return fsbind.Publication{Attempted: true, Durability: fsbind.DurabilityNotPublished}, nil, true
	}
	t.Cleanup(func() { publicationTestHook = nil })
	report, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
	publicationTestHook = nil
	if err == nil || !errors.Is(err, fsbind.ErrPublicationAmbiguous) || report.Target.Publication != "not_attempted" ||
		!report.WritesUncertain || report.Writes.JournalPublicationAttempts != 1 || report.Writes.AmbiguousPublications != 1 ||
		report.Outcome == OutcomePublicationAmbiguous || report.Outcome == OutcomePublishedDurabilityUnconfirmed {
		t.Fatalf("private ambiguity was presented as a final publication: %#v %v", report, err)
	}
}

func TestVisibleFinalBindingFailureIsPublishedIntegrityFailure(t *testing.T) {
	ctx := context.Background()
	meta, discovery, _, targetRoot := singleMaterializeFixture(t, ctx, "final.bin", []byte("visible binding failure"))
	publicationTestHook = func(kind string) (fsbind.Publication, error, bool) {
		if kind != "final" {
			return fsbind.Publication{}, nil, false
		}
		return fsbind.Publication{
			Attempted: true, Published: true, Durability: fsbind.DurabilityConfirmed,
		}, fsbind.ErrBindingChanged, true
	}
	t.Cleanup(func() { publicationTestHook = nil })
	report, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
	publicationTestHook = nil
	if err == nil || !errors.Is(err, ErrIntegrity) || report.Outcome != OutcomePublishedIntegrityFailed ||
		report.Operation.Resumable || report.WritesPerformed == 0 || report.Writes.FinalLayoutPublications != 1 ||
		report.Target.Publication != "published" {
		t.Fatalf("visible final binding failure remained resumable: %#v %v", report, err)
	}
}

func TestFinalConflictBeforePublishIntentIsBlockedAndAbandonable(t *testing.T) {
	ctx := context.Background()
	meta, discovery, _, targetRoot := singleMaterializeFixture(t, ctx, "final.bin", []byte("namespace conflict"))
	transitionHook = func(phase Phase) error {
		if phase == PhaseStageVerified {
			return os.WriteFile(filepath.Join(targetRoot, "final.bin"), []byte("occupied"), 0o600)
		}
		return nil
	}
	t.Cleanup(func() { transitionHook = nil })
	report, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
	transitionHook = nil
	if err == nil || !errors.Is(err, fsbind.ErrAlreadyExists) || report.Outcome != OutcomeBlocked ||
		!report.Operation.Resumable || report.Operation.PhaseAfter != string(PhaseStageVerified) ||
		report.Target.Publication != "not_attempted" {
		t.Fatalf("pre-intent target conflict was misclassified as journal corruption: %#v %v", report, err)
	}
	operationID, parseErr := ParseOperationID(report.Operation.ID)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	abandoned, abandonErr := Abandon(ctx, ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: DefaultLimits()})
	if abandonErr != nil || abandoned.Outcome != OutcomeAbandoned {
		t.Fatalf("healthy pre-intent operation could not be abandoned: %#v %v", abandoned, abandonErr)
	}
}

func TestCommittedResumeReportsSourceAsNotRequired(t *testing.T) {
	ctx := context.Background()
	meta, discovery, _, targetRoot := singleMaterializeFixture(t, ctx, "final.bin", []byte("source no longer required"))
	run, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
	if err != nil {
		t.Fatal(err)
	}
	operationID, _ := ParseOperationID(run.Operation.ID)
	report, err := Resume(ctx, ResumeOptions{
		Meta: meta, TargetRoot: targetRoot, OperationID: operationID,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	if err != nil || report.Outcome != OutcomeAlreadyCommitted || report.Source.Mode != "not_required_after_staging" ||
		report.Source.Outcome != "not_requested" || report.Source.ContentVerified {
		t.Fatalf("committed resume reported an unavailable live source: %#v %v", report, err)
	}
}

func TestResumeFromPublishedPhasesNeverReportsMissingFinalAsNotAttempted(t *testing.T) {
	for _, phase := range []Phase{PhasePublished, PhaseFinalVerified} {
		t.Run(string(phase), func(t *testing.T) {
			meta, discovery, _, targetRoot, operationID := interruptedMaterialize(t, phase)
			if err := os.Remove(filepath.Join(targetRoot, "final.bin")); err != nil {
				t.Fatal(err)
			}
			report, err := Resume(context.Background(), ResumeOptions{
				Meta: meta, TargetRoot: targetRoot, OperationID: operationID,
				ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
			})
			if err == nil || !errors.Is(err, ErrIntegrity) || report.Outcome != OutcomePublishedIntegrityFailed ||
				report.Target.Publication != "historical_publication_unverified" || report.Operation.Resumable {
				t.Fatalf("missing historical final was reported as not attempted: %#v %v", report, err)
			}
		})
	}
}

func TestPublishedRootBindingFailureIsIntegrityNotDurability(t *testing.T) {
	tests := []struct {
		name    string
		phase   Phase
		recover bool
	}{
		{name: "historical_published", phase: PhasePublished},
		{name: "publish_intent_recovery", phase: PhasePublishIntent, recover: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			meta, discovery, _, targetRoot, operationID := interruptedMaterialize(t, test.phase)
			if test.recover {
				operation, _ := OperationDirectoryName(operationID)
				stageTop := filepath.Join(targetRoot, operation, stageDirectoryName, "final.bin")
				if err := os.Rename(stageTop, filepath.Join(targetRoot, "final.bin")); err != nil {
					t.Fatal(err)
				}
			}
			rootSyncTestHook = func() error { return fsbind.ErrBindingChanged }
			t.Cleanup(func() { rootSyncTestHook = nil })
			report, err := Resume(context.Background(), ResumeOptions{
				Meta: meta, TargetRoot: targetRoot, OperationID: operationID,
				ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
			})
			rootSyncTestHook = nil
			if err == nil || !errors.Is(err, ErrIntegrity) || report.Outcome != OutcomePublishedIntegrityFailed ||
				report.Operation.Resumable || report.Target.Publication == "published_durability_unconfirmed" {
				t.Fatalf("published binding failure was downgraded to durability: %#v %v", report, err)
			}
		})
	}
}

func TestPredictablePolicyFailuresHappenBeforeAnyTargetWrite(t *testing.T) {
	tests := []struct {
		name   string
		meta   func(*testing.T) *metafile.MetaInfo
		limits func() Limits
	}{
		{
			name: "reserved control prefix",
			meta: func(t *testing.T) *metafile.MetaInfo {
				return materializeSingleV1Meta(t, operationDirectoryPrefix+strings.Repeat("a", 64), []byte("x"))
			},
			limits: DefaultLimits,
		},
		{
			name: "reserved malformed control prefix",
			meta: func(t *testing.T) *metafile.MetaInfo {
				return materializeSingleV1Meta(t, operationDirectoryPrefix+"garbage", []byte("x"))
			},
			limits: DefaultLimits,
		},
		{
			name: "platform component too long",
			meta: func(t *testing.T) *metafile.MetaInfo {
				return materializeMultiPathMeta(t, []string{strings.Repeat("x", 256)}, []byte("x"))
			},
			limits: DefaultLimits,
		},
		{
			name: "staging component depth",
			meta: func(t *testing.T) *metafile.MetaInfo {
				components := make([]string, 255)
				for index := range components {
					components[index] = "d"
				}
				return materializeMultiPathMeta(t, components, []byte("x"))
			},
			limits: DefaultLimits,
		},
		{
			name: "namespace amplification budget",
			meta: func(t *testing.T) *metafile.MetaInfo {
				components := make([]string, 24)
				for index := range components {
					components[index] = "d"
				}
				return materializeMultiPathMeta(t, components, []byte("x"))
			},
			limits: func() Limits {
				limits := DefaultLimits()
				limits.MaxNamespaceBytes = 1024
				return limits
			},
		},
		{
			name: "scratch cannot hold protocol event",
			meta: func(t *testing.T) *metafile.MetaInfo {
				return materializeSingleV1Meta(t, "final.bin", []byte("x"))
			},
			limits: func() Limits {
				limits := DefaultLimits()
				limits.MaxScratchBytes = 1
				return limits
			},
		},
		{
			name: "content file cannot fit scratch budget",
			meta: func(t *testing.T) *metafile.MetaInfo {
				return materializeSingleV1Meta(t, "final.bin", bytes.Repeat([]byte{'x'}, 65<<10))
			},
			limits: func() Limits {
				limits := DefaultLimits()
				limits.MaxScratchBytes = limits.MaxEventBytes
				return limits
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			targetRoot := t.TempDir()
			preflightMaterializeFilesystem(t, targetRoot)
			before, err := os.ReadDir(targetRoot)
			if err != nil {
				t.Fatal(err)
			}
			report, err := Run(context.Background(), RunOptions{
				Meta: test.meta(t), TargetRoot: targetRoot, ExpectedPlanID: strings.Repeat("0", 24),
				Limits: test.limits(),
			})
			if err == nil || report.WritesPerformed != 0 || report.Outcome != OutcomeBlocked {
				t.Fatalf("predictable policy failure wrote target state: %#v %v", report, err)
			}
			after, readErr := os.ReadDir(targetRoot)
			if readErr != nil || len(after) != len(before) {
				t.Fatalf("target root changed: before=%d after=%d err=%v", len(before), len(after), readErr)
			}
		})
	}
}

func singleMaterializeFixture(t *testing.T, ctx context.Context, name string, content []byte) (*metafile.MetaInfo, seed.DiscoveryResult, string, string) {
	t.Helper()
	meta := materializeSingleV1Meta(t, name, content)
	searchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(searchRoot, "source"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	preflightMaterializeFilesystem(t, targetRoot)
	discovery := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
	return meta, discovery, searchRoot, targetRoot
}

func multiMaterializeFixture(t *testing.T, ctx context.Context) (*metafile.MetaInfo, seed.DiscoveryResult, string) {
	t.Helper()
	meta := materializeMultiV1Meta(t)
	searchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(searchRoot, "source-a"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(searchRoot, "source-b"), []byte("def"), 0o600); err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	preflightMaterializeFilesystem(t, targetRoot)
	discovery := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
	return meta, discovery, targetRoot
}

func runOptionsFor(t *testing.T, meta *metafile.MetaInfo, discovery *seed.DiscoveryResult, targetRoot string) RunOptions {
	t.Helper()
	return RunOptions{
		Meta: meta, Discovery: discovery, TargetRoot: targetRoot,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	}
}

func onlyOperationDirectory(t *testing.T, targetRoot string) string {
	t.Helper()
	entries, err := os.ReadDir(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), operationDirectoryPrefix) {
			return filepath.Join(targetRoot, entry.Name())
		}
	}
	t.Fatal("materialize operation directory was not found")
	return ""
}

func materializeMultiPathMeta(t *testing.T, components []string, content []byte) *metafile.MetaInfo {
	t.Helper()
	path := make([]any, len(components))
	for index, component := range components {
		path[index] = component
	}
	return parseMaterializeMeta(t, map[string]any{
		"files": []any{map[string]any{"length": int64(len(content)), "path": path}},
		"name":  "bundle", "piece length": int64(len(content)), "pieces": sha1Bytes(content),
	})
}

func sha1Bytes(content []byte) []byte {
	digest := sha1.Sum(content)
	return digest[:]
}
