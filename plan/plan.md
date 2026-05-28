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
- [x] `internal/multiplex` supervisor (per-child):
  - [x] Spawn / Kill / Wait / Done / Err lifecycle on `exec.Cmd`.
  - [x] JSON-RPC client over `internal/jsonrpc` with request/response
        correlation by id, Notify, server→client notification callback,
        server→client request method-not-found stub.
  - [x] LSP Initialize handshake; capabilities returned as `json.RawMessage`
        so the next slice can union without losing fields.
  - [x] Test by spawning the tslsmcp binary as a child (TestMain builds it),
        verifying real `workspace/symbol` round-trip through the supervisor.
- [x] `internal/multiplex` manager:
  - [x] Map of language → Child with eager spawn during Start.
  - [x] `RouteByURI(uri)` returning the Child for a file's extension, or
        nil if the child has exited (callers fall back to symbol index).
  - [x] Capabilities() returns each child's raw ServerCapabilities for
        the server's initialize union (next slice).
  - [x] Shutdown sends shutdown+exit to every child, falls back to Kill.
  - [x] `(*symbols.Index).Languages()` helper so the server can decide
        which children are worth spawning from the workspace it just
        indexed.
  - [ ] Restart-on-crash with backoff *(deferred — needs policy choice)*.
- [x] Server integration:
  - [x] Generic `textDocument/*` forward through `Manager.RouteByURI`;
        notifications fire-and-forget, requests honor a 30s timeout
        before erroring. URIs with no child get a null reply.
  - [x] `textDocument/documentSymbol`: forward-first with parent-index
        fallback so the method always answers across the whole workspace.
  - [x] `textDocument/didSave` re-indexes parent AND forwards to the
        child so its in-memory view stays consistent.
  - [x] `mergeCapabilities` in `initialize` unions every child's caps
        and overlays the server's own (workspaceSymbolProvider,
        referencesProvider, documentSymbolProvider, textDocumentSync).
  - [x] `Server.New(reg, manager)` — manager may be nil so tests can
        skip child-spawn latency.
  - [x] `shutdown` drains the manager (5s timeout) before exit.
  - [ ] Workspace folder broadcast + `didChangeConfiguration` fanout
        *(not yet — needs per-child params shaping per LSP spec)*.
- [ ] `internal/treesitter`:
  - [ ] Parser registry (smacker/go-tree-sitter or
        tree-sitter/go-tree-sitter — decide when this PR lands).
  - [ ] Per-document parse cache keyed by content hash.
  - [ ] Identifier-extraction queries per language (`.scm` files).
- [ ] Fallback path: when the child LSP returns no result for `definition`,
      `references`, `documentSymbol`, or returns an empty edit for `rename`,
      synthesize a result from the tree-sitter index.
- [x] LSP conformance pack (`internal/server/conformance_test.go`):
      pre-init/post-shutdown gating with the right error codes (-32002,
      -32600), double-initialize rejection, exit-without-shutdown surfaces
      `ErrExitWithoutShutdown` so main.go log.Fatals with exit code 1,
      `jsonrpc:"2.0"` validation, framing edge cases. 18 tests. Covers
      the contract independently of any language-specific handler.
- [ ] LSP conformance smoke against a real editor (vs-code or
      `nvim-lspconfig`) — proves we satisfy editor-side expectations
      that automated tests miss (e.g., capability shape quirks).

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
- [ ] Swap each code language's extractor to a tree-sitter identifier
      query. Data formats (yaml/json/markdown) deliberately stay on
      `LexicalExtractor` — their string-literal contents are exactly
      the cross-language references we want indexed, and tree-sitter's
      `(string_scalar)` nodes would force us to scan inside them anyway.
  - [x] Go: union of `(identifier)`, `(field_identifier)`,
        `(type_identifier)`, `(package_identifier)` via
        smacker/go-tree-sitter. Comments and string contents now
        excluded. Builtins (`int64`, `string`, etc.) still filtered
        via the keyword set because grammar emits them as
        `type_identifier`.
  - [x] TypeScript / TSX: union of `(identifier)`,
        `(type_identifier)`, `(property_identifier)`, and
        `(shorthand_property_identifier)` via
        smacker/go-tree-sitter/typescript/tsx (the tsx grammar is a
        superset of pure .ts so one extractor covers both extensions).
  - [x] SQL / PostgreSQL: `(identifier)` via
        smacker/go-tree-sitter/sql. New language entry registered in
        `config.Default()` with extensions `.sql` / `.psql`, no
        default LSP. Polyglot fixture extended with
        `migrations/001_users.sql` so cross-language UserID now
        crosses go / ts / py / md / yaml / sql.
  - [x] Python: `(identifier)` via smacker/go-tree-sitter/python.
        Captures functions/classes/parameters/annotations/f-string
        interpolations. Keyword filter trimmed to just the builtins
        (`int`, `str`, `print`, `True`/`False`/`None`, etc.) the grammar
        surfaces as identifier nodes; proper Python keywords
        (`def`/`class`/`import`/…) are non-identifier nodes and don't
        need filtering. Docstrings, regular strings, and comments are
        excluded.
- [x] Watcher: rebuild a file's slice of the index on `didSave`. Wired via
      `textDocument/didSave` notification handler in `internal/server`.
- [x] `workspace/symbol`: return Index matches. Substring match on Name,
      case-insensitive, every Site emitted as a `SymbolInformation`.
- [x] `textDocument/references` **(lexical-only)**: extracts the word at
      the cursor, returns every Site for that name. LSP-result merge is
      blocked on multiplex; when that lands, lexical matches will be
      tagged as preview-only per the original plan.
- [x] End-to-end test in `internal/server/server_test.go` drives the
      full handshake over in-process `io.Pipe` pairs (no subprocess).
      Verified live against polyglot fixture: 14 `UserID` hits across
      README.md / client.ts / config.yaml / main.go / worker.py.

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
