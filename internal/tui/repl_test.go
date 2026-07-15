package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/turborg/borg/internal/account"
	"github.com/turborg/borg/internal/agent"
	"github.com/turborg/borg/internal/auth"
	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/llm"
	"github.com/turborg/borg/internal/session"
	"github.com/turborg/borg/internal/trust"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func newTestModel(t *testing.T) model {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // isolate session/trust persistence
	cwd, _ := os.Getwd()
	_ = trust.Record(cwd, trust.ScopeDir) // pre-trust so tests skip the first-run prompt
	a := agent.New(&config.Config{Model: "floko"}, &auth.Credentials{AccessToken: "x"})
	m := newModel(context.Background(), a, session.New("floko", false))
	m.bannerBlink = -1 // settle the launch blink so steady-state layout tests see the resting view
	return m
}

// step applies one message and returns the concrete model back.
func step(t *testing.T, m model, msg tea.Msg) (model, tea.Cmd) {
	t.Helper()
	u, c := m.Update(msg)
	return u.(model), c
}

func key(s string) tea.KeyPressMsg {
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

// ansiRE matches SGR color/style escape sequences.
var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

// vw renders the model's visible view content (View now returns a tea.View
// struct), with ANSI styling stripped — the real terminal cursor styles the
// char beneath it as its own span, which would otherwise split substrings.
func vw(m model) string { return ansiRE.ReplaceAllString(m.View().Content, "") }

// fmtElapsed renders a running turn's duration compactly for the working line.
func TestFmtElapsed(t *testing.T) {
	require.Equal(t, "0s", fmtElapsed(0))
	require.Equal(t, "8s", fmtElapsed(8*time.Second))
	require.Equal(t, "1m05s", fmtElapsed(65*time.Second))
	require.Equal(t, "1h02m", fmtElapsed(3720*time.Second))
}

// The working line must stop showing "running read_file…" once the model starts
// thinking the next step — the reads finished in ms; the slow part is the model,
// so a stale tool label misreads minutes of thinking as a slow read.
func TestWorkingLineClearsToolNoteOnThink(t *testing.T) {
	m := newTestModel(t)
	m.streaming = true

	m, _ = step(t, m, toolCallMsg{name: "read_file", args: `{"path":"x"}`})
	require.Contains(t, vw(m), "running read_file") // during the (ms-fast) read

	m, _ = step(t, m, thinkingMsg{})
	require.NotContains(t, vw(m), "running read_file") // cleared when thinking starts
	require.Contains(t, vw(m), "Turborging")           // honest: the model is thinking
}

// clampDebug bounds a verbose debug block so a full-file dump can't flood the
// inline renderer: capped rows + a pointer to the on-disk log.
func TestClampDebug(t *testing.T) {
	require.Equal(t, "  · hello", clampDebug("hello", 0)) // short block: just prefixed

	long := strings.Repeat("line\n", 30)
	out := clampDebug(long, 80)
	require.LessOrEqual(t, len(strings.Split(out, "\n")), maxDebugRows+1) // capped + the "+N" note
	require.Contains(t, out, "+22 lines")                                 // 30 - 8 elided
	require.Contains(t, out, "full trace in ~/.config/borg/logs")
}

// The working line shows a live elapsed timer once a turn is running, and hides
// it before any turn (turnStart zero), so it never renders a bogus "0s" at idle.
func TestWorkingLineShowsElapsed(t *testing.T) {
	m := newTestModel(t)
	m.streaming = true
	require.Contains(t, vw(m), "Turborging")

	m.turnStart = time.Now().Add(-65 * time.Second)
	require.Contains(t, vw(m), "1m") // minute granularity is stable against tick timing
}

// --- markdown rendering ---

func TestRenderMarkdown(t *testing.T) {
	md := newMDRenderer()
	out := md.render("**bold** text", 80)
	require.Contains(t, out, "bold")
	require.NotContains(t, out, "**") // the literal asterisks are rendered away

	// width<=0 falls back to the default, and a second call reuses the renderer.
	require.NotEmpty(t, md.render("# Title", 0))
	require.Equal(t, defaultWidth, md.width)
}

// --- slash commands ---

func unwrap(tm tea.Model) model { return tm.(model) }

// slash dispatches a /command the way the model does: set the input line and
// submit it (submit routes slash lines to the command handler).
func slash(m model, input string) (model, tea.Cmd) {
	m.ti.in.SetValue(input)
	tm, cmd := m.submit()
	return unwrap(tm), cmd
}

func TestCommandModelThinkClear(t *testing.T) {
	m := newTestModel(t)

	m, _ = slash(m, "/model chuppa")
	require.Equal(t, "chuppa", m.agent.Model())
	require.Contains(t, m.status, "chuppa")

	// /model with no arg opens the interactive picker (covered fully elsewhere).
	mp, _ := slash(m, "/model")
	require.Equal(t, modeModelPicker, mp.mode)

	m, _ = slash(m, "/think on")
	require.True(t, m.agent.Think())
	m, _ = slash(m, "/think off")
	require.False(t, m.agent.Think())
	m, _ = slash(m, "/think") // toggle
	require.True(t, m.agent.Think())

	_, cmd := slash(m, "/clear")
	require.NotNil(t, cmd)
}

func TestCommandHelpExitUnknownPurge(t *testing.T) {
	m := newTestModel(t)

	_, cmd := slash(m, "/help")
	require.NotNil(t, cmd)

	mq, _ := slash(m, "/exit")
	require.True(t, mq.quitting)

	_, cmd = slash(m, "/bogus")
	require.NotNil(t, cmd) // prints an "unknown command" line

	mp, _ := slash(m, "/purge")
	require.Equal(t, modeConfirmPurge, mp.mode)
}

func TestEmptyAndPlainSubmit(t *testing.T) {
	m := newTestModel(t)

	// Empty input is ignored.
	m.ti.in.SetValue("   ")
	tm, cmd := m.submit()
	require.False(t, unwrap(tm).streaming)
	require.Nil(t, cmd)

	// A plain task starts streaming.
	m.ti.in.SetValue("do a thing")
	tm, cmd = m.submit()
	require.True(t, unwrap(tm).streaming)
	require.NotNil(t, cmd)
}

// --- streaming / rendering flow ---

func TestStreamingAndFinish(t *testing.T) {
	m := newTestModel(t)
	m.streaming = true

	m, _ = step(t, m, thinkingMsg{})
	m, _ = step(t, m, deltaMsg("**hi** there"))
	require.Contains(t, m.buf, "hi")
	require.Contains(t, vw(m), "hi") // live region renders the streamed markdown

	m, cmdTc := step(t, m, toolCallMsg{name: "bash", args: `{"command":"ls"}`})
	require.Equal(t, "bash", m.toolNote)
	require.NotNil(t, cmdTc) // prints the clean "⚙ bash $ ls" line
	require.Contains(t, vw(m), "running bash")

	// A tool result prints a colored ✓/✗ status line.
	_, okCmd := step(t, m, toolResultMsg{name: "bash", ok: true, summary: "done"})
	require.NotNil(t, okCmd)
	_, failCmd := step(t, m, toolResultMsg{name: "bash", ok: false, summary: "exit 1"})
	require.NotNil(t, failCmd)

	m, cmd := step(t, m, assistantEndMsg{stats: agent.Stats{InTokens: 5, OutTokens: 8, Elapsed: time.Second}})
	require.NotNil(t, cmd)  // finished step flushed to scrollback
	require.Empty(t, m.buf) // buffer cleared for the next step
}

// After a thrash, a borg_md retrospective opens a confirm modal showing the exact
// note; 'n' declines (writes nothing), the modal returns to input.
func TestRetrospectiveLearnModal(t *testing.T) {
	m := newTestModel(t)
	m, _ = step(t, m, retroMsg{retro: &agent.Retro{Kind: agent.RetroKindBorgMD, Text: "Use vendor/bin/pint for PHP."}})
	require.Equal(t, modeConfirmRetroLearn, m.mode)
	require.Contains(t, vw(m), "Use vendor/bin/pint for PHP.") // full transparency: the note is shown
	require.Contains(t, vw(m), "[y/N]")

	m, cmd := step(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	require.Equal(t, modeInput, m.mode)
	require.Nil(t, m.retro)
	require.NotNil(t, cmd)
}

// A harness retrospective opens the report-consent modal showing exactly what would
// be sent to the team.
func TestRetrospectiveReportModal(t *testing.T) {
	m := newTestModel(t)
	m, _ = step(t, m, retroMsg{retro: &agent.Retro{Kind: agent.RetroKindHarness, Text: "grep loop not caught by the guard"}})
	require.Equal(t, modeConfirmRetroReport, m.mode)
	out := vw(m)
	require.Contains(t, out, "borg team")                         // clearly a report to the team
	require.Contains(t, out, "grep loop not caught by the guard") // shows exactly what's sent
}

// A failed harness REPORT is best-effort: it renders a calm note and never echoes
// the raw transport error (the 500 that once leaked "llm request failed"). A failed
// local BORG.md write stays an actionable error.
func TestRetroDoneLineReportFailureIsCalm(t *testing.T) {
	report := retroDoneLine(retroDoneMsg{err: errors.New("llm request failed: 500 Internal Server Error"), report: true})
	require.Contains(t, report, "optional")
	require.NotContains(t, report, "500")                // no raw status leaked
	require.NotContains(t, report, "llm request failed") // and not the misleading wording

	learn := retroDoneLine(retroDoneMsg{err: errors.New("couldn't update BORG.md: disk full")})
	require.Contains(t, learn, "disk full") // a local write failure stays actionable

	ok := retroDoneLine(retroDoneMsg{status: "report sent to the borg team"})
	require.Contains(t, ok, "report sent to the borg team")
}

// A reflection that returns nothing actionable stays silent (no modal).
func TestRetrospectiveNoneStaysSilent(t *testing.T) {
	m := newTestModel(t)
	m, _ = step(t, m, retroMsg{retro: &agent.Retro{Kind: agent.RetroKindNone, Text: ""}})
	require.Equal(t, modeInput, m.mode)
	require.Nil(t, m.retrospectCmd()) // no struggle on the agent → no reflection scheduled
}

// 'y' on the learn modal appends the note to BORG.md; the done-message renders.
func TestRetrospectiveApply(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	m := newTestModel(t)

	m.mode = modeConfirmRetroLearn
	m.retro = &agent.Retro{Kind: agent.RetroKindBorgMD, Text: "Use vendor/bin/pint for PHP."}
	m, cmd := step(t, m, key("y"))
	require.Equal(t, modeInput, m.mode)
	require.Nil(t, m.retro)
	require.NotNil(t, cmd)
	dm, ok := cmd().(retroDoneMsg) // run the apply
	require.True(t, ok)
	require.NoError(t, dm.err)
	b, _ := os.ReadFile("BORG.md")
	require.Contains(t, string(b), "Use vendor/bin/pint for PHP.")

	// Both done-message variants render a line.
	_, sc := step(t, m, dm)
	require.NotNil(t, sc)
	_, ec := step(t, m, retroDoneMsg{err: errors.New("boom")})
	require.NotNil(t, ec)

	// 'y' on the report modal returns a submit cmd (not executed here — it'd hit the
	// network); the branch + mode reset are what we're covering.
	m.mode = modeConfirmRetroReport
	m.retro = &agent.Retro{Kind: agent.RetroKindHarness, Text: "grep loop not caught"}
	m.struggle = &agent.Struggle{Task: "t"}
	m3, rc := step(t, m, key("y"))
	require.Equal(t, modeInput, m3.mode)
	require.NotNil(t, rc)
}

// /output (and ctrl+o) flush the last tool's full output to scrollback.
func TestOutputCommand(t *testing.T) {
	m := newTestModel(t)

	// Nothing run yet → a friendly note, still a cmd.
	_, cmd := slash(m, "/output")
	require.NotNil(t, cmd)

	// After a tool result is in history, /output prints it.
	m.agent.SetMessages([]llm.Message{
		{Role: "system", Content: "s"},
		{Role: "tool", Name: "bash", Content: "line1\nline2\n…lots more…"},
	})
	_, cmd = slash(m, "/output")
	require.NotNil(t, cmd)

	// ctrl+o is the keybinding equivalent.
	_, cmd = step(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	require.NotNil(t, cmd)
}

// An edit tool's diff is rendered to scrollback (the "what changed" preview).
func TestToolDiffRenders(t *testing.T) {
	m := newTestModel(t)
	_, cmd := step(t, m, toolDiffMsg("--- f.go\n+++ f.go\n@@ -1,1 +1,1 @@\n-old\n+new"))
	require.NotNil(t, cmd) // the diff is flushed to scrollback

	// renderDiff colors +/- lines and keeps the content (truncated to width).
	out := renderDiff("@@ -1,1 +1,1 @@\n-removed\n+added\n context", 80)
	require.Contains(t, out, "removed")
	require.Contains(t, out, "added")
	require.Contains(t, out, "context")
}

func TestLiveTokenCounter(t *testing.T) {
	m := newTestModel(t)
	m.streaming = true
	require.NotContains(t, vw(m), "↓") // nothing streamed yet → no counter

	// Streamed text shows a live ↓ estimate (~4 chars/token).
	m, _ = step(t, m, deltaMsg(strings.Repeat("x", 40)))
	require.Contains(t, vw(m), "↓ 10 tokens")

	// Step completes: exact usage folds in and the ↑ input count appears.
	m, _ = step(t, m, assistantEndMsg{stats: agent.Stats{InTokens: 1200, OutTokens: 8}})
	require.Equal(t, 1200, m.turnIn)
	require.Equal(t, 8, m.turnOut)
	v := vw(m)
	require.Contains(t, v, "↑ 1.2k")
	require.Contains(t, v, "↓ 8 tokens")

	// A new turn resets the counter.
	m.ti.in.SetValue("next task")
	m = unwrap(func() tea.Model { tm, _ := m.submit(); return tm }())
	require.Equal(t, 0, m.turnIn)
	require.Equal(t, 0, m.turnOut)

	// fmtTokens compact formatting (both branches).
	require.Equal(t, "999", fmtTokens(999))
	require.Equal(t, "1.5k", fmtTokens(1500))
}

func TestTurnDonePersistsAndReportsError(t *testing.T) {
	m := newTestModel(t) // isolates XDG_CONFIG_HOME
	m.streaming = true

	m, cmd := step(t, m, turnDoneMsg{err: errors.New("boom")})
	require.False(t, m.streaming)
	require.NotNil(t, cmd) // error printed

	// turnDoneMsg persists the session.
	metas, err := session.List()
	require.NoError(t, err)
	require.Len(t, metas, 1)
	require.Equal(t, m.sess.ID, metas[0].ID)

	// A clean turn still returns a cmd — the footer's live usage is refreshed
	// after every turn (no error is printed, but the usage fetch is batched).
	m.streaming = true
	_, cmd = step(t, m, turnDoneMsg{})
	require.NotNil(t, cmd)
}

func TestSpinnerTickAndWindowSize(t *testing.T) {
	m := newTestModel(t)

	_, cmd := step(t, m, spinner.TickMsg{}) // not streaming -> ignored
	require.Nil(t, cmd)

	m.streaming = true
	_, cmd = step(t, m, spinner.TickMsg{})
	require.NotNil(t, cmd)

	m, _ = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	require.Equal(t, 120, m.width)
}

// --- permission prompt ---

func TestPermitFlow(t *testing.T) {
	for in, want := range map[string]agent.Decision{
		"y": agent.AllowOnce, "a": agent.AllowAlways, "n": agent.DenyOnce,
	} {
		m := newTestModel(t)
		reply := make(chan agent.Decision, 1)
		m, _ = step(t, m, permitMsg{name: "bash", reply: reply})
		require.Equal(t, modePermit, m.mode)
		require.Contains(t, vw(m), "allow bash")

		m, _ = step(t, m, key(in))
		require.Equalf(t, want, <-reply, "key %q", in)
		require.Equal(t, modeInput, m.mode)
	}
}

func TestAskUserFlow(t *testing.T) {
	req := agent.AskRequest{Question: "Which API?", Options: []agent.AskOption{
		{Label: "REST", Description: "simple"},
		{Label: "gRPC", Description: "streaming"},
	}}
	newAsk := func() (model, chan agent.AskResult) {
		m := newTestModel(t)
		m.streaming = true // ask_user fires mid-turn
		reply := make(chan agent.AskResult, 1)
		m, _ = step(t, m, askMsg{req: req, reply: reply})
		return m, reply
	}

	// The modal renders the question, the options, AND the built-in "Something
	// else" row; Enter picks the highlighted (first) one.
	m, reply := newAsk()
	require.Equal(t, modeAskUser, m.mode)
	require.Contains(t, vw(m), "Which API?")
	require.Contains(t, vw(m), "1. REST")
	require.Contains(t, vw(m), "2. gRPC")
	require.Contains(t, vw(m), "3. Something else")
	m, cmd := step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, agent.AskResult{Choice: "REST"}, <-reply)
	require.Equal(t, modeInput, m.mode)
	require.NotNil(t, cmd) // echoes the choice to scrollback

	// ↓ then Enter picks the second option.
	m, reply = newAsk()
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, agent.AskResult{Choice: "gRPC"}, <-reply)

	// A digit key selects that option directly.
	m, reply = newAsk()
	m, _ = step(t, m, key("2"))
	require.Equal(t, agent.AskResult{Choice: "gRPC"}, <-reply)

	// The "Something else" row (row 3) opens a free-text box; the typed answer
	// comes back as a Freeform result and the turn resumes.
	m, reply = newAsk()
	m, _ = step(t, m, key("3"))
	require.True(t, m.askFreeText)
	require.False(t, m.streaming) // paused for the text box
	m.ti.in.SetValue("do B but only in eval")
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, agent.AskResult{Choice: "do B but only in eval", Freeform: true}, <-reply)
	require.False(t, m.askFreeText)
	require.True(t, m.streaming)

	// Esc on the free-text box returns to the picker without resolving.
	m, reply = newAsk()
	m, _ = step(t, m, key("3"))
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, modeAskUser, m.mode)
	require.False(t, m.askFreeText)
	require.Len(t, reply, 0) // nothing sent yet

	// Esc on the picker dismisses → zero result, agent proceeds autonomously.
	m, reply = newAsk()
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, agent.AskResult{}, <-reply)
	require.Equal(t, modeInput, m.mode)
}

