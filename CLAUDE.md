# ace-go

Go module `github.com/koicro/ace-go`. Toolchain is pinned in `mise.toml`
(Go 1.27, golangci-lint v2); dev tools (govulncheck, benchstat, nilaway) are `tool`
directives in `go.mod`, run them with `go tool <name>`.
Run `make setup` once. `make` targets work without mise shell activation.

## Definition of done

A change is done when **`make eval` exits 0**. Don't claim success otherwise.

`make eval` runs, in order: `tidy` → `fmt` → `build` → `vet` → `lint` →
`nil` → `test` (race, shuffled, coverage) → `coverage` (≥80% outside
`tools/`) → `fuzz` (5s per target) → `vuln`. It writes `.eval/report.json`:

```sh
jq '.steps[] | select(.status=="fail")' .eval/report.json   # what failed, with file:line
jq '.tests.failures' .eval/report.json                        # failing tests + their output
jq '.coverage.packages[:5]' .eval/report.json                 # least-covered packages
jq '.lint' .eval/report.json                                  # lint issues
jq '.nil' .eval/report.json                                   # potential nil panics (NilAway)
```

The `nil` step (NilAway) is blocking: `make eval` passes `-strict-nil`, so
any potential nil panic fails the gate. Each finding shows the nil flow from
source to dereference. If one is a genuine false positive, restructure the
code so the non-nil-ness is obvious rather than suppressing it. (Running
`go run ./tools/eval` without the flag reports findings as `WARN` only.)

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
- Nil safety — Go's type system won't stop a nil panic, so design it out:
  - "Not found" is `(T, bool)` or a sentinel error (`ErrNotFound`), never a
    nil pointer. Never `return nil, nil` (the `nilnil` linter enforces this).
  - Never return a typed nil as an `error`: `var e *MyErr; return e` makes
    `err != nil` true. Return a literal `nil` on success.
  - Make the zero value usable, or require a `NewXxx` constructor and keep
    fields unexported so half-initialised structs can't exist.
  - Initialise maps before writing (`make(map…)`); writing to a nil map panics.
  - Take values, not pointers, unless the function mutates or the type is
    large; check pointer parameters from outside the package at the boundary.
  - Return empty (non-nil) slices where the result is serialised: nil
    encodes as JSON `null`, empty as `[]`.
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
