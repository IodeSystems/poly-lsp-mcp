package mcp

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/iodesystems/poly-lsp-mcp/internal/symbols"
)

// Resource is one MCP resource the server exposes. Resources are
// addressable by URI and read on demand; unlike tools they don't take
// arguments and return whole-document content blocks.
type Resource struct {
	URI         string
	Name        string
	Description string
	MimeType    string
	Read        func(s *Server) (string, error)
}

// resourceContent is the wire shape resources/read returns inside the
// `contents` array. One per resource read (we always emit a single
// content block per URI).
type resourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text"`
}

// registerResources builds the read-only resource table the
// resources/list and resources/read handlers consult. URIs use a
// custom poly-lsp-mcp:// scheme so they don't collide with any file:// or
// http:// URIs the agent might be tracking from other tools.
func registerResources() map[string]Resource {
	return map[string]Resource{
		"poly-lsp-mcp://workspace": {
			URI:  "poly-lsp-mcp://workspace",
			Name: "workspace",
			Description: "Summary of what poly-lsp-mcp has indexed for this workspace: " +
				"the resolved root path, the set of languages observed, the total " +
				"name count, and how many of those names carry declared bindings. " +
				"A quick way to confirm poly-lsp-mcp.yaml is doing what you expect.",
			MimeType: "application/json",
			Read:     readWorkspace,
		},
		"poly-lsp-mcp://bindings": {
			URI:  "poly-lsp-mcp://bindings",
			Name: "bindings",
			Description: "Catalog of every cross-language binding (Tier 2 + Tier 3). " +
				"Same payload as the `list_bindings` tool but exposed as a resource so " +
				"MCP clients can pin it into model context without a tool call.",
			MimeType: "application/json",
			Read:     readBindings,
		},
		"poly-lsp-mcp://diagnostics": {
			URI:  "poly-lsp-mcp://diagnostics",
			Name: "diagnostics",
			Description: "Workspace-wide diagnostic snapshot from every running child LSP. " +
				"Each entry has the same enriched shape as the diagnostics field on a " +
				"node_edit / node_delete / node_refactor response (text / context / " +
				"enclosingNode / references). diagnosticsAvailable is false when no LSP " +
				"is running for any indexed language. The snapshot reflects whatever " +
				"the LSPs have published so far — for fresh state, edit a file or " +
				"open it via your editor; gopls in particular only publishes on " +
				"didOpen / didChange / didSave.",
			MimeType: "application/json",
			Read:     readDiagnostics,
		},
	}
}

func readWorkspace(s *Server) (string, error) {
	root := s.getRoot()
	idx := s.getIndex()
	payload := map[string]any{
		"root":      root,
		"languages": []string{},
		"names":     0,
		"declared":  0,
	}
	if idx != nil {
		payload["languages"] = idx.Languages()
		payload["names"] = len(idx.Names())
		payload["declared"] = len(idx.DeclaredNames())
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal workspace: %w", err)
	}
	return string(raw), nil
}

// readDiagnostics emits a workspace-wide snapshot of every diagnostic
// the child LSPs have published, enriched the same way as a node_edit
// response (text / context / enclosingNode / references) so consumers
// don't need a separate parser.
//
// When manager is nil OR no child LSP is currently running we return
// diagnosticsAvailable=false and an empty list — there's no compiler
// talking to us, so "no errors" can't be asserted. The agent must
// treat the absence as "unknown," not "clean."
func readDiagnostics(s *Server) (string, error) {
	payload := map[string]any{
		"diagnosticsAvailable": false,
		"languages":            []string{},
		"diagnostics":          []diagnosticJSON{},
	}
	if s.manager == nil {
		raw, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return "", fmt.Errorf("marshal diagnostics: %w", err)
		}
		return string(raw), nil
	}

	langs := s.manager.Languages()
	payload["languages"] = langs
	if len(langs) == 0 {
		raw, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return "", fmt.Errorf("marshal diagnostics: %w", err)
		}
		return string(raw), nil
	}

	snapshot := s.manager.Diagnostics().Snapshot()
	// Enrich each diagnostic using package defaults — the resource
	// has no per-call args, so cap behavior matches the edit-time
	// defaults (25 / 15 / 3). Sorted by file then position so
	// repeated reads are stable.
	uris := make([]string, 0, len(snapshot))
	for uri := range snapshot {
		uris = append(uris, uri)
	}
	sort.Strings(uris)

	opts := diagnosticOptions{}
	limit := opts.diagnosticLimit()
	items := make([]diagnosticJSON, 0, 32)
	dropped := 0
	for _, uri := range uris {
		for _, d := range snapshot[uri] {
			if len(items) >= limit {
				dropped++
				continue
			}
			items = append(items, s.enrichDiagnostic(uri, d, nil, opts))
		}
	}
	payload["diagnosticsAvailable"] = true
	payload["diagnostics"] = items
	if dropped > 0 {
		payload["droppedDiagnostics"] = dropped
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal diagnostics: %w", err)
	}
	return string(raw), nil
}

func readBindings(s *Server) (string, error) {
	idx := s.getIndex()
	if idx == nil {
		return "[]", nil
	}
	names := idx.DeclaredNames()
	out := make([]bindingSummary, 0, len(names))
	for _, name := range names {
		var sites []siteJSON
		langSet := map[string]struct{}{}
		for _, site := range idx.Lookup(name) {
			if site.Confidence < symbols.ConfidenceDeclared {
				continue
			}
			sites = append(sites, siteJSON{
				Name:       name,
				File:       relPath(site.File, s.getRoot()),
				Line:       site.Line,
				Col:        site.Col,
				Language:   site.Language,
				Confidence: confidenceLabel(site.Confidence),
			})
			if site.Language != "" {
				langSet[site.Language] = struct{}{}
			}
		}
		langs := make([]string, 0, len(langSet))
		for l := range langSet {
			langs = append(langs, l)
		}
		out = append(out, bindingSummary{
			Name:      name,
			SiteCount: len(sites),
			Languages: langs,
			Sites:     sites,
		})
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal bindings: %w", err)
	}
	return string(raw), nil
}
