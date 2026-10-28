package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// The user's server registry: <daemon home>/servers.yaml, written by
// `poly-lsp-mcp lsp add` / `lsp remove`. It is user-level, never a
// project's: the shared daemon runs the commands it names, and a file in a
// repository must not make the daemon run commands. The CLI owns the
// format; a person may edit it, and every reader validates it the same way
// (MergeServers).
//
// Its entries merge over the built-in languages BY NAME: a new name adds a
// language (child LSP only: no tree-sitter grammar), an existing name
// overrides the fields the entry gives and keeps the rest. Removing an
// entry restores the built-in. A project poly-lsp-mcp.yaml that declares
// languages: replaces the registry, and servers.yaml is then not merged.

// ServersFile is the user registry's file name in the daemon home.
const ServersFile = "servers.yaml"

// Servers is servers.yaml.
type Servers struct {
	Servers []UserServer `yaml:"servers"`
}

// UserServer is one servers.yaml entry. Empty fields of an entry that
// overrides a built-in keep the built-in's value.
type UserServer struct {
	Name       string   `yaml:"name"`
	Extensions []string `yaml:"extensions,omitempty"`
	Cmd        string   `yaml:"cmd,omitempty"`
	Args       []string `yaml:"args,omitempty"`
	Env        []string `yaml:"env,omitempty"`
	// RootMarkers: stored, not yet used (see Language.RootMarkers).
	RootMarkers           []string `yaml:"root_markers,omitempty"`
	OffByDefault          bool     `yaml:"off_by_default,omitempty"`
	TrustCleanDiagnostics *bool    `yaml:"trust_clean_diagnostics,omitempty"`
}

// LoadServers reads a servers.yaml. A missing file is an empty registry.
func LoadServers(path string) (*Servers, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Servers{}, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseServers(path, data)
}

