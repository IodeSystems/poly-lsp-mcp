// Command notices checks the licenses of everything a poly-lsp-mcp release
// ships and writes THIRD_PARTY_NOTICES, which goes in every release tarball.
//
// MIT, BSD and Apache-2.0 require their copyright notice in binary copies.
// Two kinds of third-party code are in the binary:
//
//   - Go modules: every module `go list -deps` reports for the release build
//     (cgo, -tags release). The set is the same on linux and darwin, amd64 and
//     arm64 (checked 2026-10-27), so one file serves the four tarballs.
//   - Tree-sitter grammars: smacker/go-tree-sitter vendors each grammar's
//     parser.c (and scanner.c) from an upstream repo and carries only its own
//     root LICENSE. The upstream license texts are vendored in licenses/ (from
//     the pinned revision in the grammars table below) so generation is
//     offline and reproducible.
//
// A dependency under a license outside the allow-list, or with a license text
// the classifier does not know, fails the run.
//
// Run from the repo root:
//
//	go run ./tools/notices -o THIRD_PARTY_NOTICES   write the file
//	go run ./tools/notices -check THIRD_PARTY_NOTICES   fail when it is stale
//
// Modules are listed with GOWORK=off: a developer's go.work swaps in local
// checkouts, and the release builds without one.
package main

import (
	"bytes"
	"embed"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

//go:embed licenses/*.txt
var licenseFS embed.FS

// allowed are the licenses poly-lsp-mcp (MIT) can ship without further terms.
var allowed = map[string]bool{
	"MIT": true, "BSD-2-Clause": true, "BSD-3-Clause": true, "Apache-2.0": true,
	"ISC": true, "0BSD": true, "Unlicense": true, "CC0-1.0": true, "Public-Domain": true,
}

// reviewed are license files that classify as unknown and were read by hand.
// Keyed by module path and file name, so a new unknown file fails until
// someone reads it.
var reviewed = map[string]string{}

// rules classify license text. Order matters: copyleft first, so a text that
// quotes another license is caught by the stricter one.
var rules = []struct {
	name string
	rx   *regexp.Regexp
}{
	{"AGPL", regexp.MustCompile(`(?i)GNU AFFERO GENERAL PUBLIC LICENSE`)},
	{"LGPL", regexp.MustCompile(`(?i)GNU LESSER GENERAL PUBLIC LICENSE|GNU LIBRARY GENERAL PUBLIC`)},
	{"GPL", regexp.MustCompile(`(?i)GNU GENERAL PUBLIC LICENSE`)},
	{"MPL-2.0", regexp.MustCompile(`(?i)Mozilla Public License,? (Version|v\.) ?2\.0`)},
	{"Apache-2.0", regexp.MustCompile(`(?i)Apache License,? Version 2\.0`)},
	{"BSD-3-Clause", regexp.MustCompile(`(?i)Neither the name|names of (its|the) contributors may be used`)},
	{"BSD-2-Clause", regexp.MustCompile(`(?i)Redistributions in binary form must reproduce`)},
	{"MIT", regexp.MustCompile(`(?i)Permission is hereby granted, free of charge`)},
	{"ISC", regexp.MustCompile(`(?i)Permission to use, copy, modify, and(/or)? distribute this software for any`)},
	{"Unlicense", regexp.MustCompile(`(?i)This is free and unencumbered software`)},
	{"CC0-1.0", regexp.MustCompile(`(?i)CC0 1\.0`)},
}

// classify names the license a text is, "" when no rule matches.
func classify(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	for _, r := range rules {
		if r.rx.MatchString(t) {
			return r.name
		}
	}
	return ""
}

var licenseFile = regexp.MustCompile(`(?i)^(licen[cs]e|copying|unlicense)`)
var noticeFile = regexp.MustCompile(`(?i)^notice`)

// grammar is one tree-sitter grammar (or the C runtime) compiled in through
// smacker/go-tree-sitter. Dir is its directory in that module, Rev the
// upstream commit smacker took the sources from (_automation/grammars.json in
// the module; TestGrammarPins compares). licenses/<File>.txt is the upstream
// LICENSE at Rev.
type grammar struct {
	File, Dir, URL, Rev string
}

// The internal/ts/grammars package converts these same parser.c files for the
// pure-Go runtime (ts2go) and ports the kotlin, sql, tsx and swift scanners
// from the C scanners: derived works, same license, same copyright.
var grammars = []grammar{
	{"core", "", "https://github.com/tree-sitter/tree-sitter", "cdd46888603e06d6474a96c0024907f68242c45f"}, // tag v0.22.5, the C runtime smacker vendors
	{"c", "c", "https://github.com/tree-sitter/tree-sitter-c", "deca017a554045b4c203e7ddff39ae64ff05e071"},
	{"cpp", "cpp", "https://github.com/tree-sitter/tree-sitter-cpp", "0b4aa47f07d958a49260aadc87e8474b03897c23"},
	{"go", "golang", "https://github.com/tree-sitter/tree-sitter-go", "6204b7308a32e991a8daed2e9895a90be55a510a"},
	{"groovy", "groovy", "https://github.com/murtaza64/tree-sitter-groovy", "171b3b7412bc93284fc0dad0a7a7bbbbd1b51289"},
	{"java", "java", "https://github.com/tree-sitter/tree-sitter-java", "953abfc8bb3eb2f578e1f461edba4a9885f974b8"},
	{"kotlin", "kotlin", "https://github.com/fwcd/tree-sitter-kotlin", "e1a2d5ad1f61f5740677183cd4125bb071cd2f30"},
	{"markdown", "markdown", "https://github.com/tree-sitter-grammars/tree-sitter-markdown", "62516e8c78380e3b51d5b55727995d2c511436d8"},
	{"proto", "protobuf", "https://github.com/mitchellh/tree-sitter-proto", "42d82fa18f8afe59b5fc0b16c207ee4f84cb185f"},
	{"python", "python", "https://github.com/tree-sitter/tree-sitter-python", "346fa42dc2990d2a2736cc60891369d0d3d8e65c"},
	{"sql", "sql", "https://github.com/DerekStride/tree-sitter-sql", "c67ecbd37d8d12f22e4cc7138afd14bc20253e10"},
	{"swift", "swift", "https://github.com/alex-pinkus/tree-sitter-swift", "57c1c6d6ffa1c44b330182d41717e6fe37430704"},
	{"typescript", "typescript", "https://github.com/tree-sitter/tree-sitter-typescript", "9f804be960f289a0acc59d8564acc16857b0b088"}, // the tsx grammar
}

// pkg is one shipped dependency.
type pkg struct {
	Name, Version, License, Note string
	Texts                        []text
}

type text struct{ File, Body string }

func main() {
	out := flag.String("o", "", "write the notices here")
	check := flag.String("check", "", "fail when this file differs from the generated notices")
	flag.Parse()
	mods, problems, err := goModules()
	if err != nil {
		fail(err)
	}
	gr, gprob := grammarPkgs()
	problems = append(problems, gprob...)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "notices:", p)
		}
		fail(fmt.Errorf("%d dependency license problem(s)", len(problems)))
	}
	counts := map[string]int{}
	for _, p := range append(append([]pkg{}, mods...), gr...) {
		counts[p.License]++
	}
	fmt.Fprintf(os.Stderr, "notices: %d modules, %d grammar sources, all allowed: %v\n", len(mods), len(gr), counts)
	body := render(mods, gr)
	if *out != "" {
		if err := os.WriteFile(*out, body, 0o644); err != nil {
			fail(err)
		}
	}
	if *check != "" {
		have, err := os.ReadFile(*check)
		if err != nil {
			fail(err)
		}
		if !bytes.Equal(have, body) {
			fail(fmt.Errorf("%s is stale; run `make notices` and commit it", *check))
		}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "notices:", err)
	os.Exit(1)
}

