package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/daemon"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
	"github.com/iodesystems/poly-lsp-mcp/server"
)

// multiFlag collects a repeatable string flag (e.g. --allow a --allow b).
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("poly-lsp-mcp ")
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// Before ANY subcommand: this may replace the process, and it must happen
	// before a single byte of JSON-RPC has been read or written. A no-op
	// unless the binary was source-stamped by `make build|install`.
	selfUpdate()

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
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		runDaemon()
		return
	}
	runLSP()
}

func runLSP() {
	configPath := flag.String("config", "poly-lsp-mcp.yaml", "language registry config file")
	flag.Parse()

	cfg, reg := loadConfigOrDie(*configPath, true)
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
	// ON by default. A safety net that must be asked for is a safety net
	// nobody has: dun spawns this server with no flags, so every agent edit
	// ran unvalidated until now — and node_edit's `return` op will happily
	// leave a signature the body no longer satisfies.
	//
	// Safe to default because it degrades rather than blocks: with no child
	// LSP for the language it is off entirely, and when diagnostics are
	// unavailable or time out the edit APPLIES and is flagged (fail-open,
	// Skipped: no-lsp / lsp-timeout). It cannot make an unvalidatable
	// workspace uneditable.
	//
	// --no-validate is the escape hatch, for when validation itself is the
	// problem — the known one being a never-analyzed file with pre-existing
	// errors, which has no baseline and can false-revert its first edit.
	validate := flag.Bool("validate", true, "revert-on-new-diagnostics: an edit that introduces a new error is rolled back instead of landing (needs a child LSP; degrades to apply-and-flag without one)")
	noValidate := flag.Bool("no-validate", false, "turn OFF revert-on-new-diagnostics (see --validate); for when validation itself misfires, e.g. a never-analyzed file with pre-existing errors")
	daemonMode := flag.Bool("daemon", false, "proxy tool calls to the shared per-user poly-lsp daemon (auto-starting it) instead of building the index in-process; one warm index + child-LSP fleet is shared across every client")
	readCharBudget := flag.Int("read-char-budget", 0, "implicit char cap for a node_read with no lineLimit (0 = default 2048). Raising it trades round-trips for payload size: at the default, reading a ~1100-line file takes many truncated reads, and each re-read costs a turn plus the tokens to compose it")
	flag.Parse()

	if *readCharBudget > 0 {
		mcp.SetReadCharBudget(*readCharBudget)
	}

	if *noValidate {
		*validate = false
	}

	root, err := filepath.Abs(*rootPath)
	if err != nil {
		log.Fatalf("root: %v", err)
	}

	// Daemon proxy mode: keep the stdio MCP surface, forward tool calls
	// to the shared daemon over its unix socket. The workspace index,
	// child LSPs, and parse cache live once in the daemon.
	if *daemonMode {
		client, err := daemon.EnsureRunning(nil)
		if err != nil {
			log.Fatalf("mcp: daemon: %v", err)
		}
		// Carry this client's policy to the daemon (enforced per-connection at
		// its boundary). --legacy-tools has no daemon surface — the daemon
		// serves the modern surface only.
		client.SetPolicy(*readOnly, *validate)
		if *legacyTools {
			log.Print("mcp: --legacy-tools is ignored in --daemon mode (the daemon serves the modern surface)")
		}
		log.Printf("mcp: proxying root %s to shared daemon (readOnly=%v validate=%v)", root, *readOnly, *validate)
		if err := daemon.RunProxy(client, root, os.Stdin, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg, reg := loadConfigOrDie(*configPath, true)
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
	srv.SetValidateEdits(*validate)
	// Log the SURPRISING state. Validation is the default now, so announcing
	// it every run is noise; running WITHOUT it is the fact worth recording,
	// because an edit that breaks the build will then simply land.
	if !*validate {
		log.Printf("mcp: --no-validate — edits that introduce a new error will LAND, not revert")
	}
	srv.SetCachePath(cachePathFor(root))
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
	budget := flag.String("budget", "", "query budget: Nms wall-clock (bare = ms) or Nops deterministic work units (default 10000ms). Raise when a query reports it stopped early.")
	verbose := flag.Bool("verbose", false, "log which config was resolved and other startup detail")
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

	cfg, reg := loadConfigOrDie(*configPath, *verbose)
	root, err := filepath.Abs(*rootPath)
	if err != nil {
		log.Fatalf("root: %v", err)
	}

	// No SetManager / no Serve: a query is read-only and touches
	// neither child LSPs nor the persisted index.
	srv := mcp.New(reg, root, cfg.Bindings, cfg.Schemas)
	// It DOES want the parse cache, though. Without it every invocation
	// re-parses the workspace from scratch — the dominant cost of a one-shot
	// query, and pure waste when the MCP server has already parsed the same
	// bytes. Load-only: see LoadCache.
	// Quiet FIRST: LoadCache logs what it loaded, and bring-up commentary on
	// stderr is what --verbose gates.
	srv.SetQuiet(!*verbose)
	srv.SetCachePath(cachePathFor(root))
	srv.LoadCache()
	// A successful query prints its answer and nothing else. The bring-up
	// commentary (index size, binding passes) is a server's startup record;
	// here it is a preamble the caller re-reads on every invocation.
	if err := srv.QueryText(selector, *limit, *offset, *budget, os.Stdout); err != nil {
		// A selector error is the answer to what was asked, so it prints as
		// prose. Only a genuine tool failure gets the log furniture.
		var se *mcp.SelectorError
		if errors.As(err, &se) {
			fmt.Fprintln(os.Stderr, se.Error())
			os.Exit(1)
		}
		log.Fatalf("query: %v", err)
	}
}

// runDaemon runs (or controls) the shared per-user daemon: one process
// hosting many workspace roots over a unix socket, gated by peer
// credentials and declared root prefixes. --stop / --restart control an
// already-running daemon and exit.
func runDaemon() {
	// Capture the daemon flags before parsing so --restart can replay
	// them into the relaunched process.
	daemonFlags := append([]string{}, os.Args[1:]...)

	configPath := flag.String("config", "poly-lsp-mcp.yaml", "language registry config file")
	socket := flag.String("socket", "", "unix socket path (default $XDG_RUNTIME_DIR/poly-lsp/daemon.sock)")
	readOnly := flag.Bool("read-only", false, "host every root read-only (mutating tools hidden)")
	// Same default as the stdio server (see runMCP): on, degrading to
	// apply-and-flag where diagnostics are unavailable.
	validate := flag.Bool("validate", true, "revert-on-new-diagnostics for every hosted root's edits")
	noValidate := flag.Bool("no-validate", false, "turn OFF revert-on-new-diagnostics for hosted roots")
	stop := flag.Bool("stop", false, "stop the running daemon and exit")
	restart := flag.Bool("restart", false, "restart the running daemon (replaying its flags) and exit")
	var allow multiFlag
	flag.Var(&allow, "allow", "directory prefix a client may address roots under (repeatable; default $HOME)")
	flag.Parse()

	if *stop {
		if err := daemon.Stop(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *noValidate {
		*validate = false
	}
	if *restart {
		if err := daemon.Restart(stripBoolFlags(daemonFlags, "stop", "restart")); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg, reg := loadConfigOrDie(*configPath, true)

	prefixes := []string(allow)
	if len(prefixes) == 0 {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			prefixes = []string{home}
		}
	}
	al := daemon.NewAllowList(prefixes)
	if len(al.Prefixes()) == 0 {
		log.Fatal("daemon: no usable --allow prefix (and no home dir); refusing to start with an empty allow-list")
	}

	dreg := daemon.NewRegistry(cfg, reg, *readOnly, *validate)
	if err := daemon.Serve(daemon.Config{Socket: *socket, Allow: al, Reg: dreg}); err != nil {
		log.Fatal(err)
	}
}

// stripBoolFlags removes the named boolean flags (any of -f, --f, -f=v,
// --f=v spellings) from an argument list — used to replay a daemon's
// flags on --restart without the control flag itself.
func stripBoolFlags(args []string, names ...string) []string {
	drop := map[string]bool{}
	for _, n := range names {
		drop[n] = true
	}
	out := make([]string, 0, len(args))
	for _, a := range args {
		name := strings.TrimLeft(a, "-")
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if drop[name] {
			continue
		}
		out = append(out, a)
	}
	return out
}

// loadConfigOrDie loads the config file (falling back to defaults) and
// builds the registry; both subcommands need it identically.
// announce says whether to log which config was resolved. For a server it is
// a startup fact, printed once for the life of the process and worth having in
// the log. For the one-shot query CLI it is the most-printed line the tool has
// — every invocation in every repo without a config file — and it says nothing
// the caller can act on. Volume is what makes it noise: a reader trained to
// skip the line that is always there is being trained to skip stderr, which is
// also where the selector errors land.
func loadConfigOrDie(path string, announce bool) (*config.Config, *config.Registry) {
	cfg, used, err := config.LoadOrDefault(path)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	reg, err := cfg.Build()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if !announce {
		return cfg, reg
	}
	if used {
		log.Printf("config: loaded %s", path)
	} else {
		log.Printf("config: using defaults (no %s)", path)
	}
	return cfg, reg
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
