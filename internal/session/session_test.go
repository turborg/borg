package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/turborg/borg/internal/llm"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := store{dir: t.TempDir()}
	sess := New("chuppa", true)
	sess.Messages = []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hello there"},
	}
	require.NoError(t, s.save(sess))
	require.False(t, sess.LastActive.IsZero())

	got, err := s.load(sess.ID)
	require.NoError(t, err)
	require.Equal(t, sess.ID, got.ID)
	require.Equal(t, "chuppa", got.Model)
	require.True(t, got.Think)
	require.Len(t, got.Messages, 2)
}

func TestLoadByPrefixAndErrors(t *testing.T) {
	s := store{dir: t.TempDir()}
	a := &Session{ID: "aabbccdd", Model: "floko"}
	b := &Session{ID: "aabbeeff", Model: "floko"}
	c := &Session{ID: "ffffffff", Model: "floko"}
	for _, sess := range []*Session{a, b, c} {
		require.NoError(t, s.save(sess))
	}

	// Unique prefix resolves.
	got, err := s.load("ffff")
	require.NoError(t, err)
	require.Equal(t, "ffffffff", got.ID)

	// Ambiguous prefix errors.
	_, err = s.load("aabb")
	require.ErrorContains(t, err, "ambiguous")

	// No match / empty id error.
	_, err = s.load("zzzz")
	require.ErrorContains(t, err, "no session")
	_, err = s.load("")
	require.ErrorContains(t, err, "no session id")
}

func TestListOrderingAndPreview(t *testing.T) {
	s := store{dir: t.TempDir()}
	first := New("floko", false)
	first.Messages = []llm.Message{{Role: "user", Content: "  do   the    thing  "}}
	require.NoError(t, s.save(first))

	second := New("floko", false)
	second.Messages = []llm.Message{{Role: "system", Content: "sys"}} // no user msg
	require.NoError(t, s.save(second))

	metas, err := s.list()
	require.NoError(t, err)
	require.Len(t, metas, 2)
	// Newest (saved last) is first.
	require.Equal(t, second.ID, metas[0].ID)
	require.Equal(t, "(empty)", metas[0].Preview)
	require.Equal(t, "do the thing", metas[1].Preview) // whitespace collapsed
}

func TestPreviewTruncation(t *testing.T) {
	long := strings.Repeat("x", 100)
	s := &Session{Messages: []llm.Message{{Role: "user", Content: long}}}
	p := preview(s)
	require.True(t, strings.HasSuffix(p, "…"))
	require.Equal(t, 61, len([]rune(p))) // 60 runes + ellipsis
}

func TestNameIsSetOnceAndSticky(t *testing.T) {
	s := store{dir: t.TempDir()}
	sess := New("floko", false)
	sess.Messages = []llm.Message{{Role: "user", Content: "  Fix   the   failing test  "}}
	require.NoError(t, s.save(sess))
	require.Equal(t, "Fix the failing test", sess.Name) // titled from the first prompt, whitespace collapsed

	// Simulate /clear + a brand-new first prompt: the established name must NOT change.
	sess.Messages = []llm.Message{{Role: "user", Content: "something else entirely"}}
	require.NoError(t, s.save(sess))
	require.Equal(t, "Fix the failing test", sess.Name)

	// The listing exposes the fixed Name.
	metas, err := s.list()
	require.NoError(t, err)
	require.Len(t, metas, 1)
	require.Equal(t, "Fix the failing test", metas[0].Name)
}

func TestNameTruncation(t *testing.T) {
	long := strings.Repeat("y", 100)
	n := deriveName(&Session{Messages: []llm.Message{{Role: "user", Content: long}}})
	require.True(t, strings.HasSuffix(n, "…"))
	require.Equal(t, 49, len([]rune(n))) // 48 runes + ellipsis

	// No prompt yet → no title (titling waits for one).
	require.Empty(t, deriveName(&Session{Messages: []llm.Message{{Role: "system", Content: "sys"}}}))
}

