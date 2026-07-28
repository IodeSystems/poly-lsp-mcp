package symbols

import (
	"strings"
	"testing"
)

// TestSignatureRefactorParityAcrossLanguages exercises the whole loop for
// every language that claims signature support: locate the callable at a
// position, rewrite name + parameters + return type, and resolve its call
// sites. Before these arms existed, java/kotlin/groovy/c/cpp indexed and
// answered queries but silently did nothing for refactor:{params,return}.
func TestSignatureRefactorParityAcrossLanguages(t *testing.T) {
	cases := []struct {
		lang      string
		src       string
		line, col int
		param     string // type for the two new params
		result    string
		wantLine  string // the rewritten declaration
		wantCalls int
	}{
		{
			lang: "java",
			src: "class A {\n" +
				"    int f(int x, String y) { return 1; }\n" +
				"    void run() { f(1, \"a\"); f(2, \"b\"); }\n" +
				"}\n",
			line: 2, col: 9, param: "int", result: "long",
			wantLine: "long f2(int a, int b) { return 1; }", wantCalls: 2,
		},
		{
			lang: "kotlin",
			src: "fun f(x: Int, y: String): Int { return 1 }\n" +
				"fun run() { f(1, \"a\"); f(2, \"b\") }\n",
			line: 1, col: 5, param: "Int", result: "Long",
			wantLine: "fun f2(a: Int, b: Int): Long { return 1 }", wantCalls: 2,
		},
		{
			lang: "groovy",
			src: "class A {\n" +
				"    int f(int x, String y) { return 1 }\n" +
				"    void run() { f(1, \"a\") }\n" +
				"}\n",
			line: 2, col: 9, param: "int", result: "long",
			wantLine: "long f2(int a, int b) { return 1 }", wantCalls: 1,
		},
		{
			lang: "c",
			src: "int f(int x, char *y) { return 1; }\n" +
				"void run(void) { f(1, \"a\"); f(2, \"b\"); }\n",
			line: 1, col: 5, param: "int", result: "long",
			wantLine: "long f2(int a, int b) { return 1; }", wantCalls: 2,
		},
		{
			// C++ methods reached through `->` and `.`, which are
			// field_expression call sites rather than bare identifiers.
			lang: "cpp",
			src: "class W {\n" +
				"public:\n" +
				"    int area(int x, int y) { return x; }\n" +
				"};\n" +
				"void run(W *w, W &r) { w->area(1, 2); r.area(3, 4); }\n",
			line: 3, col: 9, param: "int", result: "double",
			wantLine: "double area2(int a, int b) { return x; }", wantCalls: 2,
		},
	}

	for _, c := range cases {
		t.Run(c.lang, func(t *testing.T) {
			src := []byte(c.src)
			name := "f"
			if c.lang == "cpp" {
				name = "area"
			}
			sig, err := FindFunctionSignature(c.lang, src, c.line, c.col)
			if err != nil {
				t.Fatalf("FindFunctionSignature: %v", err)
			}
			if sig == nil {
				t.Fatalf("no signature found at %d:%d", c.line, c.col)
			}
			out, n, err := RewriteSignature(src, sig, SignatureOps{
				Rename: name + "2",
				Params: []Param{{Name: "a", Type: c.param}, {Name: "b", Type: c.param}},
				Return: c.result,
			})
			if err != nil {
				t.Fatalf("RewriteSignature: %v", err)
			}
			if n != 3 {
				t.Errorf("got %d edits, want 3 (rename + params + return)", n)
			}
			if !strings.Contains(string(out), c.wantLine) {
				t.Errorf("rewrite produced:\n%s\nwant a line containing:\n  %s", out, c.wantLine)
			}
			sites, err := FindCallSites(c.lang, src, name)
			if err != nil {
				t.Fatalf("FindCallSites: %v", err)
			}
			if len(sites) != c.wantCalls {
				t.Errorf("got %d call sites, want %d: %+v", len(sites), c.wantCalls, sites)
			}
			for _, s := range sites {
				if s.Skipped != "" {
					t.Errorf("call site unexpectedly skipped (%s): %+v", s.Skipped, s)
				}
			}
		})
	}
}

// TestCppQualifiedRenameKeepsScope pins a bug found while exercising the
// arm: cInnermostDeclarator returns the whole `Widget::area` for an
// out-of-line definition, so a rename spanning it deleted the scope and
// silently detached the definition from its class.
func TestCppQualifiedRenameKeepsScope(t *testing.T) {
	src := []byte("int Widget::area(int x) const { return x; }\n")
	sig, err := FindFunctionSignature("cpp", src, 1, 13)
	if err != nil || sig == nil {
		t.Fatalf("FindFunctionSignature: sig=%v err=%v", sig, err)
	}
	out, _, err := RewriteSignature(src, sig, SignatureOps{Rename: "size"})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "Widget::size") {
		t.Errorf("rename dropped the class scope: %q", got)
	}
}

// TestZeroValuesPerLanguage covers the placeholder a call site gets when
// a refactor ADDS a parameter.
func TestZeroValuesPerLanguage(t *testing.T) {
	cases := []struct{ lang, typ, want string }{
		{"java", "int", "0"}, {"java", "boolean", "false"}, {"java", "String", "null"},
		{"kotlin", "Int", "0"}, {"kotlin", "String", `""`}, {"kotlin", "Foo?", "null"},
		{"kotlin", "Foo", "null"},
		{"groovy", "int", "0"}, {"groovy", "String", "null"},
		{"c", "int", "0"}, {"c", "char *", "NULL"},
		{"cpp", "bool", "false"},
	}
	for _, c := range cases {
		if got := ZeroValue(c.lang, c.typ); got != c.want {
			t.Errorf("ZeroValue(%s, %q) = %q, want %q", c.lang, c.typ, got, c.want)
		}
	}
}
