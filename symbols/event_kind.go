package symbols

import (
	"regexp"
)

// eventKindRe matches dotted-identifier string literals: at least two
// dot-separated lowercase segments, e.g. "plan.landed", "issue.ingested".
var eventKindRe = regexp.MustCompile(`"([a-z][a-z0-9]*\.[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)*)"`)

// EventKindPlugin returns a Plugin that scans Go source files for string
// literals matching the dotted-identifier pattern and emits one PluginEdge
// per occurrence with Conf set to "lexical".
func EventKindPlugin() Plugin {
	return Plugin{
		Convention: "event-kind",
		Extract:    eventKindExtract,
	}
}

func eventKindExtract(files []PluginFile) []PluginEdge {
	var edges []PluginEdge
	for _, f := range files {
		if f.Language != "go" {
			continue
		}
		edges = append(edges, scanEventKinds(f)...)
	}
	return edges
}

// scanEventKinds finds every dotted-identifier string literal in one file
// and returns a PluginEdge for each occurrence.
func scanEventKinds(f PluginFile) []PluginEdge {
	matches := eventKindRe.FindAllIndex(f.Content, -1)
	if matches == nil {
		return nil
	}
	var edges []PluginEdge
	for _, m := range matches {
		lit := f.Content[m[0]:m[1]] // includes the quotes
		kind := string(lit[1 : len(lit)-1]) // strip quotes
		// Compute line and column of the opening quote.
		line, col := 1, 1
		for i := 0; i < m[0]; i++ {
			if f.Content[i] == '\n' {
				line++
				col = 1
			} else {
				col++
			}
		}
		edges = append(edges, PluginEdge{
			Kind:   kind,
			File:   f.Path,
			Line:   line,
			Col:    col,
			Conf:   "lexical",
		})
	}
	return edges
}
