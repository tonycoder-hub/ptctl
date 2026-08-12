package reconcile

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/sitebinding"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

func TestBuildConsistentReportKeepsAxesSeparateAndPathsPrivate(t *testing.T) {
	meta, discovery, source, root := reconciledSingleFile(t)
	job := matchingJob(meta, "/downloads/renamed.bin")
	before, after := ledgerPair(job)
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client:      ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3},
		SiteRef:     &domain.TorrentRef{SiteID: "tjupt", RemoteID: "123"},
		PathMapping: testPathMapping(root),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "consistent" || report.WritesPerformed != 0 || report.Ledgers.Downloader.RequestsMade != 3 {
		t.Fatalf("unexpected report: %#v", report)
	}
	if !report.Scope.PathMappingRequested || report.Scope.ClientPathSemantics != "posix_exact" || report.Scope.PathMappingID == "" {
		t.Fatalf("mapping scope was not made auditable: %#v", report.Scope)
	}
	if relationStatus(report, "metafile_variant_relation") != "unobservable" || relationStatus(report, "client_infohash_relation") != "exact_unique" || relationStatus(report, "storage_content_proof") != "verified_unique" || relationStatus(report, "verified_source_vs_job_path") != "same_location" || relationStatus(report, "site_metafile") != "declared_unbound" {
		t.Fatalf("relations were collapsed or overstated: %#v", report.Relations)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{root, "/downloads/renamed.bin", "CANARY-TRACKER-PASSKEY"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("private path or tracker data leaked: %s", encoded)
		}
	}
	publicDiscovery := report.Ledgers.Storage.Discovery
	if recovered, ok := publicDiscovery.VerifiedSource(meta); ok || recovered != nil {
		t.Fatal("the public report retained the process-local source capability")
	}
	if _, err := publicDiscovery.Matches[0].Verification.MatchSourceSnapshot(filepath.Join(root, "renamed.bin")); err == nil {
		t.Fatal("the public report retained a source-path snapshot oracle")
	}
}

func TestSerializedDiscoveryCannotBecomeProcessLocalProof(t *testing.T) {
	meta, discovery, _, _ := reconciledSingleFile(t)
	encoded, err := json.Marshal(discovery)
	if err != nil {
		t.Fatal(err)
	}
	var replayed seed.DiscoveryResult
	if err := json.Unmarshal(encoded, &replayed); err != nil {
		t.Fatal(err)
	}
	if source, ok := replayed.VerifiedSource(meta); ok || source != nil {
		t.Fatal("a serialized discovery report retained process-local authority")
	}
	report, err := Build(BuildInput{Meta: meta, Discovery: replayed})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "incomplete" || relationStatus(report, "storage_content_proof") != "incomplete" || report.Ledgers.Storage.ProcessLocalProof {
		t.Fatalf("replayed report was accepted as proof: %#v", report)
	}
}

func TestRequestedMaterializedFinalCannotFallBackToOrdinaryExactSourceProof(t *testing.T) {
	meta, discovery, source, _ := reconciledSingleFile(t)
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		MaterializedFinal: MaterializedFinalSelection{Requested: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger := report.Ledgers.Storage.MaterializedFinal
	if report.Outcome != "incomplete" || report.Ledgers.Storage.Status != "incomplete" ||
		relationStatus(report, "storage_content_proof") != "incomplete" || ledger.Status != "incomplete" ||
		ledger.ProcessLocalFinalProof || ledger.ProcessLocalSourceBridge ||
		!containsFinding(report.Blockers, "storage.materialized_final_proof_unavailable") {
		t.Fatalf("materialized-final request fell back to ordinary exact proof: %#v", report)
	}

	unexpected, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		MaterializedFinal: MaterializedFinalSelection{StopReason: "materialized_final_verification_failed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if unexpected.Outcome != "incomplete" || unexpected.Ledgers.Storage.MaterializedFinal.StopReason != "materialized_final_unexpected_activity" ||
		!containsFinding(unexpected.Blockers, "storage.materialized_final_input_inconsistent") {
		t.Fatalf("unexpected materialized-final activity was ignored: %#v", unexpected)
	}

	unsafe, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		MaterializedFinal: MaterializedFinalSelection{Requested: true, StopReason: "MATERIALIZED-FINAL-SECRET-CANARY"},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(unsafe)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "MATERIALIZED-FINAL-SECRET-CANARY") ||
		unsafe.Ledgers.Storage.MaterializedFinal.StopReason != "materialized_final_verification_failed" {
		t.Fatalf("untrusted materialized-final stop reason entered public output: %s", raw)
	}
}

func TestClientActivationRequestFailsClosedAndSanitizesUntrustedStopReason(t *testing.T) {
	meta, discovery, source, _ := reconciledSingleFile(t)
	const canary = "ACTIVATION-STOP-SECRET-CANARY"
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		ClientActivation: ClientActivationSelection{Requested: true, StopReason: canary},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "incomplete" || report.Ledgers.Activation.Status != "incomplete" ||
		report.Ledgers.Activation.StopReason != "activation_completion_load_failed" ||
		report.Ledgers.Activation.ProcessLocalCompletionProof || report.Ledgers.Activation.ProcessLocalCurrentUseProof ||
		strings.Contains(string(raw), canary) {
		t.Fatalf("unsafe activation failure report: %s", raw)
	}

	unexpected, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		ClientActivation: ClientActivationSelection{StopReason: canary},
	})
	if err != nil {
		t.Fatal(err)
	}
	if unexpected.Outcome != "incomplete" || unexpected.Ledgers.Activation.StopReason != "activation_unexpected_activity" ||
		!containsFinding(unexpected.Blockers, "activation.input_inconsistent") {
		t.Fatalf("unexpected activation activity was ignored: %#v", unexpected)
	}
}

func TestClientAdoptionRequestFailsClosedAndSanitizesUntrustedStopReason(t *testing.T) {
	meta, discovery, source, _ := reconciledSingleFile(t)
	const canary = "ADOPTION-STOP-SECRET-CANARY"
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		ClientAdoption: ClientAdoptionSelection{Requested: true, StopReason: canary},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "incomplete" || report.Ledgers.Adoption.Status != "incomplete" ||
		report.Ledgers.Adoption.StopReason != "adoption_completion_load_failed" ||
		report.Ledgers.Adoption.ProcessLocalCompletionProof || report.Ledgers.Adoption.ProcessLocalCurrentJobProof ||
		strings.Contains(string(raw), canary) {
		t.Fatalf("unsafe adoption failure report: %s", raw)
	}

	unexpected, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		ClientAdoption: ClientAdoptionSelection{StopReason: canary},
	})
	if err != nil {
		t.Fatal(err)
	}
	if unexpected.Outcome != "incomplete" || unexpected.Ledgers.Adoption.StopReason != "adoption_unexpected_activity" ||
		!containsFinding(unexpected.Blockers, "adoption.input_inconsistent") {
		t.Fatalf("unexpected adoption activity was ignored: %#v", unexpected)
	}
}

