# Query language: GSL (selector over the node tree + reference graph)

Match nodes with a CSS-style selector. Two axes: CONTAINMENT (project > dir > file > symbol) and REFERENCE EDGES (calls, type uses, imports).

- TAG: `project dir file func method type struct interface class const var field import` — `file.go` = Go files. `*` matches only nodes, never edges.
- `#Name` finds a symbol anywhere; quote anything with dots, `#` or spaces: `#'io.Writer'`, `#'store.go#Save'` pins the file.
- Attributes (brackets optional): `[path=store.go]` where it lives, `[name^=Test]` prefix, `[name*=x]` contains, `|` OR.
- Combinators: space = descendant, `>` = child, `,` = union.

EDGES: `::in` = references pointing AT the node (its callers/uses); `::out` = references it makes. Kinds: `.call` `.type` `.import`.
RULE: a `::in`/`::out` clause matches the EDGE (the use-site), not the far node. Always end with `> *` to land on the far symbols.

- Who calls Save:          `#'Save'::in.call > *`
- What Save calls:          `#'Save'::out.call > *`
- Everything reaching Save: `#'Save'::in.call{1,} > *`
- Is Save used at all:      `#'Save'::in`   (0 matches = unused)
- Types X is used as:       `#'X'::in.type > *`

A query returning edge rows (no `> *`) has no symbols to read: that is the failure mode, add `> *`.
