package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/sourceretire"
)

type sourceRetireJSONEnvelope struct {
	Schema string              `json:"schema"`
	Kind   string              `json:"kind"`
	Data   sourceretire.Report `json:"data"`
}

type sourceRetireExecutionJSONEnvelope struct {
	Schema string                       `json:"schema"`
	Kind   string                       `json:"kind"`
	Data   sourceretire.ExecutionReport `json:"data"`
}

type sourceRetireRetentionJSONEnvelope struct {
	Schema string                                `json:"schema"`
	Kind   string                                `json:"kind"`
	Data   sourceretire.ExecutionRetentionReport `json:"data"`
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

	args := sourceRetireBaseArgs(fixture, server.server.URL, activationPlan.Data.Operation.ID, activationPlan.Data.Plan.ID)
	out.Reset()
	errOut.Reset()
	requestsBeforeRetire := server.totalRequests()
	if code := Run(args, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("retire plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	report := decodeSourceRetireReport(t, out.Bytes())
	if report.Schema != "ptctl.dev/v1" || report.Kind != "content.source_retirement_plan" ||
		report.Data.Outcome != sourceretire.OutcomeEligible || report.Data.WritesPerformed != 0 ||
		report.Data.DeletionPerformed || report.Data.Plan.DeletionAuthority != "none" || report.Data.Plan.ID == "" ||
		len(report.Data.Plan.SourceFiles) != 1 || report.Data.Plan.SourceFiles[0].SourcePath != "" ||
		!report.Data.Scan.Complete || !report.Data.Scan.VerificationComplete || report.Data.Scan.StopReasons == nil ||
		report.Data.Blockers == nil || report.Data.Issues == nil || report.Data.Warnings == nil ||
		!report.Data.ClientUse.Stable || report.Data.ClientUse.RequestsMade != 3 ||
		server.totalRequests()-requestsBeforeRetire != 3 {
		t.Fatalf("unexpected retirement report: %s", out.String())
	}
	assertSourceRetirePrivate(t, out.Bytes(), fixture, server.server.URL)
	tableArgs := append([]string(nil), args...)
	for index := 0; index < len(tableArgs)-1; index++ {
		if tableArgs[index] == "--output" {
			tableArgs[index+1] = "table"
			break
		}
	}
	out.Reset()
	errOut.Reset()
	if code := Run(tableArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 ||
		!strings.Contains(out.String(), "DELETION AUTHORITY") || !strings.Contains(out.String(), "none") ||
		!strings.Contains(out.String(), "CURRENT CLIENT USE") {
		t.Fatalf("retire table code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	assertSourceRetirePrivate(t, out.Bytes(), fixture, server.server.URL)

	// Explicit path disclosure changes presentation only, not the reviewed plan
	// identity or any write/deletion count.
	shownArgs := append(append([]string(nil), args...), "--show-absolute-paths")
	out.Reset()
	errOut.Reset()
	if code := Run(shownArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("shown retire plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	shown := decodeSourceRetireReport(t, out.Bytes())
	if shown.Data.Plan.ID != report.Data.Plan.ID || shown.Data.Plan.SourceFiles[0].SourcePath != fixture.materialize.sourcePath {
		t.Fatalf("path opt-in changed authority or failed: %s", out.String())
	}

	// Selecting the published final itself is a complete positive conflict. The
	// report is still emitted first; --require-eligible converts it to exit 4.
	blockedArgs := sourceRetireBaseArgs(fixture, server.server.URL, activationPlan.Data.Operation.ID, activationPlan.Data.Plan.ID)
	for index := 0; index < len(blockedArgs)-1; index++ {
		if blockedArgs[index] == "--search-root" {
			blockedArgs[index+1] = fixture.materialize.targetRoot
			break
		}
	}
	blockedArgs = append(blockedArgs, "--require-eligible")
	out.Reset()
	errOut.Reset()
	blockedReader := &trackingReader{}
	requestsBeforeBlocked := server.totalRequests()
	if code := Run(blockedArgs, blockedReader, &out, &errOut); code != 4 {
		t.Fatalf("blocked retire plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	blocked := decodeSourceRetireReport(t, out.Bytes())
	if blocked.Data.Outcome != sourceretire.OutcomeBlocked || blocked.Data.WritesPerformed != 0 ||
		!sourceRetireFinding(blocked.Data.Blockers, "source.overlaps_final") || blockedReader.read ||
		server.totalRequests() != requestsBeforeBlocked {
		t.Fatalf("published final was not blocked: %s", out.String())
	}

	limitedArgs := append(append([]string(nil), args...), "--max-proof-bytes", "1", "--require-eligible")
	out.Reset()
	errOut.Reset()
	limitedReader := &trackingReader{}
	requestsBeforeLimited := server.totalRequests()
	if code := Run(limitedArgs, limitedReader, &out, &errOut); code != 4 {
		t.Fatalf("limited retire plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	limited := decodeSourceRetireReport(t, out.Bytes())
	if limited.Data.Outcome != sourceretire.OutcomeIncomplete || limited.Data.Scan.StopReasons == nil ||
		len(limited.Data.Scan.StopReasons) == 0 || limited.Data.WritesPerformed != 0 ||
		limited.Data.Activation.ObservedAtStart == "" || limited.Data.Final.OperationID == "" || limitedReader.read ||
		server.totalRequests() != requestsBeforeLimited {
		t.Fatalf("proof budget was not reported as incomplete: %s", out.String())
	}

	// A syntactically valid mapping that differs from the reviewed activation is
	// rejected before stdin or another client request.
	mismatchedMapping := append([]string(nil), args...)
	for index := 0; index < len(mismatchedMapping)-1; index++ {
		if mismatchedMapping[index] == "--client-root" {
			mismatchedMapping[index+1] = "/different-client-root"
			break
		}
	}
	mismatchReader := &trackingReader{}
	requestsBeforeMismatch := server.totalRequests()
	out.Reset()
	errOut.Reset()
	if code := Run(mismatchedMapping, mismatchReader, &out, &errOut); code != 4 || mismatchReader.read ||
		server.totalRequests() != requestsBeforeMismatch {
		t.Fatalf("mapping mismatch code=%d read=%t stdout=%q stderr=%q", code, mismatchReader.read, out.String(), errOut.String())
	}

	// A selected source under any ptctl control prefix is a local policy
	// conflict, so it must be reported before credential or client I/O.
	reservedSource := filepath.Join(fixture.materialize.sourceRoot, materialize.SourceRetireOperationDirectoryPrefix+"payload")
	if err := os.Rename(fixture.materialize.sourcePath, reservedSource); err != nil {
		t.Fatal(err)
	}
	reservedReader := &trackingReader{}
	requestsBeforeReserved := server.totalRequests()
	out.Reset()
	errOut.Reset()
	if code := Run(args, reservedReader, &out, &errOut); code != 0 || reservedReader.read || server.totalRequests() != requestsBeforeReserved {
		t.Fatalf("reserved source code=%d read=%t requests=%d stdout=%q stderr=%q", code, reservedReader.read,
			server.totalRequests()-requestsBeforeReserved, out.String(), errOut.String())
	}
	reserved := decodeSourceRetireReport(t, out.Bytes())
	if reserved.Data.Outcome != sourceretire.OutcomeBlocked || !sourceRetireFinding(reserved.Data.Blockers, "source.control_namespace_reserved") {
		t.Fatalf("reserved source was not blocked locally: %s", out.String())
	}
	if err := os.Rename(reservedSource, fixture.materialize.sourcePath); err != nil {
		t.Fatal(err)
	}

	// A transport failure is a complete, privacy-safe report with the adapter's
	// exact attempted-request count. It does not retry or become a fatal default
	// exit merely because --require-eligible was not requested.
	server.server.Close()
	out.Reset()
	errOut.Reset()
	const passwordCanary = "SOURCE-RETIRE-PASSWORD-CANARY"
	if code := Run(args, strings.NewReader(passwordCanary+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("transport failure code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	failed := decodeSourceRetireReport(t, out.Bytes())
	if failed.Data.Outcome != sourceretire.OutcomeIncomplete || failed.Data.ClientUse.Status != "incomplete" ||
		failed.Data.ClientUse.RequestsMade != 1 || !sourceRetireFinding(failed.Data.Issues, "client.current_use_incomplete") ||
		bytes.Contains(out.Bytes(), []byte(passwordCanary)) {
		t.Fatalf("unsafe transport failure report: %s", out.String())
	}
	assertSourceRetirePrivate(t, out.Bytes(), fixture, server.server.URL)
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
		!strings.Contains(out.String(), "deletion_authority") || !strings.Contains(out.String(), "zero writes") ||
		!strings.Contains(out.String(), "two bounded job-ledger reads") || !strings.Contains(out.String(), "retains an exact tombstone") {
		t.Fatalf("help code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	operation := "sha256:" + strings.Repeat("a", 64)
	plan := "sha256:" + strings.Repeat("b", 64)
	if code := Run([]string{"seed", "retire", "prune", "--target", "missing", "--expect-plan-id", plan, operation}, reader, &out, &errOut); code != 2 || reader.read {
		t.Fatalf("missing prune acknowledgement code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
}

func TestSeedRetireRunResumeAndStatusJournalExactDeletion(t *testing.T) {
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
	adoptionRun := append(append([]string{"client", "adopt", "run"}, baseAdopt...),
		"--expect-adoption-plan-id", adoptionPlan.Data.Plan.ID, "--acknowledge-client-add")
	if code := Run(adoptionRun, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("adoption run code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	adopted := decodeClientAdoptReport(t, out.Bytes())

	baseActivation := clientActivateBaseArgs(fixture, server.server.URL, adopted.Data.Operation.ID, adopted.Data.Plan.ID)
	out.Reset()
	errOut.Reset()
	if code := Run(append([]string{"client", "activate", "plan"}, baseActivation...), strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("activation plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	activationPlan := decodeClientActivateReport(t, out.Bytes())
	out.Reset()
	errOut.Reset()
	activationRun := append(append([]string{"client", "activate", "run"}, baseActivation...),
		"--expect-activation-plan-id", activationPlan.Data.Plan.ID, "--acknowledge-client-recheck")
	if code := Run(activationRun, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("activation run code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	server.setState("stoppedUP", 1)
	out.Reset()
	errOut.Reset()
	activationResume := append(append([]string{"client", "activate", "resume"}, baseActivation...),
		"--expect-activation-plan-id", activationPlan.Data.Plan.ID, activationPlan.Data.Operation.ID)
	if code := Run(activationResume, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("activation resume code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}

	planArgs := sourceRetireBaseArgs(fixture, server.server.URL, activationPlan.Data.Operation.ID, activationPlan.Data.Plan.ID)
	out.Reset()
	errOut.Reset()
	if code := Run(planArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("retire plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	plan := decodeSourceRetireReport(t, out.Bytes())

	missingAck := append([]string(nil), planArgs...)
	missingAck[2] = "run"
	missingAck = append(missingAck, "--expect-plan-id", plan.Data.Plan.ID)
	reader := &trackingReader{}
	out.Reset()
	errOut.Reset()
	requestsBefore := server.totalRequests()
	if code := Run(missingAck, reader, &out, &errOut); code != 2 || reader.read || server.totalRequests() != requestsBefore {
		t.Fatalf("missing ack code/read/requests=%d/%t/%d stdout=%q stderr=%q", code, reader.read, server.totalRequests()-requestsBefore, out.String(), errOut.String())
	}
	if _, err := os.Stat(fixture.materialize.sourcePath); err != nil {
		t.Fatalf("missing ack changed source: %v", err)
	}

	runArgs := append(append([]string(nil), missingAck...), "--acknowledge-source-deletion")
	out.Reset()
	errOut.Reset()
	requestsBefore = server.totalRequests()
	if code := Run(runArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("retire run code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	execution := decodeSourceRetireExecutionReport(t, out.Bytes())
	if execution.Schema != "ptctl.dev/v1" || execution.Kind != "content.source_retirement" ||
		execution.Data.Outcome != sourceretire.ExecutionOutcomeRetired || !execution.Data.DeletionPerformed ||
		execution.Data.Writes.NamesRemoved != 1 || execution.Data.Operation.Resumable ||
		server.totalRequests()-requestsBefore != 4 {
		t.Fatalf("unexpected execution report: %s", out.String())
	}
	if _, err := os.Lstat(fixture.materialize.sourcePath); !os.IsNotExist(err) {
		t.Fatalf("retired source remains: %v", err)
	}
	assertSourceRetirePrivate(t, out.Bytes(), fixture, server.server.URL)

	out.Reset()
	errOut.Reset()
	reader = &trackingReader{}
	requestsBefore = server.totalRequests()
	statusArgs := []string{"seed", "retire", "status", "--target", fixture.materialize.targetRoot, "--output", "json", execution.Data.Operation.ID}
	if code := Run(statusArgs, reader, &out, &errOut); code != 0 || reader.read || server.totalRequests() != requestsBefore {
		t.Fatalf("status code/read/requests=%d/%t/%d stdout=%q stderr=%q", code, reader.read, server.totalRequests()-requestsBefore, out.String(), errOut.String())
	}
	status := decodeSourceRetireExecutionReport(t, out.Bytes())
	if status.Data.Outcome != sourceretire.ExecutionOutcomeAlreadyRetired || status.Data.Operation.Status != "historical_complete" || status.Data.WritesPerformed != 0 {
		t.Fatalf("unexpected status: %s", out.String())
	}
	out.Reset()
	errOut.Reset()
	reader = &trackingReader{}
	requestsBefore = server.totalRequests()
	if code := Run(runArgs, reader, &out, &errOut); code != 0 || reader.read || server.totalRequests() != requestsBefore {
		t.Fatalf("terminal rerun code/read/requests=%d/%t/%d stdout=%q stderr=%q", code, reader.read, server.totalRequests()-requestsBefore, out.String(), errOut.String())
	}
	rerun := decodeSourceRetireExecutionReport(t, out.Bytes())
	if rerun.Data.Outcome != sourceretire.ExecutionOutcomeAlreadyRetired || rerun.Data.Operation.Status != "historical_complete" || rerun.Data.WritesPerformed != 0 {
		t.Fatalf("terminal rerun was not local and idempotent: %s", out.String())
	}

	resumeArgs := append([]string(nil), runArgs...)
	resumeArgs[2] = "resume"
	resumeArgs = append(resumeArgs, execution.Data.Operation.ID)
	mismatchedResume := append([]string(nil), resumeArgs...)
	for index := 0; index < len(mismatchedResume)-1; index++ {
		if mismatchedResume[index] == "--expect-plan-id" {
			mismatchedResume[index+1] = "sha256:" + strings.Repeat("a", 64)
			break
		}
	}
	out.Reset()
	errOut.Reset()
	reader = &trackingReader{}
	requestsBefore = server.totalRequests()
	if code := Run(mismatchedResume, reader, &out, &errOut); code != 4 || reader.read || server.totalRequests() != requestsBefore {
		t.Fatalf("mismatched resume code/read/requests=%d/%t/%d stdout=%q stderr=%q", code, reader.read, server.totalRequests()-requestsBefore, out.String(), errOut.String())
	}
	mismatch := decodeSourceRetireExecutionReport(t, out.Bytes())
	if mismatch.Data.Outcome != sourceretire.ExecutionOutcomeBlocked || mismatch.Data.WritesPerformed != 0 {
		t.Fatalf("mismatched resume was not blocked: %s", out.String())
	}
	out.Reset()
	errOut.Reset()
	reader = &trackingReader{}
	requestsBefore = server.totalRequests()
	if code := Run(resumeArgs, reader, &out, &errOut); code != 0 || reader.read || server.totalRequests() != requestsBefore {
		t.Fatalf("terminal resume code/read/requests=%d/%t/%d stdout=%q stderr=%q", code, reader.read, server.totalRequests()-requestsBefore, out.String(), errOut.String())
	}
	resumed := decodeSourceRetireExecutionReport(t, out.Bytes())
	if resumed.Data.Outcome != sourceretire.ExecutionOutcomeAlreadyRetired || resumed.Data.WritesPerformed != 0 {
		t.Fatalf("terminal resume was not idempotent: %s", out.String())
	}

	pruneArgs := []string{"seed", "retire", "prune", "--target", fixture.materialize.targetRoot,
		"--expect-plan-id", plan.Data.Plan.ID, "--acknowledge-operation-state-deletion", "--output", "json", execution.Data.Operation.ID}
	out.Reset()
	errOut.Reset()
	reader = &trackingReader{}
	requestsBefore = server.totalRequests()
	if code := Run(pruneArgs, reader, &out, &errOut); code != 0 || reader.read || server.totalRequests() != requestsBefore {
		t.Fatalf("prune code/read/requests=%d/%t/%d stdout=%q stderr=%q", code, reader.read, server.totalRequests()-requestsBefore, out.String(), errOut.String())
	}
	pruned := decodeSourceRetireRetentionReport(t, out.Bytes())
	if pruned.Schema != "ptctl.dev/v1" || pruned.Kind != "content.source_retirement.retention" ||
		pruned.Data.Outcome != sourceretire.ExecutionRetentionOutcomePruned || !pruned.Data.Markers.ExactTombstone ||
		pruned.Data.Writes.FilesRemoved == 0 || pruned.Data.WritesPerformed == 0 {
		t.Fatalf("unexpected prune report: %s", out.String())
	}
	assertSourceRetirePrivate(t, out.Bytes(), fixture, server.server.URL)

	out.Reset()
	errOut.Reset()
	if code := Run(statusArgs, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("retained status code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	retained := decodeSourceRetireExecutionReport(t, out.Bytes())
	if retained.Data.Outcome != sourceretire.ExecutionOutcomeAlreadyRetired || retained.Data.Operation.Status != "retained" || retained.Data.WritesPerformed != 0 {
		t.Fatalf("retained status disagrees: %s", out.String())
	}
	out.Reset()
	errOut.Reset()
	reader = &trackingReader{}
	requestsBefore = server.totalRequests()
	if code := Run(resumeArgs, reader, &out, &errOut); code != 0 || reader.read || server.totalRequests() != requestsBefore {
		t.Fatalf("retained resume code/read/requests=%d/%t/%d stdout=%q stderr=%q", code, reader.read, server.totalRequests()-requestsBefore, out.String(), errOut.String())
	}
	retainedResume := decodeSourceRetireExecutionReport(t, out.Bytes())
	if retainedResume.Data.Outcome != sourceretire.ExecutionOutcomeAlreadyRetired || retainedResume.Data.Operation.Status != "retained" || retainedResume.Data.WritesPerformed != 0 {
		t.Fatalf("retained resume disagrees: %s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run(pruneArgs, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("idempotent prune code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	repeated := decodeSourceRetireRetentionReport(t, out.Bytes())
	if repeated.Data.Outcome != sourceretire.ExecutionRetentionOutcomeAlreadyPruned || repeated.Data.WritesPerformed != 0 {
		t.Fatalf("idempotent prune changed state: %s", out.String())
	}
}

func sourceRetireBaseArgs(fixture clientAdoptCLIFixture, endpoint, activationOperation, activationPlanID string) []string {
	return []string{"seed", "retire", "plan", "--metafile-store", fixture.storeRoot, "--metafile-variant", fixture.variantID,
		"--search-root", fixture.materialize.sourceRoot, "--target", fixture.materialize.targetRoot,
		"--materialize-operation", fixture.operation, "--materialize-plan-id", fixture.materialize.planID,
		"--activation-operation", activationOperation, "--activation-plan-id", activationPlanID,
		"--host-root", fixture.materialize.targetRoot, "--client-root", clientAdoptRoot, "--client-style", "posix",
		"--driver", "qbittorrent", "--url", endpoint, "--username", clientAdoptUser, "--password-stdin",
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

func decodeSourceRetireExecutionReport(t *testing.T, raw []byte) sourceRetireExecutionJSONEnvelope {
	t.Helper()
	var result sourceRetireExecutionJSONEnvelope
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode source retirement execution report: %v\n%s", err, raw)
	}
	return result
}

func decodeSourceRetireRetentionReport(t *testing.T, raw []byte) sourceRetireRetentionJSONEnvelope {
	t.Helper()
	var result sourceRetireRetentionJSONEnvelope
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode source retirement retention report: %v\n%s", err, raw)
	}
	return result
}

func assertSourceRetirePrivate(t *testing.T, raw []byte, fixture clientAdoptCLIFixture, endpoint string) {
	t.Helper()
	for _, secret := range []string{fixture.materialize.sourceRoot, fixture.materialize.sourcePath,
		fixture.materialize.targetRoot, fixture.materialize.finalPath, materializeSourceName,
		endpoint, clientAdoptRoot, clientAdoptUser, clientAdoptPassword, clientAdoptJobKey} {
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
