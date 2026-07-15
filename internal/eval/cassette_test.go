package eval_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/turborg/borg/internal/agent"
	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/eval"
	"github.com/turborg/borg/internal/llm"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// --- a token-free stand-in for a live model, used to *record* a cassette ---

type fakeModel struct {
	steps []llm.Message
	i     int
}

func (f *fakeModel) Chat(_ context.Context, _ []llm.Message, _ []llm.Tool, _ bool, onDelta func(string), _ ...llm.ChatOption) (*llm.Message, error) {
	if f.i >= len(f.steps) {
		return nil, fmt.Errorf("fakeModel exhausted")
	}
	r := f.steps[f.i]
	f.i++
	if r.Content != "" && onDelta != nil {
		onDelta(r.Content)
	}
	return &r, nil
}
func (f *fakeModel) Models(context.Context) ([]llm.ModelInfo, error)  { return nil, nil }
func (f *fakeModel) Tier(context.Context) (string, error)             { return "", nil }
func (f *fakeModel) Usage(context.Context) (*llm.AccountUsage, error) { return nil, nil }
func (f *fakeModel) SetEffort(string)                                 {}
func (f *fakeModel) SetModel(string)                                  {}
func (f *fakeModel) SetDebug(func(string))                            {}

func say(text string) llm.Message { return llm.Message{Role: "assistant", Content: text} }
func callTool(id, name, args string) llm.Message {
	return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
		ID: id, Type: "function",
		Function: llm.ToolCallFunction{Name: name, Arguments: args},
	}}}
}

// recUI records the tool-call trajectory and auto-approves mutations. The agent
// calls the UI from multiple goroutines (a parallel read-only batch), so the
// recording slice is mutex-guarded — the UI contract requires concurrency-safety.
type recUI struct {
	mu    sync.Mutex
	calls []string
}

func (u *recUI) ThinkingStart()           {}
func (u *recUI) Delta(string)             {}
func (u *recUI) AssistantEnd(agent.Stats) {}
func (u *recUI) ToolCall(name, _ string) {
	u.mu.Lock()
	u.calls = append(u.calls, name)
	u.mu.Unlock()
}
func (u *recUI) ToolBatch(int)                            {}
func (u *recUI) ToolResult(string, bool, string)          {}
func (u *recUI) ToolDiff(string)                          {}
func (u *recUI) Permit(string) agent.Decision             { return agent.AllowOnce }
func (u *recUI) AskUser(agent.AskRequest) agent.AskResult { return agent.AskResult{} }
func (u *recUI) Debug(string)                             {}

// newRecUI yields a fresh recording UI — the per-task UI factory RunSuite wants.
func newRecUI() agent.UI { return &recUI{} }

func agentWith(model agent.LLM, ui agent.UI) *agent.Agent {
	a := agent.NewWithLLM(&config.Config{Model: "floko"}, model)
	a.SetUI(ui)
	return a
}

// --- the heart of tier 2: record a real run, replay it byte-for-byte ---

// Running the agent once through a Recorder captures a cassette; replaying that
// cassette through a Player reproduces the exact same trajectory and filesystem
// effect — no model, no tokens.
func TestRecordThenReplayReproducesTrajectory(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.txt")
	out := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(src, []byte("hello"), 0o644))

	task := "expand in.txt into out.txt"
	steps := []llm.Message{
		callTool("1", "read_file", `{"path":"`+src+`"}`),
		callTool("2", "write_file", `{"path":"`+out+`","content":"hello world"}`),
		say("Done."),
	}

	// Record: drive the real loop with a fake model wrapped in a Recorder.
	rec := eval.NewRecorder(&fakeModel{steps: steps}, task, "floko")
	recordUI := &recUI{}
	require.NoError(t, agentWith(rec, recordUI).Ask(context.Background(), task))
	require.Equal(t, []string{"read_file", "write_file"}, recordUI.calls)
	require.Len(t, rec.Cassette.Replies, 3)

	// Persist + reload (proves the on-disk format round-trips).
	path := filepath.Join(dir, "session.cassette.json")
	require.NoError(t, rec.Cassette.Save(path))
	loaded, err := eval.Load(path)
	require.NoError(t, err)
	require.Equal(t, task, loaded.Task)

	// Replay: undo the mutation, run the loop again from the cassette only.
	require.NoError(t, os.Remove(out))
	replayUI := &recUI{}
	require.NoError(t, agentWith(eval.NewPlayer(loaded), replayUI).Ask(context.Background(), task))

	require.Equal(t, recordUI.calls, replayUI.calls) // identical trajectory
	got, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, "hello world", string(got)) // identical effect
}

