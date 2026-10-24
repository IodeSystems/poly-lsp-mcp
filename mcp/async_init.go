package mcp

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/internal/ts"
	"github.com/iodesystems/poly-lsp-mcp/setup"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// Asynchronous initialize.
//
// initialize used to build the index before it replied: 52 s on dun
// (2026-10-24), where the client gives a server 1.5 s and then shows it as
// still starting. The tool schemas do not depend on the index, so with
// SetAsyncInit the reply goes out at once and the bring-up — parse-cache
// load, index, bindings, child LSPs, watcher — runs behind it. A tool call
// waits for it: what a tool needs is resolved before its first use, and the
// handshake never waits for something no call has asked for yet.
//
// When the bring-up ends the server says so unprompted, as
// notifications/message from logger "poly-lsp-mcp/index": state ready or
// failed, how long it took, and the index cost report (symbols.CostReport),
// so a client can decide what to exclude without waiting for a tool call to
// fail.

// SetAsyncInit makes initialize reply before the workspace is indexed. The
// mcp command sets it; tests and the one-shot paths keep the synchronous
// bring-up, where the initialize result carries the index report.
func (s *Server) SetAsyncInit(on bool) { s.asyncInit = on }

// SetIndexBudget bounds the index walk, in node_query's budget grammar:
// "Nms" (bare = ms) wall-clock, or "Nops", one op per file read. Over
// budget the bring-up fails, and the failure is the trace: where the files
// are, how to get under budget, how to raise it. "" or "0" = unbounded.
func (s *Server) SetIndexBudget(spec string) error {
	if t := strings.TrimSpace(spec); t == "" || t == "0" {
		s.indexBudget = symbols.Budget{}
		return nil
	}
	v, unit, ok := parseBudget(spec)
	if !ok {
		return fmt.Errorf("index budget %q: want Nms (bare = ms) or Nops", spec)
	}
	if unit == "ops" {
		s.indexBudget = symbols.Budget{Ops: v}
	} else {
		s.indexBudget = symbols.Budget{Time: time.Duration(v) * time.Millisecond}
	}
	return nil
}

// indexLogger is the notifications/message logger for bring-up state.
const indexLogger = "poly-lsp-mcp/index"

// startAsyncInit runs the bring-up in the background; waitReady blocks on it.
func (s *Server) startAsyncInit() {
	ready := make(chan struct{})
	s.stateMu.Lock()
	s.ready = ready
	s.stateMu.Unlock()
	start := time.Now()
	go func() {
		defer close(ready)
		// A root that is not a project fails in milliseconds: Init refuses it
		// first thing, so the parse cache is not worth loading for it.
		if s.indexAnyway || symbols.CheckProjectRoot(s.getRoot()) == nil {
			s.maybeLoadCache()
		}
		err := s.Init()
		s.setInitErr(err)
		s.notifyIndexState(err, time.Since(start))
		s.notifySetup()
	}()
}

func (s *Server) setInitErr(err error) {
	s.stateMu.Lock()
	s.initErr = err
	s.stateMu.Unlock()
}

