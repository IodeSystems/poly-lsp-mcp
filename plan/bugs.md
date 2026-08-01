# poly-lsp-mcp — bugs

Known defects. Format: one entry per bug, with repro, impact, and status.

## A trailing comment was credited to the declaration BELOW it

**Reported:** 2026-08-01 (dogfooding, from a `node_edit` toolcall)
**Status:** ✅ fixed 2026-08-01 — `isTrailingComment` +
`trailingCommentSpan`, pinned by `TestTrailingCommentBelongsToTheLineItSitsOn`,
`TestTrailingCommentIsTheDeclsOwnDoc`, `TestOwnLineCommentIsStillADocComment`,
`TestSiblingSpansDoNotOverlap`, and two MCP-level tests

**Repro** (as reported): edit a struct field, writing the line as it appears
on screen.

    node_edit node=harness.go#Config.EnableShip
      oldText="EnableShip  bool  // add the ship tool (opt-in: …)"
    → ERROR: oldText not found in harness.go#Config.EnableShip …
      which is:
      ---
      EnableShip  bool
      ---

**Cause.** Both span rules asked only whether a comment ENDED on the line
directly above a declaration — `declLineCols` extending the decl span upward,
`docCommentSpan` filling `::comment`. A comment trailing the PREVIOUS
declaration ends on exactly that line, so it satisfied the test. Neither rule
asked whether the comment began its line.

That single omission produced two different symptoms depending on whether the
grammar puts a node between declarations:

- **go, java** — decl spans were right (an anonymous newline terminator
  happens to sit between declarations and breaks the scan), but `::comment` on
  a field returned the comment belonging to the field ABOVE it.
- **typescript, kotlin, c** — nothing intervenes, so the neighbour's comment
  was pulled INTO the next declaration's span. `node_read Config.enableShip`
  answered `"// the harness name\n  enableShip = false"`, and deleting the
  field wrote:

      name: string = ""; ; // add the ship tool

  — `name`'s comment destroyed, `enableShip`'s own comment stranded where it
  now falsely documents `name`, plus a stray `;`.
- **python** — clean throughout; its comment is a sibling of the wrapping
  statement, which the scan never reached.

**Fix**, in two halves. `isTrailingComment` rejects a comment that has code
before it on its line, so no declaration inherits its neighbour's; and
`trailingCommentSpan` gives the comment to the declaration it actually trails,
extending both the decl span and `::comment`. The second half is a BEHAVIOR
CHANGE (USER's call 2026-08-01): a field node's text now carries its own
comment, which is what makes the reported edit apply, and is the existing
"a declaration OWNS its doc comment" rule pointed the other way — in go a
field's trailing comment IS its godoc.

**Two traps, both found by measurement rather than reasoning:**
- Go's grammar puts an anonymous newline terminator between declarations that
  ENDS at column 0 of the comment's own row, so a naive "previous sibling ends
  on this row" test marks every doc comment in the language as trailing. The
  column check is what makes the predicate sound.
- Its mirror: c's `preproc_def` swallows its terminating newline, so
  `#define A 1` reports an end at column 1 of the NEXT line and claimed the
  comment trailing the `#define` below it. A sweep of 22,858 files / 2.2M
  symbols showed **10,758 sibling span overlaps** against a baseline of 0
  before the guard was added, and 0 after. That sweep is now
  `TestSiblingSpansDoNotOverlap`, run against this repo by default and
  widenable with `SWEEP_ROOT`.

**Verified by negative control**: with both helpers stubbed out, the new tests
fail with the reported symptoms — `C.B doc comment on line 4, want 5` and
`typescript C.b: decl spans lines 2..3 … reached across into a neighbour`.

**Not fixed, recorded so it is not re-investigated:** a java field's decl span
still starts at the DECLARATOR (`name; // why`, not `String name; // why`).
That is pre-existing `declRangeNode` behaviour — java's declarators are
`variable_declarator` nodes, which its single-declarator count deliberately
ignores — and is independent of comments.

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
