package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// The update check behind `doctor`. Three build origins, one update path each
// (CLI-12):
//
//	release     built with -tags release → latest GitHub release tag; update by download
//	go install  no tag, no vcs.* settings (a proxy module zip has no VCS) → the
//	            module proxy's @latest; update by `go install …@latest`
//	source      vcs.revision present → compared against the checkout's upstream,
//	            but only when LSP_REBUILD_PATH names the checkout, because the
//	            binary deliberately carries no path (see selfupdate.go)
//
// It is advice, never a gate: offline, slow or confused, it says nothing (or
// the last answer, marked cached) and doctor's exit status is unaffected.

const (
	updateOriginRelease   = "release"
	updateOriginGoInstall = "go install"
	updateOriginSource    = "source"

	updateTimeout  = 2 * time.Second // per request, and per git fetch
	updateCacheTTL = 24 * time.Hour

	releaseLatestURL = "https://api.github.com/repos/IodeSystems/poly-lsp-mcp/releases/latest"
	proxyLatestURL   = "https://proxy.golang.org/github.com/iodesystems/poly-lsp-mcp/@latest"
	releaseAssetURL  = "https://github.com/IodeSystems/poly-lsp-mcp/releases/latest/download/poly-lsp-mcp_%s_%s.tar.gz"
	goInstallCmd     = "go install github.com/iodesystems/poly-lsp-mcp@latest"
)

// updateInfo is the `update` object of `doctor --json`. The field names are a
// contract with clients (dun); renaming one breaks them.
type updateInfo struct {
	Origin    string `json:"origin"`
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"` // omitted when it can't compare
	Available bool   `json:"available"`        // latest is strictly newer than current
	How       string `json:"how,omitempty"`    // only when available
	Note      string `json:"note,omitempty"`   // why it can't compare, or "cached Nh ago"
	CheckedAt string `json:"checkedAt"`        // RFC 3339
}

// text is the one doctor line, without the "update    " label.
func (u updateInfo) text() string {
	switch {
	case u.Available:
		return fmt.Sprintf("%s %s → %s available: %s", u.Origin, u.Current, u.Latest, u.How)
	case u.Latest != "" && u.Note == "":
		return fmt.Sprintf("up to date (%s, %s)", u.Current, u.Origin)
	case u.Latest != "":
		return fmt.Sprintf("up to date (%s, %s; %s)", u.Current, u.Origin, u.Note)
	default:
		return u.Origin + " " + u.Current + ": " + u.Note
	}
}

// buildOrigin is what a binary's build info says about where it came from.
type buildOrigin struct {
	Origin   string // "" when unknowable: a test binary, `go run`, no build info
	Current  string
	Modified bool   // source origin only: vcs.modified
	Note     string // set when the origin is known but nothing can be compared
}

