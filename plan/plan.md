# poly-lsp-mcp — roadmap

> How this plan works: this file = current state + active work + decisions
> ONLY. `plan/done.md` = archive of completed work. `plan/icebox.md` =
> deferred, opt-in next-steps. Status marks: ◻ todo · ◐ in progress · ✅ done ·
> ⏸ parked · ❓ blocked. Move rules: a finished tree → done.md (leave a
> one-line pointer); a deferred next-step → icebox.md — in the same pass as
> the work.

Fused multi-language LSP + MCP server, one binary. The LSP side multiplexes
child LSPs (gopls/tsserver/pylsp) over a tree-sitter symbol index that crosses
languages (lexical / declared / schema-anchored tiers). `poly-lsp-mcp mcp --root
<dir>` boots the MCP surface on the same index.

## North Star — what "world-class" means for us

The owned quadrant, empty of incumbents: a **live, no-build, multi-language,
reference-aware, edit-capable code querier with a predictable, explainable cost
model** — driven by an LLM agent, live, mid-task. Every priority is judged
against holding THIS position, not against beating CodeQL at its own game.

- **The consumer is a MODEL, not a security engineer** — that inverts the
  weights. A grammar the model already knows (CSS). Loud, honest partiality
  (`conf` labels, budget-blow-that-says-so, `:explain`, `>x` floors) as
  HALLUCINATION-PREVENTION, not polish — a human supplies the skepticism a
  model doesn't. Token-lean, composable output over rich reports.
- **The unoccupied intersection:** {live + no-build} × {cross-file reference
  edges with a precision LADDER} × {mutation} × {predictable cost}.
  CodeQL/Glean/Kythe/Sourcegraph buy the reference graph with a
  build/batch/per-language extractor and cannot rewrite; ast-grep/Comby/Semgrep
  rewrite live but have NO cross-file graph. Nobody sits in the middle.
- **The moat is the precision LADDER:** lexical → tree-sitter-scoped →
  LSP-resolved, each LABELED. "Resolved or lexical, and it says which" is a
  category property no interactive querier ships.