// waitReady blocks until the bring-up has finished, and returns its error.
// Immediate when the bring-up was synchronous or never started.
func (s *Server) waitReady() error {
	s.stateMu.Lock()
	ch := s.ready
	s.stateMu.Unlock()
	if ch != nil {
		<-ch
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.initErr
}

// notifyIndexState tells the client the bring-up finished, and how.
func (s *Server) notifyIndexState(err error, took time.Duration) {
	data := map[string]any{
		"state": "ready",
		"ms":    took.Milliseconds(),
	}
	if c := s.IndexCost(); c.Files > 0 {
		data["index"] = c
	}
	if b := s.bindingHealth(); b != nil {
		data["bindings"] = b
	}
	if sc := s.schemaHealth(); sc != nil {
		data["schemas"] = sc
	}
	level := "info"
	if err != nil {
		level = "error"
		data["state"] = "failed"
		data["error"] = err.Error()
		data["remedy"] = s.indexRemedy(err)
		var be *symbols.BudgetError
		if errors.As(err, &be) {
			data["budget"] = be.Budget.String()
			data["read"] = be.Read
		}
		var np *symbols.NotProjectError
		if errors.As(err, &np) {
			data["reason"] = symbols.LeftNotProject
			data["repos"] = np.Repos
			data["reposTotal"] = np.Total
		}
	}
	s.sendNotification("notifications/message", map[string]any{
		"level":  level,
		"logger": indexLogger,
		"data":   data,
	})
}

// setupLogger is the notifications/message logger for install hints.
const setupLogger = "poly-lsp-mcp/setup"

// setupHints are the install hints for the languages the last build found.
func (s *Server) setupHints() []setup.Lang {
	return setup.Hints(s.registry, s.IndexCost().Languages)
}

// notifySetup tells the client, once per bring-up, which languages the
// workspace holds and what a missing child LSP would add and needs. Level
// warning when one is missing, or when an --lsp-roots refine hint is
// offered, so a client can show it; info otherwise. Sent again on a
// child's first memory stop (see onChildStop): the hints may have changed.
func (s *Server) notifySetup() {
	s.stateMu.Lock()
	err := s.initErr
	s.stateMu.Unlock()
	data := s.setupData(err)
	if data == nil {
		return
	}
	level := "info"
	if data["missing"].(int) > 0 || (!s.lspRootsAuto && data["hints"] != nil) {
		level = "warning"
	}
	s.sendNotification("notifications/message", map[string]any{
		"level":  level,
		"logger": setupLogger,
		"data":   data,
	})
}

// setupData is the poly-lsp-mcp/setup payload: the install hints per
// language, plus the --lsp-roots hints and the roots they name. nil when
// the workspace holds no language. err is the bring-up's outcome.
func (s *Server) setupData(err error) map[string]any {
	langs := s.setupHints()
	if len(langs) == 0 {
		return nil
	}
	data := map[string]any{"languages": langs, "missing": setup.Missing(langs)}
	if hints, roots := s.rootsHints(budgetBlown(err)); len(hints) > 0 {
		data["hints"] = hints
		data["roots"] = roots
	}
	return data
}

// indexRemedy says how to get past a failed bring-up.
func (s *Server) indexRemedy(err error) string {
	var be *symbols.BudgetError
	if errors.As(err, &be) {
		msg := fmt.Sprintf("get under budget: exclude the heavy directories above (--exclude <dir>, repeatable, relative to the root; "+
			"the repository is not changed) or add them to .gitignore. Or raise it: --index-budget %s (now %s).",
			raisedBudget(be), be.Budget)
		// A blown budget is where the pure-Go parser's cost shows, so the
		// fast build is part of the way out.
		if ts.Runtime() == "pure-go" {
			msg += " Or parse faster: " + ts.FastPathHint + "."
		}
		if !s.lspRootsAuto {
			if hints, _ := s.rootsHints(true); len(hints) > 0 {
				msg += "\n" + strings.Join(hints, "\n")
			}
		}
		return msg
	}
	var np *symbols.NotProjectError
	if errors.As(err, &np) {
		return np.Remedy()
	}
	return "fix the cause above and restart the server"
}

// raisedBudget suggests a budget that would have fitted: the spent budget
// scaled by files counted over files read, plus a fifth for margin. When the
// count stopped early it is a lower bound, and so is this.
func raisedBudget(be *symbols.BudgetError) symbols.Budget {
	read := max(be.Read, 1)
	scale := float64(be.Report.Files) / float64(read) * 1.2
	if be.Budget.Ops > 0 {
		return symbols.Budget{Ops: int(float64(be.Budget.Ops)*scale) + 1}
	}
	return symbols.Budget{Time: time.Duration(float64(be.Budget.Time) * scale).Round(time.Second)}
}

// indexFailure is what a tool call answers when the bring-up failed: the
// error, the post-mortem when there is one, and the remedy.
func (s *Server) indexFailure(err error) string {
	msg := fmt.Sprintf("poly-lsp: the workspace index was not built: %v\n", err)
	var be *symbols.BudgetError
	if errors.As(err, &be) {
		msg += be.Report.Text()
	}
	var np *symbols.NotProjectError
	if errors.As(err, &np) {
		// Detail carries the repositories found and the remedy.
		return "poly-lsp: the workspace index was not built: " + np.Detail()
	}
	return msg + "remedy: " + s.indexRemedy(err)
}
