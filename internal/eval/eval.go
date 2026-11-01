// Package eval runs a Go module's quality gates and produces a
// machine-readable report.
//
// It is the single answer to "is this change done?" for humans, CI and AI
// agents alike: a report with Status Pass means every gate passed. When a gate
// fails, the report names the failing step, test, file and line so an agent
// can act on it without re-running anything.
package eval

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// OutDir is the directory, relative to the module root, that receives the
// report and intermediate artifacts.
const OutDir = ".eval"

// Status is the outcome of a single step.
type Status string

// Step outcomes. A report passes when no step has status Fail.
const (
	Pass Status = "pass"
	Fail Status = "fail"
	Skip Status = "skip"
	Warn Status = "warn" // findings reported, but the gate still passes
)

// Steps lists every gate in execution order.
var Steps = []string{"tidy", "fmt", "build", "vet", "lint", "nil", "test", "coverage", "fuzz", "vuln"}

// quickSteps also run in Quick mode.
var quickSteps = []string{"build", "vet", "test"}

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

// Config controls a run. The zero value is not useful; start from
// DefaultConfig.
type Config struct {
	Dir          string        // module root; empty means the working directory
	Quick        bool          // build, vet and -short tests only
	MinCoverage  float64       // minimum total statement coverage, in percent
	FuzzTime     time.Duration // time per fuzz target; 0 disables fuzzing
	Skip         []string      // step names to skip
	CoverExclude []string      // import-path substrings excluded from the coverage gate
	StrictNil    bool          // fail, rather than warn, on NilAway findings
	// Self is an executable providing the `nilaway` and `govulncheck`
	// subcommands (the ace binary). Required for the nil and vuln steps.
	Self string
}

// DefaultConfig returns the configuration used by `ace eval` with no flags.
func DefaultConfig() Config {
	return Config{MinCoverage: 80, FuzzTime: 5 * time.Second, CoverExclude: []string{"/tools/"}}
}

type runner struct {
	cfg Config
	dir string // absolute module root
	out string // absolute OutDir
	r   *Report
}

// Run executes the gates and writes the report to OutDir/report.json under
// cfg.Dir. A failure to write the report is recorded as a failed step.
func Run(ctx context.Context, cfg Config) *Report {
	start := time.Now()
	r := &Report{Mode: "full", StartedAt: start.UTC()}
	if cfg.Quick {
		r.Mode = "quick"
	}
	dir, err := filepath.Abs(cmp.Or(cfg.Dir, "."))
	if err == nil {
		err = os.MkdirAll(filepath.Join(dir, OutDir), 0o750)
	}
	if err != nil {
		r.Steps = append(r.Steps, Step{Name: "setup", Status: Fail, Summary: err.Error()})
		r.Status = Fail
		return r
	}
	ru := &runner{cfg: cfg, dir: dir, out: filepath.Join(dir, OutDir), r: r}

	steps := map[string]func(context.Context) Step{
		"tidy": ru.tidy, "fmt": ru.fmt, "build": ru.build, "vet": ru.vet, "lint": ru.lint,
		"nil": ru.nilaway, "test": ru.test, "coverage": ru.coverage, "fuzz": ru.fuzz, "vuln": ru.vuln,
	}
	for _, name := range Steps {
		if cfg.Quick && !slices.Contains(quickSteps, name) {
			continue
		}
		if slices.Contains(cfg.Skip, name) {
			r.Steps = append(r.Steps, Step{Name: name, Status: Skip, Summary: "skipped by -skip"})
			continue
		}
		t := time.Now()
		s := steps[name](ctx)
		s.Name = name
		s.Duration = time.Since(t).Round(time.Millisecond).String()
		r.Steps = append(r.Steps, s)
		// Nothing downstream is meaningful if the code does not compile.
		if name == "build" && s.Status == Fail {
			break
		}
	}

	r.Duration = time.Since(start).Round(time.Millisecond).String()
	r.Status = Pass
	if slices.ContainsFunc(r.Steps, func(s Step) bool { return s.Status == Fail }) {
		r.Status = Fail
	}
	if err := ru.writeReport(); err != nil {
		r.Steps = append(r.Steps, Step{Name: "report", Status: Fail, Summary: err.Error()})
		r.Status = Fail
	}
	return r
}

