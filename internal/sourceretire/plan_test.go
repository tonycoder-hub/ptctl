package sourceretire

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

type retireFixture struct {
	meta             *metafile.MetaInfo
	discovery        seed.DiscoveryResult
	final            *materialize.VerifiedFinal
	activation       *clientactivate.VerifiedCompletion
	sourceRoot       string
	sourcePath       string
	targetRoot       string
	activationPlanID string
}

type retireAdoptionSession struct {
	requests int
	ledgers  []downloader.LedgerSnapshot
}

func (session *retireAdoptionSession) ReadLedger(context.Context) (downloader.LedgerSnapshot, error) {
	session.requests++
	if len(session.ledgers) == 0 {
		return downloader.LedgerSnapshot{}, fmt.Errorf("ledger unavailable")
	}
	value := session.ledgers[0]
	session.ledgers = session.ledgers[1:]
	return value, nil
}
func (*retireAdoptionSession) ReadJobFiles(context.Context, string, downloader.JobFileLedgerLimits) (downloader.JobFileLedgerSnapshot, error) {
	return downloader.JobFileLedgerSnapshot{}, fmt.Errorf("not supported")
}
func (session *retireAdoptionSession) AddStopped(_ context.Context, request downloader.AddStoppedRequest) (downloader.MutationReceipt, error) {
	session.requests++
	reader, err := request.Metafile.Open()
	if err != nil {
		return downloader.MutationReceipt{}, err
	}
	raw, err := io.ReadAll(reader)
	if err != nil || int64(len(raw)) != request.Metafile.SizeBytes() {
		return downloader.MutationReceipt{}, fmt.Errorf("payload unavailable")
	}
	now := time.Now().UTC()
	return downloader.MutationReceipt{Effect: "submit_exact_metafile_stopped", ObservedAtStart: now,
		ObservedAtEnd: now.Add(time.Millisecond), Complete: true, RequestsAttempted: 1,
		BytesSubmitted: int64(len(raw)), BytesSubmittedKnown: true}, nil
}
func (session *retireAdoptionSession) RequestsMade() int { return session.requests }
func (*retireAdoptionSession) Close() error              { return nil }

type retireActivationSession struct {
	requests   int
	descriptor downloader.ExistingJobControlDescriptor
	ledgers    []downloader.LedgerSnapshot
	rechecks   int
}

func (session *retireActivationSession) ReadExistingJobControlDescriptor(context.Context) (downloader.ExistingJobControlDescriptor, error) {
	session.requests++
	return session.descriptor, nil
}
func (session *retireActivationSession) ReadLedger(context.Context) (downloader.LedgerSnapshot, error) {
	session.requests++
	if len(session.ledgers) == 0 {
		return downloader.LedgerSnapshot{}, fmt.Errorf("ledger unavailable")
	}
	value := session.ledgers[0]
	session.ledgers = session.ledgers[1:]
	return value, nil
}
func (*retireActivationSession) ReadJobFiles(context.Context, string, downloader.JobFileLedgerLimits) (downloader.JobFileLedgerSnapshot, error) {
	return downloader.JobFileLedgerSnapshot{}, fmt.Errorf("not supported")
}
func (session *retireActivationSession) Recheck(_ context.Context, request downloader.ExistingJobMutationRequest) (downloader.ExistingJobMutationReceipt, error) {
	session.requests++
	session.rechecks++
	now := time.Now().UTC()
	return downloader.ExistingJobMutationReceipt{Effect: downloader.ControlEffectRecheck,
		ObservedAtStart: now, ObservedAtEnd: now.Add(time.Millisecond), Complete: true,
		RequestsAttempted: 1, RequestBytesKnown: true,
		RequestBytes: int64(len(url.Values{"hashes": {request.JobKey}}.Encode()))}, nil
}
func (*retireActivationSession) Start(context.Context, downloader.ExistingJobMutationRequest) (downloader.ExistingJobMutationReceipt, error) {
	return downloader.ExistingJobMutationReceipt{}, fmt.Errorf("start not reviewed")
}
func (session *retireActivationSession) RequestsMade() int { return session.requests }
func (*retireActivationSession) Close() error              { return nil }

