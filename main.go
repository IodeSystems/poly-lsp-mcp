package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
	"github.com/iodesystems/poly-lsp-mcp/server"
)

// errUsage: the command already printed its usage; exit 2 with no message.
var errUsage = errors.New("usage")

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("poly-lsp-mcp ")
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// Before ANY subcommand: this may replace the process, and it must happen
	// before a single byte of JSON-RPC has been read or written. A no-op
	// unless LSP_REBUILD_PATH names this module's checkout (see selfupdate.go).
	selfUpdate()

	err := newRoot().Execute()
	if err == nil {
		return
	}
	if errors.Is(err, errUsage) {
		os.Exit(2)
	}
	if errors.Is(err, errDoctor) {
		os.Exit(1) // the findings are already printed
	}
	// A selector error is the answer to what was asked, so it prints as
	// prose, without the program-name prefix.
	var se *mcp.SelectorError
	if errors.As(err, &se) {
		fmt.Fprintln(os.Stderr, se.Error())
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "poly-lsp-mcp:", err)
	os.Exit(1)
}

// loader carries the flags every command shares; each RunE calls load.
type loader struct {
	configPath string
}

// load reads the config file (falling back to defaults) and builds the
// registry.
//
// announce says whether to log which config was resolved. For a server it is
// a startup fact, printed once for the life of the process and worth having in
// the log. For the one-shot query CLI it is the most-printed line the tool has
// — every invocation in every repo without a config file — and it says nothing
// the caller can act on. Volume is what makes it noise: a reader trained to
// skip the line that is always there is being trained to skip stderr, which is
// also where the selector errors land.
func (l *loader) load(announce bool) (*config.Config, *config.Registry, error) {
	cfg, used, err := config.LoadOrDefault(l.configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("config: %w", err)
	}
	reg, err := cfg.Build()
	if err != nil {
		return nil, nil, fmt.Errorf("config: %w", err)
	}
	if !announce {
		return cfg, reg, nil
	}
	if used {
		log.Printf("config: loaded %s", l.configPath)
	} else {
		log.Printf("config: using defaults (no %s)", l.configPath)
	}
	return cfg, reg, nil
}

// newRoot builds the command tree. With no subcommand the binary is the LSP
// server on stdio, because that is how editors start it.
func newRoot() *cobra.Command {
	l := &loader{}
	root := &cobra.Command{
		Use:   "poly-lsp-mcp",
		Short: "Multi-language LSP + MCP server over one symbol index",
		Long: `poly-lsp-mcp is a multi-language LSP server and MCP server over one
tree-sitter symbol index, with child LSPs (gopls, typescript-language-server,
pylsp, clangd) for resolved references and diagnostics.

With no subcommand it runs the LSP server on stdin/stdout (what an editor
starts). Run a subcommand for anything else.`,
		Example: `  poly-lsp-mcp query --root . 'file[path=main.go] > *'
  poly-lsp-mcp query --root . ':explain #runQuery'
  poly-lsp-mcp mcp --root .
  poly-lsp-mcp index --root .
  poly-lsp-mcp status
  poly-lsp-mcp doctor`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLSP(l)
		},
	}
	// Flags parse before the command name is checked, so `poly-lsp-mcp
	// explain --root .` would report "unknown flag: --root". The unknown
	// word is the real mistake; name it.
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if cmd == root && cmd.Flags().NArg() > 0 {
			return fmt.Errorf("unknown command %q for %q; run '%s --help'", cmd.Flags().Arg(0), root.Name(), root.Name())
		}
		return err
	})
	root.PersistentFlags().StringVar(&l.configPath, "config", "poly-lsp-mcp.yaml", "language registry config file")
	root.AddCommand(lspCmd(l), mcpCmd(l), queryCmd(l), indexCmd(l), statusCmd(), usageCmd(), doctorCmd(l), daemonCmd(l))
	return root
}

func lspCmd(l *loader) *cobra.Command {
	return &cobra.Command{
		Use:   "lsp",
		Short: "Run the LSP server on stdin/stdout (the default with no subcommand)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLSP(l)
		},
	}
}

func runLSP(l *loader) error {
	// A person who typed the bare command sees a server waiting on stdin,
	// which looks like a hang. Say what it is.
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		log.Print("LSP server on stdin/stdout, waiting for an editor. For commands: poly-lsp-mcp --help")
	}
	cfg, reg, err := l.load(true)
	if err != nil {
		return err
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
	return srv.Serve(os.Stdin, os.Stdout)
}

// multiFlag collects a repeatable string flag (e.g. --allow a --allow b).
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Type() string   { return "dir" }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// cachePathFor returns where this root's parse cache lives.
//
// It used to be <root>/.poly-lsp-mcp/cache.gob — inside the workspace being
// indexed. That left an untracked directory in every repo poly-lsp was ever
// pointed at: this project gitignores its own, but nobody else's repo does,
// so `git status` came back dirty in trees where nothing had been edited.
// Measured on dun: 30 of 34 "dirty" session worktrees were dirty for this
// reason alone, and dun had to add `.poly-lsp-mcp/` to an artifact-exclusion
// list to tell real work from our leftovers.
//
// The cache is derived data keyed by a path, which is what a user cache
// directory is for. Roots are hashed rather than slugged so the name is
// bounded and unambiguous; the leading path element is kept readable so the
// directory can be browsed. Falls back to the old in-tree location only when
// there is no user cache dir at all.
func cachePathFor(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(root, ".poly-lsp-mcp", "cache.gob")
	}
	sum := sha256.Sum256([]byte(abs))
	name := fmt.Sprintf("%s-%x", filepath.Base(abs), sum[:8])
	return filepath.Join(base, "poly-lsp-mcp", name, "cache.gob")
}
