package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Usage log: one JSON line per node_query, so usage decides what the query
// tree should skip. node_edit calls are logged too (Tool == "node_edit"), so a
// week of data decides whether params/return earn their schema tokens; lines
// without a Tool are node_query, which keeps old logs readable.
//
// The open question (2026-10-24): should node_query honour .gitignore the
// way the symbol index does? Doing so hides gitignored files — generated
// GraphQL types, build output — that an agent may be querying on purpose.
// And node_modules is skipped by default but reachable when a selector
// names a path inside it. Both are decided by how often such results are
// returned, and how often the selector asked for them by name; this log
// records exactly that. Local only: it never leaves the machine.

// usageMaxBytes caps the log; at the cap it is moved to usage.jsonl.1.
const usageMaxBytes = 10 << 20

// UsageRecord is one node_query, or one node_edit when Tool is "node_edit"
// (then only At, Root and the Op/Hidden/Result fields are set).
type UsageRecord struct {
	// Tool is "" for node_query (the original record) or "node_edit".
	Tool string `json:"tool,omitempty"`
	// Op, node_edit only: edit | create | delete | rename | params | return |
	// accept | batch (commit/rollback with no op), or the "+"-joined list
	// when a call wrongly supplied several. "create" is newText alone, so it
	// includes span rewrites.
	Op string `json:"op,omitempty"`
	// Hidden, node_edit only: the undeclared params used, e.g.
	// includeComments=true, commit=false, rollback.
	Hidden []string `json:"hidden,omitempty"`
	// Result, node_edit only: "ok" or "error".
	Result   string `json:"result,omitempty"`
	At       string `json:"at"` // RFC 3339
	Root     string `json:"root"`
	Selector string `json:"selector"`
	Matches  int    `json:"matches"`
	// Ignored: matches in a gitignored file or directory. IgnoredNamed: of
	// those, the ones the selector named by path.
	Ignored      int `json:"ignored"`
	IgnoredNamed int `json:"ignoredNamed"`
	// Skipped: matches inside a skipped directory (node_modules, vendor…),
	// reachable only because the selector named them or include opened them.
	Skipped int   `json:"skipped"`
	Ms      int64 `json:"ms"`
	// Include: the patterns the caller opened dead ends with, so the log shows
	// how often they are wanted.
	Include []string `json:"include,omitempty"`
}

// SetUsageLog appends a UsageRecord to path for every node_query. "" = off.
func (s *Server) SetUsageLog(path string) { s.usageLog = path }

var usageMu sync.Mutex

// recordUsage classifies the matches and appends one line. Errors are
// dropped: the log is for measurement, never for correctness.
func (s *Server) recordUsage(selector string, include []string, rows []*treeNode, took time.Duration) {
	if s.usageLog == "" {
		return
	}
	root := s.getRoot()
	ign := s.getIgnores()
	paths := selectorPaths(selector)
	rec := UsageRecord{
		At:       time.Now().Format(time.RFC3339),
		Root:     root,
		Selector: selector,
		Include:  include,
		Matches:  len(rows),
		Ms:       took.Milliseconds(),
	}
	for _, n := range rows {
		if n.file == "" {
			continue
		}
		abs := filepath.Join(root, n.file)
		if ign.KnownIgnored(root, abs) || ign.DirIgnored(root, abs) {
			rec.Ignored++
			if namesPath(paths, n.file) {
				rec.IgnoredNamed++
			}
		}
		for _, seg := range strings.Split(n.file, "/") {
			if skipScanDir(seg) {
				rec.Skipped++
				break
			}
		}
	}
	s.appendUsage(rec)
}

// withEditUsage wraps node_edit's handler so every call is logged. It lives
// here, not in modern.go, so the handler keeps its single definition there.
func withEditUsage(h func(*Server, sessionID, json.RawMessage) ([]Content, bool, error)) func(*Server, sessionID, json.RawMessage) ([]Content, bool, error) {
	return func(s *Server, sess sessionID, args json.RawMessage) ([]Content, bool, error) {
		c, isErr, err := h(s, sess, args)
		s.recordEditUsage(args, isErr || err != nil)
		return c, isErr, err
	}
}

// recordEditUsage logs one node_edit call: which op, which hidden params, and
// whether it failed. Unparseable arguments log as op "invalid".
func (s *Server) recordEditUsage(args json.RawMessage, failed bool) {
	if s.usageLog == "" {
		return
	}
	var p modernEditArgs
	op := "invalid"
	var hidden []string
	if json.Unmarshal(args, &p) == nil {
		op = editOpKind(&p)
		if p.IncludeComments != nil {
			hidden = append(hidden, fmt.Sprintf("includeComments=%t", *p.IncludeComments))
		}
		if p.Commit != nil {
			hidden = append(hidden, fmt.Sprintf("commit=%t", *p.Commit))
		}
		if p.Rollback {
			hidden = append(hidden, "rollback")
		}
	}
	res := "ok"
	if failed {
		res = "error"
	}
	s.appendUsage(UsageRecord{
		At: time.Now().Format(time.RFC3339), Root: s.getRoot(),
		Tool: "node_edit", Op: op, Hidden: hidden, Result: res,
	})
}

