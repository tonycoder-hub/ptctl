package reconcile

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
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
			if got := overallOutcome("not_requested", false, test.storageStatus, true, test.clientStatus, test.pathStatus, true); got != "conflict" {
				t.Fatalf("positive contradiction was hidden by ambiguity: got %q", got)
			}
		})
	}
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
	return downloader.Torrent{
		Hash: "opaque-job-one", InfoHashV1: meta.InfoHashV1, InfoHashV2: meta.InfoHashV2,
		IdentityStatus: downloader.IdentityStatusValid, IdentityEvidence: []string{"qbittorrent.magnet_uri.xt"}, IdentityIssues: []string{},
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
