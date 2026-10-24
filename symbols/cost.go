package symbols

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BuildCost records what each directory cost a Build, so the client that
// owns the workspace can decide what to ignore.
//
// poly-lsp indexes everything git does not ignore and leaves the choice of
// what to ignore to the client. That choice needs numbers: on dun
// (2026-10-24) 48 agent worktrees under .claude/worktrees/ were 20,708 of
// 22,012 indexed files, and MCP initialize took 52 s, but nothing said so —
// the client saw a slow handshake, not the directory that caused it.
//
// Build is sequential, so BuildCost takes no lock. Read it after Build
// returns.
type BuildCost struct {
	root   string
	start  time.Time
	wall   time.Duration
	dirs   map[string]*dirCost // repo-relative, slash-separated; "" = root
	nested map[string]bool     // directories with their own .git
}

type dirCost struct {
	files  int
	bytes  int64
	cost   time.Duration
	cached int
}

// NewBuildCost returns an empty recorder for WithCost.
func NewBuildCost() *BuildCost {
	return &BuildCost{dirs: map[string]*dirCost{}, nested: map[string]bool{}}
}

// WithCost records per-directory cost into c.
func WithCost(c *BuildCost) BuildOption {
	return func(cfg *buildConfig) { cfg.cost = c }
}

func (c *BuildCost) begin(root string) {
	c.root = root
	c.start = time.Now()
}

func (c *BuildCost) end() { c.wall = time.Since(c.start) }

func (c *BuildCost) rel(p string) string {
	r, err := filepath.Rel(c.root, p)
	if err != nil || r == "." {
		return ""
	}
	return filepath.ToSlash(r)
}

// dir notes a walked directory that holds its own .git: another repository
// (a worktree, a clone, a submodule) indexed as part of this one.
func (c *BuildCost) dir(p string) {
	if p == c.root {
		return
	}
	if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
		c.nested[c.rel(p)] = true
	}
}

func (c *BuildCost) file(p string, size int64, cost time.Duration, cached bool) {
	d := path.Dir(c.rel(p))
	if d == "." {
		d = ""
	}
	dc := c.dirs[d]
	if dc == nil {
		dc = &dirCost{}
		c.dirs[d] = dc
	}
	dc.files++
	dc.bytes += size
	dc.cost += cost
	if cached {
		dc.cached++
	}
}

// CostReport is the summary a client reads: totals, and the directories
// that dominate the build.
type CostReport struct {
	Files    int       `json:"files"`
	Bytes    int64     `json:"bytes"`
	Cached   int       `json:"cached"`   // files answered from the parse cache
	WallMs   int64     `json:"wallMs"`   // the whole walk
	Heaviest []DirCost `json:"heaviest"` // by cost, at most 10
	Hint     string    `json:"hint,omitempty"`
}

// DirCost is one directory's share of the build, its subdirectories included.
type DirCost struct {
	Dir             string  `json:"dir"` // repo-relative, ends in "/"
	Files           int     `json:"files"`
	Bytes           int64   `json:"bytes"`
	CostMs          int64   `json:"costMs"` // this build's time on its files: read, parse, index
	Share           float64 `json:"share"`  // of the indexed bytes, 0–1
	NestedCheckouts int     `json:"nestedCheckouts,omitempty"`
}

// heaviestShare is the smallest share of the indexed bytes a directory needs
// to be listed; preferChild is the share of a parent's bytes one child must
// hold for the child to be listed in its place, since the child is the
// narrower thing to ignore.
//
// Shares are of BYTES, not time. Time depends on the parse cache: of three
// identical worktrees, the first one walked parses and the other two hit the
// cache, so by time they read 74%, 9% and 8% of the build. Bytes are what a
// cold build — a new machine, a cleared cache, a branch switch — will parse.
const (
	heaviestShare = 0.05
	preferChild   = 0.8
	heaviestMax   = 10
)

