package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/version"
)

func TestMain(m *testing.M) {
	backoffBase = time.Millisecond // keep retry backoff negligible in tests
	goleak.VerifyTestMain(m)
}

func TestSubmitFeedback(t *testing.T) {
	var gotAuth, gotBody string
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath, gotMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := New(&config.Config{APIBaseURL: srv.URL, LLMProxyURL: srv.URL + "/v1/llm"}, "tok-xyz")
	err := c.SubmitFeedback(context.Background(), "harness_problem", "the grep loop wasn't caught",
		map[string]string{"model": "chuppa"})
	require.NoError(t, err)
	require.Equal(t, "POST", gotMethod)
	require.Equal(t, "/v1/borg/feedback", gotPath)
	require.Equal(t, "Bearer tok-xyz", gotAuth)
	require.Contains(t, gotBody, "harness_problem")
	require.Contains(t, gotBody, "the grep loop wasn't caught")
	require.Contains(t, gotBody, "chuppa")
}

func TestSubmitFeedbackEndpointMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := New(&config.Config{APIBaseURL: srv.URL, LLMProxyURL: srv.URL}, "tok")
	err := c.SubmitFeedback(context.Background(), "harness_problem", "x", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "isn't available")
}

// A feedback 500 must surface as an HONEST, feedback-specific error — not the
// shared "llm request failed" wording (which once made a feedback 500 look like an
// LLM failure mid-session) — and must retry the transient 5xx to the bound.
func TestSubmitFeedbackServerError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New(&config.Config{APIBaseURL: srv.URL, LLMProxyURL: srv.URL}, "tok")
	err := c.SubmitFeedback(context.Background(), "harness_problem", "x", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "temporarily unavailable")
	require.NotContains(t, err.Error(), "llm") // honest: not an LLM failure
	require.Equal(t, int32(maxAttempts), hits.Load(), "a transient 5xx must be retried to the bound")
}

// A transient 5xx blip is retried, so a user-consented report still lands.
func TestSubmitFeedbackRetriesThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError) // first attempt blips
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c := New(&config.Config{APIBaseURL: srv.URL, LLMProxyURL: srv.URL}, "tok")
	require.NoError(t, c.SubmitFeedback(context.Background(), "harness_problem", "x", nil))
	require.Equal(t, int32(2), hits.Load())
}

// WindowHoursOrDefault fills in the documented rolling-24h window when the proxy
// omits/zeroes window_hours, so /usage never shows a nonsensical "rolling-0h".
func TestWindowHoursOrDefault(t *testing.T) {
	require.Equal(t, 24, (&AccountUsage{WindowHours: 0}).WindowHoursOrDefault())
	require.Equal(t, 24, (&AccountUsage{WindowHours: -5}).WindowHoursOrDefault())
	require.Equal(t, 48, (&AccountUsage{WindowHours: 48}).WindowHoursOrDefault())
}

func TestModelsAndTier(t *testing.T) {
	var gotModelsAuth, gotModelsPath, gotMePath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			gotModelsAuth, gotModelsPath = r.Header.Get("Authorization"), r.URL.Path
			_, _ = w.Write([]byte(`{"data":[{"id":"floko","label":"Floko","version":"3.2","min_tier":"free","available":true},{"id":"chuppa","label":"Chuppa","version":"3.2","min_tier":"pro","available":false}]}`))
		case strings.HasSuffix(r.URL.Path, "/usage"):
			_, _ = w.Write([]byte(`{"plan_code":"pro","window_hours":24,"percent_used":42,"credits_used":210,"credits_per_day":500}`))
		case strings.HasSuffix(r.URL.Path, "/v1/users/me"):
			gotMePath = r.URL.Path
			_, _ = w.Write([]byte(`{"plan_code":"pro"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// LLMProxyURL is the catalog base; APIBaseURL roots /v1/users/me.
	c := New(&config.Config{LLMProxyURL: srv.URL + "/v1/llm", APIBaseURL: srv.URL, Model: "floko"}, "tok")

	models, err := c.Models(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 2)
	require.Equal(t, "Floko", models[0].Label)
	require.Equal(t, "3.2", models[0].Version)
	require.True(t, models[0].Available)
	require.False(t, models[1].Available)
	require.Equal(t, "pro", models[1].MinTier)
	require.Equal(t, "Bearer tok", gotModelsAuth)
	require.Equal(t, "/v1/llm/models", gotModelsPath)

	tier, err := c.Tier(context.Background())
	require.NoError(t, err)
	require.Equal(t, "pro", tier)
	require.Equal(t, "/v1/users/me", gotMePath)

	usage, err := c.Usage(context.Background())
	require.NoError(t, err)
	require.Equal(t, "pro", usage.PlanCode)
	require.Equal(t, 24, usage.WindowHours)
	require.Equal(t, 42, usage.PercentUsed)
	require.Equal(t, 210, usage.CreditsUsed)
	require.Equal(t, 500, usage.CreditsPerDay)
}

func TestTierDefaultsAndErrors(t *testing.T) {
	// Empty plan_code -> "free".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"plan_code":""}`))
	}))
	defer srv.Close()
	c := New(&config.Config{APIBaseURL: srv.URL, LLMProxyURL: srv.URL}, "tok")
	tier, err := c.Tier(context.Background())
	require.NoError(t, err)
	require.Equal(t, "free", tier)

	// No API base configured -> error.
	c2 := New(&config.Config{}, "tok")
	_, err = c2.Tier(context.Background())
	require.Error(t, err)
}

