package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/version"
)

// fakeBackend is a minimal OpenAI-compatible server: it records every request it
// receives (path + raw body) and streams back a canned completion. It stands in
// for Ollama/LM Studio/llama.cpp/OpenAI — they all speak this API, which is the
// whole reason one client can serve them.
type fakeBackend struct {
	srv *httptest.Server
	// bodies holds the RAW request body per path, so assertions can be made on the
	// JSON actually put on the wire rather than on the struct borg intended to
	// send — the distinction that matters for a field that must be ABSENT.
	mu     sync.Mutex
	bodies map[string]string
	paths  []string
	auth   []string
	status int // when non-zero, respond with this instead of a completion
	body   string
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	f := &fakeBackend{bodies: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies[r.URL.Path] = string(raw)
		f.paths = append(f.paths, r.URL.Path)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		status, body := f.status, f.body
		f.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			// The bare shape a plain OpenAI-compatible server serves: an id and
			// nothing else borg's catalog UI wants.
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen2.5-coder:7b","object":"model"},{"id":"llama3.1:8b"}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"choices":[{"delta":{"content":"hello"},"finish_reason":"stop"}]}` + "\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// bodyFor returns the decoded JSON of the last request to path, plus the raw text.
func (f *fakeBackend) bodyFor(t *testing.T, path string) (map[string]any, string) {
	t.Helper()
	f.mu.Lock()
	raw := f.bodies[path]
	f.mu.Unlock()
	require.NotEmpty(t, raw, "no request was made to %s", path)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &m))
	return m, raw
}

func (f *fakeBackend) hits() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

func TestCapabilitiesFor(t *testing.T) {
	// The hosted proxy is the only backend with an account behind it, and the only
	// one that understands borg's non-standard extras.
	x := CapabilitiesFor(KindXShellz)
	require.Equal(t, Capabilities{
		RequiresAuth: true, AccountEndpoints: true, ReasoningEffort: true,
		ForcedToolChoice: true, PromptCacheKey: true,
	}, x)

	// An unknown/empty kind must fall back to the hosted default (Config rejects a
	// bad BORG_PROVIDER long before this, so this is only a safety net).
	require.Equal(t, x, CapabilitiesFor(""))
	require.Equal(t, x, CapabilitiesFor("nonsense"))

	// Bring-your-own: never an account, never borg's extras.
	for _, kind := range []string{KindOllama, KindOpenAI, KindOpenRouter, KindCustom} {
		c := CapabilitiesFor(kind)
		require.False(t, c.RequiresAuth, "%s must not require a login", kind)
		require.False(t, c.AccountEndpoints, "%s has no account routes", kind)
		require.False(t, c.PromptCacheKey, "%s: prompt_cache_key is a proxy hint", kind)
		require.False(t, c.ReasoningEffort, "%s: reasoning_effort/think are not portable", kind)
	}
	// The gateways honor tool_choice; the local/unknown ones are not assumed to.
	require.True(t, CapabilitiesFor(KindOpenAI).ForcedToolChoice)
	require.True(t, CapabilitiesFor(KindOpenRouter).ForcedToolChoice)
	require.False(t, CapabilitiesFor(KindOllama).ForcedToolChoice)
	require.False(t, CapabilitiesFor(KindCustom).ForcedToolChoice)
}

// byoClient builds a client for a bring-your-own provider pointed at the fake.
func byoClient(t *testing.T, f *fakeBackend, provider, key string) *Client {
	t.Helper()
	cfg := &config.Config{Provider: provider, BaseURL: f.srv.URL, LLMProxyURL: f.srv.URL, Model: "qwen2.5-coder:7b"}
	c := New(cfg, key)
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// The non-standard fields must be ABSENT from the wire — not zero, not null.
// Most local servers reject a body with unknown fields (400) rather than ignoring
// them, so a leaked `think` would break every request against Ollama/llama.cpp.
func TestLocalRequestOmitsNonStandardFields(t *testing.T) {
	for _, provider := range []string{KindOllama, KindCustom, KindOpenAI, KindOpenRouter} {
		t.Run(provider, func(t *testing.T) {
			f := newFakeBackend(t)
			c := byoClient(t, f, provider, "")
			c.SetEffort("high") // session effort set: must still not reach a BYO backend

			// think=true AND a per-turn effort override — the two ways the fields
			// could reach the wire — both gated off.
			_, err := c.Chat(context.Background(), []Message{
				{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"},
			}, nil, true, func(string) {}, WithEffort("xhigh"))
			require.NoError(t, err)

			body, raw := f.bodyFor(t, "/chat/completions")
			for _, field := range []string{"think", "reasoning_effort", "prompt_cache_key"} {
				require.NotContains(t, body, field, "%s must be absent from a %s request", field, provider)
				require.NotContains(t, raw, field, "%s must not appear in the raw JSON", field)
			}
			// The standard fields are of course still there.
			require.Equal(t, "qwen2.5-coder:7b", body["model"])
			require.Equal(t, true, body["stream"])
			require.Contains(t, body, "messages")
		})
	}
}

// tool_choice is only sent to backends known to honor it.
func TestToolChoiceGatedByProvider(t *testing.T) {
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "finish", Parameters: json.RawMessage(`{}`)}}}
	for provider, want := range map[string]bool{
		KindOllama: false, KindCustom: false, KindOpenAI: true, KindOpenRouter: true,
	} {
		t.Run(provider, func(t *testing.T) {
			f := newFakeBackend(t)
			c := byoClient(t, f, provider, "")
			_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}},
				tools, false, func(string) {}, ForceToolChoice("required"))
			require.NoError(t, err)

			body, _ := f.bodyFor(t, "/chat/completions")
			if want {
				require.Equal(t, "required", body["tool_choice"])
			} else {
				require.NotContains(t, body, "tool_choice",
					"%s doesn't advertise forced tool_choice, so borg must leave the choice to the model", provider)
			}
		})
	}
}