func TestBuildProducesZeroWriteEligibilityWithoutPathDisclosure(t *testing.T) {
	fixture := makeRetireFixture(t)
	report, err := Build(context.Background(), BuildOptions{Meta: fixture.meta, Discovery: &fixture.discovery,
		Final: fixture.final, Activation: fixture.activation})
	if err != nil || report.Outcome != OutcomeEligible || report.WritesPerformed != 0 || report.DeletionPerformed ||
		report.Plan.DeletionAuthority != "none" || report.Plan.ID == "" || report.Plan.Validate() != nil ||
		len(report.Plan.SourceFiles) != 1 || report.Plan.SourceFiles[0].SourcePath != "" || !report.Plan.SourceFiles[0].DistinctObject ||
		!report.Scan.Complete || !report.Scan.VerificationComplete || report.Scan.StopReasons == nil {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	if _, err := os.Stat(fixture.sourcePath); err != nil {
		t.Fatalf("read-only plan changed its source: %v", err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(fixture.sourcePath)) || bytes.Contains(raw, []byte(filepath.Base(fixture.sourcePath))) {
		t.Fatalf("default report leaked source path: %s", raw)
	}
	shown, err := Build(context.Background(), BuildOptions{Meta: fixture.meta, Discovery: &fixture.discovery,
		Final: fixture.final, Activation: fixture.activation, ShowAbsolutePaths: true})
	if err != nil || shown.Plan.ID != report.Plan.ID || shown.Plan.SourceFiles[0].SourcePath != fixture.sourcePath {
		t.Fatalf("path opt-in changed authority or failed: %#v err=%v", shown, err)
	}
	if err := shown.Plan.Validate(); err != nil {
		t.Fatalf("path-disclosed plan did not validate: %v", err)
	}
	mutated := report.Plan
	mutated.SourceFiles = append([]SourceFile(nil), report.Plan.SourceFiles...)
	mutated.SourceFiles[0].SizeBytes++
	if err := mutated.Validate(); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("mutated plan retained a valid ID: %v", err)
	}
	invalidIndex := report.Plan
	invalidIndex.SourceFiles = append([]SourceFile(nil), report.Plan.SourceFiles...)
	invalidIndex.SourceFiles[0].ManifestIndex = invalidIndex.ManifestFiles
	invalidIndex.ID, err = planID(invalidIndex)
	if err != nil {
		t.Fatal(err)
	}
	if err := invalidIndex.Validate(); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("out-of-range manifest index validated: %v", err)
	}
}

func TestBuildRejectsDetachedDiscoveryAndFinalOverlap(t *testing.T) {
	fixture := makeRetireFixture(t)
	mutatedMeta := fixture.meta.Clone()
	mutatedMeta.InfoHashV1 = strings.Repeat("f", 40)
	report, err := Build(context.Background(), BuildOptions{Meta: mutatedMeta, Discovery: &fixture.discovery,
		Final: fixture.final, Activation: fixture.activation})
	if err != nil || report.Outcome != OutcomeBlocked || !hasFinding(report.Blockers, "metafile.authority_mismatch") {
		t.Fatalf("mutated metafile passed: report=%#v err=%v", report, err)
	}

	incomplete := fixture.discovery
	incomplete.Scan.Complete = false
	report, err = Build(context.Background(), BuildOptions{Meta: fixture.meta, Discovery: &incomplete,
		Final: fixture.final, Activation: fixture.activation})
	if err != nil || report.Outcome != OutcomeIncomplete || !hasFinding(report.Blockers, "source.discovery_incomplete") {
		t.Fatalf("mutated incomplete scan passed: report=%#v err=%v", report, err)
	}
	invalidSelection := fixture.discovery
	invalidSelection.Selection.Status = "blocked"
	report, err = Build(context.Background(), BuildOptions{Meta: fixture.meta, Discovery: &invalidSelection,
		Final: fixture.final, Activation: fixture.activation})
	if err != nil || report.Outcome != OutcomeBlocked || !hasFinding(report.Blockers, "source.selection_invalid") {
		t.Fatalf("mutated selection passed: report=%#v err=%v", report, err)
	}

	public := fixture.discovery.PublicReportCopy()
	detached, err := Build(context.Background(), BuildOptions{Meta: fixture.meta, Discovery: &public,
		Final: fixture.final, Activation: fixture.activation})
	if err != nil || detached.Outcome != OutcomeBlocked || !hasFinding(detached.Blockers, "source.process_authority_unavailable") {
		t.Fatalf("detached=%#v err=%v", detached, err)
	}

	finalDiscovery, err := seed.Discover(context.Background(), fixture.meta, seed.DiscoverOptions{
		SearchRoots: []string{fixture.targetRoot}, InventoryLimits: storage.DefaultInventoryLimits(),
		MatchLimits: metafile.DefaultSourceMatchLimits(), TimeBudget: time.Minute, Strategy: materialize.StrategyCopy,
	})
	if err != nil || finalDiscovery.SourceOutcome != "verified_unique" {
		t.Fatalf("final discovery=%#v err=%v", finalDiscovery, err)
	}
	overlap, err := Build(context.Background(), BuildOptions{Meta: fixture.meta, Discovery: &finalDiscovery,
		Final: fixture.final, Activation: fixture.activation})
	if err != nil || overlap.Outcome != OutcomeBlocked || !hasFinding(overlap.Blockers, "source.overlaps_final") {
		t.Fatalf("overlap=%#v err=%v", overlap, err)
	}
}

