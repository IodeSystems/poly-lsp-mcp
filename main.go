package main

import (
	"flag"
	"log"
	"os"

	"github.com/iodesystems/tslsmcp/internal/config"
	"github.com/iodesystems/tslsmcp/internal/multiplex"
	"github.com/iodesystems/tslsmcp/internal/server"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("tslsmcp ")
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	configPath := flag.String("config", "tslsmcp.yaml", "language registry config file")
	flag.Parse()

	cfg, used, err := config.LoadOrDefault(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	reg, err := cfg.Build()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if used {
		log.Printf("config: loaded %s", *configPath)
	} else {
		log.Printf("config: using defaults (no %s)", *configPath)
	}
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
