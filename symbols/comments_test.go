package symbols

import (
	"reflect"
	"testing"
)

func TestExtractCommentRefsSeeAndLink(t *testing.T) {
	src := []byte(`/**
 * Process a user.
 *
 * @see UserService
 * @see {@link UserService}
 * @see Class#methodName
 * @see path/file.ts!Symbol
 */
function process() {}
`)
	got := ExtractCommentRefs(src)
	if len(got) != 4 {
		t.Fatalf("got %d refs, want 4: %+v", len(got), got)
	}
	wantNames := []string{"UserService", "UserService", "methodName", "Symbol"}
	for i, w := range wantNames {
		if got[i].Name != w {
			t.Errorf("ref[%d].Name = %q, want %q", i, got[i].Name, w)
		}
		if got[i].Confidence != ConfidenceComment {
			t.Errorf("ref[%d].Confidence = %d, want Comment", i, got[i].Confidence)
		}
	}
}

func TestExtractCommentRefsAtRefHard(t *testing.T) {
	src := []byte(`// @ref server/main.go:listProjects
type Query {
  listProjects: ListProjectsOutput
}

# @ref UserService
`)
	got := ExtractCommentRefs(src)
	if len(got) != 2 {
		t.Fatalf("got %d refs, want 2: %+v", len(got), got)
	}
	wantNames := []string{"listProjects", "UserService"}
	for i, w := range wantNames {
		if got[i].Name != w {
			t.Errorf("ref[%d].Name = %q, want %q", i, got[i].Name, w)
		}
		if got[i].Confidence != ConfidenceDeclared {
			t.Errorf("ref[%d].Confidence = %d, want Declared", i, got[i].Confidence)
		}
	}
}

func TestExtractCommentRefsXRefExtensionKey(t *testing.T) {
	src := []byte(`openapi: 3.0.3
paths:
  /projects:
    get:
      operationId: listProjects
      x-ref: server/main.go:listProjects
      x-poly-lsp-mcp-source: server/main.go:listProjects
      x-source: "server/main.go:listProjects"
`)
	got := ExtractCommentRefs(src)
	if len(got) < 3 {
		t.Fatalf("got %d refs, want >= 3: %+v", len(got), got)
	}
	for _, r := range got {
		if r.Name != "listProjects" {
			t.Errorf("ref name = %q, want listProjects", r.Name)
		}
		if r.Confidence != ConfidenceDeclared {
			t.Errorf("x-ref should be Declared, got %d", r.Confidence)
		}
	}
}

func TestExtractCommentRefsSkipsURLs(t *testing.T) {
	src := []byte(`// @see https://example.com/foo
// @ref https://example.com/bar
`)
	got := ExtractCommentRefs(src)
	if len(got) != 0 {
		t.Errorf("URLs should not produce refs, got: %+v", got)
	}
}

func TestExtractCommentRefsTracksLineColumn(t *testing.T) {
	src := []byte("one\n\n@see Foo\n")
	got := ExtractCommentRefs(src)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1: %+v", len(got), got)
	}
	// "@see Foo" lives on line 3; the captured token "Foo" starts at col 6.
	want := CommentRef{Name: "Foo", Line: 3, Col: 6, Confidence: ConfidenceComment}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("got %+v, want %+v", got[0], want)
	}
}

func TestExtractCommentRefsStripsTrailingPunctuation(t *testing.T) {
	src := []byte("// @see Foo, also @see Bar.\n// @see Baz;\n")
	got := ExtractCommentRefs(src)
	names := make([]string, 0, len(got))
	for _, r := range got {
		names = append(names, r.Name)
	}
	want := []string{"Foo", "Bar", "Baz"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
}

func TestExtractCommentRefsPositionalRefSkipped(t *testing.T) {
	// @ref server/main.go:42:18 (purely positional) doesn't produce a
	// useful binding name — emitting a CommentRef under "" or "18"
	// would be worse than nothing. The implementation skips it.
	src := []byte("// @ref server/main.go:42:18\n")
	got := ExtractCommentRefs(src)
	if len(got) != 0 {
		t.Errorf("positional @ref should not produce a named ref, got: %+v", got)
	}
}

func TestSymbolFromRef(t *testing.T) {
	cases := map[string]string{
		"Foo":                     "Foo",
		"Foo.":                    "Foo",
		"Foo,":                    "Foo",
		"Class#method":            "method",
		"path/file.ts!Symbol":     "Symbol",
		"server/main.go:Symbol":   "Symbol",
		"server/main.go:42:18":    "",         // positional
		"https://example.com/foo": "",         // URL
		"":                        "",         // empty
		"42":                      "",         // numeric
		"_private":                "_private", // underscore identifiers
		// JSON-encoded description text bleeds escape sequences into
		// the (\S+) capture. The trailing-trim path must stop at the
		// first non-identifier byte rather than fall through to the
		// trailing `n` of `\n`.
		`server/main.go:Symbol\n",`: "Symbol",
		`Symbol\n`:                  "Symbol",
		`Symbol\t`:                  "Symbol",
	}
	for in, want := range cases {
		if got := symbolFromRef(in); got != want {
			t.Errorf("symbolFromRef(%q) = %q, want %q", in, got, want)
		}
	}
}
