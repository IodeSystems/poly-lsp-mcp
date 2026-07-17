# poly-lsp-mcp — done (archive of completed work)

> Moved here from plan.md as phases completed. Current state + active work live
> in plan.md; deferred opt-ins in icebox.md.

## :explain cost tree — commit 3 (DONE 2026-07-17)

- [x] `:explain <selector>` is a query MODE (a prefix stripped by one shared
  `splitExplain` at both entry points — CLI + node_query — kept out of the
  grammar so it can't nest). It runs the query, then returns a COST TREE not
  matches: each element's a-priori `est` (an exact #name/[name=] reads
  NameFreq, a bare class reads classCounts; edges/`*` are `?` — no fan-out in
  the index) beside `measured` (the commit-1 trace), degrading to a `>x` LOWER
  BOUND on the element the budget tripped in and `—` for elements never
  reached. node_query returns `{"explain": rows, "truncated"}` — the
  result-shape fork, resolved as a mode so plain queries are untouched. The
  `est` estimator is the shared number the descendant-chain planner will
  reorder on. Tested: splitExplain boundaries, est source, `>x`/`—` on a blow,
  and node_query returning a trace not matches.

## A-priori cost tallies — :explain commit 2 (DONE 2026-07-17)

- [x] The estimator's free cardinality sources. `Index.NameFreq(name)`: O(1) raw
  site tally across the three stores — the selectivity signal (rare name narrows
  hard). `Server.classCounts()`: symbols-per-class, but the class lives in the
  TREE not the index, so it is a one-time full-symbol walk MEMOIZED against a new
  `Index.gen` mutation counter (bumped under every write lock, read via
  `Generation()`) — recomputed only when the index changes, never spends the
  query budget. Design correction vs the plan: `outDegree`/fan-out was NOT
  built — the index has no edges — deferred to commit 3 where a consumer can
  validate it. `Index.gen` doubles as the memo key for the future inversion-floor
  cache. Tested: counts match the actual `func`/`struct` queries, same instance
  within a generation, fresh after a mutation. Commit 3 (`:explain` prefix + est
  column + `>x` floors) consumes these.

## Budget-blow cost trace — :explain commit 1 (DONE 2026-07-17)

- [x] A budget blow renders the selector as a per-element cost breakdown with
  `← budget ran out here` on the culprit, replacing the generic warning; the CLI
  prints it, node_query carries it as a `cost` array beside `truncated`. So the
  reader narrows the RIGHT element. Always-on and ~free: `evalElems` pushes a
  `costFrame` per element, `spend()` bumps the top frame's counter (slice index,
  no map write), flushed to `elemCost` on pop; `blownElem` = top frame when
  `workLeft` first goes negative. No grammar change. `:explain` commits 2 (index
  tallies / a-priori est) and 3 (the `:explain` prefix + `>x` floors) still open.

## annotation + comment as first-class nodes (DONE 2026-07-17)

- [x] **`annotation` node**: decorators (Python/TS) and struct-tag keys (Go)
  are `.annotation` children of the symbol they mark — so "who is annotated
  with X" is structural: `func:any(annotation#route)`, `#'T.Name' > annotation`.
  Leaf = last id (#route), plus a virtual-FQN alias as written (#'app.route')
  via Symbol.Alias → nodeIDs. TS decorators lift to the export_statement
  wrapper; Go directive/doc COMMENTS (no AST node) stay with :annotated.
- [x] **`::comment` pseudo-element**: the doc block above a decl, contiguous
  comment lines JOINED into one node (Go emits one per `//` line — rejoined).
  GENERATED on demand from the symbol's stored span (Symbol.Comment* →
  treeNode.commentAt), so it is invisible to `*` and the containment walk, like
  ::grep — verified `#'f' > *` returns args, never the comment.
  `func:not(:any(::comment))` = undocumented (grep-free);
  `::comment:contains('TODO')`; address = file@line. Contiguity required (a
  blank-separated comment is not the doc). Works Go/TS/Python. (First built as
  a tag `comment`, then converted to `::comment` — a doc block is trivia, not a
  member, so `*` should not surface it.)
- `annotation` stays a real tag (a decorator IS a member you enumerate);
  `argument` too (never needed `::arg`). `return` as a node → icebox.

## :annotated + bracket-aware regex — power queries (DONE 2026-07-17)

