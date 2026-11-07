package plan

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAllowSourceRewireContract(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		body := valid
		if enabled {
			body = strings.Replace(body, `"id":`, `"allowSourceRewire":true,"id":`, 1)
		}
		p, err := Parse([]byte(body))
		if err != nil || p.AllowSourceRewire != enabled {
			t.Fatalf("parse enabled=%v: plan=%+v err=%v", enabled, p, err)
		}
		encoded, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		if _, present := fields["allowSourceRewire"]; present != enabled {
			t.Fatalf("omission contract enabled=%v: %s", enabled, encoded)
		}
		roundTrip, err := Parse(encoded)
		if err != nil || roundTrip.AllowSourceRewire != enabled {
			t.Fatalf("round trip enabled=%v: %v", enabled, err)
		}
	}
	for _, value := range []string{`"true"`, `1`, `{}`} {
		body := strings.Replace(valid, `"id":`, `"allowSourceRewire":`+value+`,"id":`, 1)
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("accepted non-boolean opt-in %s", value)
		}
	}
	// Opting in does not bypass the source/target validation contract.
	bad := strings.Replace(valid, `"id":`, `"allowSourceRewire":true,"id":`, 1)
	bad = strings.Replace(bad, `"env":"eks"`, `"env":"gke"`, 1)
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("source rewire accepted identical source and target environments")
	}
}

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

func TestTargetAppModes(t *testing.T) {
	base := `{"id":"p","source":{"env":"a","namespace":"n","cluster":"c1","dsn":"x"},
	  "target":{"env":"b","namespace":"n","cluster":"c2","dsn":"y","app":%s},
	  "gateway":{"mode":%q,"url":"http://gw:9901"}}`
	cold := `{"mode":"cold","kind":"Deployment","name":"web","replicas":2,
	  "smokeUrl":"http://web.n.svc:8080","smoke":[{"method":"GET","path":"/healthz"}]}`

	p, err := Parse([]byte(fmt.Sprintf(base, `{}`, "http")))
	if err != nil {
		t.Fatal(err)
	}
	if p.Target.App.Mode != AppWarm || p.Target.App.Cold() {
		t.Errorf("empty app should default to warm: %+v", p.Target.App)
	}
	p, err = Parse([]byte(fmt.Sprintf(base, cold, "http")))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Target.App.Cold() || p.Target.App.StartTimeout.Duration != 3*time.Minute {
		t.Errorf("cold defaults: %+v", p.Target.App)
	}
	var none *AppSpec
	if none.Cold() {
		t.Error("nil app must not be cold")
	}

	bad := map[string][2]string{
		"unknown mode":     {`{"mode":"lukewarm"}`, "http"},
		"cold without gw":  {cold, "none"},
		"bad kind":         {strings.Replace(cold, "Deployment", "CronJob", 1), "http"},
		"no name":          {strings.Replace(cold, `"name":"web",`, "", 1), "http"},
		"zero replicas":    {strings.Replace(cold, `"replicas":2`, `"replicas":0`, 1), "http"},
		"bad smoke url":    {strings.Replace(cold, "http://web.n.svc:8080", "web:8080", 1), "http"},
		"no smoke":         {strings.Replace(cold, `[{"method":"GET","path":"/healthz"}]`, `[]`, 1), "http"},
		"write smoke":      {strings.Replace(cold, `"GET"`, `"POST"`, 1), "http"},
		"relative path":    {strings.Replace(cold, `"/healthz"`, `"healthz"`, 1), "http"},
		"timeout too long": {strings.Replace(cold, `"replicas":2`, `"replicas":2,"startTimeout":"1h"`, 1), "http"},
	}
	for name, c := range bad {
		if _, err := Parse([]byte(fmt.Sprintf(base, c[0], c[1]))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	src := `{"id":"p","source":{"env":"a","namespace":"n","cluster":"c1","dsn":"x","app":{}},
	  "target":{"env":"b","namespace":"n","cluster":"c2","dsn":"y"}}`
	if _, err := Parse([]byte(src)); err == nil {
		t.Error("source.app accepted")
	}
}
