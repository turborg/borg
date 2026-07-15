package eval_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/turborg/borg/internal/eval"
	"github.com/turborg/borg/internal/llm"
)

// write_file / edit_file tool calls aimed at a file in the workspace.
func writeCall(id, ws, name, body string) llm.Message {
	args, _ := json.Marshal(map[string]string{"path": filepath.Join(ws, name), "content": body})
	return callTool(id, "write_file", string(args))
}

func editCall(id, ws, name, oldS, newS string) llm.Message {
	args, _ := json.Marshal(map[string]string{
		"path": filepath.Join(ws, name), "old_string": oldS, "new_string": newS,
	})
	return callTool(id, "edit_file", string(args))
}

// bashCall is a bash tool call — used by the git task's solver, which fixes the
// fixture by running a command rather than editing a file.
func bashCall(id, command string) llm.Message {
	args, _ := json.Marshal(map[string]string{"command": command})
	return callTool(id, "bash", string(args))
}

// corpusSolvers maps each corpus task to the tool calls that fix it, so the
// deterministic suite can prove every task is solvable. A mix of write_file
// (full rewrite / new file) and edit_file (targeted in-place change) exercises
// both mutating tools end-to-end.
var corpusSolvers = map[string]func(ws string) []llm.Message{
	"fix-compile-error": func(ws string) []llm.Message {
		return []llm.Message{writeCall("1", ws, "mathx.go",
			"package mathx\n\nfunc Add(a, b int) int { return a + b }\n")}
	},
	"fix-failing-test": func(ws string) []llm.Message {
		return []llm.Message{writeCall("1", ws, "strutil.go",
			"package strutil\n\nfunc Reverse(s string) string {\n"+
				"\tr := []rune(s)\n"+
				"\tfor i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {\n"+
				"\t\tr[i], r[j] = r[j], r[i]\n\t}\n\treturn string(r)\n}\n")}
	},
	"fix-off-by-one": func(ws string) []llm.Message {
		return []llm.Message{editCall("1", ws, "sumx.go", "i < n", "i <= n")}
	},
	"implement-missing-function": func(ws string) []llm.Message {
		return []llm.Message{writeCall("1", ws, "greet.go",
			"package greet\n\nfunc Greet(name string) string { return \"Hello, \" + name + \"!\" }\n")}
	},
	"fix-across-files": func(ws string) []llm.Message {
		return []llm.Message{editCall("1", ws, "ops.go", "return x", "return 2 * x")}
	},
	"grep-and-fix": func(ws string) []llm.Message {
		return []llm.Message{editCall("1", ws, "phase.go", `"PENDING"`, `"DONE"`)}
	},
	"fix-two-bugs-two-files": func(ws string) []llm.Message {
		return []llm.Message{
			editCall("1", ws, "validate.go", "return s == \"\"", "return s != \"\""),
			writeCall("2", ws, "normalize.go",
				"package form\n\nfunc trimDot(s string) string {\n"+
					"\tif len(s) > 0 && s[len(s)-1] == '.' {\n"+
					"\t\treturn s[:len(s)-1]\n\t}\n\treturn s\n}\n"),
		}
	},
	"refactor-signature": func(ws string) []llm.Message {
		return []llm.Message{
			editCall("1", ws, "scale.go",
				"func scale(x int) int { return x * 2 }", "func scale(x, f int) int { return x * f }"),
			editCall("2", ws, "apply.go",
				"func Apply(x int) int { return scale(x) }", "func Apply(x, f int) int { return scale(x, f) }"),
		}
	},
	"fix-clamp-logic": func(ws string) []llm.Message {
		return []llm.Message{writeCall("1", ws, "clamp.go",
			"package mathx\n\nfunc Clamp(x, lo, hi int) int {\n"+
				"\tif x < lo {\n\t\treturn lo\n\t}\n"+
				"\tif x > hi {\n\t\treturn hi\n\t}\n\treturn x\n}\n")}
	},
	"merge-feature-branch": func(ws string) []llm.Message {
		return []llm.Message{bashCall("1", "git -C "+ws+" merge feature/greeting --no-edit")}
	},
	"learn-project-context": func(ws string) []llm.Message {
		return []llm.Message{writeCall("1", ws, "BORG.md",
			"# BORG.md\n\nGoal: a static marketing website to sell corn for an agro company.\n")}
	},
	"learn-stays-lean": func(ws string) []llm.Message {
		return []llm.Message{writeCall("1", ws, "BORG.md",
			"# BORG.md\n\nLedger: a CLI double-entry bookkeeping tool.\n")}
	},
	"implement-interface": func(ws string) []llm.Message {
		return []llm.Message{writeCall("1", ws, "area.go",
			"package geom\n\nfunc (r Rect) Area() float64 { return r.W * r.H }\n")}
	},
	"fix-nil-map-panic": func(ws string) []llm.Message {
		return []llm.Message{writeCall("1", ws, "count.go",
			"package tally\n\nfunc Tally(items []string) map[string]int {\n"+
				"\tm := map[string]int{}\n"+
				"\tfor _, it := range items {\n\t\tm[it]++\n\t}\n\treturn m\n}\n")}
	},
	"fix-json-tag": func(ws string) []llm.Message {
		return []llm.Message{editCall("1", ws, "user.go", "json:\"-\"", "json:\"email\"")}
	},
	"fix-bounds-panic": func(ws string) []llm.Message {
		return []llm.Message{editCall("1", ws, "last.go", "xs[len(xs)]", "xs[len(xs)-1]")}
	},
	"trace-call-chain": func(ws string) []llm.Message {
		return []llm.Message{editCall("1", ws, "norm.go", "strings.ToUpper", "strings.ToLower")}
	},
	"red-herring-fix": func(ws string) []llm.Message {
		return []llm.Message{editCall("1", ws, "discount.go", "price+price*pct/100", "price-price*pct/100")}
	},
	"implement-from-context": func(ws string) []llm.Message {
		return []llm.Message{editCall("1", ws, "store.go",
			"func (s *Store) GetOr(k string, def int) int { return 0 }",
			"func (s *Store) GetOr(k string, def int) int {\n"+
				"\tif v, ok := s.Get(k); ok {\n\t\treturn v\n\t}\n\treturn def\n}")}
	},
	"search-find-value": func(ws string) []llm.Message {
		return []llm.Message{writeCall("1", ws, "found.txt", "falcon-9")}
	},
	"locate-fix-constant": func(ws string) []llm.Message {
		return []llm.Message{editCall("1", ws, "limits.go", "dailyLimit = 50", "dailyLimit = 500")}
	},
	"fix-merge-sorted": func(ws string) []llm.Message {
		return []llm.Message{writeCall("1", ws, "merge.go",
			"package mergex\n\nfunc Merge(a, b []int) []int {\n"+
				"\tout := []int{}\n\ti, j := 0, 0\n"+
				"\tfor i < len(a) && j < len(b) {\n"+
				"\t\tif a[i] <= b[j] {\n\t\t\tout = append(out, a[i])\n\t\t\ti++\n\t\t} else {\n\t\t\tout = append(out, b[j])\n\t\t\tj++\n\t\t}\n\t}\n"+
				"\tout = append(out, a[i:]...)\n\tout = append(out, b[j:]...)\n\treturn out\n}\n")}
	},
	"fix-rotate-slice": func(ws string) []llm.Message {
		return []llm.Message{writeCall("1", ws, "rotate.go",
			"package rotatex\n\nfunc Rotate(xs []int, k int) []int {\n"+
				"\tif len(xs) == 0 {\n\t\treturn xs\n\t}\n"+
				"\tk = k % len(xs)\n"+
				"\treturn append(append([]int{}, xs[k:]...), xs[:k]...)\n}\n")}
	},
	"keeps-gofmt-clean-implement": func(ws string) []llm.Message {
		return []llm.Message{editCall("1", ws, "num.go", "return 0", "return 2 * x")}
	},
	"keeps-gofmt-clean-edit": func(ws string) []llm.Message {
		return []llm.Message{editCall("1", ws, "greeting.go", `"Hello, " + name`, `"Hi, " + name + "!"`)}
	},
}

