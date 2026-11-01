package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runArgs(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(t.Context(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRunCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args     []string
		code     int
		contains string
	}{
		{nil, 2, "Usage:"},
		{[]string{"help"}, 0, "ace init"},
		{[]string{"version"}, 0, "ace "},
		{[]string{"bogus"}, 2, `unknown command "bogus"`},
		{[]string{"eval", "-nope"}, 2, "flag provided but not defined"},
		{[]string{"init", "-nope"}, 2, "flag provided but not defined"},
	}
	for _, tt := range tests {
		code, stdout, stderr := runArgs(t, tt.args...)
		if code != tt.code || !strings.Contains(stdout+stderr, tt.contains) {
			t.Errorf("ace %v = %d, output %q; want %d containing %q", tt.args, code, stdout+stderr, tt.code, tt.contains)
		}
	}
}

func TestInit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if code, _, stderr := runArgs(t, "init", "-C", dir, "-no-tools"); code != 1 || !strings.Contains(stderr, "go mod init") {
		t.Errorf("init without go.mod = %d, %q; want 1 suggesting go mod init", code, stderr)
	}

	gomod := "module example.com/acme/widget\n\ngo 1.26\n\ntool (\n\tgithub.com/koicro/ace-go/cmd/ace\n\tgolang.org/x/perf/cmd/benchstat\n)\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o600); err != nil {
		t.Fatal(err)
	}
	// Tools are already declared, so init must not run `go get`.
	code, stdout, stderr := runArgs(t, "init", "-C", dir)
	if code != 0 || strings.Contains(stdout, "go get") {
		t.Fatalf("init = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "created   .claude/hooks/stop-gate.sh") {
		t.Errorf("init output missing created files:\n%s", stdout)
	}
	b, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil || !strings.Contains(string(b), "# widget") {
		t.Errorf("CLAUDE.md = %q, %v", b, err)
	}

	_, stdout, _ = runArgs(t, "init", "-C", dir, "-no-tools")
	if strings.Contains(stdout, "created") {
		t.Errorf("second init should not create files:\n%s", stdout)
	}

	// A file in the way of a generated directory makes init fail.
	blocked := t.TempDir()
	for name, content := range map[string]string{"go.mod": gomod, ".claude": "not a dir"} {
		if err := os.WriteFile(filepath.Join(blocked, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, _ := runArgs(t, "init", "-C", blocked, "-no-tools"); code != 1 {
		t.Errorf("init with .claude as a file = %d, want 1", code)
	}
}

func TestEval(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":    "module example.com/x\n\ngo 1.26\n",
		"x.go":      "package x\n\n// One returns 1.\nfunc One() int { return 1 }\n",
		"x_test.go": "package x\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) {\n\tif One() != 1 {\n\t\tt.Fatal()\n\t}\n}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	code, stdout, stderr := runArgs(t, "eval", "-C", dir, "-quick")
	if code != 0 || !strings.Contains(stdout, "EVAL PASS (quick mode") {
		t.Errorf("eval -quick = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	code, stdout, _ = runArgs(t, "eval", "-C", dir, "-quick", "-json", "-skip=test")
	if code != 0 || !strings.Contains(stdout, `"status": "pass"`) {
		t.Errorf("eval -json = %d\n%s", code, stdout)
	}

	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n\nfunc {\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runArgs(t, "eval", "-C", dir, "-quick"); code != 1 {
		t.Errorf("eval on broken code = %d, want 1", code)
	}
}

func TestHelpers(t *testing.T) {
	t.Parallel()
	if got := splitList(" a, ,b,"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("splitList = %q", got)
	}
	if got := aceQuery(); got != "latest" && !strings.HasPrefix(got, "v") {
		t.Errorf("aceQuery = %q, want latest or a version", got)
	}
}
