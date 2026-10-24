# poly-lsp-mcp

A polyglot LSP + MCP server in Go. One binary, two surfaces:

- **LSP server** (`poly-lsp-mcp`) — editor integration. Multiplexes child language servers (gopls / tsserver / pylsp / …) and falls back to tree-sitter where no child exists.
- **MCP server** (`poly-lsp-mcp mcp`) — LLM agent integration. The same workspace machinery exposed as **three tools** (`node_query` / `node_read` / `node_edit`) over a unified node tree, driven by a **CSS-inspired selector language** that queries containment *and* the reference graph. Edits are **LSP-validated by default** so an agent can't leave the workspace broken — an edit that introduces a new error is reverted, and `--no-validate` turns that off.
- **Query CLI** (`poly-lsp-mcp query <selector>`) — run one selector from the shell, no editor or agent.
- **Child-LSP lifecycle** — a child LSP (gopls etc.) is stopped after `--lsp-idle-stop` (15m) without use, above `--lsp-max-rss-mb` (6144), or under machine memory pressure (`--lsp-psi-threshold`, PSI some avg10 ≥ 20; Linux only — macOS has no PSI); it starts again on the next request and gets its open documents back. `poly-lsp-mcp status` lists every process's children: pid, RSS, idle time, restarts and stops with reasons.
- **Daemon mode** (the default for `poly-lsp-mcp mcp`; `--no-daemon` opts out) — every client proxies to ONE shared per-user daemon over a unix socket, so the workspace index, the child-LSP fleet and the parse cache exist once instead of once per session. Auto-starts on first use; `poly-lsp-mcp daemon --stop` / `--restart` manage it, `--read-only` / `--validate` are enforced per connection, and a newer build takes over blue/green with the index warm. See [Daemon mode](#daemon-mode-the-default).

The unique value-add over single-language LSPs is **cross-language linkage**: rename `UserID` in Go and the rename propagates through TypeScript, Python, YAML config values, proto messages, OpenAPI schemas, and prose that names code in `backticks` — declared bindings, schema-anchored sites, and `@ref` comment markers all stitch the languages together.

## Language support

Every language below is indexed for cross-language lookup. **Node model** means
a file resolves into addressable symbols (`file#Type.method`) with declaration,
name, doc-comment and signature/body ranges; without it a file is still
readable and editable, just as one whole-file node. **Child LSP** is the
default server spawned when it is on `PATH` — a missing binary is logged and
skipped, never fatal.

| Language | Extensions | Node model | Child LSP | Signature refactor |
|---|---|---|---|---|
| Go | `.go` | ✅ | gopls | ✅ |
| TypeScript / TSX / JS | `.ts .tsx .js .jsx .mjs .cjs` | ✅ | typescript-language-server | ✅ |
| Python | `.py .pyi` | ✅ | pylsp | ✅ |
| Java | `.java` | ✅ | — (jdtls opt-in) | ✅ |
| Kotlin | `.kt .kts` | ✅ | — (opt-in) | ✅ |
| Groovy | `.groovy .gradle .gvy .gy` | ✅ | — (opt-in) | ✅ |
| C | `.c` | ✅ | clangd | ✅ |
| C++ | `.cpp .cc .cxx .c++ .h .hpp .hh .hxx .h++ .ipp .tcc .inl` | ✅ | clangd | ✅ |
| SQL | `.sql .psql` | ✅ | — | — |
| XML | `.xml` | ✅ | — | ✅ attributes |
| Markdown | `.md .markdown` | ✅ sections | — | — |
| Proto / GraphQL | `.proto .graphql .gql` | — | — | — |
| YAML / JSON | `.yaml .yml .json` | — | — | — |

Notes worth knowing before you rely on them:

- **`.h` belongs to C++**, not C. The C++ grammar is a superset, so a C header
  still parses correctly, while a C++ header named `.h` — the ecosystem norm —
  would lose every class under the C grammar.
- **Markdown is structural.** A document's node tree is its outline: nested
  `section` nodes whose range covers the heading *and* its body, so
  `README.md#'MCP mode.Selector language'` addresses a section and `node_read`
  returns it. Only headings, fenced code and inline `` `code` `` spans enter
  the name index — prose does not.
- **Rename works in every language, including the ones with no grammar.** It
  runs over the index, so renaming an Android resource rewrites both the
  `<string name="x">` declaration and every `@string/x` reference in one call.
- **Signature refactor** rewrites a callable's parameters and return type. For
  **XML** the parameter list is an element's ATTRIBUTES (`params:[{name:
  "android:enabled", type: "true"}]` rewrites the open tag); XML has no return
  type and asking for one is an error. SQL and Markdown are the remaining
  exceptions — a stored function's callers are strings inside other
  statements, and a document section has no call site at all.
- **YAML and JSON are lexical on purpose** — a config *value* is a contract, so
  every token is indexed. That is what makes a YAML string match a Go type.
- A Jenkinsfile is Groovy but has no extension, and the registry keys on
  extension alone, so it is not routed.

Languages, extensions and child-LSP commands are all overridable in
`poly-lsp-mcp.yaml`.

**Queries stop at the limit.** A `limit` (default 20) short-circuits the walk
instead of computing every match and slicing, so a broad selector is cheap.
The cost is the exact count: a short-circuited run reports
`totalMatchesAtLeast: ">20"` rather than `totalMatches`, because the walk
never learned the total. Composed selectors — unions, chains, edges,
`:has()` — still evaluate fully and report an exact `totalMatches`.

**The index honours `.gitignore`.** A repo's throwaway data — tool state, lock
files, captured payloads — otherwise outweighs its source: measured on one
real repo, ignored files were 78% of every indexed site. Ignored is filtered;
**untracked is not**, so a file you just created and have not `git add`-ed is
still indexed. Outside a git repository the walk is unfiltered.

## Install

```sh
git clone https://github.com/iodesystems/poly-lsp-mcp.git
cd poly-lsp-mcp
make install
```

Or download a prebuilt binary (linux or darwin, amd64 or arm64) from the
latest GitHub release and check it against `SHA256SUMS`:

```sh
os=$(uname -s | tr A-Z a-z); arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
base=https://github.com/IodeSystems/poly-lsp-mcp/releases/latest/download
curl -fsSLO "$base/poly-lsp-mcp_${os}_${arch}.tar.gz"
curl -fsSLO "$base/SHA256SUMS"
sha256sum -c --ignore-missing SHA256SUMS   # macOS: shasum -a 256 -c --ignore-missing SHA256SUMS
tar -xzf "poly-lsp-mcp_${os}_${arch}.tar.gz" poly-lsp-mcp
install -m 755 poly-lsp-mcp ~/.local/bin/
```

Asset names carry no version. Releases are unsigned here; dun publishes the
signed manifest that points at them. `make dist` builds the same tarball for
this machine under `dist/`.

From source, the binary lands at `~/.local/bin/poly-lsp-mcp` (`make install BINDIR=...` to
change), on Linux and macOS. Without a checkout:
`go install github.com/iodesystems/poly-lsp-mcp@latest` (to `~/go/bin`).

Building from source needs only Go. With a C compiler on the machine the
build links the C tree-sitter runtime; without one, Go builds without cgo and
the binary parses with a pure-Go tree-sitter runtime instead: the same
grammars, but about 10x slower to parse and 6x slower to index, and source
with syntax errors can recover into a different tree. Go picks by itself;
`poly-lsp-mcp doctor` says which runtime a binary has. For the fast build,
install a C compiler (Linux: `gcc`, e.g. `apt install build-essential`;
macOS: `xcode-select --install`) and run the install again. Release
binaries are always the cgo build.

Child LSPs are optional, one per language: `gopls`, `typescript-language-server`,
`pylsp`, `clangd`. Without one, that language is served from tree-sitter alone:
its `::in`/`::out` edges are not resolved and edits to it are not validated.

```sh
poly-lsp-mcp doctor            # in the workspace
```

checks the binary PATH runs (and stale copies behind it), the child LSP for
each language the workspace holds with its version and install command, git,
and the daemon. Exits 1 when a language in use has no child LSP.

`doctor` also looks for a newer poly-lsp-mcp, by how the binary was built: a
release binary asks GitHub for the latest release, a `go install` binary asks
the Go module proxy, and a build from a checkout compares the checkout with its
upstream, but only when `LSP_REBUILD_PATH` names it (the binary carries no
source path, by design). The answer is one `update` line, or the `update`
object of `--json`; it is cached for 24 hours, silent when offline, never
affects the exit status, and `--no-update-check` skips it.

The MCP server says the same after bring-up: `notifications/message` from
logger `poly-lsp-mcp/setup` (level `warning` when one is missing), data
`{languages: [{lang, files, lsp: {name, found, path}, adds, install: {cmd,
needs}}], missing}` — the shape `doctor --json` uses for `languages`.

### Dev: rebuild on spawn

poly-lsp-mcp is always *spawned* — dun runs it off `PATH`, editors launch the
LSP — so nothing in the loop notices the installed binary is stale, and a
session can run for hours against a build that predates the fix it is hunting.
Point it at its own checkout and it rebuilds itself in place and re-execs
whenever a `.go` file there is newer:

```sh
export LSP_REBUILD_PATH=~/src/poly-lsp-mcp   # in the profile that starts your agent/editor
```

Unset it to turn this off; there is no other switch, and nothing is stamped
into the binary, so a build you distribute cannot do this. The path is checked
before anything is built — absolute, owned by you, not world-writable, and its
`go.mod` must declare this module — and a path that fails says so on stderr.
A build failure is non-fatal: it warns and runs the binary you already have.

## LSP mode

Default invocation. Speaks LSP over stdio. Point your editor's language client at the binary; configure per-workspace via `poly-lsp-mcp.yaml`.

Example (Neovim, nvim-lspconfig):

```lua
require('lspconfig.configs').poly = {
  default_config = {
    cmd = { 'poly-lsp-mcp' },
    filetypes = { 'go', 'typescript', 'python', 'proto' },
    root_dir = require('lspconfig.util').root_pattern('poly-lsp-mcp.yaml', '.git'),
  },
}
require('lspconfig').poly.setup({})
```

What it owns over the child LSPs:

- `workspace/symbol`, `textDocument/references` — answered from the cross-language symbol index (lexical + declared + schema-anchored sites).
- `textDocument/rename` — synthesizes a `WorkspaceEdit` that touches every site for the name including string-literal YAML/JSON values and prose `@ref` markers.
- `textDocument/documentSymbol` — forward to child LSP first, fallback to the index.

Child LSP requests (`hover`, `definition`, completion, signature help, code actions, formatting, …) are routed through to the appropriate child. If the child crashes, multiplex restarts it with exponential backoff (default 1s → 30s, 5 attempts).

## MCP mode

```sh
poly-lsp-mcp mcp --root /path/to/workspace
```

Speaks MCP (newline-delimited JSON-RPC) over stdio. The **default surface is three tools** over one unified node tree (`project > dir > file > symbols > argument`), addressed as `<file>#<sym>`, driven by a CSS-inspired selector:

