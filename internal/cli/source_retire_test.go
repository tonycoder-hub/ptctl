package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/sourceretire"
)

type sourceRetireJSONEnvelope struct {
	Schema string              `json:"schema"`
	Kind   string              `json:"kind"`
	Data   sourceretire.Report `json:"data"`
}

func TestSeedRetirePlanIsZeroWritePrivateAndRequireAware(t *testing.T) {
	fixture := newClientAdoptCLIFixture(t)
	server := newClientActivateServer(t, fixture.meta, fixture.raw)
	defer server.server.Close()

	baseAdopt := clientAdoptBaseArgs(fixture, server.server.URL)
	var out, errOut bytes.Buffer
	if code := Run(append([]string{"client", "adopt", "plan"}, baseAdopt...), strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("adoption plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	adoptionPlan := decodeClientAdoptReport(t, out.Bytes())
	out.Reset()
	errOut.Reset()
	adoptionRun := append([]string{"client", "adopt", "run"}, baseAdopt...)
	adoptionRun = append(adoptionRun, "--expect-adoption-plan-id", adoptionPlan.Data.Plan.ID, "--acknowledge-client-add")
	if code := Run(adoptionRun, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("adoption run code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	adopted := decodeClientAdoptReport(t, out.Bytes())
	if adopted.Data.Outcome != clientadopt.OutcomeAdoptedPendingRecheck {
		t.Fatalf("adoption did not complete: %s", out.String())
	}

	baseActivation := clientActivateBaseArgs(fixture, server.server.URL, adopted.Data.Operation.ID, adopted.Data.Plan.ID)
	out.Reset()
	errOut.Reset()
	if code := Run(append([]string{"client", "activate", "plan"}, baseActivation...), strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("activation plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	activationPlan := decodeClientActivateReport(t, out.Bytes())
	out.Reset()
	errOut.Reset()
	activationRun := append([]string{"client", "activate", "run"}, baseActivation...)
	activationRun = append(activationRun, "--expect-activation-plan-id", activationPlan.Data.Plan.ID, "--acknowledge-client-recheck")
	if code := Run(activationRun, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("activation run code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	activation := decodeClientActivateReport(t, out.Bytes())
	if activation.Data.Outcome != clientactivate.OutcomeRecheckInProgress {
		t.Fatalf("activation did not enter checking: %s", out.String())
	}
	server.setState("stoppedUP", 1)
	out.Reset()
	errOut.Reset()
	activationResume := append([]string{"client", "activate", "resume"}, baseActivation...)
	activationResume = append(activationResume, "--expect-activation-plan-id", activationPlan.Data.Plan.ID, activationPlan.Data.Operation.ID)
	if code := Run(activationResume, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("activation resume code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	checked := decodeClientActivateReport(t, out.Bytes())
	if checked.Data.Outcome != clientactivate.OutcomeCheckedStopped || !checked.Data.Journal.RecheckCompletionDurable {
		t.Fatalf("activation did not complete recheck: %s", out.String())
	}

	args := sourceRetireBaseArgs(fixture, activationPlan.Data.Operation.ID, activationPlan.Data.Plan.ID)
	out.Reset()
	errOut.Reset()
	if code := Run(args, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("retire plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	report := decodeSourceRetireReport(t, out.Bytes())
	if report.Schema != "ptctl.dev/v1" || report.Kind != "content.source_retirement_plan" ||
		report.Data.Outcome != sourceretire.OutcomeEligible || report.Data.WritesPerformed != 0 ||
		report.Data.DeletionPerformed || report.Data.Plan.DeletionAuthority != "none" || report.Data.Plan.ID == "" ||
		len(report.Data.Plan.SourceFiles) != 1 || report.Data.Plan.SourceFiles[0].SourcePath != "" ||
		!report.Data.Scan.Complete || !report.Data.Scan.VerificationComplete || report.Data.Scan.StopReasons == nil ||
		report.Data.Blockers == nil || report.Data.Issues == nil || report.Data.Warnings == nil {
		t.Fatalf("unexpected retirement report: %s", out.String())
	}
	assertSourceRetirePrivate(t, out.Bytes(), fixture)
	tableArgs := append([]string(nil), args...)
	for index := 0; index < len(tableArgs)-1; index++ {
		if tableArgs[index] == "--output" {
			tableArgs[index+1] = "table"
			break
		}
	}
	out.Reset()
	errOut.Reset()
	if code := Run(tableArgs, strings.NewReader(""), &out, &errOut); code != 0 ||
		!strings.Contains(out.String(), "DELETION AUTHORITY") || !strings.Contains(out.String(), "none") {
		t.Fatalf("retire table code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	assertSourceRetirePrivate(t, out.Bytes(), fixture)

	// Explicit path disclosure changes presentation only, not the reviewed plan
	// identity or any write/deletion count.
	shownArgs := append(append([]string(nil), args...), "--show-absolute-paths")
	out.Reset()
	errOut.Reset()
	if code := Run(shownArgs, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("shown retire plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	shown := decodeSourceRetireReport(t, out.Bytes())
	if shown.Data.Plan.ID != report.Data.Plan.ID || shown.Data.Plan.SourceFiles[0].SourcePath != fixture.materialize.sourcePath {
		t.Fatalf("path opt-in changed authority or failed: %s", out.String())
	}

	// Selecting the published final itself is a complete positive conflict. The
	// report is still emitted first; --require-eligible converts it to exit 4.
	blockedArgs := sourceRetireBaseArgs(fixture, activationPlan.Data.Operation.ID, activationPlan.Data.Plan.ID)
	for index := 0; index < len(blockedArgs)-1; index++ {
		if blockedArgs[index] == "--search-root" {
			blockedArgs[index+1] = fixture.materialize.targetRoot
			break
		}
	}
	blockedArgs = append(blockedArgs, "--require-eligible")
	out.Reset()
	errOut.Reset()
	if code := Run(blockedArgs, strings.NewReader(""), &out, &errOut); code != 4 {
		t.Fatalf("blocked retire plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	blocked := decodeSourceRetireReport(t, out.Bytes())
	if blocked.Data.Outcome != sourceretire.OutcomeBlocked || blocked.Data.WritesPerformed != 0 ||
		!sourceRetireFinding(blocked.Data.Blockers, "source.overlaps_final") {
		t.Fatalf("published final was not blocked: %s", out.String())
	}

	limitedArgs := append(append([]string(nil), args...), "--max-proof-bytes", "1", "--require-eligible")
	out.Reset()
	errOut.Reset()
	if code := Run(limitedArgs, strings.NewReader(""), &out, &errOut); code != 4 {
		t.Fatalf("limited retire plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	limited := decodeSourceRetireReport(t, out.Bytes())
	if limited.Data.Outcome != sourceretire.OutcomeIncomplete || limited.Data.Scan.StopReasons == nil ||
		len(limited.Data.Scan.StopReasons) == 0 || limited.Data.WritesPerformed != 0 ||
		limited.Data.Activation.ObservedAtStart == "" || limited.Data.Final.OperationID == "" {
		t.Fatalf("proof budget was not reported as incomplete: %s", out.String())
	}
}

func TestSeedRetireUsageAndHelpAreStrict(t *testing.T) {
	reader := &trackingReader{}
	var out, errOut bytes.Buffer
	code := Run([]string{"seed", "retire", "plan", "--torrent", "missing.torrent", "--search-root", "missing",
		"--target", "missing", "--materialize-operation", "bad", "--materialize-plan-id", strings.Repeat("a", 24),
		"--activation-operation", strings.Repeat("b", 64), "--activation-plan-id", strings.Repeat("c", 24)}, reader, &out, &errOut)
	if code != 2 || reader.read {
		t.Fatalf("bad usage code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"seed", "retire", "--help"}, strings.NewReader(""), &out, &errOut); code != 0 ||
		!strings.Contains(out.String(), "deletion_authority") || !strings.Contains(out.String(), "zero writes") {
		t.Fatalf("help code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func sourceRetireBaseArgs(fixture clientAdoptCLIFixture, activationOperation, activationPlanID string) []string {
	return []string{"seed", "retire", "plan", "--metafile-store", fixture.storeRoot, "--metafile-variant", fixture.variantID,
		"--search-root", fixture.materialize.sourceRoot, "--target", fixture.materialize.targetRoot,
		"--materialize-operation", fixture.operation, "--materialize-plan-id", fixture.materialize.planID,
		"--activation-operation", activationOperation, "--activation-plan-id", activationPlanID,
		"--timeout", "1m", "--output", "json"}
}

func decodeSourceRetireReport(t *testing.T, raw []byte) sourceRetireJSONEnvelope {
	t.Helper()
	var result sourceRetireJSONEnvelope
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode source retirement report: %v\n%s", err, raw)
	}
	return result
}

func assertSourceRetirePrivate(t *testing.T, raw []byte, fixture clientAdoptCLIFixture) {
	t.Helper()
	for _, secret := range []string{fixture.materialize.sourceRoot, fixture.materialize.sourcePath,
		fixture.materialize.targetRoot, fixture.materialize.finalPath, materializeSourceName} {
		if secret != "" && bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("source retirement report leaked %q: %s", secret, raw)
		}
	}
}

func sourceRetireFinding(findings []sourceretire.Finding, code string) bool {
	for _, finding := range findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}
