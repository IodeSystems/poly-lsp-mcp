package multiplex

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

func tsDefault() *config.LSP {
	return &config.LSP{Cmd: "typescript-language-server", Args: []string{"--stdio"}, Env: []string{"A=1"}}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// installTS writes a typescript package of version into nodeModules; a 7
// gets its platform package and exe, as npm installs them. It returns the
// exe path ("" for < 7).
func installTS(t *testing.T, nodeModules, version string) string {
	t.Helper()
	major := version[0] - '0'
	if major >= 7 {
		writeFile(t, filepath.Join(nodeModules, "typescript/package.json"), `{"name":"typescript","version":"`+version+`","bin":{"tsc":"./bin/tsc"}}`)
		plat := filepath.Join(nodeModules, "@typescript", tsPlatformPackage(runtime.GOOS, runtime.GOARCH))
		writeFile(t, filepath.Join(plat, "package.json"), `{"name":"@typescript/x"}`)
		exe := filepath.Join(plat, "lib", "tsc")
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		writeFile(t, exe, "")
		return exe
	}
	writeFile(t, filepath.Join(nodeModules, "typescript/package.json"), `{"name":"typescript","version":"`+version+`","bin":{"tsc":"./bin/tsc","tsserver":"./bin/tsserver"}}`)
	return ""
}

func noGlobalTS(t *testing.T, dir string) {
	t.Helper()
	old := tsGlobalRoot
	tsGlobalRoot = func() string { return dir }
	t.Cleanup(func() { tsGlobalRoot = old })
}

func wantNative(t *testing.T, got *config.LSP, exe string) {
	t.Helper()
	if got.Cmd != exe || len(got.Args) != 2 || got.Args[0] != "--lsp" || got.Args[1] != "--stdio" {
		t.Fatalf("got %s %v, want %s --lsp --stdio", got.Cmd, got.Args, exe)
	}
	if len(got.Env) != 1 || got.Env[0] != "A=1" {
		t.Errorf("env %v not kept", got.Env)
	}
}

func wantTSLS(t *testing.T, got *config.LSP) {
	t.Helper()
	if got.Cmd != "typescript-language-server" || len(got.Args) != 1 || got.Args[0] != "--stdio" {
		t.Fatalf("got %s %v, want typescript-language-server --stdio", got.Cmd, got.Args)
	}
}

func TestTSResolveLocal7(t *testing.T) {
	noGlobalTS(t, "")
	root := t.TempDir()
	exe := installTS(t, filepath.Join(root, "node_modules"), "7.0.2")
	wantNative(t, resolveTSServer("typescript", tsDefault(), root), exe)
	// A root inside the package finds it by walking up.
	sub := filepath.Join(root, "src", "app")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wantNative(t, resolveTSServer("typescript", tsDefault(), sub), exe)
}

func TestTSResolveLocal6(t *testing.T) {
	g := filepath.Join(t.TempDir(), "lib", "node_modules")
	installTS(t, g, "7.0.2") // a global 7 does not override a local 6
	noGlobalTS(t, g)
	root := t.TempDir()
	installTS(t, filepath.Join(root, "node_modules"), "6.0.2")
	wantTSLS(t, resolveTSServer("typescript", tsDefault(), root))
}

func TestTSResolveGlobalFallback(t *testing.T) {
	g := filepath.Join(t.TempDir(), "lib", "node_modules")
	exe := installTS(t, g, "7.0.2")
	noGlobalTS(t, g)
	root := t.TempDir()
	wantNative(t, resolveTSServer("typescript", tsDefault(), root), exe)

	g6 := filepath.Join(t.TempDir(), "lib", "node_modules")
	installTS(t, g6, "6.0.2")
	noGlobalTS(t, g6)
	wantTSLS(t, resolveTSServer("typescript", tsDefault(), root))

	noGlobalTS(t, "")
	wantTSLS(t, resolveTSServer("typescript", tsDefault(), root))
}

// A workspace root with no package.json whose TS project is a subdir
// (DUN: mobile/). Skipped dirs are not entered; the shallowest wins.
func TestTSResolveSubdirProject(t *testing.T) {
	noGlobalTS(t, "")
	root := t.TempDir()
	installTS(t, filepath.Join(root, ".claude/worktrees/w/node_modules"), "6.0.2")
	installTS(t, filepath.Join(root, "a/b/node_modules"), "6.0.2")
	exe := installTS(t, filepath.Join(root, "mobile/node_modules"), "7.0.2")
	wantNative(t, resolveTSServer("typescript", tsDefault(), root), exe)

	deep := t.TempDir()
	installTS(t, filepath.Join(deep, "a/b/c/d/node_modules"), "7.0.2")
	wantTSLS(t, resolveTSServer("typescript", tsDefault(), deep))
}

// A 7 whose platform package is missing (an install for another
// platform) keeps typescript-language-server, logged.
func TestTSResolve7WithoutExe(t *testing.T) {
	noGlobalTS(t, "")
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "node_modules/typescript/package.json"), `{"name":"typescript","version":"7.0.2","bin":{"tsc":"./bin/tsc"}}`)
	wantTSLS(t, resolveTSServer("typescript", tsDefault(), root))
}

