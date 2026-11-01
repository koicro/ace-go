package eval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/koicro/ace-go/internal/eval"
)

// These tests run the real gate against small throwaway modules, so they are
// slow (they build the ace binary and shell out to go) and skipped in -short.

var aceBin string

func TestMain(m *testing.M) {
	code := func() int {
		flag.Parse()
		if !testing.Short() {
			dir, err := os.MkdirTemp("", "ace-eval-test")
			if err != nil {
				panic(err)
			}
			defer os.RemoveAll(dir)
			aceBin = filepath.Join(dir, "ace")
			out, err := exec.CommandContext(context.Background(), "go", "build", "-o", aceBin, "github.com/koicro/ace-go/cmd/ace").CombinedOutput()
			if err != nil {
				panic("building ace: " + err.Error() + "\n" + string(out))
			}
		}
		return m.Run()
	}()
	os.Exit(code)
}

const goMod = "module example.com/fixture\n\ngo 1.26\n"

const goodSrc = `// Package fixture is a test module.
package fixture

// Double returns 2*n.
func Double(n int) int { return 2 * n }
`

const goodTest = `package fixture

import "testing"

func TestDouble(t *testing.T) {
	if Double(2) != 4 {
		t.Fatal("bad")
	}
}

func FuzzDouble(f *testing.F) {
	f.Add(1)
	f.Fuzz(func(t *testing.T, n int) {
		if Double(n) != n+n {
			t.Fatal("bad")
		}
	})
}
`

func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func config(dir string, skip ...string) eval.Config {
	cfg := eval.DefaultConfig()
	cfg.Dir = dir
	cfg.Self = aceBin
	cfg.FuzzTime = time.Second
	cfg.Skip = append([]string{"vuln"}, skip...) // needs network
	if _, err := exec.LookPath("golangci-lint"); err != nil {
		cfg.Skip = append(cfg.Skip, "fmt", "lint")
	}
	return cfg
}

func step(t *testing.T, r *eval.Report, name string) eval.Step {
	t.Helper()
	for _, s := range r.Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no %q step in report", name)
	return eval.Step{}
}

func summary(r *eval.Report) string {
	var b bytes.Buffer
	eval.PrintSummary(&b, r)
	return b.String()
}

