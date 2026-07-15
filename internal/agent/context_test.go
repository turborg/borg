package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/llm"
	"github.com/turborg/borg/internal/session"
)

func TestContextWindow(t *testing.T) {
	// Offline fallback table (used until the catalog is fetched).
	require.Equal(t, 262_144, NewWithLLM(&config.Config{Model: "floko"}, &scriptedLLM{}).ContextWindow())
	require.Equal(t, 1_048_576, NewWithLLM(&config.Config{Model: "Chuppa"}, &scriptedLLM{}).ContextWindow())
	require.Equal(t, 1_048_576, NewWithLLM(&config.Config{Model: "axiom"}, &scriptedLLM{}).ContextWindow())
	// Unknown codename falls back to the conservative default, not 0.
	require.Equal(t, defaultContextWindow, NewWithLLM(&config.Config{Model: "mystery"}, &scriptedLLM{}).ContextWindow())
}

func TestContextWindowFromCatalog(t *testing.T) {
	a := NewWithLLM(&config.Config{Model: "chuppa"}, &scriptedLLM{})
	// The live catalog is authoritative over the fallback table.
	a.SetModelWindows([]llm.ModelInfo{
		{ID: "chuppa", MaxInputTokens: 900_000},
		{ID: "floko", MaxInputTokens: 0}, // 0 is ignored (older server omits it)
	})
	require.Equal(t, 900_000, a.ContextWindow())

	a.SetModel("floko")
	require.Equal(t, 262_144, a.ContextWindow()) // 0 catalog value → fallback table

	// An empty catalog leaves the recorded windows untouched.
	a.SetModel("chuppa")
	a.SetModelWindows(nil)
	require.Equal(t, 900_000, a.ContextWindow())
}

func TestContextStatsEstimateAndBreakdown(t *testing.T) {
	a := NewWithLLM(&config.Config{Model: "floko"}, &scriptedLLM{})
	// No turn has run: the estimate covers just the system prompt.
	s := a.ContextStats()
	require.Equal(t, "floko", s.Model)
	require.Equal(t, 262_144, s.Window)
	require.False(t, s.Exact)
	require.Greater(t, s.SystemTokens, 0)
	require.Equal(t, 0, s.Messages)

	// Add a user prompt and a tool result; the breakdown should attribute them.
	a.messages = append(a.messages,
		llm.Message{Role: "user", Content: strings.Repeat("ask ", 100)},
		llm.Message{Role: "tool", Name: "read_file", Content: strings.Repeat("data ", 200)},
	)
	s = a.ContextStats()
	require.Equal(t, 2, s.Messages)
	require.Greater(t, s.MessageTokens, 0)
	require.Greater(t, s.ToolTokens, s.MessageTokens) // the bigger tool result weighs more
	require.Greater(t, s.Used, s.SystemTokens)
	require.GreaterOrEqual(t, s.Percent(), 0) // a few KB is ~0% of a 256k window
}

func TestContextStatsExactPrefersProxyCount(t *testing.T) {
	ui := &harnessUI{}
	// One turn that streams a final answer carrying an exact prompt-token count.
	s := &scriptedLLM{steps: []llm.Message{{
		Role: "assistant", Content: "done",
		Usage: &llm.Usage{PromptTokens: 5000, CompletionTokens: 10, CachedTokens: 120},
	}}}
	a := newHarness(t, ui, s)
	require.NoError(t, a.Ask(context.Background(), "hi"))

	cs := a.ContextStats()
	require.True(t, cs.Exact)
	require.Equal(t, 5000, cs.Used) // the exact count, not the char estimate
	require.Equal(t, 120, cs.Cached)
}

func TestContextStatsPercentClampAndZeroWindow(t *testing.T) {
	require.Equal(t, 0, ContextStats{Window: 0, Used: 100}.Percent())
	require.Equal(t, 100, ContextStats{Window: 100, Used: 250}.Percent())
	require.Equal(t, 50, ContextStats{Window: 100, Used: 50}.Percent())
}

func TestCompactReplacesTranscript(t *testing.T) {
	ui := &harnessUI{}
	// Step 0: the real turn. Step 1: the summarization call → a recap.
	s := &scriptedLLM{steps: []llm.Message{
		{Role: "assistant", Content: "answer", Usage: &llm.Usage{PromptTokens: 8000}},
		say("RECAP: the user wanted X; changed file Y; next step Z."),
	}}
	a := newHarness(t, ui, s)
	require.NoError(t, a.Ask(context.Background(), "do the thing"))
	require.Greater(t, len(a.messages), 2)

	res, err := a.Compact(context.Background())
	require.NoError(t, err)
	require.Contains(t, res.Summary, "RECAP")
	require.Equal(t, 8000, res.BeforeTokens)
	require.Greater(t, res.BeforeTokens, res.AfterTokens) // it actually shrank

	// The transcript is now [system, recap]; the recap carries the summary.
	require.Len(t, a.messages, 2)
	require.Equal(t, "system", a.messages[0].Role)
	require.Equal(t, "user", a.messages[1].Role)
	require.Contains(t, a.messages[1].Content, "RECAP")
	require.Contains(t, a.messages[1].Content, compactedPrefix)
	require.Equal(t, 0, a.lastPromptTokens) // reset so the next turn re-measures
}

func TestSessionContextTokensRoundTrip(t *testing.T) {
	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{{
		Role: "assistant", Content: "done", Usage: &llm.Usage{PromptTokens: 4200},
	}}}
	a := newHarness(t, ui, s)
	require.NoError(t, a.Ask(context.Background(), "hi"))

	var sess session.Session
	a.SnapshotSession(&sess)
	require.Equal(t, 4200, sess.ContextTokens)

	// A fresh agent restoring the session reads the exact context size immediately.
	b := NewWithLLM(&config.Config{Model: "floko"}, &scriptedLLM{})
	b.RestoreSession(&sess)
	cs := b.ContextStats()
	require.True(t, cs.Exact)
	require.Equal(t, 4200, cs.Used)
}

func TestCompactNothingToDo(t *testing.T) {
	a := NewWithLLM(&config.Config{Model: "floko"}, &scriptedLLM{})
	_, err := a.Compact(context.Background())
	require.Error(t, err) // only the system prompt — nothing to compact
}

func TestCompactEmptySummaryLeavesConversation(t *testing.T) {
	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{
		say("answer"),
		say("   "), // a blank summary must NOT clobber the transcript
	}}
	a := newHarness(t, ui, s)
	require.NoError(t, a.Ask(context.Background(), "hi"))
	before := len(a.messages)
	_, err := a.Compact(context.Background())
	require.Error(t, err)
	require.Len(t, a.messages, before) // unchanged on failure
}

// errLLM fails every Chat — to cover Compact's error path without a live proxy.
type errLLM struct{ scriptedLLM }

func (errLLM) Chat(context.Context, []llm.Message, []llm.Tool, bool, func(string), ...llm.ChatOption) (*llm.Message, error) {
	return nil, context.DeadlineExceeded
}

func TestCompactPropagatesChatError(t *testing.T) {
	a := NewWithLLM(&config.Config{Model: "floko"}, &errLLM{})
	a.messages = append(a.messages, llm.Message{Role: "user", Content: "hi"})
	_, err := a.Compact(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