- [x] **`:annotated('pat')`**: "who is annotated with X" had no answer — ::grep
  gives the annotation LINE, :contains greps the body, neither names the SYMBOL
  the decorator sits on (it is outside the decl's span). :annotated greps the
  decorator/annotation/doc block ON a declaration and returns the decl. One
  predicate spans languages: ABOVE the span for Python/TS decorators (stacked
  included), INSIDE it for Go (doc comment folded into the decl). Verified
  across Go/Python/TS; proven distinct from :contains.
- [x] **`~=` bracket-aware**: a `]` inside a regex char class was read as the
  attribute's closing bracket, so `func[name~=^[A-Z]]` (exported funcs)
  compiled the truncated `^[A-Z` and errored. The value reader now balances
  `[`/`]` for the regex op (POSIX classes too); quoting stays the escape for
  `\]`.
- These compose into sound (text/structure, no edge-guessing) power queries:
  `func[name~=^[A-Z]]:not(:annotated('-E (//|/*)'))` = undocumented public API
  (354 on this workspace); `func:annotated('@app.route')` = route handlers.
- **Not shipped: `:recursive`** — lexically unsound (func Write calling
  w.Write reads as recursive). → icebox: needs LSP call-target resolution.

## Leading-ref cardinality pushdown + containment attribution (DONE 2026-07-17)

- [x] **Pushdown** (`pushdownLeadingRef`): a global leading ref filtered to one
  exact bare-leaf far name — `::in.call#'Save'` — no longer expands the implied
  universal host to every symbol. Candidate hosts are the far ends of X's
  OPPOSITE-direction edges (an out-edge H→X is the same site as X's in-edge
  far=H), derived from X's handful of declarations. refMatches then runs
  unchanged over the small host set, so output is identical by construction —
  the fast path only skips hosts that provably cannot match. Fires only when
  the expansion is global (tips is the project node), elem 0 is the synthesized
  universal host, and the filter is one bare leaf declsOf resolves completely
  (dotted/address ids keep the full scan). Measured 6 candidate hosts vs 4,206
  for #Save; `::in.call#'New'` 158k → 86k work.
- [x] **Containment attribution**: fixing the pushdown surfaced a real
  double-count. Edges were attributed to a host by sym-path name alone
  (`site.encl == n.sym`), but paths are not unique — a `module main` (the
  package clause) and a `func main` share "main", so a call inside func main
  was emitted by BOTH. buildOutRefs now also requires the site line within the
  host's span, and nodeByAddr disambiguates colliding paths by containment.
  On the real workspace this took `::out.call#'New'` from 84 (one edge doubled)
  to 83. Tested with proven teeth: the guard reverted, the query returns 2.
- [x] **Equivalence + non-regression gated** (`pushdown_test.go`): pushdown ==
  full scan across ::in/::out, kinds, [name=], trailing combinator, empty, and
  never costs more; anchored and non-universal forms are untouched; common dev
  queries confirmed non-pathological at the default budget.

## Child-LSP precision pass (DONE 2026-07-17)

- [x] **`refConf` is live**: every edge row carries `conf: "lsp" | "lexical"`.
  It was a dead placeholder — declared, hardcoded to "lexical", never read.
- [x] **Ask only when unsure, narrow never invent** (`mcp/precision.go`). An
  edge with ONE candidate is not a guess and costs nothing; only an ambiguous
  one (>1 far end after the scope fix — avg 2.58) buys a
  `textDocument/definition` round-trip. The reply PICKS from the candidates
  lexical already found: if it points outside the modelled tree (stdlib,
  vendor, generated) the lexical candidates stand, so the pass can never add
  an edge no one reviewed.
  - `::out`: narrows the far-end list to the one really referenced.
  - `::in`: the far end is never in doubt — whether the SITE refers to the
    target is. A definition landing elsewhere is conclusive ("it is that one"
    means "it is not this one"), so the coincidental edge is dropped.
- [x] **Capped and loud**: `defaultLSPResolveCap` = 200 round-trips/query
  (`SetLSPResolveCap` for tests), 2s each. Past the cap the rest stay lexical
  and the result's `edges` note says how many were settled and that the
  remainder are CANDIDATES. Same contract as the work budget.
- [x] **Degrades to lexical everywhere**: no manager (the `query` CLI), a
  tree-sitter-only language, a timeout, a dead child, an unparseable reply.
  Precision is an upgrade, never a dependency.
- [x] **Verified against real gopls** on a two-package `Save` collision that
  lexical scope cannot settle: `Run::out.call` narrowed from
  [cache.Save, store.Save] to store.Save with conf=lsp; `cache.Save::in.call`
  went 1 → 0 (that edge was main.go's call to store.Save, matched on the
  name); `store.Save::in.call` kept its real caller. The e2e tests needed a
  manager-bearing session — `startSessionFull` has no manager, so a precision
  test built on it passes by testing nothing.

## Query CLI, deterministic truncation, name/path axes (DONE 2026-07-17)

- [x] **`poly-lsp-mcp query [flags] <selector>`** — compiles + evaluates one
  selector and prints a tree grouped by file. Same hand-rolled dispatch as
  `mcp`. Flags: `--root --config --limit --offset --budget`. `?` prints the
  grammar. `bin/dev` is an auto-building launcher (mcpshell's pattern);
  `/bin/poly-lsp-mcp-*` is gitignored, `bin/dev` is tracked.
- [x] **`Server.QueryText`** (`mcp/query_text.go`) — the CLI's renderer over
  the SAME parse/build/evaluate path node_query serves. Verified at parity on
  budget-safe selectors (1 / 787 / 20 exact vs node_query).
- [x] **`Server.BuildIndex`** — extracted from `handleInitialize`. Refs resolve
  through `s.index`, so a caller that skipped it got a tree that answered
  containment normally and silently found NO references. Both entry points now
  share one definition of "indexed workspace"; the manager/prewarm/watch bits
  stay server-only.
- [x] **Deterministic truncation** (`ordered()`, `mcp/query.go`) — node sets are
  `map[*treeNode]bool`; ranging them is randomized per run. Invisible while a
  query completes (evaluate sorts), fatal once the work budget trips: the
  cutoff landed wherever the random walk was, so the same selector answered
  24/26/48/64/74/77 across runs. Every loop that can reach `e.spend` now walks
  in document order. Regression test asserts a tripped budget is byte-identical
  across 25 runs, and FAILS against the pre-fix code.
- [x] **`[path]` axis + de-leaked `[name]`** — `nodeIDs` (which `#id` uses)
  includes a symbol's `<file>#<sym>` address, so `[name]` quietly answered path
  questions: `func[name*=test]` returned 508 (every func in a _test.go file)
  where 1 was actually named *test*. Now `[name]` = `nameIDs` (what it's
  CALLED), `[path]` = the workspace-relative file path (where it LIVES), `#id`
  unchanged and still pins addresses. **BREAKING**; the `#id ≡ [name=id]` law
  no longer holds for addresses and the grammar help says so.

- [x] **The index is inverted ONCE per query (`sitesByFile`).** `fileSites(rel)`
  answered "the sites in ONE file" by sweeping EVERY name and EVERY site in the
  workspace and discarding everything outside that file — a whole-workspace
  sweep per file, through `LookupExisting`, which stats every occurrence.
  Attribution showed it WAS the budget: 65,942 of `#nodePath::out`'s 74,523
  units (88%) and 461,594 of `func#main::out.call`'s 470,490 (98%) — seven
  sweeps of the same data. Now one inversion (name→sites into file→sites) with
  one stat per FILE. Everything fits the 200k default:
  `func#main::out.call > func` 20 matches (was budget-dead), `func::out` the
  full 24,590 (was 7.65M work), `func::in` 2,752 — each identical to a 100M
  budget, so complete rather than truncated. CPU: syscalls 52% → 3.1%,
  LookupExisting 66.8% → off the profile, 3x `#New::out` 16.3s → 1.38s.
- [x] **Edges build ONE direction (12-16x on `::out`).** `buildRefs` built both
  halves and let the caller filter, so `::out` paid the `::in` bill — and the
  incoming half sweeps every occurrence of the name workspace-wide. Cost was a
  function of NAME FREQUENCY, not of the symbol: measured ~20-45k work units per
  occurrence (#nodePath 2 occurrences = 74k … #New 93 = 1.78M), where the whole
  default budget is 200k. Asking a direction was free — `#Default::out` and
  `#Default::in` cost an identical 1,190,365. Now `refNodes(n, dir)` builds and
  caches each half separately (`buildOutRefs` / `buildInRefs`): #New::out 1.78M →
  140k, #Default::out 1.19M → 74k, **same matches**, and the residual base no
  longer scales with name frequency. `TestOutQueryNeverBuildsIncomingRefs`
  asserts the contract (no node has `refsInLoaded` after an ::out query) and
  fails against build-both.
- [x] **`~=` is the regex op — the language's OR.** `[path~=test|smoke]`.
  Unanchored RE2 (Go stdlib), compiled at PARSE time so a bad pattern is a
  selector error, never a silent zero-match. `~=` used to be an error (CSS's
  word-list match is worthless on names/paths); regex is what callers reach
  for, and anchors subsume `^=`/`$=`/`*=` exactly (verified: `[path~=^config/]`
  ≡ `[path^=config/]`, both 24).
  - **No `&&`/`||` operators.** A sum-of-products value grammar was started and
    scrapped: AND is already CSS-native — a compound conjoins, `[path*=ma][path*=in]`
    (verified 30 vs 63/129 apart) — and OR is the regex's own `|`. Inventing two
    operators would have duplicated both.
  - `#id` is never a regex: an id NAMES one thing.
- [x] **Alternation under a LITERAL op still refused loudly** — `[path*=a|b]`
  matched the literal "a|b", found nothing, and a wrapping `:not()` then
  excluded nothing and returned the whole set looking like a filter that worked
  (820/820 funcs). Now an error naming the regex repair, with quoting
  (`[path*='a|b']`) as the escape for a real '|'. Single `|`/`&` stay literal
  characters — `R&D.md` is a real filename.

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
  - [x] Test by spawning the poly-lsp-mcp binary as a child (TestMain builds it),
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

- [x] Extend `poly-lsp-mcp.yaml` with a `bindings` block.
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

A single entry under `schemas:` in poly-lsp-mcp.yaml is enough to bind every
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
      `<root>/.poly-lsp-mcp/cache.gob` for the MCP subcommand; tests don't
      set the path, so they get in-memory-only behavior with no file
      pollution. Eight new tests cover round-trip, LRU-preserving
      Load, version mismatch handling, malformed input, nil-cache
      safety, merge-into-existing, end-to-end persistence across
      sessions, and missing-file first-run behavior.
- [x] `internal/git` package: `Repo` wrapper around the `git`
      binary (rev-parse, ls-tree, show). `FromCWD` discovers the
      repo root; returns `ErrNotInRepo` / `ErrGitMissing` so
      callers can branch. `CurrentBranch`, `UpstreamBranch`
      (strips `origin/` prefix when a local branch by the same
      name exists), `AncestorChain(branch, maxDepth)` (follows
      upstream pointers, cycle-guarded), `ListFiles(branch)`,
      `FileAt(branch, path)`. Tests build real git fixtures with
      multi-branch stacks.
- [x] `symbols.PrewarmFromBranch(repo, branch, reg, cache)`: walks
      every file in a branch, runs the matching extractor if the
      content-addressed cache hasn't seen `(language, sha256)`
      yet, returns the count of fresh parses. Skipped: unknown
      extensions, oversized files (>1 MiB cap, same as Build),
      unreadable refs (submodule pointers). Idempotent across
      branches sharing identical content — that's the central
      stacked-branch payoff. Tests cover seed-then-rerun (all
      hits second pass) and ancestor reuse (shared.go parsed
      once across branches).
- [x] MCP-side wiring: `Server.kickGitPrewarm` walks the current
      branch's upstream chain (capped at 16 ancestors) and runs
      `PrewarmFromBranch` per branch in a goroutine after
      `handleInitialize` returns. Default on; `SetGitPrewarm(false)`
      to disable. `WaitForGitPrewarm(ctx)` for tests. Silent
      no-op when not in a git repo or git missing.
      `TestGitPrewarmFillsCacheForAncestorOnlyFiles` proves the
      Phase-3 win: ancestor-only files get parsed at startup, so
      a later branch-switch reuses them for free.

## Phase 4 — MCP

We shipped **Option A**: same binary, new subcommand. `poly-lsp-mcp mcp
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
  - `node_refactor(file, range, refactor:{rename?, params?, return?})`
    — multi-modal refactor channel with composable ops. The nested
    `refactor` object combines any of: identifier rename
    (workspace-wide, declared-bindings + aliasing-safety semantics),
    function-signature parameter list rewrite, and return-type
    rewrite (including insertion into previously-void / unannotated
    signatures). Supports **Go, TypeScript (.ts/.tsx/.js), and
    Python** today via `symbols.FindFunctionSignature(language, ...)`
    + `symbols.RewriteSignature` + per-language `langOps` (param
    syntax, return-type prefix, zero values for added args). When
    `params` changes the arity, callers across the workspace are
    rewritten best-effort — args truncated on shrink, padded with
    language-appropriate zero values on growth (`""`, `0`, `false`,
    `nil`/`null`/`None`, `[]`/`{}`, etc.). Spread/splat callers
    (Go `f(x...)`, TS `f(...xs)`, Python `f(*args, **kw)`) are
    reported as `skipped` so the agent decides. Legacy
    `kind="rename", newName=X` shape still accepted; internally
    normalized into the nested form.

  Workspace-relative paths in all tool output; absolute paths accepted
  on input. poly-lsp-mcp://workspace and poly-lsp-mcp://bindings resources are
  unchanged.

- [x] Live polyglot smoke against the 6-tool surface:
      `node_refactor(rename UserID, PersonID)` produces 9-file
      cross-language rewrite (proto + openapi + jsonschema + go + ts +
      py + yaml + sql + md); `node_references` returns 20 sites across
      8 languages; `structure(., depth=2)` walks the workspace tree.

- [x] Earlier (cut) tools and why:
      - find_symbol (substring) → cut; agents filter
        poly-lsp-mcp://bindings or pick exact names from structure.
      - find_references(name) → replaced by
        node_references(file, range) — point at a specific
        occurrence, no name-ambiguity.
      - document_symbols → cut; structure(file) covers it.
      - document_structure → renamed structure (now dispatches on
        directory vs file).
      - list_bindings (tool) → cut; poly-lsp-mcp://bindings resource
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
- [x] `resources/list` and `resources/read` MCP surface. Three
      resources today:
      `poly-lsp-mcp://workspace` — JSON `{root, languages, names, declared}`
      so clients can sanity-check what poly-lsp-mcp indexed without a tool
      call;
      `poly-lsp-mcp://bindings` — same JSON payload as the `list_bindings`
      tool, exposed as a resource for MCP clients that pin resources
      into model context;
      `poly-lsp-mcp://diagnostics` — workspace-wide diagnostic snapshot
      from every running child LSP, enriched with the same
      `text` / `context` / `enclosingNode` / `references` shape edit
      responses use. `diagnosticsAvailable: false` when no LSP is
      running for any indexed language, so consumers can't infer
      "clean" from absence. Same default caps as edits (25 / 15 / 3).
      Capability `resources: {}` advertised in initialize. More
      resources can be added by extending registerResources.
- [x] Proactive workspace open: after `manager.Start` succeeds,
      `kickProactiveOpen` walks the workspace asynchronously and
      sends `textDocument/didOpen` for every indexed file that
      routes to a running child LSP. This is what makes
      `poly-lsp-mcp://diagnostics` useful before any edit — gopls
      (and most LSPs) only publish after a file is opened or saved.
      Default on; `SetProactiveOpen(false)` opts out for huge
      workspaces. Tests use `WaitForProactiveOpen(ctx)` to observe
      completion; `TestProactiveOpenPopulatesDiagnosticsResource`
      proves the resource shows the broken-file error within ~1s of
      initialize, no edit required.

## Config setup: what's automatic, what isn't

Three tiers, increasing user effort:

| Tier | Setup | Coverage |
|------|-------|----------|
| 1 — lexical / tree-sitter | none | go / ts / tsx / py / md / yaml / json / sql out of the box; identifier-shaped tokens, with tree-sitter precision for code languages and lexical fallback for data formats. Unknown extensions surface as a single "text" node via `structure` so node_read / node_edit / node_delete still work. |
| 2 — declared bindings | `bindings:` section in poly-lsp-mcp.yaml | Hand-declared cross-language identity for things tree-sitter can't see (string-literal config values, prose, languages we have no grammar for) and for aliasing across naming conventions (UserID ↔ user_id). |
| 3 — schema-anchored | `schemas:` section in poly-lsp-mcp.yaml, one entry per schema file | Auto-derived bindings: parse the schema, bind every named entity (proto messages/enums/services/rpcs, openapi components.schemas.* + operationIds, jsonschema $defs.* + title), promote every workspace occurrence of those names to declared. One config line ≈ dozens of bindings. |

### Comments and prose

Tree-sitter intentionally skips comments — they aren't identifier nodes — so a default `node_refactor(kind=rename)` leaves comments untouched. This is the right safe default (a comment "we used to call this UserID" must not rewrite). For renames where the agent wants documentation references updated, pass `includeComments: true`:

- Workspace-wide regex scan with `\b<name>\b` word-boundary anchors
- Partial-word matches (`thisUserID`) are NOT touched — the anchor sees them as different tokens
- Positions already in the declared/lexical rename plan are deduped
- Per-file 1 MiB size cap mirrors the lexical pass
- Same atomic temp + Rename write path

Hand-curated comment renames (per-binding "always include comments") would be a follow-up (`bindings.<name>.rename_includes_comments: true`); not implemented today.

### Comment-marker scanner (universal)

A universal pass runs alongside the lexical extractor on every walked file. It recognizes four marker shapes, regardless of language:

| Marker | Origin | Confidence |
|--------|--------|------------|
| `@see <name>` | JSDoc / TSDoc / JavaDoc | comment (soft) |
| `{@link <name>}` | TSDoc / JavaDoc | comment (soft) |
| `@ref <name>` | Doxygen; our cross-language extension | declared (hard) |
| `x-ref: <value>` | YAML/JSON extension key (`x-poly-lsp-mcp-source`, `x-source` also accepted) | declared (hard) |

`@see` / `{@link}` are intentionally soft — they're how documentation refers to symbols, not declarations, so a default `node_refactor(rename)` skips them (same policy as plain comments). `@ref` / `x-ref` are hard — they're the convention generators use to point an emitted artifact at its source-of-truth in another language. Renames touch them by default.

Reference token shapes the scanner accepts:

| Form | Extracted name |
|------|----------------|
| `Foo` | `Foo` |
| `Class#method` | `method` (JSDoc/JavaDoc class-member shorthand) |
| `path/file.ts!Symbol` | `Symbol` (TypeDoc) |
| `server/main.go:Symbol` | `Symbol` (path:symbol — our preferred `@ref` form) |
| `server/main.go:42:18` | (skipped — positional, no usable name) |
| `https://…` | (skipped — URL) |

The new `ConfidenceComment` tier sits below lexical: at a deduped position, lexical and declared shadow the comment site. That's intentional — if a name appears as both a real reference and a comment marker at the same position, the real reference is the truthful entry.

The scanner ships now because generator-side adoption is the real lever: emitters of cross-language artifacts (gat → GraphQL/OpenAPI, codegen → typed clients) can teach themselves to drop `@ref` markers without poly-lsp-mcp needing per-framework parsing dialects. See `../gwag/docs/plan.md` for the gat-side commitment.

### Schema auto-detection

Schema auto-detection is opt-in via `auto_schemas: true` in poly-lsp-mcp.yaml. When set, `config.DetectSchemas(root, existing)` walks the workspace at startup and emits a Schema entry for each file matching one of these conservative heuristics:

- `*.proto` extension → `proto`
- YAML/JSON with a top-level `openapi:` or `swagger:` key → `openapi`
- YAML/JSON with a top-level `$schema:` key OR `*.schema.json` filename → `jsonschema`

Files explicitly declared in `schemas:` are skipped during detection (user wins). Detected schemas are appended to the user's list and processed identically by the resolver. Generic YAML/JSON without distinctive top-level keys is NOT classified, so values.yaml and config.json don't accidentally turn into bindings.

When poly-lsp-mcp.yaml is partial (e.g. only `schemas:` declared, no `languages:`), defaults are merged in — empty registry would invisibly break the lexical pass.

## Phase 5 — feedback loops for LLM edits

LLM agents editing through MCP currently get no signal about whether their edit compiles. They have to call `node_read` afterwards and infer, or wait for an out-of-band smoke test. That's a real gap.

### Diagnostics in edit/refactor responses

- [x] `internal/multiplex.DiagnosticStore` — last-write-wins map of URI → []Diagnostic with a generation counter so `WaitAfter(uri, since)` only wakes on publishes that arrived AFTER the captured point (avoids a race where a publish lands between our edit and our wait). `Attach(child)` wires `child.SetNotificationHandler` to forward `textDocument/publishDiagnostics` into the store. Manager auto-attaches every spawned child (initial Start and restart).
- [x] After `applyRangeRewrite` (the shared `node_edit` / `node_delete` write path) AND after each per-file rewrite in `node_refactor`, the server routes the URI through `manager.RouteByURI`, sends `didOpen` on first touch (with full file contents + languageId) then `didChange` + `didSave` on subsequent edits. Per-session `openDocs` map tracks (URI → version).
- [x] Bounded wait via `Server.SetDiagnosticWait(d)` (default `defaultDiagnosticWait = 1500ms`; tests bump to 8s for gopls). Per-URI `WaitAfter` blocks until a publish arrives or context fires. Response carries both `diagnostics` AND `diagnosticsTimedOut: true` when any URI hit the deadline without a fresh publish — distinguishes "no errors in window" from "no errors exist."
- [x] Tool response payload extended with `diagnosticsAvailable: bool`, `diagnosticsTimedOut: bool`, and `diagnostics: [{file, severity, code?, source?, message, startLine, startCol, endLine, endCol}]` (positions 1-based to match the rest of our wire shape). `node_refactor` rename emits a flat list keyed by file across every edited URI.
- [x] Languages with no child LSP (markdown, yaml, plain text, schema-only files): `manager.RouteByURI` returns nil, so the URI is skipped entirely. `diagnosticsAvailable: false`, empty `diagnostics`. Agents that infer "no errors" from absence of items are wrong by construction — the flag is the load-bearing signal.
- [x] Sibling-file rollup: gopls (and most LSPs) publish at the package level, so one edit can produce new diagnostics on sibling files. `collectDiagnostics` snapshots every URI's `Gen` counter pre-edit; after the per-URI waits return, any URI whose gen advanced past the baseline gets included. Edited URIs sort first (favored by the cap), then siblings. Opt out per call with `siblingDiagnostics: false` for a tighter response. `TestSiblingDiagnosticsRollup` exercises both branches against the cross-language fixture: dropping `HelloResponse.Greeting` from `types.go` brings back `main.go`'s `resp.Greeting` reference as a sibling diagnostic by default; opt-out keeps the response scoped to `types.go`.
- [x] `main.go runMCP` wires a fresh `multiplex.NewManager(reg)` into the MCP server. Tests without a manager exercise the index-only path and verify `diagnosticsAvailable: false`.
- [x] Live gopls e2e (`TestNodeEditSurfacesGoplsDiagnostics`, skipped under `-short`): writes a clean `main.go`, calls `node_edit` to insert `doesNotExist()`, and asserts the response contains at least one error-severity diagnostic — proves the full path: write → didOpen → gopls publish → WaitAfter → response. Passes in ~1.2s against gopls 0.21.

### Diagnostic enrichment (node-references-shaped payload)

Each diagnostic carries enough structure that the agent can act on it without re-querying:

- [x] `text`: source bytes between the diagnostic's start/end positions, capped at 256 chars with a mid-ellipsis on overflow. Saves a `node_read` round-trip.
- [x] `context`: configurable lines before/after the diagnostic range (`contextLines`, default 3), each entry `{line, text}` with trailing whitespace stripped. Helps the agent see the surrounding declaration without a second tool call.
- [x] `enclosingNode`: the top-level tree-sitter declaration containing the diagnostic position — `{type, name, startLine/Col, endLine/Col, nameStartLine/Col, nameEndLine/Col}`. Same shape as a `structure` node entry; pass the decl range to `node_edit` or the name range to `node_refactor(rename)`. New `symbols.EnclosingStructureNode(lang, content, line, col)` provides the lookup (root → named-children → containing range), with tests covering body-position lookup, identifier-position lookup, and unsupported languages.
- [x] `references`: when the diagnostic range is a single identifier token, `idx.Lookup(name)` runs and the hits ship in the response as `[]siteJSON` (the same shape `node_references` already returns). For multi-token ranges (statement-level errors) the references list is omitted — saves dragging in unrelated lexical noise.
- [x] Caps are configurable per call via tool args: `diagnosticLimit` (default 25), `referenceLimit` (default 15), `contextLines` (default 3). Tool args struct-embeds `diagnosticOptions` so the existing `rangeArgs` fields stay clean. Overflow surfaces as `droppedDiagnostics` on the parent payload and `truncated.references` per item — agents never have to guess whether they got a complete picture.
- [x] gopls e2e extended: asserts `text == "doesNotExist"`, `context` includes the broken line, and `enclosingNode.Name == "main"` on the same broken-edit fixture. Proves the enrichment path against real LSP output end-to-end.
- [x] Cross-language fixture (`testdata/fixtures/gat-greeter/server/`): hand-written Go stubs (`types.go` + `main.go`) shaped like `protoc-gen-go` output, each `@ref`-linked back to `greeter.proto`. `TestCrossLanguageDiagnosticOnGeneratedStub` synthesizes a flat Go module in a tempdir with the proto + Go files side by side, spawns gopls via MCP, edits `main.go` to reference an undefined field on `HelloResponse`, and asserts: (a) diagnostic comes back with `text == "Bogus"`, broken-line context, and `enclosingNode.Name == "Run"`; (b) `node_references` on `HelloResponse` from `types.go` returns BOTH the Go declaration AND the proto's `@ref`-anchored declared site, proving the comment scanner stitched the two languages together. ~1.2s.

### gat-greeter integration fixture

- [x] `testdata/fixtures/gat-greeter/` — separate Go module (so gwag's dep tree doesn't pollute poly-lsp-mcp): `greeter.proto` with `@ref` markers on rpc / message / enum-type / enum-value, and a `cmd/dump-sdl/main.go` that uses `gat.ProtoSource` + `gat.New` + `ir.PrintSchemaSDL` to emit the rendered GraphQL SDL.
- [x] `replace github.com/iodesystems/gwag => ../../../../gwag` in the fixture's go.mod so it uses the local checkout (the user's "latest gat") rather than a published version.
- [x] Integration test (`internal/symbols/gat_fixture_test.go` behind `testing.Short()` so quick iterations can skip): `go run ./cmd/dump-sdl` against the fixture, capture stdout into a temp dir alongside a copy of the proto, build `symbols.Index` over that dir, assert declared sites exist for each `@ref` target (rpc, message, enum, enum value). Proves the live gat → poly-lsp-mcp linkage end-to-end.
- [x] Registry change unblocked the scanner: `.proto` and `.graphql` / `.gql` are now in `config.Default().Languages`, both backed by `LexicalExtractor`. Without this the walker silently skipped these files, and the universal `@ref` scanner never ran on them — exactly the formats gat emits. (`go`/`ts`/`py` are tree-sitter; `proto`/`graphql` deliberately stay lexical so the comment scanner can still see embedded markers.)
- [x] Fixture extended to cover gat's full `@ref` carriage after gwag commit `09df07a` ("@ref source-of-truth marker carriage"): `cmd/dump -format graphql|openapi` emits both the rendered SDL (block-string descriptions) and the OpenAPI JSON (`x-ref` extension on operations + `@ref` text inside `info.description`). Test asserts declared sites in every emitted file for the expected names — Hello hits all three (proto + SDL + OpenAPI), service/type/value markers hit the formats gat actually writes them into.
- [x] `symbolFromRef` hardened against JSON-escape bleed: `(\S+)` could capture `Foo\n",` out of a JSON description string; the new leading-identifier walk drops everything past the first non-`[A-Za-z0-9_]` byte. Unit-tested with `Symbol\n`, `Symbol\t`, and the full `server/main.go:Symbol\n",` shape.
- [x] `testdata/fixtures/gat-greeter/server/{types.go,main.go}` — hand-written stubs shaped like `protoc-gen-go` output, `@ref`-linked back to `greeter.proto`. `TestCrossLanguageDiagnosticOnGeneratedStub` synthesizes a flat Go module from these files in a tempdir, spawns gopls via the MCP path, edits `main.go` to reference an undefined field on `HelloResponse`, and asserts the diagnostic comes back enriched (text, context, enclosingNode.Name=="Run") AND that `node_references` on `HelloResponse` from `types.go` returns both the Go declaration and the proto's `@ref`-anchored site. Closes the Phase-5 cross-language fixture loop.

## Phase 6 — tool ergonomics (shipped)

Surfaced from autowork3 integration. The 6 v0.2 tools cover the
**semantic** axis (identifier-resolution, cross-language references,
structured rename) but forced the agent into auxiliary builtin tools
(`read_file`, `grep`, `ls`, `write_file`) for everything else.
Goal: make `mcp_*` a complete editor surface so an LLM agent can
ship with just MCP + `shell` (the escape hatch for tests/git/
project-specific commands), no workspace-side
`read_file`/`grep`/`ls`/`write_file` shims.

### Gaps surfaced

**1. `structure` has no symbol/text filter.**
- Today: `structure(path?, depth?)` returns the full subtree at
  `path` (workspace, dir, or file).
- Want: `structure(path?, depth?, grep?)` where `grep` is a regex
  matched against each node's `name` (file basename for files,
  identifier for code symbols, dir name for directories) and only
  matching subtrees survive. Effectively the union of `ls` (when
  no grep) and `grep --include` (when grep is set).
- Agent flow this enables: instead of `shell grep -r FooBar`,
  call `structure(., depth=∞, grep="FooBar")`; results carry
  language-aware ranges so a follow-up `node_read` / `node_edit`
  has the right `(file, range)` without a second probe.
- Wins ls + the symbol-filter axis of grep in one call. Pure-text
  grep over comments/strings still needs `shell grep`.

**2. `node_read` rejects whole-file reads + line-offset reads.**
- Today: all four position fields required.
- Want a `node` selector form that's polymorphic:
  - `{file}` → whole file (the read_file replacement).
  - `{file, line, offset?, limit?}` → starting at `line`, optional
    `offset` skipped, `limit` lines returned. Replaces
    `read_file path | sed -n 'start,endp'` for previews.
  - `{file, startLine, startCol, endLine, endCol}` → existing
    range form, untouched.
- Decision: input schema becomes a oneOf or all-fields-optional
  with validation; pick whichever Huma's JSON-Schema generator
  produces cleanest agent-facing tool descriptions for.
- Same shape extends to `node_delete` (whole-file = delete file)
  but be careful: today `node_delete` is the deliberate-intent
  variant of `node_edit({newText:""})`. A "delete the whole file"
  primitive needs its own discussion (operator-grade destructive
  action, distinct from a range delete).

**3. `node_edit` has no "create file" mode and no diff-based form.**
- Today: requires `(file, range, newText)`; the file must exist
  AND the range must fall within it.
- Want:
  - `{file, newText}` with no range → if file doesn't exist,
    create it with `newText`; if it does, full-file rewrite.
    Replaces `write_file` for new files + wholesale rewrites.
  - `{file, diff}` → unified-diff form. Agent emits a patch
    against the current file content; we apply via the same
    temp + Rename path the range form uses. Saves multiple
    round-trips when the change touches several non-contiguous
    regions of the same file (today's agent has to call
    `node_edit` once per region; a single diff fits in one
    Turn).
- Open question: do we keep the `(range, newText)` form once
  `diff` is shipped? Probably yes — it's more compact for
  single-region edits and avoids the cost of computing a
  diff client-side. Three input shapes, one tool, polymorphic
  on which fields are set.

### Why this matters for autowork3

The autowork3 worker container currently ships these builtins
alongside the MCP tools: `shell, read_file, write_file, grep, ls`.
That's 5 extra tool descriptions per LLM call (real token cost on
every Turn) AND a parallel dispatch path the worker has to
maintain. Once Phase 6 lands, the worker's surface shrinks to
`shell` + MCP — same capability, fewer code paths.