// goModules is every module the release build links, but the main module.
// Modules only the pure-Go build (CGO_ENABLED=0, what `go install` builds
// without a C compiler) links are listed too, marked, so both builds' notices
// are complete.
func goModules() ([]pkg, []string, error) {
	rel, err := listModules("1")
	if err != nil {
		return nil, nil, err
	}
	pure, err := listModules("0")
	if err != nil {
		return nil, nil, err
	}
	var pkgs []pkg
	var problems []string
	add := func(lines []string, note string, skip map[string]bool) {
		for _, line := range lines {
			if skip[line] {
				continue
			}
			f := strings.Split(line, "\t")
			if len(f) != 3 || f[2] == "" {
				problems = append(problems, "go module with no source directory: "+line)
				continue
			}
			p, prob := goModule(f[0], f[1], f[2])
			p.Note = note
			pkgs, problems = append(pkgs, p), append(problems, prob...)
		}
	}
	inRel := map[string]bool{}
	for _, l := range rel {
		inRel[l] = true
	}
	add(rel, "", nil)
	add(pure, "linked only by the pure-Go (CGO_ENABLED=0) build", inRel)
	return pkgs, problems, nil
}

// listModules lists path, version and directory of each non-main module the
// build links, with the release tag.
func listModules(cgo string) ([]string, error) {
	cmd := exec.Command("go", "list", "-deps", "-tags", "release", "-f",
		"{{with .Module}}{{if not .Main}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{end}}{{end}}", ".")
	cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED="+cgo)
	cmd.Stderr = os.Stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line != "" && !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	return out, nil
}

