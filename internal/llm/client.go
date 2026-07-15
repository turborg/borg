// Package llm talks to the metered, OpenAI-compatible xShellz proxy.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/version"
)

// Client calls the metered proxy at cfg.LLMProxyURL. The proxy authenticates the
// access token, maps the public model codename to the real provider, meters
// usage, and streams an OpenAI-compatible response back.
type Client struct {
	baseURL string
	apiBase string // {API} root, for non-LLM calls like /v1/users/me
	token   string
	model   string
	effort  string // explicit reasoning_effort; "" lets the proxy map the think toggle
	http    *http.Client
	debug   func(string) // verbose diagnostics sink (nil = off)
}

// timeToFirstByte caps how long we wait for the response headers — i.e. a proxy
// or model that never starts replying. It does NOT bound the stream itself.
const timeToFirstByte = 120 * time.Second

// idleTimeout aborts a stream that goes silent mid-flight (no SSE line for this
// long). It is reset on every chunk, so a long-but-progressing generation runs
// as long as it needs — unlike an http.Client.Timeout, which covers the whole
// body read and would cut a healthy long reply (e.g. generating a full file).
const idleTimeout = 120 * time.Second

// New builds a Client for the configured proxy and access token.
func New(cfg *config.Config, accessToken string) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = timeToFirstByte
	return &Client{
		baseURL: strings.TrimRight(cfg.LLMProxyURL, "/"),
		apiBase: strings.TrimRight(cfg.APIBaseURL, "/"),
		token:   accessToken,
		model:   cfg.Model,
		// No http.Client.Timeout: it spans the entire body read, which for an
		// SSE stream is a hard cap on total generation time. TTFB is guarded by
		// the transport; mid-stream stalls by the idle watchdog in Chat.
		http: &http.Client{Transport: tr},
	}
}

// SetModel changes the model codename used for subsequent requests.
func (c *Client) SetModel(model string) { c.model = model }

// SetEffort sets an explicit reasoning_effort (none|low|medium|high|xhigh) for
// subsequent requests; "" lets the proxy derive it from the think toggle.
func (c *Client) SetEffort(effort string) { c.effort = effort }

// CloseIdleConnections closes any keep-alive connections the client is holding
// idle in its transport pool, ending their reader goroutines. borg's long-lived
// process reuses these (so it never calls this), but a short-lived caller — the
// eval harness, which spins up a client per model — closes them when done so the
// pooled HTTP/2 readLoop goroutine doesn't linger and trip goroutine-leak checks.
func (c *Client) CloseIdleConnections() { c.http.CloseIdleConnections() }

// SetDebug sets the verbose-diagnostics sink (nil disables it). When set, Chat
// emits a per-request summary and the model's assembled reasoning. The request
// summary deliberately excludes the Authorization header, so the caller's bearer
// token is never logged; the provider key never reaches the client at all.
func (c *Client) SetDebug(fn func(string)) { c.debug = fn }

// Model returns the current model codename.
func (c *Client) Model() string { return c.model }

// ModelInfo is one entry in the borg model catalog (GET /v1/llm/models).
type ModelInfo struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Version     string `json:"version"`
	Description string `json:"description"`
	MinTier     string `json:"min_tier"`
	Available   bool   `json:"available"`
	// MaxInputTokens is the model's context-window cap (server-authoritative), so
	// borg's /context bar measures against the real window instead of a hardcoded
	// guess. 0 when an older accounts-api omits it (borg falls back to a constant).
	MaxInputTokens int `json:"max_input_tokens"`
	// BurnRate is the model's relative budget-burn multiplier (cheapest model = 1),
	// computed server-side from prices. borg warns when switching to a high-burn
	// model (e.g. Axiom ≈ 13). 0/absent on older accounts-api → no warning.
	BurnRate int `json:"burn_rate"`
}

// Models fetches the model catalog: labels, versions, and per-plan availability
// for the authenticated user.
func (c *Client) Models(ctx context.Context) ([]ModelInfo, error) {
	var body struct {
		Data []ModelInfo `json:"data"`
	}
	if err := c.getJSON(ctx, c.baseURL+"/models", &body); err != nil {
		return nil, err
	}
	return body.Data, nil
}

// UserInfo is the caller's identity + plan from /v1/users/me. Name/Email are
// best-effort (present when the API exposes them); an absent plan reports "free".
type UserInfo struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	PlanCode string `json:"plan_code"`
}

