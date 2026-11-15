package multiplex

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/lspbin"
	"github.com/iodesystems/poly-lsp-mcp/internal/skipdirs"
)

// TypeScript 7 is native (Go) and ships its own LSP: `tsc --lsp --stdio`.
// It has no tsserver.js, so typescript-language-server cannot run it. The
// default typescript child is chosen per LSP root by the typescript the
// root resolves: 7 or later runs TS 7's native exe; anything else, or no
// typescript found, runs typescript-language-server as configured.
// docs/design.md has the rule and the measurements.

// tsNativeMin is the first typescript major with a native LSP.
const tsNativeMin = 7

// tsSubdirDepth and tsSubdirMax bound the search below a root that
// resolves no typescript itself (a repo whose TS project is in a subdir).
const (
	tsSubdirDepth = 3
	tsSubdirMax   = 2000
)

// tsGlobalRoot is the global node_modules dir; a test seam.
var tsGlobalRoot = lspbin.NpmGlobalRoot

// isDefaultTSServer reports whether lsp is the built-in typescript
// command, the only one that is replaced. Any other command or args,
// including an absolute path to typescript-language-server, is a user's
// choice and runs as given.
func isDefaultTSServer(lsp *config.LSP) bool {
	return lsp.Cmd == "typescript-language-server" && len(lsp.Args) == 1 && lsp.Args[0] == "--stdio"
}

// resolveTSServer returns the LSP to run for the typescript child at
// root: TS 7's native LSP when the typescript the root resolves is 7 or
// later, else lsp unchanged. Only the default command is replaced.
func resolveTSServer(name string, lsp *config.LSP, root string) *config.LSP {
	if !isDefaultTSServer(lsp) {
		return lsp
	}
	pkgDir, ver, ok := findTypeScript(root)
	if !ok || ver[0] < tsNativeMin {
		return lsp
	}
	exe, err := tsNativeExe(pkgDir)
	if err != "" {
		log.Printf("[%s] ERROR: typescript %d.%d.%d at %s, but its native LSP is not usable: %s. Running %s instead, which cannot work: TypeScript 7 has no tsserver.js. Install the platform package @typescript/%s for this typescript.",
			name, ver[0], ver[1], ver[2], pkgDir, err, lsp.Cmd, tsPlatformPackage(runtime.GOOS, runtime.GOARCH))
		return lsp
	}
	log.Printf("[%s] typescript %d.%d.%d at %s: running its native LSP %s", name, ver[0], ver[1], ver[2], pkgDir, exe)
	out := *lsp
	out.Cmd = exe
	out.Args = []string{"--lsp", "--stdio"}
	return &out
}

// findTypeScript finds the typescript package the LSP root uses:
//  1. node_modules/typescript in root or an ancestor (Node's lookup);
//  2. else the shallowest one below root (lexical order breaks a tie),
//     at most tsSubdirDepth dirs down, skipped dirs not entered: a
//     workspace root with no package.json whose TS project is a subdir;
//  3. else the global npm root.
//
// It returns the package dir and its version.
func findTypeScript(root string) (string, [3]int, bool) {
	for d := root; ; {
		if dir, v, ok := tsPackageAt(filepath.Join(d, "node_modules")); ok {
			return dir, v, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	if dir, v, ok := tsBelow(root); ok {
		return dir, v, true
	}
	if g := tsGlobalRoot(); g != "" {
		return tsPackageAt(g)
	}
	return "", [3]int{}, false
}

// tsPackageAt reads nodeModules/typescript/package.json.
func tsPackageAt(nodeModules string) (string, [3]int, bool) {
	dir := filepath.Join(nodeModules, "typescript")
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "", [3]int{}, false
	}
	var pkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &pkg) != nil || pkg.Name != "typescript" {
		return "", [3]int{}, false
	}
	v, ok := parseVersion(pkg.Version)
	return dir, v, ok
}

// tsBelow is findTypeScript's step 2: a breadth-first walk of root's
// subdirs.
func tsBelow(root string) (string, [3]int, bool) {
	level := []string{root}
	seen := 0
	for depth := 1; depth <= tsSubdirDepth && len(level) > 0; depth++ {
		var next []string
		for _, d := range level {
			ents, err := os.ReadDir(d)
			if err != nil {
				continue
			}
			var subs []string
			for _, e := range ents {
				if !e.IsDir() || skipdirs.IsWatchSkip(e.Name()) || e.Name()[0] == '.' {
					continue
				}
				subs = append(subs, filepath.Join(d, e.Name()))
			}
			sort.Strings(subs)
			next = append(next, subs...)
		}
		for _, d := range next {
			if dir, v, ok := tsPackageAt(filepath.Join(d, "node_modules")); ok {
				return dir, v, true
			}
			if seen++; seen >= tsSubdirMax {
				return "", [3]int{}, false
			}
		}
		level = next
	}
	return "", [3]int{}, false
}

// tsPlatformPackage is the npm package that holds TS 7's exe for this
// platform: @typescript/typescript-<process.platform>-<process.arch>.
func tsPlatformPackage(goos, goarch string) string {
	plat := goos
	if goos == "windows" {
		plat = "win32"
	}
	arch := goarch
	switch goarch {
	case "amd64":
		arch = "x64"
	case "386":
		arch = "ia32"
	}
	return "typescript-" + plat + "-" + arch
}

// tsNativeExe is typescript's lib/getExePath.js (7.0.2) for an installed
// package: the platform package is resolved the way Node resolves it
// from inside pkgDir (its real path, so pnpm's layout works), and the exe
// is its lib/tsc. On failure it returns the reason.
func tsNativeExe(pkgDir string) (string, string) {
	data, err := os.ReadFile(filepath.Join(pkgDir, "package.json"))
	if err != nil {
		return "", err.Error()
	}
	var pkg struct {
		Bin map[string]string `json:"bin"`
	}
	if json.Unmarshal(data, &pkg) != nil || len(pkg.Bin) != 1 || pkg.Bin["tsc"] == "" {
		return "", "package.json declares no single bin tsc"
	}
	real, err := filepath.EvalSymlinks(pkgDir)
	if err != nil {
		return "", err.Error()
	}
	plat := tsPlatformPackage(runtime.GOOS, runtime.GOARCH)
	exeName := "tsc"
	if runtime.GOOS == "windows" {
		exeName += ".exe"
	}
	for d := real; ; {
		if filepath.Base(d) != "node_modules" {
			pdir := filepath.Join(d, "node_modules", "@typescript", plat)
			if _, err := os.Stat(filepath.Join(pdir, "package.json")); err == nil {
				exe := filepath.Join(pdir, "lib", exeName)
				if st, err := os.Stat(exe); err != nil || st.IsDir() {
					return "", "executable not found: " + exe
				}
				return exe, ""
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", "@typescript/" + plat + " not installed"
		}
		d = parent
	}
}
