package mcp

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// TestCrossLanguageDiagnosticOnGeneratedStub closes the Phase 5 loop:
// the gat-greeter fixture has a `server/` directory with hand-written
// Go stubs that mirror greeter.proto (@ref-linked), and a main.go
// that uses them. The test lays a copy of those Go files plus the
// proto in a fresh module, spawns gopls via the MCP path, and edits
// main.go to reference an undefined field on HelloResponse.
//
// gopls publishes the resulting type error; poly-lsp-mcp's diagnostic
// enrichment then surfaces:
//
//   - text == the offending token
//   - context that spans the broken line
//   - enclosingNode pointing at func Run()
//   - the @ref-linked declared site for HelloResponse (which lives in
//     the proto AND, transitively via the comment scanner, points at
//     the Go type) is in the references list when the broken token
//     happens to be one of the indexed names.
//
// This is the realistic version of TestNodeEditSurfacesGoplsDiagnostics
// — multi-file Go module instead of a single throwaway file, with a
// proto file in the same workspace exercising the comment scanner
// alongside the LSP diagnostic path.
//
// Skipped under -short and when gopls / go are missing.
func TestCrossLanguageDiagnosticOnGeneratedStub(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-language diagnostic e2e skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}

	fixtureDir := crossLangFixtureRoot(t)
	for _, p := range []string{"greeter.proto", "server/types.go", "server/main.go"} {
		if _, err := os.Stat(filepath.Join(fixtureDir, p)); err != nil {
			t.Skipf("fixture missing %s: %v", p, err)
		}
	}

	// Synthesize a flat Go module: proto + Go files all in the root.
	// Keeps gopls's job simple (no nested packages, no external deps).
	// The @ref markers in types.go use a relative path that resolves
	// differently here than in the fixture, but the scanner only
	// extracts the symbol tail — paths are informational.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for src, dst := range map[string]string{
		"greeter.proto":   "greeter.proto",
		"server/types.go": "types.go",
		"server/main.go":  "main.go",
	} {
		data, err := os.ReadFile(filepath.Join(fixtureDir, src))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dst), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	// gopls on a multi-file module needs more headroom for first
	// publish than the throwaway single-file test.
	srv.SetDiagnosticWait(12 * time.Second)

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

	// Sanity: the comment scanner picked up the @ref linkage between
	// types.go and greeter.proto. node_references on HelloResponse
	// from types.go (the declaration site) should return BOTH the Go
	// declaration AND the proto's @ref-anchored site.
	refsResp := sess.callTool("node_references", map[string]any{"node": "types.go#HelloResponse"})
	var refPayload struct {
		Matches []wireFileMatch `json:"matches"`
	}
	json.Unmarshal([]byte(refsResp.Content[0].Text), &refPayload)
	var sawGoDecl, sawProtoRef bool
	for _, m := range refPayload.Matches {
		switch filepath.Base(m.File) {
		case "types.go":
			sawGoDecl = true
		case "greeter.proto":
			for _, h := range m.Hash {
				if h.Class == "declared" {
					sawProtoRef = true
				}
			}
		}
	}
	if !sawGoDecl {
		t.Errorf("expected node_references hit in types.go; refs=%s", refsResp.Content[0].Text)
	}
	if !sawProtoRef {
		t.Errorf("expected @ref-anchored declared site in greeter.proto; refs=%s", refsResp.Content[0].Text)
	}

	// Replace the whole main.go body with one that references a field
	// that doesn't exist on HelloResponse — the kind of break a
	// downstream consumer would hit if the proto schema dropped a
	// field. gopls flags it with the missing field name in the
	// diagnostic range.
	broken := `package server

import "fmt"

func Run() {
	req := HelloRequest{
		Name: "world",
		Mood: MoodHappy,
	}
	resp := Hello(req)
	fmt.Println(resp.Bogus)
}
`
	// Need the existing line/col bounds of main.go to do a whole-file
	// replace. Read it back from disk to compute end position.
	original, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	endLine, endCol := contentEndPositionForTest(original)

	r := sess.callTool("node_edit", map[string]any{
		"file":      "main.go",
		"startLine": 1, "startCol": 1,
		"endLine": endLine, "endCol": endCol,
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
			References []struct {
				Name       string `json:"name"`
				File       string `json:"file"`
				Confidence string `json:"confidence"`
			} `json:"references"`
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

	var hitIdx = -1
	for i, d := range payload.Diagnostics {
		if d.Severity != "error" {
			continue
		}
		// Only consider main.go diagnostics — the broken field reference
		// is in there. gopls might also publish for sibling files in
		// the same package; our response keeps everything for the
		// edited URI only.
		if filepath.Base(d.File) != "main.go" {
			continue
		}
		hitIdx = i
		break
	}
	if hitIdx < 0 {
		t.Fatalf("no error-severity diagnostic for main.go: %+v", payload.Diagnostics)
	}
	hit := payload.Diagnostics[hitIdx]
	// `Bogus` is the field we referenced that doesn't exist; gopls
	// pins the diagnostic range on it. The enrichment lifts the
	// exact token into `text`.
	if hit.Text != "Bogus" {
		t.Errorf("text = %q, want Bogus", hit.Text)
	}
	var sawBrokenLine bool
	for _, c := range hit.Context {
		if strings.Contains(c.Text, "Bogus") {
			sawBrokenLine = true
			break
		}
	}
	if !sawBrokenLine {
		t.Errorf("context missing broken line: %+v", hit.Context)
	}
	if hit.EnclosingNode == nil {
		t.Errorf("enclosingNode = nil; want enclosing func Run")
	} else if hit.EnclosingNode.Name != "Run" {
		t.Errorf("enclosingNode.Name = %q, want Run", hit.EnclosingNode.Name)
	}
}

// TestSiblingDiagnosticsRollup proves the gopls-publishes-cascade
// path: edit types.go to drop the Greeting field, and main.go's
// `resp.Greeting` reference is flagged. With the default sibling
// rollup ON, the response contains the main.go diagnostic. With
// siblingDiagnostics=false, the response stays scoped to the edited
// URI and the cascade is invisible.
func TestSiblingDiagnosticsRollup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}

	fixtureDir := crossLangFixtureRoot(t)
	for _, p := range []string{"server/types.go", "server/main.go"} {
		if _, err := os.Stat(filepath.Join(fixtureDir, p)); err != nil {
			t.Skipf("fixture missing %s: %v", p, err)
		}
	}

	// Two passes, each against a fresh tempdir: default sibling
	// rollup, then explicit opt-out.
	t.Run("default-includes-siblings", func(t *testing.T) {
		runSiblingCase(t, fixtureDir, nil, true)
	})
	t.Run("opt-out-scopes-to-edited-uri", func(t *testing.T) {
		f := false
		runSiblingCase(t, fixtureDir, &f, false)
	})
}

