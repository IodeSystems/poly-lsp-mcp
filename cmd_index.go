package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/iodesystems/poly-lsp-mcp/mcp"
)

// indexCmd builds the index once and prints what it cost, by directory —
// the same report an MCP client gets in the initialize result, for a
// person deciding what to add to .gitignore before starting a server.
func indexCmd(l *loader) *cobra.Command {
	var (
		rootPath string
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Build the index once and report its cost by directory",
		Long: `Build the index once and report its cost by directory: files, bytes,
time, and the directories that dominate it. A directory with its own .git
(a worktree, a clone, a submodule) is flagged: it is another repository
indexed as part of this one. Uses the parse cache, read-only, as query does.`,
		Example: `  poly-lsp-mcp index --root .
  poly-lsp-mcp index --root . --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, reg, err := l.load(false)
			if err != nil {
				return err
			}
			root, err := filepath.Abs(rootPath)
			if err != nil {
				return fmt.Errorf("root: %w", err)
			}
			srv := mcp.New(reg, root, cfg.Bindings, cfg.Schemas)
			srv.SetQuiet(true)
			srv.SetCachePath(cachePathFor(root))
			srv.LoadCache()
			if err := srv.BuildIndex(); err != nil {
				return err
			}
			rep := srv.IndexCost()
			if asJSON {
				b, _ := json.MarshalIndent(rep, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			fmt.Print(rep.Text())
			return nil
		},
	}
	cmd.Flags().StringVar(&rootPath, "root", ".", "workspace root directory to index")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON (the initialize result's index field)")
	return cmd
}