// A backend that advertises tool_choice can still 400 on the value: borg must
// drop the field and retry ONCE rather than lose the turn over an optimization.
func TestChatRetriesWithoutToolChoiceOn400(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"tool_choice is not supported"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n"))
	}))
	defer srv.Close()

	var dbg strings.Builder
	c := New(&config.Config{Provider: KindOpenAI, BaseURL: srv.URL, LLMProxyURL: srv.URL, Model: "gpt-x"}, "sk-1")
	defer c.CloseIdleConnections()
	c.SetDebug(func(s string) { dbg.WriteString(s + "\n") })

	msg, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}},
		nil, false, func(string) {}, ForceToolChoice("required"))
	require.NoError(t, err, "the 400 must be recovered, not surfaced")
	require.Equal(t, "ok", msg.Content)

	require.Equal(t, int32(2), calls.Load(), "exactly one retry — not a loop")
	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, bodies[0], "tool_choice", "the first attempt carried it")
	require.NotContains(t, bodies[1], "tool_choice", "the retry dropped it")
	require.Contains(t, dbg.String(), "retrying once without it")
}

// The fallback is for 400 ONLY: a 400 with no tool_choice to blame must surface.
func TestChat400WithoutToolChoiceIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"context length exceeded"}}`))
	}))
	defer srv.Close()

	c := New(&config.Config{Provider: KindOllama, BaseURL: srv.URL, LLMProxyURL: srv.URL, Model: "m"}, "")
	defer c.CloseIdleConnections()
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.Error(t, err)
	require.Contains(t, err.Error(), "context length exceeded")
	require.Equal(t, int32(1), calls.Load(), "a plain 400 is terminal — no retry")
}

// REGRESSION GUARD: the hosted path must be byte-for-byte what it always was.
// Every extra borg sends the proxy is still sent, exactly as before.
func TestXShellzRequestStillSendsEverything(t *testing.T) {
	f := newFakeBackend(t)
	cfg := &config.Config{APIBaseURL: f.srv.URL, LLMProxyURL: f.srv.URL, Model: "chuppa"} // Provider "" = hosted default
	c := New(cfg, "tok-abc")
	defer c.CloseIdleConnections()
	c.SetEffort("high")

	_, err := c.Chat(context.Background(), []Message{
		{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"},
	}, nil, true, func(string) {}, ForceToolChoice("required"))
	require.NoError(t, err)

	body, _ := f.bodyFor(t, "/chat/completions")
	require.Equal(t, "chuppa", body["model"])
	require.Equal(t, true, body["think"], "think is an xShellz-proxy field and must still be sent")
	require.Equal(t, "high", body["reasoning_effort"])
	require.Equal(t, "required", body["tool_choice"])
	require.Equal(t, promptCacheKey([]Message{{Role: "system", Content: "sys"}}), body["prompt_cache_key"],
		"the prefix-cache routing hint must be unchanged")

	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, []string{"Bearer tok-abc"}, f.auth, "the bearer is still sent")
}

// An explicit effort of "" (follow the think toggle) is omitted on the hosted
// path exactly as before — the caps gate must not turn it into a sent empty.
func TestXShellzOmitsEmptyEffort(t *testing.T) {
	f := newFakeBackend(t)
	c := New(&config.Config{APIBaseURL: f.srv.URL, LLMProxyURL: f.srv.URL, Model: "floko"}, "tok")
	defer c.CloseIdleConnections()

	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.NoError(t, err)
	body, _ := f.bodyFor(t, "/chat/completions")
	require.NotContains(t, body, "reasoning_effort")
	require.NotContains(t, body, "think", "think=false is omitempty, as it always was")
}

// The account routes must never be REQUESTED off-platform: a plain server would
// only 404 them, and borg would be probing someone else's box for xShellz URLs.
func TestAccountEndpointsAreNeverCalledOffPlatform(t *testing.T) {
	f := newFakeBackend(t)
	c := byoClient(t, f, KindOllama, "")

	// Usage / feedback: a typed refusal, no request.
	_, err := c.Usage(context.Background())
	require.ErrorIs(t, err, ErrNoMetering)
	require.ErrorIs(t, c.SubmitFeedback(context.Background(), "harness_problem", "x", nil), ErrNoMetering)

	// UserInfo / Tier: an empty answer, no request, no error.
	u, err := c.UserInfo(context.Background())
	require.NoError(t, err)
	require.Equal(t, &UserInfo{}, u, "no account ⇒ no identity, and NOT a fabricated 'free' plan")

	tier, err := c.Tier(context.Background())
	require.NoError(t, err)
	require.Empty(t, tier)

	require.Empty(t, f.hits(), "not a single HTTP request may be made for account data")
}

// Off-platform, /models is still served (every compatible backend has it) but the
// entries are bare — that's not an error, and the models must not read as locked.
func TestModelsAcceptsBareCatalog(t *testing.T) {
	f := newFakeBackend(t)
	c := byoClient(t, f, KindOllama, "")

	models, err := c.Models(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 2)

	require.Equal(t, "qwen2.5-coder:7b", models[0].ID)
	require.Equal(t, "qwen2.5-coder:7b", models[0].Label, "the id doubles as the label")
	require.True(t, models[0].Available, "your own models are never plan-gated")
	require.Empty(t, models[0].MinTier)
	require.Zero(t, models[0].MaxInputTokens, "no window claimed ⇒ the agent falls back conservatively")
	require.Equal(t, "llama3.1:8b", models[1].ID)
	require.Equal(t, []string{"/models"}, f.hits())
}

// The hosted catalog is passed through verbatim — tiers and all.
func TestModelsHostedCatalogUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"chuppa_pro","label":"Chuppa Pro","min_tier":"starter","available":false,"max_input_tokens":1048576}]}`))
	}))
	defer srv.Close()
	c := New(&config.Config{APIBaseURL: srv.URL, LLMProxyURL: srv.URL, Model: "chuppa"}, "tok")
	defer c.CloseIdleConnections()

	models, err := c.Models(context.Background())
	require.NoError(t, err)
	require.Equal(t, []ModelInfo{{
		ID: "chuppa_pro", Label: "Chuppa Pro", MinTier: "starter",
		Available: false, MaxInputTokens: 1048576,
	}}, models, "the hosted catalog must not be rewritten by the bare-catalog normalizer")
}