func TestChatParsesDeltasAndAuth(t *testing.T) {
	var gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"choices":[{"delta":{"content":"Hello"}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{"content":", world"}}]}` + "\n\n" +
				`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":34}}` + "\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok-123")

	var sb strings.Builder
	msg, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(d string) {
		sb.WriteString(d)
	})
	require.NoError(t, err)
	require.Equal(t, "Hello, world", msg.Content)
	require.Equal(t, "Hello, world", sb.String())
	require.Empty(t, msg.ToolCalls)
	require.Equal(t, "Bearer tok-123", gotAuth)
	require.Equal(t, "floko", gotModel)
	require.NotNil(t, msg.Usage)
	require.Equal(t, 12, msg.Usage.PromptTokens)
	require.Equal(t, 34, msg.Usage.CompletionTokens)
}

// The chat request carries the borg version so the metered proxy can enforce a
// minimum supported version — except on dev builds, which never get gated.
func TestChatSendsVersionHeader(t *testing.T) {
	var gotVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVer = r.Header.Get("X-Turborg-Version")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"x"}}]}` + "\n\n" + "data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok")

	orig := version.Version
	defer func() { version.Version = orig }()

	version.Version = "1.2.3"
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.NoError(t, err)
	require.Equal(t, "1.2.3", gotVer)

	gotVer = "unset"
	version.Version = "dev"
	_, err = c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.NoError(t, err)
	require.Empty(t, gotVer, "dev builds must not send the version header")
}

// A transient mid-stream provider error (e.g. an engine hiccup during a long
// generation) must be retried — the whole request re-issued — not fatal, so a
// backend blip doesn't throw away the turn's work.
func TestChatRetriesTransientStreamError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			// First attempt: stream a bit, then an error event (mimics EngineCore).
			_, _ = w.Write([]byte(
				`data: {"choices":[{"delta":{"content":"Let me write"}}]}` + "\n\n" +
					`data: {"error":{"message":"EngineCore encountered an issue"}}` + "\n\n",
			))
			return
		}
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"done"}}]}` + "\n\n" + "data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok")
	msg, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.NoError(t, err)
	require.Equal(t, "done", msg.Content)    // the clean retry's result
	require.Equal(t, int32(2), calls.Load()) // re-issued exactly once
}

// A mid-stream error that never recovers is bounded — it surfaces after the cap
// rather than retrying forever.
func TestChatTransientStreamErrorBounded(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"error":{"message":"still broken"}}` + "\n\n"))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok")
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.Error(t, err)
	require.Contains(t, err.Error(), "still broken")
	require.Equal(t, int32(maxStreamAttempts), calls.Load()) // tried exactly the cap
}

// sseLoop builds an SSE body that streams the same delta `field` value many
// times, so the stream-side repetition guard should cut the turn short.
func sseLoop(field, line string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(`data: {"choices":[{"delta":{"` + field + `":"` + line + `"}}]}` + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestChatCutsContentRepetitionLoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseLoop("content", `Actually, I'll just write it now.\n`, 30)))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok")
	msg, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.NoError(t, err)
	require.Equal(t, FinishReasonRepetition, msg.FinishReason) // cut short, not run to [DONE]
}

func TestChatCutsReasoningRepetitionLoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// A loop in the hidden reasoning channel (not streamed to the UI) is cut too.
		_, _ = w.Write([]byte(sseLoop("reasoning_content", `I should double-check the listeners directory.\n`, 30)))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok")
	msg, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.NoError(t, err)
	require.Equal(t, FinishReasonRepetition, msg.FinishReason)
}

func TestChatToolChoiceOption(t *testing.T) {
	var gotChoice string
	bodyHas := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ToolChoice string `json:"tool_choice"`
		}
		raw, _ := io.ReadAll(r.Body)
		bodyHas = strings.Contains(string(raw), `"tool_choice"`)
		_ = json.Unmarshal(raw, &body)
		gotChoice = body.ToolChoice
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok")

	// With the option, tool_choice:"required" is sent.
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {}, ForceToolChoice("required"))
	require.NoError(t, err)
	require.Equal(t, "required", gotChoice)

	// Without it, tool_choice is omitted entirely (=> the proxy/model default, "auto").
	_, err = c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.NoError(t, err)
	require.False(t, bodyHas, "tool_choice must be omitted when no option is passed")
}

