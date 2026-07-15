package eval

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/turborg/borg/internal/agent"
)

// Corpus is borg's seed set of coding tasks scored by objective oracles. Each
// task materializes a small, dependency-free Go module in a broken state and
// asks the agent to fix it; the oracle (a real `go build` / `go test`) is the
// verifiable reward. The corpus is deliberately tiny and stdlib-only so it runs
// offline and fast — grow it as borg's capability bar rises.
func Corpus() []Task {
	return []Task{
		fixCompileError(),
		fixFailingTest(),
		fixOffByOne(),
		implementMissingFunc(),
		fixAcrossFiles(),
		grepAndFix(),
		fixTwoBugsTwoFiles(),
		refactorSignature(),
		fixClampLogic(),
		mergeBranch(),
		learnProject(),
		learnStaysLean(),
		implementInterface(),
		fixNilMapPanic(),
		fixJSONTag(),
		fixBoundsPanic(),
		traceCallChain(),
		redHerringFix(),
		implementFromContext(),
		searchFindValue(),
		locateFixConstant(),
		fixMergeSorted(),
		fixRotateSlice(),
		keepsGofmtCleanOnImplement(),
		keepsGofmtCleanOnEdit(),
	}
}

// NoOpCorpus is the INVERSE of Corpus(): each fixture ALREADY builds and its tests
// already pass, so the right move is to recognize "nothing to do" and finish FAST —
// the exact situation that thrashed the 1.35M-token /upgrade session (45 steps on a
// task whose work was already done). These tasks are NOT in Corpus() because they
// violate the discriminating invariant (their oracle passes untouched); they're run
// on their own by the live convergence test. The pass/fail signal is the tight
// StepBudget (RunSuite fails a task that runs past it) PLUS an oracle that the code
// is STILL correct afterward — so "explored to the cap" and "broke working code"
// both fail. Keep them language-neutral.
func NoOpCorpus() []Task {
	return []Task{noOpTestsAlreadyPass(), noOpAlreadyImplemented()}
}

// noOpTestsAlreadyPass: the prompt implies a test might be failing, but it already
// passes. borg must confirm green and finish, not edit-churn or explore to the cap.
func noOpTestsAlreadyPass() Task {
	return Task{
		Name: "noop-tests-already-pass",
		Prompt: "A test in this directory might be failing. Make sure `go test ./...` passes. " +
			"If it already passes, don't make unnecessary edits — just confirm and finish.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"strutil.go": "package strutil\n\n" +
				"// Reverse returns s with its runes in reverse order.\n" +
				"func Reverse(s string) string {\n" +
				"\tr := []rune(s)\n" +
				"\tfor i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {\n" +
				"\t\tr[i], r[j] = r[j], r[i]\n\t}\n" +
				"\treturn string(r)\n}\n",
			"strutil_test.go": "package strutil\n\nimport \"testing\"\n\n" +
				"func TestReverse(t *testing.T) {\n" +
				"\tif got := Reverse(\"abc\"); got != \"cba\" {\n" +
				"\t\tt.Fatalf(\"Reverse(abc) = %q\", got)\n\t}\n}\n",
		}),
		Oracle:     All(Builds(), GoTestPasses()), // must STILL be green (borg didn't break it)
		StepBudget: 8,                             // a converging run confirms in ~2-4 steps; 45 is the failure
	}
}

// noOpAlreadyImplemented: the prompt asks to implement a function that's ALREADY
// implemented correctly (the /usage-feature shape). borg must recognize it's done.
func noOpAlreadyImplemented() Task {
	return Task{
		Name: "noop-already-implemented",
		Prompt: "Implement Add in mathx.go so it returns the sum of its two int arguments, " +
			"and make sure the package's tests pass.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"mathx.go": "package mathx\n\n" +
				"// Add returns the sum of its two int arguments.\n" +
				"func Add(a, b int) int { return a + b }\n",
			"mathx_test.go": "package mathx\n\nimport \"testing\"\n\n" +
				"func TestAdd(t *testing.T) {\n" +
				"\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"Add(2,3) != 5\")\n\t}\n}\n",
		}),
		Oracle:     All(Builds(), GoTestPasses()),
		StepBudget: 8,
	}
}

