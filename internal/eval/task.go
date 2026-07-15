package eval

// Tier 3: task evals scored by an objective oracle. A Task pairs a natural-
// language prompt with a fixture to materialize and an Oracle that inspects the
// resulting workspace and returns a verdict. Scoring is by *verifiable reward*
// (did the file/build/test end up correct), never by asking a model to grade
// itself — objective oracles are what make an eval trustworthy and let pass-rate
// be tracked over time.
//
// RunTask is model-agnostic: inject a Player (cassette → deterministic, runs in
// CI, no tokens) or the live metered client (nightly, behind a budget). The same
// task + oracle scores both.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/turborg/borg/internal/agent"
	"github.com/turborg/borg/internal/config"
)

// oracleTimeout bounds a command oracle so a hung build/test can't wedge the run.
const oracleTimeout = 90 * time.Second

// Result is a task's verdict: did the agent achieve the objective, and if not,
// why (so a failing eval is debuggable without a rerun).
type Result struct {
	Pass   bool
	Reason string
}

// Oracle inspects a finished workspace and scores it. Examples: the edited file
// matches, `go build` succeeds, the previously-failing test now passes.
type Oracle func(workspace string) Result

// Task is one eval: prompt the agent, then score the workspace it leaves behind.
type Task struct {
	Name   string
	Prompt string
	Setup  func(workspace string) error // materialize the fixture (optional)
	Oracle Oracle                       // score the result (nil ⇒ "ran without error" passes)
	// StepBudget, if > 0, fails the task when it takes MORE than this many steps —
	// even if the oracle passed. A latency/wander guard: e.g. a /learn that over-
	// explores instead of writing promptly produces a correct file but burns steps,
	// which a pass/fail oracle can't see. Steps ≈ wall-clock, so this catches a
	// "doubled in time" regression in the nightly eval that the corpus otherwise
	// wouldn't. Enforced in RunSuite (where per-task steps are measured).
	StepBudget int
}

// RunOption configures the agent built for a task run. These are how the eval
// harness pins behavior (effort, step cap) WITHOUT touching borg's shipped
// defaults — they apply only to the throwaway agent RunTask builds.
type RunOption func(*agent.Agent)

// RunWithEffort pins the agent's reasoning_effort for the run (e.g. "none" to keep
// the eval cheap and disable auto-escalation). Empty string is a no-op (keeps
// the agent's default auto behavior).
func RunWithEffort(effort string) RunOption {
	return func(a *agent.Agent) {
		if effort != "" {
			a.SetEffort(effort)
		}
	}
}

// RunWithMaxSteps caps the agent's per-task tool-call steps for the run, bounding
// the worst-case token spend of a task that wanders. n <= 0 is a no-op (keeps
// the shipped default of 30).
func RunWithMaxSteps(n int) RunOption {
	return func(a *agent.Agent) {
		if n > 0 {
			a.SetMaxSteps(n)
		}
	}
}

// FilterTasks returns the tasks whose Name is in keep (order follows tasks, not
// keep). An empty keep returns tasks unchanged — so "no filter" means "all".
func FilterTasks(tasks []Task, keep []string) []Task {
	if len(keep) == 0 {
		return tasks
	}
	want := make(map[string]bool, len(keep))
	for _, n := range keep {
		want[n] = true
	}
	out := make([]Task, 0, len(tasks))
	for _, tk := range tasks {
		if want[tk.Name] {
			out = append(out, tk)
		}
	}
	return out
}

// RunTask sets up the fixture, runs the real agent loop on the prompt with the
// injected model, then scores the workspace with the task's oracle. workspace is
// supplied by the caller so the fixture and a replay cassette stay co-bound.
func RunTask(ctx context.Context, tk Task, workspace string, model agent.LLM, ui agent.UI, opts ...RunOption) (Result, error) {
	if tk.Setup != nil {
		if err := tk.Setup(workspace); err != nil {
			return Result{}, fmt.Errorf("setup %q: %w", tk.Name, err)
		}
	}
	a := agent.NewWithLLM(&config.Config{Model: "floko"}, model)
	a.SetUI(ui)
	for _, opt := range opts {
		opt(a)
	}
	// Tell the agent where to work — the fixture lives in a temp workspace, so a
	// live model needs the absolute path to operate on the right files.
	prompt := fmt.Sprintf("Your working directory is %s. Read and modify files there using absolute paths.\n\n%s",
		workspace, tk.Prompt)
	if err := a.Ask(ctx, prompt); err != nil {
		return Result{}, fmt.Errorf("run %q: %w", tk.Name, err)
	}
	if tk.Oracle == nil {
		return Result{Pass: true}, nil
	}
	return tk.Oracle(workspace), nil
}

