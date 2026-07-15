package agent

// Agent-harness trajectory tests. Where loop_test.go drives the loop through a
// real *llm.Client + an httptest SSE server, these drive it through the LLM
// interface seam with a *scripted* model — a deterministic, token-free stand-in
// that returns an exact sequence of assistant replies. That lets us assert on
// the agent's *trajectory* (which tools it calls, in what order, how it handles
// tool errors and permission denials, and that it can't run away) independent of
// any real model's nondeterminism.
//
// This is the foundation the eval harness builds on: the same scripted model is
// later swapped for a *replayed* model (recorded cassettes) and, in the nightly
// eval suite, the real metered model scored by an objective oracle.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/llm"
)

// multiCall builds one assistant message that requests several tool calls at once.
func multiCall(calls ...llm.ToolCall) llm.Message {
	return llm.Message{Role: "assistant", ToolCalls: calls}
}

func read(id, path string) llm.ToolCall {
	return llm.ToolCall{ID: id, Type: "function",
		Function: llm.ToolCallFunction{Name: "read_file", Arguments: `{"path":"` + path + `"}`}}
}

// --- scripted model -------------------------------------------------------

// scriptedLLM is a deterministic LLM: each Chat call returns the next reply in
// steps. If repeat is set, it keeps returning that reply once steps run out —
// used to simulate a model that never stops (to prove the step cap holds).
type scriptedLLM struct {
	steps  []llm.Message
	repeat *llm.Message
	gen    func(i int) llm.Message // per-call generator (varying replies), when set
	calls  int                     // number of Chat invocations
	seen   [][]llm.Message         // conversation snapshot passed at each step, for assertions
	optLen []int                   // count of ChatOptions passed at each step (leak-retry escalation)
	model  string                  // last model set via SetModel (for tiering assertions)
}

func (s *scriptedLLM) Chat(_ context.Context, msgs []llm.Message, _ []llm.Tool, _ bool, onDelta func(string), opts ...llm.ChatOption) (*llm.Message, error) {
	s.seen = append(s.seen, append([]llm.Message(nil), msgs...))
	s.optLen = append(s.optLen, len(opts))
	var reply llm.Message
	switch {
	case s.gen != nil:
		reply = s.gen(s.calls)
	case s.calls < len(s.steps):
		reply = s.steps[s.calls]
	case s.repeat != nil:
		reply = *s.repeat
	default:
		return nil, fmt.Errorf("scriptedLLM: no scripted reply for step %d", s.calls)
	}
	s.calls++
	if reply.Content != "" && onDelta != nil {
		onDelta(reply.Content) // exercise the streaming path the real client uses
	}
	return &reply, nil
}

func (s *scriptedLLM) Models(context.Context) ([]llm.ModelInfo, error)  { return nil, nil }
func (s *scriptedLLM) Tier(context.Context) (string, error)             { return "", nil }
func (s *scriptedLLM) Usage(context.Context) (*llm.AccountUsage, error) { return nil, nil }
func (s *scriptedLLM) SetEffort(string)                                 {}
func (s *scriptedLLM) SetModel(m string)                                { s.model = m }
func (s *scriptedLLM) SetDebug(func(string))                            {}

// reply builders keep the scripts readable.
func say(text string) llm.Message { return llm.Message{Role: "assistant", Content: text} }

func callTool(id, name, args string) llm.Message {
	return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
		ID: id, Type: "function",
		Function: llm.ToolCallFunction{Name: name, Arguments: args},
	}}}
}

// --- trajectory recorder --------------------------------------------------

// harnessUI records the agent's externally-visible trajectory and applies a
// programmable permission policy (answer), so tests can assert on what the loop
// did and steer the allow/deny branches.
type harnessUI struct {
	mu      sync.Mutex // the agent may call the UI from parallel tool goroutines
	deltas  strings.Builder
	debug   strings.Builder
	calls   []string // tool names, in call order
	permits []string // tools the loop asked permission for, in order
	diffs   []string // unified diffs emitted by edit tools, in order
	batch   int      // size of the last announced parallel batch (0 = none)
	answer  func(tool string) Decision

	asks      []AskRequest               // ask_user questions the loop raised, in order
	askAnswer func(AskRequest) AskResult // response per question (zero = dismissed)
}

func (u *harnessUI) ThinkingStart()     {}
func (u *harnessUI) Delta(s string)     { u.mu.Lock(); defer u.mu.Unlock(); u.deltas.WriteString(s) }
func (u *harnessUI) AssistantEnd(Stats) {}
func (u *harnessUI) ToolCall(name, _ string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, name)
}
func (u *harnessUI) ToolBatch(n int)                 { u.mu.Lock(); defer u.mu.Unlock(); u.batch = n }
func (u *harnessUI) ToolResult(string, bool, string) {}
func (u *harnessUI) ToolDiff(d string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.diffs = append(u.diffs, d)
}
func (u *harnessUI) Permit(name string) Decision {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.permits = append(u.permits, name)
	if u.answer != nil {
		return u.answer(name)
	}
	return AllowOnce
}
func (u *harnessUI) Debug(s string) { u.mu.Lock(); defer u.mu.Unlock(); u.debug.WriteString(s + "\n") }
func (u *harnessUI) AskUser(req AskRequest) AskResult {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.asks = append(u.asks, req)
	if u.askAnswer != nil {
		return u.askAnswer(req)
	}
	return AskResult{}
}

// newHarness wires a scripted model + recording UI into a real Agent. The tool
// registry, permission gating, and message bookkeeping are all the production
// ones — only the model is swapped — so these tests cover the actual loop.
func newHarness(t *testing.T, ui *harnessUI, s *scriptedLLM) *Agent {
	t.Helper()
	a := NewWithLLM(&config.Config{Model: "floko"}, s)
	a.SetUI(ui)
	return a
}

// lastToolResult returns the content of the most recent `tool` message in the
// agent's conversation — i.e. what the model saw as the result of its tool call.
func lastToolResult(a *Agent) string {
	for i := len(a.messages) - 1; i >= 0; i-- {
		if a.messages[i].Role == "tool" {
			return a.messages[i].Content
		}
	}
	return ""
}

// ApplySetting applies a persistent-setting change to a running agent: a "hot"
// setting takes effect immediately, and one that feeds composeSystemPrompt (git
// attribution) rebuilds the system message in place.
func TestApplySettingHotApplies(t *testing.T) {
	cfg := &config.Config{
		Model:               "floko",
		GitAttribution:      true,
		GitAttributionName:  "Turborg",
		GitAttributionEmail: "noreply@turborg.com",
	}
	a := NewWithLLM(cfg, &scriptedLLM{})
	require.Contains(t, a.Messages()[0].Content, "Co-Authored-By") // attribution on

	a.ApplySetting("git_attribution", "false")
	require.NotContains(t, a.Messages()[0].Content, "Co-Authored-By") // rebuilt without it

	a.ApplySetting("git_attribution", "true")
	require.Contains(t, a.Messages()[0].Content, "Co-Authored-By") // and back

	require.False(t, a.Think())
	a.ApplySetting("think", "true")
	require.True(t, a.Think())

	require.NotPanics(t, func() { a.ApplySetting("escalate_model", "axiom") })
}

// Debug mode tees diagnostics to a per-session file under ~/.config/borg/logs so a
// failed session leaves a full trace, AND still streams them to the UI.
func TestDebugLogWritesPerSessionFile(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome) // os.UserConfigDir → here
	ui := &harnessUI{}
	a := newHarness(t, ui, &scriptedLLM{})

	a.SetDebug(true)
	a.dbgEmit("hello-diagnostic")
	a.SetDebug(false) // closes the file

	logs := filepath.Join(cfgHome, "borg", "logs")
	entries, err := os.ReadDir(logs)
	require.NoError(t, err)
	require.Len(t, entries, 1) // exactly one per-session log file
	data, err := os.ReadFile(filepath.Join(logs, entries[0].Name()))
	require.NoError(t, err)
	require.Contains(t, string(data), "hello-diagnostic")      // written to the file
	require.Contains(t, ui.debug.String(), "hello-diagnostic") // and teed to the UI
}

// --- the trajectories -----------------------------------------------------

// ask_user on a genuine fork: the loop drives the UI prompt (no permission gate,
// no parallel batch), feeds the chosen option's label back as the tool result,
// and the model continues from it.
func TestTrajectoryAskUser(t *testing.T) {
	ui := &harnessUI{askAnswer: func(AskRequest) AskResult { return AskResult{Choice: "REST"} }}
	s := &scriptedLLM{steps: []llm.Message{
		callTool("1", "ask_user", `{"question":"Which API style?","options":[{"label":"REST","description":"simple"},{"label":"gRPC","description":"streaming"}]}`),
		say("Going with REST."),
	}}
	a := newHarness(t, ui, s)

	require.NoError(t, a.Ask(context.Background(), "build the API"))

	require.Len(t, ui.asks, 1)
	require.Equal(t, "Which API style?", ui.asks[0].Question)
	require.Len(t, ui.asks[0].Options, 2)
	require.Equal(t, "The user chose: REST", lastToolResult(a))
	require.Empty(t, ui.permits) // ask_user is read-only — never permission-gated
	require.Contains(t, ui.deltas.String(), "REST")
}

