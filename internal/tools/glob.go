package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const maxGlobResults = 200 // cap matches so a broad pattern can't flood the context

// glob --------------------------------------------------------------------

type globTool struct{}

func (globTool) Name() string { return "glob" }
func (globTool) Description() string {
	return "Find files by path pattern (e.g. '**/*.go', 'internal/*/loop.go'). '*' matches within a path segment, '**' matches across directories. Returns paths, most-recently-modified first. Use this to locate files by name; use grep to search their contents."
}
func (globTool) Mutating() bool { return false }
func (globTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Glob pattern, e.g. '**/*.go' or 'cmd/**/main.go'."},"path":{"type":"string","description":"Directory to search under; defaults to '.'."}},"required":["pattern"]}`)
}

func (globTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if p.Pattern == "" {
		return "", errors.New("pattern is required")
	}
	root := p.Path
	if root == "" {
		root = "."
	}
	re, err := globToRegexp(p.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid pattern %q: %w", p.Pattern, err)
	}

	type hit struct {
		path string
		mod  int64
	}
	var hits []hit
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than aborting the whole walk
		}
		if d.IsDir() {
			// Don't descend into noise that's almost never the target.
			if n := d.Name(); path != root && (n == ".git" || n == "node_modules" || n == "vendor") {
				return fs.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		if !re.MatchString(filepath.ToSlash(rel)) {
			return nil
		}
		var mod int64
		if info, ierr := d.Info(); ierr == nil {
			mod = info.ModTime().UnixNano()
		}
		hits = append(hits, hit{path: filepath.ToSlash(path), mod: mod})
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	if len(hits) == 0 {
		return "(no matches)", nil
	}
	// Most-recently-modified first (ties broken by path for determinism).
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].mod != hits[j].mod {
			return hits[i].mod > hits[j].mod
		}
		return hits[i].path < hits[j].path
	})

	var b strings.Builder
	for i, h := range hits {
		if i >= maxGlobResults {
			fmt.Fprintf(&b, "… [%d more matches; refine the pattern]\n", len(hits)-maxGlobResults)
			break
		}
		b.WriteString(h.path)
		b.WriteByte('\n')
	}
	return truncate(b.String(), maxFileBytes), nil
}

// globToRegexp compiles a glob (with '**' crossing directory separators and '*'
// confined to one segment) into an anchored regexp matched against a forward-
// slash relative path.
func globToRegexp(pat string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pat); {
		c := pat[i]
		switch c {
		case '*':
			if i+1 < len(pat) && pat[i+1] == '*' {
				if i+2 < len(pat) && pat[i+2] == '/' {
					b.WriteString("(?:[^/]*/)*") // '**/': zero or more path segments
					i += 3
					continue
				}
				b.WriteString(".*") // trailing/standalone '**': anything, incl. separators
				i += 2
				continue
			}
			b.WriteString("[^/]*") // '*': within a single segment
			i++
		case '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
