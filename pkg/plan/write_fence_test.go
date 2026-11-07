package plan

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSourceWriteFenceIsExplicitAndValidated(t *testing.T) {
	base := `{"id":"test","source":{"env":"src","namespace":"app","cluster":"db","dsn":"src"},"target":{"env":"dst","namespace":"app","cluster":"db","dsn":"dst"},"gateway":{"mode":"http","url":"http://gw"}}`
	p, err := Parse([]byte(base))
	if err != nil || p.SourceWriteFence != nil {
		t.Fatalf("default changed: %v", err)
	}
	for _, tc := range []struct {
		spec    string
		invalid bool
	}{
		{`{"mode":"cnpg_hba","writerRoles":["app"]}`, false},
		{`{"mode":"default_read_only","writerRoles":["app"]}`, true},
		{`{"mode":"cnpg_hba","writerRoles":["postgres"]}`, true},
		{`{"mode":"cnpg_hba","writerRoles":["app","app"]}`, true},
		{`{"mode":"cnpg_hba","writerRoles":["app\nlocal all all trust"]}`, true},
		{`{"mode":"cnpg_hba","writerRoles":["app"],"timeout":"31s"}`, true},
	} {
		js := strings.TrimSuffix(base, "}") + `,"sourceWriteFence":` + tc.spec + `}`
		p, err = Parse([]byte(js))
		if (err != nil) != tc.invalid {
			t.Fatalf("spec %s error %v", tc.spec, err)
		}
		if err == nil {
			b, _ := json.Marshal(p)
			if !strings.Contains(string(b), `"sourceWriteFence"`) {
				t.Fatal("fence configuration dropped")
			}
		}
	}
}
