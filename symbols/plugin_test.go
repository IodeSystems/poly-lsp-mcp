package symbols

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// sortInvSites orders a SitesByFile slice deterministically for comparison.
func sortInvSites(s []InvSite) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Line != s[j].Line {
			return s[i].Line < s[j].Line
		}
		if s[i].Col != s[j].Col {
			return s[i].Col < s[j].Col
		}
		return s[i].Name < s[j].Name
	})
}

func testRegistry(t *testing.T) *config.Registry {
	t.Helper()
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// writePluginFixture lays out a tiny two-file workspace: a "writer" site
// and a "reader" site, both containing the literal "plan.landed" in a
// string, plus one plain identifier each so the lexical index is
// non-trivial.
func writePluginFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writes := "package main\n\nconst kind = \"plan.landed\"\nfunc Write() { emit(kind) }\n"
	reads := "package main\n\nfunc Fold() { on(\"plan.landed\", handle) }\n"
	if err := os.WriteFile(filepath.Join(dir, "writer.go"), []byte(writes), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reader.go"), []byte(reads), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestPluginZeroIsAdditive is the F2 guard: with no plugin registered the
// index is byte-for-byte what it is today — every store, every name,
// every site, and the derived views (Names, Lookup, SitesByFile,
// NameFreq, Cardinality) all identical to a build without the plugin
// machinery.
func TestPluginZeroIsAdditive(t *testing.T) {
	dir := writePluginFixture(t)
	reg := testRegistry(t)

	idxNoOpts, err := Build(dir, reg)
	if err != nil {
		t.Fatalf("Build (no opts): %v", err)
	}
	// An explicitly empty WithPlugins() must be indistinguishable from
	// passing nothing at all.
	idxEmpty, err := Build(dir, reg, WithPlugins())
	if err != nil {
		t.Fatalf("Build (empty plugins): %v", err)
	}

	if !reflect.DeepEqual(idxNoOpts.Names(), idxEmpty.Names()) {
		t.Errorf("Names differ:\nno opts:  %v\nempty:    %v", idxNoOpts.Names(), idxEmpty.Names())
	}
	for _, name := range idxNoOpts.Names() {
		if !reflect.DeepEqual(idxNoOpts.Lookup(name), idxEmpty.Lookup(name)) {
			t.Errorf("Lookup(%q) differs:\nno opts: %+v\nempty:   %+v",
				name, idxNoOpts.Lookup(name), idxEmpty.Lookup(name))
		}
		if idxNoOpts.NameFreq(name) != idxEmpty.NameFreq(name) {
			t.Errorf("NameFreq(%q) differs: %d vs %d",
				name, idxNoOpts.NameFreq(name), idxEmpty.NameFreq(name))
		}
	}
	// SitesByFile builds each file's slice from map iteration, so the
	// order within a slice is non-deterministic. Sort before comparing.
	a, b := idxNoOpts.SitesByFile(), idxEmpty.SitesByFile()
	if len(a) != len(b) {
		t.Errorf("SitesByFile file count differs: %d vs %d", len(a), len(b))
	}
	for file, sa := range a {
		sb, ok := b[file]
		if !ok {
			t.Errorf("SitesByFile: file %s missing in empty-plugins build", file)
			continue
		}
		sortInvSites(sa)
		sortInvSites(sb)
		if !reflect.DeepEqual(sa, sb) {
			t.Errorf("SitesByFile: %s differs:\nno opts: %+v\nempty:   %+v", file, sa, sb)
		}
	}
	if idxNoOpts.Cardinality() != idxEmpty.Cardinality() {
		t.Errorf("Cardinality differs: %d vs %d",
			idxNoOpts.Cardinality(), idxEmpty.Cardinality())
	}
	// The plugin store itself must be empty in both.
	if got := idxNoOpts.PluginNames(); len(got) != 0 {
		t.Errorf("no-opts build has plugin kinds %v, want none", got)
	}
	if got := idxEmpty.PluginNames(); len(got) != 0 {
		t.Errorf("empty-plugins build has plugin kinds %v, want none", got)
	}
	// And a lookup for a kind a plugin COULD produce finds nothing.
	if sites := idxNoOpts.Lookup("plan.landed"); len(sites) != 0 {
		t.Errorf("no-opts build finds %d sites for \"plan.landed\", want 0", len(sites))
	}
}

// TestPluginEdgeFlowsThroughLookup proves the other half of the
// contract: a registered plugin's edges land in the plugin store and are
// found through the normal name-keyed Lookup path, with the plugin sites
// added on top of — never replacing — the lexical/declared/comment sites.
func TestPluginEdgeFlowsThroughLookup(t *testing.T) {
	dir := writePluginFixture(t)
	reg := testRegistry(t)

	// A stand-in for the event-kind plugin (subtask 2 implements it for
	// real): report every PluginFile that contains the literal.
	plugin := Plugin{
		Convention: "event-kind",
		Extract: func(files []PluginFile) []PluginEdge {
			var edges []PluginEdge
			for _, f := range files {
				for i, line := range bytes.Split(f.Content, []byte("\n")) {
					if strings.Contains(string(line), "\"plan.landed\"") {
						edges = append(edges, PluginEdge{
							Kind: "plan.landed",
							File: f.Path,
							Line: i + 1,
							Col:  1,
							Conf: "lexical",
						})
					}
				}
			}
			return edges
		},
	}

	idx, err := Build(dir, reg, WithPlugins(plugin))
	if err != nil {
		t.Fatalf("Build (with plugin): %v", err)
	}

	// Both the writer and reader sites are under the kind.
	kinds := idx.PluginNames()
	if len(kinds) != 1 || kinds[0] != "plan.landed" {
		t.Fatalf("PluginNames = %v, want [plan.landed]", kinds)
	}
	sites := idx.PluginSites("plan.landed")
	if len(sites) != 2 {
		t.Fatalf("plan.landed: got %d plugin sites, want 2 (writer + reader): %+v", len(sites), sites)
	}
	files := map[string]bool{}
	for _, s := range sites {
		files[filepath.Base(s.File)] = true
	}
	for _, want := range []string{"writer.go", "reader.go"} {
		if !files[want] {
			t.Errorf("plugin site missing from %q (found in %v)", want, files)
		}
	}
	for _, s := range sites {
		if s.Confidence != ConfidenceLexical {
			t.Errorf("plugin site %s has confidence %v, want lexical", s.File, s.Confidence)
		}
	}

	// The normal name-keyed path finds the kind.
	if got := idx.Lookup("plan.landed"); len(got) != 2 {
		t.Errorf("Lookup(plan.landed) = %d sites, want 2: %+v", len(got), got)
	}
	// Lexical names are untouched by the plugin: still there, still the
	// same sites as a plugin-less build.
	plain, err := Build(dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plain.Lookup("Write"), idx.Lookup("Write")) {
		t.Errorf("Lookup(Write) changed with a plugin registered:\nplugin: %+v\nplain:  %+v",
			idx.Lookup("Write"), plain.Lookup("Write"))
	}
}


