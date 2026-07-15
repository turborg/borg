package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/turborg/borg/internal/config"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestDefaultRegistryAdvertisesTools(t *testing.T) {
	r := DefaultRegistry()
	defs := r.Definitions()
	require.NotEmpty(t, defs)

	names := map[string]bool{}
	for _, d := range defs {
		names[d.Function.Name] = true
		require.Equal(t, "function", d.Type)
		require.True(t, json.Valid(d.Function.Parameters), "schema for %s must be valid JSON", d.Function.Name)
	}
	for _, want := range []string{"read_file", "write_file", "edit_file", "edit_lines", "verify", "list_dir", "grep", "glob", "bash", "bash_output", "kill_shell", "ask_user", "finish"} {
		require.True(t, names[want], "missing tool %q", want)
	}
}

func TestWriteReadEditRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	ctx := context.Background()

	_, err := writeFile{}.Execute(ctx, mustJSON(t, map[string]string{"path": path, "content": "hello world"}))
	require.NoError(t, err)

	out, err := readFile{}.Execute(ctx, mustJSON(t, map[string]string{"path": path}))
	require.NoError(t, err)
	require.Equal(t, "1\thello world\n[1 line]", out) // numbered content + total line count

	_, err = editFile{}.Execute(ctx, mustJSON(t, map[string]string{"path": path, "old_string": "world", "new_string": "borg"}))
	require.NoError(t, err)

	b, _ := os.ReadFile(path)
	require.Equal(t, "hello borg", string(b))
}

func TestEditFileRejectsAmbiguousMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dup.txt")
	require.NoError(t, os.WriteFile(path, []byte("x x x"), 0o644))

	_, err := editFile{}.Execute(context.Background(), mustJSON(t, map[string]string{
		"path": path, "old_string": "x", "new_string": "y",
	}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "appears 3 times")
	require.Contains(t, err.Error(), "replace_all") // points at the way out

	// replace_all changes every occurrence.
	out, err := editFile{}.Execute(context.Background(), mustJSON(t, map[string]any{
		"path": path, "old_string": "x", "new_string": "y", "replace_all": true,
	}))
	require.NoError(t, err)
	require.Contains(t, out, "replaced 3 occurrences")
	b, _ := os.ReadFile(path)
	require.Equal(t, "y y y", string(b))
}

func TestEditFileWhitespaceTolerantApply(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ws.go")
	require.NoError(t, os.WriteFile(path, []byte("func f() {\n\treturn 1\n}\n"), 0o644)) // tab-indented

	// old_string uses spaces instead of the file's tab: the exact match fails, but the
	// text is a UNIQUE whitespace-flexible match, so the edit is applied to that line
	// directly (one round-trip) instead of bouncing an error back.
	out, err := editFile{}.Execute(context.Background(), mustJSON(t, map[string]string{
		"path": path, "old_string": "    return 1", "new_string": "\treturn 2",
	}))
	require.NoError(t, err)
	require.Contains(t, out, "ignoring whitespace")
	b, _ := os.ReadFile(path)
	require.Contains(t, string(b), "return 2")
	require.NotContains(t, string(b), "return 1")
}

// A command borg spawns runs in the user's REAL shell environment — not borg's — so a
// setting borg injected from its settings file never leaks into a child process (the
// bug where a config value bled into a test and made it pass in CI but fail locally).
// End-to-end through the bash tool. General: nothing here is language- or repo-specific.
func TestSpawnedCommandEnvIsHermetic(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "borg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "borg", "settings.json"),
		[]byte(`{"force_device":true}`), 0o600))
	_ = os.Unsetenv("BORG_FORCE_DEVICE")
	t.Cleanup(func() { _ = os.Unsetenv("BORG_FORCE_DEVICE") })

	config.LoadSettingsFile() // injects BORG_FORCE_DEVICE into borg's own process env
	require.Equal(t, "true", os.Getenv("BORG_FORCE_DEVICE"))

	out, err := bashTool{bg: newBGManager()}.Execute(context.Background(),
		mustJSON(t, map[string]string{"command": `echo "BFD=[${BORG_FORCE_DEVICE:-MISSING}]"`}))
	require.NoError(t, err)
	require.Contains(t, out, "BFD=[MISSING]") // the child never saw borg's injected var
}

func TestEditFileReturnsDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "code.txt")
	require.NoError(t, os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644))

	out, err := editFile{}.Execute(context.Background(), mustJSON(t, map[string]string{
		"path": path, "old_string": "beta", "new_string": "BETA",
	}))
	require.NoError(t, err)
	require.Contains(t, out, "edited")
	require.Contains(t, out, "-beta")  // unified diff: old line removed
	require.Contains(t, out, "+BETA")  // new line added
	require.Contains(t, out, " alpha") // surrounding context preserved
}

func TestWriteFileCreatedVsOverwrote(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")

	out, err := writeFile{}.Execute(context.Background(), mustJSON(t, map[string]string{"path": path, "content": "a\nb"}))
	require.NoError(t, err)
	require.Contains(t, out, "created")
	require.Contains(t, out, "2 lines")

	out, err = writeFile{}.Execute(context.Background(), mustJSON(t, map[string]string{"path": path, "content": "x"}))
	require.NoError(t, err)
	require.Contains(t, out, "overwrote")
	require.Contains(t, out, "1 line")
}

func TestEditLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	require.NoError(t, os.WriteFile(p, []byte("a\nb\nc\nd\n"), 0o644))
	ctx := context.Background()

	// Replace lines 2-3 with two new lines (using the numbers from read_file).
	out, err := editLines{}.Execute(ctx, mustJSON(t, map[string]any{"path": p, "start_line": 2, "end_line": 3, "new_text": "B\nB2"}))
	require.NoError(t, err)
	require.Contains(t, out, "-b")  // unified diff of the change
	require.Contains(t, out, "+B2") // the new lines added
	b, _ := os.ReadFile(p)
	require.Equal(t, "a\nB\nB2\nd\n", string(b))

	// Empty new_text deletes the range.
	_, err = editLines{}.Execute(ctx, mustJSON(t, map[string]any{"path": p, "start_line": 1, "end_line": 1, "new_text": ""}))
	require.NoError(t, err)
	b, _ = os.ReadFile(p)
	require.Equal(t, "B\nB2\nd\n", string(b))

	// Past-the-end start is a helpful error (likely stale line numbers).
	_, err = editLines{}.Execute(ctx, mustJSON(t, map[string]any{"path": p, "start_line": 99, "end_line": 99, "new_text": "x"}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "past the end")

	// end < start is rejected.
	_, err = editLines{}.Execute(ctx, mustJSON(t, map[string]any{"path": p, "start_line": 3, "end_line": 1, "new_text": "x"}))
	require.Error(t, err)
}

func TestEditFileStripsLineNumberGutter(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.go")
	require.NoError(t, os.WriteFile(p, []byte("alpha\nbeta\ngamma\n"), 0o644))

	// The model pasted a numbered read line ("2\tbeta") as old_string — edit_file
	// strips the gutter and still finds it.
	_, err := editFile{}.Execute(context.Background(), mustJSON(t, map[string]string{
		"path": p, "old_string": "2\tbeta", "new_string": "BETA",
	}))
	require.NoError(t, err)
	b, _ := os.ReadFile(p)
	require.Equal(t, "alpha\nBETA\ngamma\n", string(b))
}

// Auto-format-after-edit: a Go file written/edited with sloppy whitespace comes
// out gofmt-clean, so the model never has to get indentation byte-perfect (this is
// what killed the ~10-step indentation thrash). gofmt ships with the Go toolchain,
// so it's present wherever these tests run; skip defensively if not.
func requireGofmt(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gofmt"); err != nil {
		t.Skip("gofmt not on PATH")
	}
}