// UserInfo fetches the caller's identity and plan from /v1/users/me.
func (c *Client) UserInfo(ctx context.Context) (*UserInfo, error) {
	if c.apiBase == "" {
		return nil, fmt.Errorf("no API base URL configured")
	}
	var u UserInfo
	if err := c.getJSON(ctx, c.apiBase+"/v1/users/me", &u); err != nil {
		return nil, err
	}
	if u.PlanCode == "" {
		u.PlanCode = "free"
	}
	return &u, nil
}

// Tier returns the caller's plan code (free, starter, pro, max) from
// /v1/users/me; an absent plan reports as "free".
func (c *Client) Tier(ctx context.Context) (string, error) {
	u, err := c.UserInfo(ctx)
	if err != nil {
		return "", err
	}
	return u.PlanCode, nil
}

// AccountUsage is the caller's shared rolling-24h LLM budget — summed across all
// models, not per-model. The budget is metered in credits (a friendly rename of
// the internal per-model dollar cost); raw tokens are no longer exposed.
type AccountUsage struct {
	PlanCode    string `json:"plan_code"`
	WindowHours int    `json:"window_hours"`
	// PercentUsed is 0-100 of the daily budget (0 when the budget is unlimited).
	PercentUsed int `json:"percent_used"`
	// CreditsUsed / CreditsPerDay drive the "X / Y credits" display.
	CreditsUsed   int `json:"credits_used"`
	CreditsPerDay int `json:"credits_per_day"`
}

// WindowHoursOrDefault returns the budget window, defaulting to 24 when the proxy
// omits or zeroes window_hours — the budget is documented as rolling-24h, so a 0
// would otherwise render a nonsensical "rolling-0h".
func (u *AccountUsage) WindowHoursOrDefault() int {
	if u.WindowHours <= 0 {
		return 24
	}
	return u.WindowHours
}

// Usage fetches the account's plan and rolling-24h credit budget from the
// metered proxy (GET /usage).
func (c *Client) Usage(ctx context.Context) (*AccountUsage, error) {
	var u AccountUsage
	if err := c.getJSON(ctx, c.baseURL+"/usage", &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// getJSON does an authenticated GET and decodes a JSON body, with a short
// timeout (these are quick metadata calls, not streams).
// SubmitFeedback POSTs a user-approved feedback report (e.g. a harness problem) to
// the accounts-api feedback endpoint under the caller's bearer. It is only ever
// called after explicit user consent. A 404 (endpoint not deployed yet) surfaces as
// a clear error the UI can show, rather than failing silently.
func (c *Client) SubmitFeedback(ctx context.Context, kind, report string, meta map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	payload, err := json.Marshal(map[string]any{"kind": kind, "report": report, "meta": meta})
	if err != nil {
		return err
	}
	url := c.apiBase + "/v1/borg/feedback"
	if c.debug != nil {
		c.debug(fmt.Sprintf("→ POST %s kind=%q  %d bytes", url, kind, len(payload)))
	}
	// Bounded transient retry (mirrors postWithRetry) so a 5xx/network blip doesn't
	// lose a user-consented report. Errors are feedback-specific and honest — NOT
	// routed through errorFromResponse, whose "llm request failed" wording is wrong
	// for this endpoint and once surfaced a feedback 500 as a bogus LLM failure.
	var lastErr error
	var retryAfter time.Duration
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, backoff(attempt, retryAfter)); err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err() // cancelled — not a transient failure
			}
			lastErr = fmt.Errorf("couldn't reach the feedback service: %w", err)
			continue // transient transport error → retry
		}
		status, statusLine := resp.StatusCode, resp.Status
		retryAfter = parseRetryAfter(resp.Header)
		_ = resp.Body.Close()
		if c.debug != nil && (status < 200 || status >= 300) {
			c.debug(fmt.Sprintf("✗ feedback POST %d", status))
		}
		switch {
		case status >= 200 && status < 300:
			return nil
		case status == http.StatusNotFound:
			return errors.New("the feedback endpoint isn't available yet on this server")
		case status == http.StatusUnauthorized:
			// Keep the actionable re-auth hint (a bearer is sent, so 401 is reachable).
			return errors.New("session expired — run `borg auth login` to re-authenticate (in the REPL: /login)")
		case retryableStatus(status) && attempt < maxAttempts-1:
			lastErr = fmt.Errorf("the feedback service is temporarily unavailable (%s)", statusLine)
			continue // transient 5xx/408 → retry
		case status >= 500:
			return errors.New("the feedback service is temporarily unavailable")
		default:
			return fmt.Errorf("couldn't send the feedback report (%s)", statusLine)
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return errors.New("the feedback service is temporarily unavailable")
}

