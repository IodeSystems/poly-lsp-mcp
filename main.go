package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
	"github.com/iodesystems/poly-lsp-mcp/server"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("poly-lsp-mcp ")
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// Subcommand dispatch. Default (no subcommand) runs the LSP server.
	// `poly-lsp-mcp mcp [flags]` runs the MCP server.
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		runMCP()
		return
	}
	runLSP()
}

func runLSP() {
	configPath := flag.String("config", "poly-lsp-mcp.yaml", "language registry config file")
	flag.Parse()

	cfg, reg := loadConfigOrDie(*configPath)
	for _, lang := range reg.Languages() {
		backend := "treesitter-only"
		if lang.LSP != nil {
			backend = lang.LSP.Cmd
		}
		log.Printf("  %-12s exts=%v backend=%s", lang.Name, lang.Extensions, backend)
	}

	mgr := multiplex.NewManager(reg)
	srv := server.New(reg, mgr, cfg.Bindings, cfg.Schemas)
	if err := srv.Serve(os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func runMCP() {
	configPath := flag.String("config", "poly-lsp-mcp.yaml", "language registry config file")
	rootPath := flag.String("root", ".", "workspace root directory the symbol index covers")
	flag.Parse()

	cfg, reg := loadConfigOrDie(*configPath)
	root, err := filepath.Abs(*rootPath)
	if err != nil {
		log.Fatalf("root: %v", err)
	}
	log.Printf("mcp: workspace root %s", root)

	if cfg.AutoSchemas {
		detected := config.DetectSchemas(root, cfg.Schemas)
		if len(detected) > 0 {
			log.Printf("auto-schemas: detected %d schema file(s):", len(detected))
			for _, s := range detected {
				log.Printf("  - %s (%s)", s.File, s.Dialect)
			}
			cfg.Schemas = append(cfg.Schemas, detected...)
		}
	}

	srv := mcp.New(reg, root, cfg.Bindings, cfg.Schemas)
	srv.SetCachePath(filepath.Join(root, ".poly-lsp-mcp", "cache.gob"))
	// Spawn child LSPs so node_edit / node_delete / node_refactor can
	// surface publishDiagnostics in their responses. Manager.Start runs
	// inside handleInitialize once we know which languages the
	// workspace actually contains, so we just hand the pre-built
	// Manager over here.
	srv.SetManager(multiplex.NewManager(reg))
	if err := srv.Serve(os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// loadConfigOrDie loads the config file (falling back to defaults) and
// builds the registry; both subcommands need it identically.
func loadConfigOrDie(path string) (*config.Config, *config.Registry) {
	cfg, used, err := config.LoadOrDefault(path)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	reg, err := cfg.Build()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	symbols.SetGoSchemaAnchorKeys(cfg.GoSchemaAnchors) // empty → default [OperationID]
	if used {
		log.Printf("config: loaded %s", path)
	} else {
		log.Printf("config: using defaults (no %s)", path)
	}
	return cfg, reg
}
