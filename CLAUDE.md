# ace-go

Go module `github.com/koicro/ace-go`. Toolchain is pinned in `mise.toml`
(Go 1.27, golangci-lint v2); dev tools (govulncheck, benchstat) are `tool`
directives in `go.mod`, run them with `go tool <name>`.
Run `make setup` once. `make` targets work without mise shell activation.

## Definition of done

A change is done when **`make eval` exits 0**. Don't claim success otherwise.

`make eval` runs, in order: `tidy` → `fmt` → `build` → `vet` → `lint` →
`test` (race, shuffled, coverage) → `coverage` (≥80% outside `tools/`) →
`fuzz` (5s per target) → `vuln`. It writes `.eval/report.json`:

```sh
jq '.steps[] | select(.status=="fail")' .eval/report.json   # what failed, with file:line
jq '.tests.failures' .eval/report.json                        # failing tests + their output
jq '.coverage.packages[:5]' .eval/report.json                 # least-covered packages
jq '.lint' .eval/report.json                                  # lint issues
```

Inner loop while iterating: `make quick` (build, vet, `-short` tests), or a
targeted `go test -run TestName ./internal/pkg`. A Stop hook runs `make quick`
automatically when Go files changed and sends failures back to you. Go files
are formatted automatically after each edit.

Never edit `.eval/` by hand, never lower `-min-coverage`, and never add
`//nolint` or lint exclusions to make the gate pass without explaining why
in the same line. If `vuln` fails only because there's no network, rerun
with `go run ./tools/eval -skip=vuln` and say so.

## Writing code

- Layout: `internal/<pkg>` for library code, `cmd/<name>` for binaries,
  `tools/` for dev-only commands. Prefer the standard library.
- Errors: wrap with `fmt.Errorf("doing x: %w", err)`; compare with
  `errors.Is`/`errors.As`. Accept `context.Context` as the first parameter of
  anything that does I/O.
- Use modern Go: `for range n`, `slices`/`maps`/`cmp`, `strings.SplitSeq`,
  `b.Loop()` in benchmarks, `t.Context()` in tests. The `modernize` linter
  flags older idioms.

## Writing tests (see `internal/slug` for a worked example)

- Table-driven tests with `t.Run`, `t.Parallel()` where safe, messages in
  the form `Func(in) = got, want want`.
- Use `package foo_test` (black-box) by default.
- Add a `FuzzXxx` target for any parser, encoder, or function over untrusted
  input; assert invariants (round-trip, idempotence, no panic). When fuzzing
  finds a bug, the input is saved in `testdata/fuzz/` — commit it, it is now
  a regression test.
- Add an `ExampleXxx` with an `// Output:` comment for exported APIs; it is
  both documentation and a test.
- Fixtures go in `testdata/`.

## Performance work

Measure before and after; don't guess:

```sh
git stash && make bench-baseline && git stash pop
make bench-compare            # benchstat; only trust deltas with p < 0.05
make bench BENCH=BenchmarkMake COUNT=10
```

Profile with `go test -run='^$' -bench=X -cpuprofile=.eval/cpu.prof ./pkg`
then `go tool pprof -top .eval/cpu.prof`.