func runSiblingCase(t *testing.T, fixtureDir string, sibling *bool, wantSiblingDiag bool) {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for src, dst := range map[string]string{
		"server/types.go": "types.go",
		"server/main.go":  "main.go",
	} {
		data, err := os.ReadFile(filepath.Join(fixtureDir, src))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dst), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetDiagnosticWait(12 * time.Second)

	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	sess := &mcpSession{
		t: t, srv: srv,
		srvIn: cOut, clientR: json.NewDecoder(cIn),
		clientW: cOut, done: done,
	}
	defer sess.close()

	sess.request("initialize", map[string]any{})
	sess.notify("notifications/initialized", map[string]any{})

	// Rewrite types.go to drop HelloResponse.Greeting. main.go uses
	// resp.Greeting, so gopls flags main.go on the next package
	// recheck.
	broken := `package server

type Mood int32

const (
	MoodUnspecified Mood = 0
	MoodHappy       Mood = 1
	MoodGrumpy      Mood = 2
)

type HelloRequest struct {
	Name string
	Mood Mood
}

type HelloResponse struct {
}

func Hello(req HelloRequest) HelloResponse {
	return HelloResponse{}
}
`
	original, err := os.ReadFile(filepath.Join(dir, "types.go"))
	if err != nil {
		t.Fatal(err)
	}
	endLine, endCol := contentEndPositionForTest(original)

	args := map[string]any{
		"file":      "types.go",
		"startLine": 1, "startCol": 1,
		"endLine": endLine, "endCol": endCol,
		"newText": broken,
	}
	if sibling != nil {
		args["siblingDiagnostics"] = *sibling
	}
	r := sess.callTool("node_edit", args)
	if r.IsError {
		t.Fatalf("node_edit errored: %+v", r.Content)
	}

	var payload struct {
		DiagnosticsAvailable bool `json:"diagnosticsAvailable"`
		Diagnostics          []struct {
			File     string `json:"file"`
			Severity string `json:"severity"`
			Message  string `json:"message"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal([]byte(r.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var sawMainGo bool
	for _, d := range payload.Diagnostics {
		if filepath.Base(d.File) == "main.go" && d.Severity == "error" {
			sawMainGo = true
			break
		}
	}
	if wantSiblingDiag && !sawMainGo {
		t.Errorf("expected main.go sibling diagnostic; got %+v", payload.Diagnostics)
	}
	if !wantSiblingDiag && sawMainGo {
		t.Errorf("opt-out should scope to edited URI; got main.go diagnostic in %+v", payload.Diagnostics)
	}
}

// contentEndPositionForTest is a local copy of contentEndPosition (in
// tools.go) so this test file doesn't depend on internals.
func contentEndPositionForTest(content []byte) (int, int) {
	line, col := 1, 1
	for _, b := range content {
		if b == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

func crossLangFixtureRoot(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(here), ".."))
	return filepath.Join(repoRoot, "testdata", "fixtures", "gat-greeter")
}
