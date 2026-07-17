# icebox — deferred, opt-in next-steps

Designed, not built. Nothing here is in the shipped surface. See plan.md for
active work.

---

## Graph selectors: CSS is a DAG, ours is a graph

**Status:** ◻ designed, not implemented. The shipped surface still has
`:has` / `:has_parent` / `:references` / `:depth` and element semantics.

### The one sentence the docs must lead with

> This is CSS over your file tree — and it IS CSS: `a b`, `a > b`, compounds,
> all of it. **One operator leaves the tree: `parents(sel)` walks BACKWARD to
> whatever reaches the current tip.** That is the graph. That is where a tip
> becomes a set of paths, and where `:any`/`:all`/`:empty` mean something.

Not decoration. It is what makes a CSS-trained model's prior **correct** rather
than merely familiar — every failure measured on 2026-07-16 came from a prior
that was familiar and wrong.

**For HTML you WANT the DAG. For code you NEED the graph.** Containment is a
DAG; references are a cyclic graph. CSS assumes the first and has no provision
for the second.

### EVERY selector is DAG except `parents(sel)`

This is the load-bearing constraint, and it is what keeps the design small:

```
a b, a > b, compounds, :contains, ids, tags    →  containment. A DAG.
                                                   Unique paths. Literally CSS.
parents(sel)                                    →  tip := { n : n matches sel
                                                            ∧ n connects to tip }
                                                   References. A graph.
```

Consequences that fall out of the split, not from extra design:

- **The edge is named by the OPERATOR, not the syntax.** An earlier draft had
  `*` mean "connectable" and thereby merged contains+references into one
  reachability relation — so `dir *{0,} func` and `func *{0,} func` became the
  same operator over different edges. Incoherent. The combinator is containment;
  `parents` is references. No ambiguity, nothing to disambiguate.
- **Graph cost is CONFINED.** Cycles, multiplicity, path sets and the
  quantifiers exist only inside `parents`. The DAG part stays cheap and stays
  CSS — no path bookkeeping on the 95% of queries that never leave the tree.
- **A CSS prior is correct, not merely familiar,** everywhere except one clearly
  marked operator. That is the whole reason to prefer this over "everything is
  a path set": the blast radius of the new concept is one name.

### Why: the path set

A path set arises two ways, and both are invisible in CSS:

- **graph multiplicity** — many reference paths to one node, plus cycles
  (`Walk` calls `Walk`; verified in-tree).
- **set fan-in** — traverse from a SET and N nodes converge on one ancestor.
  Five files referencing `main` all under `cache/` ⇒ that dir is reached by
  five paths. Happens on a pure tree.

CSS never had to answer "all five, or any one?" because it matches ONE element
at a time and the tree gives each a unique path. Set-based traversal removes
that guarantee, so ∃/∀/∄ become distinguishable — and necessary.

### It is a CONSERVATIVE EXTENSION, which is the whole argument

Every CSS construct is recovered at set-size 1:

| construct | CSS meaning | path-set meaning | recovered when |
|---|---|---|---|
| `*` | any element | any node in between | — |
| `a > b` | child | `a *{0} b` | always |
| `a b` | descendant | `a *{0,} b` | always |
| `:empty` | no children | child path set is empty | `:empty(*)` |
| `:is(a,b)` | matches a or b | `:any(a,b)` | set size 1 |
| compound `ab` | matches a AND b | `:all(a,b)` | set size 1 |

The compound being ∀-at-size-1 was not engineered; it falls out. That is the
tell the abstraction is right.

**The test for reusing a CSS name is NOT "is it taken" — it is "does the known
meaning survive as a special case?"**

- `.cache` FAILED it: class in CSS is an OPEN author-defined vocabulary, ours
  was CLOSED. Same syntax, different concept ⇒ false friend ⇒ the model
  confidently invented `.cache` 12 times in one run.
- `:empty` PASSES it: set-emptiness in both. Same concept, wider domain.

