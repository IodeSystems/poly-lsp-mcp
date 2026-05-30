package bindings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/internal/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/symbols"
)

// buildIndex constructs an Index pre-populated with lexical sites for a
// few test names so the resolver has something to match against.
func buildIndex(root string) *symbols.Index {
	idx := symbols.NewIndex()
	idx.Refresh(filepath.Join(root, "main.go"), "go", []symbols.Hit{
		{Name: "UserID", Line: 5, Col: 6},
		{Name: "UserID", Line: 8, Col: 19},
		{Name: "GreetUser", Line: 8, Col: 6},
	})
	idx.Refresh(filepath.Join(root, "client.ts"), "typescript", []symbols.Hit{
		{Name: "UserID", Line: 1, Col: 13},
		{Name: "UserID", Line: 3, Col: 37},
	})
	return idx
}

func TestResolverSymbolFormAliasesAcrossFiles(t *testing.T) {
	root := "/ws"
	idx := buildIndex(root)
	r := NewResolver(root)

	n, err := r.Apply(idx, []config.Binding{
		{
			Name: "UserType",
			Sites: []config.BindingSite{
				{File: "main.go", Symbol: "UserID"},
				{File: "client.ts", Symbol: "UserID"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n != 4 {
		t.Errorf("inserted = %d, want 4 (2 sites in main.go + 2 in client.ts)", n)
	}

	// Now Lookup("UserType") should return UserID's positions tagged as
	// declared.
	got := idx.Lookup("UserType")
	if len(got) != 4 {
		t.Fatalf("Lookup(UserType) = %d sites, want 4: %+v", len(got), got)
	}
	for _, s := range got {
		if s.Confidence != symbols.ConfidenceDeclared {
			t.Errorf("UserType site %+v has confidence %d, want Declared", s, s.Confidence)
		}
	}
}

func TestResolverSymbolFormStaysSameNameOverridesLexical(t *testing.T) {
	root := "/ws"
	idx := buildIndex(root)
	r := NewResolver(root)

	// Declare a binding whose own Name is the same as the symbol it
	// points at — i.e., mark the existing UserID sites in main.go as
	// also declared. Lookup should NOT return duplicates; declared wins
	// at the same (file, line, col).
	_, err := r.Apply(idx, []config.Binding{
		{
			Name:  "UserID",
			Sites: []config.BindingSite{{File: "main.go", Symbol: "UserID"}},
		},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := idx.Lookup("UserID")
	// main.go has 2 UserID sites; client.ts has 2 more. Total 4 — no
	// duplicates from the declared overlay.
	if len(got) != 4 {
		t.Errorf("Lookup(UserID) = %d, want 4 (declared in main.go overrides lexical, plus 2 from client.ts)", len(got))
	}
	// The two sites in main.go must be Declared, the two in client.ts
	// must remain Lexical.
	for _, s := range got {
		want := symbols.ConfidenceLexical
		if filepath.Base(s.File) == "main.go" {
			want = symbols.ConfidenceDeclared
		}
		if s.Confidence != want {
			t.Errorf("site %s:%d:%d confidence = %d, want %d", s.File, s.Line, s.Col, s.Confidence, want)
		}
	}
}

func TestResolverMissingSymbolLogsButDoesNotAbort(t *testing.T) {
	root := "/ws"
	idx := buildIndex(root)
	r := NewResolver(root)

	n, err := r.Apply(idx, []config.Binding{
		{
			Name: "Mixed",
			Sites: []config.BindingSite{
				{File: "main.go", Symbol: "DoesNotExist"}, // should warn
				{File: "main.go", Symbol: "GreetUser"},    // should succeed
			},
		},
	})
	if err != nil {
		t.Fatalf("Apply unexpectedly returned an error: %v", err)
	}
	if n != 1 {
		t.Errorf("inserted = %d, want 1 (only GreetUser hit)", n)
	}
}

func TestResolverEmptyNameRejected(t *testing.T) {
	root := "/ws"
	idx := buildIndex(root)
	r := NewResolver(root)

	_, err := r.Apply(idx, []config.Binding{
		{Name: "", Sites: []config.BindingSite{{File: "main.go", Symbol: "UserID"}}},
	})
	if err == nil {
		t.Error("expected error for empty binding name")
	}
}

func TestResolverEmptySitesListRejected(t *testing.T) {
	root := "/ws"
	idx := buildIndex(root)
	r := NewResolver(root)

	_, err := r.Apply(idx, []config.Binding{{Name: "Foo"}})
	if err == nil {
		t.Error("expected error for binding with no sites")
	}
}

func TestResolverPartialFailureStillPersistsGoodSites(t *testing.T) {
	root := "/ws"
	idx := buildIndex(root)
	r := NewResolver(root)

	// One bad site (file does not exist on disk so regex can't read it)
	// plus one good symbol site. Resolver should log the bad and still
	// apply the good — bindings degrade gracefully.
	n, err := r.Apply(idx, []config.Binding{
		{
			Name: "PartiallyApplied",
			Sites: []config.BindingSite{
				{File: "does_not_exist.go", Regex: []string{"User.*"}}, // read fails
				{File: "main.go", Symbol: "UserID"},                    // 2 sites
			},
		},
	})
	if err != nil {
		t.Errorf("Apply errored: %v", err)
	}
	if n != 2 {
		t.Errorf("inserted = %d, want 2 (symbol form succeeds for both UserID sites)", n)
	}
}

func TestResolverJSONPathFormResolvesYAMLValue(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	yaml := `service: polyglot
user_id_type: UserID
endpoints:
  - path: /users/:id
    handler: GreetUser
`
	if err := os.WriteFile(yamlPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	idx := symbols.NewIndex()
	r := NewResolver(dir)
	n, err := r.Apply(idx, []config.Binding{
		{
			Name: "UserIdentifier",
			Sites: []config.BindingSite{
				{File: "config.yaml", JSONPath: "$.user_id_type"},
				{File: "config.yaml", JSONPath: "$.endpoints[0].handler"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n != 2 {
		t.Errorf("inserted = %d, want 2", n)
	}
	sites := idx.Lookup("UserIdentifier")
	if len(sites) != 2 {
		t.Fatalf("Lookup = %d sites, want 2: %+v", len(sites), sites)
	}
	for _, s := range sites {
		if s.Confidence != symbols.ConfidenceDeclared {
			t.Errorf("confidence = %d, want Declared", s.Confidence)
		}
		if s.Language != "yaml" {
			t.Errorf("language = %q, want yaml", s.Language)
		}
	}
}

func TestResolverJSONPathFormRejectsNonYAMLJSONFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx := symbols.NewIndex()
	r := NewResolver(dir)
	n, err := r.Apply(idx, []config.Binding{
		{
			Name:  "Bad",
			Sites: []config.BindingSite{{File: "main.go", JSONPath: "$.foo"}},
		},
	})
	if err != nil {
		t.Errorf("unexpected resolver-level error: %v", err)
	}
	if n != 0 {
		t.Errorf("inserted = %d, want 0 (Go file should not be evaluated as jsonpath)", n)
	}
}

func TestResolverAbsolutePathBypassesRoot(t *testing.T) {
	root := "/ws"
	idx := symbols.NewIndex()
	idx.Refresh("/elsewhere/gen.go", "go", []symbols.Hit{
		{Name: "Token", Line: 3, Col: 6},
	})
	r := NewResolver(root)
	n, err := r.Apply(idx, []config.Binding{
		{
			Name:  "Token",
			Sites: []config.BindingSite{{File: "/elsewhere/gen.go", Symbol: "Token"}},
		},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n != 1 {
		t.Errorf("inserted = %d, want 1", n)
	}
}
