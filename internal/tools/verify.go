package tools

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// verify ------------------------------------------------------------------

const verifyTimeout = 120 * time.Second

// verifyTool runs the project's COMPILE / SYNTAX check after edits, so the model
// can catch (and fix) a broken edit before finishing. It is deliberately limited
// to checks that compile or PARSE the code only — they never execute the project's
// code, tests, or dependencies — so it is safe to run without a permission prompt.
// Anything that runs code (tests, codegen builds, cargo/maven/gradle plugins,
// build scripts) stays behind the permission-gated bash tool.
type verifyTool struct{}

func (verifyTool) Name() string { return "verify" }
func (verifyTool) Description() string {
	return "Check that the project still COMPILES / parses after your edits — auto-detects the language and runs a SAFE compile-or-syntax-only check (Go: go build; TypeScript: tsc --noEmit; Python: compileall; PHP: php -l; JavaScript: node --check; Ruby: ruby -c), returning PASS or FAIL with the errors. It never runs your code or tests. Run it after editing code and fix any failures before finishing. For tests or other ecosystems, use bash."
}
func (verifyTool) Mutating() bool { return false } // compile/parse-only, no file writes, no code exec
func (verifyTool) Schema() json.RawMessage {
	// No arguments. Must NOT be `"properties":{}` — an empty JSON object is mangled
	// into an empty array (`[]`) by the metered proxy's PHP json round-trip, which
	// the guided-decoding grammar compiler then rejects ("properties must be an
	// object"), breaking EVERY Floko turn (it runs under tool_choice=required). A
	// bare object schema avoids emitting any empty `{}`.
	return json.RawMessage(`{"type":"object"}`)
}

func (verifyTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	chk := detectVerify()
	if chk == nil {
		return "no safe compile check detected (recognized: Go, TypeScript, JavaScript, Python, PHP, Ruby — and the toolchain must be installed). For other ecosystems or to run tests, use bash to run your build/test command.", nil
	}
	runCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	if chk.whole != nil {
		return runWholeCheck(runCtx, chk)
	}
	return runPerFileCheck(runCtx, chk)
}

// verifyCheck is a single safe check for the working directory. It is EITHER a
// whole-project command (whole), or a per-file syntax check (match selects the
// files, fileCmd is the parse-only command run on each). Both kinds are
// compile/parse-only — they never execute the project's code.
type verifyCheck struct {
	label   string                     // human label shown in PASS/FAIL, e.g. "php -l"
	whole   []string                   // whole-project command; nil ⇒ per-file
	match   func(name string) bool     // per-file: which files to check
	fileCmd func(path string) []string // per-file: the parse command for one file
}

// lookPath is exec.LookPath, indirected so tests can simulate which toolchains
// are installed without depending on the CI image's contents.
var lookPath = exec.LookPath

func have(tool string) bool { _, err := lookPath(tool); return err == nil }

// pythonExe returns the available Python interpreter ("python3" preferred), or "".
func pythonExe() string {
	for _, p := range []string{"python3", "python"} {
		if have(p) {
			return p
		}
	}
	return ""
}

// detectVerify picks a safe compile/parse-only check for the working directory,
// or nil when none applies (unknown project, or the toolchain isn't installed).
// Whole-project compilers (precise, one invocation) are preferred; per-file syntax
// linters cover the interpreted languages that have no project-wide compile step.
func detectVerify() *verifyCheck {
	switch {
	case fileExists("go.mod") && have("go"):
		return &verifyCheck{label: "go build ./...", whole: []string{"go", "build", "./..."}} // compiles, no code exec
	case fileExists("tsconfig.json") && have("npx"):
		return &verifyCheck{label: "tsc --noEmit", whole: []string{"npx", "--no-install", "tsc", "--noEmit"}} // type-check only
	}

	// Interpreted languages: detect by the presence of source files (and the
	// interpreter being installed). Each check only PARSES the code, never runs it.
	exts := sourceExtsPresent()
	switch {
	case exts[".py"] && pythonExe() != "":
		py := pythonExe()
		// compileall byte-compiles every module without importing/running any of it.
		return &verifyCheck{label: py + " -m compileall", whole: []string{py, "-m", "compileall", "-q", "."}}
	case exts[".php"] && have("php"):
		return &verifyCheck{label: "php -l", match: extMatch(".php"),
			fileCmd: func(p string) []string { return []string{"php", "-l", p} }} // lint (syntax) only
	case (exts[".js"] || exts[".mjs"] || exts[".cjs"]) && have("node"):
		return &verifyCheck{label: "node --check", match: extMatch(".js", ".mjs", ".cjs"),
			fileCmd: func(p string) []string { return []string{"node", "--check", p} }} // parse, don't run
	case exts[".rb"] && have("ruby"):
		return &verifyCheck{label: "ruby -c", match: extMatch(".rb"),
			fileCmd: func(p string) []string { return []string{"ruby", "-c", p} }} // syntax check only
	}
	return nil
}

