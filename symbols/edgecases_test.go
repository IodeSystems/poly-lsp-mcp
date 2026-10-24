package symbols

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/git"
	"github.com/iodesystems/poly-lsp-mcp/internal/pathpat"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// buildReport builds root with a cost recorder and returns the index and
// the report.
func buildReport(t *testing.T, root string, opts ...BuildOption) (*Index, CostReport) {
	t.Helper()
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	cost := NewBuildCost()
	idx, err := Build(root, reg, append([]BuildOption{WithCost(cost)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return idx, cost.Report()
}

// leftOut is the report's entries for one reason.
func leftOut(rep CostReport, reason string) []string {
	var out []string
	for _, l := range rep.LeftOut {
		if l.Reason == reason {
			out = append(out, l.Dir)
		}
	}
	return out
}

func reasonCount(rep CostReport, reason string) (int, string) {
	for _, r := range rep.LeftOutBy {
		if r.Reason == reason {
			return r.Count, r.Hint
		}
	}
	return 0, ""
}

// files is the distinct files that hold a site for name.
func files(idx *Index, root, name string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range idx.Lookup(name) {
		r, _ := filepath.Rel(root, s.File)
		r = filepath.ToSlash(r)
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

func include(pats ...string) BuildOption { return WithInclude(pathpat.New(pats)) }

// A file over the size cap and a minified one are left out and named, with
// the way to index them anyway; an include indexes each.
func TestOverSizeAndMinifiedReported(t *testing.T) {
	root := t.TempDir()
	big := "package p\n\nvar BigName = `" + strings.Repeat("x", maxFileSize) + "`\n"
	writeFiles(t, root, map[string]string{
		"big.go":          big,
		"web/app.min.js":  "function MinName(){}\n",
		"web/app.js":      "function KeepName(){}\n",
		"web/.min.js":     "function DotName(){}\n", // not *.min.*: a hidden file named .min.js
		"web/minimal.js":  "function MinimalName(){}\n",
		"web/app.min":     "x\n", // no extension after .min
		"web/admin.min.x": "x\n",
	})
	idx, rep := buildReport(t, root)
	if got := leftOut(rep, LeftOverSize); len(got) != 1 || got[0] != "big.go" {
		t.Errorf("over size = %v, want [big.go]", got)
	}
	if got := leftOut(rep, LeftMinified); len(got) != 1 || got[0] != "web/app.min.js" {
		t.Errorf("minified = %v, want [web/app.min.js]", got)
	}
	if len(idx.Lookup("BigName")) != 0 || len(idx.Lookup("MinName")) != 0 {
		t.Error("left-out files should not be indexed")
	}
	for _, n := range []string{"KeepName", "DotName", "MinimalName"} {
		if len(idx.Lookup(n)) == 0 {
			t.Errorf("%s should be indexed", n)
		}
	}
	for _, r := range []string{LeftOverSize, LeftMinified} {
		if n, hint := reasonCount(rep, r); n != 1 || !strings.Contains(hint, "--include") {
			t.Errorf("%s: count %d hint %q", r, n, hint)
		}
	}
	text := rep.Text()
	for _, want := range []string{"over size", "big.go", "minified", "web/app.min.js", "--include <path>"} {
		if !strings.Contains(text, want) {
			t.Errorf("Text should contain %q:\n%s", want, text)
		}
	}

	idx, rep = buildReport(t, root, include("big.go", "*.min.js"))
	if len(idx.Lookup("BigName")) == 0 || len(idx.Lookup("MinName")) == 0 {
		t.Error("an included file is indexed whatever its size, minified or not")
	}
	if len(rep.LeftOut) != 0 {
		t.Errorf("nothing left out with both included: %v", rep.LeftOut)
	}
}

// Thousands of left-out files keep the report readable: at most leftOutCap
// listed per reason, the true count beside them.
func TestLeftOutCapped(t *testing.T) {
	root := t.TempDir()
	n := leftOutCap + 15
	fs := map[string]string{}
	for i := range n {
		fs[fmt.Sprintf("web/m%02d.min.js", i)] = "function F(){}\n"
	}
	writeFiles(t, root, fs)
	_, rep := buildReport(t, root)
	if got := leftOut(rep, LeftMinified); len(got) != leftOutCap {
		t.Errorf("listed %d, want %d", len(got), leftOutCap)
	}
	if c, _ := reasonCount(rep, LeftMinified); c != n {
		t.Errorf("count %d, want %d", c, n)
	}
	text := rep.Text()
	if !strings.Contains(text, fmt.Sprintf("minified                %d", n)) || !strings.Contains(text, fmt.Sprintf("… and %d more", n-5)) {
		t.Errorf("Text should carry the true count:\n%s", text)
	}
}

// A Git LFS pointer is left out with `git lfs pull` as the way to its
// content; an include indexes the pointer text.
func TestLFSPointerSkipped(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"data/schema.json": "version https://git-lfs.github.com/spec/v1\noid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\nsize 12345\n",
		"real.json":        `{"RealKey": 1}`,
	})
	idx, rep := buildReport(t, root)
	if got := leftOut(rep, LeftLFS); len(got) != 1 || got[0] != "data/schema.json" {
		t.Errorf("LFS = %v, want [data/schema.json]", got)
	}
	if _, hint := reasonCount(rep, LeftLFS); !strings.Contains(hint, "git lfs pull") {
		t.Errorf("hint = %q, want git lfs pull", hint)
	}
	if len(idx.Lookup("oid")) != 0 || len(idx.Lookup("RealKey")) == 0 {
		t.Error("the pointer is not indexed; the real file is")
	}
	if rep.Files != 1 {
		t.Errorf("files = %d, want 1", rep.Files)
	}
	idx, _ = buildReport(t, root, include("data/"))
	if len(idx.Lookup("oid")) == 0 {
		t.Error("an included pointer is indexed")
	}
}

// A skipped dir is reported; an include indexes it, or only the path it
// names inside it.
func TestSkippedDirReportedAndIncluded(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"a.go":                    "package p\n",
		"node_modules/x/index.ts": "export const XName = 1\n",
		"node_modules/y/index.ts": "export const YName = 1\n",
	})
	idx, rep := buildReport(t, root)
	if got := leftOut(rep, LeftSkipped); len(got) != 1 || got[0] != "node_modules" {
		t.Errorf("skipped = %v, want [node_modules]", got)
	}
	if len(idx.Lookup("XName")) != 0 {
		t.Error("node_modules should be skipped")
	}
	idx, _ = buildReport(t, root, include("node_modules/x"))
	if len(idx.Lookup("XName")) == 0 || len(idx.Lookup("YName")) != 0 {
		t.Error("include node_modules/x: x indexed, y not")
	}
	idx, _ = buildReport(t, root, include("node_modules"))
	if len(idx.Lookup("YName")) == 0 {
		t.Error("include node_modules: all of it indexed")
	}
}

// A link to a file or dir indexed under its own path is not indexed twice; a
// link into a left-out region is followed, and its files indexed once, under
// the link's path.
func TestSymlinkInsideRootFollowedOnce(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"pkg/a.go":          "package pkg\n\nfunc AName() {}\n",
		"vendor/lib/lib.go": "package lib\n\nfunc LibName() {}\n",
	})
	symlink(t, "pkg/a.go", filepath.Join(root, "alias.go"))
	symlink(t, "pkg", filepath.Join(root, "pkglink"))
	symlink(t, "vendor/lib", filepath.Join(root, "lib"))
	symlink(t, "../vendor/lib", filepath.Join(root, "again", "lib")) // the same target twice
	idx, rep := buildReport(t, root)
	if got := files(idx, root, "AName"); len(got) != 1 || got[0] != "pkg/a.go" {
		t.Errorf("AName in %v, want only pkg/a.go", got)
	}
	got := files(idx, root, "LibName")
	if len(got) != 1 || (got[0] != "lib/lib.go" && got[0] != "again/lib/lib.go") {
		t.Errorf("LibName in %v, want once, under a link", got)
	}
	// alias.go and pkglink by their target's own path; one of lib and
	// again/lib because the other reached vendor/lib first.
	if n, _ := reasonCount(rep, LeftLinkIndexed); n != 3 {
		t.Errorf("links to indexed paths = %d, want 3: %v", n, rep.LeftOut)
	}
	if rep.Files != 2 {
		t.Errorf("files = %d, want 2", rep.Files)
	}
}

