package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"github.com/turborg/borg/internal/config"
)

// hermeticCmd builds an exec.Cmd whose environment is the user's real shell
// environment — stripped of any variable the harness injected from its own settings
// file (see config.SubprocessEnv). Every command the harness runs on the user's
// behalf (a build, a test, the bash tool) goes through here, so the harness's own
// configuration never leaks into the user's tooling and make a command behave
// differently than it would in a plain shell. General to any project — it assumes
// nothing about the command, its language, or the repo.
func hermeticCmd(ctx context.Context, name string, arg ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, arg...)
	cmd.Env = config.SubprocessEnv() // nil → inherit the environment unchanged
	return cmd
}

const maxExecBytes = 64 << 10 // cap command/grep output fed back to the model

// projectVerifyTimeout bounds a project's declared verify command (e.g. a full
// `make docker-test`) — generous, since real test suites are slow, but bounded so
// a hung run can't stall the turn forever.
var projectVerifyTimeout = 5 * time.Minute

// RunCommand runs a shell command (bash -c) with a timeout and returns its
// combined output (byte-capped) and whether it FAILED (non-zero exit). It is the
// runner behind the agent's auto-verify backstop when a project declares its own
// verify command (BORG.md "## Verify", e.g. `make docker-test`) — so the harness
// runs the project's REAL tests, the project's own way, instead of a host `go test`.
func RunCommand(ctx context.Context, command string) (string, bool) {
	runCtx, cancel := context.WithTimeout(ctx, projectVerifyTimeout)
	defer cancel()
	out, err := hermeticCmd(runCtx, "bash", "-c", command).CombinedOutput()
	return truncate(string(out), maxExecBytes), err != nil
}

// bashTimeout bounds a FOREGROUND bash command so a hung process can't stall the
// turn forever (same default as Claude Code's bash tool); long-running work goes
// through run_in_background instead. A var so tests can shrink it.
var bashTimeout = 120 * time.Second

// bash --------------------------------------------------------------------

type bashTool struct{ bg *bgManager }

func (bashTool) Name() string { return "bash" }
func (bashTool) Description() string {
	return "Run a shell command in the working directory and return its combined stdout+stderr. Use for builds, tests, git, etc. Set run_in_background for a long-running command (a dev server, a watch) — it returns a shell id immediately; read its output with bash_output and stop it with kill_shell."
}
func (bashTool) Mutating() bool { return true }
func (bashTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"The shell command to run."},"run_in_background":{"type":"boolean","description":"Run asynchronously and return a shell id instead of blocking (default false)."}},"required":["command"]}`)
}

func (t bashTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Command    string `json:"command"`
		Background bool   `json:"run_in_background"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if p.Command == "" {
		return "", errors.New("command is required")
	}
	// Deterministic footgun guard: a `git commit -m "…"` whose double-quoted
	// message contains an active backtick or $( ) is shell command substitution —
	// bash would EXECUTE it (a literal `borg learn` in the message ran that command
	// and hung the commit for minutes). Refuse it with an actionable fix instead of
	// running it; the model self-corrects to the safe heredoc form. Narrow to the
	// double-quoted commit-message case so legitimate substitution elsewhere (e.g.
	// `echo \`date\``) is untouched. See the system prompt's Shell and git section.
	if msg := unsafeGitCommitMessage(p.Command); msg != "" {
		return msg, nil
	}
	// Self-preservation guard: borg is itself a running `borg`/`turborg` process, so
	// `pkill borg` (the model's "fix" for a "Text file busy" when overwriting the
	// live binary) kills the very session running the agent. Refuse it with the
	// atomic-install alternative instead of executing it.
	if msg := unsafeSelfKill(p.Command); msg != "" {
		return msg, nil
	}

	if p.Background {
		id := t.bg.start(p.Command)
		return fmt.Sprintf("Started background shell %s. Read its output with bash_output (shell_id %q); stop it with kill_shell.", id, id), nil
	}

	runCtx, cancel := context.WithTimeout(ctx, bashTimeout)
	defer cancel()

	cmd := hermeticCmd(runCtx, "bash", "-c", p.Command)
	out, err := cmd.CombinedOutput()
	result := truncate(string(out), maxExecBytes)
	if runCtx.Err() == context.DeadlineExceeded {
		// A hung/long command is killed at the timeout — tell the model clearly and
		// actionably, so it doesn't read "signal: killed" as a mysterious failure.
		if result != "" {
			result += "\n"
		}
		return result + fmt.Sprintf("[timed out after %s and was killed — for a long-running command, re-run it with run_in_background and poll bash_output]", bashTimeout), nil
	}
	if err != nil {
		// Non-zero exit is useful signal for the agent, not a hard failure.
		return result + "\n[exit: " + err.Error() + "]", nil
	}
	if result == "" {
		result = "(no output)"
	}
	return result, nil
}

