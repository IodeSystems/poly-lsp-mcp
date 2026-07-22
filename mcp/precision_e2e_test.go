package mcp

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// The precision pass against a REAL gopls.
//
// The fixture is the collision lexical scope cannot settle: two packages
// each declaring Save, and a caller that uses exactly one of them. The
// name is identical, both declarations are top-level (so no scope rule
// applies), and only a language server knows which one `store.Save()`
// means.
func writePrecisionFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module collide\ngo 1.21\n")
	write("store/store.go", `package store

// Save is the one main.go actually calls.
func Save(v string) string {
	return "store:" + v
}
`)
	write("cache/cache.go", `package cache

// Save has the same name and is never called by main.go.
func Save(v string) string {
	return "cache:" + v
}
`)
	write("main.go", `package main

import "collide/store"

func Run() string {
	return store.Save("x")
}
`)
	return dir
}

// startPrecisionSession is startSessionFull WITH a child-LSP manager —
// the shape the MCP server runs in. startSessionFull deliberately has no
// manager, so a precision test built on it would pass by testing
// nothing.
func startPrecisionSession(t *testing.T, root string) *mcpSession {
	t.Helper()
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, root, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))

	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()

	s := &mcpSession{
		t:       t,
		srv:     srv,
		srvIn:   cOut,
		clientR: json.NewDecoder(cIn),
		clientW: cOut,
		done:    done,
	}
	// initialize spawns gopls and blocks on its handshake.
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	return s
}

func requireGopls(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping live-gopls e2e in -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
}

// Lexical offers BOTH Saves; gopls must reduce Run's outgoing call to
// the one that is really called, and mark it resolved.
func TestPrecisionPicksTheRealCallTarget(t *testing.T) {
	requireGopls(t)
	dir := writePrecisionFixture(t)

	s := startPrecisionSession(t, dir)
	defer s.close()

	q := query(t, s, map[string]any{"selector": `#'main.go#Run'::out.call`, "limit": 20})
	if q.TotalMatches == 0 {
		t.Fatal("Run calls store.Save — expected an outgoing call edge")
	}
	var saw bool
	for _, m := range q.Matches {
		if len(m.To) == 0 {
			continue
		}
		saw = true
		joined := strings.Join(m.To, ",")
		if !strings.Contains(joined, "store/store.go#Save") {
			continue
		}
		if strings.Contains(joined, "cache/cache.go#Save") {
			t.Errorf("gopls resolves store.Save; the edge still lists cache's Save too: %v\n"+
				"conf=%q — the precision pass did not narrow it", m.To, m.Conf)
		}
		if m.Conf != refLSP {
			t.Errorf("an edge a child LSP settled must say so: conf=%q, want %q", m.Conf, refLSP)
		}
	}
	if !saw {
		t.Fatal("no edge carried a far end; fixture or query is wrong")
	}
}

// The mirror: cache.Save has no callers, and lexical would hand it
// main.go's call to store.Save purely on the name.
func TestPrecisionDropsCoincidentalIncomingEdge(t *testing.T) {
	requireGopls(t)
	dir := writePrecisionFixture(t)

	s := startPrecisionSession(t, dir)
	defer s.close()

	// store.Save IS called by Run.
	got := query(t, s, map[string]any{"selector": `#'store/store.go#Save'::in.call`, "limit": 20})
	if got.TotalMatches == 0 {
		t.Error("store.Save is called by main.go#Run — expected an incoming edge")
	}

	// cache.Save is called by nobody. Name-keying claims otherwise.
	none := query(t, s, map[string]any{"selector": `#'cache/cache.go#Save'::in.call`, "limit": 20})
	for _, m := range none.Matches {
		t.Errorf("cache.Save has no callers, but an incoming edge was reported "+
			"from %v (conf=%q) — that is main.go's call to store.Save, matched on the name",
			m.From, m.Conf)
	}
}

// No manager (the `query` CLI's shape) must still answer — and, with no
// LSP to settle the collision, say so honestly: Run's call to the
// ambiguous `Save` (two same-named decls) is UNSETTLED, not a certain
// lexical hit. Precision is an upgrade, never a dependency.
func TestWithoutLSPEdgesStayUnsettledAndSaySo(t *testing.T) {
	dir := writePrecisionFixture(t)
	cfg, _, err := config.LoadOrDefault("nonexistent.yaml") // defaults
	if err != nil {
		t.Fatal(err)
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil) // no SetManager — the CLI's shape
	if err := srv.BuildIndex(); err != nil {
		t.Fatal(err)
	}
	list, err := parseModernSelector(`#'main.go#Run'::out.call`) //nolint
	if err != nil {
		t.Fatal(err)
	}
	e, err := srv.buildTree()
	if err != nil {
		t.Fatal(err)
	}
	rows := e.evaluate(list)
	if len(rows) == 0 {
		t.Fatal("no manager must still produce edges")
	}
	// Run's only outgoing call is `Save`, which two packages declare — an
	// ambiguity nothing here can settle, so it is unsettled, not lexical.
	for _, r := range rows {
		if r.refConf != refUnsettled {
			t.Errorf("an ambiguous edge with no child LSP is a GUESS (unsettled); got conf=%q", r.refConf)
		}
	}
	if e.lspAsked != 0 {
		t.Errorf("no manager means no round-trips; asked=%d", e.lspAsked)
	}
}

