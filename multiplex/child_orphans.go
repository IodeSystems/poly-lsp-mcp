package multiplex

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// A child LSP must not outlive the process that started it. On Linux the
// kernel kills a child when its parent dies (Pdeathsig, child_linux.go),
// but not the child's own children. Everywhere else nothing does. So each
// process records the children it runs, by identity, in
// <StatusDir>/<pid>.children, and a later process kills the children of
// a record whose owner is gone (ReapOrphans).
//
// Identity is pid + start time + executable: a pid reused by an unrelated
// process has another start time, and is never signalled. A record whose
// owner is alive (the old daemon during a takeover) is never touched.

// errNoProcess: readProcIdent found no process with that pid.
var errNoProcess = errors.New("no such process")

// procIdent identifies one process. Start is in a platform unit (Linux:
// clock ticks since boot; darwin: microseconds since the epoch) and is
// compared, never converted. Exe is /proc/<pid>/exe on Linux, p_comm on
// darwin. Boot is the Linux boot id: tick counts restart at each boot.
type procIdent struct {
	PID   int    `json:"pid"`
	Start uint64 `json:"start"`
	Boot  string `json:"boot,omitempty"`
	Exe   string `json:"exe"`
}

// childRecord is one recorded process. Leader is true for a child this
// process started (in its own process group, pgid = pid); false for a
// member of such a group, seen at the last record write.
type childRecord struct {
	procIdent
	Name   string `json:"name,omitempty"`
	Leader bool   `json:"leader,omitempty"`
}

// childrenFile is the content of <StatusDir>/<pid>.children.
type childrenFile struct {
	Owner     procIdent     `json:"owner"`
	UpdatedAt string        `json:"updatedAt"` // RFC 3339
	Children  []childRecord `json:"children"`
}

// reapEvery bounds how often writeStatus looks for orphans: a daemon that
// took over from one that died later still cleans up after it.
const reapEvery = 30 * time.Second

var rec struct {
	mu       sync.Mutex
	dir      string
	live     map[*Child]struct{}
	lastReap time.Time
}

func childrenPath(dir string, pid int) string {
	return filepath.Join(dir, fmt.Sprintf("%d.children", pid))
}

// trackChild records c, just started.
func trackChild(c *Child) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.live == nil {
		rec.live = map[*Child]struct{}{}
	}
	rec.live[c] = struct{}{}
	writeChildrenLocked()
}

// untrackChild drops c, reaped.
func untrackChild(c *Child) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if _, ok := rec.live[c]; !ok {
		return
	}
	delete(rec.live, c)
	writeChildrenLocked()
}

// noteStatusDir is writeStatus's hook: it makes dir the record directory
// if none is set (reaping it the first time), rewrites the record with
// the groups' current members, and reaps dir again every reapEvery.
func noteStatusDir(dir string) {
	rec.mu.Lock()
	first := rec.dir == ""
	if !first {
		writeChildrenLocked()
	}
	due := first || dir == rec.dir && time.Since(rec.lastReap) >= reapEvery
	rec.mu.Unlock()
	if due {
		ReapOrphans(dir) // sets the directory and writes, if first
	}
}

// writeChildrenLocked replaces this process's record; with no live child,
// it removes it. Errors are ignored: the record helps, it is not required.
func writeChildrenLocked() {
	if rec.dir == "" {
		return
	}
	path := childrenPath(rec.dir, os.Getpid())
	if len(rec.live) == 0 {
		_ = os.Remove(path)
		return
	}
	owner, _ := readProcIdent(os.Getpid())
	f := childrenFile{Owner: owner, UpdatedAt: time.Now().Format(time.RFC3339)}
	f.Children = []childRecord{}
	for c := range rec.live {
		if c.ident.PID == 0 {
			continue
		}
		// Re-read: a launcher that execs its server (a shell script, a
		// node shim) has another executable now. Same start time: the
		// pid is still the child's, unreaped while it is in rec.live.
		lead := c.ident
		if id, err := readProcIdent(lead.PID); err == nil && id.Start == lead.Start && id.Exe != "" {
			lead = id
		}
		f.Children = append(f.Children, childRecord{procIdent: lead, Name: c.name, Leader: true})
		for _, pid := range groupMembers(c.ident.PID) {
			if id, err := readProcIdent(pid); err == nil {
				f.Children = append(f.Children, childRecord{procIdent: id, Name: c.name})
			}
		}
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// ReapOrphans makes dir this process's record directory, if none is set,
// and kills the recorded children of every record in dir whose owner is
// gone: SIGKILL to each process whose pid, start time and executable
// still match, and to the process group of each such leader. It removes
// those records. A record whose owner is alive, or cannot be checked, is
// left alone. Returns how many processes were signalled.
func ReapOrphans(dir string) int {
	if dir == "" {
		return 0
	}
	rec.mu.Lock()
	if rec.dir == "" {
		rec.dir = dir
		// Once, here: a write never recreates a directory removed later.
		_ = os.MkdirAll(dir, 0o700)
		writeChildrenLocked()
	}
	if dir == rec.dir {
		rec.lastReap = time.Now()
	}
	rec.mu.Unlock()
	return reapDir(dir)
}

// reapDir is ReapOrphans without making dir the record directory.
func reapDir(dir string) int {
	paths, err := filepath.Glob(filepath.Join(dir, "*.children"))
	if err != nil {
		return 0
	}
	n := 0
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var f childrenFile
		if json.Unmarshal(b, &f) != nil || f.Owner.PID == os.Getpid() || ownerAlive(f.Owner) {
			continue
		}
		for _, c := range f.Children {
			if !sameProcess(c.procIdent) {
				continue
			}
			if c.Leader {
				if pgid, err := syscall.Getpgid(c.PID); err == nil && pgid == c.PID {
					_ = syscall.Kill(-c.PID, syscall.SIGKILL)
				}
			}
			if syscall.Kill(c.PID, syscall.SIGKILL) == nil {
				n++
				log.Printf("multiplex: reaped orphan %s (pid %d) of dead pid %d", c.Name, c.PID, f.Owner.PID)
			}
		}
		_ = os.Remove(p)
	}
	return n
}

// ownerAlive reports whether the record's owner may still run. Only a
// definite no is false: the pid is gone, or now has another start time.
func ownerAlive(owner procIdent) bool {
	if owner.PID <= 0 || owner.Start == 0 {
		return true // written where identity is unknown: never reap
	}
	id, err := readProcIdent(owner.PID)
	if errors.Is(err, errNoProcess) {
		return false
	}
	if err != nil {
		return true
	}
	return id.Start == owner.Start && id.Boot == owner.Boot
}

// sameProcess reports whether want's pid is still the process recorded:
// same start time, boot and executable. An unknown field never matches.
func sameProcess(want procIdent) bool {
	if want.PID <= 1 || want.Start == 0 || want.Exe == "" {
		return false
	}
	id, err := readProcIdent(want.PID)
	return err == nil && id.Start == want.Start && id.Boot == want.Boot && id.Exe == want.Exe
}
