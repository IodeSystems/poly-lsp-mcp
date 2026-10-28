package multiplex

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"
)

// A publish cut at traceMax is logged with its version and diagnostic
// count: sourcekit-lsp can put the version after the diagnostics.
func TestTraceCutPublishKeepsVersion(t *testing.T) {
	t.Setenv(TraceEnv, "1")
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	diag := `{"severity":1,"message":"` + strings.Repeat("x", traceMax) + `"}`
	traceChild("swift", "<-", "textDocument/publishDiagnostics", json.RawMessage(`{"uri":"file:///a.swift","diagnostics":[`+diag+`,`+diag+`],"version":4}`))
	traceChild("swift", "<-", "textDocument/publishDiagnostics", json.RawMessage(`{"uri":"file:///a.swift","diagnostics":[`+diag+`]}`))
	out := buf.String()
	for _, want := range []string{"... (version 4, 2 diagnostics)", "... (version none, 1 diagnostics)"} {
		if !strings.Contains(out, want) {
			t.Errorf("trace lacks %q:\n%s", want, out)
		}
	}
}
