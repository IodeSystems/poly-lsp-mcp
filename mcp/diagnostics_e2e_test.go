package mcp

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// TestNodeEditSurfacesGoplsDiagnostics proves the full Phase 5 path:
// node_edit writes a deliberately broken Go file, MCP forwards
// didOpen/didChange/didSave to gopls, and the tool response includes
// the resulting diagnostic.
//
// Skipped when:
//   - testing.Short (gopls init can take a few seconds)
//   - gopls is not on PATH
//
// gopls is the canonical real-world test target; if this passes once,
// every other LSP that follows the spec gets the same plumbing.
func TestNodeEditSurfacesGoplsDiagnostics(t *testing.T) {
	if testing.Short() {
		t.Skip("gopls diagnostics e2e skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}

	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath,
		[]byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	// gopls takes longer to first-publish than the production default.
	// 8s is comfortable.
	srv.SetDiagnosticWait(8 * time.Second)

	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()

	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()

	sess := &mcpSession{
		t:       t,
		srv:     srv,
		srvIn:   cOut,
		clientR: json.NewDecoder(cIn),
		clientW: cOut,
		done:    done,
	}
	defer sess.close()

	sess.request("initialize", map[string]any{})
	sess.notify("notifications/initialized", map[string]any{})

	// Rewrite the file to call an undeclared function: the kind of
	// error gopls will publish immediately as a diagnostic.
	broken := "package main\n\nfunc main() {\n\tdoesNotExist()\n}\n"
	r := sess.callTool("node_edit", map[string]any{
		"file":      "main.go",
		"startLine": 1, "startCol": 1,
		"endLine": 4, "endCol": 1,
		"newText": broken,
	})
	if r.IsError {
		t.Fatalf("node_edit errored: %+v", r.Content)
	}

	var payload struct {
		DiagnosticsAvailable bool `json:"diagnosticsAvailable"`
		DiagnosticsTimedOut  bool `json:"diagnosticsTimedOut"`
		Diagnostics          []struct {
			File      string `json:"file"`
			Severity  string `json:"severity"`
			Message   string `json:"message"`
			Source    string `json:"source"`
			StartLine int    `json:"startLine"`
			StartCol  int    `json:"startCol"`
			EndLine   int    `json:"endLine"`
			EndCol    int    `json:"endCol"`
			Text      string `json:"text"`
			Context   []struct {
				Line int    `json:"line"`
				Text string `json:"text"`
			} `json:"context"`
			EnclosingNode *struct {
				Type      string `json:"type"`
				Name      string `json:"name"`
				StartLine int    `json:"startLine"`
				EndLine   int    `json:"endLine"`
			} `json:"enclosingNode"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal([]byte(r.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode edit response: %v", err)
	}
	if !payload.DiagnosticsAvailable {
		t.Fatalf("diagnosticsAvailable=false; payload=%+v", payload)
	}
	if payload.DiagnosticsTimedOut {
		t.Logf("diagnostics timed out — gopls may need more time")
	}
	// gopls publishes "undeclared name" / "undefined" — assert there's
	// at least one error-severity entry on the doesNotExist token, and
	// that the enrichment fields are populated.
	var hit *struct {
		File      string `json:"file"`
		Severity  string `json:"severity"`
		Message   string `json:"message"`
		Source    string `json:"source"`
		StartLine int    `json:"startLine"`
		StartCol  int    `json:"startCol"`
		EndLine   int    `json:"endLine"`
		EndCol    int    `json:"endCol"`
		Text      string `json:"text"`
		Context   []struct {
			Line int    `json:"line"`
			Text string `json:"text"`
		} `json:"context"`
		EnclosingNode *struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			StartLine int    `json:"startLine"`
			EndLine   int    `json:"endLine"`
		} `json:"enclosingNode"`
	}
	for i := range payload.Diagnostics {
		if payload.Diagnostics[i].Severity == "error" {
			hit = &payload.Diagnostics[i]
			break
		}
	}
	if hit == nil {
		t.Fatalf("no error-severity diagnostic in response: %+v", payload.Diagnostics)
	}

	// Enrichment checks: gopls flags `doesNotExist` as an identifier,
	// so we expect Text=="doesNotExist", Context with the broken line,
	// and an EnclosingNode pointing at main().
	if hit.Text != "doesNotExist" {
		t.Errorf("Text = %q, want doesNotExist", hit.Text)
	}
	var sawBrokenLine bool
	for _, c := range hit.Context {
		if strings.Contains(c.Text, "doesNotExist") {
			sawBrokenLine = true
			break
		}
	}
	if !sawBrokenLine {
		t.Errorf("Context missing the broken line: %+v", hit.Context)
	}
	if hit.EnclosingNode == nil {
		t.Errorf("EnclosingNode = nil; want enclosing func main()")
	} else if hit.EnclosingNode.Name != "main" {
		t.Errorf("EnclosingNode.Name = %q, want main", hit.EnclosingNode.Name)
	}
}

