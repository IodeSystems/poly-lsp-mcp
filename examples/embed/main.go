// Example: embed poly-lsp-mcp's MCP server in your own binary.
//
// Drop the surrounding hook points where they make sense for you —
// custom config loading, additional tools, in-process child LSPs,
// whatever your host needs. The library packages (config / mcp /
// multiplex / symbols / server) are the same ones the standalone
// `poly-lsp-mcp` binary uses; we're just wiring them together
// in-process instead of via `go run`.
//
// Build + run:
//
//	go run ./examples/embed --root /path/to/workspace
//
// Speaks MCP over stdio just like `poly-lsp-mcp mcp`.
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("embed ")

	rootFlag := flag.String("root", ".", "workspace root to index")
	configFlag := flag.String("config", "poly-lsp-mcp.yaml", "language registry config file")
	flag.Parse()

	root, err := filepath.Abs(*rootFlag)
	if err != nil {
		log.Fatalf("root: %v", err)
	}

	// Step 1: load config. LoadOrDefault returns the defaults when
	// the file is missing — handy for ad-hoc workspaces. `used` is
	// true when the file was actually read.
	cfg, used, err := config.LoadOrDefault(*configFlag)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if used {
		log.Printf("loaded %s", *configFlag)
	} else {
		log.Print("using defaults (no config file)")
	}

	// Step 2: optionally auto-detect schema files in the workspace.
	// Append to cfg.Schemas so the Tier-3 resolver picks them up.
	if cfg.AutoSchemas {
		for _, s := range config.DetectSchemas(root, cfg.Schemas) {
			cfg.Schemas = append(cfg.Schemas, s)
		}
	}

	// Step 3: build the language registry. This is the
	// extension-to-language map symbols.Build consults during the
	// workspace walk.
	reg, err := cfg.Build()
	if err != nil {
		log.Fatalf("config build: %v", err)
	}

	// Step 4: wire the MCP server.
	srv := mcp.New(reg, root, cfg.Bindings, cfg.Schemas)

	// Step 5 (optional but recommended): attach a multiplex manager
	// so node_edit / node_delete / node_refactor responses carry
	// LSP diagnostics. Nil-manager mode works too — you just lose
	// the diagnostic round-trip.
	srv.SetManager(multiplex.NewManager(reg))

	// Step 6 (optional): cache parses to disk so the next process
	// startup is fast. Skip this in transient embeds.
	srv.SetCachePath(filepath.Join(root, ".poly-lsp-mcp", "cache.gob"))

	// Step 7 (optional): tweak background-goroutine policy. Defaults
	// are usually right; override when embedding for a constrained
	// environment.
	//   srv.SetProactiveOpen(false)   // skip didOpen-on-init walk
	//   srv.SetGitPrewarm(false)      // skip ancestor-branch cache prewarm
	//   srv.SetDiagnosticWait(2 * time.Second)

	// Step 8: serve. Blocks until the client closes the stream or
	// sends shutdown+exit. Errors from Serve indicate stream
	// closure without a clean shutdown (ErrExitWithoutShutdown) or
	// I/O failure.
	if err := srv.Serve(os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}