A false friend costs more than a stranger. A conservative extension costs
nothing.

### Operators — THREE ROLES: move, filter, validate

```
DAG (containment) — literally CSS, unique paths, cheap:
  a > b          child                    ≡ a *{0}  b
  a b            descendant               ≡ a *{0,} b
  a *{1,3} b     b within 1-3 hops of a   ← CSS cannot say this
  a func{1,3} b  …THROUGH 1-3 funcs       ← the synthetic connect-by path filter

MOVE — the ONLY operator that leaves the DAG:
  parents(sel)   tip := { n : n matches sel ∧ n connects to tip }
                 Backward. References enter HERE and nowhere else.

FILTER — composable, set → subset:
  :where(X)      keep paths matching X

VALIDATE — terminal, set → bool, keeps/kills the tip:
  :any(X)        ∃ over the path set
  :all(X)        ∀ over the path set
  :empty(X)      ∄ over the path set
```

**Filter vs validate is a real distinction, not a naming one.** A filter narrows
the set and the subset FLOWS ON (composable). A validation collapses to a bool
and DECIDES the tip (terminal). Conflating them is why an earlier draft had
`:any` doing double duty with an ambiguous scope.

**The quantifier is also what turns a MOVE into a FILTER**, which is why the
three are not sugar — they are the other half of the traversal:

```
X parents(S)           MOVES   — tip becomes the S's pointing at X
X:any(parents(S))      FILTERS — X's that have an S pointing at them
```

So forward reachability needs no forward edge. *"What does main call?"* — main
is a parent of its callees (it points at them):

```
func:any(parents(#'main'))     the funcs main calls
```

**Range belongs on a GROUP, not an edge.** Range an edge and you get a binary
relation: the intermediates are consumed and the path is gone, so there is
nothing left to filter. Range a group and the intermediate is NAMED, hence
filterable — that is the difference between a relation and a path expression,
and why `>` and ` ` are not primitives.

**`:depth(m,n)` dissolves.** It is `*{m-1,n-1}`.

### What dissolves (three pseudos out, one operator in)

```
file:has(function#test)           ≡ parents(...) inversion    ← :has was a workaround
function#test:has_parent(file#x)  ≡ file#x function#test      ← already the default
*:references(#'main')             ≈ #'main':parents(*)        ← see the CAVEAT below
```

CSS has a FIXED subject (rightmost compound) and no parent combinator. `:has()`
is not a feature — it is a workaround for a unidirectional language, and it
cannot express "the far end of a variable-length path" at all. CSS4 proposed a
subject marker (`ul! li.active`) and dropped it for `:has()`; that trade is the
origin of the asymmetry. A FUNCTION beats a marker because it COMPOSES —
`parents(A parents(B C) D)` nests; `A! B :has(C! D)` is incoherent.

`:has`, `:has_parent` and `:references` got **ZERO uses between them** across 5
measured runs. One operator replaces all three.

**CAVEAT — `parents` does NOT cleanly subsume `:references`.** `parents(*)` is
strictly BROADER: "upstream" is two relations, and containment is upstream too.

```
*:references(#'errf')     → the callers of errf
#'errf':parents(*)        → callers ∪ modSelParser (containing type)
                                    ∪ query.go (file) ∪ project
```

The 5-hop chain a frontier model hand-rolled (see Evidence) only terminated on
a useful answer BECAUSE `:references` named the edge — expand containment at
every hop and the frontier is the whole workspace by hop 3. Two candidate fixes:

1. **`:where` picks the edge after the move** — viable ONLY because the
   traversal yields a subgraph that retained the edges (see Implementation):
   `#'errf':parents(*):where(«via ref»)`.
2. **DAG-primary with references as a special edge type** (the prior art below)
   — traversal IS containment; a reference is an explicit RE-ROOT. Then the
   edge is never ambiguous and `parents` need not name it.

(2) is probably right. UNRESOLVED — decide before building.

