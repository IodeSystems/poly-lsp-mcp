package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestShadowDefaults(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"api.ts", "api.js", "api.d.ts", // compiled beside its source
		"lib.d.ts", "lib.js", // a package's types over its body
		"mod.pyi", "mod.py",
		"alone.js", "alone.py", "Comp.tsx", "Comp.jsx",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	c := reg.NewShadowChecker()
	for name, want := range map[string]string{
		"api.js":   "ts over js",
		"api.d.ts": "ts over d.ts",
		"lib.js":   "d.ts over js",
		"mod.py":   "pyi over py",
		"Comp.jsx": "ts over js",
		"api.ts":   "",
		"lib.d.ts": "",
		"alone.js": "",
		"alone.py": "",
	} {
		got, _ := c.Shadowed(filepath.Join(dir, name))
		if got != want {
			t.Errorf("%s: shadowed by %q, want %q", name, got, want)
		}
	}
}

// `shadows: []` turns it off; a missing key keeps the defaults.
func TestShadowsEmptyListDisables(t *testing.T) {
	cfg := Default()
	cfg.Shadows = []ShadowRule{}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	if reg.NewShadowChecker() != nil {
		t.Error("shadows: [] should disable shadowing")
	}
}