// gitCommitDoubleQuotedMsgRe matches a `git commit` whose message is passed via a
// DOUBLE-quoted -m/-am/--message argument, capturing the message body (escaped
// chars and embedded newlines included). A single-quoted message is safe (no
// expansion) so it intentionally doesn't match.
var gitCommitDoubleQuotedMsgRe = regexp.MustCompile(`git\s+commit\b[^\n]*?(?:-m|-am|-ma|--message=?)\s*"((?:[^"\\]|\\.)*)"`)

// unsafeGitCommitMessage returns a non-empty, actionable error string when command
// is a `git commit -m "…"` whose double-quoted message contains an UNescaped
// backtick or $( — both of which bash runs as command substitution inside double
// quotes (executing arbitrary commands / hanging the commit). Returns "" for any
// safe command, including single-quoted messages and the `git commit -F -` heredoc
// form. Escaped occurrences (\` or \$) are literal, so they're allowed.
func unsafeGitCommitMessage(command string) string {
	m := gitCommitDoubleQuotedMsgRe.FindStringSubmatch(command)
	if m == nil {
		return ""
	}
	if !hasActiveShellSubstitution(m[1]) {
		return ""
	}
	return "[blocked: this `git commit -m \"…\"` message contains a backtick or $( ), which bash runs as " +
		"command substitution inside double quotes — it can hang the commit or execute arbitrary commands. " +
		"Re-run with a quoted heredoc so the message text is taken literally:\n" +
		"  git commit -F - <<'EOF'\n  <your message, exactly as written>\n  EOF\n" +
		"(The single-quoted <<'EOF' delimiter disables all shell interpretation.)]"
}

// hasActiveShellSubstitution reports whether s contains an unescaped command
// substitution — a backtick or $( — treating a backslash as escaping the next byte
// (so \` and \$ are literal and don't count).
func hasActiveShellSubstitution(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // skip the escaped byte
		case '`':
			return true
		case '$':
			if i+1 < len(s) && s[i+1] == '(' {
				return true
			}
		}
	}
	return false
}

// selfKillRe matches a process-killing command targeting borg/turborg. The kill
// verb must be at a COMMAND position — the start of the line or after a `;`, `&&`,
// `||`, `|`, `(`, or `sudo ` — so it doesn't trip on the words appearing as
// arguments elsewhere (e.g. `grep pkill borg-notes.txt`). The target must be a
// standalone token (preceded by whitespace/`(`, not followed by a word char, `.`,
// or `-`) so `killall cyborg` and `rm borg-notes.txt` don't match. It catches
// `pkill borg`, `killall -9 turborg`, `pkill -f borg`, `sudo killall borg`,
// `kill $(pgrep borg)`, and `pkill borg; cp …`.
var selfKillRe = regexp.MustCompile(`(?:^|[;&|(]|\bsudo\s+)\s*(?:pkill|killall|kill)\b[^;&|\n]*?[\s(](?:borg|turborg)(?:[^\w.-]|$)`)

