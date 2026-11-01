// Package scaffold writes ace's project configuration (Makefile, lint config,
// agent instructions and hooks, CI, pinned toolchain) into a Go module.
package scaffold

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"text/template"

	"golang.org/x/mod/modfile"
)

// GolangciLintVersion is the golangci-lint release pinned in generated
// mise.toml files.
const GolangciLintVersion = "2.15.0"

//go:embed templates
var templates embed.FS

// File is one generated file.
type File struct {
	Path    string      // slash-separated, relative to the module root
	Mode    fs.FileMode // permission bits
	Content []byte
	// Merge means an existing file keeps its content and only gains the
	// template's lines that it lacks (used for .gitignore).
	Merge bool
}

// files maps template names to output paths. Templates are stored without
// leading dots so that, for example, templates/gitignore is not itself a
// .gitignore inside this repository.
var files = []struct {
	tmpl, out string
	mode      fs.FileMode
	merge     bool
}{
	{"Makefile", "Makefile", 0o644, false},
	{"mise.toml", "mise.toml", 0o644, false},
	{"golangci.yml", ".golangci.yml", 0o644, false},
	{"gitignore", ".gitignore", 0o644, true},
	{"CLAUDE.md", "CLAUDE.md", 0o644, false},
	{"claude/settings.json", ".claude/settings.json", 0o644, false},
	{"claude/hooks/format-go.sh", ".claude/hooks/format-go.sh", 0o755, false},
	{"claude/hooks/stop-gate.sh", ".claude/hooks/stop-gate.sh", 0o755, false},
	{"github/workflows/ci.yml", ".github/workflows/ci.yml", 0o644, false},
}

// Data parameterises the templates.
type Data struct {
	Module              string // module path, e.g. github.com/you/project
	Name                string // last element of Module
	Ace                 string // command that runs ace, e.g. "go tool ace"
	GoVersion           string // e.g. "1.27.3"
	GolangciLintVersion string
}

// NewData returns template data for module, invoking ace as `go tool ace`
// and pinning the Go version ace itself was built with.
func NewData(module string) Data {
	return Data{
		Module:              module,
		Name:                path.Base(module),
		Ace:                 "go tool ace",
		GoVersion:           strings.TrimPrefix(runtime.Version(), "go"),
		GolangciLintVersion: GolangciLintVersion,
	}
}

// Render returns every generated file for d.
func Render(d Data) ([]File, error) {
	out := make([]File, 0, len(files))
	for _, f := range files {
		src, err := templates.ReadFile("templates/" + f.tmpl)
		if err != nil {
			return nil, err
		}
		t, err := template.New(f.tmpl).Option("missingkey=error").Parse(string(src))
		if err != nil {
			return nil, fmt.Errorf("parsing template %s: %w", f.tmpl, err)
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, d); err != nil {
			return nil, fmt.Errorf("rendering %s: %w", f.tmpl, err)
		}
		out = append(out, File{Path: f.out, Mode: f.mode, Content: buf.Bytes(), Merge: f.merge})
	}
	return out, nil
}

// Action describes what Write did with one file.
type Action string

// Actions reported by Write.
const (
	Created   Action = "created"
	Updated   Action = "updated"   // overwritten (Force) or merged
	Unchanged Action = "unchanged" // already up to date
	Skipped   Action = "skipped"   // exists with different content; use Force
)

// Result is the outcome for one file.
type Result struct {
	Path   string
	Action Action
}

// ModulePath reads the module path from dir/go.mod.
func ModulePath(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod")) //nolint:gosec // reading the target module's go.mod is the point
	if err != nil {
		return "", fmt.Errorf("reading go.mod (run `go mod init` first): %w", err)
	}
	mod := modfile.ModulePath(b)
	if mod == "" {
		return "", errors.New("go.mod has no module directive")
	}
	return mod, nil
}

// HasTool reports whether dir/go.mod declares a `tool` directive for pkg.
func HasTool(dir, pkg string) (bool, error) {
	name := filepath.Join(dir, "go.mod")
	b, err := os.ReadFile(name) //nolint:gosec // reading the target module's go.mod is the point
	if err != nil {
		return false, err
	}
	f, err := modfile.Parse(name, b, nil)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(f.Tool, func(t *modfile.Tool) bool { return t.Path == pkg }), nil
}

// Write writes files under dir. Existing files are left alone unless force
// is set, except merge files, which gain any missing lines.
func Write(dir string, files []File, force bool) ([]Result, error) {
	results := make([]Result, 0, len(files))
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f.Path))
		action, err := writeOne(p, f, force)
		if err != nil {
			return results, fmt.Errorf("writing %s: %w", f.Path, err)
		}
		results = append(results, Result{Path: f.Path, Action: action})
	}
	return results, nil
}

func writeOne(p string, f File, force bool) (Action, error) {
	old, err := os.ReadFile(p) //nolint:gosec // p is a fixed template path under the target dir
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if mkErr := os.MkdirAll(filepath.Dir(p), 0o750); mkErr != nil {
			return "", mkErr
		}
		return Created, os.WriteFile(p, f.Content, f.Mode) //nolint:gosec // hooks must be executable
	case err != nil:
		return "", err
	case bytes.Equal(old, f.Content):
		return Unchanged, nil
	case f.Merge:
		merged, changed := mergeLines(old, f.Content)
		if !changed {
			return Unchanged, nil
		}
		return Updated, os.WriteFile(p, merged, f.Mode) //nolint:gosec // same mode as the existing file kind
	case force:
		return Updated, os.WriteFile(p, f.Content, f.Mode) //nolint:gosec // hooks must be executable
	default:
		return Skipped, nil
	}
}

// mergeLines appends the lines of add that old lacks.
func mergeLines(old, add []byte) ([]byte, bool) {
	have := strings.Split(string(old), "\n")
	var missing []string
	for line := range strings.Lines(string(add)) {
		line = strings.TrimRight(line, "\n")
		if line != "" && !slices.Contains(have, line) {
			missing = append(missing, line)
		}
	}
	if len(missing) == 0 {
		return old, false
	}
	out := slices.Clone(bytes.TrimRight(old, "\n"))
	if len(out) > 0 {
		out = append(out, '\n')
	}
	out = append(out, strings.Join(missing, "\n")...)
	return append(out, '\n'), true
}
