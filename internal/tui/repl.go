// Package tui is borg's interactive REPL. It runs as an inline Bubble Tea
// program (no alt screen): finished turns are flushed to real terminal
// scrollback via tea.Printf — so they stay copy-pasteable and scroll normally,
// the Ink "<Static>" pattern — while only the in-progress turn and the input
// prompt live in the redrawn region. Assistant replies are rendered as markdown
// (glamour) as they stream. Conversations persist as resumable sessions.
package tui

import (
	"context"
	"errors"

	tea "charm.land/bubbletea/v2"

	"github.com/turborg/borg/internal/agent"
	"github.com/turborg/borg/internal/session"
)

// Run starts the REPL against an agent, persisting into sess, and returns when
// the user exits. ctx cancellation (Ctrl-C / SIGTERM) quits the program and
// unblocks any pending tool-permission prompt.
func Run(ctx context.Context, a *agent.Agent, sess *session.Session, opts ...tea.ProgramOption) error {
	m := newModel(ctx, a, sess)
	p := tea.NewProgram(m, append([]tea.ProgramOption{tea.WithContext(ctx)}, opts...)...)

	// The bridge posts the agent's streamed output into the running program.
	a.SetUI(&uiBridge{send: p.Send, ctx: ctx})

	_, err := p.Run()
	// A cancelled context (SIGINT/SIGTERM) or an explicit kill is a normal exit,
	// not an error to surface to the user.
	if errors.Is(err, context.Canceled) || errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}
