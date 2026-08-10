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
	// ClientAdoptOperationDirectoryPrefix is reserved at the materialized
	// target root so a torrent payload can never collide with adoption control
	// state. clientadopt consumes this exact constant to prevent drift.
	ClientAdoptOperationDirectoryPrefix = ".ptctl-client-adopt-"
	clientAdoptDirectoryPrefix          = ClientAdoptOperationDirectoryPrefix
	// ClientActivateOperationDirectoryPrefix is reserved for explicit
	// recheck/start coordination state downstream of stopped-job adoption.
	ClientActivateOperationDirectoryPrefix = ".ptctl-client-activate-"
	clientActivateDirectoryPrefix          = ClientActivateOperationDirectoryPrefix
	// SourceRetireOperationDirectoryPrefix is reserved for the separately
	// acknowledged, journaled removal of source names after client activation.
	SourceRetireOperationDirectoryPrefix = ".ptctl-source-retire-"
	sourceRetireDirectoryPrefix          = SourceRetireOperationDirectoryPrefix
	intentFileName                       = "intent.json"
	journalDirectoryName                 = "journal"
	stageDirectoryName                   = "stage"
	scratchDirectoryName                 = "scratch"
	retentionDirectoryName               = "retention"
	retentionIntentFileName              = "intent.json"
	retentionCompleteName                = "complete.json"
	eventFilePrefix                      = "event-"
	eventFileSuffix                      = ".json"
)

func hasReservedControlPrefix(name string) bool {
	prefixes := []string{operationDirectoryPrefix, clientAdoptDirectoryPrefix, clientActivateDirectoryPrefix, sourceRetireDirectoryPrefix}
	for _, prefix := range prefixes {
		if runtime.GOOS == "windows" {
			if len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
				return true
			}
		} else if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// IsReservedControlName reports whether one lexical path component belongs to
// a ptctl operation namespace. Callers that can delete user-selected content
// use this to ensure a source path can never be reinterpreted as private
// materialize, adoption, activation, or retirement control state.
func IsReservedControlName(name string) bool { return hasReservedControlPrefix(name) }

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