// editOpKind names a node_edit call's op by what it does (see UsageRecord.Op).
func editOpKind(p *modernEditArgs) string {
	var kinds []string
	switch {
	case p.OldText != nil:
		kinds = append(kinds, "edit")
	case p.Delete != nil:
		kinds = append(kinds, "delete")
	case p.NewText != nil:
		kinds = append(kinds, "create")
	}
	if p.Rename != nil {
		kinds = append(kinds, "rename")
	}
	if p.Params != nil {
		kinds = append(kinds, "params")
	}
	if p.Return != nil {
		kinds = append(kinds, "return")
	}
	if p.Accept != nil {
		kinds = append(kinds, "accept")
	}
	if len(kinds) == 0 {
		return "batch"
	}
	return strings.Join(kinds, "+")
}

// appendUsage writes one record, rotating the log at usageMaxBytes.
func (s *Server) appendUsage(rec UsageRecord) {
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	usageMu.Lock()
	defer usageMu.Unlock()
	if fi, err := os.Stat(s.usageLog); err == nil && fi.Size() > usageMaxBytes {
		_ = os.Rename(s.usageLog, s.usageLog+".1")
	}
	if err := os.MkdirAll(filepath.Dir(s.usageLog), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(s.usageLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// selectorPaths is every path-like token (one with a slash) in a selector.
func selectorPaths(selector string) []string {
	var out []string
	for _, tok := range namedPathToken.FindAllString(selector, -1) {
		if !strings.Contains(tok, "/") {
			continue
		}
		tok = strings.TrimPrefix(tok, "./")
		if i := strings.IndexByte(tok, '#'); i >= 0 {
			tok = tok[:i]
		}
		if tok = strings.TrimSuffix(tok, "/"); tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

// namesPath reports whether file is one of paths or inside one.
func namesPath(paths []string, file string) bool {
	for _, p := range paths {
		if file == p || strings.HasPrefix(file, p+"/") {
			return true
		}
	}
	return false
}

// UsageSummary totals a usage log.
type UsageSummary struct {
	Queries         int      `json:"queries"`
	WithIgnored     int      `json:"withIgnored"`    // queries that returned a gitignored match
	IgnoredByName   int      `json:"ignoredByName"`  // …and named its path
	WithSkipped     int      `json:"withSkipped"`    // queries that returned a match in a skipped dir
	IgnoredMatches  int      `json:"ignoredMatches"` // matches in gitignored paths
	AllMatches      int      `json:"allMatches"`
	IgnoredExamples []string `json:"ignoredExamples,omitempty"` // up to 10 selectors

	// node_edit: calls by op kind, how many of them failed, and how often each
	// hidden param was used.
	Edits      int            `json:"edits"`
	EditOps    map[string]int `json:"editOps,omitempty"`
	EditFailed map[string]int `json:"editFailed,omitempty"`
	EditHidden map[string]int `json:"editHidden,omitempty"`
}

// SummarizeUsage reads a usage log (and its rotated .1) and totals it.
func SummarizeUsage(path string) (UsageSummary, error) {
	var sum UsageSummary
	read := false
	for _, p := range []string{path + ".1", path} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		read = true
		for _, line := range strings.Split(string(b), "\n") {
			var r UsageRecord
			if line == "" || json.Unmarshal([]byte(line), &r) != nil {
				continue
			}
			if r.Tool == "node_edit" {
				sum.addEdit(r)
				continue
			}
			sum.Queries++
			sum.AllMatches += r.Matches
			sum.IgnoredMatches += r.Ignored
			if r.Ignored > 0 {
				sum.WithIgnored++
				if len(sum.IgnoredExamples) < 10 {
					sum.IgnoredExamples = append(sum.IgnoredExamples, r.Selector)
				}
			}
			if r.IgnoredNamed > 0 {
				sum.IgnoredByName++
			}
			if r.Skipped > 0 {
				sum.WithSkipped++
			}
		}
	}
	if !read {
		return sum, os.ErrNotExist
	}
	return sum, nil
}

func (sum *UsageSummary) addEdit(r UsageRecord) {
	if sum.EditOps == nil {
		sum.EditOps, sum.EditFailed, sum.EditHidden = map[string]int{}, map[string]int{}, map[string]int{}
	}
	sum.Edits++
	sum.EditOps[r.Op]++
	if r.Result == "error" {
		sum.EditFailed[r.Op]++
	}
	for _, h := range r.Hidden {
		sum.EditHidden[h]++
	}
}
