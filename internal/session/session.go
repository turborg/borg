// Package session persists REPL conversations so they can be resumed later.
// Each session is one JSON file under ~/.config/borg/sessions/<id>.json (0600,
// in a 0700 dir), mirroring internal/auth's credential store conventions.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/turborg/borg/internal/llm"
)

// Session is a saved conversation: the agent's full message history plus the
// settings and timestamps needed to resume and list it.
type Session struct {
	ID         string    `json:"id"`
	Created    time.Time `json:"created"`
	LastActive time.Time `json:"last_active"`
	Name       string    `json:"name,omitempty"` // short, FIXED title (set once from the first prompt; never changes)
	Model      string    `json:"model"`
	Think      bool      `json:"think"`
	Effort     string    `json:"effort,omitempty"` // explicit reasoning_effort ("" = follow think)
	Dir        string    `json:"dir,omitempty"`    // working directory the session was started in
	// Cumulative token usage for this conversation, so the REPL footer shows the
	// running ↑in/↓out totals across resumes (not a fresh 0 each attach).
	TokensIn  int `json:"tokens_in,omitempty"`
	TokensOut int `json:"tokens_out,omitempty"`
	// ContextTokens is the last measured context-window occupancy (the most recent
	// step's prompt tokens), so /context shows an exact figure right after attach
	// instead of an estimate until the next turn.
	ContextTokens int           `json:"context_tokens,omitempty"`
	Messages      []llm.Message `json:"messages"`
}

// Meta is the listing view of a session (no message bodies).
type Meta struct {
	ID         string
	Name       string // the session's fixed title (falls back to the first prompt for legacy sessions)
	LastActive time.Time
	Dir        string
	Preview    string
}

// New mints a fresh session with a random id, now() timestamps, and the current
// working directory (so sessions can be attached by directory, not just id).
func New(model string, think bool) *Session {
	now := time.Now()
	return &Session{ID: newID(), Created: now, LastActive: now, Model: model, Think: think, Dir: cwd()}
}

// cwd returns the current working directory, or "" if it can't be determined.
func cwd() string {
	d, err := os.Getwd()
	if err != nil {
		return ""
	}
	return d
}

// newID returns 8 hex chars (4 random bytes) — short enough to type, long
// enough to disambiguate by prefix.
func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Save writes the session, stamping LastActive=now, via the default store.
func Save(s *Session) error { return defaultStore().save(s) }

// Load resolves an exact id or a unique id prefix from the default store.
func Load(idOrPrefix string) (*Session, error) { return defaultStore().load(idOrPrefix) }

// List returns saved sessions newest-first from the default store.
func List() ([]Meta, error) { return defaultStore().list() }

// ListForDir returns saved sessions started in dir, newest-first — the listing
// for "this directory's sessions" (CLI `borg sessions`, REPL `/sessions`).
func ListForDir(dir string) ([]Meta, error) { return defaultStore().listForDir(dir) }

// LatestForDir returns the most-recently-active session started in dir, or an
// error if none exists — the basis for attaching by directory (no id needed).
func LatestForDir(dir string) (*Session, error) { return defaultStore().latestForDir(dir) }

// Latest returns the most-recently-active saved session across every directory,
// or an error if none exist — the basis for `borg --resume`.
func Latest() (*Session, error) { return defaultStore().latest() }

func (s store) latest() (*Session, error) {
	metas, err := s.list() // newest-first
	if err != nil {
		return nil, err
	}
	if len(metas) == 0 {
		return nil, fmt.Errorf("no saved sessions yet")
	}
	return s.read(metas[0].ID)
}

// CurrentDir returns the process's working directory (or "" if unavailable),
// exposed so callers can resolve "this directory's session".
func CurrentDir() string { return cwd() }

func (s store) latestForDir(dir string) (*Session, error) {
	metas, err := s.list() // newest-first
	if err != nil {
		return nil, err
	}
	for _, m := range metas {
		if m.Dir == dir {
			return s.read(m.ID)
		}
	}
	return nil, fmt.Errorf("no saved session for directory %s", dir)
}

// Purge removes every saved session and returns how many were deleted.
func Purge() (int, error) { return defaultStore().purge() }

// store reads/writes sessions under a directory.
type store struct{ dir string }

func defaultStore() store {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "." // os.UserConfigDir only fails when HOME is unset; degrade locally
	}
	return store{dir: filepath.Join(dir, "borg", "sessions")}
}

func (s store) path(id string) string { return filepath.Join(s.dir, id+".json") }