// A free-text answer (the "something else" escape hatch) is fed back distinctly
// from a listed pick, so the model knows it may refine/override the options.
func TestTrajectoryAskUserFreeform(t *testing.T) {
	ui := &harnessUI{askAnswer: func(AskRequest) AskResult {
		return AskResult{Choice: "do B, but only in the eval harness", Freeform: true}
	}}
	s := &scriptedLLM{steps: []llm.Message{
		callTool("1", "ask_user", `{"question":"Which?","options":[{"label":"A"},{"label":"B"}]}`),
		say("Understood — B scoped to eval."),
	}}
	a := newHarness(t, ui, s)

	require.NoError(t, a.Ask(context.Background(), "go"))
	res := lastToolResult(a)
	require.Contains(t, res, "their own words")
	require.Contains(t, res, "do B, but only in the eval harness")
}

// Dismissing an ask_user question (Esc / no choice) feeds an autonomy nudge back
// so the model proceeds on its own rather than stalling on an unanswered prompt.
func TestTrajectoryAskUserDismissed(t *testing.T) {
	ui := &harnessUI{} // askAnswer nil → "" (dismissed)
	s := &scriptedLLM{steps: []llm.Message{
		callTool("1", "ask_user", `{"question":"Which?","options":[{"label":"A"},{"label":"B"}]}`),
		say("Proceeding with A."),
	}}
	a := newHarness(t, ui, s)

	require.NoError(t, a.Ask(context.Background(), "go"))
	require.Len(t, ui.asks, 1)
	require.Contains(t, lastToolResult(a), "dismissed")
}

// A degenerate ask_user call (only one option) is rejected at parse — it never
// reaches the UI — and the model gets an actionable message to proceed instead.
func TestTrajectoryAskUserMalformed(t *testing.T) {
	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{
		callTool("1", "ask_user", `{"question":"X?","options":[{"label":"only"}]}`),
		say("ok, deciding myself"),
	}}
	a := newHarness(t, ui, s)

	require.NoError(t, a.Ask(context.Background(), "go"))
	require.Empty(t, ui.asks) // rejected before the prompt ever showed
	require.Contains(t, lastToolResult(a), "at least two")
}

// parseAskRequest trims blank options, caps to maxAskOptions, and rejects an
// empty question or fewer than two real options.
func TestParseAskRequest(t *testing.T) {
	req, err := parseAskRequest(`{"question":" Pick ","options":[{"label":" A "},{"label":""},{"label":"B"}]}`)
	require.NoError(t, err)
	require.Equal(t, "Pick", req.Question)
	require.Equal(t, []AskOption{{Label: "A"}, {Label: "B"}}, req.Options)

	_, err = parseAskRequest(`{"question":"","options":[{"label":"A"},{"label":"B"}]}`)
	require.Error(t, err)
	_, err = parseAskRequest(`{"question":"q","options":[{"label":"A"}]}`)
	require.Error(t, err)
	_, err = parseAskRequest(`not json`)
	require.Error(t, err)

	many := `{"question":"q","options":[{"label":"1"},{"label":"2"},{"label":"3"},{"label":"4"},{"label":"5"},{"label":"6"}]}`
	req, err = parseAskRequest(many)
	require.NoError(t, err)
	require.Len(t, req.Options, maxAskOptions)
}

// Read → write (a mutating tool, so a permission prompt) → done. Asserts the
// loop calls the tools in order, gates the mutating one, and actually applies it.
func TestTrajectoryReadThenWrite(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.txt")
	out := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(src, []byte("hello"), 0o644))

	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{
		callTool("1", "read_file", `{"path":"`+src+`"}`),
		callTool("2", "write_file", `{"path":"`+out+`","content":"hello world"}`),
		say("Done — wrote out.txt."),
	}}
	a := newHarness(t, ui, s)

	require.NoError(t, a.Ask(context.Background(), "copy in.txt to out.txt, expanded"))

	require.Equal(t, []string{"read_file", "write_file"}, ui.calls)
	require.Equal(t, []string{"write_file"}, ui.permits) // only the mutating tool gated
	got, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, "hello world", string(got))
	require.Contains(t, ui.deltas.String(), "Done")
}

// A model that emits a tool call as plain TEXT (a serialization leak — e.g.
// gemma) must be re-prompted, not silently end the turn. Step 0 leaks a text
// "call:read_file{…}", step 1 issues the proper tool call, step 2 finishes.
func TestTrajectoryRecoversFromTextToolCall(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	require.NoError(t, os.WriteFile(real, []byte("data"), 0o644))

	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{
		say(`call:read_file{"path":"` + real + `"}<tool_call>`), // leaked as text
		callTool("1", "read_file", `{"path":"`+real+`"}`),       // re-issued properly
		say("Recovered."),
	}}
	a := newHarness(t, ui, s)

	require.NoError(t, a.Ask(context.Background(), "read the file"))

	// The leak was caught: the tool actually ran (not a silent stop), and the
	// model was nudged with the correction before step 1.
	require.Equal(t, []string{"read_file"}, ui.calls)
	step1 := s.seen[1]
	require.Equal(t, toolCallCorrection, step1[len(step1)-1].Content)
	require.Contains(t, ui.deltas.String(), "Recovered")
	// Escalation: the first turn ran on the default (auto, no option); only the
	// retry that follows the detected leak forces structured tool-calling.
	require.Equal(t, 0, s.optLen[0])
	require.Equal(t, 1, s.optLen[1])
	require.Equal(t, 0, s.optLen[2]) // back to auto once recovered
}

// Under guided/required tool-calling the model ends by calling `finish` (it can't
// reply with text). The loop must surface the summary and stop.
func TestTrajectoryFinishToolEndsTurn(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	require.NoError(t, os.WriteFile(real, []byte("data"), 0o644))

	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{
		callTool("1", "read_file", `{"path":"`+real+`"}`),
		callTool("2", "finish", `{"summary":"All done — the file says data."}`),
		// A third step would error (no scripted reply); reaching it means finish
		// didn't terminate.
	}}
	a := newHarness(t, ui, s)

	require.NoError(t, a.Ask(context.Background(), "read the file then finish"))
	require.Equal(t, 2, s.calls) // stopped right after the finish call
	require.Equal(t, []string{"read_file", "finish"}, ui.calls)
	require.Contains(t, ui.deltas.String(), "All done") // summary surfaced as the answer
}

// DeepInfra flags a botched tool call with finish_reason=malformed_function_call
// even when the content channel is empty. The loop must catch that and re-prompt.
func TestTrajectoryRecoversFromMalformedFinishReason(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	require.NoError(t, os.WriteFile(real, []byte("data"), 0o644))

	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{
		{Role: "assistant", Content: "", FinishReason: "malformed_function_call"},
		callTool("1", "read_file", `{"path":"`+real+`"}`),
		say("Recovered."),
	}}
	a := newHarness(t, ui, s)

	require.NoError(t, a.Ask(context.Background(), "read the file"))
	require.Equal(t, []string{"read_file"}, ui.calls) // not stranded
	step1 := s.seen[1]
	require.Equal(t, toolCallCorrection, step1[len(step1)-1].Content)
	require.Equal(t, 0, s.optLen[0]) // malformed turn ran on auto
	require.Equal(t, 1, s.optLen[1]) // retry escalated to required tool-calling
}

// A model that loops on prose instead of acting (the stream guard sets
// FinishReason=repetition) must be forced into a structured tool call, not left
// to ramble to its output cap. Step 0 loops, the loop re-issues under required
// tool-calling, step 1 acts, step 2 finishes.
func TestTrajectoryRecoversFromRepetitionLoop(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	require.NoError(t, os.WriteFile(real, []byte("data"), 0o644))

	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{
		{Role: "assistant", Content: "Actually, I'll just write it.\nActually, I'll just write it.\n", FinishReason: llm.FinishReasonRepetition},
		callTool("1", "read_file", `{"path":"`+real+`"}`),
		say("Done."),
	}}
	a := newHarness(t, ui, s)

	require.NoError(t, a.Ask(context.Background(), "learn the project"))
	require.Equal(t, []string{"read_file"}, ui.calls) // forced to act, not stranded
	step1 := s.seen[1]
	require.Equal(t, repetitionRetryMsg, step1[len(step1)-1].Content)
	require.Equal(t, 0, s.optLen[0]) // the looped turn ran on auto
	require.Equal(t, 1, s.optLen[1]) // retry escalated to required tool-calling
}

// A task with a required deliverable (e.g. /learn → BORG.md) must not "finish" by
// printing the file inline. Step 0 claims it wrote the file (no tool call); the
// loop forces a real write_file; step 1 writes it; step 2 finishes.
func TestTrajectoryForcesRequiredArtifactWrite(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "BORG.md")
	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{
		say("Here is BORG.md:\n```\n# BORG.md\nstuff\n```\nI have written BORG.md."), // claims done, no write
		callTool("1", "write_file", `{"path":"`+out+`","content":"# BORG.md\nstuff"}`),
		say("Done."),
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("write_file")
	a.RequireArtifact("BORG.md")

	require.NoError(t, a.Ask(context.Background(), "learn the project"))
	require.Equal(t, []string{"write_file"}, ui.calls) // forced the real write, not stranded
	step1 := s.seen[1]
	require.Contains(t, step1[len(step1)-1].Content, "printing its contents") // forcing nudge injected
	require.Equal(t, 1, s.optLen[1])                                          // retry under required tool-calling
	_, err := os.Stat(out)
	require.NoError(t, err) // the file actually exists now
}

