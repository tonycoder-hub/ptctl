package clientactivate

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

	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

type activationFixture struct {
	meta          *metafile.MetaInfo
	targetRoot    string
	verifiedFinal *materialize.VerifiedFinal
	authority     *PreparedAuthority
	clientConfig  string
	savePath      string
	contentPath   string
	opaqueKey     string
}

type memoryPayload struct {
	raw       []byte
	variantID string
	opened    bool
}

func (payload *memoryPayload) VariantID() string { return payload.variantID }
func (payload *memoryPayload) SizeBytes() int64  { return int64(len(payload.raw)) }
func (payload *memoryPayload) Open() (io.Reader, error) {
	if payload.opened {
		return nil, fmt.Errorf("payload opened twice")
	}
	payload.opened = true
	return bytes.NewReader(payload.raw), nil
}

type adoptionSession struct {
	requests int
	ledgers  []downloader.LedgerSnapshot
}

func (session *adoptionSession) ReadLedger(context.Context) (downloader.LedgerSnapshot, error) {
	session.requests++
	if len(session.ledgers) == 0 {
		return downloader.LedgerSnapshot{}, fmt.Errorf("ledger unavailable")
	}
	value := session.ledgers[0]
	if len(session.ledgers) > 1 {
		session.ledgers = session.ledgers[1:]
	}
	return value, nil
}
func (*adoptionSession) ReadJobFiles(context.Context, string, downloader.JobFileLedgerLimits) (downloader.JobFileLedgerSnapshot, error) {
	return downloader.JobFileLedgerSnapshot{}, fmt.Errorf("not supported")
}
func (session *adoptionSession) AddStopped(_ context.Context, request downloader.AddStoppedRequest) (downloader.MutationReceipt, error) {
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
	return downloader.MutationReceipt{Effect: "submit_exact_metafile_stopped", ObservedAtStart: now, ObservedAtEnd: now.Add(time.Millisecond),
		Complete: true, RequestsAttempted: 1, BytesSubmitted: int64(len(raw)), BytesSubmittedKnown: true}, nil
}
func (session *adoptionSession) RequestsMade() int { return session.requests }
func (*adoptionSession) Close() error              { return nil }

type activationSession struct {
	requests              int
	descriptor            downloader.ExistingJobControlDescriptor
	ledgers               []downloader.LedgerSnapshot
	files                 []downloader.JobFileLedgerSnapshot
	fileErr               error
	recheckErr            error
	startErr              error
	recheckCalls          int
	startCalls            int
	suppressActionRequest bool
}

func (session *activationSession) ReadExistingJobControlDescriptor(context.Context) (downloader.ExistingJobControlDescriptor, error) {
	session.requests++
	return session.descriptor, nil
}
func (session *activationSession) ReadLedger(context.Context) (downloader.LedgerSnapshot, error) {
	session.requests++
	if len(session.ledgers) == 0 {
		return downloader.LedgerSnapshot{}, fmt.Errorf("ledger unavailable")
	}
	value := session.ledgers[0]
	if len(session.ledgers) > 1 {
		session.ledgers = session.ledgers[1:]
	}
	return value, nil
}
func (session *activationSession) ReadJobFiles(context.Context, string, downloader.JobFileLedgerLimits) (downloader.JobFileLedgerSnapshot, error) {
	session.requests++
	if session.fileErr != nil {
		return downloader.JobFileLedgerSnapshot{}, session.fileErr
	}
	if len(session.files) == 0 {
		return downloader.JobFileLedgerSnapshot{}, fmt.Errorf("file ledger unavailable")
	}
	value := session.files[0]
	if len(session.files) > 1 {
		session.files = session.files[1:]
	}
	return value, nil
}
func (session *activationSession) Recheck(_ context.Context, request downloader.ExistingJobMutationRequest) (downloader.ExistingJobMutationReceipt, error) {
	if !session.suppressActionRequest {
		session.requests++
	}
	session.recheckCalls++
	return activationMutationReceipt(downloader.ControlEffectRecheck, request.JobKey, session.recheckErr), session.recheckErr
}
func (session *activationSession) Start(_ context.Context, request downloader.ExistingJobMutationRequest) (downloader.ExistingJobMutationReceipt, error) {
	if !session.suppressActionRequest {
		session.requests++
	}
	session.startCalls++
	return activationMutationReceipt(downloader.ControlEffectStart, request.JobKey, session.startErr), session.startErr
}
func (session *activationSession) RequestsMade() int { return session.requests }
func (*activationSession) Close() error              { return nil }

