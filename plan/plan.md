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

- [x] Extend `tslsmcp.yaml` with a `bindings` block.
      ```yaml
      bindings:
        - name: UserType
          sites:
            - {file: main.go, symbol: UserID}
            - {file: client.ts, symbol: UserID}
            - {file: worker.py, symbol: UserID}
      ```
- [x] `internal/symbols`: declared sites live in their own backing
      store; `Lookup` dedups against lexical sites at the same
      (file, line, col), declared wins.
- [x] `internal/bindings`: Resolver maps `symbol` site form. Missing
      symbols logged but not fatal. Empty name / empty sites list is
      validation-rejected.
- [x] `internal/server` wires bindings through `New(reg, mgr, bindings)`
      and applies them after `Build`. End-to-end test confirms
      `workspace/symbol UserType` returns UserID's sites in main.go.
- [x] Site forms: `jsonpath` (YAML/JSON value). Supported subset:
      `$.foo`, `$.foo.bar`, `$.foo[N]`, `$.foo[*]`. Wildcards work over
      arrays and map values; recursive descent / filters / slices /
      quoted keys are rejected with named errors. Evaluator uses
      `gopkg.in/yaml.v3` so the same code path handles both YAML and
      JSON (JSON is a strict subset of YAML). Position info comes from
      `yaml.Node.Line` / `Column`.
- [x] Site forms: `regex` (last resort). `BindingSite.Regex` is now
      `[]string` — a single site may carry multiple patterns and the
      resolver unions their matches. Each pattern allows 0 or 1
      capture group (whole match vs captured slice as the token);
      2+ groups rejected. Stdlib `regexp` (RE2), 1 MiB per-file cap,
      no implicit multiline anchoring. v0.2.x aliasing-safety: a match
      whose captured text doesn't equal the binding name is logged
      and skipped — this keeps `textDocument/rename` correct without
      `Site.Length` plumbing. Aliasing-via-regex is the future-work
      item that unlocks that.
- [x] `textDocument/rename` synthesizes a `WorkspaceEdit` from the
      symbol index. Confidence policy: if declared sites exist for the
      name at the cursor, rename only those; otherwise fall back to
      lexical. Aliasing protection: each candidate site is verified
      against the on-disk text — sites where text != name being renamed
      are skipped (so aliasing bindings like `{name: UserType, sites:
      [symbol: UserID]}` don't substitute the wrong token when
      UserType is renamed). `renameProvider: true` is advertised; we
      OWN this method (don't forward to child LSPs because they can't
      reach the other languages).
- [x] Cross-language rename validated live: a single rename of UserID
      at main.go:6:6 produces 7 atomic edits across main.go / client.ts
      / worker.py / config.yaml — including the YAML value site driven
      by a `jsonpath` binding. No single-language LSP can do that
      YAML one.
- [ ] `workspace/applyEdit` if we ever decide to apply edits ourselves
      instead of returning them in the rename response. Clients
      currently do the applying.

### Tier 3 — schema-anchored

A single entry under `schemas:` in tslsmcp.yaml is enough to bind every
workspace position for the schema's named entities.

```yaml
schemas:
  - file: api.proto
    dialect: proto
```

- [x] Proto dialect: regex-based parser extracts `message`, `enum`,
      `service`, and `rpc` declarations. Patterns are anchored to skip
      comments / strings / field types.
- [x] `Resolver.ApplySchemas` reads each schema, snapshots the workspace
      lookup BEFORE mutating the index (avoids feedback loops), then
      registers the schema declaration plus every existing index hit
      for the name as declared sites.
- [x] `Index.InsertDeclared` is now idempotent at (file, line, col), so
      user bindings + schema bindings that overlap don't produce
      duplicate edits at rename time.
- [x] Live demo against polyglot fixture: declaring `api.proto`
      produces a single rename that touches 7 files in 6 formats —
      proto + go + ts + py + yaml + sql + md — with 15 atomic edits.
