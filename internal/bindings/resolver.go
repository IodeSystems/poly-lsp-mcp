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
	"path/filepath"

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
		return 0, fmt.Errorf("site form 'jsonpath' not yet implemented")
	}
	if site.Regex != "" {
		return 0, fmt.Errorf("site form 'regex' not yet implemented")
	}
	return 0, fmt.Errorf("site for %s has no symbol / jsonpath / regex set", site.File)
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
