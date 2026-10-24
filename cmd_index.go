package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/iodesystems/poly-lsp-mcp/internal/git"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
)

// defaultIndexBudget bounds the index walk at startup, in node_query's
// budget grammar. Over it, the bring-up fails with the trace — where the
// files are — and how to get under budget or raise it. dun's own tree
// indexes in 0.2 s; with 48 untracked worktrees in it, 15.8 s cold.
const defaultIndexBudget = "30000ms"

// indexOpts are the flags that shape the index, shared by mcp, query and
// index.
type indexOpts struct {
	excludes multiFlag
	budget   string
}

func indexFlags(f *pflag.FlagSet) *indexOpts {
	o := &indexOpts{}
	f.Var(&o.excludes, "exclude", "drop this directory (relative to --root) from the index and every walk, as though .gitignore listed it; repeatable. For a client choosing what to index without editing the repository")
	f.StringVar(&o.budget, "index-budget", defaultIndexBudget, "startup budget for the index walk: Nms wall-clock (bare = ms) or Nops (one per file read). Over it the build fails with a trace of where the files are and how to get under budget or raise it. 0 = unbounded")
	return o
}

func (o *indexOpts) apply(srv *mcp.Server) error {
	for _, d := range o.excludes {
		if git.CleanExclude(d) == "" {
			return fmt.Errorf("--exclude %q: want a directory inside the root, relative to it", d)
		}
	}
	srv.SetExcludes(o.excludes)
	return srv.SetIndexBudget(o.budget)
}

// indexCmd builds the index once and prints what it cost, by directory —
// the same report an MCP client gets in the initialize result, for a
// person deciding what to add to .gitignore before starting a server.
func indexCmd(l *loader) *cobra.Command {
	var (
		rootPath string
		asJSON   bool
		idx      *indexOpts
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
			if err := idx.apply(srv); err != nil {
				return err
			}
			srv.SetQuiet(true)
			srv.SetCachePath(cachePathFor(root))
			srv.LoadCache()
			// Over the limit, the report is the post-mortem: print it, then
			// fail.
			buildErr := srv.BuildIndex()
			rep := srv.IndexCost()
			if asJSON {
				b, _ := json.MarshalIndent(rep, "", "  ")
				fmt.Println(string(b))
				return buildErr
			}
			fmt.Print(rep.Text())
			return buildErr
		},
	}
	idx = indexFlags(cmd.Flags())
	cmd.Flags().StringVar(&rootPath, "root", ".", "workspace root directory to index")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON (the initialize result's index field)")
	return cmd
}