func TestClientAdoptionAssessmentBindsHistoricalCompletionToExistingBracket(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)
	planID := strings.Repeat("c", 24)
	operationDigest := sha256.Sum256([]byte("ptctl-client-adopt-operation-v1\x00" + planID))
	adoptionOperation := "sha256:" + hex.EncodeToString(operationDigest[:])
	now := time.Now().UTC()
	meta := &metafile.MetaInfo{MetafileVariantID: digest, MetafileBytes: 64, InfoHashV1: strings.Repeat("d", 40),
		Files: []metafile.File{{Length: 4}}}
	completion := ClientAdoptionCompletion{
		Driver: downloader.DriverQBittorrent, Action: "add_stopped", OperationID: adoptionOperation, PlanID: planID, CompletionID: other,
		MetafileVariantID: digest, MetafileBytes: 64, InfoHashV1: meta.InfoHashV1,
		MaterializeOperationID: other, MaterializePlanID: planID, ClientConfigID: digest, PathMappingID: other,
		ClientPathSemantics: "posix_exact", ExpectedSavePathRef: digest, ExpectedContentPathRef: other,
		JobID: digest, TerminalJobState: "stoppedUP", TargetRootIdentity: "fsbind-v1:" + strings.Repeat("e", 64),
		FinalObjectIdentity: "fsbind-v1:" + strings.Repeat("f", 64), ManifestFiles: 1, ContentBytes: 4,
		ObservedAtStart: now, ObservedAtEnd: now.Add(time.Second),
		Assurance: "same_invocation_bound_canonical_adoption_completion_read_without_durability_refresh",
	}
	materialized := MaterializedFinalLedger{Status: "verified_current_final_source", ProcessLocalFinalProof: true,
		Observation: &materialize.FinalObservation{OperationID: other, MaterializePlanID: planID,
			MetafileVariantID: digest, MetafileBytes: 64, InfoHashV1: meta.InfoHashV1,
			TargetRootIdentity: completion.TargetRootIdentity, FinalObjectIdentity: completion.FinalObjectIdentity,
			ManifestFiles: 1, ContentBytes: 4}}
	before := downloader.LedgerSnapshot{ObservedAtStart: now.Add(2 * time.Second)}
	after := downloader.LedgerSnapshot{ObservedAtEnd: now.Add(3 * time.Second)}
	job := downloader.Torrent{}
	client := clientAssessment{ledger: DownloaderLedger{Status: "observed_stable"}, relation: Relation{Status: "exact_unique"}, job: &job}
	current := ClientAdoptionCurrentJob{Driver: downloader.DriverQBittorrent, JobID: digest, JobState: "stoppedUP",
		SavePathRef: digest, ContentPathRef: other, ObservedAtStart: before.ObservedAtStart, ObservedAtEnd: after.ObservedAtEnd,
		RequestsMade: 3, JobsExaminedBefore: 1, JobsExaminedAfter: 1, FinalObjectIdentity: completion.FinalObjectIdentity,
		Assurance: "same_invocation_existing_reconciliation_bracket_bound_to_canonical_stopped_adoption_and_exact_final_with_current_typed_job_claim_non_atomic_without_job_incarnation_proof"}
	ledger, blockers, warnings := assessClientAdoption(meta, ClientAdoptionSelection{Requested: true, CompletionAttempted: true,
		Completion: adoptionCompletionStub{value: completion}, CurrentJob: adoptionCurrentJobStub{value: current}}, materialized,
		ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3}, client, other)
	if ledger.Status != "historical_completion_current_job_bound" || !ledger.ProcessLocalCompletionProof ||
		!ledger.ProcessLocalCurrentJobProof || ledger.Completion == nil || ledger.CurrentJob == nil || len(blockers) != 0 || len(warnings) < 2 {
		t.Fatalf("ledger=%#v blockers=%#v warnings=%#v", ledger, blockers, warnings)
	}
	observedExisting := completion
	observedExisting.Action = "adopt_existing_stopped"
	observedExisting.Assurance = "same_invocation_bound_canonical_existing_stopped_adoption_completion_read_without_durability_refresh"
	existingLedger, existingBlockers, existingWarnings := assessClientAdoption(meta, ClientAdoptionSelection{Requested: true, CompletionAttempted: true,
		Completion: adoptionCompletionStub{value: observedExisting}, CurrentJob: adoptionCurrentJobStub{value: current}}, materialized,
		ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3}, client, other)
	if existingLedger.Status != "historical_completion_current_job_bound" || existingLedger.Completion == nil ||
		existingLedger.Completion.Action != "adopt_existing_stopped" || len(existingBlockers) != 0 ||
		!containsWarning(existingWarnings, "did not submit or prove") {
		t.Fatalf("existing ledger=%#v blockers=%#v warnings=%#v", existingLedger, existingBlockers, existingWarnings)
	}
	removalPlanID := strings.Repeat("1", 24)
	removalOperationDigest := sha256.Sum256([]byte("ptctl-client-removal-operation-v1\x00" + removalPlanID))
	removalAuthorized := completion
	removalAuthorized.Action = "readd_stopped_after_removal"
	removalAuthorized.RemovalOperationID = "sha256:" + hex.EncodeToString(removalOperationDigest[:])
	removalAuthorized.RemovalPlanID = removalPlanID
	removalAuthorized.RemovalCompletionID = "sha256:" + strings.Repeat("2", 64)
	removalAuthorized.RemovalCompletionBasis = "accepted_response_then_exact_absence"
	removalLedger, removalBlockers, removalWarnings := assessClientAdoption(meta,
		ClientAdoptionSelection{Requested: true, CompletionAttempted: true,
			Completion: adoptionCompletionStub{value: removalAuthorized}, CurrentJob: adoptionCurrentJobStub{value: current}},
		materialized, ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3}, client, other)
	if removalLedger.Status != "historical_completion_current_job_bound" || removalLedger.Completion == nil ||
		removalLedger.Completion.Action != "readd_stopped_after_removal" ||
		removalLedger.Completion.RemovalOperationID != removalAuthorized.RemovalOperationID ||
		len(removalBlockers) != 0 || len(removalWarnings) < 2 {
		t.Fatalf("removal-authorized ledger=%#v blockers=%#v warnings=%#v", removalLedger, removalBlockers, removalWarnings)
	}
	invalidRemoval := removalAuthorized
	invalidRemoval.RemovalCompletionBasis = "exact_absence_after_unknown_attempt_causality_unproven"
	invalidLedger, invalidBlockers, _ := assessClientAdoption(meta,
		ClientAdoptionSelection{Requested: true, CompletionAttempted: true,
			Completion: adoptionCompletionStub{value: invalidRemoval}, CurrentJob: adoptionCurrentJobStub{value: current}},
		materialized, ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3}, client, other)
	if invalidLedger.Status != "incomplete" || invalidLedger.ProcessLocalCompletionProof ||
		invalidLedger.StopReason != "adoption_completion_load_failed" ||
		!containsFinding(invalidBlockers, "adoption.completion_proof_unavailable") {
		t.Fatalf("invalid removal lineage ledger=%#v blockers=%#v", invalidLedger, invalidBlockers)
	}
	if got := overallOutcome("not_requested", false, "not_requested", false, "verified_materialized_final", true,
		"exact_unique", "same_location", true, ledger.Status, true, "not_requested", false, "not_requested", false, "not_requested", false, "not_requested", false); got != "consistent" {
		t.Fatalf("bound adoption did not preserve ordinary consistent lattice: %q", got)
	}
}

type adoptionCompletionStub struct{ value ClientAdoptionCompletion }

func (stub adoptionCompletionStub) ReconciliationAdoptionCompletion() (ClientAdoptionCompletion, bool) {
	return stub.value, true
}

type adoptionCurrentJobStub struct{ value ClientAdoptionCurrentJob }

func (stub adoptionCurrentJobStub) ReconcileAdoptedCurrentJob(ClientBracket) (ClientAdoptionCurrentJob, bool) {
	return stub.value, true
}

func TestClientRemovalRequestFailsClosedAndSanitizesUntrustedStopReason(t *testing.T) {
	meta, discovery, source, _ := reconciledSingleFile(t)
	const canary = "REMOVAL-STOP-SECRET-CANARY"
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		ClientRemoval: ClientRemovalSelection{Requested: true, StopReason: canary},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "incomplete" || report.Ledgers.Removal.Status != "incomplete" ||
		report.Ledgers.Removal.StopReason != "removal_completion_load_failed" ||
		report.Ledgers.Removal.ProcessLocalCompletionProof || strings.Contains(string(raw), canary) {
		t.Fatalf("unsafe removal failure report: %s", raw)
	}

	unexpected, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		ClientRemoval: ClientRemovalSelection{StopReason: canary},
	})
	if err != nil {
		t.Fatal(err)
	}
	if unexpected.Outcome != "incomplete" || unexpected.Ledgers.Removal.StopReason != "removal_unexpected_activity" ||
		!containsFinding(unexpected.Blockers, "removal.input_inconsistent") {
		t.Fatalf("unexpected removal activity was ignored: %#v", unexpected)
	}
}

func TestClientStopRequestFailsClosedAndSanitizesUntrustedStopReason(t *testing.T) {
	meta, discovery, source, _ := reconciledSingleFile(t)
	const canary = "CLIENT-STOP-SECRET-CANARY"
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		ClientStop: ClientStopSelection{Requested: true, StopReason: canary},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "incomplete" || report.Ledgers.Stop.Status != "incomplete" ||
		report.Ledgers.Stop.StopReason != "stop_completion_load_failed" ||
		report.Ledgers.Stop.ProcessLocalCompletionProof || report.Ledgers.Stop.ProcessLocalCurrentProof ||
		strings.Contains(string(raw), canary) {
		t.Fatalf("unsafe stop failure report: %s", raw)
	}

	unexpected, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		ClientStop: ClientStopSelection{StopReason: canary},
	})
	if err != nil {
		t.Fatal(err)
	}
	if unexpected.Outcome != "incomplete" || unexpected.Ledgers.Stop.StopReason != "stop_unexpected_activity" ||
		!containsFinding(unexpected.Blockers, "stop.input_inconsistent") {
		t.Fatalf("unexpected stop activity was ignored: %#v", unexpected)
	}
}