// A local daemon wants no Authorization header at all; a bare "Bearer " is worse
// than nothing (some servers parse it and reject the empty credential).
func TestNoAuthHeaderWithoutAKey(t *testing.T) {
	f := newFakeBackend(t)
	c := byoClient(t, f, KindOllama, "")
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.NoError(t, err)

	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, []string{""}, f.auth, "no key ⇒ no Authorization header")
}

// A BYO key is sent as a normal bearer (OpenAI/OpenRouter need it).
func TestByoKeyIsSentAsBearer(t *testing.T) {
	f := newFakeBackend(t)
	c := byoClient(t, f, KindOpenAI, "sk-secret")
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.NoError(t, err)

	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, []string{"Bearer sk-secret"}, f.auth)
}

// borg's version header is a handshake with its own proxy — nobody else's server
// has any use for it, so it isn't announced to third parties.
func TestVersionHeaderOnlyGoesToTheProxy(t *testing.T) {
	var mu sync.Mutex
	var hdr []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hdr = append(hdr, r.Header.Get("X-Turborg-Version"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	old := version.Version
	version.Version = "9.9.9"
	defer func() { version.Version = old }()

	hosted := New(&config.Config{APIBaseURL: srv.URL, LLMProxyURL: srv.URL, Model: "m"}, "t")
	defer hosted.CloseIdleConnections()
	_, err := hosted.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.NoError(t, err)

	byo := New(&config.Config{Provider: KindOllama, BaseURL: srv.URL, LLMProxyURL: srv.URL, Model: "m"}, "")
	defer byo.CloseIdleConnections()
	_, err = byo.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"9.9.9", ""}, hdr)
}

// Name/RequiresAuth back the CLI's decision to skip the whole auth stack.
func TestProviderIdentity(t *testing.T) {
	f := newFakeBackend(t)
	require.True(t, New(&config.Config{}, "t").RequiresAuth())
	require.Equal(t, KindXShellz, New(&config.Config{}, "t").Name())

	c := byoClient(t, f, KindOllama, "")
	require.False(t, c.RequiresAuth())
	require.Equal(t, KindOllama, c.Name())
	require.Equal(t, CapabilitiesFor(KindOllama), c.Caps())
}
