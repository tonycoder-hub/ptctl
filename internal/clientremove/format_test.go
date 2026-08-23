package clientremove

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

func TestClientRemovalPlanAndMarkersRoundTripCanonically(t *testing.T) {
	root, plan := testRemovalPlan(t)
	planID, err := PlanID(plan)
	if err != nil || plan.DeleteLocalData || OperationIDForPlan(planID) == "" {
		t.Fatalf("planID=%q plan=%#v err=%v", planID, plan, err)
	}
	operation := OperationIDForPlan(planID)
	intent := Intent{Schema: IntentSchemaV1, OperationID: operation, OperationRootIdentity: plan.TargetRootIdentity, PlanID: planID, Plan: plan}
	rawIntent, intentID, err := encodeIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	decodedIntent, decodedIntentID, err := decodeIntent(bytes.NewReader(rawIntent))
	if err != nil || decodedIntent != intent || decodedIntentID != intentID {
		t.Fatalf("intent=%#v id=%q err=%v", decodedIntent, decodedIntentID, err)
	}
	now := time.Now().UTC()
	attempt := testAttempt(operation, planID, plan, now, 1, "")
	rawAttempt, attemptID, err := encodeAttempt(attempt)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, id, err := decodeAttempt(bytes.NewReader(rawAttempt)); err != nil || decoded != attempt || id != attemptID {
		t.Fatalf("attempt=%#v id=%q err=%v", decoded, id, err)
	}
	response := testResponse(operation, planID, attemptID, now.Add(time.Second), true)
	rawResponse, responseID, err := encodeResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, id, err := decodeResponse(bytes.NewReader(rawResponse)); err != nil || decoded != response || id != responseID {
		t.Fatalf("response=%#v id=%q err=%v", decoded, id, err)
	}
	completion := testCompletion(operation, planID, plan, attemptID, responseID, now.Add(2*time.Second), true)
	rawCompletion, completionID, err := encodeCompletion(completion)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, id, err := decodeCompletion(bytes.NewReader(rawCompletion)); err != nil || decoded != completion || id != completionID {
		t.Fatalf("completion=%#v id=%q err=%v", decoded, id, err)
	}
	if strings.Contains(string(rawIntent), root) {
		t.Fatal("canonical plan leaked the target-root path")
	}
}

func TestClientRemovalMarkersRejectDeleteDataUnknownStateAndDuplicateKeys(t *testing.T) {
	_, plan := testRemovalPlan(t)
	plan.DeleteLocalData = true
	if _, err := PlanID(plan); !errors.Is(err, ErrPolicy) {
		t.Fatalf("delete-data plan accepted: %v", err)
	}
	plan.DeleteLocalData = false
	planID, _ := PlanID(plan)
	attempt := testAttempt(OperationIDForPlan(planID), planID, plan, time.Now().UTC(), 1, "")
	attempt.JobState = "CANARY-SECRET"
	if _, _, err := encodeAttempt(attempt); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("unknown state accepted: %v", err)
	}
	duplicate := []byte(`{"schema":"ptctl.client-removal-response/v1","schema":"ptctl.client-removal-response/v1"}` + "\n")
	if _, _, err := decodeResponse(bytes.NewReader(duplicate)); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("duplicate key accepted: %v", err)
	}
}

func TestClientRemovalReceiptMustMatchReviewedWireRequest(t *testing.T) {
	_, plan := testRemovalPlan(t)
	request := downloader.ExistingJobMutationRequest{JobKey: "opaque&a=b"}
	body, err := downloader.MarshalExistingJobRemovalRequest(plan.Removal, request.JobKey, 0)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	receipt := downloader.ExistingJobMutationReceipt{
		Effect: downloader.RemoveEffectKeepData, ObservedAtStart: now, ObservedAtEnd: now.Add(time.Millisecond),
		Complete: true, RequestsAttempted: 1, RequestBytes: int64(len(body)), RequestBytesKnown: true,
	}
	if err := validateMutationReceipt(plan, request, receipt, 1, nil); err != nil {
		t.Fatalf("exact reviewed receipt rejected: %v", err)
	}
	receipt.RequestBytes++
	if err := validateMutationReceipt(plan, request, receipt, 1, nil); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("wrong request size accepted: %v", err)
	}
	receipt.RequestBytes--
	receipt.RequestID = 1
	if err := validateMutationReceipt(plan, request, receipt, 1, nil); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("qBittorrent request ID accepted: %v", err)
	}
}

