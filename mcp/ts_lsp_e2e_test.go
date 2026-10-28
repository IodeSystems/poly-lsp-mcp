package mcp

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// typescript-language-server publishes no document version. With edit
// validation on, a broken edit whose error needs the project (a type from
// another file, through tsconfig.json) is reverted. A valid edit is kept:
// lsp-unconfirmed by default, validated with trust_clean_diagnostics.
//
// The default does not trust it (docs/known-limits.md). main.ts keeps an
// unused function so every publish has a hint: typescript-language-server
// publishes nothing for a file whose diagnostics stay empty.
func TestTSEditValidateWithTypeScriptLanguageServer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live typescript-language-server e2e in -short")
	}
	if _, err := exec.LookPath("typescript-language-server"); err != nil {
		t.Skip("typescript-language-server not on PATH")
	}
	// A failure then shows the publish/progress sequence the edit saw.
	t.Setenv(multiplex.TraceEnv, "1")
	for _, l := range config.Default().Languages {
		if l.Name == "typescript" && l.LSP.TrustsCleanDiagnostics() {
			t.Fatal("default typescript LSP config trusts clean diagnostics")
		}
	}
	for _, trust := range []bool{false, true} {
		name := "default"
		if trust {
			name = "trusted"
		}
		t.Run(name, func(t *testing.T) { tsEditValidate(t, trust) })
	}
}

func tsEditValidate(t *testing.T, trust bool) {
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
	write("tsconfig.json", `{"compilerOptions":{"strict":true,"noEmit":true},"include":["src"]}`+"\n")
	write("src/user.ts", "export interface User {\n  name: string;\n}\n\nexport function load(): User {\n  return { name: \"a\" };\n}\n")
	head := "import { load } from \"./user\";\n\nfunction unused() {}\n\n"
	orig := head + "export const who: string = load().name;\n"
	write("src/main.ts", orig)
	mainPath := filepath.Join(dir, "src/main.ts")

	cfg := config.Default()
	for i := range cfg.Languages {
		if cfg.Languages[i].Name == "typescript" {
			cfg.Languages[i].LSP.TrustCleanDiagnostics = &trust
		}
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetValidateEdits(true)
	srv.SetDiagnosticWait(60 * time.Second)

	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	sess := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
	defer sess.close()
	sess.request("initialize", map[string]any{})
	sess.notify("notifications/initialized", map[string]any{})

	edit := func(text string) map[string]any {
		t.Helper()
		cur, err := os.ReadFile(mainPath)
		if err != nil {
			t.Fatal(err)
		}
		r := sess.callTool("node_edit", map[string]any{"node": "src/main.ts", "oldText": string(cur), "newText": text})
		var m map[string]any
		if err := json.Unmarshal([]byte(r.Content[0].Text), &m); err != nil {
			t.Fatalf("decode edit response: %v (%s)", err, r.Content[0].Text)
		}
		return m
	}

	// The project's first publishes. An edit before them is answered by
	// the open's first publish only, which is skipped (waitForEditedText):
	// it times out.
	waitPublished(t, srv, mainPath)
	broken := head + "export const who: number = load().name;\n"
	m := edit(broken)
	if m["rejected"] != true || m["reverted"] != true {
		t.Fatalf("a broken edit was not reverted: %v", m)
	}
	if got, _ := os.ReadFile(mainPath); string(got) != orig {
		t.Fatalf("file not restored after revert:\n got: %q\nwant: %q", got, orig)
	}
	// The same broken edit again at once is reverted too: the revert
	// waited for the reverted text's publish, so the retry's error
	// baseline does not hold the reverted error.
	if m := edit(broken); m["rejected"] != true || m["reverted"] != true {
		t.Fatalf("the broken edit sent again after its revert was not reverted: %v", m)
	}
	if got, _ := os.ReadFile(mainPath); string(got) != orig {
		t.Fatalf("file not restored after the second revert:\n got: %q\nwant: %q", got, orig)
	}

	good := head + "export const who: string = load().name.trim();\n"
	m = edit(good)
	if m["rejected"] == true {
		t.Fatalf("a valid edit was rejected: %v", m)
	}
	if trust {
		if m["validated"] != true {
			t.Fatalf("trusted: a valid edit was not validated: %v", m)
		}
	} else if m["validated"] != false || m["validationSkipped"] != "lsp-unconfirmed" {
		t.Fatalf("default: want validated:false validationSkipped:lsp-unconfirmed, got %v", m)
	}
	if got, _ := os.ReadFile(mainPath); string(got) != good {
		t.Fatalf("valid edit did not land:\n got: %q\nwant: %q", got, good)
	}
}