// ParseServers decodes servers.yaml bytes; path names it in errors.
func ParseServers(path string, data []byte) (*Servers, error) {
	var s Servers
	if len(bytes.TrimSpace(data)) == 0 {
		return &s, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i := range s.Servers {
		s.Servers[i].normalize()
	}
	return &s, nil
}

func (u *UserServer) normalize() {
	for i, e := range u.Extensions {
		u.Extensions[i] = normalizeExt(e)
	}
}

// Save writes s to path atomically (a temp file in the same directory,
// then rename), creating the directory 0700 if missing.
func (s *Servers) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("# poly-lsp-mcp user servers; written by `poly-lsp-mcp lsp add/remove`.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(s); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Find returns the entry named name, or nil.
func (s *Servers) Find(name string) *UserServer {
	for i := range s.Servers {
		if s.Servers[i].Name == name {
			return &s.Servers[i]
		}
	}
	return nil
}

// Add puts u in s, replacing an entry of the same name, after checking
// that the result merges over base (MergeServers). s is unchanged on error.
func (s *Servers) Add(base []Language, u UserServer) error {
	u.normalize()
	next := make([]UserServer, 0, len(s.Servers)+1)
	replaced := false
	for _, e := range s.Servers {
		if e.Name == u.Name {
			next = append(next, u)
			replaced = true
			continue
		}
		next = append(next, e)
	}
	if !replaced {
		next = append(next, u)
	}
	if _, err := MergeServers(base, next); err != nil {
		return err
	}
	s.Servers = next
	return nil
}

// Remove drops the entry named name and reports whether there was one.
func (s *Servers) Remove(name string) bool {
	for i, e := range s.Servers {
		if e.Name == name {
			s.Servers = append(s.Servers[:i:i], s.Servers[i+1:]...)
			return true
		}
	}
	return false
}

// MergeServers returns base with user merged over it by language name, and
// checks the result: every entry has a name, at most one entry per name,
// a command (its own or the built-in's), and no extension claimed by two
// languages. base is not modified.
func MergeServers(base []Language, user []UserServer) ([]Language, error) {
	out := make([]Language, len(base))
	for i, l := range base {
		out[i] = l.clone()
	}
	seen := map[string]bool{}
	for i, u := range user {
		if strings.TrimSpace(u.Name) == "" {
			return nil, fmt.Errorf("server %d: name is required", i+1)
		}
		if seen[u.Name] {
			return nil, fmt.Errorf("server %s: listed twice", u.Name)
		}
		seen[u.Name] = true
		idx := -1
		for j := range out {
			if out[j].Name == u.Name {
				idx = j
				break
			}
		}
		if idx < 0 {
			out = append(out, Language{Name: u.Name})
			idx = len(out) - 1
		}
		if err := u.apply(&out[idx]); err != nil {
			return nil, err
		}
	}
	// One owner per extension.
	owner := map[string]string{}
	for _, l := range out {
		for _, e := range l.Extensions {
			e = normalizeExt(e)
			if o, ok := owner[e]; ok && o != l.Name {
				return nil, fmt.Errorf("extension .%s is claimed by both %s and %s; to move it, override %s with other extensions (lsp add %s --ext ...)", e, o, l.Name, o, o)
			}
			owner[e] = l.Name
		}
	}
	return out, nil
}

// apply overrides l with the fields u gives.
func (u UserServer) apply(l *Language) error {
	if len(u.Extensions) > 0 {
		l.Extensions = make([]string, 0, len(u.Extensions))
		for _, e := range u.Extensions {
			e = normalizeExt(e)
			if e == "" {
				return fmt.Errorf("server %s: empty extension", u.Name)
			}
			l.Extensions = append(l.Extensions, e)
		}
	}
	if len(l.Extensions) == 0 {
		return fmt.Errorf("server %s: at least one extension is required (--ext)", u.Name)
	}
	if u.Cmd != "" {
		l.LSP = &LSP{Cmd: u.Cmd}
	}
	if l.LSP == nil || strings.TrimSpace(l.LSP.Cmd) == "" {
		return fmt.Errorf("server %s: cmd is required (--cmd)", u.Name)
	}
	if u.Args != nil {
		l.LSP.Args = append([]string(nil), u.Args...)
	}
	if u.Env != nil {
		for _, kv := range u.Env {
			if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
				return fmt.Errorf("server %s: env %q: want KEY=VALUE", u.Name, kv)
			}
		}
		l.LSP.Env = append([]string(nil), u.Env...)
	}
	if u.TrustCleanDiagnostics != nil {
		v := *u.TrustCleanDiagnostics
		l.LSP.TrustCleanDiagnostics = &v
	}
	if u.RootMarkers != nil {
		l.RootMarkers = append([]string(nil), u.RootMarkers...)
	}
	if u.OffByDefault {
		l.OffByDefault = true
	}
	return nil
}

// clone copies l deeply enough that a merge never writes through to the
// language it came from.
func (l Language) clone() Language {
	out := l
	out.Extensions = append([]string(nil), l.Extensions...)
	out.RootMarkers = append([]string(nil), l.RootMarkers...)
	if l.LSP != nil {
		c := *l.LSP
		c.Args = append([]string(nil), l.LSP.Args...)
		c.Env = append([]string(nil), l.LSP.Env...)
		out.LSP = &c
	}
	return out
}

// MergeServersFile merges the servers.yaml at path over cfg's languages,
// unless cfg's languages came from a project file (ProjectLanguages). A
// missing file changes nothing.
func MergeServersFile(cfg *Config, path string) error {
	if cfg.ProjectLanguages || path == "" {
		return nil
	}
	s, err := LoadServers(path)
	if err != nil {
		return err
	}
	langs, err := MergeServers(cfg.Languages, s.Servers)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	cfg.Languages = langs
	return nil
}

// LSPOff is the set of languages whose child LSP a client does not get:
// those off by default that on does not name, and those off names. Names
// the registry does not hold are ignored.
func (r *Registry) LSPOff(on, off []string) map[string]bool {
	set := map[string]bool{}
	turnedOn := map[string]bool{}
	for _, n := range on {
		turnedOn[n] = true
	}
	for _, l := range r.order {
		if l.OffByDefault && !turnedOn[l.Name] {
			set[l.Name] = true
		}
	}
	for _, n := range off {
		if r.byName[n] != nil {
			set[n] = true
		}
	}
	return set
}
