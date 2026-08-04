# poly-lsp-mcp — convenience targets. Go is the only required tool;
# `make smoke-editor` / `make smoke-llm` need python3, and a few tests
# need gopls + git on PATH (skipped otherwise).
#
# `make build` / `make install` STAMP the source directory into the binary
# (-X main.srcDir), which turns on dev self-update: the binary rebuilds itself
# from this tree and re-execs whenever a .go file is newer than it. That
# matters here because poly-lsp-mcp is always SPAWNED (by dun, by an editor),
# never launched by hand, so nothing else in the loop notices it is stale.
# See selfupdate.go. Disable at runtime with POLY_LSP_NO_AUTOBUILD=1.
#
# A plain `go install .` leaves srcDir empty and never self-updates — that is
# the release build, and `make install-release` spells it out.

GO              ?= go
BINARY          ?= poly-lsp-mcp
PREFIX          ?= $(HOME)/go/bin
PKGS            := ./...
RACE_TIMEOUT    ?= 300s
TEST_TIMEOUT    ?= 180s
SRCDIR          := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
LDFLAGS         := -X main.srcDir=$(SRCDIR)

.PHONY: default
default: build

.PHONY: build
build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BINARY) .

.PHONY: install
install:
	$(GO) install -ldflags "$(LDFLAGS)" .

# The release build: no source stamp, so no self-update.
.PHONY: install-release
install-release:
	$(GO) install .

# Benchmarks. Generated fixtures, so a number means the same thing next month
# — never benchmark the live repo, it drifts with every commit.
#
#   make bench                     everything, quick
#   make bench BENCH=QueryShapes   one family
#   make bench-profile             + CPU profile into bench.cpu
#
# Compare two revisions with benchstat:
#   make bench > old.txt ; <change> ; make bench > new.txt ; benchstat old.txt new.txt
BENCH     ?= .
BENCHTIME ?= 20x
BENCHPKGS ?= ./mcp/ ./symbols/

.PHONY: bench
bench:
	$(GO) test $(BENCHPKGS) -run XXX -bench '$(BENCH)' -benchtime $(BENCHTIME) -count=1

.PHONY: bench-profile
bench-profile:
	$(GO) test ./mcp/ -run XXX -bench '$(BENCH)' -benchtime $(BENCHTIME) -count=1 \
		-cpuprofile bench.cpu -memprofile bench.mem
	@echo "profiles: bench.cpu bench.mem — inspect with: go tool pprof -top -cum bench.cpu"

.PHONY: test
test:
	$(GO) test -short -count=1 -timeout $(TEST_TIMEOUT) $(PKGS)

# Full suite without -short — runs the live gopls e2e tests and the
# gat-greeter fixture (needs git + gopls on PATH; needs ../gwag
# checkout for the gat fixture).
.PHONY: test-all
test-all:
	$(GO) test -count=1 -timeout $(TEST_TIMEOUT) $(PKGS)

# Standing gate: the race detector against the short suite. Catches
# concurrency regressions in DiagnosticStore, ParseCache, manager
# spawn/restart, proactive open / git prewarm goroutines, and
# Serve-loop ↔ child-LSP-readloop interactions. Skips live-gopls /
# live-gat e2e for speed; those run under test-race-all.
.PHONY: test-race
test-race:
	$(GO) test -race -short -count=1 -timeout $(RACE_TIMEOUT) $(PKGS)

# Belt-and-suspenders: race + full (no -short) so the live gopls
# tests also run under the detector. Slow + occasionally flaky on
# timing-tight LSP waits (race instrumentation adds ~3x overhead);
# pre-release check rather than per-commit gate.
.PHONY: test-race-all
test-race-all:
	$(GO) test -race -count=1 -timeout $(RACE_TIMEOUT) $(PKGS)

.PHONY: vet
vet:
	$(GO) vet $(PKGS)

.PHONY: fmt
fmt:
	$(GO) fmt $(PKGS)

# Real-binary LSP conformance smoke. Builds the binary, drives it
# over stdio with editor-shaped client capabilities, asserts the LSP
# spec contracts on responses.
.PHONY: smoke-editor
smoke-editor:
	python3 scripts/smoke/editor_smoke.py

# Live LLM end-to-end smoke. Builds the binary, points an MCP client
# at it, drives a real model through a rename + verify flow. Needs
# the upstream OpenAI-compatible endpoint reachable.
.PHONY: smoke-llm
smoke-llm:
	python3 scripts/smoke/llm_e2e.py

.PHONY: clean
clean:
	rm -f $(BINARY)
	$(GO) clean -testcache

# Quick sanity for a PR: short tests + race + vet. Fast enough to
# run on every commit.
.PHONY: check
check: vet test test-race

.PHONY: help
help:
	@echo "poly-lsp-mcp Makefile targets:"
	@echo ""
	@echo "  build           - go build into ./$(BINARY)"
	@echo "  install         - go install to \$$GOPATH/bin"
	@echo "  test            - short test suite (skips live-LSP / live-gat e2e)"
	@echo "  test-all        - full test suite, no -short"
	@echo "  test-race       - short suite + race detector (standing gate)"
	@echo "  test-race-all   - full suite + race detector (pre-release)"
	@echo "  check           - vet + test + test-race (PR gate)"
	@echo "  vet             - go vet"
	@echo "  fmt             - go fmt"
	@echo "  smoke-editor    - real-binary LSP conformance smoke"
	@echo "  smoke-llm       - live LLM end-to-end smoke"
	@echo "  clean           - remove built artifacts + test cache"