**Naming:** `parents` is right for the SHAPE (upstream over an edge) but it is a
jQuery false friend: `.parents()` returns ancestors of the current SET; this
returns nodes matching sel that connect to the tip. Related-but-different is the
`.cache` failure shape. Consider `:origin` / `:from` / `:upstream` — strangers
are safe. UNRESOLVED.

### Implementation: a SUBGRAPH, not a node set, not a sequence set

Three candidate representations; only the third is right:

| representation | cost | edges | verdict |
|---|---|---|---|
| set of sequences (real paths) | exponential; cycles diverge | kept | no |
| set of nodes | O(V+E) | **LOST** | no |
| **reachability subgraph** | **O(V+E)** | **kept** | **yes** |

"Paths as sets" means *do not ENUMERATE the structure* — NOT *discard it*. The
traversal hands back the nodes it reached **plus the edges it crossed**. The set
is a SELECTION INTO the graph, not a copy of it: the graph is still there at
evaluation time, so nothing was ever lost.

- **cycles vanish** — revisiting a node does not grow the subgraph. The
  visited-set bounds it. The fixpoint is subgraph-stability.
- **cost is O(V+E)** — one traversal, never 2^n orderings.
- **edges are testable** — `:where` can ask HOW a node connects, because the
  traversal recorded it. This is what lets `parents` stay general (upstream over
  anything) while `:where` picks the edge:

  ```
  #'errf':parents(*)         → upstream subgraph: callers ∪ containing type/file/project
          :where(«via ref»)  → drop containment edges. Callers only.
  ```

- **`:all` needs no bound for PERFORMANCE.** ∀ over the subgraph is a linear
  pass over its nodes/edges — you never enumerate a path to test one. Bounds
  (`{1,3}`) become a semantic choice, not an escape hatch. (An earlier draft of
  this file claimed `:all` must be bounded; that was the sequence-set model.)

`referenceSet` (mcp/query.go) already computes one hop and memoizes it — the
fixpoint is a work-list loop over that same map. No new index, no new parse.

**Traded away — write this down as a trade, not a bug:** a subgraph unions all
paths, so it says *"every node between a and b is a func"* but CANNOT say
*"there EXISTS a path where every hop is a func"*. One non-func node kills the ∀
even if an all-func path exists beside it. Same-path membership is what never
enumerating costs.

### Prior art (nthalk built this once, for graph path traversal)

Worth mining before designing from scratch — it solves the edge question a
different way and it is probably better:

- **The query itself is a DAG, built with the context.** The plan is a graph,
  not a string walk.
- **Worker threads fed jobs** to test nodes, plus a **path cache**. Node tests
  parallelize; the cache is what makes re-visiting cheap.
- **Each match carried a COMPLETE PATH** — which is affordable precisely
  because DAG paths are unique: a node's path from root IS its ancestor chain,
  already in hand, zero extra cost. (This is why the tree half stays cheap; the
  representation question only bites on references.)
- **Most matchers ran against the root path, or the path between the tip and
  the tree children** — i.e. against the ancestor chain, which is exactly the
  free thing.
- **Search stays DAG-primary; REFERENCES ARE A SPECIAL EDGE TYPE.** Traversal
  is containment; a reference is a *re-root* — you jump and start a fresh DAG
  path from the target.

That last point is the cleanest answer to the edge seam: a path is
**alternating DAG-segments joined by reference jumps**. The edge is not
ambiguous because containment is the default traversal and a reference is an
explicit re-root. `parents` does not need to name its edge if the engine's
primary mode is the DAG and references are a marked jump.

Reconciles with the subgraph model rather than contradicting it: the subgraph is
what one reachability step yields; the complete path is what each match carries
back, free in the DAG, re-rooted at each reference hop.

### Evidence (2026-07-16)

- `:has` / `:has_parent` / `:references` / `:depth`: **0 uses** by bonsai across
  5 runs while documented in prose. A kitchen-sink EXAMPLE did not move it
  either. A RECIPE keyed to an address ("you have store.go#Save — now what")
  took `:references` from 0 → the opening move. Prose does not teach; recipes do.