func TestClientRemovalJournalRejectsProtocolMismatchedRequestID(t *testing.T) {
	_, plan := testRemovalPlan(t)
	planID, err := PlanID(plan)
	if err != nil {
		t.Fatal(err)
	}
	operation := OperationIDForPlan(planID)
	now := time.Now().UTC()
	attempt := testAttempt(operation, planID, plan, now, 1, "")
	_, attemptID, err := encodeAttempt(attempt)
	if err != nil {
		t.Fatal(err)
	}
	response := testResponse(operation, planID, attemptID, now.Add(time.Second), true)
	response.RequestID = 1
	state := journalState{Intent: Intent{Schema: IntentSchemaV1, OperationID: operation, PlanID: planID, Plan: plan}, Attempts: []Attempt{attempt}, AttemptIDs: []MarkerID{attemptID}}
	if err := validateResponseForAttempt(state, 1, response); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("qBittorrent response with Transmission request ID accepted: %v", err)
	}

	plan.Driver = downloader.DriverTransmission
	plan.Removal = downloader.ExistingJobRemovalDescriptor{Driver: downloader.DriverTransmission, Protocol: downloader.ControlProtocolTransmissionV6, RemoveRouteID: "transmission.torrent.remove_keep_data.v1"}
	planID, err = PlanID(plan)
	if err != nil {
		t.Fatal(err)
	}
	operation = OperationIDForPlan(planID)
	attempt = testAttempt(operation, planID, plan, now, 1, "")
	_, attemptID, err = encodeAttempt(attempt)
	if err != nil {
		t.Fatal(err)
	}
	response = testResponse(operation, planID, attemptID, now.Add(time.Second), true)
	state = journalState{Intent: Intent{Schema: IntentSchemaV1, OperationID: operation, PlanID: planID, Plan: plan}, Attempts: []Attempt{attempt}, AttemptIDs: []MarkerID{attemptID}}
	if err := validateResponseForAttempt(state, 1, response); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("Transmission response without a request ID accepted: %v", err)
	}
}