// fixMergeSorted (HARD/EDGE-CASES): a merge of two sorted slices that compiles and
// works for some inputs but DROPS the tail of whichever side outlasts the other —
// a classic subtle bug a small model often "fixes" incompletely. The test covers
// uneven lengths and empties, so a half-fix still fails.
func fixMergeSorted() Task {
	return Task{
		Name: "fix-merge-sorted",
		Prompt: "Merge in merge.go should merge two sorted int slices into one sorted " +
			"slice, but it drops elements. Fix it so the tests pass — they cover uneven " +
			"lengths and empty inputs. Do not edit the test.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"merge.go": "package mergex\n\n" +
				"// Merge should return a and b (both sorted) merged into one sorted slice,\n" +
				"// but it stops once either side is exhausted and drops the remaining tail.\n" +
				"func Merge(a, b []int) []int {\n" +
				"\tout := []int{}\n\ti, j := 0, 0\n" +
				"\tfor i < len(a) && j < len(b) {\n" +
				"\t\tif a[i] <= b[j] {\n\t\t\tout = append(out, a[i])\n\t\t\ti++\n\t\t} else {\n\t\t\tout = append(out, b[j])\n\t\t\tj++\n\t\t}\n\t}\n" +
				"\treturn out\n}\n",
			"merge_test.go": "package mergex\n\nimport (\n\t\"reflect\"\n\t\"testing\"\n)\n\n" +
				"func TestMerge(t *testing.T) {\n" +
				"\tcases := []struct {\n\t\ta, b, want []int\n\t}{\n" +
				"\t\t{[]int{1, 3, 5}, []int{2, 4, 6}, []int{1, 2, 3, 4, 5, 6}},\n" +
				"\t\t{[]int{1, 2}, []int{3, 4, 5}, []int{1, 2, 3, 4, 5}},\n" +
				"\t\t{[]int{}, []int{1}, []int{1}},\n" +
				"\t\t{[]int{7}, []int{}, []int{7}},\n\t}\n" +
				"\tfor _, c := range cases {\n" +
				"\t\tif got := Merge(c.a, c.b); !reflect.DeepEqual(got, c.want) {\n" +
				"\t\t\tt.Fatalf(\"Merge(%v,%v) = %v, want %v\", c.a, c.b, got, c.want)\n\t\t}\n\t}\n}\n",
		}),
		Oracle: GoTestPasses(),
	}
}

// fixRotateSlice (HARD/EDGE-CASES): a left-rotate that slices out of range when k
// is ≥ the length (the modulo reduction is missing). Works for small k, panics for
// large k — the fix needs reasoning about the k=0 / k=len / k>len boundaries.
func fixRotateSlice() Task {
	return Task{
		Name: "fix-rotate-slice",
		Prompt: "Rotate in rotate.go should return xs rotated LEFT by k (so " +
			"Rotate([1,2,3],1) = [2,3,1]) and must handle k=0, k equal to the length, and " +
			"k larger than the length. It currently panics for some k. Fix it so the tests " +
			"pass. Do not edit the test.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"rotate.go": "package rotatex\n\n" +
				"// Rotate returns xs rotated left by k. Bug: it never reduces k modulo the\n" +
				"// length, so k >= len(xs) slices out of range.\n" +
				"func Rotate(xs []int, k int) []int {\n" +
				"\tif len(xs) == 0 {\n\t\treturn xs\n\t}\n" +
				"\treturn append(append([]int{}, xs[k:]...), xs[:k]...)\n}\n",
			"rotate_test.go": "package rotatex\n\nimport (\n\t\"reflect\"\n\t\"testing\"\n)\n\n" +
				"func TestRotate(t *testing.T) {\n" +
				"\tcases := []struct {\n\t\txs   []int\n\t\tk    int\n\t\twant []int\n\t}{\n" +
				"\t\t{[]int{1, 2, 3}, 1, []int{2, 3, 1}},\n" +
				"\t\t{[]int{1, 2, 3}, 0, []int{1, 2, 3}},\n" +
				"\t\t{[]int{1, 2, 3}, 4, []int{2, 3, 1}},\n" +
				"\t\t{[]int{1, 2, 3}, 3, []int{1, 2, 3}},\n\t}\n" +
				"\tfor _, c := range cases {\n" +
				"\t\tif got := Rotate(c.xs, c.k); !reflect.DeepEqual(got, c.want) {\n" +
				"\t\t\tt.Fatalf(\"Rotate(%v,%d) = %v, want %v\", c.xs, c.k, got, c.want)\n\t\t}\n\t}\n}\n",
		}),
		Oracle: GoTestPasses(),
	}
}

// searchFindValue (DISCOVERY/SEARCH): the answer is one value buried in one file
// among a tree of decoys — the agent must SEARCH the tree (grep/glob, ideally
// batched) rather than open files at random, then report the value. This is the
// hermetic stand-in for "find X in an unfamiliar layout" that the corpus lacked.
func searchFindValue() Task {
	return Task{
		Name: "search-find-value",
		Prompt: "Somewhere under this directory exactly one file defines RELEASE_CODE " +
			"(as RELEASE_CODE=<value>). Find it and write just that value — nothing else, " +
			"no trailing newline — to found.txt in this directory.",
		Setup: writeModule(map[string]string{
			"notes/intro.txt":    "project notes\nnothing important here\n",
			"notes/changelog.md": "## history\n- initial\n- tweaks\n",
			"config/app.env":     "DEBUG=false\nPORT=8080\n",
			"config/build.env":   "TARGET=linux\nRELEASE_CODE=falcon-9\nARCH=amd64\n",
			"config/extra.env":   "CACHE=on\nWORKERS=4\n",
			"src/main.txt":       "entry point placeholder\n",
			"src/util.txt":       "helpers placeholder\n",
			"README.md":          "# demo\nsee config/ for settings\n",
		}),
		Oracle: FileEquals("found.txt", "falcon-9"),
	}
}

