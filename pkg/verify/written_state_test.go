package verify

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadWrittenFinalState(t *testing.T) {
	for _, tc := range []struct {
		line  string
		valid bool
	}{
		{`{"table":"orders","pk":"1"}`, true},
		{`{"table":"orders","pk":"1","expected":{"id":1,"total":2}}`, true},
		{`{"table":"orders","pk":"1","absent":true}`, true},
		{`{"table":"orders","pk":"1","absent":true,"expected":{}}`, false},
		{`{"table":"orders","pk":"1","expected":[]}`, false},
		{`{"table":"orders","pk":"1","expected":null}`, false},
	} {
		t.Run(tc.line, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "writes.jsonl")
			if err := os.WriteFile(path, []byte(tc.line), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := ReadWritten(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
