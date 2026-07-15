// Package trust records, per working directory, how much filesystem access the
// user has granted borg's editing tools. The boundary is enforced by the file
// tools (write_file/edit_file): edits outside the trusted root are refused. Reads
// stay unrestricted (read-only, low risk) and bash is permission-gated — an
// arbitrary shell command can't be meaningfully path-scoped.
package trust

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Scope is how far edits may reach from the working directory.
type Scope string

const (
	ScopeDir    Scope = "dir"    // edits allowed under the working directory only
	ScopeParent Scope = "parent" // edits allowed under its parent directory too
)

// Root returns the directory edits are confined to for dir at the given scope.
func Root(dir string, scope Scope) string {
	if scope == ScopeParent {
		return filepath.Dir(dir)
	}
	return dir
}

type file struct {
	Dirs map[string]Scope `json:"dirs"`
}

func storePath() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "borg", "trust.json"), nil
}

func load() file {
	f := file{Dirs: map[string]Scope{}}
	p, err := storePath()
	if err != nil {
		return f
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return f
	}
	if json.Unmarshal(b, &f) != nil || f.Dirs == nil {
		f.Dirs = map[string]Scope{}
	}
	return f
}

// Lookup returns the recorded scope for dir and whether a decision exists.
func Lookup(dir string) (Scope, bool) {
	s, ok := load().Dirs[dir]
	return s, ok
}

// Record persists the scope granted to dir (store at 0600, dir at 0700).
func Record(dir string, scope Scope) error {
	f := load()
	f.Dirs[dir] = scope
	p, err := storePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}