// FileEquals is an oracle: the file at workspace/rel must contain exactly want.
func FileEquals(rel, want string) Oracle {
	return func(ws string) Result {
		got, err := os.ReadFile(filepath.Join(ws, rel))
		if err != nil {
			return Result{Reason: fmt.Sprintf("read %s: %v", rel, err)}
		}
		if string(got) != want {
			return Result{Reason: fmt.Sprintf("%s = %q, want %q", rel, string(got), want)}
		}
		return Result{Pass: true}
	}
}

// FileContains is an oracle: the file at workspace/rel must contain every
// substring (case-insensitively). Useful for prose outputs like BORG.md where an
// exact match is wrong but key facts must be present.
func FileContains(rel string, subs ...string) Oracle {
	return func(ws string) Result {
		b, err := os.ReadFile(filepath.Join(ws, rel))
		if err != nil {
			return Result{Reason: fmt.Sprintf("read %s: %v", rel, err)}
		}
		hay := strings.ToLower(string(b))
		for _, s := range subs {
			if !strings.Contains(hay, strings.ToLower(s)) {
				return Result{Reason: fmt.Sprintf("%s missing %q", rel, s)}
			}
		}
		return Result{Pass: true}
	}
}

// Command is an oracle that runs name+args in the workspace and passes on a zero
// exit. The combined output is the failure reason, so a red eval is debuggable
// without a rerun. This is the verifiable-reward backbone: builds and tests.
func Command(name string, args ...string) Oracle {
	return func(ws string) Result {
		ctx, cancel := context.WithTimeout(context.Background(), oracleTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = ws
		out, err := cmd.CombinedOutput()
		if err != nil {
			return Result{Reason: fmt.Sprintf("%s %s: %v\n%s",
				name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))}
		}
		return Result{Pass: true}
	}
}

// Builds passes when `go build ./...` succeeds in the workspace.
func Builds() Oracle { return Command("go", "build", "./...") }

// GoTestPasses passes when `go test ./...` succeeds in the workspace.
func GoTestPasses() Oracle { return Command("go", "test", "./...") }

// GofmtClean passes when every Go file in the workspace is gofmt-formatted —
// i.e. `gofmt -l .` lists nothing. It's the end-to-end check that borg's
// auto-format-after-edit actually keeps edits clean: if that harness feature
// regressed, a task whose model left sloppy whitespace would now fail here. If
// gofmt isn't installed it passes (can't check → don't false-fail).
func GofmtClean() Oracle {
	return func(ws string) Result {
		if _, err := exec.LookPath("gofmt"); err != nil {
			return Result{Pass: true}
		}
		ctx, cancel := context.WithTimeout(context.Background(), oracleTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "gofmt", "-l", ".")
		cmd.Dir = ws
		out, err := cmd.CombinedOutput()
		if err != nil {
			return Result{Reason: fmt.Sprintf("gofmt -l: %v\n%s", err, strings.TrimSpace(string(out)))}
		}
		if dirty := strings.TrimSpace(string(out)); dirty != "" {
			return Result{Reason: "files are not gofmt-clean (auto-format regressed?):\n" + dirty}
		}
		return Result{Pass: true}
	}
}

// All combines oracles: the result passes only if every oracle passes, returning
// the first failure (so a multi-criteria task — e.g. compiles AND gofmt-clean — is
// scored as one).
func All(oracles ...Oracle) Oracle {
	return func(ws string) Result {
		for _, o := range oracles {
			if r := o(ws); !r.Pass {
				return r
			}
		}
		return Result{Pass: true}
	}
}
