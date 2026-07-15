package eval

// Regression tracking: a committed per-model scorecard the live eval is checked
// against, so "did we regress?" has a concrete, deterministic answer instead of
// eyeballing a fresh .txt each time. The reference is a JSON file in the repo
// (testdata/baseline.json); a task only counts as a regression once it's IN the
// baseline (a green run, locked in intentionally) and later goes red.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// TaskBaseline is the committed reference for one task: whether it passed and, when
// known, its efficiency (steps + input tokens) so we can flag a still-passing task
// that got materially slower/cheaper between runs — not just pass/fail.
//
// It unmarshals from EITHER the legacy bare bool (pass/fail only, efficiency
// unknown) OR the richer object, so old baselines keep loading until re-saved.
type TaskBaseline struct {
	Passed   bool `json:"passed"`
	Steps    int  `json:"steps,omitempty"`
	InTokens int  `json:"in_tokens,omitempty"`
}

// UnmarshalJSON accepts the legacy `true`/`false` form as well as the object form.
func (t *TaskBaseline) UnmarshalJSON(b []byte) error {
	var pass bool
	if err := json.Unmarshal(b, &pass); err == nil {
		*t = TaskBaseline{Passed: pass}
		return nil
	}
	type raw TaskBaseline // avoid recursing into this method
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	*t = TaskBaseline(r)
	return nil
}

// efficiencyMargin is how much worse a still-passing task's steps or input tokens
// must get before it's flagged a regression (and how much better to be an
// improvement). Generous on purpose: the model is stochastic, so a single run is
// noisy — only a sizable, unambiguous move should trip it. Confirm with
// BORG_EVAL_REPEAT before reading too much into one run.
const efficiencyMargin = 0.30

// Baseline maps model → task name → its committed reference.
type Baseline map[string]map[string]TaskBaseline

// LoadBaseline reads a baseline JSON file. A missing file is not an error — it's
// the valid "no baseline yet" state, returning an empty baseline.
func LoadBaseline(path string) (Baseline, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Baseline{}, nil
	}
	if err != nil {
		return nil, err
	}
	var b Baseline
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	return b, nil
}

// Scoreboard distills reports into a baseline (model → task → reference), recording
// each passing task's steps + input tokens so future runs can detect efficiency
// drift, not just pass/fail. (A failing task records no efficiency numbers — there's
// nothing good to anchor to.)
func Scoreboard(reports []Report) Baseline {
	b := Baseline{}
	for _, r := range reports {
		m := make(map[string]TaskBaseline, len(r.Tasks))
		for _, tr := range r.Tasks {
			tb := TaskBaseline{Passed: tr.ok()}
			if tb.Passed {
				tb.Steps, tb.InTokens = tr.Steps, tr.InTokens
			}
			m[tr.Name] = tb
		}
		b[r.Model] = m
	}
	return b
}

// SaveBaseline writes reports' scoreboard as stable, pretty JSON — call it to lock
// in a new reference after a verified-good run.
func SaveBaseline(path string, reports []Report) error {
	raw, err := json.MarshalIndent(Scoreboard(reports), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// RegressionsVs compares this report against the baseline for its model:
// regressed = was passing in the baseline, now fails; improved = was failing or
// absent, now passes. A model absent from the baseline yields no regressions
// (nothing to regress from). Both lists are sorted for stable output.
func (r Report) RegressionsVs(base Baseline) (regressed, improved []string) {
	prior := base[r.Model]
	for _, tr := range r.Tasks {
		was, known := prior[tr.Name]
		switch {
		case known && was.Passed && !tr.ok():
			regressed = append(regressed, tr.Name)
		case (!known || !was.Passed) && tr.ok():
			improved = append(improved, tr.Name)
		}
	}
	sort.Strings(regressed)
	sort.Strings(improved)
	return regressed, improved
}

// EfficiencyVs compares still-passing tasks against their baseline efficiency:
// slower = now uses >efficiencyMargin more steps OR input tokens; faster = >margin
// fewer on both axes without getting worse on either. Only tasks that pass in BOTH
// the baseline and this run, and have a recorded baseline (Steps>0), are considered
// — so it never conflates a correctness change with an efficiency one.
func (r Report) EfficiencyVs(base Baseline) (slower, faster []string) {
	prior := base[r.Model]
	worse := func(now, ref int) bool { return ref > 0 && float64(now) > float64(ref)*(1+efficiencyMargin) }
	better := func(now, ref int) bool { return ref > 0 && float64(now) < float64(ref)*(1-efficiencyMargin) }
	for _, tr := range r.Tasks {
		ref, known := prior[tr.Name]
		if !known || !ref.Passed || !tr.ok() || ref.Steps == 0 {
			continue
		}
		switch {
		case worse(tr.Steps, ref.Steps) || worse(tr.InTokens, ref.InTokens):
			slower = append(slower, fmt.Sprintf("%s (%d→%d steps, %s→%s in)",
				tr.Name, ref.Steps, tr.Steps, humanInt(ref.InTokens), humanInt(tr.InTokens)))
		case better(tr.Steps, ref.Steps) && !worse(tr.InTokens, ref.InTokens),
			better(tr.InTokens, ref.InTokens) && !worse(tr.Steps, ref.Steps):
			faster = append(faster, fmt.Sprintf("%s (%d→%d steps, %s→%s in)",
				tr.Name, ref.Steps, tr.Steps, humanInt(ref.InTokens), humanInt(tr.InTokens)))
		}
	}
	sort.Strings(slower)
	sort.Strings(faster)
	return slower, faster
}

// RegressionSummary renders the cross-model regression check — a prominent ✓/⚠
// block so a glance answers "did anything that used to pass break?".
func RegressionSummary(reports []Report, base Baseline) string {
	var b strings.Builder
	b.WriteString("regression check (vs committed baseline)\n")
	anyReg := false
	for _, r := range reports {
		if _, ok := base[r.Model]; !ok {
			fmt.Fprintf(&b, "  %-12s no baseline yet (run with BORG_EVAL_SAVE_BASELINE=1 to set one)\n", r.Model)
			continue
		}
		reg, imp := r.RegressionsVs(base)
		if len(reg) > 0 {
			anyReg = true
			fmt.Fprintf(&b, "  %-12s ⚠ REGRESSED: %s\n", r.Model, strings.Join(reg, ", "))
		} else {
			fmt.Fprintf(&b, "  %-12s ✓ no regressions\n", r.Model)
		}
		if len(imp) > 0 {
			fmt.Fprintf(&b, "  %-12s ↑ newly passing: %s\n", "", strings.Join(imp, ", "))
		}
		// Efficiency drift among still-passing tasks: catches "still 23/23 but burning
		// more" / "same pass-rate, now cheaper" — invisible to a pass/fail check.
		slower, faster := r.EfficiencyVs(base)
		if len(slower) > 0 {
			anyReg = true
			fmt.Fprintf(&b, "  %-12s ⚠ slower/costlier (>%.0f%%): %s\n", "", efficiencyMargin*100, strings.Join(slower, "; "))
		}
		if len(faster) > 0 {
			fmt.Fprintf(&b, "  %-12s ↓ leaner: %s\n", "", strings.Join(faster, "; "))
		}
	}
	if anyReg {
		b.WriteString("  ⚠ a previously-passing task regressed (failed or got materially slower/costlier) — investigate before updating the baseline\n")
	}
	return b.String()
}
