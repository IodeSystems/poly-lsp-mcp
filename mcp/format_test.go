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

// formatSession is a server on a one-file Go workspace whose child is the
// fake diag LSP (versioned, trusted) with formatting $FAKE_FMT=fmtMode,
// go's format_on_edit set to formatOn, the client's switch prefs, and
// validation on.
func formatSession(t *testing.T, fmtMode string, formatOn bool, prefs LSPPrefs, wait time.Duration, text string) (*mcpSession, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	trust := true
	cfg := config.Default()
	for i := range cfg.Languages {
		if cfg.Languages[i].Name == "go" {
			cfg.Languages[i].LSP = &config.LSP{Cmd: exe, Args: []string{fakeDiagLSPArg},
				Env: []string{"FAKE_DIAG_MODE=versioned", "FAKE_FMT=" + fmtMode}, TrustCleanDiagnostics: &trust}
			cfg.Languages[i].FormatOnEdit = formatOn
		}
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetValidateEdits(true)
	srv.SetDiagnosticWait(wait)
	srv.SetLSPPrefs(prefs)
	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	s := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
	t.Cleanup(s.close)
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	time.Sleep(300 * time.Millisecond) // the open's publish lands
	return s, path
}

const uglyMain = "package main\n\nvar a = UGLY\n\nfunc main() {}\n"

// addB adds the line var b = UGLY after var a: the edit changes that
// line only.
const uglyMainB = "package main\n\nvar a = UGLY\nvar b = UGLY\n\nfunc main() {}\n"

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Range formatting declared: only the lines the edit changed are
// formatted, and the result returns them as the file holds them now.
func TestFormatOnEditRange(t *testing.T) {
	s, path := formatSession(t, "range", true, LSPPrefs{}, 5*time.Second, uglyMain)
	m := editMain(t, s, path, uglyMainB)
	if m["validated"] != true || m["formatted"] != "range" {
		t.Fatalf("want validated, formatted:range: %v", m)
	}
	want := "package main\n\nvar a = UGLY\nvar b = pretty\n\nfunc main() {}\n"
	if got := readFile(t, path); got != want {
		t.Fatalf("file:\n%q\nwant\n%q", got, want)
	}
	if m["formattedText"] != "var b = pretty\n" {
		t.Errorf("formattedText = %q", m["formattedText"])
	}
	if lines, _ := m["formattedLines"].([]any); len(lines) != 2 || lines[0] != 4.0 || lines[1] != 4.0 {
		t.Errorf("formattedLines = %v", m["formattedLines"])
	}
	// The next edit, based on formattedText, matches.
	r := s.callTool("node_edit", map[string]any{"node": "main.go", "oldText": "var b = pretty\n", "newText": "var b = 2\n"})
	if r.IsError {
		t.Fatalf("an edit based on formattedText failed: %s", r.Content[0].Text)
	}
}

// Only whole-file formatting declared: the file is formatted, the UGLY
// the edit did not touch included.
func TestFormatOnEditWholeFile(t *testing.T) {
	s, path := formatSession(t, "file", true, LSPPrefs{}, 5*time.Second, uglyMain)
	m := editMain(t, s, path, uglyMainB)
	if m["validated"] != true || m["formatted"] != "file" {
		t.Fatalf("want validated, formatted:file: %v", m)
	}
	want := "package main\n\nvar a = pretty\nvar b = pretty\n\nfunc main() {}\n"
	if got := readFile(t, path); got != want {
		t.Fatalf("file:\n%q\nwant\n%q", got, want)
	}
	if m["formattedText"] != "var b = pretty\n" {
		t.Errorf("formattedText = %q", m["formattedText"])
	}
}

// Off: by default (no format_on_edit), by the client's switch over
// format_on_edit, or with the language's child off for the client.
// Per client on turns it on over a default off.
func TestFormatOnEditSwitches(t *testing.T) {
	cases := []struct {
		name     string
		formatOn bool
		prefs    LSPPrefs
		want     string // "" none, "range", or "skip:<reason>"
	}{
		{"default off", false, LSPPrefs{}, ""},
		{"client off", true, LSPPrefs{FormatOff: []string{"go"}}, ""},
		{"client on", false, LSPPrefs{FormatOn: []string{"go"}}, "range"},
		{"lsp off", true, LSPPrefs{Off: []string{"go"}}, "skip:lsp-off"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, path := formatSession(t, "range", c.formatOn, c.prefs, 5*time.Second, uglyMain)
			m := editMain(t, s, path, uglyMainB)
			if m["rejected"] == true {
				t.Fatalf("rejected: %v", m)
			}
			switch {
			case c.want == "":
				if _, ok := m["formatted"]; ok {
					t.Errorf("formatted with formatting off: %v", m)
				}
				if _, ok := m["formatSkipped"]; ok {
					t.Errorf("formatSkipped with formatting off: %v", m)
				}
				if got := readFile(t, path); got != uglyMainB {
					t.Errorf("file changed: %q", got)
				}
			case strings.HasPrefix(c.want, "skip:"):
				if m["formatSkipped"] != strings.TrimPrefix(c.want, "skip:") || m["formatted"] != nil {
					t.Errorf("want formatSkipped %s: %v", c.want, m)
				}
				if got := readFile(t, path); got != uglyMainB {
					t.Errorf("file changed: %q", got)
				}
			default:
				if m["formatted"] != c.want {
					t.Errorf("want formatted %s: %v", c.want, m)
				}
			}
		})
	}
}