func TestConfirmPurgeKey(t *testing.T) {
	m := newTestModel(t) // isolates XDG_CONFIG_HOME
	require.NoError(t, session.Save(session.New("floko", false)))

	m.mode = modeConfirmPurge
	require.Contains(t, vw(m), "Delete ALL")
	require.Contains(t, vw(m), "cannot be undone")

	// "n" cancels, leaving the session in place.
	mn, cmd := step(t, m, key("n"))
	require.Equal(t, modeInput, mn.mode)
	require.NotNil(t, cmd)
	metas, _ := session.List()
	require.Len(t, metas, 1)

	// "y" purges.
	m.mode = modeConfirmPurge
	my, _ := step(t, m, key("y"))
	require.Equal(t, modeInput, my.mode)
	metas, _ = session.List()
	require.Empty(t, metas)
}

// --- key routing in input mode ---

func TestOnKeyInputMode(t *testing.T) {
	m := newTestModel(t)

	// Ctrl-C quits.
	mq, cmd := step(t, m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	require.True(t, mq.quitting)
	require.NotNil(t, cmd)

	// Enter while streaming queues the typed line as a follow-up (it isn't lost).
	m.streaming = true
	m.ti.in.SetValue("do this next")
	m, cmd = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, []string{"do this next"}, m.queued)
	require.Empty(t, m.ti.in.Value()) // input cleared, ready for more
	require.NotNil(t, cmd)            // echoes the queued line to scrollback

	// A normal rune feeds the text input.
	m.streaming = false
	m, _ = step(t, m, key("h"))
	require.Contains(t, m.ti.in.Value(), "h")
}