func TestWriteFileAutoFormatsGo(t *testing.T) {
	requireGofmt(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	out, err := writeFile{}.Execute(context.Background(), mustJSON(t, map[string]string{
		"path": p, "content": "package p\nfunc F()  int {\nreturn 1\n}\n", // bad spacing/indent
	}))
	require.NoError(t, err)
	b, _ := os.ReadFile(p)
	require.Equal(t, "package p\n\nfunc F() int {\n\treturn 1\n}\n", string(b)) // gofmt-clean
	require.Contains(t, out, "auto-formatted")
}

func TestEditFileAutoFormatsIndentation(t *testing.T) {
	requireGofmt(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "m.go")
	require.NoError(t, os.WriteFile(p, []byte("package p\n\nfunc F() {\n\tx := 1\n\t_ = x\n}\n"), 0o644))

	// The model inserts a line with WRONG (3-space) indentation — exactly the
	// failure that thrashed before. Auto-format must fix it without a re-edit.
	_, err := editFile{}.Execute(context.Background(), mustJSON(t, map[string]string{
		"path": p, "old_string": "\tx := 1", "new_string": "   x := 2",
	}))
	require.NoError(t, err)
	b, _ := os.ReadFile(p)
	require.Contains(t, string(b), "\tx := 2")     // re-indented to a tab
	require.NotContains(t, string(b), "   x := 2") // the bad indentation is gone
}

func TestAutoFormatLeavesBrokenSyntaxUntouched(t *testing.T) {
	requireGofmt(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.go")
	broken := "package p\nfunc F( {\n" // gofmt can't parse this
	out, err := writeFile{}.Execute(context.Background(), mustJSON(t, map[string]string{"path": p, "content": broken}))
	require.NoError(t, err) // the edit itself still succeeds
	b, _ := os.ReadFile(p)
	require.Equal(t, broken, string(b))      // content preserved for verify/next-edit to fix
	require.NotContains(t, out, "formatted") // nothing was reformatted
}

// Proves the formatter is NOT gofmt-only and prefers the project's OWN tool: a
// fake Laravel `vendor/bin/pint` is invoked on a .php edit (no Go involved).
func TestAutoFormatUsesProjectLocalFormatter(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // project-local lookups (vendor/bin/pint) resolve from the working dir
	require.NoError(t, os.MkdirAll("vendor/bin", 0o755))
	// A stand-in pint: rewrites the file it's given to a fixed, "formatted" form.
	fake := "#!/bin/sh\nprintf '<?php\\n// formatted\\n' > \"$1\"\n"
	require.NoError(t, os.WriteFile("vendor/bin/pint", []byte(fake), 0o755))

	p := filepath.Join(dir, "Controller.php")
	out, err := writeFile{}.Execute(context.Background(), mustJSON(t, map[string]string{
		"path": p, "content": "<?php\n   echo 1;\n", // sloppy, unformatted
	}))
	require.NoError(t, err)
	b, _ := os.ReadFile(p)
	require.Equal(t, "<?php\n// formatted\n", string(b)) // the project's pint ran
	require.Contains(t, out, "auto-formatted")
}

// The `format` override (settings.json `format` / BORG_FORMAT_CMD) wins over
// detection and runs for ANY language — the Claude-hook-style escape hatch, so we
// never have to enumerate every formatter.
func TestAutoFormatOverrideCommand(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "weird.xyz") // a type with no built-in detection
	require.NoError(t, os.WriteFile(p, []byte("messy\n"), 0o644))
	t.Setenv("BORG_FORMAT_CMD", "printf 'tidy\\n' > {file}")

	require.True(t, autoFormat(context.Background(), p))
	b, _ := os.ReadFile(p)
	require.Equal(t, "tidy\n", string(b))
}

func TestDetectFormatterEvidenceGating(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// No evidence and no project-local binary → no formatter (don't impose one).
	_, _, ok := detectFormatter("Controller.php")
	require.False(t, ok)

	// A project-local pint is itself evidence → detected.
	require.NoError(t, os.MkdirAll("vendor/bin", 0o755))
	require.NoError(t, os.WriteFile("vendor/bin/pint", []byte("#!/bin/sh\n"), 0o755))
	bin, _, ok := detectFormatter("Controller.php")
	require.True(t, ok)
	require.Contains(t, bin, "vendor/bin/pint")

	// Rust: gated on Cargo.toml (+ rustfmt presence); without Cargo.toml → no.
	_, _, ok = detectFormatter("main.rs")
	require.False(t, ok)
}

func TestPrettierConfigured(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.False(t, prettierConfigured())

	require.NoError(t, os.WriteFile(".prettierrc", []byte("{}"), 0o644))
	require.True(t, prettierConfigured())

	require.NoError(t, os.Remove(".prettierrc"))
	require.NoError(t, os.WriteFile("package.json", []byte(`{"devDependencies":{"prettier":"^3"}}`), 0o644))
	require.True(t, prettierConfigured())
}

func TestAutoFormatFormatterFailureKeepsContent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, os.MkdirAll("vendor/bin", 0o755))
	// A pint that always fails (exit 1) must NOT change the file.
	require.NoError(t, os.WriteFile("vendor/bin/pint", []byte("#!/bin/sh\nexit 1\n"), 0o755))
	p := filepath.Join(dir, "x.php")
	require.NoError(t, os.WriteFile(p, []byte("<?php   echo 1;"), 0o644))
	require.False(t, autoFormat(context.Background(), p))
	b, _ := os.ReadFile(p)
	require.Equal(t, "<?php   echo 1;", string(b)) // untouched
}

func TestResolveBinPrefersLocal(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, os.MkdirAll("node_modules/.bin", 0o755))
	require.NoError(t, os.WriteFile("node_modules/.bin/prettier", []byte("#!/bin/sh\n"), 0o755))
	got := resolveBin("node_modules/.bin/prettier", "prettier")
	require.Contains(t, got, "node_modules/.bin/prettier")
	require.True(t, filepath.IsAbs(got))

	// Neither local nor a PATH binary that exists → "".
	require.Empty(t, resolveBin("does/not/exist", "definitely-not-a-real-binary-xyz"))
}

func TestAutoFormatNoOpForUnknownType(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // no project markers here → detection finds nothing
	p := filepath.Join(dir, "notes.txt")
	content := "a\n  b\n\tc\n" // mixed whitespace must be preserved verbatim
	out, err := writeFile{}.Execute(context.Background(), mustJSON(t, map[string]string{"path": p, "content": content}))
	require.NoError(t, err)
	b, _ := os.ReadFile(p)
	require.Equal(t, content, string(b))
	require.NotContains(t, out, "formatted")

	_, _, ok := detectFormatter("notes.txt")
	require.False(t, ok) // unknown type → no formatter
	require.False(t, autoFormat(context.Background(), p))
}

