package eval

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
)

// TestSummary aggregates a `go test -json` stream.
type TestSummary struct {
	Passed   int           `json:"passed"`
	Failed   int           `json:"failed"`
	Skipped  int           `json:"skipped"`
	Failures []TestFailure `json:"failures,omitempty"`
}

func (s *TestSummary) String() string {
	return fmt.Sprintf("%d passed, %d failed, %d skipped", s.Passed, s.Failed, s.Skipped)
}

// TestFailure is a failing test (or a package that failed to build or run)
// together with the output it produced.
type TestFailure struct {
	Package string `json:"package"`
	Test    string `json:"test,omitempty"`
	Output  string `json:"output"`
}

func (f TestFailure) String() string {
	name := f.Package
	if f.Test != "" {
		name += "." + f.Test
	}
	return "--- " + name + "\n" + strings.TrimRight(f.Output, "\n")
}

// testEvent mirrors the fields of cmd/test2json output that we use.
type testEvent struct {
	Action     string
	Package    string
	Test       string
	Output     string
	ImportPath string // set on build-output / build-fail events
}

func parseTestEvents(r io.Reader) *TestSummary {
	sum := &TestSummary{}
	output := map[string]*strings.Builder{}
	buf := func(key string) *strings.Builder {
		b, ok := output[key]
		if !ok {
			b = &strings.Builder{}
			output[key] = b
		}
		return b
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var ev testEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue // non-JSON noise
		}
		pkg := cmp.Or(ev.Package, ev.ImportPath)
		key := pkg + "\x00" + ev.Test
		switch ev.Action {
		case "output", "build-output":
			buf(key).WriteString(ev.Output)
		case "pass":
			if ev.Test != "" {
				sum.Passed++
			}
		case "skip":
			if ev.Test != "" {
				sum.Skipped++
			}
		case "fail":
			if ev.Test != "" {
				// A parent fails because a subtest did; the subtest is the useful report.
				if slices.ContainsFunc(sum.Failures, func(f TestFailure) bool {
					return f.Package == pkg && strings.HasPrefix(f.Test, ev.Test+"/")
				}) {
					continue
				}
				sum.Failed++
				sum.Failures = append(sum.Failures, TestFailure{Package: pkg, Test: ev.Test, Output: buf(key).String()})
				continue
			}
			// Package-level failure. Only report it separately when no test in
			// the package explains it (build failure, panic in init, TestMain...).
			if !slices.ContainsFunc(sum.Failures, func(f TestFailure) bool { return f.Package == pkg }) {
				sum.Failed++
				sum.Failures = append(sum.Failures, TestFailure{Package: pkg, Output: buf(key).String()})
			}
		}
	}
	return sum
}

// CoverageInfo is statement coverage computed from a cover profile.
type CoverageInfo struct {
	Total    float64           `json:"total"`
	Minimum  float64           `json:"minimum"`
	Packages []PackageCoverage `json:"packages"`
}

// PackageCoverage is statement coverage for a single package.
type PackageCoverage struct {
	Package string  `json:"package"`
	Percent float64 `json:"percent"`
}

// parseCoverProfile computes total and per-package statement coverage,
// ignoring packages whose import path contains any of the exclude substrings.
// Packages are sorted from least to most covered.
func parseCoverProfile(r io.Reader, exclude []string) (*CoverageInfo, error) {
	type block struct {
		stmts   int
		covered bool
	}
	blocks := map[string]block{} // "file:range" -> block, deduplicated
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		// file.go:12.34,15.2 3 1
		loc, rest, ok := strings.Cut(line, " ")
		stmtsStr, countStr, ok2 := strings.Cut(rest, " ")
		if !ok || !ok2 {
			return nil, fmt.Errorf("malformed cover profile line %q", line)
		}
		stmts, err1 := strconv.Atoi(stmtsStr)
		count, err2 := strconv.Atoi(countStr)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("malformed cover profile line %q", line)
		}
		b := blocks[loc]
		b.stmts = stmts
		b.covered = b.covered || count > 0
		blocks[loc] = b
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	type agg struct{ total, covered int }
	pkgs := map[string]*agg{}
	var all agg
	for loc, b := range blocks {
		file, _, _ := strings.Cut(loc, ":")
		pkg := path.Dir(file)
		if slices.ContainsFunc(exclude, func(e string) bool { return strings.Contains(pkg+"/", e) }) {
			continue
		}
		a := pkgs[pkg]
		if a == nil {
			a = &agg{}
			pkgs[pkg] = a
		}
		a.total += b.stmts
		all.total += b.stmts
		if b.covered {
			a.covered += b.stmts
			all.covered += b.stmts
		}
	}

	pct := func(a agg) float64 {
		if a.total == 0 {
			return 100
		}
		return float64(a.covered) * 100 / float64(a.total)
	}
	info := &CoverageInfo{Total: pct(all)}
	for name, a := range pkgs {
		info.Packages = append(info.Packages, PackageCoverage{Package: name, Percent: pct(*a)})
	}
	slices.SortFunc(info.Packages, func(a, b PackageCoverage) int {
		return cmp.Or(cmp.Compare(a.Percent, b.Percent), cmp.Compare(a.Package, b.Package))
	})
	return info, nil
}

