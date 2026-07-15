package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/turborg/borg/internal/auth"
	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/llm"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// captureUI records the agent's output and answers permission prompts.
type captureUI struct {
	content strings.Builder
	calls   []string
	permit  Decision
	stats   Stats
}

func (u *captureUI) ThinkingStart()                  {}
func (u *captureUI) Delta(s string)                  { u.content.WriteString(s) }
func (u *captureUI) AssistantEnd(s Stats)            { u.stats = s }
func (u *captureUI) ToolCall(name, _ string)         { u.calls = append(u.calls, name) }
func (u *captureUI) ToolBatch(int)                   {}
func (u *captureUI) ToolResult(string, bool, string) {}
func (u *captureUI) ToolDiff(string)                 {}
func (u *captureUI) Permit(string) Decision          { return u.permit }
func (u *captureUI) AskUser(AskRequest) AskResult    { return AskResult{} }
func (u *captureUI) Debug(string)                    {}

// sseChunk marshals one OpenAI streaming chunk as an SSE `data:` line.
func sseChunk(t *testing.T, delta map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
	require.NoError(t, err)
	return "data: " + string(b) + "\n\n"
}

func newAgent(t *testing.T, url string, ui UI) *Agent {
	t.Helper()
	a := New(&config.Config{LLMProxyURL: url, Model: "floko"}, &auth.Credentials{AccessToken: "tok"})
	a.SetUI(ui)
	return a
}

func TestAgentRunsToolLoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	require.NoError(t, os.WriteFile(path, []byte("file says hi"), 0o644))

	var call int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		call++
		if call == 1 {
			fmt.Fprint(w, sseChunk(t, map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": "c1",
				"function": map[string]any{"name": "read_file", "arguments": `{"path":"` + path + `"}`},
			}}}))
		} else {
			fmt.Fprint(w, sseChunk(t, map[string]any{"content": "Done."}))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ui := &captureUI{permit: AllowOnce}
	a := newAgent(t, srv.URL, ui)

	require.NoError(t, a.Ask(context.Background(), "read the file"))
	require.Equal(t, 2, call)
	require.Contains(t, ui.calls, "read_file")
	require.Contains(t, ui.content.String(), "Done.")
}

func TestAgentDeniedMutationDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")

	var call int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		call++
		if call == 1 {
			fmt.Fprint(w, sseChunk(t, map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": "c1",
				"function": map[string]any{"name": "write_file", "arguments": `{"path":"` + path + `","content":"x"}`},
			}}}))
		} else {
			fmt.Fprint(w, sseChunk(t, map[string]any{"content": "ok"}))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ui := &captureUI{permit: DenyOnce}
	a := newAgent(t, srv.URL, ui)

	require.NoError(t, a.Ask(context.Background(), "write a file"))
	require.NoFileExists(t, path) // permission denied -> nothing written
}

