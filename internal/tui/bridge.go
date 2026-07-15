package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/turborg/borg/internal/agent"
)

// Bubble Tea messages posted by the bridge as the agent works. The agent loop
// runs in a goroutine (a tea.Cmd); each UI callback posts one of these into the
// program's event loop via send.
type (
	thinkingMsg   struct{}
	deltaMsg      string
	debugMsg      string
	toolBatchMsg  struct{ n int }
	toolCallMsg   struct{ name, args string }
	toolResultMsg struct {
		name    string
		ok      bool
		summary string
	}
	toolDiffMsg     string
	assistantEndMsg struct{ stats agent.Stats }
	permitMsg       struct {
		name  string
		reply chan agent.Decision
	}
	askMsg struct {
		req   agent.AskRequest
		reply chan agent.AskResult
	}
	turnDoneMsg struct{ err error }
	compactMsg  struct {
		res agent.CompactResult
		err error
	}
	// retroMsg carries the result of the post-thrash retrospective reflection call.
	retroMsg struct {
		retro *agent.Retro
		err   error
	}
	// retroDoneMsg reports the outcome of applying a retrospective (BORG.md write or
	// harness report submission): a status line to print, or an error. report marks
	// the harness-report path, whose failure is best-effort (rendered as a calm dim
	// note, not a red error — the report is optional and nothing else is affected).
	retroDoneMsg struct {
		status string
		err    error
		report bool
	}
)

// uiBridge implements agent.UI by posting tea.Msgs into the running program.
// send is program.Send in production and a capture func in tests, so the bridge
// is exercised without a live program. ctx unblocks a pending Permit when the
// session is cancelled/quit.
type uiBridge struct {
	send func(tea.Msg)
	ctx  context.Context
}

func (b *uiBridge) ThinkingStart()             { b.send(thinkingMsg{}) }
func (b *uiBridge) Delta(s string)             { b.send(deltaMsg(s)) }
func (b *uiBridge) AssistantEnd(s agent.Stats) { b.send(assistantEndMsg{stats: s}) }
func (b *uiBridge) ToolBatch(n int)            { b.send(toolBatchMsg{n: n}) }
func (b *uiBridge) ToolCall(name, args string) { b.send(toolCallMsg{name: name, args: args}) }
func (b *uiBridge) ToolResult(name string, ok bool, summary string) {
	b.send(toolResultMsg{name: name, ok: ok, summary: summary})
}

func (b *uiBridge) ToolDiff(diff string) { b.send(toolDiffMsg(diff)) }

func (b *uiBridge) Debug(s string) { b.send(debugMsg(s)) }

// Permit posts a permission request and blocks the agent goroutine until the
// model feeds back a decision (a keypress) or the context is cancelled.
func (b *uiBridge) Permit(name string) agent.Decision {
	reply := make(chan agent.Decision, 1)
	b.send(permitMsg{name: name, reply: reply})
	select {
	case d := <-reply:
		return d
	case <-b.ctx.Done():
		return agent.DenyOnce
	}
}

// AskUser posts a multiple-choice question and blocks the agent goroutine until
// the user answers (picks an option or types their own) or dismisses it. A
// cancelled context (session quit) unblocks as a dismissal so the agent proceeds
// rather than hanging.
func (b *uiBridge) AskUser(req agent.AskRequest) agent.AskResult {
	reply := make(chan agent.AskResult, 1)
	b.send(askMsg{req: req, reply: reply})
	select {
	case r := <-reply:
		return r
	case <-b.ctx.Done():
		return agent.AskResult{}
	}
}
