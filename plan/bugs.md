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

**Also fixed in the same pass** (USER, 2026-08-01): a java field's decl span
started at the DECLARATOR (`name`, not `String name;`), because java spells its
declarator `variable_declarator` and the C single-declarator count ignores that
type. `countJavaDeclarators` applies the same rule, so a java field now spans
the whole `field_declaration` — modifiers and annotations included
(`@Inject String name; // why`). `int a, b;` keeps per-declarator ranges.
Re-measured: 0 sibling span overlaps over 25,369 files / 2.24M symbols.

## Doc comments dropped in typescript exports and sql statements

**Reported:** 2026-08-01, by `TestLanguageBugMatrix` on its first run
**Status:** ✅ both fixed 2026-08-01

Both are the class above ("a declaration owns its documentation"), in the
UPWARD direction, in languages the hand-written table never covered.

**typescript — fixed.** `declRangeNode` did not rise to `export_statement`, so

    // doc for f
    export function f() {}

gave `f` the span `function f() {}`: the doc comment outside it, and the
`export` keyword too. Exported symbols are the ones a caller cares about, so
this was most of a TS codebase reading back undocumented, and node_edit
replacing the function stranded the comment above it — exactly what
`declLineCols` was written to prevent in go.

The wrappers NEST, which is what made this more than a one-line fix:
`export const x = 1` is a `variable_declarator` inside a `lexical_declaration`
inside an `export_statement`. Rising one level fixes `export function` and
leaves `export const` broken — with the class showing green, since the first
fixture only covered functions. So `docCommentAnchor` now LOOPS, `declRangeNode`
handles both wrappers, and the matrix fixture covers `export function`,
`export const`, `export class` and a bare `const` (which also had no doc
comment before: nothing ever rose from a declarator to its `lexical_declaration`).
`const a = 1, b = 2` keeps per-declarator ranges — one comment cannot document
two symbols, and the spans would overlap.

Measured: 0 sibling overlaps over 25,369 files / 2.24M symbols, and TS symbol
counts identical with and without the change (812 files / 21,857 symbols on a
real frontend), so the wider spans lose nothing.

**sql — fixed.** `-- doc` above a `CREATE TABLE` was dropped while
column-level TRAILING comments worked (`a int, -- doc`), which is what pinned
it to the block-above rule. TWO independent causes sat behind that one
symptom:

- tree-sitter-sql calls a `--` line a `comment` but a `/* … */` block
  `marginalia`. `isCommentNode` knew only the first, so a sql BLOCK comment
  was not treated as a comment anywhere in the codebase — not as a doc, not as
  a trailing comment, not by `:contains`.
- Every CREATE is wrapped in a `statement` node, and the doc comment is the
  WRAPPER's sibling — two levels up from the symbol, which is the
  `create_table`. `isStatementWrapper` rises through it, guarded on the
  wrapper having exactly one named child so no other grammar that happens to
  name a node `statement` can be caught by it.

Verified: both comment styles and `CREATE FUNCTION`, matrix green with no
KNOWN entries left, 0 sibling overlaps over 25,376 files / 2.24M symbols.

**The `;` is absorbed too** (USER, 2026-08-01). tree-sitter-sql makes the
semicolon a sibling of the statement rather than part of it, so a statement's
span stopped short of its own terminator and deleting a table left a bare `;`.
`terminatorEnd` takes it. No line check is needed: the `;` is the IMMEDIATE
sibling, so nothing sits between it and the statement even when written on its
own line, and two statements on one line stay disjoint (verified).

**A regression that fix exposed, shipped in `ef89c6c` and live until now:** a
child could climb OUT of the construct containing it and claim an ancestor's
trailing comment. In

    CREATE TABLE t (a int); -- why

the column `a int` stepped over `)`, out of its column list, and read back as
`a int); -- why` — so node_read on the column returned garbage and node_edit
on it would have destroyed the `)` and the `;`. Cause: the wrapper-rise added
for python (climb to a parent that ends exactly where this node does) composed
with the punctuation-stepping added for typescript's `;`, and nothing said
which punctuation was safe to cross. `isDeclPunctuation` now allows only `;`
and `,` — a terminator or separator of the declaration itself — never a
closing bracket.

The sweep did NOT catch this: it compares SIBLINGS, and the column stayed
inside its parent's span because the parent had legitimately grown to cover
the same comment. Pinned instead by a fixture in the trailing-comment class.

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

## `<file>@<line>` addresses silently widened to the WHOLE FILE