Non-goals — forfeiting these is what KEEPS the quadrant:
- **Dataflow / taint / points-to** (CodeQL's turf) needs the build+batch we
  refuse. Explicit non-goal, not a gap.
- **We do not out-scale Sourcegraph/Glean on multi-repo.** A single owned root
  is home.

**The known chasm — ownership domains.** The precision pass does not ASK to
leave the workspace; it crosses by definition — gopls resolves into the stdlib,
tsserver into `node_modules`, pylsp into `site-packages`. The moment it works,
a `refFar` can land OUTSIDE the git root, where `fileByRel` has no node and the
edge silently falls back to a false local match (the `Write`/`Read` collision
the icebox flags). So the single-`.project` assumption is already leaking, and
"lib linking" is not a feature to add but a boundary already being crossed.
Bridge is staged — **Stage 0 SHIPPED**: a resolved far end outside the root is
now an honest EXTERNAL STUB (`module@version#sym`, `domain: external`,
read-only, `[not indexed]`) — nameable, never a false local (see the Edges
slice below). Stages 1–2 (content-addressed lib partitions, on-demand,
evictable) are deferred design in icebox ("Ownership domains"). **Rule until
then: nothing new may hard-code the single-root assumption deeper** — a node's
`domain` (owned rw / vendored ro / external ro) is the axis that will gate
mutation and budget; the field now EXISTS on treeNode (`"" `=owned,
`"external"`), populated by Stage 0.
**Decided: crossing into libs is opt-IN** (`:with(libs)` / drill into a stub),
the default stays workspace-bounded — revisit only if adoption data shows
agents actually want the cross-lib answer unprompted.

## Current state

- Phases 0–6.1 (scaffold, multiplex, cross-language index + rename,
  stacked-branch parse cache, MCP server, diagnostics-in-edit-responses,
  tool ergonomics) — ✅ all in `done.md`.
- MCP default surface: **3 tools** — node_query / node_read / node_edit —
  over one unified node tree (project > dir > file > symbols > argument),
  addressed as `<file>#<sym>`, queried by CSS selector. Legacy 9-tool surface
  behind `--legacy-tools`. Sandbox jail + read-only mode: commit `0fbeb02`.
- Selector language: CSS containment + the graph as NODES. References are
  reified edge pseudo-elements — `::in`/`::out` on TWO orthogonal class axes,
  KIND (.call/.type/.import) × POSITION (.return/.param/.field/.var), composed
  CSS-style (`::in.return.type`); far end (via `>`) is the SOURCE symbol, the
  ref IS the occurrence (address = site file@line), invisible to `*`. `{m,n}` =
  regex repetition; edge hops on an edge element. `:parents(sel)` = the one
  inverse. Bare `:any/:all/:empty` = position claims. Language classes
  (file.go), `:first`/`:last` per anchor. Attribute axes: `[name]` (called) vs
  `[path]` (lives), ops `= ^= $= *=` literal, `~=` regex (bracket-aware).
  Shipped 2026-07-17 → done.md ("Graph selector language" + the per-feature
  entries) for the full record.
- Trivia/metadata as NODES (this session, → done.md): `annotation` (decorators
  py/ts + struct-tag keys go, a CHILD of its symbol, leaf + virtual-FQN alias),
  `::comment` (joined doc block, a GENERATED pseudo-element invisible to `*`),
  `argument` (params). `:annotated('pat')`/`:contains('pat')` are the text
  fallbacks (Go comment directives have no AST node).

## Active work

**Shipped this session (all → done.md):** query CLI + `bin/dev`; deterministic
truncation; `[path]` axis + de-leaked `[name]`; `~=` regex (bracket-aware);
edge-cost fixes (direction split + once-per-query inversion → `func::out` fits
the 200k default); child-LSP precision pass (`conf: lsp|lexical`); local-scope
fix (99% far-end noise gone); leading-ref pushdown + containment attribution;
`:annotated`; `annotation` node; `::comment` pseudo-element; `::in.return.type`
position axis. Common dev queries are NOT pathological at the default budget.

Open frontier:

◐ **Java + Android XML — the JVM/Android blind spot (started 2026-07-26).**
Motivating measurement, termux-app: **17 of 291 source files were visible**
(yml/md/json only) — 197 `.java` and 67 `.xml` were invisible, i.e. the index
saw 6% of the repo and none of the code. Any agent using us as its code tool on
an Android repo degrades to grep.

**Slice 1 SHIPPED — Java via tree-sitter.** `config.Default()` entry
(treesitter-only; jdtls is opt-in via yaml, it is too heavy to default),
`javaIdentifierQuery`, `LanguageByName`, `classifyJava`, `refinedClass` java
arm (module/import/class/interface/enum/struct(record)/ctor/method/field/var/
const), `returnTypeNodes` (`type` field), `javaParamInfos` (formal_parameter,
spread_parameter, receiver_parameter). Verified live on termux-app: 8,099 names
indexed, `class#X > method` and nested field paths
(`TermuxPreferenceConstants.TERMUX_APP.KEY_SCROLL_BEHAVIOUR`) resolve. Tests:
`TestFileSymbolsJavaClassMembers`, `TestFileSymbolsJavaArguments`.
Package declarations answer to the LEAF segment (`view`, not
`com.termux.view`) — deliberate, matches Go's `package_clause`.

**Slice 2 SHIPPED — the Android binding (`internal/bindings/android.go`).**
`ApplyAndroid` sits alongside `ApplyDerived` / `ApplyDerivedSQL`, wired into
both `mcp/server.go` and `server/lsp.go`. The structural problem it solves:
**the Java side of an Android binding is a STRING LITERAL, which the
tree-sitter extractor drops by design** — right for Go, wrong for Android.
Widening the Java query to capture all literals would flood the index, so this
takes the same want-set gate ApplyDerived uses:

1. Collect resource names from XML (`name="x"`, `@+id/x`, `@string/x`, and the
   read-by-code attributes `app:key` / `defaultValue` / `entryValues` /
   `fragment` / `android:name`).
2. Collect Java string literals via tree-sitter, keeping only values the XML
   side already declares.
3. Declare sites **only for names present on BOTH sides** — a resource nothing
   in Java addresses carries no cross-language information and the lexical tier
   already has it.

**Precision is tiered**, and it had to be: binding every XML name first gave
46,595 sites of mostly noise. Read-by-code attributes bind unconditionally;
generic `name="x"` / `@res/x` additionally require the name to look chosen
(`_`, `.`, an uppercase letter, or ≥12 chars), which drops coincidental
collisions on `color` / `key` / `layout` / `content`. Result on termux-app:
**42 bound names**, including the motivating case
(`scroll_behaviour`: 2 XML + 3 Java sites), every AndroidManifest
`android:name` ↔ activity/service class, the extra-keys names
(ALT/CTRL/SHIFT), and preference keys (`log_level`, `current_session`,
`crash_report_notifications_enabled`).

Tests: `internal/bindings/android_test.go` — cross-language pair, unpaired
resource NOT bound, unpaired Java literal NOT bound, noise filter.

- **risks**: only direct literals are read (no constant folding, so
  `KEY = PREFIX + "x"` is invisible); the tier-2 distinctiveness heuristic is
  lexical, so a genuinely short lowercase resource name paired with a real Java
  literal is dropped as noise.
- **optional extensions**: kotlin + groovy grammars are already vendored in
  smacker/go-tree-sitter (`.kt`, `.gradle`); jdtls as an opt-in child LSP for
  resolved edges and safe rename; `R.id.x` is currently bound via the `@+id/`
  declaration side only, since R.java is generated and not in the tree.

◐ **Daemon mode — ONE shared poly-lsp per user, many clients (agreed 2026-07-23).**
Steps 1-5 SHIPPED (skeleton + proxy + persisted shared cache + FileSymbols op
+ per-session mutation isolation + ref-counted child-LSP pooling; see
"SHIPPED" + steps below). Step 6 (worktree COW) MEASURED and iceboxed — its
goal is already met by the shared ParseCache; a true index COW isn't worth the
core refactor (see below + icebox). Per-connection policy (read-only/validate)
SHIPPED. **The planned daemon arc (steps 1-5 + per-connection policy) is
COMPLETE**; the only daemon non-goal left is per-connection LEGACY surface (the
daemon serves modern only) and the LSP-mode proxy (optional extension).
Today every client runs `poly-lsp-mcp mcp --root <dir>` as its own process, and
each one owns: a symbol index built by walking the workspace, a `ParseCache`
persisted to `<root>/.poly-lsp-mcp/cache.gob`, an fsnotify watcher over the
tree, git prewarm, and a `multiplex.Manager` spawning CHILD LSPs (main.go:84-100,
mcp/server.go). Every Claude session, every editor, and every future
non-interactive consumer pays for all five. gopls alone is hundreds of MB and
tens of seconds of warmup, and N copies fight over the same module cache. This
is raglit's "single writer + single worker pool" argument plus a child-LSP
fleet, which is the expensive part.

**Second consumer, and why it forces the design:** raglit wants `FileSymbols`
for structural fragmentation (symbol path + class + doc-comment span +
`BodyStartLine` = fragment atoms with titles, no LLM). raglit builds
CGO_ENABLED=0 today; `smacker/go-tree-sitter` needs cgo, so importing `symbols`
would forfeit its pure-Go build, and shelling out costs a fork per file. A
daemon removes both problems — raglit becomes a client hitting a warm,
content-keyed parse cache over the generated OpenAPI client: no cgo, no fork
per file. A non-MCP, non-editor consumer is a first-class caller, not an
afterthought.

**Shape (deliberately mirrors raglit; copy, don't invent):**
- **Transport: gat/huma, served on a UNIX SOCKET** (DECIDED, USER,
  2026-07-23). The house stack — huma handlers through gat for REST + GraphQL +
  gRPC + OpenAPI, exactly as raglit's `buildGatHandler` — bound to
  `$XDG_RUNTIME_DIR/poly-lsp/daemon.sock` (fallback `~/.poly-lsp/daemon.sock`),
  dir 0700 / socket 0600, instead of a TCP port. **Protocol and listener are
  orthogonal**: gat yields an `http.Handler`, and an `http.Handler` serves on
  any `net.Listener`, so the trust decision below costs nothing in stack
  consistency. (An earlier draft proposed JSON-RPC-over-socket to make the MCP
  proxy a pipe swap; that saves only the tool-call→HTTP translation
  `raglit/cmd/raglit/serveclient.go` already shows how to write, and is not
  worth diverging from every other service we run.)
  - `http.Server{ConnContext: …}` stashes the accepted conn's peer creds
    (uid/pid) in the request context — the mechanism that makes per-CONNECTION
    policy (read-only / validate) enforceable at the boundary.
  - Clients dial `http.Transport{DialContext: unix}` and use the generated
    OpenAPI client; `curl --unix-socket` debugs it.
  - Accepted consequences: no cross-host clients and no browser pointing at it
    (we have no review UI; an opt-in TCP listener can be added later without
    touching the trust model).
  - ⚠ **Server→client PUSH does not fit request/response.** LSP
    `publishDiagnostics`/progress need SSE, a websocket, or LSP-mode proxying
    stays out of scope. MCP tool calls are all request/response, so steps 1-3
    are unaffected — decide this when the LSP proxy is actually built.
- **Registry keyed by ABSOLUTE workspace root**, not by a name. raglit
  namespaces because "default" collides across projects; our roots are already
  unique paths. Closest analogue: raglit's `OpenScopedRegistry`.
- **Client = thin proxy.** `poly-lsp-mcp mcp` keeps its stdio JSON-RPC surface
  and forwards tool calls to the daemon — exactly what `raglit serve` does
  today. LSP mode can proxy the same way later; editors keep speaking stdio.
- **Lifecycle copied from raglit** `cmd/raglit/{runtime,daemon,httpd}.go`:
  a state file at `~/.poly-lsp/daemon.json` (pid / socket path / started_at /
  version — NOTE: "root" there means raglit's storage root, ours is per-DAEMON,
  not per-workspace) for discovery, auto-start detached (Setsid, output to
  `daemon.log`), `--stop`, and `--restart` (SIGTERM → wait for the pid to
  actually exit → relaunch detached replaying the invocation's flags). A
  successful CONNECT plus a ping is the authority on "is it up" (raglit uses an
  HTTP health probe for the same purpose); a stale socket file whose owner is
  gone gets unlinked and replaced, the way raglit drops a stale `daemon.json`.
- **ParseCache becomes daemon-wide.** It is ALREADY content-keyed
  (`symbols/cache.go`: `Language + Hash[32]byte → []Hit`, LRU, version-tagged
  gob) — it just lives per-root. Promote it to one shared store and identical
  file content across five worktrees parses once. This is raglit's pool move
  (`(recipe_hash, file_hash)`), and the cache comment already anticipates "a
  long-running agent walking many branches".
- **Branches = git worktrees, COW over the parent index.** A worktree shares
  ~95% of its content with the parent checkout, and git names the difference for
  free (`diff --name-only`). Overlay the parent's warm index and re-parse only
  the divergent files, so a fresh worktree starts warm instead of rebuilding.
  raglit's branch overlay (branch-over-parent at document grain, tombstones for
  deletes) is the model. **UPDATE (MEASURED 2026-07-25): the shared ParseCache
  already delivers this — re-parse-only-divergent falls out of content-keying;
  the extra in-memory-clone win is ~90ms on a rare, gopls-bound open. Iceboxed;
  see step 6 + icebox.**
- **Child LSPs pool by ROOT, not by content.** gopls is bound to a module root,
  so it can't be shared across worktrees — but it CAN be shared across every
  session on the same root: keyed by root, ref-counted, idle-evicted LRU (same
  policy shape as raglit's `GCPolicy`, different key). This is the single
  biggest resource win.

**SHIPPED this session (steps 1, 3-proxy, 2-share; all in `daemon/`):** the
daemon runs — `poly-lsp-mcp daemon --allow <dir>` hosts many roots over a unix
socket; `poly-lsp-mcp mcp --root X --daemon` is the thin stdio proxy that
auto-starts it and forwards tools/list + tools/call. Verified end-to-end
against the real binary (health, open=6965 names, /call, 403 on out-of-allow +
sibling-prefix bypass, stop/restart replaying flags, stale-socket reclaim).
Files: `paths/state/trust/peercred_{linux,other}/registry/server/spawn/client/proxy.go`;
mcp gained `Init`/`Shutdown`/`CallTool`/`Tools`/`IndexedNames`/`SetParseCache`/
`Root`/`RollbackSession` (`mcp/daemon_api.go`, `mcp/session.go`) — the exported
seams the daemon drives a Server through without the stdio handshake. Tests:
`daemon/{trust,daemon}_test.go` (bypass matrix, symlink escape, e2e round-trip,
stale/live socket, disconnect auto-rollback), `mcp/session_test.go` (per-session
batch isolation, claim conflict, external-write conflict).

**next** (each step independently useful; 1-3 deliver the raglit consumer):
  1. ✅ Daemon skeleton: gat/huma handler on a unix listener, peer-cred check
     (`SO_PEERCRED` on Linux via a cred-filtering `net.Listener` + `ConnContext`
     stash; mode-bit-only fallback on `!linux`) + root-prefix gate (`AllowList`,
     EvalSymlinks/Clean + component-wise `filepath.Rel`, default `$HOME`),
     registry keyed by canonical abs root (lazy, per-root serialized build),
     health, `daemon.json`, auto-start detached (Setsid → `daemon.log`),
     `--stop`/`--restart` (replays flags via `stripBoolFlags`). Straight port of
     raglit's lifecycle; the socket/creds/gate part was new, as predicted.
  2. ✅ Daemon-wide content-keyed ParseCache. One `symbols.NewParseCache()` on
     the `Registry`, injected into every hosted Server via `SetParseCache`, so
     identical bytes across roots parse once (safe: content-keyed, own mutex).
     Persistence is daemon-OWNED — `Registry.LoadCache` at `Serve` start,
     `SaveCache` (temp+rename) on shutdown, one load/save at
     `~/.poly-lsp/cache.gob`, not the N racing per-root gobs the stdio path
     does. Verified e2e: 172 entries saved on stop, reloaded on restart. Test:
     `TestRegistryCachePersistence`.
  3. ✅ Client-proxy (`daemon/proxy.go`: stdio MCP ⇄ socket, warms the root on
     initialize so the trust-gate 403 surfaces early; shutdown is local, the
     daemon keeps the root warm) + the typed read-only **`/filesymbols`** op
     (`POST` content → structural atoms; language derived from `path` ext or
     given). Content-first, NO root/file access + NO trust gate (the caller
     owns the bytes; peer-cred + 0600 bound callers) — the "no cgo, no
     fork-per-file" path the plan wants for raglit. Clean json-tagged
     `FileSymbol` DTO (decoupled from `symbols.Symbol`) carries sym/class/decl+
     name ranges/doc-comment span/body-start — fragment atoms with titles.
     `Client.FileSymbols` + `TestDaemonEndToEnd` cover it; e2e-verified.
     **Deferred (measure-first):** a content-keyed cache of `[]Symbol` results
     (the existing `ParseCache` stores `[]Hit`, a different shape) — parse is
     one tree-sitter pass/call; cache only if raglit re-ingest shows it hot.
  4. ✅ Per-session mutation isolation. **Session identity = a client-minted
     id (X-Poly-Session header) + a watched connection** (USER, 2026-07-24):
     the proxy mints a random id, sends it on every `/call`, and holds one
     long-lived `GET /session/watch` open; when it drops the daemon
     auto-rolls-back that session's batch (`Registry.RollbackSession` sweeps
     hosted roots). mcp side (`mcp/session.go`): the single `editBatch` field
     became `batches map[sessionID]*editBatch` + a `claims map[uri]sessionID`,
     all under `editMu` (the per-root commit serializer). The session reaches
     the shared write funnels via an `editMu`-guarded `activeSession` field
     (the node_edit handler sets it) rather than threading `sess` through every
     apply signature; only the tool-handler dispatch type + `CallTool` gained
     the param. **Per-file claim** (as predicted, not per-root lease): a second
     session staging a held file is rejected naming the holder. **Stage-time
     hash + commit-time recheck** (`editBatch.staged` + `externalWrites`): a
     staged file written underneath the batch (another session/editor/
     formatter/git) aborts the commit with a `conflict` + `changedFiles`,
     leaving the batch OPEN — never silently overwritten or reverted. Tests:
     `mcp/session_test.go` (isolation, claim conflict, external-write conflict),
     `daemon TestDaemonRollsBackBatchOnDisconnect` (e2e auto-rollback).
     **Scope note (batch isolation only, USER 2026-07-24):** per-CONNECTION
     policy (read-only/validate) was a follow-up — now SHIPPED (see the
     per-connection policy entry below). **Derived, still deferred:** the workspace-wide `--validate`
     cross-session false-reject REPORT ("these errors are in files you didn't
     touch; session X has an open batch") — a reporting nicety, not a
     correctness gap; commit hashing + claims already prevent the corruption.
  5. ✅ Child-LSP pooling: ref-count + idle eviction per root. Child LSPs were
     ALREADY shared per-root across sessions (one *Server per root); the gap
     was EVICTION — a root, once opened, pinned its gopls (hundreds of MB)
     forever. Now `entry` carries `holders` (sessions keeping it warm) +
     `evictTimer`: `Registry.Acquire(session,root)` holds + cancels any pending
     evict, `Release(session)` (called on the SAME /session/watch disconnect
     that rolls back the batch) drops the ref and, at zero holders, arms an
     idle timer; `evict` shuts the server down IFF still unheld (re-checked
     under mu, so a reconnect races safely). `idleTimeout` default 5m, 0
     disables. The daemon-owned shared parse cache survives eviction, so a
     re-open comes up warm on parses. Wired: `/open` Acquires (session header
     on `openIn`), `/call`+`/tools` use the non-holding `Get`. Tests:
     `TestRegistryRefCountEviction` (two holders keep it, last release evicts),
     `TestRegistryReacquireCancelsEviction`; `-race` clean.
  6. ⏸ Worktree overlay (COW index over a parent root) — MEASURED, iceboxed
     (2026-07-25). The goal (re-parse only divergent files on worktree open) is
     already met by the shared content-keyed ParseCache (step 2). Measured
     (`daemon/measure_test.go`, `-tags measure`, this repo): warm worktree index
     build with the shared cache is **93ms** (cold is 552ms — the cache already
     saves 84% for free), while gopls workspace warmth is **355ms** and
     UNSHAREABLE (per module root). A true in-memory COW would shave ~90ms off a
     RARE, gopls-bound operation at the cost of refactoring the hottest struct
     (`symbols.Index` is absolute-path, name-keyed, no clone). Not worth it now;
     re-measure on a large repo before revisiting — see icebox
     "Worktree COW index overlay" for the gate.

**risks**
- **Mutation is the hard part, and raglit never had to solve it.**
  `node_edit`/`node_refactor`/`--validate` write the user's real source and
  revert on new diagnostics; the staged-edit batch is "one open batch per
  server, `editMu`-serialized". Concurrent agents already race today with stale
  indexes — the daemon is the fix only if it owns apply→diagnose→revert
  serially per root. **DECIDED (USER, 2026-07-23): one open batch per CLIENT
  SESSION, and a commit whose underlying file changed underneath it must SAY
  SO.** Consequences, in order of how much they change the code:
  - **Staging is ON DISK** (a deliberate earlier choice — reuses
    atomicWrite/revert, fires no PostToolUse hooks, and the file-watch wants to
    see it), so per-session isolation is BOOKKEEPING, not filesystem
    isolation. Two sessions with disjoint file sets are genuinely independent;
    two sessions staging the SAME file are not, and a naive revert would
    restore session A's original over session B's staged edit — silent data
    loss. So a staged file is CLAIMED by its session: a second session staging
    it gets `rejected` + help naming the holder. A per-FILE claim, not the
    per-root lease we considered — far less restrictive and it falls out of the
    originals map the batch already keeps. (Derived, not user-stated — flag on
    review.)
  - **Stage-time hash, commit-time recheck.** The batch already records each
    touched file's pre-edit bytes for revert; hash them at stage time and
    re-hash at commit. If the on-disk bytes are neither the original nor what
    we staged, something else wrote the file — another session, the user's
    editor, a formatter, `git checkout`. Report the conflict; never silently
    overwrite it and never silently revert it away. Content hashing is already
    idiomatic here (`ParseCache` is content-keyed; raglit hashes documents the
    same way).
  - **Commits serialize per root** (short exclusive hold across
    apply→diagnose→verify; commits are brief). Even so, `--validate`'s
    fingerprint is WORKSPACE-WIDE, so a concurrent session's staged broken
    intermediate can make another session's commit look like it introduced
    errors. A false reject, not a corruption — but the report must be able to
    say "these errors are in files you didn't touch, and session X has an open
    batch" instead of blaming the committer. (Derived — flag on review.)
  - **A dropped connection must not strand a batch.** Today the batch dies with
    the process because server and client are the same lifetime; in a daemon a
    client can vanish mid-batch and leave a BROKEN INTERMEDIATE on disk. This
    is a failure mode the daemon INTRODUCES: auto-rollback on disconnect is the
    safe default. (Derived — flag on review.)
  - The file watcher sees staged edits and refreshes the index, which other
    sessions' queries then observe. That is arguably correct — the working tree
    really did change — but it is a behavior change worth naming.
- **Trust boundary — DECIDED (USER, 2026-07-23): peer creds + declared root
  prefixes.** A daemon that opens any root a client names lets any client read
  and EDIT any file on the machine (raglit answered the equivalent with
  namespaces — "a project can't reach an arbitrary project's indexes by
  guessing"). Two gates, both cheap:
  - **Peer credentials on accept** — `SO_PEERCRED` (Linux) / `getpeereid`
    (macOS); reject any uid but the daemon's own. Redundant with 0600 socket
    mode in the normal case, which is the point: it still holds if the mode
    bits are ever wrong.
  - **Declared root prefixes** — a client may only address roots UNDER a
    configured prefix (`--allow <dir>`, repeatable, plus a config list;
    default `$HOME`). The check is the part that goes wrong in practice:
    compare `filepath.EvalSymlinks`'d, `filepath.Clean`'d ABSOLUTE paths, and
    match on PATH COMPONENTS — `/home/u/local` must not admit
    `/home/u/localsecrets`, and `..` must not walk out. Test the bypasses, not
    just the happy path.
  - ◻ **Optional strengthening (Linux-only, later):** peer creds carry the
    client PID, so `/proc/<pid>/cwd` can bind a connection to its ACTUAL
    working directory — kernel-verified instead of self-declared. No portable
    macOS equivalent, so it can only ever be a bonus tier, never the base.
- ✅ **Per-client policy — SHIPPED (read-only + validate).** These were
  process-global flags; in a shared daemon they are per-CONNECTION attributes,
  now enforced at the daemon boundary and TIGHTEN-ONLY (a client may add
  read-only/validate on top of the daemon baseline, never remove it — a
  read-only daemon stays locked for everyone). The proxy sends
  `X-Poly-Read-Only`/`X-Poly-Validate` (from its `--read-only`/`--validate`
  flags); `CallTool(sess, name, args, CallOptions)` rejects mutating tools
  (`mcp.IsMutatingTool`, one source of truth with `applyReadOnly`) for a
  read-only call and injects `validate:true` into edit args for a validate
  call; `/tools` returns a filtered catalog per connection. The shared *Server*
  stays read-write for other clients — enforcement is at the boundary, not on
  the Server. `--legacy-tools` has NO daemon path (the daemon serves the modern
  surface only; the proxy logs that it's ignored) — the one piece deliberately
  left out. Tests: `mcp` (`TestCallToolReadOnlyPolicy`, `TestWithValidateInjects`),
  `daemon TestDaemonReadOnlyIsPerConnection` (per-connection catalog + reject).
- **Generation-keyed caches** (`defCache` is valid for one index
  `Generation()`) must be per-root and invalidate for all clients of that root
  at once — a stale definition surviving another client's edit is the failure.
- One watcher per root instead of N is strictly better; no risk, just the win.

**blocking decisions**
- ✅ **Transport** — gat/huma on a unix-socket listener (USER, 2026-07-23).
  See Shape. Stack consistency and peer creds are not in tension.
- ✅ **Trust model** — peer creds + declared root prefixes (USER, 2026-07-23).
  See risks.
- ✅ **Concurrent staged batches on one root** — one open batch per CLIENT
  SESSION, with commit-time change detection (USER, 2026-07-23). See the
  mutation risk above for the four consequences; three of them are derived
  rather than user-stated and want a review pass.

No open blocking decisions. Steps 1-2 are unblocked and independent of the
mutation design.

**optional extensions** (explicitly out of scope now): LSP-mode proxy for
editors; a `poly-lsp-mcp status` CLI over the daemon; extracting the daemon
scaffolding into a shared `iodesystems/daemonkit` — see the decision below.

**Decided (USER, 2026-07-23): COPY raglit's daemon scaffolding, do not extract
a shared module yet.** raglit's version is battle-tested but has exactly one
user; extracting now is speculative, extracting after a second real copy is
refactoring against two known cases. Revisit once this daemon runs.

✅ **`--validate` (revert-on-new-diagnostics) + the safe-edit-loop thesis,
shipped, tested, and MEASURED.** The whole arc — reframe → build → benchmark →
tune → measure with error bars.

**Thesis (why):** LLMs run a grep→read→edit loop and reach for grep by habit.
Don't fight it — ABSORB it: keep the loop, make edits *safe*. node_edit is the
edit; `--validate` makes it un-break-able.

**Built (poly-lsp side, all in `mcp/validate.go`):**
- Write paths (range/whole-file/diff) run through `applyBytes`: fingerprint the
  workspace's pre-edit errors, write, re-collect, and if the edit introduces a
  NEW error, atomically restore + report `rejected` (isError=true; `newErrors`).
- Multi-file rename/signature via `validationTxn` — records every touched file's
  pre-edit bytes before writing, reverts them ALL as one unit on any new error
  (nested rename inside signature shares the outer txn: all-or-nothing).
- **CROSS-FILE**: the fingerprint is WORKSPACE-WIDE (`errorFingerprintAll` over
  the store snapshot), so an edit that breaks an IMPORTER (rename a type its
  callers use) is caught — gopls publishes package-level, `settleErrorFingerprint`
  waits for the sibling republish to land before the diff. This was the binding
  constraint the benchmark exposed.
- Server flag `--validate` (or per-call `validate:true`); no-op-but-flagged
  without a child LSP (`validated:false`).
- **Sharpened `node_edit` rename description** (modern.go, shipped default):
  leads with "renaming? use the rename op — one atomic call, don't hand-edit".
- Tests (gopls-backed, stable): `TestNodeEditValidateReverts`,
  `TestRefactorRenameValidateRevertsAllFiles`, `TestNodeEditValidateCrossFileRevert`;
  full mcp suite green.

**Measured (corrallm llm-bench, Qwen3-6-27B-MPT via llm.iodesystems.com, n=5):**
on a cross-file rename (`edit-safety-rename`, type used across 4 files):
| arm | rename-op | broken-intermediates | pass | tokens |
|---|---|---|---|---|
| baseline (shell/read/edit) | n/a | **2 [2–2]** | 5/5 | 9k |
| polylsp / polylsp-validate | **5/5** | **0 [0–0]** | 5/5 | ~30k |
**Net benefit, with error bars:** poly-lsp reliably completes the refactor with
ZERO broken intermediate states (baseline lands 2 every time) — structural
safety grep+sed can't match — at ~3× the token cost. **The lever was
PRESENTATION**: the sharpened description gets Qwen onto the atomic rename op
10/10 runs; that op is safe by construction, so `broken=0` even WITHOUT
validation. Validation is the untested insurance for when presentation doesn't
land (weaker model / harder refactor / hand-editing).

**Lessons the runs taught (each cost a wrong turn until measured):** (1) on
tasks a capable model passes, pass/fail is BLIND to the offering's value — the
`broken_intermediates` safety metric is what separates the arms. (2) `--validate`
is redundant for a diligent model WITH a build tool (it self-heals); its value
needs the no-self-check path (`--run-tool=false`) or a task the model breaks.
(3) single runs LIE — the validate arm "hand-edited (64k tokens)" was n=1 noise;
at n=5 it's 5/5 atomic rename. Always `--runs`.

**Remaining (opt-in):** `validate:"strict"` (refuse, not fail-open, when no LSP);
pre-touch baseline for never-analyzed files (an unanalyzed file with prior
errors could false-revert its first edit — documented limitation); the ~3×
token premium is the thing to shave if this goes wide.

✅ **Staged-edit transaction (`commit:false`) — SHIPPED.** `--validate` reverts
any edit that breaks the build, which BLOCKS a refactor that must pass through
a broken intermediate (change a signature AND its body/callers). The
transaction lets those commit atomically: `commit:false` stages an edit
(applied but NOT validated); the FIRST `node_edit` WITHOUT `commit:false` (or
`commit:true` with no op — the noop) validates the whole batch's UNION and
persists, or reverts ALL; `rollback:true` discards to the last committed state.
**INSTRUCTIVE by design** — the three flags are HIDDEN from the schema; a
plain edit that's valid alone but breaks the build gets `rejected` + a `help`
string that teaches the multi-stage path exactly when it's needed.
Implementation: `editBatch` (validate.go) is a long-lived `validationTxn` —
on-disk staging (each staged edit is written so later edits/resolves see it;
the intermediate reverts on rollback/fail-commit; chosen over an in-memory
buffer because it reuses the existing atomicWrite/revert machinery, node_edit
fires no PostToolUse hooks, and the file-watch WANTS to see it). `applyBytes`
routes through the open batch; the handler is the state machine; semantic ops
(rename/params/return) refuse staging (already coherent). One open batch per
server, `editMu`-serialized. Tests: `TestEditBatchStageAndRollback` (mechanics,
no-gopls), `TestEditBatchAtomicCommit` (gopls: sig+caller as one clean unit;
lone breaking edit → rejected+help).
  - ✅ **Semantic refactors (`params`/`return`/`rename`) can now JOIN a batch.**
    `validationTxn` is batch-aware: when a `commit:false` batch is open,
    `beginValidationTxn` returns a txn whose `record` feeds the batch's
    originals and whose `verify` DEFERS to the batch commit — so a refactor's
    multi-file edits stage into the same all-or-nothing unit as raw text edits.
    (A nested rename inside a signature refactor already skips its own verify —
    `ownTxn` — so it can't commit the batch early.) Now
    `node_edit(#F, return:'string', commit:false)` + `node_edit(#F::body,
    newText, commit:false)` + commit is one atomic sig+body change. Count is
    handler-managed (one per staged node_edit). Test:
    `TestEditBatchStagesSemanticRefactor` (gopls).

✅ **Editable `::signature` / `::body` — SHIPPED (on the txn).** `::body` is
now the STATEMENTS between the braces (a rewrite replaces just the impl,
leaving the sig line and `}` intact); it and `::signature` carry a RANGE
address (`file@start-end`) so node_read/node_edit hit the whole span, and a
generated-span address treats `newText` ALONE as replace-the-span (no need to
repeat the old text): `node_edit #'F'::body newText:'…'`. Both the selector
(`#'F'::body`) and the emitted address resolve to the span (the selector path
now routes generated nodes — ref/comment/signature/body — through their
address instead of collapsing to the whole file; an `external` stub is
refused as read-only). `::body` edits alone (safe); `::signature` edits inside
a `commit:false` batch with its counterpart (the broken-intermediate the txn
exists for). Tests: `TestGenPartBodyEditable` (selector + address, statements
only). Degenerate single-line/empty bodies yield no `::body` node.

**⚑ corrallm-side changes (their repo, uncommitted — flag for review):**
`services/corrallm` gained, to make the above measurable: the
`broken_intermediates` metric + task `safetyCheck` field (run after each mutating
call); `--run-tool` gate + toolset `baseArgs` (argument the base llm-bench-mcp,
generalizing `cedeFileTools`); `--runs N` + per-run artifact naming (`_rN`); the
`edit-safety-{pop,import,rename}` probes + `polylsp-validate`/`polylsp-norun*`
toolsets.

◐ **Adoption measurement — the existential question, now instrumented.**
`llm-bench/` (own nested module, uses `agentkit` — `mcpmgr` spawns the server,
`agent.Session` drives the model on llm.iodesystems.com) poses relationship-
shaped code-nav tasks with BOTH the graph tools AND a strong grep/read/list
baseline present, and tallies **reach-rate** — the fraction of tasks that call
`node_query` at all. Compiles + the server binary spawns (verified); NOT yet
run (needs `AGENTKIT_API_KEY` + network). **next**:
  - ◻ **Run the `asis` baseline.** Reach-rate high → adoption isn't the wall,
    engine work is justified. Low → the icebox's "0 calls / 8 runs" reproduces
    and everything below is premature.
  - ✅ **First A/B run (asis vs pattern vs inspired, 3 direct-question tasks,
    n=1).** Reach-rate SATURATED — 3/3 for all three variants; even spec-first
    `asis` gets node_query reached for (callers answered correctly). **Finding:
    on direct relational questions the docs are NOT the wall** — which means the
    icebox "0 calls / 8 runs" was the IN-FLOW case (model mid-task, grep
    momentum, unprompted), not the wording. The direct-question set can't
    measure the thing that matters; I measured the easy case. Only flicker:
    `pattern` reached graph grep-free most often (weak, n=1).
  - ✅ **In-flow grep-tempting set built + run (5 tasks incl. a collision
    canary; asis/pattern/inspired, n=1).** Metrics: reach, graph-FIRST,
    grep-free. Result across ALL three variants: reach **4/4**, graph-first
    **4/4** even in-flow — the model reaches for node_query FIRST, unprompted,
    on grep-tempting tasks, regardless of doc wording. grep-free was the only
    mover (asis/pattern 3/4, **inspired 2/4** — its longer prose induced MORE
    grep scaffolding; the verbose "inspirations" strategy underperforms tight
    recipes and even the plain spec).
  - ❗ **The bench structurally CANNOT answer the absolute adoption question.**
    In-harness, grep is just another advertised tool with a one-line desc — it
    has NO home-field advantage. The icebox's "0 calls" was grep as the model's
    NATIVE, reflexively-trained tool inside a real agent. This bench is NEUTRAL
    ground, where node_query competes as a peer and wins on description alone.
    So: node_query is *competitive as a peer* (real result), but "does it beat
    the model's native grep/read in a real agent" needs a REAL agent — and the
    one we had wired (autowork3) is a DEAD PROJECT (dropped 2026-07-21, user
    call). The native-vs-newcomer asymmetry is now UNMEASURED with no vehicle;
    llm-bench's standing value is A/B descriptions as peers, correctness, cost.
  - ◻ **Collision canary** (`collision*`): grep AND lexical node_query both
    return the merged set (verified) — flips to a graph win only when the LSP
    precision pass resolves the site. Re-run with precision ON to show the
    graph's real differentiator.
**Do NOT rewrite `modern.go`'s description** — across quick + in-flow, two
sets × three variants, the shipped spec-first desc already saturates reach and
graph-first. The wording is not the bottleneck on neutral ground; if `inspired`
taught anything it's that MORE prose is worse, not better.
**blocking decision (USER owns)**: adoption can no longer be measured on
home turf (autowork3 dead). Either find/build another real-agent vehicle, or
accept the llm-bench peer result and let the roadmap ride on it.

✅ **Broader dogfood pass — 3 cliffs found + fixed, all invisible to tests.**
Drove the real MCP server across ~20 queries. Edit/transaction UX + instructive
help confirmed excellent. Three issues surfaced, all fixed:
  1. `:recursive` broad exhausted the cap (see Edges) — 1 match → 10, complete.
  2. **`:any`/`:empty` over a bare edge over-resolved → the FLAGSHIP dead-code
     recipe (in the description) hit the cap and returned non-deterministic,
     incomplete results.** Fixed with a SHORT-CIRCUIT: `bareEdge()` detects a
     plain `::in`/`::out` existence test and routes it to `anyEdge`/`anyInEdge`/
     `anyOutEdge`, which stop at the FIRST real edge instead of building+resolving
     the whole set (out-edges need no LSP; in-edges resolve only ambiguous names,
     until the first real caller). A live func stops at its first caller; only a
     truly-uncalled one pays the full scan. Recipe now 2.0s, complete (3 real
     dead non-test funcs; the other 482 "dead" were `Test*` funcs — no direct
     caller by design). `bareEdge` deliberately REJECTS a bare-claim shape
     (`::in:empty`, a positionClaim) — that path stays full/correct. Recipe in
     description/README/grammar switched to the fast `:empty(::in)` form. Tests
     `TestPositionClaims`/`TestNotAndIsAreSelfTests` pinned it (caught a
     complement-inversion mid-fix).
  3. **Addressing friction: `#'file#method'` silently matched nothing** (the sym
     is `Type.method`). `nodeIDs` now also answers to `file#leaf` for nested
     symbols, so the natural `#'precision.go#resolveDefinition'` matches the
     method — a model rarely knows the receiver type.

✅ **Dogfooding wired: `.mcp.json` registers poly-lsp as a NATIVE tool** for
Claude Code in this repo (`./bin/dev mcp --root . --validate` — build chatter
is stderr, stdout is clean JSON-RPC, verified). So the project develops
itself with its own `node_query`/`node_read`/`node_edit` — the closest thing
to the missing real-agent vehicle, and a standing dogfood. **First dogfood
session already paid off**: driving the real MCP server surfaced the
`:recursive` cost cliff (see Edges) that 100% test coverage missed — the
edit/transaction UX and instructive help were confirmed excellent in real
use, `:recursive` broad was noisy+incomplete, now fixed. "Complete" = the
natural queries a model reaches for are cheap, quiet, and complete; tests
prove correctness, dogfooding proves usability, and they diverge exactly at
cost cliffs like this one.
  - ✅ **Binary files polluted the projection (DOGFOOD, 2026-07-21).** The very
    first `:root > *` tour surfaced `mcp.test` (56916 "lines") and
    `poly-lsp-mcp` (45616) — gitignored ELF binaries — as file nodes.
    `node_read mcp.test` returned **12.9M chars** of garbage and the hint
    *invited* reading all 56916 lines (context-nuking footgun); `countFileLines`
    slurped the whole 13 MB binary on every query build. `walkDir` (query.go)
    made a file node for every regular file with NO binary/ignore filter — the
    other two projection walkers (node_query.go) already skip non-source via
    `languageForFile==""`. Fixed: `looksBinaryFile` (prefix-only null-byte
    probe, same heuristic as `symbols.Search`/search.go — never slurps the
    binary whole) gates `walkDir`. Tour 22→20, all text files retained. Chose
    binary-only skip over honoring `.gitignore` (USER call) — narrow,
    unambiguous, keeps gitignored-but-text files queryable. Tests:
    `TestWalkDirSkipsBinaryFiles`, `TestLooksBinaryFile`. Invisible to the
    existing suite (tests use clean temp fixtures; a real repo has build
    artifacts) — the exact tests-vs-dogfooding divergence noted above.
    **Decided (USER, 2026-07-21): do NOT honor `.gitignore`** — hiding files
    loses context and it's hard to tell what's missing; only binary encodings
    and extremely-large files are no-gos, and only for the OPS, not visibility.
  - ✅ **`node_read` unbounded on a pathological long line (DOGFOOD follow-on,
    2026-07-21).** The binary skip fixed the PROJECTION, but a direct
    `node_read file.min.js` still dumped megabytes: `buildReadPayload`'s auto
    char budget (2048) is defeated because the FIRST line is appended BEFORE
    the budget check, and with no caller `lineLength` there was NO per-line cap
    — a 5 MB line-1 returned all 5 MB. Worse, the truncation hint then advised
    "re-read with lineLimit=N for the whole file in ONE call", walking the model
    straight into the dump. Fixed with `renderLine`, TWO regimes: an explicit
    `lineLength` clips every line (legacy); with NONE, normal lines return
    WHOLE and only a GENERATED line (> `readGeneratedLineLen` = 5000, mirrors
    search.go's `maxSearchLineBytes`) is previewed to `readLongLinePreview` =
    500. **Why not a flat 500 cap (USER pushed): a mid-content clip of real
    prose fed to an LLM is strange, and 500 is wrong by DATA** — this repo has
    13 lines > 500, ZERO > 800, longest legit 742 (plan/done.md); a 5000
    threshold clips zero real lines, 500 clips 13 (incl. markdown paragraphs).
    The cap must sit in the 3-orders-of-magnitude GAP between legit (hundreds)
    and pathological (millions). The blob preview is honestly labeled
    ("a generated/minified line (N chars) was previewed to 500; pass lineLength
    or search(pattern=) to target"), not a silent box. VISIBLE (`truncated` +
    true `maxLineLength`); ops bounded. Verified e2e on the real server: 5 MB
    blob → 515 chars + generated hint; real 742-char line → returned WHOLE, no
    ellipsis. Tests: `TestReadPayloadBoundsPathologicalLongLine`,
    `TestReadPayloadLegitLongLineNotClipped`, `TestReadPayloadNormalFileUnaffected`.
    (Whole-file BROWSE path only; an addressed-symbol read stays byte-exact.)
  - ✅ **Search/`::grep` context is byte-budgeted, not line-counted (DOGFOOD
    design, USER-driven, 2026-07-22).** Same bytes-not-lines theme: search
    context was N whole neighbour LINES (`-A/-B/-C`), so match + 2×N context
    could be multiple KB/hit — token-heavy across many hits, though already
    bounded (each line ≤500B, generated files skipped). Reframed to grep-style
    **min(bytes, lines)**: `symbols.BudgetHitContext` trims already-capped,
    already-sliced context so the WHOLE hit ≤ `maxHitTotalBytes` = 500B
    (~125 tokens/hit), match rendered FIRST (CapHitLine) and context filling
    the remainder nearest-first + contiguous. Shared helper in symbols, called
    from `searchFile` + `fragmentsOf` (one source of truth). Tests:
    `TestBudgetHitContext`, `TestBudgetHitContextStopsSideOnOverflow`.
  - ✅ **Default context is now OFF; a hit is the matched line + a per-file
    rollup (DOGFOOD, USER-driven, 2026-07-22).** Reverses the "default ON"
    above after dogfooding the funnel (grep wide → refine → read the *symbol*).
    The realization: the matched LINE is grep's signal and is paginated (≤20
    rows), so it's cheap; CONTEXT (before/after) multiplies every hit 3–7× and
    is the real token sink. So flipped: **context opt-in** (`-A/-B/-C` on
    `::grep`, `contextLines` on the search tool; both default 0), matched line
    always shown, still byte-bounded when context IS requested. Removed the
    now-dead `DefaultContextLines`/`grepSpec.ctxSet`. **Added `rollup`**: a
    per-file match count over the WHOLE result set (pre-pagination), so a wide
    search shows WHERE a term concentrates without paying for any line body —
    `fragmentRollup(rows)` in modern.go, only present for grep. USER chose
    "line + rollup, context opt-in" over pure address-only (address-only forces
    a read to tell hits apart; the paginated line disambiguates for free).
    Token-budget guardrail caught the new ::grep doc line 1 token over —
    tightened, not bumped. Test: `TestModernQueryGrepDefaultNoContextHasRollup`.
    Deferred to icebox: `-c` counts-only mode (rollup already gives the
    distribution; -c only saves the 20-row page's line text — marginal).
  - ✅ **LLM e2e bench (`scripts/smoke/llm_e2e.py`) drove out two rename
    friction bugs + a harness drift (DOGFOOD, 2026-07-22).** Fired the bench
    (real Qwen-27B, cross-language `UserID`→`PersonID` rename on a temp fixture
    copy). PASSED but took **11 tool calls**: the model did the atomic rename
    (`filesChanged:9`) then DIDN'T TRUST it — re-ran per-file (errors) and
    hand-patched comments. Fixes: **(A)** `node_edit` rename on a non-symbol
    node (whole file, or a ref/::grep/span — all `sym==""`) used to silently
    rename whatever token sat on the span's first line and report
    `filesChanged:0`; now ERRORS pointing at the `file#Name` form
    (`TestModernNodeEditRenameRejectsNonSymbol`). **(B)** the rename RESULT now
    carries a terminal `note` ("DONE — workspace-wide … in this ONE call; do
    NOT rename per-file"); models act on results, not descriptions
    (`TestModernNodeEditRename` asserts it). **(C)** the harness `SYSTEM_PROMPT`
    had drifted to the legacy surface (`structure`/`node_refactor`); rewrote to
    the modern 3-tool flow. Re-ran: **4 tool calls**, 0 hand-patches, model's
    own words "the first rename already handled everything workspace-wide."
    11→4. NOTE (USER): the only baseline worth running is vs vanilla
    grep/read/edit-style tools, not the legacy surface.
  - ✅ **Vanilla A/B baseline arm (`scripts/smoke/vanilla_e2e.py`).** Same
    model/fixture/task as `llm_e2e.py`, but the model gets ONLY structure-blind
    tools — `grep` / `read_file` / `str_replace` (unique-match), implemented
    locally over the temp fixture copy, no MCP. Measures the tool-call cost of
    the same cross-language rename WITHOUT the structural surface, to quantify
    the delta vs poly-lsp's 4 calls. Self-contained (KISS>DRY — does not touch
    the poly arm). Same PASS criteria (changed≥8, PersonID≥15).
  - ✅ **Task-registry A/B harness (`scripts/smoke/ab_bench.py`) + first real-repo
    task, which EXPOSED a correctness bug (DOGFOOD, 2026-07-23).** Generalized
    the one-off demos into a registry (id → instruction, optional setup patch,
    static verify predicate, fixture); reuses the MCP + vanilla plumbing by
    import (no refactor of the working scripts). Fixture = a fresh copy of a
    sibling repo (redline: 529 .go files, 0.4s copy, no `replace` dirs so
    `go vet` runs in-copy); verify is deterministic shell (go vet + grep counts),
    validated on both PASS and FAIL paths before any LLM spend. First task
    `islive-rename` (rename `payments.Gateway.IsLive` while two unrelated `llm`
    interfaces share the name). Result: **poly 56 calls / PASS, vanilla 22 / FAIL**
    — but the story is the bug: poly's lexical rename corrupted the `llm` package
    (`filesChanged:15`), the model brute-forced a 20-edit manual repair. See the
    escalated icebox item "Go refs/rename lexical → CORRECTNESS bug." Harness is
    the reusable repro; more tasks (mined from redline) drop into the registry.
    Mined catalog of 10 candidate tasks lives in this session's history (6
    statically verifiable; controls where grep wins included for honesty).
  - ✅ **Rename collision guardrail SHIPPED (stopgap for the lexical-rename bug,
    2026-07-23).** `lexicalRenameCollision` (tools.go) blocks a rename when the
    name is DECLARED in >1 package with no authoritative site coupling them —
    the coincidental-clash signature — returning `kind:"rename-blocked"` + the
    collision list instead of applying. The authority check (`Confidence >=
    ConfidenceDeclared`) is the discriminator that keeps schema-coupled cross-
    language renames (polyglot `UserID`, which has declared bindings) working
    while blocking pure-lexical clashes (`IsLive` across payments/llm). Only
    runs on the lexical path, only parses touched files. Tests:
    `TestModernNodeEditRenameBlocksCrossPackageCollision` (+ Free→Freed and the
    schema path still pass). Bench-validated: the `filesChanged:15` corruption is
    now blocked at call 1; model went straight to safe scoped edits, llm never
    touched. **NEXT (owns the real fix): gopls `textDocument/rename`** — type-
    scoped correctness, removes the guard's false-positives on legit cross-
    package interface renames and restores one-call ergonomics. See icebox
    "Go refs/rename lexical → CORRECTNESS bug."
  - ✅ **gopls type-scoped rename SHIPPED — the real fix for the lexical-rename
    bug (2026-07-23).** `refactorRename` now tries the child LSP first
    (`mcp/rename_gopls.go`: `goplsRenameEdits` routes the file via
    `manager.RouteByURI`, calls `textDocument/rename`, converts the WorkspaceEdit
    to byte-ranged resolvedEdits, and reuses the existing apply/txn/diagnostics
    pipeline unchanged). Correct on collisions: renames only the addressed
    symbol's decl/impls/usages by declaring TYPE, so `payments.Gateway.IsLive`
    no longer touches `llm.Rewriter.IsLive`. Stamps `resolvedBy:"lsp"`; the DONE
    note now states the scope (type-scoped vs name-matched). Fallback: only a
    tree-sitter-only file (no child LSP) takes the lexical path + collision
    guard; a server that refuses returns `rename-error` (never silently degrades
    to lexical). Tests: `TestRefactorRenameTypeScopedViaGopls` (gopls-guarded),
    reconciled the revert test (gopls front-stops conflicts). Caveat: LSP
    `character` is UTF-16; `lineColToByteOffset` treats it as bytes — correct for
    ASCII identifiers, a known edge for non-ASCII (noted, not yet handled).
    **Bench-validated on the real bug**: `ab_bench islive-rename` now does the
    rename in ONE correct call (`filesChanged:9`, the right scope; llm package
    untouched; verify PASS) vs the lexical `filesChanged:15` corruption. Residual
    overhead: the model still spent ~30 calls grep-auditing AFTER the correct
    rename (didn't trust `filesChanged:9`/DONE) → 39 total. That's model over-
    verification, not tool cost — the "models don't trust the rename result"
    theme again; a note/UX follow-up, not a correctness gap. ◻ OPTIONAL next:
    make the rename result even harder to distrust (e.g. echo the touched
    symbols so an audit is unnecessary), and the UTF-16 column fix.

◻ **Cost visibility + planning share an estimator.**
  - ◻ **Cardinality-order a descendant chain.** `A B` evaluates left-to-right;
    if B is far rarer than A, start from B and check ancestors. Needs the same
    per-compound estimate `:explain` renders (below). The ref pushdown was the
    measured 700× case; this is the general form.
  - ✅ **The ~76k inversion floor is gone from query budget.** `sitesByFile`
    is now `symbols.Index.SitesByFile` — index-owned derived state, memoized on
    `gen` (invalidates on Refresh), abs-keyed, liveness-evicting at build. An
    edge query no longer charges the inversion to its budget: `func::in.call`
    dropped 89,894 → 13,379 ops (−85%), `struct::in.type` 89,451 → 12,940.
    **Measured caveat (measure-first paid off): the win is OPS, not wall** —
    the inversion was 50ms of a 1.9s query; wall is unchanged because the real
    bottleneck is the per-target far-end build (the O(sites) item below), which
    this does not touch. Also note the 200k→10000ms default already relieved
    the ops PRESSURE this item was written for; the value now is a lower ops
    floor for `Nops` budgets + large workspaces, not a broad speedup. Tests:
    `TestSitesByFile{EquivalenceAndMemo,EvictsVanishedFiles}`; determinism
    budgets in `TestTrippedBudgetIsReproducible` retuned (trip moved into the
    walk).
  - ◻ **Nothing short-circuits.** `evaluate()` computes the FULL set, then the
    caller slices, so `--limit 5` / `:first` pay for everything. Traversal is
    document-ordered so a top-level early exit at offset+limit is sound.
    **blocking decision**: costs `totalMatches` (can't report "of 24,590"
    without finishing) — node_query's result shape changes.

✅ **`:explain` — cost-visible queries, all 3 commits shipped → done.md.**
`:explain <selector>` returns a cost tree: a-priori `est` (free, from the
commit-2 tallies) beside `measured` work, with `>x` lower bounds on the element
the budget tripped in and `—` for unreached elements. The always-on trace also
upgrades every plain budget-blow to point at the culprit. node_query returns
`{"explain": rows, "truncated"}` — a trace, not matches (the result-shape fork
the plan flagged; resolved by making it a MODE, not a change to plain queries).
**Open remainder**: the est column shows `?` for edges/`*` — the index has no
fan-out. A fan-out estimate (from `::out` avg degree, or the pushdown's
opposite-edge count) would fill those in; deferred until the descendant-chain
planner (below) needs it, since they share the estimator.

✅ **Cardinality-order a descendant chain → done.md.** A plain pure-descendant
chain whose tip is an exact NAME far rarer than the broad leading element is
now seeded from the INDEX (`declsNamed` loads only the files containing the
name) and filtered by an ancestor SUBSEQUENCE — `struct #Name` dropped ~6.9k
work → 0, equivalence + "actually cheaper" gated by test. Lesson recorded: the
first cut (seed via collectMatches) was a correct NO-OP — the tree walk negated
the win; the fix was index-seeding + an O(1) decision (`estCardCheap`, NOT
classCounts). **Remaining planner ideas** (opt-in): a bare-class or edge tip
can't be index-seeded (no class/fan-out in the index) — those still forward.
The fan-out estimate `:explain` shows as `?` for edges is the same gap; fill it
only when a query needs an edge-tip reorder.

◐ **Edges: from coincidence toward reference.** Two of three steps done
(→ done.md): lexical scope killed 99% of far ends (a local is not visible
outside its function), and the child-LSP pass now settles what remains,
per edge, with tri-state `conf: lsp|lexical|unsettled` on every row. **next**:
  - ✅ **The CLI is lexical-only, and now SAYS so.** `bin/dev query` has no
    manager (a one-shot gopls spawn is seconds); its tree renders far ends with
    no conf column. Fixed by a footer caveat that fires whenever a selector uses
    `::in`/`::out` (`usesEdge`), on every path — match, traversal-to-symbols,
    empty, or budget-blow: "edges are name-keyed (lexical) here … the MCP server
    resolves via child LSPs (conf: lsp); `query` does not." Pinned by
    `TestQueryTextLexicalEdgeCaveat`. A per-row conf column is the fuller fix
    but needs the manager the CLI deliberately skips; the footer is the honest
    minimum. (Live proof it's needed: `func#New::out.call` shows `engine.s,
    modSelParser.s` — lexical collisions on the field name `s`.)
  - ✅ **Transitive queries still compound — now they SAY where trust runs
    out.** `::in.call{1,}` spends the per-query LSP cap shallowest-first, so
    deep hops fall back to name-keying. **Decided (USER): not a refusal — a
    WARNING. Say what IS precise and what wasn't.** Shipped: `conf` is now
    TRI-STATE — the old `lexical` conflated two opposites, split into `lexical`
    (name UNIQUE in workspace → certain without an LSP) and `unsettled` (≥2
    same-named decls, no LSP settled it → a GUESS listing candidates).
    `refineFar`/`refineIn` return `unsettled` on every ambiguous-unresolved
    path (incl. the out-of-root `picked==nil` case — Stage 0 no longer reads as
    a false-local `lexical`). `precisionNote` is hop-aware: `evalRepeat`→
    `noteHop` tallies per-hop, and a transitive walk reports "crossed up to N
    hops; M unsettled edges begin at hop K — distant nodes least certain" (or
    "all LSP-resolved or name-unique" when clean). Tests (no-gopls, fast):
    `TestTriStateConfSplitsCertainFromGuess`, `TestTransitiveNoteReportsUnsettledHop`,
    `TestTransitiveNoteCleanWalkSaysSo`; `TestWithoutLSPEdges…` renamed to assert
    `unsettled`. Docs (grammar CONF line, modern.go conf comment, query_text
    caveat `lsp|lexical|unsettled`) updated. **Remaining**: per-hop LSP CAPS
    (spend budget across depth, not all on hop 1) were NOT done — the warning
    makes the current spend honest; a fairer spend is a separate opt-in.
  - ✅ **`::in`/precision round-trips are now cached across queries.** A warm
    session re-asked `textDocument/definition` for the same site every query
    (#New = 93 callers; every edge-precision pass, `:recursive`, external-stub
    resolution). `resolveDefinition` now memoizes on a Server-side `defCache`
    keyed on the site position, valid for ONE index `Generation()` — any
    mutation drops the whole cache, so a stale definition can't outlive an
    edit. The LSP round-trip runs OUTSIDE the lock (never serializes concurrent
    queries); negatives are cached too (an unresolvable site isn't re-asked).
    `defMisses` counts real round-trips. Tests: `TestDefCacheGenInvalidation`,
    `TestDefCacheCachesNegatives` (mechanics), `TestResolveDefinitionCachedAcrossQueries`
    (gopls e2e: identical 2nd query = 0 new round-trips). Pure perf, no
    behavior change.
  - ✅ **The LSP cap is TUNED from the workspace collision rate (Timsort-style).**
    The flat `defaultLSPResolveCap = 200` is gone. Only AMBIGUOUS edges cost a
    round-trip, and names are Zipfian (most unique → free), so the cap only has
    to cover the collision-prone tail: it now scales with the count of declared
    names that have ≥2 EDGE-TARGETABLE declarations (params/return/annotation
    excluded) — `tunedLSPCap` = floor 64 + 4/name, ceilinged at 1500 (bounded
    cost = the explainable-cost moat). Set LAZILY (`ensureLSPCap`) on the first
    round-trip off the already-built `declsByName` (free — the edge builds it
    first), so a non-edge query never pays. Legible: the cap + collision counts
    surface in `precisionNote` when it's hit; `SetLSPResolveCap` overrides.
    **Measured on THIS repo: 216 collision-prone of 1964 declared → cap 928**
    (4.6× the old flat 200 — a collision-heavy codebase gets budget where it
    needs it; a clean one sits at the floor and never hits it). Tests:
    `TestTunedLSPCapFormula`, `TestLSPCapTunedFromCollisions`,
    `TestLSPCapExplicitOverride`. **Next (the broader "tune for code" arc):**
    same treatment for the other magic constants (generated-file line
    threshold as a percentile, etc.), and adaptive re-plan when realized
    cardinality diverges from `:explain`'s estimate.
  - ✅ **A resolved far end OUTSIDE the root is now an EXTERNAL STUB** (North
    Star Stage 0 — SHIPPED). `refineFar`'s `picked==nil` path splits on
    `filepath.IsLocal(defRel)`: outside the root → mint an `external` node
    (`module@version#sym`, `domain:"external"`, ro, `[not indexed]`, conf `lsp`)
    as the far end; inside-but-unmatched stays `unsettled`. `externalIdentity`
    derives the identity best-effort per ecosystem (Go mod cache `@version`,
    stdlib/`node_modules`/`site-packages` package path, dir-base fallback) — always
    nameable, never a false local. `addr()`/`nodeIDs()` handle the class;
    node_query flags the row `domain:"external"`. Tests: `TestExternalIdentity`,
    `TestExternalStubShape` (fast), `TestPrecisionResolvesToExternalStub`
    (gopls e2e: two local `Split` + a `strings.Split` call → `strings#Split`, no
    false local). **Scope note**: only fires on the ≥2-candidate ambiguous path
    (`refineFar` skips len<2); a single local candidate that's actually external
    still fast-paths as `lexical` (asking the LSP per unambiguous edge is the
    cost the skip buys) — documented limitation, not this slice.
  - ✅ **`:recursive` — the first edge-SEMANTIC predicate, LSP-confirmed.** A
    callable with a self-call the child LSP resolves back into its OWN span.
    Unblocked by the precision pass (the icebox parked it as lexically unsound:
    `func Write` calling `w.Write` is io.Writer's, not itself). `isRecursive`
    scans ONLY the func's self-name call sites (`fileSites` filtered to
    `kind==call && name==n.leaf` inside its body) and `confirmSelfCall`
    resolves those via the LSP. No LSP ⇒ confirms nothing and SAYS so
    (`recursive` note / CLI caveat), never a silent false negative. Bare only —
    mutual/cyclic rejects an arg and points at `::out.call{1,}`. Tests:
    `TestRecursivePredicateLSPConfirmed` (gopls), `TestRecursiveWithoutLSPIsUnderResolved`.
    - ✅ **Cost-cliff fixed (DOGFOODING).** The first cut walked `::out.call`
      and resolved EVERY outgoing call of every candidate — a broad
      `func:recursive` on this repo did ~700 round-trips, tripped the cap, and
      returned 1 match with an "UNDER-RESOLVED, may miss real recursion"
      caveat. Only self-NAME calls can be self-edges, so scanning just those
      dropped it to a handful of resolutions: broad `func:recursive` is now
      2.2s and returns **10** (complete) — the cap exhaustion had been HIDING
      9 of them. Found by actually driving the MCP server, not the tests.
  - ✅ **`.implements` — the first LSP-NATIVE edge kind.** `interface#Foo::in.implements
    > *` = implementers; `type#Bar::out.implements > *` = interfaces Bar
    satisfies. Go's structural typing has NO lexical clause to key on, so
    unlike .call/.type/.import this edge is resolved ENTIRELY by the child LSP
    (`textDocument/implementation`, `resolveImplementations` + a gen-keyed
    `implCache`), built ONLY when explicitly named (one round-trip per host,
    never for a bare ::in/::out). `implementsRefs` maps each target via `declAt`
    (in-root) or an external stub (out-of-root, e.g. a workspace type
    satisfying `io.Reader`); needed the symbol NAME position, so `nameAt` is now
    on treeNode. conf `lsp`; no-LSP ⇒ `implements` note / CLI caveat, never a
    silent empty. Tests: `TestImplementsEdgeKind` (gopls: Animal→Dog/Cat, not
    NotAnimal; Dog→Animal), `TestImplementsWithoutLSPIsUnavailable`, parse.
**Assumption made**: `textDocument/definition`'s first location is the
declaration. True for gopls; unverified for tsserver/pylsp.

◻ **Node model — loose ends found this session.**
  - ✅ **TS `::in.type` double-count — NOT reproducible; the ❓ was stale.**
    Verified across interface / class / generic / export / .tsx / union /
    cross-file: `Widget::in.type` counts each occurrence ONCE, split cleanly by
    the position axis (param/return/field), with value refs (`new Widget()`) out
    of `.type`. The index emits 4 DISTINCT positions for 1 decl + 3 uses (no
    site dup); the old "4 on 2 uses" was fixed en route (likely the span-
    containment attribution that stopped name-only double-attribution). Pinned
    by `TestTSInTypeNoDoubleCount` so a real dup can't creep back.
  - ✅ **`return` as a NODE — shipped.** The return TYPE is now a `return`
    CHILD of every callable (`func:any(return#error)` = funcs returning error),
    across Go/TS/Python (`appendReturnSymbols` + `returnTypeNodes`: Go `result`,
    TS/Python `return_type`). Go's `(T, error)` tuple SPLITS into one child per
    type so `return#error` matches it; a qualified type answers to its leaf
    (`return#Writer`) and its full alias (`return#'io.Writer'`). Three
    integration snags, all fixed + pinned: a `return` node's span is the
    signature line, so `enclosingSymPath` must skip it (else it steals call/type
    sites from the func — like `argument`); its name span sits ON the type
    usage, so `isDeclSite` must skip it (else the ref is deleted); and it answers
    to `#Type`, so it's excluded from `refNodes` edge-building (else
    `#Type::in.type` doubles). Tests: `symbols/return_node_test.go` (Go/TS/Py),
    `TestModernQueryReturnNode`. **Remaining** (icebox): `var` slot nodes, and
    the return-VALUE slot (needs column precision — param vs return share a line).
  - ✅ **`:arity(m,n)` — signature-size filter, shipped.** Sound/structural
    (counts `argument` children, no edge guessing): `:arity(2)` exact,
    `:arity(2,)` 2+, `:arity(0,0)` no-arg. `parseParenRange` mirrors the `{m,n}`
    shape; `TestModernQueryArity`.
  - ✅ **`search`/`::grep` long-line cap + generated-file skip, shipped** (icebox
    field-report BUG). `symbols.CapHitLine` (rune-safe, match-centred, 500B)
    caps every matched line on BOTH surfaces; `symbols.Search` skips files with
    a >5000B line (reported as `skippedGeneratedFiles`, `IncludeGenerated` opts
    back in). Tests: `symbols/cap_test.go`, `TestModernQueryGrepCapsLongLine`.
  - ✅ **`::signature` / `::body` pseudo-elements — shipped.** A callable split
    into its decl HEAD (doc- and body-excluded) and body block, GENERATED nodes
    (invisible to `*`) carrying source INLINE so `func::signature` is a one-query
    overview. `Symbol.BodyStartLine` (tree-sitter `body` field) is the split;
    the doc is skipped via the stored `commentAt`. `genPartOf`/`genPartMatches`
    mirror `::comment`; `isGenerated()` folds them into the planner guards.
    Known imprecision (line-granular): the `{` line shows in both halves on a
    single-line signature — column precision is v2. Tests: `mcp/genpart_test.go`
    (Go/TS/Python, invisibility to `*`, non-callables excluded).

Next candidates are opt-in, in icebox.md — most valuable: a real-agent
adoption vehicle (autowork3 is dead; needs a replacement), then the
external-stub Stage 0 node (conf is now honest, the node is not yet).

Known caveats (documented in code): edges are name-keyed via the lexical
index, so same-named symbols share edges (the LSP pass is the fix);
unbounded `{m,}` collects nodes at their shortest hop; `:where(sel)` ≡
`:any(sel)` at tip granularity (pseudoHolds).

## Non-goals (for now)

- Indexing the entire host filesystem; we only index inside the git root. (The
  daemon serving MANY roots does not change this — each workspace stays
  single-rooted; the daemon is a process-sharing move, not a scope change.)
- Replacing any single child LSP. We multiplex, we don't reimplement.
- Sandboxing child LSPs. They run as the user.
- Windows support until someone asks.