func TestAgentSurfacesLLMError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"type":"model_not_in_plan","message":"needs a higher plan"}}`))
	}))
	defer srv.Close()

	a := newAgent(t, srv.URL, &captureUI{})
	err := a.Ask(context.Background(), "do it")
	require.Error(t, err)
	require.Contains(t, err.Error(), "model_not_in_plan")
}

func TestAgentSettersAndReset(t *testing.T) {
	a := newAgent(t, "http://example", &captureUI{})

	require.Equal(t, "floko", a.Model())
	a.SetModel("chuppa")
	require.Equal(t, "chuppa", a.Model())

	require.False(t, a.Think())
	a.SetThink(true)
	require.True(t, a.Think())

	require.Empty(t, a.Effort())
	a.SetEffort("high")
	require.Equal(t, "high", a.Effort())

	a.messages = append(a.messages, llm.Message{Role: "user", Content: "x"})
	a.Reset()
	require.Len(t, a.messages, 1) // system prompt only
}

func TestAgentMessagesSnapshotAndRestore(t *testing.T) {
	a := newAgent(t, "http://example", &captureUI{})

	// Messages returns a copy — mutating it must not affect the agent.
	snap := a.Messages()
	require.Len(t, snap, 1)
	snap[0].Content = "tampered"
	require.NotEqual(t, "tampered", a.Messages()[0].Content)

	// SetMessages replaces history; empty input falls back to system-only.
	a.SetMessages([]llm.Message{{Role: "system", Content: "s"}, {Role: "user", Content: "resumed"}})
	require.Len(t, a.Messages(), 2)
	require.Equal(t, "resumed", a.Messages()[1].Content)

	a.SetMessages(nil)
	require.Len(t, a.Messages(), 1)
}

// A finish call's summary is recovered even when the model hit its output cap and
// the arguments JSON is truncated (no closing quote/brace) — so a cut-off turn
// still shows its partial answer instead of rendering nothing.
func TestExtractSummarySalvagesTruncatedFinish(t *testing.T) {
	// Well-formed: parsed normally, escapes decoded.
	require.Equal(t, "all done\nline2",
		extractSummary(`{"summary":"all done\nline2"}`))

	// Truncated mid-string (cap hit): no closing quote/brace — salvage the prefix.
	require.Equal(t, "Bosch is the better pick because",
		extractSummary(`{"summary":"Bosch is the better pick because`))

	// Salvage path decodes the full range of escapes: \t \r \" \\ / \uXXXX.
	require.Equal(t, "a\tb\rc\"d\\e/fé",
		extractSummary(`{"summary":"a\tb\rc\"d\\e/f\u00e9`))

	// Dangling backslash exactly at the truncation point → decoded prefix only.
	require.Equal(t, "para\n",
		extractSummary(`{"summary":"para\n\`))

	// No summary field → empty.
	require.Empty(t, extractSummary(`{"other":"x"}`))
	// summary key but cut off before the colon → empty.
	require.Empty(t, extractSummary(`{"summary"`))
	// colon present but no opening quote yet → empty.
	require.Empty(t, extractSummary(`{"summary": `))
}

// A resumed session adopts the CURRENT system prompt (harness config) instead of
// replaying the stale one saved at creation, while preserving the rest of the
// transcript — so prompt improvements reach old sessions.
func TestResumedSessionAdoptsCurrentSystemPrompt(t *testing.T) {
	a := NewWithLLM(&config.Config{Model: "floko"}, &scriptedLLM{})

	a.SetMessages([]llm.Message{
		{Role: "system", Content: "OLD STALE PROMPT"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	})
	msgs := a.Messages()
	require.Len(t, msgs, 3)
	require.Equal(t, "system", msgs[0].Role)
	require.NotContains(t, msgs[0].Content, "OLD STALE PROMPT")
	require.Contains(t, msgs[0].Content, "SAME language") // current prompt adopted
	require.Equal(t, "hi", msgs[1].Content)               // rest preserved verbatim
	require.Equal(t, "hello", msgs[2].Content)

	// Stored history without a system message gets a current one prepended.
	a.SetMessages([]llm.Message{{Role: "user", Content: "no sys"}})
	msgs = a.Messages()
	require.Len(t, msgs, 2)
	require.Equal(t, "system", msgs[0].Role)
	require.Contains(t, msgs[0].Content, "SAME language")
	require.Equal(t, "no sys", msgs[1].Content)
}

func TestAgentUserInfoAuthInfoLogout(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // isolate the credentials store

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"Ada","email":"ada@example.test","plan_code":"max"}`))
	}))
	defer srv.Close()

	a := New(&config.Config{
		APIBaseURL: srv.URL, AppURL: "https://app.test",
		LLMProxyURL: srv.URL, Model: "floko",
	}, &auth.Credentials{AccessToken: "tok"})

	// UserInfo on the live client hits /v1/users/me.
	u, err := a.UserInfo(context.Background())
	require.NoError(t, err)
	require.Equal(t, "Ada", u.Name)
	require.Equal(t, "max", u.PlanCode)

	// The metadata wrappers delegate to the client (Tier reads the same body).
	tier, err := a.Tier(context.Background())
	require.NoError(t, err)
	require.Equal(t, "max", tier)
	_, _ = a.Models(context.Background())
	_, _ = a.Usage(context.Background())

	// Login surfaces flow errors — a cancelled context aborts the device flow.
	a.cfg.ForceDevice = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, a.Login(ctx))

	// AuthInfo reflects the environment; not logged in until creds are on disk.
	info := a.AuthInfo()
	require.False(t, info.LoggedIn)
	require.Equal(t, srv.URL, info.APIBaseURL)
	require.Equal(t, "https://app.test", info.AppURL)

	// Plant credentials, then AuthInfo reports logged in.
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "borg")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	b, _ := json.Marshal(&auth.Credentials{AccessToken: "tok", TokenType: "Bearer"})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "credentials.json"), b, 0o600))

	info = a.AuthInfo()
	require.True(t, info.LoggedIn)
	require.Equal(t, "Bearer", info.TokenType)

	// Logout removes the stored credentials.
	require.NoError(t, a.Logout())
	require.False(t, a.AuthInfo().LoggedIn)
}

