// Package files migrates S3-API object storage with rclone.
//
// Remotes are configured through rclone's environment variables, e.g.
//
//	RCLONE_CONFIG_SRC_TYPE=s3 RCLONE_CONFIG_SRC_PROVIDER=Minio RCLONE_CONFIG_SRC_ENDPOINT=...
package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Rclone runs the rclone binary.
type Rclone struct {
	Bin   string
	Flags []string
}

// New returns an Rclone runner.
func New(bin string, flags []string) *Rclone {
	if bin == "" {
		bin = "rclone"
	}
	return &Rclone{Bin: bin, Flags: flags}
}

// Available checks the binary is on PATH.
func (r *Rclone) Available() error {
	if _, err := exec.LookPath(r.Bin); err != nil {
		return fmt.Errorf("rclone binary %q not found: %w", r.Bin, err)
	}
	return nil
}

// SyncResult is evidence from one sync pass.
type SyncResult struct {
	Duration time.Duration `json:"duration"`
	Output   string        `json:"output,omitempty"`
}

// Sync makes dst identical to src (incremental).
func (r *Rclone) Sync(ctx context.Context, src, dst string) (SyncResult, error) {
	args := append([]string{"sync", src, dst, "--stats-one-line", "--stats-log-level", "NOTICE", "--stats", "0"}, r.Flags...)
	start := time.Now()
	out, err := r.run(ctx, args)
	res := SyncResult{Duration: time.Since(start), Output: tail(out, 2000)}
	if err != nil {
		return res, fmt.Errorf("rclone sync %s -> %s: %w: %s", src, dst, err, tail(out, 500))
	}
	return res, nil
}

// Copy copies new and changed objects from src to dst and never deletes
// from dst (stateless plans, ADR 0020).
func (r *Rclone) Copy(ctx context.Context, src, dst string) (SyncResult, error) {
	args := append([]string{"copy", src, dst, "--stats-one-line", "--stats-log-level", "NOTICE", "--stats", "0"}, r.Flags...)
	start := time.Now()
	out, err := r.run(ctx, args)
	res := SyncResult{Duration: time.Since(start), Output: tail(out, 2000)}
	if err != nil {
		return res, fmt.Errorf("rclone copy %s -> %s: %w: %s", src, dst, err, tail(out, 500))
	}
	return res, nil
}

// Readable lists the top level of remote (rclone lsf --max-depth 1), a cheap
// check that it exists and the credentials can read it.
func (r *Rclone) Readable(ctx context.Context, remote string) error {
	args := append([]string{"lsf", remote, "--max-depth", "1"}, r.Flags...)
	if out, err := r.run(ctx, args); err != nil {
		return fmt.Errorf("rclone lsf %s: %w: %s", remote, err, tail(out, 500))
	}
	return nil
}

// CheckResult is evidence from `rclone check --one-way`.
type CheckResult struct {
	OK          bool   `json:"ok"`
	Matching    int    `json:"matching"`
	Differences int    `json:"differences"`
	Missing     int    `json:"missingOnTarget"`
	Output      string `json:"output,omitempty"`
}

var (
	reDiff     = regexp.MustCompile(`(\d+) differences found`)
	reMatching = regexp.MustCompile(`(\d+) matching files?`)
	reMissing  = regexp.MustCompile(`(\d+) files? missing`)
)

// Check verifies every object in src exists identically in dst.
// Objects that exist only in dst (written after cutover) are not counted.
func (r *Rclone) Check(ctx context.Context, src, dst string) (CheckResult, error) {
	args := append([]string{"check", src, dst, "--one-way"}, r.Flags...)
	out, err := r.run(ctx, args)
	res := ParseCheck(out)
	res.Output = tail(out, 2000)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		res.OK = res.Differences == 0
		return res, nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
		// rclone exits 1 when differences were found; that is a result, not a failure.
		res.OK = false
		return res, nil
	default:
		return res, fmt.Errorf("rclone check %s -> %s: %w: %s", src, dst, err, tail(out, 500))
	}
}

// ParseCheck extracts counters from rclone check output.
func ParseCheck(out string) CheckResult {
	var r CheckResult
	r.Differences = lastInt(reDiff, out)
	r.Matching = lastInt(reMatching, out)
	r.Missing = lastInt(reMissing, out)
	return r
}

func lastInt(re *regexp.Regexp, s string) int {
	m := re.FindAllStringSubmatch(s, -1)
	if len(m) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(m[len(m)-1][1])
	return n
}

func (r *Rclone) run(ctx context.Context, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
