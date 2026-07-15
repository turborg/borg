package tui

import (
	"errors"

	tea "charm.land/bubbletea/v2"

	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/selfupdate"
	"github.com/turborg/borg/internal/version"
)

// updateMsg carries the startup version check; updateDoneMsg the install result.
type updateMsg struct {
	latest string
	newer  bool
}

type updateDoneMsg struct {
	version  string
	upToDate bool
	err      error
}

// checkUpdate fetches the latest published version off the event loop (throttled
// to once a day by the selfupdate cache) so the startup nudge never blocks the
// first paint. Any error is swallowed to a quiet no-op.
func (m model) checkUpdate() tea.Cmd {
	cur := version.Version
	ctx := m.ctx
	return func() tea.Msg {
		cfg, err := config.Load()
		if err != nil {
			return updateMsg{}
		}
		latest, newer, err := selfupdate.Check(ctx, cfg.InstallBase, cur)
		if err != nil {
			return updateMsg{}
		}
		return updateMsg{latest: latest, newer: newer}
	}
}

// runUpdate installs the latest release off the event loop, replacing the binary.
func (m model) runUpdate() tea.Cmd {
	cur := version.Version
	ctx := m.ctx
	return func() tea.Msg {
		cfg, err := config.Load()
		if err != nil {
			return updateDoneMsg{err: err}
		}
		v, err := selfupdate.Update(ctx, cfg.InstallBase, cur)
		if errors.Is(err, selfupdate.ErrUpToDate) {
			return updateDoneMsg{version: v, upToDate: true}
		}
		return updateDoneMsg{version: v, err: err}
	}
}

func updateNudgeText(latest string) string {
	cmd := version.Command()
	return "↑ " + cmd + " " + latest + " is available — run /update (or `" + cmd + " update`)"
}
