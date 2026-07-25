package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/iodesystems/gwag/gw/gat"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// peerKey is the context key under which ConnContext stashes the
// connecting peer's credentials, so per-connection policy (read-only /
// validate, a later slice) can read them at the handler boundary.
type peerKey struct{}

type peerInfo struct {
	UID int
	PID int
}

// Config parameterizes the daemon server.
type Config struct {
	Socket string     // listen path; "" = SocketPath()
	Allow  *AllowList // declared root prefixes a client may address
	Reg    *Registry  // the per-root server host
}

// Serve runs the daemon: it listens on the unix socket (peer-cred gated),
// serves the gat/huma handler, records the state file, and blocks until
// SIGINT/SIGTERM, then tears down hosted servers gracefully. It is the
// process `poly-lsp-mcp daemon` becomes.
func Serve(cfg Config) error {
	socket := cfg.Socket
	if socket == "" {
		socket = SocketPath()
	}

	ln, err := listenUnix(socket)
	if err != nil {
		return err
	}

	// Seed the shared cache before serving so the first root opens warm.
	cfg.Reg.LoadCache()

	handler, err := buildHandler(cfg.Allow, cfg.Reg)
	if err != nil {
		_ = ln.Close()
		return err
	}

	srv := &http.Server{
		Handler: handler,
		// Stash the accepted conn's peer creds for per-connection policy.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			uid, pid, err := peerCred(c)
			if err != nil {
				return ctx
			}
			return context.WithValue(ctx, peerKey{}, peerInfo{UID: uid, PID: pid})
		},
	}

	removeState, err := WriteState(socket)
	if err != nil {
		log.Printf("daemon: write state: %v (continuing)", err)
	}
	defer removeState()

	// Graceful shutdown: SIGTERM is what --stop/--restart send.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Print("daemon: shutting down")
		cfg.Reg.Close()
		cfg.Reg.SaveCache()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	log.Printf("daemon: listening on %s (pid %d, allow %v)", socket, os.Getpid(), cfg.Allow.Prefixes())
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// listenUnix creates the socket with tight permissions: dir 0700, socket
// 0600. A stale socket file whose owner is gone is unlinked and replaced
// (the raglit "drop a stale daemon.json" move, applied to the socket).
func listenUnix(socket string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return nil, fmt.Errorf("daemon: socket dir: %w", err)
	}
	// If something is already listening, refuse; if it's a stale file,
	// remove it and take over.
	if _, err := os.Stat(socket); err == nil {
		if c, derr := net.Dial("unix", socket); derr == nil {
			_ = c.Close()
			return nil, fmt.Errorf("daemon: socket %s is already served (another daemon is running)", socket)
		}
		if err := os.Remove(socket); err != nil {
			return nil, fmt.Errorf("daemon: remove stale socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("daemon: listen %s: %w", socket, err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("daemon: chmod socket: %w", err)
	}
	// Reject any connecting uid but our own at accept — redundant with
	// 0600 in the normal case, which is the point: it still holds if the
	// mode bits are ever wrong.
	return &credListener{Listener: ln, selfUID: os.Getuid()}, nil
}

// credListener drops connections whose peer uid is not the daemon's own
// before net/http ever sees them.
type credListener struct {
	net.Listener
	selfUID int
}

func (l *credListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, _, cerr := peerCred(c)
		if cerr == nil && uid != l.selfUID {
			log.Printf("daemon: rejected connection from uid %d", uid)
			_ = c.Close()
			continue
		}
		return c, nil
	}
}

// --- HTTP surface (gat/huma) ---

type healthOut struct {
	Body struct {
		OK      bool     `json:"ok"`
		Version string   `json:"version"`
		PID     int      `json:"pid"`
		Roots   []string `json:"roots"`
	}
}

type openIn struct {
	// Session holds this root warm for the client's lifetime (ref-counted;
	// released when its /session/watch drops). Empty = a one-shot open with
	// no hold, eligible for idle eviction immediately.
	Session string `header:"X-Poly-Session"`
	Body    struct {
		Root string `json:"root"`
	}
}
type openOut struct {
	Body struct {
		Root  string `json:"root"`
		Names int    `json:"names"`
	}
}

type toolsIn struct {
	Root string `query:"root"`
}
type toolsOut struct {
	Body struct {
		Tools []mcp.ToolDescriptor `json:"tools"`
	}
}