func TestClientStopAssessmentBindsAttributedHistoryToCurrentStoppedJob(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)
	planID := strings.Repeat("c", 24)
	operationDigest := sha256.Sum256([]byte("ptctl-client-stop-operation-v1\x00" + planID))
	stopOperation := "sha256:" + hex.EncodeToString(operationDigest[:])
	now := time.Now().UTC()
	meta := &metafile.MetaInfo{MetafileVariantID: digest, InfoHashV1: strings.Repeat("d", 40)}
	completion := ClientStopCompletion{
		Driver: downloader.DriverQBittorrent, OperationID: stopOperation, PlanID: planID, IntentID: digest,
		CompletionID: other, CompletionBasis: "accepted_response_then_exact_stopped", UseID: digest, JobID: other,
		FileLayoutID: digest, CompleteSnapshotID: other, StoppedJobState: "stoppedUP",
		ClientConfigID: digest, PathMappingID: other, ActivationOperationID: digest, ActivationPlanID: planID,
		ActivationTerminalID: other, MetafileVariantID: digest, InfoHashV1: meta.InfoHashV1,
		MaterializeOperationID: other, MaterializePlanID: planID,
		TargetRootIdentity: "fsbind-v1:" + strings.Repeat("e", 64), FinalObjectIdentity: "fsbind-v1:" + strings.Repeat("f", 64),
		ManifestFiles: 1, ContentBytes: 4, ObservedAtStart: now.Add(time.Second), ObservedAtEnd: now.Add(2 * time.Second),
		Assurance: "same_invocation_bound_canonical_terminal_client_stop_journal_read_without_current_client_inference",
	}
	materialized := MaterializedFinalLedger{Status: "verified_current_final_source", ProcessLocalFinalProof: true,
		Observation: &materialize.FinalObservation{OperationID: other, MaterializePlanID: planID,
			MetafileVariantID: digest, InfoHashV1: meta.InfoHashV1, TargetRootIdentity: completion.TargetRootIdentity,
			FinalObjectIdentity: completion.FinalObjectIdentity, ManifestFiles: 1, ContentBytes: 4}}
	activation := ClientActivationLedger{Status: "historical_completion_current_job_bound", Historical: true,
		ProcessLocalCompletionProof: true, ProcessLocalCurrentUseProof: true,
		Completion: &ClientActivationCompletion{Driver: downloader.DriverQBittorrent, OperationID: digest, PlanID: planID,
			TerminalMarkerID: other, ClientConfigID: digest, PathMappingID: other, JobID: other,
			ObservedAtStart: now.Add(-time.Second), ObservedAtEnd: now},
		CurrentUse: &ClientActivationCurrentUse{Driver: downloader.DriverQBittorrent, UseID: digest, JobID: other,
			FileLayoutID: digest, CompleteSnapshotID: other, JobState: "stoppedUP", JobProgress: 1,
			ObservedAtStart: now.Add(3 * time.Second), ObservedAtEnd: now.Add(4 * time.Second),
			FinalObjectIdentity: completion.FinalObjectIdentity,
			Assurance:           "same_invocation_existing_reconciliation_bracket_bound_to_canonical_terminal_activation_and_exact_final_non_atomic"},
	}
	current := ClientStopCurrentJob{Driver: downloader.DriverQBittorrent, UseID: digest, JobID: other,
		FileLayoutID: digest, CompleteSnapshotID: other, JobState: "stoppedUP", JobProgress: 1,
		ObservedAtStart: activation.CurrentUse.ObservedAtStart, ObservedAtEnd: activation.CurrentUse.ObservedAtEnd,
		FinalObjectIdentity: completion.FinalObjectIdentity,
		Assurance:           "same_invocation_existing_reconciliation_bracket_bound_to_canonical_terminal_client_stop_and_exact_final_with_current_stopped_typed_job_claim_non_atomic_without_job_incarnation_proof"}
	ledger, blockers, warnings := assessClientStop(meta, ClientStopSelection{Requested: true, CompletionAttempted: true,
		Completion: stopCompletionStub{value: completion}, CurrentStopped: stopCurrentStub{value: current}}, materialized, activation)
	if ledger.Status != "historical_stop_current_job_stopped" || !ledger.ProcessLocalCompletionProof ||
		!ledger.ProcessLocalCurrentProof || ledger.Completion == nil || ledger.CurrentStopped == nil || len(blockers) != 0 || len(warnings) < 2 {
		t.Fatalf("ledger=%#v blockers=%#v warnings=%#v", ledger, blockers, warnings)
	}
	if got := overallOutcomeWithStop("not_requested", false, "not_requested", false, "verified_materialized_final", true,
		"exact_unique", "same_location", true, "not_requested", false, "historical_completion_current_job_bound", true,
		ledger.Status, true, "not_requested", false, "not_requested", false, "not_requested", false); got != "consistent" {
		t.Fatalf("terminal stop current stopped did not preserve consistent lattice: %q", got)
	}

	completion.CompletionBasis = "exact_stopped_after_unknown_attempt_causality_unproven"
	unattributed, blockers, _ := assessClientStop(meta, ClientStopSelection{Requested: true, CompletionAttempted: true,
		Completion: stopCompletionStub{value: completion}, CurrentStopped: stopCurrentStub{value: current}}, materialized, activation)
	if unattributed.Status != "historical_stop_causality_unproven" || unattributed.StopReason != "stop_causality_unproven" ||
		!containsFinding(blockers, "stop.causality_unproven") {
		t.Fatalf("unattributed=%#v blockers=%#v", unattributed, blockers)
	}
}

type stopCompletionStub struct{ value ClientStopCompletion }

func (stub stopCompletionStub) ReconciliationStopCompletion() (ClientStopCompletion, bool) {
	return stub.value, true
}

type stopCurrentStub struct{ value ClientStopCurrentJob }

func (stub stopCurrentStub) ReconcileCurrentStopped(ClientActivationCurrentUse) (ClientStopCurrentJob, bool) {
	return stub.value, true
}

func TestClientRemovalAssessmentRequiresAttributedHistoryAndMatchingCurrentAbsence(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)
	planID := strings.Repeat("c", 24)
	now := time.Now().UTC()
	completion := ClientRemovalCompletion{
		Driver: downloader.DriverQBittorrent, OperationID: digest, PlanID: planID, IntentID: digest, CompletionID: other,
		CompletionBasis: "accepted_response_then_exact_absence", UseID: digest, JobID: other,
		FileLayoutID: digest, CompleteSnapshotID: other, ClientConfigID: digest, PathMappingID: other,
		ActivationOperationID: digest, ActivationPlanID: planID, ActivationTerminalID: other,
		MetafileVariantID: digest, MaterializeOperationID: other, MaterializePlanID: planID,
		TargetRootIdentity: "fsbind-v1:" + strings.Repeat("d", 64), FinalObjectIdentity: "fsbind-v1:" + strings.Repeat("e", 64),
		ManifestFiles: 1, ContentBytes: 4, ObservedAtStart: now.Add(time.Second), ObservedAtEnd: now.Add(2 * time.Second),
		Assurance: "same_invocation_bound_canonical_terminal_client_removal_journal_read_without_current_queue_inference",
	}
	meta := &metafile.MetaInfo{MetafileVariantID: digest}
	materialized := MaterializedFinalLedger{Status: "verified_current_final_source", ProcessLocalFinalProof: true,
		Observation: &materialize.FinalObservation{OperationID: other, MaterializePlanID: planID, MetafileVariantID: digest,
			TargetRootIdentity: completion.TargetRootIdentity, FinalObjectIdentity: completion.FinalObjectIdentity,
			ManifestFiles: 1, ContentBytes: 4}}
	activation := ClientActivationLedger{Status: "historical_completion_current_job_absent", Historical: true,
		ProcessLocalCompletionProof: true, ProcessLocalCurrentAbsenceProof: true,
		Completion: &ClientActivationCompletion{Driver: downloader.DriverQBittorrent, OperationID: digest, PlanID: planID,
			TerminalMarkerID: other, ClientConfigID: digest, PathMappingID: other, JobID: other,
			ObservedAtStart: now.Add(-time.Second), ObservedAtEnd: now},
		CurrentAbsence: &ClientActivationCurrentAbsence{Driver: downloader.DriverQBittorrent, UseID: digest, JobID: other,
			FileLayoutID: digest, CompleteSnapshotID: other, FinalObjectIdentity: completion.FinalObjectIdentity,
			ObservedAtStart: now.Add(3 * time.Second), ObservedAtEnd: now.Add(4 * time.Second)},
	}
	ledger, blockers, _ := assessClientRemoval(meta, ClientRemovalSelection{Requested: true, CompletionAttempted: true,
		Completion: removalCompletionStub{value: completion}}, materialized, activation)
	if ledger.Status != "historical_keep_data_removal_current_job_absent" || !ledger.ProcessLocalCompletionProof ||
		len(blockers) != 0 || ledger.Completion == nil {
		t.Fatalf("ledger=%#v blockers=%#v", ledger, blockers)
	}

	completion.CompletionBasis = "exact_absence_after_unknown_attempt_causality_unproven"
	unattributed, blockers, _ := assessClientRemoval(meta, ClientRemovalSelection{Requested: true, CompletionAttempted: true,
		Completion: removalCompletionStub{value: completion}}, materialized, activation)
	if unattributed.Status != "historical_absence_causality_unproven" ||
		unattributed.StopReason != "removal_absence_causality_unproven" || !containsFinding(blockers, "removal.absence_causality_unproven") {
		t.Fatalf("unattributed=%#v blockers=%#v", unattributed, blockers)
	}

	if got := overallOutcome("not_requested", false, "not_requested", false, "verified_materialized_final", true,
		"absent", "not_comparable", true, "not_requested", false, "historical_completion_current_job_absent", true,
		"historical_keep_data_removal_current_job_absent", true, "not_requested", false, "not_requested", false); got != "consistent" {
		t.Fatalf("terminal removal current absence did not close the expected lattice: %q", got)
	}
}

type removalCompletionStub struct{ value ClientRemovalCompletion }

func (stub removalCompletionStub) ReconciliationRemovalCompletion() (ClientRemovalCompletion, bool) {
	return stub.value, true
}

func TestSourceRetirementRequestFailsClosedAndSanitizesUntrustedStopReason(t *testing.T) {
	meta, discovery, source, _ := reconciledSingleFile(t)
	const canary = "RETIREMENT-STOP-SECRET-CANARY"
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		SourceRetirement: SourceRetirementSelection{Requested: true, StopReason: canary},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "incomplete" || report.Ledgers.Retirement.Status != "incomplete" ||
		report.Ledgers.Retirement.StopReason != "retirement_completion_load_failed" ||
		report.Ledgers.Retirement.ProcessLocalCompletionProof || report.Ledgers.Retirement.ProcessLocalAbsenceProof ||
		strings.Contains(string(raw), canary) || slices.Contains(report.Effect, "read_source_retirement_operation_state") ||
		slices.Contains(report.Effect, "read_retired_source_name_absence") {
		t.Fatalf("unsafe source-retirement failure report: %s", raw)
	}

	attempted, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client: ClientBracket{Requested: true, StopReason: "client_snapshot_incomplete"},
		SourceRetirement: SourceRetirementSelection{
			Requested: true, CompletionAttempted: true, AbsenceAttempted: true, StopReason: canary,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(attempted.Effect, "read_source_retirement_operation_state") ||
		!slices.Contains(attempted.Effect, "read_retired_source_name_absence") || slices.Contains(attempted.Effect, "read_downloader_state") {
		t.Fatalf("report effects did not distinguish attempted local reads from a skipped client gate: %#v", attempted.Effect)
	}

	unexpected, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		SourceRetirement: SourceRetirementSelection{StopReason: canary},
	})
	if err != nil {
		t.Fatal(err)
	}
	if unexpected.Outcome != "incomplete" || unexpected.Ledgers.Retirement.StopReason != "retirement_unexpected_activity" ||
		!containsFinding(unexpected.Blockers, "retirement.input_inconsistent") {
		t.Fatalf("unexpected source-retirement activity was ignored: %#v", unexpected)
	}

	selectedElsewhere, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		SourceRetirement: SourceRetirementSelection{
			Requested: true, CompletionAttempted: true, Completion: retirementCompletionStub{value: validRetirementCompletionFixture()},
			StopReason: "retirement_completion_selector_mismatch",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if selectedElsewhere.Outcome != "conflict" || selectedElsewhere.Ledgers.Retirement.Status != "selected_retirement_mismatch" ||
		!selectedElsewhere.Ledgers.Retirement.ProcessLocalCompletionProof || !selectedElsewhere.Ledgers.Retirement.Historical ||
		!containsFinding(selectedElsewhere.Blockers, "retirement.selection_mismatch") {
		t.Fatalf("verified retirement from another lineage was not preserved as a conflict: %#v", selectedElsewhere)
	}
}

