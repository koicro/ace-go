package scaffold_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koicro/ace-go/internal/scaffold"
)

func TestRender(t *testing.T) {
	t.Parallel()
	files, err := scaffold.Render(scaffold.NewData("example.com/acme/widget"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]scaffold.File{}
	for _, f := range files {
		if bytes.Contains(f.Content, []byte("{{")) {
			t.Errorf("%s: unrendered template action", f.Path)
		}
		got[f.Path] = f
	}
	for path, want := range map[string]string{
		".golangci.yml":              "- example.com/acme/widget",
		"CLAUDE.md":                  "# widget",
		"Makefile":                   "ACE      ?= go tool ace",
		".claude/settings.json":      `"Bash(go tool ace:*)"`,
		".claude/hooks/stop-gate.sh": "go tool ace eval -quick",
		"mise.toml":                  `golangci-lint = "` + scaffold.GolangciLintVersion + `"`,
	} {
		if !bytes.Contains(got[path].Content, []byte(want)) {
			t.Errorf("%s does not contain %q", path, want)
		}
	}
	if m := got[".claude/hooks/format-go.sh"].Mode; m != 0o755 {
		t.Errorf("hook mode = %v, want 0755", m)
	}
	if !got[".gitignore"].Merge {
		t.Error(".gitignore should be merged, not overwritten")
	}
}

func TestWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := []scaffold.File{
		{Path: "a/new.txt", Mode: 0o644, Content: []byte("new\n")},
		{Path: "same.txt", Mode: 0o644, Content: []byte("same\n")},
		{Path: "mine.txt", Mode: 0o644, Content: []byte("template\n")},
		{Path: ".gitignore", Mode: 0o644, Content: []byte(".eval/\n*.test\n"), Merge: true},
		{Path: "ignore-same", Mode: 0o644, Content: []byte("x\n"), Merge: true},
	}
	write(t, dir, "same.txt", "same\n")
	write(t, dir, "mine.txt", "user content\n")
	write(t, dir, ".gitignore", "node_modules/\n*.test")
	write(t, dir, "ignore-same", "x\ny\n")

	results, err := scaffold.Write(dir, files, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []scaffold.Action{scaffold.Created, scaffold.Unchanged, scaffold.Skipped, scaffold.Updated, scaffold.Unchanged}
	for i, r := range results {
		if r.Action != want[i] {
			t.Errorf("%s: action = %s, want %s", r.Path, r.Action, want[i])
		}
	}
	if got := read(t, dir, "mine.txt"); got != "user content\n" {
		t.Errorf("existing file overwritten without force: %q", got)
	}
	if got := read(t, dir, ".gitignore"); got != "node_modules/\n*.test\n.eval/\n" {
		t.Errorf("merged .gitignore = %q", got)
	}

	results, err = scaffold.Write(dir, files[2:3], true)
	if err != nil || results[0].Action != scaffold.Updated {
		t.Fatalf("force: results = %v, err = %v", results, err)
	}
	if got := read(t, dir, "mine.txt"); got != "template\n" {
		t.Errorf("force did not overwrite: %q", got)
	}

	// A path that exists as a directory cannot be read as a file.
	if err := os.Mkdir(filepath.Join(dir, "isdir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := scaffold.Write(dir, []scaffold.File{{Path: "isdir", Content: []byte("x")}}, false); err == nil {
		t.Error("writing over a directory: want error")
	}
}

func TestModulePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := scaffold.ModulePath(dir); err == nil {
		t.Error("no go.mod: want error")
	}
	write(t, dir, "go.mod", "go 1.26\n")
	if _, err := scaffold.ModulePath(dir); err == nil {
		t.Error("no module directive: want error")
	}
	write(t, dir, "go.mod", "module example.com/x\n\ngo 1.26\n\ntool example.com/tool\n")
	if got, err := scaffold.ModulePath(dir); err != nil || got != "example.com/x" {
		t.Errorf("ModulePath = %q, %v; want example.com/x", got, err)
	}
	for pkg, want := range map[string]bool{"example.com/tool": true, "example.com/other": false} {
		if got, err := scaffold.HasTool(dir, pkg); err != nil || got != want {
			t.Errorf("HasTool(%q) = %v, %v; want %v", pkg, got, err, want)
		}
	}
	if _, err := scaffold.HasTool(t.TempDir(), "x"); err == nil {
		t.Error("HasTool without go.mod: want error")
	}
	write(t, dir, "go.mod", "module (\n")
	if _, err := scaffold.HasTool(dir, "x"); err == nil {
		t.Error("HasTool with malformed go.mod: want error")
	}
}

// TestRepoMatchesTemplates keeps this repository's own config generated from
// the templates it ships. Fix failures by editing internal/scaffold/templates
// and regenerating (see CLAUDE.md), not by editing the root files.
func TestRepoMatchesTemplates(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	data := scaffold.NewData("github.com/koicro/ace-go")
	data.Ace = "go run ./cmd/ace"
	files, err := scaffold.Render(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		got := read(t, root, f.Path)
		switch {
		case f.Path == "CLAUDE.md":
			// The repo appends a hand-written section after the generated part.
			if !strings.HasPrefix(got, string(f.Content)) {
				t.Errorf("CLAUDE.md does not start with the rendered template")
			}
		case f.Merge:
			for line := range strings.Lines(string(f.Content)) {
				if !strings.Contains(got, line) {
					t.Errorf("%s lacks template line %q", f.Path, strings.TrimSpace(line))
				}
			}
		case got != string(f.Content):
			t.Errorf("%s differs from its template", f.Path)
		}
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
