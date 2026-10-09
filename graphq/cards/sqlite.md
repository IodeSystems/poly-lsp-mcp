# Query language: SQL (SQLite, node/edge tables)

SCHEMA
  node(id INTEGER PRIMARY KEY, name TEXT, path TEXT, class TEXT)
    class in: func, method, type, struct, interface, class, const, var, field, import, file, dir, project
    name = symbol name (files: the filename); path = workspace-relative file path.
  edge(id INTEGER PRIMARY KEY, src INTEGER, dst INTEGER, kind TEXT)
    kind in: call, type, import. src = the referrer, dst = the referent
    (a call's src is the caller, dst the callee).
    Containment is NOT an edge: filter with path / class.

Available: SELECT, WHERE (=, LIKE, IN, AND, OR, NOT), JOIN, DISTINCT, COUNT,
ORDER BY, LIMIT, GROUP BY, and RECURSIVE CTEs (WITH RECURSIVE).

Return answer sets as `SELECT DISTINCT n.name, n.path FROM node n ...`.

Examples —
- Who calls Save:
    SELECT DISTINCT n.name, n.path FROM edge e JOIN node n ON n.id = e.src
    WHERE e.kind = 'call' AND e.dst = (SELECT id FROM node WHERE name = 'Save')
- What Save calls:
    SELECT DISTINCT n.name, n.path FROM edge e JOIN node n ON n.id = e.dst
    WHERE e.kind = 'call' AND e.src = (SELECT id FROM node WHERE name = 'Save')
- Everything reaching X (transitive callers):
    WITH RECURSIVE r(id) AS (
      SELECT src FROM edge WHERE kind='call' AND dst = (SELECT id FROM node WHERE name='X')
      UNION
      SELECT e.src FROM edge e JOIN r ON e.dst = r.id WHERE e.kind='call')
    SELECT DISTINCT n.name, n.path FROM r JOIN node n ON n.id = r.id
- Is X used at all:
    SELECT COUNT(*) FROM edge WHERE dst = (SELECT id FROM node WHERE name = 'X')
