package clientadopt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

type testTerminalRemovalProof struct {
	prerequisite TerminalRemovalPrerequisite
	available    bool
}

func (proof *testTerminalRemovalProof) AdoptionReAddPrerequisite() (TerminalRemovalPrerequisite, bool) {
	if proof == nil || !proof.available {
		return TerminalRemovalPrerequisite{}, false
	}
	return proof.prerequisite, true
}

func TestAttributedRemovalAuthorizesOneFreshStoppedReAdd(t *testing.T) {
	ctx := context.Background()
	fixture := makeMaterializedFixture(t, ctx)
	base := prepareFixturePlan(t, fixture)
	proof := removalProofForFixture(t, fixture, base, false)
	prepared := prepareRemovalReAddPlan(t, fixture, base, proof)
	if prepared.plan.Action != ActionReAddAfterRemoval || prepared.plan.TerminalRemoval == nil ||
		prepared.plan.TerminalRemoval.CompletionID != proof.prerequisite.CompletionID ||
		prepared.plan.PriorAdoptionCompletionID != "" {
		t.Fatalf("removal-authorized plan=%#v", prepared.plan)
	}

	stale := ledgerSnapshot(fixture.meta, nil, proof.prerequisite.ObservedAtEnd.Add(-time.Second))
	staleReport, err := Preview(ctx, prepared, &fakeMutationSession{requests: 1, ledgers: []downloader.LedgerSnapshot{stale}})
	if !errors.Is(err, ErrIntegrity) || staleReport.Outcome != OutcomeIntegrityFailed || staleReport.WritesPerformed != 0 {
		t.Fatalf("stale report=%#v err=%v", staleReport, err)
	}

	before := ledgerSnapshot(fixture.meta, nil, proof.prerequisite.ObservedAtEnd.Add(time.Second))
	preview, err := Preview(ctx, prepared, &fakeMutationSession{requests: 1, ledgers: []downloader.LedgerSnapshot{before}})
	if err != nil || preview.Outcome != OutcomeReady || preview.Plan.Action != ActionReAddAfterRemoval ||
		preview.TerminalRemoval == nil || preview.TerminalRemoval.AuthorityForm != "live_journal" ||
		preview.TerminalRemoval.Basis != "accepted_response_then_exact_absence" ||
		!containsEffect(preview.Effect, "read_terminal_client_removal") {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}

	blockedSession := &fakeMutationSession{requests: 1, ledgers: []downloader.LedgerSnapshot{before}}
	blocked, err := Run(ctx, RunOptions{Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Metafile: fixture.payload(t),
		Session: blockedSession, AcknowledgeAdd: true})
	if !errors.Is(err, ErrPolicy) || blocked.Outcome != OutcomeBlocked || blocked.WritesPerformed != 0 ||
		blockedSession.requests != 1 || blockedSession.adds != 0 ||
		!findingCode(blocked.Blockers, "acknowledgement.client_re_add_after_removal_required") {
		t.Fatalf("blocked=%#v requests=%d adds=%d err=%v", blocked, blockedSession.requests, blockedSession.adds, err)
	}

	savePath, _ := prepared.savePath()
	contentPath, _ := prepared.contentPath()
	job := &downloader.Torrent{
		Hash: "re-added-opaque-job", InfoHashV1: fixture.meta.InfoHashV1, IdentityStatus: downloader.IdentityStatusValid,
		IdentityEvidence: []string{"magnet_xt_btih_hex"}, IdentityIssues: []string{}, SizeBytes: fixture.meta.TotalLength,
		State: "stoppedDL", SavePath: savePath, ContentPath: contentPath,
	}
	after := ledgerSnapshot(fixture.meta, job, before.ObservedAtEnd.Add(time.Second))
	session := &fakeMutationSession{requests: 1, ledgers: []downloader.LedgerSnapshot{before, after}}
	report, err := Run(ctx, RunOptions{Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Metafile: fixture.payload(t),
		Session: session, AcknowledgeAdd: true, AcknowledgeRemovalReAdd: true})
	if err != nil || report.Outcome != OutcomeAdoptedPendingRecheck || report.Plan.Action != ActionReAddAfterRemoval ||
		session.adds != 1 || session.requests != 4 || !report.Journal.CompletionDurable || report.TerminalRemoval == nil {
		t.Fatalf("report=%#v requests=%d adds=%d err=%v", report, session.requests, session.adds, err)
	}
	verified, observation, err := VerifyCompletion(ctx, CompletionProofOptions{
		TargetRoot: fixture.targetRoot, OperationID: prepared.OperationID(), ExpectedPlanID: prepared.PlanID(),
	})
	if err != nil || !verified.Verified() || observation.Action != ActionReAddAfterRemoval ||
		observation.RemovalCompletionID != proof.prerequisite.CompletionID {
		t.Fatalf("verified=%v observation=%#v err=%v", verified != nil && verified.Verified(), observation, err)
	}
	reconciliation, ok := verified.ReconciliationAdoptionCompletion()
	if !ok || reconciliation.Action != ActionReAddAfterRemoval ||
		reconciliation.RemovalOperationID != proof.prerequisite.OperationID ||
		reconciliation.RemovalPlanID != proof.prerequisite.PlanID ||
		reconciliation.RemovalCompletionID != proof.prerequisite.CompletionID ||
		reconciliation.RemovalCompletionBasis != "accepted_response_then_exact_absence" {
		t.Fatalf("reconciliation completion=%#v ok=%t", reconciliation, ok)
	}
	status, err := Status(ctx, StatusOptions{TargetRoot: fixture.targetRoot, OperationID: prepared.OperationID()})
	if err != nil || status.TerminalRemoval == nil || status.TerminalRemoval.AuthorityForm != "historical_plan_reference" ||
		status.TerminalRemoval.CompletionID != proof.prerequisite.CompletionID {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	pruned, err := Prune(ctx, PruneOptions{TargetRoot: fixture.targetRoot, OperationID: prepared.OperationID(),
		ExpectedPlanID: prepared.PlanID(), Acknowledge: true, Limits: DefaultRetentionLimits()})
	if err != nil || pruned.Outcome != RetentionOutcomePruned || !pruned.Markers.ExactTombstone {
		t.Fatalf("prune=%#v err=%v", pruned, err)
	}
	retained, retainedObservation, err := VerifyCompletion(ctx, CompletionProofOptions{
		TargetRoot: fixture.targetRoot, OperationID: prepared.OperationID(), ExpectedPlanID: prepared.PlanID(),
	})
	if err != nil || retained == nil || !retained.Verified() || !retainedObservation.RetainedTombstone ||
		retainedObservation.Action != ActionReAddAfterRemoval || retainedObservation.RemovalCompletionID != proof.prerequisite.CompletionID {
		t.Fatalf("retained=%v observation=%#v err=%v", retained != nil && retained.Verified(), retainedObservation, err)
	}
}

func TestRemovalReAddPlanIsStableAcrossLiveAndRetainedAuthority(t *testing.T) {
	fixture := makeMaterializedFixture(t, context.Background())
	base := prepareFixturePlan(t, fixture)
	live := removalProofForFixture(t, fixture, base, false)
	retained := removalProofForFixture(t, fixture, base, true)
	livePlan := prepareRemovalReAddPlan(t, fixture, base, live)
	retainedPlan := prepareRemovalReAddPlan(t, fixture, base, retained)
	if livePlan.PlanID() != retainedPlan.PlanID() || !reflect.DeepEqual(livePlan.plan, retainedPlan.plan) {
		t.Fatalf("authority storage form changed reviewed identity: live=%#v retained=%#v", livePlan.plan, retainedPlan.plan)
	}
	nonCanonical := *livePlan.plan.TerminalRemoval
	observedEnd, err := time.Parse(time.RFC3339Nano, nonCanonical.ObservedAtEnd)
	if err != nil {
		t.Fatal(err)
	}
	nonCanonical.ObservedAtEnd = observedEnd.In(time.FixedZone("offset", 8*60*60)).Format(time.RFC3339Nano)
	if err := nonCanonical.Validate(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("non-canonical removal time accepted: %v", err)
	}
	nonCanonicalPlan := livePlan.plan
	nonCanonicalPlan.TerminalRemoval = &nonCanonical
	if err := nonCanonicalPlan.Validate(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("plan accepted non-canonical removal time: %v", err)
	}

	unattributed := *live
	unattributed.prerequisite.CompletionBasis = "exact_absence_after_unknown_attempt_causality_unproven"
	if _, err := buildRemovalReAddPlan(fixture, base, &unattributed); !errors.Is(err, ErrPolicy) {
		t.Fatalf("unattributed removal authorized re-add: %v", err)
	}
	if _, err := buildRemovalReAddPlan(fixture, base, nil); err != nil {
		t.Fatalf("ordinary initial add unexpectedly rejected: %v", err)
	}
	other := *live
	other.prerequisite.FinalObjectIdentity = "fsbind-v1:" + strings.Repeat("f", 64)
	if _, err := buildRemovalReAddPlan(fixture, base, &other); !errors.Is(err, ErrPolicy) {
		t.Fatalf("different final accepted terminal removal authority: %v", err)
	}
}

func prepareRemovalReAddPlan(t *testing.T, fixture materializedFixture, base *PreparedPlan, proof TerminalRemovalReAddProof) *PreparedPlan {
	t.Helper()
	prepared, err := buildRemovalReAddPlan(fixture, base, proof)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func buildRemovalReAddPlan(fixture materializedFixture, base *PreparedPlan, proof TerminalRemovalReAddProof) (*PreparedPlan, error) {
	root, err := filepath.EvalSymlinks(fixture.targetRoot)
	if err != nil {
		return nil, err
	}
	return BuildPlan(fixture.verified, PlanOptions{Driver: base.plan.Driver, ClientConfigID: base.plan.ClientConfigID,
		HostRoot: root, ClientRoot: "/downloads", ClientWindows: false, PriorRemoval: proof})
}

func removalProofForFixture(t *testing.T, fixture materializedFixture, base *PreparedPlan, retained bool) *testTerminalRemovalProof {
	t.Helper()
	sha := func(character string) string { return "sha256:" + strings.Repeat(character, 64) }
	removalPlanID := strings.Repeat("a", 24)
	activationPlanID := strings.Repeat("b", 24)
	started := time.Now().UTC().Add(-5 * time.Second)
	final := fixture.verified.Observation()
	return &testTerminalRemovalProof{available: true, prerequisite: TerminalRemovalPrerequisite{
		Driver: base.plan.Driver, OperationID: validRemovalID(removalPlanID), PlanID: removalPlanID, IntentID: sha("1"),
		CompletionID: sha("2"), CompletionBasis: "accepted_response_then_exact_absence", UseID: sha("3"),
		JobID: sha("4"), FileLayoutID: sha("5"), CompleteFileSnapshotID: sha("6"),
		ClientConfigID: base.plan.ClientConfigID, PathMappingID: base.plan.PathMappingID,
		ActivationOperationID: validActivationID(activationPlanID), ActivationPlanID: activationPlanID,
		ActivationTerminalID: sha("7"), MetafileVariantID: final.MetafileVariantID,
		InfoHashV1: final.InfoHashV1, InfoHashV2: final.InfoHashV2, MaterializeOperationID: final.OperationID,
		MaterializePlanID: final.MaterializePlanID, TargetRootIdentity: final.TargetRootIdentity,
		FinalObjectIdentity: final.FinalObjectIdentity, MultiFile: final.MultiFile, ManifestFiles: final.ManifestFiles,
		ContentBytes: final.ContentBytes, ObservedAtStart: started, ObservedAtEnd: started.Add(time.Second),
		RetainedTombstone: retained,
	}}
}

func validRemovalID(planID string) string {
	digest := sha256String("ptctl-client-removal-operation-v1\x00" + planID)
	return "sha256:" + digest
}

func validActivationID(planID string) string {
	return "sha256:" + sha256String("ptctl-client-activation-operation-v1\x00"+planID)
}

func sha256String(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