// A broad task (e.g. /learn) that keeps exploring must not run out the step cap
// EMPTY: when it nears the cap with the required file still unwritten, the loop
// injects a "write it NOW" flush so the run always ends WITH the deliverable.
func TestTrajectoryArtifactFlushNearCap(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "BORG.md")
	ui := &harnessUI{}
	// Distinct reads each step (so the no-progress guard stays quiet), until the
	// near-cap flush nudge fires at step 4 — then write the file and finish.
	s := &scriptedLLM{gen: func(i int) llm.Message {
		switch {
		case i == 4:
			return callTool("w", "write_file", `{"path":"`+out+`","content":"# BORG.md\nlearned"}`)
		case i >= 5:
			return say("Done.")
		default:
			return callTool("r", "list_dir", fmt.Sprintf(`{"path":"%s/sub%d"}`, dir, i))
		}
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("write_file")
	a.SetMaxSteps(8) // artifactFlushMargin (4) → flush fires at step 4
	a.RequireArtifact("BORG.md")

	require.NoError(t, a.Ask(context.Background(), "learn the project"))

	flushSeen := false
	for _, snap := range s.seen {
		if len(snap) > 0 && strings.Contains(snap[len(snap)-1].Content, "nearly out of room") {
			flushSeen = true
		}
	}
	require.True(t, flushSeen) // the near-cap nudge was injected
	require.Contains(t, ui.calls, "write_file")
	require.FileExists(t, out)  // the deliverable actually landed
	require.Less(t, s.calls, 8) // ended via the write, didn't burn the whole cap
}

// The flush ALSO fires on context pressure (a huge repo nearing the window), not
// just the step cap — the non-lossy alternative to auto-compaction. A tiny window
// trips it immediately, so the file is written from real context, with the model
// told to disclose partial coverage.
func TestTrajectoryArtifactFlushOnContextPressure(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "BORG.md")
	ui := &harnessUI{}
	s := &scriptedLLM{gen: func(i int) llm.Message {
		if i == 0 {
			return callTool("w", "write_file", `{"path":"`+out+`","content":"# BORG.md\npartial"}`)
		}
		return say("Done — written under a size limit; I did not fully examine sub/.")
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("write_file")
	a.RequireArtifact("BORG.md")
	a.SetModelWindows([]llm.ModelInfo{{ID: "floko", MaxInputTokens: 100}}) // tiny window → context pressure at once

	require.NoError(t, a.Ask(context.Background(), "learn the project"))

	require.Contains(t, s.seen[0][len(s.seen[0])-1].Content, "nearly out of room") // flush from context, not steps
	require.Contains(t, s.seen[0][len(s.seen[0])-1].Content, "HONEST about coverage")
	require.Contains(t, ui.calls, "write_file")
	require.FileExists(t, out)
}

// The exploration budget bounds wall-clock deterministically: a model that keeps
// exploring (distinct list_dir each step — the defer-the-write wander) is forced
// to write at artifactExploreBudget, FAR below the hard step cap. This is the
// guard that prompt wording alone can't give us on a stochastic model.
func TestTrajectoryArtifactFlushOnExploreBudget(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "BORG.md")
	ui := &harnessUI{}
	s := &scriptedLLM{gen: func(i int) llm.Message {
		switch {
		case i == artifactExploreBudget: // the flush fires here → write
			return callTool("w", "write_file", `{"path":"`+out+`","content":"# BORG.md\nlearned"}`)
		case i > artifactExploreBudget:
			return say("Done.")
		default: // distinct dirs → no-progress guard stays quiet, would wander forever
			return callTool("r", "list_dir", fmt.Sprintf(`{"path":"%s/sub%d"}`, dir, i))
		}
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("write_file")
	a.SetMaxSteps(120) // learn's hard cap — the budget must fire well before this
	a.RequireArtifact("BORG.md")

	require.NoError(t, a.Ask(context.Background(), "learn the project"))

	require.Contains(t, ui.calls, "write_file")
	require.FileExists(t, out)
	require.LessOrEqual(t, s.calls, artifactExploreBudget+2) // forced at the budget, nowhere near 120
}

// The artifact guard is bounded: a model that never writes the file still ends
// after maxArtifactRetries instead of looping forever.
func TestTrajectoryRequiredArtifactBounded(t *testing.T) {
	ui := &harnessUI{}
	claim := say("I have written BORG.md.") // never actually calls write_file
	s := &scriptedLLM{repeat: &claim}
	a := newHarness(t, ui, s)
	a.RequireArtifact("BORG.md")

	require.NoError(t, a.Ask(context.Background(), "learn the project"))
	require.Equal(t, 1+maxArtifactRetries, s.calls) // initial + bounded retries, then ends
	require.Empty(t, ui.calls)
}

// A model that keeps looping must not spin forever: after maxRepetitionRetries
// forced retries the loop falls through to a normal finish.
func TestTrajectoryRepetitionLoopIsBounded(t *testing.T) {
	ui := &harnessUI{}
	rep := llm.Message{Role: "assistant", Content: "Actually, I'll just write it.\n", FinishReason: llm.FinishReasonRepetition}
	s := &scriptedLLM{repeat: &rep}
	a := newHarness(t, ui, s)

	require.NoError(t, a.Ask(context.Background(), "go"))
	require.Equal(t, 1+maxRepetitionRetries, s.calls) // initial + bounded retries, then it ends
	require.Empty(t, ui.calls)                        // never managed a tool call, but didn't loop forever
}

func TestLooksLikeTextToolCall(t *testing.T) {
	names := []string{"read_file", "bash"}
	require.True(t, looksLikeTextToolCall("<tool_call>{...}", names))
	require.True(t, looksLikeTextToolCall(`call:read_file{"path":"x"}`, names))
	require.True(t, looksLikeTextToolCall(`{"name": "bash", "arguments": {}}`, names))
	require.False(t, looksLikeTextToolCall("I'll read the file now.", names)) // prose mentioning a tool
	require.False(t, looksLikeTextToolCall("", names))
}

// A tool error must be fed back to the model as a `tool` message (not abort the
// run), so the model can recover. Step 0 reads a missing file (tool errors),
// step 1 reads the real file, step 2 finishes.
func TestTrajectoryRecoversFromToolError(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	require.NoError(t, os.WriteFile(real, []byte("data"), 0o644))

	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{
		callTool("1", "read_file", `{"path":"`+filepath.Join(dir, "nope.txt")+`"}`),
		callTool("2", "read_file", `{"path":"`+real+`"}`),
		say("Recovered."),
	}}
	a := newHarness(t, ui, s)

	require.NoError(t, a.Ask(context.Background(), "read the file"))

	require.Equal(t, []string{"read_file", "read_file"}, ui.calls)
	// The model's view at step 1 must include the error result from step 0.
	step1 := s.seen[1]
	require.Contains(t, step1[len(step1)-1].Content, "error:")
	require.Contains(t, lastToolResult(a), "data") // the recovery read succeeded
}

// Denying permission must surface to the model as a tool error and leave the
// filesystem untouched — without aborting the run.
func TestTrajectoryPermissionDenied(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "blocked.txt")

	ui := &harnessUI{answer: func(string) Decision { return DenyOnce }}
	s := &scriptedLLM{steps: []llm.Message{
		callTool("1", "write_file", `{"path":"`+out+`","content":"nope"}`),
		say("Understood — left it alone."),
	}}
	a := newHarness(t, ui, s)

	require.NoError(t, a.Ask(context.Background(), "overwrite blocked.txt"))

	require.Equal(t, []string{"write_file"}, ui.permits)
	require.NoFileExists(t, out) // denial blocked the write
	require.Contains(t, lastToolResult(a), "denied permission")
}

// A model that never stops calling tools must hit the step cap and end GRACEFULLY
// (with what it got to) rather than loop forever or surface a raw error. A
// DIFFERENT successful EDIT each step keeps "progress" flowing (lastEditStep stays
// fresh) so neither the no-progress guard nor the escalation backstop pre-empts the
// run — isolating the hard step cap on a genuinely non-stuck runaway.
func TestTrajectoryStepCapHalts(t *testing.T) {
	dir := t.TempDir()
	ui := &harnessUI{}
	s := &scriptedLLM{gen: func(i int) llm.Message {
		return callTool("x", "write_file", fmt.Sprintf(`{"path":"%s/f%d.txt","content":"x"}`, dir, i))
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("write_file")

	require.NoError(t, a.Ask(context.Background(), "go forever")) // graceful, not an error
	require.Contains(t, ui.deltas.String(), "step limit")
	require.Equal(t, defaultMaxSteps, s.calls) // stopped exactly at the cap
	require.Len(t, ui.calls, defaultMaxSteps)  // one tool call per step, no more
}

// SetMaxSteps lets the eval harness lower the per-Ask cap (and 0 restores the
// default). The loop must honor it exactly, leaving prod's default untouched. Uses
// an editing runaway (distinct successful edits) so the cap — not the escalation
// backstop — is what stops it.
func TestTrajectoryCustomStepCap(t *testing.T) {
	dir := t.TempDir()
	ui := &harnessUI{}
	s := &scriptedLLM{gen: func(i int) llm.Message {
		return callTool("x", "write_file", fmt.Sprintf(`{"path":"%s/f%d.txt","content":"x"}`, dir, i))
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("write_file")
	a.SetMaxSteps(5)
	require.NoError(t, a.Ask(context.Background(), "go forever"))
	require.Equal(t, 5, s.calls) // stopped at the lowered cap

	// 0 restores the default (covers the guard branch).
	s2 := &scriptedLLM{gen: func(i int) llm.Message {
		return callTool("x", "write_file", fmt.Sprintf(`{"path":"%s/z%d.txt","content":"x"}`, dir, i))
	}}
	a2 := newHarness(t, ui, s2)
	a2.AllowTools("write_file")
	a2.SetMaxSteps(0)
	require.NoError(t, a2.Ask(context.Background(), "go forever"))
	require.Equal(t, defaultMaxSteps, s2.calls)
}

// Oscillation: a model that alternates between two distinct unproductive calls
// (read X, read Y, read X, read Y…) has differing CONSECUTIVE signatures, so only
// windowed cycle detection catches it. It must bail before the step cap.
func TestTrajectoryStopsOnOscillationLoop(t *testing.T) {
	dir := t.TempDir()
	a1 := filepath.Join(dir, "a.txt")
	b1 := filepath.Join(dir, "b.txt")
	ui := &harnessUI{}
	s := &scriptedLLM{gen: func(i int) llm.Message {
		if i%2 == 0 {
			return callTool("x", "read_file", `{"path":"`+a1+`"}`)
		}
		return callTool("y", "read_file", `{"path":"`+b1+`"}`)
	}}
	a := newHarness(t, ui, s)

	require.NoError(t, a.Ask(context.Background(), "read them"))
	require.Less(t, s.calls, defaultMaxSteps) // 2-cycle caught, not run to the cap
	// It bails early — either via the no-progress stop ("Stopped") or, once the
	// reasoning ladder has climbed to xhigh without progress, via the give-up
	// backstop ("max-reasoning steps"). Both are graceful terminations before the cap.
	out := ui.deltas.String()
	require.True(t, strings.Contains(out, "Stopped") || strings.Contains(out, "max-reasoning steps"),
		"oscillation must terminate gracefully before the cap, got: %q", out)
}

// A model that repeats the IDENTICAL tool call (same args, same result) is stuck
// — the no-progress guard nudges it, then bails early instead of burning the full
// step budget. (This is what Floko did under required tool-calling in an empty
// directory: the same glob, over and over.)
func TestTrajectoryStopsOnNoProgressLoop(t *testing.T) {
	dir := t.TempDir()
	ui := &harnessUI{}
	stuck := callTool("x", "read_file", `{"path":"`+filepath.Join(dir, "nope.txt")+`"}`) // always the same error
	s := &scriptedLLM{repeat: &stuck}
	a := newHarness(t, ui, s)

	require.NoError(t, a.Ask(context.Background(), "read it"))
	require.Less(t, s.calls, defaultMaxSteps) // bailed early via the guard, not the step cap
	require.Contains(t, ui.deltas.String(), "Stopped")
	// It was nudged before stopping.
	nudged := false
	for _, snap := range s.seen {
		for _, m := range snap {
			if m.Content == noProgressNudge {
				nudged = true
			}
		}
	}
	require.True(t, nudged)
}

// A going-in-circles loop now CLIMBS the reasoning ladder (none→medium→high→xhigh)
// before giving up — a harder problem deserves more thinking. (Earlier the loop
// refused to escalate on circling, because the canonical circling case was a
// broken grep returning nothing forever; with that tool bug fixed, real "stuck on
// a hard problem" should think harder, and the xhigh-then-stop backstop caps the
// token downside.) So: the first turn carries the cheap default (optLen 0), then
// escalated turns carry a WithEffort option (optLen 1), and the run still
// terminates well before the step cap.
func TestTrajectoryEscalatesEffortOnNoProgress(t *testing.T) {
	dir := t.TempDir()
	ui := &harnessUI{}
	stuck := callTool("x", "read_file", `{"path":"`+filepath.Join(dir, "nope.txt")+`"}`)
	s := &scriptedLLM{repeat: &stuck}
	a := newHarness(t, ui, s) // default effort/think => auto-escalation active

	require.NoError(t, a.Ask(context.Background(), "read it"))

	require.Equal(t, 0, s.optLen[0], "the first turn runs on the cheap default")
	escalated := false
	for _, n := range s.optLen {
		if n > 0 {
			escalated = true
		}
	}
	require.True(t, escalated, "a persistent loop must climb the reasoning ladder")
	require.Less(t, s.calls, defaultMaxSteps, "the xhigh-then-stop backstop must still bail before the cap")
	require.Contains(t, ui.deltas.String(), "Stopped")

	// It climbed via the escalate-and-retry nudge before stopping.
	climbed := false
	for _, snap := range s.seen {
		for _, m := range snap {
			if strings.Contains(m.Content, "raised your reasoning effort") {
				climbed = true
			}
		}
	}
	require.True(t, climbed, "the loop should tell the model its effort was raised")
}

// Re-edit thrash escalates IN-TURN: editing the SAME file enough times in one task
// (the debug-loop signature — making "progress" so the cycle detector and circuit
// breaker stay quiet, yet not resolving anything) raises reasoning effort and injects
// a root-cause nudge, rather than letting the model churn edit-by-edit to the cap.
func TestTrajectoryReEditThrashEscalatesAndNudges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	ui := &harnessUI{}
	// Re-write the SAME file every step (distinct content, so no-progress can't trip),
	// then finish — so the re-edit-thrash trigger is what fires, nothing else.
	s := &scriptedLLM{gen: func(i int) llm.Message {
		if i >= 6 {
			return callTool("d", "finish", `{"summary":"done"}`)
		}
		return callTool("w", "write_file", fmt.Sprintf(`{"path":"%s","content":"v%d"}`, path, i))
	}}
	a := newHarness(t, ui, s) // default effort/think => auto-escalation active
	a.AllowTools("write_file")

	require.NoError(t, a.Ask(context.Background(), "fix it"))

	nudged := false
	for _, m := range a.messages {
		if m.Role == "user" && strings.Contains(m.Content, "ROOT CAUSE") {
			nudged = true
		}
	}
	require.True(t, nudged, "re-edit thrash must inject the root-cause nudge")

	escalated := false
	for _, n := range s.optLen {
		if n > 0 {
			escalated = true
		}
	}
	require.True(t, escalated, "re-edit thrash must climb the reasoning ladder")
}

// Model tiering (opt-in): once reasoning effort can climb no further and the task
// is still struggling, the agent tiers up to the configured premium model — once.
// Here effort is pinned (no ladder), so the no-progress nudge tiers immediately.
func TestTrajectoryTiersModelWhenStruggling(t *testing.T) {
	dir := t.TempDir()
	ui := &harnessUI{}
	stuck := callTool("x", "read_file", `{"path":"`+filepath.Join(dir, "nope.txt")+`"}`)
	s := &scriptedLLM{repeat: &stuck}
	a := newHarness(t, ui, s)
	a.SetEffort("high")         // pinned → no effort ladder → escalate tiers the model
	a.SetEscalateModel("axiom") // opt in

	require.NoError(t, a.Ask(context.Background(), "go"))
	require.Equal(t, "axiom", s.model) // tiered up after struggling at max effort
	// And it RETRIED with the stronger model (a fresh window) rather than tiering then
	// immediately quitting — the tier message was injected and the run continued.
	tiered := false
	for _, m := range a.messages {
		if m.Role == "user" && strings.Contains(m.Content, "switched to a stronger model") {
			tiered = true
		}
	}
	require.True(t, tiered, "tiering must retry with a fresh window, not set the model then give up")
}

// THE regression test for the 1.45M-token incident: a model that re-issues the
// SAME grep with the pattern's alternation reshuffled (a|b|c → b|c|a) used to slip
// past the no-progress guard (the signature hashed the raw arg string) and thrash
// to the step cap. With canonicalized signatures the reorderings collapse to one
// signature, so the guard counts them and bails early.
func TestTrajectoryStopsOnReorderedGrepLoop(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("nothing relevant here\n"), 0o644))
	ui := &harnessUI{}
	patterns := []string{"alpha|beta|gamma", "beta|gamma|alpha", "gamma|alpha|beta"} // same set, reordered
	s := &scriptedLLM{gen: func(i int) llm.Message {
		return callTool("g", "grep", `{"path":"`+dir+`","pattern":"`+patterns[i%len(patterns)]+`"}`)
	}}
	a := newHarness(t, ui, s)

	require.NoError(t, a.Ask(context.Background(), "find the thing"))
	require.Less(t, s.calls, defaultMaxSteps, "canonical signatures must collapse the reorderings and bail early")
	require.Contains(t, ui.deltas.String(), "Stopped")
}

// canonicalArgs collapses surface-only differences (key order, reordered regex
// alternation) so two semantically-identical calls hash the same.
func TestCanonicalArgs(t *testing.T) {
	require.Equal(t,
		canonicalArgs(`{"path":"x","pattern":"a|b|c"}`),
		canonicalArgs(`{"pattern":"c|a|b","path":"x"}`),
		"key order + alternation order must not change the signature")
	// A pattern with regex metacharacters is left alone (reordering could change it).
	require.NotEqual(t,
		canonicalArgs(`{"pattern":"(a|b)"}`),
		canonicalArgs(`{"pattern":"(b|a)"}`))
	// Non-object args are returned unchanged.
	require.Equal(t, "not json", canonicalArgs("not json"))
}

// The result-only loop net: read-only steps whose RESULTS recur — even with
// DIFFERENT args the canonical signature can't collapse (distinct grep patterns
// all returning "(no matches)") — still earn a no-progress nudge.
func TestTrajectoryNudgesOnRepeatedReadOnlyResult(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("unrelated\n"), 0o644))
	ui := &harnessUI{}
	s := &scriptedLLM{gen: func(i int) llm.Message {
		if i >= 5 {
			return say("nothing matched")
		}
		// Genuinely distinct patterns (no shared alternation), all → "(no matches)".
		return callTool("g", "grep", fmt.Sprintf(`{"path":"%s","pattern":"zzz%d"}`, dir, i))
	}}
	a := newHarness(t, ui, s)

	require.NoError(t, a.Ask(context.Background(), "search"))
	nudged := false
	for _, snap := range s.seen {
		for _, m := range snap {
			if m.Content == noProgressNudge {
				nudged = true
			}
		}
	}
	require.True(t, nudged, "repeated identical read-only results must trigger a nudge")
}

