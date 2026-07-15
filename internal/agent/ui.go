package agent

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Stats summarizes one assistant turn for the activity footer (token usage +
// wall-clock), the way Claude/Gemini/OpenAI CLIs report what a turn cost.
type Stats struct {
	InTokens     int
	OutTokens    int
	CachedTokens int // input tokens served from the prompt cache (prefix reuse)
	Elapsed      time.Duration
}

// Line renders the footer, or "" when there's nothing to report (no usage).
func (s Stats) Line() string {
	if s.InTokens == 0 && s.OutTokens == 0 {
		return ""
	}
	secs := s.Elapsed.Seconds()
	rate := ""
	if secs > 0 && s.OutTokens > 0 {
		rate = fmt.Sprintf(" · %.0f tok/s", float64(s.OutTokens)/secs)
	}
	cached := ""
	if s.CachedTokens > 0 {
		cached = fmt.Sprintf(" (%d cached)", s.CachedTokens)
	}
	return fmt.Sprintf("· %d in%s / %d out tokens · %.1fs%s", s.InTokens, cached, s.OutTokens, secs, rate)
}

// Decision is the outcome of a tool-permission prompt.
type Decision int

const (
	DenyOnce Decision = iota
	AllowOnce
	AllowAlways
)

// AskOption is one choice offered by the ask_user tool: a short label the user
// selects and a one-line description of what it means.
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// AskRequest is a single multiple-choice question the agent puts to the user via
// the ask_user tool. The UI renders it (a modal in the REPL, a numbered list in
// one-shot mode) and returns an AskResult.
type AskRequest struct {
	Question string      `json:"question"`
	Options  []AskOption `json:"options"`
}

// AskResult is the user's response to an ask_user prompt. The UI always offers a
// "something else" escape hatch alongside the listed options, so the user can pick
// one OR answer in their own words (to refine, combine, or override them):
//   - picked a listed option → Choice = its label, Freeform = false
//   - typed their own answer  → Choice = that text,  Freeform = true
//   - dismissed (skipped)     → Choice = "",         Freeform = false
type AskResult struct {
	Choice   string
	Freeform bool
}

// UI renders the agent's work and asks for tool permissions. Implementations
// range from plain stdout (one-shot `borg "task"`) to the styled REPL.
type UI interface {
	// ThinkingStart signals a model turn has begun, before any output — a TTY UI
	// shows a working indicator so a long silent stretch (the model thinking, or
	// streaming a big tool-call argument) doesn't look frozen.
	ThinkingStart()
	// Delta receives a streamed chunk of the assistant's reply.
	Delta(string)
	// AssistantEnd marks the end of a reply (emit a trailing newline if shown)
	// and reports the turn's token/timing stats.
	AssistantEnd(Stats)
	// ToolBatch announces that the next n tool calls are an independent read-only
	// batch run CONCURRENTLY (in parallel), not one at a time — so the UI can make
	// the parallelism visible instead of it looking like sequential calls.
	ToolBatch(n int)
	// ToolCall announces a tool about to run, with its raw JSON arguments.
	ToolCall(name, args string)
	// ToolResult reports the outcome after a tool ran: ok=false on error/denial,
	// with a one-line preview of the output (or error) for display.
	ToolResult(name string, ok bool, summary string)
	// ToolDiff reports a unified diff produced by a successful edit tool, so the UI
	// can render a colored "what changed" preview. Empty diffs are not sent.
	ToolDiff(diff string)
	// Permit asks the user whether to run a mutating tool.
	Permit(toolName string) Decision
	// AskUser puts a multiple-choice question to the user (the ask_user tool) and
	// blocks until they answer, returning their pick or free-text response (or a
	// zero AskResult if they dismissed it — the agent then proceeds autonomously).
	AskUser(req AskRequest) AskResult
	// Debug emits a verbose diagnostic line (full tool args/results, per-step
	// request trace, model reasoning, raw HTTP) when debug mode is on. The agent
	// only calls it when debug is enabled; UIs render it dimmed / on stderr.
	Debug(string)
}

// plainUI is the default unstyled UI used by one-shot `borg "task"`: the answer
// goes to out (stdout) and tool chatter to errw (stderr), keeping piped output
// clean.
type plainUI struct {
	out   io.Writer
	errw  io.Writer
	in    *bufio.Reader
	wrote bool
}

func newPlainUI() *plainUI {
	return &plainUI{out: os.Stdout, errw: os.Stderr, in: bufio.NewReader(os.Stdin)}
}

// ThinkingStart is a no-op for the plain UI: one-shot output is meant to be
// clean/pipeable, so no animated indicator.
func (u *plainUI) ThinkingStart() {}

func (u *plainUI) Delta(s string) {
	u.wrote = true
	fmt.Fprint(u.out, s)
}

func (u *plainUI) AssistantEnd(s Stats) {
	if u.wrote {
		fmt.Fprintln(u.out)
	}
	u.wrote = false
	if line := s.Line(); line != "" {
		fmt.Fprintf(u.errw, "  %s\n", line)
	}
}

func (u *plainUI) ToolBatch(n int) {
	fmt.Fprintf(u.errw, "⚡ %d tools in parallel\n", n)
}

func (u *plainUI) ToolCall(name, args string) {
	fmt.Fprintf(u.errw, "⚙ %s\n", ToolCallLine(name, args))
}

func (u *plainUI) ToolResult(_ string, ok bool, summary string) {
	if ok {
		fmt.Fprintf(u.errw, "  ✓ %s\n", summary)
	} else {
		fmt.Fprintf(u.errw, "  ✗ %s\n", summary)
	}
}

func (u *plainUI) ToolDiff(diff string) {
	for _, ln := range strings.Split(strings.TrimRight(diff, "\n"), "\n") {
		fmt.Fprintf(u.errw, "  %s\n", ln)
	}
}

func (u *plainUI) Debug(s string) {
	for _, ln := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		fmt.Fprintf(u.errw, "  · %s\n", ln)
	}
}