| Tool | Purpose |
|------|---------|
| `node_query` | Find nodes with a CSS-like selector over the AST **and the reference graph**. Returns `matches[].node` addresses. `:explain <selector>` returns a cost tree instead of matches; `"?"` returns the grammar. |
| `node_read` | Read a node whole — a symbol's complete declaration (never truncated), or a file — via a `<file>#<sym>` address or a selector matching exactly one node. |
| `node_edit` | Edit one node: `oldText`+`newText`, whole-node rewrite, `delete`, `rename` (workspace-wide, atomic), `params`/`return` (signature). Under `--validate`, an edit that introduces a new error is reverted. |

### Startup and index size

`initialize` replies at once with `"index": {"state": "building"}`; the index, bindings and child LSPs come up behind it, and a tool call waits for them. When the bring-up ends, the server sends `notifications/message` from logger `poly-lsp-mcp/index` with `state` (`ready` or `failed`), `ms`, and `index`: files, bytes, and the heaviest directories by share of indexed bytes, each with its file count, bytes, time and the number of nested checkouts (directories with their own `.git`) under it.

In a git repository the file set is git's: `git ls-files --cached --recurse-submodules` plus `--others --exclude-standard`, iterated with no walk. Submodules are included; a nested repository git reports (`dir/` with its own `.git`) is left out as `worktree of this repo` or `another repo`, from the index, the watcher, child-LSP opens and broad `node_query` (naming a path inside one still reaches it). The report's `leftOut` lists every directory not looked into, with its reason (`git-ignored`, a nested repo, `excluded`, `unreadable`). Outside a repository the walk is used.

