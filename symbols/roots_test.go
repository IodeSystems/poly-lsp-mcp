package symbols

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/internal/git"
)

func rootsTree(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, rel := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func relRoots(root string, dirs []string) []string {
	var out []string
	for _, d := range dirs {
		out = append(out, RelRoot(root, d))
	}
	return out
}

func detect(root string) ProjectRoots {
	return DetectProjectRoots(root, git.LoadIgnores(root), DefaultRootsTime, DefaultRootsDirs)
}

func TestProjectRootsTwoGoModules(t *testing.T) {
	root := rootsTree(t, "a/go.mod", "a/x.go", "b/go.mod", "b/y.go")
	got := detect(root)
	if want := []string{"a", "b"}; !reflect.DeepEqual(relRoots(root, got.Roots["go"]), want) {
		t.Fatalf("go roots = %v, want %v", relRoots(root, got.Roots["go"]), want)
	}
	if got.GoWork || got.Bounded != "" {
		t.Fatalf("%+v", got)
	}
}

// go.work tells gopls about every module: one root, at the workspace.
func TestProjectRootsGoWorkKeepsOne(t *testing.T) {
	root := rootsTree(t, "go.work", "a/go.mod", "b/go.mod")
	got := detect(root)
	if want := []string{"."}; !reflect.DeepEqual(relRoots(root, got.Roots["go"]), want) || !got.GoWork {
		t.Fatalf("go roots = %v (goWork %v), want %v", relRoots(root, got.Roots["go"]), got.GoWork, want)
	}
}

func TestProjectRootsNestedPython(t *testing.T) {
	root := rootsTree(t, "pyproject.toml", "setup.cfg", "libs/x/setup.py", "libs/y/pyproject.toml", "libs/y/setup.cfg")
	got := detect(root)
	if want := []string{".", "libs/x", "libs/y"}; !reflect.DeepEqual(relRoots(root, got.Roots["python"]), want) {
		t.Fatalf("python roots = %v, want %v", relRoots(root, got.Roots["python"]), want)
	}
}

// A marker under a skipped or gitignored directory is not a root, and the
// walk does not enter that directory.
func TestProjectRootsSkipsIgnoredTrees(t *testing.T) {
	root := rootsTree(t, "go.mod", "vendor/x/go.mod", "node_modules/p/tsconfig.json", "out/go.mod", ".gitignore")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("out/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("git"); err == nil {
		if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v %s", err, out)
		}
	} else {
		_ = os.RemoveAll(filepath.Join(root, "out")) // no git: nothing is ignored
	}
	got := detect(root)
	if want := []string{"."}; !reflect.DeepEqual(relRoots(root, got.Roots["go"]), want) {
		t.Fatalf("go roots = %v, want %v", relRoots(root, got.Roots["go"]), want)
	}
	if len(got.Roots["typescript"]) != 0 {
		t.Fatalf("node_modules gave a typescript root: %v", got.Roots["typescript"])
	}
}

// A package's src/tsconfig.json is that package's, not a root; the
// outermost CMakeLists.txt is the C/C++ root, not each subdirectory's.
func TestProjectRootsTypescriptAndCMake(t *testing.T) {
	root := rootsTree(t,
		"web/package.json", "web/tsconfig.json", "web/src/tsconfig.json",
		"api/package.json", "api/tsconfig.json",
		"native/CMakeLists.txt", "native/lib/CMakeLists.txt", "tools/compile_commands.json")
	got := detect(root)
	if want := []string{"api", "web"}; !reflect.DeepEqual(relRoots(root, got.Roots["typescript"]), want) {
		t.Errorf("typescript roots = %v, want %v", relRoots(root, got.Roots["typescript"]), want)
	}
	if want := []string{"native", "tools"}; !reflect.DeepEqual(relRoots(root, got.Roots["cpp"]), want) {
		t.Errorf("cpp roots = %v, want %v", relRoots(root, got.Roots["cpp"]), want)
	}
}

func TestProjectRootsBoundedSaysSo(t *testing.T) {
	root := rootsTree(t, "a/go.mod", "b/go.mod", "c/go.mod")
	got := DetectProjectRoots(root, nil, time.Minute, 2)
	if got.Bounded == "" {
		t.Fatalf("a search stopped at 2 directories should say so: %+v", got)
	}
}
