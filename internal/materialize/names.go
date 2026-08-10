package materialize

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

func hasOperationDirectoryPrefix(name string) bool {
	if runtime.GOOS == "windows" {
		return len(name) >= len(operationDirectoryPrefix) && strings.EqualFold(name[:len(operationDirectoryPrefix)], operationDirectoryPrefix)
	}
	return strings.HasPrefix(name, operationDirectoryPrefix)
}

const (
	operationDirectoryPrefix = ".ptctl-materialize-"
	intentFileName           = "intent.json"
	journalDirectoryName     = "journal"
	stageDirectoryName       = "stage"
	scratchDirectoryName     = "scratch"
	retentionDirectoryName   = "retention"
	retentionIntentFileName  = "intent.json"
	retentionCompleteName    = "complete.json"
	eventFilePrefix          = "event-"
	eventFileSuffix          = ".json"
)

func OperationDirectoryName(id OperationID) (string, error) {
	parsed, err := ParseOperationID(id.String())
	if err != nil || parsed != id {
		return "", fmt.Errorf("invalid materialize operation ID")
	}
	return operationDirectoryPrefix + strings.TrimPrefix(id.String(), "sha256:"), nil
}

func ParseOperationDirectoryName(name string) (OperationID, error) {
	if !strings.HasPrefix(name, operationDirectoryPrefix) {
		return "", fmt.Errorf("invalid materialize operation directory")
	}
	return ParseOperationID("sha256:" + strings.TrimPrefix(name, operationDirectoryPrefix))
}

func EventFileName(sequence int, id EventID) (string, error) {
	parsed, err := ParseEventID(id.String())
	if err != nil || parsed != id || sequence < 0 || sequence > hardMaxJournalEvents {
		return "", fmt.Errorf("invalid materialize event filename input")
	}
	return fmt.Sprintf("%s%09d-%s%s", eventFilePrefix, sequence, strings.TrimPrefix(id.String(), "sha256:"), eventFileSuffix), nil
}

func ParseEventFileName(name string) (int, EventID, error) {
	if !strings.HasPrefix(name, eventFilePrefix) || !strings.HasSuffix(name, eventFileSuffix) {
		return 0, "", fmt.Errorf("invalid materialize event filename")
	}
	value := strings.TrimSuffix(strings.TrimPrefix(name, eventFilePrefix), eventFileSuffix)
	if len(value) != 9+1+64 || value[9] != '-' {
		return 0, "", fmt.Errorf("invalid materialize event filename")
	}
	sequence, err := strconv.Atoi(value[:9])
	if err != nil || sequence < 0 || sequence > hardMaxJournalEvents || fmt.Sprintf("%09d", sequence) != value[:9] {
		return 0, "", fmt.Errorf("invalid materialize event filename")
	}
	id, err := ParseEventID("sha256:" + value[10:])
	if err != nil {
		return 0, "", fmt.Errorf("invalid materialize event filename")
	}
	return sequence, id, nil
}

func operationStaticNames() []string {
	return []string{intentFileName, journalDirectoryName, stageDirectoryName, scratchDirectoryName}
}
