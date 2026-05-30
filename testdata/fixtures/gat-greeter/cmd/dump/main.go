// dump reads greeter.proto from the fixture root and writes a
// gat-rendered schema artifact to stdout. The format is selected by
// the -format flag:
//
//	-format graphql   (default) GraphQL SDL, including "@ref" in
//	                            triple-quoted descriptions
//	-format openapi              OpenAPI 3.x JSON, with "x-ref"
//	                            extension on operations
//
// Per gwag commit 09df07a, gat carries the @ref marker through every
// format it emits. Our universal scanner picks up both shapes — this
// dumper produces the artifacts the poly-lsp-mcp integration test then
// walks.
//
// Usage:
//
//	go run ./cmd/dump                       # GraphQL SDL
//	go run ./cmd/dump -format openapi       # OpenAPI JSON
//
// The grpc target is bogus by design — gat.New only dials lazily,
// and this program never dispatches a query.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/iodesystems/gwag/gw/gat"
	"github.com/iodesystems/gwag/gw/ir"
)

func main() {
	format := flag.String("format", "graphql", "schema format: graphql | openapi")
	flag.Parse()
	if err := run(os.Stdout, *format); err != nil {
		fmt.Fprintln(os.Stderr, "dump:", err)
		os.Exit(1)
	}
}

func run(out io.Writer, format string) error {
	root, err := fixtureRoot()
	if err != nil {
		return err
	}
	body, err := os.ReadFile(filepath.Join(root, "greeter.proto"))
	if err != nil {
		return fmt.Errorf("read greeter.proto: %w", err)
	}
	regs, err := gat.ProtoSource("greeter.proto", body, nil, "127.0.0.1:1")
	if err != nil {
		return fmt.Errorf("gat.ProtoSource: %w", err)
	}

	switch format {
	case "graphql", "":
		g, err := gat.New(regs...)
		if err != nil {
			return fmt.Errorf("gat.New: %w", err)
		}
		_, err = io.WriteString(out, ir.PrintSchemaSDL(g.Schema()))
		return err

	case "openapi":
		// ServiceRegistration.Service is the ingested IR; render the
		// OpenAPI projection directly. We don't need gat.New here
		// because we're not dispatching — just emitting the schema.
		if len(regs) == 0 || regs[0].Service == nil {
			return fmt.Errorf("ProtoSource returned no services")
		}
		doc, err := ir.RenderOpenAPI(regs[0].Service)
		if err != nil {
			return fmt.Errorf("ir.RenderOpenAPI: %w", err)
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(doc)

	default:
		return fmt.Errorf("unknown -format %q (try graphql | openapi)", format)
	}
}

// fixtureRoot returns the directory containing greeter.proto, derived
// from this source file's location so `go run` works no matter the
// caller's cwd.
func fixtureRoot() (string, error) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(self), "..", "..")), nil
}
