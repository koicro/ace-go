# Every target works without shell activation: mise's tool paths are prepended.
export PATH := $(shell mise bin-paths 2>/dev/null | tr '\n' ':')$(PATH)

FUZZTIME ?= 30s
BENCH    ?= .
COUNT    ?= 6

.DEFAULT_GOAL := check
.PHONY: setup check eval eval-json quick test fmt lint fuzz bench bench-baseline bench-compare cover clean

setup: ## Install the pinned toolchain and download modules
	mise install
	go mod download

check: eval ## Alias for eval

eval: ## Full quality gate (CI runs this). Writes .eval/report.json
	go run ./tools/eval

eval-json: ## Full gate, printing the JSON report to stdout
	go run ./tools/eval -json

quick: ## Fast inner loop: build, vet, short tests
	go run ./tools/eval -quick

test: ## Run tests directly (pass ARGS="-run TestX ./internal/slug")
	go test -race -shuffle=on $(or $(ARGS),./...)

fmt: ## Format all code (gofumpt + goimports)
	golangci-lint fmt

lint: ## Lint only
	golangci-lint run ./...

fuzz: ## Fuzz every target for FUZZTIME each
	go run ./tools/eval -fuzztime=$(FUZZTIME) -skip=tidy,fmt,lint,vuln

bench: ## Run benchmarks into .eval/bench/new.txt
	@mkdir -p .eval/bench
	go test -run='^$$' -bench='$(BENCH)' -benchmem -count=$(COUNT) ./... | tee .eval/bench/new.txt

bench-baseline: bench ## Record current benchmarks as the baseline
	cp .eval/bench/new.txt .eval/bench/base.txt

bench-compare: bench ## Compare current benchmarks against the baseline
	@test -f .eval/bench/base.txt || { echo "no baseline: run 'make bench-baseline' on the reference commit first"; exit 1; }
	go tool benchstat .eval/bench/base.txt .eval/bench/new.txt

cover: ## Open the HTML coverage report from the last eval
	go tool cover -html=.eval/cover.out

clean:
	rm -rf .eval

help:
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-15s %s\n", $$1, $$2}'
