package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/iodesystems/tslsmcp/internal/symbols"
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
// custom tslsmcp:// scheme so they don't collide with any file:// or
// http:// URIs the agent might be tracking from other tools.
func registerResources() map[string]Resource {
	return map[string]Resource{
		"tslsmcp://workspace": {
			URI:  "tslsmcp://workspace",
			Name: "workspace",
			Description: "Summary of what tslsmcp has indexed for this workspace: " +
				"the resolved root path, the set of languages observed, the total " +
				"name count, and how many of those names carry declared bindings. " +
				"A quick way to confirm tslsmcp.yaml is doing what you expect.",
			MimeType: "application/json",
			Read:     readWorkspace,
		},
		"tslsmcp://bindings": {
			URI:  "tslsmcp://bindings",
			Name: "bindings",
			Description: "Catalog of every cross-language binding (Tier 2 + Tier 3). " +
				"Same payload as the `list_bindings` tool but exposed as a resource so " +
				"MCP clients can pin it into model context without a tool call.",
			MimeType: "application/json",
			Read:     readBindings,
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