// unsafeSelfKill returns a non-empty, actionable message when command would kill
// the running borg/turborg process (the agent's own session), and "" otherwise.
// The model reaches for `pkill borg` to clear a "Text file busy" when overwriting
// the live binary; the real fix is an atomic install, which we point it to.
func unsafeSelfKill(command string) string {
	if !selfKillRe.MatchString(command) {
		return ""
	}
	return "[blocked: this would kill the running borg/turborg process — i.e. the session you are running in. " +
		"If you hit \"Text file busy\" overwriting a running binary, do NOT kill it and do NOT `cp` onto it. " +
		"Use an atomic replace that swaps the file's inode, which works while the old binary is still executing:\n" +
		"  install -m755 <src> <dst>   # or: mv <src> <dst>\n" +
		"The new binary takes effect on the next launch; the current session keeps running.]"
}

// background shells -------------------------------------------------------

// lockedBuf is an io.Writer-safe buffer: a background command's goroutine writes
// to it while the agent reads, so access must be serialized.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// drain returns the output accumulated since the last call and clears it, so
// bash_output reports only what's new (capped so one read can't flood context).
func (l *lockedBuf) drain() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := truncate(l.b.String(), maxExecBytes)
	l.b.Reset()
	return s
}

// bgShell is one background command: its live output, completion state, and a
// cancel func to kill it.
type bgShell struct {
	out    lockedBuf
	cancel context.CancelFunc

	mu   sync.Mutex
	done bool
	err  error
}

// bgManager tracks the background shells bash started. One instance is shared by
// the bash / bash_output / kill_shell tools (via DefaultRegistry).
type bgManager struct {
	mu     sync.Mutex
	shells map[string]*bgShell
	seq    int
}

func newBGManager() *bgManager { return &bgManager{shells: map[string]*bgShell{}} }

// start launches command asynchronously and returns its shell id. The command is
// rooted at context.Background() so it outlives the turn that started it (use
// kill_shell to stop it).
func (m *bgManager) start(command string) string {
	ctx, cancel := context.WithCancel(context.Background())
	sh := &bgShell{cancel: cancel}

	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("bash_%d", m.seq)
	m.shells[id] = sh
	m.mu.Unlock()

	go func() {
		cmd := hermeticCmd(ctx, "bash", "-c", command)
		cmd.Stdout = &sh.out
		cmd.Stderr = &sh.out
		err := cmd.Run()
		sh.mu.Lock()
		sh.done, sh.err = true, err
		sh.mu.Unlock()
	}()
	return id
}

// output returns the new output since the last read, whether the shell is done,
// and its exit error; ok is false when the id is unknown.
func (m *bgManager) output(id string) (out string, done bool, runErr error, ok bool) {
	m.mu.Lock()
	sh, found := m.shells[id]
	m.mu.Unlock()
	if !found {
		return "", false, nil, false
	}
	out = sh.out.drain()
	sh.mu.Lock()
	done, runErr = sh.done, sh.err
	sh.mu.Unlock()
	return out, done, runErr, true
}

// bgPollTick is how often waitOutput re-checks a background shell while blocking.
var bgPollTick = 200 * time.Millisecond

// waitOutput BLOCKS until the shell finishes or maxWait elapses, accumulating all
// output since the caller's last read — so one call replaces a spin of immediate
// polls (the "watching an empty pipe" token waste). It returns early the moment the
// shell completes; on timeout it returns whatever ran so far with done=false. A
// maxWait<=0 is a single non-blocking peek (today's behavior). The context cancels
// the wait (Esc/interrupt).
func (m *bgManager) waitOutput(ctx context.Context, id string, maxWait time.Duration) (out string, done bool, runErr error, ok bool) {
	deadline := time.Now().Add(maxWait)
	var buf bytes.Buffer
	for {
		chunk, d, e, found := m.output(id)
		if !found {
			return "", false, nil, false
		}
		buf.WriteString(chunk)
		done, runErr = d, e
		if done || !time.Now().Before(deadline) {
			return truncate(buf.String(), maxExecBytes), done, runErr, true
		}
		select {
		case <-ctx.Done():
			return truncate(buf.String(), maxExecBytes), done, runErr, true
		case <-time.After(bgPollTick):
		}
	}
}

// kill stops a background shell; ok is false when the id is unknown.
func (m *bgManager) kill(id string) bool {
	m.mu.Lock()
	sh, found := m.shells[id]
	m.mu.Unlock()
	if !found {
		return false
	}
	sh.cancel()
	return true
}