func TestInitAndIdleView(t *testing.T) {
	m := newTestModel(t)
	require.NotNil(t, m.Init())
	// Idle view shows the input prompt's placeholder.
	require.Contains(t, vw(m), "ask borg")

	m.status = "model: chuppa"
	require.Contains(t, vw(m), "chuppa")
}

// freshModel builds a pre-trusted model with the launch blink UN-settled (unlike
// newTestModel, which settles it for steady-state layout tests).
func freshModel(t *testing.T) model {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cwd, _ := os.Getwd()
	_ = trust.Record(cwd, trust.ScopeDir)
	a := agent.New(&config.Config{Model: "floko"}, &auth.Credentials{AccessToken: "x"})
	return newModel(context.Background(), a, session.New("floko", false))
}

// visorBand is the mascot's scanning band — a stable marker that the droid is in
// the live region (it's not in the settled-to-scrollback path the tests can't see).
const visorBand = "░░░░░░░░░"

func TestLaunchMascotBlink(t *testing.T) {
	m := freshModel(t)
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.Equal(t, 0, m.bannerBlink) // a fresh REPL animates
	require.NotNil(t, m.Init())

	// Resting frame: the visor band and open square eyes show in the live region.
	v := vw(m)
	require.Contains(t, v, visorBand)
	require.Contains(t, v, "▢")

	// One frame in, the eyes close.
	m, _ = step(t, m, blinkMsg{})
	require.Equal(t, 1, m.bannerBlink)
	require.Contains(t, vw(m), "▬")

	// Running the schedule to completion settles the mascot out of the live region
	// (it's flushed to scrollback, which the headless View can't observe).
	for i := 0; i < len(blinkSchedule)+2; i++ {
		m, _ = step(t, m, blinkMsg{})
	}
	require.Equal(t, -1, m.bannerBlink)
	require.NotContains(t, vw(m), visorBand)
}

func TestMascotSettlesOnFirstKeypress(t *testing.T) {
	m := freshModel(t)
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.Equal(t, 0, m.bannerBlink)

	m, cmd := step(t, m, key("h")) // the first keystroke settles the banner early
	require.Equal(t, -1, m.bannerBlink)
	require.NotNil(t, cmd)                    // the settle Printf (batched) fired
	require.NotContains(t, vw(m), visorBand)  // gone from the live region
	require.Contains(t, m.ti.in.Value(), "h") // ...and the keystroke still typed
}

func TestMascotSkippedOnNarrowTerminal(t *testing.T) {
	m := freshModel(t)
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 12, Height: 24}) // below mascotMinWidth
	require.Equal(t, 0, m.bannerBlink)
	require.NotContains(t, vw(m), visorBand) // too narrow → not drawn in the redraw region
}

func TestResumedSessionSkipsBlink(t *testing.T) {
	m := freshModel(t)
	sess := session.New("floko", false)
	sess.Messages = []llm.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "yo"}}
	m2 := newModel(context.Background(), m.agent, sess)
	require.Equal(t, -1, m2.bannerBlink) // resumed → instant, no animation
	require.NotNil(t, m2.Init())
}

func TestInputBoxRendersCursorAndFooter(t *testing.T) {
	const w = 60
	m := newTestModel(t)
	m, _ = step(t, m, tea.WindowSizeMsg{Width: w, Height: 24})
	// Pin a short path: the footer keeps the right (model/effort) and truncates
	// the left, so a long real cwd (CI's is long) would otherwise be clipped here.
	m.cwd = "/home/u/proj"

	// The idle prompt shows the placeholder and the home-abbreviated cwd footer.
	v := vw(m)
	require.Contains(t, v, "ask borg")
	require.Contains(t, v, tildeDir(m.cwd))

	// No line ever exceeds the terminal width (no wrap / overflow).
	for _, ln := range strings.Split(m.View().Content, "\n") {
		require.LessOrEqual(t, ansi.StringWidth(ln), w)
	}

	// The cursor sits on the first text row (below the top margin and the box's
	// top pad row), shifted right past the left pad and the "› " prompt.
	cur := m.View().Cursor
	require.NotNil(t, cur)
	require.Equal(t, inputTopMargin+inputPadRows, cur.Y)
	require.Equal(t, inputPrefix, cur.X)

	// A long line wraps and grows the box, and still never overflows the width.
	m.ti.in.SetValue(strings.Repeat("word ", 40))
	for _, ln := range strings.Split(m.View().Content, "\n") {
		require.LessOrEqual(t, ansi.StringWidth(ln), w)
	}

	// While streaming, the "working…" line sits above the box and the input box
	// stays active (cursor present) so a follow-up can be queued.
	m.streaming = true
	m.toolNote = "bash"
	m.ti.in.SetValue("")
	require.Contains(t, vw(m), "running bash")
	require.NotNil(t, m.View().Cursor)
}

func TestInputBoxGrowsWithWrappedContent(t *testing.T) {
	const w = 30
	m := newTestModel(t)
	m, _ = step(t, m, tea.WindowSizeMsg{Width: w, Height: 24})
	emptyRows := strings.Count(m.View().Content, "\n")

	// A line longer than the box wraps onto more rows and grows the box.
	m.ti.in, _ = m.ti.in.Update(tea.PasteMsg{Content: strings.Repeat("word ", 12)})
	grownRows := strings.Count(m.View().Content, "\n")
	require.Greater(t, grownRows, emptyRows)

	// Growth is capped, and no row ever overflows the width.
	m.ti.in, _ = m.ti.in.Update(tea.PasteMsg{Content: strings.Repeat("word ", 400)})
	for _, ln := range strings.Split(m.View().Content, "\n") {
		require.LessOrEqual(t, ansi.StringWidth(ln), w)
	}
	require.LessOrEqual(t, m.ti.in.Height(), inputMaxRows)
}

// TestResizeKeepsLayoutWidthSafe drives a resize sequence (incl. shrink → grow,
// and very narrow widths) and asserts no rendered line ever exceeds the terminal
// width — at idle, with a long wrapped prompt, and while streaming with a full
// footer (path + branch + session tokens + budget bar). This is the bulletproof
// guarantee: the box/footer math never wraps or overflows at any width.
func TestResizeKeepsLayoutWidthSafe(t *testing.T) {
	m := newTestModel(t)
	m.gitBranch = "staging"
	assertWidthSafe := func(w int) {
		t.Helper()
		for _, ln := range strings.Split(m.View().Content, "\n") {
			require.LessOrEqualf(t, ansi.StringWidth(ln), w, "line wider than %d: %q", w, ln)
		}
	}
	for _, w := range []int{120, 80, 40, 24, 10, 200, 60} {
		m, _ = step(t, m, tea.WindowSizeMsg{Width: w, Height: 24})

		// Idle, empty prompt.
		m.ti.in.SetValue("")
		m.streaming = false
		assertWidthSafe(w)

		// Idle, a long prompt that wraps and grows the box.
		m.ti.in.SetValue(strings.Repeat("word ", 80))
		assertWidthSafe(w)
		m.ti.in.SetValue("")

		// Streaming with a long assistant buffer and a fully-populated footer.
		m.streaming = true
		m.buf = strings.Repeat("lorem ipsum dolor ", 40)
		m.toolNote = "bash"
		m.sessIn, m.sessOut = 12345, 6789
		m.usage = &llm.AccountUsage{CreditsPerDay: 500, PercentUsed: 50}
		assertWidthSafe(w)
		m.streaming, m.buf, m.toolNote = false, "", ""
	}
}

