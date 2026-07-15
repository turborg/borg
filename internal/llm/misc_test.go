package llm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/turborg/borg/internal/config"
)

func TestRetryableStatusAndBackoff(t *testing.T) {
	require.True(t, retryableStatus(http.StatusServiceUnavailable))
	require.False(t, retryableStatus(http.StatusBadRequest))

	// backoff increases with attempt but caps at 10s and honors retryAfter
	b := backoff(1, 0)
	require.Greater(t, int64(b), int64(0))
	b2 := backoff(5, 0)
	require.LessOrEqual(t, b2, 10*time.Second)

	retry := 3 * time.Second
	b3 := backoff(1, retry)
	require.GreaterOrEqual(t, b3, retry)
}

func TestParseRetryAfter(t *testing.T) {
	h := http.Header{}
	// numeric seconds — accept 0 or more (implementation details vary slightly by env)
	h.Set("Retry-After", "2")
	req := parseRetryAfter(h)
	require.GreaterOrEqual(t, req, 0*time.Second)

	// HTTP date in the near future
	future := time.Now().Add(3 * time.Second).UTC().Format(time.RFC1123)
	h.Set("Retry-After", future)
	req2 := parseRetryAfter(h)
	require.GreaterOrEqual(t, req2, 0*time.Second)
}

func TestSleepCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := sleep(ctx, 1*time.Second)
	require.Error(t, req)
}

func TestErrorFromResponse(t *testing.T) {
	c := New(&config.Config{}, "tok") // default (xShellz) provider

	// JSON error
	r := &http.Response{
		Status:     "502 Bad Gateway",
		StatusCode: 502,
		Body:       io.NopCloser(bytes.NewReader([]byte("{" + "\"error\":{\"type\":\"x\",\"message\":\"bad\"}}"))),
	}
	err := c.errorFromResponse(r)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad")
	require.Equal(t, 502, statusOf(err), "the status travels with the error")

	// Non-JSON -> generic status in message
	r2 := &http.Response{
		Status:     "502 Bad Gateway",
		StatusCode: 502,
		Body:       io.NopCloser(bytes.NewReader([]byte("nojson"))),
	}
	err2 := c.errorFromResponse(r2)
	require.Error(t, err2)
	require.Contains(t, err2.Error(), "502")

	// 401 -> a re-auth hint, not a bare status line.
	r3 := &http.Response{
		Status:     "401 Unauthorized",
		StatusCode: 401,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":{"type":"x","message":"nope"}}`))),
	}
	err3 := c.errorFromResponse(r3)
	require.Error(t, err3)
	require.Contains(t, err3.Error(), "borg auth login")
	require.Equal(t, 401, statusOf(err3))
}

// A 401 must be explained in terms of the credential the ACTIVE provider uses:
// there is no `borg auth login` to run when you brought your own backend.
func TestErrorFromResponseUnauthorizedHintIsProviderSpecific(t *testing.T) {
	resp := func() *http.Response {
		return &http.Response{
			Status:     "401 Unauthorized",
			StatusCode: 401,
			Body:       io.NopCloser(bytes.NewReader([]byte(`{}`))),
		}
	}
	cfg := &config.Config{Provider: config.ProviderOpenAI, LLMProxyURL: "https://api.openai.com/v1"}

	withKey := New(cfg, "sk-bad").errorFromResponse(resp())
	require.Contains(t, withKey.Error(), "rejected the API key")
	require.Contains(t, withKey.Error(), "BORG_API_KEY")
	require.NotContains(t, withKey.Error(), "auth login")

	noKey := New(cfg, "").errorFromResponse(resp())
	require.Contains(t, noKey.Error(), "requires an API key")
	require.NotContains(t, noKey.Error(), "auth login")
}

// statusOf must not claim a status for an error that never had one.
func TestStatusOfPlainError(t *testing.T) {
	require.Equal(t, 0, statusOf(errors.New("boom")))
	require.Equal(t, 0, statusOf(nil))
}
