package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ShadowRule hides a lesser-typed file behind a stronger-typed one of the
// same name: in one directory, a file ending in one of Over hides the file
// with the same stem ending in one of Hides.
//
// The common case is compiled output next to its source. visionbridge
// (2026-10-24) keeps 19 shared/src/*.js beside the .ts they were compiled
// from; couch has 39 .js behind .d.ts. Indexed together, every name in them
// is defined twice, so `#apiClient` answers twice and ::in/::out cannot tell
// which definition a call reaches. A shadowed file is out of the index,
// broad node_query and child-LSP opens, and still reachable by naming its
// path.
type ShadowRule struct {
	Name  string   `yaml:"name"`
	Over  []string `yaml:"over"`  // suffixes without the leading dot: "ts", "d.ts"
	Hides []string `yaml:"hides"` // suffixes without the leading dot: "js"
}

// DefaultShadows are the rules used when the config does not set
// `shadows:`. `shadows: []` turns shadowing off.
func DefaultShadows() []ShadowRule {
	return []ShadowRule{
		// tsc writes foo.js and (with --declaration) foo.d.ts beside foo.ts.
		{Name: "ts over js", Over: []string{"ts", "tsx", "mts", "cts"}, Hides: []string{"js", "jsx", "mjs", "cjs"}},
		{Name: "ts over d.ts", Over: []string{"ts", "tsx", "mts", "cts"}, Hides: []string{"d.ts", "d.mts", "d.cts"}},
		// A package's typed surface over its compiled body.
		{Name: "d.ts over js", Over: []string{"d.ts", "d.mts", "d.cts"}, Hides: []string{"js", "mjs", "cjs"}},
		// A stub over its module.
		{Name: "pyi over py", Over: []string{"pyi"}, Hides: []string{"py"}},
	}
}

// ShadowChecker answers whether a file is shadowed, reading each directory
// once. Safe for concurrent use. Make one per walk: it caches directory
// listings, so a long-lived checker misses files created after it read.
type ShadowChecker struct {
	rules []ShadowRule
	mu    sync.Mutex
	dirs  map[string]map[string]bool
}

// NewShadowChecker returns a checker for the registry's rules; nil (safe to
// call, never shadows) when there are none.
func (r *Registry) NewShadowChecker() *ShadowChecker {
	if r == nil || len(r.shadows) == 0 {
		return nil
	}
	return &ShadowChecker{rules: r.shadows, dirs: map[string]map[string]bool{}}
}

// Shadowed reports whether the file at path is hidden by a stronger sibling,
// and by which rule.
func (c *ShadowChecker) Shadowed(path string) (rule string, ok bool) {
	if c == nil {
		return "", false
	}
	dir, name := filepath.Split(path)
	lower := strings.ToLower(name)
	var siblings map[string]bool
	for _, r := range c.rules {
		for _, h := range r.Hides {
			if !strings.HasSuffix(lower, "."+h) {
				continue
			}
			stem := name[:len(name)-len(h)-1]
			if siblings == nil {
				siblings = c.list(dir)
			}
			for _, o := range r.Over {
				if siblings[strings.ToLower(stem+"."+o)] {
					return r.Name, true
				}
			}
		}
	}
	return "", false
}

func (c *ShadowChecker) list(dir string) map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.dirs[dir]; ok {
		return s
	}
	s := map[string]bool{}
	if des, err := os.ReadDir(dir); err == nil {
		for _, de := range des {
			if !de.IsDir() {
				s[strings.ToLower(de.Name())] = true
			}
		}
	}
	c.dirs[dir] = s
	return s
}