func (ru *runner) tidy(ctx context.Context) Step {
	out, err := ru.sh(ctx, "go", "mod", "tidy", "-diff")
	return fromCmd("go mod tidy -diff", out, err, "go.mod/go.sum are not tidy: run `go mod tidy`")
}

func (ru *runner) fmt(ctx context.Context) Step {
	out, err := ru.sh(ctx, "golangci-lint", "fmt", "--diff")
	if err == nil && len(bytes.TrimSpace(out)) > 0 {
		err = errors.New("unformatted files")
	}
	return fromCmd("golangci-lint fmt --diff", out, err, "files are not formatted: run `make fmt`")
}

func (ru *runner) build(ctx context.Context) Step {
	out, err := ru.sh(ctx, "go", "build", "./...")
	return fromCmd("go build ./...", out, err, "build failed")
}

func (ru *runner) vet(ctx context.Context) Step {
	out, err := ru.sh(ctx, "go", "vet", "./...")
	return fromCmd("go vet ./...", out, err, "go vet reported problems")
}

func (ru *runner) lint(ctx context.Context) Step {
	jsonPath := filepath.Join(ru.out, "lint.json")
	_ = os.Remove(jsonPath)
	args := []string{"run", "--output.text.path=stderr", "--output.json.path=" + jsonPath, "./..."}
	cmd := "golangci-lint run ./..."
	out, err := ru.sh(ctx, "golangci-lint", args...)
	issues, perr := parseLint(jsonPath)
	ru.r.Lint = issues
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

// nilaway runs NilAway, which tracks nil flows across function and package
// boundaries (go vet's nilness only sees within one function). It has false
// positives, so findings warn unless StrictNil is set.
func (ru *runner) nilaway(ctx context.Context) Step {
	const cmd = "ace nilaway -json -pretty-print=false -include-pkgs=<module> ./..."
	if ru.cfg.Self == "" {
		return Step{Status: Fail, Command: cmd, Summary: "no ace executable configured (Config.Self)"}
	}
	mod, err := ru.sh(ctx, "go", "list", "-m")
	if err != nil {
		return fromCmd("go list -m", mod, err, "cannot determine module path")
	}
	out, err := ru.sh(ctx, ru.cfg.Self, "nilaway", "-json", "-pretty-print=false",
		"-include-pkgs="+strings.TrimSpace(string(mod)), "./...")
	if err != nil {
		return fromCmd(cmd, out, err, "nilaway failed to run")
	}
	issues, err := parseNilAway(out, ru.dir)
	if err != nil {
		return Step{Status: Fail, Command: cmd, Summary: err.Error()}
	}
	ru.r.Nil = issues
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
	if ru.cfg.StrictNil {
		s.Status = Fail
		s.Summary = fmt.Sprintf("%d potential nil panic(s)", len(issues))
	}
	return s
}

func (ru *runner) test(ctx context.Context) Step {
	args := []string{"test", "-json", "-count=1", "-shuffle=on"}
	if ru.cfg.Quick {
		args = append(args, "-short")
	} else {
		args = append(args, "-race", "-covermode=atomic", "-coverprofile="+filepath.Join(ru.out, "cover.out"))
	}
	args = append(args, "./...")
	cmd := "go " + strings.Join(args, " ")

	c := exec.CommandContext(ctx, "go", args...) //nolint:gosec // fixed binary, args built above
	c.Dir = ru.dir
	var stderr bytes.Buffer
	c.Stderr = &stderr
	stdout, err := c.StdoutPipe()
	if err == nil {
		err = c.Start()
	}
	if err != nil {
		return Step{Status: Fail, Command: cmd, Summary: err.Error()}
	}
	var src io.Reader = stdout
	if raw, err := os.Create(filepath.Join(ru.out, "test.jsonl")); err == nil {
		defer raw.Close()
		src = io.TeeReader(stdout, raw)
	}
	sum := parseTestEvents(src)
	waitErr := c.Wait()
	ru.r.Tests = sum

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

func (ru *runner) coverage(context.Context) Step {
	if ru.cfg.Quick || ru.r.Tests == nil || ru.r.Tests.Failed > 0 {
		return Step{Status: Skip, Summary: "tests did not pass"}
	}
	f, err := os.Open(filepath.Join(ru.out, "cover.out"))
	if err != nil {
		return Step{Status: Fail, Summary: "no coverage profile: " + err.Error()}
	}
	defer f.Close()
	info, err := parseCoverProfile(f, ru.cfg.CoverExclude)
	if err != nil {
		return Step{Status: Fail, Summary: err.Error()}
	}
	info.Minimum = ru.cfg.MinCoverage
	ru.r.Coverage = info

	var details []string
	for _, p := range info.Packages {
		details = append(details, fmt.Sprintf("%6.1f%%  %s", p.Percent, p.Package))
	}
	s := Step{Summary: fmt.Sprintf("total %.1f%% (minimum %.1f%%)", info.Total, ru.cfg.MinCoverage), Details: details}
	s.Status = Pass
	if info.Total < ru.cfg.MinCoverage {
		s.Status = Fail
	}
	return s
}

func (ru *runner) fuzz(ctx context.Context) Step {
	if ru.cfg.FuzzTime <= 0 {
		return Step{Status: Skip, Summary: "fuzz time is 0"}
	}
	if ru.r.Tests == nil || ru.r.Tests.Failed > 0 {
		return Step{Status: Skip, Summary: "tests did not pass"}
	}
	out, err := ru.sh(ctx, "go", "test", "-list=^Fuzz", "./...")
	if err != nil {
		return fromCmd("go test -list=^Fuzz ./...", out, err, "listing fuzz targets failed")
	}
	targets := parseFuzzList(out)
	if len(targets) == 0 {
		return Step{Status: Pass, Summary: "no fuzz targets"}
	}
	var failed []string
	for _, t := range targets {
		args := []string{"test", "-run=^$", "-fuzz=^" + t.Name + "$", "-fuzztime=" + ru.cfg.FuzzTime.String(), t.Package}
		out, err := ru.sh(ctx, "go", args...)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s.%s (failing input saved under testdata/fuzz/%s):\n%s",
				t.Package, t.Name, t.Name, tail(string(out), 40)))
		}
	}
	s := Step{Summary: fmt.Sprintf("%d target(s) x %s, %d failed", len(targets), ru.cfg.FuzzTime, len(failed)), Details: failed}
	s.Status = Pass
	if len(failed) > 0 {
		s.Status = Fail
	}
	return s
}

func (ru *runner) vuln(ctx context.Context) Step {
	const cmd = "ace govulncheck ./..."
	if ru.cfg.Self == "" {
		return Step{Status: Fail, Command: cmd, Summary: "no ace executable configured (Config.Self)"}
	}
	out, err := ru.sh(ctx, ru.cfg.Self, "govulncheck", "./...")
	return fromCmd(cmd, out, err, "govulncheck found reachable vulnerabilities (or could not reach vuln.go.dev; use -skip=vuln offline)")
}

// sh runs a command in the module root and returns its combined output.
func (ru *runner) sh(ctx context.Context, name string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, fmt.Errorf("%s not found: run `mise install` (or `make setup`)", name)
	}
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // running dev tools is this package's job
	c.Dir = ru.dir
	return c.CombinedOutput()
}

func (ru *runner) writeReport() error {
	b, err := json.MarshalIndent(ru.r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(ru.out, "report.json"), append(b, '\n'), 0o600)
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

// PrintSummary writes a human-readable summary of r, including the details of
// failed and warning steps.
func PrintSummary(w io.Writer, r *Report) {
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
		strings.ToUpper(string(r.Status)), r.Mode, r.Duration, filepath.Join(OutDir, "report.json"))
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
