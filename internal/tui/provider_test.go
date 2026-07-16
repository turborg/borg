package tui

// The REPL's account-facing panels must not invent an xShellz account for a
// session that doesn't have one. A fabricated "plan: Free — 50 credits/day" while
// the user runs their own Ollama box is worse than showing nothing.

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/turborg/borg/internal/agent"
	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/llm"
	"github.com/turborg/borg/internal/session"
	"github.com/turborg/borg/internal/trust"
)

// newLocalTestModel builds a REPL model backed by a bring-your-own provider.
func newLocalTestModel(t *testing.T) model {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cwd, _ := os.Getwd()
	_ = trust.Record(cwd, trust.ScopeDir)
	cfg := &config.Config{
		Provider: config.ProviderOllama, Model: "qwen2.5-coder:7b",
		BaseURL: "http://localhost:11434/v1", LLMProxyURL: "http://localhost:11434/v1",
	}
	a := agent.NewWithLLM(cfg, llm.New(cfg, ""))
	m := newModel(context.Background(), a, session.New("qwen2.5-coder:7b", false))
	m.bannerBlink = -1
	return m
}

// /usage must say there's nothing to meter — not fall back to a plan cap.
func TestUsagePanelOnLocalProviderReportsNoMetering(t *testing.T) {
	m := newLocalTestModel(t)
	out := ansiRE.ReplaceAllString(m.renderUsage(usageMsg{err: llm.ErrNoMetering}), "")

	require.Contains(t, out, "no usage to report")
	require.Contains(t, out, "ollama")
	require.NotContains(t, out, "credits / day", "no invented cap")
	require.NotContains(t, out, "Free", "no invented plan")
}

// A real fetch failure on the HOSTED path still falls back to the static caps —
// the ErrNoMetering branch must not swallow ordinary errors.
func TestUsagePanelHostedFallbackUnchanged(t *testing.T) {
	m := newTestModel(t)
	out := ansiRE.ReplaceAllString(m.renderUsage(usageMsg{err: context.DeadlineExceeded}), "")
	require.Contains(t, out, "live usage unavailable")
	require.Contains(t, out, "credits / day")
}

// /status must describe the backend, not a login the user doesn't need.
func TestStatusPanelOnLocalProvider(t *testing.T) {
	m := newLocalTestModel(t)
	out := ansiRE.ReplaceAllString(m.renderStatus(statusMsg{}), "")

	require.Contains(t, out, "provider: ollama")
	require.Contains(t, out, "http://localhost:11434/v1")
	require.Contains(t, out, "not metered by borg")
	require.NotContains(t, out, "not logged in", "there is nothing to log in to")
	require.NotContains(t, out, "plan:", "there is no plan")
	// The session tail is still there.
	require.Contains(t, out, "qwen2.5-coder:7b")
	require.Contains(t, out, "cwd:")
}

// The hosted /status keeps every line it always had.
func TestStatusPanelHostedUnchanged(t *testing.T) {
	m := newTestModel(t)
	out := ansiRE.ReplaceAllString(m.renderStatus(statusMsg{
		user:  &llm.UserInfo{Name: "Ada", Email: "ada@example.com", PlanCode: "pro"},
		usage: &llm.AccountUsage{PlanCode: "pro", WindowHours: 24, CreditsUsed: 10, CreditsPerDay: 500, PercentUsed: 2},
	}), "")
	// (No credentials are stored in the test env, so the user line reads "not
	// logged in" — that's the pre-existing hosted behavior and the point here: the
	// hosted panel still shows the login/plan/usage block that BYO drops.)
	require.Contains(t, out, "user:")
	require.Contains(t, out, "plan:    Pro")
	require.Contains(t, out, "10 / 500")
	require.Contains(t, out, "think off")
	require.Contains(t, out, "floko")
}

// With no tier (the local case) the banner's info line must not print "plan: ".
func TestInfoLineOmitsEmptyPlan(t *testing.T) {
	m := newLocalTestModel(t)
	m.tier = ""
	m.models = []llm.ModelInfo{{ID: "qwen2.5-coder:7b", Label: "qwen2.5-coder:7b", Available: true}}
	out := ansiRE.ReplaceAllString(m.infoLine(), "")
	require.NotContains(t, out, "plan:")
	require.Contains(t, out, "qwen2.5-coder:7b")
	require.Contains(t, out, "✓", "your own models are never shown as locked")
}

// The concern is a local run leaking account UI. Tier() reports "" off-platform
// (see llm.Client.Tier for why it's "" rather than an error), and the banner keys
// off that — so the plan badge is absent, not blank. Pinned because "never mention
// an account the user doesn't have" is the rule this whole path exists to keep.
func TestLocalBannerShowsNoAccountUI(t *testing.T) {
	m := newLocalTestModel(t)
	m.tier = "" // what Tier() yields on a bring-your-own backend
	m.models = []llm.ModelInfo{{ID: "qwen2.5-coder:7b", Label: "qwen2.5-coder:7b", Available: true}}
	out := ansiRE.ReplaceAllString(m.infoLine(), "")
	require.NotContains(t, out, "plan")
	require.NotContains(t, out, "free")
	require.NotContains(t, out, "credits")
	require.NotContains(t, out, "login")
}

// When auto-approve is on, the footer must show the amber chip: with the prompt
// gone, that chip is the only standing signal that mutating tools run unattended.
func TestFooterFlagsAutoApprove(t *testing.T) {
	m := newLocalTestModel(t)
	m.width = 120
	m.gitBranch = "" // isolate the chip from a branch name that may contain the word

	m.agent.SetAutoApprove(false)
	require.NotContains(t, ansiRE.ReplaceAllString(m.footerView(""), ""), "auto-approve")

	m.agent.SetAutoApprove(true)
	require.Contains(t, ansiRE.ReplaceAllString(m.footerView(""), ""), "auto-approve",
		"an auto-approving session must be visibly flagged")
}
