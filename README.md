# ace

An agent-friendly quality gate and dev environment for Go modules.

One command, `make eval`, decides whether a change is done: tidy, format,
build, vet, lint (golangci-lint v2), nil-safety (NilAway), race-enabled tests,
coverage, fuzzing and govulncheck. It writes `.eval/report.json` naming the
failing step, test, file and line, so an AI agent can fix problems without
digging through logs. `ace init` also sets up Claude Code hooks that format Go
files on edit and refuse to end a turn while the code fails a quick eval.

## Install in a module

Requires Go 1.26+, [mise](https://mise.jdx.dev) (for golangci-lint) and `jq`
(used by the Claude Code hooks).

```sh
go get -tool github.com/koicro/ace-go/cmd/ace
go tool ace init        # writes the files below; adds benchstat; runs go mod tidy
mise install
make eval
```

`ace init` writes `Makefile`, `mise.toml`, `.golangci.yml`, `.gitignore`
(merged), `CLAUDE.md`, `.claude/settings.json`, `.claude/hooks/*` and
`.github/workflows/ci.yml`. Existing files are left alone (reported as
`skipped`) unless you pass `-force`. Re-run it after upgrading ace to pick up
template changes.

Upgrade with `go get -tool github.com/koicro/ace-go/cmd/ace@latest`.

## Commands

```text
ace init    [-force] [-no-tools] [-C dir]
ace eval    [-quick] [-strict-nil] [-skip=step,...] [-fuzztime=5s] [-min-coverage=80] [-json] [-C dir]
ace version
```

Steps: `tidy fmt build vet lint nil test coverage fuzz vuln`. `-quick` runs
only `build vet test` (with `-short`). The generated Makefile runs
`ace eval -strict-nil`, making NilAway findings blocking.

ace bundles NilAway and govulncheck as `ace nilaway` and `ace govulncheck`,
so a module needs only one tool dependency.
