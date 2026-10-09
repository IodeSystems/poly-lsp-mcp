package symbols

import "sort"

// Plugin is a convention extractor that adds typed edges to the index.
// A plugin is registered for a named convention (e.g. "event-kind") and
// runs over every file the Build walk saw. Its edges are stored in a
// dedicated store on Index, separate from lexical, declared, and comment
// sites, so that zero plugins → zero plugin edges → the index is
// byte-for-byte what it is today.
type Plugin struct {
	// Convention is the named convention this plugin covers, e.g.
	// "event-kind". It becomes the edge kind in query output.
	Convention string
	// Extract runs over the files the Build walk indexed. It returns
	// the plugin's edges. It must be deterministic and side-effect-free.
	Extract func(files []PluginFile) []PluginEdge
}

// PluginFile is one file a plugin can read: its workspace-relative path,
// raw content, and the language the Build walk assigned it.
type PluginFile struct {
	Path     string
	Content  []byte
	Language string
}

// PluginEdge is one edge a plugin produces. Kind is the convention kind
// name (e.g. "plan.landed"); File/Line/Col locate the site; Conf is the
// confidence label from the same set query already prints ("lexical",
// "lsp", "unsettled").
type PluginEdge struct {
	Kind   string
	File   string
	Line   int
	Col    int
	Conf   string
}

// confFromLabel maps a plugin confidence label to the Site's Confidence
// rank. "unsettled" has no dedicated rank; it maps to ConfidenceLexical
// since the plugin's deterministic match is lexical-grade. The raw label
// is preserved in PluginEdge.Conf for query display.
func confFromLabel(conf string) Confidence {
	switch conf {
	case "lsp":
		return ConfidenceLSP
	default: // "lexical", "unsettled", ""
		return ConfidenceLexical
	}
}

// InsertPlugin registers one plugin edge site. Idempotent at
// (kind, file, line, col) so a plugin that runs twice does not produce
// duplicates.
func (i *Index) InsertPlugin(e PluginEdge) {
	i.mu.Lock()
	i.gen++
	defer i.mu.Unlock()
	sites := i.pluginEdges[e.Kind]
	for _, s := range sites {
		if s.File == e.File && s.Line == e.Line && s.Col == e.Col {
			return
		}
	}
	sites = append(sites, Site{
		File:       e.File,
		Line:       e.Line,
		Col:        e.Col,
		Confidence: confFromLabel(e.Conf),
	})
	i.pluginEdges[e.Kind] = sites
}

// PluginNames returns every kind with at least one plugin edge, sorted.
func (i *Index) PluginNames() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]string, 0, len(i.pluginEdges))
	for k := range i.pluginEdges {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PluginSites returns the sites for a plugin kind. Returns nil if the
// kind has no plugin edges.
func (i *Index) PluginSites(kind string) []Site {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.pluginEdges[kind]
}

// WithPlugins registers one or more plugins to run after the lexical
// extraction in Build. Each plugin sees every file the walk indexed and
// its edges are stored in the Index's pluginEdges map.
func WithPlugins(plugins ...Plugin) BuildOption {
	return func(cfg *buildConfig) { cfg.plugins = append(cfg.plugins, plugins...) }
}
