// Command eval runs the project's quality gates and writes a machine-readable
// report to .eval/report.json.
//
// It is the single answer to "is this change done?" for humans, CI and AI
// agents alike: exit status 0 means every gate passed. When a gate fails, the
// report names the failing step, test, file and line so an agent can act on it
// without re-running anything.
//
// Usage:
//
//	go run ./tools/eval              # full gate (what CI runs)
//	go run ./tools/eval -quick       # fast inner loop: build, vet, short tests
//	go run ./tools/eval -fuzztime=30s -skip=vuln
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const outDir = ".eval"

// Status is the outcome of a single step.
type Status string

const (
	Pass Status = "pass"
	Fail Status = "fail"
	Skip Status = "skip"
	Warn Status = "warn" // findings reported, but the gate still passes
)

// Step is one quality gate in the report.
type Step struct {
	Name     string   `json:"name"`
	Status   Status   `json:"status"`
	Command  string   `json:"command,omitempty"`
	Duration string   `json:"duration"`
	Summary  string   `json:"summary,omitempty"`
	Details  []string `json:"details,omitempty"`
}

// Report is the document written to .eval/report.json.
type Report struct {
	Status    Status        `json:"status"`
	Mode      string        `json:"mode"`
	StartedAt time.Time     `json:"started_at"`
	Duration  string        `json:"duration"`
	Steps     []Step        `json:"steps"`
	Tests     *TestSummary  `json:"tests,omitempty"`
	Coverage  *CoverageInfo `json:"coverage,omitempty"`
	Lint      []LintIssue   `json:"lint,omitempty"`
	Nil       []NilIssue    `json:"nil,omitempty"`
}

type config struct {
	quick        bool
	minCoverage  float64
	fuzzTime     time.Duration
	skip         []string
	coverExclude []string
	printJSON    bool
	strictNil    bool
}

func main() {
	var cfg config
	var skip, exclude string
	flag.BoolVar(&cfg.quick, "quick", false, "fast mode: build, vet and short tests only")
	flag.Float64Var(&cfg.minCoverage, "min-coverage", 80, "minimum total statement coverage (percent)")
	flag.DurationVar(&cfg.fuzzTime, "fuzztime", 5*time.Second, "time to spend on each fuzz target (0 disables fuzzing)")
	flag.StringVar(&skip, "skip", "", "comma-separated step names to skip (tidy,fmt,build,vet,lint,nil,test,coverage,fuzz,vuln)")
	flag.StringVar(&exclude, "cover-exclude", "/tools/", "comma-separated import-path substrings excluded from the coverage gate")
	flag.BoolVar(&cfg.printJSON, "json", false, "print the full report as JSON to stdout instead of a summary")
	flag.BoolVar(&cfg.strictNil, "strict-nil", false, "fail the gate on NilAway findings instead of only reporting them")
	flag.Parse()
	cfg.skip = splitList(skip)
	cfg.coverExclude = splitList(exclude)

	ctx := context.Background()
	report := run(ctx, cfg)

	if err := writeReport(report); err != nil {
		fmt.Fprintln(os.Stderr, "eval: writing report:", err)
	}
	if cfg.printJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		printSummary(os.Stdout, report)
	}
	if report.Status != Pass {
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config) *Report {
	start := time.Now()
	r := &Report{Mode: "full", StartedAt: start.UTC()}
	if cfg.quick {
		r.Mode = "quick"
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		r.Steps = append(r.Steps, Step{Name: "setup", Status: Fail, Summary: err.Error()})
	}

	type gate struct {
		name  string
		quick bool // also runs in -quick mode
		fn    func(context.Context, config, *Report) Step
	}
	gates := []gate{
		{"tidy", false, stepTidy},
		{"fmt", false, stepFmt},
		{"build", true, stepBuild},
		{"vet", true, stepVet},
		{"lint", false, stepLint},
		{"nil", false, stepNil},
		{"test", true, stepTest},
		{"coverage", false, stepCoverage},
		{"fuzz", false, stepFuzz},
		{"vuln", false, stepVuln},
	}
	for _, g := range gates {
		if cfg.quick && !g.quick {
			continue
		}
		if slices.Contains(cfg.skip, g.name) {
			r.Steps = append(r.Steps, Step{Name: g.name, Status: Skip, Summary: "skipped by -skip"})
			continue
		}
		t := time.Now()
		s := g.fn(ctx, cfg, r)
		s.Name = g.name
		s.Duration = time.Since(t).Round(time.Millisecond).String()
		r.Steps = append(r.Steps, s)
		// Nothing downstream is meaningful if the code does not compile.
		if g.name == "build" && s.Status == Fail {
			break
		}
	}

	r.Status = Pass
	for _, s := range r.Steps {
		if s.Status == Fail {
			r.Status = Fail
		}
	}
	r.Duration = time.Since(start).Round(time.Millisecond).String()
	return r
}

