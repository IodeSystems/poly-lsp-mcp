package symbols

import (
	"bytes"
	"testing"
)

func TestFindGoFunctionSignatureBasic(t *testing.T) {
	src := []byte("package main\n\nfunc Greet(name string, age int) (string, error) {\n\treturn name, nil\n}\n")
	//             1               2  3
	//             123456789012345  ...
	// Greet's name range is line 3, cols 6..11 (1-based inclusive,
	// 6..11 → bytes inside the file).

	sig, err := FindFunctionSignature("go", src, 4, 2) // inside the body
	if err != nil {
		t.Fatal(err)
	}
	if sig == nil {
		t.Fatal("nil signature")
	}
	if sig.Type != "function_declaration" {
		t.Errorf("Type = %q, want function_declaration", sig.Type)
	}
	if got := string(src[sig.Name.Start:sig.Name.End]); got != "Greet" {
		t.Errorf("Name slice = %q, want Greet", got)
	}
	if got := string(src[sig.Params.Start:sig.Params.End]); got != "(name string, age int)" {
		t.Errorf("Params slice = %q", got)
	}
	if got := string(src[sig.Result.Start:sig.Result.End]); got != "(string, error)" {
		t.Errorf("Result slice = %q", got)
	}
	if got := string(src[sig.BodyStart : sig.BodyStart+1]); got != "{" {
		t.Errorf("BodyStart slice = %q, want {", got)
	}
}

func TestFindGoFunctionSignatureVoidResult(t *testing.T) {
	src := []byte("package main\n\nfunc Void() {}\n")
	sig, err := FindFunctionSignature("go", src, 3, 12)
	if err != nil {
		t.Fatal(err)
	}
	if sig == nil {
		t.Fatal("nil signature")
	}
	if !sig.Result.Empty() {
		t.Errorf("Result should be empty for void function, got %+v (%q)", sig.Result,
			string(src[sig.Result.Start:sig.Result.End]))
	}
	if got := string(src[sig.BodyStart : sig.BodyStart+2]); got != "{}" {
		t.Errorf("BodyStart points at %q, want {", got)
	}
}

func TestFindGoFunctionSignatureMethod(t *testing.T) {
	src := []byte("package main\n\ntype R struct{}\n\nfunc (r R) Method(x int) error { return nil }\n")
	sig, err := FindFunctionSignature("go", src, 5, 12) // on Method
	if err != nil {
		t.Fatal(err)
	}
	if sig == nil {
		t.Fatal("nil signature")
	}
	if sig.Type != "method_declaration" {
		t.Errorf("Type = %q, want method_declaration", sig.Type)
	}
	if got := string(src[sig.Name.Start:sig.Name.End]); got != "Method" {
		t.Errorf("Name = %q", got)
	}
	if got := string(src[sig.Receiver.Start:sig.Receiver.End]); got != "(r R)" {
		t.Errorf("Receiver = %q", got)
	}
	if got := string(src[sig.Params.Start:sig.Params.End]); got != "(x int)" {
		t.Errorf("Params = %q", got)
	}
	if got := string(src[sig.Result.Start:sig.Result.End]); got != "error" {
		t.Errorf("Result = %q", got)
	}
}

func TestFindGoFunctionSignatureMissing(t *testing.T) {
	// Position outside any function declaration.
	src := []byte("package main\n\ntype X int\n")
	sig, err := FindFunctionSignature("go", src, 3, 6)
	if err != nil {
		t.Fatal(err)
	}
	if sig != nil {
		t.Errorf("expected nil for non-function position, got %+v", sig)
	}
}

func TestFindGoCallSitesIdentifierCalls(t *testing.T) {
	src := []byte(`package main

func Greet(name string) string { return name }

func use() {
	_ = Greet("a")
	_ = Greet("b")
	_ = NotGreet()
}
`)
	sites, err := FindCallSites("go", src, "Greet")
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 2 {
		t.Fatalf("got %d sites, want 2: %+v", len(sites), sites)
	}
	for _, s := range sites {
		if len(s.CurrentArgs) != 1 {
			t.Errorf("site %+v: want 1 arg", s)
		}
		if s.Skipped != "" {
			t.Errorf("site %+v: unexpected skipped reason", s)
		}
	}
}

func TestFindGoCallSitesSelectorCalls(t *testing.T) {
	src := []byte(`package main

type R struct{}

func (r R) Greet(name string) string { return name }

func use(r R) {
	_ = r.Greet("hi")
}
`)
	sites, err := FindCallSites("go", src, "Greet")
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 {
		t.Fatalf("got %d sites, want 1", len(sites))
	}
	if sites[0].CurrentArgs[0] != `"hi"` {
		t.Errorf("CurrentArgs[0] = %q", sites[0].CurrentArgs[0])
	}
}