// Every corpus task must be both discriminating (its oracle fails on the
// untouched broken fixture) and solvable (passes once the known fix is applied)
// — proven deterministically through the real loop, no tokens.
func TestCorpusIsDiscriminatingAndSolvable(t *testing.T) {
	for _, tk := range eval.Corpus() {
		t.Run(tk.Name, func(t *testing.T) {
			wsBroken := t.TempDir()
			res, err := eval.RunTask(context.Background(), tk, wsBroken,
				&fakeModel{steps: []llm.Message{say("I changed nothing.")}}, &recUI{})
			require.NoError(t, err)
			require.False(t, res.Pass, "oracle should fail on the untouched broken fixture")

			build, ok := corpusSolvers[tk.Name]
			require.Truef(t, ok, "no wired solver for task %q", tk.Name)
			wsFixed := t.TempDir()
			steps := append(build(wsFixed), say("fixed"))
			res, err = eval.RunTask(context.Background(), tk, wsFixed, &fakeModel{steps: steps}, &recUI{})
			require.NoError(t, err)
			require.Truef(t, res.Pass, "task %q should pass once fixed: %s", tk.Name, res.Reason)
		})
	}
}

// The no-op corpus is the INVERSE of the fix corpus: each fixture already builds and
// passes, so a model that correctly recognizes "nothing to do" (here: a do-nothing
// model) passes its oracle UNTOUCHED. This proves the fixtures are genuine no-ops;
// the live value — converging FAST instead of exploring to the cap — is measured by
// TestEvalNoOpConvergence. (They deliberately fail the discriminating invariant, so
// they live outside Corpus().)
func TestNoOpCorpusAlreadyPasses(t *testing.T) {
	for _, tk := range eval.NoOpCorpus() {
		t.Run(tk.Name, func(t *testing.T) {
			ws := t.TempDir()
			res, err := eval.RunTask(context.Background(), tk, ws,
				&fakeModel{steps: []llm.Message{say("Already builds and the tests pass — nothing to change.")}}, &recUI{})
			require.NoError(t, err)
			require.Truef(t, res.Pass, "a no-op task must pass untouched: %s", res.Reason)
		})
	}
}