func TestVerify(t *testing.T) {
	ctx := context.Background()

	// Nothing detectable → a clear deferral to bash.
	t.Chdir(t.TempDir())
	out, err := verifyTool{}.Execute(ctx, nil)
	require.NoError(t, err)
	require.Contains(t, out, "no safe compile check")

	// A compiling Go module → PASS.
	good := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(good, "go.mod"), []byte("module ex\n\ngo 1.26\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(good, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644))
	t.Chdir(good)
	out, err = verifyTool{}.Execute(ctx, nil)
	require.NoError(t, err)
	require.Contains(t, out, "PASS")

	// A broken edit → FAIL with the compiler error.
	require.NoError(t, os.WriteFile(filepath.Join(good, "main.go"), []byte("package main\n\nfunc main() { undefinedFn() }\n"), 0o644))
	out, err = verifyTool{}.Execute(ctx, nil)
	require.NoError(t, err)
	require.Contains(t, out, "FAIL")
}

func TestMutatingFlags(t *testing.T) {
	require.False(t, readFile{}.Mutating())
	require.False(t, listDir{}.Mutating())
	require.False(t, grepTool{}.Mutating())
	require.False(t, verifyTool{}.Mutating()) // compile-only, no permission prompt
	require.True(t, writeFile{}.Mutating())
	require.True(t, editFile{}.Mutating())
	require.True(t, editLines{}.Mutating())
	require.True(t, bashTool{}.Mutating())
}

func TestBashTool(t *testing.T) {
	ctx := context.Background()
	out, err := bashTool{}.Execute(ctx, mustJSON(t, map[string]string{"command": "echo hello"}))
	require.NoError(t, err)
	require.Contains(t, out, "hello")

	// Non-zero exit is reported, not failed.
	out, err = bashTool{}.Execute(ctx, mustJSON(t, map[string]string{"command": "exit 3"}))
	require.NoError(t, err)
	require.Contains(t, out, "exit")

	_, err = bashTool{}.Execute(ctx, mustJSON(t, map[string]string{"command": ""}))
	require.Error(t, err)
}

// A `git commit -m "…"` whose double-quoted message contains an active backtick or
// $( is shell command substitution (bash would execute it) — the bash tool must
// refuse it with an actionable heredoc fix instead of running it. Safe forms
// (single-quoted, escaped, or the -F - heredoc) must pass through untouched.
func TestBashBlocksUnsafeGitCommitMessage(t *testing.T) {
	unsafe := []string{
		`git add . && git commit -m "feat: x` + "`borg learn`" + ` y"`, // the real incident
		`git commit -m "msg with $(rm -rf /tmp/x)"`,                    // $( ) substitution
		`git commit -am "wip ` + "`whoami`" + `"`,                      // -am flag form
		`git commit --message="oops ` + "`id`" + `"`,                   // --message= form
	}
	for _, cmd := range unsafe {
		require.NotEmpty(t, unsafeGitCommitMessage(cmd), "should flag: %s", cmd)
		out, err := bashTool{}.Execute(context.Background(), mustJSON(t, map[string]string{"command": cmd}))
		require.NoError(t, err)
		require.Contains(t, out, "git commit -F - <<'EOF'", "block message must teach the heredoc fix")
	}

	safe := []string{
		`git commit -m "plain subject line"`,                       // no metachars
		`git commit -m 'literal ` + "`borg learn`" + ` in quotes'`, // single-quoted = no expansion
		"git commit -F - <<'EOF'\nfeat: x `borg learn`\nEOF",       // the safe heredoc form
		`git commit -m "escaped \` + "`" + `tick\` + "`" + `"`,     // escaped backticks are literal
		"echo `date`", // legitimate substitution, not a commit
	}
	for _, cmd := range safe {
		require.Empty(t, unsafeGitCommitMessage(cmd), "should NOT flag: %s", cmd)
	}
}

func TestBashBlocksSelfKill(t *testing.T) {
	unsafe := []string{
		`pkill borg`,
		`pkill -f borg`,
		`killall -9 turborg`,
		`pkill borg 2>/dev/null; sleep 1; cp bin/borg ~/go/bin/borg`, // the real incident
		`sudo killall borg`,
		`kill $(pgrep borg)`,
	}
	for _, cmd := range unsafe {
		require.NotEmpty(t, unsafeSelfKill(cmd), "should block: %s", cmd)
		out, err := bashTool{}.Execute(context.Background(), mustJSON(t, map[string]string{"command": cmd}))
		require.NoError(t, err)
		require.Contains(t, out, "blocked", "must refuse: %s", cmd)
		require.Contains(t, out, "install -m755", "must teach the atomic-install fix")
	}

	safe := []string{
		`pkill node`,                // unrelated process
		`kill 12345`,                // a PID, not borg
		`echo "borg is great"`,      // mentions borg but doesn't kill it
		`grep pkill borg-notes.txt`, // the word pkill, not the command
		`killall cyborg`,            // contains "borg" but not as a whole word
	}
	for _, cmd := range safe {
		require.Empty(t, unsafeSelfKill(cmd), "should NOT block: %s", cmd)
	}
}

func TestEditFileReplaceAllWhitespaceMismatchSuggestsEditLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ws.go")
	// Tab-indented source; the model pastes spaces. replace_all skips the single-range
	// whitespace-tolerant apply (it needs exact, possibly-multiple matches), so a
	// whitespace-only mismatch still routes to the precise edit_lines suggestion.
	require.NoError(t, os.WriteFile(path, []byte("func f() {\n\tx := 1\n\treturn x\n}\n"), 0o644))

	_, err := editFile{}.Execute(context.Background(), mustJSON(t, map[string]any{
		"path": path, "old_string": "    return x", "new_string": "    return x + 1", "replace_all": true,
	}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "edit_lines")
	require.Contains(t, err.Error(), "lines 3-3")
}

func TestEditFileAmbiguousWhitespaceNotGuessed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ws.go")
	// "return x" appears twice ignoring indentation (both tab-indented) — an ambiguous
	// flexible match, so the fallback must NOT guess a location; it asks the model to
	// re-read. The space-indented old_string has no exact substring match in either line.
	require.NoError(t, os.WriteFile(path, []byte("func a() {\n\treturn x\n}\nfunc b() {\n\treturn x\n}\n"), 0o644))

	_, err := editFile{}.Execute(context.Background(), mustJSON(t, map[string]string{
		"path": path, "old_string": "    return x", "new_string": "    return y",
	}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "whitespace")
}

func TestWriteFileOverwriteShowsDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")

	// Create: no diff (the model already has the content).
	out, err := writeFile{}.Execute(context.Background(), mustJSON(t, map[string]string{"path": path, "content": "a\nb\nc\n"}))
	require.NoError(t, err)
	require.Contains(t, out, "created")
	require.NotContains(t, out, "@@")

	// Overwrite: a diff of what changed.
	out, err = writeFile{}.Execute(context.Background(), mustJSON(t, map[string]string{"path": path, "content": "a\nB\nc\n"}))
	require.NoError(t, err)
	require.Contains(t, out, "overwrote")
	require.Contains(t, out, "-b")
	require.Contains(t, out, "+B")
}

