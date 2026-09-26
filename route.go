package prometheus

import (
	"slices"
	"strings"
)

// matcher turns a request path into a route label. It is built once at Init and
// replaced, never changed, when a document is learned, so HTTPResponse reads it
// without a lock.
type matcher struct {
	// locales are the supported locales; a leading segment naming one is the
	// router's locale prefix, not part of any pattern.
	locales []string
	// exact is the metrics path, labelled as itself.
	exact    string
	prefixes []string
	patterns []pattern
	names    map[string]bool
}

type segKind int

// Ordered so that sorting by kind puts the most specific pattern first, as the
// router prefers a literal segment over a parameter, and a parameter over a
// catch-all.
const (
	segStatic segKind = iota
	segParam
	segCatchAll
)

type segment struct {
	kind segKind
	text string
}

type pattern struct {
	segments []segment
	label    string
}

func (m *matcher) clone() *matcher {
	next := &matcher{
		locales:  m.locales,
		exact:    m.exact,
		prefixes: m.prefixes,
		patterns: slices.Clone(m.patterns),
		names:    make(map[string]bool, len(m.names)+1),
	}
	for name := range m.names {
		next.names[name] = true
	}
	return next
}

func (m *matcher) add(raw, label string) {
	if m.names == nil {
		m.names = map[string]bool{}
	}
	m.names[label] = true
	var segs []segment
	for _, s := range split(m.stripLocale(raw)) {
		switch {
		case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "...}"):
			segs = append(segs, segment{kind: segCatchAll})
		case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"):
			segs = append(segs, segment{kind: segParam})
		default:
			segs = append(segs, segment{kind: segStatic, text: s})
		}
	}
	m.patterns = append(m.patterns, pattern{segments: segs, label: label})
}

func (m *matcher) sort() {
	slices.SortStableFunc(m.patterns, func(a, b pattern) int {
		for i := 0; i < len(a.segments) && i < len(b.segments); i++ {
			if a.segments[i].kind != b.segments[i].kind {
				return int(a.segments[i].kind) - int(b.segments[i].kind)
			}
		}
		return len(b.segments) - len(a.segments)
	})
	// The longest prefix wins, so "/api/v2/" is not reported as "/api/".
	slices.SortStableFunc(m.prefixes, func(a, b string) int { return len(b) - len(a) })
}

func (m *matcher) match(path string) string {
	if m.exact != "" && path == m.exact {
		return path
	}
	for _, prefix := range m.prefixes {
		if strings.HasPrefix(path, prefix) {
			return prefix
		}
	}
	segs := split(m.stripLocale(path))
	for _, p := range m.patterns {
		if p.matches(segs) {
			return p.label
		}
	}
	return OtherRoute
}

func (p pattern) matches(segs []string) bool {
	for i, s := range p.segments {
		if s.kind == segCatchAll {
			return len(segs) > i
		}
		if i >= len(segs) || (s.kind == segStatic && s.text != segs[i]) {
			return false
		}
	}
	return len(segs) == len(p.segments)
}

func (m *matcher) stripLocale(path string) string {
	rest := strings.TrimPrefix(path, "/")
	first, after, _ := strings.Cut(rest, "/")
	if first != "" && slices.Contains(m.locales, first) {
		return "/" + after
	}
	return path
}

// split breaks a path into its segments, ignoring leading, trailing and doubled
// slashes, as the router's trailing-slash equivalence does.
func split(path string) []string {
	return strings.FieldsFunc(path, func(r rune) bool { return r == '/' })
}