type retirementCompletionStub struct {
	value SourceRetirementCompletion
}

func (stub retirementCompletionStub) ReconciliationRetirementCompletion() (SourceRetirementCompletion, bool) {
	return stub.value, true
}

type retirementAbsenceStub struct {
	value SourceRetirementCurrentAbsence
}

func (stub retirementAbsenceStub) ReconcileCurrentRetiredNameAbsence() (SourceRetirementCurrentAbsence, bool) {
	return stub.value, true
}

func validRetirementCompletionFixture() SourceRetirementCompletion {
	digest := "sha256:" + strings.Repeat("a", 64)
	return SourceRetirementCompletion{
		OperationID: digest, PlanID: digest, IntentID: digest, CompletionID: digest, SearchScopeID: digest,
		MetafileVariantID: digest, MaterializeOperationID: digest, MaterializePlanID: strings.Repeat("b", 24),
		ActivationOperationID: digest, ActivationPlanID: strings.Repeat("c", 24), ClientCompletionID: digest,
		CurrentClientUseID: digest, SourceSelectionID: digest, TargetRootIdentity: "fsbind-v1:" + strings.Repeat("d", 64),
		FinalObjectIdentity: "fsbind-v1:" + strings.Repeat("e", 64), ClientSnapshotID: digest,
		FilesRetired: 1, BytesRetired: 1,
		Assurance: "same_invocation_bound_canonical_terminal_source_retirement_journal_read_without_current_absence_inference",
	}
}

func TestRetainedRetirementCanUseMatchingLiveParentCleanupAbsence(t *testing.T) {
	completion := validRetirementCompletionFixture()
	completion.RetainedTombstone = true
	completion.Assurance = "same_invocation_bound_canonical_source_retirement_retention_tombstone_read_without_current_absence_inference"
	now := time.Now().UTC()
	absence := SourceRetirementCurrentAbsence{
		OperationID: completion.OperationID, PlanID: completion.PlanID, CompletionID: completion.CompletionID,
		AbsenceID: "sha256:" + strings.Repeat("9", 64), SearchScopeID: completion.SearchScopeID,
		FilesChecked: completion.FilesRetired, BytesRetired: completion.BytesRetired, ParentDirectoriesChecked: 1,
		ObservedAtStart: now, ObservedAtEnd: now.Add(time.Millisecond),
		Assurance: "same_invocation_two_pass_identity_bound_removed_parent_absence_implies_retired_name_absence_bracketed_non_atomic",
	}
	meta := &metafile.MetaInfo{MetafileVariantID: completion.MetafileVariantID}
	materialized := MaterializedFinalLedger{
		Status: "verified_current_final_source", ProcessLocalFinalProof: true,
		Observation: &materialize.FinalObservation{
			OperationID: completion.MaterializeOperationID, MaterializePlanID: completion.MaterializePlanID,
			MetafileVariantID: completion.MetafileVariantID, TargetRootIdentity: completion.TargetRootIdentity,
			FinalObjectIdentity: completion.FinalObjectIdentity,
		},
	}
	activation := ClientActivationLedger{
		Status: "historical_completion_current_job_bound", ProcessLocalCompletionProof: true, ProcessLocalCurrentUseProof: true,
		Completion: &ClientActivationCompletion{
			OperationID: completion.ActivationOperationID, PlanID: completion.ActivationPlanID,
			TerminalMarkerID: completion.ClientCompletionID,
		},
		CurrentUse: &ClientActivationCurrentUse{UseID: completion.CurrentClientUseID, CompleteSnapshotID: completion.ClientSnapshotID},
	}
	ledger, blockers, warnings := assessSourceRetirement(meta, SourceRetirementSelection{
		Requested: true, CompletionAttempted: true, AbsenceAttempted: true,
		Completion: retirementCompletionStub{value: completion}, CurrentAbsence: retirementAbsenceStub{value: absence},
	}, materialized, activation)
	if ledger.Status != "historical_completion_current_absence_observed" || ledger.CurrentAbsence == nil ||
		!ledger.ProcessLocalCompletionProof || !ledger.ProcessLocalAbsenceProof || len(blockers) != 0 ||
		!slices.Contains(warnings, "the retained retirement tombstone supplied only historical lineage; current retired-name absence was implied by a matching live parent-cleanup absence proof") {
		t.Fatalf("ledger=%#v blockers=%#v warnings=%#v", ledger, blockers, warnings)
	}

	absence.Assurance = "same_invocation_two_pass_identity_bound_retired_name_absence_bracketed_non_atomic"
	rejected, blockers, _ := assessSourceRetirement(meta, SourceRetirementSelection{
		Requested: true, CompletionAttempted: true, AbsenceAttempted: true,
		Completion: retirementCompletionStub{value: completion}, CurrentAbsence: retirementAbsenceStub{value: absence},
	}, materialized, activation)
	if rejected.Status != "historical_completion_current_absence_unobserved" || rejected.ProcessLocalAbsenceProof ||
		rejected.CurrentAbsence != nil || !containsFinding(blockers, "retirement.current_absence_unavailable") {
		t.Fatalf("retained retirement accepted direct path authority: ledger=%#v blockers=%#v", rejected, blockers)
	}
}

func TestParentCleanupRequestFailsClosedAndSanitizesUntrustedStopReason(t *testing.T) {
	meta, discovery, source, _ := reconciledSingleFile(t)
	const canary = "PARENT-CLEANUP-STOP-SECRET-CANARY"
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		ParentCleanup: ParentCleanupSelection{Requested: true, StopReason: canary},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "incomplete" || report.Ledgers.ParentCleanup.Status != "incomplete" ||
		report.Ledgers.ParentCleanup.StopReason != "parent_cleanup_completion_load_failed" ||
		report.Ledgers.ParentCleanup.ProcessLocalCompletionProof || report.Ledgers.ParentCleanup.ProcessLocalAbsenceProof ||
		strings.Contains(string(raw), canary) || slices.Contains(report.Effect, "read_parent_cleanup_operation_state") ||
		slices.Contains(report.Effect, "read_removed_parent_name_absence") {
		t.Fatalf("unsafe parent-cleanup failure report: %s", raw)
	}

	attempted, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client: ClientBracket{Requested: true, StopReason: "client_snapshot_incomplete"},
		ParentCleanup: ParentCleanupSelection{
			Requested: true, CompletionAttempted: true, AbsenceAttempted: true, StopReason: canary,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(attempted.Effect, "read_parent_cleanup_operation_state") ||
		!slices.Contains(attempted.Effect, "read_removed_parent_name_absence") || slices.Contains(attempted.Effect, "read_downloader_state") {
		t.Fatalf("report effects did not distinguish attempted parent-cleanup reads from a skipped client gate: %#v", attempted.Effect)
	}

	unexpected, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		ParentCleanup: ParentCleanupSelection{StopReason: canary},
	})
	if err != nil {
		t.Fatal(err)
	}
	if unexpected.Outcome != "incomplete" || unexpected.Ledgers.ParentCleanup.StopReason != "parent_cleanup_unexpected_activity" ||
		!containsFinding(unexpected.Blockers, "parent_cleanup.input_inconsistent") {
		t.Fatalf("unexpected parent-cleanup activity was ignored: %#v", unexpected)
	}
}

