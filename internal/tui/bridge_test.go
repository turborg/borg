package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"github.com/turborg/borg/internal/agent"
)

func TestUIBridgeDebugSendsMsg(t *testing.T) {
	var gotMsgs []interface{}
	send := func(m tea.Msg) { gotMsgs = append(gotMsgs, m) }
	b := &uiBridge{send: send, ctx: context.Background()}
	b.Debug("line1\nline2")
	// expect a debugMsg was sent
	require.Len(t, gotMsgs, 1)
	if dm, ok := gotMsgs[0].(debugMsg); ok {
		require.Contains(t, string(dm), "line1")
	} else {
		require.Fail(t, "expected debugMsg")
	}

	// also sanity-check other bridge methods post expected msg types
	b.ThinkingStart()
	b.Delta("d")
	b.ToolCall("t", "{}")
	b.ToolResult("x", true, "ok")
	b.AssistantEnd(agent.Stats{})
	b.ToolBatch(3)
	// sent messages count increased
	require.GreaterOrEqual(t, len(gotMsgs), 2)
}

func TestUIBridgeAskUser(t *testing.T) {
	// A reply on the channel is returned to the (blocked) agent goroutine.
	msgs := make(chan tea.Msg, 1)
	b := &uiBridge{send: func(m tea.Msg) { msgs <- m }, ctx: context.Background()}
	go func() { (<-msgs).(askMsg).reply <- agent.AskResult{Choice: "B"} }()
	got := b.AskUser(agent.AskRequest{Question: "Q?", Options: []agent.AskOption{{Label: "A"}, {Label: "B"}}})
	require.Equal(t, agent.AskResult{Choice: "B"}, got)

	// A cancelled context unblocks AskUser as a dismissal (zero result).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bc := &uiBridge{send: func(tea.Msg) {}, ctx: ctx}
	require.Equal(t, agent.AskResult{}, bc.AskUser(agent.AskRequest{Question: "Q?"}))
}
