# Design: why poly-lsp-mcp is the way it is

This file records decisions and the reasons for them. Work items are aw4
issues in project `poly-lsp-mcp`, not entries here. The MCP tool schemas
(`mcp/modern.go`) are the source of truth for the tool surface; this file
does not repeat them.

## Position

The target: a **live, no-build, multi-language, reference-aware,
edit-capable code querier with a predictable, explainable cost model**,
driven by an LLM agent in the middle of a task.

- **The consumer is a model, not a security engineer.** A grammar the model
  already knows (CSS selectors). Partial answers say so (`conf` labels, a
  budget blow that says it stopped, `:explain`, `>N` floors). This prevents
  hallucination; a person supplies their own doubt, a model does not.
  Token-lean, composable output over rich reports.
- **The unoccupied intersection:** {live + no-build} x {cross-file
  reference edges with a precision ladder} x {mutation} x {predictable
  cost}. CodeQL / Glean / Kythe / Sourcegraph buy the reference graph with a
  build or batch extractor per language and cannot rewrite.
  ast-grep / Comby / Semgrep rewrite live but have no cross-file graph.
- **The precision ladder:** lexical -> tree-sitter-scoped -> LSP-resolved,
  each row labelled (`conf: lsp | lexical | unsettled`). `lexical` means the
  name is unique in the workspace (certain without an LSP); `unsettled`
  means two or more same-named declarations and no LSP settled it (a guess,
  with candidates).

### Non-goals

- Dataflow / taint / points-to. That needs the build and batch we refuse.
- Out-scaling Sourcegraph / Glean on multi-repo. A single owned root is home.
  The daemon serving many roots is process sharing, not a scope change.
- Indexing the whole host filesystem; we index inside the git root.
- Replacing any child LSP. We multiplex.
- Sandboxing child LSPs. They run as the user.
- Windows, until someone asks.

### Ownership domains

The precision pass crosses out of the workspace by definition (gopls into
the stdlib, tsserver into `node_modules`, pylsp into `site-packages`). A far
end outside the root is an **external stub** (`module@version#sym`,
`domain: external`, read-only, `[not indexed]`), never a false local match.

- **Decided (Carl):** crossing into libraries is opt-in; the default stays
  workspace-bounded. Revisit only if adoption data shows agents want the
  cross-lib answer unprompted.
- **Rule:** nothing new may hard-code the single-root assumption deeper.
  `treeNode.domain` (`""` = owned, `"external"`) is the axis that will gate
  mutation and budget.

## Design rule: fields, not prose

Measured on the volcycle bench (2026-08-04/05; a real agent, a real Android
app): a read hint that said, in first position, "re-read with lineLimit=N"
was ignored **44 of 44** times. Adding `enclosingSymbol` as a **field** (no
prose) took paging hints 44 -> 0 and `lineLimit` use 0 -> 23. Four attempts
to teach through description prose failed (the space rule, an llm-bench
A/B, `:root > *`, the read hint).

So: hand back an address the caller can paste, not a sentence telling it
what to do. The same applies inside a payload: Go serializes map keys in
order, so an `action` field that sorts before `diagnostics` is read, while
the same text after 1,500 characters of diagnostics is not
(`incompleteEditAction`, `mcp/validate.go`).

Corollary: **do not rewrite `modern.go`'s description to improve
adoption.** Across two bench sets x three description variants, the
spec-first description already saturates reach and graph-first on neutral
ground; the most verbose variant did worse (more grep scaffolding).

## Selector language

- **A space is always a node boundary** (2026-08-03, breaking, `656114e`).
  `func[path=a.go]` filters; `func path=a.go` descends; a bare attribute is
  its own `*[...]`. The earlier "bare attribute attaches" exception lived
  only in the `?` help, which 2 of 426 measured calls asked for. Zero real
  selectors used the old form after the change.
- **`|` is an operator only before another test** (2026-08-04, Carl).
  Measured: 0 real boolean ORs against 19 regex alternations.
- References are edge pseudo-elements (`::in` / `::out`) with two orthogonal
  class axes, kind (`.call .type .import .implements`) x position
  (`.return .param .field .var`). `.implements` is LSP-native (Go structural
  typing has no lexical clause), so without a child LSP it is reported
  UNAVAILABLE, not empty.
- A workspace path is an id, so `node_query` accepts every address it
  prints. Absolute paths are refused: addresses are workspace-relative.