func TestParentCleanupRequestRequiresMatchingHistoricalAndCurrentProof(t *testing.T) {
	retirementCompletion := validRetirementCompletionFixture()
	retirement := SourceRetirementLedger{
		Status: "historical_completion_current_absence_observed", Completion: &retirementCompletion,
		CurrentAbsence:              &SourceRetirementCurrentAbsence{AbsenceID: "sha256:" + strings.Repeat("f", 64)},
		ProcessLocalCompletionProof: true, ProcessLocalAbsenceProof: true, Historical: true,
	}
	completion := validParentCleanupCompletionFixture(retirementCompletion)
	now := time.Now().UTC()
	absence := ParentCleanupCurrentAbsence{
		OperationID: completion.OperationID, CleanupPlanID: completion.CleanupPlanID, CompletionID: completion.CompletionID,
		AbsenceID: "sha256:" + strings.Repeat("9", 64), SearchScopeID: completion.SearchScopeID,
		ParentsChecked: completion.ParentsRemoved, RetiredFiles: completion.RetiredFiles,
		ObservedAtStart: now, ObservedAtEnd: now.Add(time.Millisecond),
		Assurance: "same_invocation_two_pass_identity_bound_removed_parent_name_absence_bracketed_non_atomic",
	}
	ledger, blockers, warnings := assessParentCleanup(ParentCleanupSelection{
		Requested: true, CompletionAttempted: true, AbsenceAttempted: true,
		Completion: parentCleanupCompletionStub{value: completion}, CurrentAbsence: parentCleanupAbsenceStub{value: absence},
	}, retirement)
	if ledger.Status != "historical_completion_current_absence_observed" || !ledger.ProcessLocalCompletionProof ||
		!ledger.ProcessLocalAbsenceProof || ledger.Completion == nil || ledger.CurrentAbsence == nil || len(blockers) != 0 || len(warnings) < 2 {
		t.Fatalf("ledger=%#v blockers=%#v warnings=%#v", ledger, blockers, warnings)
	}
	selectorMismatch, blockers, _ := assessParentCleanup(ParentCleanupSelection{
		Requested: true, CompletionAttempted: true, Completion: parentCleanupCompletionStub{value: completion},
		StopReason: "parent_cleanup_completion_selector_mismatch",
	}, retirement)
	if selectorMismatch.Status != "selected_parent_cleanup_mismatch" || !selectorMismatch.ProcessLocalCompletionProof ||
		!containsFinding(blockers, "parent_cleanup.selection_mismatch") {
		t.Fatalf("selector mismatch=%#v blockers=%#v", selectorMismatch, blockers)
	}
	integrityFailed, blockers, _ := assessParentCleanup(ParentCleanupSelection{
		Requested: true, CompletionAttempted: true, Completion: parentCleanupCompletionStub{value: completion},
		StopReason: "parent_cleanup_completion_integrity_failed",
	}, retirement)
	if integrityFailed.Status != "integrity_failed" || !integrityFailed.ProcessLocalCompletionProof ||
		!containsFinding(blockers, "parent_cleanup.completion_integrity_failed") {
		t.Fatalf("integrity failed=%#v blockers=%#v", integrityFailed, blockers)
	}
	if got := overallOutcome("not_requested", false, "not_requested", false, "verified_materialized_final", true,
		"exact_unique", "same_location", true, "not_requested", false, "historical_completion_current_job_bound", true,
		"not_requested", false, retirement.Status, true, ledger.Status, true); got != "consistent" {
		t.Fatalf("parent cleanup did not preserve a fully proven consistent lattice: %q", got)
	}

	reappeared, blockers, _ := assessParentCleanup(ParentCleanupSelection{
		Requested: true, CompletionAttempted: true, Completion: parentCleanupCompletionStub{value: completion},
		StopReason: "parent_cleanup_removed_parent_reappeared",
	}, retirement)
	if reappeared.Status != "removed_parent_reappeared" || !containsFinding(blockers, "parent_cleanup.removed_parent_reappeared") {
		t.Fatalf("reappeared=%#v blockers=%#v", reappeared, blockers)
	}
	if got := overallOutcome("not_requested", false, "not_requested", false, "verified_ambiguous", true,
		"ambiguous", "not_comparable", true, "not_requested", false, "historical_completion_current_job_bound", true,
		"not_requested", false, retirement.Status, true, reappeared.Status, true); got != "conflict" {
		t.Fatalf("current parent conflict was hidden by ambiguity: %q", got)
	}

	completion.RetainedTombstone = true
	completion.Assurance = "same_invocation_bound_canonical_parent_cleanup_retention_tombstone_read_without_current_absence_inference"
	retained, blockers, _ := assessParentCleanup(ParentCleanupSelection{
		Requested: true, CompletionAttempted: true, Completion: parentCleanupCompletionStub{value: completion},
	}, retirement)
	if retained.Status != "historical_completion_current_absence_unobserved" || retained.StopReason != "parent_cleanup_current_absence_unavailable" ||
		!containsFinding(blockers, "parent_cleanup.current_absence_unavailable") {
		t.Fatalf("retained=%#v blockers=%#v", retained, blockers)
	}
}

type parentCleanupCompletionStub struct{ value ParentCleanupCompletion }

func (stub parentCleanupCompletionStub) ReconciliationParentCleanupCompletion() (ParentCleanupCompletion, bool) {
	return stub.value, true
}

type parentCleanupAbsenceStub struct{ value ParentCleanupCurrentAbsence }

func (stub parentCleanupAbsenceStub) ReconcileCurrentRemovedParentAbsence() (ParentCleanupCurrentAbsence, bool) {
	return stub.value, true
}

func validParentCleanupCompletionFixture(retirement SourceRetirementCompletion) ParentCleanupCompletion {
	planID := "sha256:" + strings.Repeat("7", 64)
	digest := sha256.Sum256([]byte("ptctl-source-retirement-parent-cleanup-operation-v1\x00" + planID))
	return ParentCleanupCompletion{
		OperationID: "sha256:" + hex.EncodeToString(digest[:]), CleanupPlanID: planID,
		IntentID: "sha256:" + strings.Repeat("8", 64), CompletionID: "sha256:" + strings.Repeat("6", 64),
		RetirementOperationID: retirement.OperationID, RetirementPlanID: retirement.PlanID,
		RetirementCompletionID: retirement.CompletionID, SearchScopeID: retirement.SearchScopeID,
		TargetRootIdentity: retirement.TargetRootIdentity, ParentsRemoved: 1, RetiredFiles: retirement.FilesRetired,
		Assurance: "same_invocation_bound_canonical_terminal_parent_cleanup_journal_read_without_current_absence_inference",
	}
}

func containsFinding(values []ReportFinding, code string) bool {
	for _, value := range values {
		if value.Code == code {
			return true
		}
	}
	return false
}

func containsWarning(values []string, fragment string) bool {
	for _, value := range values {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}

func TestExplicitSealedSiteBindingAddsHistoricalAxisWithoutUpgradingLocalProof(t *testing.T) {
	meta, discovery, source, root := reconciledSingleFile(t)
	recordID, ref, verified := sealedSiteBindingForMeta(t, meta)
	job := matchingJob(meta, "/downloads/renamed.bin")
	before, after := ledgerPair(job)
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client:      ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3},
		SiteRef:     &ref,
		SiteBinding: SiteBindingSelection{Requested: true, RecordID: recordID, Verified: verified},
		PathMapping: testPathMapping(root),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "consistent" || relationStatus(report, "site_metafile") != "historical_observed_exact_variant" ||
		!report.Ledgers.Site.ProcessLocalProof || !report.Ledgers.Site.Historical ||
		report.Ledgers.Site.BindingRecordID != recordID.String() || !report.Scope.SiteBindingRequested ||
		report.Scope.SiteBindingSelector != "explicit_record" || !strings.Contains(report.Assurance, "sealed_historical_site_observation") {
		t.Fatalf("historical site proof was not represented safely: %#v", report)
	}

	encoded, err := json.Marshal(verified)
	if err != nil {
		t.Fatal(err)
	}
	var replay sitebinding.VerifiedSiteBinding
	if err := json.Unmarshal(encoded, &replay); err != nil {
		t.Fatal(err)
	}
	replayed, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		SiteBinding: SiteBindingSelection{Requested: true, RecordID: recordID, Verified: &replay},
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Outcome != "incomplete" || relationStatus(replayed, "site_metafile") != "incomplete" || replayed.Ledgers.Site.ProcessLocalProof {
		t.Fatalf("serialized site binding regained authority: %#v", replayed)
	}

	localOnly, err := Build(BuildInput{
		Meta: meta, Discovery: seed.DiscoveryResult{SourceOutcome: "incomplete"},
		SiteBinding: SiteBindingSelection{Requested: true, RecordID: recordID, Verified: verified},
	})
	if err != nil {
		t.Fatal(err)
	}
	if localOnly.Outcome != "incomplete" || relationStatus(localOnly, "site_metafile") != "historical_observed_exact_variant" {
		t.Fatalf("site proof improperly upgraded missing storage proof: %#v", localOnly)
	}
}

