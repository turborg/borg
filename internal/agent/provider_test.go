package agent

// End-to-end tests of the agent loop against a bring-your-own backend. Unlike
// harness_test.go (which swaps the model out at the LLM interface), these drive
// the REAL *llm.Client against a fake OpenAI-compatible server — the same wiring
// the CLI builds for a local provider — so they cover the whole path: config →
// capabilities → request body → SSE → tool dispatch.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/llm"
)

// localBackend is a fake Ollama-style server. It answers /chat/completions from a
// queue of canned SSE replies and records the raw request bodies it was sent.
type localBackend struct {
	srv *httptest.Server

	mu      sync.Mutex
	bodies  []string
	paths   []string
	replies []string // consumed in order; the last one repeats
}

func newLocalBackend(t *testing.T, replies ...string) *localBackend {
	t.Helper()
	b := &localBackend{replies: replies}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.bodies = append(b.bodies, string(raw))
		b.paths = append(b.paths, r.URL.Path)
		reply := b.replies[min(len(b.bodies)-1, len(b.replies)-1)]
		b.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, reply, "data: [DONE]\n\n")
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *localBackend) requests() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.bodies...)
}

func (b *localBackend) hitPaths() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.paths...)
}

// newLocalAgent builds the agent exactly the way `newAuthedAgent` does for a
// bring-your-own provider: no credentials anywhere in sight.
func newLocalAgent(t *testing.T, b *localBackend, ui UI, provider string) *Agent {
	t.Helper()
	cfg := &config.Config{
		Provider: provider, BaseURL: b.srv.URL, LLMProxyURL: b.srv.URL,
		Model: "qwen2.5-coder:7b",
	}
	client := llm.New(cfg, "")
	t.Cleanup(client.CloseIdleConnections)
	a := NewWithLLM(cfg, client)
	a.SetUI(ui)
	return a
}

func toolCallChunk(t *testing.T, name, args string) string {
	t.Helper()
	return sseChunk(t, map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "id": "c1",
		"function": map[string]any{"name": name, "arguments": args},
	}}})
}

// The whole loop must work against a plain local model server: read a file with a
// tool, then answer. Nothing about the agent is xShellz-specific.
func TestAgentToolLoopAgainstLocalProvider(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	require.NoError(t, os.WriteFile(path, []byte("file says hi"), 0o644))

	b := newLocalBackend(t,
		toolCallChunk(t, "read_file", `{"path":"`+path+`"}`),
		sseChunk(t, map[string]any{"content": "Done."}),
	)
	ui := &captureUI{permit: AllowOnce}
	a := newLocalAgent(t, b, ui, config.ProviderOllama)

	require.NoError(t, a.Ask(context.Background(), "read the file"))
	require.Contains(t, ui.calls, "read_file")
	require.Contains(t, ui.content.String(), "Done.")
	require.Len(t, b.requests(), 2)
	require.Equal(t, []string{"/chat/completions", "/chat/completions"}, b.hitPaths(),
		"a local run must touch nothing but the standard completions route")
}

// Every request the loop sends a local backend must be plain, standard OpenAI —
// asserted on the RAW JSON, since the failure mode is a 400 on an unknown field.
func TestLocalLoopSendsNoProprietaryFields(t *testing.T) {
	b := newLocalBackend(t, sseChunk(t, map[string]any{"content": "hi"}))
	a := newLocalAgent(t, b, &captureUI{permit: AllowOnce}, config.ProviderOllama)
	a.SetThink(true)     // the think toggle...
	a.SetEffort("xhigh") // ...and an explicit effort: neither may reach the wire
	require.NoError(t, a.Ask(context.Background(), "hi"))

	reqs := b.requests()
	require.NotEmpty(t, reqs)
	for _, raw := range reqs {
		for _, field := range []string{"think", "reasoning_effort", "prompt_cache_key", "tool_choice"} {
			require.NotContains(t, raw, field)
		}
		var body map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &body))
		require.Equal(t, "qwen2.5-coder:7b", body["model"])
	}
}

// A local provider must never trigger an account lookup — no /v1/users/me, no
// /usage. Beyond the wasted round-trip, borg would be probing a stranger's server
// for xShellz routes it has no business asking about.
func TestLocalProviderNeverCallsAccountEndpoints(t *testing.T) {
	b := newLocalBackend(t, sseChunk(t, map[string]any{"content": "hi"}))
	a := newLocalAgent(t, b, &captureUI{permit: AllowOnce}, config.ProviderOllama)
	ctx := context.Background()

	require.NoError(t, a.Ask(ctx, "hi"))

	tier, err := a.Tier(ctx)
	require.NoError(t, err)
	require.Empty(t, tier)

	user, err := a.UserInfo(ctx)
	require.NoError(t, err)
	require.Equal(t, &llm.UserInfo{}, user)

	_, err = a.Usage(ctx)
	require.ErrorIs(t, err, llm.ErrNoMetering, "callers can detect this and say metering is xShellz-only")

	for _, p := range b.hitPaths() {
		require.Equal(t, "/chat/completions", p, "no account route may ever be requested")
	}
}

// A model that can't tool-call answers with nothing at all. borg must stop with an
// error that names the likely cause — not idle silently, and not retry forever.
func TestEmptyRepliesStopWithAnActionableError(t *testing.T) {
	b := newLocalBackend(t, sseChunk(t, map[string]any{"content": ""}))
	ui := &captureUI{permit: AllowOnce}
	a := newLocalAgent(t, b, ui, config.ProviderOllama)

	err := a.Ask(context.Background(), "fix the bug")
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty reply")
	require.Contains(t, err.Error(), "tool call", "the cause is named, not just the symptom")
	require.Contains(t, err.Error(), "tools-first")

	// Bounded: one nudge, then stop — never a loop that burns the budget on silence.
	require.Len(t, b.requests(), maxEmptyReplies)
	require.Contains(t, b.requests()[1], "Your last reply was empty", "the nudge is fed back first")

	// The give-up is recorded as terminal, so the retrospective can offer a report.
	s := a.LastStruggle()
	require.NotNil(t, s)
	require.True(t, s.Terminal)
}