func TestUnifiedDiff(t *testing.T) {
	// A single-line change shows context + the -/+ pair, not the whole file.
	d := unifiedDiff("a\nb\nc\nd\ne\n", "a\nb\nX\nd\ne\n", "f.txt")
	require.Contains(t, d, "--- f.txt")
	require.Contains(t, d, "-c")
	require.Contains(t, d, "+X")
	require.Contains(t, d, " b") // context line
	require.NotContains(t, d, "+a")

	// No change → empty diff.
	require.Empty(t, unifiedDiff("same\n", "same\n", "f.txt"))

	// A huge rewrite is capped, not dumped whole.
	var big strings.Builder
	for i := 0; i < 500; i++ {
		big.WriteString("line\n")
	}
	d = unifiedDiff("old\n", big.String(), "f.txt")
	require.LessOrEqual(t, len(strings.Split(d, "\n")), maxDiffLines+5)
	require.Contains(t, d, "more diff lines")
}

func TestGrepTool(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("alpha\nneedle here\nbeta"), 0o644))
	ctx := context.Background()

	out, err := grepTool{}.Execute(ctx, mustJSON(t, map[string]string{"pattern": "needle", "path": dir}))
	require.NoError(t, err)
	require.Contains(t, out, "needle")

	out, err = grepTool{}.Execute(ctx, mustJSON(t, map[string]string{"pattern": "zzzznope", "path": dir}))
	require.NoError(t, err)
	require.Contains(t, out, "no matches")

	_, err = grepTool{}.Execute(ctx, mustJSON(t, map[string]string{"pattern": ""}))
	require.Error(t, err)
}

// bash_output BLOCKS until the command finishes (or wait_seconds elapses),
// returning the moment it completes — so one call replaces a spin of polls. A
// wait_seconds:0 peek returns immediately even mid-run.
func TestBashOutputBlocksUntilDone(t *testing.T) {
	bg := newBGManager()
	bt := bashTool{bg: bg}
	bo := bashOutputTool{bg: bg}
	ctx := context.Background()

	_, err := bt.Execute(ctx, mustJSON(t, map[string]any{"command": "sleep 0.3; echo hi", "run_in_background": true}))
	require.NoError(t, err)

	// ONE blocking call waits for completion and returns it.
	start := time.Now()
	out, err := bo.Execute(ctx, mustJSON(t, map[string]any{"shell_id": "bash_1", "wait_seconds": 5}))
	require.NoError(t, err)
	require.Contains(t, out, "completed")
	require.Contains(t, out, "hi")
	require.GreaterOrEqual(t, time.Since(start), 250*time.Millisecond) // it actually waited, didn't return empty

	// wait_seconds:0 is a non-blocking peek — returns [running] immediately mid-run.
	_, err = bt.Execute(ctx, mustJSON(t, map[string]any{"command": "sleep 30", "run_in_background": true}))
	require.NoError(t, err)
	out, err = bo.Execute(ctx, mustJSON(t, map[string]any{"shell_id": "bash_2", "wait_seconds": 0}))
	require.NoError(t, err)
	require.Contains(t, out, "running")

	// Clean up the long sleep so goleak stays happy.
	require.True(t, bg.kill("bash_2"))
	require.Eventually(t, func() bool {
		o, _ := bo.Execute(ctx, mustJSON(t, map[string]any{"shell_id": "bash_2", "wait_seconds": 0}))
		return !strings.Contains(o, "[running]")
	}, 3*time.Second, 20*time.Millisecond)
}

