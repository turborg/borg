package eval_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/turborg/borg/internal/eval"
)

func TestBaselineRoundtripAndRegressions(t *testing.T) {
	good := eval.Report{Model: "chuppa", Tasks: []eval.TaskResult{
		{Name: "t1", Result: eval.Result{Pass: true}},
		{Name: "t2", Result: eval.Result{Pass: true}},
	}}

	path := filepath.Join(t.TempDir(), "baseline.json")
	require.NoError(t, eval.SaveBaseline(path, []eval.Report{good}))

	base, err := eval.LoadBaseline(path)
	require.NoError(t, err)
	require.True(t, base["chuppa"]["t1"].Passed)
	require.True(t, base["chuppa"]["t2"].Passed)

	// A later run: t2 regressed (was passing), t3 is new and passing (improved).
	later := eval.Report{Model: "chuppa", Tasks: []eval.TaskResult{
		{Name: "t1", Result: eval.Result{Pass: true}},
		{Name: "t2", Result: eval.Result{Pass: false, Reason: "broke"}},
		{Name: "t3", Result: eval.Result{Pass: true}},
	}}
	regressed, improved := later.RegressionsVs(base)
	require.Equal(t, []string{"t2"}, regressed)
	require.Equal(t, []string{"t3"}, improved)

	s := eval.RegressionSummary([]eval.Report{later}, base)
	require.Contains(t, s, "⚠ REGRESSED: t2")
	require.Contains(t, s, "newly passing: t3")
	require.Contains(t, s, "investigate before updating the baseline")
}

func TestLoadBaselineMissingIsEmpty(t *testing.T) {
	base, err := eval.LoadBaseline(filepath.Join(t.TempDir(), "nope.json"))
	require.NoError(t, err)
	require.Empty(t, base)
}

func TestRegressionSummaryNoBaselineAndClean(t *testing.T) {
	// A model with no baseline entry is reported as such, not as a regression.
	newModel := eval.Report{Model: "newmodel", Tasks: []eval.TaskResult{{Name: "t1", Result: eval.Result{Pass: true}}}}
	require.Contains(t, eval.RegressionSummary([]eval.Report{newModel}, eval.Baseline{}), "no baseline yet")

	// A model in the baseline with all tasks still green reports ✓.
	base := eval.Baseline{"m": {"t1": eval.TaskBaseline{Passed: true}}}
	clean := eval.Report{Model: "m", Tasks: []eval.TaskResult{{Name: "t1", Result: eval.Result{Pass: true}}}}
	out := eval.RegressionSummary([]eval.Report{clean}, base)
	require.Contains(t, out, "✓ no regressions")
	require.NotContains(t, out, "REGRESSED")
}

// Efficiency drift: a still-passing task that gets materially slower/costlier is
// flagged (and counts as a regression for the summary); a leaner one is noted.
func TestEfficiencyDrift(t *testing.T) {
	base := eval.Baseline{"chuppa": {
		"steady":  eval.TaskBaseline{Passed: true, Steps: 5, InTokens: 20000},
		"blew-up": eval.TaskBaseline{Passed: true, Steps: 5, InTokens: 20000},
		"leaner":  eval.TaskBaseline{Passed: true, Steps: 10, InTokens: 40000},
	}}
	now := eval.Report{Model: "chuppa", Tasks: []eval.TaskResult{
		{Name: "steady", Result: eval.Result{Pass: true}, Steps: 5, InTokens: 21000},   // within margin
		{Name: "blew-up", Result: eval.Result{Pass: true}, Steps: 12, InTokens: 60000}, // >30% worse
		{Name: "leaner", Result: eval.Result{Pass: true}, Steps: 5, InTokens: 18000},   // >30% better
	}}

	slower, faster := now.EfficiencyVs(base)
	require.Len(t, slower, 1)
	require.Contains(t, slower[0], "blew-up")
	require.Len(t, faster, 1)
	require.Contains(t, faster[0], "leaner")

	s := eval.RegressionSummary([]eval.Report{now}, base)
	require.Contains(t, s, "slower/costlier")
	require.Contains(t, s, "leaner")
	require.Contains(t, s, "investigate before updating the baseline") // drift counts as a regression
}

// The committed baseline must stay valid: parseable, non-empty, and referencing
// only real corpus task names (so a rename/removal can't leave it pointing at a
// ghost task and silently never flag a regression).
func TestCommittedBaselineIsValid(t *testing.T) {
	base, err := eval.LoadBaseline("testdata/baseline.json")
	require.NoError(t, err)
	require.NotEmpty(t, base["chuppa"], "committed baseline should score chuppa")

	known := map[string]bool{}
	for _, tk := range eval.Corpus() {
		known[tk.Name] = true
	}
	for task := range base["chuppa"] {
		require.Truef(t, known[task], "baseline references unknown task %q", task)
	}
}
