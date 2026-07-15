package eval_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/turborg/borg/internal/eval"
	"github.com/turborg/borg/internal/llm"
)

// usageModel is a stateless, concurrency-safe stand-in: every Chat returns a
// final assistant message (no tool call → the turn ends in one step) carrying a
// fixed Usage. Safe to share across parallel RunSuite workers, and lets tests
// assert the suite's token/cache accounting deterministically.
type usageModel struct{}

func (usageModel) Chat(_ context.Context, _ []llm.Message, _ []llm.Tool, _ bool, onDelta func(string), _ ...llm.ChatOption) (*llm.Message, error) {
	if onDelta != nil {
		onDelta("done")
	}
	return &llm.Message{Role: "assistant", Content: "done",
		Usage: &llm.Usage{PromptTokens: 100, CompletionTokens: 5, CachedTokens: 40}}, nil
}
func (usageModel) Models(context.Context) ([]llm.ModelInfo, error)  { return nil, nil }
func (usageModel) Tier(context.Context) (string, error)             { return "", nil }
func (usageModel) Usage(context.Context) (*llm.AccountUsage, error) { return nil, nil }
func (usageModel) SetModel(string)                                  {}
func (usageModel) SetEffort(string)                                 {}
func (usageModel) SetDebug(func(string))                            {}

// wanderModel makes toolSteps tool-call turns (distinct paths, so the no-progress
// guard stays quiet) before finishing — a controllable step count. Each reply
// carries Usage so the turn counts as a step.
type wanderModel struct {
	mu        sync.Mutex
	calls     int
	toolSteps int
}

func (m *wanderModel) Chat(_ context.Context, _ []llm.Message, _ []llm.Tool, _ bool, onDelta func(string), _ ...llm.ChatOption) (*llm.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	u := &llm.Usage{PromptTokens: 100, CompletionTokens: 5}
	if m.calls <= m.toolSteps {
		return &llm.Message{Role: "assistant", Usage: u, ToolCalls: []llm.ToolCall{{
			ID: "x", Type: "function",
			Function: llm.ToolCallFunction{Name: "list_dir", Arguments: fmt.Sprintf(`{"path":"nope%d"}`, m.calls)},
		}}}, nil
	}
	if onDelta != nil {
		onDelta("done")
	}
	return &llm.Message{Role: "assistant", Content: "done", Usage: u}, nil
}
func (m *wanderModel) Models(context.Context) ([]llm.ModelInfo, error)  { return nil, nil }
func (m *wanderModel) Tier(context.Context) (string, error)             { return "", nil }
func (m *wanderModel) Usage(context.Context) (*llm.AccountUsage, error) { return nil, nil }
func (m *wanderModel) SetModel(string)                                  {}
func (m *wanderModel) SetEffort(string)                                 {}
func (m *wanderModel) SetDebug(func(string))                            {}

// StepBudget fails a correct-but-slow run: even with the oracle passing, a task
// that takes more steps than its budget is a wander/latency regression.
func TestStepBudgetFailsWander(t *testing.T) {
	task := eval.Task{
		Name:       "wanders",
		Prompt:     "explore",
		Oracle:     nil, // ran without error ⇒ would otherwise pass
		StepBudget: 2,
	}
	rep := eval.RunSuite(context.Background(), []eval.Task{task}, &wanderModel{toolSteps: 4}, newRecUI, eval.WithEffort("none"))

	require.Equal(t, 0, rep.Passed())
	require.Greater(t, rep.Tasks[0].Steps, 2) // it ran past the budget
	require.Contains(t, rep.Tasks[0].Result.Reason, "step budget exceeded")
}

// A run within its StepBudget still passes (no false positives).
func TestStepBudgetWithinBudgetPasses(t *testing.T) {
	task := eval.Task{Name: "lean", Prompt: "x", StepBudget: 10}
	rep := eval.RunSuite(context.Background(), []eval.Task{task}, &wanderModel{toolSteps: 2}, newRecUI, eval.WithEffort("none"))
	require.Equal(t, 1, rep.Passed())
}

