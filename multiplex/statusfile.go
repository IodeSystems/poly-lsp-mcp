package multiplex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StatusFile is what a Manager writes to StatusDir every Interval, so
// `poly-lsp-mcp status` can show every process's children in one place.
type StatusFile struct {
	PID       int           `json:"pid"` // the poly-lsp-mcp process
	Root      string        `json:"root"`
	UpdatedAt string        `json:"updatedAt"` // RFC 3339
	Children  []ChildStatus `json:"children"`
}

// statusPath is unique per manager: one process (the daemon) can hold a
// manager per root.
func (m *Manager) statusPath() string {
	sum := sha256.Sum256([]byte(m.startCwd))
	return filepath.Join(m.StatusDir, fmt.Sprintf("%d-%s.json", os.Getpid(), hex.EncodeToString(sum[:4])))
}

// writeStatus replaces this manager's status file atomically. Errors are
// ignored: status is for people, not for correctness.
func (m *Manager) writeStatus() {
	if m.StatusDir == "" {
		return
	}
	// Under statusMu, and not after Shutdown: a write racing removeStatus
	// would leave a file behind for a root the daemon already evicted.
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	if m.stopping() {
		return
	}
	b, err := json.MarshalIndent(StatusFile{
		PID:       os.Getpid(),
		Root:      m.startCwd,
		UpdatedAt: time.Now().Format(time.RFC3339),
		Children:  m.Status(),
	}, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(m.StatusDir, 0o700); err != nil {
		return
	}
	path := m.statusPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func (m *Manager) removeStatus() {
	if m.StatusDir == "" {
		return
	}
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	_ = os.Remove(m.statusPath())
}

// ReadStatusFiles reads every status file in dir whose process is still
// alive, and deletes the files of dead processes.
func ReadStatusFiles(dir string) ([]StatusFile, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []StatusFile
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var sf StatusFile
		if json.Unmarshal(b, &sf) != nil {
			continue
		}
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", sf.PID)); os.IsNotExist(err) {
			_ = os.Remove(p)
			continue
		}
		out = append(out, sf)
	}
	return out, nil
}