// locateFixConstant (DISCOVERY/TRACE): a wrong constant lives in one of several
// files, and the obvious file (budget.go) only USES it — the agent must search
// for where it's DEFINED before fixing. A scaled-up grep-and-fix across a wider
// surface that rewards efficient (batched) searching.
func locateFixConstant() Task {
	return Task{
		Name: "locate-fix-constant",
		Prompt: "A test is failing: Budget() should return 500 but returns the wrong " +
			"number. The value comes from a constant defined in one of several files in " +
			"this module — find where it's defined and fix it. Do not edit the test.",
		Setup: writeModule(map[string]string{
			"go.mod":    "module borgeval\n\ngo 1.21\n",
			"budget.go": "package plan\n\n// Budget is the daily limit, read from the configured constant.\nfunc Budget() int { return dailyLimit }\n",
			"limits.go": "package plan\n\n// dailyLimit is wrong — it should be 500.\nconst dailyLimit = 50\n",
			"plan.go":   "package plan\n\nconst PlanName = \"pro\"\n",
			"meta.go":   "package plan\n\nconst Version = 3\n",
			"budget_test.go": "package plan\n\nimport \"testing\"\n\n" +
				"func TestBudget(t *testing.T) {\n" +
				"\tif got := Budget(); got != 500 {\n\t\tt.Fatalf(\"Budget = %d, want 500\", got)\n\t}\n}\n",
		}),
		Oracle: GoTestPasses(),
	}
}

// traceCallChain (DISCOVERY): the bug sits two calls deep in a chain, and the
// obvious entry-point file is clean — the agent must trace through the code to
// find the real culprit rather than edit the first file it opens.
func traceCallChain() Task {
	return Task{
		Name: "trace-call-chain",
		Prompt: "A test is failing: Handle(\"  Hi There  \") should return \"hi there\" " +
			"(trimmed and lowercased). Trace the implementation to find the real bug and " +
			"fix it — the entry point is not where the bug is. Do not edit the test.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"handler.go": "package svc\n\n" +
				"// Handle is the entry point; it delegates down the chain.\n" +
				"func Handle(in string) string { return clean(in) }\n",
			"clean.go": "package svc\n\nimport \"strings\"\n\n" +
				"func clean(s string) string { return strings.TrimSpace(norm(s)) }\n",
			"norm.go": "package svc\n\nimport \"strings\"\n\n" +
				"// norm normalizes the string's case.\n" +
				"func norm(s string) string { return strings.ToUpper(s) }\n",
			"handler_test.go": "package svc\n\nimport \"testing\"\n\n" +
				"func TestHandle(t *testing.T) {\n" +
				"\tif got := Handle(\"  Hi There  \"); got != \"hi there\" {\n" +
				"\t\tt.Fatalf(\"Handle = %q, want %q\", got, \"hi there\")\n\t}\n}\n",
		}),
		Oracle: GoTestPasses(),
	}
}

// redHerringFix (ADAPTATION): a scary-looking but CORRECT helper is a decoy; the
// real bug is a wrong operator in the function under test. The agent must not be
// misled into "fixing" the correct code.
func redHerringFix() Task {
	return Task{
		Name: "red-herring-fix",
		Prompt: "A test is failing: Discount(100, 20) should return 80. The helper " +
			"unsafeRound has an alarming comment but is actually correct — the real bug is " +
			"elsewhere. Find and fix it without editing the test or the helper.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"discount.go": "package pricing\n\n" +
				"// unsafeRound LOOKS dangerous but is correct: step 1 is a no-op round.\n" +
				"func unsafeRound(x, step int) int { return x - x%step }\n\n" +
				"// Discount returns price with pct percent taken off.\n" +
				"func Discount(price, pct int) int { return unsafeRound(price+price*pct/100, 1) }\n",
			"discount_test.go": "package pricing\n\nimport \"testing\"\n\n" +
				"func TestDiscount(t *testing.T) {\n" +
				"\tif got := Discount(100, 20); got != 80 {\n" +
				"\t\tt.Fatalf(\"Discount(100,20) = %d, want 80\", got)\n\t}\n}\n",
		}),
		Oracle: GoTestPasses(),
	}
}

