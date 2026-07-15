package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/turborg/borg/internal/llm"
)

// modelContextWindows is the OFFLINE FALLBACK for a model's maximum input
// context window (tokens) — used only until the live catalog is fetched (or when
// it's unreachable). The server's `max_input_tokens` is authoritative (see
// SetModelWindows); this table just keeps /context sensible before the first
// /models call. Floko (gemma-class) ~256k; the Chuppa/Axiom (DeepSeek-V4) tiers 1M.
var modelContextWindows = map[string]int{
	"floko":  262_144,
	"chuppa": 1_048_576,
	"axiom":  1_048_576,
}

// defaultContextWindow is the assumed window for an xShellz model not in the
// table and not (yet) in the fetched catalog. Every model on the platform is at
// least this big, so it's a safe floor there.
const defaultContextWindow = 262_144

// byoContextWindow is the assumed window for an unknown model on a
// bring-your-own backend, which serves no catalog for borg to read the real one
// from. It is deliberately CONSERVATIVE: the models people run themselves are
// typically 8k–128k, and guessing high is the dangerous direction — borg would
// keep appending to a conversation the server has already begun truncating from
// the front, silently dropping the system prompt and the task while /context
// happily reported 3% used. Guessing low only costs an early /compact nudge.
// BORG_CONTEXT overrides it when you know your model's real window.
const byoContextWindow = 32_768

// SetModelWindows records each model's context-window cap from the live catalog
// (GET /v1/llm/models), so ContextWindow reports the server-authoritative window
// instead of the hardcoded fallback. The TUI calls this whenever it (re)loads the
// catalog. A nil/empty catalog leaves the current windows untouched.
func (a *Agent) SetModelWindows(models []llm.ModelInfo) {
	w := make(map[string]int, len(models))
	for _, m := range models {
		if m.MaxInputTokens > 0 {
			w[strings.ToLower(m.ID)] = m.MaxInputTokens
		}
	}
	if len(w) > 0 {
		a.modelWindows = w
	}
}

// ContextWindow returns the current model's maximum input context window in
// tokens, in order of authority: an explicit BORG_CONTEXT override, the live
// catalog, the offline codename table, then a per-backend conservative default.
func (a *Agent) ContextWindow() int {
	// An explicit override wins over everything, including the catalog: it exists
	// for the case where borg CAN'T know the answer (a backend that serves no
	// window) and for the case where it's been told wrong.
	if a.cfg.ContextWindow > 0 {
		return a.cfg.ContextWindow
	}
	model := strings.ToLower(a.cfg.Model)
	if n, ok := a.modelWindows[model]; ok && n > 0 {
		return n
	}
	// The codename table describes xShellz's own models. It must not be consulted
	// off-platform: the codenames aren't reserved words, and a local model that
	// happens to share a name would inherit a wildly wrong 1M window.
	if !a.cfg.BringYourOwn() {
		if n, ok := modelContextWindows[model]; ok {
			return n
		}
		return defaultContextWindow
	}
	return byoContextWindow
}

// ContextStats is a snapshot of how much of the model's context window the
// running conversation occupies, with a per-category breakdown — the data
// behind /context and the footer's usage bar.
type ContextStats struct {
	Model    string // current model codename
	Window   int    // the model's max input context window (tokens)
	Used     int    // best estimate of the conversation's current input size
	Exact    bool   // Used came from the proxy's exact prompt-token count (not an estimate)
	Cached   int    // input tokens served from the prompt cache on the last step
	Messages int    // conversation messages, excluding the system prompt

	// Token breakdown (estimated from message content) so the user can see what
	// is taking up the window.
	SystemTokens  int // the system prompt + project context
	MessageTokens int // user + assistant turns (content and tool-call arguments)
	ToolTokens    int // tool-result messages (file reads, command output, …)
}

// Percent returns Used as a 0-100 percentage of the window (0 when the window is
// unknown/zero), clamped to 100.
func (s ContextStats) Percent() int {
	if s.Window <= 0 {
		return 0
	}
	p := s.Used * 100 / s.Window
	if p > 100 {
		return 100
	}
	return p
}