// THE regression test for the ~1M-token session: a NON-repeating read-only search
// (distinct reads every step, NO edits, no cycle). Climbing the reasoning ladder
// here was the trap — more thinking can't focus an unfocused search, it just
// prolongs the n-squared re-send. So a search that NEVER edits must NOT escalate:
// it gets one act-or-finish nudge, then a graceful give-up, well before the cap.
func TestTrajectoryNudgesAndStopsOnNonEditingSearch(t *testing.T) {
	dir := t.TempDir()
	ui := &harnessUI{}
	// A DIFFERENT non-existent read each step: read-only, no edit, distinct results
	// → neither the cycle guard nor the result-only net fires. Only the breaker.
	s := &scriptedLLM{gen: func(i int) llm.Message {
		return callTool("r", "read_file", fmt.Sprintf(`{"path":"%s/none%d.txt"}`, dir, i))
	}}
	a := newHarness(t, ui, s)
	a.SetMaxSteps(60) // generous cap — the nudge+give-up must end it well before this

	require.NoError(t, a.Ask(context.Background(), "investigate forever"))

	require.Less(t, s.calls, 60, "the give-up backstop must end it before the cap")
	// It must NOT have climbed the reasoning ladder — escalating a never-editing
	// search is the token trap this fixes. (It DOES force a structured tool call to
	// push the model to act/finish, so we check for the escalate MESSAGE, not optLen.)
	for _, snap := range s.seen {
		for _, m := range snap {
			require.NotContains(t, m.Content, "raised your reasoning effort",
				"a never-editing search must not climb the reasoning ladder")
		}
	}
	// It nudged to act-or-finish first, then gave up gracefully (not the
	// "max-reasoning steps" message, which would imply it had escalated).
	nudged := false
	for _, snap := range s.seen {
		for _, m := range snap {
			if m.Content == exploreActOrFinishMsg {
				nudged = true
			}
		}
	}
	require.True(t, nudged, "a non-editing search must get the act-or-finish nudge")
	require.Contains(t, ui.deltas.String(), "explored the project")
	require.NotContains(t, ui.deltas.String(), "max-reasoning steps")
}