**Reported:** 2026-08-03 (session review — dun session `20260803-162851`,
135 `node_*` calls)
**Status:** ✅ fixed 2026-08-03 — `modernNode.wholeFile()`, pinned by
`TestRefSiteAddrReadsTheAddressedLine`, `TestRefSiteAddrReadsASpan`,
`TestRefSiteAddrRejectsLineWindowArgs`, `TestRefSiteAddrEditIsScopedToThatLine`,
`TestRefSiteAddrDeleteEmptiesTheLineNotTheFile`, and
`TestWholeFileAddrStillBrowsesAndDeletes` (the over-narrowing guard). All five
ref-site tests fail against the old predicate; the delete one fails with
`main.go: no such file or directory`.

**Repro** (as observed, twice, in one session):

    node_query "path=cmd/dun/main.go ::grep('compaction')"
    → matches[0].node = "cmd/dun/main.go@377"
    node_read node="cmd/dun/main.go@377"
    → lines 1-71.        # no error, no note

**Cause.** `resolveRefSiteAddr` returns a node with `class:"ref"` and a real
`decl` span but no dotted `sym`. Four call sites used `rn.sym == ""` as the
test for "this address means the whole file", so every ref-site op widened:

- `modern.go` node_read — returned page 1 instead of the addressed line.
- `modern.go` `nodeCurrentText` — an `oldText` edit matched the first
  occurrence anywhere in the FILE, not on that line.
- `modern.go` node_edit `delete:true` — reached `applyWholeFileDelete` and
  **removed the file**. The one silent data-loss path.

The rename branch already got this right (it tests `rn.class == "ref"`
explicitly, with a comment recording the same confusion found once before) —
the other four were never converted, and no test covered the address form at
all.

**Why it mattered more than the shape suggests.** This is not an obscure
address: `::grep`, edge and `::signature`/`::body` rows all report it, and the
node_query description tells the caller to feed `matches[].node` straight into
node_read/node_edit. The documented happy path was the broken one.

**Fix.** `func (n *modernNode) wholeFile() bool { return n.sym == "" &&
n.class != "ref" }` at all four sites. A ref-site `delete:true` now empties the
line in place (the newline is outside the span) — same splice as every other
span delete, pinned so the blank-line result is documented rather than
accidental.

**Open, not fixed:** whether `delete:true` on a single-line `@N` should also
consume the newline and close the line up. Defensible either way (a
`@start-end` span from `::body` clearly should not), so it is left as-is —
`icebox.md`.

## Read hint pointed at a tool the modern surface does not have

**Reported:** 2026-08-03 (same session review — 21 truncated reads, the
"avoid paging chunk-by-chunk" hint followed **0 times**)
**Status:** ✅ fixed 2026-08-03 — `targetedSearchAdvice`, pinned by
`TestReadHintNamesASearchThisSurfaceHas`

**Cause.** `buildReadPayload` is shared by both tool surfaces, and its hint
named the CLASSIC `search` tool (`pattern=<regex>`) unconditionally. On the
default three-tool surface there is no such tool, so the hint's two pieces of
advice landed as: one impossible (search), one expensive (`lineLimit=<whole
file>`), one actionable (`startLine=N`). The caller took the actionable one,
every time — which is exactly the paging the hint exists to prevent.

**Fix.** The advice clause is now a parameter, and the modern form is spelled
as a runnable call over the file in hand —
`node_query(selector: "path=<file> ::grep('-E <regex>')")` — because the
observed failure was not knowing a search existed but not knowing how to
address one file with it.

## No way to ADD a symbol to an existing file

**Reported:** 2026-08-03 (same session — the model passed a 93 KB whole-file
`newText` to insert one function)
**Status:** ✅ error/description fixed 2026-08-03, pinned by
`TestMissingSymbolErrorTeachesHowToAddOne`. A real insert op is NOT built —
see `icebox.md`.

**Cause.** `node_edit`'s description said *"newText alone — CREATE the node;
only where the address resolves to nothing yet"*, which reads as though
`file.go#NewFunc` creates a symbol. It does not: `resolveClassicAddr` returns
a hard `no symbol %q in %s; did you mean: …` error, and that did-you-mean list
answers only the TYPO reading of the address. A caller that meant to ADD had
nowhere to go, and fell back to rewriting the whole file.

**Fix.** The description no longer over-promises (newText alone creates a
FILE), and the missing-symbol error now answers both readings: the typo list,
plus the idiom that works — address a NEIGHBOUR, `oldText`=its whole text,
`newText`=that + the new declaration. O(neighbour), not O(file).

