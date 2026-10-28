package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func langByName(t *testing.T, langs []Language, name string) *Language {
	t.Helper()
	for i := range langs {
		if langs[i].Name == name {
			return &langs[i]
		}
	}
	return nil
}

// A new name adds a language; an existing name overrides only the fields
// it gives; the base is not written through.
func TestMergeServersAddsAndOverrides(t *testing.T) {
	base := Default().Languages
	trust := true
	got, err := MergeServers(base, []UserServer{
		{Name: "zig", Extensions: []string{".zig", "ZON"}, Cmd: "zls", Args: []string{"--stdio"}, Env: []string{"A=b"}, RootMarkers: []string{"build.zig"}},
		{Name: "go", OffByDefault: true, TrustCleanDiagnostics: &trust},
	})
	if err != nil {
		t.Fatal(err)
	}
	zig := langByName(t, got, "zig")
	if zig == nil || zig.LSP == nil || zig.LSP.Cmd != "zls" || !reflect.DeepEqual(zig.Extensions, []string{"zig", "zon"}) ||
		!reflect.DeepEqual(zig.LSP.Args, []string{"--stdio"}) || zig.TreeSitter != "" || !reflect.DeepEqual(zig.RootMarkers, []string{"build.zig"}) {
		t.Errorf("zig: %+v", zig)
	}
	g := langByName(t, got, "go")
	if g.LSP.Cmd != "gopls" || !g.OffByDefault || g.TreeSitter != "go" || !g.LSP.TrustsCleanDiagnostics() {
		t.Errorf("go override: %+v %+v", g, g.LSP)
	}
	if b := langByName(t, base, "go"); b.OffByDefault || b.LSP.TrustCleanDiagnostics != nil {
		t.Errorf("merge wrote through to the base: %+v", b)
	}
	if len(got) != len(base)+1 {
		t.Errorf("languages: %d, want %d", len(got), len(base)+1)
	}
	if _, err := (&Config{Languages: got}).Build(); err != nil {
		t.Errorf("merged registry does not build: %v", err)
	}
}