// A server with no formatting capability: skipped, the edit kept.
func TestFormatOnEditNoCapability(t *testing.T) {
	s, path := formatSession(t, "", true, LSPPrefs{}, 5*time.Second, uglyMain)
	m := editMain(t, s, path, uglyMainB)
	if m["validated"] != true || m["formatSkipped"] != "no-capability" {
		t.Fatalf("want validated, formatSkipped:no-capability: %v", m)
	}
	if got := readFile(t, path); got != uglyMainB {
		t.Errorf("file: %q", got)
	}
}

// A reverted edit is never formatted.
func TestFormatOnEditNotOnReverted(t *testing.T) {
	s, path := formatSession(t, "range", true, LSPPrefs{}, 5*time.Second, uglyMain)
	m := editMain(t, s, path, "package main\n\n// BROKEN\nvar b = UGLY\n\nfunc main() {}\n")
	if m["rejected"] != true || m["reverted"] != true {
		t.Fatalf("broken edit not reverted: %v", m)
	}
	if m["formatted"] != nil || m["formatSkipped"] != nil {
		t.Errorf("a reverted edit reports formatting: %v", m)
	}
	if got := readFile(t, path); got != uglyMain {
		t.Errorf("file: %q", got)
	}
}

// A format error or timeout keeps the accepted, unformatted edit.
func TestFormatOnEditFailureKeepsEdit(t *testing.T) {
	for _, c := range []struct {
		mode, want string
		wait       time.Duration
	}{
		{"error", "error", 5 * time.Second},
		{"slow", "timeout", 1500 * time.Millisecond},
	} {
		t.Run(c.mode, func(t *testing.T) {
			s, path := formatSession(t, c.mode, true, LSPPrefs{}, c.wait, uglyMain)
			m := editMain(t, s, path, uglyMainB)
			if m["rejected"] == true || m["validated"] != true || m["formatSkipped"] != c.want {
				t.Fatalf("want kept, validated, formatSkipped:%s: %v", c.want, m)
			}
			if note, _ := m["formatNote"].(string); !strings.Contains(note, "edit kept") {
				t.Errorf("formatNote = %q", note)
			}
			if got := readFile(t, path); got != uglyMainB {
				t.Errorf("file: %q", got)
			}
		})
	}
}