- `:recursive` is direct self-recursion confirmed by a child LSP. The
  lexical rung was tried and reverted (2026-08-04): a workspace-unique name
  says nothing about external declarations, and the first false positive was
  `func Write` calling `w.Write` (io.Writer's).
- Go keeps pointer/slice decoration on return segments (`*Config`,
  `[]Schema`) while java/kotlin/typescript/c strip it (Carl, 2026-07-27;
  documented on `goTypeSegment`).
- **Swift extensions have no node of their own; their members join the
  type** (2026-10-27, `symbols/swiftsymbols.go` `swiftExtensions`). An
  extension of a type declared in the same file is merged into it:
  `Greeter.wave` is one path whether `wave` is in the class body or an
  extension, and an overload split across the two reads `greet[1]`,
  `greet[2]` instead of two colliding `Greeter.greet`. An extension of a type
  declared elsewhere (`extension String`, `Greeter+Networking.swift`) files
  its members under the extended type, as a Go method is filed under its
  receiver; several such extensions in one file share one level, so their
  overloads disambiguate the same way. Cost: an extension block (with its
  conformance list and `where` clause) cannot be read or deleted as one
  node; its members can.
- Swift `init`, `deinit` and `subscript` are named by their keyword
  (`Greeter.init[2]`), the way Swift code spells them; their name range is
  the declaration, so rename does not pin on a keyword. `deinit` is class
  `ctor` (C++ files its destructors the same way).

## Budgets

- Query `budget` carries a unit: `Nops` (deterministic work units) or `Nms`
  / bare number (wall clock). The omitted default is **10000ms** (Carl). A
  time-tripped result labels itself non-deterministic; `Nops` gives a
  reproducible cut. Both are capped (5M ops / 30 s).
- Index `--index-budget` defaults to `30000ms`. Over budget the build fails
  with a trace by directory and two remedies: get under it (`--exclude`,
  `.gitignore`) or raise it (a scaled suggestion). Tools refuse with the
  same text.
- Zero-result hints scale their probe allowance on the corpus
  (`50ms + 0.45ms/file`, cap 400 ms, `hintProbeBudget`): a flat 10 ms
  silenced every symbol probe on a cold corpus, a flat 50 ms fired on 1 run
  in 6. A hint that appears at random cannot be relied on.

## Indexing

- **git decides the file set.** Tracked files plus untracked-but-not-ignored
  ones (an agent's new file still indexes). Ignored, never untracked, is the
  filter: on `zdx-go` honouring `.gitignore` took 2,128,213 sites to
  471,029 (-78%).
- **Decided (Carl, 2026-10-24): `node_query` does not apply `.gitignore`.**
  Usage decides; skip dirs (`symbols/symbols.go` `skipDirs`) are reachable
  when a selector names them, and `include` patterns open any dead end.
- Edge cases are decided by heuristics and each is reported with how to
  refine or revert (Carl): not-a-project roots (home, `/`, a dir of clones)
  are refused before any build (`--index-anyway`); over 1 MiB, `*.min.*`,
  LFS pointers and skipped dirs are left out per file; symlinks inside the
  root are followed after plain files (dedupe by real path), bounded by
  `--symlink-depth` (4).
- iOS trees: `Pods`, `Carthage`, `.build`, `.swiftpm`, `DerivedData`,
  `xcuserdata` and `*.xcassets` are skip dirs (vendored, generated, per-user,
  or one `Contents.json` per image). `.xcodeproj` contents and
  `.storyboard` / `.xib` have no registered extension and are not indexed;
  linking a storyboard's `customClass` / outlets to Swift is an aw4 issue
  (the XML extractor would read every `key=` attribute as a name).
- Shadowing: a lesser-typed file beside a stronger one (foo.js/foo.ts,
  foo.py/foo.pyi) is out of the index and broad queries, reachable by path.
- Markdown is structural: `section` nodes own their body; only headings,
  fenced code and inline code spans enter the index (prose out: 34,093 ->
  4,979 sites). Inline backtick spans stay in the index (dropping them breaks
  prose rename).
- The parse cache is content-keyed and shared, so identical bytes across
  worktrees parse once. A worktree index COW overlay was measured and not
  built (warm worktree build 93 ms vs 355 ms of gopls warmth that cannot be
  shared).

## Editing

- Rename tries the child LSP first (`textDocument/rename`, type-scoped).
  A server that serves the file but refuses returns an error and does not
  fall back to the lexical path. Lexical rename is blocked when the name is
  declared in more than one package with no authoritative coupling
  (`lexicalRenameCollision`): it once returned `filesChanged:15` and renamed
  an unrelated package.
- Rename rewrites comment and prose mentions by default; plain lowercase-word
  names are exempt. A derived rename (sqlc column, gat operation) always
  cascades and names the contracts it breaks (a DB column needs a migration,
  a GraphQL operation breaks clients). Narrower edits are a `commit:false`
  transaction.
- `lostWrites`: an edit that removes the last assignment to a name that is
  still read comes back with an `alert`. It reports, never refuses (removing
  a write is legitimate). Built after four bench runs deleted
  `mVirtualControlKeyDown = down` and killed Ctrl while every build passed.
- A trailing comment belongs to the line it sits on; a declaration owns its
  doc comment above and its trailing comment. `TestSiblingSpansDoNotOverlap`
  sweeps the repo (widen with `SWEEP_ROOT`).
- A ref-site address (`file@N`, `file@a-b`) is never the whole file
  (`modernNode.wholeFile`): the old predicate made `delete:true` on `@N`
  delete the file.
- **An edit waits for diagnostics of the edited text** (Carl, 2026-10-27;
  `waitForEditedText`). A publish that merely arrives after the didChange
  can describe the text before it: sourcekit-lsp publishes a just-opened
  file ~1 s later computed without build settings (a call to an undeclared
  function reads clean), and a broken edit made at bring-up was validated
  and kept. A publish carrying a document version counts when the version
  is the edit's or later; an unversioned one counts unless it is the first
  since the file's didOpen, and then the last of a burst (150 ms quiet) is
  taken. An unversioned publish that arrives while the child has a
  work-done progress open (the client advertises `window.workDoneProgress`)
  is not trusted when it has no errors: sourcekit-lsp 6.4.1, while
  "SourceKit-LSP: Reloading Package" is open, publishes [] for the open and
  for every change, and an edit made after the first of those was
  validated broken (v0.3.0 release, macos-15-intel; reproduced on Linux
  under a 5-20% CPU quota). One with errors is taken but can only revert:
  with no new errors the edit is not validated. When the child's last
  progress ends with no publish after the untrusted one, the text is sent
  again (didChange, same text, next version) and the answer to that is
  taken: sourcekit-lsp ends "Indexing" right after publishing a change and
  publishes nothing more (traced; without the resend 2 of 5 Linux e2e
  runs waited out the cap). The cost: a clean edit made while an
  unversioned server indexes is reported `lsp-timeout` when indexing
  outlasts `--validate-wait`. Versioned publishes (gopls) ignore progress. Capped by `--validate-wait` (2 s; per connection in daemon mode,
  `X-Poly-Validate-Wait`): past it the edit is kept with
  `validationSkipped: lsp-timeout` and a `validationNote`, and the stale
  diagnostics are not reported. **Only a versioned result validates**
  (Carl, 2026-10-27): an unversioned one (sourcekit-lsp,
  typescript-language-server) can revert an edit, but a clean one keeps it
  with `validated:false`, `validationSkipped: lsp-unconfirmed`. Xcode's
  sourcekit-lsp on macos-15-intel answered a broken edit with [] under
  "Reloading Package", sent nothing when it ended, and answered the resent
  text with [] again 2 s later (CI, 87d9429): nothing in a publish says it
  lacks semantic errors (sourcekit-lsp computes syntactic ones only while a
  file has no build settings). A batch commit follows the same rule for
  its staged files. Pull diagnostics (`textDocument/diagnostic`) were
  measured and rejected: sourcekit-lsp 6.4.1 registers it when the client
  advertises it, then stops publishing, and answers a pull made before its
  build settings load at once with a full empty report for a broken file;
  only after its `workspace/diagnostic/refresh` does a pull carry the
  error. No better evidence than a publish. typescript-language-server
  5.1.3, clangd and pylsp 1.15 reject the request. gopls edit latency unchanged (median 452 ms over 10 warm edits,
  before and after each change). `POLY_LSP_CHILD_TRACE=1` logs what each
  child sends and the notifications sent to it; the Swift edit e2e sets
  it, so a CI failure shows the sequence.

- **Files outside the root** (Carl, 2026-10-27; `mcp/outside.go`):
  `node_read` / `node_edit` take an absolute path under an allowed
  directory (`--allow-path`, default `os.TempDir()` and `/tmp`, minus
  `--exclude-path`), not indexed, not watched, parsed on demand; the
  address is the absolute path as given, cleaned. Validation is a
  tree-sitter parse (`validation: "parse-only"`): there is no project for a
  child LSP. `/tmp` is shared, so: no symlink component below the allowed
  directory (Lstat each; every open goes through `os.Root` with
  `O_NOFOLLOW`, which also refuses a symlink swapped in later), regular
  files of our uid only, a create only with `O_EXCL` in a directory of our
  uid, a rewrite via an `O_EXCL` temp file renamed over the target. The
  lists are per connection in daemon mode (`X-Poly-Allow-Paths`,
  `X-Poly-Exclude-Paths`), resolved by the proxy with its own `$TMPDIR`;
  a connection that sends none reaches nothing outside. The modern surface
  only (the legacy tools stay inside the root); the tool schemas do not
  mention it, the refusal names the allowed directories.

## Merge conflicts

Conflicts are nodes (`::conflict`, `::mine`, `::theirs`, `::base`, sides
carrying git's ref, since in a rebase "theirs" is your own commit).
Writes that straddle a marker are refused; straddling declarations are
withheld from results (they also skew ordinals). Each side is reconstructed
whole-file; side declarations are names + classes, never addresses, because
a reconstructed span does not address the bytes on disk. A conflict that
appears while running is announced (`logging` + `notifications/message`).

## Daemon

`mcp` proxies to one per-user daemon over a unix socket (the default since
`1be97d0`; `--no-daemon` or `POLY_LSP_NO_DAEMON=1` serves in-process): one
warm index, one child-LSP fleet, one parse cache for every client. README
"Daemon mode" covers use. Decisions:

- **Transport and trust** (Carl, 2026-07-23): gat/huma on a unix socket;
  peer credentials (same uid) plus declared root prefixes.
- **The allow list grows with opens** (Carl, 2026-10-24). The uid can run
  poly-lsp in-process on any root, so the list is no boundary against it;
  `/open` admits an existing directory outside every prefix and logs it.
- **Per-connection policy** is tighten-only: a client may add read-only or
  validate on top of the daemon baseline, never remove it.
- **One open batch per client session**, with commit-time change detection
  (Carl, 2026-07-23).
- **Root lifetime follows its clients** (Carl, 2026-10-24): last client gone
  -> child LSPs stop after `--root-lsp-grace` (1m); the index stays in an LRU
  of client-less roots capped by `--idle-roots-mb` (1024). Estimate =
  `CostReport.Bytes x 8` (measured live heap 5.8x cold / 3.6x index-only,
  ~9x after a resolved query; one repository).
- **One index per root, last open wins** on differing index options; holders
  get a `rebuilding` notice.
- **Blue/green upgrade:** green is `<exe> daemon --takeover-from <pid>`
  (not a `daemon serve` subcommand); the flock is passed on fd 3 so it is
  never free during the swap; green renames its socket over the main path
  (the path always names a listener). Upgrades are refused when not newer by
  `vcs.time`; a build that fails twice is refused (`upgrade-failures.json`);
  `exe.prev` keeps the last good binary.
- Build identity is `<rev12>[+dirty]/<sha256(exe)[:8]>` from build info and
  the executable; nothing is stamped at link time.

## Build, install, self-update

- `go install` needs only Go. With a C compiler the binary links smacker's C
  tree-sitter runtime; without one (`CGO_ENABLED=0`) it uses gotreesitter
  with smacker's grammars converted by ts2go
  (`internal/ts/grammars/generate.sh`), about 10x slower to parse. gotreesitter's
  bundled sql is a different grammar and its go/kotlin are newer revisions,
  hence the conversion.
- **No `-X` stamp** (diverges from the canon CLI-12). Self-rebuild reads
  `LSP_REBUILD_PATH` at runtime: a stamp travels with a copied binary, and
  whoever controls that path controls the binary. The path is vetted
  (absolute, owned by this uid, not world-writable, `go.mod` declares this
  module). `TestNoBuildTargetStampsTheBinary` guards it. Rebuild is temp +
  atomic rename (several clients start at once).
- Install commands printed by `doctor` pin a version, never a bare
  `@latest` (a proxy can cache an old `@latest`).
- OS calls live in per-OS files (`multiplex/proc_*`, `daemon/zombie_*`,
  `daemon/peercred_*`). macOS RSS and zombie checks use `ps`; PSI is Linux
  only; other OSes refuse peer creds. darwin is type-checked with
  `GOOS=darwin gopls check` (cgo blocks `go vet` cross-OS).
- CLI: cobra, one file per command; the bare binary is the LSP server
  (editors start it with no args); an unknown first word is an error, not
  the LSP server waiting on stdin. `tools/graphq` stays on stdlib flag.

## Tests as rules

- `TestLanguageBugMatrix` (+ `symbols/langmatrix_classes_test.go`) states a
  property once and runs it against every registered language. Each language
  lands in `ok`, `n/a`+reason, `KNOWN`+reason, or unmeasured (which fails); a
  `KNOWN` entry that starts passing also fails. Add a class whenever a bug
  turns out to be language-shaped.
- `TestModernToolSurfaceTokenBudget` (`mcp/tokcount_test.go`) bounds the
  tool-surface tokens. Each raise is justified in that file's comment.
- `TestFileAccessMatchesTheGuard` keeps the daemon proxy's file-access hook
  in step with the guard sites in `modern.go`.