func stepTidy(ctx context.Context, _ config, _ *Report) Step {
	out, err := sh(ctx, "go", "mod", "tidy", "-diff")
	return fromCmd("go mod tidy -diff", out, err, "go.mod/go.sum are not tidy: run `go mod tidy`")
}

func stepFmt(ctx context.Context, _ config, _ *Report) Step {
	out, err := sh(ctx, "golangci-lint", "fmt", "--diff")
	if err == nil && len(bytes.TrimSpace(out)) > 0 {
		err = errors.New("unformatted files")
	}
	return fromCmd("golangci-lint fmt --diff", out, err, "files are not formatted: run `make fmt`")
}

func stepBuild(ctx context.Context, _ config, _ *Report) Step {
	out, err := sh(ctx, "go", "build", "./...")
	return fromCmd("go build ./...", out, err, "build failed")
}

func stepVet(ctx context.Context, _ config, _ *Report) Step {
	out, err := sh(ctx, "go", "vet", "./...")
	return fromCmd("go vet ./...", out, err, "go vet reported problems")
}

func stepLint(ctx context.Context, _ config, r *Report) Step {
	jsonPath := filepath.Join(outDir, "lint.json")
	args := []string{"run", "--output.text.path=stderr", "--output.json.path=" + jsonPath, "./..."}
	cmd := "golangci-lint " + strings.Join(args, " ")
	out, err := sh(ctx, "golangci-lint", args...)
	issues, perr := parseLint(jsonPath)
	r.Lint = issues
	if err == nil {
		return Step{Status: Pass, Command: cmd, Summary: "no issues"}
	}
	if perr != nil || len(issues) == 0 {
		return fromCmd(cmd, out, err, "golangci-lint failed")
	}
	details := make([]string, 0, len(issues))
	for _, is := range issues {
		details = append(details, is.String())
	}
	return Step{Status: Fail, Command: cmd, Summary: fmt.Sprintf("%d lint issue(s)", len(issues)), Details: capLines(details, 50)}
}

// stepNil runs NilAway, which tracks nil flows across function and package
// boundaries (go vet's nilness only sees within one function). It has false
// positives, so findings warn by default; -strict-nil makes them blocking.
func stepNil(ctx context.Context, cfg config, r *Report) Step {
	const cmd = "go tool nilaway -json -pretty-print=false -include-pkgs=<module> ./..."
	mod, err := sh(ctx, "go", "list", "-m")
	if err != nil {
		return fromCmd("go list -m", mod, err, "cannot determine module path")
	}
	out, err := sh(ctx, "go", "tool", "nilaway", "-json", "-pretty-print=false",
		"-include-pkgs="+strings.TrimSpace(string(mod)), "./...")
	if err != nil {
		return fromCmd(cmd, out, err, "nilaway failed to run")
	}
	root, _ := os.Getwd()
	issues, err := parseNilAway(out, root)
	if err != nil {
		return Step{Status: Fail, Command: cmd, Summary: err.Error()}
	}
	r.Nil = issues
	if len(issues) == 0 {
		return Step{Status: Pass, Command: cmd, Summary: "no potential nil panics"}
	}
	details := make([]string, 0, len(issues))
	for _, is := range issues {
		details = append(details, is.String())
	}
	s := Step{
		Status: Warn, Command: cmd, Details: capLines(details, 50),
		Summary: fmt.Sprintf("%d potential nil panic(s) (report-only; -strict-nil to enforce)", len(issues)),
	}
	if cfg.strictNil {
		s.Status = Fail
		s.Summary = fmt.Sprintf("%d potential nil panic(s)", len(issues))
	}
	return s
}

