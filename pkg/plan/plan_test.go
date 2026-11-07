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
	if !*p.Verify.FenceGate || len(p.Verify.Schemas) != 0 {
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

func TestExternalSource(t *testing.T) {
	t.Setenv("LEGACY_PG_PASSWORD", "s3cret")
	base := `{"id":"p","source":%s,
	  "target":{"env":"eks","namespace":"n","cluster":"c2","dsn":"y","app":%s%s},
	  "gateway":{"mode":"http","url":"http://gw:9901"}%s}`
	src := `{"kind":"external","name":"legacy","env":"legacy","dsn":"postgres://app:${LEGACY_PG_PASSWORD}@db-legacy-shop:5432/app"}`
	cold := `{"mode":"cold","kind":"Deployment","name":"web","replicas":1,
	  "smokeUrl":"http://web.n.svc:8080","smoke":[{"method":"GET","path":"/healthz"}]}`
	reindex := `,"reindex":["public.items_name_idx"]`

	p, err := Parse([]byte(fmt.Sprintf(base, src, cold, reindex, "")))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Source.External() || p.Target.External() || !strings.Contains(p.Source.DSN, "s3cret") {
		t.Errorf("source: %+v", p.Source)
	}
	if p.Gateway.SourceBackend != "legacy" || p.Target.Reindex[0] != "public.items_name_idx" {
		t.Errorf("plan: %+v %+v", p.Gateway, p.Target)
	}
	// The CNPG default stays as it was: no kind is written.
	p, err = Parse([]byte(valid))
	if err != nil || p.Source.Kind != "" || p.Source.External() {
		t.Fatalf("cnpg plan: %v %+v", err, p.Source)
	}
	if _, err := Parse([]byte(strings.Replace(valid, `"env":"gke"`, `"kind":"cnpg","env":"gke"`, 1))); err != nil {
		t.Errorf("explicit kind cnpg: %v", err)
	}

	bad := map[string][4]string{
		"no name":       {strings.Replace(src, `"name":"legacy",`, "", 1), cold, "", ""},
		"no dsn":        {strings.Replace(src, `,"dsn":"postgres://app:${LEGACY_PG_PASSWORD}@db-legacy-shop:5432/app"`, "", 1), cold, "", ""},
		"cluster set":   {strings.Replace(src, `"env":"legacy"`, `"env":"legacy","namespace":"n","cluster":"c1"`, 1), cold, "", ""},
		"warm app":      {src, `{"mode":"warm"}`, "", ""},
		"no app":        {src, `null`, "", ""},
		"unknown kind":  {strings.Replace(src, `"external"`, `"rds"`, 1), cold, "", ""},
		"bad reindex":   {src, cold, `,"reindex":["items_name_idx"]`, ""},
		"decommission":  {src, cold, "", `,"decommission":{"selector":"app=shop","smokeUrl":"http://gw","smoke":[{"method":"GET","path":"/"}]}`},
		"target extern": {src, cold, `,"kind":"external"`, ""},
		"no fence gate": {src, cold, "", `,"verify":{"fenceGate":false}`},
		"source rewire": {src, cold, "", `,"allowSourceRewire":true`},
		"space reindex": {src, cold, `,"reindex":["public.items name"]`, ""},
	}
	for name, c := range bad {
		if _, err := Parse([]byte(fmt.Sprintf(base, c[0], c[1], c[2], c[3]))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	cnpgSrc := `{"env":"gke","namespace":"n","cluster":"c1","dsn":"x"}`
	for name, c := range map[string][2]string{
		"reindex with cnpg source": {cnpgSrc, reindex},
		"name with cnpg source":    {strings.Replace(cnpgSrc, `"env"`, `"name":"legacy","env"`, 1), ""},
	} {
		if _, err := Parse([]byte(fmt.Sprintf(base, c[0], cold, c[1], ""))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStateless(t *testing.T) {
	base := `{"id":"doom","source":%s,"target":%s,
	  "gateway":{"mode":"http","url":"http://gw:9901"}%s}`
	src, dst := `{"kind":"none","env":"eks"}`, `{"kind":"none","env":"gke"}`
	cold := `{"kind":"none","env":"gke","namespace":"doom","app":{"mode":"cold","kind":"Deployment","name":"doom","replicas":1,
	  "smokeUrl":"http://doom.gke:8080","smoke":[{"method":"GET","path":"/"}]}}`

	p, err := Parse([]byte(fmt.Sprintf(base, src, dst, `,"files":{"source":"s:b","target":"d:b"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Stateless() || p.Source.External() {
		t.Errorf("not stateless: %+v %+v", p.Source, p.Target)
	}
	if *p.Verify.FenceGate || len(p.Verify.Schemas) != 0 {
		t.Errorf("verify defaults must stay empty: %+v", p.Verify)
	}
	if p.Gateway.SourceBackend != "eks" || p.Gateway.TargetBackend != "gke" {
		t.Errorf("gateway backends: %+v", p.Gateway)
	}
	for name, c := range map[string][3]string{
		"cold app in target.namespace": {src, cold, ""},
		"warm smoke":                   {src, `{"kind":"none","env":"gke","app":{"smokeUrl":"http://doom.gke:8080","smoke":[{"method":"GET","path":"/"}]}}`, ""},
		"fenceGate false":              {src, dst, `,"verify":{"fenceGate":false}`},
	} {
		if _, err := Parse([]byte(fmt.Sprintf(base, c[0], c[1], c[2]))); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	bad := map[string]struct {
		src, dst, extra, want string
	}{
		"none with cnpg target":    {src, `{"env":"gke","namespace":"n","cluster":"c","dsn":"y"}`, "", "must both be"},
		"cnpg source, none target": {`{"env":"eks","namespace":"n","cluster":"c","dsn":"y"}`, dst, "", "must both be"},
		"external with none":       {`{"kind":"external","name":"legacy","env":"eks","dsn":"x"}`, dst, "", "must both be"},
		"source dsn":               {`{"kind":"none","env":"eks","dsn":"x"}`, dst, "", "source.dsn"},
		"target cluster":           {src, `{"kind":"none","env":"gke","cluster":"c"}`, "", "target.cluster"},
		"warm target namespace":    {src, `{"kind":"none","env":"gke","namespace":"doom"}`, "", "target.namespace"},
		"source kubeconfig":        {`{"kind":"none","env":"eks","kubeconfig":"k","context":"c"}`, dst, "", "source.kubeconfig, context"},
		"manifest":                 {src, `{"kind":"none","env":"gke","manifest":"t.yaml"}`, "", "target.manifest"},
		"reindex":                  {src, `{"kind":"none","env":"gke","reindex":["public.i"]}`, "", "target.reindex"},
		"cold without namespace":   {src, strings.Replace(cold, `"namespace":"doom",`, "", 1), "", "target.namespace is required"},
		"warm write smoke":         {src, `{"kind":"none","env":"gke","app":{"smokeUrl":"http://doom.gke:8080","smoke":[{"method":"POST","path":"/"}]}}`, "", "GET or HEAD"},
		"warm smoke without url":   {src, `{"kind":"none","env":"gke","app":{"smoke":[{"method":"GET","path":"/"}]}}`, "", "smokeUrl"},
		"no gateway":               {src, dst, `,"gateway":{"mode":"none"}`, "gateway.mode http"},
		"fenceGate true":           {src, dst, `,"verify":{"fenceGate":true}`, "fenceGate"},
		"verify tables":            {src, dst, `,"verify":{"tables":["public.t"]}`, "verify.schemas"},
		"written records":          {src, dst, `,"verify":{"writtenRecords":"w.jsonl"}`, "verify.schemas"},
		"source rewire":            {src, dst, `,"allowSourceRewire":true`, "allowSourceRewire"},
		"decommission":             {src, dst, `,"decommission":{"selector":"app=doom","smokeUrl":"http://gw","smoke":[{"method":"GET","path":"/"}]}`, "decommission"},
		"warm app kind":            {src, `{"kind":"none","env":"gke","app":{"kind":"Deployment","name":"doom"}}`, "", "only for target.app.mode cold"},
		"same backends":            {src, dst, `,"gateway":{"mode":"http","url":"http://gw","targetBackend":"eks"}`, "route would not move"},
		"kind mix message":         {src, `{"env":"gke","namespace":"n","cluster":"c","dsn":"y"}`, "", "(cnpg)"},
		"same env":                 {src, `{"kind":"none","env":"eks"}`, "", "both"},
	}
	for name, c := range bad {
		js := fmt.Sprintf(base, c.src, c.dst, c.extra)
		if name == "no gateway" {
			js = fmt.Sprintf(`{"id":"doom","source":%s,"target":%s%s}`, c.src, c.dst, c.extra)
		}
		_, err := Parse([]byte(js))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}