// Post-verify finish brake: once an edit has LANDED and a verify went GREEN, a run
// of read-only steps (re-reading/re-grepping instead of finishing) is over-
// verification — the n-squared tail that ran a DONE task from ~step 39 to 45 in the
// 1.35M-token session. The loop nudges it to finish (forcing a structured call),
// WITHOUT climbing the reasoning ladder. General: keys only on edit-landed + the
// project's own green verify + read-only steps.
func TestTrajectoryFinishBrakeAfterGreenVerify(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fb\n\ngo 1.21\n"), 0o644))
	t.Chdir(dir)

	valid := `{"path":"p.go","content":"package p\n\nfunc F() int { return 1 }\n"}`
	s := &scriptedLLM{gen: func(i int) llm.Message {
		switch {
		case i == 0:
			return callTool("w", "write_file", valid) // edit lands → everEdited
		case i == 1:
			return callTool("v", "verify", `{}`) // compiles → green AFTER the edit
		case i >= 8:
			return callTool("f", "finish", `{"summary":"done"}`)
		default:
			// Over-verification: distinct read-only steps instead of finishing.
			return callTool("r", "read_file", fmt.Sprintf(`{"path":"%s/none%d.txt"}`, dir, i))
		}
	}}
	ui := &harnessUI{}
	a := newHarness(t, ui, s)
	a.AllowTools("write_file")

	require.NoError(t, a.Ask(context.Background(), "implement F"))

	braked := false
	for _, m := range a.messages {
		if m.Role == "user" && m.Content == finishBrakeMsg {
			braked = true
		}
	}
	require.True(t, braked, "a green-verify-then-read-only tail must trigger the finish brake")
	// The brake nudges to finish; it does NOT escalate reasoning.
	require.NotContains(t, ui.deltas.String(), "raised your reasoning effort")
	for _, snap := range s.seen {
		for _, m := range snap {
			require.NotEqual(t, fmt.Sprintf(escalateRetryMsg, "medium"), m.Content)
		}
	}
}

// Re-edit churn must not mask a stall forever. Repeatedly editing the SAME file (no
// verify, no finish) used to re-arm the give-up breaker on every edit, so it never
// tripped and the run burned to the step cap. Capping the re-arm at maxReEdits per
// path lets the circuit breaker reach its graceful give-up. Keyed only on edit-tool
// + path count, so it's language-agnostic.
func TestTrajectoryChurnStillReachesGiveUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt") // non-source: no auto-verify, isolates the churn signal
	s := &scriptedLLM{gen: func(i int) llm.Message {
		return callTool("w", "write_file", fmt.Sprintf(`{"path":"%s","content":"v%d"}`, path, i))
	}}
	ui := &harnessUI{}
	a := newHarness(t, ui, s)
	a.AllowTools("write_file")
	a.SetMaxSteps(60) // generous cap — the breaker must give up well before it

	require.NoError(t, a.Ask(context.Background(), "fix it"))

	require.Less(t, s.calls, 60, "churn must reach the give-up backstop, not run to the step cap")
	require.Contains(t, ui.deltas.String(), "max-reasoning steps") // gave up via the breaker (everEdited path)
}

// A successful edit tool emits a unified diff to the UI (the "what changed"
// preview) AND returns it to the model in the tool result.
func TestTrajectoryEmitsEditDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	require.NoError(t, os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644))
	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{
		callTool("e", "edit_file", `{"path":"`+path+`","old_string":"beta","new_string":"BETA"}`),
		say("done"),
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("edit_file") // skip the permission prompt

	require.NoError(t, a.Ask(context.Background(), "rename beta"))
	require.NotEmpty(t, ui.diffs, "a successful edit must emit a diff preview")
	require.Contains(t, ui.diffs[0], "+BETA")
	require.Contains(t, lastToolResult(a), "+BETA") // the model sees the diff too
}

// Escalation circuit breaker: once auto-effort raises reasoning (here via a failed
// compile fed back), if many steps pass with no successful edit the breaker now
// CLIMBS another rung (a harder problem deserves more reasoning) rather than
// dropping back — staying escalated, not de-escalating. (The upper backstop, the
// xhigh-then-give-up terminate, is covered by the no-progress ladder test.)
func TestEscalationCircuitBreakerClimbs(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module cb\n\ngo 1.21\n"), 0o644))
	t.Chdir(dir)

	broken := `{"path":"p.go","content":"package p\nfunc F() int { return }\n"}`
	s := &scriptedLLM{gen: func(i int) llm.Message {
		switch {
		case i == 0:
			return callTool("w", "write_file", broken) // succeeds → lastEditStep=0
		case i == 1:
			return callTool("f", "finish", `{"summary":"x"}`) // verify fails → escalate to medium
		case i >= 14:
			return callTool("d", "finish", `{"summary":"giving up"}`)
		default:
			// Distinct non-existent reads: read-only, no edit, distinct results — so
			// only the circuit breaker (no edit for many steps) drives escalation.
			return callTool("r", "read_file", fmt.Sprintf(`{"path":"%s/none%d.txt"}`, dir, i))
		}
	}}
	ui := &harnessUI{}
	a := newHarness(t, ui, s)
	a.AllowTools("write_file")

	require.NoError(t, a.Ask(context.Background(), "implement F"))

	require.Greater(t, len(s.optLen), 12)
	require.Equal(t, 1, s.optLen[2], "escalated to medium right after the compile failure")
	require.Equal(t, 1, s.optLen[12], "still escalated (breaker climbs, does not drop back)")
	climbed := false
	for _, m := range a.messages {
		if m.Role == "user" && strings.Contains(m.Content, "raised your reasoning effort") {
			climbed = true
		}
	}
	require.True(t, climbed, "the breaker should climb a rung, telling the model effort was raised")
}

// parseVerifyCommand extracts the project's declared verify command from BORG.md:
// the first command in a fenced block (or first real line) under a "## Verify"
// heading, case-insensitive, stopping at the next heading. "" when none.
func TestParseVerifyCommand(t *testing.T) {
	fenced := "# proj\n\n## Verify\n\n```\nmake docker-test\n```\n\n## Next\nother"
	require.Equal(t, "make docker-test", parseVerifyCommand(fenced))

	// Case-insensitive heading, a "$ " prompt prefix is stripped, stops at next heading.
	require.Equal(t, "npm test", parseVerifyCommand("## verify\n$ npm test\n## Layout\nx"))

	require.Equal(t, "", parseVerifyCommand("# proj\nno verify section here"))
	require.Equal(t, "", parseVerifyCommand("## Verify\n\n## Next\nx"))      // empty section
	require.Equal(t, "", parseVerifyCommand("## Verification\n```\nx\n```")) // not an exact "Verify" heading
}