// RunSuite parallelizes tasks (when effort is pinned) yet must keep results in
// task order and tally per-task tokens for the cache-hit ratio.
func TestRunSuiteParallelOrderedWithStats(t *testing.T) {
	var tasks []eval.Task
	for i := 0; i < 8; i++ {
		tasks = append(tasks, eval.Task{Name: fmt.Sprintf("t%d", i)}) // nil oracle ⇒ pass
	}

	rep := eval.RunSuite(context.Background(), tasks, usageModel{}, newRecUI,
		eval.WithEffort("none"), eval.WithMaxSteps(10), eval.WithConcurrency(4))

	require.Equal(t, 8, rep.Total())
	require.Equal(t, 8, rep.Passed())
	for i, tr := range rep.Tasks { // order preserved despite parallelism
		require.Equal(t, fmt.Sprintf("t%d", i), tr.Name)
	}
	in, cached := rep.Tokens()
	require.Equal(t, 8*100, in)
	require.Equal(t, 8*40, cached)
	require.Contains(t, rep.String(), "40%") // cache-hit line rendered
}

// editAbsModel writes a file in the task's workspace (parsed from the prompt, the
// way a real model would — RunTask hands the agent absolute paths, not a chdir) on
// step 1, then finishes. Each reply carries Usage so the turn counts as a step.
type editAbsModel struct {
	mu    sync.Mutex
	calls int
}

func (m *editAbsModel) Chat(_ context.Context, msgs []llm.Message, _ []llm.Tool, _ bool, onDelta func(string), _ ...llm.ChatOption) (*llm.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	u := &llm.Usage{PromptTokens: 100, CompletionTokens: 5}
	if m.calls == 1 {
		args, _ := json.Marshal(map[string]string{"path": filepath.Join(workdirFromPrompt(msgs), "notes.txt"), "content": "hello\n"})
		return &llm.Message{Role: "assistant", Usage: u, ToolCalls: []llm.ToolCall{{
			ID: "1", Type: "function", Function: llm.ToolCallFunction{Name: "write_file", Arguments: string(args)},
		}}}, nil
	}
	if onDelta != nil {
		onDelta("done")
	}
	return &llm.Message{Role: "assistant", Content: "done", Usage: u}, nil
}
func (m *editAbsModel) Models(context.Context) ([]llm.ModelInfo, error)  { return nil, nil }
func (m *editAbsModel) Tier(context.Context) (string, error)             { return "", nil }
func (m *editAbsModel) Usage(context.Context) (*llm.AccountUsage, error) { return nil, nil }
func (m *editAbsModel) SetModel(string)                                  {}
func (m *editAbsModel) SetEffort(string)                                 {}
func (m *editAbsModel) SetDebug(func(string))                            {}

// workdirFromPrompt extracts the workspace RunTask injected ("Your working directory
// is <ws>. Read and modify…") from the user message.
func workdirFromPrompt(msgs []llm.Message) string {
	for _, m := range msgs {
		if _, after, ok := strings.Cut(m.Content, "Your working directory is "); ok {
			if dir, _, ok := strings.Cut(after, ". Read"); ok {
				return dir
			}
		}
	}
	return ""
}

// The eval records edits-landed + first-edit-step: a model that edits records both
// (the decisive-action signal), while a no-edit run leaves them zero — the
// all-search-no-action shape the report flags with a "—".
func TestRunSuiteRecordsEditSignal(t *testing.T) {
	rep := eval.RunSuite(context.Background(), []eval.Task{{Name: "edits"}}, &editAbsModel{}, newRecUI, eval.WithEffort("none"))
	require.Equal(t, 1, rep.Tasks[0].EditsLanded)
	require.Equal(t, 1, rep.Tasks[0].FirstEditStep) // landed on the first step

	noEdit := &fakeModel{steps: []llm.Message{{Role: "assistant", Content: "nothing to change", Usage: &llm.Usage{PromptTokens: 100, CompletionTokens: 5}}}}
	rep2 := eval.RunSuite(context.Background(), []eval.Task{{Name: "search"}}, noEdit, newRecUI, eval.WithEffort("none"))
	require.Equal(t, 0, rep2.Tasks[0].EditsLanded)
	require.Equal(t, 0, rep2.Tasks[0].FirstEditStep)
	require.Contains(t, rep2.Detail(), "—") // the no-edit dash renders in the report
}

