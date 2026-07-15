package eval_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/turborg/borg/internal/auth"
	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/eval"
	"github.com/turborg/borg/internal/llm"
	"github.com/turborg/borg/internal/tools"
)

// A task run through a (deterministic) model is scored pass/fail by its oracle —
// the full eval shape, no tokens. Swap the fakeModel for a Player or the live
// client and the same task scores those instead.
func TestRunTaskScoredByOracle(t *testing.T) {
	ws := t.TempDir()
	answer := filepath.Join(ws, "answer.txt")

	task := eval.Task{
		Name:   "write-the-answer",
		Prompt: "write 42 to answer.txt",
		Oracle: eval.FileEquals("answer.txt", "42"),
	}

	// Model does the right thing → oracle passes.
	good := &fakeModel{steps: []llm.Message{
		callTool("1", "write_file", `{"path":"`+answer+`","content":"42"}`),
		say("done"),
	}}
	res, err := eval.RunTask(context.Background(), task, ws, good, &recUI{})
	require.NoError(t, err)
	require.True(t, res.Pass, res.Reason)

	// Model writes the wrong thing → oracle fails with a debuggable reason.
	require.NoError(t, os.Remove(answer))
	bad := &fakeModel{steps: []llm.Message{
		callTool("1", "write_file", `{"path":"`+answer+`","content":"WRONG"}`),
		say("done"),
	}}
	res, err = eval.RunTask(context.Background(), task, ws, bad, &recUI{})
	require.NoError(t, err)
	require.False(t, res.Pass)
	require.Contains(t, res.Reason, "want")
}

func TestRunTaskSetupAndNilOracle(t *testing.T) {
	ws := t.TempDir()

	// Setup error short-circuits.
	_, err := eval.RunTask(context.Background(), eval.Task{
		Name:  "bad-setup",
		Setup: func(string) error { return os.ErrPermission },
	}, ws, &fakeModel{steps: []llm.Message{say("x")}}, &recUI{})
	require.Error(t, err)

	// No oracle ⇒ "ran without error" passes; Setup may seed the fixture.
	seeded := false
	res, err := eval.RunTask(context.Background(), eval.Task{
		Name:   "no-oracle",
		Prompt: "do nothing",
		Setup:  func(string) error { seeded = true; return nil },
	}, ws, &fakeModel{steps: []llm.Message{say("ok")}}, &recUI{})
	require.NoError(t, err)
	require.True(t, res.Pass)
	require.True(t, seeded)
}

func TestFileEqualsMissingFile(t *testing.T) {
	res := eval.FileEquals("nope.txt", "x")(t.TempDir())
	require.False(t, res.Pass)
	require.Contains(t, res.Reason, "read nope.txt")
}