func (s store) save(sess *Session) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	sess.LastActive = time.Now()
	// Title the session ONCE, the first time it has a prompt to summarize. After
	// that Name is sticky — it's a stable label, not a live preview, so it doesn't
	// churn as the conversation grows or after /clear.
	if sess.Name == "" {
		sess.Name = deriveName(sess)
	}
	b, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path(sess.ID), b, 0o600)
}

func (s store) read(id string) (*Session, error) {
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(b, &sess); err != nil {
		return nil, fmt.Errorf("parse session %s: %w", id, err)
	}
	return &sess, nil
}

func (s store) load(idOrPrefix string) (*Session, error) {
	if idOrPrefix == "" {
		return nil, fmt.Errorf("no session id given")
	}
	// Exact id first.
	if sess, err := s.read(idOrPrefix); err == nil {
		return sess, nil
	}
	// Otherwise resolve by unique prefix.
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	var matches []string
	for _, id := range ids {
		if strings.HasPrefix(id, idOrPrefix) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no session matching %q (try `borg sessions`)", idOrPrefix)
	case 1:
		return s.read(matches[0])
	default:
		return nil, fmt.Errorf("%q is ambiguous: matches %s", idOrPrefix, strings.Join(matches, ", "))
	}
}

// ids returns the session ids (filenames without .json) in the store dir.
func (s store) ids() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no sessions yet
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if name := e.Name(); !e.IsDir() && strings.HasSuffix(name, ".json") {
			ids = append(ids, strings.TrimSuffix(name, ".json"))
		}
	}
	return ids, nil
}

func (s store) list() ([]Meta, error) {
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	var metas []Meta
	for _, id := range ids {
		sess, err := s.read(id)
		if err != nil {
			continue // skip unreadable/corrupt files rather than failing the listing
		}
		// Legacy sessions saved before titling existed have no Name — derive one
		// for display (it persists on the session's next save).
		name := sess.Name
		if name == "" {
			name = deriveName(sess)
		}
		metas = append(metas, Meta{ID: sess.ID, Name: name, LastActive: sess.LastActive, Dir: sess.Dir, Preview: preview(sess)})
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].LastActive.After(metas[j].LastActive) })
	return metas, nil
}

func (s store) listForDir(dir string) ([]Meta, error) {
	metas, err := s.list() // newest-first
	if err != nil {
		return nil, err
	}
	out := make([]Meta, 0, len(metas))
	for _, m := range metas {
		if m.Dir == dir {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s store) purge() (int, error) {
	ids, err := s.ids()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if err := os.Remove(s.path(id)); err != nil && !os.IsNotExist(err) {
			return n, err
		}
		n++
	}
	return n, nil
}

// deriveName builds a short, fixed title from the first user prompt — a quick,
// deterministic summary of what the session is about. It's a heuristic (no
// metered LLM call): the first prompt, whitespace-collapsed and trimmed to a few
// words. "" when there's no prompt yet (so titling waits for one).
func deriveName(s *Session) string {
	for _, m := range s.Messages {
		if m.Role != "user" {
			continue
		}
		title := strings.Join(strings.Fields(m.Content), " ")
		if title == "" {
			continue
		}
		const maxName = 48
		if r := []rune(title); len(r) > maxName {
			title = strings.TrimSpace(string(r[:maxName])) + "…"
		}
		return title
	}
	return ""
}

// Ago renders how long ago t was, compactly ("just now", "3 minutes ago", "5
// hours ago", "2 days ago") — so a listing reads "when" at a glance, not just a
// bare date.
func Ago(t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour")
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day")
	case d < 365*24*time.Hour:
		return plural(int(d.Hours()/(24*30)), "month")
	default:
		return plural(int(d.Hours()/(24*365)), "year")
	}
}

// plural formats "<n> <unit>[s] ago" (n is always ≥1 from Ago's branches).
func plural(n int, unit string) string {
	if n <= 1 {
		return "1 " + unit + " ago"
	}
	return fmt.Sprintf("%d %ss ago", n, unit)
}

// HumanTime renders a session timestamp for listings: the relative "time ago"
// plus the absolute local-timezone date in brackets, e.g.
// "5 hours ago (2026-06-19 15:04 CEST)".
func HumanTime(t time.Time) string {
	return Ago(t) + " (" + t.Local().Format("2006-01-02 15:04 MST") + ")"
}

// preview is the first user message, single-lined and trimmed to ~60 runes, for
// the `borg sessions` listing.
func preview(s *Session) string {
	for _, m := range s.Messages {
		if m.Role != "user" {
			continue
		}
		text := strings.Join(strings.Fields(m.Content), " ")
		if r := []rune(text); len(r) > 60 {
			return string(r[:60]) + "…"
		}
		return text
	}
	return "(empty)"
}