`shell` still survives even after Phase 6 — it's the escape hatch
for everything that isn't read/edit/structure (running tests,
running git, anything project-specific). MCP shouldn't try to
subsume it.

### Implementation order — all shipped

1. [x] `structure` gains `grep` regex param. Matches each entry's
   `name` (file basename, directory name, code identifier);
   subtrees with no descendant match are pruned. Auto-bumps depth
   to 32 when `grep` is set, so the agent doesn't need to ask
   for a deep walk explicitly. Grep-mode expands files into their
   AST nodes so identifier matching works alongside filename
   matching. Tests cover identifier match / basename match /
   no-match-returns-empty / invalid-regex-is-error.
2. [x] `node_read` polymorphic input: `{file}` whole-file,
   `{file, line, offset?, limit?}` line preview (defaults
   offset=0, limit=50), existing `{file, startLine/Col,
   endLine/Col}` byte-precise range. Mixed shapes are
   error. Tests cover each form including past-EOF.
3. [x] `node_edit` polymorphic: existing range form,
   `{file, newText}` create-or-overwrite (auto-mkdir parent),
   `{file, diff}` unified-diff patch (strict context matching,
   in-order hunks, CRLF normalization). Tests cover each form
   plus context-mismatch and mixed-shape errors. `ApplyUnifiedDiff`
   lives in `mcp/diff.go` for reuse.