// A dir link back to a directory open on its own path is a loop: that
// branch is aborted and named.
func TestSymlinkLoopAborted(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"a/a.go":        "package a\n",
		"vendor/v/v.go": "package v\n\nfunc VName() {}\n",
	})
	symlink(t, "..", filepath.Join(root, "a", "up"))             // to the root
	symlink(t, "../a", filepath.Join(root, "a", "me"))           // to its own dir
	symlink(t, "vendor/v", filepath.Join(root, "v"))             // followed: vendor is skipped
	symlink(t, "..", filepath.Join(root, "vendor", "v", "back")) // inside it, back to vendor/v's parent... and on
	_, rep := buildReport(t, root)
	loops := leftOut(rep, LeftLinkLoop)
	for _, want := range []string{"a/up", "a/me"} {
		found := false
		for _, l := range loops {
			found = found || l == want
		}
		if !found {
			t.Errorf("loop should name %s: %v", want, rep.LeftOut)
		}
	}
	if rep.Files != 2 {
		t.Errorf("files = %d, want 2 (a/a.go, v/v.go): %v", rep.Files, rep.LeftOut)
	}
}

// A link whose target leaves the root is not followed, unless an include
// names the link.
func TestSymlinkLeavingRoot(t *testing.T) {
	outside := t.TempDir()
	writeFiles(t, outside, map[string]string{"o.go": "package o\n\nfunc OutName() {}\n"})
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a.go": "package p\n"})
	symlink(t, outside, filepath.Join(root, "ext"))
	symlink(t, filepath.Join(outside, "o.go"), filepath.Join(root, "o.go"))
	idx, rep := buildReport(t, root)
	got := leftOut(rep, LeftLinkLeaves)
	if len(got) != 2 {
		t.Errorf("leaves root = %v, want ext and o.go", got)
	}
	if _, hint := reasonCount(rep, LeftLinkLeaves); !strings.Contains(hint, "--include <link path>") {
		t.Errorf("hint = %q", hint)
	}
	if len(idx.Lookup("OutName")) != 0 {
		t.Error("a target outside the root is not indexed")
	}
	idx, _ = buildReport(t, root, include("ext"))
	if got := files(idx, root, "OutName"); len(got) != 1 || got[0] != "ext/o.go" {
		t.Errorf("included link: OutName in %v, want [ext/o.go]", got)
	}
}