// implementFromContext (FEATURE FROM CONTEXT): a stub method must be implemented
// to pass a test, and doing it right means READING the existing methods first
// (reuse Get, mirror the receiver style) rather than poking the map directly.
func implementFromContext() Task {
	return Task{
		Name: "implement-from-context",
		Prompt: "Implement the GetOr method in store.go so the test passes: it returns the " +
			"stored value for the key, or the provided default when the key is absent. Read " +
			"the existing Store methods and reuse Get rather than touching the map directly. " +
			"Do not edit the test.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"store.go": "package kv\n\n" +
				"type Store struct{ m map[string]int }\n\n" +
				"func New() *Store { return &Store{m: map[string]int{}} }\n\n" +
				"func (s *Store) Set(k string, v int) { s.m[k] = v }\n\n" +
				"func (s *Store) Get(k string) (int, bool) { v, ok := s.m[k]; return v, ok }\n\n" +
				"// GetOr returns the value for k, or def when k is absent. TODO: implement.\n" +
				"func (s *Store) GetOr(k string, def int) int { return 0 }\n",
			"store_test.go": "package kv\n\nimport \"testing\"\n\n" +
				"func TestGetOr(t *testing.T) {\n" +
				"\ts := New()\n\ts.Set(\"a\", 5)\n" +
				"\tif got := s.GetOr(\"a\", 9); got != 5 {\n\t\tt.Fatalf(\"GetOr present = %d, want 5\", got)\n\t}\n" +
				"\tif got := s.GetOr(\"missing\", 9); got != 9 {\n\t\tt.Fatalf(\"GetOr absent = %d, want 9\", got)\n\t}\n}\n",
		}),
		Oracle: GoTestPasses(),
	}
}

// implementInterface: a compile-time interface assertion fails because a method
// is missing. Exercises "make this type satisfy the interface" — a very common
// coding task. Oracle is `go build` (the assertion won't compile until fixed).
func implementInterface() Task {
	return Task{
		Name: "implement-interface",
		Prompt: "This module does not compile: Rect must satisfy the Shape interface " +
			"(see the `var _ Shape = Rect{}` assertion in shape.go) but is missing the " +
			"Area method. Add an Area method on Rect that returns width times height so " +
			"`go build ./...` passes.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"shape.go": "package geom\n\n" +
				"type Shape interface{ Area() float64 }\n\n" +
				"// Compile-time assertion: Rect must implement Shape.\n" +
				"var _ Shape = Rect{}\n",
			"rect.go": "package geom\n\ntype Rect struct{ W, H float64 }\n",
		}),
		Oracle: Builds(),
	}
}

// fixNilMapPanic: a function writes to a nil map and panics at runtime. Exercises
// recognizing and fixing a classic Go nil-map bug from a failing test.
func fixNilMapPanic() Task {
	return Task{
		Name: "fix-nil-map-panic",
		Prompt: "A test in this directory panics with \"assignment to entry in nil map\". " +
			"Fix Tally in count.go so it initializes the map before writing. Do not edit " +
			"the test.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"count.go": "package tally\n\n" +
				"// Tally counts occurrences, but writes to a nil map and panics.\n" +
				"func Tally(items []string) map[string]int {\n" +
				"\tvar m map[string]int\n" +
				"\tfor _, it := range items {\n\t\tm[it]++\n\t}\n\treturn m\n}\n",
			"count_test.go": "package tally\n\nimport \"testing\"\n\n" +
				"func TestTally(t *testing.T) {\n" +
				"\tm := Tally([]string{\"a\", \"a\", \"b\"})\n" +
				"\tif m[\"a\"] != 2 || m[\"b\"] != 1 {\n\t\tt.Fatalf(\"got %v\", m)\n\t}\n}\n",
		}),
		Oracle: GoTestPasses(),
	}
}

// fixJSONTag: a struct field is dropped on marshal because its json tag is "-".
// Exercises a subtle, real serialization bug surfaced by a round-trip test.
func fixJSONTag() Task {
	return Task{
		Name: "fix-json-tag",
		Prompt: "A JSON round-trip test is failing: the Email field is dropped because " +
			"its struct tag in user.go is wrong. Fix the tag so Email marshals as " +
			"\"email\" and survives the round-trip. Do not edit the test.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"user.go": "package account\n\n" +
				"type User struct {\n" +
				"\tName  string `json:\"name\"`\n" +
				"\tEmail string `json:\"-\"`\n" +
				"}\n",
			"user_test.go": "package account\n\n" +
				"import (\n\t\"encoding/json\"\n\t\"testing\"\n)\n\n" +
				"func TestRoundtrip(t *testing.T) {\n" +
				"\tb, _ := json.Marshal(User{Name: \"a\", Email: \"x@y.z\"})\n" +
				"\tvar u User\n\t_ = json.Unmarshal(b, &u)\n" +
				"\tif u.Email != \"x@y.z\" {\n\t\tt.Fatalf(\"email lost: %s\", string(b))\n\t}\n}\n",
		}),
		Oracle: GoTestPasses(),
	}
}

