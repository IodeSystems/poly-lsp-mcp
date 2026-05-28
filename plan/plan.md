# tslsmcp — roadmap

Fused multi-language LSP server. Child LSPs handle in-language semantics,
tree-sitter handles parsing and cross-language linkages, and a symbol index
above the multiplex layer is what makes "fused" mean something. MCP comes
later via an external bridge.

## Architecture (target)

```
        ┌───────── editor / MCP bridge ─────────┐
        │                                       │
        ▼                                       │
  ┌──────────────────────────────┐              │
  │  tslsmcp LSP                 │              │
  │ ┌──────────────────────────┐ │              │
  │ │ symbol index (lexical /  │ │  ← cross-language layer
  │ │ declared / schema)       │ │              │
  │ └──────────┬───────────────┘ │              │
  │            │ merge results   │              │
  │ ┌──────────▼───────────────┐ │              │
  │ │ multiplex (per-file)     │ │  ← single-language layer
  │ └─┬────────┬────────┬──────┘ │              │
  └───┼────────┼────────┼────────┘              │
      ▼        ▼        ▼                       │
   gopls   tsserver   pylsp   …   tree-sitter (for files
                                                w/o a child LSP,
                                                and for the index)
```

State is keyed by **stacked branch**: child branch indexes inherit from
parent branch indexes by content hash, so switching `feature/c → feature/b`
is cheap.

## Phase 0 — scaffold (DONE)

- [x] Go module, JSON-RPC framing (`internal/jsonrpc`), stdio loop
- [x] `initialize` / `initialized` / `shutdown` / `exit` handshake
- [x] Empty capabilities (placeholder)

## Phase 1 — multiplex + fallback edit (v0.1)

