package bindings

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/iodesystems/tslsmcp/internal/config"
	"github.com/iodesystems/tslsmcp/internal/symbols"
)

// SchemaEntity is one named declaration extracted from a schema file
// (proto message, openapi schema, jsonschema definition, …). Position
// is in the schema file itself; the resolver also pulls in workspace
// occurrences of the same name when applying.
type SchemaEntity struct {
	Name string
	Line int
	Col  int
}

// ApplySchemas reads each schema file declared in config and promotes
// its named entities to declared bindings:
//
//  1. The entity's declaration line in the schema becomes a declared
//     site under the entity's name.
//  2. Every existing index hit (lexical or already-declared) for the
//     same name in any language is also declared.
//
// Effect: a single entry in `schemas:` auto-binds every workspace
// position where the schema's names appear, with no per-language sites
// needed in tslsmcp.yaml. The dedup in Index.InsertDeclared keeps the
// store clean even when user bindings and schema bindings overlap.
func (r *Resolver) ApplySchemas(idx *symbols.Index, schemas []config.Schema) int {
	inserted := 0
	for _, sch := range schemas {
		n, err := r.applySchema(idx, sch)
		inserted += n
		if err != nil {
			log.Printf("schemas: %s (%s): %v", sch.File, sch.Dialect, err)
		}
	}
	return inserted
}

func (r *Resolver) applySchema(idx *symbols.Index, sch config.Schema) (int, error) {
	if sch.File == "" {
		return 0, fmt.Errorf("schema has no file")
	}
	if sch.Dialect == "" {
		return 0, fmt.Errorf("schema for %s has no dialect", sch.File)
	}
	abs := r.absFile(sch.File)
	content, err := os.ReadFile(abs)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", abs, err)
	}

	var entities []SchemaEntity
	switch strings.ToLower(sch.Dialect) {
	case "proto", "protobuf":
		entities = parseProto(content)
	case "openapi":
		return 0, fmt.Errorf("openapi dialect not yet implemented")
	case "jsonschema":
		return 0, fmt.Errorf("jsonschema dialect not yet implemented")
	default:
		return 0, fmt.Errorf("unknown dialect %q (supported: proto)", sch.Dialect)
	}

	if len(entities) == 0 {
		return 0, fmt.Errorf("no entities extracted from %s", sch.File)
	}

	schemaLang := dialectLanguage(sch.Dialect)
	inserted := 0
	for _, e := range entities {
		// Snapshot workspace hits BEFORE we mutate the index, otherwise
		// the schema declaration we just inserted would feed back into
		// Lookup and we'd loop over our own writes.
		workspaceHits := idx.Lookup(e.Name)

		idx.InsertDeclared(e.Name, abs, schemaLang, e.Line, e.Col)
		inserted++

		for _, s := range workspaceHits {
			if s.File == abs && s.Line == e.Line && s.Col == e.Col {
				continue
			}
			idx.InsertDeclared(e.Name, s.File, s.Language, s.Line, s.Col)
			inserted++
		}
	}
	return inserted, nil
}

// dialectLanguage returns the language tag to stamp on schema
// declaration sites. Workspace hits keep their own (lexical) language.
func dialectLanguage(d string) string {
	switch strings.ToLower(d) {
	case "proto", "protobuf":
		return "proto"
	case "openapi":
		return "openapi"
	case "jsonschema":
		return "jsonschema"
	}
	return ""
}
