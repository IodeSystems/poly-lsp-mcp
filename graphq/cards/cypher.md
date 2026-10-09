# Query language: Cypher subset (over the node/edge model)

Nodes: `(:Node {name, path, class})`. `class` is the kind: func, method, type, struct, interface, class, const, var, field, import, file, dir, project. `name` is the symbol's name (files: the filename). `path` is the workspace-relative file path.

Edges: `(:Node)-[:EDGE {kind}]->(:Node)`, kind = call, type, import. Direction: the REFERRER points at the REFERENT — a call site's function points at the callee. Containment is NOT an edge: use `path` / `class` instead.

Syntax available:
- `MATCH (n:Node) WHERE ... RETURN n` — WHERE with `=`, `STARTS WITH(n.path, ...)`, `CONTAINS`, `AND`, `OR`, `NOT`, `IN`.
- Patterns in MATCH: `(a:Node)-[r:EDGE {kind: 'call'}]->(b:Node)`.
- `OPTIONAL MATCH` for "may not exist".
- Recursion: `*1..` for transitive closure, e.g. `(a)-[:EDGE *1..]->(b)`.
- `RETURN` (aliases, `COUNT(...)`), `DISTINCT`, `ORDER BY`, `LIMIT`.

Answer node sets as `RETURN DISTINCT n.name` (or `n.path`).

Examples —
- Who calls Save:        `MATCH (a:Node)-[:EDGE {kind: 'call'}]->(b:Node) WHERE b.name = 'Save' RETURN a.name, a.path`
- What Save calls:        `MATCH (a:Node {name: 'Save'})-[:EDGE {kind: 'call'}]->(b:Node) RETURN b.name, b.path`
- Everything reaching X:  `MATCH (a:Node)-[:EDGE *1..]->(b:Node {name: 'X'}) RETURN DISTINCT a.name`
- Is X used at all:       `MATCH (:Node)-[:EDGE]->(b:Node {name: 'X'}) RETURN COUNT(DISTINCT b)`
- Types X is used as:     `MATCH (a:Node)-[:EDGE {kind: 'type'}]->(b:Node {name: 'X'}) RETURN a.name`
