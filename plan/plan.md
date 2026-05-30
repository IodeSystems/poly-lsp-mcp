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
  - [x] Restart-on-crash with backoff. Each Start spawn gets a
        watchdog goroutine waiting on child.Done. Unexpected exit
        triggers `restart(name)`: exponential backoff starting at
        `RestartInitialBackoff` (default 1s), doubling up to
        `RestartMaxBackoff` (default 30s), giving up after
        `RestartMaxAttempts` (default 5). The shutdown flag is
        re-checked before each attempt so an in-flight Shutdown
        cancels the retry loop cleanly. On success, the new child
        replaces the old one in the children/caps maps and gets its
        own watchdog. On exhaustion the language is dropped (so
        RouteByURI returns nil and callers fall back to the symbol
        index).
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
  - [x] Workspace folder broadcast + `didChangeConfiguration` fanout.
        `Manager.Broadcast(method, params)` sends one notification to
        every running child, logs per-child errors, never aborts the
        fanout. Server dispatch routes
        `workspace/didChangeConfiguration` and
        `workspace/didChangeWorkspaceFolders` through it. Params pass
        through as `json.RawMessage` so per-child shaping happens on
        the child's side — we don't try to second-guess LSP spec
        differences across child servers.
- [x] Tree-sitter integration (rolled into `internal/symbols` rather
      than a separate `internal/treesitter` package):
  - Binding: smacker/go-tree-sitter (decision locked in the Go slice).
  - Extractors implemented for go / typescript+tsx / python / sql via
    inline query constants (no .scm sidecar files; queries are
    compile-time constants per language). Tree replaces lexical for
    code languages; data formats stay on the regex `LexicalExtractor`
    on purpose.
- [x] Fallback path for the methods we already serve:
  `textDocument/documentSymbol` forwards to child LSP first and falls
  back to the symbol-index slice for the file; `references` and
  `workspace/symbol` are served from the index unconditionally; rename
  is owned by us (synthesizes a `WorkspaceEdit` from the unioned
  index). Per-method merging that combines child LSP results WITH the
  index for `definition` and `hover` is still a follow-up — needs
  per-method result-shape adapters.
- [x] LSP conformance pack (`internal/server/conformance_test.go`):
      pre-init/post-shutdown gating with the right error codes (-32002,
      -32600), double-initialize rejection, exit-without-shutdown surfaces
      `ErrExitWithoutShutdown` so main.go log.Fatals with exit code 1,
      `jsonrpc:"2.0"` validation, framing edge cases. 18 tests. Covers
      the contract independently of any language-specific handler.
- [x] LSP conformance smoke against a real-editor-shaped client.
      `scripts/smoke/editor_smoke.py` drives the binary as a real
      subprocess with the kitchen-sink ClientCapabilities a typical
      LSP client sends (nvim-lspconfig / vs-code / helix shape) and
      asserts every response against the LSP spec's required-field
      contracts: SymbolKind in 1..26, Range.end >= start, Location.uri
      is a file:// URI, WorkspaceEdit.changes keyed by URI with
      non-empty edits, textDocument/hover returns null (not -32601)
      so editor popups don't break. 7/7 checks pass against the
      polyglot fixture. Worth re-running before any release tagged
      with editor-facing changes; doubles as a precise complement to
      a true nvim/vs-code smoke when one is available.

**Phase 1 open decisions, now resolved:**
- LSP framework: stdlib won. `internal/jsonrpc` framing + the small
  typed structs in `internal/server/lsp.go` are enough; pulling in
  tliron/glsp or go.lsp.dev/protocol would add a dep without changing
  what we actually serve.
- Tree-sitter binding: smacker/go-tree-sitter won by virtue of
  bundling go/ts/tsx/python/sql grammars in one module. Worth
  revisiting if we ever need a grammar smacker doesn't ship.

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
- [x] Swap each code language's extractor to a tree-sitter identifier
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
- [x] Tree-sitter-protobuf via smacker/go-tree-sitter/protobuf
      replaces the regex parser. Query unions
      `(message (message_name (identifier)))`,
      `(enum (enum_name …))`, `(service (service_name …))`,
      `(rpc (rpc_name …))` so nested declarations work the same as
      top-level ones (the regex MVP missed those). Parsers pool;
      query compiles once via sync.Once. Stricter than the regex —
      requires a `syntax = "..."` directive at the top of the file
      to parse cleanly, which matches what real proto files
      actually look like.

## Phase 3 — stacked-branch index

Goal: switching branches in a stack doesn't re-parse the world.

- [x] Content-addressed parse cache. `internal/symbols.ParseCache` is
      keyed by `(language, sha256(content))`. `symbols.Build` accepts
      `WithCache(c)`; cache hits skip the extractor entirely.
      Two files with identical content (across branches, across
      worktrees, across renames) share one parse. Branch switches
      that don't change a file's bytes get cache hits automatically
      — the content-address scheme implicitly handles "files
      unchanged from parent branch" without any git awareness in our
      code.
- [x] Working-tree overlay is automatic: we hash files as they are on
      disk, so unsaved edits get fresh hashes and fresh parses on
      next refresh.
- [x] Server / MCP server / LSP server each hold a single
      ParseCache across refresh / didSave / handleInitialize. Tests
      verify the cache doesn't grow when content is unchanged, and
      grows by exactly one entry per changed file.