// A batch commit formats each touched file once, after the commit.
func TestFormatOnEditBatchCommit(t *testing.T) {
	s, path := formatSession(t, "range", true, LSPPrefs{}, 5*time.Second, uglyMain)
	other := filepath.Join(filepath.Dir(path), "other.go")
	if err := os.WriteFile(other, []byte("package main\n\nvar c = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	call := func(args map[string]any) map[string]any {
		t.Helper()
		r := s.callTool("node_edit", args)
		var m map[string]any
		if err := json.Unmarshal([]byte(r.Content[0].Text), &m); err != nil {
			t.Fatalf("decode: %v (%s)", err, r.Content[0].Text)
		}
		return m
	}
	if m := call(map[string]any{"node": "main.go", "oldText": "var a = UGLY\n", "newText": "var a = UGLY\nvar b = UGLY\n", "commit": false}); m["staged"] != true {
		t.Fatalf("stage: %v", m)
	}
	if got := readFile(t, path); got != uglyMainB {
		t.Fatalf("a staged edit was formatted: %q", got)
	}
	m := call(map[string]any{"node": "other.go", "oldText": "var c = 1\n", "newText": "var c = UGLY\n"})
	if m["committed"] != true {
		t.Fatalf("commit: %v", m)
	}
	files, _ := m["formattedFiles"].(map[string]any)
	if files["main.go"] != "range" || files["other.go"] != "range" {
		t.Fatalf("formattedFiles = %v (%v)", m["formattedFiles"], m)
	}
	if got := readFile(t, path); got != "package main\n\nvar a = UGLY\nvar b = pretty\n\nfunc main() {}\n" {
		t.Errorf("main.go: %q", got)
	}
	if got := readFile(t, other); got != "package main\n\nvar c = pretty\n" {
		t.Errorf("other.go: %q", got)
	}
}

// A kept refactor (params) formats the file it changed.
func TestFormatOnEditRefactor(t *testing.T) {
	s, path := formatSession(t, "range", true, LSPPrefs{}, 5*time.Second,
		"package main\n\nvar a = UGLY\n\nfunc f() int { return UGLY }\n")
	r := s.callTool("node_edit", map[string]any{"node": "main.go#f", "params": []map[string]any{{"name": "x", "type": "int"}}})
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Content[0].Text), &m); err != nil {
		t.Fatalf("decode: %v (%s)", err, r.Content[0].Text)
	}
	files, _ := m["formattedFiles"].(map[string]any)
	if r.IsError || files["main.go"] != "range" {
		t.Fatalf("want formattedFiles main.go:range: %v", m)
	}
	if got := readFile(t, path); got != "package main\n\nvar a = UGLY\n\nfunc f(x int) int { return pretty }\n" {
		t.Errorf("file: %q", got)
	}
}

// Real gopls declares documentFormattingProvider only (0.24): a kept
// edit is formatted whole-file.
func TestFormatOnEditGopls(t *testing.T) {
	if testing.Short() {
		t.Skip("gopls e2e skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	for i := range cfg.Languages {
		if cfg.Languages[i].Name == "go" {
			cfg.Languages[i].FormatOnEdit = true
		}
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetValidateEdits(true)
	srv.SetDiagnosticWait(10 * time.Second)
	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	s := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
	t.Cleanup(s.close)
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	m := editMain(t, s, path, "package main\n\nfunc main() {\nx:=1\n_ =    x\n}\n")
	if m["rejected"] == true || m["formatted"] != "file" {
		t.Fatalf("want kept, formatted:file: %v", m)
	}
	want := "package main\n\nfunc main() {\n\tx := 1\n\t_ = x\n}\n"
	if got := readFile(t, path); got != want {
		t.Fatalf("file:\n%q\nwant\n%q", got, want)
	}
	if m["formattedText"] != "func main() {\n\tx := 1\n\t_ = x\n" { // the closing brace line was not changed
		t.Errorf("formattedText = %q", m["formattedText"])
	}
}

func TestMapOffset(t *testing.T) {
	text := []byte("aaUGLYbbUGLYcc")
	edits := []byteEdit{{2, 6, "pretty"}, {8, 12, "x"}}
	if got := string(applyByteEdits(text, edits)); got != "aaprettybbxcc" {
		t.Fatalf("apply: %q", got)
	}
	for _, c := range []struct {
		off   int
		toEnd bool
		want  int
	}{
		{0, false, 0}, {2, false, 2}, {4, false, 2}, {4, true, 8}, {6, false, 8}, {7, false, 9}, {10, true, 11}, {14, false, 13},
	} {
		if got := mapOffset(edits, c.off, c.toEnd); got != c.want {
			t.Errorf("mapOffset(%d, %v) = %d, want %d", c.off, c.toEnd, got, c.want)
		}
	}
	before := []byte("a\nb\nc\n")
	join := []byteEdit{{1, 2, " "}} // a b on one line
	if got := mapLine(before, join, 3); got != 2 {
		t.Errorf("mapLine = %d, want 2", got)
	}
}
