package materialize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
)

type materializeForgetFixture struct {
	meta       *metafile.MetaInfo
	content    []byte
	targetRoot string
	planID     string
	operation  OperationID
}

func retainedMaterializeFixture(t *testing.T) (*materializeForgetFixture, *ForgetOptions) {
	t.Helper()
	ctx := context.Background()
	meta, content, targetRoot, planID, operation := committedOperationForPrune(t, ctx)
	pruned, err := Prune(ctx, PruneOptions{
		Meta: meta, TargetRoot: targetRoot, OperationID: operation, ExpectedPlanID: planID,
		JournalLimits: DefaultLimits(), RetentionLimits: DefaultRetentionLimits(),
	})
	if err != nil || pruned.Outcome != RetentionOutcomePruned || !pruned.Markers.ExactTombstone {
		t.Fatalf("prune=%#v err=%v", pruned, err)
	}
	fixture := &materializeForgetFixture{meta: meta, content: content, targetRoot: targetRoot, planID: planID, operation: operation}
	options := &ForgetOptions{TargetRoot: targetRoot, OperationID: operation, ExpectedPlanID: planID, Acknowledge: true, Limits: DefaultForgetLimits()}
	return fixture, options
}

func TestForgetRemovesLastMaterializeHistoricalEvidence(t *testing.T) {
	fixture, options := retainedMaterializeFixture(t)
	report, err := Forget(context.Background(), *options)
	if err != nil || report.Outcome != ForgetOutcomeForgotten || !report.Authority.TargetHistoricalEvidenceErased ||
		report.Authority.MarkerDurable || report.Authority.ExactTombstoneEvidenceAvailable || report.WritesPerformed == 0 || report.Operation.Resumable ||
		!report.RootIntentPublication.Published || !report.Removals.RetentionIntent.Removed ||
		!report.Removals.RetentionComplete.Removed || !report.Removals.RetentionDirectory.Removed ||
		!report.Removals.OperationSubtree.Removed || !report.Removals.RootIntent.Removed {
		t.Fatalf("forget=%#v err=%v", report, err)
	}
	operationName, _ := OperationDirectoryName(fixture.operation)
	rootName, _ := ForgetRootName(fixture.operation)
	for _, path := range []string{filepath.Join(fixture.targetRoot, operationName), filepath.Join(fixture.targetRoot, rootName)} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("forgotten control object remains at %q: %v", path, statErr)
		}
	}
	if raw, readErr := os.ReadFile(filepath.Join(fixture.targetRoot, "final.bin")); readErr != nil || !bytes.Equal(raw, fixture.content) {
		t.Fatalf("forget changed final: %q %v", raw, readErr)
	}
	rawReport, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, secret := range []string{fixture.targetRoot, filepath.Join(fixture.targetRoot, "final.bin"), "final.bin"} {
		if bytes.Contains(rawReport, []byte(secret)) {
			t.Fatalf("forget report leaked %q: %s", secret, rawReport)
		}
	}

	repeated, repeatErr := Forget(context.Background(), *options)
	if !errors.Is(repeatErr, ErrOperationNotFound) || repeated.Outcome != ForgetOutcomeAbsentUnattributed ||
		repeated.WritesPerformed != 0 || repeated.Authority.TargetHistoricalEvidenceErased {
		t.Fatalf("repeated forget=%#v err=%v", repeated, repeatErr)
	}
}

