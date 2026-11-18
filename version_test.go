package main

import (
	"bytes"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func TestVersionLine(t *testing.T) {
	plat := runtime.GOOS + "/" + runtime.GOARCH
	const rev = "4530caa7e9d635abcdef0123456789abcdef0123"
	vcs := func(modified string) []debug.BuildSetting {
		return []debug.BuildSetting{
			{Key: "vcs.revision", Value: rev},
			{Key: "vcs.time", Value: "2026-11-15T10:11:12Z"},
			{Key: "vcs.modified", Value: modified},
		}
	}
	for _, c := range []struct {
		name    string
		bi      *debug.BuildInfo
		release bool
		want    string
	}{
		{"release", &debug.BuildInfo{Main: debug.Module{Version: "v0.5.2"}, Settings: vcs("false")}, true,
			"poly-lsp-mcp v0.5.2 (4530caa, 2026-11-15, release, " + plat + ")"},
		{"dirty checkout", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: vcs("true")}, false,
			"poly-lsp-mcp (devel) (4530caa-dirty, 2026-11-15, checkout, " + plat + ")"},
		{"go install", &debug.BuildInfo{Main: debug.Module{Version: "v0.5.2"}}, false,
			"poly-lsp-mcp v0.5.2 (go install, " + plat + ")"},
		{"no build info", nil, false,
			"poly-lsp-mcp unknown (go install, " + plat + ")"},
	} {
		if got := versionLine(c.bi, c.release); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestVersionFlag(t *testing.T) {
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	line := out.String()
	if !strings.HasPrefix(line, "poly-lsp-mcp ") || strings.Count(line, "\n") != 1 {
		t.Fatalf("--version printed %q, want one line starting with %q", line, "poly-lsp-mcp ")
	}
}