The index walk has a startup budget, `--index-budget`, in `node_query`'s grammar: `Nms` wall-clock (bare = ms) or `Nops`, one op per file read (default `30000ms`; `0` = unbounded). Over budget the build fails, but the walk goes on counting without reading, so the failure is a trace of where the files are. The notification (level `error`, `budget`, `read`) and every tool call carry that trace and both ways out: get under budget with `--exclude <dir>` (repeatable, relative to `--root`; the directory is dropped from the index, the query tree and every walk, and the repository is not changed) or `.gitignore`; or raise it, with a suggested `--index-budget` scaled from what was read.

Some roots and files are decided for you. Each decision is reported, with the way to revert it:

| Decision | Reason in `leftOut` | Override |
|----------|---------------------|----------|
| Root is the home directory, `/`, or a directory outside any repository with 3+ clones in it: not indexed. The bring-up fails at once (`state` `failed`, `reason` `not a project`, `repos`: up to 20 found, relative) and every tool call says the same | — | `--root <one of them>`, or `--index-anyway` |
| A directory such as `node_modules`, `vendor`, `dist`, `build` | `skipped dir` | `--include <dir>` |
| A file over 1 MiB | `over size` | `--include <path>` (indexed whatever its size; the budget still bounds the build) |
| A `*.min.*` file | `minified` | `--include <path>` |
| A Git LFS pointer (content not fetched) | `LFS pointer (not downloaded)` | `git lfs pull` |
| A symlink whose target leaves the root | `symlink leaves root` | `--include <link path>` |
| A symlink more than `--symlink-depth` (4) links deep; `0` follows none | `symlink depth` | `--symlink-depth N` |
| A dir symlink to a directory already open on its own path | `symlink loop` | — |
| A symlink to a path indexed under its own name (indexed once) | `symlink to indexed path` | — |

Symlinks whose target is inside the root are followed, files and directories, with ignores applied to both the link's path and the target's. `--include <pattern>` (repeatable, gitignore-style, on `mcp`, `index` and `query`; also `include:` in `poly-lsp-mcp.yaml`) overrides the guesses in the table and opens a nested repository; it does not override `.gitignore` or `--exclude`. `leftOut` lists at most 20 entries per reason; `leftOutBy` gives each reason's true count and its hint.

