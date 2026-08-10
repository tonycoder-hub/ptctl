package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

const (
	materializeFinalName  = "PTCTL-MATERIALIZE-FINAL-CANARY.bin"
	materializeSourceName = "PTCTL-MATERIALIZE-RAW-SOURCE-CANARY.bin"
)

type materializeCLIFixture struct {
	torrentPath string
	sourceRoot  string
	sourcePath  string
	targetRoot  string
	finalPath   string
	planID      string
	content     []byte
}

type materializeJSONEnvelope struct {
	Schema string             `json:"schema"`
	Kind   string             `json:"kind"`
	Data   materialize.Report `json:"data"`
}

func TestSeedMaterializeRunStatusResumeAndPrivacy(t *testing.T) {
	fixture := newMaterializeCLIFixture(t)
	storeRoot, variantID := storeMaterializeMetafile(t, fixture.torrentPath)

	runArgs := []string{
		"seed", "materialize", "run",
		"--metafile-store", storeRoot, "--metafile-variant", variantID,
		"--search-root", fixture.sourceRoot, "--target", fixture.targetRoot,
		"--expect-plan-id", fixture.planID, "--acknowledge-filesystem-write", "--output", "json",
	}
	var out, errOut bytes.Buffer
	if code := Run(runArgs, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("run code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	report := decodeMaterializeReport(t, out.Bytes())
	if report.Data.Outcome != materialize.OutcomeMaterializedVerified || report.Data.WritesPerformed == 0 ||
		report.Data.WritesUncertain || !report.Data.Plan.Matches || report.Data.Plan.ObservedID != fixture.planID ||
		report.Data.Source.Outcome != "verified_unique" || !report.Data.Source.ContentVerified ||
		!report.Data.Target.FinalContentVerified || !report.Data.Target.DurabilityConfirmed {
		t.Fatalf("unexpected run report: %s", out.String())
	}
	if _, err := materialize.ParseOperationID(report.Data.Operation.ID); err != nil || report.Data.Operation.Resumable {
		t.Fatalf("invalid terminal operation handoff: %+v", report.Data.Operation)
	}
	if !reflect.DeepEqual(report.Data.Limits, materialize.DefaultLimits()) {
		t.Fatalf("CLI did not use fixed materialize defaults: %+v", report.Data.Limits)
	}
	assertMaterializeArrays(t, report.Data)
	assertMaterializeJSONPrivate(t, out.Bytes(), fixture, storeRoot)
	materialized, err := os.ReadFile(fixture.finalPath)
	if err != nil || !bytes.Equal(materialized, fixture.content) {
		t.Fatalf("published content mismatch: bytes=%q err=%v", materialized, err)
	}

	operationID := report.Data.Operation.ID
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"seed", "materialize", "status", "--target", fixture.targetRoot, "--output", "json", operationID}, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("status code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	status := decodeMaterializeReport(t, out.Bytes())
	if status.Data.Outcome != materialize.OutcomeAlreadyCommitted || status.Data.Operation.ID != operationID || status.Data.Operation.Resumable {
		t.Fatalf("unexpected exact status: %s", out.String())
	}
	assertMaterializeArrays(t, status.Data)
	assertMaterializeJSONPrivate(t, out.Bytes(), fixture, storeRoot)

	missingMetafile := filepath.Join(physicalCLITempDir(t), "PTCTL-MATERIALIZE-MISSING-SELECTOR-CANARY.torrent")
	out.Reset()
	errOut.Reset()
	if code := Run([]string{
		"seed", "materialize", "resume", "--torrent", missingMetafile,
		"--target", fixture.targetRoot, "--expect-plan-id", fixture.planID,
		"--acknowledge-filesystem-write", "--output", "json", operationID,
	}, strings.NewReader(""), &out, &errOut); code != 1 {
		t.Fatalf("missing-selector resume code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	inputFailure := decodeMaterializeReport(t, out.Bytes())
	if inputFailure.Data.Outcome != materialize.OutcomeInterrupted || inputFailure.Data.Operation.ID != operationID ||
		len(inputFailure.Data.Issues) == 0 || inputFailure.Data.Issues[len(inputFailure.Data.Issues)-1].Code != "metafile.read_failed" {
		t.Fatalf("resume input failure was not report-first: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), missingMetafile, fixture.targetRoot, fixture.sourceRoot, fixture.sourcePath)
	if strings.Contains(errOut.String(), missingMetafile) || strings.Contains(errOut.String(), fixture.targetRoot) {
		t.Fatalf("resume input failure leaked a path: %q", errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"seed", "materialize", "status", "--target", fixture.targetRoot, "--output", "json"}, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("list code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var listEnvelope struct {
		Schema string                          `json:"schema"`
		Kind   string                          `json:"kind"`
		Data   materialize.OperationListResult `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &listEnvelope); err != nil {
		t.Fatal(err)
	}
	if listEnvelope.Schema != "ptctl.dev/v1" || listEnvelope.Kind != "content.materialization.operation_list" ||
		!listEnvelope.Data.Complete || listEnvelope.Data.Writes != 0 || len(listEnvelope.Data.Operations) != 1 ||
		listEnvelope.Data.Operations[0].ID.String() != operationID || listEnvelope.Data.Operations[0].Status != "not_inspected" ||
		listEnvelope.Data.Warnings == nil {
		t.Fatalf("unexpected bounded operation list: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), fixture.targetRoot, fixture.sourceRoot, fixture.sourcePath, fixture.torrentPath, storeRoot)

	out.Reset()
	errOut.Reset()
	latePhaseRoot := filepath.Join(physicalCLITempDir(t), "PTCTL-MATERIALIZE-LATE-ROOT-MUST-NOT-BE-READ")
	resumeArgs := []string{
		"seed", "materialize", "resume",
		"--metafile-store", storeRoot, "--metafile-variant", variantID,
		"--target", fixture.targetRoot, "--expect-plan-id", fixture.planID,
		"--acknowledge-filesystem-write", "--search-root", latePhaseRoot, "--output", "json", operationID,
	}
	if code := Run(resumeArgs, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("resume code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	resumed := decodeMaterializeReport(t, out.Bytes())
	if resumed.Data.Outcome != materialize.OutcomeAlreadyCommitted || resumed.Data.Source.Mode != "not_required_after_staging" ||
		!resumed.Data.Target.FinalContentVerified || len(resumed.Data.Warnings) == 0 ||
		!strings.Contains(resumed.Data.Warnings[len(resumed.Data.Warnings)-1], "were not read") {
		t.Fatalf("unexpected committed resume: %s", out.String())
	}
	assertMaterializeArrays(t, resumed.Data)
	assertMaterializeJSONPrivate(t, out.Bytes(), fixture, storeRoot, latePhaseRoot)

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"seed", "materialize", "abandon", "--target", fixture.targetRoot, "--acknowledge-abandon", "--output", "json", operationID}, strings.NewReader(""), &out, &errOut); code != 4 {
		t.Fatalf("committed abandon code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	abandon := decodeMaterializeReport(t, out.Bytes())
	if abandon.Data.Outcome != materialize.OutcomeBlocked || len(abandon.Data.Blockers) == 0 || !strings.Contains(errOut.String(), "blocked") {
		t.Fatalf("committed abandon did not fail closed: stdout=%q stderr=%q", out.String(), errOut.String())
	}
	assertMaterializeJSONPrivate(t, out.Bytes(), fixture, storeRoot)
}

func TestSeedMaterializePlanMismatchIsReportFirstAndZeroWrite(t *testing.T) {
	fixture := newMaterializeCLIFixture(t)
	meta, err := metafile.Read(fixture.torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	standalonePlan, err := seed.BuildMaterializePlan(context.Background(), meta, fixture.sourcePath, fixture.targetRoot, materialize.StrategyCopy)
	if err != nil {
		t.Fatalf("build standalone preview plan: %v", err)
	}
	if standalonePlan.Readiness != "layout_only" || standalonePlan.ReadyToApply || standalonePlan.Effect != "none" || standalonePlan.ID == fixture.planID {
		t.Fatalf("standalone preview must remain distinct from discovered-map review: %+v", standalonePlan)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{
		"seed", "materialize", "run", "--torrent", fixture.torrentPath,
		"--search-root", fixture.sourceRoot, "--target", fixture.targetRoot,
		"--expect-plan-id", standalonePlan.ID, "--acknowledge-filesystem-write",
	}, strings.NewReader(""), &out, &errOut)
	if code != 4 || !strings.Contains(out.String(), "OUTCOME") || !strings.Contains(out.String(), "blocked") ||
		!strings.Contains(out.String(), "WRITES PERFORMED") || !strings.Contains(out.String(), "plan.id_mismatch") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	for _, later := range []string{"PLAN\n", "SOURCE\n", "TARGET\n", "WRITE BREAKDOWN", "LIMITS / USED", "ISSUES"} {
		if strings.Index(out.String(), "BLOCKERS") < 0 || strings.Index(out.String(), later) < strings.Index(out.String(), "BLOCKERS") {
			t.Fatalf("blockers must precede evidence section %q: %q", later, out.String())
		}
	}
	for _, budget := range []string{"MAX NAMESPACE OBJECTS", "MAX SCRATCH BYTES", "FINDING OVERFLOW"} {
		if !strings.Contains(out.String(), budget) {
			t.Fatalf("human budget output is missing %q: %q", budget, out.String())
		}
	}
	if strings.Contains(out.String()+errOut.String(), fixture.torrentPath) || strings.Contains(out.String()+errOut.String(), fixture.sourcePath) ||
		strings.Contains(out.String()+errOut.String(), fixture.sourceRoot) || strings.Contains(out.String()+errOut.String(), fixture.targetRoot) {
		t.Fatalf("materialize table/error leaked a path: stdout=%q stderr=%q", out.String(), errOut.String())
	}
	if _, err := os.Stat(fixture.finalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plan mismatch made the final layout visible: %v", err)
	}
}

func TestSeedMaterializeStatusMissingIsReportFirst(t *testing.T) {
	targetRoot := materializeTargetRoot(t)
	var emptyOut, emptyErr bytes.Buffer
	if code := Run([]string{"seed", "materialize", "status", "--target", targetRoot, "--output", "json"}, strings.NewReader(""), &emptyOut, &emptyErr); code != 0 || emptyErr.Len() != 0 {
		t.Fatalf("empty list code=%d stdout=%q stderr=%q", code, emptyOut.String(), emptyErr.String())
	}
	var emptyList struct {
		Kind string `json:"kind"`
		Data struct {
			Complete   bool                           `json:"complete"`
			Operations []materialize.OperationSummary `json:"operations"`
			Warnings   []string                       `json:"warnings"`
		} `json:"data"`
	}
	if err := json.Unmarshal(emptyOut.Bytes(), &emptyList); err != nil {
		t.Fatal(err)
	}
	if emptyList.Kind != "content.materialization.operation_list" || !emptyList.Data.Complete ||
		emptyList.Data.Operations == nil || len(emptyList.Data.Operations) != 0 || emptyList.Data.Warnings == nil {
		t.Fatalf("empty operation arrays must be explicit: %s", emptyOut.String())
	}
	assertJSONStringsExclude(t, emptyOut.Bytes(), targetRoot)

	missingID := "sha256:" + strings.Repeat("0", 64)
	var out, errOut bytes.Buffer
	if code := Run([]string{"seed", "materialize", "status", "--target", targetRoot, "--output", "json", missingID}, strings.NewReader(""), &out, &errOut); code != 4 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	report := decodeMaterializeReport(t, out.Bytes())
	if report.Data.Outcome != materialize.OutcomeInterrupted || report.Data.Operation.Status != "not_found" ||
		len(report.Data.Issues) != 1 || report.Data.Issues[0].Code != "operation.not_found" {
		t.Fatalf("unexpected missing-operation report: %s", out.String())
	}
	assertMaterializeArrays(t, report.Data)
	assertJSONStringsExclude(t, out.Bytes(), targetRoot)
}

func TestSeedMaterializeInputFailureIsReportFirstAndPrivate(t *testing.T) {
	missingRoot := physicalCLITempDir(t)
	missingMetafile := filepath.Join(missingRoot, "PTCTL-MATERIALIZE-MISSING-METAFILE-CANARY.torrent")
	missingSource := filepath.Join(missingRoot, "PTCTL-MATERIALIZE-MISSING-SOURCE-CANARY")
	targetRoot := materializeTargetRoot(t)
	var out, errOut bytes.Buffer
	code := Run([]string{
		"seed", "materialize", "run", "--torrent", missingMetafile,
		"--search-root", missingSource, "--target", targetRoot,
		"--expect-plan-id", strings.Repeat("4", 24), "--acknowledge-filesystem-write", "--output", "json",
	}, strings.NewReader(""), &out, &errOut)
	if code != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	report := decodeMaterializeReport(t, out.Bytes())
	if report.Data.Outcome != materialize.OutcomeInterrupted || report.Data.WritesPerformed != 0 ||
		report.Data.Operation.Status != "not_created" || len(report.Data.Blockers) != 0 ||
		len(report.Data.Issues) != 1 || report.Data.Issues[0].Code != "metafile.read_failed" {
		t.Fatalf("unexpected input failure report: %s", out.String())
	}
	assertMaterializeArrays(t, report.Data)
	assertJSONStringsExclude(t, out.Bytes(), missingRoot, missingMetafile, missingSource, targetRoot)
	if strings.Contains(errOut.String(), missingRoot) || strings.Contains(errOut.String(), targetRoot) {
		t.Fatalf("input failure leaked a path: %q", errOut.String())
	}
}

func TestSeedMaterializeUsagePrecedesFilesystemAndSecretInput(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "PTCTL-MATERIALIZE-MUST-NOT-BE-READ")
	validOperationID := "sha256:" + strings.Repeat("1", 64)
	validVariantID := "sha256:" + strings.Repeat("2", 64)
	validPlanID := strings.Repeat("3", 24)
	tests := [][]string{
		{"seed", "materialize", "run", "--torrent", missing, "--search-root", missing, "--target", missing, "--expect-plan-id", validPlanID},
		{"seed", "materialize", "run", "--torrent", missing, "--search-root", missing, "--target", missing, "--expect-plan-id", "BAD", "--acknowledge-filesystem-write"},
		{"seed", "materialize", "run", "--torrent", missing, "--metafile-store", missing, "--metafile-variant", validVariantID, "--search-root", missing, "--target", missing, "--expect-plan-id", validPlanID, "--acknowledge-filesystem-write"},
		{"seed", "materialize", "run", "--torrent", missing, "--search-root", "", "--target", missing, "--expect-plan-id", validPlanID, "--acknowledge-filesystem-write"},
		{"seed", "materialize", "resume", "--torrent", missing, "--target", missing, "--expect-plan-id", validPlanID, "--acknowledge-filesystem-write", "--max-depth", "1", validOperationID},
		{"seed", "materialize", "status", "--target", missing, "--max-operations", "1", validOperationID},
		{"seed", "materialize", "abandon", "--target", missing, validOperationID},
	}
	for _, args := range tests {
		reader := &trackingReader{}
		var out, errOut bytes.Buffer
		if code := Run(args, reader, &out, &errOut); code != 2 {
			t.Fatalf("args=%v code=%d stdout=%q stderr=%q", args, code, out.String(), errOut.String())
		}
		if reader.read {
			t.Fatalf("rejected command read stdin: %v", args)
		}
		if strings.Contains(out.String()+errOut.String(), missing) {
			t.Fatalf("rejected command leaked a private path: args=%v stdout=%q stderr=%q", args, out.String(), errOut.String())
		}
	}
}

func TestSeedMaterializeHelpIsCompleteAndFixedBudget(t *testing.T) {
	commands := []struct {
		args     []string
		required []string
	}{
		{[]string{"seed", "materialize", "--help"}, []string{"run", "resume", "status", "abandon", "standalone seed plan ID is not an", "never selects a latest operation"}},
		{[]string{"seed", "materialize", "run", "--help"}, []string{"acknowledge-filesystem-write", "metafile-store", "search-root", "max-proof-bytes", "standalone seed plan ID is not accepted"}},
		{[]string{"seed", "materialize", "resume", "--help"}, []string{"OPERATION_ID", "partially staged", "search-root"}},
		{[]string{"seed", "materialize", "status", "--help"}, []string{"max-root-entries", "max-operations", "not_inspected"}},
		{[]string{"seed", "materialize", "abandon", "--help"}, []string{"acknowledge-abandon", "retains staged and scratch bytes", "no deletion"}},
	}
	for _, command := range commands {
		var out, errOut bytes.Buffer
		if code := Run(command.args, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
			t.Fatalf("args=%v code=%d stdout=%q stderr=%q", command.args, code, out.String(), errOut.String())
		}
		for _, required := range command.required {
			if !strings.Contains(out.String(), required) {
				t.Fatalf("help %v is missing %q: %q", command.args, required, out.String())
			}
		}
		if command.args[2] == "run" || command.args[2] == "resume" {
			for _, forbidden := range []string{"max-files", "copy-buffer-bytes", "max-journal-events", "max-scratch-entries"} {
				if strings.Contains(out.String(), forbidden) {
					t.Fatalf("fixed materialize limit leaked as a flag %q: %q", forbidden, out.String())
				}
			}
		}
	}
}

func newMaterializeCLIFixture(t *testing.T) materializeCLIFixture {
	t.Helper()
	content := []byte("small exact materialize content")
	torrentPath := filepath.Join(physicalCLITempDir(t), "PTCTL-MATERIALIZE-INPUT-PATH-CANARY.torrent")
	if err := os.WriteFile(torrentPath, testV1Metafile(materializeFinalName, content), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceRoot := physicalCLITempDir(t)
	sourcePath := filepath.Join(sourceRoot, materializeSourceName)
	if err := os.WriteFile(sourcePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	targetRoot := materializeTargetRoot(t)

	meta, err := metafile.Read(torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	discovery, err := seed.Discover(ctx, meta, seed.DiscoverOptions{
		SearchRoots: []string{sourceRoot}, InventoryLimits: storage.DefaultInventoryLimits(),
		MatchLimits: metafile.DefaultSourceMatchLimits(), TimeBudget: time.Minute,
		TargetRoot: targetRoot, Strategy: materialize.StrategyCopy,
	})
	if err != nil {
		t.Fatalf("prepare reviewed plan: %v", err)
	}
	if discovery.SourceOutcome != "verified_unique" || discovery.Plan == nil || discovery.Plan.ID == "" {
		t.Fatalf("fixture did not produce a reviewed plan: %+v", discovery)
	}
	return materializeCLIFixture{
		torrentPath: torrentPath, sourceRoot: sourceRoot, sourcePath: sourcePath,
		targetRoot: targetRoot, finalPath: filepath.Join(targetRoot, materializeFinalName),
		planID: discovery.Plan.ID, content: content,
	}
}

func materializeTargetRoot(t *testing.T) string {
	t.Helper()
	targetRoot := physicalCLITempDir(t)
	session, _, err := fsbind.BindExisting(targetRoot)
	if errors.Is(err, fsbind.ErrUnsupported) {
		t.Skipf("materialize filesystem binding is unsupported: %v", err)
	}
	if err != nil {
		t.Fatalf("bind target root: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close target preflight: %v", err)
	}
	return targetRoot
}

func storeMaterializeMetafile(t *testing.T, sourcePath string) (string, string) {
	t.Helper()
	storeRoot := filepath.Join(physicalCLITempDir(t), "PTCTL-MATERIALIZE-STORE-PATH-CANARY")
	store, _, err := metastore.Init(storeRoot)
	if err != nil {
		t.Fatalf("initialize fixture store: %v", err)
	}
	_, ref, _, err := store.ImportFile(context.Background(), sourcePath, metastore.DefaultLimits())
	if err != nil {
		t.Fatalf("import fixture metafile: %v", err)
	}
	return storeRoot, ref.ID.String()
}

func decodeMaterializeReport(t *testing.T, raw []byte) materializeJSONEnvelope {
	t.Helper()
	var envelope materializeJSONEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Schema != "ptctl.dev/v1" || envelope.Kind != "content.materialization" {
		t.Fatalf("unexpected materialize envelope: %s", raw)
	}
	return envelope
}

func assertMaterializeArrays(t *testing.T, report materialize.Report) {
	t.Helper()
	if report.Effect == nil || report.Blockers == nil || report.Issues == nil || report.Warnings == nil {
		t.Fatalf("materialize arrays must never be null: %+v", report)
	}
}

func assertMaterializeJSONPrivate(t *testing.T, raw []byte, fixture materializeCLIFixture, extra ...string) {
	t.Helper()
	secrets := []string{
		fixture.torrentPath, fixture.sourceRoot, fixture.sourcePath, fixture.targetRoot,
		materializeSourceName, base64.StdEncoding.EncodeToString([]byte(materializeSourceName)),
	}
	secrets = append(secrets, extra...)
	assertJSONStringsExclude(t, raw, secrets...)
}