// fixBoundsPanic: an off-by-one slice index panics. Exercises the read-the-panic,
// fix-the-index loop on a failing test.
func fixBoundsPanic() Task {
	return Task{
		Name: "fix-bounds-panic",
		Prompt: "Last in last.go panics with \"index out of range\". Fix it to return the " +
			"final element of the slice so the test passes. Do not edit the test.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"last.go": "package sliceutil\n\n" +
				"// Last should return the final element, but indexes one past the end.\n" +
				"func Last(xs []int) int { return xs[len(xs)] }\n",
			"last_test.go": "package sliceutil\n\nimport \"testing\"\n\n" +
				"func TestLast(t *testing.T) {\n" +
				"\tif got := Last([]int{1, 2, 3}); got != 3 {\n" +
				"\t\tt.Fatalf(\"Last = %d, want 3\", got)\n\t}\n}\n",
		}),
		Oracle: GoTestPasses(),
	}
}

// mergeBranch exercises real git work through the bash tool, not file edits: the
// workspace is a git repository on main with a `feature/greeting` branch one
// commit ahead, and the agent must merge that branch into main. The oracle checks
// the feature branch is now an ancestor of main's HEAD — i.e. the merge actually
// landed. This is the hermetic half of the "merge/promote a PR" behavior the
// shell-and-git prompt guidance targets; the gh-PR-discovery half needs a real
// remote, so it's verified live by hand rather than in this offline corpus. Bash
// runs in the process cwd (not the workspace), so the prompt tells the agent to
// target the repo explicitly — mirroring the "use absolute paths" convention.
func mergeBranch() Task {
	return Task{
		Name: "merge-feature-branch",
		Prompt: "This working directory is a git repository on branch main. A branch " +
			"named feature/greeting is one commit ahead of main. Merge feature/greeting " +
			"into main. Run git against this repository explicitly — cd into the working " +
			"directory first, or pass `git -C <that directory>` — since your shell may " +
			"start elsewhere.",
		Setup:  gitRepo,
		Oracle: Command("git", "merge-base", "--is-ancestor", "feature/greeting", "HEAD"),
	}
}

// gitRepo materializes a real git repository in the workspace: main with a base
// commit, plus a feature/greeting branch one commit ahead, left checked out on
// main. Config is forced local (global/system config ignored) so the fixture is
// identical on any machine.
func gitRepo(ws string) error {
	if err := runGit(ws, "init", "-b", "main"); err != nil {
		return err
	}
	if err := runGit(ws, "config", "user.email", "eval@borg.test"); err != nil {
		return err
	}
	if err := runGit(ws, "config", "user.name", "borg eval"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		return err
	}
	if err := runGit(ws, "add", "README.md"); err != nil {
		return err
	}
	if err := runGit(ws, "commit", "-m", "base"); err != nil {
		return err
	}
	if err := runGit(ws, "checkout", "-b", "feature/greeting"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(ws, "greeting.txt"), []byte("hello\n"), 0o644); err != nil {
		return err
	}
	if err := runGit(ws, "add", "greeting.txt"); err != nil {
		return err
	}
	if err := runGit(ws, "commit", "-m", "add greeting"); err != nil {
		return err
	}
	return runGit(ws, "checkout", "main")
}

// runGit runs one git subcommand against the workspace repo, isolated from any
// host git config so the fixture is deterministic.
func runGit(ws string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", ws}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// learnProject exercises `borg learn`: the model must read the brief (which
// states the goal) and recognize the decoy, then write a BORG.md that names the
// real goal. The oracle checks the goal made it into the file — a depth test, not
// just "a file exists". A shallow harness that reads only the root index fails.
func learnProject() Task {
	return Task{
		Name:   "learn-project-context",
		Prompt: agent.LearnPrompt,
		Setup: writeModule(map[string]string{
			"BRIEF.md": "# Project brief\n\nGoal: a static marketing website to sell CORN for our agro company.\n",
			"index.html": "<!-- Unrelated demo: a Web Audio beat machine, NOT the corn site. -->\n" +
				"<html><body>beats</body></html>\n",
			"site/index.html": "<html><head><title>Golden Harvest — Corn</title></head><body>corn</body></html>\n",
		}),
		Oracle: FileContains(agent.ProjectContextFile, "corn"),
	}
}

// learnStaysLean is the timed /learn smoke check: a multi-directory fixture where
// a healthy run writes BORG.md after a few batched read bursts, but a wandering
// run (listing/reading in many separate steps, or deferring the write) racks up
// steps. The StepBudget fails a run that wanders even when the file is correct —
// the guard that would have caught the prompt regression that ~doubled /learn's
// time. Steps ≈ wall-clock, so a "got slower" regression surfaces as a FAIL in
// the nightly eval that a pass/fail oracle alone can't see.
func learnStaysLean() Task {
	return Task{
		Name:   "learn-stays-lean",
		Prompt: agent.LearnPrompt,
		Setup: writeModule(map[string]string{
			"README.md":                   "# Ledger\n\nGoal: a CLI double-entry BOOKKEEPING tool.\n",
			"go.mod":                      "module ledger\n\ngo 1.21\n",
			"cmd/ledger/main.go":          "package main\n\nfunc main() {}\n",
			"internal/account/account.go": "package account\n\ntype Account struct{ Name string }\n",
			"internal/posting/posting.go": "package posting\n\ntype Posting struct{ Amount int }\n",
			"internal/report/report.go":   "package report\n\nfunc Balance() int { return 0 }\n",
			"docs/USAGE.md":               "Run `ledger add` to record a posting.\n",
		}),
		Oracle:     FileContains(agent.ProjectContextFile, "bookkeeping"),
		StepBudget: 12, // a batching run writes in ~4-6 steps; the explore-budget forces by 16
	}
}

// fixCompileError: the module references an undefined identifier; the agent must
// make `go build ./...` succeed while keeping Add a correct sum.
func fixCompileError() Task {
	return Task{
		Name: "fix-compile-error",
		Prompt: "The Go module in this directory does not compile (`go build ./...` " +
			"reports an undefined identifier). Fix mathx.go so it builds, keeping " +
			"Add a function that returns the sum of its two int arguments.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"mathx.go": "package mathx\n\n" +
				"// Add should return a + b, but references an undefined identifier.\n" +
				"func Add(a, b int) int { return a + b + c }\n",
		}),
		Oracle: Builds(),
	}
}

