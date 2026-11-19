package symbols

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// TestEventKindPluginExtractsPlanLanded verifies the event-kind plugin
// finds "plan.landed" in both a writer site and a reader site, produces
// one PluginEdge per occurrence with Kind="plan.landed" and Conf="lexical",
// and that PluginNames/PluginSites return the expected results.
func TestEventKindPluginExtractsPlanLanded(t *testing.T) {
	dir := writePluginFixture(t)
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}

	idx, err := Build(dir, reg, WithPlugins(EventKindPlugin()))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// PluginNames returns exactly one kind.
	names := idx.PluginNames()
	if !reflect.DeepEqual(names, []string{"plan.landed"}) {
		t.Fatalf("PluginNames = %v, want [plan.landed]", names)
	}

	// PluginSites returns both sites (writer + reader).
	sites := idx.PluginSites("plan.landed")
	if len(sites) != 2 {
		t.Fatalf("PluginSites(plan.landed) = %d sites, want 2: %+v", len(sites), sites)
	}

	// Each site is in the expected file with lexical confidence.
	files := map[string]bool{}
	for _, s := range sites {
		files[filepath.Base(s.File)] = true
		if s.Confidence != ConfidenceLexical {
			t.Errorf("site in %s has confidence %v, want lexical", s.File, s.Confidence)
		}
	}
	for _, want := range []string{"writer.go", "reader.go"} {
		if !files[want] {
			t.Errorf("no plugin site in %q; found in %v", want, files)
		}
	}

	// Lookup finds both sites through the normal path.
	if got := idx.Lookup("plan.landed"); len(got) != 2 {
		t.Errorf("Lookup(plan.landed) = %d sites, want 2: %+v", len(got), got)
	}
}

// TestEventKindPluginIgnoresNonDotted verifies that plain string literals
// without a dot (e.g. "hello") are not treated as event kinds.
func TestEventKindPluginIgnoresNonDotted(t *testing.T) {
	dir := t.TempDir()
	src := "package main\n\nconst s = \"hello\"\nfunc F() { x := \"world\" }\n"
	if err := writeTestFile(dir, "plain.go", src); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(dir, "go.mod", "module x\ngo 1.26\n"); err != nil {
		t.Fatal(err)
	}
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}

	idx, err := Build(dir, reg, WithPlugins(EventKindPlugin()))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if names := idx.PluginNames(); len(names) != 0 {
		t.Errorf("PluginNames = %v, want empty (no dotted literals)", names)
	}
}

// TestEventKindPluginMultiSegment verifies multi-segment kinds like
// "a.b.c" are extracted correctly.
func TestEventKindPluginMultiSegment(t *testing.T) {
	dir := t.TempDir()
	src := "package main\n\nfunc F() { a(\"a.b.c\"); b(\"x.y\") }\n"
	if err := writeTestFile(dir, "multi.go", src); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(dir, "go.mod", "module x\ngo 1.26\n"); err != nil {
		t.Fatal(err)
	}
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}

	idx, err := Build(dir, reg, WithPlugins(EventKindPlugin()))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	names := idx.PluginNames()
	if !reflect.DeepEqual(names, []string{"a.b.c", "x.y"}) {
		t.Fatalf("PluginNames = %v, want [a.b.c x.y]", names)
	}
}

func writeTestFile(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}