4. [x] `node_delete` polymorphic: existing range form,
   `{file}` whole-file delete (`os.Remove` + drop the file's
   slice from the index). Operator-grade destructive — surfaces
   errors clearly when the path is missing or is a directory.
5. [x] New `search` MCP tool — regex over file contents across the
   workspace. `symbols.Search(root, pattern, opts)` library helper
   does the walk (same skip-dirs + 1 MiB cap as Build, binary
   skip via null-byte probe, optional glob over basenames). Tool
   wraps it with `pattern` / `path?` / `glob?` / `limit?` (default
   100) / `contextLines?` (default 0). `structure(grep=…)` stays
   for symbol/file-name search; `search` is the full-text channel.
   Closes the last `shell grep` gap from the autowork3 worker.
6. [ ] autowork3 drops `read_file` / `write_file` / `grep` / `ls`
   from `cmd/worker/main.go`. Filed in autowork3's `plan/plan.md`
   (per-thread MCP follow-ups) as the consuming change.

## Phase 6.1 — natural-intent shapes (filed 2026-05-31)

Surfaced during an autowork3 end-to-end trial: the model reached for
`node_read({file, startLine, endLine})` — "read lines 35 through
37" — and the polymorphic validator rejected it:

  > range form requires all of startLine, startCol, endLine, endCol