func TestLiveSiteDetailAddsCurrentRefClaimWithoutVariantProof(t *testing.T) {
	meta, discovery, source, root := reconciledSingleFile(t)
	ref := domain.TorrentRef{SiteID: "tjupt", RemoteID: "123"}
	observed, receipt := observedDetailForRef(t, ref)
	job := matchingJob(meta, "/downloads/renamed.bin")
	before, after := ledgerPair(job)
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client:  ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3},
		SiteRef: &ref,
		SiteDetail: SiteDetailSelection{
			Requested: true, Config: site.TorrentDetailConfig{Origin: receipt.Origin, RouteID: receipt.RouteID},
			Observed: observed, Receipt: receipt, RequestsMade: 1,
		},
		PathMapping: testPathMapping(root),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "consistent" || relationStatus(report, "site_metafile") != "declared_unbound" ||
		!report.Scope.SiteDetailRequested || report.Ledgers.Site.Detail.Status != "observed_current_ref" ||
		!report.Ledgers.Site.Detail.ProcessLocalProof || report.Ledgers.Site.Detail.Observation == nil ||
		!strings.Contains(report.Assurance, "same_invocation_current_site_ref_claim") {
		t.Fatalf("live detail was not represented as a separate claim: %#v", report)
	}
	for _, basis := range report.Relations[0].EvidenceBasis {
		if basis == "metafile_variant_observed" || basis == "private_metafile_observed" {
			t.Fatalf("live detail was promoted to variant proof: %#v", report.Relations[0])
		}
	}

	raw, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	var replay site.ObservedTorrentDetail
	if err := json.Unmarshal(raw, &replay); err != nil {
		t.Fatal(err)
	}
	replayed, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source, SiteRef: &ref,
		SiteDetail: SiteDetailSelection{
			Requested: true, Config: site.TorrentDetailConfig{Origin: receipt.Origin, RouteID: receipt.RouteID},
			Observed: &replay, Receipt: receipt, RequestsMade: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Outcome != "incomplete" || replayed.Ledgers.Site.Detail.Status != "incomplete" || replayed.Ledgers.Site.Detail.ProcessLocalProof {
		t.Fatalf("serialized detail regained authority: %#v", replayed)
	}
	wrongConfig, err := site.NewTorrentDetailConfig("https://other.invalid", "other.details_by_id.v1")
	if err != nil {
		t.Fatal(err)
	}
	wrongOrigin, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source, SiteRef: &ref,
		SiteDetail: SiteDetailSelection{Requested: true, Config: wrongConfig, Observed: observed, Receipt: receipt, RequestsMade: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wrongOrigin.Outcome != "incomplete" || wrongOrigin.Ledgers.Site.Detail.Status != "incomplete" || wrongOrigin.Ledgers.Site.Detail.ProcessLocalProof {
		t.Fatalf("observation crossed its pinned site origin/route: %#v", wrongOrigin)
	}
}

func TestLiveSiteDetailFailureIsIncompleteAndSanitizesAdapterData(t *testing.T) {
	meta, discovery, source, _ := reconciledSingleFile(t)
	ref := domain.TorrentRef{SiteID: "tjupt", RemoteID: "123"}
	const canary = "SITE-DETAIL-RECEIPT-CANARY"
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source, SiteRef: &ref,
		SiteDetail: SiteDetailSelection{
			Requested: true, StopReason: canary, RequestsMade: 999,
			Receipt: site.TorrentDetailReceipt{
				Ref: domain.TorrentRef{SiteID: canary, RemoteID: canary}, Origin: "https://" + canary, RouteID: canary,
				Used: site.TorrentDetailUsage{RequestsAttempted: 999, ResponseBytesRead: 1 << 40},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "incomplete" || report.Ledgers.Site.Detail.Status != "incomplete" || report.Ledgers.Site.Detail.ProcessLocalProof ||
		report.Ledgers.Site.Detail.RequestsMade != 2 || strings.Contains(string(encoded), canary) {
		t.Fatalf("unsafe failed live detail report: %s", encoded)
	}
}

func TestExplicitSiteBindingMismatchAndIntegrityGateOverallOutcome(t *testing.T) {
	meta, discovery, source, _ := reconciledSingleFile(t)
	recordID, ref, verified := sealedSiteBindingForMeta(t, meta)
	other := ref
	other.RemoteID = "124"
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source, SiteRef: &other,
		SiteBinding: SiteBindingSelection{Requested: true, RecordID: recordID, Verified: verified},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "conflict" || relationStatus(report, "site_metafile") != "selected_binding_mismatch" {
		t.Fatalf("explicit ref mismatch was not a conflict: %#v", report)
	}
	report, err = Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		SiteBinding: SiteBindingSelection{Requested: true, RecordID: recordID, StopReason: "site_binding_integrity_failed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "integrity_failed" || relationStatus(report, "site_metafile") != "integrity_failed" {
		t.Fatalf("binding corruption did not gate the report: %#v", report)
	}
}

func TestOverallConflictOutranksAmbiguityAcrossAxes(t *testing.T) {
	tests := []struct {
		name          string
		storageStatus string
		clientStatus  string
		pathStatus    string
	}{
		{name: "client identity conflict with ambiguous storage", storageStatus: "verified_ambiguous", clientStatus: "conflict", pathStatus: "not_comparable"},
		{name: "client size conflict with ambiguous client identity", storageStatus: "verified_unique", clientStatus: "ambiguous", pathStatus: "client_size_conflict"},
		{name: "client file layout conflict with ambiguous storage", storageStatus: "verified_ambiguous", clientStatus: "exact_unique", pathStatus: "client_file_layout_conflict"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := overallOutcome("not_requested", false, "not_requested", false, test.storageStatus, true, test.clientStatus, test.pathStatus, true, "not_requested", false, "not_requested", false, "not_requested", false, "not_requested", false, "not_requested", false); got != "conflict" {
				t.Fatalf("positive contradiction was hidden by ambiguity: got %q", got)
			}
		})
	}
	if got := overallOutcome("not_requested", false, "not_requested", false, "verified_materialized_final", true,
		"exact_unique", "same_location", true, "not_requested", false, "selected_activation_mismatch", true, "not_requested", false, "not_requested", false, "not_requested", false); got != "conflict" {
		t.Fatalf("activation mismatch did not gate the lattice: %q", got)
	}
	if got := overallOutcome("not_requested", false, "not_requested", false, "verified_materialized_final", true,
		"exact_unique", "same_location", true, "selected_adoption_mismatch", true, "not_requested", false, "not_requested", false, "not_requested", false, "not_requested", false); got != "conflict" {
		t.Fatalf("adoption mismatch did not gate the lattice: %q", got)
	}
	if got := overallOutcomeWithStop("not_requested", false, "not_requested", false, "verified_ambiguous", true,
		"conflict", "not_comparable", true, "not_requested", false, "historical_completion_current_job_bound", true,
		"incomplete", true, "not_requested", false, "not_requested", false, "not_requested", false); got != "conflict" {
		t.Fatalf("a stop prerequisite failure hid a positive client conflict: %q", got)
	}
	if got := overallOutcomeWithStop("not_requested", false, "not_requested", false, "verified_materialized_final", true,
		"exact_unique", "same_location", true, "not_requested", false, "historical_completion_current_job_bound", true,
		"integrity_failed", true, "not_requested", false, "not_requested", false, "not_requested", false); got != "integrity_failed" {
		t.Fatalf("stop integrity failure did not outrank otherwise consistent axes: %q", got)
	}
	if got := overallOutcomeWithStop("not_requested", false, "not_requested", false, "verified_materialized_final", true,
		"exact_unique", "same_location", true, "not_requested", false, "historical_completion_current_job_bound", true,
		"selected_stop_mismatch", true, "not_requested", false, "not_requested", false, "not_requested", false); got != "conflict" {
		t.Fatalf("stop selector mismatch did not become conflict: %q", got)
	}
	if got := overallOutcome("not_requested", false, "not_requested", false, "verified_materialized_final", true,
		"exact_unique", "same_location", true, "historical_completion_current_job_unbound", true, "not_requested", false, "not_requested", false, "not_requested", false, "not_requested", false); got != "incomplete" {
		t.Fatalf("unbound requested adoption did not make the report incomplete: %q", got)
	}
	if got := overallOutcome("not_requested", false, "not_requested", false, "verified_materialized_final", true,
		"exact_unique", "same_location", true, "not_requested", false, "historical_completion_current_job_unbound", true, "not_requested", false, "not_requested", false, "not_requested", false); got != "incomplete" {
		t.Fatalf("unbound requested activation did not make the report incomplete: %q", got)
	}
	if got := overallOutcome("not_requested", false, "not_requested", false, "verified_materialized_final", true,
		"exact_unique", "same_location", true, "not_requested", false, "historical_completion_current_job_bound", true, "not_requested", false, "source_name_reappeared", true, "not_requested", false); got != "conflict" {
		t.Fatalf("reappeared retired source name did not gate the lattice: %q", got)
	}
	if got := overallOutcome("not_requested", false, "not_requested", false, "verified_materialized_final", true,
		"exact_unique", "same_location", true, "not_requested", false, "historical_completion_current_job_bound", true,
		"not_requested", false, "historical_completion_current_absence_unobserved", true, "not_requested", false); got != "incomplete" {
		t.Fatalf("unobserved requested retirement absence did not make the report incomplete: %q", got)
	}
}

func observedDetailForRef(t *testing.T, ref domain.TorrentRef) (*site.ObservedTorrentDetail, site.TorrentDetailReceipt) {
	t.Helper()
	start := time.Date(2026, 8, 11, 4, 5, 6, 0, time.UTC)
	receipt := site.TorrentDetailReceipt{
		Effect: site.TorrentDetailReadEffect, Ref: ref, Origin: "https://www.tjupt.org", RouteID: "tjupt.details_by_id_no_hit.v1",
		ObservedAtStart: start, ObservedAtEnd: start.Add(time.Second), Complete: true,
		Limits: site.DefaultTorrentDetailLimits(),
		Used:   site.TorrentDetailUsage{RequestsAttempted: 1, ResponseBytesRead: 1024, ResponseBytesKnown: true},
	}
	detail := domain.TorrentDetail{
		Ref: ref, DisplayTitle: "Observed release", DownloadReferenceObserved: true,
		EvidenceBasis: []string{site.DetailBasisAuthenticated, site.DetailBasisExactRef, site.DetailBasisOneRequest, site.DetailBasisSiteClaimOnly, site.DetailBasisDownloadRef},
	}
	observed, err := site.NewObservedTorrentDetail(detail, receipt)
	if err != nil {
		t.Fatal(err)
	}
	return observed, receipt
}

func TestProofFromAnotherDiscoveryCannotBackSelectedPath(t *testing.T) {
	meta, discovery, _, _ := reconciledSingleFile(t)
	otherRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(otherRoot, "another-copy.bin"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	other, err := seed.Discover(context.Background(), meta, seed.DiscoverOptions{
		SearchRoots: []string{otherRoot}, InventoryLimits: storage.DefaultInventoryLimits(), MatchLimits: metafile.DefaultSourceMatchLimits(), Strategy: "copy",
	})
	if err != nil {
		t.Fatal(err)
	}
	otherSource, ok := other.VerifiedSource(meta)
	if !ok {
		t.Fatal("second discovery did not verify")
	}
	report, err := Build(BuildInput{Meta: meta, Discovery: discovery, VerifiedSource: otherSource})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "incomplete" || report.Ledgers.Storage.ProcessLocalProof {
		t.Fatalf("proof from a different source selection was accepted: %#v", report)
	}
}

func TestMutatedSelectedIDInvalidatesProcessLocalProof(t *testing.T) {
	meta, discovery, source, _ := reconciledSingleFile(t)
	discovery.Selection.SelectedID = "forged-selection"
	report, err := Build(BuildInput{Meta: meta, Discovery: discovery, VerifiedSource: source})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "incomplete" || report.Ledgers.Storage.ProcessLocalProof {
		t.Fatalf("a mutated selection ID retained proof authority: %#v", report)
	}
}

func TestClientDuplicateAndBracketMutationFailClosed(t *testing.T) {
	meta, discovery, source, root := reconciledSingleFile(t)
	first := matchingJob(meta, "/downloads/renamed.bin")
	second := matchingJob(meta, "/downloads/copy.bin")
	second.Hash = "opaque-job-two"
	before, after := ledgerPair(first, second)
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client:      ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3},
		PathMapping: testPathMapping(root),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "ambiguous" || relationStatus(report, "client_infohash_relation") != "ambiguous" {
		t.Fatalf("duplicate exact jobs were not ambiguous: %#v", report)
	}

	before, after = ledgerPair(first)
	after.Jobs[0].ContentPath = "/downloads/moved.bin"
	report, err = Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client:      ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3},
		PathMapping: testPathMapping(root),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "incomplete" || relationStatus(report, "client_infohash_relation") != "incomplete" || report.Ledgers.Downloader.Status != "unstable" {
		t.Fatalf("bracket mutation was not fail-closed: %#v", report)
	}
}

func TestUnrelatedQueueChangeDoesNotDestabilizeTargetIdentity(t *testing.T) {
	meta, discovery, source, root := reconciledSingleFile(t)
	target := matchingJob(meta, "/downloads/renamed.bin")
	before, after := ledgerPair(target)
	unrelated := target
	unrelated.Hash = "unrelated-job"
	unrelated.InfoHashV1 = strings.Repeat("e", 40)
	unrelated.ContentPath = "/downloads/unrelated.bin"
	after.Jobs = append(after.Jobs, unrelated)
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client:      ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3},
		PathMapping: testPathMapping(root),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "consistent" || report.Ledgers.Downloader.Status != "observed_stable" || report.Ledgers.Downloader.JobsExaminedBefore != 1 || report.Ledgers.Downloader.JobsExaminedAfter != 2 {
		t.Fatalf("unrelated queue activity destabilized the target relation: %#v", report)
	}
}

