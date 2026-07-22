package mcp

import (
	"slices"
	"testing"
)

// externalIdentity parses a resolved out-of-root path into a nameable
// module@version identity — never a workspace path, always something.
func TestExternalIdentity(t *testing.T) {
	cases := []struct {
		abs             string
		module, version string
	}{
		// Go module cache carries the @version on the module segment.
		{"/home/u/go/pkg/mod/github.com/foo/bar@v1.2.3/baz/x.go", "github.com/foo/bar/baz", "v1.2.3"},
		{"/home/u/go/pkg/mod/github.com/foo/bar@v1.2.3/x.go", "github.com/foo/bar", "v1.2.3"},
		// Go stdlib (GOROOT/src): package path, no on-disk version.
		{"/usr/lib/go/src/strings/strings.go", "strings", ""},
		{"/usr/local/go/src/encoding/json/json.go", "encoding/json", ""},
		// JS node_modules, plain and @scope.
		{"/proj/node_modules/lodash/index.js", "lodash", ""},
		{"/proj/node_modules/@babel/core/lib/x.js", "@babel/core", ""},
		// Python site-packages.
		{"/venv/lib/python3.11/site-packages/requests/api.py", "requests", ""},
		// Last resort — the containing dir, still nameable.
		{"/somewhere/odd/thing.rb", "odd", ""},
	}
	for _, c := range cases {
		m, v := externalIdentity(c.abs)
		if m != c.module || v != c.version {
			t.Errorf("externalIdentity(%q) = (%q, %q), want (%q, %q)", c.abs, m, v, c.module, c.version)
		}
	}
}

// externalStub builds a nameable, read-only, domain:external far end.
func TestExternalStubShape(t *testing.T) {
	s := externalStub("/usr/lib/go/src/strings/strings.go", 42, "Split")
	if s.class != "external" || s.domain != "external" {
		t.Errorf("stub class/domain = %q/%q, want external/external", s.class, s.domain)
	}
	if s.addr() != "strings#Split" {
		t.Errorf("stub addr = %q, want strings#Split", s.addr())
	}
	if s.leaf != "Split" {
		t.Errorf("stub leaf = %q, want Split", s.leaf)
	}
	// A versioned dep renders module@version#sym.
	s2 := externalStub("/home/u/go/pkg/mod/github.com/foo/bar@v1.2.3/x.go", 1, "New")
	if s2.addr() != "github.com/foo/bar@v1.2.3#New" {
		t.Errorf("versioned stub addr = %q", s2.addr())
	}
	// It answers to both the bare leaf and the full identity.
	ids := s2.nodeIDs()
	if !slices.Contains(ids, "New") || !slices.Contains(ids, "github.com/foo/bar@v1.2.3#New") {
		t.Errorf("stub ids = %v, want both #New and the full identity", ids)
	}
}