func (c *Client) getJSON(ctx context.Context, url string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errorFromResponse(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Message is one conversation turn (OpenAI shape).
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
	// Usage is populated on the assistant reply returned by Chat, from the
	// stream's final usage chunk. Never sent on the wire (it's a response stat).
	Usage *Usage `json:"-"`
	// FinishReason is the stream's terminal reason on the assistant reply
	// (stop|length|tool_calls|content_filter|malformed_function_call). Never sent
	// on the wire (DeepInfra rejects unknown assistant fields).
	FinishReason string `json:"-"`
}

// Usage is the token accounting for one completion (from the proxy's
// include_usage final chunk).
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int // input tokens served from the prompt cache (prefix reuse)
}

// ToolCall is a function call the model requested.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction is the called function name + raw JSON arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is a tool definition advertised to the model (OpenAI shape).
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction names + describes a tool and its JSON-schema parameters.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatRequest struct {
	Model           string    `json:"model"`
	Messages        []Message `json:"messages"`
	Stream          bool      `json:"stream"`
	Think           bool      `json:"think,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"` // explicit level; overrides Think on the proxy
	Tools           []Tool    `json:"tools,omitempty"`
	ToolChoice      string    `json:"tool_choice,omitempty"`      // usually empty => proxy/model default ("auto")
	PromptCacheKey  string    `json:"prompt_cache_key,omitempty"` // prefix-cache routing hint, forwarded by the proxy
}

// promptCacheKey derives a STABLE prefix-cache routing key from the conversation's
// system prompt (messages[0]). It's constant across every step of a session (the
// system prompt doesn't change), and identical for sessions that share a system
// prompt (same repo + model + BORG.md) — so requests that share the cacheable
// system+tools prefix prefer the same backend instance and keep that prefill hot,
// raising the prefix-cache hit rate that automatic caching alone leaves on the table
// (~46% observed). The metered proxy forwards this to the provider; automatic prefix
// caching still does the real work. Empty when there's no leading system message
// (then the field is omitted and behavior is unchanged).
func promptCacheKey(msgs []Message) string {
	if len(msgs) == 0 || msgs[0].Role != "system" || msgs[0].Content == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(msgs[0].Content))
	return "borg-" + hex.EncodeToString(sum[:8])
}

// ChatOption tweaks a single Chat request. It's the per-turn seam the agent uses
// to force guided/structured tool-calling on a leak-retry without making it the
// default — so ordinary turns stay on the model's default ("auto") and can stream
// a plain-text answer instead of being funneled through a tool call.
type ChatOption func(*chatRequest)

// ForceToolChoice sets the request's tool_choice (e.g. "required"). The metered
// proxy honors an explicit tool_choice; borg sends "required" only to re-issue a
// turn the model botched as a text/leaked tool call, so the backend's guided
// decoding makes the retry a valid structured call.
func ForceToolChoice(choice string) ChatOption {
	return func(r *chatRequest) { r.ToolChoice = choice }
}

// WithEffort overrides reasoning_effort for a single request, overriding the
// client's session-level effort. The agent uses it to auto-escalate reasoning on
// a turn that's visibly struggling, without permanently changing the dev's chosen
// effort/think setting.
func WithEffort(level string) ChatOption {
	return func(r *chatRequest) { r.ReasoningEffort = level }
}

// maxAttempts bounds the streaming request's transient-failure retries (1 initial
// + retries). backoffBase is a var so tests can shrink the waits.
const maxAttempts = 3

// maxStreamAttempts bounds how many times Chat re-issues the WHOLE request after a
// transient MID-STREAM failure — a provider `error` event, or an idle-watchdog
// stall that isn't a caller cancel. postWithRetry only retries the initial
// connection; this covers a blip after the stream has started (e.g. during the
// heavy /learn write), so one transient hiccup doesn't discard the turn's work.
const maxStreamAttempts = 3

var backoffBase = 200 * time.Millisecond

// streamRetryErr marks a mid-stream failure as transient — worth re-issuing the
// identical request. A plain (unwrapped) error from streamOnce is treated as
// terminal and surfaced as-is.
type streamRetryErr struct{ err error }

func (e *streamRetryErr) Error() string { return e.err.Error() }
func (e *streamRetryErr) Unwrap() error { return e.err }

func retryableStream(err error) error { return &streamRetryErr{err} }

