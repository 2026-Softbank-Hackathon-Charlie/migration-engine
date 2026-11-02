package plan

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const valid = `{
  "id": "shop-gke-to-eks",
  "source": {"env":"gke","namespace":"shop","cluster":"shop-db-gke","dsn":"postgres://u:${PGPASS_TEST}@src/app"},
  "target": {"env":"eks","namespace":"shop","cluster":"shop-db-eks","dsn":"postgres://u:p@dst/app"},
  "thresholds": {"maxDowntime": "90s"}
}`

func TestParseDefaultsAndExpand(t *testing.T) {
	t.Setenv("PGPASS_TEST", "s3cret")
	p, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Source.DSN, "s3cret") {
		t.Errorf("dsn not expanded: %s", p.Source.DSN)
	}
	if p.Gateway.Mode != "none" || p.Gateway.SourceBackend != "gke" || p.Gateway.TargetBackend != "eks" {
		t.Errorf("gateway defaults: %+v", p.Gateway)
	}
	if p.Thresholds.MaxDowntime.Duration != 90*time.Second {
		t.Errorf("maxDowntime = %v", p.Thresholds.MaxDowntime)
	}
	if p.Thresholds.SwitchoverTimeout.Duration != 2*time.Minute || p.Thresholds.MaxLagBytes != 16<<20 {
		t.Errorf("threshold defaults: %+v", p.Thresholds)
	}
	if !*p.Verify.FenceGate || p.Verify.Schemas[0] != "public" {
		t.Errorf("verify defaults: %+v", p.Verify)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]string{
		"same env":      `{"id":"x","source":{"env":"gke","namespace":"a","cluster":"a","dsn":"d"},"target":{"env":"gke","namespace":"b","cluster":"b","dsn":"d"}}`,
		"missing dsn":   `{"id":"x","source":{"env":"gke","namespace":"a","cluster":"a"},"target":{"env":"eks","namespace":"b","cluster":"b","dsn":"d"}}`,
		"http no url":   `{"id":"x","source":{"env":"gke","namespace":"a","cluster":"a","dsn":"d"},"target":{"env":"eks","namespace":"b","cluster":"b","dsn":"d"},"gateway":{"mode":"http"}}`,
		"unknown field": `{"id":"x","bogus":1}`,
		"bad duration":  `{"id":"x","thresholds":{"maxDowntime":"soon"}}`,
	}
	for name, js := range cases {
		if _, err := Parse([]byte(js)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestDecommissionValidation(t *testing.T) {
	base := `{"id":"p","source":{"env":"a","namespace":"n","cluster":"c1","dsn":"x"},
	  "target":{"env":"b","namespace":"n","cluster":"c2","dsn":"y"},"decommission":%s}`
	ok := `{"selector":"app=shop","smokeUrl":"http://gw:8000","smoke":[{"method":"GET","path":"/"}]}`
	p, err := Parse([]byte(fmt.Sprintf(base, ok)))
	if err != nil {
		t.Fatal(err)
	}
	if p.Decommission.Observe.Duration != 2*time.Minute {
		t.Errorf("default observe = %s", p.Decommission.Observe)
	}
	bad := map[string]string{
		"no selector":      `{"smokeUrl":"http://gw","smoke":[{"method":"GET","path":"/"}]}`,
		"no smoke":         `{"selector":"app=shop","smokeUrl":"http://gw","smoke":[]}`,
		"bad url":          `{"selector":"app=shop","smokeUrl":"gw:8000","smoke":[{"method":"GET","path":"/"}]}`,
		"bad method":       `{"selector":"app=shop","smokeUrl":"http://gw","smoke":[{"method":"DELETE","path":"/"}]}`,
		"relative path":    `{"selector":"app=shop","smokeUrl":"http://gw","smoke":[{"method":"GET","path":"x"}]}`,
		"observe too long": `{"selector":"app=shop","observe":"2h","smokeUrl":"http://gw","smoke":[{"method":"GET","path":"/"}]}`,
	}
	for name, d := range bad {
		if _, err := Parse([]byte(fmt.Sprintf(base, d))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
