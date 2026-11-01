// Command ace sets up and runs an agent-friendly quality gate for Go modules.
//
// In a module:
//
//	go get -tool github.com/koicro/ace-go/cmd/ace
//	go tool ace init     # write Makefile, lint config, CLAUDE.md, hooks, CI
//	make eval            # or: go tool ace eval -strict-nil
//
// Usage:
//
//	ace init [-force] [-no-tools] [-C dir]
//	ace eval [-quick] [-strict-nil] [-skip=a,b] [-fuzztime=5s] [-min-coverage=80] [-json] [-C dir]
//	ace version
//
// ace also bundles NilAway and govulncheck (`ace nilaway`, `ace govulncheck`),
// so a module needs only one tool dependency.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"

	"go.uber.org/nilaway"
	"go.uber.org/nilaway/config"
	"golang.org/x/mod/semver"
	"golang.org/x/tools/go/analysis/singlechecker"
	"golang.org/x/vuln/scan"

	"github.com/koicro/ace-go/internal/eval"
	"github.com/koicro/ace-go/internal/scaffold"
)

const (
	acePkg       = "github.com/koicro/ace-go/cmd/ace"
	benchstatPkg = "golang.org/x/perf/cmd/benchstat"
)

func main() {
	// The bundled analyzers own the process: they parse os.Args and exit.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "nilaway":
			os.Args = append([]string{"ace nilaway"}, os.Args[2:]...)
			config.Analyzer.Flags.VisitAll(func(f *flag.Flag) { flag.Var(f.Value, f.Name, f.Usage) })
			singlechecker.Main(nilaway.Analyzer)
			return
		case "govulncheck":
			cmd := scan.Command(context.Background(), os.Args[2:]...)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			err := cmd.Start()
			if err == nil {
				err = cmd.Wait()
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "ace govulncheck:", err)
				os.Exit(3)
			}
			return
		}
	}
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "eval":
		return runEval(ctx, args[1:], stdout, stderr)
	case "init":
		return runInit(ctx, args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, "ace", version())
		return 0
	case "help", "-h", "-help", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "ace: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `ace sets up and runs an agent-friendly quality gate for Go modules.

Usage:
  ace init      write Makefile, lint config, CLAUDE.md, Claude Code hooks, CI and mise.toml
  ace eval      run the quality gate and write .eval/report.json
  ace version   print the ace version

Run "ace <command> -h" for flags.
`)
}

func runEval(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg := eval.DefaultConfig()
	fs := flag.NewFlagSet("ace eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var skip, exclude string
	var printJSON bool
	fs.StringVar(&cfg.Dir, "C", "", "run in `dir` (module root) instead of the current directory")
	fs.BoolVar(&cfg.Quick, "quick", false, "fast mode: build, vet and short tests only")
	fs.Float64Var(&cfg.MinCoverage, "min-coverage", cfg.MinCoverage, "minimum total statement coverage (percent)")
	fs.DurationVar(&cfg.FuzzTime, "fuzztime", cfg.FuzzTime, "time to spend on each fuzz target (0 disables fuzzing)")
	fs.StringVar(&skip, "skip", "", "comma-separated step names to skip ("+strings.Join(eval.Steps, ",")+")")
	fs.StringVar(&exclude, "cover-exclude", strings.Join(cfg.CoverExclude, ","), "comma-separated import-path substrings excluded from the coverage gate")
	fs.BoolVar(&cfg.StrictNil, "strict-nil", false, "fail the gate on NilAway findings instead of only reporting them")
	fs.BoolVar(&printJSON, "json", false, "print the full report as JSON to stdout instead of a summary")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg.Skip = splitList(skip)
	cfg.CoverExclude = splitList(exclude)
	if self, err := os.Executable(); err == nil {
		cfg.Self = self
	}

	report := eval.Run(ctx, cfg)
	if printJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintln(stderr, "ace:", err)
		}
	} else {
		eval.PrintSummary(stdout, report)
	}
	if report.Status != eval.Pass {
		return 1
	}
	return 0
}

func runInit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ace init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("C", ".", "module root to initialise")
	force := fs.Bool("force", false, "overwrite existing files that differ from the templates")
	noTools := fs.Bool("no-tools", false, "don't add ace and benchstat as go.mod tool dependencies")
	ace := fs.String("ace", "go tool ace", "command the generated files use to run ace")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	mod, err := scaffold.ModulePath(*dir)
	if err != nil {
		fmt.Fprintln(stderr, "ace init:", err)
		return 1
	}
	data := scaffold.NewData(mod)
	data.Ace = *ace
	files, err := scaffold.Render(data)
	if err == nil {
		var results []scaffold.Result
		results, err = scaffold.Write(*dir, files, *force)
		for _, r := range results {
			fmt.Fprintf(stdout, "%-9s %s\n", r.Action, r.Path)
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "ace init:", err)
		return 1
	}

	if !*noTools {
		var cmds [][]string
		for _, pkg := range []string{acePkg + "@" + aceQuery(), benchstatPkg + "@latest"} {
			name, _, _ := strings.Cut(pkg, "@")
			if ok, err := scaffold.HasTool(*dir, name); err == nil && !ok {
				cmds = append(cmds, []string{"get", "-tool", pkg})
			}
		}
		if len(cmds) > 0 {
			// `go get` leaves go.sum untidy, which the gate's tidy step rejects.
			cmds = append(cmds, []string{"mod", "tidy"})
		}
		for _, args := range cmds {
			fmt.Fprintf(stdout, "go %s\n", strings.Join(args, " "))
			c := exec.CommandContext(ctx, "go", args...) //nolint:gosec // fixed go subcommands built above
			c.Dir, c.Stdout, c.Stderr = *dir, stdout, stderr
			if err := c.Run(); err != nil {
				fmt.Fprintln(stderr, "ace init:", err)
				return 1
			}
		}
	}
	fmt.Fprintln(stdout, "\nNext: `mise install`, then `make eval`. Files marked skipped already exist; rerun with -force to overwrite them.")
	return 0
}

// version is the ace module version, or "(devel)" for a local build.
func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}

// aceQuery is the version to request when adding ace as a tool: the running
// version if it is a released one, so `go run ...@v1.2.3 init` pins v1.2.3.
func aceQuery() string {
	// Local builds are stamped "(devel)" or with VCS build metadata
	// ("v0.0.0-...+dirty"), neither of which `go get` accepts.
	if v := version(); semver.IsValid(v) && semver.Build(v) == "" {
		return v
	}
	return "latest"
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
