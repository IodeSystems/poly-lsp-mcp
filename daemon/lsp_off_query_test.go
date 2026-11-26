package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// (b) and (d) The daemon's /query settles with the CALLING client's
// child-LSP switch: a session that sent X-Poly-Session with go off gets the
// lexical answer, one that left it on gets the LSP answer, and a client
// that sent no session at all settles exactly as before, with the
// daemon's defaults, in both text and JSON mode. The fixture has two
// same-named declarations, so the switch is observable in the answer
// itself.
func TestQuerySettlesWithTheSessionSwitch(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module collide\ngo 1.21\n")
	write("store/save.go", "package store\n\n// STORE-ANCHOR\nfunc Save(v string) string {\n\treturn \"store:\" + v\n}\n")
	write("other/save.go", "package other\n\n// OTHER-ANCHOR\nfunc Save(v string) string {\n\treturn \"other:\" + v\n}\n")
	write("main.go", "package main\n\nimport (\n\t\"collide/other\"\n\t\"collide/store\"\n)\n\nfunc Run() (string, string) {\n\treturn store.Save(\"x\"), other.Save(\"y\")\n}\n")
	resolved, _ := resolvePath(root)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	for i := range cfg.Languages {
		if cfg.Languages[i].Name == "go" {
			cfg.Languages[i].LSP = &config.LSP{Cmd: exe, Args: []string{fakeLSPArg}, Env: []string{
				"FAKE_DEF_URI=file://" + filepath.Join(resolved, "store", "save.go"),
				"FAKE_DEF_LINE=2",
			}}
		}
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(cfg, reg, false, true)
	r.cachePath = ""
	socket := serveRegistry(t, resolved, r)

	// The on client opens the root (the child starts for it) and queries
	// as its session: the LSP picks the far end, so the far node IS
	// store/save.go#Save and the answer carries it with conf lsp.
	on := NewClient(socket)
	if res, err := on.OpenWith(resolved, OpenOptions{}); err != nil || res.Error != "" {
		t.Fatalf("open: %v %s", err, res.Error)
	}
	const sel = `#'main.go#Run'::out.call`
	q, err := queryRaw(t, socket, on.Session(), resolved, sel, false)
	if err != nil {
		t.Fatalf("query as the on session: %v", err)
	}
	if !strings.Contains(q, "store/save.go") {
		t.Fatalf("on session: the LSP-picked far file is missing:\n%s", q)
	}
	if !strings.Contains(q, "settled by a child LSP") {
		t.Fatalf("on session: the edge is not settled by the LSP:\n%s", q)
	}

	// The off client: go off in its /open, so its /query settles lexical —
	// the far end is NOT the LSP-picked one.
	off := NewClient(socket)
	if res, err := off.OpenWith(resolved, OpenOptions{LSPOff: []string{"go"}}); err != nil || res.Error != "" {
		t.Fatalf("open: %v %s", err, res.Error)
	}
	q, err = queryRaw(t, socket, off.Session(), resolved, sel, false)
	if err != nil {
		t.Fatalf("query as the off session: %v", err)
	}
	if !strings.Contains(q, "store/save.go") {
		t.Fatalf("off session: the lexical candidate file is missing:\n%s", q)
	}
	if strings.Contains(q, "settled by a child LSP") {
		t.Fatalf("off session: the edge is settled by the LSP:\n%s", q)
	}
	if !strings.Contains(q, "name-keyed") {
		t.Fatalf("off session: the edge is not marked lexical:\n%s", q)
	}

	// (d) No session at all: exactly the daemon's defaults — the LSP
	// answer, as before the switch existed, in text and JSON mode.
	for _, j := range []bool{false, true} {
		q, err := queryRaw(t, socket, "", resolved, sel, j)
		if err != nil {
			t.Fatalf("no-switch query (json=%v): %v", j, err)
		}
		if !strings.Contains(q, "store/save.go") {
			t.Fatalf("no-switch query (json=%v): settled without the daemon's defaults:\n%s", j, q)
		}
		if !j && !strings.Contains(q, "settled by a child LSP") {
			t.Fatalf("no-switch text query: the edge is not settled by the daemon's LSP:\n%s", q)
		}
		if j && !strings.Contains(q, `"matches"`) {
			t.Fatalf("no-switch JSON query: not a JSON document:\n%s", q)
		}
	}
}

// queryRaw posts one /query to the socket as the given session id
// ("" = the header absent, the no-switch client) and returns the answer.
func queryRaw(t *testing.T, socket, session, root, selector string, jsonMode bool) (string, error) {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", socket)
	}}}
	body, _ := json.Marshal(map[string]any{"root": root, "selector": selector, "limit": 0, "offset": 0, "json": jsonMode})
	req, err := http.NewRequest(http.MethodPost, "http://poly-lsp/query", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("X-Poly-Session", session)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Kind   string `json:"kind"`
			Title  string `json:"title"`
			Detail string `json:"detail"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return "", fmt.Errorf("query: %d: %s", resp.StatusCode, e.Detail)
	}
	var out struct {
		Output string `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Output, nil
}
