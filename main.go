package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
	"github.com/iodesystems/poly-lsp-mcp/server"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("poly-lsp-mcp ")
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// Subcommand dispatch. Default (no subcommand) runs the LSP server.
	// `poly-lsp-mcp mcp [flags]` runs the MCP server.
	// `poly-lsp-mcp query [flags] <selector>` runs one selector and prints it.
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		runMCP()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "query" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		runQuery()
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
	legacyTools := flag.Bool("legacy-tools", false, "expose the legacy 9-tool MCP surface instead of the 3-tool surface")
	readOnly := flag.Bool("read-only", false, "hide every mutating tool (node_edit/node_delete/node_refactor/node_rename_file); navigation + reading only")
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
	srv.SetLegacyTools(*legacyTools)
	srv.SetReadOnly(*readOnly)
	if *readOnly {
		log.Printf("mcp: READ-ONLY — mutating tools are not registered")
	}
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

// runQuery compiles and evaluates one selector against the workspace
// and prints the matches. It is the human-facing door to the same
// engine node_query serves: no JSON-RPC, no child LSPs, no warm index
// — buildTree walks the workspace and parses only the files the
// selector actually descends into.
func runQuery() {
	configPath := flag.String("config", "poly-lsp-mcp.yaml", "language registry config file")
	rootPath := flag.String("root", ".", "workspace root directory to query")
	limit := flag.Int("limit", 0, "max matches to print (0 = all)")
	offset := flag.Int("offset", 0, "skip this many matches")
	budget := flag.String("budget", "", "query budget: Nms wall-clock (bare = ms) or Nops deterministic work units (default 200000ops). Raise when a query reports it stopped early.")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: poly-lsp-mcp query [flags] <selector>\n\n")
		fmt.Fprintf(os.Stderr, "Evaluate a node selector and print the matches, grouped by file.\n")
		fmt.Fprintf(os.Stderr, "Pass '?' as the selector for the full selector grammar.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  poly-lsp-mcp query ':root > *'\n")
		fmt.Fprintf(os.Stderr, "  poly-lsp-mcp query --root ../other 'file.go func'\n")
		fmt.Fprintf(os.Stderr, "  poly-lsp-mcp query '?'\n")
	}
	flag.Parse()

	// The selector is one argument, but an unquoted one arrives as
	// several — rejoin rather than silently querying only argv[0].
	selector := strings.Join(flag.Args(), " ")
	if strings.TrimSpace(selector) == "" {
		flag.Usage()
		os.Exit(2)
	}

	cfg, reg := loadConfigOrDie(*configPath)
	root, err := filepath.Abs(*rootPath)
	if err != nil {
		log.Fatalf("root: %v", err)
	}

	// No SetManager / no Serve: a query is read-only and touches
	// neither child LSPs nor the persisted index.
	srv := mcp.New(reg, root, cfg.Bindings, cfg.Schemas)
	if err := srv.QueryText(selector, *limit, *offset, *budget, os.Stdout); err != nil {
		log.Fatalf("query: %v", err)
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
	if used {
		log.Printf("config: loaded %s", path)
	} else {
		log.Printf("config: using defaults (no %s)", path)
	}
	return cfg, reg
}
