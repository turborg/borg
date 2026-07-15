package eval

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/turborg/borg/internal/agent"
)

// TaskResult is one task's outcome in a suite run.
type TaskResult struct {
	Name      string
	Result    Result
	Err       error  // non-nil if the run itself failed (setup/agent error), distinct from a failing oracle
	Workspace string // retained scratch dir for inspection (only when kept; see KeepFailedWorkspaces)

	// Per-task metrics, summed across the task's steps — the harness-tuning signal:
	// where a model spends tokens/steps, and how the prompt cache performs.
	InTokens        int           // prompt (input) tokens billed
	CachedTokens    int           // of those, served from the prompt cache
	OutTokens       int           // completion (output) tokens billed
	Steps           int           // token-bearing assistant turns (loop iterations)
	ToolCalls       int           // tool invocations made
	ParallelBatches int           // turns that issued ≥2 independent read-only calls at once
	Duration        time.Duration // wall-clock for the task run
	// EditsLanded / FirstEditStep measure how decisively the model ACTS vs explores:
	// the 1.35M-token incident explored ~21 steps before its first edit. EditsLanded
	// is successful edit-tool calls; FirstEditStep is the step the first one landed (0
	// = never edited — the "all search, no action" shape).
	EditsLanded   int
	FirstEditStep int
}

// ok reports whether the task was solved: the run completed and the oracle passed.
func (tr TaskResult) ok() bool { return tr.Err == nil && tr.Result.Pass }

// Report aggregates a suite run — borg's quality signal. Track Passed()/Total()
// over time as the corpus and the model improve.
type Report struct {
	Model string
	Tasks []TaskResult
}

// Passed counts solved tasks. Total is the number run.
func (r Report) Passed() int {
	n := 0
	for _, tr := range r.Tasks {
		if tr.ok() {
			n++
		}
	}
	return n
}

func (r Report) Total() int { return len(r.Tasks) }

// Tokens returns the suite's summed input tokens and the cached subset.
func (r Report) Tokens() (in, cached int) {
	for _, tr := range r.Tasks {
		in += tr.InTokens
		cached += tr.CachedTokens
	}
	return in, cached
}

// Totals sums the suite's metrics across all tasks.
func (r Report) Totals() (in, cached, out, steps, tools int, dur time.Duration) {
	for _, tr := range r.Tasks {
		in += tr.InTokens
		cached += tr.CachedTokens
		out += tr.OutTokens
		steps += tr.Steps
		tools += tr.ToolCalls
		dur += tr.Duration
	}
	return
}

// CachePct is the share of input tokens served from the prompt cache (0 when no
// tokens were billed, e.g. a deterministic run).
func (r Report) CachePct() int {
	in, cached := r.Tokens()
	if in == 0 {
		return 0
	}
	return cached * 100 / in
}

// ParallelBatches sums the parallel read-only batches across the suite — the
// batching/efficiency signal (higher = the model issues independent probes
// together rather than single-stepping them).
func (r Report) ParallelBatches() int {
	n := 0
	for _, tr := range r.Tasks {
		n += tr.ParallelBatches
	}
	return n
}

// AvgSteps is the mean token-bearing turns per task (0 when no tasks ran).
func (r Report) AvgSteps() float64 {
	if len(r.Tasks) == 0 {
		return 0
	}
	_, _, _, steps, _, _ := r.Totals()
	return float64(steps) / float64(len(r.Tasks))
}

// cacheLine renders the prompt-cache hit ratio, or "" when no tokens were
// measured (e.g. a deterministic/cassette run that doesn't bill usage).
func (r Report) cacheLine() string {
	in, cached := r.Tokens()
	if in == 0 {
		return ""
	}
	return fmt.Sprintf("cache: %d/%d input tokens cached (%d%%)", cached, in, cached*100/in)
}