func TestActiveOrChangingClientJobCannotProduceConsistency(t *testing.T) {
	meta, discovery, source, root := reconciledSingleFile(t)
	job := matchingJob(meta, "/downloads/renamed.bin")
	job.State = "downloading"
	job.Progress = 0.25
	before, after := ledgerPair(job)
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client:      ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3},
		PathMapping: testPathMapping(root),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "partial" || relationStatus(report, "client_infohash_relation") != "exact_unique" || relationStatus(report, "verified_source_vs_job_path") != "client_content_unsettled" {
		t.Fatalf("active download produced consistency: %#v", report)
	}

	job = matchingJob(meta, "/downloads/renamed.bin")
	before, after = ledgerPair(job)
	after.Jobs[0].State = "stalledUP"
	report, err = Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client:      ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3},
		PathMapping: testPathMapping(root),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "incomplete" || report.Ledgers.Downloader.Status != "unstable" {
		t.Fatalf("changing job state was not detected: %#v", report)
	}
}

func TestDownloaderCapabilitiesGateClaims(t *testing.T) {
	meta, discovery, source, root := reconciledSingleFile(t)
	job := matchingJob(meta, "/downloads/renamed.bin")
	before, after := ledgerPair(job)
	before.Capabilities.TypedInfoHashes = false
	after.Capabilities.TypedInfoHashes = false
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client:      ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3},
		PathMapping: testPathMapping(root),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "incomplete" || relationStatus(report, "client_infohash_relation") != "incomplete" {
		t.Fatalf("typed fields bypassed a missing capability: %#v", report)
	}

	before, after = ledgerPair(job)
	before.Capabilities.ContentPath = false
	after.Capabilities.ContentPath = false
	report, err = Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client:      ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3},
		PathMapping: testPathMapping(root),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "partial" || relationStatus(report, "verified_source_vs_job_path") != "unsupported" {
		t.Fatalf("content path bypassed a missing capability: %#v", report)
	}
}

func TestClientSizeMustAgreeBeforePathCanBeConsistent(t *testing.T) {
	meta, discovery, source, root := reconciledSingleFile(t)
	job := matchingJob(meta, "/downloads/renamed.bin")
	job.SizeBytes = 0
	before, after := ledgerPair(job)
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client:      ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3},
		PathMapping: testPathMapping(root),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "conflict" || relationStatus(report, "verified_source_vs_job_path") != "client_size_conflict" {
		t.Fatalf("a conflicting client size was accepted: %#v", report)
	}
}

func TestPathProjectionIgnoresMutableDiscoveryDTOFields(t *testing.T) {
	meta, discovery, source, root := reconciledSingleFile(t)
	discovery.Matches[0].Bindings[0].ClientPath = "/forged/client/path.bin"
	job := matchingJob(meta, "/forged/client/path.bin")
	before, after := ledgerPair(job)
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client:      ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3},
		PathMapping: testPathMapping(root),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome == "consistent" || relationStatus(report, "verified_source_vs_job_path") != "different_location" {
		t.Fatalf("mutable public mapping fields became path authority: %#v", report)
	}
}

func TestUntrustedClientStateAndEvidenceCannotLeak(t *testing.T) {
	const canary = "CANARY-CLIENT-STATE-AND-EVIDENCE"
	meta, discovery, source, root := reconciledSingleFile(t)
	job := matchingJob(meta, "/downloads/renamed.bin")
	job.State = canary
	job.IdentityEvidence = append(job.IdentityEvidence, canary)
	before, after := ledgerPair(job)
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client:      ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3},
		PathMapping: testPathMapping(root),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), canary) || len(report.Ledgers.Downloader.Matches) != 1 || report.Ledgers.Downloader.Matches[0].State != "unknown" {
		t.Fatalf("untrusted client text leaked into report: %s", encoded)
	}
	if report.Outcome == "consistent" {
		t.Fatal("an unknown client state produced consistency")
	}
}

func TestMalformedGenericLedgerSnapshotsFailClosed(t *testing.T) {
	meta, discovery, source, root := reconciledSingleFile(t)
	job := matchingJob(meta, "/downloads/renamed.bin")
	tests := []struct {
		name   string
		mutate func(*downloader.LedgerSnapshot, *downloader.LedgerSnapshot)
	}{
		{name: "unsupported driver", mutate: func(before, after *downloader.LedgerSnapshot) { before.Driver, after.Driver = "other", "other" }},
		{name: "reversed observation time", mutate: func(before, after *downloader.LedgerSnapshot) { after.ObservedAtStart = before.ObservedAtStart }},
		{name: "duplicate job key", mutate: func(before, after *downloader.LedgerSnapshot) {
			before.Jobs = append(before.Jobs, before.Jobs[0])
			after.Jobs = append(after.Jobs, after.Jobs[0])
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before, after := ledgerPair(job)
			test.mutate(&before, &after)
			report, err := Build(BuildInput{
				Meta: meta, Discovery: discovery, VerifiedSource: source,
				Client:      ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3},
				PathMapping: testPathMapping(root),
			})
			if err != nil {
				t.Fatal(err)
			}
			if report.Outcome != "incomplete" || relationStatus(report, "client_infohash_relation") != "incomplete" {
				t.Fatalf("malformed bracket was accepted: %#v", report)
			}
		})
	}
}

func TestTypedIdentityClassificationNeverUsesOpaqueHash(t *testing.T) {
	pureV2 := &metafile.MetaInfo{InfoHashV2: strings.Repeat("b", 64)}
	job := downloader.Torrent{
		Hash: strings.Repeat("b", 40), InfoHashV1: strings.Repeat("a", 40),
		IdentityStatus: downloader.IdentityStatusValid,
	}
	if got := classifyJob(pureV2, job); got != "unrelated" {
		t.Fatalf("opaque/native 40-hex key influenced pure-v2 identity: %s", got)
	}
	hybrid := &metafile.MetaInfo{InfoHashV1: strings.Repeat("a", 40), InfoHashV2: strings.Repeat("b", 64)}
	job.InfoHashV1, job.InfoHashV2 = hybrid.InfoHashV1, ""
	if got := classifyJob(hybrid, job); got != "incomplete" {
		t.Fatalf("hybrid missing v2 claim should be incomplete, got %s", got)
	}
	job.InfoHashV2 = strings.Repeat("c", 64)
	if got := classifyJob(hybrid, job); got != "conflict" {
		t.Fatalf("hybrid v2 mismatch should conflict, got %s", got)
	}
}

func TestTypedIdentityClassificationMatrix(t *testing.T) {
	v1 := strings.Repeat("a", 40)
	v2 := strings.Repeat("b", 64)
	otherV1 := strings.Repeat("c", 40)
	otherV2 := strings.Repeat("d", 64)
	tests := []struct {
		name string
		meta *metafile.MetaInfo
		job  downloader.Torrent
		want string
	}{
		{"pure v1 exact", &metafile.MetaInfo{InfoHashV1: v1}, downloader.Torrent{InfoHashV1: v1}, "exact"},
		{"pure v1 extra family conflicts", &metafile.MetaInfo{InfoHashV1: v1}, downloader.Torrent{InfoHashV1: v1, InfoHashV2: v2}, "conflict"},
		{"pure v2 exact", &metafile.MetaInfo{InfoHashV2: v2}, downloader.Torrent{InfoHashV2: v2}, "exact"},
		{"pure v2 extra family conflicts", &metafile.MetaInfo{InfoHashV2: v2}, downloader.Torrent{InfoHashV1: v1, InfoHashV2: v2}, "conflict"},
		{"hybrid exact", &metafile.MetaInfo{InfoHashV1: v1, InfoHashV2: v2}, downloader.Torrent{InfoHashV1: v1, InfoHashV2: v2}, "exact"},
		{"hybrid missing v2", &metafile.MetaInfo{InfoHashV1: v1, InfoHashV2: v2}, downloader.Torrent{InfoHashV1: v1}, "incomplete"},
		{"hybrid mismatched v2", &metafile.MetaInfo{InfoHashV1: v1, InfoHashV2: v2}, downloader.Torrent{InfoHashV1: v1, InfoHashV2: otherV2}, "conflict"},
		{"hybrid unrelated", &metafile.MetaInfo{InfoHashV1: v1, InfoHashV2: v2}, downloader.Torrent{InfoHashV1: otherV1, InfoHashV2: otherV2}, "unrelated"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyJob(test.meta, test.job); got != test.want {
				t.Fatalf("classifyJob=%s, want %s", got, test.want)
			}
		})
	}
}