func TestChatWithEffortOption(t *testing.T) {
	var gotEffort string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ReasoningEffort string `json:"reasoning_effort"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		gotEffort = body.ReasoningEffort
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok")
	c.SetEffort("none") // session baseline

	// WithEffort overrides the session-level effort for this one request.
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {}, WithEffort("high"))
	require.NoError(t, err)
	require.Equal(t, "high", gotEffort)

	// Without the option, the request carries the session baseline.
	_, err = c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.NoError(t, err)
	require.Equal(t, "none", gotEffort)
}

func TestChatAccumulatesToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// A tool call streamed in fragments: id+name first, arguments split.
		_, _ = w.Write([]byte(
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file"}}]}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"go.mod\"}"}}]}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "chuppa"}, "tok")
	msg, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "read go.mod"}}, nil, false, func(string) {})
	require.NoError(t, err)
	require.Len(t, msg.ToolCalls, 1)
	require.Equal(t, "call_1", msg.ToolCalls[0].ID)
	require.Equal(t, "read_file", msg.ToolCalls[0].Function.Name)
	require.JSONEq(t, `{"path":"go.mod"}`, msg.ToolCalls[0].Function.Arguments)
	require.Equal(t, "tool_calls", msg.FinishReason) // captured from the stream
}

func TestChatSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"budget_exhausted","message":"Daily LLM token budget exhausted."}}`))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok")
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.Error(t, err)
	require.Contains(t, err.Error(), "budget_exhausted")
	require.Contains(t, err.Error(), "429")
}

func TestChatRetriesTransientThenSucceeds(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // 503 on the first try
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok")
	msg, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.NoError(t, err)
	require.Contains(t, msg.Content, "hi")
	require.Equal(t, int32(2), atomic.LoadInt32(&n)) // one retry, then success
}

func TestChatGivesUpAfterMaxRetries(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusBadGateway) // 502 every time
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok")
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.Error(t, err)
	require.Contains(t, err.Error(), "502")
	require.Equal(t, int32(maxAttempts), atomic.LoadInt32(&n)) // bounded
}

func TestChatDoesNotRetryBudget(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"budget_exhausted","message":"spent"}}`))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok")
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.Error(t, err)
	require.Contains(t, err.Error(), "budget_exhausted")
	require.Equal(t, int32(1), atomic.LoadInt32(&n)) // 429 budget → not retried
}

func TestChatParsesCachedTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
				`data: {"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":900}}}` + "\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok")
	msg, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.NoError(t, err)
	require.NotNil(t, msg.Usage)
	require.Equal(t, 1000, msg.Usage.PromptTokens)
	require.Equal(t, 900, msg.Usage.CachedTokens) // prompt-cache hit surfaced
}

func TestChatSendsReasoningEffort(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok")
	c.SetEffort("xhigh")
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.NoError(t, err)
	require.Contains(t, gotBody, `"reasoning_effort":"xhigh"`)
}

// Chat sends a prompt_cache_key derived from the system prompt (a prefix-cache
// routing hint the proxy forwards). It's stable across steps of one conversation,
// and absent when there's no leading system message.
func TestChatSendsPromptCacheKey(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok")
	sys := Message{Role: "system", Content: "You are borg working in /repo."}
	// Two steps of the SAME conversation (history grows) → identical cache key.
	_, err := c.Chat(context.Background(), []Message{sys, {Role: "user", Content: "a"}}, nil, false, func(string) {})
	require.NoError(t, err)
	_, err = c.Chat(context.Background(), []Message{sys, {Role: "user", Content: "a"}, {Role: "assistant", Content: "ok"}, {Role: "user", Content: "b"}}, nil, false, func(string) {})
	require.NoError(t, err)
	require.Len(t, bodies, 2)
	key := promptCacheKey([]Message{sys})
	require.NotEmpty(t, key)
	require.Contains(t, bodies[0], `"prompt_cache_key":"`+key+`"`)
	require.Contains(t, bodies[1], `"prompt_cache_key":"`+key+`"`, "the key is stable across a conversation's steps")
}

// promptCacheKey is stable per system prompt, distinct across system prompts, and
// empty when there's no leading system message (then the field is omitted).
func TestPromptCacheKey(t *testing.T) {
	a := promptCacheKey([]Message{{Role: "system", Content: "prompt A"}})
	b := promptCacheKey([]Message{{Role: "system", Content: "prompt A"}})
	c := promptCacheKey([]Message{{Role: "system", Content: "prompt B"}})
	require.NotEmpty(t, a)
	require.Equal(t, a, b, "same system prompt → same key")
	require.NotEqual(t, a, c, "different system prompt → different key")
	require.Empty(t, promptCacheKey([]Message{{Role: "user", Content: "no system"}}))
	require.Empty(t, promptCacheKey(nil))
}

func TestSetModel(t *testing.T) {
	c := New(&config.Config{LLMProxyURL: "http://x", Model: "floko"}, "tok")
	require.Equal(t, "floko", c.Model())
	c.SetModel("chuppa")
	require.Equal(t, "chuppa", c.Model())
}

func TestChatSurfacesNonJSONError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream is down"))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok")
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.Error(t, err)
	require.Contains(t, err.Error(), "502")
}

func TestChatSurfacesInStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"error":{"message":"upstream error (500)"}}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok")
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.Error(t, err)
	require.Contains(t, err.Error(), "upstream error")
}
