package mcp

import "testing"

// A Go toolchain auto-downloaded into the module cache
// (golang.org/toolchain@...) is a GOROOT tree: its files are the stdlib
// and must name the package with no version, as a plain GOROOT path does.
// Every other module-cache path keeps its module@version reading.
func TestExternalIdentityReadsAToolchainModuleAsTheStdlib(t *testing.T) {
	cases := []struct {
		abs             string
		module, version string
	}{
		{"/home/u/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.26.8.linux-amd64/src/strings/strings.go", "strings", ""},
		{"/usr/local/go/src/strings/strings.go", "strings", ""},
		{"/home/u/go/pkg/mod/github.com/foo/bar@v1.2.3/sub/x.go", "github.com/foo/bar/sub", "v1.2.3"},
	}
	for _, c := range cases {
		m, v := externalIdentity(c.abs)
		if m != c.module || v != c.version {
			t.Errorf("externalIdentity(%q) = (%q, %q), want (%q, %q)", c.abs, m, v, c.module, c.version)
		}
	}
}