// Report rolls each directory's figures up into its ancestors and picks the
// directories worth naming: the widest ones over heaviestShare, each
// replaced by its child when one child holds nearly all of it, and none
// listed inside another.
func (c *BuildCost) Report() CostReport {
	type roll struct {
		files, nested int
		bytes         int64
		cost          time.Duration
	}
	rolls := map[string]*roll{}
	get := func(d string) *roll {
		r := rolls[d]
		if r == nil {
			r = &roll{}
			rolls[d] = r
		}
		return r
	}
	var total dirCost
	for d, dc := range c.dirs {
		total.files += dc.files
		total.bytes += dc.bytes
		total.cost += dc.cost
		total.cached += dc.cached
		for _, a := range ancestors(d) {
			r := get(a)
			r.files += dc.files
			r.bytes += dc.bytes
			r.cost += dc.cost
		}
	}
	for n := range c.nested {
		for _, a := range ancestors(n) {
			get(a).nested++
		}
	}

	rep := CostReport{
		Files:  total.files,
		Bytes:  total.bytes,
		Cached: total.cached,
		WallMs: c.wall.Milliseconds(),
	}
	if total.bytes == 0 {
		return rep
	}
	share := func(r *roll) float64 { return float64(r.bytes) / float64(total.bytes) }
	keep := map[string]bool{}
	for d, r := range rolls {
		if share(r) < heaviestShare {
			continue
		}
		narrower := false
		for d2, r2 := range rolls {
			if path.Dir(d2) == d && float64(r2.bytes) >= preferChild*float64(r.bytes) {
				narrower = true
				break
			}
		}
		if !narrower {
			keep[d] = true
		}
	}
	var out []DirCost
	for d := range keep {
		inside := false
		for _, a := range ancestors(path.Dir(d)) {
			if keep[a] {
				inside = true
				break
			}
		}
		if inside {
			continue
		}
		r := rolls[d]
		out = append(out, DirCost{
			Dir:             d + "/",
			Files:           r.files,
			Bytes:           r.bytes,
			CostMs:          r.cost.Milliseconds(),
			Share:           float64(int(share(r)*1000)) / 1000,
			NestedCheckouts: r.nested,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return out[i].Dir < out[j].Dir
	})
	if len(out) > heaviestMax {
		out = out[:heaviestMax]
	}
	rep.Heaviest = out
	for _, d := range out {
		if d.NestedCheckouts > 0 {
			rep.Hint = "a directory with its own .git is another repository indexed as this one; " + dropHint
			break
		}
	}
	if rep.Hint == "" && len(out) > 0 {
		rep.Hint = dropHint
	}
	return rep
}

// dropHint names both ways to drop a directory: the client's, which leaves
// the repository alone, and the repository's own.
const dropHint = "to drop a directory from the index, pass --exclude <dir> or add it to .gitignore"

// ancestors returns d and every directory above it, excluding the root.
func ancestors(d string) []string {
	var out []string
	for d != "" && d != "." && d != "/" {
		out = append(out, d)
		d = path.Dir(d)
		if d == "." {
			break
		}
	}
	return out
}

// Text renders the report for a person.
func (r CostReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "index: %d files, %s, %s walk, %d from the parse cache\n",
		r.Files, humanBytes(r.Bytes), time.Duration(r.WallMs)*time.Millisecond, r.Cached)
	if len(r.Heaviest) == 0 {
		return b.String()
	}
	fmt.Fprintf(&b, "heaviest directories (>= %d%% of indexed bytes):\n", int(heaviestShare*100))
	for _, d := range r.Heaviest {
		fmt.Fprintf(&b, "  %3.0f%%  %-24s %6d files  %9s  %8s",
			d.Share*100, d.Dir, d.Files, humanBytes(d.Bytes), time.Duration(d.CostMs)*time.Millisecond)
		if d.NestedCheckouts > 0 {
			fmt.Fprintf(&b, "  %d nested checkout(s)", d.NestedCheckouts)
		}
		b.WriteByte('\n')
	}
	if r.Hint != "" {
		b.WriteString(r.Hint + "\n")
	}
	return b.String()
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