// classifyBuild tells the three origins apart. release is the -tags release
// marker, passed in so tests can reach both values.
func classifyBuild(bi *debug.BuildInfo, release bool) buildOrigin {
	if bi == nil {
		return buildOrigin{}
	}
	var rev string
	var modified bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	ver := bi.Main.Version
	switch {
	case release:
		// A tagged checkout stamps Main.Version with the tag. An untagged
		// one (the workflow's dry run) stamps a pseudo-version, which still
		// compares correctly: it sorts below the tag it precedes.
		if _, ok := parseSemver(ver); !ok {
			return buildOrigin{Origin: updateOriginRelease, Current: orUnknown(ver), Note: "version is not a release tag: can't compare"}
		}
		return buildOrigin{Origin: updateOriginRelease, Current: ver}
	case rev != "":
		if len(rev) > 12 {
			rev = rev[:12]
		}
		if modified {
			return buildOrigin{Origin: updateOriginSource, Current: rev + "+dirty", Modified: true}
		}
		return buildOrigin{Origin: updateOriginSource, Current: rev}
	default:
		// `go install pkg@version` and a proxy module zip carry no VCS data.
		// An empty or (devel) version is a test binary or `go run`: no origin.
		if _, ok := parseSemver(ver); !ok {
			return buildOrigin{}
		}
		return buildOrigin{Origin: updateOriginGoInstall, Current: ver}
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// updateChecker holds what tests replace: URLs, client, cache file, clock.
type updateChecker struct {
	client       *http.Client
	releaseURL   string
	proxyURL     string
	cacheFile    string // "" disables the cache
	now          func() time.Time
	goos, goarch string
	rebuildPath  string // LSP_REBUILD_PATH, vetted; "" when unset
}

func newUpdateChecker() *updateChecker {
	c := &updateChecker{
		client:     &http.Client{Timeout: updateTimeout},
		releaseURL: releaseLatestURL,
		proxyURL:   proxyLatestURL,
		now:        time.Now,
		goos:       runtime.GOOS,
		goarch:     runtime.GOARCH,
	}
	if dir, err := os.UserCacheDir(); err == nil {
		c.cacheFile = filepath.Join(dir, "poly-lsp-mcp", "update-check.json")
	}
	c.rebuildPath, _ = rebuildDir()
	return c
}

// check returns nil when nothing is known: no origin, or offline with nothing
// cached.
func (c *updateChecker) check(bo buildOrigin) *updateInfo {
	if bo.Origin == "" {
		return nil
	}
	u := &updateInfo{Origin: bo.Origin, Current: bo.Current, CheckedAt: c.now().UTC().Format(time.RFC3339)}
	switch {
	case bo.Note != "":
		u.Note = bo.Note
		return u
	case bo.Modified:
		// A checkout under edit is ahead of anything upstream by definition.
		u.Note = "modified checkout: not checking"
		return u
	case bo.Origin == updateOriginSource && c.rebuildPath == "":
		u.Note = fmt.Sprintf("built from a checkout at %s; can't compare without its path (git pull && make install there)", bo.Current)
		return u
	}

	key := bo.Origin + "\x00" + bo.Current + "\x00" + c.rebuildPath
	cache := c.loadCache()
	if e, ok := cache[key]; ok {
		if at, err := time.Parse(time.RFC3339, e.CheckedAt); err == nil && c.now().Sub(at) < updateCacheTTL {
			return &e
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*updateTimeout)
	defer cancel()
	var err error
	switch bo.Origin {
	case updateOriginRelease:
		err = c.lookupRelease(ctx, u)
	case updateOriginGoInstall:
		err = c.lookupProxy(ctx, u)
	default:
		err = c.lookupUpstream(ctx, u)
	}
	if err != nil {
		// Offline, rate limited, or no upstream: say nothing new. An older
		// answer for this very build is still better than silence.
		if e, ok := cache[key]; ok {
			if at, perr := time.Parse(time.RFC3339, e.CheckedAt); perr == nil {
				e.Note = fmt.Sprintf("cached %s ago", roundAge(c.now().Sub(at)))
				return &e
			}
		}
		return nil
	}
	if cache == nil {
		cache = map[string]updateInfo{}
	}
	cache[key] = *u
	c.saveCache(cache)
	return u
}

func roundAge(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// compare fills Latest/Available/How from a semver latest.
func (u *updateInfo) compare(latest, how string) {
	newer, ok := semverNewer(latest, u.Current)
	if !ok {
		u.Note = "can't compare " + u.Current + " with " + latest // no latest we can't relate
		return
	}
	u.Latest = latest
	if newer {
		u.Available = true
		u.How = how
	}
}

func (c *updateChecker) getJSON(ctx context.Context, url string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "poly-lsp-mcp-doctor")
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into)
}

func (c *updateChecker) lookupRelease(ctx context.Context, u *updateInfo) error {
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := c.getJSON(ctx, c.releaseURL, &r); err != nil {
		return err
	}
	if r.TagName == "" {
		return fmt.Errorf("release has no tag_name")
	}
	// The tarball's sha256 is in SHA256SUMS beside it on the release page.
	u.compare(r.TagName, fmt.Sprintf(releaseAssetURL, c.goos, c.goarch))
	return nil
}

func (c *updateChecker) lookupProxy(ctx context.Context, u *updateInfo) error {
	var r struct {
		Version string
	}
	if err := c.getJSON(ctx, c.proxyURL, &r); err != nil {
		return err
	}
	if r.Version == "" {
		return fmt.Errorf("proxy answer has no Version")
	}
	u.compare(r.Version, goInstallCmd)
	return nil
}

// lookupUpstream asks the named checkout how far behind its upstream it is.
// The fetch is the only network step and is bounded like the HTTP ones; a
// failed fetch is an offline answer, not a stale comparison.
func (c *updateChecker) lookupUpstream(ctx context.Context, u *updateInfo) error {
	dir := c.rebuildPath
	fctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	if err := exec.CommandContext(fctx, "git", "-C", dir, "fetch", "--quiet").Run(); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-list", "--count", "HEAD..@{upstream}").Output()
	if err != nil {
		u.Note = "checkout at " + dir + " has no upstream to compare with"
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return err
	}
	if n == 0 {
		u.Latest = "upstream"
		return nil
	}
	u.Latest = fmt.Sprintf("upstream (+%d)", n)
	u.Available = true
	u.How = fmt.Sprintf("git -C %s pull && make -C %s install", dir, dir)
	return nil
}

func (c *updateChecker) loadCache() map[string]updateInfo {
	if c.cacheFile == "" {
		return nil
	}
	b, err := os.ReadFile(c.cacheFile)
	if err != nil {
		return nil
	}
	var m map[string]updateInfo
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// saveCache is best effort: a read-only cache dir costs a network call per
// run, nothing else. Temp name, then rename, so a concurrent doctor never
// reads half a file.
func (c *updateChecker) saveCache(m map[string]updateInfo) {
	if c.cacheFile == "" {
		return
	}
	b, err := json.Marshal(m)
	if err != nil || os.MkdirAll(filepath.Dir(c.cacheFile), 0o755) != nil {
		return
	}
	tmp := fmt.Sprintf("%s.%d.tmp", c.cacheFile, os.Getpid())
	if os.WriteFile(tmp, b, 0o644) != nil {
		return
	}
	if os.Rename(tmp, c.cacheFile) != nil {
		os.Remove(tmp)
	}
}

// semver is vMAJOR.MINOR.PATCH[-pre][+build]. Pseudo-versions
// (v0.0.0-2026…-abc, v1.2.4-0.2026…-abc) are ordinary prereleases, so the
// standard ordering already places them correctly.
type semver struct {
	maj, min, pat int
	pre           []string
}

func parseSemver(v string) (semver, bool) {
	var s semver
	if !strings.HasPrefix(v, "v") {
		return s, false
	}
	v, _, _ = strings.Cut(v[1:], "+") // build metadata never orders
	core, pre, hasPre := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return s, false
	}
	nums := [3]int{}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') {
			return s, false
		}
		nums[i] = n
	}
	s.maj, s.min, s.pat = nums[0], nums[1], nums[2]
	if hasPre {
		if pre == "" {
			return s, false
		}
		s.pre = strings.Split(pre, ".")
	}
	return s, true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// cmp returns -1, 0, 1 per semver precedence.
func (a semver) cmp(b semver) int {
	for _, c := range []int{cmpInt(a.maj, b.maj), cmpInt(a.min, b.min), cmpInt(a.pat, b.pat)} {
		if c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1 // a release outranks its prereleases
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		switch {
		case xerr == nil && yerr == nil:
			if c := cmpInt(xn, yn); c != 0 {
				return c
			}
		case xerr == nil:
			return -1 // numeric identifiers sort below alphanumeric
		case yerr == nil:
			return 1
		case x != y:
			return strings.Compare(x, y)
		}
	}
	return cmpInt(len(a.pre), len(b.pre))
}

// semverNewer reports whether latest is strictly newer than current; ok is
// false when either is not a semver.
func semverNewer(latest, current string) (newer, ok bool) {
	l, lok := parseSemver(latest)
	c, cok := parseSemver(current)
	if !lok || !cok {
		return false, false
	}
	return l.cmp(c) > 0, true
}