func TestAgentUserInfoUnavailableOnSubstituteLLM(t *testing.T) {
	// A non-live LLM (eval/scripted seam) has no /v1/users/me — UserInfo no-ops.
	a := NewWithLLM(&config.Config{Model: "floko"}, &scriptedLLM{})
	u, err := a.UserInfo(context.Background())
	require.NoError(t, err)
	require.Nil(t, u)
}

// emptyTool returns no output — exercises the loop's empty-result guard so a
// tool can never produce a `tool` message with empty content.
type emptyTool struct{}

func (emptyTool) Name() string                                             { return "empty_tool" }
func (emptyTool) Description() string                                      { return "returns nothing" }
func (emptyTool) Mutating() bool                                           { return false }
func (emptyTool) Schema() json.RawMessage                                  { return json.RawMessage(`{"type":"object"}`) }
func (emptyTool) Execute(context.Context, json.RawMessage) (string, error) { return "", nil }

func TestRunToolEmptyOutputGuard(t *testing.T) {
	a := New(&config.Config{Model: "floko"}, &auth.Credentials{AccessToken: "tok"})
	a.SetUI(&captureUI{permit: AllowOnce})
	a.tools.Register(emptyTool{})

	out := a.runTool(context.Background(), llm.ToolCall{
		ID:       "1",
		Function: llm.ToolCallFunction{Name: "empty_tool", Arguments: "{}"},
	})
	require.Equal(t, "(no output)", out)
}

// The learn prompt must keep driving deep exploration — guard the key
// instructions so a future edit can't quietly make it shallow again.
func TestSystemPromptEncouragesParallelCalls(t *testing.T) {
	require.Contains(t, systemPrompt, "parallel")        // nudge the model to batch
	require.Contains(t, systemPrompt, "SINGLE response") // independent calls together
	require.Contains(t, systemPrompt, "token-efficient") // grep-first, ranged reads
	require.Contains(t, systemPrompt, "offset/limit")
	require.Contains(t, systemPrompt, "glob")                            // locate files by pattern
	require.Contains(t, systemPrompt, "run_in_background")               // long-running commands
	require.Contains(t, systemPrompt, "edit_lines")                      // precise line-number edits
	require.Contains(t, systemPrompt, "verify")                          // compile-check after edits
	require.Contains(t, systemPrompt, "SAME language")                   // reply in the user's language
	require.Contains(t, systemPrompt, "ONE bash call")                   // chain shell steps, don't single-step git
	require.Contains(t, systemPrompt, "gh pr list")                      // discover the PR instead of asking
	require.Contains(t, systemPrompt, "git commit -F - <<'EOF'")         // safe commit messages via quoted heredoc, not -m "..."
	require.Contains(t, systemPrompt, "command substitution")            // explain WHY -m with backticks/$() is dangerous
	require.Contains(t, systemPrompt, "Write a real commit message")     // body, not just a subject line
	require.Contains(t, systemPrompt, "explaining WHAT changed and WHY") // proportional commit body
	require.Contains(t, systemPrompt, "EXPLORE before you edit")         // gather context before implementing
	require.Contains(t, systemPrompt, "discover it")                     // inspect an unknown name rather than guess
}

// Attribution is on by default and injects the turborg co-author trailer into the
// system prompt; disabling it (or clearing the identity) removes the section.
func TestAttributionAddendum(t *testing.T) {
	on := &config.Config{
		Model: "chuppa", GitAttribution: true,
		GitAttributionName: "Turborg", GitAttributionEmail: "noreply@turborg.com",
	}
	got := composeSystemPrompt(on)
	require.Contains(t, got, "# Attribution")
	require.Contains(t, got, "Co-Authored-By: Turborg <noreply@turborg.com>")
	require.Contains(t, got, "Opened with borg")

	off := *on
	off.GitAttribution = false
	require.NotContains(t, composeSystemPrompt(&off), "# Attribution")

	// Attribution on but no identity configured ⇒ no section (nothing to credit).
	blank := *on
	blank.GitAttributionName, blank.GitAttributionEmail = "", ""
	require.NotContains(t, composeSystemPrompt(&blank), "# Attribution")
}