// bash_output -------------------------------------------------------------

type bashOutputTool struct{ bg *bgManager }

func (bashOutputTool) Name() string { return "bash_output" }
func (bashOutputTool) Description() string {
	return "Read the output of a background shell started by bash (run_in_background), and whether it has finished. It WAITS (blocks) until the command completes or wait_seconds elapses, returning as soon as it's done — so call it ONCE and let it wait, rather than polling repeatedly. Set wait_seconds:0 for an immediate non-blocking peek."
}
func (bashOutputTool) Mutating() bool { return false }
func (bashOutputTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"shell_id":{"type":"string","description":"The shell id returned by bash run_in_background."},"wait_seconds":{"type":"integer","description":"Max seconds to wait for the command to finish before returning (default 60; 0 = immediate peek). Returns early the moment it completes."}},"required":["shell_id"]}`)
}

// defaultBashOutputWait is how long bash_output blocks for completion when the
// caller doesn't specify wait_seconds — long enough to swallow a typical test/build
// in one call, bounded so an endless command (a dev server) still returns.
const defaultBashOutputWait = 60 * time.Second

func (t bashOutputTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		ShellID     string `json:"shell_id"`
		WaitSeconds *int   `json:"wait_seconds"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	wait := defaultBashOutputWait
	if p.WaitSeconds != nil {
		wait = time.Duration(*p.WaitSeconds) * time.Second
	}
	out, done, runErr, ok := t.bg.waitOutput(ctx, p.ShellID, wait)
	if !ok {
		return "", fmt.Errorf("no background shell %q", p.ShellID)
	}
	status := "running"
	if done {
		status = "completed"
		if runErr != nil {
			status = "exited: " + runErr.Error()
		}
	}
	if out == "" {
		out = "(no new output)"
	}
	return fmt.Sprintf("[%s]\n%s", status, out), nil
}

// kill_shell --------------------------------------------------------------

type killShellTool struct{ bg *bgManager }

func (killShellTool) Name() string { return "kill_shell" }
func (killShellTool) Description() string {
	return "Stop a background shell started by bash (run_in_background)."
}
func (killShellTool) Mutating() bool { return false }
func (killShellTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"shell_id":{"type":"string","description":"The shell id to stop."}},"required":["shell_id"]}`)
}

func (t killShellTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var p struct {
		ShellID string `json:"shell_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if !t.bg.kill(p.ShellID) {
		return "", fmt.Errorf("no background shell %q", p.ShellID)
	}
	return "Killed " + p.ShellID + ".", nil
}

// grep --------------------------------------------------------------------

type grepTool struct{}

func (grepTool) Name() string { return "grep" }
func (grepTool) Description() string {
	return "Search files recursively for an extended regular expression (ERE) and return matching lines with file:line. ERE syntax: use | for alternation (a|b), + ? * for quantifiers, () for groups — no backslash needed."
}
func (grepTool) Mutating() bool { return false }
func (grepTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Extended regular expression (ERE) to search for, e.g. \"foo|bar\" for alternation."},"path":{"type":"string","description":"Path to search; defaults to '.'."}},"required":["pattern"]}`)
}

func (grepTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if p.Pattern == "" {
		return "", errors.New("pattern is required")
	}
	if p.Path == "" {
		p.Path = "."
	}

	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// -rEIn: recursive, EXTENDED regex (so `a|b` alternation, `+`, `?`, `()` work as
	// the model expects — plain grep is BRE, where `|` is a literal pipe and every
	// alternation search silently returns no matches), skip binary, show line
	// numbers; no shell, so the pattern and path are passed as literal args (no
	// injection). Skip the same noise dirs glob does — searching
	// .git/node_modules/vendor wastes time and floods matches.
	cmd := exec.CommandContext(runCtx, "grep", "-rEIn",
		"--exclude-dir=.git", "--exclude-dir=node_modules", "--exclude-dir=vendor",
		"--", p.Pattern, p.Path)
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		// grep exits 1 with no output when there are no matches.
		return "(no matches)", nil
	}
	return truncate(string(out), maxExecBytes), nil
}