func TestLegacySessionGetsDerivedNameInListing(t *testing.T) {
	dir := t.TempDir()
	s := store{dir: dir}
	// A session persisted before titling existed: no "name" field on disk.
	raw := `{"id":"legacy01","model":"floko","messages":[{"role":"user","content":"old task"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "legacy01.json"), []byte(raw), 0o600))

	metas, err := s.list()
	require.NoError(t, err)
	require.Len(t, metas, 1)
	require.Equal(t, "old task", metas[0].Name) // derived for display (not persisted yet)
}

func TestAgoAndHumanTime(t *testing.T) {
	now := time.Now()
	require.Equal(t, "just now", Ago(now.Add(-30*time.Second)))
	require.Equal(t, "1 minute ago", Ago(now.Add(-time.Minute)))
	require.Equal(t, "5 minutes ago", Ago(now.Add(-5*time.Minute)))
	require.Equal(t, "1 hour ago", Ago(now.Add(-time.Hour)))
	require.Equal(t, "3 hours ago", Ago(now.Add(-3*time.Hour)))
	require.Equal(t, "2 days ago", Ago(now.Add(-48*time.Hour)))

	h := HumanTime(now.Add(-3 * time.Hour))
	require.Contains(t, h, "3 hours ago")
	require.Contains(t, h, "(") // bracketed absolute date follows
	require.Contains(t, h, ")")
}

func TestLatest(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, err := Latest()
	require.Error(t, err) // none yet

	first := New("floko", false)
	first.Messages = []llm.Message{{Role: "user", Content: "first"}}
	require.NoError(t, Save(first))
	time.Sleep(2 * time.Millisecond) // distinct LastActive

	second := New("floko", false)
	second.Dir = "/some/other/dir" // a different directory entirely
	second.Messages = []llm.Message{{Role: "user", Content: "second"}}
	require.NoError(t, Save(second))

	got, err := Latest()
	require.NoError(t, err)
	require.Equal(t, second.ID, got.ID) // newest across ALL directories
}

func TestListForDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	here := CurrentDir()

	mine := New("floko", false) // stamped with cwd
	mine.Messages = []llm.Message{{Role: "user", Content: "mine"}}
	require.NoError(t, Save(mine))

	elsewhere := New("floko", false)
	elsewhere.Dir = filepath.Join(here, "elsewhere")
	require.NoError(t, Save(elsewhere))

	metas, err := ListForDir(here)
	require.NoError(t, err)
	require.Len(t, metas, 1) // only this directory's session
	require.Equal(t, mine.ID, metas[0].ID)
}

func TestPurge(t *testing.T) {
	s := store{dir: t.TempDir()}
	require.NoError(t, s.save(New("floko", false)))
	require.NoError(t, s.save(New("floko", false)))

	n, err := s.purge()
	require.NoError(t, err)
	require.Equal(t, 2, n)

	metas, err := s.list()
	require.NoError(t, err)
	require.Empty(t, metas)
}

func TestEmptyStoreIsNotAnError(t *testing.T) {
	s := store{dir: t.TempDir()} // dir doesn't exist yet
	metas, err := s.list()
	require.NoError(t, err)
	require.Empty(t, metas)

	n, err := s.purge()
	require.NoError(t, err)
	require.Zero(t, n)

	_, err = s.load("anything")
	require.ErrorContains(t, err, "no session")
}

func TestDefaultStorePackageFuncs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	sess := New("floko", false)
	sess.Messages = []llm.Message{{Role: "user", Content: "hi"}}
	require.NoError(t, Save(sess))

	got, err := Load(sess.ID)
	require.NoError(t, err)
	require.Equal(t, sess.ID, got.ID)

	metas, err := List()
	require.NoError(t, err)
	require.Len(t, metas, 1)

	n, err := Purge()
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func TestLatestForDirAndDirStamp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	s1 := New("floko", false)
	require.NotEmpty(t, s1.Dir) // New stamps the working directory
	here := s1.Dir
	s1.Messages = []llm.Message{{Role: "user", Content: "first"}}
	require.NoError(t, Save(s1))

	other := New("floko", false)
	other.Dir = filepath.Join(here, "elsewhere")
	require.NoError(t, Save(other))

	// LatestForDir filters to the requested directory.
	got, err := LatestForDir(here)
	require.NoError(t, err)
	require.Equal(t, s1.ID, got.ID)

	// Meta exposes Dir for the listing.
	metas, err := List()
	require.NoError(t, err)
	byID := map[string]Meta{}
	for _, m := range metas {
		byID[m.ID] = m
	}
	require.Equal(t, here, byID[s1.ID].Dir)
	require.Equal(t, other.Dir, byID[other.ID].Dir)

	// A directory with no session is an error.
	_, err = LatestForDir(filepath.Join(here, "nope"))
	require.Error(t, err)

	require.NotEmpty(t, CurrentDir())
}

func TestCorruptFileHandling(t *testing.T) {
	dir := t.TempDir()
	s := store{dir: dir}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o600))

	// List skips unreadable files rather than failing the whole listing.
	metas, err := s.list()
	require.NoError(t, err)
	require.Empty(t, metas)

	// Loading the corrupt id surfaces the parse error.
	_, err = s.load("bad")
	require.ErrorContains(t, err, "parse session")
}

func TestNewIDsAreDistinct(t *testing.T) {
	require.NotEqual(t, newID(), newID())
	require.Len(t, New("floko", false).ID, 8)
}
