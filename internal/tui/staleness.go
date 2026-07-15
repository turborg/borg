package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/turborg/borg/internal/agent"
	"github.com/turborg/borg/internal/config"
)

// staleNudgeText is the unstyled one-line nudge shown at startup when BORG.md has
// drifted past the configured commit threshold. Factored out so it's testable
// without reaching into bubbletea's unexported print message.
func staleNudgeText(behind int) string {
	return fmt.Sprintf("BORG.md is %d commits behind HEAD — run /learn to refresh the project context.", behind)
}

// staleMsg carries the BORG.md staleness verdict from checkStaleness to Update.
// nudge is true only when the file is applicable AND far enough behind the
// configured threshold; behind is shown in the message.
type staleMsg struct {
	behind int
	nudge  bool
}

// checkStaleness decides, off the event loop, whether to nudge the user to re-run
// /learn. It's gated by the learn_stale_after setting (0 = disabled, skipping git
// entirely), then measures commits behind via git with a short timeout; any failure
// means no nudge. Never on the paint path.
func (m model) checkStaleness() tea.Cmd {
	dir := m.cwd
	threshold := config.LearnStaleThreshold()
	return func() tea.Msg {
		if threshold <= 0 {
			return staleMsg{} // nudge disabled
		}
		behind, ok := borgMdCommitsBehind(dir)
		return staleMsg{behind: behind, nudge: ok && behind >= threshold}
	}
}

// borgMdCommitsBehind returns how many commits HEAD has gained since the commit
// that last modified BORG.md in dir. ok is false (→ no nudge) when dir isn't a git
// work tree (this is what spares SVN/Mercurial/no-VCS checkouts), git is
// unavailable, or BORG.md is absent, never committed, or has uncommitted edits
// (just regenerated — already fresh). git plumbing only; no working-tree mutation.
func borgMdCommitsBehind(dir string) (behind int, ok bool) {
	if dir == "" {
		return 0, false
	}
	if _, err := os.Stat(filepath.Join(dir, agent.ProjectContextFile)); err != nil {
		return 0, false // no BORG.md → nothing to be stale
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	git := func(args ...string) (string, bool) {
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(string(out)), true
	}
	// A git work tree? (false for SVN/Mercurial/no-VCS → no nudge.)
	if _, isGit := git("rev-parse", "--is-inside-work-tree"); !isGit {
		return 0, false
	}
	// Uncommitted edits to BORG.md ⇒ just (re)generated, so it's already fresh.
	if st, gotStatus := git("status", "--porcelain", "--", agent.ProjectContextFile); !gotStatus || st != "" {
		return 0, false
	}
	last, gotLast := git("log", "-1", "--format=%H", "--", agent.ProjectContextFile)
	if !gotLast || last == "" {
		return 0, false // present but never committed
	}
	cnt, gotCount := git("rev-list", "--count", last+"..HEAD")
	if !gotCount {
		return 0, false
	}
	n, err := strconv.Atoi(cnt)
	if err != nil {
		return 0, false
	}
	return n, true
}