That's a fair message for the implemented schema (range = four
fields, preview = `{line, limit}`, whole-file = `{file}`) but it's
not the model's natural prior. Most LLMs are trained on tool
surfaces where "give me lines A through B" is `{path, startLine,
endLine}` — exactly the shape they emit first.

The principle: **the tool should match the model's prior; we
shouldn't make the model burn tokens figuring out our special
syntax.** When a model reaches for it that way initially, that's
the most obvious way.

### Gap

Today's `node_read` accepts:
- `{file}` — whole file
- `{file, line, offset?, limit?}` — preview
- `{file, startLine, startCol, endLine, endCol}` — range (cols required)

Missing: `{file, startLine, endLine}` (no cols). Should be accepted
and treated as a line-pair range. Equivalent to `{file, line:
startLine, limit: endLine - startLine + 1}` semantically, but the
input shape matches the model's intuition without the renaming.
Same return shape (text + actual covered lines) is fine.

Same gap likely applies to `node_edit({file, startLine, endLine,
newText})` and `node_delete({file, startLine, endLine})` —
whole-line edits/deletes without the agent having to compute cols.

### Implementation sketch

Inside the polymorphic validator: if `startLine` AND `endLine` are
set AND `startCol` + `endCol` are unset, accept as a line-pair
range. Default `startCol = 1`, `endCol = len(line[endLine]) + 1`
(end-of-line exclusive). Wires to the same range-form code path as
today.