func TestPasteMultilineBecomesPlaceholder(t *testing.T) {
	m := newTestModel(t)

	// A multi-line paste collapses to a compact placeholder; the full text is
	// buffered and restored by expandPastes (what the model actually receives).
	mm, _ := m.onPaste(tea.PasteMsg{Content: "alpha\nbeta\ngamma"})
	m = mm.(model)
	require.Equal(t, "[Pasted Text #1: 3 lines]", m.ti.in.Value())
	require.Equal(t, "alpha\nbeta\ngamma", m.expandPastes(m.ti.in.Value()))

	// A second paste mints a fresh id; both expand within a larger line.
	m.ti.in.SetValue(m.ti.in.Value() + " and ")
	m.ti.in.CursorEnd() // append the next paste at the end
	mm, _ = m.onPaste(tea.PasteMsg{Content: "x\ny"})
	m = mm.(model)
	require.Equal(t, "[Pasted Text #1: 3 lines] and [Pasted Text #2: 2 lines]", m.ti.in.Value())
	require.Equal(t, "alpha\nbeta\ngamma and x\ny", m.expandPastes(m.ti.in.Value()))

	// Submitting expands the pastes for the model but echoes the compact form, and
	// clears the buffer so it can't accumulate.
	m.streaming = false
	sm, cmd := m.submit()
	m = sm.(model)
	require.NotNil(t, cmd)
	require.Nil(t, m.pastes)
}

func TestPasteSingleLineInsertsRaw(t *testing.T) {
	m := newTestModel(t)
	mm, _ := m.onPaste(tea.PasteMsg{Content: "one line"})
	m = mm.(model)
	require.Equal(t, "one line", m.ti.in.Value()) // inserted as-is, no placeholder
	require.Empty(t, m.pastes)
}

func TestPasteCarriageReturnNewlinesDetected(t *testing.T) {
	// Terminals send "\r" for newlines in bracketed paste — it must still collapse
	// to a placeholder and expand to clean "\n"-separated text.
	m := newTestModel(t)
	mm, _ := m.onPaste(tea.PasteMsg{Content: "alpha\r\nbeta\rgamma"})
	m = mm.(model)
	require.Equal(t, "[Pasted Text #1: 3 lines]", m.ti.in.Value())
	require.Equal(t, "alpha\nbeta\ngamma", m.expandPastes(m.ti.in.Value()))
}

func TestEscInterruptsRunningTurn(t *testing.T) {
	m := newTestModel(t)
	m.streaming = true
	ctx, cancel := context.WithCancel(context.Background())
	m.turnCancel = cancel
	defer cancel()

	// Esc while streaming cancels the turn's context (it does not quit).
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.False(t, m.quitting)
}

func TestTurnDoneInterruptedAcknowledges(t *testing.T) {
	m := newTestModel(t)
	m.streaming = true
	m.turnCancel = func() {}

	// A cancelled turn ends streaming, clears the cancel func, and prints an
	// "Interrupted" acknowledgement (not an error).
	m, cmd := step(t, m, turnDoneMsg{err: context.Canceled})
	require.False(t, m.streaming)
	require.Nil(t, m.turnCancel)
	require.NotNil(t, cmd)
}

// A follow-up typed while a turn streams is queued, then auto-submitted as a new
// turn when the current one finishes — so borg stays usable while it works.
func TestQueuedFollowUpAutoSubmits(t *testing.T) {
	m := newTestModel(t)
	m.streaming = true

	// Type and Enter twice while streaming: both are queued, none dropped.
	m.ti.in.SetValue("first follow-up")
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m.ti.in.SetValue("second follow-up")
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, []string{"first follow-up", "second follow-up"}, m.queued)

	// While streaming, the working line reports the queue depth.
	require.Contains(t, vw(m), "2 queued")

	// The turn ends → the oldest queued item is dequeued and started as a new turn
	// (streaming stays true), with the rest still queued in order.
	m, cmd := step(t, m, turnDoneMsg{})
	require.True(t, m.streaming)
	require.Equal(t, []string{"second follow-up"}, m.queued)
	require.NotNil(t, cmd)

	// That turn ends → the last queued item runs, draining the queue.
	m, _ = step(t, m, turnDoneMsg{})
	require.True(t, m.streaming)
	require.Empty(t, m.queued)

	// With nothing queued, a turn end returns to idle.
	m, _ = step(t, m, turnDoneMsg{})
	require.False(t, m.streaming)
}

func TestMarkdownHeadingsDropHashes(t *testing.T) {
	md := newMDRenderer()
	out := md.render("### The Core Concept\n", 60)
	require.NotContains(t, out, "###") // no literal hashes
	require.Contains(t, ansi.Strip(out), "The Core Concept")
}

func TestTildeDir(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.Equal(t, "~", tildeDir(home))
	require.Equal(t, "~"+string(os.PathSeparator)+"proj", tildeDir(filepath.Join(home, "proj")))
	require.Equal(t, "/etc", tildeDir("/etc")) // outside home: unchanged
	require.Empty(t, tildeDir(""))
}

// --- bridge ---

func TestBridgeCapturesMessages(t *testing.T) {
	var msgs []tea.Msg
	b := &uiBridge{send: func(m tea.Msg) { msgs = append(msgs, m) }, ctx: context.Background()}
	b.ThinkingStart()
	b.Delta("x")
	b.ToolCall("bash", "{}")
	b.ToolResult("bash", true, "ok")
	b.AssistantEnd(agent.Stats{})
	require.Len(t, msgs, 5)
}

func TestBridgePermitReplyAndCancel(t *testing.T) {
	// Reply path: send answers on the reply channel (buffered), Permit returns it.
	b := &uiBridge{
		send: func(m tea.Msg) { m.(permitMsg).reply <- agent.AllowAlways },
		ctx:  context.Background(),
	}
	require.Equal(t, agent.AllowAlways, b.Permit("bash"))

	// Cancel path: a cancelled context unblocks Permit with DenyOnce.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b2 := &uiBridge{send: func(tea.Msg) {}, ctx: ctx}
	require.Equal(t, agent.DenyOnce, b2.Permit("bash"))
}

// --- end-to-end: agent -> bridge -> model render ---

func TestAgentThroughBridgeRendersMarkdown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"**the answer**"}}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	var msgs []tea.Msg
	a := agent.New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, &auth.Credentials{AccessToken: "tok"})
	a.SetUI(&uiBridge{send: func(m tea.Msg) { msgs = append(msgs, m) }, ctx: context.Background()})
	require.NoError(t, a.Ask(context.Background(), "say hi"))

	// Feed the captured stream through a model and confirm the finished turn
	// renders markdown (no literal asterisks).
	m := newTestModel(t)
	m.streaming = true
	var gotDelta bool
	for _, msg := range msgs {
		if d, ok := msg.(deltaMsg); ok && strings.Contains(string(d), "the answer") {
			gotDelta = true
		}
		m, _ = step(t, m, msg)
	}
	require.True(t, gotDelta)
	require.NotContains(t, m.md.render("**the answer**", 80), "**")
}

func TestMiscModelBranches(t *testing.T) {
	m := newTestModel(t)

	// An unhandled message while idle feeds the text input (default branch).
	_, cmd := step(t, m, 42)
	require.Nil(t, cmd)

	// renderHelp falls back to a package renderer when none is supplied.
	require.NotEmpty(t, renderHelp(nil, 80))

	// persist is a no-op when there's no session.
	m.sess = nil
	require.NotPanics(t, m.persist)

	// summarize truncates long argument blobs.
	require.True(t, strings.HasSuffix(summarize(strings.Repeat("a", 200)), "…"))
}

func TestStartTurnExecutesAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	a := agent.New(&config.Config{LLMProxyURL: srv.URL, Model: "floko"}, &auth.Credentials{AccessToken: "tok"})
	a.SetUI(&uiBridge{send: func(tea.Msg) {}, ctx: context.Background()})
	m := newModel(context.Background(), a, session.New("floko", false))

	msg := m.startTurn(context.Background(), "say hi")() // run the turn Cmd directly
	done, ok := msg.(turnDoneMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
}

func TestReplayTranscript(t *testing.T) {
	require.Empty(t, replayTranscript(nil, newMDRenderer(), 80))

	sess := session.New("floko", false)
	sess.Messages = []llm.Message{
		{Role: "system", Content: "sys"}, // omitted
		{Role: "user", Content: "fix the parser"},
		{Role: "assistant", Content: "**done**", ToolCalls: []llm.ToolCall{
			{Function: llm.ToolCallFunction{Name: "list_dir", Arguments: `{"path":"."}`}},
		}},
		{Role: "tool", Content: "noise"}, // omitted
	}
	out := replayTranscript(sess, newMDRenderer(), 80)
	require.Contains(t, out, "fix the parser") // user prompt replayed
	require.Contains(t, out, "done")           // assistant markdown rendered
	require.NotContains(t, out, "**")          // ...without literal asterisks
	require.Contains(t, out, "list_dir")       // tool call replayed
	require.NotContains(t, out, "sys")
	require.NotContains(t, out, "noise")
}

func TestInitReplaysResumedSession(t *testing.T) {
	m := newTestModel(t)
	m.sess.Messages = []llm.Message{{Role: "user", Content: "earlier question"}}
	require.NotNil(t, m.Init())
}

