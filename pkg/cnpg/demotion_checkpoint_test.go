package cnpg

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestDemotionCheckpointRequiresIdentifiedNonzeroShutdownToken(t *testing.T) {
	valid := map[string]string{"databaseSystemIdentifier": "123", "latestCheckpointTimelineID": "1", "latestCheckpointREDOLocation": "0/3000000", "redoWalFile": "000000010000000000000003", "timeOfLatestCheckpoint": "2026-11-07 10:00:00 UTC", "operatorVersion": "1.30.1"}
	encode := func(v map[string]string) string { b, _ := json.Marshal(v); return base64.StdEncoding.EncodeToString(b) }
	if got, err := DemotionCheckpoint(encode(valid), "123"); err != nil || got != "0/3000000" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	for key, value := range map[string]string{"databaseSystemIdentifier": "456", "latestCheckpointTimelineID": "2", "latestCheckpointREDOLocation": "0/0", "redoWalFile": "garbage", "timeOfLatestCheckpoint": "", "operatorVersion": "unknown"} {
		t.Run(key, func(t *testing.T) {
			v := map[string]string{}
			for k, x := range valid {
				v[k] = x
			}
			v[key] = value
			if _, err := DemotionCheckpoint(encode(v), "123"); err == nil {
				t.Fatal("invalid checkpoint accepted")
			}
		})
	}
	for _, token := range []string{"", "not-base64", base64.StdEncoding.EncodeToString([]byte(`{}`))} {
		if _, err := DemotionCheckpoint(token, "123"); err == nil {
			t.Fatal("absent checkpoint accepted")
		}
	}
	valid["databaseSystemIdentifier"] = "0"
	if _, err := DemotionCheckpoint(encode(valid), "0"); err == nil {
		t.Fatal("zero token system identifier accepted")
	}
}