`poly-lsp-mcp index --root .` prints the same report from the shell (`--json` for the notification's `index` shape); `poly-lsp-mcp status` prints it for every running server.

`node_query` skips `node_modules`, `vendor`, `dist`, `build` and the like in broad traversal, but a selector that names a path inside one (`file[path=node_modules/react/index.d.ts]`, `#'vendor/x' func`) reaches that path and nothing beside it. It does not apply `.gitignore`; every `node_query` appends one line to `~/.poly-lsp/usage.jsonl` (matches, how many were gitignored or in a skipped directory, and whether the selector named them), and `poly-lsp-mcp usage` totals it — the evidence for whether it should.

When a `node_query` leaves something out, its result carries `leftOut`: `pruned` counts what the walk did not enter, by reason (a skipped directory such as `node_modules/`, `worktree of this repo`, `another repo`, `excluded`; `shadowed` counts files, the rest directories), `deadEnds` counts edges a child LSP resolved into left-out code, and `hint` says how to search inside: name a path in the selector (reaches that path only), or pass `include`, an array of gitignore-style patterns (`node_modules` at any depth, `.claude/worktrees/a1` anchored, `*`/`?`/`**`, trailing `/` for directories). A pattern opens whatever the walk left out for any reason (skipped directory, nested repo, `excluded`, `shadowed`) and walks it fully; `.git` is never entered. `poly-lsp-mcp query --include <pattern>` does the same and may repeat. `leftOut` is absent when nothing was left out.

### Child LSPs per project root — `--lsp-roots`

`--lsp-roots single` (the default) runs one child LSP per language at the workspace root. gopls and typescript-language-server resolve several modules or packages from there; pylsp and clangd in a monorepo do not, and one instance holds every package in memory. `--lsp-roots auto` (on `mcp` and `daemon`) runs one child per (language, project root) instead:

| Language | Project root marker |
|----------|---------------------|
| go | `go.mod`; a `go.work` at the workspace root keeps one root there |
| typescript | `tsconfig.json` beside a `package.json`, or one no other `tsconfig.json` encloses |
| python | `pyproject.toml`, `setup.py`, `setup.cfg` (nested packages are roots of their own) |
| c, cpp | `compile_commands.json`, or the outermost `CMakeLists.txt` |

The search skips what the index skips (`node_modules`, `vendor` and the like, gitignored and excluded directories) and test fixtures (`testdata`, and `fixtures`/`__fixtures__` under a test dir; they stay indexed), and stops after 2 s or 20,000 directories, and says so. Each child starts on the first request for a file under its root; a file goes to the nearest enclosing root, else to a child at the workspace root. At most `--lsp-roots-max` (8) run per language: the least recently used is stopped (reason `lru`) and starts again on demand, and idle and memory stops apply to each child as before. `poly-lsp-mcp status` shows each child's root (`LSP ROOT`).

Nothing switches mode by itself. In `single` mode, when the index budget is blown or a child is stopped for memory and a language has several project roots, the `poly-lsp-mcp/setup` notification (`hints`, `roots`) and the budget remedy offer `--lsp-roots auto`; `poly-lsp-mcp doctor` lists the roots per language and offers it when there are several. In `auto` mode the setup notification lists the roots and how to revert (`--lsp-roots single`).

### Daemon mode (the default)

`poly-lsp-mcp mcp --root <dir>` serves the MCP surface from one shared per-user daemon, so the index, the parse cache and the child LSPs exist once for every session on a root. `--daemon` is still accepted: it keeps daemon mode when an in-process-only setting (below) is also given.

- **In-process.** `--no-daemon` (or `$POLY_LSP_NO_DAEMON=1`) builds the index and runs the child LSPs in the `mcp` process, one set per session. Settings the daemon does not take from a client also mean in-process, logged: `--legacy-tools`, `--read-char-budget` / `$POLY_LSP_READ_CHAR_BUDGET`, `--config` or a `poly-lsp-mcp.yaml` in the working directory, `--lsp-idle-stop`, `--lsp-max-rss-mb`, `--lsp-psi-threshold`, `--lsp-roots-max`. With an explicit `--daemon` they are ignored instead, logged. A daemon that cannot start (no writable `~/.poly-lsp`, a socket that does not come up within 10 s) is logged and the session runs in-process.

- **Start.** Before it reads stdin the proxy dials the daemon and starts it if nothing answers (`poly-lsp-mcp daemon`, detached, log in `~/.poly-lsp/daemon.log`); a daemon binds its socket before it loads anything. Then it answers `initialize` at once and opens the root behind the reply. The serving daemon holds `~/.poly-lsp/daemon.lock` (flock): of two daemons started at once, one serves and the other exits. `~/.poly-lsp/daemon.json` names its pid, socket, build and flags. The socket is `$XDG_RUNTIME_DIR/poly-lsp/daemon.sock`, else `~/.poly-lsp/daemon.sock`; a path too long for a unix socket falls back to a short hashed name.
- **Same as stdio.** Each root comes up as an async stdio server does, bounded by the index budget, with the `poly-lsp-mcp/index` (cost, `leftOut`) and `poly-lsp-mcp/setup` notifications, and a tool call waits for it. A client's `--exclude`, `--include`, `--index-budget`, `--index-anyway`, `--symlink-depth` and `--lsp-roots` travel with its open. One index per root, and the last open wins: an open whose options differ from those the root was built with (the daemon's defaults filled in) rebuilds it, and every client on the root gets a `poly-lsp-mcp/daemon` notice `rebuilding` (from, to); equal options keep the warm root. `daemon` takes the same flags for every root. A failed root answers tool calls with its post-mortem; a new client retries it. `daemon --restart` with no flags replays the running daemon's.
- **Lifetime.** A root lives as long as it has clients. When its last client disconnects, its child LSPs stop after `daemon --root-lsp-grace` (1m; they start again on demand) and its index stays, in an LRU of client-less roots bounded by `--idle-roots-mb` (1024; 0 keeps none, -1 no bound): past it the least recently used root is evicted. A root with a client is never evicted; the size is an estimate, bytes indexed × 8 (the live Go heap per indexed byte, measured on this repository). The daemon exits after `--idle-exit` (1h; 0 never) with no client, or within ~10 s when it has no client and keeps no root; the next `mcp` client starts it again. `--lsp-idle-stop` applies to each child as in stdio mode. `doctor` lists the daemon's roots: clients, estimate, whether their child LSPs are stopped.
- **Roots.** The socket admits only processes of the daemon's own user. `daemon --allow <dir>` (repeatable; default `$HOME`) is the starting allow list; a root a client opens outside it is added (logged) and carried across an upgrade. `poly-lsp-mcp query` through the daemon does not open a root: on a root not on the list it runs standalone.
- **File-access hook.** `--hook` / `$POLY_LSP_HOOK` (see `mcp/hook.go`: exit 2 cancels with stdout as the answer) runs in the proxy, with the client's environment, before each `node_read` / `node_edit`; the daemon names the file and op as the stdio server would.
- **Build identity.** Every binary has a build id with no stamp: VCS revision (`+dirty`) and a sha256 prefix of the executable. `doctor` prints it and whether the running daemon is the same build.
- **Upgrade.** A proxy of a newer build asks the daemon to move to the proxy's binary (once per build pair). The daemon also notices when its own binary file is replaced (checked every 2 s). The upgrade is blue/green: the old daemon starts the new build beside it. The new one warms the roots used in the last `--warm-recent` (30m): parse cache and index, no child LSPs, which start on a root's first request. When it is warm, it renames its socket over the main path and the old daemon drains (up to 10 s) and exits. Calls keep being served during the swap. A read-only call cut by the swap is sent again once; a cut `node_edit` returns an error that says to re-read and retry.
- **Notices.** Clients get `notifications/message` from logger `poly-lsp-mcp/daemon`: `upgrading` (from, to, why), `upgraded`, `failed` (the error, and `poly-lsp-mcp daemon --restart` to recover), and `rebuilding` (a root's index options changed: root, from, to).
- **Rollback.** If the new build fails to come up, the old daemon keeps serving. After 2 failures that build is refused (`~/.poly-lsp/upgrade-failures.json`; delete it to try again). The binary that served before the last upgrade is kept at `~/.poly-lsp/exe.prev`: run `~/.poly-lsp/exe.prev daemon --restart` to go back to it. `doctor` reports both.

### Selector language

CSS selectors, but the document is your codebase. `func` is a tag, `#name` an id, `>` is child, space is descendant, `,` is union; `:has()` / `:not()` / `:where()` behave as in CSS, `:first` / `:last` as in jQuery. One new idea: the reference **graph** rides on pseudo-element syntax — `::in` / `::out` are edges the way `::before` is generated content, and `::grep` / `::comment` / `::signature` / `::body` are other generated views of a node.

```
#'store.go#Save'::in.call                              who calls Save (callers, with call sites)
#main::out.call > *                                    what main calls (callees)
#Handler::in.call{1,} > *                              everything that reaches Handler (transitive)
type#Server > method                                   methods of the Server type
struct:has(field[name$=ID])                            structs that declare an *ID field
func:any(return#error)                                 funcs that return an error
func:recursive                                         funcs that call themselves (LSP-confirmed)
func:arity(0,0)                                         no-argument funcs
func::signature                                        every function signature (one-query overview)
interface#Reader::in.implements > *                    types that implement Reader (LSP-resolved)
func:not([name^=Test]):empty(::in)                     dead code
file:has(func:where(::out.call{15,16}))                files whose funcs call 15–16 deep
import#huma::in.call::grep('-E (Get|Post)\(')          endpoints of a dependency
:root > *                                              tour the workspace
```

Take any match's `node` (`<file>#<sym>`) straight to `node_read` / `node_edit`. Attribute axes: `[name]` (what it's called) vs `[path]` (where it lives); operators `= ^= $= *=` (literal) and `~=` (regex). A **space is always a node boundary** — to filter an element, bracket onto it (`func[path=a.go]` = funcs in that file; `func path=a.go` = anything in that file *inside* a func), and a bare attribute is its own `*[…]` element. Inside brackets, `|` is OR, `&` is AND and `()` groups — but **only when the next thing is another test or a group**: `[name=a|name=b]` is boolean, while `[name~=a|b]` is a single regex, since `b` is not an attribute. The reading returned with every query says which way it was taken. Bare `:any/:all/:empty` are position claims; `:parents(sel)` is the one inverse (upstream). `{m,n}` is regex repetition on elements, edge *hops* on an edge (`::in.call{1,}` = transitive). Reference **kind** is a class (`.call`/`.type`/`.import`), **position** another (`.return`/`.param`/`.field`). A callable's **return type** and **parameters** are addressable children (`return#error`, `argument#ctx`), and `:arity(m,n)` / `:recursive` filter by signature size and self-recursion.

