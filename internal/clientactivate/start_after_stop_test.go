package clientactivate

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

type testTerminalStopProof struct{ prerequisite TerminalStopPrerequisite }

func (proof *testTerminalStopProof) ActivationStartPrerequisite() (TerminalStopPrerequisite, bool) {
	if proof == nil {
		return TerminalStopPrerequisite{}, false
	}
	return proof.prerequisite, true
}

func TestStartAfterStopUsesExactStopPrerequisiteAndOneStartOnly(t *testing.T) {
	fixture := makeActivationFixture(t)
	prior := completeStartedActivation(t, fixture)
	authority, proof := prepareRestartAuthority(t, fixture, prior, false)

	base := proof.prerequisite.ObservedAtEnd.Add(time.Second)
	stopped := fixture.job("stoppedUP", 1)
	started := fixture.job("uploading", 1)
	preview, err := Preview(context.Background(), authority,
		newActivationSession(fixture, activationLedger(fixture.meta, &stopped, base)), false)
	if err != nil || preview.Outcome != OutcomeReady || preview.Plan.Action != ActionStartAfterStop ||
		preview.Plan.Control.StartRouteID == "" || preview.Plan.Control.RecheckRouteID == "" ||
		countString(preview.Effect, "read_terminal_client_stop") != 1 ||
		countString(preview.Effect, "read_prior_terminal_client_activation") != 1 ||
		countString(preview.Effect, "read_stopped_adoption") != 0 {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	runSession := newActivationSession(fixture,
		activationLedger(fixture.meta, &stopped, base.Add(time.Second)),
		activationLedger(fixture.meta, &started, base.Add(2*time.Second)))
	report, err := Run(context.Background(), RunOptions{Authority: authority, ExpectedPlanID: preview.Plan.ID,
		Session: runSession, AcknowledgeStart: true})
	if err != nil || report.Outcome != OutcomeStartedClientClaim || runSession.startCalls != 1 || runSession.recheckCalls != 0 ||
		report.Journal.RecheckAttempts != 0 || report.Journal.RecheckCompletionDurable || report.Journal.StartAttempts != 1 ||
		!report.Journal.ActivationCompletionDurable || report.Client.ActionReceipt.Effect != downloader.ControlEffectStart {
		t.Fatalf("report=%#v start=%d recheck=%d err=%v", report, runSession.startCalls, runSession.recheckCalls, err)
	}
	operation, parseErr := ParseOperationID(report.Operation.ID)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	verified, observation, verifyErr := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: preview.Plan.ID,
	})
	if verifyErr != nil || !verified.Verified() || observation.Action != ActionStartAfterStop ||
		observation.RecheckCompletionID != "" || observation.StopCompletionID != proof.prerequisite.CompletionID ||
		observation.TerminalMarkerID != observation.ActivationCompletionID {
		t.Fatalf("verified=%v observation=%#v err=%v", verified != nil && verified.Verified(), observation, verifyErr)
	}
}

func countString(values []string, wanted string) int {
	count := 0
	for _, value := range values {
		if value == wanted {
			count++
		}
	}
	return count
}

func TestStartAfterStopUnknownRequestNeedsExplicitRepeat(t *testing.T) {
	fixture := makeActivationFixture(t)
	prior := completeStartedActivation(t, fixture)
	authority, proof := prepareRestartAuthority(t, fixture, prior, false)
	base := proof.prerequisite.ObservedAtEnd.Add(time.Second)
	stopped := fixture.job("stoppedUP", 1)
	preview, err := Preview(context.Background(), authority,
		newActivationSession(fixture, activationLedger(fixture.meta, &stopped, base)), false)
	if err != nil {
		t.Fatal(err)
	}
	unknownSession := newActivationSession(fixture,
		activationLedger(fixture.meta, &stopped, base.Add(time.Second)),
		activationLedger(fixture.meta, &stopped, base.Add(2*time.Second)))
	unknownSession.startErr = context.DeadlineExceeded
	report, err := Run(context.Background(), RunOptions{Authority: authority, ExpectedPlanID: preview.Plan.ID,
		Session: unknownSession, AcknowledgeStart: true})
	if err == nil || report.Outcome != OutcomeStartRequestUnknown || unknownSession.startCalls != 1 {
		t.Fatalf("report=%#v starts=%d err=%v", report, unknownSession.startCalls, err)
	}
	operation, parseErr := ParseOperationID(report.Operation.ID)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	resumeSession := newActivationSession(fixture, activationLedger(fixture.meta, &stopped, base.Add(3*time.Second)))
	resumed, resumeErr := Resume(context.Background(), operation, RunOptions{Authority: authority,
		ExpectedPlanID: preview.Plan.ID, Session: resumeSession})
	if resumeErr == nil || resumed.Outcome != OutcomeStartRequestUnknown || resumeSession.startCalls != 0 {
		t.Fatalf("resumed=%#v starts=%d err=%v", resumed, resumeSession.startCalls, resumeErr)
	}
}

