// Package bindings applies Tier-2 declared bindings from config to a
// symbol index. A binding asserts that several physical sites — across
// files, possibly across languages — represent the same conceptual
// entity. The resolver turns those declarations into Site entries
// stamped as ConfidenceDeclared so that workspace/symbol and
// textDocument/references surface them.
//
// v0.2.1 implements the `symbol` site form only: "every position where
// identifier X appears in file F" is mapped under the binding's Name.
// jsonpath and regex forms are reserved on the config side; they land
// in their own slices.
package bindings

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/iodesystems/tslsmcp/internal/config"
	"github.com/iodesystems/tslsmcp/internal/symbols"
)

// Resolver applies bindings declared in config to a symbol index. The
// workspace root is the directory all file references in bindings are
// taken relative to.
type Resolver struct {
	root string
}

func NewResolver(root string) *Resolver {
	return &Resolver{root: root}
}

// Apply walks every binding and inserts the resolved sites into idx.
// Missing references (the symbol isn't found in the file) are logged
// and counted but never fatal — partial bindings still improve the
// index. The returned int is the number of declared sites inserted.
func (r *Resolver) Apply(idx *symbols.Index, bindings []config.Binding) (int, error) {
	var (
		inserted int
		errs     []error
	)
	for _, b := range bindings {
		if b.Name == "" {
			errs = append(errs, errors.New("binding has empty name"))
			continue
		}
		if len(b.Sites) == 0 {
			errs = append(errs, fmt.Errorf("binding %q has no sites", b.Name))
			continue
		}
		for _, site := range b.Sites {
			n, err := r.applySite(idx, b.Name, site)
			inserted += n
			if err != nil {
				log.Printf("bindings: %s: %v", b.Name, err)
			}
		}
	}
	if len(errs) == 0 {
		return inserted, nil
	}
	return inserted, errors.Join(errs...)
}

func (r *Resolver) applySite(idx *symbols.Index, bindingName string, site config.BindingSite) (int, error) {
	if site.File == "" {
		return 0, fmt.Errorf("site has no file")
	}
	// Symbol form: find every lexical site for site.Symbol in the named
	// file and re-register them under the binding's name as declared.
	if site.Symbol != "" {
		abs := r.absFile(site.File)
		var inserted int
		for _, s := range idx.Lookup(site.Symbol) {
			if s.File != abs {
				continue
			}
			idx.InsertDeclared(bindingName, s.File, s.Language, s.Line, s.Col)
			inserted++
		}
		if inserted == 0 {
			return 0, fmt.Errorf("symbol %q not found in %s", site.Symbol, site.File)
		}
		return inserted, nil
	}
	if site.JSONPath != "" {
		return r.applyJSONPathSite(idx, bindingName, site)
	}
	if len(site.Regex) > 0 {
		return r.applyRegexSite(idx, bindingName, site)
	}
	return 0, fmt.Errorf("site for %s has no symbol / jsonpath / regex set", site.File)
}

// applyJSONPathSite reads the file, evaluates the jsonpath, and registers
// every matched node as a declared site under bindingName. Only YAML and
// JSON files are supported — the language tag on inserted sites comes
// from the file extension. Files of other types yield an error (we're
// not going to fish identifier-like strings out of arbitrary content).
func (r *Resolver) applyJSONPathSite(idx *symbols.Index, bindingName string, site config.BindingSite) (int, error) {
	abs := r.absFile(site.File)
	lang := jsonpathLanguage(abs)
	if lang == "" {
		return 0, fmt.Errorf("jsonpath only supports .yaml/.yml/.json files; got %s", site.File)
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", abs, err)
	}
	path, err := parsePath(site.JSONPath)
	if err != nil {
		return 0, fmt.Errorf("parse jsonpath %q: %w", site.JSONPath, err)
	}
	positions, err := evalYAMLJSON(content, path)
	if err != nil {
		return 0, fmt.Errorf("eval jsonpath in %s: %w", site.File, err)
	}
	if len(positions) == 0 {
		return 0, fmt.Errorf("jsonpath %q matched nothing in %s", site.JSONPath, site.File)
	}
	for _, p := range positions {
		idx.InsertDeclared(bindingName, abs, lang, p.Line, p.Col)
	}
	return len(positions), nil
}

// applyRegexSite reads the file, evaluates every pattern in the site,
// and registers each match whose captured text equals bindingName.
// Matches whose text differs are logged and skipped — that's the
// aliasing-safety contract for the regex form in v0.2.x.
//
// Files larger than maxFileSize are skipped with an error so a stray
// regex against a generated bundle doesn't tie up the loop.
func (r *Resolver) applyRegexSite(idx *symbols.Index, bindingName string, site config.BindingSite) (int, error) {
	abs := r.absFile(site.File)
	info, err := os.Stat(abs)
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", abs, err)
	}
	if info.Size() > maxRegexFileSize {
		return 0, fmt.Errorf("%s exceeds regex size cap (%d bytes)", abs, maxRegexFileSize)
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", abs, err)
	}
	hits, err := evalRegex(content, site.Regex)
	if err != nil {
		return 0, fmt.Errorf("regex in %s: %w", site.File, err)
	}
	if len(hits) == 0 {
		return 0, fmt.Errorf("regex patterns matched nothing in %s", site.File)
	}
	lang := regexLanguage(abs)
	inserted := 0
	for _, h := range hits {
		if h.Text != bindingName {
			log.Printf("bindings: regex match %q != binding %q at %s:%d:%d (aliasing not supported in v0.2.x; skipping)",
				h.Text, bindingName, site.File, h.Line, h.Col)
			continue
		}
		idx.InsertDeclared(bindingName, abs, lang, h.Line, h.Col)
		inserted++
	}
	if inserted == 0 {
		return 0, fmt.Errorf("no regex match in %s matched binding name %q", site.File, bindingName)
	}
	return inserted, nil
}

// maxRegexFileSize mirrors symbols.Build's per-file cap so the regex
// form doesn't blow up on the same files the lexical pass skips.
const maxRegexFileSize = 1 << 20

// regexLanguage tags the inserted sites with the file's language so
// downstream consumers (workspace/symbol formatting, rename grouping)
// still get a useful label. Unknown extensions fall back to "" — the
// site still indexes; just doesn't carry a language hint.
func regexLanguage(path string) string {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) {
	case "go":
		return "go"
	case "ts", "tsx", "js", "jsx", "mjs", "cjs":
		return "typescript"
	case "py", "pyi":
		return "python"
	case "yaml", "yml":
		return "yaml"
	case "json":
		return "json"
	case "md", "markdown":
		return "markdown"
	case "sql", "psql":
		return "sql"
	}
	return ""
}

// jsonpathLanguage returns "yaml" or "json" for files we can evaluate
// jsonpath against, or "" for everything else. Hard-coded extension
// list rather than going through the config.Registry to keep this
// package decoupled.
func jsonpathLanguage(path string) string {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) {
	case "yaml", "yml":
		return "yaml"
	case "json":
		return "json"
	}
	return ""
}

// absFile resolves a binding's relative File against the workspace root.
// Absolute paths in the config are passed through unchanged (lets the
// user point at generated files outside the tree if they really want).
func (r *Resolver) absFile(file string) string {
	if filepath.IsAbs(file) {
		return file
	}
	return filepath.Join(r.root, file)
}