Detection should NOT require both cols to be unset to disambiguate
— `{startLine, endLine, startCol}` (one col, missing partner) is
still ambiguous and should still error, just with a clearer
message: "if any startCol/endCol is set, all four are required;
omit both to use line-pair form".

### Out of scope

- `{file, startLine}` without `endLine` — single-line read. Could
  alias to `{line: startLine, limit: 1}` but it's also reasonably
  asked for by preview-form callers; leave both forms alone for
  now.
- Mixed `{startLine, endCol}` (line + half-col) — ambiguous, error
  out. Don't try to be clever.

### Evidence

From an autowork3 trial (impl dev session
`8b9f3327-e94a-4602-8177-e34b79ad7608`, 2026-05-31): model called
`mcp_node_read` after applying an edit to verify the result. Got
the validator error; retried with the preview form and succeeded.
Single MCP error across 29 successful tool results in that
session, but the call shape suggests every model will hit it the
first time it tries to spot-check a multi-line edit.

(autowork3's tool span now captures `arguments` for any future
debug session, so the next occurrence will land the exact JSON in
the spans table — see autowork3 commit `<this branch>`.)


## Graph selector language — slice 1 (shipped 2026-07-17)

The designed successor to :has/:has_parent/:references/:depth (design history
below). Decisions locked with the user 2026-07-17:

- **Semantics are path sets, counter-documented against the jQuery prior.** At
  every point in a selector there is a set of paths; each segment builds new
  paths from the last tip. Combinators walk DOWN through containment (pure
  CSS, a DAG). `:parents(sel)` INVERTS: the tip becomes the nodes matching sel
  that REFERENCE the current tip — the graph enters there and only there, and
  re-rooting is legal at ANY point in the chain (the next segment continues
  from the new tip).
- **Name: `parents`** (user call; the jQuery false-friend risk is handled by
  documenting the path-set model, not by picking a stranger name).
- **Edge model: refs-only re-root** (icebox option 2). The move crosses only
  reference edges; containment never enters it, so `#'errf':parents(*)` is
  callers, period — no frontier explosion, no :where needed to pick the edge.
- **Hop ranges on the group:** `:parents(sel){m,n}`, `{1,}` = transitive
  fixpoint. Every hop must match sel ("through"): intermediates are named,
  hence constrained. Cycles are bounded by the visited set (subgraph
  stability); unbounded ranges collect each node at its shortest hop.
- **`:where`/`:any`/`:all`/`:empty(sel)`** — filter / ∃ / ∀ / ∄ over the paths
  of sel from this node. sel is RELATIVE (CSS-:has-style): descendants by
  default, leading `>` = children, and a leading pseudo-only compound (e.g.
  `:parents(...)`) binds to the node ITSELF — that's how
  `func:any(:parents(#'main'))` = "what main calls" works.
- **∀ = relaxed-domain comparison:** evaluate the inner once as written and
  once with the subject's constraints dropped (recursing into the last move's
  inner); ∀ holds iff the sets are equal. Reachability sets, never path
  enumeration — cycles cost nothing, no bound needed. ∀ over an empty domain
  is vacuously true.
- **:where ≡ :any at tip granularity today** (documented at pseudoHolds); kept
  as two spellings so path-level filtering can diverge without a grammar
  change.
- **Dissolutions:** `:has(S)` → `:any(S)`; `:has_parent(S)` → write the
  ancestor first (`#'a.ts' func`); `:references(S)` → inverted into
  `S:parents(X)`. All three had ZERO measured uses; each now returns a terse
  guided error naming its replacement. `:depth` stays until group ranges land.

Implementation (mcp/query.go): forward set-based evaluator (evalList /
evalComplex / collectMatches / applyPseudos / moveParents / referrersOf)
replaced the right-to-left boolean matcher, which could not express a
mid-chain re-root. Pre-order output via (fileOrd, symOrd) keys instead of a
full-tree walk; `file`/`dir`-only compounds no longer parse file symbols at
all (cheaper than the old subjectMaxDepth pruning). Referrers ride the
existing lexical index + decl-site exclusion (recursion kept, declarations
dropped — LSP includeDeclaration semantics preserved). Inner-selector match
sets memoized per query AST.

Verified: graph_selector_test.go (single hop, transitive closure with a real
X↔Y cycle, exact hop windows {2,2}, mid-chain re-root continuing into
containment, filter form, dead-code ∄, ∀ incl. the vacuous case, guided
errors, hop-range parse errors) + reworked modern_test.go/references_decl
coverage. Live smoke on this repo:
`#'mcp/query.go#selectorGrammarHelp':parents(func){1,}` answers in one call
what a frontier model once hand-rolled as 7 chained :references calls (and
the func hop-filter demonstrably excludes method-enclosed chains);
`func:empty(:parents(*))` returns the uncalled set. Tool description + grammar
rewritten to the shipped language only; token budget 1000 → 1080 (recipes are
the one form measured to move usage).

### Design history (from icebox, written 2026-07-16 pre-implementation)

- CSS is a DAG; code needs a graph. Every selector stays containment except
  the one move; that confinement is what keeps a CSS prior CORRECT rather than
  merely familiar. Conservative extension: every CSS construct recovered at
  set-size 1 (compound = ∀ at size 1 fell out unengineered).
- The false-friend test for reusing a name: does the known meaning survive as
  a special case? `.cache` failed it (open vs closed vocabulary → model
  invented `.cache` 12×/run); `:empty` passes it.
- Filter vs validate is structural: a filter narrows the set and FLOWS ON; a
  validation collapses to bool and decides the tip.
- Representation: reachability subgraph, not node-set (edges lost) nor
  sequence-set (exponential, cycles diverge). Trade written down: a subgraph
  unions paths, so ∀ says "every node between a and b matches" and CANNOT say
  "some path exists whose every hop matches".
- Evidence (2026-07-16): :has/:has_parent/:references/:depth got 0 uses across
  5 measured runs; recipes keyed to an address moved :references 0 → opening
  move; forced onto poly-lsp a model found 12/12 render paths vs bash's 10/12
  that "looked complete"; selector error rate 42% → 17% came entirely from
  output-side fixes.

## Graph selector language — slice 2: the postfix algebra (shipped 2026-07-17)

Same-day user-driven redesign of slice 1's operator surface. Decisions:

- **Two directions, two honest names.** `:parents` = incoming (who points at
  the tip), `:references` = outgoing (what the tip points at). Both bare
  (= unfiltered) or `(sel)`, both `{m,n}` hops. "What main calls" is now one
  move (`#'main':references(func)`), not a nested quantifier.
- **Postfix pipeline on a compound.** A move opens an excursion (tips walk
  the reference graph); parenthesized pseudos filter the current tips; a
  BARE `:any/:all/:empty` CLOSES the excursion — it decides by the reached
  set and collapses back to the subject. `func:parents:empty` = dead code,
  the easy rewrite of the canonical `func:where(&:parents:empty)`. A bare
  claim with no open move is a parse error naming the fix. Bare `:all`
  compares the written walk against the unfiltered walk (∀, per excursion).