func TestForgetRecoversAcrossBothDurableBoundaries(t *testing.T) {
	for _, stopPhase := range []string{"root_intent_published", "operation_removed"} {
		t.Run(stopPhase, func(t *testing.T) {
			fixture, options := retainedMaterializeFixture(t)
			stop := errors.New("stop after " + stopPhase)
			forgetTransitionHook = func(phase string) error {
				if phase == stopPhase {
					return stop
				}
				return nil
			}
			t.Cleanup(func() { forgetTransitionHook = nil })
			report, err := Forget(context.Background(), *options)
			if !errors.Is(err, stop) || !report.Authority.MarkerDurable || report.Authority.TargetHistoricalEvidenceErased {
				t.Fatalf("interrupted forget=%#v err=%v", report, err)
			}
			rootName, _ := ForgetRootName(fixture.operation)
			raw, readErr := os.ReadFile(filepath.Join(fixture.targetRoot, rootName))
			marker, markerID, decodeErr := DecodeForgetIntent(bytes.NewReader(raw))
			canonical, canonicalID, encodeErr := EncodeForgetIntent(marker)
			if readErr != nil || decodeErr != nil || encodeErr != nil || markerID != canonicalID || !bytes.Equal(raw, canonical) {
				t.Fatalf("forget marker round trip id=%s/%s read=%v decode=%v encode=%v", markerID, canonicalID, readErr, decodeErr, encodeErr)
			}
			for index, mutated := range [][]byte{
				bytes.Replace(raw, []byte(`"schema":`), []byte(`"schema":"duplicate","schema":`), 1),
				bytes.Replace(raw, []byte(`"basis":`), []byte(`"unknown":true,"basis":`), 1),
				append(append([]byte(nil), raw...), []byte("{}\n")...),
				append(bytes.Repeat([]byte{'x'}, int(maxForgetMarkerBytes)), 'x'),
			} {
				if _, _, err := DecodeForgetIntent(bytes.NewReader(mutated)); err == nil {
					t.Fatalf("unsafe forget marker mutation %d accepted", index)
				}
			}
			listed, listErr := ListOperations(context.Background(), fixture.targetRoot, DefaultOperationListLimits())
			if listErr != nil || !listed.Complete || len(listed.Operations) != 1 ||
				listed.Operations[0].ID != fixture.operation || listed.Operations[0].Status != "forget_in_progress_not_inspected" {
				t.Fatalf("forget listing=%#v err=%v", listed, listErr)
			}
			status, statusErr := Status(context.Background(), ControlOptions{TargetRoot: fixture.targetRoot, OperationID: fixture.operation, Limits: DefaultLimits()})
			if statusErr != nil || status.Outcome != OutcomeForgetting || status.Operation.Status != "forgetting" ||
				status.Operation.Resumable || status.WritesPerformed != 0 {
				t.Fatalf("forget status=%#v err=%v", status, statusErr)
			}
			resumed, resumeErr := Resume(context.Background(), ResumeOptions{
				TargetRoot: fixture.targetRoot, OperationID: fixture.operation,
				ExpectedPlanID: fixture.planID, Limits: DefaultLimits(),
			})
			if !errors.Is(resumeErr, ErrPolicy) || resumed.Outcome != OutcomeForgetting || resumed.WritesPerformed != 0 || resumed.Operation.Resumable {
				t.Fatalf("forget-boundary resume=%#v err=%v", resumed, resumeErr)
			}
			wrongPlan := strings.Repeat("f", 24)
			if wrongPlan == fixture.planID {
				wrongPlan = strings.Repeat("e", 24)
			}
			mismatched, mismatchErr := Resume(context.Background(), ResumeOptions{
				TargetRoot: fixture.targetRoot, OperationID: fixture.operation,
				ExpectedPlanID: wrongPlan, Limits: DefaultLimits(),
			})
			if !errors.Is(mismatchErr, ErrPolicy) || mismatched.Outcome != OutcomeBlocked || mismatched.WritesPerformed != 0 ||
				len(mismatched.Blockers) != 1 || mismatched.Blockers[0].Code != "operation.forget_selector_mismatch" {
				t.Fatalf("forget-boundary mismatched resume=%#v err=%v", mismatched, mismatchErr)
			}
			abandoned, abandonErr := Abandon(context.Background(), ControlOptions{
				TargetRoot: fixture.targetRoot, OperationID: fixture.operation, Limits: DefaultLimits(),
			})
			if !errors.Is(abandonErr, ErrPolicy) || abandoned.Outcome != OutcomeForgetting || abandoned.WritesPerformed != 0 || abandoned.Operation.Resumable {
				t.Fatalf("forget-boundary abandon=%#v err=%v", abandoned, abandonErr)
			}
			verified, observation, finalErr := VerifyCurrentFinal(context.Background(), FinalProofOptions{
				Meta: fixture.meta, TargetRoot: fixture.targetRoot, OperationID: fixture.operation,
				ExpectedPlanID: fixture.planID, Limits: DefaultLimits(),
			})
			if !errors.Is(finalErr, ErrPolicy) || verified != nil || observation.OperationID != "" || observation.BytesVerified != 0 {
				t.Fatalf("forget-boundary current final=%#v observation=%#v err=%v", verified, observation, finalErr)
			}
			prune, pruneErr := Prune(context.Background(), PruneOptions{
				TargetRoot: fixture.targetRoot, OperationID: fixture.operation, ExpectedPlanID: fixture.planID,
				JournalLimits: DefaultLimits(), RetentionLimits: DefaultRetentionLimits(),
			})
			if !errors.Is(pruneErr, ErrPolicy) || prune.Outcome != RetentionOutcomeBlocked || prune.WritesPerformed != 0 || prune.Operation.Status != "forgetting" {
				t.Fatalf("forget-boundary prune=%#v err=%v", prune, pruneErr)
			}
			wrongPrune, wrongPruneErr := Prune(context.Background(), PruneOptions{
				TargetRoot: fixture.targetRoot, OperationID: fixture.operation, ExpectedPlanID: wrongPlan,
				JournalLimits: DefaultLimits(), RetentionLimits: DefaultRetentionLimits(),
			})
			if !errors.Is(wrongPruneErr, ErrPolicy) || wrongPrune.Outcome != RetentionOutcomeBlocked || wrongPrune.WritesPerformed != 0 ||
				wrongPrune.Plan.Matches || wrongPrune.Plan.ObservedID != fixture.planID {
				t.Fatalf("forget-boundary mismatched prune=%#v err=%v", wrongPrune, wrongPruneErr)
			}
			wrongForgetOptions := *options
			wrongForgetOptions.ExpectedPlanID = wrongPlan
			wrongForget, wrongForgetErr := Forget(context.Background(), wrongForgetOptions)
			if !errors.Is(wrongForgetErr, ErrPolicy) || wrongForget.Outcome != ForgetOutcomeBlocked || wrongForget.WritesPerformed != 0 ||
				wrongForget.Plan.Matches || wrongForget.Authority.TargetHistoricalEvidenceErased {
				t.Fatalf("forget-boundary mismatched forget=%#v err=%v", wrongForget, wrongForgetErr)
			}
			forgetTransitionHook = nil
			recovered, recoverErr := Forget(context.Background(), *options)
			if recoverErr != nil || recovered.Outcome != ForgetOutcomeForgotten || !recovered.Authority.TargetHistoricalEvidenceErased {
				t.Fatalf("recovered forget=%#v err=%v", recovered, recoverErr)
			}
		})
	}
}

