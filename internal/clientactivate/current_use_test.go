package clientactivate

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

type inflatedCurrentUseSession struct{ *activationSession }

func (session *inflatedCurrentUseSession) ReadLedger(ctx context.Context) (downloader.LedgerSnapshot, error) {
	value, err := session.activationSession.ReadLedger(ctx)
	session.requests += 2
	return value, err
}

func TestCurrentUseRequiresStableCompleteExactJob(t *testing.T) {
	fixture := makeActivationFixture(t)
	completion := completeRecheckOnlyForCurrentUse(t, fixture)
	hostRoot, err := filepath.EvalSymlinks(fixture.targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := PrepareCurrentUse(fixture.verifiedFinal, completion, CurrentUseOptions{
		ClientConfigID: fixture.clientConfig, HostRoot: hostRoot, ClientRoot: "/downloads",
		FileLimits: downloader.DefaultJobFileLedgerLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	complete := fixture.job("stoppedUP", 1)
	session := newActivationSession(fixture,
		activationLedger(fixture.meta, &complete, now),
		activationLedger(fixture.meta, &complete, now.Add(time.Second)))
	before, beforeObservation, err := VerifyCurrentUse(context.Background(), authority, session)
	if err != nil || !before.Verified() || beforeObservation.UseID == "" || beforeObservation.RequestsMade != 1 ||
		beforeObservation.Driver != downloader.DriverQBittorrent || beforeObservation.JobID != completion.Plan().JobID ||
		!beforeObservation.AllSelected || !beforeObservation.AllComplete {
		t.Fatalf("before=%#v verified=%t err=%v", beforeObservation, before != nil && before.Verified(), err)
	}
	request, ok := before.RemovalRequest()
	if !ok || request.JobKey != fixture.opaqueKey {
		t.Fatalf("removal request authority missing: %#v ok=%t", request, ok)
	}
	serialized, err := json.Marshal(struct {
		Verified    *VerifiedCurrentUse   `json:"verified"`
		Observation CurrentUseObservation `json:"observation"`
	}{Verified: before, Observation: beforeObservation})
	if err != nil || strings.Contains(string(serialized), fixture.opaqueKey) {
		t.Fatalf("current-use JSON leaked opaque key: %s err=%v", serialized, err)
	}
	after, afterObservation, err := VerifyCurrentUse(context.Background(), authority, session)
	if err != nil || !after.Verified() || !before.StableWith(after) || beforeObservation.UseID != afterObservation.UseID ||
		afterObservation.Driver != downloader.DriverQBittorrent || session.RequestsMade() != 3 {
		t.Fatalf("after=%#v stable=%t requests=%d err=%v", afterObservation, before.StableWith(after), session.RequestsMade(), err)
	}

	started := fixture.job("uploading", 1)
	changedSession := newActivationSession(fixture,
		activationLedger(fixture.meta, &complete, now.Add(2*time.Second)),
		activationLedger(fixture.meta, &started, now.Add(3*time.Second)))
	first, _, err := VerifyCurrentUse(context.Background(), authority, changedSession)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := VerifyCurrentUse(context.Background(), authority, changedSession)
	if err != nil || first.StableWith(second) {
		t.Fatalf("state-changing bracket was stable: first=%#v second=%#v err=%v", first.Observation(), second.Observation(), err)
	}
}

func TestCurrentUseProvesSameSessionTypedAbsenceWithoutCausality(t *testing.T) {
	fixture := makeActivationFixture(t)
	completion := completeRecheckOnlyForCurrentUse(t, fixture)
	hostRoot, err := filepath.EvalSymlinks(fixture.targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := PrepareCurrentUse(fixture.verifiedFinal, completion, CurrentUseOptions{
		ClientConfigID: fixture.clientConfig, HostRoot: hostRoot, ClientRoot: "/downloads",
		FileLimits: downloader.DefaultJobFileLedgerLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	complete := fixture.job("stoppedUP", 1)
	session := newActivationSession(fixture,
		activationLedger(fixture.meta, &complete, now),
		activationLedger(fixture.meta, nil, now.Add(time.Second)))
	before, _, err := VerifyCurrentUse(context.Background(), authority, session)
	if err != nil {
		t.Fatal(err)
	}
	absence, observation, err := VerifyCurrentJobAbsent(context.Background(), before, session)
	if err != nil || !absence.Verified() || observation.Status != "exact_typed_job_absent" || observation.RequestsMade != 1 ||
		observation.JobID != before.Observation().JobID || observation.Final != before.Observation().Final || session.RequestsMade() != 3 {
		t.Fatalf("absence=%#v verified=%t requests=%d err=%v", observation, absence != nil && absence.Verified(), session.RequestsMade(), err)
	}
	other := newActivationSession(fixture, activationLedger(fixture.meta, nil, now.Add(2*time.Second)))
	if _, _, err := VerifyCurrentJobAbsent(context.Background(), before, other); !errors.Is(err, ErrPolicy) || other.RequestsMade() != 1 {
		// Constructor bookkeeping counts as one synthetic open request; no ledger read is allowed.
		t.Fatalf("cross-session absence was accepted: requests=%d err=%v", other.RequestsMade(), err)
	}
}

func TestCurrentUseRejectsIncompleteJobAndMappingMismatch(t *testing.T) {
	fixture := makeActivationFixture(t)
	completion := completeRecheckOnlyForCurrentUse(t, fixture)
	hostRoot, err := filepath.EvalSymlinks(fixture.targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	options := CurrentUseOptions{ClientConfigID: fixture.clientConfig, HostRoot: hostRoot, ClientRoot: "/downloads",
		FileLimits: downloader.DefaultJobFileLedgerLimits()}
	authority, err := PrepareCurrentUse(fixture.verifiedFinal, completion, options)
	if err != nil {
		t.Fatal(err)
	}
	incomplete := fixture.job("stoppedDL", 0.5)
	if _, observation, err := VerifyCurrentUse(context.Background(), authority,
		newActivationSession(fixture, activationLedger(fixture.meta, &incomplete, time.Now().UTC()))); !errors.Is(err, ErrPolicy) ||
		observation.RequestsMade != 1 {
		t.Fatalf("incomplete job observation=%#v err=%v", observation, err)
	}
	options.ClientRoot = "/different"
	if _, err := PrepareCurrentUse(fixture.verifiedFinal, completion, options); !errors.Is(err, ErrPolicy) {
		t.Fatalf("mapping mismatch retained current-use authority: %v", err)
	}

	complete := fixture.job("stoppedUP", 1)
	inflated := &inflatedCurrentUseSession{activationSession: newActivationSession(fixture,
		activationLedger(fixture.meta, &complete, time.Now().UTC()))}
	if _, observation, err := VerifyCurrentUse(context.Background(), authority, inflated); !errors.Is(err, ErrIntegrity) ||
		observation.RequestsMade != 3 {
		t.Fatalf("over-budget observation=%#v err=%v", observation, err)
	}
	missingCapability := activationLedger(fixture.meta, &complete, time.Now().UTC())
	missingCapability.Capabilities.ContentPath = false
	if _, observation, err := VerifyCurrentUse(context.Background(), authority,
		newActivationSession(fixture, missingCapability)); !errors.Is(err, ErrPolicy) || observation.RequestsMade != 1 {
		t.Fatalf("missing path capability observation=%#v err=%v", observation, err)
	}
}

func TestCurrentUseMultiFileReadsOneBoundedFileLedgerPerObservation(t *testing.T) {
	raw, sources := activationMultiV1Metafile()
	fixture := makeActivationFixtureFrom(t, raw, sources)
	completion := completeRecheckOnlyForCurrentUse(t, fixture)
	hostRoot, err := filepath.EvalSymlinks(fixture.targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := PrepareCurrentUse(fixture.verifiedFinal, completion, CurrentUseOptions{
		ClientConfigID: fixture.clientConfig, HostRoot: hostRoot, ClientRoot: "/downloads",
		FileLimits: downloader.DefaultJobFileLedgerLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	complete := fixture.job("stoppedUP", 1)
	session := newActivationSession(fixture,
		activationLedger(fixture.meta, &complete, now),
		activationLedger(fixture.meta, &complete, now.Add(time.Second)))
	session.files = []downloader.JobFileLedgerSnapshot{
		completeActivationFileSnapshot(fixture, now.Add(2*time.Millisecond)),
		completeActivationFileSnapshot(fixture, now.Add(time.Second+2*time.Millisecond)),
	}
	before, beforeObservation, err := VerifyCurrentUse(context.Background(), authority, session)
	if err != nil {
		t.Fatal(err)
	}
	after, afterObservation, err := VerifyCurrentUse(context.Background(), authority, session)
	if err != nil || !before.StableWith(after) || beforeObservation.RequestsMade != 2 || afterObservation.RequestsMade != 2 ||
		beforeObservation.FilesObserved != 2 || session.RequestsMade() != 5 {
		t.Fatalf("before=%#v after=%#v requests=%d stable=%t err=%v", beforeObservation, afterObservation,
			session.RequestsMade(), before.StableWith(after), err)
	}
}

func completeRecheckOnlyForCurrentUse(t *testing.T, fixture activationFixture) *VerifiedCompletion {
	t.Helper()
	now := time.Now().UTC()
	incomplete := fixture.job("stoppedDL", 0.25)
	complete := fixture.job("stoppedUP", 1)
	previewSession := newActivationSession(fixture, activationLedger(fixture.meta, &incomplete, now))
	if fixture.meta.MultiFile {
		previewSession.files = []downloader.JobFileLedgerSnapshot{
			activationFileSnapshot(fixture, now.Add(2*time.Millisecond), downloader.JobFileSelectionSelected),
		}
	}
	preview, err := Preview(context.Background(), fixture.authority, previewSession, false)
	if err != nil {
		t.Fatal(err)
	}
	runSession := newActivationSession(fixture,
		activationLedger(fixture.meta, &incomplete, now.Add(time.Second)),
		activationLedger(fixture.meta, &complete, now.Add(2*time.Second)))
	if fixture.meta.MultiFile {
		runSession.files = []downloader.JobFileLedgerSnapshot{
			activationFileSnapshot(fixture, now.Add(time.Second+2*time.Millisecond), downloader.JobFileSelectionSelected),
			completeActivationFileSnapshot(fixture, now.Add(2*time.Second+2*time.Millisecond)),
		}
	}
	run, err := Run(context.Background(), RunOptions{Authority: fixture.authority, ExpectedPlanID: preview.Plan.ID,
		Session:            runSession,
		AcknowledgeRecheck: true})
	if err != nil || run.Outcome != OutcomeCheckedStopped {
		t.Fatalf("activation=%#v err=%v", run, err)
	}
	operationID, err := ParseOperationID(run.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	verified, _, err := VerifyCompletion(context.Background(), CompletionProofOptions{TargetRoot: fixture.targetRoot,
		OperationID: operationID, ExpectedPlanID: preview.Plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	return verified
}

func completeActivationFileSnapshot(fixture activationFixture, started time.Time) downloader.JobFileLedgerSnapshot {
	snapshot := activationFileSnapshot(fixture, started, downloader.JobFileSelectionSelected)
	for index := range snapshot.Files {
		snapshot.Files[index].Progress = 1
		snapshot.Files[index].Complete = true
	}
	return snapshot
}
