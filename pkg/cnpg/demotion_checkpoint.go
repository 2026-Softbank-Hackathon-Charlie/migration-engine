package cnpg

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// DemotionCheckpoint reads the shutdown checkpoint from the fresh demotion
// token issued by CNPG 1.30.1. CNPG generates this token only after PostgreSQL
// has shut down, and promotion validates the same REDO location. The caller
// must also prove cluster UID, fresh token ownership, and actual SQL recovery.
// A standby's current replay LSN cannot replace this final commit boundary.
func DemotionCheckpoint(token, systemID string) (string, error) {
	bad := errors.New("CNPG demotion checkpoint is absent, invalid, or from another PostgreSQL system")
	if len(token) > 16384 {
		return "", bad
	}
	data, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return "", bad
	}
	var v struct {
		SystemID string `json:"databaseSystemIdentifier"`
		Timeline string `json:"latestCheckpointTimelineID"`
		LSN      string `json:"latestCheckpointREDOLocation"`
		WAL      string `json:"redoWalFile"`
		Time     string `json:"timeOfLatestCheckpoint"`
		Version  string `json:"operatorVersion"`
	}
	if json.Unmarshal(data, &v) != nil || systemID == "" || v.SystemID != systemID || v.Time == "" || v.Version != "1.30.1" || len(v.WAL) != 24 {
		return "", bad
	}
	timeline, err := strconv.ParseUint(v.Timeline, 10, 32)
	if err != nil || timeline == 0 {
		return "", bad
	}
	walTimeline, err := strconv.ParseUint(v.WAL[:8], 16, 32)
	if err != nil || walTimeline != timeline {
		return "", bad
	}
	if _, err = strconv.ParseUint(v.WAL[8:], 16, 64); err != nil {
		return "", bad
	}
	parts := strings.Split(v.LSN, "/")
	if len(parts) != 2 {
		return "", bad
	}
	hi, e1 := strconv.ParseUint(parts[0], 16, 32)
	lo, e2 := strconv.ParseUint(parts[1], 16, 32)
	if e1 != nil || e2 != nil || hi == 0 && lo == 0 {
		return "", bad
	}
	return v.LSN, nil
}
