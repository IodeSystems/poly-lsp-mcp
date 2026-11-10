package multiplex

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// spawnHelperEnv, set to a status dir, makes the test binary a stand-in
// daemon (spawnHelper) instead of running tests.
const spawnHelperEnv = "POLY_LSP_TEST_SPAWN_HELPER"

// spawnHelper records its children in dir and spawns two: "main", whose
// shell runs two background sleeps (grandchildren), and "thread", from a
// goroutine whose locked OS thread then exits. It prints
// "<main pid> <thread pid> <helper pid>", then blocks until killed.
func spawnHelper(dir string) {
	ReapOrphans(dir)
	lsp := &config.LSP{Cmd: "sh", Args: []string{"-c", "sleep 300 & sleep 300 & wait"}}
	main, err := Spawn("main", lsp, dir)
	if err != nil {
		fmt.Println("spawn:", err)
		os.Exit(2)
	}
	got := make(chan *Child)
	go func() {
		runtime.LockOSThread() // never unlocked: the thread exits with the goroutine
		c, err := Spawn("thread", &config.LSP{Cmd: "sleep", Args: []string{"300"}}, dir)
		if err != nil {
			fmt.Println("spawn:", err)
			os.Exit(2)
		}
		got <- c
	}()
	thread := <-got
	time.Sleep(300 * time.Millisecond) // the shell starts its sleeps
	noteStatusDir(dir)                 // records them as group members
	fmt.Println(main.Pid(), thread.Pid(), os.Getpid())
	select {}
}

func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := readProcIdent(pid); err == errNoProcess {
			return true
		}
		if id, err := readProcIdent(pid); err == nil && id.Exe == "" {
			return true // a zombie, not yet reaped by its new parent
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// sleeper starts `sleep 300`; it is killed at cleanup.
func sleeper(t *testing.T) (*exec.Cmd, procIdent) {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	id, err := readProcIdent(cmd.Process.Pid)
	if err != nil {
		t.Skip("no process identity here:", err)
	}
	return cmd, id
}

// deadOwner returns the identity of a process that has exited.
func deadOwner(t *testing.T) procIdent {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip(err)
	}
	return procIdent{PID: cmd.Process.Pid, Start: 1, Exe: "/gone"}
}

func writeRecord(t *testing.T, dir string, f childrenFile) string {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	p := childrenPath(dir, f.Owner.PID)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func exited(cmd *exec.Cmd, d time.Duration) bool {
	ch := make(chan struct{})
	go func() { _ = cmd.Wait(); close(ch) }()
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

func TestReapOrphansWrongStartTimeIsLeftAlone(t *testing.T) {
	cmd, id := sleeper(t)
	dir := t.TempDir()
	id.Start++
	p := writeRecord(t, dir, childrenFile{Owner: deadOwner(t), Children: []childRecord{{procIdent: id, Leader: true}}})
	if n := reapDir(dir); n != 0 {
		t.Errorf("reaped %d, want 0", n)
	}
	if exited(cmd, 200*time.Millisecond) {
		t.Fatal("a process with another start time was killed")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("record of a dead owner not removed: %v", err)
	}
}

func TestReapOrphansKillsMatchingChildOfDeadOwner(t *testing.T) {
	cmd, id := sleeper(t)
	dir := t.TempDir()
	p := writeRecord(t, dir, childrenFile{Owner: deadOwner(t), Children: []childRecord{{procIdent: id}}})
	if n := reapDir(dir); n != 1 {
		t.Errorf("reaped %d, want 1", n)
	}
	if !exited(cmd, 5*time.Second) {
		t.Fatal("orphan with matching identity not killed")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("record not removed: %v", err)
	}
}

// The takeover case: the owner (the old daemon) is alive.
func TestReapOrphansLiveOwnerIsLeftAlone(t *testing.T) {
	cmd, id := sleeper(t)
	owner, err := readProcIdent(os.Getppid())
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	p := writeRecord(t, dir, childrenFile{Owner: owner, Children: []childRecord{{procIdent: id, Leader: true}}})
	if n := reapDir(dir); n != 0 {
		t.Errorf("reaped %d, want 0", n)
	}
	if exited(cmd, 200*time.Millisecond) {
		t.Fatal("a live owner's child was killed")
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("live owner's record removed: %v", err)
	}
}

func TestDaemonKilledLeavesNoChildren(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Pdeathsig is Linux only")
	}
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(self)
	helper.Env = append(os.Environ(), spawnHelperEnv+"="+dir)
	out, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = helper.Process.Kill(); _ = helper.Wait() }()
	var mainPid, threadPid, helperPid int
	if _, err := fmt.Fscan(out, &mainPid, &threadPid, &helperPid); err != nil {
		t.Fatalf("helper output: %v", err)
	}
	b, err := os.ReadFile(childrenPath(dir, helperPid))
	if err != nil {
		t.Fatalf("no record written: %v", err)
	}
	var f childrenFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	var members []int
	for _, c := range f.Children {
		if !c.Leader {
			members = append(members, c.PID)
		}
	}
	if len(f.Children) != 4 || len(members) != 2 {
		t.Fatalf("record: want 2 leaders + 2 members, got %+v", f.Children)
	}
	// The spawning goroutine's thread is gone; its child must not be.
	if waitGone(threadPid, 200*time.Millisecond) {
		t.Fatal("child died with the thread of the goroutine that spawned it")
	}

	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	for _, pid := range []int{mainPid, threadPid} {
		if !waitGone(pid, 5*time.Second) {
			t.Errorf("child %d outlived the SIGKILLed daemon", pid)
		}
	}
	// Grandchildren get no Pdeathsig: the next start reaps them.
	for _, pid := range members {
		if waitGone(pid, 100*time.Millisecond) {
			t.Fatalf("grandchild %d died before the reap; the test proves nothing", pid)
		}
	}
	if n := reapDir(dir); n != 2 {
		t.Errorf("reaped %d, want 2", n)
	}
	for _, pid := range members {
		if !waitGone(pid, 5*time.Second) {
			t.Errorf("grandchild %d not reaped", pid)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d.children", helperPid))); !os.IsNotExist(err) {
		t.Errorf("record not removed: %v", err)
	}
}

// Kill takes the process group with the child.
func TestKillTakesTheGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads group members from /proc")
	}
	c, err := Spawn("group", &config.LSP{Cmd: "sh", Args: []string{"-c", "sleep 300 & wait"}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var members []int
	for i := 0; i < 50 && len(members) == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		members = groupMembers(c.Pid())
	}
	if len(members) != 1 {
		t.Fatalf("group members: %v", members)
	}
	_ = c.Kill()
	_ = c.Wait()
	if !waitGone(members[0], 5*time.Second) {
		t.Errorf("grandchild %d outlived Kill", members[0])
	}
}
