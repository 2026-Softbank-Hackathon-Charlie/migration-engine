// Command hopmigrate is the HopCharlie migration engine CLI.
//
//	hopmigrate validate -plan plan.json
//	hopmigrate status   -plan plan.json
//	hopmigrate run      -plan plan.json [-run RUN_ID] [-state-dir DIR] [-json]
//	hopmigrate step     -plan plan.json -run RUN_ID -step NAME [-json]
//	hopmigrate verify   -plan plan.json [-json]
//	hopmigrate show     -run RUN_ID [-state-dir DIR]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/files"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/kube"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/pg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

const usage = `hopmigrate — HopCharlie migration engine

Commands:
  validate  Check a plan file
  status    Show both CNPG clusters and current replication lag
  run       Execute (or resume with -run) a full migration
  step      Execute one step of an existing run (rehearsal)
  verify    Run the data/file verification report on its own
  show      Print a run's saved state

Steps: ` + "precheck → prepare → presync → delta → fence → switchover → cutover → verify → finalize" + `

Run "hopmigrate <command> -h" for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "validate":
		err = cmdValidate(args)
	case "status":
		err = cmdStatus(ctx, args)
	case "run":
		err = cmdRun(ctx, args)
	case "step":
		err = cmdStep(ctx, args)
	case "verify":
		err = cmdVerify(ctx, args)
	case "show":
		err = cmdShow(args)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type common struct {
	planPath string
	stateDir string
	runID    string
	json     bool
}

func flags(name string, c *common, withRun bool) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&c.planPath, "plan", "plan.json", "plan file")
	fs.BoolVar(&c.json, "json", false, "print machine-readable JSON")
	if withRun {
		fs.StringVar(&c.stateDir, "state-dir", filepath.Join(".hopcharlie", "runs"), "run state directory")
		fs.StringVar(&c.runID, "run", "", "run id to resume")
	}
	return fs
}

func cmdValidate(args []string) error {
	var c common
	flags("validate", &c, false).Parse(args)
	p, err := plan.Load(c.planPath)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(p)
	}
	fmt.Printf("plan %q OK: %s/%s (%s) → %s/%s (%s)\n", p.ID,
		p.Source.Namespace, p.Source.Cluster, p.Source.Env,
		p.Target.Namespace, p.Target.Cluster, p.Target.Env)
	return nil
}

func cmdStatus(ctx context.Context, args []string) error {
	var c common
	flags("status", &c, false).Parse(args)
	p, err := plan.Load(c.planPath)
	if err != nil {
		return err
	}
	out := map[string]any{}
	for role, ep := range map[string]plan.Endpoint{"source": p.Source, "target": p.Target} {
		dyn, err := kube.Dynamic(ep.Kubeconfig, ep.Context)
		if err != nil {
			out[role] = map[string]string{"error": err.Error()}
			continue
		}
		s, err := cnpg.New(dyn, ep.Namespace, ep.Cluster).Get(ctx)
		if err != nil {
			out[role] = map[string]string{"error": err.Error()}
			continue
		}
		out[role] = s
	}
	src, serr := pg.Connect(ctx, p.Source.DSN)
	dst, derr := pg.Connect(ctx, p.Target.DSN)
	switch {
	case serr != nil:
		out["lag"] = map[string]string{"error": "source: " + serr.Error()}
	case derr != nil:
		out["lag"] = map[string]string{"error": "target: " + derr.Error()}
	default:
		lag, sl, dl, err := pg.Lag(ctx, src, dst)
		if err != nil {
			out["lag"] = map[string]string{"error": err.Error()}
		} else {
			out["lag"] = map[string]any{"bytes": lag, "sourceLsn": sl, "targetLsn": dl}
		}
	}
	src.Close()
	dst.Close()
	return printJSON(out)
}

func cmdRun(ctx context.Context, args []string) error {
	var c common
	flags("run", &c, true).Parse(args)
	e, err := newEngine(c)
	if err != nil {
		return err
	}
	defer e.Close()
	if !c.json {
		fmt.Printf("run %s (state: %s)\n", e.RunID(), e.StatePath())
	}
	return e.Run(ctx)
}

func cmdStep(ctx context.Context, args []string) error {
	var c common
	var step string
	fs := flags("step", &c, true)
	fs.StringVar(&step, "step", "", "step name: "+strings.Join(engine.Steps, ", "))
	fs.Parse(args)
	if step == "" {
		return errors.New("-step is required")
	}
	if c.runID == "" && step != engine.StepPrecheck {
		return errors.New("-run is required (start a run with -step precheck)")
	}
	e, err := newEngine(c)
	if err != nil {
		return err
	}
	defer e.Close()
	if !c.json {
		fmt.Printf("run %s (state: %s)\n", e.RunID(), e.StatePath())
	}
	return e.RunStep(ctx, step)
}

func cmdVerify(ctx context.Context, args []string) error {
	var c common
	flags("verify", &c, false).Parse(args)
	p, err := plan.Load(c.planPath)
	if err != nil {
		return err
	}
	src, err := pg.Connect(ctx, p.Source.DSN)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	defer src.Close()
	dst, err := pg.Connect(ctx, p.Target.DSN)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	defer dst.Close()
	var rc engine.FileSyncer
	if p.Files != nil {
		rc = files.New(p.Files.Rclone, p.Files.Flags)
	}
	rep, err := engine.Verify(ctx, p, src, dst, rc)
	if err != nil {
		return err
	}
	if c.json {
		if err := printJSON(rep); err != nil {
			return err
		}
	} else {
		printReport(rep)
	}
	if !rep.OK {
		return errors.New("verification failed")
	}
	return nil
}

func cmdShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	dir := fs.String("state-dir", filepath.Join(".hopcharlie", "runs"), "run state directory")
	run := fs.String("run", "", "run id (empty lists runs)")
	fs.Parse(args)
	if *run == "" {
		entries, err := os.ReadDir(*dir)
		if err != nil {
			return err
		}
		for _, en := range entries {
			if strings.HasSuffix(en.Name(), ".json") {
				fmt.Println(strings.TrimSuffix(en.Name(), ".json"))
			}
		}
		return nil
	}
	b, err := os.ReadFile(filepath.Join(*dir, *run+".json"))
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
}

func newEngine(c common) (*engine.Engine, error) {
	p, err := plan.Load(c.planPath)
	if err != nil {
		return nil, err
	}
	emit := printEvent
	if c.json {
		enc := json.NewEncoder(os.Stdout)
		emit = func(ev engine.Event) { _ = enc.Encode(ev) }
	}
	return engine.New(p, engine.Options{StateDir: c.stateDir, RunID: c.runID, Emit: emit})
}

func printEvent(ev engine.Event) {
	ts := ev.Time.Local().Format("15:04:05.000")
	step := ev.Step
	if step == "" {
		step = "-"
	}
	switch ev.Kind {
	case engine.EventStepStarted:
		fmt.Printf("%s ▶ %s\n", ts, step)
	case engine.EventStepSucceeded:
		fmt.Printf("%s ✔ %s (%v ms)\n", ts, step, ev.Data["durationMillis"])
	case engine.EventStepSkipped:
		fmt.Printf("%s ↷ %s %s\n", ts, step, ev.Message)
	case engine.EventStepFailed:
		fmt.Printf("%s ✘ %s: %s\n", ts, step, ev.Message)
	case engine.EventMetric:
		fmt.Printf("%s   %s %s %s\n", ts, step, ev.Message, kv(ev.Data))
	case engine.EventRollback, engine.EventHalt:
		fmt.Printf("%s ! %s [%s] %s %s\n", ts, step, ev.Kind, ev.Message, kv(ev.Data))
	case engine.EventDone:
		fmt.Printf("%s ★ %s %s\n", ts, ev.Message, kv(ev.Data))
	default:
		fmt.Printf("%s   %s %s %s\n", ts, step, ev.Message, kv(ev.Data))
	}
}

func kv(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%v", k, m[k])
	}
	return strings.Join(parts, " ")
}

func printReport(r *engine.Report) {
	for _, t := range r.Tables {
		switch {
		case t.Skipped != "":
			fmt.Printf("  - %-40s skipped (%s)\n", t.Table, t.Skipped)
		case t.Error != "":
			fmt.Printf("  ✘ %-40s error: %s\n", t.Table, t.Error)
		case t.Missing > 0:
			fmt.Printf("  ✘ %-40s %d/%d missing e.g. %v\n", t.Table, t.Missing, t.Checked, t.Samples)
		default:
			fmt.Printf("  ✔ %-40s %d rows, 0 missing\n", t.Table, t.Checked)
		}
	}
	if r.Written != nil {
		fmt.Printf("k6 writes: %d checked, %d missing\n", r.Written.Checked, r.Written.Missing)
	}
	if r.Files != nil {
		fmt.Printf("files: %d matching, %d differences, %d missing\n", r.Files.Matching, r.Files.Differences, r.Files.Missing)
	}
	if r.OK {
		fmt.Printf("RESULT: OK — 0 missing rows (%s)\n", time.Now().Format(time.RFC3339))
	} else {
		fmt.Println("RESULT: FAILED —", strings.Join(r.Problems, "; "))
	}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