// liveClient builds the real metered client pinned to model, or skips when the
// live eval isn't enabled. Set BORG_EVAL=1 and be logged in (`borg auth login`).
// Status() mints a fresh access token from the stored refresh token when the
// access token has expired — required for unattended (nightly) runs, where the
// stashed access token is always stale.
//
// Axiom is refused outright: it's our most expensive model and must never be
// driven by the full-corpus eval (a runaway would burn real budget). The eval
// runs only the cheap models — floko + chuppa.
func liveClient(t *testing.T, model string) *llm.Client {
	t.Helper()
	if os.Getenv("BORG_EVAL") != "1" {
		t.Skip("set BORG_EVAL=1 and log in to run the live eval (costs tokens)")
	}
	if strings.EqualFold(model, "axiom") && !axiomAllowed() {
		t.Fatalf("refusing axiom — set BORG_EVAL_ALLOW_AXIOM=1 to permit a gated, subset-only measurement (and cap the account budget server-side first)")
	}
	cfg, err := config.Load()
	require.NoError(t, err)
	a, err := auth.New(cfg)
	require.NoError(t, err)
	creds, err := a.Status(context.Background())
	require.NoError(t, err)
	client := llm.New(cfg, creds.AccessToken)
	client.SetModel(model)
	// Close the live client's pooled connections when the test ends, so the
	// HTTP/2 keep-alive readLoop goroutine doesn't linger past the run and trip
	// goleak (the eval passes, then a leaked idle conn would fail the package).
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func liveFlokoClient(t *testing.T) *llm.Client { return liveClient(t, "floko") }

// evalModels is the set of models the live eval drives, from BORG_EVAL_MODELS
// (comma-separated), defaulting to the two cheap models: floko and chuppa. Axiom
// is rejected here too (in addition to liveClient's backstop) so a misconfigured
// env fails loudly with a clear message rather than silently spending.
func evalModels(t *testing.T) []string {
	t.Helper()
	raw := os.Getenv("BORG_EVAL_MODELS")
	if strings.TrimSpace(raw) == "" {
		raw = "floko,chuppa"
	}
	var models []string
	for _, m := range strings.Split(raw, ",") {
		m = strings.TrimSpace(strings.ToLower(m))
		if m == "" {
			continue
		}
		if m == "axiom" && !axiomAllowed() {
			t.Fatalf("BORG_EVAL_MODELS includes axiom — set BORG_EVAL_ALLOW_AXIOM=1 to permit a gated, subset-only measurement (cap the account budget server-side first)")
		}
		models = append(models, m)
	}
	return models
}

// TestSchemaContractLiveFloko is a near-free live smoke test: it sends borg's FULL
// tool set (including the finish tool, which switches the backend to tool_choice=
// required + guided decoding) to the live model in ONE turn, asserting the backend
// can COMPILE a grammar from every advertised tool schema. This catches the class
// of "valid-locally, rejected-by-backend" schema bugs that NO mocked-model test
// can see — e.g. an empty `properties:{}` that the PHP proxy mangles into `[]`,
// which broke every Floko turn until #48. It needs no task to succeed; the request
// merely has to not error. Run it first in the nightly suite as a canary.
func TestSchemaContractLiveFloko(t *testing.T) {
	client := liveFlokoClient(t)

	var defs []llm.Tool
	for _, d := range tools.DefaultRegistry().Definitions() {
		defs = append(defs, llm.Tool{Type: "function", Function: llm.ToolFunction{
			Name:        d.Function.Name,
			Description: d.Function.Description,
			Parameters:  d.Function.Parameters,
		}})
	}

	msgs := []llm.Message{
		{Role: "system", Content: "You are a tool-using assistant. To answer, call the finish tool."},
		{Role: "user", Content: "Reply with a one-word greeting."},
	}
	_, err := client.Chat(context.Background(), msgs, defs, false, func(string) {})
	require.NoError(t, err, "every advertised tool schema must compile under the backend's guided decoding")
}

// Eval-run knobs (all env-overridable) — these tune ONLY the eval harness, never
// borg's shipped agent defaults.
const (
	defaultEvalEffort   = "none" // reasoning off: cheap + fast, and disables auto-escalation (also enables parallelism)
	defaultEvalMaxSteps = 16     // per-task step cap for the eval (prod's defaultMaxSteps is 60)
	defaultEvalConc     = 4      // tasks in parallel
	maxAxiomTasks       = 4      // axiom (premium) is gated to at most this many tasks per run, ever
)

// axiomAllowed reports whether the operator explicitly enabled an axiom run. Even
// then it's confined to a small subset (see guardAxiomScope) — axiom is ~60×
// chuppa, so an unbounded run could burn real spend.
func axiomAllowed() bool { return os.Getenv("BORG_EVAL_ALLOW_AXIOM") == "1" }

// guardAxiomScope confines a permitted axiom run to an explicit, small subset:
// BORG_EVAL_TASKS must be set and resolve to ≤ maxAxiomTasks tasks. This is the
// code-side spend cap (pair it with a server-side account budget cap). No-op when
// axiom isn't among the models.
func guardAxiomScope(t *testing.T, models []string, corpus []eval.Task) {
	t.Helper()
	for _, m := range models {
		if m != "axiom" {
			continue
		}
		require.NotEmptyf(t, evalTasks(), "axiom needs an explicit BORG_EVAL_TASKS subset (≤%d tasks)", maxAxiomTasks)
		require.LessOrEqualf(t, len(corpus), maxAxiomTasks,
			"axiom is gated to ≤%d tasks — narrow BORG_EVAL_TASKS (got %d)", maxAxiomTasks, len(corpus))
		return
	}
}

// envInt reads an int env var, falling back to def when unset/blank/invalid.
func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// evalTasks reads BORG_EVAL_TASKS (comma-separated task names) — empty means the
// whole corpus. Lets the nightly run a cheap curated subset.
func evalTasks() []string {
	var names []string
	for _, n := range strings.Split(os.Getenv("BORG_EVAL_TASKS"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// TestEvalLive is the real thing: it runs the agent against each configured live
// model (floko + chuppa by default — never axiom) over the corpus, scores each
// objectively, and logs a side-by-side comparison. It's opt-in (costs tokens) —
// set BORG_EVAL=1 and be logged in (`borg auth login`). This is the nightly
// quality signal: track each model's pass-rate over time, and see where chuppa
// (our strongest coding model) clears tasks floko can't.
//
// Knobs (all eval-only): BORG_EVAL_MODELS, BORG_EVAL_EFFORT (default none),
// BORG_EVAL_MAX_STEPS (16), BORG_EVAL_CONCURRENCY (4), BORG_EVAL_TASKS (subset).
func TestEvalLive(t *testing.T) {
	if os.Getenv("BORG_EVAL") != "1" {
		t.Skip("set BORG_EVAL=1 and log in to run the live eval (costs tokens)")
	}

	corpus := eval.FilterTasks(eval.Corpus(), evalTasks())
	require.NotEmpty(t, corpus, "BORG_EVAL_TASKS matched no corpus tasks")
	guardAxiomScope(t, evalModels(t), corpus)

	effort := defaultEvalEffort
	if v, ok := os.LookupEnv("BORG_EVAL_EFFORT"); ok {
		effort = v // allow explicit "" to restore auto-escalation
	}
	suiteOpts := []eval.SuiteOption{
		eval.KeepFailedWorkspaces(), // a local `make eval` leaves failures for inspection
		eval.WithEffort(effort),
		eval.WithMaxSteps(envInt("BORG_EVAL_MAX_STEPS", defaultEvalMaxSteps)),
		eval.WithConcurrency(envInt("BORG_EVAL_CONCURRENCY", defaultEvalConc)),
	}

	// Run the corpus per model. We don't require 100% (the point is to track
	// pass-rate over time), but each model must solve at least one task —
	// otherwise auth/proxy/loop is broken, not the model. The report is written
	// after EACH model so a timeout or budget-exhaust still leaves a partial
	// report instead of nothing.
	var reports []eval.Report
	for _, model := range evalModels(t) {
		client := liveClient(t, model)
		rep := eval.RunSuite(context.Background(), corpus, client, newRecUI, suiteOpts...)
		rep.Model = model
		t.Logf("\n%s", rep.Detail())
		reports = append(reports, rep)
		writeEvalReport(t, reports)

		require.Equal(t, len(corpus), rep.Total())
		require.GreaterOrEqualf(t, rep.Passed(), 1, "live model %s solved 0/%d corpus tasks:\n%s", model, rep.Total(), rep)
	}

	base, err := eval.LoadBaseline(baselinePath)
	require.NoError(t, err)
	t.Logf("\n%s\n%s", eval.RegressionSummary(reports, base), eval.CompareReports(reports))

	// BORG_EVAL_SAVE_BASELINE=1 locks the current results in as the new reference
	// (do this intentionally after a verified-good run, e.g. once new tasks pass).
	if os.Getenv("BORG_EVAL_SAVE_BASELINE") == "1" {
		require.NoError(t, eval.SaveBaseline(baselinePath, reports))
		t.Logf("saved new baseline to %s", baselinePath)
	}
}

// TestEvalMeasureRepeated is the measurement harness: it runs the corpus N times
// (BORG_EVAL_REPEAT) for ONE model and reports the AGGREGATE — mean ± range of
// pass-rate, avg-steps, and parallel-batches — so a prompt/harness change is
// judged by a rate across runs, not a single transcript (the anti-overfit tool).
// Opt-in and N>=2; costs N× the tokens, so use a subset (BORG_EVAL_TASKS) to keep
//
//	it cheap. Example: BORG_EVAL=1 BORG_EVAL_REPEAT=5 BORG_EVAL_MODELS=chuppa \
//	  BORG_EVAL_TASKS=grep-and-fix,trace-call-chain make eval
func TestEvalMeasureRepeated(t *testing.T) {
	runs := envInt("BORG_EVAL_REPEAT", 1)
	if os.Getenv("BORG_EVAL") != "1" || runs < 2 {
		t.Skip("set BORG_EVAL=1 and BORG_EVAL_REPEAT>=2 to run the repeated measurement (costs N× tokens)")
	}

	model := evalModels(t)[0] // one model keeps the N-run cost bounded (default chuppa)
	corpus := eval.FilterTasks(eval.Corpus(), evalTasks())
	require.NotEmpty(t, corpus, "BORG_EVAL_TASKS matched no corpus tasks")
	guardAxiomScope(t, []string{model}, corpus) // N× runs on axiom: gate hardest of all

	effort := defaultEvalEffort
	if v, ok := os.LookupEnv("BORG_EVAL_EFFORT"); ok {
		effort = v
	}
	opts := []eval.SuiteOption{
		eval.WithEffort(effort),
		eval.WithMaxSteps(envInt("BORG_EVAL_MAX_STEPS", defaultEvalMaxSteps)),
		eval.WithConcurrency(envInt("BORG_EVAL_CONCURRENCY", defaultEvalConc)),
	}

	client := liveClient(t, model)
	all := eval.RunRepeated(context.Background(), corpus, client, newRecUI, runs, opts...)
	for i := range all {
		all[i].Model = model
	}
	agg := eval.AggregateRuns(all)
	t.Logf("\n%s", agg)
	if path := os.Getenv("BORG_EVAL_REPORT"); path != "" {
		require.NoError(t, os.WriteFile(path, []byte(agg), 0o644))
	}
}

// TestEvalNoOpConvergence is the non-deterministic regression test for THIS
// incident: the 1.35M-token /upgrade session ran 45 steps on a task whose work was
// already done. It runs the NoOpCorpus (fixtures that already build + pass) N times
// against the real model and asserts borg CONVERGES — recognizes "nothing to do"
// within each task's tight StepBudget (RunSuite fails a task that runs past it) while
// keeping the code green (the oracle). The signal is a RATE across runs, not one
// transcript: a harness regression that re-introduces the explore-to-the-cap thrash
// drops the pass fraction below the floor. Gated + opt-in (costs tokens).
func TestEvalNoOpConvergence(t *testing.T) {
	if os.Getenv("BORG_EVAL") != "1" {
		t.Skip("set BORG_EVAL=1 and log in to run the live no-op convergence eval (costs tokens)")
	}
	runs := envInt("BORG_EVAL_REPEAT", 3)
	if runs < 1 {
		runs = 1
	}
	model := evalModels(t)[0] // one model keeps the N-run cost bounded (default chuppa)
	corpus := eval.NoOpCorpus()

	opts := []eval.SuiteOption{
		eval.WithEffort(defaultEvalEffort),
		// A real prod-shaped cap: the no-op StepBudget (not this) is the convergence
		// line; this just bounds the worst case so a regression can't burn unbounded.
		eval.WithMaxSteps(envInt("BORG_EVAL_MAX_STEPS", defaultEvalMaxSteps)),
		eval.WithConcurrency(envInt("BORG_EVAL_CONCURRENCY", defaultEvalConc)),
	}

	client := liveClient(t, model)
	all := eval.RunRepeated(context.Background(), corpus, client, newRecUI, runs, opts...)

	var passed, total int
	for i := range all {
		all[i].Model = model
		passed += all[i].Passed()
		total += all[i].Total()
		t.Logf("run %d/%d: %d/%d converged under budget", i+1, runs, all[i].Passed(), all[i].Total())
	}
	t.Logf("\n%s", eval.AggregateRuns(all))
	require.Positive(t, total)
	// Floor: the majority of (task × run) trials must converge under budget. With the
	// finish-brake + explore-nudge in place this should be near 1.0; a regression that
	// brings back the 45-step thrash blows the StepBudget and drops this below 0.5.
	rate := float64(passed) / float64(total)
	require.GreaterOrEqualf(t, rate, 0.5,
		"no-op convergence rate %.0f%% (%d/%d) — borg is exploring past completion instead of finishing",
		rate*100, passed, total)
}

// TestEvalCacheFloor guards prefix-cache effectiveness: it runs the corpus once and
// asserts the aggregate cache-hit rate stays above a floor. The observed rate is
// ~46% (backend eviction); a determinism regression (a non-byte-stable prefix) or a
// proxy change that drops the prompt_cache_key would tank it. Floor is set well below
// the current level so it flags a real drop, not normal variance. Live-only (a
// cassette doesn't bill cache), gated + opt-in.
func TestEvalCacheFloor(t *testing.T) {
	if os.Getenv("BORG_EVAL") != "1" {
		t.Skip("set BORG_EVAL=1 and log in to run the live cache-floor eval (costs tokens)")
	}
	const cacheFloorPct = 30 // current ~46%; below this means caching/determinism regressed
	model := evalModels(t)[0]
	corpus := eval.FilterTasks(eval.Corpus(), evalTasks())
	require.NotEmpty(t, corpus)
	guardAxiomScope(t, []string{model}, corpus)

	opts := []eval.SuiteOption{
		eval.WithEffort(defaultEvalEffort),
		eval.WithMaxSteps(envInt("BORG_EVAL_MAX_STEPS", defaultEvalMaxSteps)),
		eval.WithConcurrency(envInt("BORG_EVAL_CONCURRENCY", defaultEvalConc)),
	}
	client := liveClient(t, model)
	rep := eval.RunSuite(context.Background(), corpus, client, newRecUI, opts...)
	rep.Model = model
	t.Logf("\n%s", rep.Detail())
	in, cached := rep.Tokens()
	require.Positive(t, in)
	require.GreaterOrEqualf(t, rep.CachePct(), cacheFloorPct,
		"prefix-cache hit %d%% (%d/%d tok) below floor %d%% — caching/determinism or the prompt_cache_key path regressed",
		rep.CachePct(), cached, in, cacheFloorPct)
}

// baselinePath is the committed per-model scorecard the live run is checked
// against for regressions (relative to the package dir, where `go test` runs).
const baselinePath = "testdata/baseline.json"

// writeEvalReport renders the regression check + comparison + per-model detail to
// BORG_EVAL_REPORT (if set). Called after every model so the artifact survives an
// aborted run; the regression header makes "did we break anything?" answerable
// at a glance and lands at the top of the GitHub run summary.
func writeEvalReport(t *testing.T, reports []eval.Report) {
	t.Helper()
	path := os.Getenv("BORG_EVAL_REPORT")
	if path == "" {
		return
	}
	base, err := eval.LoadBaseline(baselinePath)
	require.NoError(t, err)
	doc := eval.RegressionSummary(reports, base) + "\n" + eval.Render(reports)
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o644))
}