func TestBuildFailsClosedWhenSourceChangesAfterDiscovery(t *testing.T) {
	fixture := makeRetireFixture(t)
	if err := os.WriteFile(fixture.sourcePath, bytes.Repeat([]byte("x"), int(fixture.meta.TotalLength)), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Build(context.Background(), BuildOptions{Meta: fixture.meta, Discovery: &fixture.discovery,
		Final: fixture.final, Activation: fixture.activation})
	if !errors.Is(err, ErrIntegrity) || report.Outcome != OutcomeIntegrityFailed || report.WritesPerformed != 0 {
		t.Fatalf("report=%#v err=%v", report, err)
	}
}

func TestBuildRehashesSourceWhenSizeAndTimestampAreRestored(t *testing.T) {
	fixture := makeRetireFixture(t)
	before, err := os.Stat(fixture.sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.sourcePath, bytes.Repeat([]byte("z"), int(fixture.meta.TotalLength)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(fixture.sourcePath, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	restored, err := os.Stat(fixture.sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Size() != before.Size() || !restored.ModTime().Equal(before.ModTime()) {
		t.Skip("test filesystem cannot restore the source timestamp exactly")
	}
	report, err := Build(context.Background(), BuildOptions{Meta: fixture.meta, Discovery: &fixture.discovery,
		Final: fixture.final, Activation: fixture.activation})
	if !errors.Is(err, ErrIntegrity) || report.Outcome != OutcomeIntegrityFailed ||
		!hasFinding(report.Blockers, "integrity.proof_changed") {
		t.Fatalf("metadata-preserving byte change passed: report=%#v err=%v", report, err)
	}
}

func makeRetireFixture(t *testing.T) retireFixture {
	t.Helper()
	ctx := context.Background()
	content := []byte("retirement source fixture")
	raw := retireV1Metafile("retired-final.bin", content)
	meta, err := metafile.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	sourcePath := filepath.Join(sourceRoot, "PRIVATE-SOURCE-PATH-CANARY.bin")
	if err := os.WriteFile(sourcePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	if session, _, err := fsbind.BindExisting(targetRoot); errors.Is(err, fsbind.ErrUnsupported) {
		t.Skipf("materialize filesystem binding is unsupported: %v", err)
	} else if err != nil {
		t.Fatal(err)
	} else {
		_ = session.Close()
	}
	discovery, err := seed.Discover(ctx, meta, seed.DiscoverOptions{SearchRoots: []string{sourceRoot},
		InventoryLimits: storage.DefaultInventoryLimits(), MatchLimits: metafile.DefaultSourceMatchLimits(),
		TimeBudget: time.Minute, TargetRoot: targetRoot, Strategy: materialize.StrategyCopy})
	if err != nil || discovery.Plan == nil || discovery.SourceOutcome != "verified_unique" {
		t.Fatalf("discovery=%#v err=%v", discovery, err)
	}
	materialized, err := materialize.Run(ctx, materialize.RunOptions{Meta: meta, Discovery: &discovery,
		TargetRoot: targetRoot, ExpectedPlanID: discovery.Plan.ID, Limits: materialize.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	materializeOperation, err := materialize.ParseOperationID(materialized.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	final, _, err := materialize.VerifyCurrentFinal(ctx, materialize.FinalProofOptions{Meta: meta, TargetRoot: targetRoot,
		OperationID: materializeOperation, ExpectedPlanID: discovery.Plan.ID, Limits: materialize.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	hostRoot, err := filepath.EvalSymlinks(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig := "sha256:" + strings.Repeat("c", 64)
	projection, err := final.ProjectClientPaths(hostRoot, "/downloads", false)
	if err != nil {
		t.Fatal(err)
	}
	savePath, _ := projection.SavePath()
	contentPath, _ := projection.ContentPath()
	adoptionPlan, err := clientadopt.BuildPlan(final, clientadopt.PlanOptions{ClientConfigID: clientConfig,
		HostRoot: hostRoot, ClientRoot: "/downloads", ClientWindows: false})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job := downloader.Torrent{Hash: "opaque-retirement-job", InfoHashV1: meta.InfoHashV1,
		IdentityStatus: downloader.IdentityStatusValid, IdentityEvidence: []string{"qbittorrent.magnet_uri.xt"},
		IdentityIssues: []string{}, SizeBytes: meta.TotalLength, State: "stoppedDL", Progress: 0.25,
		SavePath: savePath, ContentPath: contentPath}
	adoptionSession := &retireAdoptionSession{requests: 1, ledgers: []downloader.LedgerSnapshot{
		retireLedger(nil, now), retireLedger(&job, now.Add(2*time.Second)),
	}}
	store, _, err := metastore.Init(filepath.Join(t.TempDir(), "metastore"))
	if err != nil {
		t.Fatal(err)
	}
	_, artifact, _, err := store.Import(ctx, bytes.NewReader(raw), metastore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := store.LoadPayload(ctx, artifact.ID, metastore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	adoptionReport, err := clientadopt.Run(ctx, clientadopt.RunOptions{Prepared: adoptionPlan,
		ExpectedPlanID: adoptionPlan.PlanID(), Metafile: payload, Session: adoptionSession, AcknowledgeAdd: true})
	if err != nil {
		t.Fatalf("adoption=%#v err=%v", adoptionReport, err)
	}
	verifiedAdoption, _, err := clientadopt.VerifyCompletion(ctx, clientadopt.CompletionProofOptions{TargetRoot: targetRoot,
		OperationID: adoptionPlan.OperationID(), ExpectedPlanID: adoptionPlan.PlanID()})
	if err != nil {
		t.Fatal(err)
	}
	authority, err := clientactivate.PrepareAuthority(final, verifiedAdoption, clientactivate.AuthorityOptions{
		ClientConfigID: clientConfig, HostRoot: hostRoot, ClientRoot: "/downloads", ClientWindows: false,
		FileLimits: downloader.DefaultJobFileLedgerLimits()})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := downloader.ExistingJobControlDescriptor{Driver: "qbittorrent",
		Protocol: downloader.ControlProtocolQBittorrentV5, RecheckRouteID: "qbittorrent.torrents.recheck.v1",
		StartRouteID: "qbittorrent.torrents.start.v1"}
	previewSession := &retireActivationSession{requests: 1, descriptor: descriptor, ledgers: []downloader.LedgerSnapshot{retireLedger(&job, now.Add(3*time.Second))}}
	preview, err := clientactivate.Preview(ctx, authority, previewSession, false)
	if err != nil {
		t.Fatal(err)
	}
	completeJob := job
	completeJob.State, completeJob.Progress = "stoppedUP", 1
	runSession := &retireActivationSession{requests: 1, descriptor: descriptor, ledgers: []downloader.LedgerSnapshot{
		retireLedger(&job, now.Add(4*time.Second)), retireLedger(&completeJob, now.Add(6*time.Second)),
	}}
	activated, err := clientactivate.Run(ctx, clientactivate.RunOptions{Authority: authority, ExpectedPlanID: preview.Plan.ID,
		Session: runSession, AcknowledgeRecheck: true})
	if err != nil || activated.Outcome != clientactivate.OutcomeCheckedStopped || runSession.rechecks != 1 {
		t.Fatalf("activation=%#v err=%v", activated, err)
	}
	activationOperation, err := clientactivate.ParseOperationID(activated.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	completion, _, err := clientactivate.VerifyCompletion(ctx, clientactivate.CompletionProofOptions{TargetRoot: targetRoot,
		OperationID: activationOperation, ExpectedPlanID: preview.Plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Re-run live source discovery after all downstream steps. The retirement
	// planner must consume this invocation's current proof, not the historical
	// source capability used by materialization.
	freshDiscovery, err := seed.Discover(ctx, meta, seed.DiscoverOptions{SearchRoots: []string{sourceRoot},
		InventoryLimits: storage.DefaultInventoryLimits(), MatchLimits: metafile.DefaultSourceMatchLimits(),
		TimeBudget: time.Minute, Strategy: materialize.StrategyCopy})
	if err != nil || freshDiscovery.SourceOutcome != "verified_unique" {
		t.Fatalf("fresh discovery=%#v err=%v", freshDiscovery, err)
	}
	return retireFixture{meta: meta, discovery: freshDiscovery, final: final, activation: completion,
		sourceRoot: sourceRoot, sourcePath: sourcePath, targetRoot: targetRoot, activationPlanID: preview.Plan.ID}
}

func retireLedger(job *downloader.Torrent, started time.Time) downloader.LedgerSnapshot {
	jobs := []downloader.Torrent{}
	if job != nil {
		jobs = append(jobs, *job)
	}
	return downloader.LedgerSnapshot{Driver: "qbittorrent", ObservedAtStart: started,
		ObservedAtEnd: started.Add(time.Millisecond), Complete: true,
		Capabilities: downloader.LedgerCapabilities{TypedInfoHashes: true, ContentPath: true}, Jobs: jobs}
}

func retireV1Metafile(name string, content []byte) []byte {
	piece := sha1.Sum(content)
	info := fmt.Sprintf("d6:lengthi%de4:name%d:%s12:piece lengthi16384e6:pieces20:", len(content), len(name), name)
	return append(append([]byte("d4:info"+info), piece[:]...), []byte("ee")...)
}

func hasFinding(findings []Finding, code string) bool {
	for _, finding := range findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}
