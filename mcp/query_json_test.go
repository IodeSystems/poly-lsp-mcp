package mcp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// query --json is for a program, not a person: stdout must decode as ONE
// JSON document, and an edge row must carry what the text tree leaves to
// the reader's eye - the node address, the file, the far end and the conf
// label that says what settled the edge. No child LSP here, so the edge is
// lexical.
func TestQueryJSONPrintsOneDocumentWithNodeFileFarEndAndConf(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module q\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package q\n\nfunc Save() {}\n\nfunc Caller() {\n\tSave()\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newQueryServer(t, dir)

	var out bytes.Buffer
	if err := s.QueryJSON(`#'a.go#Save'::in.call`, 0, 0, "", &out); err != nil {
		t.Fatal(err)
	}

	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	var doc struct {
		Matches []struct {
			Node string   `json:"node"`
			File string   `json:"file"`
			Type string   `json:"type"`
			From []string `json:"from"`
			To   []string `json:"to"`
			Conf string   `json:"conf"`
		} `json:"matches"`
		Total  *int `json:"total"`
		Offset *int `json:"offset"`
	}
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("stdout is not a JSON document: %v\n%s", err, out.String())
	}
	if dec.More() {
		t.Fatalf("stdout holds more than one JSON document:\n%s", out.String())
	}
	if doc.Total == nil || *doc.Total != 1 || doc.Offset == nil || *doc.Offset != 0 {
		t.Fatalf("want total 1 and offset 0; got:\n%s", out.String())
	}
	if len(doc.Matches) != 1 {
		t.Fatalf("want the one call edge into Save; got:\n%s", out.String())
	}
	m := doc.Matches[0]
	if m.Node != "a.go@6" {
		t.Errorf("node = %q, want the call site a.go@6", m.Node)
	}
	if m.File != "a.go" {
		t.Errorf("file = %q, want a.go", m.File)
	}
	if m.Type != "::in.call" {
		t.Errorf("type = %q, want ::in.call", m.Type)
	}
	if len(m.From) != 1 || m.From[0] != "a.go#Caller" || len(m.To) != 0 {
		t.Errorf("far end: from = %q, to = %q; want from [a.go#Caller]", m.From, m.To)
	}
	if m.Conf != "lexical" {
		t.Errorf("conf = %q, want lexical (no child LSP)", m.Conf)
	}
}