- [x] OpenAPI dialect. Walks `gopkg.in/yaml.v3` *Node tree to extract:
      (a) every key under `components.schemas.<Name>` (OpenAPI 3.x),
      (b) every key under `definitions.<Name>` (Swagger 2.0 fallback),
      (c) every `operationId` scalar value inside path operations. JSON
      OpenAPI documents work the same way because JSON is a strict
      subset of YAML.
      Live polyglot smoke with both proto + openapi schemas declared:
      a single UserID rename now produces 18 atomic edits across 8 files
      in 7 formats (proto + openapi + go + ts + py + yaml + sql + md).
- [x] JSONSchema dialect. Walks the *yaml.Node tree like openapi —
      extracts (a) `$defs.<Name>` keys (Draft 2019-09+), (b)
      `definitions.<Name>` keys (Draft 4–7 fallback), (c) top-level
      `title` when scalar string. JSON and YAML JSONSchema documents
      handled by the same code path. `mappingKeys` helper is shared
      between openapi.go and jsonschema.go.
      Live polyglot smoke with all three dialects declared (proto +
      openapi + jsonschema): a single UserID rename now produces 20
      atomic edits across 9 files in 8 formats.
- [ ] Tree-sitter-protobuf upgrade (replace regex parser) once we
      hit a real codebase whose proto style breaks the regex MVP.

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

We shipped **Option A**: same binary, new subcommand. `tslsmcp mcp
--root <dir> [--config <file>]` boots a Model Context Protocol server
over newline-delimited JSON-RPC 2.0 on stdio. The MCP layer reuses the
symbol index, bindings resolver, and schema dialects so LLM agents get
the full cross-language stack the LSP layer already serves to editors.

- [x] `internal/mcp` package: lifecycle (initialize / shutdown /
      pre-init guards / double-init rejection / EOF-without-shutdown
      sentinel), tool registry, `tools/list`, `tools/call`. Reuses
      `jsonrpc.Message` for the shapes and swaps the LSP Content-Length
      framing for newline-delimited JSON via `encoding/json`'s streaming
      Decoder/Encoder.
- [x] Tools (v0.1):
  - `find_symbol(query)` — case-insensitive substring search across the
    cross-language index.
  - `find_references(name)` — every workspace position for an exact
    name, declared + lexical + schema-anchored.
  - `rename(name, newName)` — returns a list of file edits
    `{file, line, col, oldText, newText}` with the same confidence
    policy and aliasing-safety check as the LSP rename handler.
  - `list_bindings()` — catalog of declared bindings the index knows
    about (Tier 2 + Tier 3). Per name: site count, languages covered,
    every (file, line, col) position. Lets agents survey the
    cross-language model without running rename queries.
  - `document_symbols(file)` — every symbol in a single file, sorted
    by (line, col), with confidence tags. Accepts workspace-relative
    or absolute paths; output is always workspace-relative.
  - `refresh(workspace_root?)` — rebuild the index from disk. With no
    args, rebuilds the current root (after an agent finishes writing
    edits). With `workspace_root`, points the same MCP instance at a
    different absolute path (use case: one tslsmcp serves multiple
    git worktrees of the same project). Bindings and schemas
    configured at startup re-apply at the new root.
- [x] Workspace-relative file paths in tool output for stable
      cross-machine references.
- [x] Live polyglot smoke through all three tools: `find_symbol(UserID)`
      returns 24 hits across 9 languages; `rename(UserID, PersonID)`
      produces 20 atomic edits across proto + openapi + jsonschema + go
      + ts + py + yaml + sql + md.
- [ ] More tools as use cases surface: `list_bindings`,
      `document_symbols`, `did_save_refresh`, etc.
- [ ] Live editing tool: instead of returning edits for the agent to
      apply, atomically apply them ourselves on `apply_rename`.
- [ ] `resources/list` and `resources/read` for treating workspace
      files (or bindings catalog) as MCP resources.

## Non-goals (for now)

- Indexing the entire host filesystem; we only index inside the git root.
- Replacing any single child LSP. We multiplex, we don't reimplement.
- Sandboxing child LSPs. They run as the user.
- Windows support until someone asks.