**Cost note.** The node_edit description is budgeted
(`TestModernToolSurfaceTokenBudget`, 1160 tok). The new clause was paid for by
deleting two redundant fragments, not by raising the budget: the
`(type/func/method/var/field)` enumeration, and `(the right tool for ANY
rename)` — which repeats the sentence directly above it. Every directive is
intact.

## Every name in a grouped `var (…)` block was missing from the index

**Reported:** 2026-08-03 (session review — dun session `20260803-172451`;
the same hunt had already burned ~11 calls in `20260803-162851`)
**Status:** ✅ fixed 2026-08-03 — `var_spec_list` added to `classifyGo`'s
container case, plus the single-spec range rule in `declRangeNode`. Pinned by
`TestGroupedDeclarationsIndexEveryName`,
`TestGroupedVarSpecsKeepPerSpecRanges`,
`TestSingleSpecGroupedVarTakesTheWholeDeclaration`.

**Repro** (minimal, `symbols.FileSymbols("go", …)`):

    var ( a = 1; b = 2 )   → nothing indexed
    var c = 3              → var c        ✅
    const ( d = 4 )        → const d      ✅

In the workspace: `symbols/embedded_graphql.go` declares `gqlTemplateTags`
and `gqlEmbedIdentRe` at lines 27-30. Neither appears in
`path=symbols/embedded_graphql.go *` — not as `var`, not as anything. Same for
`symbols/markdown.go`, `symbols/comments.go`. In dun, all 8 lipgloss style
vars (`cmd/dun/tui.go:103-111`) were unreachable.

**Cause.** Of Go's four grouped declaration forms, tree-sitter parses exactly
ONE through a list node:

    import_declaration > import_spec_list > import_spec    ← handled
    var_declaration    > var_spec_list    > var_spec       ← NOT handled
    const_declaration  > const_spec                        (no wrapper, even grouped)
    type_declaration   > type_spec                         (no wrapper, even grouped)

`classifyGo` listed `import_spec_list` but not `var_spec_list`, so
`var_spec_list` fell to `roleSkip` and `gather()` dropped the entire subtree.
That asymmetry is why const looked fine and var did not — and why a
var-only test would have been the wrong test.

**Why it cost so much.** The failure is INVISIBLE, not wrong: the names have
no node, so every correct query returns empty and every hint says "the tag is
what emptied it". Two sessions ran the full ladder — `var`, `var#stSel`,
`var[name*=stSel]`, `^var st` greps — and the model escaped only by a
`:not(...)` chain that surfaced unrelated *fields*. ~22 calls, no answer.

**Fix.** `var_spec_list` is a container. `declRangeNode` gained a matching
arm: the single-spec rule (a one-spec group takes the whole declaration,
keyword and parens included) has to reach one level further up for var,
otherwise `var ( x = 1 )` would read back as `x = 1` while the identical
`const ( x = 1 )` reads back whole — and deleting the spec would strand an
empty `var ()`. Multi-spec groups keep per-spec ranges so siblings don't
overlap.

**Negative control:** with the fix reverted, all three tests fail, and the
message shows the exact shape — `missing "alpha"; have [p:module fmt:import
os:import gamma:const delta:const Eps:type Zeta:type solo:var]`. The `const`
subtest passes either way, which is what isolates the bug to var.

## Nothing ever rebuilt poly-lsp-mcp, so every fix shipped late

**Reported:** 2026-08-03 (found while confirming the three fixes above were
NOT live in the binary that served the sessions that revealed them)
**Status:** ✅ fixed 2026-08-03 — `selfupdate.go` (ported from dun's
`cmd/dun/selfupdate.go`), `-X main.srcDir` stamped by `make build|install`.
Pinned by `TestBuildInputIsGoSourceAndModuleFilesOnly`,
`TestSourceNewerThanSpotsAnEditInAnyPackage`,
`TestSourceNewerThanPruneList`, `TestSelfUpdateIsANoOpWithoutASourceStamp`,
`TestRebuildSelfLeavesTheBinaryIntactOnFailure`.