// ContextStats computes the current context-window occupancy and breakdown. The
// total prefers the proxy's exact prompt-token count from the most recent step
// (what was actually billed as input); it falls back to a character-based
// estimate before any model call, and uses whichever is larger so messages
// appended since that call (tool results, the final reply) still count.
func (a *Agent) ContextStats() ContextStats {
	s := ContextStats{
		Model:  a.cfg.Model,
		Window: a.ContextWindow(),
		Cached: a.lastCachedTokens,
	}
	est := 0
	for i, m := range a.messages {
		t := estTextTokens(m.Content)
		for _, tc := range m.ToolCalls {
			t += estTextTokens(tc.Function.Name) + estTextTokens(tc.Function.Arguments)
		}
		est += t
		switch {
		case i == 0 && m.Role == "system":
			s.SystemTokens += t
		case m.Role == "tool":
			s.ToolTokens += t
			s.Messages++
		default:
			s.MessageTokens += t
			s.Messages++
		}
	}
	// Prefer the exact prompt-token count, but never report less than the raw
	// estimate (the conversation grows after the last measured step).
	if a.lastPromptTokens >= est {
		s.Used, s.Exact = a.lastPromptTokens, true
	} else {
		s.Used = est
	}
	return s
}

// estTextTokens is a rough char-based token estimate (~4 chars/token), matching
// the live estimate the REPL shows while streaming. It's only for display — the
// proxy's exact count is authoritative whenever it's available.
func estTextTokens(s string) int { return (len(s) + 3) / 4 }

// CompactResult reports the outcome of compacting the conversation.
type CompactResult struct {
	BeforeTokens int    // context size (tokens) before compaction
	AfterTokens  int    // estimated context size after compaction
	Summary      string // the recap that replaced the transcript
}

// compactInstruction asks the model to distill the conversation into a recap
// dense enough to continue the work from, losing no load-bearing context.
const compactInstruction = `Summarize the ENTIRE conversation above into a concise but COMPLETE recap that another instance of you could use to continue the work with no loss of important context. Include, as applicable:
- the user's goal(s) and any explicit instructions, preferences, or constraints they gave;
- key facts discovered about the codebase: exact file paths, function/type names, conventions, and how things fit together;
- decisions made and the reasoning behind them;
- changes already applied — which files and what edits — and commands run with their results;
- what is currently in progress and the concrete next steps to finish.
Preserve exact identifiers, paths, and command lines verbatim. Do NOT add pleasantries or say that you are summarizing — output only the recap.`

// compactedPrefix frames the recap as established context when it re-enters the
// conversation in place of the full transcript.
const compactedPrefix = "The earlier conversation was compacted to free up context. The following is a faithful summary of everything that happened so far; treat it as the established context and continue the work from here.\n\n"

// Compact summarizes the conversation so far into a single recap message and
// replaces the full transcript with it (keeping the system prompt), freeing up
// the context window — the explicit, user-invoked equivalent of Claude Code's
// /compact. It makes one metered LLM call and does NOT mutate the conversation
// unless a usable summary comes back, so a failed call leaves the session intact.
func (a *Agent) Compact(ctx context.Context) (CompactResult, error) {
	// Nothing but the system prompt (and maybe a single turn) isn't worth
	// compacting — and the recap would cost more than it saves.
	if len(a.messages) <= 1 {
		return CompactResult{}, fmt.Errorf("nothing to compact yet")
	}
	before := a.ContextStats().Used

	// One-off request: the current conversation plus the summarize instruction.
	// No tools (we want a plain-text recap, not more tool calls) and reasoning
	// off (a summary doesn't need it; keeps the call cheap).
	req := append(append([]llm.Message(nil), a.messages...),
		llm.Message{Role: "user", Content: compactInstruction})
	reply, err := a.llm.Chat(ctx, req, nil, false, func(string) {}, llm.WithEffort("none"))
	if err != nil {
		return CompactResult{}, err
	}
	summary := ""
	if reply != nil {
		summary = strings.TrimSpace(reply.Content)
	}
	if summary == "" {
		return CompactResult{}, fmt.Errorf("compaction produced no summary; the conversation is unchanged")
	}

	// Replace the transcript with [system, recap]. lastPromptTokens is reset so the
	// next turn re-measures the (now much smaller) context exactly.
	sys := a.messages[0]
	a.messages = []llm.Message{sys, {Role: "user", Content: compactedPrefix + summary}}
	a.lastPromptTokens = 0
	a.lastCachedTokens = 0

	return CompactResult{
		BeforeTokens: before,
		AfterTokens:  a.ContextStats().Used,
		Summary:      summary,
	}, nil
}