// A cassette shorter than the loop needs is a real divergence: replay must fail
// loudly rather than silently truncate the run.
func TestReplayExhaustedCassetteErrors(t *testing.T) {
	dir := t.TempDir()
	// One tool call, no terminating reply — the loop will ask for a 2nd reply.
	c := &eval.Cassette{Task: "t", Model: "floko", Replies: []llm.Message{
		callTool("1", "list_dir", `{"path":"`+dir+`"}`),
	}}
	err := agentWith(eval.NewPlayer(c), &recUI{}).Ask(context.Background(), "t")
	require.Error(t, err)
	require.Contains(t, err.Error(), "cassette exhausted")
}

func TestCassetteSaveLoadRoundTrip(t *testing.T) {
	c := &eval.Cassette{Task: "task", Model: "floko", Replies: []llm.Message{say("hi")}}
	path := filepath.Join(t.TempDir(), "c.json")
	require.NoError(t, c.Save(path))

	got, err := eval.Load(path)
	require.NoError(t, err)
	require.Equal(t, c.Task, got.Task)
	require.Equal(t, c.Model, got.Model)
	require.Len(t, got.Replies, 1)
	require.Equal(t, "hi", got.Replies[0].Content)

	_, err = eval.Load(filepath.Join(t.TempDir(), "missing.json"))
	require.Error(t, err)

	// Saving under a non-existent directory surfaces the write error.
	require.Error(t, c.Save(filepath.Join(t.TempDir(), "no-such-dir", "c.json")))
}

// The non-Chat methods are interface plumbing: Recorder delegates to the wrapped
// client, Player is inert. Exercise both ends so the seam stays satisfied.
func TestModelSeamPassthroughs(t *testing.T) {
	models := []llm.ModelInfo{{ID: "floko"}}
	rec := eval.NewRecorder(&stubModel{models: models, tier: "pro"}, "t", "floko")
	rec.SetModel("chuppa")
	gotModels, err := rec.Models(context.Background())
	require.NoError(t, err)
	require.Equal(t, models, gotModels) // delegated through to the wrapped client
	tier, err := rec.Tier(context.Background())
	require.NoError(t, err)
	require.Equal(t, "pro", tier)

	p := eval.NewPlayer(&eval.Cassette{})
	p.SetModel("x")
	m, err := p.Models(context.Background())
	require.NoError(t, err)
	require.Nil(t, m)
	pt, err := p.Tier(context.Background())
	require.NoError(t, err)
	require.Empty(t, pt)
}

// stubModel returns canned catalog/tier so Recorder delegation is observable.
type stubModel struct {
	models []llm.ModelInfo
	tier   string
}

func (s *stubModel) Chat(context.Context, []llm.Message, []llm.Tool, bool, func(string), ...llm.ChatOption) (*llm.Message, error) {
	return &llm.Message{}, nil
}
func (s *stubModel) Models(context.Context) ([]llm.ModelInfo, error)  { return s.models, nil }
func (s *stubModel) Tier(context.Context) (string, error)             { return s.tier, nil }
func (s *stubModel) Usage(context.Context) (*llm.AccountUsage, error) { return nil, nil }
func (s *stubModel) SetEffort(string)                                 {}
func (s *stubModel) SetModel(string)                                  {}
func (s *stubModel) SetDebug(func(string))                            {}