func TestStartAfterStopRejectsUnattributedOrSerializedProof(t *testing.T) {
	fixture := makeActivationFixture(t)
	prior := completeStartedActivation(t, fixture)
	_, attributed := prepareRestartAuthority(t, fixture, prior, false)
	unattributed := *attributed
	unattributed.prerequisite.CompletionBasis = "exact_stopped_after_unknown_attempt_causality_unproven"
	if _, err := PrepareStartAfterStopAuthority(fixture.verifiedFinal, prior, &unattributed, restartOptions(t, fixture)); err == nil {
		t.Fatal("unattributed terminal stop authorized a start")
	}
	if _, err := PrepareStartAfterStopAuthority(fixture.verifiedFinal, prior, nil, restartOptions(t, fixture)); err == nil {
		t.Fatal("public data without process-local proof authorized a start")
	}
}

func TestStartAfterStopTerminalCanBePrunedAndReusedForCurrentUse(t *testing.T) {
	fixture := makeActivationFixture(t)
	prior := completeStartedActivation(t, fixture)
	authority, proof := prepareRestartAuthority(t, fixture, prior, true)
	base := proof.prerequisite.ObservedAtEnd.Add(time.Second)
	stopped := fixture.job("stoppedUP", 1)
	started := fixture.job("uploading", 1)
	preview, err := Preview(context.Background(), authority,
		newActivationSession(fixture, activationLedger(fixture.meta, &stopped, base)), false)
	if err != nil {
		t.Fatal(err)
	}
	run, err := Run(context.Background(), RunOptions{Authority: authority, ExpectedPlanID: preview.Plan.ID,
		Session: newActivationSession(fixture,
			activationLedger(fixture.meta, &stopped, base.Add(time.Second)),
			activationLedger(fixture.meta, &started, base.Add(2*time.Second))),
		AcknowledgeStart: true})
	if err != nil {
		t.Fatal(err)
	}
	operation, err := ParseOperationID(run.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	retention, err := Prune(context.Background(), PruneOptions{TargetRoot: fixture.targetRoot, OperationID: operation,
		ExpectedPlanID: preview.Plan.ID, Acknowledge: true, Limits: DefaultRetentionLimits()})
	if err != nil || retention.Outcome != RetentionOutcomePruned || !retention.Markers.ExactTombstone {
		t.Fatalf("retention=%#v err=%v", retention, err)
	}
	terminal, observation, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: preview.Plan.ID,
	})
	if err != nil || !terminal.Verified() || observation.Action != ActionStartAfterStop ||
		observation.Assurance != "same_invocation_bound_canonical_activation_retention_tombstone_read_without_durability_or_client_refresh" {
		t.Fatalf("terminal=%v observation=%#v err=%v", terminal != nil && terminal.Verified(), observation, err)
	}
	options := restartOptions(t, fixture)
	currentAuthority, err := PrepareCurrentUse(fixture.verifiedFinal, terminal, CurrentUseOptions{
		ClientConfigID: options.ClientConfigID, HostRoot: options.HostRoot, ClientRoot: options.ClientRoot,
		ClientWindows: options.ClientWindows, FileLimits: options.FileLimits,
	})
	if err != nil {
		t.Fatal(err)
	}
	current, currentObservation, err := VerifyCurrentUse(context.Background(), currentAuthority,
		newActivationSession(fixture, activationLedger(fixture.meta, &started, base.Add(3*time.Second))))
	if err != nil || current == nil || !current.JobStarted() || currentObservation.TerminalMarkerID != observation.TerminalMarkerID {
		t.Fatalf("current=%#v observation=%#v err=%v", current, currentObservation, err)
	}
}

