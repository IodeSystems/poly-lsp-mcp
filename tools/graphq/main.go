package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

func main() {
	var (
		bankPath  = flag.String("bank", "", "question bank JSON (required)")
		cardPath  = flag.String("card", "", "language card file (system prompt; required)")
		root      = flag.String("root", ".", "the poly-lsp-mcp checkout to query")
		lang      = flag.String("lang", "gsl", "language under test: gsl | cypher | sql")
		base      = flag.String("base", "http://127.0.0.1:11434/v1", "model API root (…/v1)")
		modelName = flag.String("model", "qwen3.8:27b", "model name")
		outPath   = flag.String("out", "graphq-results.json", "where to write the JSON results")
		limit     = flag.Int("limit", 200, "per-query match limit (GSL)")
		budget    = flag.String("budget", "", "GSL work budget, e.g. 200000ops (empty = default)")
		edgeCap   = flag.Int("edgecap", 2000, "max edge queries when building the shared rows")
		seeds     = flag.String("seeds", "", "comma-separated extra symbol seeds for the edge BFS")
	)
	flag.Parse()

	if *bankPath == "" || *cardPath == "" {
		fmt.Fprintln(os.Stderr, "usage: graphq -bank <bank.json> -card <card> -root <checkout> [-lang gsl|cypher|sql]")
		os.Exit(2)
	}
	bank, err := loadBank(*bankPath)
	if err != nil {
		fail("bank: %v", err)
	}
	card, err := os.ReadFile(*cardPath)
	if err != nil {
		fail("card: %v", err)
	}

	// The GSL server is needed for both the GSL executor and the shared row
	// builder. Cypher/SQL only need the rows, but they come from the same
	// server, so build it once.
	srv, err := newGSLServer(*root)
	if err != nil {
		fail("server: %v", err)
	}
	h := &harness{server: srv, limit: *limit, budget: *budget, edgeQueryCap: *edgeCap}

	// Seeds: the symbols the round is about. Derived from the bank's truth
	// (the symbols every question names) plus any explicit -seeds.
	seedSet := map[string]bool{}
	for _, q := range bank.Questions {
		for _, t := range q.Truth {
			if n := symbolLeaf(t); n != "" {
				seedSet[n] = true
			}
		}
	}
	for _, s := range strings.Split(*seeds, ",") {
		if s = strings.TrimSpace(s); s != "" {
			seedSet[symbolLeaf(s)] = true
		}
	}
	var seedsList []string
	for s := range seedSet {
		if s != "" {
			seedsList = append(seedsList, s)
		}
	}
	sort.Strings(seedsList)

	rows, err := h.BuildRows(seedsList)
	if err != nil {
		fail("rows: %v", err)
	}

	model := newChatModel(*base, *modelName)
	var results []Result
	for _, q := range bank.Questions {
		res := runQuestion(h, rows, q, *lang, string(card), model)
		results = append(results, res)
	}

	aggs := []Aggregate{aggregate(*lang, results)}
	fmt.Print(renderTable(aggs))
	if err := writeJSON(*outPath, *lang, results, aggs); err != nil {
		fail("out: %v", err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", *outPath)
}

// runQuestion asks the model for one query, executes it (with a single retry
// on a parse/execute error), and scores the answer against the question's
// truth.
func runQuestion(h *harness, rows *Rows, q Question, lang, card string, model Model) Result {
	res := Result{ID: q.ID, Text: q.Text, Language: lang, Truth: q.Truth}

	query, pt, ct, wall, err := model(card, q.Text)
	res.PromptTok += pt
	res.ComplTok += ct
	res.WallMs += int64(wall.Milliseconds())
	if err != nil {
		res.ExecErr = "model: " + err.Error()
		return res
	}
	query = extractQuery(query)

	answer, execErr, se := execute(h, rows, lang, query)
	res.Query = query
	res.Answer = answer
	if execErr != nil {
		res.ExecErr = execErr.Error()
		return res
	}
	if se != nil {
		// Single retry: feed the parse/execute error back to the model ONCE.
		res.Retries = 1
		retry := "Your query failed with: " + se.Err +
			"\nReturn a corrected query (the query and nothing else)."
		rq, rpt, rct, rwall, rerr := model(card, retry)
		res.PromptTok += rpt
		res.ComplTok += rct
		res.WallMs += int64(rwall.Milliseconds())
		if rerr != nil {
			res.ExecErr = "model retry: " + rerr.Error()
			return res
		}
		rq = extractQuery(rq)
		res.Query = rq
		answer, execErr, _ = execute(h, rows, lang, rq)
		res.Answer = answer
		if execErr != nil {
			res.ExecErr = execErr.Error()
			return res
		}
	}
	res.Correct = scoreQuestion(answer, q.Truth, q.Match)
	return res
}

// execute runs one query in the given language and returns the answer set
// (canonical node addresses). It returns (answer, opErr, selectorErr):
//   - opErr: an operational failure (the question is a hard miss).
//   - selectorErr: a parse/execute error the model can fix (triggers the
//     single retry); non-nil only for GSL.
func execute(h *harness, rows *Rows, lang, query string) (answer []string, opErr error, selErr *selectorErr) {
	switch lang {
	case "gsl":
		matches, se, err := h.runQuery(query)
		if err != nil {
			return nil, err, nil
		}
		if se != nil {
			return nil, nil, &selectorErr{Err: se.Err.Error()}
		}
		var ans []string
		for _, m := range matches {
			if m.Node != "" {
				ans = append(ans, m.Node)
			}
		}
		return dedupeStr(ans), nil, nil
	case "cypher":
		ans, err := execCypher(rows, query)
		if err != nil {
			return nil, err, nil
		}
		return ans, nil, nil
	case "sql":
		ans, err := execSQL(rows, query)
		if err != nil {
			return nil, err, nil
		}
		return ans, nil, nil
	default:
		return nil, fmt.Errorf("unknown language %q", lang), nil
	}
}

// selectorErr carries a model-fixable parse/execute message for the retry.
type selectorErr struct{ Err string }
func (e *selectorErr) Error() string { return e.Err }

// writeJSON writes the results and aggregates to the out path.
func writeJSON(path, lang string, results []Result, aggs []Aggregate) error {
	doc := map[string]any{
		"language":   lang,
		"results":    results,
		"aggregate":  aggs,
		"generated":  time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
