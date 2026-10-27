# poly-lsp-mcp — convenience targets. Go is the only required tool;
# `make smoke-editor` / `make smoke-llm` need python3, and a few tests
# need gopls + git on PATH (skipped otherwise).
#
# Every build here is a RELEASE build — no target stamps anything into the
# binary, and none may. Dev self-update is opt-in at RUNTIME instead:
#
#   export LSP_REBUILD_PATH=$(abspath $(dir $(lastword $(MAKEFILE_LIST))))
#
# With that set, the binary rebuilds itself from that tree and re-execs
# whenever a .go file there is newer than it — which matters because
# poly-lsp-mcp is always SPAWNED (by dun, by an editor), never launched by
# hand, so nothing else in the loop notices it is stale. Unset it to turn the
# whole thing off. See selfupdate.go, and TestNoBuildTargetStampsTheBinary,
# which fails if a target ever grows an -X stamp again.

GO              ?= go
BINARY          ?= poly-lsp-mcp
BINDIR          ?= $(HOME)/.local/bin
PKGS            := ./...
RACE_TIMEOUT    ?= 300s
TEST_TIMEOUT    ?= 180s

.PHONY: default
default: build

.PHONY: build
build:
	$(GO) build -o $(BINARY) .

# Per-user bin dir on Linux and macOS. Built to a temp name and renamed
# over the old binary, so a running copy is never half-written.
.PHONY: install
install:
	@mkdir -p $(BINDIR)
	$(GO) build -o $(BINDIR)/.$(BINARY).tmp .
	mv -f $(BINDIR)/.$(BINARY).tmp $(BINDIR)/$(BINARY)
	@case ":$$PATH:" in *":$(BINDIR):"*) ;; *) echo "warning: $(BINDIR) is not on PATH" >&2 ;; esac

# Release tarball for the CURRENT os/arch, named as the release workflow names
# it (no version, so /releases/latest/download/<asset> is stable):
#   dist/poly-lsp-mcp_<os>_<arch>.tar.gz  holding poly-lsp-mcp (+ LICENSE, THIRD_PARTY_NOTICES, README.md if present)
# Unstamped, like every other target here; -tags release is a marker, not a stamp
# (doctor reads it to pick the release update check).
GOOS_NAME   := $(shell $(GO) env GOOS)
GOARCH_NAME := $(shell $(GO) env GOARCH)
DIST_ASSET  := dist/$(BINARY)_$(GOOS_NAME)_$(GOARCH_NAME).tar.gz

.PHONY: dist
dist:
	rm -rf dist/stage
	mkdir -p dist/stage
	CGO_ENABLED=1 $(GO) build -trimpath -tags release -o dist/stage/$(BINARY) .
	for f in LICENSE THIRD_PARTY_NOTICES README.md; do [ -f $$f ] && cp $$f dist/stage/ || true; done
	tar -czf $(DIST_ASSET) -C dist/stage $$(ls dist/stage)
	rm -rf dist/stage
	@echo $(DIST_ASSET)

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

# THIRD_PARTY_NOTICES lists every Go module and tree-sitter grammar in the
# release binary with its license (tools/notices). It fails on a license outside
# the allow-list. `notices-check` fails when the committed file is stale.
.PHONY: notices
notices:
	$(GO) run ./tools/notices -o THIRD_PARTY_NOTICES

.PHONY: notices-check
notices-check:
	$(GO) run ./tools/notices -check THIRD_PARTY_NOTICES

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
	@echo "  install         - build into \$$BINDIR (default ~/.local/bin)"
	@echo "  dist            - dist/$(BINARY)_<os>_<arch>.tar.gz for this machine"
	@echo "  test            - short test suite (skips live-LSP / live-gat e2e)"
	@echo "  test-all        - full test suite, no -short"
	@echo "  test-race       - short suite + race detector (standing gate)"
	@echo "  test-race-all   - full suite + race detector (pre-release)"
	@echo "  check           - vet + test + test-race (PR gate)"
	@echo "  vet             - go vet"
	@echo "  fmt             - go fmt"
	@echo "  smoke-editor    - real-binary LSP conformance smoke"
	@echo "  smoke-llm       - live LLM end-to-end smoke"
	@echo "  notices         - regenerate THIRD_PARTY_NOTICES (also checks licenses)"
	@echo "  notices-check   - fail when THIRD_PARTY_NOTICES is stale"
	@echo "  clean           - remove built artifacts + test cache"