// grep must use EXTENDED regex so `a|b` alternation works. Plain grep is BRE,
// where `|` is a literal pipe — so `usage|status` silently matched nothing,
// which made the agent loop thrash for ~1M tokens trying different patterns that
// could never match. This is the deterministic guard for that regression.
func TestGrepAlternationMatches(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"),
		[]byte("has usage here\nhas status here\nneither\n"), 0o644))
	ctx := context.Background()

	out, err := grepTool{}.Execute(ctx, mustJSON(t, map[string]string{"pattern": "usage|status", "path": dir}))
	require.NoError(t, err)
	require.Contains(t, out, "usage", "ERE alternation must match the first branch")
	require.Contains(t, out, "status", "ERE alternation must match the second branch")
	require.NotContains(t, out, "no matches")

	// A literal pipe must NOT be matched as text (proves we're in ERE, not searching
	// for the literal string "usage|status").
	out, err = grepTool{}.Execute(ctx, mustJSON(t, map[string]string{"pattern": "usage|nope", "path": dir}))
	require.NoError(t, err)
	require.Contains(t, out, "usage")

	// Other ERE metacharacters work too (quantifier + group, no backslash).
	out, err = grepTool{}.Execute(ctx, mustJSON(t, map[string]string{"pattern": "(usage|status) here", "path": dir}))
	require.NoError(t, err)
	require.Contains(t, out, "usage here")
}

// grep must skip .git/node_modules/vendor (like glob) — searching them wastes
// time and floods the model with irrelevant matches.
func TestGrepExcludesNoiseDirs(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "real.txt"), []byte("needle in source"), 0o644))
	for _, noise := range []string{".git", "node_modules", "vendor"} {
		require.NoError(t, os.Mkdir(filepath.Join(dir, noise), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, noise, "junk.txt"), []byte("needle in noise"), 0o644))
	}

	out, err := grepTool{}.Execute(context.Background(), mustJSON(t, map[string]string{"pattern": "needle", "path": dir}))
	require.NoError(t, err)
	require.Contains(t, out, "real.txt") // source match kept
	require.NotContains(t, out, ".git")  // noise dirs excluded
	require.NotContains(t, out, "node_modules")
	require.NotContains(t, out, "vendor")
}

func TestListDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))

	out, err := listDir{}.Execute(context.Background(), mustJSON(t, map[string]string{"path": dir}))
	require.NoError(t, err)
	require.Contains(t, out, "a.txt")
	require.Contains(t, out, "sub/")
}

func TestReadFileEmptyAndBinary(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Empty file -> a sentinel, never "" (which would serialize away and 422).
	empty := filepath.Join(dir, "empty.txt")
	require.NoError(t, os.WriteFile(empty, nil, 0o644))
	out, err := readFile{}.Execute(ctx, mustJSON(t, map[string]string{"path": empty}))
	require.NoError(t, err)
	require.Equal(t, "(empty file)", out)

	// Non-UTF-8 bytes -> a clear error (the tool promises text).
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.WriteFile(bin, []byte{0xff, 0xfe, 0x00, 0x01}, 0o644))
	_, err = readFile{}.Execute(ctx, mustJSON(t, map[string]string{"path": bin}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "UTF-8")
}

func TestListDirEmpty(t *testing.T) {
	out, err := listDir{}.Execute(context.Background(), mustJSON(t, map[string]string{"path": t.TempDir()}))
	require.NoError(t, err)
	require.Equal(t, "(empty directory)", out)
}

func TestReadFileMissing(t *testing.T) {
	_, err := readFile{}.Execute(context.Background(), mustJSON(t, map[string]string{"path": filepath.Join(t.TempDir(), "nope")}))
	require.Error(t, err)
}

func TestEditFileNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	require.NoError(t, os.WriteFile(path, []byte("abc"), 0o644))

	_, err := editFile{}.Execute(context.Background(), mustJSON(t, map[string]string{
		"path": path, "old_string": "zzz", "new_string": "y",
	}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestWriteFileCreatesNestedDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "c.txt")
	_, err := writeFile{}.Execute(context.Background(), mustJSON(t, map[string]string{"path": path, "content": "hi"}))
	require.NoError(t, err)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "hi", string(b))
}

func TestRegistryGet(t *testing.T) {
	r := DefaultRegistry()
	tool, ok := r.Get("read_file")
	require.True(t, ok)
	require.Equal(t, "read_file", tool.Name())

	_, ok = r.Get("does_not_exist")
	require.False(t, ok)
}

func TestToolsRejectBadArgs(t *testing.T) {
	ctx := context.Background()
	bad := json.RawMessage(`{not json`)
	for _, tool := range []Tool{readFile{}, writeFile{}, editFile{}, bashTool{}, grepTool{}} {
		_, err := tool.Execute(ctx, bad)
		require.Errorf(t, err, "tool %s should reject bad args", tool.Name())
	}
}