func TestSlashMenuAndCompletion(t *testing.T) {
	// matchingCommands: "/" lists all, a prefix narrows, a space (args) hides it.
	require.Len(t, matchingCommands("/"), len(slashCmds))
	one := matchingCommands("/mo")
	require.Len(t, one, 1)
	require.Equal(t, "/model", one[0].name)
	require.Nil(t, matchingCommands("/model x"))
	require.Nil(t, matchingCommands("hello"))

	// The live menu renders in the idle view while composing a command.
	m := newTestModel(t)
	m.ti.in.SetValue("/cl")
	require.Contains(t, vw(m), "/clear")

	// Tab completes an unambiguous command...
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, "/clear ", m.ti.in.Value())

	// ...and on an ambiguous prefix completes the highlighted entry (the first).
	m.ti.in.SetValue("/")
	m.menuIdx = 0
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, slashCmds[0].name+" ", m.ti.in.Value())
}

func TestMenuNavigationAndEnter(t *testing.T) {
	m := newTestModel(t)
	m.ti.in.SetValue("/")
	require.Greater(t, len(matchingCommands("/")), 1)

	// Down/Up move the highlight and clamp at the ends.
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, 1, m.menuIdx)
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, 0, m.menuIdx)
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, 0, m.menuIdx) // clamped

	// Tab on the menu completes the highlighted command (index 2 == /clear).
	m.ti.in.SetValue("/")
	m.menuIdx = 2
	mt, _ := step(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, slashCmds[2].name+" ", mt.ti.in.Value())

	// Enter on the menu runs the highlighted command (index 0 == /model, which
	// opens the model picker).
	m.ti.in.SetValue("/")
	m.menuIdx = 0
	me, _ := step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, modeModelPicker, me.mode)
}

func TestHistoryRecall(t *testing.T) {
	m := newTestModel(t)
	m.history = []string{"alpha", "beta"} // plain inputs (no menu)
	m.histIdx = len(m.history)
	up := tea.KeyPressMsg{Code: tea.KeyUp}
	down := tea.KeyPressMsg{Code: tea.KeyDown}

	m, _ = step(t, m, up)
	require.Equal(t, "beta", m.ti.in.Value())
	m, _ = step(t, m, up)
	require.Equal(t, "alpha", m.ti.in.Value())
	m, _ = step(t, m, up)
	require.Equal(t, "alpha", m.ti.in.Value()) // clamped at oldest
	m, _ = step(t, m, down)
	require.Equal(t, "beta", m.ti.in.Value())
	m, _ = step(t, m, down)
	require.Equal(t, "", m.ti.in.Value()) // past newest -> blank line

	// Empty history: Up/Down are no-ops.
	e := newTestModel(t)
	e, _ = step(t, e, up)
	require.Equal(t, "", e.ti.in.Value())
}

func TestSubmitRecordsHistory(t *testing.T) {
	m := newTestModel(t)
	m.ti.in.SetValue("first task")
	tm, _ := m.submit()
	m = unwrap(tm)
	require.Equal(t, []string{"first task"}, m.history)
	require.Equal(t, len(m.history), m.histIdx)

	// A consecutive duplicate isn't appended twice.
	m.ti.in.SetValue("first task")
	m = unwrap(func() tea.Model { tm, _ := m.submit(); return tm }())
	require.Equal(t, []string{"first task"}, m.history)
}

func TestEscDismissesMenuKeepingInput(t *testing.T) {
	m := newTestModel(t)
	m.ti.in.SetValue("/")
	require.NotEmpty(t, m.menu())

	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.True(t, m.menuDismissed)
	require.Empty(t, m.menu())             // suppressed
	require.Equal(t, "/", m.ti.in.Value()) // text kept
	require.NotContains(t, vw(m), "/clear")

	// Editing the line re-activates the menu.
	m, _ = step(t, m, key("c")) // "/c"
	require.False(t, m.menuDismissed)
	require.NotEmpty(t, m.menu())
}

func TestMenuViewIsFixedHeight(t *testing.T) {
	require.Empty(t, menuView(nil, 0, 0))

	// The menu always renders maxMenuRows lines (blank-padded) so the inline
	// renderer never shrinks the region as matches are filtered.
	require.Equal(t, maxMenuRows, strings.Count(menuView(slashCmds, 0, 0), "\n"))
	one := menuView([]slashCmd{{name: "/model", desc: "d"}}, 0, 0)
	require.Equal(t, maxMenuRows, strings.Count(one, "\n")) // 1 match + blank padding
	require.Contains(t, one, "/model")

	// A list longer than maxMenuRows windows around the selection (still fixed
	// height), keeping the selected item visible.
	big := make([]slashCmd, 10)
	for i := range big {
		big[i] = slashCmd{name: fmt.Sprintf("/c%d", i), desc: "d"}
	}
	top := menuView(big, 0, 0)
	require.Equal(t, maxMenuRows, strings.Count(top, "\n"))
	require.Contains(t, top, "/c0")
	require.NotContains(t, top, "/c9")

	bottom := menuView(big, 9, 0)
	require.Equal(t, maxMenuRows, strings.Count(bottom, "\n"))
	require.Contains(t, bottom, "/c9")
	require.NotContains(t, bottom, "/c0")

	// A narrow width clips a row to a single line (no wrap) with an ellipsis.
	narrow := menuView([]slashCmd{{name: "/model", desc: "a very long description that overflows"}}, 0, 20)
	require.Equal(t, maxMenuRows, strings.Count(narrow, "\n")) // still fixed height
	require.Contains(t, narrow, "…")
}

func TestMenuViewHighlightsSelectedDescription(t *testing.T) {
	if dim.Render("x") == "x" {
		t.Skip("no color profile active — styling not asserted")
	}
	items := []slashCmd{{name: "/a", desc: "alpha"}, {name: "/b", desc: "beta"}}
	out := menuView(items, 0, 0) // /a selected

	// The selected row's description renders bright (plain), not dim-wrapped,
	// while the unselected row's description stays dim.
	require.Contains(t, out, "alpha")
	require.Contains(t, out, dim.Render("beta"))     // unselected: dim
	require.NotContains(t, out, dim.Render("alpha")) // selected: brightened
}

func TestPickersBrightenSelectedRow(t *testing.T) {
	if dim.Render("x") == "x" {
		t.Skip("no color profile active — styling not asserted")
	}

	// /effort picker: the selected level's description is bright, others dim.
	m := newTestModel(t)
	m.mode = modeEffortPicker
	m.effortIdx = 0
	ev := m.effortPickerView()
	require.NotContains(t, ev, dim.Render("  "+effortLevels[0].desc)) // selected: bright
	require.Contains(t, ev, dim.Render("  "+effortLevels[1].desc))    // unselected: dim

	// /model picker: the selected model's version + description are bright.
	m.mode = modeModelPicker
	m.models = []llm.ModelInfo{
		{Label: "floko", Version: "3.2", Description: "fast", MinTier: "free"},
		{Label: "chuppa", Version: "3.2", Description: "deep", MinTier: "free"},
	}
	m.modelIdx = 0
	mv := m.modelPickerView()
	require.NotContains(t, mv, dim.Render("  fast")) // selected description: bright
	require.Contains(t, mv, dim.Render("  deep"))    // unselected description: dim
}

func TestPromptChevronAffordance(t *testing.T) {
	// knownCommand recognizes full command names (and the /quit alias), not
	// partials or plain text — that drives the input chevron's color.
	require.True(t, knownCommand("/model"))
	require.True(t, knownCommand("/status"))
	require.True(t, knownCommand("/quit"))
	require.False(t, knownCommand("/mod"))   // partial
	require.False(t, knownCommand("hello"))  // plain text
	require.False(t, knownCommand("/bogus")) // unknown

	if dim.Render("x") == "x" {
		t.Skip("no color profile active — chevron color not asserted")
	}
	// A fully-typed command turns the chevron teal; plain text keeps it blue.
	// (Assert on raw View content — vw() would strip the color we're checking.)
	m := newTestModel(t)
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 60, Height: 24})
	m.ti.in.SetValue("/status")
	require.Contains(t, m.View().Content, tool.Render("›")) // teal chevron
	m.ti.in.SetValue("do a thing")
	require.NotContains(t, m.View().Content, tool.Render("›")) // blue, not teal
}

func TestCommandMatchingIsCaseInsensitive(t *testing.T) {
	require.Len(t, matchingCommands("/M"), 1)
	require.Equal(t, "/model", matchingCommands("/M")[0].name)
}

// Reproduces the report: typing "/" then "m" should keep the menu visible,
// filtered down to /model — driven through the real keypress path.
func TestTypingFiltersMenuLive(t *testing.T) {
	m := newTestModel(t)

	m, _ = step(t, m, key("/"))
	require.Equal(t, "/", m.ti.in.Value())
	require.Len(t, m.menu(), len(slashCmds))
	require.Contains(t, vw(m), "/model")

	m, _ = step(t, m, key("m")) // "/m"
	require.Equal(t, "/m", m.ti.in.Value())
	require.Len(t, m.menu(), 1)
	require.Equal(t, "/model", m.menu()[0].name)
	require.Contains(t, vw(m), "/model")

	for _, r := range "odel" { // "/model"
		m, _ = step(t, m, key(string(r)))
	}
	require.Equal(t, "/model", m.ti.in.Value())
	require.Contains(t, vw(m), "/model")
}

