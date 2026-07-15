package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/turborg/borg/internal/config"
)

func TestChatEmitsDebugReasoning(t *testing.T) {
	var dbg strings.Builder
	// Server streams a reasoning_content delta then DONE.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"choices":[{"delta":{"reasoning_content":"thinking step 1"}}]}` + "\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	defer srv.Close()

	c := New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, "tok")
	c.SetDebug(func(s string) { dbg.WriteString(s) })

	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, false, func(string) {})
	require.NoError(t, err)
	out := dbg.String()
	require.Contains(t, out, "reasoning:")
	require.Contains(t, out, "thinking step 1")
}