func isRetryableStream(err error) bool {
	var e *streamRetryErr
	return errors.As(err, &e)
}

// postWithRetry POSTs body and returns a 200 response, retrying TRANSIENT
// failures — network errors, 408, and 5xx — with exponential backoff + jitter,
// honoring a server Retry-After. It does not retry 4xx (including 429, which here
// means the daily budget is spent — retrying wouldn't help and would just delay
// the clear error), and never retries mid-stream (only the initial response).
func (c *Client) postWithRetry(ctx context.Context, url string, body []byte) (*http.Response, error) {
	var lastErr error
	var retryAfter time.Duration
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, backoff(attempt, retryAfter)); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		// Let the metered proxy enforce a minimum supported version (426 ⇒ the
		// agent loop surfaces an "update" message). Dev builds send nothing so a
		// version gate never blocks local development.
		if v := version.Version; v != "" && v != "dev" {
			req.Header.Set("X-Turborg-Version", v)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err() // cancelled (Ctrl-C) — not a transient failure
			}
			lastErr = fmt.Errorf("llm request: %w", err)
			continue // transient transport error → retry
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		if !retryableStatus(resp.StatusCode) || attempt == maxAttempts-1 {
			err := errorFromResponse(resp)
			_ = resp.Body.Close()
			return nil, err
		}
		retryAfter = parseRetryAfter(resp.Header)
		lastErr = errorFromResponse(resp)
		_ = resp.Body.Close()
	}
	return nil, lastErr
}

// retryableStatus reports whether an HTTP status is worth retrying (transient).
func retryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, // 408 (e.g. an Octane worker blocked)
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout: // 500/502/503/504
		return true
	}
	return false
}

// backoff is exponential (backoffBase·2^(attempt-1)) with up to ~50% jitter,
// raised to any server Retry-After and capped at 10s.
func backoff(attempt int, retryAfter time.Duration) time.Duration {
	d := backoffBase * time.Duration(1<<uint(attempt-1))
	d += time.Duration(rand.Int63n(int64(d) + 1)) //nolint:gosec // jitter, not crypto
	if retryAfter > d {
		d = retryAfter
	}
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	return d
}

