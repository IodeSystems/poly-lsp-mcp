package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iodesystems/poly-lsp-mcp/daemon"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// queryCmd compiles and evaluates one selector against the workspace
// and prints the matches. It is the human-facing door to the same
// engine node_query serves: no JSON-RPC, no warm index — buildTree walks
// the workspace and parses only the files the selector actually descends
// into. Child LSPs only with --resolve: then ambiguous ::in/::out edges
// are resolved through them and each edge carries its conf.
func queryCmd(l *loader) *cobra.Command {
	var (
		rootPath string
		limit    int
		offset   int
		budget   string
		verbose  bool
		asJSON   bool
		resolve  bool
	)
	cmd := &cobra.Command{
		Use:   "query [flags] <selector>",
		Short: "Evaluate a node selector and print the matches, grouped by file",
		Long: `Evaluate a node selector and print the matches, grouped by file.
Pass '?' as the selector for the full selector grammar. Prefix the selector
with ':explain ' to see what each step cost instead of the matches.`,
		Example: `  poly-lsp-mcp query ':root > *'
  poly-lsp-mcp query --root ../other 'file.go func'
  poly-lsp-mcp query ':explain #runQuery'
  poly-lsp-mcp query '?'`,
	}
	f := cmd.Flags()
	f.StringVar(&rootPath, "root", ".", "workspace root directory to query")
	f.IntVar(&limit, "limit", 0, "max matches to print (0 = all)")
	f.IntVar(&offset, "offset", 0, "skip this many matches")
	f.StringVar(&budget, "budget", "", "query budget: Nms wall-clock (bare = ms) or Nops deterministic work units (default 10000ms). Raise when a query reports it stopped early.")
	f.BoolVar(&verbose, "verbose", false, "log which config was resolved and other startup detail")
	f.BoolVar(&asJSON, "json", false, "print ONE JSON document (matches with node, file, type, @, and for an edge from/to and conf; total, offset, stop-early flags) instead of the grouped text tree")
	idx := indexFlags(f)
	f.BoolVar(&resolve, "resolve", false, "resolve ::in/::out edges through child LSPs (spawns them for this run, e.g. gopls) and label each edge conf: lsp|lexical|unsettled, as node_query does; slower to start")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		// The selector is one argument, but an unquoted one arrives as
		// several — rejoin rather than silently querying only argv[0].
		selector := strings.Join(args, " ")
		if strings.TrimSpace(selector) == "" {
			_ = cmd.Usage()
			return errUsage
		}

		cfg, reg, err := l.load(verbose)
		if err != nil {
			return err
		}
		root, err := filepath.Abs(rootPath)
		if err != nil {
			return fmt.Errorf("root: %w", err)
		}

		// A shared per-user daemon, if it is already running for this workspace,
		// answers the query so this process never rebuilds the workspace index —
		// the ~15s-per-query cost the one-shot path pays. PROBE ONLY, never
		// auto-start: a one-shot query must not leave a daemon behind. The state
		// file names the socket; a healthy probe is the authority, and a live
		// daemon is LSP-resolved by construction (each hosted root carries its
		// own child-LSP manager), so this path needs no --resolve. On no daemon,
		// a dead one, or any transport error, fall back to the standalone path
		// below — the answer is the same matches, built in-process.
		if out, ok := queryViaDaemon(verbose, root, asJSON, selector, limit, offset, budget); ok {
			fmt.Print(out)
			return nil
		}

		// No Serve: a query is read-only and never writes the persisted index.
		// Child LSPs only under --resolve (below).
		srv := mcp.New(reg, root, cfg.Bindings, cfg.Schemas)
		if err := idx.apply(srv); err != nil {
			return err
		}
		// It DOES want the parse cache, though. Without it every invocation
		// re-parses the workspace from scratch — the dominant cost of a one-shot
		// query, and pure waste when the MCP server has already parsed the same
		// bytes. Load-only: see LoadCache.
		// Quiet FIRST: LoadCache logs what it loaded, and bring-up commentary on
		// stderr is what --verbose gates.
		srv.SetQuiet(!verbose)
		srv.SetCachePath(cachePathFor(root))
		srv.LoadCache()
		// --resolve: start the child LSPs so evaluate settles ambiguous edges
		// the way node_query does. Shutdown on every return path below, so no
		// child process outlives the run.
		if resolve {
			if err := srv.InitResolve(multiplex.NewManager(reg)); err != nil {
				srv.Shutdown()
				return fmt.Errorf("query: %w", err)
			}
			defer srv.Shutdown()
		}
		// A successful query prints its answer and nothing else. The bring-up
		// commentary (index size, binding passes) is a server's startup record;
		// here it is a preamble the caller re-reads on every invocation.
		query := srv.QueryText
		if asJSON {
			query = srv.QueryJSON
		}
		if err := query(selector, limit, offset, budget, os.Stdout); err != nil {
			// main prints a *mcp.SelectorError as plain prose.
			return err
		}
		return nil
	}
	return cmd
}

// queryViaDaemon answers the query through an ALREADY-RUNNING shared daemon,
// if there is one. It probes the state file (never auto-starts) and a health
// check over the socket; ok is false on no state file, a dead daemon, or any
// transport/refusal error, in which case the caller runs standalone.
func queryViaDaemon(verbose bool, root string, asJSON bool, selector string, limit, offset int, budget string) (string, bool) {
	st, ok := daemon.ReadState()
	if !ok || !daemon.PidAlive(st.PID) {
		return "", false
	}
	c := daemon.NewClient(st.Socket)
	if !c.Healthy() {
		return "", false
	}
	out, err := c.Query(daemon.QueryParams{
		Root:     root,
		Selector: selector,
		Limit:    limit,
		Offset:   offset,
		Budget:   budget,
		JSON:     asJSON,
	})
	if err != nil {
		if verbose {
			log.Printf("query: daemon %s did not answer (%v); running standalone", st.Socket, err)
		}
		return "", false
	}
	if verbose {
		log.Printf("query: answered through daemon %s (pid %d)", st.Socket, st.PID)
	}
	return out, true
}
