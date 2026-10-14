package materialize

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A committed final has left the private operation subtree. Granting a
// downloader read access or adding a same-filesystem cross-seeding hard link
// must not turn current-final proof into an integrity failure, while write
// access for other principals still fails closed.
func TestCommittedFinalToleratesReadableModesAndHardLinks(t *testing.T) {
	ctx := context.Background()
	meta, discovery, targetRoot := multiMaterializeFixture(t, ctx)
	report, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
	if err != nil || report.Outcome != OutcomeMaterializedVerified {
		t.Fatalf("run=%#v err=%v", report, err)
	}
	operationID, err := ParseOperationID(report.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(targetRoot, "bundle")
	chmod := func(path string, mode os.FileMode) {
		t.Helper()
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	chmod(final, 0o755)
	chmod(filepath.Join(final, "a"), 0o644)
	chmod(filepath.Join(final, "b"), 0o444)
	if err := os.Link(filepath.Join(final, "a"), filepath.Join(targetRoot, "cross-seed-a")); err != nil {
		t.Skipf("hard links are unavailable on the test filesystem: %v", err)
	}

	options := FinalProofOptions{
		Meta: meta, TargetRoot: targetRoot, OperationID: operationID,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	}
	verified, observation, err := VerifyCurrentFinal(ctx, options)
	if err != nil || verified == nil || !verified.Verified() || observation.BytesVerified != 6 {
		t.Fatalf("readable hard-linked final proof=%#v verified=%v err=%v", observation, verified, err)
	}
	resumed, err := Resume(ctx, ResumeOptions{
		Meta: meta, TargetRoot: targetRoot, OperationID: operationID,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	if err != nil || resumed.Outcome != OutcomeAlreadyCommitted || resumed.WritesPerformed != 0 {
		t.Fatalf("committed recovery rejected a readable hard-linked final: %#v %v", resumed, err)
	}

	chmod(filepath.Join(final, "a"), 0o664)
	if _, _, err := VerifyCurrentFinal(ctx, options); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("group-writable final file = %v, want ErrIntegrity", err)
	}
	chmod(filepath.Join(final, "a"), 0o644)
	chmod(final, 0o757)
	if _, _, err := VerifyCurrentFinal(ctx, options); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("world-writable final directory = %v, want ErrIntegrity", err)
	}
}