// fixFailingTest: a committed test fails because the implementation is wrong; the
// agent must make `go test ./...` pass without touching the test.
func fixFailingTest() Task {
	return Task{
		Name: "fix-failing-test",
		Prompt: "A test in this directory is failing (`go test ./...`). Fix the " +
			"implementation in strutil.go so the test passes. Do not edit the test.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"strutil.go": "package strutil\n\n" +
				"// Reverse should return s reversed, but returns it unchanged.\n" +
				"func Reverse(s string) string { return s }\n",
			"strutil_test.go": "package strutil\n\n" +
				"import \"testing\"\n\n" +
				"func TestReverse(t *testing.T) {\n" +
				"\tif got := Reverse(\"abc\"); got != \"cba\" {\n" +
				"\t\tt.Fatalf(\"Reverse(abc) = %q, want cba\", got)\n" +
				"\t}\n}\n",
		}),
		Oracle: GoTestPasses(),
	}
}

// fixOffByOne: a real bug in existing logic (a loop bound stops one short). The
// agent must edit the implementation so `go test ./...` passes — a natural
// edit_file target (change one token in place).
func fixOffByOne() Task {
	return Task{
		Name: "fix-off-by-one",
		Prompt: "Sum(n) should return 1+2+...+n, but a failing test shows it stops " +
			"one short. Fix sumx.go so `go test ./...` passes. Do not edit the test.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"sumx.go": "package sumx\n\n" +
				"// Sum returns 1+2+...+n, but the loop bound is off by one.\n" +
				"func Sum(n int) int {\n" +
				"\ttotal := 0\n" +
				"\tfor i := 1; i < n; i++ {\n" +
				"\t\ttotal += i\n" +
				"\t}\n" +
				"\treturn total\n}\n",
			"sumx_test.go": "package sumx\n\n" +
				"import \"testing\"\n\n" +
				"func TestSum(t *testing.T) {\n" +
				"\tif got := Sum(5); got != 15 {\n" +
				"\t\tt.Fatalf(\"Sum(5) = %d, want 15\", got)\n" +
				"\t}\n}\n",
		}),
		Oracle: GoTestPasses(),
	}
}

// implementMissingFunc: a test references a function that doesn't exist (the
// package won't compile). The agent must add it — exercises writing new code.
func implementMissingFunc() Task {
	return Task{
		Name: "implement-missing-function",
		Prompt: "A test references a function Greet that does not exist yet, so the " +
			"package fails to compile. Add greet.go implementing Greet so `go test " +
			"./...` passes. Do not edit the test.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"greet_test.go": "package greet\n\n" +
				"import \"testing\"\n\n" +
				"func TestGreet(t *testing.T) {\n" +
				"\tif got := Greet(\"Bo\"); got != \"Hello, Bo!\" {\n" +
				"\t\tt.Fatalf(\"Greet(Bo) = %q, want Hello, Bo!\", got)\n" +
				"\t}\n}\n",
		}),
		Oracle: GoTestPasses(),
	}
}

