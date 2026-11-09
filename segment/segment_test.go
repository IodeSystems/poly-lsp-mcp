package segment

import (
	"os/exec"
	"strings"
	"testing"
)

func TestDepsExcludeHeavyPackages(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	const mod = "github.com/iodesystems/poly-lsp-mcp"
	banned := []string{
		mod + "/config", mod + "/internal/git", mod + "/mcp", mod + "/server",
		mod + "/multiplex", mod + "/daemon", mod + "/internal/jsonrpc",
		mod + "/internal/lspbin", mod + "/symbols",
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.Contains(dep, "yaml") {
			t.Errorf("segment depends on %s", dep)
		}
		for _, b := range banned {
			if dep == b || strings.HasPrefix(dep, b+"/") {
				t.Errorf("segment depends on %s", dep)
			}
		}
	}
}

func TestSymbolsSpans(t *testing.T) {
	src := []byte("package p\n\n// Start runs.\nfunc (s *Server) Start(ctx int) error {\n\treturn nil\n}\n")
	got, err := Symbols("go", src)
	if err != nil {
		t.Fatal(err)
	}
	var m *Span
	for i := range got {
		if got[i].Sym == "Server.Start" {
			m = &got[i]
		}
		if got[i].Class == "argument" {
			t.Errorf("argument span leaked: %+v", got[i])
		}
	}
	if m == nil {
		t.Fatalf("no Server.Start in %+v", got)
	}
	if m.Class != "method" || m.CommentStartLine != 3 || m.BodyStartLine != 4 || m.EndLine != 7 {
		t.Errorf("Server.Start = %+v", *m)
	}
}

func TestSymbolsNoGrammar(t *testing.T) {
	if _, err := Symbols("yaml", []byte("a: 1\n")); err == nil {
		t.Error("want an error for a language without a grammar")
	}
}

func TestSections(t *testing.T) {
	src := []byte("intro\n\n# Top\n\nx\n\n## Sub\n\ny\n")
	got, err := Sections(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Sym != "Top" || got[1].Sym != "Top.Sub" ||
		got[0].StartLine != 3 || got[1].StartLine != 7 || got[0].EndLine != 10 {
		t.Errorf("sections = %+v", got)
	}
}

func TestForPathAndLanguages(t *testing.T) {
	for p, want := range map[string]string{
		"a/b/Main.GO": "go", "x.tsx": "typescript", "x.md": "markdown",
		"x.h": "cpp", "build.gradle": "groovy", "w\\x.kt": "kotlin",
	} {
		if got, ok := ForPath(p); !ok || got != want {
			t.Errorf("ForPath(%q) = %q, %v; want %q", p, got, ok, want)
		}
	}
	for _, p := range []string{"Makefile", "x.yaml", "x.unknown", ""} {
		if _, ok := ForPath(p); ok {
			t.Errorf("ForPath(%q) ok, want not", p)
		}
	}
	for _, l := range Languages() {
		if LanguageByName(l) == nil {
			t.Errorf("Languages lists %q without a grammar", l)
		}
	}
}
