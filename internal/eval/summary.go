package eval

// Human-readable, deterministic eval reporting: per-model detail tables, an
// at-a-glance model-vs-model comparison with ASCII bar "graphs", and a winner
// verdict. All rendering is a pure function of the Report data — fully
// deterministic and unit-tested — so the live numbers vary but the format never
// does. ASCII (not image) graphs keep the report a plain-text artifact: greppable,
// diffable across runs, zero dependencies.

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// barWidth is the cell count of a chart bar. Kept modest so a row fits a terminal.
const barWidth = 24

// bar renders a proportional unicode bar for value/max over barWidth cells.
func bar(value, max float64) string {
	if max <= 0 {
		return strings.Repeat("░", barWidth)
	}
	filled := int(math.Round(value / max * float64(barWidth)))
	if filled < 0 {
		filled = 0
	}
	if filled > barWidth {
		filled = barWidth
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
}

// humanInt compacts a token count: 942 → "942", 12_044 → "12.0k", 1_048_576 → "1.0M".
func humanInt(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// humanDur compacts a duration: 8.2s, 1m32s.
func humanDur(d time.Duration) string {
	d = d.Round(100 * time.Millisecond)
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d / time.Minute)
	s := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm%02ds", m, s)
}

// verdict is a task's one-word outcome for a model.
func verdict(tr TaskResult, ok bool) string {
	switch {
	case ok:
		return "PASS"
	case tr.Err != nil:
		return "ERR"
	default:
		return "FAIL"
	}
}

// Detail renders a model's per-task table — verdict, steps, tokens, cache, time —
// plus a totals row. This is the in-depth "where did it spend effort" view.
func (r Report) Detail() string {
	var b strings.Builder
	fmt.Fprintf(&b, "eval report — model %s — %d/%d passed\n", r.Model, r.Passed(), r.Total())
	if cl := r.cacheLine(); cl != "" {
		fmt.Fprintf(&b, "  %s\n", cl)
	}

	fmt.Fprintf(&b, "  %-26s %-6s %5s %7s %8s %8s %8s %7s\n",
		"task", "result", "steps", "1st-ed", "in", "out", "cached", "time")
	for _, tr := range r.Tasks {
		reason := ""
		if !tr.ok() {
			if tr.Err != nil {
				reason = "  " + tr.Err.Error()
			} else if tr.Result.Reason != "" {
				reason = "  " + tr.Result.Reason
			}
		}
		fmt.Fprintf(&b, "  %-26s %-6s %5d %7s %8s %8s %8s %7s%s\n",
			truncate(tr.Name, 26), verdict(tr, tr.ok()), tr.Steps, firstEditCol(tr),
			humanInt(tr.InTokens), humanInt(tr.OutTokens), humanInt(tr.CachedTokens),
			humanDur(tr.Duration), reason)
	}

	in, cached, out, steps, tools, dur := r.Totals()
	fmt.Fprintf(&b, "  %-26s %-6s %5d %7s %8s %8s %8s %7s  (%d tool calls)\n",
		"TOTAL", fmt.Sprintf("%d/%d", r.Passed(), r.Total()), steps, "",
		humanInt(in), humanInt(out), humanInt(cached), humanDur(dur), tools)
	return b.String()
}

// firstEditCol renders the "step the first edit landed" cell: a dash when the model
// never edited (the all-search-no-action shape), else the step number. A high number
// (late first edit) or a dash on a fix task flags exploration that should've acted.
func firstEditCol(tr TaskResult) string {
	if tr.FirstEditStep == 0 {
		return "—"
	}
	return strconv.Itoa(tr.FirstEditStep)
}

// CompareReports renders the model-vs-model comparison: a pass-rate + token +
// step bar chart, a per-task verdict matrix, an aggregate table, and a winner
// verdict. Reads task order from the first report; a model missing a task shows
// "—". Safe for one report (skips the cross-model sections).
func CompareReports(reports []Report) string {
	var b strings.Builder
	b.WriteString("eval comparison\n")
	for _, r := range reports {
		fmt.Fprintf(&b, "  %-14s %d/%d passed", r.Model, r.Passed(), r.Total())
		if cl := r.cacheLine(); cl != "" {
			fmt.Fprintf(&b, "  (%s)", cl)
		}
		b.WriteByte('\n')
	}
	if len(reports) == 0 {
		return b.String()
	}

	b.WriteString(chartPassRate(reports))
	b.WriteString(chartTokens(reports))
	b.WriteString(chartAvgSteps(reports))
	b.WriteString(matrix(reports))
	b.WriteString(aggregateTable(reports))
	if len(reports) >= 2 {
		b.WriteString("\nverdict: " + winner(reports) + "\n")
	}
	return b.String()
}

// chartPassRate: higher is better — bars scaled to each model's pass count / total.
func chartPassRate(reports []Report) string {
	var b strings.Builder
	b.WriteString("\npass-rate (higher is better)\n")
	for _, r := range reports {
		pct := 0
		if r.Total() > 0 {
			pct = r.Passed() * 100 / r.Total()
		}
		fmt.Fprintf(&b, "  %-12s %s %d/%d (%d%%)\n",
			r.Model, bar(float64(r.Passed()), float64(r.Total())), r.Passed(), r.Total(), pct)
	}
	return b.String()
}

// chartTokens: input + output, scaled to the busiest model (lower is better).
func chartTokens(reports []Report) string {
	var maxIn, maxOut float64
	for _, r := range reports {
		in, _, out, _, _, _ := r.Totals()
		maxIn = math.Max(maxIn, float64(in))
		maxOut = math.Max(maxOut, float64(out))
	}
	if maxIn == 0 && maxOut == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\ninput tokens (lower is better)\n")
	for _, r := range reports {
		in, _, _, _, _, _ := r.Totals()
		fmt.Fprintf(&b, "  %-12s %s %s\n", r.Model, bar(float64(in), maxIn), humanInt(in))
	}
	b.WriteString("output tokens (lower is better)\n")
	for _, r := range reports {
		_, _, out, _, _, _ := r.Totals()
		fmt.Fprintf(&b, "  %-12s %s %s\n", r.Model, bar(float64(out), maxOut), humanInt(out))
	}
	return b.String()
}

// chartAvgSteps: mean steps/task, scaled to the busiest model (lower is better).
func chartAvgSteps(reports []Report) string {
	var max float64
	for _, r := range reports {
		max = math.Max(max, r.AvgSteps())
	}
	if max == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\navg steps/task (lower is better)\n")
	for _, r := range reports {
		fmt.Fprintf(&b, "  %-12s %s %.1f\n", r.Model, bar(r.AvgSteps(), max), r.AvgSteps())
	}
	return b.String()
}

// matrix is the per-task verdict grid, one column per model.
func matrix(reports []Report) string {
	idx := make([]map[string]TaskResult, len(reports))
	for i, r := range reports {
		m := make(map[string]TaskResult, len(r.Tasks))
		for _, tr := range r.Tasks {
			m[tr.Name] = tr
		}
		idx[i] = m
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nper-task verdict\n  %-26s", "task")
	for _, r := range reports {
		fmt.Fprintf(&b, " %-8s", r.Model)
	}
	b.WriteByte('\n')
	for _, tr := range reports[0].Tasks {
		fmt.Fprintf(&b, "  %-26s", truncate(tr.Name, 26))
		for i := range reports {
			mark := "—"
			if t, ok := idx[i][tr.Name]; ok {
				mark = verdict(t, t.ok())
			}
			fmt.Fprintf(&b, " %-8s", mark)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// aggregateTable is the one-row-per-model rollup of every headline metric.
func aggregateTable(reports []Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\naggregate\n  %-12s %-7s %8s %8s %8s %9s %7s\n",
		"model", "pass", "in", "out", "cache%", "avg-steps", "time")
	for _, r := range reports {
		in, _, out, _, _, dur := r.Totals()
		fmt.Fprintf(&b, "  %-12s %-7s %8s %8s %7d%% %9.1f %7s\n",
			r.Model, fmt.Sprintf("%d/%d", r.Passed(), r.Total()),
			humanInt(in), humanInt(out), r.CachePct(), r.AvgSteps(), humanDur(dur))
	}
	return b.String()
}

// winner picks the better model: most tasks solved, tie-broken by fewer input
// tokens then fewer total steps. Returns a one-line, human-readable rationale.
func winner(reports []Report) string {
	type score struct {
		r     Report
		pass  int
		in    int
		steps int
	}
	scores := make([]score, 0, len(reports))
	for _, r := range reports {
		in, _, _, steps, _, _ := r.Totals()
		scores = append(scores, score{r: r, pass: r.Passed(), in: in, steps: steps})
	}
	sort.SliceStable(scores, func(i, j int) bool {
		if scores[i].pass != scores[j].pass {
			return scores[i].pass > scores[j].pass // more passes wins
		}
		if scores[i].in != scores[j].in {
			return scores[i].in < scores[j].in // fewer input tokens wins
		}
		return scores[i].steps < scores[j].steps // fewer steps wins
	})

	best, runner := scores[0], scores[1]
	if best.pass == runner.pass && best.in == runner.in && best.steps == runner.steps {
		return fmt.Sprintf("tie — %s and %s are even on passes, tokens, and steps", best.r.Model, runner.r.Model)
	}

	switch {
	case best.pass > runner.pass:
		return fmt.Sprintf("%s wins — solved %d/%d vs %s's %d/%d (+%d tasks)",
			best.r.Model, best.pass, best.r.Total(), runner.r.Model, runner.pass, runner.r.Total(),
			best.pass-runner.pass)
	default: // same pass count, decided on efficiency
		return fmt.Sprintf("%s wins on efficiency — same %d/%d, but %s input tokens%s",
			best.r.Model, best.pass, best.r.Total(), ratio(runner.in, best.in),
			stepsNote(runner.steps, best.steps))
	}
}

// ratio renders "2.0× fewer" / "1.3× fewer" for a>b, else "more".
func ratio(a, b int) string {
	if b <= 0 || a <= b {
		return "more"
	}
	return fmt.Sprintf("%.1f× fewer", float64(a)/float64(b))
}

func stepsNote(runnerSteps, bestSteps int) string {
	if bestSteps > 0 && runnerSteps > bestSteps {
		return fmt.Sprintf(" and %.1f× fewer steps", float64(runnerSteps)/float64(bestSteps))
	}
	return ""
}

// Render is the full report written to BORG_EVAL_REPORT: the cross-model
// comparison (when ≥1 model) followed by each model's per-task detail.
func Render(reports []Report) string {
	var b strings.Builder
	b.WriteString(CompareReports(reports))
	for _, r := range reports {
		b.WriteString("\n")
		b.WriteString(r.Detail())
	}
	return b.String()
}

// truncate clips s to n runes, ending in "…" when cut, so table columns align.
func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n-1]) + "…"
}