// The auto-verify backstop runs the PROJECT'S declared verify command (BORG.md
// "## Verify") rather than the built-in compile check — so the harness runs the
// real tests the project's way (e.g. a containerized `make docker-test`) and the
// model never needs an ad-hoc host `go test`. A side-effecting command proves it ran.
func TestProjectVerifyBackstopRunsDeclaredCommand(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, os.WriteFile("f.go", []byte("package p\n\nvar X = 1\n"), 0o644))
	require.NoError(t, os.WriteFile(ProjectContextFile,
		[]byte("# p\n\n## Verify\n\n```\ntouch verified.marker\n```\n"), 0o644))

	ui := &harnessUI{} // Permit → AllowOnce by default → consents to the verify command
	s := &scriptedLLM{steps: []llm.Message{
		callTool("e", "edit_file", `{"path":"f.go","old_string":"var X = 1","new_string":"var X = 2"}`),
		say("done"),
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("edit_file")

	require.NoError(t, a.Ask(context.Background(), "bump X"))
	require.FileExists(t, filepath.Join(dir, "verified.marker"))     // the declared command actually ran
	require.Contains(t, ui.permits, "verify: touch verified.marker") // and it was gated, not silent
}

// When the declared verify command FAILS, the backstop feeds the failure back so
// the model must fix it before finishing — bounded so a persistently-red command
// can't loop forever.
func TestProjectVerifyBackstopFeedsFailureBack(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, os.WriteFile("f.go", []byte("package p\n\nvar X = 1\n"), 0o644))
	require.NoError(t, os.WriteFile(ProjectContextFile,
		[]byte("## Verify\n```\nexit 1\n```\n"), 0o644))

	ui := &harnessUI{}
	s := &scriptedLLM{gen: func(i int) llm.Message {
		if i == 0 {
			return callTool("e", "edit_file", `{"path":"f.go","old_string":"var X = 1","new_string":"var X = 2"}`)
		}
		return say("done")
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("edit_file")

	require.NoError(t, a.Ask(context.Background(), "bump X"))
	fedBack := false
	for _, m := range a.messages {
		if m.Role == "user" && strings.Contains(m.Content, "verify check and it FAILED") {
			fedBack = true
		}
	}
	require.True(t, fedBack, "a failing project verify command must be fed back to fix")
}

// A denied verify command is skipped (not run) and the turn can still finish —
// borg never silently executes a command from a repo file without consent.
func TestProjectVerifyBackstopRespectsDenial(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, os.WriteFile("f.go", []byte("package p\n\nvar X = 1\n"), 0o644))
	require.NoError(t, os.WriteFile(ProjectContextFile,
		[]byte("## Verify\n```\ntouch should-not-exist.marker\n```\n"), 0o644))

	ui := &harnessUI{answer: func(string) Decision { return DenyOnce }}
	s := &scriptedLLM{steps: []llm.Message{
		callTool("e", "edit_file", `{"path":"f.go","old_string":"var X = 1","new_string":"var X = 2"}`),
		say("done"),
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("edit_file")

	require.NoError(t, a.Ask(context.Background(), "bump X"))
	require.NoFileExists(t, filepath.Join(dir, "should-not-exist.marker")) // denial → never ran
}

// When the project declares its own verify command, a model-issued compile-only
// `verify{}` must NOT suppress the backstop: a compile check is not the project's
// real (often containerized) test run, so the declared command still runs before the
// turn finishes. Regression guard for the bug where a model `verify{}` cleared the
// dirty flag unconditionally, silently skipping e.g. `make docker-test`.
func TestModelCompileVerifyDoesNotSuppressDeclaredBackstop(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, os.WriteFile("f.go", []byte("package p\n\nvar X = 1\n"), 0o644))
	require.NoError(t, os.WriteFile(ProjectContextFile,
		[]byte("# p\n\n## Verify\n\n```\ntouch verified.marker\n```\n"), 0o644))

	ui := &harnessUI{} // Permit → AllowOnce by default → consents to the declared command
	s := &scriptedLLM{steps: []llm.Message{
		callTool("e", "edit_file", `{"path":"f.go","old_string":"var X = 1","new_string":"var X = 2"}`),
		callTool("v", "verify", `{}`), // model self-checks (compile-only) — must NOT dedup the declared backstop
		say("done"),
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("edit_file")

	require.NoError(t, a.Ask(context.Background(), "bump X"))
	require.FileExists(t, filepath.Join(dir, "verified.marker")) // declared command still ran despite the model's verify
}

// commandRunsVerify matches the declared command exactly or as a run inside a
// larger line (a leading `cd … &&`), but not an unrelated command, and bashSucceeded
// treats only a clean zero-exit run as a pass — a non-zero exit, a timeout, or a
// permission denial must NOT dedup the backstop.
func TestVerifyDedupHelpers(t *testing.T) {
	require.True(t, commandRunsVerify(`{"command":"make docker-test"}`, "make docker-test"))
	require.True(t, commandRunsVerify(`{"command":"cd sub && make docker-test"}`, "make docker-test"))
	require.False(t, commandRunsVerify(`{"command":"go test ./..."}`, "make docker-test"))
	require.False(t, commandRunsVerify(`{"command":"make docker-test"}`, "")) // no declared command

	require.True(t, bashSucceeded("PASS\nok  pkg  1.2s"))
	require.True(t, bashSucceeded("(no output)"))
	require.False(t, bashSucceeded("FAIL\n[exit: exit status 1]"))
	require.False(t, bashSucceeded("partial\n[timed out after 2m0s and was killed]"))
	require.False(t, bashSucceeded("error: the user denied permission to run this tool"))
}

// The inverse dedup: when the model runs the project's DECLARED verify command
// itself (via bash, and it passes), the backstop recognizes it and does not run the
// slow suite a second time.
func TestModelRunningDeclaredVerifyDedupsBackstop(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, os.WriteFile("f.go", []byte("package p\n\nvar X = 1\n"), 0o644))
	require.NoError(t, os.WriteFile(ProjectContextFile,
		[]byte("## Verify\n```\necho run >> runs.log\n```\n"), 0o644))

	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{
		callTool("e", "edit_file", `{"path":"f.go","old_string":"var X = 1","new_string":"var X = 2"}`),
		callTool("b", "bash", `{"command":"echo run >> runs.log"}`), // the model runs the declared command itself
		say("done"),
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("edit_file", "bash")

	require.NoError(t, a.Ask(context.Background(), "bump X"))
	b, err := os.ReadFile(filepath.Join(dir, "runs.log"))
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(b), "run"), "declared verify ran once — the model's run dedups the backstop")
}

// Waiting on a background command is NOT circling: a model that keeps calling
// bash_output (a pure poll step) must be exempt from the no-progress / escalation
// guards — even repeated identical results don't count as a stuck loop, so reasoning
// is never escalated and it's never nudged as "going in circles". (This was the
// false-positive: a `make docker-test` run polled its way to medium effort.)
func TestTrajectoryPollingDoesNotEscalate(t *testing.T) {
	ui := &harnessUI{}
	poll := callTool("p", "bash_output", `{"shell_id":"bash_1"}`) // same poll, same result each time
	s := &scriptedLLM{gen: func(i int) llm.Message {
		if i >= 8 {
			return say("done — tests passed")
		}
		return poll
	}}
	a := newHarness(t, ui, s) // default effort => escalation would be active if it mis-fired

	require.NoError(t, a.Ask(context.Background(), "run the tests"))

	for i, n := range s.optLen {
		require.Equalf(t, 0, n, "polling turn %d must not auto-escalate effort", i)
	}
	for _, snap := range s.seen {
		for _, m := range snap {
			require.NotContains(t, m.Content, "going in circles") // never nudged as stuck
		}
	}
}

// Step-count guard for the canonical "run tests" flow — the deterministic measure
// of its low token usage: start a long command in the BACKGROUND, ONE blocking
// bash_output that waits for completion, then report. That is exactly 3 model
// steps; if a regression reintroduced per-chunk polling (the old 9-step spin) this
// fails. A real `bash_output` that blocks for the command proves the collapse end
// to end (not a scripted [completed]).
func TestRunTestsFlowIsThreeSteps(t *testing.T) {
	dir := t.TempDir()
	ui := &harnessUI{}
	marker := filepath.Join(dir, "done.marker")
	s := &scriptedLLM{steps: []llm.Message{
		// 1) launch in the background (returns a shell id immediately)
		callTool("1", "bash", `{"command":"sleep 0.4; touch `+marker+`","run_in_background":true}`),
		// 2) ONE blocking read that waits for it to finish (no polling spin)
		callTool("2", "bash_output", `{"shell_id":"bash_1","wait_seconds":5}`),
		// 3) report
		say("tests done"),
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("bash")

	require.NoError(t, a.Ask(context.Background(), "run the tests"))

	require.Equal(t, 3, s.calls, "background command + one blocking read + report = 3 steps")
	require.Equal(t, []string{"bash", "bash_output"}, ui.calls) // no extra poll round-trips
	require.FileExists(t, marker)                               // the blocking read actually waited for completion
	for i, n := range s.optLen {
		require.Equalf(t, 0, n, "an efficient run-tests flow must not escalate effort (step %d)", i)
	}
}

// environmentAddendum names the actual working directory in the system prompt so
// the model anchors paths to it instead of inventing one (a run thrashed after
// grepping a hallucinated absolute path that didn't exist).
func TestEnvironmentAddendumNamesCwd(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	a := NewWithLLM(&config.Config{Model: "floko"}, &scriptedLLM{})
	sys := a.Messages()[0].Content
	require.Contains(t, sys, "Working directory")
	require.Contains(t, sys, filepath.Base(dir))
}

// bashDisplay shows the FULL bash command (multi-line, beyond 100 chars) in the
// tool line, not just a clipped first line — so the user can see exactly what ran.
func TestBashDisplayShowsFullCommand(t *testing.T) {
	long := strings.Repeat("x", 120)
	args, err := json.Marshal(map[string]string{"command": "echo one\necho two " + long})
	require.NoError(t, err)
	line := ToolCallLine("bash", string(args))
	require.Contains(t, line, "echo one") // first line present
	require.Contains(t, line, "echo two") // second line NOT dropped
	require.Contains(t, line, long)       // not clipped at 100 chars
}

// A task that trips a recovery guard records a Struggle for the post-turn
// retrospective; a clean task records nothing.
func TestStruggleRecordedOnGuardFire(t *testing.T) {
	dir := t.TempDir()
	stuck := callTool("x", "read_file", `{"path":"`+filepath.Join(dir, "nope.txt")+`"}`)
	a := newHarness(t, &harnessUI{}, &scriptedLLM{repeat: &stuck})
	require.NoError(t, a.Ask(context.Background(), "read it"))
	st := a.LastStruggle()
	require.NotNil(t, st)
	require.Equal(t, "read it", st.Task)
	require.NotEmpty(t, st.Reasons)
	require.Greater(t, st.Steps, 0)
	require.True(t, st.Terminal, "a no-progress STOP is a terminal give-up → report path")
}

// Re-edit thrash that ultimately finishes is a SOFT struggle (not terminal) → the
// BORG.md-note path, not a harness report.
func TestSoftStruggleFromReEdits(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	require.NoError(t, os.WriteFile(p, []byte("a\n"), 0o644))
	chain := []string{"a", "b", "c", "d", "e", "f"}
	i := 0
	a := newHarness(t, &harnessUI{}, &scriptedLLM{gen: func(int) llm.Message {
		if i >= len(chain)-1 {
			return say("done")
		}
		old, nw := chain[i], chain[i+1]
		i++
		return callTool("e", "edit_file", fmt.Sprintf(`{"path":%q,"old_string":%q,"new_string":%q}`, p, old, nw))
	}})
	a.AllowTools("edit_file")

	require.NoError(t, a.Ask(context.Background(), "bump the value"))
	st := a.LastStruggle()
	require.NotNil(t, st)
	require.False(t, st.Terminal, "finished despite churn → soft → BORG.md path, not a report")
}

func TestNoStruggleOnCleanTurn(t *testing.T) {
	a := newHarness(t, &harnessUI{}, &scriptedLLM{steps: []llm.Message{say("done")}})
	require.NoError(t, a.Ask(context.Background(), "hi"))
	require.Nil(t, a.LastStruggle())
}

// Severity drives the path: a TERMINAL give-up yields a harness report; a SOFT
// thrash yields a BORG.md note; a "NONE" reply yields nothing (stays silent).
func TestReflectOnSeverityDrivesKind(t *testing.T) {
	// Terminal → harness report.
	a := newHarness(t, &harnessUI{}, &scriptedLLM{steps: []llm.Message{
		say("The grep loop wasn't caught by the no-progress guard; canonicalize args."),
	}})
	r, err := a.ReflectOn(context.Background(), "TASK: x\nGUARDS:\n- gave up", true)
	require.NoError(t, err)
	require.Equal(t, RetroKindHarness, r.Kind)
	require.Contains(t, r.Text, "grep loop")

	// Soft → BORG.md note.
	a2 := newHarness(t, &harnessUI{}, &scriptedLLM{steps: []llm.Message{
		say("This project formats PHP with vendor/bin/pint."),
	}})
	r, err = a2.ReflectOn(context.Background(), "TASK: y\nGUARDS:\n- re-edited", false)
	require.NoError(t, err)
	require.Equal(t, RetroKindBorgMD, r.Kind)

	// "NONE" → stays silent regardless of severity.
	a3 := newHarness(t, &harnessUI{}, &scriptedLLM{steps: []llm.Message{say("NONE")}})
	r, err = a3.ReflectOn(context.Background(), "TASK: z\nGUARDS:\n- re-edited", false)
	require.NoError(t, err)
	require.Equal(t, RetroKindNone, r.Kind)

	// Empty input → none.
	r, err = a3.ReflectOn(context.Background(), "", true)
	require.NoError(t, err)
	require.Equal(t, RetroKindNone, r.Kind)
}

func TestApplyRetroLearnAppends(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	a := newHarness(t, &harnessUI{}, &scriptedLLM{})

	require.NoError(t, a.ApplyRetroLearn("Run make fmt before finishing."))
	b, _ := os.ReadFile(ProjectContextFile)
	require.Contains(t, string(b), "Lessons (auto-added by borg)")
	require.Contains(t, string(b), "Run make fmt before finishing.")

	// A second note appends under the SAME section (no duplicate header).
	require.NoError(t, a.ApplyRetroLearn("Prefer edit_lines for precise edits."))
	b, _ = os.ReadFile(ProjectContextFile)
	require.Equal(t, 1, strings.Count(string(b), "Lessons (auto-added by borg)"))
	require.Contains(t, string(b), "Prefer edit_lines")

	// Re-applying an existing note is a no-op (dedup — anti-bloat).
	require.NoError(t, a.ApplyRetroLearn("Run make fmt before finishing."))
	b, _ = os.ReadFile(ProjectContextFile)
	require.Equal(t, 1, strings.Count(string(b), "Run make fmt before finishing."))
}

// RetrospectInput captures the task + guards + a tool-call trace, and folds in the
// existing auto-lessons so a soft reflection won't propose a duplicate.
func TestRetrospectInputIncludesLessons(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, os.WriteFile(ProjectContextFile,
		[]byte("# proj\n\n## Lessons (auto-added by borg)\n- Old lesson here.\n"), 0o644))
	stuck := callTool("x", "read_file", `{"path":"`+filepath.Join(dir, "nope.txt")+`"}`)
	a := newHarness(t, &harnessUI{}, &scriptedLLM{repeat: &stuck})
	require.NoError(t, a.Ask(context.Background(), "do the thing"))

	in := a.RetrospectInput()
	require.Contains(t, in, "TASK: do the thing")
	require.Contains(t, in, "GUARDS THAT FIRED")
	require.Contains(t, in, "EXISTING BORG.md LESSONS")
	require.Contains(t, in, "Old lesson here.")

	a.ClearStruggle()
	require.Empty(t, a.RetrospectInput()) // nothing to reflect on
}

// Without a feedback-capable client (the scripted test LLM), reporting fails with a
// clear message rather than silently — nothing is ever sent.
func TestSubmitHarnessReportUnavailable(t *testing.T) {
	a := newHarness(t, &harnessUI{}, &scriptedLLM{})
	err := a.SubmitHarnessReport(context.Background(), "a report", &Struggle{Task: "x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unavailable")
}

// Tiering is OFF by default: the same stuck task never switches model unless the
// dev opted in via SetEscalateModel — so there's no surprise premium spend.
func TestModelTieringOffByDefault(t *testing.T) {
	dir := t.TempDir()
	ui := &harnessUI{}
	stuck := callTool("x", "read_file", `{"path":"`+filepath.Join(dir, "nope.txt")+`"}`)
	s := &scriptedLLM{repeat: &stuck}
	a := newHarness(t, ui, s)
	a.SetEffort("high") // would tier if opted in — but it isn't

	require.NoError(t, a.Ask(context.Background(), "go"))
	require.Empty(t, s.model) // SetModel never called for tiering
}

// An explicit effort (or --think) is the dev's choice and is never overridden by
// auto-escalation: even a stuck loop sends no per-call effort option.
func TestExplicitEffortDisablesAutoEscalation(t *testing.T) {
	dir := t.TempDir()
	ui := &harnessUI{}
	stuck := callTool("x", "read_file", `{"path":"`+filepath.Join(dir, "nope.txt")+`"}`)
	s := &scriptedLLM{repeat: &stuck}
	a := newHarness(t, ui, s)
	a.SetEffort("low") // dev pinned it — auto-escalation must stay off

	require.NoError(t, a.Ask(context.Background(), "read it"))
	for i, n := range s.optLen {
		require.Equalf(t, 0, n, "turn %d should carry no auto-escalation option", i)
	}
}

// AllowTools pre-approves a mutating tool so it runs without a permission prompt
// — even when the UI would otherwise deny. (Used by `borg install`.)
func TestAllowToolsSkipsPermit(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "f.txt")
	ui := &harnessUI{answer: func(string) Decision { return DenyOnce }} // would deny if asked
	s := &scriptedLLM{steps: []llm.Message{
		callTool("1", "write_file", `{"path":"`+out+`","content":"hi"}`),
		say("done"),
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("write_file")

	require.NoError(t, a.Ask(context.Background(), "write the file"))
	require.Empty(t, ui.permits) // pre-approved → never prompted
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, "hi", string(b))
}

// A turn with several read-only calls runs them in parallel; results must come
// back in order, matched to each call. (Race-checked under -race.)
func TestParallelReadOnlyToolCalls(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{"a.txt": "AAA", "b.txt": "BBB", "c.txt": "CCC"}
	var calls []llm.ToolCall
	id := 0
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
		id++
		calls = append(calls, read(fmt.Sprint(id), filepath.Join(dir, name)))
	}

	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{multiCall(calls...), say("done")}}
	a := newHarness(t, ui, s)
	require.NoError(t, a.Ask(context.Background(), "read all three"))

	// Each tool result is appended in call order with the matching file's content.
	var toolMsgs []llm.Message
	for _, m := range a.messages {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	require.Len(t, toolMsgs, 3)
	for i, tc := range calls {
		want := files[filepath.Base(argPath(t, tc))]
		require.Equal(t, tc.ID, toolMsgs[i].ToolCallID) // order preserved
		require.Contains(t, toolMsgs[i].Content, want)
	}
	require.Equal(t, 3, ui.batch) // the UI was told it was a 3-way parallel batch
}

// A batch mixing a read and a (mutating) write runs SEQUENTIALLY in order — the
// write is permission-gated, proving mutations aren't parallelized.
func TestMixedBatchRunsSequentially(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.txt")
	out := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(src, []byte("hi"), 0o644))

	batch := multiCall(
		read("1", src),
		llm.ToolCall{ID: "2", Type: "function", Function: llm.ToolCallFunction{
			Name: "write_file", Arguments: `{"path":"` + out + `","content":"x"}`}},
	)
	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{batch, say("done")}}
	a := newHarness(t, ui, s)
	require.NoError(t, a.Ask(context.Background(), "read then write"))

	require.Equal(t, []string{"read_file", "write_file"}, ui.calls) // sequential order
	require.Equal(t, []string{"write_file"}, ui.permits)            // mutation gated
	require.FileExists(t, out)
}

// A batch containing an unknown tool can't be parallelized (allReadOnly is
// false), so it runs sequentially; the unknown tool's error is fed back.
func TestBatchWithUnknownToolRunsSequentially(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(f, []byte("A"), 0o644))

	batch := multiCall(
		read("1", f),
		llm.ToolCall{ID: "2", Type: "function", Function: llm.ToolCallFunction{Name: "nope_tool", Arguments: "{}"}},
	)
	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{batch, say("done")}}
	a := newHarness(t, ui, s)
	require.NoError(t, a.Ask(context.Background(), "go"))

	var toolMsgs []llm.Message
	for _, m := range a.messages {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	require.Len(t, toolMsgs, 2)
	require.Contains(t, toolMsgs[0].Content, "A")            // the read ran
	require.Contains(t, toolMsgs[1].Content, "unknown tool") // the bogus tool reported
	require.Equal(t, []string{"read_file"}, ui.calls)        // only the real tool announced
}

