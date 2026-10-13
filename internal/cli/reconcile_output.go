package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/reconcile"
)

func writeReconciliationHuman(out io.Writer, report reconcile.Report) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	mappingID := report.Scope.PathMappingID
	if mappingID == "" {
		mappingID = "not_requested"
	}
	fmt.Fprintf(w, "PT RECONCILIATION\nOUTCOME\t%s\nEFFECT\t%s\nWRITES\t%d\nASSURANCE\t%s\nINDEX REFRESH\t%t\nPATH MAPPING\t%s\nCLIENT PATH SEMANTICS\t%s\n", terminalSafe(report.Outcome), terminalSafe(strings.Join(report.Effect, "+")), report.WritesPerformed, terminalSafe(report.Assurance), report.Scope.StorageIndexRefreshRequested, terminalSafe(mappingID), terminalSafe(report.Scope.ClientPathSemantics))

	fmt.Fprintln(w, "\nBLOCKERS\nCODE\tMESSAGE")
	if len(report.Blockers) == 0 {
		fmt.Fprintln(w, "-\tnone")
	} else {
		for _, blocker := range report.Blockers {
			fmt.Fprintf(w, "%s\t%s\n", terminalSafe(blocker.Code), terminalSafe(blocker.Message))
		}
	}

	fmt.Fprintln(w, "\nRELATIONS\nKIND\tSTATUS\tEVIDENCE\tBASIS\tBLOCKERS")
	for _, relation := range report.Relations {
		basis := strings.Join(relation.EvidenceBasis, ",")
		if basis == "" {
			basis = "-"
		}
		blockers := strings.Join(relation.BlockerCodes, ",")
		if blockers == "" {
			blockers = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", terminalSafe(relation.Kind), terminalSafe(relation.Status), terminalSafe(relation.EvidenceLevel), terminalSafe(basis), terminalSafe(blockers))
	}
	fmt.Fprintln(w, "METAFILE VARIANT NOTE\tdownloader APIs do not expose the private raw metafile variant; infohash linkage cannot prove variant equality")
	fmt.Fprintln(w, "PATH NOTE\thost/client namespace projection and content-path comparison are lexical only")

	siteLedger := report.Ledgers.Site
	bindingRecordID := siteLedger.BindingRecordID
	if bindingRecordID == "" {
		bindingRecordID = "-"
	}
	boundRef := "-"
	if siteLedger.Ref != nil {
		boundRef = siteLedger.Ref.SiteID + "/" + siteLedger.Ref.RemoteID
	}
	observedStart, observedEnd := "-", "-"
	if siteLedger.ObservedAtStart != nil {
		observedStart = siteLedger.ObservedAtStart.Format(time.RFC3339Nano)
	}
	if siteLedger.ObservedAtEnd != nil {
		observedEnd = siteLedger.ObservedAtEnd.Format(time.RFC3339Nano)
	}
	fmt.Fprintf(w, "\nSITE BINDING\nREQUESTED\t%t\nSELECTOR\t%s\nSTATUS\t%s\nRECORD ID\t%s\nSITE REF\t%s\nPROCESS-LOCAL PROOF\t%t\nHISTORICAL\t%t\nORIGIN\t%s\nROUTE\t%s\nOBSERVED START\t%s\nOBSERVED END\t%s\n",
		report.Scope.SiteBindingRequested, terminalSafe(report.Scope.SiteBindingSelector), terminalSafe(siteLedger.Status),
		terminalSafe(bindingRecordID), terminalSafe(boundRef), siteLedger.ProcessLocalProof, siteLedger.Historical,
		terminalSafe(valueOrUnknown(siteLedger.Origin)), terminalSafe(valueOrUnknown(siteLedger.RouteID)),
		terminalSafe(observedStart), terminalSafe(observedEnd))
	if siteLedger.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(siteLedger.StopReason))
	}
	detailLedger := siteLedger.Detail
	detailRef := "-"
	if detailLedger.Ref != nil {
		detailRef = detailLedger.Ref.SiteID + "/" + detailLedger.Ref.RemoteID
	}
	detailStart, detailEnd := "-", "-"
	if detailLedger.ObservedAtStart != nil {
		detailStart = detailLedger.ObservedAtStart.Format(time.RFC3339Nano)
	}
	if detailLedger.ObservedAtEnd != nil {
		detailEnd = detailLedger.ObservedAtEnd.Format(time.RFC3339Nano)
	}
	fmt.Fprintf(w, "\nLIVE SITE DETAIL\nREQUESTED\t%t\nSTATUS\t%s\nSITE REF\t%s\nPROCESS-LOCAL PROOF\t%t\nORIGIN\t%s\nROUTE\t%s\nOBSERVED START\t%s\nOBSERVED END\t%s\nREQUESTS\t%d\nAUTOMATIC RETRIES\t%d\nREDIRECTS FOLLOWED\t%d\nRESPONSE BYTES\t%d\n",
		report.Scope.SiteDetailRequested, terminalSafe(detailLedger.Status), terminalSafe(detailRef), detailLedger.ProcessLocalProof,
		terminalSafe(valueOrUnknown(detailLedger.Origin)), terminalSafe(valueOrUnknown(detailLedger.RouteID)),
		terminalSafe(detailStart), terminalSafe(detailEnd), detailLedger.RequestsMade, detailLedger.Used.AutomaticRetries,
		detailLedger.Used.RedirectsFollowed, detailLedger.Used.ResponseBytesRead)
	if detailLedger.Observation != nil {
		fmt.Fprintf(w, "DISPLAY TITLE\t%s\nDOWNLOAD REFERENCE\t%t\n", terminalSafe(detailLedger.Observation.DisplayTitle), detailLedger.Observation.DownloadReferenceObserved)
	}
	if detailLedger.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(detailLedger.StopReason))
	}

	materialized := report.Ledgers.Storage.MaterializedFinal
	materializedOperation, materializedPlan, materializedBasis, materializedAssurance := "-", "-", "-", "-"
	materializedBytes := int64(0)
	if materialized.Observation != nil {
		materializedOperation = shortID(materialized.Observation.OperationID)
		materializedPlan = materialized.Observation.MaterializePlanID
		materializedBasis = materialized.Observation.AuthorityBasis
		materializedAssurance = materialized.Observation.Assurance
		materializedBytes = materialized.Observation.BytesVerified
	}
	fmt.Fprintf(w, "\nMATERIALIZED FINAL\nREQUESTED\t%t\nSTATUS\t%s\nOPERATION\t%s\nPLAN\t%s\nPROCESS-LOCAL FINAL PROOF\t%t\nPROCESS-LOCAL SOURCE BRIDGE\t%t\nBYTES VERIFIED\t%d\nAUTHORITY BASIS\t%s\nASSURANCE\t%s\n",
		report.Scope.MaterializedFinalRequested, terminalSafe(materialized.Status), terminalSafe(materializedOperation), terminalSafe(materializedPlan),
		materialized.ProcessLocalFinalProof, materialized.ProcessLocalSourceBridge, materializedBytes,
		terminalSafe(materializedBasis), terminalSafe(materializedAssurance))
	if materialized.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(materialized.StopReason))
	}

	adoption := report.Ledgers.Adoption
	adoptionAction, adoptionOperation, adoptionPlan, adoptionCompletion, adoptionJob, adoptionState := "-", "-", "-", "-", "-", "-"
	adoptionStart, adoptionEnd, currentAdoptionStart, currentAdoptionEnd := "-", "-", "-", "-"
	adoptionRetained := false
	if adoption.Completion != nil {
		adoptionAction = adoption.Completion.Action
		adoptionOperation = shortID(adoption.Completion.OperationID)
		adoptionPlan = adoption.Completion.PlanID
		adoptionCompletion = shortID(adoption.Completion.CompletionID)
		adoptionStart = adoption.Completion.ObservedAtStart.Format(time.RFC3339Nano)
		adoptionEnd = adoption.Completion.ObservedAtEnd.Format(time.RFC3339Nano)
		adoptionRetained = adoption.Completion.RetainedTombstone
	}
	if adoption.CurrentJob != nil {
		adoptionJob = shortID(adoption.CurrentJob.JobID)
		adoptionState = adoption.CurrentJob.JobState
		currentAdoptionStart = adoption.CurrentJob.ObservedAtStart.Format(time.RFC3339Nano)
		currentAdoptionEnd = adoption.CurrentJob.ObservedAtEnd.Format(time.RFC3339Nano)
	}
	fmt.Fprintf(w, "\nCLIENT ADOPTION\nREQUESTED\t%t\nSTATUS\t%s\nACTION\t%s\nOPERATION\t%s\nPLAN\t%s\nCOMPLETION\t%s\nHISTORICAL\t%t\nRETAINED TOMBSTONE\t%t\nPROCESS-LOCAL COMPLETION PROOF\t%t\nPROCESS-LOCAL CURRENT-JOB BRIDGE\t%t\nCURRENT JOB\t%s\nCURRENT JOB STATE\t%s\nHISTORICAL OBSERVED START\t%s\nHISTORICAL OBSERVED END\t%s\nCURRENT OBSERVED START\t%s\nCURRENT OBSERVED END\t%s\n",
		report.Scope.ClientAdoptionRequested, terminalSafe(adoption.Status), terminalSafe(adoptionAction), terminalSafe(adoptionOperation), terminalSafe(adoptionPlan),
		terminalSafe(adoptionCompletion), adoption.Historical, adoptionRetained, adoption.ProcessLocalCompletionProof,
		adoption.ProcessLocalCurrentJobProof, terminalSafe(adoptionJob), terminalSafe(adoptionState),
		terminalSafe(adoptionStart), terminalSafe(adoptionEnd), terminalSafe(currentAdoptionStart), terminalSafe(currentAdoptionEnd))
	if adoption.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(adoption.StopReason))
	}

	activation := report.Ledgers.Activation
	activationOperation, activationPlan, terminalMarker, terminalPhase := "-", "-", "-", "-"
	activationStart, activationEnd, currentUseID, currentJob, currentLayout := "-", "-", "-", "-", "-"
	if activation.Completion != nil {
		activationOperation = shortID(activation.Completion.OperationID)
		activationPlan = activation.Completion.PlanID
		terminalMarker = shortID(activation.Completion.TerminalMarkerID)
		terminalPhase = activation.Completion.TerminalPhase
		activationStart = activation.Completion.ObservedAtStart.Format(time.RFC3339Nano)
		activationEnd = activation.Completion.ObservedAtEnd.Format(time.RFC3339Nano)
	}
	if activation.CurrentUse != nil {
		currentUseID = shortID(activation.CurrentUse.UseID)
		currentJob = shortID(activation.CurrentUse.JobID)
		currentLayout = shortID(activation.CurrentUse.FileLayoutID)
	} else if activation.CurrentAbsence != nil {
		currentUseID = shortID(activation.CurrentAbsence.UseID)
		currentJob = shortID(activation.CurrentAbsence.JobID)
		currentLayout = shortID(activation.CurrentAbsence.FileLayoutID)
	}
	fmt.Fprintf(w, "\nCLIENT ACTIVATION\nREQUESTED\t%t\nSTATUS\t%s\nOPERATION\t%s\nPLAN\t%s\nTERMINAL MARKER\t%s\nTERMINAL PHASE\t%s\nHISTORICAL\t%t\nPROCESS-LOCAL COMPLETION PROOF\t%t\nPROCESS-LOCAL CURRENT-USE BRIDGE\t%t\nPROCESS-LOCAL ABSENCE BRIDGE\t%t\nCURRENT USE / ABSENCE\t%s\nCURRENT JOB\t%s\nHISTORICAL FILE LAYOUT\t%s\nHISTORICAL OBSERVED START\t%s\nHISTORICAL OBSERVED END\t%s\n",
		report.Scope.ClientActivationRequested, terminalSafe(activation.Status), terminalSafe(activationOperation), terminalSafe(activationPlan),
		terminalSafe(terminalMarker), terminalSafe(terminalPhase), activation.Historical, activation.ProcessLocalCompletionProof,
		activation.ProcessLocalCurrentUseProof, activation.ProcessLocalCurrentAbsenceProof,
		terminalSafe(currentUseID), terminalSafe(currentJob), terminalSafe(currentLayout),
		terminalSafe(activationStart), terminalSafe(activationEnd))
	if activation.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(activation.StopReason))
	}

	stop := report.Ledgers.Stop
	stopOperation, stopPlan, stopCompletion, stopBasis, stoppedState := "-", "-", "-", "-", "-"
	stopStart, stopEnd, currentStoppedStart, currentStoppedEnd := "-", "-", "-", "-"
	stopRetained := false
	if stop.Completion != nil {
		stopOperation = shortID(stop.Completion.OperationID)
		stopPlan = stop.Completion.PlanID
		stopCompletion = shortID(stop.Completion.CompletionID)
		stopBasis = stop.Completion.CompletionBasis
		stopStart = stop.Completion.ObservedAtStart.Format(time.RFC3339Nano)
		stopEnd = stop.Completion.ObservedAtEnd.Format(time.RFC3339Nano)
		stopRetained = stop.Completion.RetainedTombstone
	}
	if stop.CurrentStopped != nil {
		stoppedState = stop.CurrentStopped.JobState
		currentStoppedStart = stop.CurrentStopped.ObservedAtStart.Format(time.RFC3339Nano)
		currentStoppedEnd = stop.CurrentStopped.ObservedAtEnd.Format(time.RFC3339Nano)
	}
	fmt.Fprintf(w, "\nCLIENT STOP\nREQUESTED\t%t\nSTATUS\t%s\nOPERATION\t%s\nPLAN\t%s\nCOMPLETION\t%s\nCOMPLETION BASIS\t%s\nHISTORICAL\t%t\nRETAINED TOMBSTONE\t%t\nPROCESS-LOCAL COMPLETION PROOF\t%t\nPROCESS-LOCAL CURRENT-STOPPED BRIDGE\t%t\nCURRENT JOB STATE\t%s\nHISTORICAL OBSERVED START\t%s\nHISTORICAL OBSERVED END\t%s\nCURRENT OBSERVED START\t%s\nCURRENT OBSERVED END\t%s\n",
		report.Scope.ClientStopRequested, terminalSafe(stop.Status), terminalSafe(stopOperation), terminalSafe(stopPlan),
		terminalSafe(stopCompletion), terminalSafe(stopBasis), stop.Historical, stopRetained,
		stop.ProcessLocalCompletionProof, stop.ProcessLocalCurrentProof, terminalSafe(stoppedState),
		terminalSafe(stopStart), terminalSafe(stopEnd), terminalSafe(currentStoppedStart), terminalSafe(currentStoppedEnd))
	if stop.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(stop.StopReason))
	}

	removal := report.Ledgers.Removal
	removalOperation, removalPlan, removalCompletion, removalBasis := "-", "-", "-", "-"
	removalStart, removalEnd := "-", "-"
	removalRetained := false
	if removal.Completion != nil {
		removalOperation = shortID(removal.Completion.OperationID)
		removalPlan = removal.Completion.PlanID
		removalCompletion = shortID(removal.Completion.CompletionID)
		removalBasis = removal.Completion.CompletionBasis
		removalStart = removal.Completion.ObservedAtStart.Format(time.RFC3339Nano)
		removalEnd = removal.Completion.ObservedAtEnd.Format(time.RFC3339Nano)
		removalRetained = removal.Completion.RetainedTombstone
	}
	fmt.Fprintf(w, "\nCLIENT REMOVAL (KEEP DATA)\nREQUESTED\t%t\nSTATUS\t%s\nOPERATION\t%s\nPLAN\t%s\nCOMPLETION\t%s\nCOMPLETION BASIS\t%s\nHISTORICAL\t%t\nRETAINED TOMBSTONE\t%t\nPROCESS-LOCAL COMPLETION PROOF\t%t\nHISTORICAL OBSERVED START\t%s\nHISTORICAL OBSERVED END\t%s\n",
		report.Scope.ClientRemovalRequested, terminalSafe(removal.Status), terminalSafe(removalOperation), terminalSafe(removalPlan),
		terminalSafe(removalCompletion), terminalSafe(removalBasis), removal.Historical, removalRetained,
		removal.ProcessLocalCompletionProof, terminalSafe(removalStart), terminalSafe(removalEnd))
	if removal.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(removal.StopReason))
	}

	retirement := report.Ledgers.Retirement
	retirementOperation, retirementPlan, retirementCompletion, retirementAbsence := "-", "-", "-", "-"
	retirementFiles, retirementBytes, retirementParents := 0, int64(0), 0
	retirementStart, retirementEnd := "-", "-"
	retainedTombstone := false
	if retirement.Completion != nil {
		retirementOperation = shortID(retirement.Completion.OperationID)
		retirementPlan = shortID(retirement.Completion.PlanID)
		retirementCompletion = shortID(retirement.Completion.CompletionID)
		retirementFiles, retirementBytes = retirement.Completion.FilesRetired, retirement.Completion.BytesRetired
		retainedTombstone = retirement.Completion.RetainedTombstone
	}
	if retirement.CurrentAbsence != nil {
		retirementAbsence = shortID(retirement.CurrentAbsence.AbsenceID)
		retirementParents = retirement.CurrentAbsence.ParentDirectoriesChecked
		retirementStart = retirement.CurrentAbsence.ObservedAtStart.Format(time.RFC3339Nano)
		retirementEnd = retirement.CurrentAbsence.ObservedAtEnd.Format(time.RFC3339Nano)
	}
	fmt.Fprintf(w, "\nSOURCE RETIREMENT\nREQUESTED\t%t\nSTATUS\t%s\nOPERATION\t%s\nPLAN\t%s\nCOMPLETION\t%s\nHISTORICAL\t%t\nRETAINED TOMBSTONE\t%t\nPROCESS-LOCAL COMPLETION PROOF\t%t\nPROCESS-LOCAL CURRENT-ABSENCE PROOF\t%t\nCURRENT ABSENCE\t%s\nFILES RETIRED / CHECKED\t%d\nBYTES RETIRED\t%d\nPARENT DIRECTORIES CHECKED\t%d\nCURRENT OBSERVED START\t%s\nCURRENT OBSERVED END\t%s\n",
		report.Scope.SourceRetirementRequested, terminalSafe(retirement.Status), terminalSafe(retirementOperation), terminalSafe(retirementPlan),
		terminalSafe(retirementCompletion), retirement.Historical, retainedTombstone, retirement.ProcessLocalCompletionProof,
		retirement.ProcessLocalAbsenceProof, terminalSafe(retirementAbsence), retirementFiles, retirementBytes, retirementParents,
		terminalSafe(retirementStart), terminalSafe(retirementEnd))
	if retirement.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(retirement.StopReason))
	}

	parentCleanup := report.Ledgers.ParentCleanup
	cleanupOperation, cleanupPlan, cleanupCompletion, cleanupAbsence := "-", "-", "-", "-"
	cleanupParents, cleanupFiles := 0, 0
	cleanupStart, cleanupEnd := "-", "-"
	cleanupRetained := false
	if parentCleanup.Completion != nil {
		cleanupOperation = shortID(parentCleanup.Completion.OperationID)
		cleanupPlan = shortID(parentCleanup.Completion.CleanupPlanID)
		cleanupCompletion = shortID(parentCleanup.Completion.CompletionID)
		cleanupParents, cleanupFiles = parentCleanup.Completion.ParentsRemoved, parentCleanup.Completion.RetiredFiles
		cleanupRetained = parentCleanup.Completion.RetainedTombstone
	}
	if parentCleanup.CurrentAbsence != nil {
		cleanupAbsence = shortID(parentCleanup.CurrentAbsence.AbsenceID)
		cleanupStart = parentCleanup.CurrentAbsence.ObservedAtStart.Format(time.RFC3339Nano)
		cleanupEnd = parentCleanup.CurrentAbsence.ObservedAtEnd.Format(time.RFC3339Nano)
	}
	fmt.Fprintf(w, "\nPARENT CLEANUP\nREQUESTED\t%t\nSTATUS\t%s\nOPERATION\t%s\nPLAN\t%s\nCOMPLETION\t%s\nHISTORICAL\t%t\nRETAINED TOMBSTONE\t%t\nPROCESS-LOCAL COMPLETION PROOF\t%t\nPROCESS-LOCAL CURRENT-ABSENCE PROOF\t%t\nCURRENT ABSENCE\t%s\nPARENTS REMOVED / CHECKED\t%d\nRETIRED FILES\t%d\nCURRENT OBSERVED START\t%s\nCURRENT OBSERVED END\t%s\n",
		report.Scope.ParentCleanupRequested, terminalSafe(parentCleanup.Status), terminalSafe(cleanupOperation), terminalSafe(cleanupPlan),
		terminalSafe(cleanupCompletion), parentCleanup.Historical, cleanupRetained, parentCleanup.ProcessLocalCompletionProof,
		parentCleanup.ProcessLocalAbsenceProof, terminalSafe(cleanupAbsence), cleanupParents, cleanupFiles,
		terminalSafe(cleanupStart), terminalSafe(cleanupEnd))
	if parentCleanup.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(parentCleanup.StopReason))
	}

	siteID := "-"
	if report.Ledgers.Site.Ref != nil {
		siteID = report.Ledgers.Site.Ref.SiteID + "/" + report.Ledgers.Site.Ref.RemoteID
	}
	storageID := report.Ledgers.Storage.SelectedSourceID
	if storageID == "" {
		storageID = "-"
	}
	downloaderID := report.Ledgers.Downloader.Driver
	if downloaderID == "" {
		downloaderID = "-"
	}
	fmt.Fprintln(w, "\nLEDGERS\nLEDGER\tSTATUS\tID\tSUMMARY")
	siteSummary := "not requested"
	if report.Ledgers.Site.Status == "declared_unbound" {
		siteSummary = "user declaration only"
	} else if report.Ledgers.Site.ProcessLocalProof {
		siteSummary = "sealed historical exact-response observation; current site mapping unobservable"
	} else if report.Scope.SiteBindingRequested {
		siteSummary = "explicit binding could not be verified"
	}
	if report.Ledgers.Site.Detail.Status == "observed_current_ref" {
		siteSummary += "; live remote-ID detail observed (site claim only)"
	}
	fmt.Fprintf(w, "site\t%s\t%s\t%s\n", terminalSafe(report.Ledgers.Site.Status), terminalSafe(siteID), terminalSafe(siteSummary))
	fmt.Fprintf(w, "metafile\t%s\t%s\t%s; %s\n", terminalSafe(report.Ledgers.Metafile.Status), terminalSafe(shortID(report.Ledgers.Metafile.VariantID)), terminalSafe(report.Ledgers.Metafile.Version), humanBytes(report.Ledgers.Metafile.PhysicalBytes))
	storageSummary := fmt.Sprintf("process-local proof=%t", report.Ledgers.Storage.ProcessLocalProof)
	if materialized.Status != "not_requested" {
		storageSummary += fmt.Sprintf("; materialized-final=%s; source-bridge=%t", materialized.Status, materialized.ProcessLocalSourceBridge)
	}
	fmt.Fprintf(w, "storage\t%s\t%s\t%s\n", terminalSafe(report.Ledgers.Storage.Status), terminalSafe(shortID(storageID)), terminalSafe(storageSummary))
	fmt.Fprintf(w, "downloader\t%s\t%s\trequests=%d; jobs=%d/%d\n", terminalSafe(report.Ledgers.Downloader.Status), terminalSafe(downloaderID), report.Ledgers.Downloader.RequestsMade, report.Ledgers.Downloader.JobsExaminedBefore, report.Ledgers.Downloader.JobsExaminedAfter)
	adoptionSummary := "not requested"
	adoptionID := "-"
	if adoption.Completion != nil {
		adoptionID = shortID(adoption.Completion.OperationID)
		adoptionSummary = fmt.Sprintf("historical=%t; retained=%t; current-job bridge=%t", adoption.Historical, adoption.Completion.RetainedTombstone, adoption.ProcessLocalCurrentJobProof)
	} else if report.Scope.ClientAdoptionRequested {
		adoptionSummary = "explicit terminal stopped adoption could not be verified"
	}
	fmt.Fprintf(w, "client adoption\t%s\t%s\t%s\n", terminalSafe(adoption.Status), terminalSafe(adoptionID), terminalSafe(adoptionSummary))
	activationSummary := "not requested"
	activationID := "-"
	if activation.Completion != nil {
		activationID = shortID(activation.Completion.OperationID)
		activationSummary = fmt.Sprintf("historical=%t; current-use bridge=%t", activation.Historical, activation.ProcessLocalCurrentUseProof)
	} else if report.Scope.ClientActivationRequested {
		activationSummary = "explicit terminal activation could not be verified"
	}
	fmt.Fprintf(w, "client activation\t%s\t%s\t%s\n", terminalSafe(activation.Status), terminalSafe(activationID), terminalSafe(activationSummary))
	stopSummary := "not requested"
	stopID := "-"
	if stop.Completion != nil {
		stopID = shortID(stop.Completion.OperationID)
		stopSummary = fmt.Sprintf("historical=%t; retained=%t; current-stopped bridge=%t; basis=%s", stop.Historical,
			stop.Completion.RetainedTombstone, stop.ProcessLocalCurrentProof, stop.Completion.CompletionBasis)
	} else if report.Scope.ClientStopRequested {
		stopSummary = "explicit terminal client stop could not be verified"
	}
	fmt.Fprintf(w, "client stop\t%s\t%s\t%s\n", terminalSafe(stop.Status), terminalSafe(stopID), terminalSafe(stopSummary))
	removalSummary := "not requested"
	removalID := "-"
	if removal.Completion != nil {
		removalID = shortID(removal.Completion.OperationID)
		removalSummary = fmt.Sprintf("historical=%t; retained=%t; basis=%s", removal.Historical, removal.Completion.RetainedTombstone, removal.Completion.CompletionBasis)
	} else if report.Scope.ClientRemovalRequested {
		removalSummary = "explicit terminal keep-data removal could not be verified"
	}
	fmt.Fprintf(w, "client removal\t%s\t%s\t%s\n", terminalSafe(removal.Status), terminalSafe(removalID), terminalSafe(removalSummary))
	retirementSummary := "not requested"
	retirementID := "-"
	if retirement.Completion != nil {
		retirementID = shortID(retirement.Completion.OperationID)
		retirementSummary = fmt.Sprintf("historical=%t; current-absence=%t; files=%d", retirement.Historical, retirement.ProcessLocalAbsenceProof, retirement.Completion.FilesRetired)
	} else if report.Scope.SourceRetirementRequested {
		retirementSummary = "explicit terminal source retirement could not be verified"
	}
	fmt.Fprintf(w, "source retirement\t%s\t%s\t%s\n", terminalSafe(retirement.Status), terminalSafe(retirementID), terminalSafe(retirementSummary))
	parentCleanupSummary := "not requested"
	parentCleanupID := "-"
	if parentCleanup.Completion != nil {
		parentCleanupID = shortID(parentCleanup.Completion.OperationID)
		parentCleanupSummary = fmt.Sprintf("historical=%t; current-absence=%t; parents=%d", parentCleanup.Historical, parentCleanup.ProcessLocalAbsenceProof, parentCleanup.Completion.ParentsRemoved)
	} else if report.Scope.ParentCleanupRequested {
		parentCleanupSummary = "explicit terminal parent cleanup could not be verified"
	}
	fmt.Fprintf(w, "parent cleanup\t%s\t%s\t%s\n", terminalSafe(parentCleanup.Status), terminalSafe(parentCleanupID), terminalSafe(parentCleanupSummary))

	fileLayout := report.Ledgers.Downloader.FileLayout
	fileStops := strings.Join(fileLayout.StopReasons, ",")
	if fileStops == "" {
		fileStops = "none"
	}
	fmt.Fprintf(w, "\nCLIENT FILE LAYOUT (BOUNDED)\nSTATUS\t%s\nSTABILITY\t%s\nREQUESTS\t%d\nFILES\t%d observed / %d expected\nSELECTED\t%d / %d\nCOMPLETE\t%d / %d\nBEFORE FILES CONSIDERED\t%d / %d\nAFTER FILES CONSIDERED\t%d / %d\nBEFORE PATH BYTES\t%d / %d\nAFTER PATH BYTES\t%d / %d\nBEFORE RESPONSE BYTES\t%d / %d\nAFTER RESPONSE BYTES\t%d / %d\nSTOP REASONS\t%s\n",
		terminalSafe(fileLayout.Status), terminalSafe(fileLayout.StabilityAssurance), fileLayout.RequestsMade,
		fileLayout.FilesObserved, fileLayout.FilesExpected, fileLayout.FilesSelected, fileLayout.FilesExpected, fileLayout.FilesComplete, fileLayout.FilesExpected,
		fileLayout.UsedBefore.FilesConsidered, fileLayout.Limits.MaxFiles, fileLayout.UsedAfter.FilesConsidered, fileLayout.Limits.MaxFiles,
		fileLayout.UsedBefore.PathBytes, fileLayout.Limits.MaxPathBytes, fileLayout.UsedAfter.PathBytes, fileLayout.Limits.MaxPathBytes,
		fileLayout.UsedBefore.ResponseBytes, fileLayout.Limits.MaxResponseBytes, fileLayout.UsedAfter.ResponseBytes, fileLayout.Limits.MaxResponseBytes,
		terminalSafe(fileStops))

	fmt.Fprintln(w, "\nCLIENT FILE FINDINGS (BOUNDED)\nINDEX\tSTATUS\tTORRENT PATH\tEXPECTED BYTES\tCLIENT BYTES\tCLIENT PATH")
	if len(fileLayout.Findings) == 0 {
		fmt.Fprintln(w, "-\tnone\t-\t-\t-\t-")
	} else {
		for _, finding := range fileLayout.Findings {
			clientSize := "-"
			if finding.ClientSize != nil {
				clientSize = strconv.FormatInt(*finding.ClientSize, 10)
			}
			expectedSize := "-"
			if finding.Status != "unexpected_index" {
				expectedSize = strconv.FormatInt(finding.ExpectedSize, 10)
			}
			clientPath := finding.ClientPathRef
			if finding.ClientPath != "" {
				clientPath = finding.ClientPath
			}
			if clientPath == "" {
				clientPath = "-"
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", finding.ManifestIndex, terminalSafe(finding.Status), terminalSafe(valueOrUnknown(finding.TorrentPath)), terminalSafe(expectedSize), terminalSafe(clientSize), terminalSafe(clientPath))
		}
	}
	if fileLayout.FindingOverflow > 0 {
		fmt.Fprintf(w, "...\t%d additional findings omitted by bounded retention\t-\t-\t-\t-\n", fileLayout.FindingOverflow)
	}

	fmt.Fprintln(w, "\nDOWNLOADER MATCHES\nID\tRELATION\tSTATE\tPROGRESS\tSIZE\tCONTENT PATH\tIDENTITY EVIDENCE")
	if len(report.Ledgers.Downloader.Matches) == 0 {
		fmt.Fprintln(w, "-\tnone\t-\t-\t-\t-\t-")
	} else {
		for _, match := range report.Ledgers.Downloader.Matches {
			contentPath := match.ContentPathRef
			if match.ContentPath != "" {
				contentPath = match.ContentPath
			}
			if contentPath == "" {
				contentPath = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%.1f%%\t%d\t%s\t%s\n", terminalSafe(shortID(match.ID)), terminalSafe(match.Relation), terminalSafe(match.State), match.Progress*100, match.SizeBytes, terminalSafe(contentPath), terminalSafe(strings.Join(match.IdentityEvidence, ",")))
		}
	}

	discovery := report.Ledgers.Storage.Discovery
	fmt.Fprintf(w, "\nSTORAGE SCAN\nSOURCE OUTCOME\t%s\nSOURCE SCOPE\t%s\nSCAN COMPLETE\t%t\nVERIFICATION COMPLETE\t%t\nSHARED COMMAND TIME BUDGET\t%s\nENTRIES\t%d / %d\nRETAINED FILES\t%d / %d\nCANDIDATE EDGES OBSERVED\t%d / %d (+1 proves truncation)\nCANDIDATE STATES\t%d / %d\nPROOF BUDGET CHARGED\t%s / %s\n",
		terminalSafe(discovery.SourceOutcome), terminalSafe(discovery.Scan.PathConfinement), discovery.Scan.Complete, discovery.Scan.VerificationComplete,
		(time.Duration(discovery.Scan.TimeBudgetMillis) * time.Millisecond).String(),
		discovery.Scan.InventoryUsed.EntriesExamined, discovery.Scan.InventoryLimits.MaxEntries,
		discovery.Scan.InventoryUsed.CandidatesRetained, discovery.Scan.InventoryLimits.MaxCandidates,
		discovery.Scan.MatchUsed.CandidateEdgesConsidered, discovery.Scan.MatchLimits.MaxCandidateEdges,
		discovery.Scan.MatchUsed.StatesExplored, discovery.Scan.MatchLimits.MaxStates,
		humanBytes(discovery.Scan.MatchUsed.ProofWorkBytesCharged), humanBytes(discovery.Scan.MatchLimits.MaxProofWorkBytes))

	fmt.Fprintln(w, "\nSTORAGE SCAN STOPS")
	if len(discovery.Scan.StopReasons) == 0 {
		fmt.Fprintln(w, "-\tnone")
	} else {
		for _, reason := range discovery.Scan.StopReasons {
			fmt.Fprintf(w, "-\t%s\n", terminalSafe(reason))
		}
	}
	fmt.Fprintln(w, "\nSTORAGE ISSUES\nTYPE\tCODE\tSUBJECT\tMESSAGE")
	if len(discovery.Scan.InventoryIssues) == 0 && len(discovery.Scan.MatchIssues) == 0 {
		fmt.Fprintln(w, "-\tnone\t-\t-")
	} else {
		for _, issue := range discovery.Scan.InventoryIssues {
			subject := issue.RootID
			if issue.RelativePath != "" {
				subject += ":" + issue.RelativePath
			}
			if subject == "" {
				subject = "-"
			}
			fmt.Fprintf(w, "inventory\t%s\t%s\t%s\n", terminalSafe(issue.Code), terminalSafe(subject), terminalSafe(issue.Message))
		}
		for _, issue := range discovery.Scan.MatchIssues {
			subject := shortID(issue.CandidateID)
			if subject == "" {
				subject = "-"
			}
			fmt.Fprintf(w, "verification\t%s\t%s\t%s\n", terminalSafe(issue.Code), terminalSafe(subject), terminalSafe(issue.Message))
		}
	}

	fmt.Fprintln(w, "\nVERIFIED STORAGE MATCHES\nID\tEVIDENCE\tLAYOUT\tFILES")
	if len(discovery.Matches) == 0 {
		fmt.Fprintln(w, "-\tnone\t-\t-")
	} else {
		for _, match := range discovery.Matches {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d/%d\n", terminalSafe(shortID(match.ID)), terminalSafe(match.EvidenceLevel), terminalSafe(match.Layout), match.Coverage.FilesFound, match.Coverage.FilesExpected)
		}
	}
	fmt.Fprintln(w, "\nVERIFIED STORAGE BINDINGS (BOUNDED)\nMATCH\tINDEX\tTORRENT PATH\tSOURCE\tCLIENT PATH")
	bindingsShown := 0
	bindingsOmitted := 0
	for _, match := range discovery.Matches {
		for _, binding := range match.Bindings {
			if bindingsShown >= 20 {
				bindingsOmitted++
				continue
			}
			sourcePath := binding.RelativePath
			if binding.AbsolutePath != "" {
				sourcePath = binding.AbsolutePath
			} else if binding.RootID != "" {
				sourcePath = binding.RootID + ":" + binding.RelativePath
			}
			clientPath := binding.ClientPath
			if clientPath == "" {
				clientPath = "-"
			}
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n", terminalSafe(shortID(match.ID)), binding.FileIndex, terminalSafe(binding.TorrentPath), terminalSafe(sourcePath), terminalSafe(clientPath))
			bindingsShown++
		}
	}
	if bindingsShown == 0 {
		fmt.Fprintln(w, "-\t-\tnone\t-\t-")
	}
	if bindingsOmitted > 0 {
		fmt.Fprintf(w, "...\t-\t%d additional bindings omitted by bounded rendering\t-\t-\n", bindingsOmitted)
	}
	if len(report.Warnings) > 0 {
		fmt.Fprintln(w, "\nWARNINGS")
		for _, warning := range report.Warnings {
			fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
		}
	}
	return w.Flush()
}
