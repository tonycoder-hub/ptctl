package materialize

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/seed"
)

func TestVerifyCurrentFinalWorksBeforeAndAfterRetentionPrune(t *testing.T) {
	ctx := context.Background()
	content := []byte("current final authority")
	meta := materializeSingleV1Meta(t, "adopt-me.bin", content)
	searchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(searchRoot, "source"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	preflightMaterializeFilesystem(t, targetRoot)
	discovery := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
	report, err := Run(ctx, RunOptions{
		Meta: meta, Discovery: &discovery, TargetRoot: targetRoot,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := ParseOperationID(report.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	options := FinalProofOptions{
		Meta: meta, TargetRoot: targetRoot, OperationID: operationID,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	}
	verified, before, err := VerifyCurrentFinal(ctx, options)
	if err != nil || verified == nil || !verified.Verified() || before.AuthorityBasis != FinalAuthorityJournal || before.BytesVerified != int64(len(content)) {
		t.Fatalf("journal proof=%#v verified=%v err=%v", before, verified, err)
	}
	bridgedFinal, bridge, exact, bridgedObservation, err := VerifyCurrentFinalSource(ctx, options, seed.ExactSourceOptions{})
	if err != nil || bridgedFinal == nil || bridge == nil || exact.SourceOutcome != "verified_exact_root" ||
		bridgedObservation.AuthorityBasis != FinalAuthorityJournal {
		t.Fatalf("bridged proof=%#v source=%#v final=%v bridge=%v err=%v", bridgedObservation, exact, bridgedFinal, bridge, err)
	}
	bridgedSource, ok := exact.VerifiedSource(meta)
	if !ok || !bridge.Matches(bridgedFinal, meta, &exact, bridgedSource) {
		t.Fatal("same-invocation exact source did not retain materialized-final bridge authority")
	}
	public := exact.PublicReportCopy()
	if bridge.Matches(bridgedFinal, meta, &public, bridgedSource) {
		t.Fatal("public discovery copy retained materialized-final source authority")
	}
	rawBridge, err := json.Marshal(bridge)
	if err != nil {
		t.Fatal(err)
	}
	var replayedBridge VerifiedFinalSource
	if err := json.Unmarshal(rawBridge, &replayedBridge); err != nil {
		t.Fatal(err)
	}
	if replayedBridge.Matches(bridgedFinal, meta, &exact, bridgedSource) {
		t.Fatal("serialized materialized-final bridge regained process-local authority")
	}
	independent, err := seed.ObserveExactSource(ctx, meta, filepath.Join(targetRoot, "adopt-me.bin"), seed.ExactSourceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	independentSource, ok := independent.VerifiedSource(meta)
	if !ok || bridge.Matches(bridgedFinal, meta, &independent, independentSource) {
		t.Fatal("a separately observed path recreated materialized-final bridge authority")
	}
	pruned, err := Prune(ctx, PruneOptions{
		Meta: meta, TargetRoot: targetRoot, OperationID: operationID, ExpectedPlanID: discovery.Plan.ID,
		JournalLimits: DefaultLimits(), RetentionLimits: DefaultRetentionLimits(),
	})
	if err != nil || pruned.Outcome != RetentionOutcomePruned {
		t.Fatalf("prune=%#v err=%v", pruned, err)
	}
	verified, after, err := VerifyCurrentFinal(ctx, options)
	if err != nil || verified == nil || !verified.Verified() || after.AuthorityBasis != FinalAuthorityRetention || after.FinalObjectIdentity != before.FinalObjectIdentity {
		t.Fatalf("retained proof=%#v verified=%v err=%v", after, verified, err)
	}
	if _, _, err := verified.Reverify(ctx); err != nil {
		t.Fatalf("reverify after prune: %v", err)
	}
	retainedFinal, retainedBridge, retainedExact, retainedObservation, err := VerifyCurrentFinalSource(ctx, options, seed.ExactSourceOptions{})
	if err != nil || retainedFinal == nil || retainedBridge == nil || retainedObservation.AuthorityBasis != FinalAuthorityRetention ||
		retainedExact.SourceOutcome != "verified_exact_root" {
		t.Fatalf("retained bridge proof=%#v discovery=%#v err=%v", retainedObservation, retainedExact, err)
	}
	retainedSource, ok := retainedExact.VerifiedSource(meta)
	if !ok || !retainedBridge.Matches(retainedFinal, meta, &retainedExact, retainedSource) {
		t.Fatal("sealed retention tombstone did not retain current materialized-final source bridge authority")
	}

	// A completed tombstone is authority only when all heavy mutable operation
	// state is absent and its retained namespace remains exact.
	session, _, err := fsbind.BindExisting(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	directoryName, err := OperationDirectoryName(operationID)
	if err != nil {
		t.Fatal(err)
	}
	subtree, err := session.OpenPrivateSubtreeObserved(directoryName)
	if err != nil {
		t.Fatal(err)
	}
	stage, _ := fsbind.PathFromComponents([]string{stageDirectoryName})
	if _, err := subtree.MkdirAll(ctx, stage); err != nil {
		t.Fatal(err)
	}
	if err := subtree.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyCurrentFinal(ctx, options); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("retained proof accepted restored heavy state: %v", err)
	}
}