func TestRunPassing(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: runs the full gate")
	}
	t.Parallel()
	dir := fixture(t, map[string]string{"go.mod": goMod, "f.go": goodSrc, "f_test.go": goodTest})
	r := eval.Run(t.Context(), config(dir))
	if r.Status != eval.Pass {
		t.Fatalf("status = %s, want pass\n%s", r.Status, summary(r))
	}
	for name, want := range map[string]eval.Status{"tidy": eval.Pass, "nil": eval.Pass, "fuzz": eval.Pass, "vuln": eval.Skip} {
		if got := step(t, r, name).Status; got != want {
			t.Errorf("step %s = %s, want %s", name, got, want)
		}
	}
	if r.Coverage == nil || r.Coverage.Total != 100 {
		t.Errorf("coverage = %+v, want 100%%", r.Coverage)
	}

	// The report on disk matches the returned one.
	b, err := os.ReadFile(filepath.Join(dir, eval.OutDir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk eval.Report
	if err := json.Unmarshal(b, &onDisk); err != nil || onDisk.Status != eval.Pass || len(onDisk.Steps) != len(r.Steps) {
		t.Errorf("report.json = %+v, %v; want the passing report", onDisk.Status, err)
	}
	if s := summary(r); !strings.Contains(s, "EVAL PASS (full mode") {
		t.Errorf("summary missing verdict:\n%s", s)
	}
}

func TestRunFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: runs the full gate")
	}
	t.Parallel()
	nilSrc := goodSrc + `
type user struct{ name string }

func find(id int) *user {
	if id == 0 {
		return nil
	}
	return &user{name: "x"}
}

// Name returns a user's name.
func Name(id int) string { return find(id).name }
`
	failingTest := strings.Replace(goodTest, "Double(2) != 4", "Double(2) != 5", 1)
	files := map[string]string{"go.mod": goMod, "f.go": nilSrc, "f_test.go": failingTest}

	t.Run("warn", func(t *testing.T) {
		t.Parallel()
		dir := fixture(t, map[string]string{"go.mod": goMod, "f.go": nilSrc, "f_test.go": goodTest})
		cfg := config(dir, "coverage", "fuzz")
		r := eval.Run(t.Context(), cfg)
		s := step(t, r, "nil")
		if s.Status != eval.Warn || len(r.Nil) != 1 || !strings.HasPrefix(r.Nil[0].Position, "f.go:") {
			t.Fatalf("nil step = %+v, issues = %+v; want one warning in f.go", s, r.Nil)
		}
		if r.Status != eval.Pass {
			t.Errorf("status = %s, want pass: warnings must not fail the gate\n%s", r.Status, summary(r))
		}
		if !strings.Contains(summary(r), "WARN  nil") {
			t.Error("summary does not show the warning")
		}
	})

	t.Run("strict", func(t *testing.T) {
		t.Parallel()
		cfg := config(fixture(t, files))
		cfg.StrictNil = true
		r := eval.Run(t.Context(), cfg)
		if r.Status != eval.Fail || step(t, r, "nil").Status != eval.Fail {
			t.Fatalf("status = %s, nil = %s; want both fail", r.Status, step(t, r, "nil").Status)
		}
		if r.Tests == nil || len(r.Tests.Failures) != 1 || r.Tests.Failures[0].Test != "TestDouble" {
			t.Errorf("failures = %+v, want TestDouble", r.Tests)
		}
		for _, name := range []string{"coverage", "fuzz"} {
			if got := step(t, r, name).Status; got != eval.Skip {
				t.Errorf("%s = %s, want skip after failing tests", name, got)
			}
		}
	})

	t.Run("build", func(t *testing.T) {
		t.Parallel()
		dir := fixture(t, map[string]string{"go.mod": goMod, "f.go": "package fixture\n\nfunc broken( {\n"})
		r := eval.Run(t.Context(), config(dir, "tidy", "fmt"))
		last := r.Steps[len(r.Steps)-1]
		if last.Name != "build" || last.Status != eval.Fail {
			t.Errorf("last step = %s %s, want the gate to stop at a failed build", last.Name, last.Status)
		}
	})

	t.Run("quick", func(t *testing.T) {
		t.Parallel()
		cfg := config(fixture(t, files))
		cfg.Quick = true
		r := eval.Run(t.Context(), cfg)
		var names []string
		for _, s := range r.Steps {
			names = append(names, s.Name)
		}
		if r.Mode != "quick" || !slices.Equal(names, []string{"build", "vet", "test"}) || r.Status != eval.Fail {
			t.Errorf("quick run: mode %s, steps %v, status %s; want build,vet,test failing", r.Mode, names, r.Status)
		}
	})

	t.Run("coverage", func(t *testing.T) {
		t.Parallel()
		src := goodSrc + "\n// Triple returns 3*n.\nfunc Triple(n int) int { return 3 * n }\n"
		cfg := config(fixture(t, map[string]string{"go.mod": goMod, "f.go": src, "f_test.go": goodTest}), "fuzz")
		cfg.MinCoverage = 90
		r := eval.Run(t.Context(), cfg)
		if s := step(t, r, "coverage"); s.Status != eval.Fail || r.Status != eval.Fail {
			t.Errorf("coverage step = %+v; want fail at 50%% < 90%%", s)
		}
	})

	t.Run("no ace binary", func(t *testing.T) {
		t.Parallel()
		cfg := config(fixture(t, files), "tidy", "fmt", "lint", "test", "coverage", "fuzz")
		cfg.Self = ""
		cfg.Skip = slices.DeleteFunc(cfg.Skip, func(s string) bool { return s == "vuln" })
		r := eval.Run(t.Context(), cfg)
		if step(t, r, "nil").Status != eval.Fail || step(t, r, "vuln").Status != eval.Fail {
			t.Errorf("nil/vuln without Config.Self should fail:\n%s", summary(r))
		}
	})
}

func TestRunBadDir(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := eval.DefaultConfig()
	cfg.Dir = file // .eval cannot be created inside a regular file
	r := eval.Run(context.Background(), cfg)
	if r.Status != eval.Fail || r.Steps[0].Name != "setup" {
		t.Errorf("report = %+v, want a failed setup step", r)
	}
}
