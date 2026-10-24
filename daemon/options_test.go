package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// One index per root, the last open wins: a client opening a warm root
// with other index options rebuilds it with its own, and every session on
// the root is told, old → new.
func TestOpenWithOtherOptionsRebuildsTheRoot(t *testing.T) {
	root, resolved := goWorkspace(t)
	if err := os.MkdirAll(filepath.Join(root, "extra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "extra", "e.go"), []byte("package extra\n\nfunc Extra() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, stop := startTestDaemon(t, resolved)
	defer stop()
	query := func(h *proxyHarness, sel string) string {
		text, _ := callText(t, h.reply(h.request("tools/call", map[string]any{"name": "node_query", "arguments": map[string]any{"selector": sel}}), 30*time.Second))
		return text
	}

	a := startProxy(t, c, root, OpenOptions{Excludes: []string{"extra"}})
	a.reply(a.request("initialize", map[string]any{}), 5*time.Second)
	a.logged(indexLogger, 30*time.Second, nil)
	if text := query(a, "#Extra"); strings.Contains(text, "extra/e.go") {
		t.Fatalf("extra/ indexed despite the exclude: %q", text)
	}

	b := startProxy(t, NewClient(c.socket), root, OpenOptions{})
	b.reply(b.request("initialize", map[string]any{}), 5*time.Second)
	for name, h := range map[string]*proxyHarness{"first": a, "second": b} {
		n := h.logged(daemonLogger, 30*time.Second, func(d map[string]any) bool { return d["state"] == "rebuilding" })
		if n["from"] != "exclude extra" || n["to"] != "the daemon's defaults" || n["root"] != resolved ||
			!strings.Contains(fmt.Sprint(n["message"]), "exclude extra → the daemon's defaults") {
			t.Errorf("%s client's notice: %v", name, n)
		}
	}
	if text := query(a, "#Extra"); !strings.Contains(text, "extra/e.go") {
		t.Errorf("the root was not rebuilt without the exclude: %q", text)
	}
}

// Options that build the same index — excludes in another order or
// spelling, the daemon's own symlink depth said out loud — keep the warm
// root: no rebuild, no notice.
func TestOpenWithEqualOptionsKeepsTheRoot(t *testing.T) {
	cfg, reg := testRegistryDeps(t)
	r := NewRegistry(cfg, reg, false, false)
	defer r.Close()
	_, resolved := goWorkspace(t)
	ch, cancel := r.hub.subscribe("s2", nil)
	defer cancel()

	first, err := r.Open("s1", resolved, OpenOptions{Excludes: []string{"vendor", "a/b"}})
	if err != nil {
		t.Fatal(err)
	}
	depth := r.idx.SymlinkDepth
	same, err := r.Open("s2", resolved, OpenOptions{Excludes: []string{"./a/b/", "vendor"}, SymlinkDepth: &depth, LSPRoots: "single"})
	if err != nil {
		t.Fatal(err)
	}
	if same != first {
		t.Fatal("equal options rebuilt the root")
	}
	for {
		select {
		case ev := <-ch:
			if strings.Contains(string(ev.Params), "rebuilding") {
				t.Fatalf("notice for equal options: %s", ev.Params)
			}
			continue
		case <-time.After(200 * time.Millisecond):
		}
		break
	}
	other, err := r.Open("s2", resolved, OpenOptions{Excludes: []string{"vendor"}})
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("other options did not rebuild the root")
	}
}

// A takeover warms each root with the options it was built with, so the
// clients' re-opens on the new daemon find it the same.
func TestWarmArgCarriesTheOptions(t *testing.T) {
	if got := warmArg("/w/a", OpenOptions{}); got != "/w/a" {
		t.Errorf("no options: %q", got)
	}
	depth := 2
	opts := OpenOptions{Excludes: []string{"extra"}, Budget: "5s", SymlinkDepth: &depth}
	root, back := parseWarm(warmArg("/w/a", opts))
	if root != "/w/a" || back.String() != opts.String() {
		t.Errorf("round trip: %q %v", root, back)
	}
	if root, back := parseWarm("/w/b"); root != "/w/b" || back.String() != (OpenOptions{}).String() {
		t.Errorf("plain root: %q %v", root, back)
	}
}
