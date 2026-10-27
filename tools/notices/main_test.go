package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	for name, want := range map[string]string{
		"Permission is hereby granted, free of charge, to any person":          "MIT",
		"Apache License\n Version 2.0, January 2004":                           "Apache-2.0",
		"GNU LESSER GENERAL PUBLIC LICENSE and the GNU GENERAL PUBLIC LICENSE": "LGPL",
		"Redistributions in binary form must reproduce the above copyright":    "BSD-2-Clause",
		"something nobody has read":                                            "",
	} {
		if got := classify(name); got != want {
			t.Errorf("classify(%q) = %q, want %q", name, got, want)
		}
	}
}

// Every vendored grammar license must read as an allowed license.
func TestVendoredLicenses(t *testing.T) {
	for _, g := range grammars {
		b, err := licenseFS.ReadFile("licenses/" + g.File + ".txt")
		if err != nil {
			t.Fatal(err)
		}
		if lic := classify(string(b)); !allowed[lic] {
			t.Errorf("%s: license %q", g.File, lic)
		}
	}
}

// The grammars table pins the revisions smacker vendors. When go.mod moves
// the smacker pin, this fails until the licenses are re-fetched and the table
// updated.
func TestGrammarPins(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/smacker/go-tree-sitter").Output()
	if err != nil {
		t.Skip("smacker module not available:", err)
	}
	b, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "_automation", "grammars.json"))
	if err != nil {
		t.Fatal(err)
	}
	var up []struct{ Language, URL, Revision string }
	if err := json.Unmarshal(b, &up); err != nil {
		t.Fatal(err)
	}
	rev := map[string]string{}
	for _, u := range up {
		rev[u.URL] = u.Revision
	}
	for _, g := range grammars {
		if g.File == "core" {
			continue // the C runtime is not in grammars.json
		}
		if rev[g.URL] != g.Rev {
			t.Errorf("%s: table pins %s, smacker vendors %s", g.File, g.Rev, rev[g.URL])
		}
	}
}