// fixAcrossFiles: the failing test and the visible symptom are in different
// files from the actual bug — the agent must trace `Quadruple` → `double` to the
// real defect in ops.go. Exercises multi-file reasoning / search.
func fixAcrossFiles() Task {
	return Task{
		Name: "fix-across-files",
		Prompt: "A test for Quadruple is failing. The bug is not in the test or in " +
			"Quadruple itself — trace the call chain to the real defect and fix it so " +
			"`go test ./...` passes. Do not edit the test.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"ops.go": "package calc\n\n" +
				"// double should return 2*x, but returns x unchanged.\n" +
				"func double(x int) int { return x }\n",
			"api.go": "package calc\n\n" +
				"// Quadruple returns 4*x by doubling twice.\n" +
				"func Quadruple(x int) int { return double(double(x)) }\n",
			"calc_test.go": "package calc\n\n" +
				"import \"testing\"\n\n" +
				"func TestQuadruple(t *testing.T) {\n" +
				"\tif got := Quadruple(3); got != 12 {\n" +
				"\t\tt.Fatalf(\"Quadruple(3) = %d, want 12\", got)\n" +
				"\t}\n}\n",
		}),
		Oracle: GoTestPasses(),
	}
}

// grepAndFix: the wrong string literal is produced deep in the package, away
// from the failing test and the public entrypoint, among decoy files. The agent
// has to *search* (grep) the tree to find where "PENDING" is returned and change
// it — exercises locating code, not just editing a known file.
func grepAndFix() Task {
	return Task{
		Name: "grep-and-fix",
		Prompt: "A test expects Status() to return \"DONE\", but it returns the wrong " +
			"value. The offending string literal is produced somewhere in this package " +
			"(not in Status itself, and not in the test). Search the files to find it and " +
			"fix it so `go test ./...` passes. Do not edit the test.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"status.go": "package status\n\n" +
				"// Status is the public entrypoint; the real value comes from phase().\n" +
				"func Status() string { return phase() }\n",
			"phase.go": "package status\n\n" +
				"// phase returns the current phase string (this is the buggy one).\n" +
				"func phase() string { return \"PENDING\" }\n",
			"labels.go": "package status\n\n" +
				"// Label decorates a phase for display — an unrelated decoy.\n" +
				"func Label(s string) string { return \"[\" + s + \"]\" }\n",
			"status_test.go": "package status\n\n" +
				"import \"testing\"\n\n" +
				"func TestStatus(t *testing.T) {\n" +
				"\tif got := Status(); got != \"DONE\" {\n" +
				"\t\tt.Fatalf(\"Status() = %q, want DONE\", got)\n" +
				"\t}\n}\n",
		}),
		Oracle: GoTestPasses(),
	}
}

// fixTwoBugsTwoFiles: the failing test needs TWO independent defects fixed, one
// in each of two files — fixing only one still fails. Exercises coordinated
// multi-file edits, not a single hop (the agent must not stop at the first bug).
func fixTwoBugsTwoFiles() Task {
	return Task{
		Name: "fix-two-bugs-two-files",
		Prompt: "A test for Clean is failing. There are TWO separate bugs — one in " +
			"validate.go and one in normalize.go — and BOTH must be fixed for the test " +
			"to pass. Fix them so `go test ./...` passes. Do not edit the test.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"validate.go": "package form\n\n" +
				"// nonEmpty should report whether s is non-empty, but the check is inverted.\n" +
				"func nonEmpty(s string) bool { return s == \"\" }\n",
			"normalize.go": "package form\n\n" +
				"// trimDot should strip a single trailing '.', but returns s unchanged.\n" +
				"func trimDot(s string) string { return s }\n",
			"form.go": "package form\n\n" +
				"// Clean trims a trailing dot and reports whether the input was non-empty.\n" +
				"func Clean(s string) (string, bool) { return trimDot(s), nonEmpty(s) }\n",
			"form_test.go": "package form\n\n" +
				"import \"testing\"\n\n" +
				"func TestClean(t *testing.T) {\n" +
				"\tgot, ok := Clean(\"hi.\")\n" +
				"\tif got != \"hi\" || !ok {\n" +
				"\t\tt.Fatalf(\"Clean(hi.) = (%q, %v), want (hi, true)\", got, ok)\n" +
				"\t}\n}\n",
		}),
		Oracle: GoTestPasses(),
	}
}

// refactorSignature: a test now calls Apply with an extra argument, so both Apply
// and the scale helper it delegates to must change signature in lockstep across
// two files — leaving either stale fails to compile. A real refactor, not a
// one-token bugfix.
func refactorSignature() Task {
	return Task{
		Name: "refactor-signature",
		Prompt: "A test now calls Apply(x, factor) with two arguments, but Apply and " +
			"the scale helper it uses each take only one. Update BOTH so Apply(x, factor) " +
			"returns x*factor and `go test ./...` passes. Do not edit the test.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"scale.go": "package calc\n\n" +
				"// scale multiplies x by a fixed factor of 2.\n" +
				"func scale(x int) int { return x * 2 }\n",
			"apply.go": "package calc\n\n" +
				"// Apply scales x.\n" +
				"func Apply(x int) int { return scale(x) }\n",
			"apply_test.go": "package calc\n\n" +
				"import \"testing\"\n\n" +
				"func TestApply(t *testing.T) {\n" +
				"\tif got := Apply(3, 4); got != 12 {\n" +
				"\t\tt.Fatalf(\"Apply(3, 4) = %d, want 12\", got)\n" +
				"\t}\n}\n",
		}),
		Oracle: GoTestPasses(),
	}
}

