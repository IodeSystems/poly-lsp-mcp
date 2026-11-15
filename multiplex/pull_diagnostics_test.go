package multiplex

import (
	"encoding/json"
	"sort"
	"testing"
)

func TestPullsDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		path string
		args []string
		want bool
	}{
		{"/p/node_modules/@typescript/typescript-linux-x64/lib/tsc", []string{"--lsp", "--stdio"}, true},
		{"/bin/tsgo", []string{"--lsp", "--stdio"}, true},
		{"/bin/tsc", []string{"-p", "."}, false},
		{"/bin/typescript-language-server", []string{"--stdio"}, false},
		{"/bin/gopls", nil, false},
	} {
		if got := pullsDiagnostics(tc.path, tc.args); got != tc.want {
			t.Errorf("pullsDiagnostics(%s %v) = %v, want %v", tc.path, tc.args, got, tc.want)
		}
	}
}

func TestPullStateObserve(t *testing.T) {
	var p pullState
	doc := func(uri string, v int) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"textDocument": map[string]any{"uri": uri, "version": v}})
		return b
	}
	if uris, sweep := p.observe("textDocument/didOpen", doc("a", 1)); len(uris) != 1 || uris[0] != "a" || sweep {
		t.Fatalf("didOpen a: %v %v", uris, sweep)
	}
	p.observe("textDocument/didOpen", doc("b", 1))
	p.observe("textDocument/didOpen", doc("c", 1))
	uris, sweep := p.observe("textDocument/didChange", doc("b", 2))
	if !sweep || len(uris) != 3 || uris[0] != "b" {
		t.Fatalf("didChange b: %v %v, want b first then a, c", uris, sweep)
	}
	rest := append([]string(nil), uris[1:]...)
	sort.Strings(rest)
	if rest[0] != "a" || rest[1] != "c" {
		t.Fatalf("siblings %v", rest)
	}
	if v, ok := p.version("b"); !ok || v != 2 {
		t.Fatalf("version b = %d %v", v, ok)
	}
	if uris, _ := p.observe("textDocument/didClose", doc("c", 0)); uris != nil {
		t.Fatalf("didClose pulls %v", uris)
	}
	if _, ok := p.version("c"); ok {
		t.Fatal("closed c still open")
	}
	if uris, _ := p.observe("textDocument/didSave", doc("c", 0)); uris != nil {
		t.Fatalf("didSave of a closed doc pulls %v", uris)
	}
	if uris, _ := p.observe("workspace/didChangeWatchedFiles", json.RawMessage(`{"changes":[]}`)); uris != nil {
		t.Fatalf("watched files pulls %v", uris)
	}
	g := p.nextGen()
	p.nextGen()
	if p.current(g) {
		t.Fatal("an older sweep is still current")
	}
}