// pnpm: node_modules/typescript links into .pnpm, where the platform
// package sits beside it.
func TestTSResolvePnpmLayout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	noGlobalTS(t, "")
	root := t.TempDir()
	store := filepath.Join(root, "node_modules/.pnpm/typescript@7.0.2/node_modules")
	exe := installTS(t, store, "7.0.2")
	if err := os.Symlink(filepath.Join(store, "typescript"), filepath.Join(root, "node_modules/typescript")); err != nil {
		t.Fatal(err)
	}
	got := resolveTSServer("typescript", tsDefault(), root)
	realExe, _ := filepath.EvalSymlinks(exe)
	if got.Cmd != exe && got.Cmd != realExe {
		t.Fatalf("got %s, want %s", got.Cmd, exe)
	}
}

// A user's typescript command, even typescript-language-server by path
// or with other args, runs as given.
func TestTSResolveUserOverride(t *testing.T) {
	noGlobalTS(t, "")
	root := t.TempDir()
	installTS(t, filepath.Join(root, "node_modules"), "7.0.2")
	for _, l := range []*config.LSP{
		{Cmd: "/opt/tsls/bin/typescript-language-server", Args: []string{"--stdio"}},
		{Cmd: "typescript-language-server", Args: []string{"--stdio", "--log-level=4"}},
		{Cmd: "tsgo", Args: []string{"--lsp", "--stdio"}},
	} {
		if got := resolveTSServer("typescript", l, root); got != l {
			t.Errorf("%s %v replaced by %s %v", l.Cmd, l.Args, got.Cmd, got.Args)
		}
	}
}

// TS 7's exe is not typescript-language-server: no --version probe and
// no tsserver options.
func TestNativeTSGetsNoVersionProbe(t *testing.T) {
	old := tslsVersionOf
	tslsVersionOf = func(string, []string) string { t.Fatal("version probe ran against TS 7's exe"); return "" }
	defer func() { tslsVersionOf = old }()
	if got := initializationOptions("typescript", "/p/node_modules/@typescript/typescript-linux-x64/lib/tsc", nil); got != nil {
		t.Fatalf("initializationOptions = %v, want nil", got)
	}
}

func TestTSPlatformPackage(t *testing.T) {
	for _, tc := range []struct{ goos, goarch, want string }{
		{"linux", "amd64", "typescript-linux-x64"},
		{"darwin", "arm64", "typescript-darwin-arm64"},
		{"windows", "386", "typescript-win32-ia32"},
	} {
		if got := tsPlatformPackage(tc.goos, tc.goarch); got != tc.want {
			t.Errorf("%s/%s = %s, want %s", tc.goos, tc.goarch, got, tc.want)
		}
	}
}