// Auto-verify backstop: a turn that edited SOURCE must compile before it can end.
// The model writes a .go file in a BROKEN state and tries to finish; the loop runs
// the compile check ITSELF, feeds the failure back, and the model fixes it before
// the turn is allowed to finish — the read→edit→build loop closed by the harness,
// not by trusting the model to self-check.
func TestTrajectoryAutoVerifyRepairsBrokenEdit(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module av\n\ngo 1.21\n"), 0o644))
	t.Chdir(dir) // verify runs `go build ./...` in the working dir

	broken := `{"path":"p.go","content":"package p\nfunc F() int { return }\n"}`
	fixed := `{"path":"p.go","content":"package p\nfunc F() int { return 1 }\n"}`
	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{
		callTool("1", "write_file", broken),
		callTool("2", "finish", `{"summary":"done"}`), // premature — code doesn't build
		callTool("3", "write_file", fixed),
		callTool("4", "finish", `{"summary":"fixed it"}`),
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("write_file")

	require.NoError(t, a.Ask(context.Background(), "implement F"))

	require.Equal(t, 4, s.calls) // the broken finish was rejected, forcing a fix
	require.Equal(t, []string{"write_file", "finish", "verify", "write_file", "finish", "verify"}, ui.calls)

	var fedBack bool
	for _, m := range a.messages {
		if m.Role == "user" && strings.Contains(m.Content, "verify check and it FAILED") {
			fedBack = true
		}
	}
	require.True(t, fedBack, "the compile failure should be fed back to the model")

	got, err := os.ReadFile(filepath.Join(dir, "p.go"))
	require.NoError(t, err)
	require.Contains(t, string(got), "return 1") // user is left with code that builds
}

// A source edit that DOES compile triggers exactly one auto-verify (passing) and
// the turn ends — no repair round-trip.
func TestAutoVerifyPassesOnGoodEdit(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module av\n\ngo 1.21\n"), 0o644))
	t.Chdir(dir)

	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{
		callTool("1", "write_file", `{"path":"p.go","content":"package p\nfunc F() int { return 1 }\n"}`),
		callTool("2", "finish", `{"summary":"done"}`),
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("write_file")

	require.NoError(t, a.Ask(context.Background(), "implement F"))
	require.Equal(t, 2, s.calls) // verify passed, no repair turn
	require.Equal(t, []string{"write_file", "finish", "verify"}, ui.calls)
}

// If the model verifies its own edit and it passes, the loop does NOT re-run the
// compile check on finish — the two mechanisms dedup (no double build).
func TestAutoVerifySkipsWhenModelAlreadyVerified(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module av\n\ngo 1.21\n"), 0o644))
	t.Chdir(dir)

	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{
		callTool("1", "write_file", `{"path":"p.go","content":"package p\nfunc F() int { return 1 }\n"}`),
		callTool("2", "verify", `{}`), // model checks it itself → clears the dirty flag
		callTool("3", "finish", `{"summary":"done"}`),
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("write_file")

	require.NoError(t, a.Ask(context.Background(), "implement F"))
	require.Equal(t, 3, s.calls)
	require.Equal(t, []string{"write_file", "verify", "finish"}, ui.calls) // no second verify
}

// Auto-verify only arms on edits to verifiable SOURCE files. Writing a non-source
// file — what `borg learn` does with BORG.md — never triggers a compile check,
// even in a buildable project.
func TestAutoVerifySkipsNonSourceEdits(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module av\n\ngo 1.21\n"), 0o644))
	t.Chdir(dir)

	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{
		callTool("1", "write_file", `{"path":"BORG.md","content":"# notes\n"}`),
		callTool("2", "finish", `{"summary":"wrote notes"}`),
	}}
	a := newHarness(t, ui, s)
	a.AllowTools("write_file")

	require.NoError(t, a.Ask(context.Background(), "write notes"))
	require.Equal(t, 2, s.calls)                                 // no repair turn
	require.Equal(t, []string{"write_file", "finish"}, ui.calls) // no auto verify
}

// A finish call that hit the output-token cap (finish_reason "length") renders the
// salvaged partial answer plus a truncation note — never a silent turn.
func TestTrajectoryTruncatedFinishShowsNote(t *testing.T) {
	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{{
		Role: "assistant", FinishReason: "length",
		ToolCalls: []llm.ToolCall{{ID: "1", Type: "function",
			Function: llm.ToolCallFunction{Name: "finish", Arguments: `{"summary":"partial answer that got cut`}}},
	}}}
	a := newHarness(t, ui, s)

	require.NoError(t, a.Ask(context.Background(), "explain"))
	out := ui.deltas.String()
	require.Contains(t, out, "partial answer that got cut") // salvaged
	require.Contains(t, out, "output limit")                // truncation note
}

// With debug on, the loop emits full tool args/results and a per-step request
// trace through the UI's Debug sink, and SetDebug toggles cleanly.
func TestTrajectoryDebugEmitsDiagnostics(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(f, []byte("hi"), 0o644))
	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{
		callTool("1", "read_file", `{"path":"`+f+`"}`),
		callTool("2", "finish", `{"summary":"done"}`),
	}}
	a := newHarness(t, ui, s)

	require.False(t, a.Debug())
	a.SetDebug(true)
	require.True(t, a.Debug())

	require.NoError(t, a.Ask(context.Background(), "read it"))
	out := ui.debug.String()
	require.Contains(t, out, "tool read_file args:") // full tool args logged
	require.Contains(t, out, "done in")              // tool result + timing
	require.Contains(t, out, "step 1")               // per-step request trace

	a.SetDebug(false)
	require.False(t, a.Debug())
}

func argPath(t *testing.T, tc llm.ToolCall) string {
	t.Helper()
	var p struct {
		Path string `json:"path"`
	}
	require.NoError(t, json.Unmarshal([]byte(tc.Function.Arguments), &p))
	return p.Path
}

// SetTrustRoot confines edits: a write outside the trusted root is refused (the
// error is fed back to the model), and the file is never created.
func TestTrustRootConfinesEdits(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escape.txt")
	ui := &harnessUI{}
	s := &scriptedLLM{steps: []llm.Message{
		callTool("1", "write_file", `{"path":"`+outside+`","content":"x"}`),
		say("blocked"),
	}}
	a := newHarness(t, ui, s)
	a.SetTrustRoot(root)

	require.NoError(t, a.Ask(context.Background(), "try to escape"))
	require.NoFileExists(t, outside)
	require.Contains(t, lastToolResult(a), "outside the trusted")
}