// parseRetryAfter reads a Retry-After header (seconds or HTTP-date).
func parseRetryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// sleep waits d or returns early if ctx is cancelled.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Chat streams a completion, invoking onDelta for each content chunk as it
// arrives (for live display), and returns the assembled assistant message —
// including any tool calls the model requested.
func (c *Client) Chat(ctx context.Context, msgs []Message, tools []Tool, think bool, onDelta func(string), opts ...ChatOption) (*Message, error) {
	req := chatRequest{Model: c.model, Messages: msgs, Stream: true, Think: think, ReasoningEffort: c.effort, Tools: tools, PromptCacheKey: promptCacheKey(msgs)}
	for _, opt := range opts {
		opt(&req)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if c.debug != nil {
		// Request SUMMARY only (no headers → no bearer token leaked; the body holds
		// the caller's own messages/tools).
		c.debug(fmt.Sprintf("→ POST %s/chat/completions  model=%s effort=%q think=%v tool_choice=%q  %d msgs  %d tools  %d bytes",
			c.baseURL, c.model, req.ReasoningEffort, think, req.ToolChoice, len(msgs), len(tools), len(body)))
	}

	// Re-issue the identical request on a TRANSIENT mid-stream failure — a provider
	// `error` event, or an idle stall that isn't a caller cancel. Distinct from
	// postWithRetry (which only retries the INITIAL connection): a blip after the
	// stream has started (e.g. during the heavy /learn write) shouldn't discard the
	// whole turn. Bounded; a caller cancel (Esc/Ctrl-C) is never retried. A retried
	// step may re-stream its (usually tiny) pre-failure content — acceptable versus
	// losing the turn.
	var lastErr error
	for attempt := 0; attempt < maxStreamAttempts; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, backoff(attempt, 0)); err != nil {
				return nil, err
			}
			if c.debug != nil {
				c.debug(fmt.Sprintf("↻ transient llm error: %v — retrying (attempt %d/%d)", lastErr, attempt+1, maxStreamAttempts))
			}
		}
		msg, err := c.streamOnce(ctx, body, onDelta)
		if err == nil {
			return msg, nil
		}
		if ctx.Err() != nil { // caller cancelled (Esc/Ctrl-C) — never retry
			return nil, ctx.Err()
		}
		if !isRetryableStream(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// streamOnce makes ONE request attempt: POST the body and consume the SSE stream
// into a Message. A transient mid-stream failure (a provider `error` event, or an
// idle-watchdog abort that isn't a caller cancellation) is wrapped via
// retryableStream so Chat can re-issue the identical request.
func (c *Client) streamOnce(ctx context.Context, body []byte, onDelta func(string)) (*Message, error) {
	// A cancellable child context lets the idle watchdog abort a stalled stream
	// (and the caller's ctx still cancels on Ctrl-C).
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	resp, err := c.postWithRetry(streamCtx, c.baseURL+"/chat/completions", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Abort if the stream goes silent for idleTimeout; reset on every line so a
	// long, actively-streaming reply is never cut.
	idle := time.AfterFunc(idleTimeout, cancel)
	defer idle.Stop()

	var content strings.Builder
	var reasoning strings.Builder // captured only for the debug sink
	calls := map[int]*ToolCall{}
	var order []int
	var usage *Usage
	var finishReason string
	repGuard := newRepetitionGuard()    // cut a degenerate prose loop short
	reasonGuard := newRepetitionGuard() // ...and a loop in the (hidden) reasoning channel

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		idle.Reset(idleTimeout)
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			// A provider error mid-stream (e.g. an engine hiccup during a long
			// generation) is usually transient — let Chat re-issue the request.
			return nil, retryableStream(fmt.Errorf("llm: %s", chunk.Error.Message))
		}
		if chunk.Usage != nil {
			usage = &Usage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				CachedTokens:     chunk.Usage.PromptTokensDetails.CachedTokens,
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if fr := chunk.Choices[0].FinishReason; fr != "" {
			finishReason = fr
		}
		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			content.WriteString(delta.Content)
			onDelta(delta.Content)
			// Stop a model looping on the same prose instead of acting — stream the
			// partial out (above) but end the turn here. The agent loop reads this
			// finish_reason and forces a structured tool call so the model acts.
			if delta.ToolCalls == nil && repGuard.feed(delta.Content) {
				finishReason = FinishReasonRepetition
				break
			}
		}
		if delta.ReasoningContent != "" {
			if c.debug != nil {
				reasoning.WriteString(delta.ReasoningContent)
			}
			// A model can loop in its reasoning (invisible — not streamed to the UI),
			// burning the whole reasoning budget before it ever acts. Cut it the same
			// way; the agent loop then forces a structured tool call.
			if delta.ToolCalls == nil && reasonGuard.feed(delta.ReasoningContent) {
				finishReason = FinishReasonRepetition
				break
			}
		}
		for _, tcd := range delta.ToolCalls {
			tc, ok := calls[tcd.Index]
			if !ok {
				tc = &ToolCall{Type: "function"}
				calls[tcd.Index] = tc
				order = append(order, tcd.Index)
			}
			if tcd.ID != "" {
				tc.ID = tcd.ID
			}
			if tcd.Function.Name != "" {
				tc.Function.Name = tcd.Function.Name
			}
			tc.Function.Arguments += tcd.Function.Arguments
		}
	}
	if err := scanner.Err(); err != nil {
		// An idle-watchdog abort cancels streamCtx while the CALLER's ctx is still
		// live — a transient stall, retryable. A caller cancel (Esc/Ctrl-C) cancels
		// the parent ctx, so surface that as-is (terminal).
		if ctx.Err() == nil {
			return nil, retryableStream(err)
		}
		return nil, err
	}
	if c.debug != nil && reasoning.Len() > 0 {
		c.debug("reasoning:\n" + reasoning.String())
	}

	msg := &Message{Role: "assistant", Content: content.String(), Usage: usage, FinishReason: finishReason}
	for _, idx := range order {
		msg.ToolCalls = append(msg.ToolCalls, *calls[idx])
	}
	return msg, nil
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"` // reasoning models stream thinking here
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			// cached_tokens is the OpenAI-compatible prompt-cache hit count
			// (prefix reused by the backend) — present when caching is active.
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// errorFromResponse turns a non-200 (e.g. budget/tier gate) into a clear error.
func errorFromResponse(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	// A 401 means the access token was rejected and silent refresh couldn't
	// recover it (most often the refresh token has expired) — the only fix is to
	// log in again, so say so plainly rather than leaking a bare status line.
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("session expired — run `borg auth login` to re-authenticate (in the REPL: /login)")
	}
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		return fmt.Errorf("llm %d (%s): %s", resp.StatusCode, e.Error.Type, e.Error.Message)
	}
	return fmt.Errorf("llm request failed: %s", resp.Status)
}