// Floko's per-model guidance must steer the small model off the waste we observed
// under required tool-calling: re-reading the already-injected BORG.md, and
// exploring instead of finishing a question that needs no tools.
func TestFlokoAddendumGuidesDirectFinish(t *testing.T) {
	a := modelAddendum("floko")
	require.Contains(t, a, "ANSWER DIRECTLY")    // finish straight away when no tools are needed
	require.Contains(t, a, "never read_file it") // don't re-read the injected BORG.md
	require.Contains(t, a, "same file twice")    // don't repeat an identical read
	require.Empty(t, modelAddendum("chuppa"))    // chuppa stays lean (no addendum)
}

func TestStatsLineShowsCached(t *testing.T) {
	require.Contains(t, Stats{InTokens: 1000, OutTokens: 20, CachedTokens: 900}.Line(), "(900 cached)")
	require.NotContains(t, Stats{InTokens: 10, OutTokens: 5}.Line(), "cached") // none → omitted
}

func TestToolCallLineAndFirstLine(t *testing.T) {
	require.Equal(t, "bash $ ls -a", ToolCallLine("bash", `{"command":"ls -a"}`))
	require.Equal(t, "read_file main.go", ToolCallLine("read_file", `{"path":"main.go"}`))
	require.Equal(t, "grep TODO in .", ToolCallLine("grep", `{"pattern":"TODO","path":"."}`))
	require.Contains(t, ToolCallLine("unknown", `{"x":1}`), "unknown") // generic fallback

	require.Equal(t, "first", firstLine("\n  first \nsecond"))
	require.Empty(t, firstLine("  \n\t"))
}

func TestLearnPromptIsThorough(t *testing.T) {
	for _, want := range []string{
		"EXISTING CONTEXT", "INTENT", "every *.md", "STRUCTURE",
		"COMPARE", "DECOYS", "HOW TO RUN", "never invent", "SUPERSEDE",
		// completeness + anti-guessing levers that pull a small model toward
		// frontier quality on this structured task:
		"Precision over confidence", "identical", "COMPLETE",
		"deployed", "GOTCHAS", "never drop this section",
	} {
		require.Containsf(t, LearnPrompt, want, "learn prompt lost the %q instruction", want)
	}
}

func TestProjectContextInjected(t *testing.T) {
	// No BORG.md in the working dir → base system prompt only.
	t.Chdir(t.TempDir())
	base := New(&config.Config{Model: "floko"}, &auth.Credentials{AccessToken: "x"})
	require.NotContains(t, base.Messages()[0].Content, "Project context")

	// With BORG.md present → its contents are appended to the system prompt.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ProjectContextFile), []byte("RULE: prefer tabs"), 0o644))
	t.Chdir(dir)
	a := New(&config.Config{Model: "floko"}, &auth.Credentials{AccessToken: "x"})
	sys := a.Messages()[0]
	require.Equal(t, "system", sys.Role)
	require.Contains(t, sys.Content, "RULE: prefer tabs")
	require.Contains(t, sys.Content, ProjectContextFile)
}

// One shared prompt; a small per-model addendum (Floko/gemma gets extra
// tool-call discipline, Chuppa stays lean), refreshed on a mid-session switch.
func TestPerModelSystemPromptAddendum(t *testing.T) {
	t.Chdir(t.TempDir()) // no BORG.md

	flo := New(&config.Config{Model: "floko"}, &auth.Credentials{AccessToken: "x"})
	require.Contains(t, flo.Messages()[0].Content, "You are Floko")
	require.Contains(t, flo.Messages()[0].Content, "finish tool")

	chu := New(&config.Config{Model: "chuppa"}, &auth.Credentials{AccessToken: "x"})
	require.NotContains(t, chu.Messages()[0].Content, "You are Floko") // lean

	// Switching model refreshes the addendum on the system message.
	chu.SetModel("floko")
	require.Contains(t, chu.Messages()[0].Content, "You are Floko")
	chu.SetModel("chuppa")
	require.NotContains(t, chu.Messages()[0].Content, "You are Floko")
}
