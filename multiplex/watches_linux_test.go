//go:build linux

package multiplex

import (
	"bufio"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// A python process makes an inotify instance with two watches and spawns a
// grandchild. The tree count from its pid must find the two watches.
func TestCountTreeWatchesLive(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	script := `
import ctypes, os, sys
libc = ctypes.CDLL(None, use_errno=True)
fd = libc.inotify_init1(0x80000)  # O_CLOEXEC: the grandchild must not share the instance
d = sys.argv[1]
for i in range(2):
    sub = os.path.join(d, "w%d" % i)
    os.mkdir(sub)
    assert libc.inotify_add_watch(fd, sub.encode(), 0x100) >= 0
import subprocess; subprocess.Popen(["sleep", "30"])  # returns after exec
print(os.getpid(), flush=True)
sys.stdin.read()
`
	cmd := exec.Command(py, "-c", script, t.TempDir())
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); cmd.Process.Kill(); cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Skipf("child did not start: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(line))
	wc, err := countTreeWatches(procRoot, pid)
	if err != nil {
		t.Fatal(err)
	}
	if wc.Total != 2 || wc.TopPid != pid || wc.TopCount != 2 {
		t.Fatalf("got %+v want total 2 in pid %d", wc, pid)
	}
	kids := childPids(procRoot, pid)
	if len(kids) == 0 {
		t.Fatal("grandchild not found")
	}
	if parentOf(procRoot, kids[0]) != pid {
		t.Fatal("parentOf mismatch")
	}
}
