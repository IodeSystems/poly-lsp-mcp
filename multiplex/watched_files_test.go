package multiplex

import (
	"encoding/json"
	"testing"
)

func mustWatcher(t *testing.T, raw string) fileWatcher {
	t.Helper()
	w, ok := compileWatcher(json.RawMessage(raw))
	if !ok {
		t.Fatalf("compileWatcher(%s) failed", raw)
	}
	return w
}

// The watchers below are the shapes measured from the real servers:
// gopls 0.24 and typescript-language-server 6.1.0 (docs/design.md).
func TestFileWatcherMatches(t *testing.T) {
	cases := []struct {
		watcher string
		path    string
		typ     FileChangeType
		want    bool
	}{
		// gopls: a plain string pattern matches the whole path.
		{`{"globPattern":"**/*.{mod,work}","kind":7}`, "/r/go.mod", FileChanged, true},
		{`{"globPattern":"**/*.{mod,work}","kind":7}`, "/elsewhere/x/go.work", FileCreated, true},
		{`{"globPattern":"**/*.{mod,work}","kind":7}`, "/r/go.sum", FileChanged, false},
		// gopls: relative to the root.
		{`{"globPattern":{"baseUri":"file:///r","pattern":"**/*.{go,mod,sum,work}"},"kind":7}`, "/r/a/b.go", FileDeleted, true},
		{`{"globPattern":{"baseUri":"file:///r","pattern":"**/*.{go,mod,sum,work}"},"kind":7}`, "/r2/b.go", FileChanged, false},
		{`{"globPattern":{"baseUri":"file:///r","pattern":"**/*.{go,mod,sum,work}"},"kind":7}`, "/r/b.txt", FileChanged, false},
		// tsserver: everything under a base, the base itself excluded.
		{`{"globPattern":{"baseUri":"file:///r","pattern":"**/*"},"kind":7}`, "/r/node_modules", FileChanged, true},
		{`{"globPattern":{"baseUri":"file:///r","pattern":"**/*"},"kind":7}`, "/r/src/a.ts", FileCreated, true},
		{`{"globPattern":{"baseUri":"file:///r","pattern":"**/*"},"kind":7}`, "/r", FileChanged, false},
		{`{"globPattern":{"baseUri":"file:///r","pattern":"package.json"},"kind":7}`, "/r/package.json", FileChanged, true},
		{`{"globPattern":{"baseUri":"file:///r","pattern":"package.json"},"kind":7}`, "/r/sub/package.json", FileChanged, false},
		// baseUri as a WorkspaceFolder.
		{`{"globPattern":{"baseUri":{"uri":"file:///r","name":"r"},"pattern":"*.ts"}}`, "/r/a.ts", FileChanged, true},
		// kind: a bitmask of create 1, change 2, delete 4; absent is 7.
		{`{"globPattern":"**/*.ts","kind":1}`, "/r/a.ts", FileCreated, true},
		{`{"globPattern":"**/*.ts","kind":1}`, "/r/a.ts", FileChanged, false},
		{`{"globPattern":"**/*.ts","kind":6}`, "/r/a.ts", FileDeleted, true},
		{`{"globPattern":"**/*.ts"}`, "/r/a.ts", FileDeleted, true},
		// `*` and `?` stay in one segment; `[..]` is a class.
		{`{"globPattern":{"baseUri":"file:///r","pattern":"*.ts"}}`, "/r/a/b.ts", FileChanged, false},
		{`{"globPattern":{"baseUri":"file:///r","pattern":"?.ts"}}`, "/r/a.ts", FileChanged, true},
		{`{"globPattern":{"baseUri":"file:///r","pattern":"[ab].ts"}}`, "/r/c.ts", FileChanged, false},
		{`{"globPattern":{"baseUri":"file:///r","pattern":"src/**/x.ts"}}`, "/r/src/x.ts", FileChanged, true},
		{`{"globPattern":{"baseUri":"file:///r","pattern":"src/**/x.ts"}}`, "/r/src/a/b/x.ts", FileChanged, true},
		// Nested braces.
		{`{"globPattern":"**/*.{ts,{m,c}js}"}`, "/r/a.cjs", FileChanged, true},
		{`{"globPattern":"**/*.{ts,{m,c}js}"}`, "/r/a.js", FileChanged, false},
	}
	for _, c := range cases {
		w := mustWatcher(t, c.watcher)
		if got := w.matches(c.path, c.typ); got != c.want {
			t.Errorf("%s matches(%s, %d) = %v, want %v", c.watcher, c.path, c.typ, got, c.want)
		}
	}
}

