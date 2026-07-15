package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, out)
}

// borgMdCommitsBehind counts commits since BORG.md's last commit, and stays
// inapplicable (no nudge) for non-git dirs, a missing/never-committed BORG.md, and
// uncommitted edits — the cases that spare SVN/Mercurial/no-VCS and fresh files.
func TestBorgMdCommitsBehind(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "BORG.md"), []byte("ctx\n"), 0o644))

	// Not a git repo yet (covers SVN/Mercurial/no-VCS) → inapplicable.
	_, ok := borgMdCommitsBehind(dir)
	require.False(t, ok)

	runGit(t, dir, "init")
	// BORG.md present but never committed → inapplicable.
	_, ok = borgMdCommitsBehind(dir)
	require.False(t, ok)

	runGit(t, dir, "add", "BORG.md")
	runGit(t, dir, "commit", "-m", "add BORG.md")
	behind, ok := borgMdCommitsBehind(dir)
	require.True(t, ok)
	require.Equal(t, 0, behind) // just committed → 0 behind

	// Two commits that don't touch BORG.md → 2 behind.
	for _, name := range []string{"a.txt", "b.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644))
		runGit(t, dir, "add", name)
		runGit(t, dir, "commit", "-m", "add "+name)
	}
	behind, ok = borgMdCommitsBehind(dir)
	require.True(t, ok)
	require.Equal(t, 2, behind)

	// Re-touching BORG.md (uncommitted) marks it fresh → inapplicable.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "BORG.md"), []byte("ctx v2\n"), 0o644))
	_, ok = borgMdCommitsBehind(dir)
	require.False(t, ok)

	// No BORG.md at all → inapplicable.
	_, ok = borgMdCommitsBehind(t.TempDir())
	require.False(t, ok)
}

// The startup nudge prints only when BORG.md is applicable AND at/over the
// threshold; below-threshold and inapplicable results stay silent.
func TestStaleMsgNudge(t *testing.T) {
	m := newTestModel(t)

	_, cmd := step(t, m, staleMsg{behind: 3, nudge: false})
	require.Nil(t, cmd) // not flagged (below threshold / disabled / inapplicable) → silent

	_, cmd = step(t, m, staleMsg{behind: 40, nudge: true})
	require.NotNil(t, cmd) // flagged → nudge cmd

	// The nudge text recommends /learn.
	require.Contains(t, staleNudgeText(40), "/learn")
}