// fixClampLogic: a "compiles-but-wrong" trap. Clamp returns its input unchanged;
// a partial fix that handles only one bound still builds and still fails. The
// multi-case test is the only thing that catches it — so this task discriminates
// a *compile* check (which would pass a half-fix) from real test feedback.
func fixClampLogic() Task {
	return Task{
		Name: "fix-clamp-logic",
		Prompt: "Clamp(x, lo, hi) should constrain x to [lo, hi] — return lo when x<lo, " +
			"hi when x>hi, otherwise x — but it returns x unchanged. Fix clamp.go so " +
			"`go test ./...` passes. A partial fix that handles only one bound will still " +
			"fail. Do not edit the test.",
		Setup: writeModule(map[string]string{
			"go.mod": "module borgeval\n\ngo 1.21\n",
			"clamp.go": "package mathx\n\n" +
				"// Clamp should constrain x to [lo, hi], but returns x unchanged.\n" +
				"func Clamp(x, lo, hi int) int { return x }\n",
			"clamp_test.go": "package mathx\n\n" +
				"import \"testing\"\n\n" +
				"func TestClamp(t *testing.T) {\n" +
				"\tcases := []struct{ x, lo, hi, want int }{\n" +
				"\t\t{5, 0, 3, 3},\n" +
				"\t\t{-1, 0, 3, 0},\n" +
				"\t\t{2, 0, 3, 2},\n" +
				"\t}\n" +
				"\tfor _, c := range cases {\n" +
				"\t\tif got := Clamp(c.x, c.lo, c.hi); got != c.want {\n" +
				"\t\t\tt.Fatalf(\"Clamp(%d,%d,%d) = %d, want %d\", c.x, c.lo, c.hi, got, c.want)\n" +
				"\t\t}\n" +
				"\t}\n}\n",
		}),
		Oracle: GoTestPasses(),
	}
}

// writeModule returns a Setup that writes the given files (relative path →
// contents) into the workspace.
// keepsGofmtCleanOnImplement: implement a function so the test passes — and the
// result must ALSO be gofmt-clean. End-to-end guard for auto-format-after-edit: if
// that harness feature regressed, a model that wrote slightly-off whitespace would
// fail the GofmtClean half even though the code compiles.
func keepsGofmtCleanOnImplement() Task {
	return Task{
		Name:   "keeps-gofmt-clean-implement",
		Prompt: "Implement the function Double in num.go so it returns 2*x and `go test ./...` passes. Do not edit the test.",
		Setup: writeModule(map[string]string{
			"go.mod":      "module borgeval\n\ngo 1.21\n",
			"num.go":      "package num\n\n// Double should return 2*x.\nfunc Double(x int) int {\n\treturn 0\n}\n",
			"num_test.go": "package num\n\nimport \"testing\"\n\nfunc TestDouble(t *testing.T) {\n\tif got := Double(21); got != 42 {\n\t\tt.Fatalf(\"Double(21) = %d, want 42\", got)\n\t}\n}\n",
		}),
		Oracle: All(GoTestPasses(), GofmtClean()),
	}
}

// keepsGofmtCleanOnEdit: a small fix to an existing function; the file must end
// gofmt-clean (auto-format guard) and the test must pass.
func keepsGofmtCleanOnEdit() Task {
	return Task{
		Name:   "keeps-gofmt-clean-edit",
		Prompt: "greeting.go's Greet returns the wrong text — a test shows it should return \"Hi, <name>!\". Fix Greet so `go test ./...` passes. Do not edit the test.",
		Setup: writeModule(map[string]string{
			"go.mod":           "module borgeval\n\ngo 1.21\n",
			"greeting.go":      "package greeting\n\n// Greet returns a greeting for name.\nfunc Greet(name string) string {\n\treturn \"Hello, \" + name\n}\n",
			"greeting_test.go": "package greeting\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif got := Greet(\"Bo\"); got != \"Hi, Bo!\" {\n\t\tt.Fatalf(\"Greet(Bo) = %q, want Hi, Bo!\", got)\n\t}\n}\n",
		}),
		Oracle: All(GoTestPasses(), GofmtClean()),
	}
}

func writeModule(files map[string]string) func(string) error {
	return func(ws string) error {
		for name, body := range files {
			p := filepath.Join(ws, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				return err
			}
		}
		return nil
	}
}
