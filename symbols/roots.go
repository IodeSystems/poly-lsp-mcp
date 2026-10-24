package symbols

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/internal/git"
)

// Project roots: the directories a child LSP would want as its root.
//
// One child per language at the workspace root is right for gopls and
// typescript-language-server, which find every module or package below
// it. pylsp, and JVM servers with several builds, are not: one instance
// at a monorepo's root resolves imports against the wrong package and
// holds every package in memory at once. The roots found here feed
// `--lsp-roots auto` (one child per root) and the hints that offer it.

// rootMarkers maps a marker file name to the languages it marks a root
// for. c and cpp share markers: clangd serves both.
var rootMarkers = map[string][]string{
	"go.mod":                {"go"},
	"tsconfig.json":         {"typescript"},
	"pyproject.toml":        {"python"},
	"setup.py":              {"python"},
	"setup.cfg":             {"python"},
	"compile_commands.json": {"c", "cpp"},
	"CMakeLists.txt":        {"c", "cpp"},
}

// ProjectRoots is what DetectProjectRoots found.
type ProjectRoots struct {
	// Roots maps a language to its project roots: absolute, sorted. A
	// language with no marker is absent.
	Roots map[string][]string `json:"roots"`
	// GoWork: go.work at the workspace root, so Go keeps one root there.
	GoWork bool `json:"goWork,omitempty"`
	// Bounded says why the search stopped early ("" = it finished). The
	// roots found so far are kept; there may be more.
	Bounded string `json:"bounded,omitempty"`
}

// RelRoot renders a project root relative to the workspace root, "." for
// the root itself.
func RelRoot(root, dir string) string {
	r, err := filepath.Rel(root, dir)
	if err != nil {
		return dir
	}
	return filepath.ToSlash(r)
}

// Default bounds for DetectProjectRoots. A walk that only lists
// directories is cheap; these stop it on a tree whose ignores git does
// not know (no repository) before it costs the bring-up real time.
const (
	DefaultRootsTime = 2 * time.Second
	DefaultRootsDirs = 20000
)

// DetectProjectRoots finds the project roots under root, per language,
// by marker file:
//
//   - go: go.mod. A go.work at root keeps a single root, at root: the
//     workspace file already tells gopls about every module.
//   - typescript: tsconfig.json. One beside a package.json is a package
//     root; one alone is a root only when no root encloses it (a
//     src/tsconfig.json inside a package is that package's config).
//   - python: pyproject.toml, setup.py or setup.cfg. Nested packages are
//     roots of their own.
//   - c, cpp: compile_commands.json, or the outermost CMakeLists.txt
//     (every add_subdirectory has one of its own).
//
// The walk skips what the index skips: skipDirs and ign's ignored
// directories are never entered, and an ignored marker file does not
// count. It stops after maxDirs directories or limit, and says so in
// Bounded.
func DetectProjectRoots(root string, ign *git.IgnoreSet, limit time.Duration, maxDirs int) ProjectRoots {
	out := ProjectRoots{Roots: map[string][]string{}}
	if _, err := os.Stat(filepath.Join(root, "go.work")); err == nil {
		out.GoWork = true
	}
	found := map[string]map[string]bool{} // marker → dirs
	start := time.Now()
	dirs := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && (skipDirs[d.Name()] || ign.DirIgnored(root, p)) {
				return fs.SkipDir
			}
			dirs++
			if maxDirs > 0 && dirs > maxDirs {
				out.Bounded = fmt.Sprintf("stopped after %d directories", maxDirs)
				return fs.SkipAll
			}
			if limit > 0 && time.Since(start) > limit {
				out.Bounded = fmt.Sprintf("stopped after %s", limit)
				return fs.SkipAll
			}
			return nil
		}
		if _, ok := rootMarkers[d.Name()]; !ok || ign.KnownIgnored(root, p) {
			return nil
		}
		if found[d.Name()] == nil {
			found[d.Name()] = map[string]bool{}
		}
		found[d.Name()][filepath.Dir(p)] = true
		return nil
	})

	add := func(lang, dir string) {
		out.Roots[lang] = append(out.Roots[lang], dir)
	}
	// go
	if out.GoWork {
		if len(found["go.mod"]) > 0 {
			add("go", root)
		}
	} else {
		for dir := range found["go.mod"] {
			add("go", dir)
		}
	}
	// typescript: every package root, and each lone config no other
	// config encloses.
	tsconfigs := markerDirs(found["tsconfig.json"])
	for _, dir := range tsconfigs {
		if hasFile(dir, "package.json") || !underAny(dir, tsconfigs) {
			add("typescript", dir)
		}
	}
	// python
	py := map[string]bool{}
	for _, m := range []string{"pyproject.toml", "setup.py", "setup.cfg"} {
		for dir := range found[m] {
			py[dir] = true
		}
	}
	for dir := range py {
		add("python", dir)
	}
	// c, cpp
	cmake := markerDirs(found["CMakeLists.txt"])
	cc := map[string]bool{}
	for dir := range found["compile_commands.json"] {
		cc[dir] = true
	}
	for _, dir := range cmake {
		if !underAny(dir, cmake) {
			cc[dir] = true
		}
	}
	for dir := range cc {
		add("c", dir)
		add("cpp", dir)
	}
	for lang := range out.Roots {
		sort.Strings(out.Roots[lang])
	}
	return out
}

func hasFile(dir, name string) bool {
	fi, err := os.Stat(filepath.Join(dir, name))
	return err == nil && !fi.IsDir()
}

func markerDirs(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// underAny reports whether dir sits strictly below one of roots.
func underAny(dir string, roots []string) bool {
	for _, r := range roots {
		if r != dir && strings.HasPrefix(dir, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