func TestForgetRecoversPartiallyRemovedTombstone(t *testing.T) {
	fixture, options := retainedMaterializeFixture(t)
	stop := errors.New("stop after root intent")
	forgetTransitionHook = func(phase string) error {
		if phase == "root_intent_published" {
			return stop
		}
		return nil
	}
	if _, err := Forget(context.Background(), *options); !errors.Is(err, stop) {
		t.Fatalf("initial forget error=%v", err)
	}
	forgetTransitionHook = nil
	t.Cleanup(func() { forgetTransitionHook = nil })
	operationName, _ := OperationDirectoryName(fixture.operation)
	completePath := filepath.Join(fixture.targetRoot, operationName, retentionDirectoryName, retentionCompleteName)
	if err := os.Remove(completePath); err != nil {
		t.Fatal(err)
	}
	recovered, err := Forget(context.Background(), *options)
	if err != nil || recovered.Outcome != ForgetOutcomeForgotten || !recovered.Removals.RetentionIntent.Removed {
		t.Fatalf("partial recovery=%#v err=%v", recovered, err)
	}
}

func TestForgetPolicyAndLastMarkerRacesFailClosed(t *testing.T) {
	t.Run("pre-cancelled", func(t *testing.T) {
		fixture, options := retainedMaterializeFixture(t)
		before, err := os.ReadDir(fixture.targetRoot)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		report, forgetErr := Forget(ctx, *options)
		after, readErr := os.ReadDir(fixture.targetRoot)
		if !errors.Is(forgetErr, context.Canceled) || report.Outcome != ForgetOutcomeInterrupted || report.WritesPerformed != 0 ||
			readErr != nil || len(after) != len(before) {
			t.Fatalf("pre-cancelled forget=%#v err=%v before=%d after=%d read=%v", report, forgetErr, len(before), len(after), readErr)
		}
	})

	t.Run("acknowledgement", func(t *testing.T) {
		fixture, options := retainedMaterializeFixture(t)
		options.Acknowledge = false
		before, err := os.ReadDir(fixture.targetRoot)
		if err != nil {
			t.Fatal(err)
		}
		report, forgetErr := Forget(context.Background(), *options)
		after, readErr := os.ReadDir(fixture.targetRoot)
		if !errors.Is(forgetErr, ErrPolicy) || readErr != nil || report.Outcome != ForgetOutcomeBlocked || report.WritesPerformed != 0 || len(after) != len(before) {
			t.Fatalf("blocked forget=%#v err=%v before=%d after=%d read=%v", report, forgetErr, len(before), len(after), readErr)
		}
	})

	t.Run("plan selector", func(t *testing.T) {
		fixture, options := retainedMaterializeFixture(t)
		options.ExpectedPlanID = strings.Repeat("f", 24)
		if options.ExpectedPlanID == fixture.planID {
			options.ExpectedPlanID = strings.Repeat("e", 24)
		}
		report, forgetErr := Forget(context.Background(), *options)
		if !errors.Is(forgetErr, ErrPolicy) || report.Outcome != ForgetOutcomeBlocked || report.WritesPerformed != 0 ||
			report.Authority.MarkerDurable || report.Authority.TargetHistoricalEvidenceErased {
			t.Fatalf("mismatched plan forget=%#v err=%v", report, forgetErr)
		}
		status, statusErr := Status(context.Background(), ControlOptions{
			TargetRoot: fixture.targetRoot, OperationID: fixture.operation, Limits: DefaultLimits(),
		})
		if statusErr != nil || status.Outcome != OutcomeRetained {
			t.Fatalf("mismatched selector changed tombstone: %#v err=%v", status, statusErr)
		}
	})

	t.Run("marker budget", func(t *testing.T) {
		fixture, options := retainedMaterializeFixture(t)
		options.Limits.MaxMarkerBytes = 1
		before, err := os.ReadDir(fixture.targetRoot)
		if err != nil {
			t.Fatal(err)
		}
		report, forgetErr := Forget(context.Background(), *options)
		after, readErr := os.ReadDir(fixture.targetRoot)
		if forgetErr == nil || report.Outcome != ForgetOutcomeBlocked || report.WritesPerformed != 0 || readErr != nil || len(after) != len(before) {
			t.Fatalf("budget forget=%#v err=%v before=%d after=%d read=%v", report, forgetErr, len(before), len(after), readErr)
		}
	})

	t.Run("not pruned", func(t *testing.T) {
		ctx := context.Background()
		_, _, targetRoot, planID, operation := committedOperationForPrune(t, ctx)
		report, err := Forget(ctx, ForgetOptions{TargetRoot: targetRoot, OperationID: operation, ExpectedPlanID: planID, Acknowledge: true, Limits: DefaultForgetLimits()})
		if !errors.Is(err, ErrPolicy) || report.Outcome != ForgetOutcomeBlocked || report.WritesPerformed != 0 {
			t.Fatalf("unpruned forget=%#v err=%v", report, err)
		}
	})

	t.Run("changed root intent", func(t *testing.T) {
		fixture, options := retainedMaterializeFixture(t)
		stop := errors.New("stop after root intent")
		forgetTransitionHook = func(phase string) error {
			if phase == "root_intent_published" {
				return stop
			}
			return nil
		}
		t.Cleanup(func() { forgetTransitionHook = nil })
		if _, err := Forget(context.Background(), *options); !errors.Is(err, stop) {
			t.Fatalf("initial forget error=%v", err)
		}
		forgetTransitionHook = nil
		rootName, _ := ForgetRootName(fixture.operation)
		if err := os.WriteFile(filepath.Join(fixture.targetRoot, rootName), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		status, statusErr := Status(context.Background(), ControlOptions{
			TargetRoot: fixture.targetRoot, OperationID: fixture.operation, Limits: DefaultLimits(),
		})
		if !errors.Is(statusErr, ErrIntegrity) || status.Outcome != OutcomeIntegrityFailed || status.Operation.Resumable || !status.Target.RootIdentityBound ||
			len(status.Issues) != 1 || status.Issues[0].Code != "operation.forget_marker_invalid" {
			t.Fatalf("changed intent status=%#v err=%v", status, statusErr)
		}
		report, err := Forget(context.Background(), *options)
		if err == nil || !errors.Is(err, ErrIntegrity) || report.Outcome != ForgetOutcomeIntegrityFailed || report.Operation.Resumable {
			t.Fatalf("changed intent forget=%#v err=%v", report, err)
		}
	})

	t.Run("operation name reappears", func(t *testing.T) {
		fixture, options := retainedMaterializeFixture(t)
		operationName, _ := OperationDirectoryName(fixture.operation)
		forgetTransitionHook = func(phase string) error {
			if phase == "before_root_intent_remove" {
				return os.Mkdir(filepath.Join(fixture.targetRoot, operationName), 0o700)
			}
			return nil
		}
		t.Cleanup(func() { forgetTransitionHook = nil })
		report, err := Forget(context.Background(), *options)
		if err == nil || !errors.Is(err, ErrIntegrity) || report.Outcome != ForgetOutcomeIntegrityFailed ||
			report.Authority.TargetHistoricalEvidenceErased || report.Operation.Resumable {
			t.Fatalf("reappeared operation forget=%#v err=%v", report, err)
		}
		rootName, _ := ForgetRootName(fixture.operation)
		if _, statErr := os.Lstat(filepath.Join(fixture.targetRoot, rootName)); statErr != nil {
			t.Fatalf("root intent was erased despite operation replacement: %v", statErr)
		}
	})

	t.Run("last marker disappears", func(t *testing.T) {
		fixture, options := retainedMaterializeFixture(t)
		rootName, _ := ForgetRootName(fixture.operation)
		forgetTransitionHook = func(phase string) error {
			if phase == "before_root_intent_remove" {
				return os.Remove(filepath.Join(fixture.targetRoot, rootName))
			}
			return nil
		}
		t.Cleanup(func() { forgetTransitionHook = nil })
		report, err := Forget(context.Background(), *options)
		if !errors.Is(err, ErrOperationNotFound) || report.Outcome != ForgetOutcomeAbsentUnattributed ||
			report.Operation.Resumable || report.Authority.TargetHistoricalEvidenceErased ||
			report.Authority.MarkerDurable || report.Authority.ExactTombstoneEvidenceAvailable || report.Removals.RootIntent.Attempted {
			t.Fatalf("disappeared marker forget=%#v err=%v", report, err)
		}
	})
}

func TestForgetRootNameFamiliesDoNotOverlapOperationDirectories(t *testing.T) {
	operation := OperationID("sha256:" + strings.Repeat("a", 64))
	operationName, err := OperationDirectoryName(operation)
	if err != nil {
		t.Fatal(err)
	}
	forgetName, err := ForgetRootName(operation)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseForgetRootName(forgetName)
	if err != nil || parsed != operation || !hasOperationDirectoryPrefix(operationName) || hasForgetMarkerPrefix(operationName) ||
		hasOperationDirectoryPrefix(forgetName) || !hasForgetMarkerPrefix(forgetName) {
		t.Fatalf("materialize control families overlap: operation=%q forget=%q parsed=%s err=%v", operationName, forgetName, parsed, err)
	}
}