- [x] Eviction policy. `ParseCache` is now an LRU keyed on the same
      `(language, sha256(content))` tuple. `NewParseCache()` picks a
      default cap (5000 entries) so long-running servers have a
      stable memory ceiling; `NewParseCacheLRU(n)` is the explicit
      constructor (n=0 means unbounded, useful in tests). Get moves
      the entry to the front; Put adds at the front and evicts the
      back when over cap. Five new tests cover eviction order,
      promote-on-Get, promote-on-Put-replace, the unbounded mode,
      and the default-cap sanity check.
- [x] Disk persistence. `ParseCache.Save(w)` / `Load(r)` use a
      version-tagged gob format. `mcp.Server.SetCachePath(path)` opts
      a server into persistence: load on Serve start, save on Serve
      return via temp + Rename for atomicity. `main.go` wires
      `<root>/.tslsmcp/cache.gob` for the MCP subcommand; tests don't
      set the path, so they get in-memory-only behavior with no file
      pollution. Eight new tests cover round-trip, LRU-preserving
      Load, version mismatch handling, malformed input, nil-cache
      safety, merge-into-existing, end-to-end persistence across
      sessions, and missing-file first-run behavior.
- [ ] `internal/git` for explicit stack detection + eager prewarm.
      The on-demand cache covers the common "switched back to a
      branch you were just on" case for free. The remaining win
      (pre-parsing ancestor branches you haven't visited yet) needs
      `git read-tree` plumbing and a policy choice; defer.

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
- [x] Tools (v0.2 — surface trimmed from 10 to 6):

  The earlier surface (find_symbol / find_references / document_symbols /
  document_structure / list_bindings / rename / apply_rename / refresh /
  read_range / replace_range) had real ambiguity — preview vs apply,
  substring vs exact, document_symbols vs document_structure — that
  works against "one obvious way". v0.2 collapses to six tools that
  each do one job:

  - `structure(path, depth=1)` — hierarchical tour. At workspace level
    walks directories; at file level returns tree-sitter named children
    with BOTH the declaration range and the identifier's name range as
    separate fields. Calling on the workspace root implicitly content-
    hashes every file and refreshes the index for changed slices — no
    explicit refresh tool needed.
  - `node_references(file, range)` — references to the identifier at
    range. Agent passes the `nameStart*`/`nameEnd*` fields from
    structure; we read the text, use it as the lookup name, return
    every workspace position (lexical + declared + schema-anchored).
  - `node_read(file, range)` — text at range. Works on any file.
  - `node_edit(file, range, newText)` — atomic rewrite at range via
    temp + Rename. After the write the file's index slice is re-parsed
    so subsequent `node_references` sees the new state.
  - `node_delete(file, range)` — equivalent to node_edit with empty
    newText but states intent. Exact deletion (no surrounding-
    whitespace trimming).
  - `node_refactor(file, range, kind, ...kind_args)` — multi-modal
    refactor channel. v0.2 ships `kind="rename"`; future kinds
    (change_signature etc.) land here without growing the tool count.
    Rename preserves the declared-bindings + aliasing-safety semantics
    from the deleted apply_rename tool, atomically across every file.

  Workspace-relative paths in all tool output; absolute paths accepted
  on input. tslsmcp://workspace and tslsmcp://bindings resources are
  unchanged.

- [x] Live polyglot smoke against the 6-tool surface:
      `node_refactor(rename UserID, PersonID)` produces 9-file
      cross-language rewrite (proto + openapi + jsonschema + go + ts +
      py + yaml + sql + md); `node_references` returns 20 sites across
      8 languages; `structure(., depth=2)` walks the workspace tree.

- [x] Earlier (cut) tools and why:
      - find_symbol (substring) → cut; agents filter
        tslsmcp://bindings or pick exact names from structure.
      - find_references(name) → replaced by
        node_references(file, range) — point at a specific
        occurrence, no name-ambiguity.
      - document_symbols → cut; structure(file) covers it.
      - document_structure → renamed structure (now dispatches on
        directory vs file).
      - list_bindings (tool) → cut; tslsmcp://bindings resource
        still exposes the catalog.
      - rename (preview) → cut; node_refactor writes.
      - apply_rename → renamed node_refactor with kind=rename.
      - refresh → cut; structure() does implicit FS sweep,
        node_edit / node_delete refresh the file slice they wrote.
      - read_range / replace_range → renamed node_read / node_edit;
        node_delete is a sibling for explicit erasure.

- [x] Live editing semantics merged into node_edit / node_delete /
      node_refactor(rename). Per-file edits sorted (line desc, col
      desc) and applied right-to-left so byte offsets don't shift;
      each file goes through temp + Rename for partial-failure safety;
      file mode preserved so executable bits survive.
- [x] `resources/list` and `resources/read` MCP surface. Two
      resources land in v0.1:
      `tslsmcp://workspace` — JSON `{root, languages, names, declared}`
      so clients can sanity-check what tslsmcp indexed without a tool
      call;
      `tslsmcp://bindings` — same JSON payload as the `list_bindings`
      tool, exposed as a resource for MCP clients that pin resources
      into model context. Capability `resources: {}` advertised in
      initialize. More resources can be added by extending
      registerResources.

## Non-goals (for now)

- Indexing the entire host filesystem; we only index inside the git root.
- Replacing any single child LSP. We multiplex, we don't reimplement.
- Sandboxing child LSPs. They run as the user.
- Windows support until someone asks.
