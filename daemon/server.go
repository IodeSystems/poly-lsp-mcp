package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/iodesystems/gwag/v2/gw/gat"
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

// credListener drops connections whose peer uid is not the daemon's own,
// or cannot be read, before net/http ever sees them.
type credListener struct {
	net.Listener
	selfUID int
	// bound is the socket file as bound: a takeover that fails after the
	// rename is detected by the main path no longer being this file.
	bound os.FileInfo
}

func (l *credListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, _, cerr := peerCred(c)
		if cerr != nil {
			// Unknown peer: refuse, do not trust the socket mode alone.
			log.Printf("daemon: rejected connection: %v", cerr)
			_ = c.Close()
			continue
		}
		if uid != l.selfUID {
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
		OK bool `json:"ok"`
		// Version is Build, kept under its old name for older clients.
		Version  string   `json:"version"`
		Build    string   `json:"build"`
		Protocol int      `json:"protocol"`
		PID      int      `json:"pid"`
		Roots    []string `json:"roots"`
		// Warm is false while a takeover daemon is still bringing up the
		// roots it will take over.
		Warm bool `json:"warm"`
		// RootStats: each root's clients and resident estimate;
		// IdleRootsCap the bound on the client-less ones (bytes, < 0 none).
		RootStats    []RootStat `json:"rootStats,omitempty"`
		IdleRootsCap int64      `json:"idleRootsCap"`
		Clients      int64      `json:"clients"`
	}
}

type openIn struct {
	// Session holds this root warm for the client's lifetime (ref-counted;
	// released when its /session/watch drops). Empty = a one-shot open with
	// no hold, eligible for idle eviction immediately.
	Session string `header:"X-Poly-Session"`
	Body    struct {
		Root string `json:"root"`
		OpenOptions
	}
}
type openOut struct {
	Body struct {
		Root  string `json:"root"`
		Names int    `json:"names"`
		// Error is the failed bring-up's post-mortem and remedy (what a
		// tool call on the root answers), "" when the root is up.
		Error string `json:"error,omitempty"`
	}
}