**Edges carry a confidence label.** The index is name-keyed by default; a running child LSP settles the true target, stamped per edge: `lexical` (name is unique — certain), `lsp` (resolved), or `unsettled` (several same-named decls, no LSP — a labelled guess). A far end the LSP resolves *outside* the git root (stdlib, a dependency) becomes a read-only **external stub** — `to: ["strings#Split"], domain: "external"` — nameable, never a false local. Partiality is always surfaced — a budget-exhausted query says so, an ambiguous edge says so — so an agent never mistakes a guess for a fact.

### Safe editing — `--validate`

```sh
poly-lsp-mcp mcp --root . --validate
```

`node_edit` runs the child LSP after each write and **reverts any edit that introduces a NEW error** — same-file *or* cross-file (a rename that breaks a caller is caught workspace-wide, once gopls's package re-check settles). The edit is reported `rejected` with the offending `newErrors` instead of landing; a multi-file rename/signature reverts as one all-or-nothing unit. Without a child LSP the edit is applied but flagged `validated:false` — never a silent pass. This keeps the agent's grep→read→edit loop but makes it unable to leave the workspace in a broken state.

### Legacy 9-tool surface (`--legacy-tools`)

The prior surface — `structure`, `search`, `node_references`, `node_read`, `node_edit`, `node_delete`, `node_refactor`, `node_rename_file`, and a bare-grammar `node_query` — is still available behind `--legacy-tools` (add `--read-only` to hide every mutating tool). Its tools:

| Tool | Purpose |
|------|---------|
| `structure` | Directory walk OR tree-sitter named children of a file with decl + name ranges. Optional `grep` regex prunes to matching subtrees (by name). |
| `search` | Regex search over file *contents* across the workspace. Use this for full-text search; `structure(grep=…)` is for symbol/file-name search. |
| `node_references` | Workspace-wide references to the identifier at a range (lexical / declared / comment confidence). |
| `node_read` | Read whole file, line preview, or byte-precise range. |
| `node_edit` | Atomic write: whole-file create-or-overwrite, range replace, or unified-diff patch. |
| `node_delete` | Delete a range OR delete the whole file. |
| `node_refactor` | Composable cross-language refactor: `refactor:{rename?, params?, return?}`. Signature rewriting supports go / typescript / python / java / kotlin / groovy / c / c++, plus xml (attributes); rename works everywhere. |

### Tool capability matrix

Most tools are polymorphic — pick the input shape that fits the task. The "node" prefix in the names is historical from the early AST-only days; today the tools work at three levels: raw file, line preview, and AST/range.

| Tool | `{file}` | line-based | byte-precise range | `{file, diff}` | Other |
|---|---|---|---|---|---|
| `structure` | ✓ (listing, optional `grep`, `depth`, `nodeLimit`) | — | — | — | — |
| `search` | — | — | — | — | `{pattern, path?, glob?, limit?, contextLines?}` |
| `node_read` | ✓ whole file (auto-capped ~2k chars; reports `truncated` / `totalChars` / `totalLines` / `hint`) | ✓ `{file, startLine?, lineLimit?, lineLength?}` | ✓ `{file, startLine, startCol, endLine, endCol}` | — | — |
| `node_edit` | ✓ (with `newText`: create-or-overwrite, auto-mkdir parent) | — | ✓ (with `newText`: range replace) | ✓ unified-diff patch (strict context) | — |
| `node_delete` | ✓ delete file from disk | — | ✓ delete range | — | — |
| `node_references` | — | — | ✓ identifier range required | — | — |
| `node_refactor` | — | — | ✓ identifier range required | — | `refactor:{rename, params, return}` |

**Truncation contract.** `structure` and `node_read` both auto-cap on size (250 nodes / ~2k chars by default). When the cap fires they emit `truncated: true` plus `truncatedReason` (`"auto"` when the implicit cap fired, `"nodeLimit"` / `"lineLimit"` / `"lineLength"` when the agent set the cap), `totalNodes` or `totalChars` / `totalLines` / `maxLineLength`, and a `hint` string explaining how to widen or continue. The agent never has to wonder whether a response was clipped.

`node_read` / `node_edit` / `node_delete` work on **any file** regardless of language or whether a tree-sitter grammar exists — markdown, JSON, plain text, config files all fine. AST features only activate when you ask for them via `structure` or pass identifier ranges to the semantic tools.

### Resources

- `poly-lsp-mcp://workspace` — `{root, languages, names, declared}` summary.
- `poly-lsp-mcp://bindings` — every declared cross-language binding (Tier 2 + Tier 3).
- `poly-lsp-mcp://diagnostics` — workspace-wide diagnostic snapshot enriched the same way edit responses are.

Edit / refactor responses carry enriched diagnostics: `{text, context, enclosingNode, references}` per diagnostic. Sibling-file diagnostics roll up by default so compile cascades are visible in one response. Configurable caps per call (`diagnosticLimit`, `referenceLimit`, `contextLines`, `siblingDiagnostics`).

## Query CLI

Run a selector once from the shell — no editor, no agent:

```sh
poly-lsp-mcp query --root . "#spend::in.call"          # callers of spend
poly-lsp-mcp query --root . "type#Server > method"     # methods of the Server type
poly-lsp-mcp query --root . ":root > *"                # workspace tour
poly-lsp-mcp query --root . ":explain <selector>"      # cost tree, not matches
poly-lsp-mcp query --root . "?"                        # the full grammar
```

Renders a grouped tree of matches per file. Lexical-only by default (a one-shot run spawns no child LSP), so reference edges are name-keyed — run the MCP server for LSP-settled edges. `--budget Nms|Nops` raises the work budget when a broad query reports it stopped early.

## Configuration

`poly-lsp-mcp.yaml` at the workspace root. All sections optional; defaults work for the supported languages.

```yaml
# Per-language config. Override the defaults if you need custom LSP
# args or extensions.
languages:
  - name: go
    extensions: [go]
    lsp: {cmd: gopls}
    treesitter: go
  - name: typescript
    extensions: [ts, tsx, js, jsx, mjs, cjs]
    lsp: {cmd: typescript-language-server, args: ["--stdio"]}
    treesitter: typescript

# Tier 2: hand-declared cross-language bindings. Three site forms:
# symbol (identifier match), jsonpath (YAML/JSON values), regex.
bindings:
  - name: UserType
    sites:
      - {file: main.go, symbol: UserID}
      - {file: client.ts, symbol: UserID}
      - {file: config.yaml, jsonpath: "$.users[*].id"}
      - {file: schema.sql, regex: ["\\buser_id\\b"]}

# Tier 3: schema-anchored. One entry per schema file auto-binds every
# named entity (proto messages / openapi components / jsonschema $defs).
schemas:
  - {file: api.proto, dialect: proto}
  - {file: openapi.yaml, dialect: openapi}

# Auto-detect schemas in the workspace at startup. Opt-in because the
# scan touches every YAML/JSON file looking for distinctive top-level
# keys.
auto_schemas: true

# Shadowing: in one directory, a file ending in one of `over` hides the
# same-named file ending in one of `hides` (foo.js beside foo.ts). Hidden
# from the index, broad node_query and child-LSP opens; reachable by
# naming its path; counted in the index report. Omit the key for these
# defaults; `shadows: []` turns it off.
shadows:
  - {name: ts over js,   over: [ts, tsx, mts, cts], hides: [js, jsx, mjs, cjs]}
  - {name: ts over d.ts, over: [ts, tsx, mts, cts], hides: [d.ts, d.mts, d.cts]}
  - {name: d.ts over js, over: [d.ts, d.mts, d.cts], hides: [js, mjs, cjs]}
  - {name: pyi over py,  over: [pyi], hides: [py]}
```

## Cross-language linkage — three tiers

| Tier | Setup | What it catches |
|------|-------|-----------------|
| 1. Lexical / tree-sitter | none | Identifier tokens. High recall, low precision. Drives `workspace/symbol` and references-as-preview. |
| 2. Declared bindings | `bindings:` section | Hand-declared cross-language identity including string-literal config values and aliases across naming conventions. |
| 3. Schema-anchored | `schemas:` section | Auto-derived bindings: proto messages/enums/services/rpcs, openapi components + operationIds, jsonschema $defs + title. One config line ≈ dozens of bindings. |

Plus a **universal comment scanner** that runs on every walked file:

- `@see X`, `{@link X}` → soft (comment-confidence) reference.
- `@ref X`, `x-ref: X` → hard (declared-confidence) reference.

Generators that emit cross-language artifacts (e.g., gat in gwag emits `@ref` back-references in proto / GraphQL SDL / OpenAPI x-ref) get cross-language linkage for free with no per-framework parsing dialect on our side.

## Derivation-aware refactoring (`@derived`)

Tiers 1–3 answer *"are these the same symbol?"*. Generated code needs a stronger, **directional** relationship: a GraphQL field or a generated Go struct field isn't the *same as* its source — it's **derived from** it, and regenerates. Editing the derived copy is futile (codegen overwrites it); the edit belongs at the source. poly-lsp-mcp models this as `@derived` edges that **the generators emit** — it never guesses the mapping by replicating a naming rule. (A lexical name-match across namespaces — a Go string vs a GraphQL field, a column vs a PascalCase field — is a *guess*, and guesses are advisory only.)

**Two emitters today** (both monorepo forks):

- **gat** (gwag) emits `@derived(operationId: "x")` on every generated GraphQL SDL field — it derives from the Go huma operation with that `OperationID`.
- **the sqlc-metaquery fork** emits a `derived:"table.column"` struct tag on every generated Go field — it derives from that schema column.

**The SQL root is the migration-fold.** A column's source of truth isn't any single file — it's the cumulative result of the ordered migrations. `migrations.Fold` parses the DDL (CREATE/ALTER TABLE, CREATE/DROP VIEW) across the `*.up.sql` files in order and yields each column's current **defining site**.

**Consumers** turn the declared edges into authoritative bindings at index build:

- `ApplyDerived` reads SDL `@derived` → binds each field's Go `OperationID` source (declared confidence).
- `ApplyDerivedSQL` reads the `derived:` tags → folds the migrations → binds each column's defining migration site (declared).

Because the link is **declared, not guessed**, cross-namespace lexical matches stay advisory: `node_refactor` renames the authoritative sites and returns the lexical ones under `candidates` — *recommendations, not actions* — unless you opt in with `applyCandidates: true`.

### Renaming a derived name

Renaming a `@derived` **source** (a column, an operation) cascades by default: every source the name derives from, the derived name and every reference are renamed in one validated transaction. The rename never refuses. The reply's `cascade` field lists each source (`file:line`), the reference count, and a one-line note when a source is an outside contract: a renamed SQL column needs a database migration; a renamed GraphQL operation breaks external clients that call it by name.

For anything narrower (one source of several, a local alias), stage plain edits with `commit: false` and commit when the result is right. To undo a cascade, rename back.

## Diagnostics in edit responses (MCP)

After every `node_edit` / `node_delete` / `node_refactor`:

1. The edited file is sent through `didOpen`/`didChange`/`didSave` to the matching child LSP.
2. Per-URI `WaitAfter` blocks for up to 1500ms (default) for `publishDiagnostics`.
3. Sibling files in the same package that gain new diagnostics are rolled in by default.
4. Each diagnostic carries `text` (range source), `context` (configurable lines around it), `enclosingNode` (containing tree-sitter declaration with name + decl ranges), and `references` (node_references-shape hits when the range is an identifier).

The diagnostic store also feeds `poly-lsp-mcp://diagnostics` for workspace-wide health checks without an edit. A proactive workspace open at MCP initialize seeds the store so the resource is useful before the agent makes its first change.

## `node_refactor` — composable signature ops

Supports Go, TypeScript (.ts/.tsx/.js), and Python.

```jsonc
{
  "file": "lib.ts",
  "startLine": 1, "startCol": 17, "endLine": 1, "endCol": 22,
  "refactor": {
    "rename": "hello",                                       // workspace-wide
    "params": [                                              // rebuild signature
      {"name": "name", "type": "string"},
      {"name": "age",  "type": "number"}
    ],
    "return": "string"                                       // replace or insert
  }
}
```

- **rename**: workspace-wide, with declared-binding + aliasing safety. Also rewrites whole-word mentions in comments and prose by default (the reply lists the files and how to get a code-only rename); a plain lowercase-word old name (`save`, `data`) leaves prose alone and reports the skipped count. `includeComments: true|false` is a hidden override. Cross-namespace lexical guesses are returned as `candidates` (apply with `applyCandidates: true`); renaming a `@derived` source cascades to every source and reference — see [Derivation-aware refactoring](#derivation-aware-refactoring-derived).
- **params**: rebuilds the function declaration. When arity changes, callers across the workspace are rewritten best-effort — args truncated on shrink, padded with language-appropriate zero values on growth (`""`, `0`, `false`, `nil` / `null` / `None`, `[]` / `{}`, …). Spread / splat callers are reported as `skipped` so you decide.
- **return**: replaces the existing return type or inserts one into a previously-void declaration.

All three combine in one call. Diagnostic round-trip on every touched file.

## Stacked-branch parse cache

Phase-3 win for stacked-branch workflows:

- Parse results are content-addressed by `(language, sha256(content))` — switching back to a branch you were just on hits the cache for free.
- On MCP initialize, the upstream chain (`feature/c` → `feature/b` → `feature/a` → `main`) is walked asynchronously and every ancestor's files are pre-parsed so a switch *forward* to those branches is also free.
- LRU-bounded (5000 entries by default), persisted to `<root>/.poly-lsp-mcp/cache.gob` across MCP sessions.

Disable per-session via `Server.SetGitPrewarm(false)` if the up-front cost outweighs the later switch-time saving (very large stacks, ephemeral CI workloads).

## Testing

Convenience targets in the `Makefile`:

```sh
make test          # short suite — fast, skips live-LSP / live-gat e2e
make test-all      # full suite (needs gopls + git on PATH)
make test-race     # short suite + race detector (per-PR gate)
make test-race-all # full suite + race detector (pre-release)
make check         # vet + test + test-race
make smoke-editor  # real-binary LSP conformance smoke
make smoke-llm     # live LLM end-to-end smoke
```

The race detector is the standing concurrency gate — exercises the
`DiagnosticStore` ↔ child-LSP-readloop interactions, parse cache
under concurrent reads/writes, and the manager spawn/restart
goroutines. Clean under three consecutive `make test-race-all`
runs as of the most recent commit on `main`.

## Library usage

The same packages the standalone binary uses are importable. See
`examples/embed/main.go` for a small program that builds an MCP
server in its own process.

```go
import (
    "github.com/iodesystems/poly-lsp-mcp/config"
    "github.com/iodesystems/poly-lsp-mcp/mcp"
    "github.com/iodesystems/poly-lsp-mcp/multiplex"
)

cfg, _, _ := config.LoadOrDefault("poly-lsp-mcp.yaml")
reg, _ := cfg.Build()
srv := mcp.New(reg, "/path/to/workspace", cfg.Bindings, cfg.Schemas)
srv.SetManager(multiplex.NewManager(reg))
srv.Serve(os.Stdin, os.Stdout)
```

Public packages (stable):

| Package | What's in it |
|---|---|
| `config` | Language registry, YAML loader, `auto_schemas` detect. |
| `mcp` | MCP server, tools, resources. |
| `server` | LSP server (multiplex + index fallback). |
| `multiplex` | Child LSP supervisor + `DiagnosticStore`. |
| `symbols` | Cross-language index, parse cache, tree-sitter extractors, refactor primitives (`FindFunctionSignature`, `RewriteSignature`, `FindCallSites`, `PrewarmFromBranch`). |

Internal-only (subject to change without notice):

- `internal/bindings` — declared-binding resolver + schema dialects (used by `server` and `mcp` directly).
- `internal/git` — `git` binary wrapper (used by `symbols.PrewarmFromBranch` and `mcp`'s prewarm).
- `internal/jsonrpc` — JSON-RPC 2.0 framing.

## Layout

```
main.go                      entry, subcommand dispatch
config/                      language registry, YAML loader, schema
                             auto-detect (PUBLIC)
mcp/                         MCP server + tools + resources (PUBLIC)
server/                      LSP server (PUBLIC)
multiplex/                   child LSP supervisor + diagnostic store
                             (PUBLIC)
symbols/                     index, tree-sitter extractors, lexical
                             fallback, parse cache, comment scanner,
                             refactor primitives, branch prewarm
                             (PUBLIC)
internal/jsonrpc/            JSON-RPC framing
internal/bindings/           declared bindings (Tier 2) + schema
                             dialects (Tier 3) + @derived consumers
                             (gat / sqlc → declared sources)
internal/migrations/         *.up.sql migration-fold → cumulative
                             schema (the SQL derivation root)
internal/git/                git binary wrapper
testdata/fixtures/polyglot/  multi-language fixture
testdata/fixtures/gat-greeter/
                             live gat → poly-lsp-mcp @ref fixture +
                             cross-language diagnostic Go server
examples/embed/              library-mode example
plan/plan.md                 phased roadmap (all phases shipped)
```

## Status

Phase 0 through Phase 5 plus the stacked-branch tail of Phase 3 are all shipped. On top of that, the **derivation model** (`@derived` emit→consume→cascading refactor, with the SQL migration-fold) is complete. The roadmap is effectively complete; further work is scope expansion (new refactor kinds, more `@derived` emitters/modes, more language coverage) and ergonomics.

See `plan/plan.md` for the full feature history with rationale for each design decision.