func TestScatteredVerifiedFilesAreNotClaimedAsDownloaderContentRoot(t *testing.T) {
	data := []byte("abcdef")
	piece0, piece1 := sha1.Sum(data[:4]), sha1.Sum(data[4:])
	pieces := append(piece0[:], piece1[:]...)
	meta, err := metafile.Parse(testBencode(map[string]any{"info": map[string]any{
		"files": []any{
			map[string]any{"length": int64(3), "path": []any{"a.bin"}},
			map[string]any{"length": int64(3), "path": []any{"b.bin"}},
		},
		"name": "bundle", "piece length": int64(4), "pieces": pieces,
	}}))
	if err != nil {
		t.Fatal(err)
	}
	hostRoot := t.TempDir()
	rootA, rootB := filepath.Join(hostRoot, "a"), filepath.Join(hostRoot, "b")
	if err := os.Mkdir(rootA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(rootB, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootA, "renamed-one"), data[:3], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "renamed-two"), data[3:], 0o600); err != nil {
		t.Fatal(err)
	}
	discovery, err := seed.Discover(context.Background(), meta, seed.DiscoverOptions{
		SearchRoots: []string{rootA, rootB}, InventoryLimits: storage.DefaultInventoryLimits(), MatchLimits: metafile.DefaultSourceMatchLimits(), Strategy: "copy",
		ClientMapping: &seed.ClientMappingOptions{HostRoot: hostRoot, ClientRoot: "/downloads"},
	})
	if err != nil {
		t.Fatal(err)
	}
	source, ok := discovery.VerifiedSource(meta)
	if !ok || len(discovery.Matches) != 1 || discovery.Matches[0].Layout != "scattered_set" {
		t.Fatalf("unexpected discovery: %#v", discovery)
	}
	job := matchingJob(meta, "/downloads/bundle")
	job.SizeBytes = 6
	before, after := ledgerPair(job)
	report, err := Build(BuildInput{
		Meta: meta, Discovery: discovery, VerifiedSource: source,
		Client:      ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3},
		PathMapping: &PathMappingOptions{HostRoot: hostRoot, ClientRoot: "/downloads"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "partial" || relationStatus(report, "storage_content_proof") != "verified_unique" || relationStatus(report, "verified_source_vs_job_path") != "client_file_layout_unobservable" {
		t.Fatalf("scattered bytes were overstated as client layout: %#v", report)
	}
}

func TestClientPathParsingIsCrossPlatformAndStrict(t *testing.T) {
	posix, err := parseClientPath("/downloads/show/file.bin", false)
	if err != nil || posix.canonical() != "/downloads/show/file.bin" {
		t.Fatalf("POSIX path=%#v err=%v", posix, err)
	}
	windows, err := parseClientPath(`D:\Downloads\Show\File.bin`, true)
	if err != nil {
		t.Fatal(err)
	}
	caseVariant, err := parseClientPath(`d:/downloads/show/file.BIN`, true)
	if err != nil || windows.equal(caseVariant) {
		t.Fatalf("Windows paths with different case must remain unproven under unknown per-directory semantics: %#v %#v err=%v", windows, caseVariant, err)
	}
	for _, invalid := range []struct {
		value   string
		windows bool
	}{{"downloads/file", false}, {"/downloads/../file", false}, {`C:\Downloads\..\file`, true}, {`\\server\share\\file`, true}} {
		if _, err := parseClientPath(invalid.value, invalid.windows); err == nil {
			t.Fatalf("accepted invalid client path %q", invalid.value)
		}
	}
}

func TestPathMappingIDUsesCanonicalRoots(t *testing.T) {
	root := t.TempDir()
	plain := PathMappingOptions{HostRoot: root, ClientRoot: "/downloads"}
	equivalent := PathMappingOptions{HostRoot: root + string(os.PathSeparator) + ".", ClientRoot: "/downloads"}
	if pathMappingID(plain) != pathMappingID(equivalent) {
		t.Fatal("equivalent mapping roots produced different IDs")
	}
}

func reconciledSingleFile(t *testing.T) (*metafile.MetaInfo, seed.DiscoveryResult, *metafile.VerifiedSource, string) {
	t.Helper()
	content := []byte("payload")
	piece := sha1.Sum(content)
	meta, err := metafile.Parse(testBencode(map[string]any{
		"announce": "https://tracker.invalid/announce?passkey=CANARY-TRACKER-PASSKEY",
		"info":     map[string]any{"length": int64(len(content)), "name": "source.bin", "piece length": int64(len(content)), "pieces": piece[:], "private": int64(1)},
	}))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "renamed.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	options := seed.DiscoverOptions{
		SearchRoots: []string{root}, InventoryLimits: storage.DefaultInventoryLimits(), MatchLimits: metafile.DefaultSourceMatchLimits(), Strategy: "copy",
		ClientMapping: &seed.ClientMappingOptions{HostRoot: root, ClientRoot: "/downloads"},
	}
	discovery, err := seed.Discover(context.Background(), meta, options)
	if err != nil {
		t.Fatal(err)
	}
	source, ok := discovery.VerifiedSource(meta)
	if !ok {
		t.Fatalf("discovery did not retain process-local proof: %#v", discovery)
	}
	return meta, discovery, source, root
}

func sealedSiteBindingForMeta(t *testing.T, meta *metafile.MetaInfo) (metastore.RecordID, domain.TorrentRef, *sitebinding.VerifiedSiteBinding) {
	t.Helper()
	content := []byte("payload")
	piece := sha1.Sum(content)
	raw := testBencode(map[string]any{
		"announce": "https://tracker.invalid/announce?passkey=CANARY-TRACKER-PASSKEY",
		"info":     map[string]any{"length": int64(len(content)), "name": "source.bin", "piece length": int64(len(content)), "pieces": piece[:], "private": int64(1)},
	})
	parsed, err := metafile.Parse(raw)
	if err != nil || parsed.MetafileVariantID != meta.MetafileVariantID {
		t.Fatalf("site binding fixture does not match metafile: parsed=%v err=%v", parsed, err)
	}
	physical, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	storeRoot := filepath.Join(physical, "binding-store")
	store, _, err := metastore.Init(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	_, artifact, imported, err := store.Import(context.Background(), bytes.NewReader(raw), metastore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.TorrentRef{SiteID: "tjupt", RemoteID: "123"}
	start := time.Date(2026, 8, 10, 5, 6, 7, 0, time.UTC)
	end := start.Add(time.Second)
	fetched, err := site.NewFetchedMetafile(ref, "https://www.tjupt.org", "tjupt.download_by_id.v1", start, end, raw)
	if err != nil {
		t.Fatal(err)
	}
	limits := site.DefaultMetafileFetchLimits()
	fetch := site.MetafileFetchReceipt{
		Effect: site.MetafileFetchEffect, Ref: ref, Origin: "https://www.tjupt.org", RouteID: "tjupt.download_by_id.v1",
		ObservedAtStart: start, ObservedAtEnd: end, Complete: true, Limits: limits,
		Used: site.MetafileFetchUsage{RequestsAttempted: 1, ResponseBytesRead: int64(len(raw)), ResponseBytesKnown: true},
	}
	observed, err := fetched.BindImported(artifact.MetafileVariantID, imported.BytesConsumed)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sitebinding.NewRepository(store, sitebinding.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := repository.Seal(context.Background(), observed, fetch, artifact)
	if err != nil {
		t.Fatal(err)
	}
	verified, _, err := repository.Load(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	return record.ID, ref, verified
}

func testPathMapping(root string) *PathMappingOptions {
	return &PathMappingOptions{HostRoot: root, ClientRoot: "/downloads"}
}

func matchingJob(meta *metafile.MetaInfo, contentPath string) downloader.Torrent {
	evidence := []string{}
	if meta.InfoHashV1 != "" {
		evidence = append(evidence, "magnet_xt_btih_hex")
	}
	if meta.InfoHashV2 != "" {
		evidence = append(evidence, "magnet_xt_btmh_sha256")
	}
	return downloader.Torrent{
		Hash: "opaque-job-one", InfoHashV1: meta.InfoHashV1, InfoHashV2: meta.InfoHashV2,
		IdentityStatus: downloader.IdentityStatusValid, IdentityEvidence: evidence, IdentityIssues: []string{},
		Name: "client-claim", SizeBytes: 7, Progress: 1, State: "uploading", SavePath: "/downloads", ContentPath: contentPath,
	}
}

func ledgerPair(jobs ...downloader.Torrent) (downloader.LedgerSnapshot, downloader.LedgerSnapshot) {
	start := time.Date(2026, 8, 10, 1, 2, 3, 0, time.UTC)
	base := downloader.LedgerSnapshot{
		Driver: "qbittorrent", ObservedAtStart: start, ObservedAtEnd: start.Add(time.Second), Complete: true,
		Capabilities: downloader.LedgerCapabilities{TypedInfoHashes: true, ContentPath: true}, Jobs: append([]downloader.Torrent{}, jobs...),
	}
	after := base
	after.ObservedAtStart = start.Add(2 * time.Second)
	after.ObservedAtEnd = start.Add(3 * time.Second)
	after.Jobs = append([]downloader.Torrent{}, jobs...)
	return base, after
}

func relationStatus(report Report, kind string) string {
	for _, relation := range report.Relations {
		if relation.Kind == kind {
			return relation.Status
		}
	}
	return ""
}

func testBencode(value any) []byte {
	var out bytes.Buffer
	var encode func(any)
	encode = func(value any) {
		switch typed := value.(type) {
		case string:
			out.WriteString(strconv.Itoa(len(typed)))
			out.WriteByte(':')
			out.WriteString(typed)
		case []byte:
			out.WriteString(strconv.Itoa(len(typed)))
			out.WriteByte(':')
			out.Write(typed)
		case int64:
			out.WriteByte('i')
			out.WriteString(strconv.FormatInt(typed, 10))
			out.WriteByte('e')
		case []any:
			out.WriteByte('l')
			for _, item := range typed {
				encode(item)
			}
			out.WriteByte('e')
		case map[string]any:
			out.WriteByte('d')
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				encode(key)
				encode(typed[key])
			}
			out.WriteByte('e')
		default:
			panic("unsupported test bencode type")
		}
	}
	encode(value)
	return out.Bytes()
}