type toolsIn struct {
	Root     string `query:"root"`
	ReadOnly bool   `header:"X-Poly-Read-Only"`
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
	// ReadOnly / Validate are the per-connection policy. They can only TIGHTEN
	// the daemon baseline (a read-only daemon stays read-only regardless).
	ReadOnly bool `header:"X-Poly-Read-Only"`
	Validate bool `header:"X-Poly-Validate"`
	// ValidateWait is the client's --validate-wait (a Go duration); empty
	// keeps the daemon's.
	ValidateWait string `header:"X-Poly-Validate-Wait"`
	Body         struct {
		Root      string          `json:"root"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
}

type accessIn struct {
	Body struct {
		Root      string          `json:"root"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
}
type accessOut struct{ Body FileAccess }

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

type queryIn struct {
	Body struct {
		Root     string   `json:"root"`
		Selector string   `json:"selector"`
		Limit    int      `json:"limit"`
		Offset   int      `json:"offset"`
		Budget   string   `json:"budget,omitempty"`
		Include  []string `json:"include,omitempty"`
		JSON     bool     `json:"json"`
	}
}
type queryOut struct {
	Body struct {
		Output string `json:"output"`
		Root   string `json:"root"`
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

type helloIn struct {
	Body struct {
		Build    string `json:"build"`
		Protocol int    `json:"protocol"`
		Exe      string `json:"exe,omitempty"`
		VCSTime  string `json:"vcsTime,omitempty"`
	}
}
type helloOut struct{ Body Hello }

// Hello is the daemon's side of the handshake.
type Hello struct {
	Build    string `json:"build"`
	Protocol int    `json:"protocol"`
	PID      int    `json:"pid"`
	VCSTime  string `json:"vcsTime,omitempty"`
}

type takeoverIn struct {
	Body struct {
		Socket string `json:"socket"`
	}
}
type takeoverOut struct {
	Body struct {
		OK bool `json:"ok"`
	}
}

type upgradeIn struct{ Body UpgradeRequest }
type upgradeOut struct{ Body UpgradeAnswer }

// buildHandler is the handler of a daemon that does not upgrade (tests).
func buildHandler(allow *AllowList, reg *Registry) (http.Handler, error) {
	h := &host{allow: allow, reg: reg, up: newUpgrader(func(UpgradeRequest) error {
		return errors.New("this daemon does not upgrade")
	})}
	return h.handler()
}

// handler wires the gat/huma surface — the same house stack as raglit's
// buildGatHandler (REST + GraphQL + gRPC + OpenAPI off one set of ops),
// served here over the unix socket instead of TCP.
func (h *host) handler() (http.Handler, error) {
	allow, reg := h.allow, h.reg
	router := chi.NewRouter()
	api := humachi.New(router, huma.DefaultConfig("poly-lsp", strconv.Itoa(Protocol)))
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
			out.Body.Version = BuildID()
			out.Body.Build = BuildID()
			out.Body.Protocol = Protocol
			out.Body.PID = os.Getpid()
			out.Body.Roots = reg.Roots()
			out.Body.Warm = h.takeoverFrom == 0 || h.warm.Load()
			out.Body.RootStats, out.Body.IdleRootsCap = reg.RootStats()
			out.Body.Clients = h.clients.Load()
			return out, nil
		})

	// The version handshake: a client says its build, the daemon its own,
	// and a client of another build decides (buildDecision) whether to ask
	// for /upgrade.
	gat.Register(api, g, op("hello", http.MethodPost, "/hello", "Exchange build identities."),
		func(_ context.Context, in *helloIn) (*helloOut, error) {
			if in.Body.Build != "" && in.Body.Build != BuildID() {
				log.Printf("daemon: a client of build %s (protocol %d) connected", in.Body.Build, in.Body.Protocol)
			}
			out := &helloOut{}
			out.Body = Hello{Build: BuildID(), Protocol: Protocol, PID: os.Getpid(), VCSTime: VCSTime()}
			return out, nil
		})

	gat.Register(api, g, op("upgrade", http.MethodPost, "/upgrade", "Ask the daemon to move to another build of its binary."),
		func(_ context.Context, in *upgradeIn) (*upgradeOut, error) {
			out := &upgradeOut{}
			out.Body = h.up.request(in.Body)
			return out, nil
		})

	// Green's side of a blue/green swap; only blue (its takeover-from pid,
	// by peer credentials) may call it. See takeover.go.
	gat.Register(api, g, op("takeover", http.MethodPost, "/takeover", "Take over the main socket from the daemon being replaced."),
		func(ctx context.Context, in *takeoverIn) (*takeoverOut, error) {
			if err := h.takeOver(ctx, in.Body.Socket); err != nil {
				return nil, huma.Error409Conflict(err.Error())
			}
			out := &takeoverOut{}
			out.Body.OK = true
			return out, nil
		})

	gat.Register(api, g, op("open", http.MethodPost, "/open", "Open (warm) a workspace root."),
		func(_ context.Context, in *openIn) (*openOut, error) {
			if err := checkOpenOptions(in.Body.OpenOptions); err != nil {
				return nil, huma.Error400BadRequest(err.Error())
			}
			canonical, err := admit(allow, in.Body.Root)
			if err != nil {
				return nil, err
			}
			var srv *mcp.Server
			var berr error
			if in.Session != "" {
				srv, berr = reg.Open(in.Session, canonical, in.Body.OpenOptions)
			} else {
				srv, berr = reg.get(canonical, "", &in.Body.OpenOptions, true)
			}
			out := &openOut{}
			out.Body.Root = canonical
			if berr != nil {
				out.Body.Error = srv.IndexFailure(berr)
				return out, nil
			}
			out.Body.Names = srv.IndexedNames()
			return out, nil
		})

	gat.Register(api, g, op("tools", http.MethodGet, "/tools", "List the MCP tool catalog for a root."),
		func(_ context.Context, in *toolsIn) (*toolsOut, error) {
			// No trust gate: the catalog depends on neither the root nor
			// its index, and a proxy asks for it while its /open may not
			// have reached the daemon yet — a root that open adds to the
			// allow list would be refused here first.
			if in.Root == "" {
				return nil, huma.Error400BadRequest("root is required")
			}
			out := &toolsOut{}
			tools := reg.Tools()
			// A read-only connection sees a read-only catalog even if the
			// shared server is read-write for other clients.
			if reg.readOnly || in.ReadOnly {
				filtered := tools[:0:0]
				for _, t := range tools {
					if !mcp.IsMutatingTool(t.Name) {
						filtered = append(filtered, t)
					}
				}
				tools = filtered
			}
			out.Body.Tools = tools
			return out, nil
		})

	gat.Register(api, g, op("call", http.MethodPost, "/call", "Invoke an MCP tool against a root."),
		func(_ context.Context, in *callIn) (*callOut, error) {
			canonical, err := gate(allow, in.Body.Root)
			if err != nil {
				return nil, err
			}
			// The first call on a root waits for its bring-up, bounded by
			// the index budget; a failed one answers with its post-mortem,
			// as the stdio server does.
			srv, berr := reg.Get(canonical)
			if berr != nil {
				out := &callOut{}
				out.Body.Content = []mcp.Content{{Type: "text", Text: srv.IndexFailure(berr)}}
				out.Body.IsError = true
				return out, nil
			}
			// Per-connection policy TIGHTENS the daemon baseline (never loosens).
			pol := mcp.CallOptions{
				ReadOnly: reg.readOnly || in.ReadOnly,
				Validate: reg.validate || in.Validate,
			}
			if in.ValidateWait != "" {
				d, err := time.ParseDuration(in.ValidateWait)
				if err != nil || d <= 0 {
					return nil, huma.Error400BadRequest(fmt.Sprintf("X-Poly-Validate-Wait %q: want a positive duration such as 2s", in.ValidateWait))
				}
				pol.ValidateWait = d
			}
			content, isError, cerr := srv.CallTool(in.Session, in.Body.Name, in.Body.Arguments, pol)
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

	// The file a node_read/node_edit call would touch, for a proxy that
	// runs its client's file-access hook before forwarding the call (see
	// mcp.Server.FileAccess). The hook runs client-side, with the client's
	// env: the daemon serves many clients and runs none of their hooks.
	gat.Register(api, g, op("access", http.MethodPost, "/access", "Name the file a read/edit tool call would touch, for the caller's file-access hook."),
		func(_ context.Context, in *accessIn) (*accessOut, error) {
			canonical, err := gate(allow, in.Body.Root)
			if err != nil {
				return nil, err
			}
			out := &accessOut{}
			// A failed bring-up touches no file: the call answers why.
			if srv, berr := reg.Get(canonical); berr == nil {
				out.Body.Op, out.Body.Path, out.Body.OK = srv.FileAccess(in.Body.Name, in.Body.Arguments)
			}
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
			select { // blocks until the client disconnects, or this daemon hands over
			case <-ctx.Done():
			case <-h.draining:
			}
			if rolled := reg.RollbackSession(in.Session); len(rolled) > 0 {
				log.Printf("daemon: session %s dropped — rolled back %v", in.Session, rolled)
			}
			// Drop the session's root refs; a root left with no holders
			// stops its child LSPs after --root-lsp-grace and joins the
			// LRU of client-less roots.
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

	// Query is the non-MCP door to the same engine node_query serves, and
	// the daemon answer is LSP-resolved: every hosted root gets its own
	// multiplex.Manager at build time (Registry.build), so edges settle
	// through the child LSPs without the caller asking. The warm server is
	// resolved through the same trust gate as /open; the answer is exactly
	// what QueryText/QueryJSON writes, so a daemon answer and a standalone
	// answer for the same workspace are the same text.
	gat.Register(api, g, op("query", http.MethodPost, "/query", "Evaluate a selector against a root and return the text (or one JSON document) answer."),
		func(_ context.Context, in *queryIn) (*queryOut, error) {
			srv, canonical, err := resolveServer(allow, reg, in.Body.Root)
			if err != nil {
				return nil, err
			}
			var buf bytes.Buffer
			var qerr error
			if in.Body.JSON {
				qerr = srv.QueryJSON(in.Body.Selector, in.Body.Limit, in.Body.Offset, in.Body.Budget, &buf, in.Body.Include...)
			} else {
				qerr = srv.QueryText(in.Body.Selector, in.Body.Limit, in.Body.Offset, in.Body.Budget, &buf, in.Body.Include...)
			}
			if qerr != nil {
				// A selector error is the answer to what was asked: a
				// 200 carrying the prose, like the CLI prints it to stdout.
				// Only a genuine tool failure gets an HTTP error.
				var se *mcp.SelectorError
				if errors.As(qerr, &se) {
					out := &queryOut{}
					out.Body.Output = se.Error() + "\n"
					out.Body.Root = canonical
					return out, nil
				}
				return nil, huma.Error500InternalServerError("query", qerr)
			}
			out := &queryOut{}
			out.Body.Output = buf.String()
			out.Body.Root = canonical
			return out, nil
		})

	if err := gat.RegisterHuma(api, g, ""); err != nil {
		return nil, err
	}
	router.Get("/session/events", eventsHandler(reg, h.draining))
	return router, nil
}

// gate is the trust gate (declared-prefix check): the canonical root, or a
// 400 for no root and a 403 for a root outside every allowed prefix.
func gate(allow *AllowList, root string) (string, error) {
	if root == "" {
		return "", huma.Error400BadRequest("root is required")
	}
	canonical, err := allow.Resolve(root)
	if err != nil {
		return "", huma.Error403Forbidden(err.Error())
	}
	return canonical, nil
}

// admit is gate for /open: a root outside every allowed prefix is added
// to the allow list (AllowList.Admit says why that is no loss) and logged.
func admit(allow *AllowList, root string) (string, error) {
	if root == "" {
		return "", huma.Error400BadRequest("root is required")
	}
	canonical, added, err := allow.Admit(root)
	if err != nil {
		return "", huma.Error400BadRequest(err.Error())
	}
	if added {
		log.Printf("daemon: allow: added %s (opened outside every allowed prefix)", canonical)
	}
	return canonical, nil
}

// resolveServer runs the trust gate and returns the warm server for the
// canonical root, building it on first use. A build failure is a 500.
func resolveServer(allow *AllowList, reg *Registry, root string) (*mcp.Server, string, error) {
	canonical, err := gate(allow, root)
	if err != nil {
		return nil, "", err
	}
	srv, err := reg.Get(canonical)
	if err != nil {
		return nil, "", huma.Error500InternalServerError("open root", err)
	}
	return srv, canonical, nil
}

// eventsHandler is GET /session/events: /session/watch that also streams.
// The client holds it open for its lifetime; it carries, as NDJSON, every
// notification of the roots the session holds (and the daemon-wide ones),
// and when it drops the session's batches are rolled back and its holds
// released, as /session/watch does. A plain handler beside the huma
// operations, because it streams.
func eventsHandler(reg *Registry, draining <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		sess := req.Header.Get(sessionHeader)
		if sess == "" {
			http.Error(w, "X-Poly-Session header is required", http.StatusBadRequest)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		ch, cancel := reg.hub.subscribe(sess, reg.heldBy(sess))
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		enc := json.NewEncoder(w)
		ctx := req.Context()
		for loop := true; loop; {
			select {
			case ev := <-ch:
				if err := enc.Encode(ev); err != nil {
					loop = false
					break
				}
				fl.Flush()
			case <-ctx.Done():
				loop = false
			case <-draining:
				loop = false
			}
		}
		// A newer stream of the same session (a reconnect) owns its holds
		// and batches now; only the session's current stream releases them.
		if !cancel() {
			return
		}
		if rolled := reg.RollbackSession(sess); len(rolled) > 0 {
			log.Printf("daemon: session %s dropped — rolled back %v", sess, rolled)
		}
		reg.Release(sess)
	}
}