**Symptom.** poly-lsp-mcp is always SPAWNED, never launched by hand: dun runs
`poly-lsp-mcp mcp --root <ws>` off PATH (`dun harness.go:156`), editors spawn
the LSP. dun self-updates; it does not rebuild the servers it spawns. So there
was no point in the loop where anyone noticed the binary was old. Measured
state when this was found:

    pid 1071643  root=dun      ~/go/bin/poly-lsp-mcp (Aug 3 16:43)
    pid  438950  root=raglit   ~/go/bin/poly-lsp-mcp (DELETED)
    pid  523516  root=raglit   —
    pid  488908  root=.        bin/poly-lsp-mcp-linux-amd64 (Jul 17)

438950 was executing a binary that no longer existed on disk — it had survived
an earlier `go install`. 1071643 is the process that served both sessions in
which the `<file>@<line>` bug was hunted, running a build that predated the
fix.

**Fix.** dun's design, ported: `srcDir` stamped only by `make build|install`
(a plain `go install .` stays a release build, now spelled
`make install-release`), rebuild when any `.go`/`go.mod`/`go.sum` under the
tree is newer than the binary, then `syscall.Exec` the fresh one.
`POLY_LSP_AUTOBUILD_DONE` breaks the loop, `POLY_LSP_NO_AUTOBUILD=1` disables.

Three things the port does NOT copy from dun, because this binary's job is
different:

- **Temp + atomic rename** instead of `go build -o <exe>` in place. Several
  clients (dun's harness, an editor, a shell) can start at once against the
  same stale tree; concurrent builds to one path interleave into a corrupt
  binary. Also dodges ETXTBSY when a copy is already running.
- **A shorter prune list.** A missed directory is a missed rebuild — the exact
  failure being fixed — so only trees that cannot reach `go build .` are
  skipped (`bin`, `testdata` and `bench` are separate modules/fixtures, `plan`
  and `scripts` are not Go). `examples/` is walked despite being unreachable
  from main: a wasted build is a cheap false positive, a skipped one is the bug.
- **No asset extensions in `buildInput`.** This repo has no `go:embed`, and
  `poly-lsp-mcp.yaml` is READ at runtime — counting it would rebuild on every
  config tweak and change nothing.

**Verified end to end**, not just by unit test: a stamped binary with its mtime
set to 2020 rebuilt, re-execed, and answered the query correctly; a second run
did not rebuild (no loop); and in `mcp` mode with a stale binary, stdout
carried nothing but the JSON-RPC `initialize` response while "source changed —
rebuilding…" went to stderr — the stdio pipe survives `syscall.Exec`, so the
client never sees the swap.

## A conflict crossing a function boundary produced a declaration from nowhere

**Reported:** 2026-08-04 (USER asked what happens to "a messy diff, one that
crosses two functions but doesn't complete them", then "and a second diff in
one of the same functions")
**Status:** ✅ fixed 2026-08-04 across four commits — refuse to write it
(`e95d005`), reconstruct the sides (`33207b2`), warn on query and read
(`500b0df`), withhold it from results (`7642cd1`).

**Repro.** A conflict opening inside `A` and closing inside `B`:

    func A() int {
        x := 1
    <<<<<<< HEAD
        return x
    }

    func B() int {
        z := 3
    =======
    …

`path=m.go func` reported `func A 3-7`, `func B[1] 9-13`, `func B[2] 15-24`,
and `node_read("m.go#B[1]")` handed back, with no warning:

    func B() int {
        z := 3
    =======
        return x + 1
    }

**Cause.** tree-sitter is error-TOLERANT: it recovers past the markers and
builds declarations out of BOTH sides at once. `symbols.FileSymbols` therefore
returns a symbol list for input that is not valid source in any commit — which
is why `ParsesCleanly` had to be added, since "symbols came back" was never
evidence the file parsed.

**Two consequences worse than the phantom itself.** Editing `B[1]` would have
written ACROSS the markers and corrupted the merge — there is no correct
oldText for a span that is half of each side. And ordinal disambiguation
counts what the index holds, so the phantom claimed `B[1]` and pushed the
REAL function to `B[2]`: any address written down before the merge silently
pointed elsewhere for its duration.

**Fix.** Writes that straddle a marker are refused (containing one is fine —
that is what `accept:` operates on). Straddling rows are withheld from query
results, with the count and the file named, which also recomputes the ordinals
over real declarations only. Each side is reconstructed WHOLE-FILE — a side
alone is a fragment, the file with that side is what git writes on
`--ours`/`--theirs` and parses — and when neither reconstructs, the region is
rendered as diffed TEXT rather than a structural answer that would be invented.

**Verified** on the shape that prompted it (two conflicts, one crossing a
boundary and one wholly inside a function) and on a real `git merge`.