Goal: editor connects, opens any file, gets either real LSP semantics or
tree-sitter–derived hover/refs/edits. **Per-file routing only — no
cross-language stitching yet (that's Phase 2).**

- [x] `internal/config`: language registry (`ext → {lsp_cmd, treesitter_lang}`,
      yaml.v3, defaults for go/ts/py + treesitter-only md/yaml/json, wired
      into main with --config flag).
- [ ] `internal/multiplex` *(deferred during Phase 2 design pivot)*:
  - [ ] Child LSP process supervisor (spawn, restart-on-crash, drain, kill).
  - [ ] Per-child JSON-RPC client over `internal/jsonrpc`.
  - [ ] Forward `textDocument/*` to the child owning that URI.
  - [ ] Capabilities merge: union child `ServerCapabilities`, then mask with
        anything we override (e.g. we own `textDocument/rename` once
        cross-language rename lands).
  - [ ] Workspace folder broadcast + `didChangeConfiguration` fanout.
- [ ] `internal/treesitter`:
  - [ ] Parser registry (smacker/go-tree-sitter or
        tree-sitter/go-tree-sitter — decide when this PR lands).
  - [ ] Per-document parse cache keyed by content hash.
  - [ ] Identifier-extraction queries per language (`.scm` files).
- [ ] Fallback path: when the child LSP returns no result for `definition`,
      `references`, `documentSymbol`, or returns an empty edit for `rename`,
      synthesize a result from the tree-sitter index.
- [ ] LSP conformance smoke test: open vs-code or `nvim-lspconfig`, see that
      definition/references work across go + ts in one session.

**Open decisions (resolve in Phase 1):**
- LSP framework: stdlib (current) vs `tliron/glsp` vs `go.lsp.dev/protocol`.
- Tree-sitter binding: `smacker/go-tree-sitter` (mature, CGO) vs
  `tree-sitter/go-tree-sitter` (official, newer). Both CGO.

## Phase 2 — cross-language symbol index (v0.2)

Goal: `UserID` declared in `main.go` is *findable* from `client.ts`,
`worker.py`, `config.yaml`, and `package.json`. **Three tiers, sequenced.**

The index sits above multiplex: it consumes parser output from every file
regardless of which child LSP owns it, and answers `Refs(symbol)` from the
union. Results carry a confidence tag (`lsp` / `declared` / `lexical`) so
the editor can rank.

### Tier 1 — lexical (v0.2.0)

Cheap, noisy, immediate value for "find everywhere this name appears."

- [x] `internal/symbols` foundation:
  - [x] `Site` (file, line, col, language, confidence). Kind deferred until
        a consumer needs it.
  - [x] `Index` (name → []Site) with Lookup/Names/Refresh.
  - [x] `LexicalExtractor` + per-language keyword sets for go/ts/py.
        Data formats (yaml/json/markdown) skip the keyword filter on
        purpose — that's how string-literal sites become indexable.
  - [x] `Build(root, registry)` workspace walk, skipping
        .git/node_modules/vendor/__pycache__/dist/build.
  - [x] Verified end-to-end against `testdata/fixtures/polyglot`: `UserID`
        appears in go + ts + python + markdown + yaml; `GreetUser` crosses
        go + yaml + markdown.
- [ ] Swap each language's extractor to a tree-sitter `(identifier)` query.
- [ ] Watcher: rebuild a file's slice of the index on `didSave`. Depends on
      multiplex `textDocument/didSave` wiring.
- [ ] `workspace/symbol`: return Index matches.
- [ ] `textDocument/references`: merge LSP result + Index lexical matches.
      Lexical matches default to **shown but unselected** in the response so
      tools can preview.

### Tier 2 — declared bindings (v0.2.1)

Precise. Required for safe cross-language rename, including string-literal
sites (the unique value-add vs single-language LSPs).

- [ ] Extend `tslsmcp.yaml` with a `bindings` block. Same file by user
      decision (2026-05-28); revisit if it grows past ~1k lines.
      ```yaml
      bindings:
        - name: UserID
          sites:
            - {file: main.go, symbol: UserID}
            - {file: client.ts, symbol: UserID}
            - {file: worker.py, symbol: UserID}
            - {file: config.yaml, jsonpath: $.user_id_type}
      ```
- [ ] Site forms: `symbol` (identifier in any language), `jsonpath`
      (YAML/JSON value), `regex` (last resort).
- [ ] `textDocument/rename`: use declared sites by default. Lexical
      requires `--rename-confidence=lexical`.
- [ ] `workspace/applyEdit` synthesizes the combined edit atomically.

### Tier 3 — schema-anchored (deferred)

- [ ] Read `.proto`, OpenAPI, JSONSchema; derive bindings automatically.

## Phase 3 — stacked-branch index

Goal: switching branches in a stack doesn't re-parse the world.

- [ ] `internal/git`:
  - [ ] Detect stack via `git config branch.<name>.parent` or a
        `.tslsmcp/stack` file (TBD; survey gt/branchless conventions).
  - [ ] Compute the parent→child diff once per branch switch.
- [ ] Symbol-index store keyed by `(file_content_hash, language)`.
- [ ] On branch switch: invalidate only files whose content hash differs
      from the parent branch's snapshot.
- [ ] On working-tree edits: overlay over the branch's frozen index.

## Phase 4 — MCP

Two options:

- **Option A:** ship `cmd/tslsmcp-mcp` that speaks MCP and adapts to our
  LSP — same binary, two subcommands.
- **Option B:** point users at `isaacphi/mcp-language-server` configured
  to spawn `tslsmcp` as the LSP. Zero work for us, but no tslsmcp-specific
  MCP tools (e.g. `tslsmcp.cross_lang_refs`).

Probable answer: B for v0.1 ship, A once Phase 2 has bindings worth their
own MCP tool surface.

## Non-goals (for now)

- Indexing the entire host filesystem; we only index inside the git root.
- Replacing any single child LSP. We multiplex, we don't reimplement.
- Sandboxing child LSPs. They run as the user.
- Windows support until someone asks.
