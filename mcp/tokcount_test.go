package mcp

import (
	"encoding/json"
	"testing"
)

// modernTokenBudget caps the WHOLE 3-tool surface, in approximate tokens
// (chars/4).
//
// READ THIS BEFORE TRIMMING ANYTHING TO FIT.
//
// This budget used to be justified as "MCP re-sends every tool definition on
// every turn, so it is paid per turn, forever". That is FALSE, and believing it
// cost a lot. llama.cpp reuses the KV slot: a stable prefix is SENT every turn
// and EVALUATED once. Measured on a real run: 19,044 prompt tokens sent, 15,968
// served from cache (83%), 3,076 actually evaluated. The tool schema is the most
// cacheable region of the entire context — byte-identical every turn — so it is
// very nearly free after turn 1.
//
// Summing per-turn prompt_tokens (crucible's old headline metric) therefore
// charged this schema once per turn and made it look like the dominant cost. It
// isn't. Measured on generated tokens and wall clock, poly-lsp vs bash was ~12%,
// not the 2.2x the token count claimed — and the 9→3 redesign, the 4327→949 cut,
// and "move the grammar out of the schema" were all aimed at bytes the cache had
// already made free.
//
// So what is this budget still for? NOT cost:
//   - Attention: every line here is read by the model on every turn. Sprawl
//     competes with the task.
//   - Self-consistency: a long description drifts. This one once said "TYPES are
//     bare tags" and then demonstrated `.file` two lines later.
//   - First turn is genuinely uncached.
//
// None of those justify shaving 10 tokens off a working explanation. Treat a
// failure as "is this line earning its attention?" — and if the answer is yes,
// raise the number and say why. Evidence beats the ceiling.
//
// History: 900 → 950 to buy the TYPES/ids lesson (tags, not `.class`, after the
// model invented `.cache` 12 times in one run). 950 → 1000 for the kitchen-sink
// example: :has/:has_parent/:references/:depth were used ZERO times while
// documented in prose, and prose is demonstrably not what the model copies —
// examples are. If that example doesn't move their usage, delete it AND the
// features' prose, and take ~90 tokens back.
// 1000 → 1080 for the graph half: :parents / :where / :any / :all / :empty
// replaced the three zero-use pseudos, and the extra tokens are almost all
// RECIPES keyed to an address ("you have store.go#Save — now what"), the one
// form measured to move usage (took :references 0 → opening move). If the
// recipes don't move :parents usage, cut them and take the tokens back.
// 1080 → 1160 for the node model: references became ::in/::out pseudo-element
// NODES with kind classes, plus language classes, {m,n} repetition/groups,
// position claims and the upstream :parents — a whole graph algebra whose
// description is recipes-first by design. If edge usage doesn't move, the
// recipes are the first thing to re-measure, not the first thing to cut.
// 1160 → 1050: the description compacted to surprises-and-footguns-first
// (the FOOTGUNS block + recipes ARE the description; everything derivable
// from CSS priors got cut — the priors are correct now, that's the point).
// 1050 → 1160: features shipped after the compaction re-added bytes, each
// earning its attention — the node_edit RENAME-SAFETY block ("use the
// rename op, don't hand-edit"), which the --validate benchmark measured as
// THE lever that put Qwen on the atomic rename op 10/10 (broken=0 without
// even validating); the node_query `budget` arg (Nms/Nops) + its blow note;
// ::in.return.type / precision `conf`. Re-audited at 1133: every line is a
// footgun or a measured recipe, no sprawl to shave. Back to the prior
// high-water ceiling, not past it.
const modernTokenBudget = 1160

// TestModernToolSurfaceTokenBudget reports the per-tool cost and guards
// the total.
func TestModernToolSurfaceTokenBudget(t *testing.T) {
	tools := registerModernTools()
	total := 0
	t.Logf("%-12s %8s %8s %8s", "TOOL", "DESC", "SCHEMA", "TOKENS")
	for _, name := range []string{"node_query", "node_read", "node_edit"} {
		tool, ok := tools[name]
		if !ok {
			t.Fatalf("modern surface missing %q", name)
		}
		// The schema goes to the wire verbatim, so it must be valid
		// JSON even though it's hand-written and minified.
		var probe any
		if err := json.Unmarshal(tool.InputSchema, &probe); err != nil {
			t.Errorf("%s: inputSchema is not valid JSON: %v", name, err)
		}
		d, s := len(tool.Description), len(string(tool.InputSchema))
		tok := (d + s) / 4
		total += tok
		t.Logf("%-12s %8d %8d %8d", name, d, s, tok)
	}
	t.Logf("%-12s %26d (budget %d)", "TOTAL", total, modernTokenBudget)
	if total > modernTokenBudget {
		t.Errorf("modern surface costs ~%d tokens/turn, over the %d budget", total, modernTokenBudget)
	}
}