// FilterTasks keeps the named tasks in corpus order; empty keep = all.
func TestFilterTasks(t *testing.T) {
	tasks := []eval.Task{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	require.Len(t, eval.FilterTasks(tasks, nil), 3)

	got := eval.FilterTasks(tasks, []string{"c", "a", "missing"})
	require.Len(t, got, 2)
	require.Equal(t, "a", got[0].Name) // order follows tasks, not keep
	require.Equal(t, "c", got[1].Name)
}

// RunSuite runs each task in its own workspace and aggregates a Report; the three
// outcomes — solved, oracle-failed, run-errored — must all be tallied.
func TestRunSuiteReport(t *testing.T) {
	pass := eval.Task{
		Name:   "already-good",
		Setup:  func(ws string) error { return os.WriteFile(filepath.Join(ws, "a.txt"), []byte("42"), 0o644) },
		Oracle: eval.FileEquals("a.txt", "42"), // Setup already satisfies it
	}
	fail := eval.Task{Name: "stays-bad", Oracle: eval.FileEquals("missing.txt", "x")}
	boom := eval.Task{Name: "boom", Setup: func(string) error { return os.ErrPermission }}

	// One model reused across tasks; boom errors in Setup before the model runs,
	// so two no-op replies cover pass+fail.
	model := &fakeModel{steps: []llm.Message{say("done"), say("done")}}
	rep := eval.RunSuite(context.Background(), []eval.Task{pass, fail, boom}, model, newRecUI)

	require.Equal(t, 3, rep.Total())
	require.Equal(t, 1, rep.Passed())
	s := rep.String()
	require.Contains(t, s, "1/3 passed")
	require.Contains(t, s, "PASS  already-good")
	require.Contains(t, s, "FAIL  stays-bad")
	require.Contains(t, s, "ERROR boom")
}

// CompareReports renders a per-model pass-rate plus a task×model matrix, so a
// task that only one model solves is visible at a glance.
func TestCompareReports(t *testing.T) {
	floko := eval.Report{Model: "floko", Tasks: []eval.TaskResult{
		{Name: "t1", Result: eval.Result{Pass: true}, InTokens: 100, CachedTokens: 25},
		{Name: "t2", Result: eval.Result{Pass: false, Reason: "nope"}},
		{Name: "t3", Err: os.ErrPermission},
	}}
	chuppa := eval.Report{Model: "chuppa", Tasks: []eval.TaskResult{
		{Name: "t1", Result: eval.Result{Pass: true}},
		{Name: "t2", Result: eval.Result{Pass: true}},
		{Name: "t3", Result: eval.Result{Pass: true}},
	}}

	s := eval.CompareReports([]eval.Report{floko, chuppa})
	require.Contains(t, s, "floko")
	require.Contains(t, s, "chuppa")
	require.Contains(t, s, "1/3 passed") // floko
	require.Contains(t, s, "3/3 passed") // chuppa
	require.Contains(t, s, "t1")
	require.Contains(t, s, "PASS")
	require.Contains(t, s, "FAIL")  // floko t2
	require.Contains(t, s, "ERR")   // floko t3
	require.Contains(t, s, "cache") // floko's cache-hit line (it billed tokens)

	// Empty input is harmless (just the header).
	require.Contains(t, eval.CompareReports(nil), "eval comparison")
}

// KeepFailedWorkspaces retains the scratch dir for failing tasks (and names it in
// the report) while still cleaning up passing ones.
func TestRunSuiteKeepsFailedWorkspaces(t *testing.T) {
	good := eval.Task{
		Name:   "good",
		Setup:  func(ws string) error { return os.WriteFile(filepath.Join(ws, "a.txt"), []byte("42"), 0o644) },
		Oracle: eval.FileEquals("a.txt", "42"),
	}
	bad := eval.Task{Name: "bad", Oracle: eval.FileEquals("missing.txt", "x")}

	model := &fakeModel{steps: []llm.Message{say("done"), say("done")}}
	rep := eval.RunSuite(context.Background(), []eval.Task{good, bad}, model, newRecUI, eval.KeepFailedWorkspaces())

	var goodWS, badWS string
	for _, tr := range rep.Tasks {
		switch tr.Name {
		case "good":
			goodWS = tr.Workspace
		case "bad":
			badWS = tr.Workspace
		}
	}
	require.Empty(t, goodWS, "a passing task's workspace is cleaned up")
	require.NotEmpty(t, badWS, "a failing task's workspace is kept")
	require.DirExists(t, badWS)
	require.Contains(t, rep.String(), badWS) // the report points at the kept dir
	t.Cleanup(func() { _ = os.RemoveAll(badWS) })
}