// FuzzTarget is a fuzz test discovered by `go test -list`.
type FuzzTarget struct {
	Package string
	Name    string
}

// parseFuzzList parses `go test -list=^Fuzz ./...` output, in which each
// package's test names are followed by an "ok  <pkg>  <time>" line.
func parseFuzzList(out []byte) []FuzzTarget {
	var targets []FuzzTarget
	var pending []string
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
		case fields[0] == "ok" && len(fields) >= 2:
			for _, n := range pending {
				targets = append(targets, FuzzTarget{Package: fields[1], Name: n})
			}
			pending = pending[:0]
		case len(fields) == 1 && strings.HasPrefix(fields[0], "Fuzz"):
			pending = append(pending, fields[0])
		default:
			pending = pending[:0] // "?  pkg [no test files]" and other noise
		}
	}
	return targets
}

// LintIssue is a single golangci-lint finding.
type LintIssue struct {
	Linter string `json:"linter"`
	Text   string `json:"text"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

func (i LintIssue) String() string {
	return fmt.Sprintf("%s:%d:%d: %s (%s)", i.File, i.Line, i.Column, i.Text, i.Linter)
}

func parseLint(file string) ([]LintIssue, error) {
	b, err := os.ReadFile(file) //nolint:gosec // our own .eval/lint.json
	if err != nil {
		return nil, err
	}
	var doc struct {
		Issues []struct {
			FromLinter string
			Text       string
			Pos        struct {
				Filename string
				Line     int
				Column   int
			}
		}
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	issues := make([]LintIssue, 0, len(doc.Issues))
	for _, is := range doc.Issues {
		issues = append(issues, LintIssue{
			Linter: is.FromLinter, Text: is.Text,
			File: is.Pos.Filename, Line: is.Pos.Line, Column: is.Pos.Column,
		})
	}
	return issues, nil
}

// NilIssue is a potential nil panic reported by NilAway.
type NilIssue struct {
	Position string `json:"position"` // file:line:col, relative to the module root
	Message  string `json:"message"`  // includes the nil flow from source to dereference
}

func (i NilIssue) String() string {
	return i.Position + ": " + strings.TrimSpace(i.Message)
}

// parseNilAway parses `nilaway -json` output, shaped as
// {"<pkg>": {"nilaway": [{"posn": ..., "message": ...}]}}. An analyzer
// error replaces the list with {"error": "..."}.
func parseNilAway(out []byte, root string) ([]NilIssue, error) {
	var doc map[string]map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("parsing nilaway output: %w", err)
	}
	issues := []NilIssue{}
	for pkg, analyzers := range doc {
		for _, raw := range analyzers {
			var diags []struct{ Posn, Message string }
			if err := json.Unmarshal(raw, &diags); err != nil {
				var e struct{ Error string }
				if json.Unmarshal(raw, &e) == nil && e.Error != "" {
					return nil, fmt.Errorf("nilaway on %s: %s", pkg, e.Error)
				}
				return nil, fmt.Errorf("parsing nilaway output for %s: %w", pkg, err)
			}
			for _, d := range diags {
				issues = append(issues, NilIssue{Position: relPath(d.Posn, root), Message: d.Message})
			}
		}
	}
	// A package is analysed both alone and as compiled with its tests, so the
	// same finding can appear under two package keys.
	slices.SortFunc(issues, func(a, b NilIssue) int {
		return cmp.Or(cmp.Compare(a.Position, b.Position), cmp.Compare(a.Message, b.Message))
	})
	return slices.Compact(issues), nil
}

// relPath trims root from p. The comparison ignores case because macOS paths
// are case-insensitive and tools may report /dev where the cwd says /Dev.
func relPath(p, root string) string {
	prefix := strings.TrimSuffix(root, "/") + "/"
	if root != "" && len(p) > len(prefix) && strings.EqualFold(p[:len(prefix)], prefix) {
		return p[len(prefix):]
	}
	return p
}
