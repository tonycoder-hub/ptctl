package reconcile

import (
	"strings"
	"testing"
)

func TestActiveStateCoversEveryRunningStateAndOnlyQuietsPausedOrStopped(t *testing.T) {
	for _, state := range []string{
		"uploading", "queuedUP", "stalledUP", "forcedUP", "checkingUP",
		"allocating", "downloading", "metaDL", "forcedMetaDL", "queuedDL", "stalledDL", "checkingDL", "forcedDL",
		"checkingResumeData", "moving", "unknown",
	} {
		if !activeState(state) {
			t.Errorf("running or unrecognized state %q was treated as quiet", state)
		}
	}
	for _, state := range []string{"pausedUP", "stoppedUP", "pausedDL", "stoppedDL", "error", "missingFiles"} {
		if activeState(state) {
			t.Errorf("paused, stopped, or failed state %q was treated as active", state)
		}
	}
}

func TestStalledSeedingJobCarriesActiveReadWarning(t *testing.T) {
	meta, discovery, source, root := reconciledSingleFile(t)
	for _, test := range []struct {
		state  string
		active bool
	}{{"stalledUP", true}, {"queuedUP", true}, {"uploading", true}, {"pausedUP", false}, {"stoppedUP", false}} {
		job := matchingJob(meta, "/downloads/renamed.bin")
		job.State = test.state
		before, after := ledgerPair(job)
		report, err := Build(BuildInput{
			Meta: meta, Discovery: discovery, VerifiedSource: source,
			Client:      ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3},
			PathMapping: testPathMapping(root),
		})
		if err != nil {
			t.Fatal(err)
		}
		warned := false
		for _, warning := range report.Warnings {
			warned = warned || strings.Contains(warning, "downloader job is active")
		}
		if warned != test.active {
			t.Fatalf("state %s active warning = %v, want %v: %#v", test.state, warned, test.active, report.Warnings)
		}
	}
}