// --symlink-depth bounds nested link hops; 0 follows none.
func TestSymlinkDepth(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"vendor/d1/one.go": "package d1\n\nfunc OneName() {}\n",
		"vendor/d2/two.go": "package d2\n\nfunc TwoName() {}\n",
	})
	symlink(t, "vendor/d1", filepath.Join(root, "l1"))
	symlink(t, "../d2", filepath.Join(root, "vendor", "d1", "l2"))

	idx, rep := buildReport(t, root, WithSymlinkDepth(0))
	if got := leftOut(rep, LeftLinkDepth); len(got) != 1 || got[0] != "l1" || len(idx.Lookup("OneName")) != 0 {
		t.Errorf("depth 0: depth = %v, OneName indexed = %v", got, len(idx.Lookup("OneName")) > 0)
	}
	idx, rep = buildReport(t, root, WithSymlinkDepth(1))
	if got := leftOut(rep, LeftLinkDepth); len(got) != 1 || got[0] != "l1/l2" {
		t.Errorf("depth 1: depth = %v, want [l1/l2]", got)
	}
	if _, hint := reasonCount(rep, LeftLinkDepth); !strings.Contains(hint, "--symlink-depth 2") {
		t.Errorf("hint = %q", hint)
	}
	if len(idx.Lookup("OneName")) == 0 || len(idx.Lookup("TwoName")) != 0 {
		t.Error("depth 1: one.go indexed, two.go not")
	}
	idx, _ = buildReport(t, root, WithSymlinkDepth(2))
	if got := files(idx, root, "TwoName"); len(got) != 1 || got[0] != "l1/l2/two.go" {
		t.Errorf("depth 2: TwoName in %v, want [l1/l2/two.go]", got)
	}
}

// In a git repository the manifest lists a link itself: a dir link is
// walked, bounded and with ignores applied.
func TestManifestFollowsDirLink(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		".gitignore":            "vendor/lib/gen/\n",
		"main.go":               "package main\n",
		"vendor/lib/lib.go":     "package lib\n\nfunc LibName() {}\n",
		"vendor/lib/gen/gen.go": "package gen\n\nfunc GenName() {}\n",
	})
	symlink(t, "vendor/lib", filepath.Join(root, "lib"))
	symlink(t, "main.go", filepath.Join(root, "alias.go"))
	cmd := exec.Command("git", "-C", root, "init", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	m := git.LoadManifest(root)
	if m == nil || len(m.Links) != 2 {
		t.Fatalf("manifest links = %+v", m)
	}
	idx, rep := buildReport(t, root, WithManifest(m))
	if got := files(idx, root, "LibName"); len(got) != 1 || got[0] != "lib/lib.go" {
		t.Errorf("LibName in %v, want [lib/lib.go]", got)
	}
	if n, _ := reasonCount(rep, LeftLinkIndexed); n != 1 {
		t.Errorf("alias.go should be named as a link to an indexed path: %v", rep.LeftOut)
	}
	if got := leftOut(rep, LeftSkipped); len(got) != 1 || got[0] != "vendor" {
		t.Errorf("skipped = %v, want [vendor]", got)
	}
	// vendor/lib/gen/ is ignored by its own path: the link does not bring it
	// back as lib/gen/.
	if len(idx.Lookup("GenName")) != 0 {
		t.Errorf("an ignored dir under a link target is not indexed: %v", files(idx, root, "GenName"))
	}
}