func completeStartedActivation(t *testing.T, fixture activationFixture) *VerifiedCompletion {
	t.Helper()
	base := time.Now().UTC().Add(-20 * time.Second)
	incomplete := fixture.job("stoppedDL", 0.25)
	checking := fixture.job("checkingDL", 0.25)
	complete := fixture.job("stoppedUP", 1)
	started := fixture.job("uploading", 1)
	preview, err := Preview(context.Background(), fixture.authority,
		newActivationSession(fixture, activationLedger(fixture.meta, &incomplete, base)), true)
	if err != nil {
		t.Fatal(err)
	}
	run, err := Run(context.Background(), RunOptions{Authority: fixture.authority, ExpectedPlanID: preview.Plan.ID,
		Session: newActivationSession(fixture,
			activationLedger(fixture.meta, &incomplete, base.Add(time.Second)),
			activationLedger(fixture.meta, &checking, base.Add(2*time.Second))),
		StartAfterRecheck: true, AcknowledgeRecheck: true})
	if err != nil {
		t.Fatal(err)
	}
	operation, err := ParseOperationID(run.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Resume(context.Background(), operation, RunOptions{Authority: fixture.authority, ExpectedPlanID: preview.Plan.ID,
		Session: newActivationSession(fixture,
			activationLedger(fixture.meta, &complete, base.Add(3*time.Second)),
			activationLedger(fixture.meta, &started, base.Add(4*time.Second))),
		StartAfterRecheck: true, AcknowledgeStart: true})
	if err != nil {
		t.Fatal(err)
	}
	verified, _, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: preview.Plan.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return verified
}

func prepareRestartAuthority(t *testing.T, fixture activationFixture, prior *VerifiedCompletion, retained bool) (*PreparedAuthority, *testTerminalStopProof) {
	t.Helper()
	options := restartOptions(t, fixture)
	currentAuthority, err := PrepareCurrentUse(fixture.verifiedFinal, prior, CurrentUseOptions{
		ClientConfigID: options.ClientConfigID, HostRoot: options.HostRoot, ClientRoot: options.ClientRoot,
		ClientWindows: options.ClientWindows, FileLimits: options.FileLimits,
	})
	if err != nil {
		t.Fatal(err)
	}
	expectation := currentAuthority.Expectation()
	base := time.Now().UTC().Add(-5 * time.Second)
	startedJob := fixture.job("uploading", 1)
	current, observation, err := VerifyCurrentUse(context.Background(), currentAuthority,
		newActivationSession(fixture, activationLedger(fixture.meta, &startedJob, base)))
	if err != nil || current == nil {
		t.Fatalf("current=%#v observation=%#v err=%v", current, observation, err)
	}
	final := observation.Final
	sha := func(character string) string { return "sha256:" + strings.Repeat(character, 64) }
	stopPlanID := strings.Repeat("e", 24)
	proof := &testTerminalStopProof{prerequisite: TerminalStopPrerequisite{
		Driver: observation.Driver, OperationID: stopOperationIDForPlan(stopPlanID), PlanID: stopPlanID, IntentID: sha("f"),
		CompletionID: sha("1"), CompletionBasis: "accepted_response_then_exact_stopped", UseID: observation.UseID,
		JobID: observation.JobID, FileLayoutID: observation.FileLayoutID, CompleteFileSnapshotID: observation.CompleteFileSnapshotID,
		StoppedJobState: "stoppedUP", ClientConfigID: observation.ClientConfigID, PathMappingID: observation.PathMappingID,
		ActivationOperationID: expectation.ActivationOperationID, ActivationPlanID: expectation.ActivationPlanID,
		ActivationTerminalID: expectation.TerminalMarkerID, MetafileVariantID: final.MetafileVariantID,
		InfoHashV1: final.InfoHashV1, InfoHashV2: final.InfoHashV2, MaterializeOperationID: final.OperationID,
		MaterializePlanID: final.MaterializePlanID, TargetRootIdentity: final.TargetRootIdentity,
		FinalObjectIdentity: final.FinalObjectIdentity, MultiFile: final.MultiFile, ManifestFiles: final.ManifestFiles,
		ContentBytes: final.ContentBytes, ObservedAtStart: base.Add(time.Second), ObservedAtEnd: base.Add(2 * time.Second),
		RetainedTombstone: retained,
	}}
	authority, err := PrepareStartAfterStopAuthority(fixture.verifiedFinal, prior, proof, options)
	if err != nil {
		t.Fatal(err)
	}
	return authority, proof
}

func restartOptions(t *testing.T, fixture activationFixture) AuthorityOptions {
	t.Helper()
	hostRoot, err := filepath.EvalSymlinks(fixture.targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	return AuthorityOptions{Driver: fixture.driver, ClientConfigID: fixture.clientConfig, HostRoot: hostRoot,
		ClientRoot: "/downloads", ClientWindows: false, FileLimits: downloader.DefaultJobFileLedgerLimits()}
}