func TestFindGoCallSitesSpreadIsSkipped(t *testing.T) {
	src := []byte(`package main

func Apply(args ...int) int { return 0 }

func use() {
	args := []int{1, 2, 3}
	_ = Apply(args...)
}
`)
	sites, err := FindCallSites("go", src, "Apply")
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 {
		t.Fatalf("got %d sites, want 1", len(sites))
	}
	if sites[0].Skipped == "" {
		t.Errorf("expected spread skip reason, got %+v", sites[0])
	}
}

func TestFindGoCallSitesEmptyArgsList(t *testing.T) {
	src := []byte(`package main

func Tick() {}

func use() {
	Tick()
}
`)
	sites, err := FindCallSites("go", src, "Tick")
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 {
		t.Fatalf("got %d sites, want 1", len(sites))
	}
	if len(sites[0].CurrentArgs) != 0 {
		t.Errorf("CurrentArgs should be empty, got %v", sites[0].CurrentArgs)
	}
	if sites[0].ArgsInnerStart != sites[0].ArgsInnerEnd {
		t.Errorf("inner range should be empty for Tick(); got %d..%d",
			sites[0].ArgsInnerStart, sites[0].ArgsInnerEnd)
	}
}

// ---------- TypeScript ----------

func TestFindTSFunctionSignatureDeclaration(t *testing.T) {
	src := []byte(`function greet(name: string, age: number): string {
	return name;
}
`)
	sig, err := FindFunctionSignature("typescript", src, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if sig == nil {
		t.Fatal("nil signature")
	}
	if sig.Language != "typescript" || sig.Type != "function_declaration" {
		t.Errorf("Language=%q Type=%q", sig.Language, sig.Type)
	}
	if got := string(src[sig.Name.Start:sig.Name.End]); got != "greet" {
		t.Errorf("Name = %q", got)
	}
	if got := string(src[sig.Params.Start:sig.Params.End]); got != "(name: string, age: number)" {
		t.Errorf("Params = %q", got)
	}
	if got := string(src[sig.Result.Start:sig.Result.End]); got != ": string" {
		t.Errorf("Result (includes `: `) = %q", got)
	}
}

func TestFindTSFunctionSignatureArrow(t *testing.T) {
	src := []byte(`const hi = (x: number): string => "hi";`)
	sig, err := FindFunctionSignature("typescript", src, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if sig == nil {
		t.Fatal("nil signature")
	}
	if sig.Type != "arrow_function" {
		t.Errorf("Type = %q, want arrow_function", sig.Type)
	}
	// Arrow function's name comes from the enclosing variable_declarator.
	if got := string(src[sig.Name.Start:sig.Name.End]); got != "hi" {
		t.Errorf("Name = %q, want hi", got)
	}
}

func TestFindTSCallSitesIdentifierAndMember(t *testing.T) {
	src := []byte(`function greet(x: string) {}
class C {
	method(y: number) {}
}
const c = new C();
greet("a");
c.method(1);
greet("b");
`)
	sites, err := FindCallSites("typescript", src, "greet")
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 2 {
		t.Errorf("greet sites: got %d, want 2", len(sites))
	}
	msites, err := FindCallSites("typescript", src, "method")
	if err != nil {
		t.Fatal(err)
	}
	if len(msites) != 1 {
		t.Errorf("method sites: got %d, want 1", len(msites))
	}
}

func TestFindTSCallSitesSpreadSkipped(t *testing.T) {
	src := []byte(`function apply(...args: number[]) {}
const xs = [1, 2, 3];
apply(...xs);
`)
	sites, err := FindCallSites("typescript", src, "apply")
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 {
		t.Fatalf("got %d sites, want 1", len(sites))
	}
	if sites[0].Skipped == "" {
		t.Errorf("expected spread skipped, got %+v", sites[0])
	}
}

// ---------- Python ----------

func TestFindPythonFunctionSignatureTyped(t *testing.T) {
	src := []byte(`def greet(name: str, age: int) -> str:
    return name
`)
	sig, err := FindFunctionSignature("python", src, 2, 5)
	if err != nil {
		t.Fatal(err)
	}
	if sig == nil {
		t.Fatal("nil signature")
	}
	if sig.Language != "python" || sig.Type != "function_definition" {
		t.Errorf("Language=%q Type=%q", sig.Language, sig.Type)
	}
	if got := string(src[sig.Name.Start:sig.Name.End]); got != "greet" {
		t.Errorf("Name = %q", got)
	}
	if got := string(src[sig.Params.Start:sig.Params.End]); got != "(name: str, age: int)" {
		t.Errorf("Params = %q", got)
	}
	if got := string(src[sig.Result.Start:sig.Result.End]); got != "str" {
		t.Errorf("Result (no `->` prefix) = %q", got)
	}
}

func TestFindPythonFunctionSignatureUntyped(t *testing.T) {
	src := []byte(`def greet(name, age):
    return name
`)
	sig, err := FindFunctionSignature("python", src, 1, 6)
	if err != nil {
		t.Fatal(err)
	}
	if sig == nil {
		t.Fatal("nil signature")
	}
	if !sig.Result.Empty() {
		t.Errorf("Result should be empty for unannotated function")
	}
}

func TestFindPythonCallSitesIdentifierAndAttribute(t *testing.T) {
	src := []byte(`def greet(x): pass
class C:
    def method(self, y): pass
c = C()
greet("a")
c.method(1)
greet("b")
`)
	sites, err := FindCallSites("python", src, "greet")
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 2 {
		t.Errorf("greet sites: got %d, want 2", len(sites))
	}
	msites, err := FindCallSites("python", src, "method")
	if err != nil {
		t.Fatal(err)
	}
	if len(msites) != 1 {
		t.Errorf("method sites: got %d, want 1", len(msites))
	}
}

func TestFindPythonCallSitesSplatSkipped(t *testing.T) {
	src := []byte(`def apply(*args, **kw): pass
apply(*[1,2], **{"k":1})
`)
	sites, err := FindCallSites("python", src, "apply")
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 {
		t.Fatalf("got %d sites, want 1", len(sites))
	}
	if sites[0].Skipped == "" {
		t.Errorf("expected splat skipped, got %+v", sites[0])
	}
}

// ---------- RewriteSignature smoke ----------

func TestRewriteSignatureTSReplacesParams(t *testing.T) {
	src := []byte(`function greet(name: string): string {
	return name;
}
`)
	sig, err := FindFunctionSignature("typescript", src, 1, 10)
	if err != nil || sig == nil {
		t.Fatalf("find: %v", err)
	}
	out, n, err := RewriteSignature(src, sig, SignatureOps{
		Params: []Param{
			{Name: "name", Type: "string"},
			{Name: "age", Type: "number"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 edit, got %d", n)
	}
	if !contains(out, "function greet(name: string, age: number): string {") {
		t.Errorf("output:\n%s", out)
	}
}

func TestRewriteSignaturePythonInsertReturn(t *testing.T) {
	src := []byte(`def greet(name):
    return name
`)
	sig, err := FindFunctionSignature("python", src, 1, 5)
	if err != nil || sig == nil {
		t.Fatalf("find: %v", err)
	}
	out, _, err := RewriteSignature(src, sig, SignatureOps{Return: "str"})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(out, "def greet(name) -> str:") {
		t.Errorf("output:\n%s", out)
	}
}

func TestRewriteSignaturePythonReplaceReturn(t *testing.T) {
	src := []byte(`def greet(name) -> int:
    return 0
`)
	sig, err := FindFunctionSignature("python", src, 1, 5)
	if err != nil || sig == nil {
		t.Fatalf("find: %v", err)
	}
	out, _, err := RewriteSignature(src, sig, SignatureOps{Return: "str"})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(out, "def greet(name) -> str:") {
		t.Errorf("output:\n%s", out)
	}
}

func TestRewriteCallSiteArgsLanguages(t *testing.T) {
	cases := []struct {
		lang     string
		current  []string
		params   []Param
		want     string
	}{
		{"go", []string{`"a"`}, []Param{{Name: "name", Type: "string"}, {Name: "age", Type: "int"}}, `"a", 0`},
		{"typescript", []string{`"a"`}, []Param{{Name: "name", Type: "string"}, {Name: "age", Type: "number"}}, `"a", 0`},
		{"python", []string{`"a"`}, []Param{{Name: "name", Type: "str"}, {Name: "items", Type: "list"}}, `"a", []`},
		{"go", []string{`"a"`, `1`, `true`}, []Param{{Name: "name", Type: "string"}}, `"a"`},
	}
	for _, c := range cases {
		got, err := RewriteCallSiteArgs(c.lang, c.current, c.params)
		if err != nil {
			t.Errorf("%s: %v", c.lang, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.lang, got, c.want)
		}
	}
}

func contains(s []byte, sub string) bool {
	return bytes.Contains(s, []byte(sub))
}