func TestTruncate(t *testing.T) {
	require.Equal(t, "abc", truncate("abc", 10))
	out := truncate("abcdef", 3)
	require.Contains(t, out, "truncated")
	require.True(t, len(out) > 3)
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func TestWriteEditBoundary(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "ok.txt")
	outside := filepath.Join(t.TempDir(), "bad.txt")
	ctx := WithRoot(context.Background(), root)

	// write_file: inside the root succeeds, outside is refused.
	_, err := writeFile{}.Execute(ctx, mustJSON(t, map[string]string{"path": inside, "content": "x"}))
	require.NoError(t, err)
	_, err = writeFile{}.Execute(ctx, mustJSON(t, map[string]string{"path": outside, "content": "x"}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "outside the trusted")

	// edit_file is guarded the same way.
	_, err = editFile{}.Execute(ctx, mustJSON(t, map[string]string{
		"path": outside, "old_string": "a", "new_string": "b",
	}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "outside the trusted")

	// No root in context → unrestricted (boundary off).
	_, err = writeFile{}.Execute(context.Background(), mustJSON(t, map[string]string{"path": outside, "content": "x"}))
	require.NoError(t, err)
}

func TestReadFileRange(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	require.NoError(t, os.WriteFile(p, []byte("l1\nl2\nl3\nl4\nl5"), 0o644))

	// offset+limit returns just that line range, numbered, with a note.
	out, err := readFile{}.Execute(context.Background(), mustJSON(t, map[string]any{"path": p, "offset": 2, "limit": 2}))
	require.NoError(t, err)
	require.Contains(t, out, "2\tl2") // numbered with the real line numbers
	require.Contains(t, out, "3\tl3")
	require.NotContains(t, out, "l1")
	require.NotContains(t, out, "l4")
	require.Contains(t, out, "lines 2-3 of 5")

	// offset past the end is reported, not an error.
	out, err = readFile{}.Execute(context.Background(), mustJSON(t, map[string]any{"path": p, "offset": 99}))
	require.NoError(t, err)
	require.Contains(t, out, "past end")

	// No offset/limit returns the whole file, footered with its total line count.
	out, err = readFile{}.Execute(context.Background(), mustJSON(t, map[string]any{"path": p}))
	require.NoError(t, err)
	require.Contains(t, out, "l1")
	require.Contains(t, out, "l5")
	require.Contains(t, out, "[5 lines]")
}

func TestReadFileTruncatesHuge(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.txt")
	big := make([]byte, 200<<10) // > the 96KB cap
	for i := range big {
		big[i] = 'a'
	}
	require.NoError(t, os.WriteFile(p, big, 0o644))

	out, err := readFile{}.Execute(context.Background(), mustJSON(t, map[string]string{"path": p}))
	require.NoError(t, err)
	require.Contains(t, out, "truncated")
	require.Contains(t, out, "offset/limit") // tells the model how to read more
}

// The default tool order must stay deterministic: the tools array is part of the
// prompt prefix, so a nondeterministic order (e.g. ranging a map) would break
// the backend's automatic prompt caching.
func TestGlobFindsFilesByPattern(t *testing.T) {
	dir := t.TempDir()
	mk := func(rel string) {
		p := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	}
	mk("a.go")
	mk("sub/b.go")
	mk("sub/c.txt")
	mk(".git/ignored.go") // .git is skipped

	run := func(pattern string) string {
		out, err := globTool{}.Execute(context.Background(),
			mustJSON(t, map[string]string{"pattern": pattern, "path": dir}))
		require.NoError(t, err)
		return out
	}

	// '**/*.go' matches at any depth (but not inside .git, and not .txt).
	rec := run("**/*.go")
	require.Contains(t, rec, "a.go")
	require.Contains(t, rec, "sub/b.go")
	require.NotContains(t, rec, "ignored.go")
	require.NotContains(t, rec, "c.txt")

	// '*' stays within one segment: only the top-level .go file matches.
	top := run("*.go")
	require.Contains(t, top, "a.go")
	require.NotContains(t, top, "b.go")

	require.Equal(t, "(no matches)", run("**/*.rs"))
}

// glob's two load-bearing semantics, pinned so they can't silently drift (the way
// grep's regex flavor did): '**' crosses MULTIPLE directory levels, '*' is confined
// to a single segment, and results come back most-recently-modified FIRST (so the
// model sees the file it's likely working on at the top).
func TestGlobSegmentSemanticsAndRecency(t *testing.T) {
	dir := t.TempDir()
	mk := func(rel string, ageSecs int) {
		p := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
		mt := time.Now().Add(-time.Duration(ageSecs) * time.Second)
		require.NoError(t, os.Chtimes(p, mt, mt))
	}
	mk("old.go", 100)   // oldest
	mk("a/mid.go", 50)  // middle, one level deep
	mk("a/b/new.go", 1) // newest, two levels deep

	run := func(pat string) string {
		out, err := globTool{}.Execute(context.Background(),
			mustJSON(t, map[string]string{"pattern": pat, "path": dir}))
		require.NoError(t, err)
		return out
	}

	// '**' crosses multiple levels: all three .go files match.
	rec := run("**/*.go")
	require.Contains(t, rec, "old.go")
	require.Contains(t, rec, "a/mid.go")
	require.Contains(t, rec, "a/b/new.go")
	// Most-recently-modified first: new (1s) before mid (50s) before old (100s).
	require.Less(t, strings.Index(rec, "new.go"), strings.Index(rec, "mid.go"))
	require.Less(t, strings.Index(rec, "mid.go"), strings.Index(rec, "old.go"))

	// 'a/*/*.go' requires exactly two segments under a/: a/b/new.go matches,
	// a/mid.go (only one segment) does NOT — '*' never crosses a separator.
	seg := run("a/*/*.go")
	require.Contains(t, seg, "a/b/new.go")
	require.NotContains(t, seg, "mid.go")
}

// The load-bearing coupling between read_file and edit_lines: the 1-based "N\t"
// gutter read_file prints IS the coordinate edit_lines acts on. A drift in either
// (numbering off by one, a gutter format change) would make line-targeted edits
// silently hit the wrong line — so pin the round trip end to end.
func TestReadFileLineNumbersDriveEditLines(t *testing.T) {
	dir := t.TempDir()
	// A .txt file so the Go auto-formatter doesn't reshape it — this test isolates
	// the read_file↔edit_lines numbering contract, not formatting.
	p := filepath.Join(dir, "f.txt")
	require.NoError(t, os.WriteFile(p,
		[]byte("alpha\nbeta\ngamma\ndelta\nepsilon\n"), 0o644))
	ctx := context.Background()

	// read_file shows line 4 as "4\tdelta".
	out, err := readFile{}.Execute(ctx, mustJSON(t, map[string]string{"path": p}))
	require.NoError(t, err)
	require.Contains(t, out, "4\tdelta")

	// Feeding THAT number to edit_lines edits exactly that line — nothing else moves.
	_, err = editLines{}.Execute(ctx, mustJSON(t, map[string]any{
		"path": p, "start_line": 4, "end_line": 4, "new_text": "DELTA"}))
	require.NoError(t, err)
	b, _ := os.ReadFile(p)
	require.Equal(t, "alpha\nbeta\ngamma\nDELTA\nepsilon\n", string(b))
}

func TestBackgroundBashOutputAndKill(t *testing.T) {
	bg := newBGManager()
	bt := bashTool{bg: bg}
	bo := bashOutputTool{bg: bg}
	kt := killShellTool{bg: bg}
	ctx := context.Background()

	// Start a quick command in the background — returns a shell id immediately.
	start, err := bt.Execute(ctx, mustJSON(t, map[string]any{"command": "printf 'hi\\n'", "run_in_background": true}))
	require.NoError(t, err)
	require.Contains(t, start, "bash_1")

	// Poll bash_output (it drains per call) until the shell completes.
	var acc string
	require.Eventually(t, func() bool {
		out, _ := bo.Execute(ctx, mustJSON(t, map[string]string{"shell_id": "bash_1"}))
		acc += out
		return strings.Contains(acc, "completed")
	}, 3*time.Second, 20*time.Millisecond)
	require.Contains(t, acc, "hi")

	// An unknown shell id is an error.
	_, err = bo.Execute(ctx, mustJSON(t, map[string]string{"shell_id": "nope"}))
	require.Error(t, err)

	// A long-running shell can be killed; afterward it's no longer "running".
	startK, err := bt.Execute(ctx, mustJSON(t, map[string]any{"command": "sleep 30", "run_in_background": true}))
	require.NoError(t, err)
	require.Contains(t, startK, "bash_2")
	killed, err := kt.Execute(ctx, mustJSON(t, map[string]string{"shell_id": "bash_2"}))
	require.NoError(t, err)
	require.Contains(t, killed, "Killed")
	require.Eventually(t, func() bool {
		out, _ := bo.Execute(ctx, mustJSON(t, map[string]string{"shell_id": "bash_2"}))
		return !strings.Contains(out, "[running]")
	}, 3*time.Second, 20*time.Millisecond)

	_, err = kt.Execute(ctx, mustJSON(t, map[string]string{"shell_id": "nope"}))
	require.Error(t, err)
}

func TestDefaultRegistryStableOrder(t *testing.T) {
	want := []string{"read_file", "list_dir", "grep", "glob", "write_file", "edit_file", "edit_lines", "verify", "bash", "bash_output", "kill_shell", "ask_user", "finish"}
	var got []string
	for _, d := range DefaultRegistry().Definitions() {
		got = append(got, d.Function.Name)
	}
	require.Equal(t, want, got)
}

// A foreground bash command that hangs is killed at bashTimeout, with a clear,
// actionable message (not a mysterious "signal: killed").
func TestBashForegroundTimeout(t *testing.T) {
	old := bashTimeout
	bashTimeout = 50 * time.Millisecond
	defer func() { bashTimeout = old }()

	out, err := bashTool{bg: newBGManager()}.Execute(context.Background(),
		mustJSON(t, map[string]string{"command": "sleep 5"}))
	require.NoError(t, err) // a timeout is reported as content, not a hard error
	require.Contains(t, out, "timed out")
	require.Contains(t, out, "run_in_background")
}