// A single empty reply is just a hiccup: if the model recovers after the nudge,
// the task carries on normally.
func TestSingleEmptyReplyRecovers(t *testing.T) {
	b := newLocalBackend(t,
		sseChunk(t, map[string]any{"content": ""}),
		sseChunk(t, map[string]any{"content": "Here's the answer."}),
	)
	ui := &captureUI{permit: AllowOnce}
	a := newLocalAgent(t, b, ui, config.ProviderOllama)

	require.NoError(t, a.Ask(context.Background(), "hi"))
	require.Contains(t, ui.content.String(), "Here's the answer.")
}

// The counter tracks CONSECUTIVE emptiness: an empty reply, then real work, then
// another empty one much later is not a broken backend.
func TestEmptyReplyCounterResetsAfterProgress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

	b := newLocalBackend(t,
		sseChunk(t, map[string]any{"content": ""}),           // 1: empty → nudge
		toolCallChunk(t, "read_file", `{"path":"`+path+`"}`), // 2: progress → reset
		sseChunk(t, map[string]any{"content": ""}),           // 3: empty → nudge again
		sseChunk(t, map[string]any{"content": "ok"}),         // 4: recovers
	)
	a := newLocalAgent(t, b, &captureUI{permit: AllowOnce}, config.ProviderOllama)
	require.NoError(t, a.Ask(context.Background(), "go"), "isolated empties must not add up to a give-up")
}

// REGRESSION GUARD: the hosted path's context windows are exactly as before.
func TestContextWindowHostedUnchanged(t *testing.T) {
	for model, want := range map[string]int{
		"floko": 262_144, "chuppa": 1_048_576, "axiom": 1_048_576,
		"something-new": 262_144, // unknown on-platform → the historical default
	} {
		a := NewWithLLM(&config.Config{Model: model}, &scriptedLLM{})
		require.Equal(t, want, a.ContextWindow(), "model %q", model)
	}
}

// A BYO backend serves no catalog, so an unknown model must be assumed SMALL.
// Guessing high is the dangerous direction: the server silently truncates from
// the front while /context reports plenty of room.
func TestContextWindowByoIsConservative(t *testing.T) {
	a := NewWithLLM(&config.Config{Provider: config.ProviderOllama, Model: "qwen2.5-coder:7b"}, &scriptedLLM{})
	require.Equal(t, 32_768, a.ContextWindow())

	// The codenames are not reserved words: a local model sharing one must NOT
	// inherit the hosted table's 1M window.
	a = NewWithLLM(&config.Config{Provider: config.ProviderOllama, Model: "chuppa"}, &scriptedLLM{})
	require.Equal(t, 32_768, a.ContextWindow(), "the codename table is xShellz-only")
}

// BORG_CONTEXT is the escape hatch for a backend borg can't interrogate.
func TestContextWindowOverride(t *testing.T) {
	a := NewWithLLM(&config.Config{Provider: config.ProviderOllama, Model: "qwen2.5-coder:32b", ContextWindow: 131_072}, &scriptedLLM{})
	require.Equal(t, 131_072, a.ContextWindow())

	// It outranks the catalog too — it exists for when borg has been told wrong.
	a.SetModelWindows([]llm.ModelInfo{{ID: "qwen2.5-coder:32b", MaxInputTokens: 8192}})
	require.Equal(t, 131_072, a.ContextWindow())

	// …and it still applies on the hosted provider.
	h := NewWithLLM(&config.Config{Model: "chuppa", ContextWindow: 200_000}, &scriptedLLM{})
	require.Equal(t, 200_000, h.ContextWindow())
}

// A live catalog still wins over the fallbacks when there is one.
func TestContextWindowFromCatalogStillWins(t *testing.T) {
	a := NewWithLLM(&config.Config{Provider: config.ProviderOllama, Model: "llama3.1:8b"}, &scriptedLLM{})
	require.Equal(t, 32_768, a.ContextWindow())
	a.SetModelWindows([]llm.ModelInfo{{ID: "llama3.1:8b", MaxInputTokens: 128_000}})
	require.Equal(t, 128_000, a.ContextWindow())
}

// The local path must not mention logging in — there is no account to log into,
// so a login hint would be both wrong and impossible to act on.
func TestLocalErrorsNeverMentionLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()

	cfg := &config.Config{Provider: config.ProviderOpenAI, BaseURL: srv.URL, LLMProxyURL: srv.URL, Model: "gpt-x"}
	client := llm.New(cfg, "sk-bad")
	defer client.CloseIdleConnections()
	a := NewWithLLM(cfg, client)
	a.SetUI(&captureUI{permit: AllowOnce})

	err := a.Ask(context.Background(), "hi")
	require.Error(t, err)
	require.NotContains(t, strings.ToLower(err.Error()), "auth login")
	require.Contains(t, err.Error(), "BORG_API_KEY")
}

// The context-window table keys off config.Codenames, which is the single list of
// what a codename is — the same one config validates BORG_MODEL against. A key
// here that isn't a codename would mean the two had drifted, and a window silently
// aimed at a model config would reject.
func TestContextWindowKeysAreAllCodenames(t *testing.T) {
	for name := range modelContextWindows {
		require.True(t, config.IsCodename(name), "%q is keyed as a hosted model but config doesn't know it as a codename", name)
	}
}
