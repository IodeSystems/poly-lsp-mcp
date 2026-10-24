package mcp

import (
	"path"
	"strings"
)

// includeSet is node_query's `include` argument: gitignore-style patterns
// naming dead ends (skipped dirs, nested repos, excluded dirs, shadowed
// files) the walk should enter anyway.
//
// Why a small matcher and not the git package's: that one answers "is this
// path ignored" against a repo's own ignore files. This one answers the
// reverse question for a list the caller typed, and needs one thing it lacks,
// onWay — a pattern like `.claude/worktrees/a1` must let the walk step
// through `.claude` to reach it.
//
// Semantics, as gitignore:
//   - no slash (`node_modules`, `*.js`): the name, at any depth;
//   - a leading or middle slash anchors to the root (`.claude/worktrees/a1`);
//   - `*` `?` `[..]` stay inside one segment, `**` spans segments;
//   - a trailing `/` matches directories only.
//
// A pattern matching a dir includes everything under it. Unlike git, a
// pattern never reaches THROUGH a left-out dir to something beside it: `*.js`
// does not open node_modules to find .js files. That would turn every
// include into a full walk of every dead end.
type includeSet struct{ pats []includePat }

type includePat struct {
	segs    []string
	dirOnly bool
}

// newIncludeSet compiles patterns; blanks are dropped. nil when none remain.
func newIncludeSet(patterns []string) *includeSet {
	var set includeSet
	for _, p := range patterns {
		p = strings.TrimPrefix(strings.TrimSpace(p), "./")
		ip := includePat{}
		if strings.HasSuffix(p, "/") {
			ip.dirOnly = true
			p = strings.TrimRight(p, "/")
		}
		anchored := strings.Contains(p, "/")
		p = strings.TrimLeft(p, "/")
		if p == "" {
			continue
		}
		ip.segs = strings.Split(p, "/")
		if !anchored {
			ip.segs = append([]string{"**"}, ip.segs...)
		}
		set.pats = append(set.pats, ip)
	}
	if len(set.pats) == 0 {
		return nil
	}
	return &set
}

// matches reports whether rel (root-relative, slash-separated) is named by a
// pattern itself. Everything under a matching dir is included by the caller.
func (s *includeSet) matches(rel string, isDir bool) bool {
	if s == nil {
		return false
	}
	segs := strings.Split(rel, "/")
	for _, p := range s.pats {
		if (isDir || !p.dirOnly) && matchSegs(p.segs, segs) {
			return true
		}
	}
	return false
}

// onWay reports whether dir rel lies on the route to something an anchored
// pattern names, so the walk steps in and takes only what leads there. A
// pattern that starts with ** (which includes every slash-free one) names no
// route: it would step into every dead end.
func (s *includeSet) onWay(rel string) bool {
	if s == nil {
		return false
	}
	segs := strings.Split(rel, "/")
	for _, p := range s.pats {
		if p.segs[0] != "**" && prefixMatches(p.segs, segs) {
			return true
		}
	}
	return false
}

// covers reports whether rel is, or sits under, a path a pattern names.
func (s *includeSet) covers(rel string) bool {
	if s == nil {
		return false
	}
	segs := strings.Split(rel, "/")
	for i := 1; i <= len(segs); i++ {
		if s.matches(strings.Join(segs[:i], "/"), i < len(segs)) {
			return true
		}
	}
	return false
}

// matchSegs matches whole path segments against pattern segments.
func matchSegs(p, r []string) bool {
	if len(p) == 0 {
		return len(r) == 0
	}
	if p[0] == "**" {
		for i := 0; i <= len(r); i++ {
			if matchSegs(p[1:], r[i:]) {
				return true
			}
		}
		return false
	}
	if len(r) == 0 {
		return false
	}
	// path.Match: `*` and `?` never cross a segment, `[..]` classes work.
	// A malformed pattern matches nothing.
	if ok, err := path.Match(p[0], r[0]); err != nil || !ok {
		return false
	}
	return matchSegs(p[1:], r[1:])
}

// prefixMatches reports whether r could be extended to a path p matches.
func prefixMatches(p, r []string) bool {
	if len(r) == 0 {
		return true
	}
	if len(p) == 0 {
		return false
	}
	if p[0] == "**" {
		return true
	}
	if ok, err := path.Match(p[0], r[0]); err != nil || !ok {
		return false
	}
	return prefixMatches(p[1:], r[1:])
}
