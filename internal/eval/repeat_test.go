package eval_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/turborg/borg/internal/eval"
	"github.com/turborg/borg/internal/llm"
)

// batchingModel issues TWO independent read-only calls in one turn (so the loop
// runs them as a parallel batch → ToolBatch(2)), then finishes. Used to prove the
// parallel-batch metric is captured.
type batchingModel struct{ call int }

func (m *batchingModel) Chat(_ context.Context, _ []llm.Message, _ []llm.Tool, _ bool, onDelta func(string), _ ...llm.ChatOption) (*llm.Message, error) {
	defer func() { m.call++ }()
	if m.call == 0 {
		return &llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "a", Type: "function", Function: llm.ToolCallFunction{Name: "read_file", Arguments: `{"path":"/nonexistent-1"}`}},
			{ID: "b", Type: "function", Function: llm.ToolCallFunction{Name: "read_file", Arguments: `{"path":"/nonexistent-2"}`}},
		}, Usage: &llm.Usage{PromptTokens: 50, CompletionTokens: 5}}, nil
	}
	if onDelta != nil {
		onDelta("done")
	}
	return &llm.Message{Role: "assistant", Content: "done", Usage: &llm.Usage{PromptTokens: 60, CompletionTokens: 3}}, nil
}
func (m *batchingModel) Models(context.Context) ([]llm.ModelInfo, error)  { return nil, nil }
func (m *batchingModel) Tier(context.Context) (string, error)             { return "", nil }
func (m *batchingModel) Usage(context.Context) (*llm.AccountUsage, error) { return nil, nil }
func (m *batchingModel) SetModel(string)                                  {}
func (m *batchingModel) SetEffort(string)                                 {}
func (m *batchingModel) SetDebug(func(string))                            {}

// The statsUI must count a turn with ≥2 independent read-only calls as one
// parallel batch — the batching/efficiency signal the measurement harness tracks.
func TestStatsUICountsParallelBatch(t *testing.T) {
	rep := eval.RunSuite(context.Background(), []eval.Task{{Name: "probe"}}, &batchingModel{}, newRecUI)
	require.Len(t, rep.Tasks, 1)
	require.Equal(t, 1, rep.Tasks[0].ParallelBatches) // the 2-read_file turn
	require.Equal(t, 1, rep.ParallelBatches())
	require.Equal(t, 2, rep.Tasks[0].ToolCalls) // both calls executed
}

// RunRepeated runs the suite N times and AggregateRuns reports the mean ± range —
// the rate-not-a-transcript output that lets a change be judged across runs.
func TestRunRepeatedAndAggregate(t *testing.T) {
	tasks := []eval.Task{{Name: "a"}, {Name: "b"}} // nil oracle ⇒ pass

	runs := eval.RunRepeated(context.Background(), tasks, usageModel{}, newRecUI, 3)
	require.Len(t, runs, 3)
	for i := range runs {
		runs[i].Model = "chuppa"
	}

	agg := eval.AggregateRuns(runs)
	require.Contains(t, agg, "aggregate over 3 runs")
	require.Contains(t, agg, "model chuppa")
	require.Contains(t, agg, "pass-rate")
	require.Contains(t, agg, "avg-steps")
	require.Contains(t, agg, "parallel-batches")
	require.Contains(t, agg, "in-tokens (total)")
	require.Contains(t, agg, "out-tokens (total)")
	require.Contains(t, agg, "cached-tokens")
	require.Contains(t, agg, "2.0 avg (min 2.0, max 2.0)") // both tasks pass every run

	// edges
	require.Contains(t, eval.AggregateRuns(nil), "no runs")
	require.Len(t, eval.RunRepeated(context.Background(), tasks, usageModel{}, newRecUI, 0), 1) // <1 ⇒ 1
}