// An override with a new command drops the built-in's arguments; one
// with new extensions frees the old ones.
func TestMergeServersOverrideReplacesFields(t *testing.T) {
	got, err := MergeServers(Default().Languages, []UserServer{
		{Name: "typescript", Cmd: "deno", Args: []string{"lsp"}},
		{Name: "cpp", Extensions: []string{"cpp", "cc"}},
		{Name: "c", Extensions: []string{"c", "h"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := langByName(t, got, "typescript")
	if ts.LSP.Cmd != "deno" || !reflect.DeepEqual(ts.LSP.Args, []string{"lsp"}) {
		t.Errorf("typescript: %+v", ts.LSP)
	}
	if c := langByName(t, got, "c"); !reflect.DeepEqual(c.Extensions, []string{"c", "h"}) {
		t.Errorf("c: %v", c.Extensions)
	}
}

func TestMergeServersRefuses(t *testing.T) {
	base := Default().Languages
	cases := map[string]struct {
		in   []UserServer
		want string
	}{
		"no name":         {[]UserServer{{Extensions: []string{"x"}, Cmd: "x"}}, "name is required"},
		"no cmd":          {[]UserServer{{Name: "zig", Extensions: []string{"zig"}}}, "cmd is required"},
		"no cmd, ts-only": {[]UserServer{{Name: "java", OffByDefault: true}}, "cmd is required"},
		"no ext":          {[]UserServer{{Name: "zig", Cmd: "zls"}}, "extension is required"},
		"claimed ext":     {[]UserServer{{Name: "golike", Extensions: []string{".go"}, Cmd: "x"}}, "claimed by both go and golike"},
		"twice":           {[]UserServer{{Name: "zig", Extensions: []string{"zig"}, Cmd: "zls"}, {Name: "zig", Extensions: []string{"zon"}, Cmd: "zls"}}, "listed twice"},
		"bad env":         {[]UserServer{{Name: "zig", Extensions: []string{"zig"}, Cmd: "zls", Env: []string{"NOEQUALS"}}}, "KEY=VALUE"},
	}
	for name, c := range cases {
		_, err := MergeServers(base, c.in)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	// Overriding go itself with .go is not a conflict.
	if _, err := MergeServers(base, []UserServer{{Name: "go", Extensions: []string{"go"}, Cmd: "gopls"}}); err != nil {
		t.Errorf("go overriding its own extension: %v", err)
	}
}

// Add validates before it changes anything, replaces by name; Remove
// restores the built-in.
func TestServersAddRemove(t *testing.T) {
	base := Default().Languages
	var s Servers
	if err := s.Add(base, UserServer{Name: "zig", Extensions: []string{"zig"}, Cmd: "zls"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(base, UserServer{Name: "bad", Extensions: []string{"zig"}, Cmd: "x"}); err == nil {
		t.Fatal("an extension another entry claims was accepted")
	}
	if len(s.Servers) != 1 {
		t.Fatalf("a refused add changed the registry: %+v", s.Servers)
	}
	if err := s.Add(base, UserServer{Name: "zig", Extensions: []string{"zig"}, Cmd: "zls2"}); err != nil {
		t.Fatal(err)
	}
	if len(s.Servers) != 1 || s.Servers[0].Cmd != "zls2" {
		t.Fatalf("add by the same name did not replace: %+v", s.Servers)
	}
	if err := s.Add(base, UserServer{Name: "go", Cmd: "my-gopls"}); err != nil {
		t.Fatal(err)
	}
	if !s.Remove("go") || s.Remove("go") {
		t.Fatal("remove")
	}
	got, err := MergeServers(base, s.Servers)
	if err != nil {
		t.Fatal(err)
	}
	if g := langByName(t, got, "go"); g.LSP.Cmd != "gopls" {
		t.Errorf("removing the override did not restore the built-in: %+v", g.LSP)
	}
}

// Save writes atomically (no temp file left) and Load reads back the same
// registry; a missing file is empty.
func TestServersFileRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home")
	path := filepath.Join(dir, ServersFile)
	empty, err := LoadServers(path)
	if err != nil || len(empty.Servers) != 0 {
		t.Fatalf("missing file: %v %+v", err, empty)
	}
	trust := false
	s := &Servers{Servers: []UserServer{
		{Name: "zig", Extensions: []string{"zig"}, Cmd: "zls", Args: []string{"--x"}, Env: []string{"K=V"}, RootMarkers: []string{"build.zig"}, OffByDefault: true, TrustCleanDiagnostics: &trust},
		{Name: "go", OffByDefault: true},
	}}
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := LoadServers(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, s) {
		t.Errorf("round trip:\n got %+v\nwant %+v", back.Servers, s.Servers)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("files in the home after save: %v", ents)
	}
	if err := os.WriteFile(path, []byte("servers:\n  - name: x\n    command: y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadServers(path); err == nil {
		t.Error("an unknown field was accepted")
	}
}

// servers.yaml merges over the defaults, and not over a project file's
// own languages: list.
func TestMergeServersFile(t *testing.T) {
	dir := t.TempDir()
	sp := filepath.Join(dir, ServersFile)
	if err := (&Servers{Servers: []UserServer{{Name: "zig", Extensions: []string{"zig"}, Cmd: "zls"}}}).Save(sp); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadOrDefault(filepath.Join(dir, "none.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := MergeServersFile(cfg, sp); err != nil {
		t.Fatal(err)
	}
	if langByName(t, cfg.Languages, "zig") == nil {
		t.Error("servers.yaml not merged over the defaults")
	}
	proj := filepath.Join(dir, "poly-lsp-mcp.yaml")
	if err := os.WriteFile(proj, []byte("languages:\n  - name: go\n    extensions: [go]\n    treesitter: go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = LoadOrDefault(proj)
	if err != nil {
		t.Fatal(err)
	}
	if err := MergeServersFile(cfg, sp); err != nil {
		t.Fatal(err)
	}
	if !cfg.ProjectLanguages || len(cfg.Languages) != 1 {
		t.Errorf("a project languages: list was merged over: %+v", cfg.Languages)
	}
}

func TestRegistryLSPOff(t *testing.T) {
	langs, err := MergeServers(Default().Languages, []UserServer{{Name: "zig", Extensions: []string{"zig"}, Cmd: "zls", OffByDefault: true}})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := (&Config{Languages: langs}).Build()
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.LSPOff(nil, nil); !reflect.DeepEqual(got, map[string]bool{"zig": true}) {
		t.Errorf("defaults: %v", got)
	}
	if got := reg.LSPOff([]string{"zig"}, []string{"go", "nosuch"}); !reflect.DeepEqual(got, map[string]bool{"go": true}) {
		t.Errorf("on zig, off go: %v", got)
	}
}

// format_on_edit: off by default; servers.yaml turns it on (or writes an
// explicit false); a client's on/off lists override it, off winning.
func TestFormatOnEdit(t *testing.T) {
	on, off := true, false
	langs, err := MergeServers(Default().Languages, []UserServer{
		{Name: "go", FormatOnEdit: &on},
		{Name: "python", FormatOnEdit: &off},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := (&Config{Languages: langs}).Build()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		lang    string
		on, off []string
		want    bool
	}{
		{"go", nil, nil, true},
		{"python", nil, nil, false},
		{"typescript", nil, nil, false},
		{"typescript", []string{"typescript"}, nil, true},
		{"go", nil, []string{"go"}, false},
		{"go", []string{"go"}, []string{"go"}, false},
		{"nosuch", []string{"nosuch"}, nil, false},
	}
	for _, c := range cases {
		if got := reg.FormatOnEdit(c.lang, c.on, c.off); got != c.want {
			t.Errorf("FormatOnEdit(%s, on %v, off %v) = %v, want %v", c.lang, c.on, c.off, got, c.want)
		}
	}
	if b := langByName(t, Default().Languages, "go"); b.FormatOnEdit {
		t.Error("format_on_edit is on by default")
	}
	// Round trip keeps an explicit false.
	path := filepath.Join(t.TempDir(), ServersFile)
	s := &Servers{Servers: []UserServer{{Name: "go", FormatOnEdit: &off}}}
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "format_on_edit: false") {
		t.Errorf("explicit false not written:\n%s", data)
	}
	back, err := LoadServers(path)
	if err != nil || !reflect.DeepEqual(back, s) {
		t.Errorf("round trip: %v %+v", err, back)
	}
}