func stepTest(ctx context.Context, cfg config, r *Report) Step {
	args := []string{"test", "-json", "-count=1", "-shuffle=on"}
	if cfg.quick {
		args = append(args, "-short")
	} else {
		args = append(args, "-race", "-covermode=atomic", "-coverprofile="+filepath.Join(outDir, "cover.out"))
	}
	args = append(args, "./...")
	cmd := "go " + strings.Join(args, " ")

	c := exec.CommandContext(ctx, "go", args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	stdout, _ := c.StdoutPipe()
	if err := c.Start(); err != nil {
		return Step{Status: Fail, Command: cmd, Summary: err.Error()}
	}
	raw, _ := os.Create(filepath.Join(outDir, "test.jsonl"))
	var src io.Reader = stdout
	if raw != nil {
		defer raw.Close()
		src = io.TeeReader(stdout, raw)
	}
	sum := parseTestEvents(src)
	waitErr := c.Wait()
	r.Tests = sum

	if waitErr == nil && sum.Failed == 0 {
		return Step{Status: Pass, Command: cmd, Summary: sum.String()}
	}
	var details []string
	for _, f := range sum.Failures {
		details = append(details, f.String())
	}
	if s := strings.TrimSpace(stderr.String()); s != "" {
		details = append(details, s)
	}
	if len(details) == 0 && waitErr != nil {
		details = append(details, waitErr.Error())
	}
	return Step{Status: Fail, Command: cmd, Summary: sum.String(), Details: capLines(details, 200)}
}

func stepCoverage(_ context.Context, cfg config, r *Report) Step {
	if r.Tests == nil || r.Tests.Failed > 0 {
		return Step{Status: Skip, Summary: "tests did not pass"}
	}
	f, err := os.Open(filepath.Join(outDir, "cover.out"))
	if err != nil {
		return Step{Status: Fail, Summary: "no coverage profile: " + err.Error()}
	}
	defer f.Close()
	info, err := parseCoverProfile(f, cfg.coverExclude)
	if err != nil {
		return Step{Status: Fail, Summary: err.Error()}
	}
	info.Minimum = cfg.minCoverage
	r.Coverage = info

	var details []string
	for _, p := range info.Packages {
		details = append(details, fmt.Sprintf("%6.1f%%  %s", p.Percent, p.Package))
	}
	s := Step{Summary: fmt.Sprintf("total %.1f%% (minimum %.1f%%)", info.Total, cfg.minCoverage), Details: details}
	s.Status = Pass
	if info.Total < cfg.minCoverage {
		s.Status = Fail
	}
	return s
}

func stepFuzz(ctx context.Context, cfg config, r *Report) Step {
	if cfg.fuzzTime <= 0 {
		return Step{Status: Skip, Summary: "-fuzztime=0"}
	}
	if r.Tests == nil || r.Tests.Failed > 0 {
		return Step{Status: Skip, Summary: "tests did not pass"}
	}
	out, err := sh(ctx, "go", "test", "-list=^Fuzz", "./...")
	if err != nil {
		return fromCmd("go test -list=^Fuzz ./...", out, err, "listing fuzz targets failed")
	}
	targets := parseFuzzList(out)
	if len(targets) == 0 {
		return Step{Status: Pass, Summary: "no fuzz targets"}
	}
	var failed []string
	for _, t := range targets {
		args := []string{"test", "-run=^$", "-fuzz=^" + t.Name + "$", "-fuzztime=" + cfg.fuzzTime.String(), t.Package}
		out, err := sh(ctx, "go", args...)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s.%s (failing input saved under testdata/fuzz/%s):\n%s",
				t.Package, t.Name, t.Name, tail(string(out), 40)))
		}
	}
	s := Step{Summary: fmt.Sprintf("%d target(s) x %s, %d failed", len(targets), cfg.fuzzTime, len(failed)), Details: failed}
	s.Status = Pass
	if len(failed) > 0 {
		s.Status = Fail
	}
	return s
}

func stepVuln(ctx context.Context, _ config, _ *Report) Step {
	out, err := sh(ctx, "go", "tool", "govulncheck", "./...")
	return fromCmd("go tool govulncheck ./...", out, err, "govulncheck found reachable vulnerabilities (or could not reach vuln.go.dev; use -skip=vuln offline)")
}

// sh runs a command and returns its combined output.
func sh(ctx context.Context, name string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, fmt.Errorf("%s not found on PATH: run `mise install` (or `make setup`)", name)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func fromCmd(cmd string, out []byte, err error, failMsg string) Step {
	if err == nil {
		return Step{Status: Pass, Command: cmd}
	}
	details := capLines(strings.Split(strings.TrimSpace(string(out)), "\n"), 100)
	if len(out) == 0 {
		details = []string{err.Error()}
	}
	return Step{Status: Fail, Command: cmd, Summary: failMsg, Details: details}
}

func writeReport(r *Report) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "report.json"), append(b, '\n'), 0o644)
}

func printSummary(w io.Writer, r *Report) {
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	icon := map[Status]string{Pass: "PASS", Fail: "FAIL", Skip: "SKIP", Warn: "WARN"}
	for _, s := range r.Steps {
		fmt.Fprintf(bw, "%-4s  %-9s %8s  %s\n", icon[s.Status], s.Name, s.Duration, s.Summary)
		if s.Status == Fail || s.Status == Warn {
			for _, d := range s.Details {
				for line := range strings.SplitSeq(d, "\n") {
					fmt.Fprintf(bw, "      | %s\n", line)
				}
			}
		}
	}
	fmt.Fprintf(bw, "\nEVAL %s (%s mode, %s) — report: %s\n",
		strings.ToUpper(string(r.Status)), r.Mode, r.Duration, filepath.Join(outDir, "report.json"))
}

func splitList(s string) []string {
	var out []string
	for f := range strings.SplitSeq(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func capLines(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return append(lines[:n:n], fmt.Sprintf("... %d more (see .eval/)", len(lines)-n))
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