func TestInfoLineAndMsg(t *testing.T) {
	m := newTestModel(t)
	require.Empty(t, m.infoLine()) // nothing fetched yet

	// Availability is computed from the tier vs each model's min_tier, NOT the
	// catalog's `available` flag — note chuppa is sent as Available:true but must
	// still read as locked on the free plan.
	m, cmd := step(t, m, infoMsg{tier: "free", models: []llm.ModelInfo{
		{ID: "floko", Label: "Floko", Version: "3.2", MinTier: "free", Available: true},
		{ID: "chuppa", Label: "Chuppa", Version: "3.2", MinTier: "pro", Available: true},
	}})
	require.NotNil(t, cmd)
	require.Equal(t, "free", m.tier)
	line := m.infoLine()
	require.Contains(t, line, "Free")      // tier title-cased
	require.Contains(t, line, "Floko")     // catalog labels
	require.Contains(t, line, "3.2")       // versions
	require.Contains(t, line, "current")   // current model marked (floko)
	require.Contains(t, line, "needs pro") // chuppa locked despite Available:true
}

func TestAvailabilityFromTierNotFlag(t *testing.T) {
	m := newTestModel(t)
	chuppa := llm.ModelInfo{ID: "chuppa", MinTier: "pro", Available: true} // flag lies

	m.tier = "free"
	require.False(t, m.modelAvailable(chuppa)) // free < pro → locked
	m.tier = "pro"
	require.True(t, m.modelAvailable(chuppa)) // pro >= pro → available
	m.tier = ""                               // unknown tier → treat as free, paid models locked
	require.False(t, m.modelAvailable(chuppa))
	require.True(t, m.modelAvailable(llm.ModelInfo{MinTier: "free"}))
}

func TestUsageAndPrivacyCommands(t *testing.T) {
	m := newTestModel(t)
	m.tier = "free"

	// /usage dispatches a live fetch and shows a "fetching usage…" placeholder so
	// the layout stays stable during the async window (the cursor doesn't jump).
	m, cmd := slash(m, "/usage")
	require.NotNil(t, cmd)
	require.Contains(t, m.status, "fetching usage")

	// A background (quiet) footer refresh must NOT clear that placeholder.
	mq, _ := step(t, m, usageMsg{usage: &llm.AccountUsage{PlanCode: "free", WindowHours: 24}, quiet: true})
	require.Contains(t, mq.status, "fetching usage")

	// The visible response clears the placeholder and renders the panel.
	m, cmd = step(t, m, usageMsg{usage: &llm.AccountUsage{PlanCode: "free", WindowHours: 24}})
	require.NotNil(t, cmd) // handler prints the rendered usage
	require.Empty(t, m.status)

	// Live usage renders plan + the shared credit budget as "used / per-day (pct%)".
	live := m.renderUsage(usageMsg{usage: &llm.AccountUsage{
		PlanCode: "max", WindowHours: 24,
		PercentUsed: 42, CreditsUsed: 210, CreditsPerDay: 500,
	}})
	require.Contains(t, live, "Max")
	require.Contains(t, live, "210 / 500")
	require.Contains(t, live, "42% used")

	// A 0 per-day grant renders as unlimited.
	unlim := m.renderUsage(usageMsg{usage: &llm.AccountUsage{PlanCode: "max", WindowHours: 24, CreditsUsed: 9}})
	require.Contains(t, unlim, "9 / ∞")

	// A fetch error falls back to the plan's static credit hint.
	fb := m.renderUsage(usageMsg{err: errors.New("404")})
	require.Contains(t, fb, "Free")
	require.Contains(t, fb, "50 credits")
	require.Contains(t, fb, "unavailable")

	_, cmd = slash(m, "/privacy")
	require.NotNil(t, cmd)
	require.Contains(t, renderPrivacy(nil, 80), privacyURL)
}

// writeCreds plants a credentials file at the (XDG-isolated) default path so the
// auth-aware commands see a logged-in state.
func writeCreds(t *testing.T, c *auth.Credentials) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "borg")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	b, err := json.Marshal(c)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "credentials.json"), b, 0o600))
}

func TestStatusCommand(t *testing.T) {
	m := newTestModel(t)
	m.tier = "free"

	// /status dispatches an async fetch; the result renders on statusMsg.
	m, cmd := slash(m, "/status")
	require.NotNil(t, cmd)
	require.Contains(t, m.status, "fetching status")

	// The visible response clears the placeholder and renders the panel.
	m, cmd = step(t, m, statusMsg{usage: &llm.AccountUsage{PlanCode: "free", WindowHours: 24}})
	require.NotNil(t, cmd)
	require.Empty(t, m.status)

	// Not logged in (no creds on disk): the panel says so but still shows settings.
	out := m.renderStatus(statusMsg{})
	require.Contains(t, out, "not logged in")
	require.Contains(t, out, "floko") // current model

	// Logged in, with identity + live usage: shows the user, plan, and meters.
	writeCreds(t, &auth.Credentials{
		AccessToken: "x", TokenType: "Bearer",
		Expiry:     time.Now().Add(time.Hour),
		APIBaseURL: "https://api.example.test",
	})
	full := m.renderStatus(statusMsg{
		user: &llm.UserInfo{Name: "Ada", Email: "ada@example.test", PlanCode: "max"},
		usage: &llm.AccountUsage{
			PlanCode: "max", WindowHours: 24,
			PercentUsed: 42, CreditsUsed: 210, CreditsPerDay: 500,
		},
	})
	require.Contains(t, full, "Ada <ada@example.test>")
	require.Contains(t, full, "api:") // effective endpoint (cfg-derived) line present
	require.Contains(t, full, "Max")
	require.Contains(t, full, "210 / 500")
	require.Contains(t, full, "42% used")
	require.Contains(t, full, "Bearer")
}

func TestLoginLogoutCommands(t *testing.T) {
	m := newTestModel(t)
	m.tier = "pro"

	// /login kicks off the OAuth flow off the event loop and sets a hint.
	m, cmd := slash(m, "/login")
	require.NotNil(t, cmd)
	require.Contains(t, m.status, "logging in")

	// A failed login surfaces the error and clears the hint.
	mErr, cmd := step(t, m, loginMsg{err: errors.New("denied")})
	require.NotNil(t, cmd)
	require.Empty(t, mErr.status)

	// A successful login confirms and refreshes plan/catalog.
	mOk, cmd := step(t, m, loginMsg{})
	require.NotNil(t, cmd)
	require.Empty(t, mOk.status)

	// /logout removes the stored credentials and drops the cached tier.
	writeCreds(t, &auth.Credentials{AccessToken: "x"})
	m, cmd = slash(m, "/logout")
	require.NotNil(t, cmd)
	require.Empty(t, m.tier)
	_, err := auth.LoadCredentials()
	require.Error(t, err) // file is gone
}

func TestModelPicker(t *testing.T) {
	m := newTestModel(t)

	// /model with no arg opens the picker (and fetches if catalog is empty).
	m, cmd := slash(m, "/model")
	require.Equal(t, modeModelPicker, m.mode)
	require.NotNil(t, cmd) // fetchInfo kicked off

	// Loading view while the catalog is empty.
	require.Contains(t, vw(m), "loading models")

	// Populate the catalog (as the async fetch would) and render the list.
	m.models = []llm.ModelInfo{
		{ID: "floko", Label: "Floko", Version: "3.2", MinTier: "free", Available: true},
		{ID: "chuppa", Label: "Chuppa", Version: "3.2", MinTier: "pro", Available: false},
	}
	require.Contains(t, vw(m), "Chuppa")
	require.Contains(t, vw(m), "🔒")

	// Down to the locked model, Enter -> not switched, explains the gate.
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, 1, m.modelIdx)
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, modeInput, m.mode)
	require.Equal(t, "floko", m.agent.Model()) // unchanged
	require.Contains(t, m.status, "pro plan")

	// Reopen, pick the available model -> switches.
	m, _ = slash(m, "/model")
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // idx 0 (floko, current) available
	require.Equal(t, "floko", m.agent.Model())
	require.Contains(t, m.status, "model: floko")

	// Esc cancels the picker.
	m.mode = modeModelPicker
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, modeInput, m.mode)
}

func TestModelSeedsFromAccountCache(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	require.NoError(t, account.Save(&account.Info{Tier: "pro", Models: []llm.ModelInfo{
		{ID: "floko", Label: "Floko", Version: "3.2", MinTier: "free", Available: true},
	}}))

	a := agent.New(&config.Config{Model: "floko"}, &auth.Credentials{AccessToken: "x"})
	m := newModel(context.Background(), a, session.New("floko", false))
	require.Equal(t, "pro", m.tier) // seeded from disk -> instant banner
	require.Len(t, m.models, 1)
	require.Contains(t, m.infoLine(), "Pro")
}

