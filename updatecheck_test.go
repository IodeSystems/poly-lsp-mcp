package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func bi(version string, settings ...string) *debug.BuildInfo {
	b := &debug.BuildInfo{Main: debug.Module{Path: "github.com/iodesystems/poly-lsp-mcp", Version: version}}
	for i := 0; i+1 < len(settings); i += 2 {
		b.Settings = append(b.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
	}
	return b
}

func TestClassifyBuild(t *testing.T) {
	const rev = "46caee4f6393495e93a609e93fca774521d2c309"
	tests := []struct {
		name    string
		bi      *debug.BuildInfo
		release bool
		want    buildOrigin
	}{
		{"no build info", nil, false, buildOrigin{}},
		{"go install", bi("v0.9.2"), false, buildOrigin{Origin: "go install", Current: "v0.9.2"}},
		{"test binary or go run", bi(""), false, buildOrigin{}},
		{"(devel) without vcs", bi("(devel)"), false, buildOrigin{}},
		{"release", bi("v0.9.4", "vcs.revision", rev), true, buildOrigin{Origin: "release", Current: "v0.9.4"}},
		{"release untagged", bi("v0.0.0-20261024175639-46caee4f6393"), true,
			buildOrigin{Origin: "release", Current: "v0.0.0-20261024175639-46caee4f6393"}},
		{"release without a version", bi("(devel)"), true,
			buildOrigin{Origin: "release", Current: "(devel)", Note: "version is not a release tag: can't compare"}},
		{"source", bi("v0.0.0-20261024175639-46caee4f6393", "vcs.revision", rev, "vcs.modified", "false"), false,
			buildOrigin{Origin: "source", Current: rev[:12]}},
		{"source dirty", bi("(devel)", "vcs.revision", rev, "vcs.modified", "true"), false,
			buildOrigin{Origin: "source", Current: rev[:12] + "+dirty", Modified: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyBuild(tt.bi, tt.release); got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSemverNewer(t *testing.T) {
	tests := []struct {
		latest, current string
		newer, ok       bool
	}{
		{"v0.9.4", "v0.9.2", true, true},
		{"v0.9.2", "v0.9.2", false, true},
		{"v0.9.2", "v0.9.4", false, true},
		{"v0.10.0", "v0.9.9", true, true}, // numeric, not lexical
		{"v1.0.0", "v1.0.0-rc.1", true, true},
		{"v1.0.0-rc.2", "v1.0.0-rc.10", false, true},
		{"v1.0.0+incompatible", "v1.0.0", false, true},
		// Pseudo-versions: v0.0.0-… precedes every tag.
		{"v0.1.0", "v0.0.0-20261024175639-46caee4f6393", true, true},
		// vX.Y.Z+1-0.… follows tag vX.Y.Z and precedes vX.Y.Z+1.
		{"v0.9.2", "v0.9.3-0.20261024175639-46caee4f6393", false, true},
		{"v0.9.3", "v0.9.3-0.20261024175639-46caee4f6393", true, true},
		{"v0.9.3-0.20261025000000-bbbbbbbbbbbb", "v0.9.3-0.20261024175639-46caee4f6393", true, true},
		// vX.Y.Z-pre.0.… (after prerelease vX.Y.Z-pre)
		{"v1.2.3-pre", "v1.2.3-pre.0.20261024175639-46caee4f6393", false, true},
		{"upstream", "v0.9.2", false, false},
		{"v0.9.2", "(devel)", false, false},
		{"v1.2", "v1.2.0", false, false},
	}
	for _, tt := range tests {
		newer, ok := semverNewer(tt.latest, tt.current)
		if newer != tt.newer || ok != tt.ok {
			t.Errorf("semverNewer(%q, %q) = %v,%v, want %v,%v", tt.latest, tt.current, newer, ok, tt.newer, tt.ok)
		}
	}
}

// fakeServer answers every path with body and counts requests.
func fakeServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func testChecker(t *testing.T) *updateChecker {
	t.Helper()
	return &updateChecker{
		client:     &http.Client{Timeout: time.Second},
		releaseURL: "http://127.0.0.1:1/unreachable", // a test that forgets to point it fails loudly
		proxyURL:   "http://127.0.0.1:1/unreachable",
		cacheFile:  filepath.Join(t.TempDir(), "cache", "update-check.json"),
		now:        time.Now,
		goos:       "linux",
		goarch:     "arm64",
	}
}

func TestUpdateReleaseLookup(t *testing.T) {
	srv, _ := fakeServer(t, 200, `{"tag_name":"v0.9.4","name":"v0.9.4"}`)
	c := testChecker(t)
	c.releaseURL = srv.URL
	u := c.check(buildOrigin{Origin: "release", Current: "v0.9.2"})
	if u == nil || !u.Available || u.Latest != "v0.9.4" {
		t.Fatalf("got %+v", u)
	}
	if want := "https://github.com/IodeSystems/poly-lsp-mcp/releases/latest/download/poly-lsp-mcp_linux_arm64.tar.gz"; u.How != want {
		t.Errorf("how = %q, want %q", u.How, want)
	}
	if want := "release v0.9.2 → v0.9.4 available: " + u.How; u.text() != want {
		t.Errorf("text = %q, want %q", u.text(), want)
	}
}

func TestUpdateProxyLookup(t *testing.T) {
	srv, _ := fakeServer(t, 200, `{"Version":"v0.9.4","Time":"2026-10-01T00:00:00Z"}`)
	c := testChecker(t)
	c.proxyURL = srv.URL

	u := c.check(buildOrigin{Origin: "go install", Current: "v0.9.2"})
	if u == nil || !u.Available || u.Latest != "v0.9.4" || u.How != "go install github.com/iodesystems/poly-lsp-mcp@latest" {
		t.Fatalf("got %+v", u)
	}
	if want := "go install v0.9.2 → v0.9.4 available: go install github.com/iodesystems/poly-lsp-mcp@latest"; u.text() != want {
		t.Errorf("text = %q, want %q", u.text(), want)
	}

	u = c.check(buildOrigin{Origin: "go install", Current: "v0.9.4"})
	if u == nil || u.Available || u.How != "" || u.text() != "up to date (v0.9.4, go install)" {
		t.Fatalf("got %+v / %q", u, u.text())
	}

	// A build newer than the published latest is not an update.
	u = c.check(buildOrigin{Origin: "go install", Current: "v0.9.5-0.20261024175639-46caee4f6393"})
	if u == nil || u.Available {
		t.Fatalf("got %+v", u)
	}
}

func TestUpdateCacheHitAndExpiry(t *testing.T) {
	srv, n := fakeServer(t, 200, `{"Version":"v0.9.4"}`)
	c := testChecker(t)
	c.proxyURL = srv.URL
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	bo := buildOrigin{Origin: "go install", Current: "v0.9.2"}

	c.check(bo)
	now = now.Add(23 * time.Hour)
	if u := c.check(bo); u == nil || !u.Available || u.Note != "" {
		t.Fatalf("fresh hit: %+v", u)
	}
	if n.Load() != 1 {
		t.Fatalf("requests = %d, want 1 (cache hit)", n.Load())
	}

	// Keyed by current: another build is a miss.
	c.check(buildOrigin{Origin: "go install", Current: "v0.9.3"})
	if n.Load() != 2 {
		t.Fatalf("requests = %d, want 2 (different current)", n.Load())
	}

	now = now.Add(2 * time.Hour) // 25h since the first
	c.check(bo)
	if n.Load() != 3 {
		t.Fatalf("requests = %d, want 3 (expired)", n.Load())
	}
}

func TestUpdateOffline(t *testing.T) {
	srv, _ := fakeServer(t, 200, `{"Version":"v0.9.4"}`)
	c := testChecker(t)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	bo := buildOrigin{Origin: "go install", Current: "v0.9.2"}

	// Offline, nothing cached: no block, no error.
	c.proxyURL = "http://127.0.0.1:1/unreachable"
	if u := c.check(bo); u != nil {
		t.Fatalf("offline with an empty cache should give nothing, got %+v", u)
	}
	// A server error is the same as offline.
	bad, _ := fakeServer(t, 503, `down`)
	c.proxyURL = bad.URL
	if u := c.check(bo); u != nil {
		t.Fatalf("503 with an empty cache should give nothing, got %+v", u)
	}

	// Cache once, let it expire, go offline: the old answer, marked cached.
	c.proxyURL = srv.URL
	c.check(bo)
	now = now.Add(30 * time.Hour)
	c.proxyURL = "http://127.0.0.1:1/unreachable"
	u := c.check(bo)
	if u == nil || !u.Available || u.Latest != "v0.9.4" || u.Note != "cached 30h ago" {
		t.Fatalf("got %+v", u)
	}
}

func TestUpdateModifiedCheckout(t *testing.T) {
	c := testChecker(t) // any request fails: this must not make one
	u := c.check(buildOrigin{Origin: "source", Current: "46caee4f6393+dirty", Modified: true})
	if u == nil || u.Note != "modified checkout: not checking" || u.Latest != "" || u.Available {
		t.Fatalf("got %+v", u)
	}
	if want := "source 46caee4f6393+dirty: modified checkout: not checking"; u.text() != want {
		t.Errorf("text = %q, want %q", u.text(), want)
	}
}

func TestUpdateSourceWithoutPath(t *testing.T) {
	c := testChecker(t)
	u := c.check(buildOrigin{Origin: "source", Current: "46caee4f6393"})
	want := "built from a checkout at 46caee4f6393; can't compare without its path (git pull && make install there)"
	if u == nil || u.Note != want || u.Latest != "" {
		t.Fatalf("got %+v", u)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestUpdateSourceUpstream(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	clone := filepath.Join(tmp, "clone")
	if err := os.Mkdir(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, origin, "init", "-q", "-b", "main")
	runGit(t, origin, "commit", "-q", "--allow-empty", "-m", "one")
	runGit(t, tmp, "clone", "-q", origin, clone)

	c := testChecker(t)
	c.rebuildPath = clone
	bo := buildOrigin{Origin: "source", Current: "abc123"}

	u := c.check(bo)
	if u == nil || u.Available || u.Latest != "upstream" || u.text() != "up to date (abc123, source)" {
		t.Fatalf("level with upstream: %+v", u)
	}

	runGit(t, origin, "commit", "-q", "--allow-empty", "-m", "two")
	runGit(t, origin, "commit", "-q", "--allow-empty", "-m", "three")
	c.cacheFile = filepath.Join(tmp, "fresh-cache.json") // skip the 24 h hit
	u = c.check(bo)
	if u == nil || !u.Available || u.Latest != "upstream (+2)" {
		t.Fatalf("behind: %+v", u)
	}
	if want := "git -C " + clone + " pull && make -C " + clone + " install"; u.How != want {
		t.Errorf("how = %q, want %q", u.How, want)
	}
}

func TestUpdateJSONFieldNames(t *testing.T) {
	b, err := json.Marshal(doctorReport{Update: &updateInfo{
		Origin: "go install", Current: "v0.9.2", Latest: "v0.9.4", Available: true,
		How: "go install x", Note: "n", CheckedAt: "2026-10-24T12:00:00Z",
	}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	got, _ := m["update"].(map[string]any)
	for _, k := range []string{"origin", "current", "latest", "available", "how", "note", "checkedAt"} {
		if _, ok := got[k]; !ok {
			t.Errorf("update.%s missing in %s", k, b)
		}
	}
	if len(got) != 7 {
		t.Errorf("update has %d fields, want 7: %s", len(got), b)
	}

	// Absent when nothing is known; omitempty fields drop out.
	b, _ = json.Marshal(doctorReport{})
	if strings.Contains(string(b), `"update"`) {
		t.Errorf("update should be absent: %s", b)
	}
	b, _ = json.Marshal(updateInfo{Origin: "source", Current: "abc", CheckedAt: "t"})
	if want := `{"origin":"source","current":"abc","available":false,"checkedAt":"t"}`; string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}