- Claude, given bash AND poly-lsp, used poly-lsp **0 times** — it grepped.
  Denied bash, it used the language fluently and hand-rolled transitive closure
  with **7 chained `:references` calls**. The tool was never unreachable; it was
  unchosen. `func:origin(*{0,} #'selectorGrammarHelp')` is that entire chain in
  one expression.
- Forced onto poly-lsp it found **12/12** render paths; bash found **10/12** and
  looked complete. `:references` is exhaustive; grep+reason is sampling.
- Selector error rate 42% → 17% came ENTIRELY from output-side fixes (terse
  errors naming the fix, examples that don't contradict the rules, result keys
  that speak the grammar). The 615-token description moved nothing.

### Where the code is (as of 2026-07-16)

```
mcp/query.go       the MODERN selector engine — parser + evaluator. THE file.
                     modSelParser        the parser (parseCompound/parsePseudo/readID)
                     engine, treeNode     the unified tree (project>dir>file>sym>argument)
                     buildTree()          walks the workspace; file symbols load LAZILY
                     referenceSet()       ONE reference hop, memoized per query.
                                          The fixpoint is a work-list loop over this.
                     isDeclSite()         excludes a decl from its own references
                     selectorGrammarHelp  the grammar text (selector "?" returns it)
mcp/modern.go      the 3-tool surface: descriptions, schemas, handlers,
                   resolveModernNode (address-or-selector, ambiguity = error)
mcp/node_query.go  the LEGACY engine (bare `type[name=x]`) — untouched, behind
                   --legacy-tools. Do not confuse the two.
symbols/           tree-sitter parsing; Symbol{Decl*, Name*}; the lexical Index
mcp/tokcount_test.go  per-tool token budget. READ ITS COMMENT before trimming.
```

### Build order (smallest slice that proves the model first)

1. **`parents(sel)`** as a MOVE, DAG-primary, references as a marked re-root.
   Replaces `:references`/`:has`/`:has_parent` — all three have ZERO measured
   uses, so nothing regresses. Resolve the edge CAVEAT here, first.
2. **Subgraph traversal** with edges retained, on top of `referenceSet`'s memo.
3. **`:where` / `:any` / `:all` / `:empty`** over it — filter vs validate.
4. `{m,n}` group ranges and the `*{0}`/`*{0,}` unification.
5. Only then: retire `:depth`.

**Test the way this session learned to:** a test that does not fail against the
OLD behavior proves nothing. Every fix committed today was verified by reverting
the fix and watching the test fail. Do that.

**Do not document a feature before it exists.** The tool descriptions and the
grammar help must describe the SHIPPED language, never this file. A description
that contradicts its implementation is the single most expensive bug measured
today (`Examples: .file` while the rules said tags; `"class"` in output under a
tag grammar). The docs line at the top of this file is for WHEN it ships.

### Open

- **The `parents` edge CAVEAT** (see above) — the one blocker. Decide (2)
  DAG-primary-with-re-root vs (1) `:where` picks the edge.
- **`parents` naming** — jQuery false friend. `:origin`/`:from`/`:upstream`?
- **`:any` scope.** Over a path set vs over a node set is the same spelling for
  two operators. Decide before either exists — this is the `.cache` shape.
- **Paren depth.** Nested inversion + nested quantifiers reaches depth 4 in a
  single-line string inside a JSON field. The DESIGNER miscounted parens writing
  the example by hand (one `)` short); a small model will too. Composability
  buys expressiveness and charges depth. Consider whether `parents` chains
  should be flattenable.
- **Does anything USE it?** The hard-won lesson: capability ≠ adoption. Given
  bash, NOTHING used poly-lsp — not a 27B quant, not a frontier model (0 calls
  out of 8, with node_query in the tool list). It is only ever used when the
  alternative is removed (cedeFileTools). A better language does not fix that;
  only a recipe example keyed to the caller's actual state moved usage at all.
