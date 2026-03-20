package filter

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

type Excluder struct {
	globs []string
	rexes []*regexp.Regexp
}

func NewExcluder(globs []string, regexes []string) (*Excluder, error) {
	e := &Excluder{globs: make([]string, 0, len(globs))}
	for _, g := range globs {
		if strings.TrimSpace(g) == "" {
			continue
		}
		e.globs = append(e.globs, filepath.Clean(g))
	}
	for _, r := range regexes {
		if strings.TrimSpace(r) == "" {
			continue
		}
		rx, err := regexp.Compile(r)
		if err != nil {
			return nil, fmt.Errorf("compile regex %q: %w", r, err)
		}
		e.rexes = append(e.rexes, rx)
	}
	return e, nil
}

func (e *Excluder) Match(path string) (bool, string) {
	if e == nil {
		return false, ""
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	for _, g := range e.globs {
		gm := filepath.ToSlash(g)
		ok, err := filepath.Match(gm, clean)
		if err == nil && ok {
			return true, "glob:" + g
		}
		if strings.HasSuffix(gm, "/**") {
			prefix := strings.TrimSuffix(gm, "**")
			if strings.HasPrefix(clean, strings.TrimSuffix(prefix, "/")) {
				return true, "glob:" + g
			}
		}
	}
	for _, rx := range e.rexes {
		if rx.MatchString(clean) {
			return true, "regex:" + rx.String()
		}
	}
	return false, ""
}