func activationMutationReceipt(effect, key string, mutationErr error) downloader.ExistingJobMutationReceipt {
	now := time.Now().UTC()
	receipt := downloader.ExistingJobMutationReceipt{Effect: effect, ObservedAtStart: now, ObservedAtEnd: now.Add(time.Millisecond),
		Complete: mutationErr == nil, RequestsAttempted: 1, RequestBytesKnown: mutationErr == nil,
		RequestBytes: int64(len(url.Values{"hashes": {key}}.Encode()))}
	if mutationErr != nil {
		receipt.StopReason = "transport_failed"
	}
	return receipt
}

func newActivationSession(fixture activationFixture, ledgers ...downloader.LedgerSnapshot) *activationSession {
	return &activationSession{requests: 1, descriptor: downloader.ExistingJobControlDescriptor{
		Driver: DriverQBittorrent, Protocol: downloader.ControlProtocolQBittorrentV5,
		RecheckRouteID: "qbittorrent.torrents.recheck.v1", StartRouteID: "qbittorrent.torrents.start.v1",
	}, ledgers: ledgers}
}

type activationSource struct {
	name    string
	content []byte
}

func makeActivationFixture(t *testing.T) activationFixture {
	t.Helper()
	content := []byte("client activation fixture")
	raw := activationSingleV1Metafile("activate.bin", content)
	return makeActivationFixtureFrom(t, raw, []activationSource{{name: "renamed-source", content: content}})
}