// A warm session resolves each site ONCE: the second identical edge query
// is served entirely from the definition cache — same answers, zero new
// LSP round-trips.
func TestResolveDefinitionCachedAcrossQueries(t *testing.T) {
	requireGopls(t)
	dir := writePrecisionFixture(t)

	s := startPrecisionSession(t, dir)
	defer s.close()

	sel := map[string]any{"selector": `#'main.go#Run'::out.call`, "limit": 20}
	q1 := query(t, s, sel)
	after1 := s.srv.defMisses
	if after1 == 0 {
		t.Fatal("the ambiguous Save edge must force at least one round-trip")
	}

	q2 := query(t, s, sel)
	after2 := s.srv.defMisses
	if after2 != after1 {
		t.Errorf("the second identical query must hit the cache — 0 new round-trips; misses %d → %d", after1, after2)
	}
	// And the cached run returns the SAME answer.
	if q1.TotalMatches != q2.TotalMatches {
		t.Errorf("cached query changed the result: %d vs %d matches", q1.TotalMatches, q2.TotalMatches)
	}
}

// :recursive is the edge-semantic predicate the icebox parked until the
// precision pass existed. The soundness case: `fib` really recurses, but
// `func Write` calling `w.Write` (io.Writer's method, same name) must NOT
// read as recursive — the exact lexical false positive that blocked it.
func writeRecursiveFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module rec\ngo 1.21\n")
	write("main.go", `package main

import "io"

func fib(n int) int {
	if n < 2 {
		return n
	}
	return fib(n-1) + fib(n-2)
}

func Write(w io.Writer, b []byte) { w.Write(b) }

func Plain() int { return 1 }

func Ping() { Pong() }
func Pong() { Ping() }

type Server struct{}

func (s *Server) Loop() { s.Loop() }
`)
	return dir
}

func TestRecursivePredicateLSPConfirmed(t *testing.T) {
	requireGopls(t)
	dir := writeRecursiveFixture(t)

	s := startPrecisionSession(t, dir)
	defer s.close()

	// func:recursive — only fib. Write (w.Write is io.Writer's), Plain (no
	// call), Ping/Pong (mutual, not DIRECT) must all be excluded.
	q := query(t, s, map[string]any{"selector": `func:recursive`, "limit": 20})
	got := nodes(q)
	if !slices.Contains(got, "main.go#fib") {
		t.Errorf("fib directly recurses — :recursive must match it; got %v", got)
	}
	for _, bad := range []string{"main.go#Write", "main.go#Plain", "main.go#Ping", "main.go#Pong"} {
		if slices.Contains(got, bad) {
			t.Errorf(":recursive false positive on %s (a name collision or mutual call); got %v", bad, got)
		}
	}

	// Method self-recursion is real recursion — s.Loop() resolves to Loop.
	m := query(t, s, map[string]any{"selector": `method:recursive`, "limit": 20})
	if !slices.Contains(nodes(m), "main.go#Server.Loop") {
		t.Errorf("Server.Loop calls itself — :recursive must match it; got %v", nodes(m))
	}
}

// The North Star Stage 0 case: a name that COLLIDES locally but whose real
// target is the stdlib. Two packages declare Split; main.go calls
// strings.Split. Lexical offers both local Splits; gopls resolves OUTSIDE
// the root, so the edge must become an honest EXTERNAL STUB (strings#Split,
// domain external, conf lsp) — never a false local.
func writeExternalStubFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module extstub\ngo 1.21\n")
	write("pkga/a.go", "package pkga\n\nfunc Split(s string) string { return s }\n")
	write("pkgb/b.go", "package pkgb\n\nfunc Split(s string) string { return s }\n")
	write("main.go", `package main

import "strings"

func Run() []string {
	return strings.Split("a,b", ",")
}
`)
	return dir
}

func TestPrecisionResolvesToExternalStub(t *testing.T) {
	requireGopls(t)
	dir := writeExternalStubFixture(t)

	s := startPrecisionSession(t, dir)
	defer s.close()

	q := query(t, s, map[string]any{"selector": `#'main.go#Run'::out.call`, "limit": 20})
	if q.TotalMatches == 0 {
		t.Fatal("Run calls strings.Split — expected an outgoing call edge")
	}
	var saw bool
	for _, m := range q.Matches {
		joined := strings.Join(m.To, ",")
		if !strings.Contains(joined, "Split") {
			continue
		}
		saw = true
		// The real target is external: the stub is named strings#Split,
		// NOT either local pkg's Split.
		if !strings.Contains(joined, "strings#Split") {
			t.Errorf("Split edge far end = %v, want the external stub strings#Split", m.To)
		}
		if strings.Contains(joined, "pkga") || strings.Contains(joined, "pkgb") {
			t.Errorf("edge fell back to a FALSE LOCAL: %v", m.To)
		}
		if m.Domain != "external" {
			t.Errorf("an out-of-root far end must be domain=external; got %q", m.Domain)
		}
		if m.Conf != refLSP {
			t.Errorf("gopls resolved it — conf must be %q; got %q", refLSP, m.Conf)
		}
	}
	if !saw {
		t.Fatal("no Split edge in the result; fixture or query is wrong")
	}
}
