# poly-lsp-mcp — bugs

Known defects. Format: one entry per bug, with repro, impact, and status.

## Stale diagnostics after an OUT-OF-BAND file change

**Reported:** 2026-07-31
**Status:** ✅ fixed 2026-07-31 — `notifyChildOfExternalChange`, pinned by
`TestOutOfBandRewriteDoesNotStrandTheChildLSP`

A file rewritten by anything OTHER than `node_edit` — `git rebase` /
`checkout` / `merge`, a formatter, another agent, the user's editor — left the
child LSP answering from the pre-change content indefinitely. Files that WERE
edited through the tool were then type-checked against a version of their
neighbours that no longer existed on disk.

**Repro** (the recorded one: `~/.dun/sessions/…-dun/20260731-000841.jsonl`):
1. A dun session opens worktree `dun-worktree-2310294701`, branched from
   `a3f176f`, where `systemFor` takes ONE parameter. Proactive open sends
   `didOpen` for `harness.go` at that content.
2. t=…2649: the session runs `git rebase origin/main` through its **exec**
   tool. The rebase replays onto `0faeab9`, which makes `systemFor` take three
   parameters — rewriting `harness.go` and its call site in
   `servers_runtime.go` **on disk**. (`stat -c %Y harness.go` in that session
   returns exactly `1785482649`, the rebase second.)
3. t=…2956 onward: every `node_edit` on `servers_runtime.go` returns
   `too many arguments in call to systemFor — have 3, want 1`.
4. `go build ./...` passes throughout. So do `node_read` and `node_query`,
   which report the correct three-parameter signature the whole time.

**Cause.** Two things true at once:
- `runProactiveOpen` `didOpen`s every routable file at startup and nothing
  ever `didClose`s them. Per LSP the client's copy is authoritative for an
  OPEN document, so the child stops consulting disk for it.
- Nothing told the child it had changed. No `workspace/didChangeWatchedFiles`
  was registered (client capabilities were `{}`), gopls does not watch the
  filesystem itself, and the fsnotify watcher that does exist ended at
  `refreshFileInIndex` — it fed the symbol INDEX and never touched
  `s.manager`.

That split is why the evidence read as contradictory: the index half was
current and the LSP half was stale, and different code paths populate them.

**What ruled out the alternatives**, both from the session's own payloads:
- `diagnosticsTimedOut: false` on all four stale responses — so this was not
  `WaitAfter` re-serving `s.Get(uri)` past its deadline.
- One response carries `servers_runtime.go:259 too many arguments` (stale)
  AND `servers_runtime.go:281 cannot use "wrong" as []mcpmgr.MCPTool` (a
  correct error introduced seconds earlier). The child had freshly
  type-checked the current file against a stale neighbour — so the staleness
  was in the child's overlay, not in our `DiagnosticStore`.

**Not the cause**, recorded because two earlier writeups asserted it: the
signature change did NOT arrive via `node_edit` in that session. All 25 of its
`node_edit` calls are accounted for (blob-backed ones included) and only three
touch `harness.go` — one `#Config` edit BEFORE the rebase, two comment-only
`#systemFor` edits after. None changed a signature. The change was authored by
`node_edit` in a DIFFERENT session (`20260730-231740.jsonl`, t=…9320), against
a different worktree, a different poly-lsp process and a different gopls; that
instance was correct throughout. Nor is `node_edit`'s atomic temp-file+rename
implicated — `node_edit` is the one path that always kept the child current.
And `node_read` returning the right signature was never evidence about the
LSP: it reads disk.

**Impact while open:** false-positive diagnostics that never self-heal until
something `node_edit`s the stale file, on the one surface whose job is to be
more trustworthy than grep. dun's `ship` tool runs `git rebase`, so a shipping
session hit this every time.

**Fix.** `watchRefreshFile` now pushes the bytes it just indexed at the
matching child (`notifyChildOfExternalChange`); a watched delete sends
`didClose`. `didChange` rather than `didChangeWatchedFiles`: it works whether
or not the URI is open, needs no capability negotiation, and reuses the path
an edit already takes. A SHA-256 of the last text pushed per URI (`sentDocs`)
drops the ~200ms fsnotify echo of the tool's own writes.

**Verified by negative control**, not only by a green test: with the single
notify call commented out, `TestOutOfBandRewriteDoesNotStrandTheChildLSP`
fails with `too many arguments in call to systemFor / have (nil, string,
number) / want ([]string)` — the dogfood error, reproduced on demand.
