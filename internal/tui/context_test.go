package tui

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/turborg/borg/internal/agent"
	"github.com/turborg/borg/internal/auth"
	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/llm"
	"github.com/turborg/borg/internal/session"
	"github.com/turborg/borg/internal/trust"
)

func TestContextCommand(t *testing.T) {
	m := newTestModel(t)
	_, cmd := slash(m, "/context")
	require.NotNil(t, cmd) // prints the /context panel

	out := ansiRE.ReplaceAllString(m.renderContext(), "")
	require.Contains(t, out, "context")
	require.Contains(t, out, "floko")
	require.Contains(t, out, "window")
	require.Contains(t, out, "262.1k") // the model's window (floko fallback, 262144)
	require.Contains(t, out, "breakdown")
}

func TestContextPanelWarnsWhenNearlyFull(t *testing.T) {
	m := newTestModel(t)
	// Inflate the live measurement past the warn threshold (floko = 256k window).
	m.ctxTokens = 250_000
	// The footer warning line appears once we're near the limit.
	require.Contains(t, ansiRE.ReplaceAllString(m.contextWarning(), ""), "/compact")
	require.Contains(t, ansiRE.ReplaceAllString(vw(m), ""), "context")
}

func TestContextWarningSilentWhenLow(t *testing.T) {
	m := newTestModel(t)
	require.Empty(t, m.contextWarning()) // no measurement yet
	m.ctxTokens = 1000
	require.Empty(t, m.contextWarning()) // well under the threshold
}

func TestContextLabelFooter(t *testing.T) {
	require.Empty(t, contextLabel(0, 256_000)) // no measurement
	require.Empty(t, contextLabel(1000, 0))    // unknown window
	require.Contains(t, ansiRE.ReplaceAllString(contextLabel(25_600, 256_000), ""), "ctx 10%")
	require.Contains(t, ansiRE.ReplaceAllString(contextLabel(250_000, 256_000), ""), "ctx 97%")
}

func TestContextBar(t *testing.T) {
	require.Len(t, ansiRE.ReplaceAllString(contextBar(0, 10), ""), 10)
	require.Len(t, ansiRE.ReplaceAllString(contextBar(50, 10), ""), 10)
	require.Len(t, ansiRE.ReplaceAllString(contextBar(150, 10), ""), 10) // clamps past 100%
}

func TestCompactCommandNoConversation(t *testing.T) {
	m := newTestModel(t)
	// Only the system prompt exists, so /compact refuses and explains.
	mc, cmd := slash(m, "/compact")
	require.False(t, mc.streaming)
	require.Nil(t, cmd)
	require.Contains(t, mc.status, "nothing to compact")
}

func TestCompactCommandStartsStreaming(t *testing.T) {
	m := newTestModel(t)
	// Seed a conversation directly (no real turn / network) so /compact proceeds.
	m.agent.SetMessages([]llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "do the thing"},
		{Role: "assistant", Content: "done"},
	})

	mc, cmd := slash(m, "/compact")
	require.True(t, mc.streaming)
	require.True(t, mc.compacting)
	require.NotNil(t, cmd)
	// The working box shows the compacting note.
	require.Contains(t, ansiRE.ReplaceAllString(vw(mc), ""), "compacting")
}

func TestCompactMsgResetsAndReports(t *testing.T) {
	m := newTestModel(t)
	m.streaming = true
	m.compacting = true
	m.ctxTokens = 9000
	m.sessIn, m.sessOut = 5000, 200

	m, cmd := step(t, m, compactMsg{res: agent.CompactResult{BeforeTokens: 9000, AfterTokens: 1200}})
	require.False(t, m.streaming)
	require.False(t, m.compacting)
	require.Equal(t, 0, m.ctxTokens)
	require.Equal(t, 0, m.sessIn)
	require.NotNil(t, cmd) // prints the "compacted …" summary line

	out := ansiRE.ReplaceAllString(m.renderCompact(agent.CompactResult{BeforeTokens: 9000, AfterTokens: 1200}), "")
	require.Contains(t, out, "compacted")
	require.Contains(t, out, "freed")
}

