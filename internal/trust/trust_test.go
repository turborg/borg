package trust

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestRecordLookupRoot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	dir := filepath.Join("/home", "me", "project")
	_, ok := Lookup(dir)
	require.False(t, ok) // nothing recorded yet

	require.NoError(t, Record(dir, ScopeParent))
	got, ok := Lookup(dir)
	require.True(t, ok)
	require.Equal(t, ScopeParent, got)

	// Root confines to the dir (dir scope) or its parent (parent scope).
	require.Equal(t, dir, Root(dir, ScopeDir))
	require.Equal(t, filepath.Dir(dir), Root(dir, ScopeParent))

	// Recording a second directory doesn't clobber the first.
	require.NoError(t, Record(filepath.Join("/other"), ScopeDir))
	got, _ = Lookup(dir)
	require.Equal(t, ScopeParent, got)
}

func TestCorruptStoreDegrades(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	store := filepath.Join(dir, "borg")
	require.NoError(t, os.MkdirAll(store, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(store, "trust.json"), []byte("{not json"), 0o600))

	// A corrupt store is treated as empty, not a crash...
	_, ok := Lookup("/x")
	require.False(t, ok)
	// ...and Record recovers by overwriting it.
	require.NoError(t, Record("/x", ScopeDir))
	s, ok := Lookup("/x")
	require.True(t, ok)
	require.Equal(t, ScopeDir, s)
}
