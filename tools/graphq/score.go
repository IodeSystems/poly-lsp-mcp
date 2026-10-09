package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/iodesystems/poly-lsp-mcp/mcp"
)

// harness bundles the state one round of the experiment shares: the in-process
// mcp server (GSL executor + row source), the per-question limits, and the
// edge-query budget. It is built in main.go and used by the executors; the
// unit tests build a harness directly (no network, no model).
type harness struct {
	server       *mcp.Server
	limit        int
	budget       string
	edgeQueryCap int
}

// Canonical answer-address form.
//
// The scorer normalizes every returned node to ONE string and compares the
// set to the bank's truth. The form is:
//
//   - a symbol (func/method/type/...): "file#Name" — workspace-relative file
//     path + "#" + leaf symbol name. This is mcp's own addr() for a symbol.
//   - a call-site / ref edge row: "file@line".
//   - a file or dir: its workspace-relative path.
//   - the project root: its base name.
//
// The research subtask writing bank `truth` MUST use this exact form.
//
//   - "who calls Save"  → truth is the set of caller SYMBOLS: ["a.go#Caller"].
//   - "where Save is called" (the sites) → truth is ["a.go@6"].

// normalizeAnswers maps raw returned node addresses to the canonical form and
// de-duplicates them into a set. Addresses are already canonical from the
// executors (GSL addr() / Cypher & SQL node.id), so this is a set + trim.
func normalizeAnswers(addrs []string) map[string]bool {
	out := map[string]bool{}
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a != "" {
			out[a] = true
		}
	}
	return out
}

// scoreQuestion compares a returned answer set to the bank's truth per the
// question's match mode:
//
//   - "set"      (default): correct iff the answer set EQUALS the truth set.
//   - "superset": correct iff truth ⊆ answer (the answer may have extras).
//
// This is the ONLY correctness rule — the bank holds ground truth; the
// harness does not re-derive it.
func scoreQuestion(answer, truth []string, match string) bool {
	a := normalizeAnswers(answer)
	t := normalizeAnswers(truth)
	switch match {
	case "superset":
		for x := range t {
			if !a[x] {
				return false
			}
		}
		return true
	default: // "set"
		if len(a) != len(t) {
			return false
		}
		for x := range t {
			if !a[x] {
				return false
			}
		}
		return true
	}
}

// Result is the per-question, per-language record.
type Result struct {
	ID        string   `json:"id"`
	Text      string   `json:"text"`
	Language  string   `json:"language"`
	Correct   bool     `json:"correct"`
	Retries   int      `json:"retries"`
	PromptTok int      `json:"prompt_tokens"`
	ComplTok  int      `json:"completion_tokens"`
	WallMs    int64    `json:"wall_ms"`
	Query     string   `json:"query"`
	Answer    []string `json:"answer"`
	Truth     []string `json:"truth"`
	ExecErr   string   `json:"exec_error,omitempty"`
}

// Aggregate is the per-language rollup.
type Aggregate struct {
	Language       string  `json:"language"`
	Total          int     `json:"total"`
	Correct        int     `json:"correct"`
	Retries        int     `json:"retries"`
	MeanPromptTok  float64 `json:"mean_prompt_tokens"`
	MeanWallMs     float64 `json:"mean_wall_ms"`
}

// aggregate rolls per-question results into the per-language summary.
func aggregate(lang string, rs []Result) Aggregate {
	agg := Aggregate{Language: lang, Total: len(rs)}
	var pt, wm int
	for _, r := range rs {
		if r.Correct {
			agg.Correct++
		}
		agg.Retries += r.Retries
		pt += r.PromptTok
		wm += int(r.WallMs)
	}
	if len(rs) > 0 {
		agg.MeanPromptTok = float64(pt) / float64(len(rs))
		agg.MeanWallMs = float64(wm) / float64(len(rs))
	}
	return agg
}

// renderTable is the markdown table printed to stdout.
func renderTable(aggs []Aggregate) string {
	var b strings.Builder
	b.WriteString("| language | correct/total | retries | mean prompt tok | mean wall ms |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, a := range aggs {
		fmt.Fprintf(&b, "| %s | %d/%d | %d | %.0f | %.0f |\n",
			a.Language, a.Correct, a.Total, a.Retries, a.MeanPromptTok, a.MeanWallMs)
	}
	return b.String()
}

// sortedKeys is a small helper for deterministic output.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
