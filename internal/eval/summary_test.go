package eval_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/turborg/borg/internal/eval"
)

// twoModelReports builds a floko (1/3) vs chuppa (3/3) pair with full per-task
// metrics, for exercising the rich rendering deterministically.
func twoModelReports() []eval.Report {
	floko := eval.Report{Model: "floko", Tasks: []eval.TaskResult{
		{Name: "fix-compile-error", Result: eval.Result{Pass: true}, Steps: 6, InTokens: 200_000, OutTokens: 20_000, CachedTokens: 50_000, Duration: 60 * time.Second},
		{Name: "fix-off-by-one", Result: eval.Result{Pass: false, Reason: "x = 3, want 4"}, Steps: 16, InTokens: 300_000, OutTokens: 30_000, CachedTokens: 60_000, Duration: 120 * time.Second},
		{Name: "grep-and-fix", Err: os.ErrPermission},
	}}
	chuppa := eval.Report{Model: "chuppa", Tasks: []eval.TaskResult{
		{Name: "fix-compile-error", Result: eval.Result{Pass: true}, Steps: 3, InTokens: 100_000, OutTokens: 8_000, CachedTokens: 80_000, Duration: 30 * time.Second},
		{Name: "fix-off-by-one", Result: eval.Result{Pass: true}, Steps: 4, InTokens: 120_000, OutTokens: 9_000, CachedTokens: 90_000, Duration: 40 * time.Second},
		{Name: "grep-and-fix", Result: eval.Result{Pass: true}, Steps: 5, InTokens: 90_000, OutTokens: 7_000, CachedTokens: 70_000, Duration: 35 * time.Second},
	}}
	return []eval.Report{floko, chuppa}
}

func TestHumanIntAndDur(t *testing.T) {
	require.Equal(t, "942", eval.HumanIntForTest(942))
	require.Equal(t, "12.0k", eval.HumanIntForTest(12_044))
	require.Equal(t, "1.0M", eval.HumanIntForTest(1_048_576))

	require.Equal(t, "8.2s", eval.HumanDurForTest(8200*time.Millisecond))
	require.Equal(t, "1m32s", eval.HumanDurForTest(92*time.Second))
}

func TestReportDetail(t *testing.T) {
	r := twoModelReports()[0] // floko 1/3
	d := r.Detail()

	require.Contains(t, d, "model floko — 1/3 passed")
	require.Contains(t, d, "fix-compile-error")
	require.Contains(t, d, "PASS")
	require.Contains(t, d, "FAIL")
	require.Contains(t, d, "x = 3, want 4") // failure reason inline
	require.Contains(t, d, "ERR")           // the errored task
	require.Contains(t, d, "TOTAL")
	require.Contains(t, d, "tool calls")
	require.Contains(t, d, "cache:") // it billed tokens
}

func TestCompareReportsRich(t *testing.T) {
	s := eval.CompareReports(twoModelReports())

	// sections present
	require.Contains(t, s, "pass-rate (higher is better)")
	require.Contains(t, s, "input tokens (lower is better)")
	require.Contains(t, s, "output tokens (lower is better)")
	require.Contains(t, s, "avg steps/task (lower is better)")
	require.Contains(t, s, "per-task verdict")
	require.Contains(t, s, "aggregate")

	// bar chars + scaled values
	require.Contains(t, s, "█")
	require.Contains(t, s, "░")
	require.Contains(t, s, "1/3 (33%)")
	require.Contains(t, s, "3/3 (100%)")

	// winner: chuppa solved more
	require.Contains(t, s, "verdict:")
	require.Contains(t, s, "chuppa wins — solved 3/3 vs floko's 1/3 (+2 tasks)")
}

func TestCompareReportsSingleModelNoVerdict(t *testing.T) {
	s := eval.CompareReports(twoModelReports()[:1]) // floko only
	require.Contains(t, s, "pass-rate")
	require.NotContains(t, s, "verdict:") // needs ≥2 models
}

func TestCompareReportsEmpty(t *testing.T) {
	require.Contains(t, eval.CompareReports(nil), "eval comparison")
}

// When both models solve the same count, the winner is decided on efficiency
// (fewer input tokens, then fewer steps).
func TestWinnerByEfficiency(t *testing.T) {
	lean := eval.Report{Model: "chuppa", Tasks: []eval.TaskResult{
		{Name: "a", Result: eval.Result{Pass: true}, InTokens: 100_000, Steps: 4},
	}}
	heavy := eval.Report{Model: "floko", Tasks: []eval.TaskResult{
		{Name: "a", Result: eval.Result{Pass: true}, InTokens: 200_000, Steps: 8},
	}}
	s := eval.CompareReports([]eval.Report{heavy, lean})
	require.Contains(t, s, "chuppa wins on efficiency")
	require.Contains(t, s, "2.0× fewer input tokens")
	require.Contains(t, s, "2.0× fewer steps")
}

// Identical metrics ⇒ an explicit tie.
func TestWinnerTie(t *testing.T) {
	a := eval.Report{Model: "floko", Tasks: []eval.TaskResult{{Name: "x", Result: eval.Result{Pass: true}, InTokens: 100, Steps: 2}}}
	b := eval.Report{Model: "chuppa", Tasks: []eval.TaskResult{{Name: "x", Result: eval.Result{Pass: true}, InTokens: 100, Steps: 2}}}
	require.Contains(t, eval.CompareReports([]eval.Report{a, b}), "tie —")
}

func TestRenderFullDocument(t *testing.T) {
	out := eval.Render(twoModelReports())
	require.Contains(t, out, "eval comparison") // comparison block
	require.Contains(t, out, "model floko")     // per-model detail
	require.Contains(t, out, "model chuppa")    // per-model detail
	require.Contains(t, out, "fix-compile-error")
}

// Tables stay aligned: a very long task name is truncated with an ellipsis.
func TestDetailTruncatesLongName(t *testing.T) {
	r := eval.Report{Model: "m", Tasks: []eval.TaskResult{
		{Name: strings.Repeat("verylongtaskname", 4), Result: eval.Result{Pass: true}, InTokens: 10},
	}}
	require.Contains(t, r.Detail(), "…")
}
