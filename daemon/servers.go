package daemon

import (
	"crypto/sha256"
	"log"
	"os"
	"path/filepath"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// ServersPath is the user's server registry (servers.yaml) in the daemon
// home: written by `poly-lsp-mcp lsp add/remove`, read by the daemon and
// by an in-process server. User-level on purpose: the daemon runs the
// commands it names, and a repository's file must not make it do so.
func ServersPath() string {
	return filepath.Join(ConfigHome(), config.ServersFile)
}

// SetServersFile makes the registry re-read path on each client open and
// merge it over base (the languages before servers.yaml: the defaults).
// The registry it was built with is taken to be path as it is now. Call
// before Serve; not called when a config file's languages: replace the
// registry.
func (r *Registry) SetServersFile(path string, base []config.Language) {
	r.serversMu.Lock()
	defer r.serversMu.Unlock()
	r.serversPath, r.serversBase = path, base
	r.serversSeen, _ = readServersStamp(path)
}

// readServersStamp stats and hashes path. The data is nil for a missing
// file.
func readServersStamp(path string) (serversStamp, []byte) {
	fi, err := os.Stat(path)
	if err != nil {
		return serversStamp{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return serversStamp{}, nil
	}
	return serversStamp{mod: fi.ModTime(), size: fi.Size(), sum: sha256.Sum256(data), ok: true}, data
}

// checkServers re-reads servers.yaml when its mtime, size or presence
// changed and its content did too, and swaps in the merged registry. The
// roots built with the old one are rebuilt on their next open (get: the
// generation differs), by the same path as an open with other index
// options. A file that does not parse or merge is logged and the registry
// in use is kept.
func (r *Registry) checkServers() {
	r.serversMu.Lock()
	defer r.serversMu.Unlock()
	if r.serversBase == nil {
		return
	}
	fi, err := os.Stat(r.serversPath)
	switch {
	case err != nil && !r.serversSeen.ok:
		return // still missing
	case err == nil && r.serversSeen.ok && fi.ModTime().Equal(r.serversSeen.mod) && fi.Size() == r.serversSeen.size:
		return
	}
	st, data := readServersStamp(r.serversPath)
	if st.ok == r.serversSeen.ok && st.sum == r.serversSeen.sum {
		r.serversSeen = st // touched, same content
		return
	}
	r.serversSeen = st
	s, err := config.ParseServers(r.serversPath, data)
	if err != nil {
		log.Printf("daemon: %v; keeping the language registry in use", err)
		return
	}
	langs, err := config.MergeServers(r.serversBase, s.Servers)
	if err != nil {
		log.Printf("daemon: %s: %v; keeping the language registry in use", r.serversPath, err)
		return
	}
	r.mu.Lock()
	cfg := *r.cfg
	r.mu.Unlock()
	cfg.Languages = langs
	reg, err := cfg.Build()
	if err != nil {
		log.Printf("daemon: %s: %v; keeping the language registry in use", r.serversPath, err)
		return
	}
	r.mu.Lock()
	r.cfg, r.reg = &cfg, reg
	r.gen++
	r.mu.Unlock()
	log.Printf("daemon: %s changed (%d server(s)); each root rebuilds on its next open", r.serversPath, len(s.Servers))
}