// goModule reads one module's license and NOTICE files. Every license file
// must classify as allowed or be reviewed; the module's license is its first
// classified one.
func goModule(path, version, dir string) (pkg, []string) {
	p := pkg{Name: path, Version: version}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return p, []string{fmt.Sprintf("%s: %v", path, err)}
	}
	var problems []string
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !(licenseFile.MatchString(name) || noticeFile.MatchString(name)) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s %s: %v", path, name, err))
			continue
		}
		p.Texts = append(p.Texts, text{File: name, Body: string(b)})
		if noticeFile.MatchString(name) {
			continue
		}
		lic := classify(string(b))
		if lic == "" {
			if r, ok := reviewed[path+" "+name]; ok {
				if allowed[r] && p.License == "" {
					p.License = r
				}
				continue
			}
			problems = append(problems, fmt.Sprintf("%s %s: license text not recognised; read it and add it to reviewed", path, name))
			continue
		}
		if !allowed[lic] {
			problems = append(problems, fmt.Sprintf("%s %s: %s is not on the allow-list", path, name, lic))
		}
		if p.License == "" {
			p.License = lic
		}
	}
	if p.License == "" && len(problems) == 0 {
		problems = append(problems, path+": no license file")
	}
	return p, problems
}

// grammarPkgs reads the vendored upstream license of each grammar.
func grammarPkgs() ([]pkg, []string) {
	var pkgs []pkg
	var problems []string
	for _, g := range grammars {
		b, err := licenseFS.ReadFile("licenses/" + g.File + ".txt")
		if err != nil {
			problems = append(problems, fmt.Sprintf("grammar %s: %v", g.File, err))
			continue
		}
		lic := classify(string(b))
		if !allowed[lic] {
			problems = append(problems, fmt.Sprintf("grammar %s: license %q is not on the allow-list", g.File, lic))
		}
		pkgs = append(pkgs, pkg{
			Name: g.URL, Version: g.Rev[:12], License: lic,
			Texts: []text{{File: "LICENSE (upstream, at commit " + g.Rev + ")", Body: string(b)}},
		})
	}
	return pkgs, problems
}

func render(mods, grams []pkg) []byte {
	sort.Slice(mods, func(i, j int) bool { return mods[i].Name < mods[j].Name })
	sort.Slice(grams, func(i, j int) bool { return grams[i].Name < grams[j].Name })
	var b bytes.Buffer
	b.WriteString("THIRD-PARTY NOTICES\n\n")
	b.WriteString("GENERATED FILE, DO NOT EDIT. Generator: go run ./tools/notices -o THIRD_PARTY_NOTICES\n")
	b.WriteString("(make notices). `make notices-check` fails when this file is stale.\n\n")
	b.WriteString("poly-lsp-mcp is MIT licensed (LICENSE). A release binary also contains the\n")
	b.WriteString("software below, each under its own license, reproduced here as those licenses\n")
	b.WriteString("require.\n\n")
	b.WriteString("Tree-sitter grammars: smacker/go-tree-sitter compiles each grammar's parser.c\n")
	b.WriteString("(and scanner.c) from the upstream repository listed under \"Tree-sitter\n")
	b.WriteString("grammars\", at the commit shown, together with the tree-sitter C runtime. The\n")
	b.WriteString("pure-Go runtime's grammar blobs (internal/ts/grammars) are converted from those\n")
	b.WriteString("same parser.c files, and its kotlin, sql, tsx and swift scanners are ports of\n")
	b.WriteString("the C scanners: derived works under the same licenses and copyrights.\n\n")
	b.WriteString("Go modules\n")
	for _, p := range mods {
		fmt.Fprintf(&b, "- %s %s - %s%s\n", p.Name, p.Version, p.License, note(p))
	}
	b.WriteString("\nTree-sitter grammars\n")
	for _, p := range grams {
		fmt.Fprintf(&b, "- %s @ %s - %s\n", p.Name, p.Version, p.License)
	}
	section := func(title string, ps []pkg) {
		for _, p := range ps {
			fmt.Fprintf(&b, "\n--------------------------------------------------------------------------------\n%s: %s %s (%s)%s\n", title, p.Name, p.Version, p.License, note(p))
			for _, t := range p.Texts {
				fmt.Fprintf(&b, "\n[%s]\n\n%s\n", t.File, strings.TrimRight(t.Body, "\n"))
			}
		}
	}
	section("Go module", mods)
	section("Tree-sitter grammar", grams)
	return b.Bytes()
}

func note(p pkg) string {
	if p.Note == "" {
		return ""
	}
	return " (" + p.Note + ")"
}