- **The start of a relative inner is ASSUMED to be `&`** — the CSS nesting
  rule, so the CSS prior is correct: a leading pseudo attaches to the node
  under test (`:where(:parents:empty)`), a leading tag/*/#id means a
  descendant, `>` a child; `:root` re-anchors globally (the one exception);
  explicit `&` is always allowed. Slice 1's implicit scope-binding special
  case dissolved into this uniform rule.
- **`{m,n}` is THE range syntax** — move hops and compound depth share it.
  `b{1,3}` = within 1..3 levels of the previous target, `{0}` = that target
  itself (the self-reference trick: `method:where(:references(*) #'X'{0})` =
  methods that mention X). Space ≡ `{1,}` and `>` ≡ `{1}` stay as CSS sugar;
  `:depth(m,n)` stays as an accepted alias. The icebox's through-typed
  containment ranges (`a func{1,3} b`) were DROPPED: no use case in a
  containment tree, and moves already do through-filtering where it matters.
- **Description is recipes-first** (user call: a few complex-but-common
  traversals beat language-spec prose for quantized models); the spec lives
  behind selector "?". 1059/1080 tokens.
- **Outgoing edge** (`referencesOf`): built whole on first use — decl nodes
  per name × non-decl sites of that name → src → targets. Name-keyed and
  decl-excluded like the incoming edge; a node references its own arguments
  and nested symbols when its body mentions them (filter with `(sel)`).

Verified: graph_selector_test.go — outgoing single/filtered/transitive, bare
claims incl. the `:any` complement, canonical `:where(&…)` ≡ bare-postfix
equivalence, bare `:all` ≡ parenthesized `:all` agreement, `&`/claim parse
rules, `{m,n}` ≡ `:depth` byte-equal results, `{0}` self trick. Live-smoked
on this repo.

## Graph selector language — slice 3: references are NODES (shipped 2026-07-17)

The final shape, converged with the user (and bonsai) same-day. Supersedes the
slice-1/2 operator surface entirely; we are the only consumers, so no shims.

- **A reference is a pseudo-element NODE, named by direction**: `::in` (who
  points here) / `::out` (what this node's body points at); KIND as class
  (.call/.type/.import — closed, tree-sitter-classified per language; bare
  matches any kind incl. unclassified — a Go func pointer is `::out`, not
  `::out.call`). Each edge appears twice (out under the source, in under the
  target); id = the far end's ids; span/address = the SITE ("file@line"), so
  node_read/node_edit touch the call site. refConf = "lexical" (name-keyed
  index); per-edge "lsp" upgrade via child-LSP definition is the planned
  precision pass — works for TS (tsserver) and Go (gopls) alike; the node
  model itself needs no LSP.
- **The pseudo-element contract does the safety work**: `*` never matches an
  edge, walks never enter one — containment queries can't leak into the graph;
  crossing is explicit (`::out.call > #'B'`, the far end is the edge's child).
  Attachment is CSS-exact: `X::out` = X's own edges, `X ::out` = nested
  symbols' too (#a::before vs #a ::before) — ref nodes hang under the
  INNERMOST enclosing symbol, so `>` vs space IS the attribution axis (no
  :first/:last gymnastics needed for it).
- **{m,n} is repetition** (regex prior, icebox-original): child-joined chains
  (func{2} = func > func), groups repeat as units ((a b){2}), zero reps make
  the element vanish (its combinator vanishes with it), {m,} is a cycle-safe
  fixpoint. On an edge element it counts EDGES crossed ((::out > *){k} > ::out
  expansion — always ends AT an edge). :depth retired (guided error).
- **:parents(sel) is the one inverse**: roots of sel with a path down/out to
  the tip — containment ancestors ∪ incoming-reference sources, transitive.
  `*:parents:empty` = exactly the workspace root (proven in test).
- **Bare claims are position claims**: :any/:all/:empty judge the arrival set
  at their chain position (inside :where/…; terminal) or close a :parents
  excursion; :all is the relaxed-domain compare, direction stays structural
  under relaxation. Top-level bare claims are guided errors.
- **Language classes**: file.go / func.ts (closed registry vocabulary +
  aliases). :first/:last = per-anchor document-order selection (jQuery at the
  root anchor). :with(project.go) scoping → icebox.
- Description rewritten recipes-first for quantized models (user call after
  bonsai trials); budget 1080 → 1160. Guided errors for every retired
  spelling (:references, :depth, ::ref, :has/:has_parent, top-level claims).

Verified: graph_selector_test.go rewritten against a polyglot go+ts fixture —
kind classification (call vs type vs import vs unclassified var-ref), `*`
exclusion + gate opacity, direct/transitive/exact-window crossing over a real
cycle, dead-code/leaf/∀ position claims, upstream :parents incl. multi-element
roots, language classes, :first/:last per anchor, repetition/groups/zero-rep,
site addressing (node_read of an edge shows the call), 13 guided errors. Live
on this repo: #'nodeLess'::in.call returns exactly its two call sites with
from: carrying the callers.

## Import edges — external dependencies enter the graph (shipped 2026-07-17)

Found by the acceptance exercise "find all endpoints of a generic huma app":
external symbols (huma.Register) have no decl in the workspace, so no edge
existed. Fix, three parts:

- **Import nodes are named for the PACKAGE**: alias honored (`import h "…"`
  → #h), Go /vN major-version segments skipped ("…/huma/v2" → #huma) —
  importBase in symbols/filesymbols.go. The import node is thereby the decl
  that qualified references (`huma.Register`) resolve to.
- **Import edges are FILE-scoped** (language semantics: an import only names
  things in its own file): an import node is only ever the far end of its own
  file's sites, both directions — `import#huma::in.call` is per-file
  dependency usage, not name-keyed noise across files.
- **The endpoint sweep is one call**: selector `import#huma::in.call` + grep
  `-E (Register|Get|Post|…)\(` → every registration site with route/opID text
  and an editable file@line address. Handlers stay graph-native:
  `func:where(::in:contains('-E (huma|h)\.'))`. Both proven in
  TestImportEdgesFindHumaEndpoints (two files, alias + vN, scoping asserted)
  and live against a huma fixture app.

## :not/:is, colon auto-repair, ~= guidance (shipped 2026-07-17)

Driven by bonsai's hand-written dead-func detector
(func:not(has(out.call)):not(#main):not([name~=Test])):

- **:not(sel) / :is(sel)** — SELF-anchored, exactly CSS: the inner tests the
  node itself (func:not(#main) = not named main), never a descendant. This
  splits the inner conventions on purpose: :not/:is are the element-test
  family (CSS :not), :where/:any/:all/:empty are the relative family (CSS
  :has) — each name keeps its own prior. Single-compound inners test in
  place; chained inners fall back to memoized global-set membership
  (func:not(#web *)). :is fills mid-chain compound union (file > :is(func,
  method)). :not(:any(::out.call)) ≡ :where(::out.call:empty), proven.
- **Colon auto-repair** (normalizeSelector, pre-parse; the `.func` lesson
  generalized): outside quotes/brackets and never after #/., `not(`/`any(`/…
  gain their ':'; `in`/`out`/`grep`/`ref` gain their '::' (zero or one colon
  repaired to two); `::any(` drops to one; and `has(` maps straight to
  `:any(` — :has IS :any, so it now just works instead of lecturing.
  Attachment semantics survive repair (X in.call → X ::in.call stays the
  descendant form; the tight form is the host's own edges).
- **[name~=X]** — guided error, not an alias: CSS ~= is word-list matching
  and names aren't word lists; silently mapping to *= would be quietly
  wrong, the worst kind. Error names ^=/*=/$=.
- **The dead-code recipe is now the honest one**:
  func:not(#main):not(#init):not([name^=Test]):where(::in:empty) — ∄
  callers minus the roots the runtime calls for you. bonsai's exclusion
  instinct was the insight; its direction (out vs in) was flipped — kept the
  recipe prominent for exactly that reason.

## Query work budget (shipped 2026-07-17)

"Isn't {1,} dangerous?" — no, and the analysis is the record: unbounded
traversals are termination-safe (visited sets = subgraph stability, O(V+E)),
and a hop cap guards the WRONG axis — breadth (hot-name fan-out on the
lexical index) is the cost, not depth, and capping hops reintroduces the
measured worst failure (silently-incomplete results that look complete).
The real guard is a query-wide WORK budget: every node visited, edge
crossed, and site/line scanned spends one unit (default 200k,
SetQueryWorkBudget to override). Tripping is LOUD and non-fatal: partial
results with truncated:true and a note naming the repair (kind class,
filtered inner, bounded hops, tighter scope) — the same never-cut-silently
contract pagination keeps.