// String renders a human-readable summary with a per-task verdict and reason.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "eval report — model %s — %d/%d passed\n", r.Model, r.Passed(), r.Total())
	if cl := r.cacheLine(); cl != "" {
		fmt.Fprintf(&b, "  %s\n", cl)
	}
	for _, tr := range r.Tasks {
		switch {
		case tr.Err != nil:
			fmt.Fprintf(&b, "  ERROR %s: %v", tr.Name, tr.Err)
		case tr.Result.Pass:
			fmt.Fprintf(&b, "  PASS  %s", tr.Name)
		default:
			fmt.Fprintf(&b, "  FAIL  %s: %s", tr.Name, tr.Result.Reason)
		}
		if tr.Workspace != "" {
			fmt.Fprintf(&b, "  (kept: %s)", tr.Workspace)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// SuiteOption configures a RunSuite invocation.
type SuiteOption func(*suiteOpts)

type suiteOpts struct {
	keepFailed  bool
	effort      string // pinned reasoning effort ("" = the agent's auto default)
	maxSteps    int    // per-task step cap (0 = shipped default)
	concurrency int    // max tasks in flight (0/1 = sequential)
}

// KeepFailedWorkspaces leaves the scratch dir on disk for any task that didn't
// pass, recording its path in TaskResult.Workspace (and the report) so a failure
// can be inspected post-mortem. Passed tasks are still cleaned up. The caller
// owns removing the kept dirs.
func KeepFailedWorkspaces() SuiteOption { return func(o *suiteOpts) { o.keepFailed = true } }

// WithEffort pins the reasoning effort for every task in the suite (e.g. "none"
// to keep the eval cheap and deterministic in cost). Empty keeps the agent's
// auto-escalating default.
func WithEffort(effort string) SuiteOption { return func(o *suiteOpts) { o.effort = effort } }

// WithMaxSteps caps each task's tool-call steps (bounds worst-case token spend).
func WithMaxSteps(n int) SuiteOption { return func(o *suiteOpts) { o.maxSteps = n } }

// WithConcurrency runs up to n tasks in parallel (the big wall-clock win). It is
// only honored when the effort is PINNED: with auto-effort the agent mutates the
// shared model client's reasoning level mid-run, which isn't safe to race, so an
// unpinned suite falls back to sequential regardless of n.
func WithConcurrency(n int) SuiteOption { return func(o *suiteOpts) { o.concurrency = n } }

// statsUI wraps the per-task UI and sums the tokens / steps / tool calls the
// agent reports, so the suite can build per-task metrics without the agent
// needing to expose cumulative counters.
// statsUI's counters are mutated from multiple goroutines: the loop runs a batch
// of independent read-only tools CONCURRENTLY, so ToolCall/ToolResult fire in
// parallel (the agent.UI contract is concurrency-safe). A mutex guards the sums;
// the suite reads them only after RunTask returns (all tool goroutines joined).
type statsUI struct {
	agent.UI
	mu                                     sync.Mutex
	in, cached, out, steps, tools, batches int
	edits, firstEditStep                   int
}

// ToolBatch fires when the loop runs ≥1 independent read-only tool calls
// concurrently; n>1 is a genuine PARALLEL batch — the efficiency behavior we
// measure (a model that batches independent probes vs single-steps them).
func (s *statsUI) ToolBatch(n int) {
	s.mu.Lock()
	if n > 1 {
		s.batches++
	}
	s.mu.Unlock()
	s.UI.ToolBatch(n)
}

func (s *statsUI) AssistantEnd(st agent.Stats) {
	s.mu.Lock()
	s.in += st.InTokens
	s.cached += st.CachedTokens
	s.out += st.OutTokens
	if st.InTokens > 0 || st.OutTokens > 0 {
		s.steps++ // a token-bearing turn; the zero-token Stats{} flushes don't count
	}
	s.mu.Unlock()
	s.UI.AssistantEnd(st)
}

func (s *statsUI) ToolCall(name, args string) {
	s.mu.Lock()
	s.tools++
	s.mu.Unlock()
	s.UI.ToolCall(name, args)
}

// ToolResult counts a SUCCESSFUL edit (ok && an edit tool) and records the step the
// first one landed — the "did it act, and how soon" efficiency signal. Steps is read
// under the same lock; ToolResult fires after the turn's AssistantEnd, so steps is
// the step number on which the edit was issued.
func (s *statsUI) ToolResult(name string, ok bool, summary string) {
	s.mu.Lock()
	if ok && agent.IsEditTool(name) {
		s.edits++
		if s.firstEditStep == 0 {
			s.firstEditStep = s.steps
		}
	}
	s.mu.Unlock()
	s.UI.ToolResult(name, ok, summary)
}

// RunSuite runs each task in its own fresh temp workspace with the given model,
// collecting a Report. newUI yields a per-task UI (so permission state etc. don't
// bleed between tasks). The same model client is reused across tasks; RunTask
// builds a fresh agent each time, so conversations never bleed either. Tasks may
// run in parallel (WithConcurrency) — results stay in task order regardless.
func RunSuite(ctx context.Context, tasks []Task, model agent.LLM, newUI func() agent.UI, opts ...SuiteOption) Report {
	var o suiteOpts
	for _, f := range opts {
		f(&o)
	}

	conc := o.concurrency
	if conc < 1 || o.effort == "" {
		// Sequential when unbounded-unset, or when effort is auto (escalation
		// mutates the shared client — racy under parallelism).
		conc = 1
	}

	runOpts := []RunOption{RunWithEffort(o.effort), RunWithMaxSteps(o.maxSteps)}
	results := make([]TaskResult, len(tasks))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup

	for i, tk := range tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, tk Task) {
			defer wg.Done()
			defer func() { <-sem }()

			ws, err := os.MkdirTemp("", "borg-eval-*")
			if err != nil {
				results[i] = TaskResult{Name: tk.Name, Err: err}
				return
			}
			su := &statsUI{UI: newUI()}
			start := time.Now()
			res, runErr := RunTask(ctx, tk, ws, model, su, runOpts...)
			tr := TaskResult{
				Name: tk.Name, Result: res, Err: runErr,
				InTokens: su.in, CachedTokens: su.cached, OutTokens: su.out,
				Steps: su.steps, ToolCalls: su.tools, ParallelBatches: su.batches, Duration: time.Since(start),
				EditsLanded: su.edits, FirstEditStep: su.firstEditStep,
			}
			// Latency/wander guard: a correct-but-slow run (too many steps) is a
			// regression even when the oracle passed. Steps ≈ wall-clock, so this
			// flags a /learn that over-explores before the explore-budget force-write.
			if tk.StepBudget > 0 && runErr == nil && tr.Steps > tk.StepBudget && tr.Result.Pass {
				tr.Result = Result{Reason: fmt.Sprintf("step budget exceeded: %d > %d (over-exploration/wander regression)", tr.Steps, tk.StepBudget)}
			}
			if o.keepFailed && !tr.ok() {
				tr.Workspace = ws // leave it for inspection; caller cleans up
			} else {
				_ = os.RemoveAll(ws)
			}
			results[i] = tr
		}(i, tk)
	}
	wg.Wait()

	return Report{Tasks: results}
}