func TestClientRemovalJournalRecoversOneCanonicalPendingMarker(t *testing.T) {
	root, plan := testRemovalPlan(t)
	planID, _ := PlanID(plan)
	handle, _, err := createJournal(context.Background(), root, plan, planID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	attempt := testAttempt(OperationIDForPlan(planID), planID, plan, now, 1, "")
	raw, attemptID, err := encodeAttempt(attempt)
	if err != nil {
		t.Fatal(err)
	}
	pending, _ := fsbind.PathFromComponents([]string{scratchDirectory, pendingName(attemptFileName(1))})
	file, err := handle.subtree.CreateRegular(context.Background(), pending)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeAll(context.Background(), file, raw); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	operation := OperationIDForPlan(planID)
	if _, _, err := openJournal(context.Background(), root, operation, false, nil); !errors.Is(err, ErrInitializationIncomplete) {
		t.Fatalf("read-only open recovered or misclassified pending marker: %v", err)
	}
	recovered, receipt, err := openJournal(context.Background(), root, operation, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if len(recovered.state.Attempts) != 1 || recovered.state.AttemptIDs[0] != attemptID || !receipt.Marker.Publication.Published || recovered.state.Pending != "" {
		t.Fatalf("state=%#v receipt=%#v", recovered.state, receipt)
	}
}

func TestClientRemovalJournalRecoversInterruptedInitializationOnlyWithReviewedPlan(t *testing.T) {
	root, plan := testRemovalPlan(t)
	planID, _ := PlanID(plan)
	operation := OperationIDForPlan(planID)
	directory, _ := operationDirectoryName(operation)
	session, _, err := fsbind.BindExisting(root)
	if err != nil {
		t.Fatal(err)
	}
	subtree, err := session.CreatePrivateSubtree(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := subtree.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openJournal(context.Background(), root, operation, false, nil); !errors.Is(err, ErrInitializationIncomplete) {
		t.Fatalf("read-only open changed interrupted initialization: %v", err)
	}
	if _, _, err := openJournal(context.Background(), root, operation, true, nil); !errors.Is(err, ErrInitializationIncomplete) {
		t.Fatalf("recovery without reviewed plan succeeded: %v", err)
	}
	recovered, receipt, err := openJournal(context.Background(), root, operation, true, &plan)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if recovered.state.Intent.PlanID != planID || receipt.Mkdir.DirectoriesCreated != 1 || !receipt.Marker.Publication.Published {
		t.Fatalf("state=%#v receipt=%#v", recovered.state, receipt)
	}
}

func TestClientRemovalMarkerRecoveryRemovesIdenticalPublishedPendingCopy(t *testing.T) {
	root, plan := testRemovalPlan(t)
	planID, _ := PlanID(plan)
	handle, _, err := createJournal(context.Background(), root, plan, planID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	attempt := testAttempt(OperationIDForPlan(planID), planID, plan, now, 1, "")
	raw, _, err := encodeAttempt(attempt)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := handle.appendAttempt(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	pendingPath, _ := fsbind.PathFromComponents([]string{scratchDirectory, pendingName(attemptFileName(1))})
	pending, err := handle.subtree.CreateRegular(context.Background(), pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeAll(context.Background(), pending, raw); err != nil {
		t.Fatal(err)
	}
	if err := pending.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := pending.Close(); err != nil {
		t.Fatal(err)
	}
	receipt, err := handle.writeMarker(context.Background(), attemptFileName(1), raw)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if !receipt.AlreadyPresent || !receipt.TemporaryRemoved || receipt.Publication.Published {
		t.Fatalf("receipt=%#v", receipt)
	}
	report := newReport(nil)
	report.applyMarker(receipt)
	if report.Writes.WritesPerformed != 1 || report.Writes.PrivateFilesRemoved != 1 || report.Writes.WritesUncertain {
		t.Fatalf("write report=%#v", report.Writes)
	}
	if pending, err := handle.pendingName(context.Background()); err != nil || pending != "" {
		t.Fatalf("pending=%q err=%v", pending, err)
	}
}

func TestClientRemovalJournalRejectsUnknownEntries(t *testing.T) {
	root, plan := testRemovalPlan(t)
	planID, _ := PlanID(plan)
	handle, _, err := createJournal(context.Background(), root, plan, planID)
	if err != nil {
		t.Fatal(err)
	}
	name, _ := operationDirectoryName(OperationIDForPlan(planID))
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name, "CANARY-UNKNOWN"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openJournal(context.Background(), root, OperationIDForPlan(planID), false, nil); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("unknown journal entry accepted: %v", err)
	}
}

func TestClientRemovalNotSentStatusIsIncomplete(t *testing.T) {
	_, plan := testRemovalPlan(t)
	planID, err := PlanID(plan)
	if err != nil {
		t.Fatal(err)
	}
	operation := OperationIDForPlan(planID)
	now := time.Now().UTC()
	attempt := testAttempt(operation, planID, plan, now, 1, "")
	_, attemptID, err := encodeAttempt(attempt)
	if err != nil {
		t.Fatal(err)
	}
	response := testResponse(operation, planID, attemptID, now, false)
	response.RequestsAttempted = 0
	response.RequestBytesKnown = false
	response.RequestBytes = 0
	response.StopReason = "context_cancelled"
	state := journalState{
		Intent:   Intent{Schema: IntentSchemaV1, OperationID: operation, PlanID: planID, Plan: plan},
		Attempts: []Attempt{attempt}, AttemptIDs: []MarkerID{attemptID},
		Responses: map[int]Response{1: response}, ResponseIDs: map[int]MarkerID{},
	}
	if outcome := statusOutcome(state); outcome != OutcomeIncomplete {
		t.Fatalf("outcome=%q", outcome)
	}
	if effectfulAttemptPossible(state) {
		t.Fatal("zero-request receipt treated as a possible effectful attempt")
	}
	response.RequestsAttempted = 1
	response.Complete = true
	response.StopReason = ""
	response.RequestBytesKnown = true
	response.RequestBytes = 1
	state.Responses[1] = response
	if latestRequestResultUnknown(state) || statusOutcome(state) != OutcomeIncomplete {
		t.Fatal("accepted response was mislabeled as an unknown request result")
	}
	delete(state.Responses, 1)
	if !effectfulAttemptPossible(state) || statusOutcome(state) != OutcomeRequestUnknown {
		t.Fatal("missing response did not preserve the unknown-result boundary")
	}
}

func testRemovalPlan(t *testing.T) (string, Plan) {
	t.Helper()
	root := t.TempDir()
	session, info, err := fsbind.BindExisting(root)
	if errors.Is(err, fsbind.ErrUnsupported) {
		t.Skipf("client removal filesystem binding is unsupported: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = session.Close()
	id := info.Identity.String()
	sha := func(character string) string { return "sha256:" + strings.Repeat(character, 64) }
	return root, Plan{
		Schema: PlanSchemaV1, Action: ActionRemoveKeepData, Driver: downloader.DriverQBittorrent,
		ClientConfigID: sha("1"), Removal: downloader.ExistingJobRemovalDescriptor{Driver: downloader.DriverQBittorrent, Protocol: downloader.ControlProtocolQBittorrentV5, RemoveRouteID: "qbittorrent.torrents.delete_keep_data.v1"},
		UseID: sha("2"), JobID: sha("3"), FileLayoutID: sha("4"), CompleteFileSnapshotID: sha("5"), PathMappingID: sha("6"),
		ActivationOperationID: sha("7"), ActivationPlanID: strings.Repeat("8", 24), ActivationTerminalID: sha("9"),
		MetafileVariantID: sha("a"), InfoHashV1: strings.Repeat("b", 40), MaterializeOperationID: sha("c"),
		MaterializePlanID: strings.Repeat("d", 24), TargetRootIdentity: id, FinalObjectIdentity: id,
		ManifestFiles: 1, ContentBytes: 4, FileLimits: downloader.DefaultJobFileLedgerLimits(),
	}
}

func testAttempt(operation OperationID, planID string, plan Plan, observed time.Time, sequence int, previous string) Attempt {
	return Attempt{
		Schema: AttemptSchemaV1, OperationID: operation, PlanID: planID, Sequence: sequence, PreviousAttemptID: previous,
		UseID: plan.UseID, JobID: plan.JobID, FileLayoutID: plan.FileLayoutID, FileSnapshotID: plan.CompleteFileSnapshotID,
		ObservedAtStart: observed, ObservedAtEnd: observed.Add(time.Millisecond), JobState: "stoppedUP", JobProgress: 1,
	}
}

func testResponse(operation OperationID, planID string, attempt MarkerID, observed time.Time, complete bool) Response {
	value := Response{
		Schema: ResponseSchemaV1, OperationID: operation, PlanID: planID, AttemptID: attempt,
		Effect: downloader.RemoveEffectKeepData, ObservedAtStart: observed, ObservedAtEnd: observed.Add(time.Millisecond),
		Complete: complete, RequestsAttempted: 1,
	}
	if complete {
		value.RequestBytesKnown, value.RequestBytes = true, 42
	} else {
		value.RequestBytesKnown, value.RequestBytes = true, 42
		value.StopReason = "transport_failed"
	}
	return value
}

func testCompletion(operation OperationID, planID string, plan Plan, attempt, response MarkerID, observed time.Time, attributed bool) Completion {
	basis := "exact_absence_after_unknown_attempt_causality_unproven"
	if attributed {
		basis = "accepted_response_then_exact_absence"
	}
	return Completion{
		Schema: CompletionSchemaV1, OperationID: operation, PlanID: planID, AttemptID: attempt, ResponseID: response.String(), Basis: basis,
		UseID: plan.UseID, JobID: plan.JobID, ObservedAtStart: observed, ObservedAtEnd: observed.Add(time.Millisecond),
		FinalObjectIdentity: plan.FinalObjectIdentity, FinalVerificationBasis: "same_invocation_post_removal_exact_final_reverification",
	}
}
