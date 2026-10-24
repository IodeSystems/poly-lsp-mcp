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

// defaultIndexMaxFiles is the hard limit on indexable files. dun's own tree
// is 455 files; with 48 untracked worktrees in it, 22,012, and a 52 s
// initialize. A build that fails at the limit reports where the files are,
// which is what a client needs to choose its excludes.
const defaultIndexMaxFiles = 10000

// indexOpts are the flags that shape the index, shared by mcp, query and
// index.
type indexOpts struct {
	excludes multiFlag
	maxFiles int
}

func indexFlags(f *pflag.FlagSet) *indexOpts {
	o := &indexOpts{}
	f.Var(&o.excludes, "exclude", "drop this directory (relative to --root) from the index and every walk, as though .gitignore listed it; repeatable. For a client choosing what to index without editing the repository")
	f.IntVar(&o.maxFiles, "index-max-files", defaultIndexMaxFiles, "fail the index build above this many indexable files, with a report of the directories that hold them; 0 = no limit")
	return o
}

func (o *indexOpts) apply(srv *mcp.Server) error {
	for _, d := range o.excludes {
		if git.CleanExclude(d) == "" {
			return fmt.Errorf("--exclude %q: want a directory inside the root, relative to it", d)
		}
	}
	srv.SetExcludes(o.excludes)
	srv.SetIndexLimit(o.maxFiles)
	return nil
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