func TestCompileWatcherRejects(t *testing.T) {
	for _, raw := range []string{
		`{"globPattern":""}`,
		`{"globPattern":{"baseUri":"http://x/y","pattern":"*"}}`,
		`{"globPattern":42}`,
	} {
		if _, ok := compileWatcher(json.RawMessage(raw)); ok {
			t.Errorf("compileWatcher(%s) ok, want rejected", raw)
		}
	}
}

func TestHandleRegistration(t *testing.T) {
	c := &Child{name: "t"}
	reg := `{"registrations":[{"id":"w1","method":"workspace/didChangeWatchedFiles","registerOptions":{"watchers":[{"globPattern":"**/*.go"},{"globPattern":"**/go.mod"}]}}]}`
	if msg := c.handleRegistration("client/registerCapability", json.RawMessage(reg)); msg != "" {
		t.Fatalf("register: %s", msg)
	}
	if n := c.FileWatchers(); n != 2 {
		t.Fatalf("FileWatchers = %d, want 2", n)
	}
	if !c.WantsFileEvent("/r/a.go", FileChanged) || c.WantsFileEvent("/r/a.ts", FileChanged) {
		t.Fatal("WantsFileEvent does not follow the registered watchers")
	}

	// A registration of any other method is refused whole and stores
	// nothing: this client implements no other dynamic registration.
	mixed := `{"registrations":[{"id":"w2","method":"workspace/didChangeWatchedFiles","registerOptions":{"watchers":[{"globPattern":"**/*.ts"}]}},{"id":"f","method":"textDocument/formatting"}]}`
	if msg := c.handleRegistration("client/registerCapability", json.RawMessage(mixed)); msg == "" {
		t.Fatal("a registration of textDocument/formatting was accepted")
	}
	if c.WantsFileEvent("/r/a.ts", FileChanged) {
		t.Fatal("a refused request stored its watchers")
	}

	// didChangeConfiguration is accepted and stores no watcher (TS 7).
	cfg := `{"registrations":[{"id":"typescript-config-watch-id","method":"workspace/didChangeConfiguration","registerOptions":{"section":["typescript"]}}]}`
	if msg := c.handleRegistration("client/registerCapability", json.RawMessage(cfg)); msg != "" {
		t.Fatalf("register didChangeConfiguration: %s", msg)
	}
	if n := c.FileWatchers(); n != 2 {
		t.Fatalf("FileWatchers = %d after didChangeConfiguration, want 2", n)
	}

	unreg := `{"unregisterations":[{"id":"w1","method":"workspace/didChangeWatchedFiles"}]}`
	if msg := c.handleRegistration("client/unregisterCapability", json.RawMessage(unreg)); msg != "" {
		t.Fatalf("unregister: %s", msg)
	}
	if c.FileWatchers() != 0 || c.WantsFileEvent("/r/a.go", FileChanged) {
		t.Fatal("unregister left watchers")
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string][3]int{
		"6.1.0\n":       {6, 1, 0},
		"v5.1.3":        {5, 1, 3},
		"tsls 6.2.0-rc": {6, 2, 0},
	} {
		got, ok := parseVersion(in)
		if !ok || got != want {
			t.Errorf("parseVersion(%q) = %v %v, want %v", in, got, ok, want)
		}
	}
	if _, ok := parseVersion("unknown"); ok {
		t.Error("parseVersion(unknown) ok")
	}
}

func TestInitializationOptions(t *testing.T) {
	orig := tslsVersionOf
	defer func() { tslsVersionOf = orig }()
	versions := map[string]string{"/a/typescript-language-server": "6.1.0", "/b/typescript-language-server": "5.1.3"}
	calls := 0
	tslsVersionOf = func(path string, _ []string) string { calls++; return versions[path] }
	tslsVersions.Delete("/a/typescript-language-server")
	tslsVersions.Delete("/b/typescript-language-server")

	if got := initializationOptions("ts", "/a/typescript-language-server", nil); got == nil {
		t.Fatal("6.1.0: no initializationOptions")
	} else if b, _ := json.Marshal(got); string(b) != `{"tsserver":{"useClientFileWatcher":true}}` {
		t.Fatalf("6.1.0: got %s", b)
	}
	if got := initializationOptions("ts", "/b/typescript-language-server", nil); got != nil {
		t.Fatalf("5.1.3: got %v, want nil", got)
	}
	if got := initializationOptions("go", "/x/gopls", nil); got != nil {
		t.Fatalf("gopls: got %v, want nil", got)
	}
	initializationOptions("ts", "/a/typescript-language-server", nil)
	if calls != 2 {
		t.Fatalf("--version ran %d times, want 2 (cached per path)", calls)
	}
}
