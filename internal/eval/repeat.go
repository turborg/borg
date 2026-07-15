package eval

// Measurement harness: run a suite N times and report the AGGREGATE — mean and
// spread of pass-rate, steps, and parallel-batching — so a prompt/harness change
// is judged by a RATE across repeated live runs, not by eyeballing one transcript.
// This is the anti-overfit instrument: a tweak earns its keep only if it moves
// the aggregate (and the spread shows whether a delta is signal or noise).

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/turborg/borg/internal/agent"
)

// RunRepeated runs the suite `runs` times against the same model, returning one
// Report per run (each tagged with the model). runs<1 is treated as 1.
func RunRepeated(ctx context.Context, tasks []Task, model agent.LLM, newUI func() agent.UI, runs int, opts ...SuiteOption) []Report {
	if runs < 1 {
		runs = 1
	}
	out := make([]Report, 0, runs)
	for i := 0; i < runs; i++ {
		out = append(out, RunSuite(ctx, tasks, model, newUI, opts...))
	}
	return out
}

// stat holds the mean and observed range of a metric across runs.
type stat struct {
	mean, min, max float64
}

func summarize(vals []float64) stat {
	if len(vals) == 0 {
		return stat{}
	}
	s := stat{min: vals[0], max: vals[0]}
	var sum float64
	for _, v := range vals {
		sum += v
		s.min = math.Min(s.min, v)
		s.max = math.Max(s.max, v)
	}
	s.mean = sum / float64(len(vals))
	return s
}

func (s stat) String() string {
	return fmt.Sprintf("%.1f avg (min %.1f, max %.1f)", s.mean, s.min, s.max)
}

// AggregateRuns renders the across-runs summary for a single model's repeated
// runs: pass-rate, avg-steps, and parallel-batches as mean ± range. The spread
// is the point — a change is only "real" if it moves the mean beyond the noise.
func AggregateRuns(runs []Report) string {
	var b strings.Builder
	if len(runs) == 0 {
		b.WriteString("aggregate: no runs\n")
		return b.String()
	}

	model := runs[0].Model
	total := runs[0].Total()
	var passed, avgSteps, batches, inTok, outTok, cachedTok []float64
	for _, r := range runs {
		passed = append(passed, float64(r.Passed()))
		avgSteps = append(avgSteps, r.AvgSteps())
		batches = append(batches, float64(r.ParallelBatches()))
		in, cached, out, _, _, _ := r.Totals()
		inTok = append(inTok, float64(in))
		outTok = append(outTok, float64(out))
		cachedTok = append(cachedTok, float64(cached))
	}

	fmt.Fprintf(&b, "aggregate over %d runs — model %s — corpus %d tasks\n", len(runs), model, total)
	fmt.Fprintf(&b, "  pass-rate           %s  (of %d)\n", summarize(passed), total)
	fmt.Fprintf(&b, "  avg-steps/task      %s\n", summarize(avgSteps))
	fmt.Fprintf(&b, "  parallel-batches    %s\n", summarize(batches))
	fmt.Fprintf(&b, "  in-tokens (total)   %s\n", summarize(inTok))
	fmt.Fprintf(&b, "  out-tokens (total)  %s\n", summarize(outTok))
	fmt.Fprintf(&b, "  cached-tokens       %s\n", summarize(cachedTok))
	return b.String()
}