func TestInfoMsgRefreshSavesAndReprintsOnChange(t *testing.T) {
	m := newTestModel(t) // fresh XDG -> no cache
	floko := []llm.ModelInfo{{ID: "floko", Label: "Floko", Version: "3.2", MinTier: "free", Available: true}}

	// First refresh (nothing shown yet) prints and writes the cache.
	m, cmd := step(t, m, infoMsg{tier: "free", models: floko})
	require.NotNil(t, cmd)
	info, err := account.Load()
	require.NoError(t, err)
	require.Equal(t, "free", info.Tier)

	// Identical refresh -> no reprint.
	_, cmd = step(t, m, infoMsg{tier: "free", models: floko})
	require.Nil(t, cmd)

	// Changed (upgraded plan / new availability) -> reprint.
	both := append(floko, llm.ModelInfo{ID: "chuppa", Label: "Chuppa", Version: "3.2", MinTier: "pro", Available: true})
	m, cmd = step(t, m, infoMsg{tier: "pro", models: both})
	require.NotNil(t, cmd)
	require.Equal(t, "pro", m.tier)

	// A failed refresh (empty) keeps the cached display, no reprint.
	m2, cmd := step(t, m, infoMsg{})
	require.Nil(t, cmd)
	require.Equal(t, "pro", m2.tier)
}

func TestModelDirectArgStillWorks(t *testing.T) {
	m := newTestModel(t)
	m, _ = slash(m, "/model chuppa")
	require.Equal(t, "chuppa", m.agent.Model())
	require.Equal(t, modeInput, m.mode) // no picker when an arg is given
}

func TestTitleCaseAndCurrentIdx(t *testing.T) {
	require.Equal(t, "Free", titleCase("free"))
	require.Equal(t, "", titleCase(""))

	m := newTestModel(t)
	m.models = []llm.ModelInfo{{ID: "floko"}, {ID: "chuppa"}}
	m.agent.SetModel("chuppa")
	require.Equal(t, 1, m.currentModelIdx())
	m.agent.SetModel("nope")
	require.Equal(t, 0, m.currentModelIdx())
}

func TestSessionsCommandSwitchesSession(t *testing.T) {
	m := newTestModel(t) // isolates XDG_CONFIG_HOME (and stamps cwd on New)

	target := session.New("chuppa", true)
	target.Messages = []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "earlier work"},
		{Role: "assistant", Content: "did it"},
	}
	require.NoError(t, session.Save(target))

	// Keep target the only saved cwd session: nil out our own session so the
	// leaving-persist during /sessions is a no-op.
	m.sess = nil

	// /sessions with no id opens the picker listing this directory's sessions.
	mNo, cmd := slash(m, "/sessions")
	require.Nil(t, cmd)
	require.Equal(t, modeSessionPicker, mNo.mode)
	require.Len(t, mNo.sessList, 1)
	require.Contains(t, vw(mNo), target.ID)

	// Enter on the highlighted session attaches it.
	mPick, cmd := step(t, mNo, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	require.Equal(t, modeInput, mPick.mode)
	require.Equal(t, target.ID, mPick.sess.ID)

	// Esc cancels the picker without switching.
	mEsc, _ := slash(m, "/sessions")
	mEsc, _ = step(t, mEsc, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, modeInput, mEsc.mode)

	// /sessions <id> swaps the REPL to the target session.
	m2, cmd := slash(m, "/sessions "+target.ID)
	require.NotNil(t, cmd) // transcript replayed into scrollback
	require.Equal(t, target.ID, m2.sess.ID)
	require.Equal(t, "chuppa", m2.agent.Model())
	require.True(t, m2.agent.Think())
	require.Contains(t, m2.status, "attached")
	require.Equal(t, []string{"earlier work"}, m2.history) // history seeded from target

	// An unknown id reports an error and leaves the session unchanged.
	m3, cmd := slash(m2, "/sessions nope-not-real")
	require.NotNil(t, cmd)
	require.Equal(t, target.ID, m3.sess.ID)
}

func TestTrustPrompt(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // fresh: no trust recorded
	cwd, _ := os.Getwd()
	mk := func() model {
		a := agent.New(&config.Config{Model: "floko"}, &auth.Credentials{AccessToken: "x"})
		return newModel(context.Background(), a, session.New("floko", false))
	}

	// Untrusted dir → first-run trust prompt.
	m := mk()
	require.Equal(t, modeTrust, m.mode)
	require.Contains(t, vw(m), "Trust this directory")

	// Cancel quits without recording anything.
	mc, _ := step(t, m, key("3"))
	require.True(t, mc.quitting)
	_, ok := trust.Lookup(cwd)
	require.False(t, ok)

	// Choosing parent scope records it and proceeds to input.
	m2, cmd := step(t, mk(), key("2"))
	require.Equal(t, modeInput, m2.mode)
	require.NotNil(t, cmd) // prints the granted-scope line
	scope, ok := trust.Lookup(cwd)
	require.True(t, ok)
	require.Equal(t, trust.ScopeParent, scope)

	// Now trusted → a fresh model starts straight in input mode.
	require.Equal(t, modeInput, mk().mode)
}

func TestLearnCommandStartsTurn(t *testing.T) {
	m := newTestModel(t)
	mu, cmd := slash(m, "/learn")
	require.True(t, mu.streaming)                    // runs as a turn
	require.Equal(t, "high", mu.agent.Effort())      // learn reasons HARD — BORG.md is a write-once, high-leverage artifact
	require.Equal(t, "BORG.md", mu.agent.Artifact()) // completion guard armed: must write BORG.md before finishing
	require.NotNil(t, cmd)
}

func TestEffortCommand(t *testing.T) {
	m := newTestModel(t)

	m, _ = slash(m, "/effort high")
	require.Equal(t, "high", m.agent.Effort())
	require.Contains(t, m.status, "high")

	m, _ = slash(m, "/effort xhigh")
	require.Equal(t, "xhigh", m.agent.Effort())

	m, _ = slash(m, "/effort off")
	require.Empty(t, m.agent.Effort()) // back to following /think

	m, _ = slash(m, "/effort bogus")
	require.Contains(t, m.status, "usage") // rejected
}

func TestEffortPicker(t *testing.T) {
	m := newTestModel(t)
	m, _ = slash(m, "/effort high") // current level
	require.Equal(t, "high", m.agent.Effort())

	// /effort with no arg opens the picker, pre-selecting the current level.
	m, cmd := slash(m, "/effort")
	require.Nil(t, cmd)
	require.Equal(t, modeEffortPicker, m.mode)
	require.Equal(t, m.currentEffortIdx(), m.effortIdx)
	require.Contains(t, vw(m), "reasoning effort")
	require.Contains(t, vw(m), "(current)")

	// Up to "default" (idx 0), Enter -> follows /think again ("").
	for i := 0; i < len(effortLevels); i++ {
		m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	}
	require.Equal(t, 0, m.effortIdx)
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, modeInput, m.mode)
	require.Empty(t, m.agent.Effort())
	require.Contains(t, m.status, "default")

	// Reopen, Down to a concrete level, Enter -> switches.
	m, _ = slash(m, "/effort")
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown}) // idx 1 = "none"
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, "none", m.agent.Effort())

	// Esc cancels.
	m, _ = slash(m, "/effort")
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, modeInput, m.mode)
}

// cleanupSettingsEnv unsets the BORG_* vars SetSetting writes via os.Setenv (which
// t.Setenv wouldn't restore), so settings tests don't leak into one another.
func cleanupSettingsEnv(t *testing.T) {
	t.Cleanup(func() {
		for _, s := range config.Settings {
			_ = os.Unsetenv(s.Env)
		}
	})
}

func TestSettingsCommand(t *testing.T) {
	cleanupSettingsEnv(t)
	m := newTestModel(t)

	// Direct form: /settings <key> <value> persists + applies hot settings live.
	m, _ = slash(m, "/settings think on")
	require.True(t, m.agent.Think())
	require.Contains(t, m.status, "Reasoning")

	m, _ = slash(m, "/settings think off")
	require.False(t, m.agent.Think())

	// Enum validation surfaces a helpful error, leaving the value unchanged.
	m, _ = slash(m, "/settings escalate_model axiom")
	require.Contains(t, m.status, "axiom")
	m, _ = slash(m, "/settings escalate_model bogus")
	require.Contains(t, m.status, "must be one of")

	// Unknown key is rejected.
	m, _ = slash(m, "/settings nope x")
	require.Contains(t, m.status, "unknown setting")

	// The change is persisted to settings.json so a restart would pick it up.
	data, err := os.ReadFile(config.SettingsFilePath())
	require.NoError(t, err)
	require.Contains(t, string(data), "escalate_model")
}

func TestSettingsPicker(t *testing.T) {
	cleanupSettingsEnv(t)
	m := newTestModel(t)
	m.width = 100

	m, cmd := slash(m, "/settings")
	require.Nil(t, cmd)
	require.Equal(t, modeSettingsPicker, m.mode)
	require.Contains(t, vw(m), "settings —")

	// Enter on the first row (escalate_model, an enum) cycles it and stays open.
	require.Equal(t, "escalate_model", config.Settings[0].Key)
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, modeSettingsPicker, m.mode)
	require.Contains(t, m.status, "axiom")

	// Down to the bool "think" row and toggle it in place.
	thinkIdx := -1
	for i, s := range config.Settings {
		if s.Key == "think" {
			thinkIdx = i
		}
	}
	require.GreaterOrEqual(t, thinkIdx, 0)
	for i := 0; i < thinkIdx; i++ {
		m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	require.False(t, m.agent.Think())
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, modeSettingsPicker, m.mode)
	require.True(t, m.agent.Think())

	// Esc closes the picker.
	m, _ = slash(m, "/settings")
	m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, modeInput, m.mode)
}