type callIn struct {
	// Session isolates this client's edit batch from other clients on the
	// same root (see mcp session.go). The proxy mints it once and sends it on
	// every call; empty maps to the implicit local session.
	Session string `header:"X-Poly-Session"`
	Body    struct {
		Root      string          `json:"root"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
}

type watchIn struct {
	Session string `header:"X-Poly-Session"`
}
type watchOut struct {
	Body struct {
		Released bool `json:"released"`
	}
}
type callOut struct {
	Body struct {
		Content []mcp.Content `json:"content"`
		IsError bool          `json:"isError"`
	}
}

// FileSymbol is the daemon's wire contract for one structural atom — a
// json-tagged projection of symbols.Symbol, decoupled from the internal
// struct so the OpenAPI schema a non-MCP consumer (raglit) generates
// against stays stable. These are fragment atoms: a dotted path, a
// class, the declaration/name ranges, and the doc-comment + body-start
// spans that give a fragment its title and boundary — no LLM, no cgo,
// no fork-per-file (the caller sends bytes over the socket).
type FileSymbol struct {
	Sym   string `json:"sym"`
	Class string `json:"class"`
	Alias string `json:"alias,omitempty"`

	DeclStartLine int `json:"decl_start_line"`
	DeclStartCol  int `json:"decl_start_col"`
	DeclEndLine   int `json:"decl_end_line"`
	DeclEndCol    int `json:"decl_end_col"`

	NameStartLine int `json:"name_start_line"`
	NameStartCol  int `json:"name_start_col"`
	NameEndLine   int `json:"name_end_line"`
	NameEndCol    int `json:"name_end_col"`

	CommentStartLine int `json:"comment_start_line,omitempty"`
	CommentStartCol  int `json:"comment_start_col,omitempty"`
	CommentEndLine   int `json:"comment_end_line,omitempty"`
	CommentEndCol    int `json:"comment_end_col,omitempty"`

	BodyStartLine int `json:"body_start_line,omitempty"`
}

func fileSymbolFrom(s symbols.Symbol) FileSymbol {
	return FileSymbol{
		Sym: s.Sym, Class: s.Class, Alias: s.Alias,
		DeclStartLine: s.DeclStartLine, DeclStartCol: s.DeclStartCol,
		DeclEndLine: s.DeclEndLine, DeclEndCol: s.DeclEndCol,
		NameStartLine: s.NameStartLine, NameStartCol: s.NameStartCol,
		NameEndLine: s.NameEndLine, NameEndCol: s.NameEndCol,
		CommentStartLine: s.CommentStartLine, CommentStartCol: s.CommentStartCol,
		CommentEndLine: s.CommentEndLine, CommentEndCol: s.CommentEndCol,
		BodyStartLine: s.BodyStartLine,
	}
}

type fileSymbolsIn struct {
	Body struct {
		Content  string `json:"content"`            // the file text to fragment
		Language string `json:"language,omitempty"` // grammar name; derived from Path if empty
		Path     string `json:"path,omitempty"`     // only for language detection + echo
	}
}
type fileSymbolsOut struct {
	Body struct {
		Language string       `json:"language"`
		Symbols  []FileSymbol `json:"symbols"`
	}
}

// buildHandler wires the gat/huma surface — the same house stack as
// raglit's buildGatHandler (REST + GraphQL + gRPC + OpenAPI off one set
// of ops), served here over the unix socket instead of TCP.
func buildHandler(allow *AllowList, reg *Registry) (http.Handler, error) {
	router := chi.NewRouter()
	api := humachi.New(router, huma.DefaultConfig("poly-lsp", Version))
	g, err := gat.New()
	if err != nil {
		return nil, err
	}
	op := func(id, method, path, summary string) huma.Operation {
		return huma.Operation{OperationID: id, Method: method, Path: path, Summary: summary}
	}

	gat.Register(api, g, op("health", http.MethodGet, "/health", "Liveness probe + open roots."),
		func(_ context.Context, _ *struct{}) (*healthOut, error) {
			out := &healthOut{}
			out.Body.OK = true
			out.Body.Version = Version
			out.Body.PID = os.Getpid()
			out.Body.Roots = reg.Roots()
			return out, nil
		})

	gat.Register(api, g, op("open", http.MethodPost, "/open", "Open (warm) a workspace root."),
		func(_ context.Context, in *openIn) (*openOut, error) {
			srv, canonical, err := resolveServer(allow, reg, in.Body.Root, in.Session)
			if err != nil {
				return nil, err
			}
			out := &openOut{}
			out.Body.Root = canonical
			out.Body.Names = srv.IndexedNames()
			return out, nil
		})

	gat.Register(api, g, op("tools", http.MethodGet, "/tools", "List the MCP tool catalog for a root."),
		func(_ context.Context, in *toolsIn) (*toolsOut, error) {
			srv, _, err := resolveServer(allow, reg, in.Root, "")
			if err != nil {
				return nil, err
			}
			out := &toolsOut{}
			out.Body.Tools = srv.Tools()
			return out, nil
		})

	gat.Register(api, g, op("call", http.MethodPost, "/call", "Invoke an MCP tool against a root."),
		func(_ context.Context, in *callIn) (*callOut, error) {
			srv, _, err := resolveServer(allow, reg, in.Body.Root, "")
			if err != nil {
				return nil, err
			}
			content, isError, cerr := srv.CallTool(in.Session, in.Body.Name, in.Body.Arguments)
			if cerr != nil {
				// Mirror stdio handleToolsCall: a handler error becomes an
				// isError text result, not an HTTP error.
				content = []mcp.Content{{Type: "text", Text: cerr.Error()}}
				isError = true
			}
			out := &callOut{}
			out.Body.Content = content
			out.Body.IsError = isError
			return out, nil
		})

	// A long-lived liveness channel: the client holds this request open for
	// its whole lifetime. When the client vanishes (crash, kill, network
	// drop), net/http cancels the request context, and we auto-roll-back that
	// session's open batch on every root — the disconnect safety the daemon
	// introduces (server and client no longer share a process lifetime, so a
	// dropped client could otherwise strand a broken staged intermediate on
	// disk). No trust gate: it names no root and only affects its OWN batches;
	// peer-cred + 0600 already bound the caller.
	gat.Register(api, g, op("sessionWatch", http.MethodGet, "/session/watch", "Hold open to bind a session's lifetime; on disconnect its staged batch is rolled back."),
		func(ctx context.Context, in *watchIn) (*watchOut, error) {
			if in.Session == "" {
				return nil, huma.Error400BadRequest("X-Poly-Session header is required")
			}
			<-ctx.Done() // blocks until the client disconnects
			if rolled := reg.RollbackSession(in.Session); len(rolled) > 0 {
				log.Printf("daemon: session %s dropped — rolled back %v", in.Session, rolled)
			}
			// Drop the session's root refs; a root left with no holders is
			// idle-evicted (its child LSPs freed) after idleTimeout.
			reg.Release(in.Session)
			out := &watchOut{}
			out.Body.Released = true
			return out, nil
		})

	gat.Register(api, g, op("fileSymbols", http.MethodPost, "/filesymbols", "Fragment file content into structural symbol atoms (no root/file access)."),
		func(_ context.Context, in *fileSymbolsIn) (*fileSymbolsOut, error) {
			lang := in.Body.Language
			if lang == "" {
				lang = reg.LanguageForPath(in.Body.Path)
			}
			if lang == "" {
				return nil, huma.Error400BadRequest("language is required (or a path with a known extension)")
			}
			syms, err := symbols.FileSymbols(lang, []byte(in.Body.Content))
			if err != nil {
				return nil, huma.Error422UnprocessableEntity("fragment", err)
			}
			out := &fileSymbolsOut{}
			out.Body.Language = lang
			out.Body.Symbols = make([]FileSymbol, len(syms))
			for i, s := range syms {
				out.Body.Symbols[i] = fileSymbolFrom(s)
			}
			return out, nil
		})

	if err := gat.RegisterHuma(api, g, ""); err != nil {
		return nil, err
	}
	return router, nil
}

// resolveServer runs the trust gate (declared-prefix check) and returns the
// warm server for the canonical root. When holder != "" the session takes a
// ref on the root (ref-counted eviction; the /open path). A root outside
// every allowed prefix is a 403; a build failure is a 500.
func resolveServer(allow *AllowList, reg *Registry, root, holder string) (*mcp.Server, string, error) {
	if root == "" {
		return nil, "", huma.Error400BadRequest("root is required")
	}
	canonical, err := allow.Resolve(root)
	if err != nil {
		return nil, "", huma.Error403Forbidden(err.Error())
	}
	if holder != "" {
		srv, err := reg.Acquire(holder, canonical)
		if err != nil {
			return nil, "", huma.Error500InternalServerError("open root", err)
		}
		return srv, canonical, nil
	}
	srv, err := reg.Get(canonical)
	if err != nil {
		return nil, "", huma.Error500InternalServerError("open root", err)
	}
	return srv, canonical, nil
}