func makeActivationFixtureFrom(t *testing.T, raw []byte, sources []activationSource) activationFixture {
	t.Helper()
	ctx := context.Background()
	meta, err := metafile.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	searchRoot := t.TempDir()
	for _, source := range sources {
		if err := os.WriteFile(filepath.Join(searchRoot, source.name), source.content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	targetRoot := t.TempDir()
	discovery, err := seed.Discover(ctx, meta, seed.DiscoverOptions{
		SearchRoots: []string{searchRoot}, InventoryLimits: storage.DefaultInventoryLimits(),
		MatchLimits: metafile.DefaultSourceMatchLimits(), TimeBudget: 10 * time.Second,
		TargetRoot: targetRoot, Strategy: materialize.StrategyCopy,
	})
	if err != nil || discovery.Plan == nil || discovery.SourceOutcome != "verified_unique" {
		t.Fatalf("discovery=%#v err=%v", discovery, err)
	}
	report, err := materialize.Run(ctx, materialize.RunOptions{Meta: meta, Discovery: &discovery, TargetRoot: targetRoot,
		ExpectedPlanID: discovery.Plan.ID, Limits: materialize.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	materializeOperation, err := materialize.ParseOperationID(report.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	verifiedFinal, _, err := materialize.VerifyCurrentFinal(ctx, materialize.FinalProofOptions{Meta: meta, TargetRoot: targetRoot,
		OperationID: materializeOperation, ExpectedPlanID: discovery.Plan.ID, Limits: materialize.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	hostRoot, err := filepath.EvalSymlinks(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig := "sha256:" + strings.Repeat("c", 64)
	projection, err := verifiedFinal.ProjectClientPaths(hostRoot, "/downloads", false)
	if err != nil {
		t.Fatal(err)
	}
	savePath, _ := projection.SavePath()
	contentPath, _ := projection.ContentPath()
	adoptionPlan, err := clientadopt.BuildPlan(verifiedFinal, clientadopt.PlanOptions{ClientConfigID: clientConfig,
		HostRoot: hostRoot, ClientRoot: "/downloads", ClientWindows: false})
	if err != nil {
		t.Fatal(err)
	}
	opaque := "opaque-activation-job"
	before := activationLedger(meta, nil, time.Now().UTC())
	job := downloader.Torrent{Hash: opaque, InfoHashV1: meta.InfoHashV1, IdentityStatus: downloader.IdentityStatusValid,
		IdentityEvidence: []string{"qbittorrent.magnet_uri.xt"}, IdentityIssues: []string{}, SizeBytes: meta.TotalLength,
		State: "stoppedDL", Progress: 0.25, SavePath: savePath, ContentPath: contentPath}
	after := activationLedger(meta, &job, before.ObservedAtEnd.Add(time.Millisecond))
	adoption := &adoptionSession{requests: 1, ledgers: []downloader.LedgerSnapshot{before, after}}
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
	adoptionReport, err := clientadopt.Run(ctx, clientadopt.RunOptions{Prepared: adoptionPlan, ExpectedPlanID: adoptionPlan.PlanID(),
		Metafile: payload, Session: adoption, AcknowledgeAdd: true})
	if err != nil {
		t.Fatalf("adoption report=%#v err=%v", adoptionReport, err)
	}
	verifiedAdoption, _, err := clientadopt.VerifyCompletion(ctx, clientadopt.CompletionProofOptions{TargetRoot: targetRoot,
		OperationID: adoptionPlan.OperationID(), ExpectedPlanID: adoptionPlan.PlanID()})
	if err != nil {
		t.Fatal(err)
	}
	authority, err := PrepareAuthority(verifiedFinal, verifiedAdoption, AuthorityOptions{ClientConfigID: clientConfig,
		HostRoot: hostRoot, ClientRoot: "/downloads", ClientWindows: false, FileLimits: downloader.DefaultJobFileLedgerLimits()})
	if err != nil {
		t.Fatal(err)
	}
	return activationFixture{meta: meta, targetRoot: targetRoot, verifiedFinal: verifiedFinal, authority: authority,
		clientConfig: clientConfig, savePath: savePath, contentPath: contentPath, opaqueKey: opaque}
}

func (fixture activationFixture) job(state string, progress float64) downloader.Torrent {
	return downloader.Torrent{Hash: fixture.opaqueKey, InfoHashV1: fixture.meta.InfoHashV1, IdentityStatus: downloader.IdentityStatusValid,
		IdentityEvidence: []string{"qbittorrent.magnet_uri.xt"}, IdentityIssues: []string{}, SizeBytes: fixture.meta.TotalLength,
		State: state, Progress: progress, SavePath: fixture.savePath, ContentPath: fixture.contentPath}
}

func activationLedger(meta *metafile.MetaInfo, job *downloader.Torrent, started time.Time) downloader.LedgerSnapshot {
	jobs := []downloader.Torrent{}
	if job != nil {
		jobs = append(jobs, *job)
	}
	return downloader.LedgerSnapshot{Driver: DriverQBittorrent, ObservedAtStart: started, ObservedAtEnd: started.Add(time.Millisecond),
		Complete: true, Capabilities: downloader.LedgerCapabilities{TypedInfoHashes: true, ContentPath: true, JobFiles: true}, Jobs: jobs}
}

func activationSingleV1Metafile(name string, content []byte) []byte {
	piece := sha1.Sum(content)
	info := fmt.Sprintf("d6:lengthi%de4:name%d:%s12:piece lengthi16384e6:pieces20:", len(content), len(name), name)
	return append(append([]byte("d4:info"+info), piece[:]...), []byte("ee")...)
}

func activationMultiV1Metafile() ([]byte, []activationSource) {
	dataA, dataB := []byte("abc"), []byte("def")
	joined := append(append([]byte(nil), dataA...), dataB...)
	piece0, piece1 := sha1.Sum(joined[:4]), sha1.Sum(joined[4:])
	pieces := append(append([]byte(nil), piece0[:]...), piece1[:]...)
	var raw bytes.Buffer
	raw.WriteString("d4:infod5:filesl")
	raw.WriteString("d6:lengthi3e4:pathl1:aee")
	raw.WriteString("d6:lengthi3e4:pathl1:bee")
	raw.WriteString("e4:name6:bundle12:piece lengthi4e6:pieces40:")
	raw.Write(pieces)
	raw.WriteString("ee")
	return raw.Bytes(), []activationSource{{name: "renamed-a", content: dataA}, {name: "renamed-b", content: dataB}}
}

func activationFileSnapshot(fixture activationFixture, started time.Time, selection downloader.JobFileSelection) downloader.JobFileLedgerSnapshot {
	limits := downloader.DefaultJobFileLedgerLimits()
	files := []downloader.JobFile{
		{Index: 0, RelativeComponents: []string{"bundle", "a"}, SizeBytes: 3, Progress: 0, Selection: selection, Complete: false},
		{Index: 1, RelativeComponents: []string{"bundle", "b"}, SizeBytes: 3, Progress: 0, Selection: downloader.JobFileSelectionSelected, Complete: false},
	}
	return downloader.JobFileLedgerSnapshot{Driver: DriverQBittorrent, JobKey: fixture.opaqueKey,
		ObservedAtStart: started, ObservedAtEnd: started.Add(time.Millisecond), Complete: true, Limits: limits,
		Used: downloader.JobFileLedgerUsage{FilesConsidered: 2, PathBytes: int64(len("bundle") + len("a") + len("bundle") + len("b")), ResponseBytes: 256}, Files: files}
}

func TestActivationRunObservesCheckingThenResumeCompletesAndStarts(t *testing.T) {
	fixture := makeActivationFixture(t)
	now := time.Now().UTC()
	stopped := fixture.job("stoppedDL", 0.25)
	checking := fixture.job("checkingDL", 0.25)
	complete := fixture.job("stoppedUP", 1)
	started := fixture.job("uploading", 1)
	previewSession := newActivationSession(fixture, activationLedger(fixture.meta, &stopped, now))
	preview, err := Preview(context.Background(), fixture.authority, previewSession, true)
	if err != nil || preview.Outcome != OutcomeReady || preview.Plan.ID == "" || preview.WritesPerformed != 0 {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	runSession := newActivationSession(fixture, activationLedger(fixture.meta, &stopped, now.Add(time.Second)),
		activationLedger(fixture.meta, &checking, now.Add(2*time.Second)))
	run, err := Run(context.Background(), RunOptions{Authority: fixture.authority, ExpectedPlanID: preview.Plan.ID,
		Session: runSession, StartAfterRecheck: true, AcknowledgeRecheck: true})
	if err != nil || run.Outcome != OutcomeRecheckInProgress || runSession.recheckCalls != 1 || runSession.startCalls != 0 ||
		!run.Journal.RecheckStartedDurable || run.Operation.PhaseAfter != "recheck_in_progress" {
		t.Fatalf("run=%#v recheck=%d start=%d err=%v", run, runSession.recheckCalls, runSession.startCalls, err)
	}
	operationID, err := ParseOperationID(run.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	resumeSession := newActivationSession(fixture, activationLedger(fixture.meta, &complete, now.Add(3*time.Second)),
		activationLedger(fixture.meta, &started, now.Add(4*time.Second)))
	resumed, err := Resume(context.Background(), operationID, RunOptions{Authority: fixture.authority, ExpectedPlanID: preview.Plan.ID,
		Session: resumeSession, StartAfterRecheck: true, AcknowledgeStart: true})
	if err != nil || resumed.Outcome != OutcomeStartedClientClaim || resumeSession.recheckCalls != 0 || resumeSession.startCalls != 1 ||
		!resumed.Journal.RecheckCompletionDurable || !resumed.Journal.ActivationCompletionDurable || resumed.Operation.Resumable {
		t.Fatalf("resume=%#v recheck=%d start=%d err=%v", resumed, resumeSession.recheckCalls, resumeSession.startCalls, err)
	}
	status, err := Status(context.Background(), StatusOptions{TargetRoot: fixture.targetRoot, OperationID: operationID})
	if err != nil || status.Outcome != OutcomeHistoricalStarted || status.WritesPerformed != 0 || status.Operation.Resumable {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	cancelledContext, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled, err := Status(cancelledContext, StatusOptions{TargetRoot: fixture.targetRoot, OperationID: operationID})
	if !errors.Is(err, context.Canceled) || cancelled.Outcome != OutcomeIncomplete || cancelled.Outcome == OutcomeIntegrityFailed ||
		cancelled.Operation.Status != "inspection_incomplete" || cancelled.Operation.ID != operationID.String() {
		t.Fatalf("cancelled status=%#v err=%v", cancelled, err)
	}
}

func TestActivationDoesNotReplayUnknownRecheckWithoutExplicitRepeat(t *testing.T) {
	fixture := makeActivationFixture(t)
	now := time.Now().UTC()
	complete := fixture.job("stoppedUP", 1)
	preview, err := Preview(context.Background(), fixture.authority,
		newActivationSession(fixture, activationLedger(fixture.meta, &complete, now)), false)
	if err != nil {
		t.Fatal(err)
	}
	runSession := newActivationSession(fixture, activationLedger(fixture.meta, &complete, now.Add(time.Second)),
		activationLedger(fixture.meta, &complete, now.Add(2*time.Second)))
	run, err := Run(context.Background(), RunOptions{Authority: fixture.authority, ExpectedPlanID: preview.Plan.ID,
		Session: runSession, AcknowledgeRecheck: true})
	if !errors.Is(err, ErrRequestUnknown) || run.Outcome != OutcomeRecheckRequestUnknown || runSession.recheckCalls != 1 || run.Journal.RecheckAttempts != 1 {
		t.Fatalf("run=%#v calls=%d err=%v", run, runSession.recheckCalls, err)
	}
	operationID, _ := ParseOperationID(run.Operation.ID)
	observeOnly := newActivationSession(fixture, activationLedger(fixture.meta, &complete, now.Add(3*time.Second)))
	resumed, err := Resume(context.Background(), operationID, RunOptions{Authority: fixture.authority, ExpectedPlanID: preview.Plan.ID, Session: observeOnly})
	if !errors.Is(err, ErrRequestUnknown) || resumed.Outcome != OutcomeRecheckRequestUnknown || observeOnly.recheckCalls != 0 || resumed.Journal.RecheckAttempts != 1 {
		t.Fatalf("resume=%#v calls=%d err=%v", resumed, observeOnly.recheckCalls, err)
	}

	incomplete := fixture.job("stoppedDL", 0.25)
	secondPreview, err := Preview(context.Background(), fixture.authority,
		newActivationSession(fixture, activationLedger(fixture.meta, &incomplete, now.Add(4*time.Second))), true)
	if err != nil {
		t.Fatal(err)
	}
	failedAfter := newActivationSession(fixture,
		activationLedger(fixture.meta, &incomplete, now.Add(5*time.Second)), downloader.LedgerSnapshot{})
	secondRun, err := Run(context.Background(), RunOptions{Authority: fixture.authority, ExpectedPlanID: secondPreview.Plan.ID,
		Session: failedAfter, StartAfterRecheck: true, AcknowledgeRecheck: true})
	if !errors.Is(err, ErrRequestUnknown) || secondRun.Journal.RecheckAttempts != 1 || secondRun.Journal.RecheckCompletionDurable {
		t.Fatalf("second run=%#v err=%v", secondRun, err)
	}
	secondOperation, _ := ParseOperationID(secondRun.Operation.ID)
	laterComplete := newActivationSession(fixture, activationLedger(fixture.meta, &complete, now.Add(6*time.Second)))
	later, err := Resume(context.Background(), secondOperation, RunOptions{Authority: fixture.authority,
		ExpectedPlanID: secondPreview.Plan.ID, Session: laterComplete, StartAfterRecheck: true})
	if !errors.Is(err, ErrRequestUnknown) || later.Outcome != OutcomeRecheckRequestUnknown ||
		later.Journal.RecheckCompletionDurable || laterComplete.recheckCalls != 0 || laterComplete.startCalls != 0 {
		t.Fatalf("later=%#v recheck=%d start=%d err=%v", later, laterComplete.recheckCalls, laterComplete.startCalls, err)
	}
}

func TestActivationRejectsOutOfOrderActionBracket(t *testing.T) {
	fixture := makeActivationFixture(t)
	now := time.Now().UTC()
	job := fixture.job("stoppedDL", 0.25)
	preview, err := Preview(context.Background(), fixture.authority,
		newActivationSession(fixture, activationLedger(fixture.meta, &job, now)), false)
	if err != nil {
		t.Fatal(err)
	}
	runSession := newActivationSession(fixture,
		activationLedger(fixture.meta, &job, now.Add(4*time.Second)),
		activationLedger(fixture.meta, &job, now.Add(2*time.Second)))
	report, err := Run(context.Background(), RunOptions{Authority: fixture.authority, ExpectedPlanID: preview.Plan.ID,
		Session: runSession, AcknowledgeRecheck: true})
	if !errors.Is(err, ErrIntegrity) || report.Outcome != OutcomeIntegrityFailed || runSession.recheckCalls != 1 ||
		report.Journal.RecheckAttempts != 1 || report.Journal.RecheckCompletionDurable {
		t.Fatalf("report=%#v calls=%d err=%v", report, runSession.recheckCalls, err)
	}
}

func TestActivationPlanMismatchDoesNotCreateJournalOrMutateClient(t *testing.T) {
	fixture := makeActivationFixture(t)
	before, err := os.ReadDir(fixture.targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	job := fixture.job("stoppedDL", 0.5)
	session := newActivationSession(fixture, activationLedger(fixture.meta, &job, time.Now().UTC()))
	report, err := Run(context.Background(), RunOptions{Authority: fixture.authority, ExpectedPlanID: strings.Repeat("f", 24),
		Session: session, AcknowledgeRecheck: true})
	after, readErr := os.ReadDir(fixture.targetRoot)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !errors.Is(err, ErrPolicy) || report.WritesPerformed != 0 || session.recheckCalls != 0 || session.startCalls != 0 || len(before) != len(after) {
		t.Fatalf("report=%#v before=%d after=%d recheck=%d start=%d err=%v", report, len(before), len(after), session.recheckCalls, session.startCalls, err)
	}
	canary := fixture.job("CANARY-CLIENT-STATE", 0.5)
	unsafe, err := Preview(context.Background(), fixture.authority,
		newActivationSession(fixture, activationLedger(fixture.meta, &canary, time.Now().UTC())), false)
	encoded, marshalErr := json.Marshal(unsafe)
	if !errors.Is(err, ErrPolicy) || marshalErr != nil || unsafe.Client.JobState != "unknown" || bytes.Contains(encoded, []byte("CANARY")) {
		t.Fatalf("unsafe report=%s err=%v marshal=%v", encoded, err, marshalErr)
	}
}

func TestActivationMultiFilePlanRequiresExactSelectedIndexedLayout(t *testing.T) {
	raw, sources := activationMultiV1Metafile()
	fixture := makeActivationFixtureFrom(t, raw, sources)
	job := fixture.job("stoppedDL", 0)
	now := time.Now().UTC()
	validFiles := activationFileSnapshot(fixture, now.Add(time.Millisecond), downloader.JobFileSelectionSelected)
	session := newActivationSession(fixture, activationLedger(fixture.meta, &job, now))
	session.files = []downloader.JobFileLedgerSnapshot{validFiles}
	report, err := Preview(context.Background(), fixture.authority, session, false)
	if err != nil || report.Outcome != OutcomeReady || report.Client.FileLedgerReads != 1 || !report.Client.AllFilesSelected || session.RequestsMade() != 4 {
		t.Fatalf("report=%#v requests=%d err=%v", report, session.RequestsMade(), err)
	}

	unsafeFiles := validFiles
	unsafeFiles.Files = append([]downloader.JobFile(nil), validFiles.Files...)
	unsafeFiles.Files[0].RelativeComponents = []string{"bundle", "b"}
	unsafeFiles.Used.PathBytes = int64(len("bundle") + len("b") + len("bundle") + len("b"))
	unsafeSession := newActivationSession(fixture, activationLedger(fixture.meta, &job, now.Add(time.Second)))
	unsafeSession.files = []downloader.JobFileLedgerSnapshot{unsafeFiles}
	unsafe, err := Preview(context.Background(), fixture.authority, unsafeSession, false)
	if !errors.Is(err, ErrIntegrity) || unsafe.Outcome != OutcomeIntegrityFailed || unsafe.WritesPerformed != 0 {
		t.Fatalf("unsafe report=%#v err=%v", unsafe, err)
	}

	skippedFiles := activationFileSnapshot(fixture, now.Add(2*time.Second+time.Millisecond), downloader.JobFileSelectionSkipped)
	skippedSession := newActivationSession(fixture, activationLedger(fixture.meta, &job, now.Add(2*time.Second)))
	skippedSession.files = []downloader.JobFileLedgerSnapshot{skippedFiles}
	skipped, err := Preview(context.Background(), fixture.authority, skippedSession, false)
	if !errors.Is(err, ErrPolicy) || skipped.Outcome != OutcomeBlocked || skipped.WritesPerformed != 0 ||
		skipped.Client.LedgerReads != 1 || skipped.Client.FileLedgerReads != 1 {
		t.Fatalf("skipped report=%#v err=%v", skipped, err)
	}
}

func TestActivationInFlightStateAfterStartAttemptRemainsUnknownWithoutReplay(t *testing.T) {
	handle := &journalHandle{state: journalState{
		Intent:            Intent{Plan: Plan{Action: ActionRecheckThenStart}},
		RecheckCompletion: &RecheckCompletion{}, StartAttempts: []Attempt{{Action: AttemptActionStart}},
		StartAttemptIDs: []MarkerID{MarkerID("sha256:" + strings.Repeat("a", 64))},
	}}
	report := newReport(nil, "")
	observed := clientObservation{job: downloader.Torrent{State: "checkingUP", Progress: 1}, allSelected: true, allComplete: false}
	err := continueOperation(context.Background(), &PreparedPlan{}, handle, nil, RunOptions{}, observed, &report)
	if !errors.Is(err, ErrRequestUnknown) || report.Outcome != OutcomeStartRequestUnknown || !report.Operation.Resumable {
		t.Fatalf("report=%#v err=%v", report, err)
	}
}

func TestActivationStartedClaimCannotCompleteWithIncompleteFiles(t *testing.T) {
	handle := &journalHandle{state: journalState{
		Intent:            Intent{Plan: Plan{Action: ActionRecheckThenStart}},
		RecheckCompletion: &RecheckCompletion{}, StartAttempts: []Attempt{{Action: AttemptActionStart}},
		StartAttemptIDs: []MarkerID{MarkerID("sha256:" + strings.Repeat("a", 64))},
	}}
	report := newReport(nil, "")
	observed := clientObservation{job: downloader.Torrent{State: "uploading", Progress: 1}, allSelected: true, allComplete: false}
	err := continueOperation(context.Background(), &PreparedPlan{}, handle, nil, RunOptions{}, observed, &report)
	if !errors.Is(err, ErrPolicy) || handle.state.ActivationCompletion != nil || report.Outcome == OutcomeStartedClientClaim {
		t.Fatalf("report=%#v err=%v", report, err)
	}
}

func TestActivationZeroRequestMutationIsUnknownAndNotAttributed(t *testing.T) {
	session := &activationSession{requests: 3, suppressActionRequest: true, recheckErr: context.Canceled}
	report := newReport(nil, "")
	before := clientObservation{job: downloader.Torrent{Hash: "opaque"}}
	err := executeAction(context.Background(), &PreparedPlan{}, nil, session, AttemptActionRecheck, before, &report)
	if !errors.Is(err, ErrRequestUnknown) || !errors.Is(err, context.Canceled) || report.Client.ActionAttempted != "" ||
		report.Outcome != OutcomeRecheckRequestUnknown || session.RequestsMade() != 3 {
		t.Fatalf("report=%#v requests=%d err=%v", report, session.RequestsMade(), err)
	}
}