// runWholeCheck runs a whole-project command and reports PASS / FAIL.
func runWholeCheck(ctx context.Context, c *verifyCheck) (string, error) {
	out, err := hermeticCmd(ctx, c.whole[0], c.whole[1:]...).CombinedOutput()
	if err != nil {
		return "FAIL (" + c.label + "):\n" + truncate(string(out), maxExecBytes) + "\n[exit: " + err.Error() + "]", nil
	}
	return "PASS (" + c.label + ")", nil
}

// runPerFileCheck parses each matching source file and aggregates any failures.
func runPerFileCheck(ctx context.Context, c *verifyCheck) (string, error) {
	files := collectFiles(c.match)
	if len(files) == 0 {
		return "PASS (" + c.label + ": no files)", nil
	}
	var failures strings.Builder
	failed := 0
	for _, f := range files {
		if ctx.Err() != nil {
			break
		}
		argv := c.fileCmd(f)
		if out, err := hermeticCmd(ctx, argv[0], argv[1:]...).CombinedOutput(); err != nil {
			failed++
			failures.WriteString(strings.TrimSpace(string(out)))
			failures.WriteByte('\n')
		}
	}
	if failed > 0 {
		return "FAIL (" + c.label + ", " + strconv.Itoa(failed) + " of " + strconv.Itoa(len(files)) +
			" files):\n" + truncate(failures.String(), maxExecBytes), nil
	}
	return "PASS (" + c.label + ", " + strconv.Itoa(len(files)) + " files)", nil
}

// VerifyApplicable reports whether a safe compile check exists for the working
// directory. The agent loop uses it to decide whether auto-verifying an edit
// would do anything (so it stays silent in projects with no compile check).
func VerifyApplicable() bool { return detectVerify() != nil }

// verifySkipDirs are directories never walked for source detection — dependency
// trees and build output, which aren't the user's code (and can be huge).
var verifySkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"dist": true, "build": true, "target": true, "__pycache__": true,
	".tox": true, ".mypy_cache": true, ".next": true,
}

// verifyWalkCap / verifyFileCap bound the tree scan and the per-file check so a
// huge repo can't wedge a non-gated verify.
const (
	verifyWalkCap = 20000
	verifyFileCap = 2000
)

// sourceExtsPresent returns the set of file extensions present in the tree
// (lowercased, skipping dependency/build dirs), used to detect the language.
func sourceExtsPresent() map[string]bool {
	exts := map[string]bool{}
	n := 0
	_ = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != "." && verifySkipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if n++; n > verifyWalkCap {
			return fs.SkipAll
		}
		exts[strings.ToLower(filepath.Ext(d.Name()))] = true
		return nil
	})
	return exts
}

// collectFiles returns the matching source files in the tree (skipping
// dependency/build dirs), capped so a non-gated check stays bounded.
func collectFiles(match func(name string) bool) []string {
	var files []string
	_ = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != "." && verifySkipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if match(d.Name()) {
			if files = append(files, path); len(files) >= verifyFileCap {
				return fs.SkipAll
			}
		}
		return nil
	})
	return files
}

// extMatch builds a per-file selector matching any of the given extensions.
func extMatch(exts ...string) func(name string) bool {
	return func(name string) bool {
		e := strings.ToLower(filepath.Ext(name))
		for _, x := range exts {
			if e == x {
				return true
			}
		}
		return false
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