func (u *plainUI) Permit(name string) Decision {
	fmt.Fprintf(u.errw, "   allow %s? [y]es / [n]o / [a]lways: ", name)
	line, err := u.in.ReadString('\n')
	if err != nil {
		return DenyOnce
	}
	return decide(line)
}

// AskUser renders the question + numbered options to stderr (keeping stdout
// pipe-clean) and reads a response from stdin: a number picks that option, any
// other non-empty text is taken as the user's own answer (Freeform), and a blank
// line or EOF (a non-interactive pipe) dismisses — so a piped one-shot run never
// blocks waiting for input it can't get.
func (u *plainUI) AskUser(req AskRequest) AskResult {
	fmt.Fprintf(u.errw, "\n%s\n", req.Question)
	for i, o := range req.Options {
		if o.Description != "" {
			fmt.Fprintf(u.errw, "  %d) %s — %s\n", i+1, o.Label, o.Description)
		} else {
			fmt.Fprintf(u.errw, "  %d) %s\n", i+1, o.Label)
		}
	}
	fmt.Fprintf(u.errw, "  pick [1-%d], or type your own answer (Enter to skip): ", len(req.Options))
	line, err := u.in.ReadString('\n')
	text := strings.TrimSpace(line)
	if text == "" { // blank line or EOF → dismissed
		_ = err
		return AskResult{}
	}
	if n, e := strconv.Atoi(text); e == nil && n >= 1 && n <= len(req.Options) {
		return AskResult{Choice: req.Options[n-1].Label}
	}
	return AskResult{Choice: text, Freeform: true} // their own words
}

// decide maps a y/n/a answer to a Decision.
func decide(line string) Decision {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "a", "always":
		return AllowAlways
	case "y", "yes":
		return AllowOnce
	default:
		return DenyOnce
	}
}
