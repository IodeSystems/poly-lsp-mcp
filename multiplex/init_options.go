package multiplex

import (
	"context"
	"log"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// tslsClientWatchMin is the first typescript-language-server release with
// tsserver.useClientFileWatcher. With it, tsserver takes file events from
// this client (didChangeWatchedFiles) and holds no inotify watches:
// measured 18,196 without it and 0 with it, on the same repo.
var tslsClientWatchMin = [3]int{6, 1, 0}

// tslsVersions caches `typescript-language-server --version` by binary
// path: a child is spawned again on every restart and lazy wake, and the
// warning for an old one is logged once.
var tslsVersions sync.Map // path → [3]int (zero when unknown)

// tslsVersionOf runs the binary's --version; a test seam.
var tslsVersionOf = func(path string, env []string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// initializationOptions is the initializationOptions a child is started
// with, nil for none. Only typescript-language-server gets any: from
// 6.1.0, tsserver.useClientFileWatcher.
func initializationOptions(name, binPath string, env []string) any {
	if strings.TrimSuffix(filepath.Base(binPath), ".exe") != "typescript-language-server" {
		return nil
	}
	v, cached := tslsVersions.Load(binPath)
	if !cached {
		parsed, ok := parseVersion(tslsVersionOf(binPath, env))
		switch {
		case !ok:
			log.Printf("[%s] %s --version not understood: tsserver may create its own inotify watches", name, binPath)
		case versionLess(parsed, tslsClientWatchMin):
			log.Printf("[%s] typescript-language-server %d.%d.%d (%s) is older than 6.1.0: tsserver creates its own inotify watches (thousands on a large repo, node_modules included). Upgrade typescript-language-server to 6.1.0 or later to let poly-lsp-mcp watch files for it.", name, parsed[0], parsed[1], parsed[2], binPath)
		}
		v, _ = tslsVersions.LoadOrStore(binPath, parsed)
	}
	if versionLess(v.([3]int), tslsClientWatchMin) {
		return nil
	}
	return map[string]any{"tsserver": map[string]any{"useClientFileWatcher": true}}
}

// parseVersion reads the first "X.Y.Z" in s, "v" prefix allowed.
func parseVersion(s string) ([3]int, bool) {
	for _, f := range strings.Fields(s) {
		f = strings.TrimPrefix(f, "v")
		if i := strings.IndexAny(f, "-+"); i >= 0 {
			f = f[:i]
		}
		parts := strings.Split(f, ".")
		if len(parts) != 3 {
			continue
		}
		var v [3]int
		ok := true
		for i, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil {
				ok = false
				break
			}
			v[i] = n
		}
		if ok {
			return v, true
		}
	}
	return [3]int{}, false
}

func versionLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