func TestCompactMsgError(t *testing.T) {
	m := newTestModel(t)
	m.streaming = true
	m.compacting = true
	m, cmd := step(t, m, compactMsg{err: errTest})
	require.False(t, m.streaming)
	require.False(t, m.compacting)
	require.NotNil(t, cmd) // prints the error line
}

var errTest = errString("boom")

type errString string

func (e errString) Error() string { return string(e) }

func TestClearResetsContextTokens(t *testing.T) {
	m := newTestModel(t)
	m.ctxTokens = 5000
	m, _ = slash(m, "/clear")
	require.Equal(t, 0, m.ctxTokens)
}

func TestContextHelpAndMenu(t *testing.T) {
	// /context and /compact are advertised in the slash menu + help.
	require.Contains(t, strings.Join(commandNames(), " "), "/context")
	require.Contains(t, strings.Join(commandNames(), " "), "/compact")
}

func TestNewModelSeedsSessionTotals(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cwd, _ := os.Getwd()
	_ = trust.Record(cwd, trust.ScopeDir)
	a := agent.New(&config.Config{Model: "floko"}, &auth.Credentials{AccessToken: "x"})
	sess := session.New("floko", false)
	sess.TokensIn, sess.TokensOut, sess.ContextTokens = 12_000, 3_400, 9_000

	m := newModel(context.Background(), a, sess)
	require.Equal(t, 12_000, m.sessIn)
	require.Equal(t, 3_400, m.sessOut)
	require.Equal(t, 9_000, m.ctxTokens)
}

func TestSwitchSessionRestoresTotals(t *testing.T) {
	m := newTestModel(t) // isolates XDG_CONFIG_HOME
	target := session.New("floko", false)
	target.TokensIn, target.TokensOut, target.ContextTokens = 8_000, 2_000, 7_000
	target.Messages = []llm.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"}}
	require.NoError(t, session.Save(target))

	tm, _ := m.attachSession([]string{"/sessions", target.ID})
	m2 := tm.(model)
	require.Equal(t, 8_000, m2.sessIn)
	require.Equal(t, 2_000, m2.sessOut)
	require.Equal(t, 7_000, m2.ctxTokens)
}

func TestPersistSavesTokenTotals(t *testing.T) {
	m := newTestModel(t)
	m.sessIn, m.sessOut = 1_111, 222
	m.persist()
	got, err := session.Load(m.sess.ID)
	require.NoError(t, err)
	require.Equal(t, 1_111, got.TokensIn)
	require.Equal(t, 222, got.TokensOut)
}

func TestModelSwitchBurnHint(t *testing.T) {
	m := newTestModel(t)
	m.models = []llm.ModelInfo{
		{ID: "chuppa", Label: "Chuppa", Version: "3.2", MinTier: "free", BurnRate: 1},
		{ID: "axiom", Label: "Axiom", Version: "3.2", MinTier: "starter", BurnRate: 13},
	}

	// Switching to a high-burn model warns about budget spend.
	m, _ = slash(m, "/model axiom")
	require.Equal(t, "axiom", m.agent.Model())
	require.Contains(t, m.status, "Axiom")
	require.Contains(t, m.status, "13×")
	require.Contains(t, m.status, "faster")

	// Switching to a 1× model gives the plain confirmation (no warning).
	m, _ = slash(m, "/model chuppa")
	require.Equal(t, "model: chuppa", m.status)

	// The picker also shows the burn marker on the high-burn row.
	require.Contains(t, ansiRE.ReplaceAllString(m.modelPickerView(), ""), "≈13× budget")
}

func commandNames() []string {
	var out []string
	for _, c := range slashCmds {
		out = append(out, c.name)
	}
	return out
}