func TestSessionPickerEmpty(t *testing.T) {
	m := newTestModel(t)
	m.sess = nil // no own session to save
	m, cmd := slash(m, "/sessions")
	require.Nil(t, cmd)
	require.Equal(t, modeInput, m.mode) // no picker with nothing to list
	require.Contains(t, m.status, "no saved sessions")
}

func TestSessionPickerWindowsAndSelects(t *testing.T) {
	m := newTestModel(t)
	m.sess = nil
	m.width = 100

	// Save more sessions than fit in one window so the picker scrolls.
	for i := 0; i < maxMenuRows+3; i++ {
		s := session.New("floko", false)
		s.Messages = []llm.Message{{Role: "user", Content: fmt.Sprintf("task %d", i)}}
		require.NoError(t, session.Save(s))
		time.Sleep(2 * time.Millisecond) // distinct LastActive for stable ordering
	}

	m, cmd := slash(m, "/sessions")
	require.Nil(t, cmd)
	require.Equal(t, modeSessionPicker, m.mode)
	require.Len(t, m.sessList, maxMenuRows+3)
	require.Contains(t, vw(m), "session(s)") // overflow footer

	// Down past the first window keeps the selection visible (windowing).
	for i := 0; i < maxMenuRows; i++ {
		m, _ = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	require.Equal(t, maxMenuRows, m.sessIdx)
	require.Contains(t, vw(m), m.sessList[m.sessIdx].ID)

	// Enter attaches the highlighted session.
	m, cmd = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	require.Equal(t, modeInput, m.mode)
	require.Equal(t, m.sess.ID, m.sessList[maxMenuRows].ID)
}

func TestThinkStatusIsDescriptive(t *testing.T) {
	m := newTestModel(t)
	m, _ = slash(m, "/think on")
	require.True(t, m.agent.Think())
	require.Contains(t, m.status, "thinking on")
	require.Contains(t, m.status, "reasons")

	m, _ = slash(m, "/think off")
	require.False(t, m.agent.Think())
	require.Contains(t, m.status, "thinking off")
}

func TestTruncate(t *testing.T) {
	require.Equal(t, "hello", truncate("hello", 0))  // width 0 = no clip
	require.Equal(t, "hello", truncate("hello", 10)) // fits
	require.Contains(t, truncate("hello world", 6), "…")
}

func TestAttachHintAndHistory(t *testing.T) {
	require.Empty(t, attachHint(nil))
	require.Empty(t, attachHint(&session.Session{})) // no real content
	require.Nil(t, historyFromSession(nil))

	s := &session.Session{ID: "abcd1234", Messages: []llm.Message{
		{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"},
	}}
	require.Contains(t, attachHint(s), "borg --attach abcd1234")
	require.Equal(t, []string{"hi"}, historyFromSession(s))
}

func TestHistorySeededFromResumedSession(t *testing.T) {
	m := newTestModel(t)
	m.sess.Messages = []llm.Message{{Role: "user", Content: "old prompt"}}
	m2 := newModel(context.Background(), m.agent, m.sess)
	require.Equal(t, []string{"old prompt"}, m2.history)
}

// reasoningLabel shows the --think toggle and the /effort level as two distinct
// footer fields; an unset effort reads as "default".
func TestReasoningLabel(t *testing.T) {
	require.Equal(t, "think:off · effort:high", reasoningLabel("high", false))
	require.Equal(t, "think:on · effort:low", reasoningLabel("low", true))
	require.Equal(t, "think:on · effort:default", reasoningLabel("", true))
	require.Equal(t, "think:off · effort:default", reasoningLabel("", false))
}

func TestSessionTokenLabel(t *testing.T) {
	require.Empty(t, sessionTokenLabel(0, 0))
	require.Equal(t, "session ↑ 1.2k ↓ 300", sessionTokenLabel(1200, 300))
}

func TestCreditBar(t *testing.T) {
	require.Empty(t, creditBar(nil))
	// Unlimited budget (0 per-day): an ∞ marker, no bar.
	require.Contains(t, creditBar(&llm.AccountUsage{CreditsUsed: 12}), "∞")
	// A real budget renders a fixed-width track followed by the percent number.
	bar := ansiRE.ReplaceAllString(creditBar(&llm.AccountUsage{CreditsPerDay: 500, PercentUsed: 25}), "")
	require.Contains(t, bar, "25%")
	require.Equal(t, creditBarWidth+len(" 25%"), len(bar)) // track + " NN%"
	// Clamped at the ends.
	require.Contains(t, ansiRE.ReplaceAllString(creditBar(&llm.AccountUsage{CreditsPerDay: 500, PercentUsed: 200}), ""), "100%")
}

func TestJoinLR(t *testing.T) {
	require.Empty(t, joinLR("a", "b", 0))
	// Right segment is pushed to the right edge.
	got := joinLR("left", "right", 20)
	require.Equal(t, 20, ansi.StringWidth(got))
	require.True(t, strings.HasPrefix(got, "left"))
	require.True(t, strings.HasSuffix(got, "right"))
	// Too narrow: the right segment (model/effort) is preserved; the left is
	// truncated to make room.
	got = joinLR("a very long left side", "right", 10)
	require.Equal(t, 10, ansi.StringWidth(got))
	require.True(t, strings.HasSuffix(got, "right"))
	// Right alone overflows: it's truncated to width.
	got = joinLR("left", "a very long right segment", 6)
	require.Equal(t, 6, ansi.StringWidth(got))
}

func TestGitBranch(t *testing.T) {
	require.Empty(t, gitBranch("")) // empty input
	dir := t.TempDir()
	require.Empty(t, gitBranch(dir)) // not a repo

	// A real .git dir with a HEAD ref, discovered from a subdirectory.
	gitDir := filepath.Join(dir, ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/staging\n"), 0o644))
	sub := filepath.Join(dir, "a", "b")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.Equal(t, "staging", gitBranch(sub))

	// Detached HEAD: show the short commit.
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("0123456789abcdef\n"), 0o644))
	require.Equal(t, "0123456", gitBranch(dir))

	// Worktree/submodule form: .git is a file pointing at the real git dir.
	wt := t.TempDir()
	realGit := filepath.Join(wt, "realgit")
	require.NoError(t, os.Mkdir(realGit, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(realGit, "HEAD"), []byte("ref: refs/heads/feat/x\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+realGit+"\n"), 0o644))
	require.Equal(t, "feat/x", gitBranch(wt))
}

// The settings picker pads its label column to the widest label, so every value
// is separated from its label by whitespace (regression: a 20/23-char label used
// to abut its value, e.g. "Reasoning by defaultoff", "...nudge25").
func TestSettingsPickerColumnsAlign(t *testing.T) {
	m := newTestModel(t)
	m.width = 400 // wide enough that nothing truncates
	plain := ansiRE.ReplaceAllString(m.settingsPickerView(), "")
	for _, s := range config.Settings {
		re := regexp.MustCompile(regexp.QuoteMeta(s.Label) + ` +` + regexp.QuoteMeta(s.Display()))
		require.Regexp(t, re, plain, "value must be space-separated from label %q", s.Label)
	}
	require.NotContains(t, plain, "defaultoff") // the two formerly-colliding rows
	require.NotContains(t, plain, "nudge25")
}

// The footer branch refreshes on discrete events (no polling): a checkout made
// after startup is picked up when the terminal regains focus (tea.FocusMsg). The
// refreshGitBranch helper is the shared seam (also called on submit + turn end).
func TestGitBranchRefreshOnFocus(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/old-branch\n"), 0o644))

	m := newTestModel(t)
	m.cwd = dir
	m.refreshGitBranch()
	require.Equal(t, "old-branch", m.gitBranch)

	// An external checkout, then a focus-regained event refreshes the footer.
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/staging\n"), 0o644))
	m, _ = step(t, m, tea.FocusMsg{})
	require.Equal(t, "staging", m.gitBranch)

	// View opts the terminal into focus reporting so the event actually arrives.
	require.True(t, m.View().ReportFocus)
}

// TestFooterShowsBranchTokensCredits checks the footer surfaces the git branch,
// the running session token total, and the live credit figure.
func TestFooterShowsBranchTokensCredits(t *testing.T) {
	m := newTestModel(t)
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 24})
	m.cwd = "/home/u/proj" // pin the path so footer width is env-independent (CI cwd is long)
	m.gitBranch = "staging"
	m.sessIn, m.sessOut = 1500, 400
	m.usage = &llm.AccountUsage{CreditsUsed: 7, CreditsPerDay: 500, PercentUsed: 3}

	v := vw(m)
	require.Contains(t, v, "(staging)")
	require.Contains(t, v, "session ↑ 1.5k ↓ 400")
	require.Contains(t, v, "3%") // budget shown as a percentage bar, not raw credits
	require.Contains(t, v, m.agent.Model())
	require.Contains(t, v, "think:off") // thinking state is always shown
}

// --- Run wrapper ---

func TestRunQuitsOnCancelledContext(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // program should quit promptly

	a := agent.New(&config.Config{Model: "floko"}, &auth.Credentials{AccessToken: "x"})
	// No TTY in CI/containers: inject a non-TTY input and skip the renderer.
	err := Run(ctx, a, session.New("floko", false),
		tea.WithInput(strings.NewReader("")), tea.WithoutRenderer())
	require.NoError(t, err)
}
