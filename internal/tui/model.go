package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	tkey "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/x/ansi"

	"github.com/turborg/borg/internal/account"
	"github.com/turborg/borg/internal/agent"
	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/llm"
	"github.com/turborg/borg/internal/session"
	"github.com/turborg/borg/internal/trust"
	"github.com/turborg/borg/internal/version"
)

var (
	prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("#2d5cf3")).Bold(true)
	tool   = lipgloss.NewStyle().Foreground(lipgloss.Color("#14c3a2"))
	dim    = lipgloss.NewStyle().Foreground(lipgloss.Color("#94a3b8"))
	errSty = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5d8f"))
	brand  = lipgloss.NewStyle().Foreground(lipgloss.Color("#2d5cf3")).Bold(true)
	// warn is amber: a caution state, not an error (red) and not nominal (dim).
	// Used for the footer's auto-approve chip — "prompts are off" is worth noticing.
	warn = lipgloss.NewStyle().Foreground(lipgloss.Color("#f0b833"))

	// Edit-diff preview colors: added lines green, removed red, hunk header dim —
	// the "git diff" of what an edit tool just changed, shown in scrollback.
	diffAdd = lipgloss.NewStyle().Foreground(lipgloss.Color("#3fb950"))
	diffDel = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5d8f"))

	// inputBG shades the input/working box into a borderless, visible gray box —
	// no border glyphs (which misaligned on resize / long input).
	inputBG = lipgloss.NewStyle().Background(lipgloss.Color("#363c4a"))

	// userSty highlights a submitted prompt in scrollback (bold, light-on-shade) so
	// it reads as clearly "mine" versus borg's reply below it.
	userSty = lipgloss.NewStyle().Foreground(lipgloss.Color("#e2e8f0")).Background(lipgloss.Color("#363c4a")).Bold(true)

	// Footer budget bar: the percent label is overlaid on a fixed-width track —
	// filled cells (teal, red near the cap) for the used portion, gray for the rest.
	barFill    = lipgloss.NewStyle().Background(lipgloss.Color("#14c3a2")).Foreground(lipgloss.Color("#0b1220")).Bold(true)
	barFillHot = lipgloss.NewStyle().Background(lipgloss.Color("#ff5d8f")).Foreground(lipgloss.Color("#0b1220")).Bold(true)
	barEmpty   = lipgloss.NewStyle().Background(lipgloss.Color("#475569")).Foreground(lipgloss.Color("#e2e8f0"))
)

// creditBarWidth is the budget bar's fixed column count (the percent label is
// centered within it).
const creditBarWidth = 12

// userEcho renders a submitted user prompt for scrollback — the blue chevron plus
// the highlighted prompt text — so the transcript clearly separates "my prompt"
// from "borg's thinking". A multi-line prompt highlights each line.
func userEcho(text string) string {
	var b strings.Builder
	for i, ln := range strings.Split(text, "\n") {
		if i > 0 {
			b.WriteString("\n  ") // align continuation lines under the text
		} else {
			b.WriteString(prompt.Render("›") + " ")
		}
		b.WriteString(userSty.Render(" " + ln + " "))
	}
	return b.String()
}

// Input-box geometry. The box is borderless and full-width: internal padding on
// every side (a vertical pad row above/below, a left gutter + "› " prompt, and a
// right gutter), with the text area in the middle. The textarea wraps + grows
// inside it. These account for every column/row so the box is fit to exact
// dimensions and can never wrap or misalign.
const (
	inputLeftPad   = 2                           // columns of padding before the prompt
	inputPromptW   = 2                           // the "› " prompt
	inputRightPad  = 2                           // columns of padding after the text
	inputPrefix    = inputLeftPad + inputPromptW // cursor X shift (left pad + prompt)
	inputMaxRows   = 8                           // cap the box's growth (it scrolls internally beyond this)
	inputTopMargin = 1                           // blank rows above the box (gap from the banner/scrollback)
	inputPadRows   = 1                           // blank shaded rows inside the box, above + below the text
)

// inputTextWidth is the textarea's own content width so a wrapped line fits the
// box exactly: the terminal minus the left pad, the prompt, and the right pad.
// Clamped so a tiny terminal stays valid.
func inputTextWidth(termWidth int) int {
	w := termWidth - inputPrefix - inputRightPad
	if w < 8 {
		w = 8
	}
	return w
}

// fitWidth forces s to exactly w printable columns — truncating (ellipsis) if too
// wide, right-padding with spaces if too short — so the shaded line never wraps
// or leaves a ragged right edge. ANSI-aware, so embedded styling is preserved.
func fitWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "…")
	if pad := w - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// padTo right-pads s with spaces to a visible width of w (never truncates), for
// aligning picker columns to their widest entry. ANSI-aware via StringWidth, but
// callers should pad the PLAIN text and style the result, since trailing spaces
// inside a style span are invisible anyway.
func padTo(s string, w int) string {
	if d := w - ansi.StringWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// shade applies bg as a continuous background across s, re-asserting it after
// every SGR reset the inner styling emits (a plain bg wrap would otherwise lose
// the background between the chevron's reset and the text — verified). This is
// what makes the borderless box a solid, gap-free bar regardless of content.
func shade(s string, bg lipgloss.Style) string {
	s = strings.ReplaceAll(s, "\x1b[0m", "\x1b[m")
	parts := strings.Split(s, "\x1b[m")
	for i, p := range parts {
		parts[i] = bg.Render(p)
	}
	return strings.Join(parts, "")
}

// shadeLine fits one content line to the full terminal width and shades it — the
// unit the box is built from, deterministic at any width or content length.
func shadeLine(s string, termWidth int) string {
	return shade(fitWidth(s, termWidth), inputBG)
}

// inputBoxLines wraps content lines (each already carrying its left pad + prompt
// prefix) in the shaded box: every row fit to the terminal width and shaded so
// the box is a solid, gap-free rectangle that grows with the content.
func inputBoxLines(lines []string, termWidth int) string {
	var b strings.Builder
	for i, ln := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(shadeLine(ln, termWidth))
	}
	return b.String()
}

// reasoningLabel summarizes the active reasoning settings for the footer as two
// distinct fields, so the user can always see both the `--think` toggle and the
// `/effort` level: "think:on · effort:high". An unset effort reads as "default"
// (it follows the think toggle / the model's own default level).
func reasoningLabel(effort string, think bool) string {
	t := "off"
	if think {
		t = "on"
	}
	e := effort
	if e == "" {
		e = "default"
	}
	return "think:" + t + " · effort:" + e
}

// footerView renders the status footer under the input box: the working
// directory + git branch + this session's running token total + a live budget
// percentage bar on the left, and the active model + reasoning level pushed to
// the right edge. lpad is the box's left gutter so the footer lines up under it.
func (m model) footerView(lpad string) string {
	left := dim.Render(tildeDir(m.cwd))
	if m.gitBranch != "" {
		left += " " + tool.Render("("+m.gitBranch+")")
	}
	if s := sessionTokenLabel(m.sessIn, m.sessOut); s != "" {
		left += dim.Render("  " + s)
	}
	if s := contextLabel(m.ctxTokens, m.agent.ContextWindow()); s != "" {
		left += "  " + s
	}
	if s := creditBar(m.usage); s != "" {
		left += "  " + s
	}
	right := dim.Render(m.agent.Model() + " · " + reasoningLabel(m.agent.Effort(), m.agent.Think()))
	// Flag an auto-approving session in the footer: when the permission prompt is
	// off, the only remaining signal that a mutating tool ran is this chip, so it's
	// always visible while auto-approve is on. Warn-colored on purpose.
	if m.agent.AutoApprove() {
		right = warn.Render("auto-approve") + dim.Render(" · ") + right
	}
	return lpad + joinLR(left, right, m.width-inputLeftPad)
}

// sessionTokenLabel sums every turn's tokens this session into one "↑in ↓out"
// figure, so the user can see the whole conversation's cost at a glance (the
// per-turn figure still streams in the working indicator). Empty until used.
func sessionTokenLabel(in, out int) string {
	if in == 0 && out == 0 {
		return ""
	}
	return "session ↑ " + fmtTokens(in) + " ↓ " + fmtTokens(out)
}

// creditBar renders the live rolling-24h budget as a compact percentage bar for
// the footer — humans read "3% used" faster than "38/1250 cr" (the raw figures
// stay in /usage and /status). "" until usage is fetched; an unlimited budget
// (0 per-day) shows an ∞ marker instead of a bar. The fill turns red near the
// cap so an imminent exhaustion is obvious.
func creditBar(u *llm.AccountUsage) string {
	if u == nil {
		return ""
	}
	if u.CreditsPerDay <= 0 {
		return dim.Render("budget ∞")
	}
	pct := u.PercentUsed
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}
	filled := (pct*creditBarWidth + 50) / 100 // round to nearest cell
	fill := barFill
	if pct >= 90 {
		fill = barFillHot
	}
	track := fill.Render(strings.Repeat(" ", filled)) + barEmpty.Render(strings.Repeat(" ", creditBarWidth-filled))
	// Percent number printed after the bar.
	return track + dim.Render(fmt.Sprintf(" %d%%", pct))
}

// joinLR lays left and right on one line of exactly width columns, right-aligning
// right. The right segment (model + reasoning) is the priority: when they don't
// both fit, the left is truncated to keep the right fully visible; only if the
// right alone overflows is it itself truncated. So the footer never wraps and the
// model/effort never vanish on a narrow terminal. ANSI-width aware.
func joinLR(left, right string, width int) string {
	if width <= 0 {
		return ""
	}
	rw := ansi.StringWidth(right)
	if rw >= width {
		return truncate(right, width)
	}
	avail := width - rw - 1 // reserve at least one space before the right segment
	if ansi.StringWidth(left) > avail {
		left = truncate(left, avail)
	}
	gap := width - ansi.StringWidth(left) - rw
	return left + strings.Repeat(" ", gap) + right
}

// gitBranch resolves the current branch for dir (walking up to the repo root),
// or "" when dir isn't inside a git work tree. It reads .git/HEAD directly (no
// subprocess) so it's cheap enough to re-read at discrete events (see
// refreshGitBranch) — never on a timer, so the idle prompt still does zero work.
func gitBranch(dir string) string {
	if dir == "" {
		return ""
	}
	for {
		gitPath := filepath.Join(dir, ".git")
		info, err := os.Stat(gitPath)
		switch {
		case err == nil && info.IsDir():
			return headBranch(gitPath)
		case err == nil:
			// A worktree/submodule: .git is a file "gitdir: <path>".
			if data, err := os.ReadFile(gitPath); err == nil {
				if gd, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: "); ok {
					return headBranch(gd)
				}
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// headBranch reads <gitDir>/HEAD and returns the branch name (or a short commit
// for a detached HEAD), "" when it can't be read.
func headBranch(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(data))
	if ref, ok := strings.CutPrefix(head, "ref: refs/heads/"); ok {
		return ref
	}
	if len(head) >= 7 { // detached HEAD: show the short commit
		return head[:7]
	}
	return head
}

// refreshGitBranch re-reads the current branch into the footer cache. It's a single
// .git/HEAD read (no subprocess), driven only by discrete events — the terminal
// regaining focus (tea.FocusMsg, catches a checkout made in another window), a
// prompt submit, and each turn-step end (catches a checkout borg itself ran via
// bash). No timer/poll, so the idle prompt keeps doing zero work between keystrokes.
func (m *model) refreshGitBranch() { m.gitBranch = gitBranch(m.cwd) }

// tildeDir abbreviates the home directory to ~ for the footer path.
func tildeDir(dir string) string {
	if dir == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if dir == home {
			return "~"
		}
		if strings.HasPrefix(dir, home+string(os.PathSeparator)) {
			return "~" + dir[len(home):]
		}
	}
	return dir
}

// mode is the model's current input state.
type mode int

const (
	modeInput              mode = iota // accepting a task / slash command
	modePermit                         // awaiting y/n/a for a mutating tool
	modeAskUser                        // awaiting the user's pick for an ask_user question
	modeConfirmPurge                   // awaiting y/N for /purge
	modeModelPicker                    // choosing a model from the catalog
	modeEffortPicker                   // choosing a reasoning-effort level
	modeSessionPicker                  // choosing one of this directory's sessions to switch to
	modeSettingsPicker                 // viewing/toggling persistent settings
	modeTrust                          // first-run: grant directory access (1/2/3)
	modeConfirmRetroLearn              // post-thrash: awaiting y/N to append a lesson to BORG.md
	modeConfirmRetroReport             // post-thrash: awaiting y/N to report a harness problem to the team
)

// model is borg's REPL as a Bubble Tea model, run inline (no alt screen):
// finished turns are flushed to scrollback via tea.Printf, while the live region
// renders only the in-progress turn + the input prompt.
type model struct {
	ctx   context.Context
	agent *agent.Agent
	sess  *session.Session

	ti spinnerInput
	md *mdRenderer

	width     int
	streaming bool
	mode      mode

	buf      string // current step's streamed assistant text (string, not a
	toolNote string // strings.Builder: Bubble Tea copies the model by value)

	turnIn    int       // exact input tokens accumulated over the current turn's steps
	turnOut   int       // exact output tokens accumulated over completed steps this turn
	turnStart time.Time // when the current turn began, for the live "working… (Ns)" timer

	sessIn  int // running input-token total across every turn this session (footer)
	sessOut int // running output-token total across every turn this session (footer)

	ctxTokens  int  // exact prompt tokens from the most recent step (current context occupancy)
	compacting bool // a /compact summarization call is in flight

	// Launch mascot: bannerBlink >= 0 while the eyes blink in the live region
	// (frame index into blinkSchedule); -1 once settled into scrollback. bannerIntro
	// is the text under the mascot (version line + cached info + any resumed
	// transcript), flushed together with the settled mascot.
	bannerBlink int
	bannerIntro string

	purgeCount int // sessions that /purge would delete (shown in the confirm modal)

	retro    *agent.Retro    // a pending post-thrash retrospective awaiting the user's y/N
	struggle *agent.Struggle // the struggle snapshot behind a pending retro (for report metadata)

	permitName  string
	permitReply chan agent.Decision

	askReq      agent.AskRequest     // the in-flight ask_user question (modeAskUser)
	askIdx      int                  // highlighted row in the modal (last row = "something else")
	askReply    chan agent.AskResult // delivers the user's answer back to the agent goroutine
	askFreeText bool                 // collecting a typed free-text answer for the pending ask

	status     string // transient one-liner (e.g. "model: chuppa")
	quitting   bool
	cwd        string             // working directory (for the trust prompt)
	gitBranch  string             // current git branch for cwd ("" when not a repo), for the footer
	usage      *llm.AccountUsage  // latest live credit usage, refreshed after each turn (footer)
	turnCancel context.CancelFunc // cancels the in-flight turn (Esc interrupts)

	menuIdx       int      // highlighted entry in the slash-command menu
	menuDismissed bool     // Esc hides the menu until the input changes again
	history       []string // submitted inputs this session (for Up/Down recall)
	histIdx       int      // cursor into history; len(history) == "new line"

	queued []string // follow-ups typed while a turn streams; auto-submitted in order when it ends

	pastes   map[int]string // multi-line pastes, shown as a compact placeholder and
	pasteSeq int            // expanded on submit; pasteSeq mints the placeholder ids

	tier     string          // the caller's plan code (free/starter/pro/max)
	models   []llm.ModelInfo // catalog (labels, versions, availability), once fetched
	modelIdx int             // highlighted entry in the model picker

	effortIdx int // highlighted entry in the effort picker

	sessList []session.Meta // saved sessions listed in the attach picker
	sessIdx  int            // highlighted entry in the attach picker

	settingsIdx int // highlighted entry in the /settings picker
}

// spinnerInput groups the two bubbles widgets the model drives.
type spinnerInput struct {
	in textarea.Model
	sp spinner.Model
}

// infoMsg carries the asynchronously-fetched plan tier + model catalog.
type infoMsg struct {
	tier   string
	models []llm.ModelInfo
}

// usageMsg carries the asynchronously-fetched account usage (or a fetch error,
// which falls back to static plan caps).
type usageMsg struct {
	usage *llm.AccountUsage
	err   error
	quiet bool // footer refresh: store the usage but don't print the /usage panel
}

// statusMsg carries the asynchronously-fetched /status snapshot: the logged-in
// user and the live usage. Either may be nil if its fetch failed (the panel
// degrades gracefully rather than erroring).
type statusMsg struct {
	user  *llm.UserInfo
	usage *llm.AccountUsage
}

// loginMsg reports the outcome of an interactive /login.
type loginMsg struct{ err error }

func newModel(ctx context.Context, a *agent.Agent, sess *session.Session) model {
	in := textarea.New()
	in.Placeholder = "ask " + version.Command() + " to do something…  (/ for commands)"
	in.Prompt = "" // the chevron + padding are drawn around the textarea, not by it
	in.ShowLineNumbers = false
	in.CharLimit = 0
	// Grow with the content (wrap a long line onto more rows, like Claude/Codex),
	// capped so a huge paste can't eat the screen — it scrolls internally instead.
	in.DynamicHeight = true
	in.MinHeight = 1
	in.MaxHeight = inputMaxRows
	// Use the *real* terminal cursor (not a drawn glyph): View() hands its
	// position to Bubble Tea and the terminal blinks it natively, with zero
	// program repaints — so scrollback stays copyable at an idle prompt.
	in.SetVirtualCursor(false)
	// Enter submits (handled in onKey); a literal newline is shift+enter / ctrl+j.
	in.KeyMap.InsertNewline = tkey.NewBinding(tkey.WithKeys("shift+enter", "ctrl+j"))
	// Neutralize the widget's own styling; the box shades + colors itself.
	st := textarea.DefaultStyles(true)
	plain := lipgloss.NewStyle()
	for _, ss := range []*textarea.StyleState{&st.Focused, &st.Blurred} {
		ss.Base, ss.Text, ss.CursorLine, ss.EndOfBuffer, ss.Prompt = plain, plain, plain, plain, plain
		ss.Placeholder = dim
	}
	in.SetStyles(st)
	in.SetWidth(inputTextWidth(defaultWidth))
	in.Focus()

	sp := spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(dim))

	// Seed history with this session's prior user inputs (so a resumed session
	// recalls them too).
	history := historyFromSession(sess)

	// Seed plan/catalog from the on-disk cache so the banner paints instantly;
	// fetchInfo refreshes it in the background.
	var tier string
	var models []llm.ModelInfo
	if info, err := account.Load(); err == nil {
		tier, models = info.Tier, info.Models
		a.SetModelWindows(models) // seed /context windows from the cached catalog
	}

	// Resolve directory trust: apply a prior decision, or prompt on first run.
	cwd, _ := os.Getwd()
	startMode := modeInput
	if scope, ok := trust.Lookup(cwd); ok {
		a.SetTrustRoot(trust.Root(cwd, scope))
	} else {
		startMode = modeTrust
	}

	// Seed the running token/context display from the (possibly resumed) session so
	// an attach shows its accumulated usage, not a fresh 0.
	var sessIn, sessOut, ctxTokens int
	if sess != nil {
		sessIn, sessOut, ctxTokens = sess.TokensIn, sess.TokensOut, sess.ContextTokens
	}

	m := model{
		ctx:       ctx,
		agent:     a,
		sess:      sess,
		ti:        spinnerInput{in: in, sp: sp},
		md:        newMDRenderer(),
		width:     defaultWidth,
		history:   history,
		histIdx:   len(history),
		tier:      tier,
		models:    models,
		cwd:       cwd,
		gitBranch: gitBranch(cwd),
		mode:      startMode,
		sessIn:    sessIn,
		sessOut:   sessOut,
		ctxTokens: ctxTokens,
	}

	// Compose the text under the mascot once, so the launch blink can flush the
	// mascot + this tail to scrollback together (and Init can print it instantly
	// on the no-animation path).
	m.bannerIntro = brand.Render(version.Command()) +
		dim.Render(" "+version.Version+"  — AI coding agent · model "+m.agent.Model()+" · /help")
	if info := m.infoLine(); info != "" {
		m.bannerIntro += "\n" + info
	}
	// On resume, replay the saved conversation into scrollback so the prior context
	// is visible (it's already loaded into the agent for the model). A resumed
	// session skips the launch blink (bannerBlink = -1) so the attach is instant;
	// a fresh REPL blinks the eyes a couple times (bannerBlink starts at 0).
	if tr := replayTranscript(m.sess, m.md, m.width); tr != "" {
		m.bannerIntro += "\n" + dim.Render("— attached session "+m.sess.ID+" —") + "\n" + tr
		m.bannerBlink = -1
	}
	return m
}

// mascotMinWidth is the narrowest terminal at which the live (blinking) mascot
// is drawn; below it the launch animation is skipped so a tiny terminal's redraw
// region can never overflow (the width-safe guarantee). The settled banner still
// prints to scrollback like the long intro line, which was never width-clamped.
const mascotMinWidth = 16

// blinkSchedule are the inter-frame delays for the launch blink: the eyes close
// and open twice, then settle. Frame N's eye state is "closed" when N is odd; the
// animation ends once bannerBlink reaches len(blinkSchedule). Two quick blinks
// (~1s total) give the droid life at startup, then it rests static — no idle
// repaint (the speed/low-footprint rules).
var blinkSchedule = []time.Duration{
	240 * time.Millisecond, // open  → closed
	150 * time.Millisecond, // closed → open
	320 * time.Millisecond, // open  → closed
	150 * time.Millisecond, // closed → open
	260 * time.Millisecond, // open  → settle (flush to scrollback)
}

// blinkMsg advances the launch-blink animation one frame.
type blinkMsg struct{}

// blinkCmd schedules the next blink frame from the current bannerBlink index.
func (m model) blinkCmd() tea.Cmd {
	if m.bannerBlink < 0 || m.bannerBlink >= len(blinkSchedule) {
		return nil
	}
	return tea.Tick(blinkSchedule[m.bannerBlink], func(time.Time) tea.Msg { return blinkMsg{} })
}

// mascotFrame renders the "Visor Humanoid" — borg's terminal mascot — with the
// eyes open (resting) or closed (mid-blink). The nameplate echoes
// version.Command() so it reads "turborg" or "borg" depending on which name
// launched the binary. Drawn once into scrollback at startup (after a brief
// launch blink); never animated at idle.
//
//	 ┌─────────┐
//	 │ ▢     ▢ │   square eyes (teal); blink → ▬     ▬
//	 │░░░░░░░░░│   scanning visor band (dim)
//	 └──┬───┬──┘   head + shoulders (brand blue)
//	═╤══╧═══╧══╤═
//	   turborg     nameplate (brand blue)
func mascotFrame(blink bool) string {
	eye := "▢"
	if blink {
		eye = "▬"
	}
	name := version.Command()
	// Centre the nameplate under the figure (centre column ≈ 8 in the art below).
	pad := 8 - len(name)/2
	if pad < 0 {
		pad = 0
	}
	var b strings.Builder
	b.WriteString("   " + brand.Render("┌─────────┐") + "\n")
	b.WriteString("   " + brand.Render("│") + " " + tool.Render(eye) + "     " + tool.Render(eye) + " " + brand.Render("│") + "\n")
	b.WriteString("   " + brand.Render("│") + dim.Render("░░░░░░░░░") + brand.Render("│") + "\n")
	b.WriteString("   " + brand.Render("└──┬───┬──┘") + "\n")
	b.WriteString("  " + brand.Render("═╤══╧═══╧══╤═") + "\n")
	b.WriteString(strings.Repeat(" ", pad) + brand.Render(name) + "\n")
	return b.String()
}

// settleBanner flushes the settled mascot + intro to scrollback and stops the
// launch animation. Called when the blink finishes or the user interacts first
// (a keystroke/paste), so an early command never prints above the still-live
// mascot. A no-op once already settled.
func (m *model) settleBanner() tea.Cmd {
	if m.bannerBlink < 0 {
		return nil
	}
	m.bannerBlink = -1
	return tea.Printf("%s", mascotFrame(false)+m.bannerIntro)
}

func (m model) Init() tea.Cmd {
	async := tea.Batch(m.fetchInfo(), m.fetchUsage(true), m.checkStaleness(), m.checkUpdate())
	// Resumed sessions (and reduced-width terminals) skip the animation: the
	// banner + replayed transcript print at once so an attach is instant.
	if m.bannerBlink < 0 {
		return tea.Batch(tea.Printf("%s", mascotFrame(false)+m.bannerIntro), async)
	}
	// Fresh REPL: View() draws the mascot in the live region while it blinks; the
	// blink ticker advances frames and flushes the settled banner to scrollback.
	return tea.Batch(m.blinkCmd(), async)
}

// fetchInfo loads the caller's plan tier and the model catalog off the event
// loop (a network round-trip), delivering an infoMsg.
func (m model) fetchInfo() tea.Cmd {
	return func() tea.Msg {
		tier, _ := m.agent.Tier(m.ctx)
		models, _ := m.agent.Models(m.ctx)
		return infoMsg{tier: tier, models: models}
	}
}

// fetchUsage loads the account's live usage off the event loop. When quiet, the
// result only refreshes the footer's live credit figure (no scrollback print) —
// that's how the footer stays current after each turn; the /usage command uses
// quiet=false to also print the panel.
func (m model) fetchUsage(quiet bool) tea.Cmd {
	return func() tea.Msg {
		u, err := m.agent.Usage(m.ctx)
		return usageMsg{usage: u, err: err, quiet: quiet}
	}
}

// fetchStatus loads the logged-in user + live usage off the event loop for
// /status. A failed sub-fetch yields a nil field; the renderer handles it.
func (m model) fetchStatus() tea.Cmd {
	return func() tea.Msg {
		user, _ := m.agent.UserInfo(m.ctx)
		usage, _ := m.agent.Usage(m.ctx)
		return statusMsg{user: user, usage: usage}
	}
}

// login runs the OAuth flow off the event loop, reporting the result on loginMsg.
func (m model) login() tea.Cmd {
	return func() tea.Msg {
		return loginMsg{err: m.agent.Login(m.ctx)}
	}
}

// compact runs the conversation summarization off the event loop (a metered LLM
// call), delivering the result on compactMsg.
func (m model) compact() tea.Cmd {
	return func() tea.Msg {
		res, err := m.agent.Compact(m.ctx)
		return compactMsg{res: res, err: err}
	}
}

// infoLine renders the one-time plan + catalog summary shown after startup.
func (m model) infoLine() string {
	if m.tier == "" && len(m.models) == 0 {
		return ""
	}
	var b strings.Builder
	// No tier off-platform (there are no plans to be on), so the plan chunk is
	// dropped rather than rendered as an empty "plan:".
	if m.tier != "" {
		b.WriteString(dim.Render("plan: ") + brand.Render(titleCase(m.tier)))
	}
	for _, mi := range m.models {
		mark := tool.Render("✓")
		if !m.modelAvailable(mi) {
			mark = errSty.Render("🔒 needs " + mi.MinTier)
		}
		cur := ""
		if mi.ID == m.agent.Model() {
			cur = dim.Render("  (current)")
		}
		b.WriteString("\n  " + tool.Render(fmt.Sprintf("%-7s", mi.Label)) +
			"  " + dim.Render("v"+mi.Version) + "   " + mark + cur)
	}
	return b.String()
}

// replayTranscript renders a resumed session's user prompts, assistant replies
// (as markdown), and tool calls for display in scrollback. System and raw tool
// result messages are omitted.
func replayTranscript(sess *session.Session, md *mdRenderer, width int) string {
	if sess == nil {
		return ""
	}
	var b strings.Builder
	add := func(s string) {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s)
	}
	for _, msg := range sess.Messages {
		switch msg.Role {
		case "user":
			add(userEcho(msg.Content))
		case "assistant":
			if msg.Content != "" {
				add(strings.TrimRight(md.render(msg.Content, width), "\n"))
			}
			for _, tc := range msg.ToolCalls {
				add(tool.Render("⚙ "+tc.Function.Name) + " " + dim.Render(summarize(tc.Function.Arguments)))
			}
		}
	}
	return b.String()
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.ti.in.SetWidth(inputTextWidth(msg.Width))
		return m, nil

	case tea.FocusMsg:
		// Terminal regained focus — a branch switch in another window is the common
		// reason the footer would be stale, so re-read it now (one tiny file read).
		m.refreshGitBranch()
		return m, nil

	case staleMsg:
		// BORG.md drifted far enough behind HEAD — nudge once at startup to re-run
		// /learn. Zero-LLM, git-only; disabled/inapplicable cases arrive as nudge=false.
		if !msg.nudge {
			return m, nil
		}
		return m, tea.Printf("%s", dim.Render(staleNudgeText(msg.behind)))

	case updateMsg:
		// A newer release is published — nudge once at startup (mirrors staleMsg).
		// Throttled to a daily network check by the selfupdate cache.
		if !msg.newer {
			return m, nil
		}
		return m, tea.Printf("%s", dim.Render(updateNudgeText(msg.latest)))

	case updateDoneMsg:
		switch {
		case msg.upToDate:
			m.status = version.Command() + " is already up to date (" + msg.version + ")"
		case msg.err != nil:
			m.status = "update failed: " + msg.err.Error()
		default:
			m.status = "updated to " + msg.version + " — restart " + version.Command() + " to use it"
		}
		return m, nil

	case spinner.TickMsg:
		if !m.streaming {
			return m, nil
		}
		var cmd tea.Cmd
		m.ti.sp, cmd = m.ti.sp.Update(msg)
		return m, cmd

	case infoMsg:
		// A failed refresh (offline / not logged in) keeps the cached display.
		if msg.tier == "" && len(msg.models) == 0 {
			return m, nil
		}
		old := m.infoLine()
		m.tier, m.models = msg.tier, msg.models
		m.agent.SetModelWindows(m.models)                               // refresh /context windows from the live catalog
		_ = account.Save(&account.Info{Tier: m.tier, Models: m.models}) // best-effort
		line := m.infoLine()
		// Only (re)print when the truth differs from what's already on screen —
		// the common case (cache matched) costs nothing.
		if line == "" || line == old {
			return m, nil
		}
		if old != "" {
			line = dim.Render("↻ ") + line // mark an update to the cached banner
		}
		return m, tea.Printf("%s", line)

	case usageMsg:
		// Keep the latest usage for the footer's live credit figure.
		if msg.usage != nil {
			m.usage = msg.usage
		}
		if msg.quiet {
			return m, nil // background footer refresh: don't touch the status line
		}
		m.status = "" // clear the "fetching usage…" placeholder before showing the panel
		return m, tea.Printf("%s", m.renderUsage(msg))

	case statusMsg:
		m.status = "" // clear the "fetching status…" placeholder before showing the panel
		return m, tea.Printf("%s", m.renderStatus(msg))

	case loginMsg:
		m.status = ""
		if msg.err != nil {
			return m, tea.Printf("%s", errSty.Render("login failed: "+msg.err.Error()))
		}
		// Re-fetch the plan/catalog for the new identity (repaints the banner if
		// it changed) and confirm the switch.
		return m, tea.Batch(
			tea.Printf("%s", tool.Render("✓ ")+dim.Render("logged in")),
			m.fetchInfo(),
		)

	case thinkingMsg:
		// A new step's model call is starting: clear the previous step's tool label
		// so the working line shows "working…" (the model is THINKING, often for
		// many seconds) instead of a stale "running read_file…" — the reads finished
		// in milliseconds; what's slow is the model, and the line must say so.
		m.toolNote = ""
		return m, nil

	case deltaMsg:
		m.buf += string(msg)
		return m, nil

	case toolBatchMsg:
		return m, tea.Printf("%s", dim.Render(fmt.Sprintf("⚡ %d tools in parallel", msg.n)))

	case toolCallMsg:
		m.toolNote = msg.name
		return m, tea.Printf("%s", tool.Render("⚙ "+agent.ToolCallLine(msg.name, msg.args)))

	case toolResultMsg:
		mark, summary := tool.Render("✓"), dim.Render(msg.summary)
		if !msg.ok {
			mark, summary = errSty.Render("✗"), errSty.Render(msg.summary)
		}
		return m, tea.Printf("  %s %s", mark, summary)

	case toolDiffMsg:
		return m, tea.Printf("%s", renderDiff(string(msg), m.width))

	case debugMsg:
		// Bound the on-screen debug block: a verbose dump (a whole file's numbered
		// contents, a long reasoning trace) would otherwise wrap and flood the inline
		// renderer, colliding with the live working line. Cap rows + truncate each to
		// the terminal width; the FULL trace is in ~/.config/borg/logs.
		return m, tea.Printf("%s", dim.Render(clampDebug(string(msg), m.width)))

	case assistantEndMsg:
		// Fold this step's exact usage into the turn totals; the live counter
		// then reconciles from estimate to exact as each step completes.
		m.turnIn += msg.stats.InTokens
		m.turnOut += msg.stats.OutTokens
		m.sessIn += msg.stats.InTokens
		m.sessOut += msg.stats.OutTokens
		// The latest step's prompt size is the current context occupancy (each step
		// re-sends the whole conversation) — drives the footer bar + near-full warning.
		if msg.stats.InTokens > 0 {
			m.ctxTokens = msg.stats.InTokens
		}
		// A step may have run `git checkout`/`switch`/`commit` via bash — keep the
		// footer branch current without polling.
		m.refreshGitBranch()
		return m, m.finishStep(msg.stats)

	case permitMsg:
		m.mode = modePermit
		m.permitName = msg.name
		m.permitReply = msg.reply
		return m, nil

	case askMsg:
		m.mode = modeAskUser
		m.askReq = msg.req
		m.askIdx = 0
		m.askReply = msg.reply
		return m, nil

	case turnDoneMsg:
		m.streaming = false
		m.toolNote = ""
		m.turnCancel = nil
		m.persist()
		// Refresh the footer's live credit figure now that the turn billed.
		cmds := []tea.Cmd{m.fetchUsage(true)}
		switch {
		case errors.Is(msg.err, context.Canceled):
			// Esc (or Ctrl-C mid-turn) cancels the context: flush whatever streamed
			// so far, then acknowledge the interrupt rather than printing an error.
			if flush := m.finishStep(agent.Stats{}); flush != nil {
				cmds = append(cmds, flush)
			}
			cmds = append(cmds, tea.Printf("%s", dim.Render("⊘ Interrupted — tell me what to do differently, or try again.")))
		case msg.err != nil:
			cmds = append(cmds, tea.Printf("%s", errSty.Render("error: "+msg.err.Error())))
		}
		// A follow-up typed while this turn ran takes priority: start it now. (It
		// supersedes the post-thrash retrospective, which would be dropped anyway
		// once the next turn starts streaming.)
		if len(m.queued) > 0 {
			next, cmd := m.dequeue()
			return next, tea.Batch(append(cmds, cmd)...)
		}
		// The turn completed cleanly but tripped a recovery guard — reflect once
		// (off the event loop) on whether a BORG.md note or a harness report helps.
		if msg.err == nil && m.agent.LastStruggle() != nil {
			cmds = append(cmds, tea.Printf("%s", dim.Render("· that was a rough one — reflecting on how to make it smoother…")), m.retrospectCmd())
		}
		return m, tea.Batch(cmds...)

	case compactMsg:
		m.streaming = false
		m.compacting = false
		m.toolNote = ""
		if msg.err != nil {
			return m, tea.Printf("%s", errSty.Render("compact failed: "+msg.err.Error()))
		}
		// The conversation was replaced by a recap: the live token counters reset,
		// and the next turn re-measures the (smaller) context exactly.
		m.ctxTokens = 0
		m.sessIn, m.sessOut = 0, 0
		m.persist()
		return m, tea.Printf("%s", m.renderCompact(msg.res))

	case retroMsg:
		// Drop a late reflection if a new turn already started (don't pop a modal
		// mid-stream); the struggle for that turn is gone anyway.
		if m.streaming {
			m.agent.ClearStruggle()
			return m, nil
		}
		m.struggle = m.agent.LastStruggle() // snapshot pointer before clearing (for report meta)
		m.agent.ClearStruggle()             // handled — don't re-offer for this turn
		if msg.err != nil || msg.retro == nil || strings.TrimSpace(msg.retro.Text) == "" {
			return m, nil // reflection failed or had nothing actionable — stay quiet
		}
		switch msg.retro.Kind {
		case agent.RetroKindBorgMD:
			m.retro = msg.retro
			m.mode = modeConfirmRetroLearn
		case agent.RetroKindHarness:
			m.retro = msg.retro
			m.mode = modeConfirmRetroReport
		}
		return m, nil

	case retroDoneMsg:
		m.retro = nil
		m.mode = modeInput
		return m, tea.Printf("%s", retroDoneLine(msg))

	case blinkMsg:
		if m.bannerBlink < 0 {
			return m, nil // already settled (e.g. the user interacted first)
		}
		m.bannerBlink++
		if m.bannerBlink >= len(blinkSchedule) {
			return m, m.settleBanner() // eyes-open mascot + intro → scrollback
		}
		return m, m.blinkCmd()

	case tea.PasteMsg:
		// Settle the launch mascot first so an early paste lands below it, not above.
		settle := m.settleBanner()
		nm, cmd := m.onPaste(msg)
		return nm, tea.Batch(settle, cmd)

	case tea.KeyPressMsg:
		// Settle the launch mascot first so an early command echoes below it.
		settle := m.settleBanner()
		nm, cmd := m.onKey(msg)
		return nm, tea.Batch(settle, cmd)
	}

	// Default: feed the input widget (cursor blink etc.) when idle.
	if m.mode == modeInput && !m.streaming {
		var cmd tea.Cmd
		m.ti.in, cmd = m.ti.in.Update(msg)
		return m, cmd
	}
	return m, nil
}

// onKey routes a keypress by mode.
func (m model) onKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Free-text entry for an ask_user prompt borrows the normal input box (mode
	// stays modeInput with streaming paused), so intercept its keys first.
	if m.askFreeText {
		return m.onAskFreeTextKey(msg)
	}
	switch m.mode {
	case modeTrust:
		return m.onTrustKey(msg)
	case modePermit:
		return m.onPermitKey(msg)
	case modeAskUser:
		return m.onAskKey(msg)
	case modeConfirmPurge:
		return m.onPurgeKey(msg)
	case modeConfirmRetroLearn, modeConfirmRetroReport:
		return m.onRetroKey(msg)
	case modeModelPicker:
		return m.onModelPickerKey(msg)
	case modeEffortPicker:
		return m.onEffortPickerKey(msg)
	case modeSessionPicker:
		return m.onSessionPickerKey(msg)
	case modeSettingsPicker:
		return m.onSettingsPickerKey(msg)
	}

	matches := m.menu()
	// While a turn streams the input stays live for a follow-up, but the slash
	// menu is suppressed (it's not rendered then) — so navigation/Enter act on the
	// queued text, not a hidden menu.
	menuOpen := len(matches) > 0 && !m.streaming

	switch msg.String() {
	case "ctrl+c":
		m.quitting = true
		m.persist()
		return m, m.quitCmd()
	case "ctrl+o":
		// Expand the last tool's FULL output to scrollback (inline shows a one-line
		// preview). Works mid-turn too — it flushes above the live region.
		return m, m.printLastToolOutput()
	case "esc":
		// While a turn is running, Esc interrupts it (without quitting); the
		// cancelled context surfaces as turnDoneMsg, which prints "Interrupted".
		if m.streaming {
			if m.turnCancel != nil {
				m.turnCancel()
			}
			return m, nil
		}
		// Otherwise dismiss the menu but keep the typed text.
		if menuOpen {
			m.menuDismissed = true
		}
		return m, nil
	case "enter":
		if m.streaming {
			// A turn is in flight — don't drop the input, queue it as a follow-up
			// that's auto-submitted when the current turn (and any earlier queued
			// items) finish. This is how borg stays usable while it works.
			return m.queueInput()
		}
		// With the menu open, Enter runs the highlighted command.
		if menuOpen {
			m.ti.in.SetValue(matches[clampIdx(m.menuIdx, len(matches))].name)
		}
		return m.submit()
	case "up":
		// Up navigates the open menu, otherwise recalls older history.
		if menuOpen {
			m.menuIdx = clampIdx(m.menuIdx-1, len(matches))
			return m, nil
		}
		return m.recall(-1), nil
	case "down":
		if menuOpen {
			m.menuIdx = clampIdx(m.menuIdx+1, len(matches))
			return m, nil
		}
		return m.recall(+1), nil
	case "tab":
		// Tab completes the highlighted command, leaving the cursor ready for args.
		if menuOpen {
			m.ti.in.SetValue(matches[clampIdx(m.menuIdx, len(matches))].name + " ")
			m.ti.in.CursorEnd()
			m.menuIdx = 0
		}
		return m, nil
	}

	// Any other key edits the line: leave history browsing and re-activate the menu.
	var cmd tea.Cmd
	m.ti.in, cmd = m.ti.in.Update(msg)
	m.histIdx = len(m.history)
	m.menuIdx = 0
	m.menuDismissed = false
	return m, cmd
}

// onPaste handles bracketed paste. A multi-line paste is replaced with a compact
// "[Pasted Text #id: N lines]" placeholder (the full text is buffered and
// expanded on submit) so it can't flood or wrap the single-line input; a
// single-line paste is inserted normally.
func (m model) onPaste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	// Terminals send "\r" (or "\r\n") for newlines in bracketed paste — normalize
	// so multi-line pastes are detected (and stored cleanly) regardless.
	content := strings.ReplaceAll(strings.ReplaceAll(msg.Content, "\r\n", "\n"), "\r", "\n")
	if m.mode != modeInput || m.streaming || !strings.Contains(content, "\n") {
		var cmd tea.Cmd
		m.ti.in, cmd = m.ti.in.Update(tea.PasteMsg{Content: content})
		return m, cmd
	}
	lines := len(strings.Split(strings.TrimRight(content, "\n"), "\n"))
	if m.pastes == nil {
		m.pastes = map[int]string{}
	}
	m.pasteSeq++
	m.pastes[m.pasteSeq] = content
	placeholder := fmt.Sprintf("[Pasted Text #%d: %d lines]", m.pasteSeq, lines)
	var cmd tea.Cmd
	m.ti.in, cmd = m.ti.in.Update(tea.PasteMsg{Content: placeholder})
	// Treat it like any edit: leave history browsing and re-activate the menu.
	m.histIdx = len(m.history)
	m.menuIdx = 0
	m.menuDismissed = false
	return m, cmd
}

// menu returns the active slash-command matches for the current input, or nil
// when there's no "/command" being composed or the menu was dismissed (Esc).
func (m model) menu() []slashCmd {
	if m.menuDismissed {
		return nil
	}
	return matchingCommands(m.ti.in.Value())
}

// recall steps through input history: dir<0 = older, dir>0 = newer. Stepping
// past the newest restores an empty line.
func (m model) recall(dir int) model {
	if len(m.history) == 0 {
		return m
	}
	idx := m.histIdx + dir
	switch {
	case idx < 0:
		idx = 0
	case idx >= len(m.history):
		m.histIdx = len(m.history)
		m.ti.in.SetValue("")
		m.ti.in.CursorEnd()
		return m
	}
	m.histIdx = idx
	m.ti.in.SetValue(m.history[idx])
	m.ti.in.CursorEnd()
	return m
}

// clampIdx keeps a selection index within [0, n).
func clampIdx(i, n int) int {
	switch {
	case n == 0:
		return 0
	case i < 0:
		return 0
	case i >= n:
		return n - 1
	default:
		return i
	}
}

// validEffort reports whether s is an accepted /effort level.
func validEffort(s string) bool {
	switch s {
	case "none", "low", "medium", "high", "xhigh", "off":
		return true
	}
	return false
}

// effortLevel is one selectable reasoning-effort level shown in the picker, with
// a human label and a one-line explanation of the trade-off it makes.
type effortLevel struct{ value, label, desc string }

// effortLevels drives the /effort picker. value is what's stored on the agent
// ("" follows the /think toggle); the order is low→high so ↑/↓ reads naturally.
var effortLevels = []effortLevel{
	{"", "default", "follow the /think toggle (the model's own default level)"},
	{"none", "none", "no reasoning — fastest and cheapest"},
	{"low", "low", "a little thinking before answering"},
	{"medium", "medium", "balanced reasoning"},
	{"high", "high", "deep reasoning — better planning & tool batching"},
	{"xhigh", "xhigh", "maximum reasoning — slowest, most thorough"},
}

// currentEffortIdx is the index of the agent's current effort in effortLevels
// (0 = "default" when unset/unknown), used to pre-select it in the picker.
func (m model) currentEffortIdx() int {
	for i, e := range effortLevels {
		if e.value == m.agent.Effort() {
			return i
		}
	}
	return 0
}

// truncate clips s (which may contain ANSI styling) to width columns with a "…"
// tail, leaving it untouched when width is unset (≤0) so tests and the first
// pre-WindowSizeMsg paint render in full.
func truncate(s string, width int) string {
	if width <= 0 {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

// maxDebugRows caps how many lines of one debug block reach the screen. Debug
// dumps (a whole file's numbered contents, a long reasoning trace) are otherwise
// unbounded and flood the inline renderer; the full trace lives in the session log.
const maxDebugRows = 8

// clampDebug formats a debug block for scrollback: each line prefixed and
// truncated to width (so a long line can't wrap and corrupt the live region),
// capped at maxDebugRows with a "+N lines" pointer to the on-disk log. The full,
// untruncated trace is always written to ~/.config/borg/logs by the agent.
func clampDebug(s string, width int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	extra := 0
	if len(lines) > maxDebugRows {
		extra = len(lines) - maxDebugRows
		lines = lines[:maxDebugRows]
	}
	for i, ln := range lines {
		lines[i] = truncate("  · "+ln, width)
	}
	if extra > 0 {
		lines = append(lines, truncate(fmt.Sprintf("  · …(+%d lines — full trace in ~/.config/borg/logs)", extra), width))
	}
	return strings.Join(lines, "\n")
}

// renderDiff colors an edit tool's unified diff for scrollback: added lines green,
// removed red, the file/hunk headers dim. Each line is indented and truncated to
// the terminal width so it can't wrap and corrupt the inline render region.
func renderDiff(diff string, width int) string {
	var b strings.Builder
	for _, ln := range strings.Split(strings.TrimRight(diff, "\n"), "\n") {
		var sty lipgloss.Style
		switch {
		case strings.HasPrefix(ln, "+++") || strings.HasPrefix(ln, "---") || strings.HasPrefix(ln, "@@"):
			sty = dim
		case strings.HasPrefix(ln, "+"):
			sty = diffAdd
		case strings.HasPrefix(ln, "-"):
			sty = diffDel
		default:
			sty = dim
		}
		b.WriteString(sty.Render(truncate("  "+ln, width)) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// slashCmd is one REPL command shown in the live menu / Tab completion.
type slashCmd struct{ name, desc string }

var slashCmds = []slashCmd{
	{"/model", "pick a model (interactive list, or /model <name>)"},
	{"/think", "toggle reasoning [on|off]"},
	{"/effort", "pick reasoning effort (interactive list, or /effort <level>)"},
	{"/sessions", "switch session in this directory (interactive list, or /sessions <id>)"},
	{"/learn", "study this project and write BORG.md"},
	{"/context", "show context-window usage + a breakdown"},
	{"/compact", "summarize the conversation to free up context"},
	{"/settings", "view & change persistent settings (or /settings <key> <value>)"},
	{"/upgrade", "upgrade your plan (opens xShellz pricing page)"},
	{"/usage", "show plan, quota caps, and usage"},
	{"/status", "show login, plan, usage, and settings"},
	{"/login", "log in to xShellz (browser or device)"},
	{"/logout", "remove the stored credentials"},
	{"/privacy", "how borg handles your data + policy link"},
	{"/output", "show the last tool's FULL output (or ctrl+o)"},
	{"/clear", "reset the conversation"},
	{"/purge", "delete ALL saved sessions"},
	{"/update", "update to the latest release"},
	{"/help", "show detailed help"},
	{"/exit", "quit (the session is saved)"},
}

// maxMenuRows is the slash menu's fixed height. The menu always renders this
// many rows while open (a window of matches around the selection, blank-padded)
// — Bubble Tea's inline renderer drops lines when the live region *shrinks*, so
// a constant height keeps every match visible as you filter. The height only
// collapses when the menu closes entirely.
const maxMenuRows = 6

// menuView renders the fixed-height slash-command menu around the selected
// index, clipping each row to width so a narrow terminal doesn't wrap a row into
// two lines (which would break the inline renderer's fixed-height assumption).
func menuView(matches []slashCmd, sel, width int) string {
	if len(matches) == 0 {
		return ""
	}
	start := 0
	if len(matches) > maxMenuRows {
		start = clampIdx(sel-maxMenuRows/2, len(matches)-maxMenuRows+1)
	}
	var b strings.Builder
	for row := 0; row < maxMenuRows; row++ {
		b.WriteString("\n")
		i := start + row
		if i >= len(matches) {
			continue // blank padding row keeps the block height constant
		}
		c := matches[i]
		marker, name, desc := "  ", tool.Render(fmt.Sprintf("%-12s", c.name)), dim.Render(c.desc)
		if i == sel {
			// Highlight the whole row: brighten the description (plain default fg)
			// too, not just the command name, so the selection reads as one unit.
			marker, name, desc = prompt.Render("› "), brand.Render(fmt.Sprintf("%-12s", c.name)), c.desc
		}
		b.WriteString(truncate(marker+name+" "+desc, width))
	}
	return b.String()
}

// matchingCommands returns the commands whose name starts with the typed token,
// while the user is still composing the command name (no space yet). Typing just
// "/" lists them all.
func matchingCommands(val string) []slashCmd {
	if !strings.HasPrefix(val, "/") || strings.Contains(val, " ") {
		return nil
	}
	val = strings.ToLower(val) // tolerate "/M" etc.
	var out []slashCmd
	for _, c := range slashCmds {
		if strings.HasPrefix(c.name, val) {
			out = append(out, c)
		}
	}
	return out
}

// knownCommand reports whether tok is a dispatchable slash command (including
// the back-compat aliases). It drives the prompt's live affordance: once a full
// command name is typed, the chevron switches color to signal "this will run".
func knownCommand(tok string) bool {
	if tok == "/quit" { // alias for /exit, not listed in slashCmds
		return true
	}
	for _, c := range slashCmds {
		if c.name == tok {
			return true
		}
	}
	return false
}

// currentModelIdx is the catalog index of the agent's current model (0 if not
// found / catalog empty), used to pre-select it in the picker.
func (m model) currentModelIdx() int {
	for i, mi := range m.models {
		if mi.ID == m.agent.Model() {
			return i
		}
	}
	return 0
}

// burnHintThreshold is the relative burn rate at/above which switching to a model
// warns that it spends the shared daily budget faster (cheapest model = 1).
const burnHintThreshold = 2

// modelInfoByID returns the catalog entry for a codename, or nil when unknown
// (catalog not fetched yet, or a stale codename).
func (m model) modelInfoByID(id string) *llm.ModelInfo {
	for i := range m.models {
		if m.models[i].ID == id {
			return &m.models[i]
		}
	}
	return nil
}

// modelSwitchStatus is the status line shown after switching to model id: a
// budget-burn warning when the target spends the shared daily budget materially
// faster than the cheapest model (burn_rate ≥ threshold), else a plain confirmation.
func (m model) modelSwitchStatus(id string) string {
	if mi := m.modelInfoByID(id); mi != nil && mi.BurnRate >= burnHintThreshold {
		label := mi.Label
		if label == "" {
			label = id
		}
		return fmt.Sprintf("⚠ %s spends your daily AI budget ≈%d× faster — save it for the hard tasks", label, mi.BurnRate)
	}
	return "model: " + id
}

// tierRank orders plan tiers so model availability can be derived from the
// caller's tier vs a model's min_tier. Unknown/empty tiers rank lowest (free).
func tierRank(tier string) int {
	switch strings.ToLower(tier) {
	case "starter":
		return 1
	case "pro":
		return 2
	case "max":
		return 3
	default: // free, "", unknown
		return 0
	}
}

// modelAvailable reports whether the caller's plan can use mi, computed from the
// tier vs the model's min_tier — not the catalog's `available` flag, which can
// be stale or not plan-gated server-side. Before the tier is known, only
// no-minimum (free) models read as available, so paid models never show as
// unlocked by mistake.
func (m model) modelAvailable(mi llm.ModelInfo) bool {
	return tierRank(m.tier) >= tierRank(mi.MinTier)
}

// titleCase upper-cases the first rune (free -> Free); "" stays "".
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// onTrustKey answers the first-run directory-trust prompt: 1 = this dir, 2 = the
// parent too, 3/Esc/Ctrl-C = cancel (quit). The choice is persisted per-dir.
func (m model) onTrustKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	var scope trust.Scope
	switch msg.String() {
	case "1":
		scope = trust.ScopeDir
	case "2":
		scope = trust.ScopeParent
	case "3", "esc", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	default:
		return m, nil // ignore other keys until a choice is made
	}
	_ = trust.Record(m.cwd, scope)
	root := trust.Root(m.cwd, scope)
	m.agent.SetTrustRoot(root)
	m.mode = modeInput
	return m, tea.Printf("%s", dim.Render("borg may edit files in "+root))
}

// trustView renders the first-run directory-trust prompt.
func (m model) trustView() string {
	var b strings.Builder
	b.WriteString(errSty.Render("Trust this directory? ") +
		dim.Render("borg can read and edit files within the scope you grant.") + "\n")
	b.WriteString("  " + prompt.Render("[1]") + " Trust this directory       " + dim.Render(m.cwd) + "\n")
	b.WriteString("  " + prompt.Render("[2]") + " Also allow the parent dir  " + dim.Render(filepath.Dir(m.cwd)) + "\n")
	b.WriteString("  " + prompt.Render("[3]") + " Cancel " + dim.Render("(quit)"))
	return b.String()
}

func (m model) onPermitKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	d := agent.DenyOnce
	switch strings.ToLower(msg.String()) {
	case "a":
		d = agent.AllowAlways
	case "y":
		d = agent.AllowOnce
	}
	if m.permitReply != nil {
		m.permitReply <- d
		m.permitReply = nil
	}
	m.mode = modeInput
	m.permitName = ""
	return m, nil
}

// askRows is the number of selectable rows in the modal: the model's options
// plus the built-in "something else" (free-text) row appended at the end.
func (m model) askRows() int { return len(m.askReq.Options) + 1 }

// onAskKey drives the ask_user modal: ↑/↓ (or 1-9) move/select a row, Enter
// confirms the highlighted one, Esc skips. Choosing a listed option hands its
// label straight back; choosing the last "something else" row opens a free-text
// box so the user can answer in their own words.
func (m model) onAskKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	n := m.askRows()
	switch s := msg.String(); s {
	case "up", "ctrl+p":
		m.askIdx = clampIdx(m.askIdx-1, n)
		return m, nil
	case "down", "ctrl+n":
		m.askIdx = clampIdx(m.askIdx+1, n)
		return m, nil
	case "esc":
		return m.sendAsk(agent.AskResult{})
	case "enter":
		return m.chooseAskRow(clampIdx(m.askIdx, n))
	default:
		// A digit 1-9 selects that row directly (options, then the free-text row).
		if len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
			if i := int(s[0] - '1'); i < n {
				return m.chooseAskRow(i)
			}
		}
		return m, nil
	}
}

// chooseAskRow acts on the selected modal row: a listed option resolves the ask
// immediately; the final "something else" row opens the free-text box instead.
func (m model) chooseAskRow(i int) (tea.Model, tea.Cmd) {
	if i < len(m.askReq.Options) {
		return m.sendAsk(agent.AskResult{Choice: m.askReq.Options[i].Label})
	}
	// "Something else" — pause the streaming display and reuse the normal input box
	// (mode stays modeInput) to collect a typed answer; the agent stays blocked.
	m.mode = modeInput
	m.askFreeText = true
	m.streaming = false
	m.ti.in.Reset()
	m.status = "Type your answer for borg (Enter to send · Esc to go back to the options)"
	return m, nil
}

// onAskFreeTextKey handles typing the free-text answer to an ask_user prompt:
// Enter sends it (as a Freeform result), Esc returns to the picker, everything
// else edits the input line.
func (m model) onAskFreeTextKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		text := strings.TrimSpace(m.ti.in.Value())
		if text == "" {
			return m, nil // nothing typed yet — wait
		}
		m.ti.in.Reset()
		m.askFreeText = false
		mm, cmd := m.sendAsk(agent.AskResult{Choice: text, Freeform: true})
		// Streaming was paused for the text box, so the spinner tick loop died —
		// restart it now that the turn resumes.
		return mm, tea.Batch(cmd, mm.(model).ti.sp.Tick)
	case "esc":
		// Back to the options without losing them; the agent is still blocked. The
		// tick loop died while streaming was paused for the text box, so restart it.
		m.ti.in.Reset()
		m.askFreeText = false
		m.status = ""
		m.mode = modeAskUser
		m.streaming = true
		return m, m.ti.sp.Tick
	default:
		var cmd tea.Cmd
		m.ti.in, cmd = m.ti.in.Update(msg)
		return m, cmd
	}
}

// sendAsk delivers the user's ask_user answer to the waiting agent goroutine,
// closes the modal, resumes the turn's streaming display, and echoes the outcome
// to scrollback so the Q&A is preserved in the transcript.
func (m model) sendAsk(res agent.AskResult) (tea.Model, tea.Cmd) {
	if m.askReply != nil {
		m.askReply <- res
		m.askReply = nil
	}
	m.mode = modeInput
	m.askReq = agent.AskRequest{}
	m.askIdx = 0
	m.status = ""
	m.streaming = true // the agent goroutine continues the turn from here
	var echo string
	switch {
	case res.Freeform:
		echo = dim.Render("❯ you said: ") + res.Choice
	case res.Choice != "":
		echo = dim.Render("❯ you chose: ") + tool.Render(res.Choice)
	default:
		echo = dim.Render("❯ dismissed — borg will use its best judgment")
	}
	// No spinner Tick here: on a direct pick the turn never paused, so the tick
	// loop is still alive — restarting it would double the ticker. The free-text
	// path (which did pause streaming) restarts it at its call site.
	return m, tea.Printf("%s", echo)
}

// onModelPickerKey drives the interactive model list: Up/Down move, Enter
// selects an available model, Esc cancels.
func (m model) onModelPickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up":
		m.modelIdx = clampIdx(m.modelIdx-1, len(m.models))
	case "down":
		m.modelIdx = clampIdx(m.modelIdx+1, len(m.models))
	case "esc":
		m.mode = modeInput
	case "enter":
		m.mode = modeInput
		if len(m.models) == 0 {
			return m, nil
		}
		sel := m.models[clampIdx(m.modelIdx, len(m.models))]
		if !m.modelAvailable(sel) {
			m.status = fmt.Sprintf("%s needs the %s plan", sel.Label, sel.MinTier)
			return m, nil
		}
		m.agent.SetModel(sel.ID)
		m.persist()
		m.status = m.modelSwitchStatus(sel.ID)
	}
	return m, nil
}

// onEffortPickerKey drives the interactive effort list: Up/Down move, Enter
// applies the highlighted level, Esc cancels.
func (m model) onEffortPickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up":
		m.effortIdx = clampIdx(m.effortIdx-1, len(effortLevels))
	case "down":
		m.effortIdx = clampIdx(m.effortIdx+1, len(effortLevels))
	case "esc":
		m.mode = modeInput
	case "enter":
		m.mode = modeInput
		sel := effortLevels[clampIdx(m.effortIdx, len(effortLevels))]
		m.agent.SetEffort(sel.value)
		m.persist()
		if sel.value == "" {
			m.status = "effort: default (follows /think)"
		} else {
			m.status = "effort: " + sel.value
		}
	}
	return m, nil
}

// onSessionPickerKey drives the saved-session list: Up/Down move, Enter switches
// to the highlighted session, Esc cancels.
func (m model) onSessionPickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up":
		m.sessIdx = clampIdx(m.sessIdx-1, len(m.sessList))
	case "down":
		m.sessIdx = clampIdx(m.sessIdx+1, len(m.sessList))
	case "esc":
		m.mode = modeInput
	case "enter":
		m.mode = modeInput
		if len(m.sessList) == 0 {
			return m, nil
		}
		sel := m.sessList[clampIdx(m.sessIdx, len(m.sessList))]
		return m.attachSession([]string{"/sessions", sel.ID})
	}
	return m, nil
}

// onSettingsPickerKey drives the persistent-settings list: Up/Down move, Enter
// changes the highlighted setting (bool toggles / enum cycles in place; a string
// drops to the input prefilled for editing), Esc closes.
func (m model) onSettingsPickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up":
		m.settingsIdx = clampIdx(m.settingsIdx-1, len(config.Settings))
	case "down":
		m.settingsIdx = clampIdx(m.settingsIdx+1, len(config.Settings))
	case "esc":
		m.mode = modeInput
	case "enter":
		s := config.Settings[clampIdx(m.settingsIdx, len(config.Settings))]
		if s.Kind == config.KindString {
			// Strings can't be picked from a list — prefill the command so the user
			// edits the value and submits it.
			m.mode = modeInput
			m.ti.in.SetValue("/settings " + s.Key + " " + s.Effective())
			m.ti.in.CursorEnd()
			return m, nil
		}
		return m.editSetting(s.Key) // bool toggle / enum cycle, staying in the picker
	}
	return m, nil
}

// editSetting handles the no-value forms (/settings <key>, or Enter in the
// picker): toggle a bool, cycle an enum, or prefill a string for editing.
func (m model) editSetting(key string) (tea.Model, tea.Cmd) {
	s, ok := config.SettingByKey(key)
	if !ok {
		m.status = "settings: unknown setting " + key
		return m, nil
	}
	switch s.Kind {
	case config.KindBool:
		on, _ := strconv.ParseBool(s.Effective())
		return m.applySetting(key, strconv.FormatBool(!on))
	case config.KindEnum:
		return m.applySetting(key, s.NextEnum(s.Effective()))
	default: // string: prefill the input with the current value for editing
		m.mode = modeInput
		m.ti.in.SetValue("/settings " + key + " " + s.Effective())
		m.ti.in.CursorEnd()
		return m, nil
	}
}

// applySetting persists a setting (settings.json), applies it live to the agent
// when it's a "hot" setting, and reports the outcome — including a warning when an
// exported BORG_* var shadows the saved value.
func (m model) applySetting(key, value string) (tea.Model, tea.Cmd) {
	s, ok := config.SettingByKey(key)
	if !ok {
		m.status = "settings: unknown setting " + key
		return m, nil
	}
	norm, shadow, err := config.SetSetting(key, strings.Trim(value, `"'`))
	if err != nil {
		m.status = "settings: " + err.Error()
		return m, nil
	}
	if s.Hot {
		m.agent.ApplySetting(key, norm)
	}
	status := s.Label + ": " + s.DisplayValue(norm)
	switch {
	case shadow:
		status += "  (saved, but " + s.Env + " is set in your environment and overrides it)"
	case !s.Hot:
		status += "  (applies on restart)"
	}
	m.status = status
	return m, nil
}

// settingsPickerView renders the persistent-settings list: each row shows the
// label, its current value, and a 🔒 marker when an exported BORG_* var overrides
// the file. The highlighted row is changed on Enter.
func (m model) settingsPickerView() string {
	var b strings.Builder
	b.WriteString(dim.Render("settings — ↑/↓, Enter to change, Esc:"))
	sel := clampIdx(m.settingsIdx, len(config.Settings))
	// Size the label/value columns to their widest entry (+gap) so every value and
	// description lines up, no matter the label length.
	labelW, valueW := 0, 0
	for _, s := range config.Settings {
		labelW, valueW = max(labelW, ansi.StringWidth(s.Label)), max(valueW, ansi.StringWidth(s.Display()))
	}
	labelW, valueW = labelW+2, valueW+2
	for i, s := range config.Settings {
		label, val := padTo(s.Label, labelW), padTo(s.Display(), valueW)
		marker, labelR, descR := "  ", tool.Render(label), dim.Render(s.Desc)
		if i == sel {
			marker, labelR, descR = prompt.Render("› "), brand.Render(label), s.Desc
		}
		row := marker + labelR + brand.Render(val) + descR
		if config.IsShadowed(s.Env) {
			row += errSty.Render("  🔒 env") // trails the row, so it can't shift the columns
		}
		b.WriteString("\n" + truncate(row, m.width))
	}
	return b.String()
}

// retroView renders the post-thrash consent prompt, showing the EXACT text that
// will be written to BORG.md or sent to the borg team — full transparency, nothing
// leaves the machine (or touches BORG.md) without the user seeing it and pressing y.
func (m model) retroView() string {
	if m.retro == nil {
		return ""
	}
	var b strings.Builder
	if m.mode == modeConfirmRetroLearn {
		b.WriteString(tool.Render("borg struggled on that task. ") +
			dim.Render("Add this note to BORG.md so future sessions go smoother?\n\n"))
		b.WriteString(dim.Render("  " + strings.ReplaceAll(m.retro.Text, "\n", "\n  ")))
		b.WriteString("\n\n" + dim.Render("Append to BORG.md? ") + tool.Render("[y/N]"))
		return b.String()
	}
	b.WriteString(tool.Render("borg hit a limitation it can't fix on its own. ") +
		dim.Render("This would be sent to the borg team to help improve borg — nothing else, only what you see:\n\n"))
	b.WriteString(dim.Render("  " + strings.ReplaceAll(m.retro.Text, "\n", "\n  ")))
	b.WriteString("\n\n" + dim.Render("Send this report to the borg team? ") + tool.Render("[y/N]"))
	return b.String()
}

// retrospectCmd runs the post-thrash reflection off the event loop; its result
// arrives as retroMsg.
func (m model) retrospectCmd() tea.Cmd {
	input := m.agent.RetrospectInput() // snapshot conversation state on the event loop
	if input == "" {
		return nil
	}
	terminal := false // terminal give-up → report path; soft thrash → BORG.md path
	if s := m.agent.LastStruggle(); s != nil {
		terminal = s.Terminal
	}
	return func() tea.Msg {
		r, err := m.agent.ReflectOn(m.ctx, input, terminal)
		return retroMsg{retro: r, err: err}
	}
}

// onRetroKey handles the y/N for both post-thrash prompts: append a lesson to
// BORG.md, or report a harness problem to the team. Anything but 'y' declines —
// nothing is written or sent without an explicit yes.
func (m model) onRetroKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	learn := m.mode == modeConfirmRetroLearn
	retro := m.retro
	struggle := m.struggle
	if strings.ToLower(msg.String()) != "y" {
		m.mode = modeInput
		m.retro, m.struggle = nil, nil
		return m, tea.Printf("%s", dim.Render("· no problem — left BORG.md and the borg team out of it"))
	}
	m.mode = modeInput
	m.retro, m.struggle = nil, nil
	if retro == nil {
		return m, nil
	}
	if learn {
		note := retro.Text
		return m, func() tea.Msg {
			if err := m.agent.ApplyRetroLearn(note); err != nil {
				return retroDoneMsg{err: fmt.Errorf("couldn't update BORG.md: %w", err)}
			}
			return retroDoneMsg{status: "added that lesson to BORG.md for future sessions"}
		}
	}
	report := retro.Text
	return m, func() tea.Msg {
		if err := m.agent.SubmitHarnessReport(m.ctx, report, struggle); err != nil {
			return retroDoneMsg{err: err, report: true}
		}
		return retroDoneMsg{status: "report sent to the borg team — thank you for helping improve borg"}
	}
}

// retroDoneLine renders the outcome of applying a retrospective. A failed harness
// report is best-effort: keep it calm (dim, not red) and don't echo a raw transport
// error — the report is optional and nothing the user did is affected. A failed
// local BORG.md write is actionable, so it stays a red error.
func retroDoneLine(msg retroDoneMsg) string {
	if msg.err != nil {
		if msg.report {
			return dim.Render("· couldn't reach the borg feedback service — no problem, it's optional")
		}
		return errSty.Render("· " + msg.err.Error())
	}
	return dim.Render("· " + msg.status)
}

func (m model) onPurgeKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.mode = modeInput
	if strings.ToLower(msg.String()) != "y" {
		return m, tea.Printf("%s", dim.Render("purge cancelled"))
	}
	n, err := session.Purge()
	if err != nil {
		return m, tea.Printf("%s", errSty.Render("purge failed: "+err.Error()))
	}
	return m, tea.Printf("%s", dim.Render(fmt.Sprintf("purged %d session(s)", n)))
}

// submit handles Enter in input mode: dispatch a slash command or start a turn.
func (m model) submit() (tea.Model, tea.Cmd) {
	input := strings.TrimSpace(m.ti.in.Value())
	m.ti.in.Reset()
	m.status = ""
	m.menuIdx = 0
	m.menuDismissed = false
	if input == "" {
		return m, nil
	}
	// You're acting on the repo now — make sure the footer branch is current (catches
	// a checkout made outside borg since the last event). One tiny .git/HEAD read.
	m.refreshGitBranch()
	// Record the displayed input (placeholders intact) for Up/Down recall.
	if n := len(m.history); n == 0 || m.history[n-1] != input {
		m.history = append(m.history, input)
	}
	m.histIdx = len(m.history)
	if strings.HasPrefix(input, "/") {
		return m.command(input)
	}

	// Expand any [Pasted Text …] placeholders to their full content, echoing that
	// full text into scrollback (so you see what you actually sent). Drop the
	// buffered pastes afterward so they don't accumulate.
	task := m.expandPastes(input)
	m.pastes = nil
	echo := tea.Printf("%s", userEcho(task))
	m.streaming = true
	m.buf = ""
	m.turnIn, m.turnOut = 0, 0 // reset the live token counter for the new turn
	return m, tea.Batch(echo, m.ti.sp.Tick, m.startTurn(m.beginTurn(), task))
}

// queueInput stashes a follow-up typed while a turn is streaming. It's submitted
// automatically once the current turn (and any earlier queued items) finish — so
// the prompt stays usable while borg works, the way Claude Code queues messages.
// The queued text is echoed to scrollback (marked "queued") so it's clearly
// captured, not lost.
func (m model) queueInput() (tea.Model, tea.Cmd) {
	input := strings.TrimSpace(m.ti.in.Value())
	if input == "" {
		return m, nil
	}
	m.ti.in.Reset()
	m.menuIdx = 0
	m.menuDismissed = false
	m.queued = append(m.queued, input)
	// Record it for Up/Down recall, like a submitted prompt.
	if n := len(m.history); n == 0 || m.history[n-1] != input {
		m.history = append(m.history, input)
	}
	m.histIdx = len(m.history)
	return m, tea.Printf("%s\n%s", dim.Render("⏳ queued — borg will get to this after the current turn"), userEcho(input))
}

// dequeue pops the oldest queued follow-up (if any) and submits it as a new turn,
// reusing submit() so slash commands / pasted text behave identically to a typed
// prompt. Returns the model unchanged with a nil cmd when nothing is queued.
func (m model) dequeue() (model, tea.Cmd) {
	if len(m.queued) == 0 {
		return m, nil
	}
	next := m.queued[0]
	m.queued = m.queued[1:]
	m.ti.in.SetValue(next)
	mm, cmd := m.submit()
	return mm.(model), cmd
}

// pasteRE matches the placeholder drawInputBox shows for a multi-line paste.
var pasteRE = regexp.MustCompile(`\[Pasted Text #(\d+): \d+ lines\]`)

// expandPastes replaces each [Pasted Text #id: N lines] placeholder with the
// buffered content it stands for, so the model receives the real pasted text.
func (m model) expandPastes(s string) string {
	if len(m.pastes) == 0 {
		return s
	}
	return pasteRE.ReplaceAllStringFunc(s, func(match string) string {
		id, _ := strconv.Atoi(pasteRE.FindStringSubmatch(match)[1])
		if content, ok := m.pastes[id]; ok {
			return content
		}
		return match
	})
}

// startTurn runs one agent task in a tea.Cmd goroutine; the bridge streams
// deltas/tool calls into the program while Ask runs, and turnDoneMsg ends it.
func (m model) startTurn(ctx context.Context, task string) tea.Cmd {
	return func() tea.Msg {
		err := m.agent.Ask(ctx, task)
		return turnDoneMsg{err: err}
	}
}

// beginTurn derives a cancellable context for one turn (so Esc can interrupt it
// without quitting), records its cancel func, and returns the context.
func (m *model) beginTurn() context.Context {
	ctx, cancel := context.WithCancel(m.ctx)
	m.turnCancel = cancel
	m.turnStart = time.Now() // start the live elapsed timer for this turn
	return ctx
}

// finishStep flushes the current step's streamed assistant text (rendered as
// markdown) plus its stats footer into scrollback, then clears the live buffer.
func (m *model) finishStep(stats agent.Stats) tea.Cmd {
	text := strings.TrimRight(m.buf, "\n")
	m.buf = ""
	var out strings.Builder
	if text != "" {
		out.WriteString(strings.TrimRight(m.md.render(text, m.width), "\n"))
	}
	if line := stats.Line(); line != "" {
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(dim.Render("  " + line))
	}
	if out.Len() == 0 {
		return nil
	}
	return tea.Printf("%s", out.String())
}

// command dispatches a /slash command, returning a Cmd (often a scrollback print
// or tea.Quit).
// printLastToolOutput flushes the most recent tool's FULL output to scrollback —
// the inline tool line shows only a one-line preview (and bash to ~100 cols), so
// this is how the user reads a long bash output, grep dump, or diff in full. The
// content is the byte-capped result the model saw; the complete trace is always in
// ~/.config/borg/logs.
func (m model) printLastToolOutput() tea.Cmd {
	name, out := m.agent.LastToolOutput()
	if out == "" {
		return tea.Printf("%s", dim.Render("no tool has run yet in this session"))
	}
	header := dim.Render(fmt.Sprintf("── full output: %s ──", name))
	return tea.Printf("%s\n%s", header, out)
}

func (m model) command(input string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(input)
	switch fields[0] {
	case "/exit", "/quit":
		m.quitting = true
		m.persist()
		return m, m.quitCmd()
	case "/sessions":
		// No id: open the picker of this directory's saved sessions to choose one
		// (an explicit id still attaches directly, even across directories).
		if len(fields) < 2 {
			return m.openSessionPicker()
		}
		return m.attachSession(fields)
	case "/help":
		return m, tea.Printf("%s", renderHelp(m.md, m.width))
	case "/learn":
		// Run the project-learn task as a turn (writing BORG.md prompts for
		// permission like any edit). Same harness config as `borg learn`.
		m.agent.ConfigureLearn()
		m.persist()
		echo := tea.Printf("%s %s", prompt.Render("›"), dim.Render("learn this project → "+agent.ProjectContextFile))
		m.streaming = true
		m.buf = ""
		m.turnIn, m.turnOut = 0, 0
		return m, tea.Batch(echo, m.ti.sp.Tick, m.startTurn(m.beginTurn(), agent.LearnPrompt))
	case "/clear":
		m.agent.Reset()
		m.sessIn, m.sessOut = 0, 0 // the conversation's token tally resets with it
		m.ctxTokens = 0
		m.persist()
		return m, tea.Printf("%s", dim.Render("conversation cleared"))
	case "/context":
		return m, tea.Printf("%s", m.renderContext())
	case "/compact":
		// Summarize the conversation off the event loop (a metered LLM call); the
		// working box shows progress and compactMsg reports the result.
		if len(m.agent.Messages()) <= 1 {
			m.status = "nothing to compact yet — have a conversation first"
			return m, nil
		}
		m.streaming = true
		m.compacting = true
		m.buf = ""
		m.turnIn, m.turnOut = 0, 0
		return m, tea.Batch(m.ti.sp.Tick, m.compact())
	case "/think":
		on := !m.agent.Think()
		if len(fields) > 1 {
			on = fields[1] == "on"
		}
		m.agent.SetThink(on)
		m.persist()
		if on {
			m.status = "thinking on — borg reasons before replying (slower, deeper)"
		} else {
			m.status = "thinking off — borg replies directly (faster, lighter)"
		}
		return m, nil
	case "/effort":
		// No argument: open the interactive picker, highlighting the current
		// level. An explicit level still switches directly.
		if len(fields) < 2 {
			m.mode = modeEffortPicker
			m.effortIdx = m.currentEffortIdx()
			return m, nil
		}
		if !validEffort(fields[1]) {
			m.status = "usage: /effort none|low|medium|high|xhigh (or off)"
			return m, nil
		}
		eff := fields[1]
		if eff == "off" {
			eff = ""
		}
		m.agent.SetEffort(eff)
		m.persist()
		if eff == "" {
			m.status = "effort: default (follows /think)"
		} else {
			m.status = "effort: " + eff
		}
		return m, nil
	case "/model":
		if len(fields) > 1 {
			m.agent.SetModel(fields[1])
			m.persist()
			m.status = m.modelSwitchStatus(fields[1])
			return m, nil
		}
		// No argument: open the interactive picker, highlighting the current
		// model, and fetch the catalog if we don't have it yet.
		m.mode = modeModelPicker
		m.modelIdx = m.currentModelIdx()
		if len(m.models) == 0 {
			return m, m.fetchInfo()
		}
		return m, nil
	case "/purge":
		m.purgeCount = 0
		if metas, err := session.List(); err == nil {
			m.purgeCount = len(metas)
		}
		m.mode = modeConfirmPurge
		return m, nil
	case "/settings":
		// /settings <key> <value> sets it directly; /settings <key> toggles a bool /
		// cycles an enum / opens a string for editing; no args opens the picker.
		if len(fields) >= 3 {
			return m.applySetting(fields[1], strings.Join(fields[2:], " "))
		}
		if len(fields) == 2 {
			return m.editSetting(fields[1])
		}
		m.mode = modeSettingsPicker
		m.settingsIdx = 0
		return m, nil
	case "/privacy":
		return m, tea.Printf("%s", renderPrivacy(m.md, m.width))
	case "/output":
		return m, m.printLastToolOutput()
	case "/upgrade":
		auth := m.agent.AuthInfo()
		appURL := strings.TrimRight(auth.AppURL, "/")
		pricingURL := appURL + "/pricing"
		md := "### Upgrade\n\n" +
			"Visit the [xShellz pricing page](" + pricingURL + ") to see available plans and upgrade your account.\n"
		if m.md != nil {
			return m, tea.Printf("%s", strings.TrimRight(m.md.render(md, m.width), "\n"))
		}
		return m, tea.Printf("%s", md)

	case "/update":
		// Download + install the latest release off the event loop; renders on
		// updateDoneMsg. Takes effect on the next launch.
		m.status = "updating…"
		return m, m.runUpdate()
	case "/usage":
		// Fetch live usage off the event loop; renders on usageMsg, falling back
		// to static caps if the endpoint isn't available.
		m.status = "fetching usage…"
		return m, m.fetchUsage(false)
	case "/status":
		// Fetch identity + usage off the event loop; renders on statusMsg.
		m.status = "fetching status…"
		return m, m.fetchStatus()
	case "/login":
		// Run the OAuth flow off the event loop (the flow prints its URL to
		// stderr and waits for browser/device approval); the agent swaps to the
		// new token on success. Renders on loginMsg.
		m.status = "logging in… approve in your browser, then return here"
		return m, m.login()
	case "/logout":
		if err := m.agent.Logout(); err != nil {
			return m, tea.Printf("%s", errSty.Render("logout failed: "+err.Error()))
		}
		_ = account.Clear() // drop the cached plan/catalog too
		m.tier, m.models = "", nil
		m.status = ""
		return m, tea.Printf("%s", dim.Render("logged out — stored credentials removed. This session's token "+
			"stays valid until you /exit; run /login to re-authenticate as another account."))
	default:
		return m, tea.Printf("%s", errSty.Render("unknown command "+fields[0]+" — try /help"))
	}
}

// quitCmd quits, first printing how to come back to this session (when it has
// any real content). On /sessions this reflects the *currently* attached session.
func (m model) quitCmd() tea.Cmd {
	if h := attachHint(m.sess); h != "" {
		return tea.Sequence(tea.Printf("%s", h), tea.Quit)
	}
	return tea.Quit
}

// attachHint is the "how to resume" line shown on quit, or "" when there's no
// session worth resuming (none, or empty bar the system prompt).
func attachHint(sess *session.Session) string {
	if sess == nil || len(sess.Messages) <= 1 {
		return ""
	}
	return dim.Render("session saved — resume with ") + brand.Render(version.Command()+" --attach "+sess.ID)
}

// openSessionPicker lists the saved sessions started in this directory and
// enters the session picker, pre-selecting the one currently open. With no
// sessions for this directory it just reports so. (For every directory's
// sessions, use `borg sessions --all` from the shell.)
func (m model) openSessionPicker() (tea.Model, tea.Cmd) {
	metas, err := session.ListForDir(session.CurrentDir())
	if err != nil {
		return m, tea.Printf("%s", errSty.Render("sessions: "+err.Error()))
	}
	if len(metas) == 0 {
		m.status = "no saved sessions for this directory"
		return m, nil
	}
	m.sessList = metas
	m.sessIdx = 0
	for i, meta := range metas {
		if m.sess != nil && meta.ID == m.sess.ID {
			m.sessIdx = i
			break
		}
	}
	m.mode = modeSessionPicker
	return m, nil
}

// attachSession switches the REPL to another saved session without leaving: it
// persists the one being left, loads the target (by id/prefix, or — with no arg
// — this directory's most-recent session), swaps it into the agent, and replays
// the target's transcript into scrollback.
func (m model) attachSession(fields []string) (tea.Model, tea.Cmd) {
	m.persist() // save the session we're leaving
	var (
		target *session.Session
		err    error
	)
	if len(fields) > 1 {
		target, err = session.Load(fields[1])
	} else {
		target, err = session.LatestForDir(session.CurrentDir())
	}
	if err != nil {
		return m, tea.Printf("%s", errSty.Render("attach: "+err.Error()))
	}
	m.agent.RestoreSession(target)
	m.sess = target
	// Restore the target's running token/context totals so the footer + /context
	// reflect that conversation's accumulated usage, not the one we just left.
	m.sessIn, m.sessOut, m.ctxTokens = target.TokensIn, target.TokensOut, target.ContextTokens
	m.history = historyFromSession(target)
	m.histIdx = len(m.history)
	m.status = "attached " + target.ID
	header := dim.Render("— attached session " + target.ID + " —")
	if tr := replayTranscript(target, m.md, m.width); tr != "" {
		return m, tea.Printf("%s\n%s", header, tr)
	}
	return m, tea.Printf("%s", header)
}

// historyFromSession returns the session's user prompts, for Up/Down recall.
func historyFromSession(sess *session.Session) []string {
	if sess == nil {
		return nil
	}
	var h []string
	for _, msg := range sess.Messages {
		if msg.Role == "user" {
			h = append(h, msg.Content)
		}
	}
	return h
}

// persist snapshots the agent's state into the session and saves it, so the
// conversation can be resumed with `borg --attach`.
func (m model) persist() {
	if m.sess == nil {
		return
	}
	m.agent.SnapshotSession(m.sess)
	// The cumulative footer totals are a REPL display concern (no agent/CLI
	// equivalent), so they're persisted here rather than through the agent seam.
	m.sess.TokensIn, m.sess.TokensOut = m.sessIn, m.sessOut
	_ = session.Save(m.sess) // best-effort; a save failure shouldn't crash the REPL
}

func (m model) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}
	var b strings.Builder
	var cur *tea.Cursor // the real terminal cursor, set only at the input prompt

	// Launch mascot: while it blinks it lives in the redrawn region (eye state
	// toggles per frame); settleBanner() promotes it to scrollback once done. The
	// cursor Y below counts newlines in b, so it self-adjusts for these rows. Drawn
	// only when the terminal is wide enough to hold it without overflowing.
	if m.bannerBlink >= 0 && m.width >= mascotMinWidth {
		b.WriteString(mascotFrame(m.bannerBlink%2 == 1) + m.bannerIntro + "\n")
	}

	if m.streaming && m.buf != "" {
		b.WriteString(strings.TrimRight(m.md.render(m.buf, m.width), "\n"))
		b.WriteString("\n")
	}

	switch m.mode {
	case modeTrust:
		b.WriteString(m.trustView())
	case modePermit:
		b.WriteString(dim.Render(fmt.Sprintf("  allow %s? ", m.permitName)) +
			"[y]es / [n]o / [a]lways")
	case modeAskUser:
		b.WriteString(m.askView())
	case modeConfirmPurge:
		b.WriteString(errSty.Render(fmt.Sprintf("Delete ALL %d saved session(s) ", m.purgeCount)) +
			dim.Render("from ~/.config/borg/sessions? This is permanent and cannot be undone. ") +
			errSty.Render("[y/N]"))
	case modeConfirmRetroLearn, modeConfirmRetroReport:
		b.WriteString(m.retroView())
	case modeModelPicker:
		b.WriteString(m.modelPickerView())
	case modeEffortPicker:
		b.WriteString(m.effortPickerView())
	case modeSessionPicker:
		b.WriteString(m.sessionPickerView())
	case modeSettingsPicker:
		b.WriteString(m.settingsPickerView())
	default:
		// A context near-full warning + a status line (idle only) sit above the input.
		if !m.streaming {
			if w := m.contextWarning(); w != "" {
				b.WriteString(truncate(w, m.width) + "\n")
			}
			if m.status != "" {
				b.WriteString(truncate(dim.Render(m.status), m.width) + "\n")
			}
		}
		lpad := strings.Repeat(" ", inputLeftPad)
		// While a turn runs, the spinner/"Turborging…" indicator sits on its own
		// line ABOVE the input box — the box itself keeps showing the (still
		// editable) textarea, so a follow-up can be typed and queued while borg
		// works. The brand-play verb ("Turborging") is the default; specific phases
		// (compacting, a named tool) say what's actually happening instead.
		if m.streaming {
			note := "Turborging…"
			switch {
			case m.compacting:
				note = "compacting — summarizing the conversation…"
			case m.toolNote != "":
				note = "running " + m.toolNote + "…"
			}
			inner := m.ti.sp.View() + " " + dim.Render(note)
			if !m.turnStart.IsZero() {
				inner += dim.Render("  " + fmtElapsed(time.Since(m.turnStart)))
			}
			if tl := m.tokenLine(); tl != "" {
				inner += dim.Render("  · " + tl)
			}
			if n := len(m.queued); n > 0 {
				inner += dim.Render(fmt.Sprintf("  · %d queued", n))
			}
			// Truncate to the terminal width so a long working line (tool + elapsed +
			// tokens + queue depth) can't wrap and corrupt the inline render region.
			b.WriteString(truncate(lpad+inner, m.width) + "\n")
		}
		// Build the box's content rows from the (wrapping, growing) textarea. Each
		// row carries the left pad + prompt prefix so it aligns inside the box.
		var lines []string
		// Live affordance: once a full command name is typed, turn the chevron
		// teal to signal "Enter runs a command" (plain text keeps the blue ›).
		chev := prompt.Render("›")
		if f := strings.Fields(m.ti.in.Value()); len(f) > 0 && knownCommand(f[0]) && !m.streaming {
			chev = tool.Render("›")
		}
		for i, ln := range strings.Split(m.ti.in.View(), "\n") {
			pre := lpad + "  " // continuation rows align under the text
			if i == 0 {
				pre = lpad + chev + " "
			}
			lines = append(lines, pre+ln)
		}
		// Pad the box with a blank shaded row above and below the text, so the
		// content has a little breathing room inside the box (an empty line is
		// fit-to-width + shaded into a full-width blank row).
		pad := make([]string, 0, len(lines)+2*inputPadRows)
		for i := 0; i < inputPadRows; i++ {
			pad = append(pad, "")
		}
		pad = append(pad, lines...)
		for i := 0; i < inputPadRows; i++ {
			pad = append(pad, "")
		}
		lines = pad
		// A small top margin separates the box from the banner/scrollback above.
		b.WriteString(strings.Repeat("\n", inputTopMargin))
		offsetY := strings.Count(b.String(), "\n")
		// The borderless shaded box grows with the textarea; the same box wraps the
		// streaming line for a steady height.
		b.WriteString(inputBoxLines(lines, m.width))
		// Cursor: shift X past the left pad + prompt; Y onto the box's text rows
		// (below the top margin and the box's top pad row). It's shown even while a
		// turn streams, since the box stays editable for a queued follow-up.
		if cur = m.ti.in.Cursor(); cur != nil {
			cur.X += inputPrefix
			cur.Y += offsetY + inputPadRows
		}
		// Footer, separated from the box by a blank line: on the left the working
		// directory (home-abbreviated) + git branch + this session's running token
		// total + live credit usage; on the right the active model + reasoning
		// level — so the user always sees what's in effect and what they've spent.
		b.WriteString("\n\n" + m.footerView(lpad))
		if !m.streaming {
			// Live, windowed slash-command menu while composing a "/command" — it
			// renders below the footer; the highlighted entry runs on Enter.
			matches := m.menu()
			b.WriteString(menuView(matches, clampIdx(m.menuIdx, len(matches)), m.width))
		}
	}
	v := tea.NewView(b.String())
	v.Cursor = cur
	// Ask the terminal to report focus gain/loss so the footer's git branch can
	// refresh the instant you switch back from another window (handled in Update via
	// tea.FocusMsg) — event-driven, no polling. Terminals without focus reporting
	// just won't send it; the submit/turn-end refreshes still keep the branch current.
	v.ReportFocus = true
	return v
}

// askView renders the ask_user modal: the question, then the 2–4 options as a
// numbered, navigable list (the highlighted row is brightened whole, like the
// other pickers) with a one-line key hint. Its height is constant while open
// (navigation doesn't add/remove rows), so the inline renderer never has to
// shrink the live region mid-update — it only collapses when the modal closes.
func (m model) askView() string {
	var b strings.Builder
	b.WriteString(brand.Render(m.askReq.Question) + "\n")
	b.WriteString(dim.Render("↑/↓ or 1–" + strconv.Itoa(m.askRows()) + ", Enter to choose, Esc to skip"))
	sel := clampIdx(m.askIdx, m.askRows())
	row := func(i int, label, desc string) {
		num := strconv.Itoa(i + 1)
		marker, lab, d := "  ", tool.Render(num+". "+label), ""
		if desc != "" {
			d = dim.Render("  — " + desc)
		}
		if i == sel {
			// Brighten the whole selected row (label + description), not just the label.
			marker, lab = prompt.Render("› "), brand.Render(num+". "+label)
			if desc != "" {
				d = "  — " + desc
			}
		}
		b.WriteString("\n" + truncate(marker+lab+d, m.width))
	}
	for i, o := range m.askReq.Options {
		row(i, o.Label, o.Description)
	}
	// The built-in escape hatch: answer in your own words instead of picking.
	row(len(m.askReq.Options), "Something else", "type your own answer or discuss")
	return b.String()
}

// effortPickerView renders the reasoning-effort list with a one-line
// explanation per level; the highlighted row is marked and chosen on Enter.
func (m model) effortPickerView() string {
	var b strings.Builder
	b.WriteString(dim.Render("reasoning effort — ↑/↓, Enter, Esc:"))
	sel := clampIdx(m.effortIdx, len(effortLevels))
	cur := m.agent.Effort()
	for i, e := range effortLevels {
		marker, label, desc := "  ", tool.Render(fmt.Sprintf("%-8s", e.label)), dim.Render("  "+e.desc)
		if i == sel {
			// Brighten the whole selected row (label + description), not just the label.
			marker, label, desc = prompt.Render("› "), brand.Render(fmt.Sprintf("%-8s", e.label)), "  "+e.desc
		}
		row := marker + label + desc
		if e.value == cur {
			row += dim.Render("  (current)")
		}
		b.WriteString("\n" + truncate(row, m.width))
	}
	return b.String()
}

// sessionPickerView renders this directory's saved-session list (windowed to
// maxMenuRows so a long list scrolls), each row showing id, last-active, and a
// preview; the highlighted row is marked and attached on Enter. All rows belong
// to the current directory (the list is dir-scoped), so there's no * marker.
func (m model) sessionPickerView() string {
	var b strings.Builder
	b.WriteString(dim.Render("this directory's sessions — ↑/↓, Enter, Esc:"))
	if len(m.sessList) == 0 {
		return b.String()
	}
	sel := clampIdx(m.sessIdx, len(m.sessList))
	start := 0
	if len(m.sessList) > maxMenuRows {
		start = clampIdx(sel-maxMenuRows/2, len(m.sessList)-maxMenuRows+1)
	}
	for row := 0; row < maxMenuRows && start+row < len(m.sessList); row++ {
		i := start + row
		meta := m.sessList[i]
		marker, id := "  ", tool.Render(meta.ID)
		if i == sel {
			marker, id = prompt.Render("› "), brand.Render(meta.ID)
		}
		name := meta.Name
		if name == "" {
			name = "(untitled)"
		}
		line := marker + id + "  " + name + dim.Render("  ·  "+session.HumanTime(meta.LastActive))
		b.WriteString("\n" + truncate(line, m.width))
	}
	if len(m.sessList) > maxMenuRows {
		b.WriteString("\n" + dim.Render(fmt.Sprintf("  %d session(s)", len(m.sessList))))
	}
	return b.String()
}

// modelPickerView renders the interactive model list with versions and
// availability; the highlighted row is marked (Up/Down) and chosen on Enter.
func (m model) modelPickerView() string {
	if len(m.models) == 0 {
		return dim.Render("loading models…  ") + m.ti.sp.View()
	}
	var b strings.Builder
	b.WriteString(dim.Render("select a model — ↑/↓, Enter, Esc:"))
	sel := clampIdx(m.modelIdx, len(m.models))
	// Size the name column to the widest label (+gap) so a multi-word label like
	// "Chuppa Flash" doesn't run into the version column.
	nameW := 2
	for _, mi := range m.models {
		nameW = max(nameW, ansi.StringWidth(mi.Label)+2)
	}
	for i, mi := range m.models {
		marker, name := "  ", tool.Render(padTo(mi.Label, nameW))
		ver, desc := dim.Render("v"+mi.Version), dim.Render("  "+mi.Description)
		if i == sel {
			// Brighten the whole selected row (label + version + description); the
			// ✓/🔒 availability marker keeps its semantic color.
			marker, name = prompt.Render("› "), brand.Render(padTo(mi.Label, nameW))
			ver, desc = "v"+mi.Version, "  "+mi.Description
		}
		avail := tool.Render("✓")
		if !m.modelAvailable(mi) {
			avail = errSty.Render("🔒 " + mi.MinTier)
		}
		burn := ""
		if mi.BurnRate >= burnHintThreshold {
			burn = errSty.Render(fmt.Sprintf("  ≈%d× budget", mi.BurnRate))
		}
		b.WriteString("\n" + truncate(marker+name+ver+"  "+avail+desc+burn, m.width))
	}
	return b.String()
}

const helpMD = "### Commands\n" +
	"- **/model** `[name]` — interactive model picker (versions + your plan), or switch directly\n" +
	"- **/think** `[on|off]` — toggle reasoning\n" +
	"- **/effort** `[level]` — interactive effort picker (none…xhigh), or set it directly\n" +
	"- **/sessions** `[id]` — pick one of this directory's sessions, or switch to a saved session by id\n" +
	"- **/learn** — study this project and write BORG.md (its CLAUDE.md)\n" +
	"- **/context** — show how much of the model's context window the conversation uses\n" +
	"- **/compact** — summarize the conversation into a recap to free up the context window\n" +
	"- **/settings** `[key] [value]` — view & change persistent settings (attribution, auto-escalate model, …)\n" +
	"- **/upgrade** — visit the xShellz pricing page to upgrade your plan\n" +
	"- **/usage** — show your plan, quota caps, and usage\n" +
	"- **/status** — login (user + environment), plan, usage, and current settings\n" +
	"- **/login** — log in to xShellz (browser, or the device flow on headless boxes)\n" +
	"- **/logout** — remove the stored credentials (local only)\n" +
	"- **/privacy** — how borg handles your data, with the policy link\n" +
	"- **/output** — print the last tool's FULL output (also **ctrl+o**); inline shows only a one-line preview\n" +
	"- **/clear** — reset the conversation\n" +
	"- **/purge** — delete ALL saved sessions\n" +
	"- **/help** — this help\n" +
	"- **/exit** — quit (the session is saved; resume with `borg --attach <id>`)\n\n" +
	"Type a task in plain language. borg reads/searches/edits files and runs commands " +
	"to do it — mutating actions ask for permission. The full, untruncated trace of every " +
	"tool call is always written to `~/.config/borg/logs`.\n"

// renderHelp renders the help markdown, falling back to glamour's default if the
// model's width-cached renderer isn't ready.
func renderHelp(md *mdRenderer, width int) string {
	if md != nil {
		return strings.TrimRight(md.render(helpMD, width), "\n")
	}
	out, err := glamour.Render(helpMD, "dark")
	if err != nil {
		return helpMD
	}
	return out
}

// privacyURL is borg's canonical privacy policy.
const privacyURL = "https://www.xshellz.com/privacy-policy"

const privacyMD = "### Privacy\n" +
	"borg is an authenticated, metered coding agent. The task you type and the " +
	"file snippets and command output the agent chooses to send are transmitted to " +
	"xShellz, which meters usage and forwards the request to the model provider to " +
	"fulfill it. Your auth token is stored locally (`~/.config/borg`, mode 0600); " +
	"**no provider key ever lives on your machine**.\n\n" +
	"Saved sessions are **local only** — plain files under `~/.config/borg/sessions` " +
	"on this machine, never uploaded or synced.\n\n" +
	"Full policy: " + privacyURL + "\n"

// renderPrivacy renders the privacy notice (markdown), like renderHelp.
func renderPrivacy(md *mdRenderer, width int) string {
	if md != nil {
		return strings.TrimRight(md.render(privacyMD, width), "\n")
	}
	return privacyMD
}

// planCap returns the rolling-24h token cap for a tier as a display string, or
// "" when borg doesn't know it (paid-plan caps live server-side; the live usage
// endpoint will supply real numbers).
func planCap(tier string) string {
	switch strings.ToLower(tier) {
	case "free", "":
		return "50 credits / day"
	case "starter":
		return "233 credits / day"
	case "pro":
		return "500 credits / day"
	case "max":
		return "1250 credits / day"
	default:
		return ""
	}
}

// renderUsage shows live account usage (plan + rolling-24h used/limit, summed
// across all models). If the live endpoint isn't available (older accounts-api),
// it falls back to the plan's static caps.
func (m model) renderUsage(msg usageMsg) string {
	// Nothing is metered on a backend the user pays for themselves. Say that
	// plainly: the static-plan fallback below would otherwise invent an xShellz
	// plan and a credit cap that have nothing to do with this session.
	if errors.Is(msg.err, llm.ErrNoMetering) {
		return dim.Render("no usage to report — this session runs on your own "+m.agent.Provider()+" backend ("+m.agent.Endpoint()+").\n") +
			dim.Render("Plans and credits are xShellz-only; whatever this backend costs is between you and it.")
	}
	if msg.err != nil || msg.usage == nil {
		return m.usageFallback()
	}
	u := msg.usage
	plan := titleCase(u.PlanCode)
	if plan == "" {
		plan = "Free"
	}
	var b strings.Builder
	b.WriteString(dim.Render("plan: ") + brand.Render(plan) + "\n")
	b.WriteString(dim.Render(fmt.Sprintf("rolling-%dh AI budget (all models):", u.WindowHoursOrDefault())) + "\n")
	b.WriteString("  " + dim.Render("credits: ") + renderCredits(u))
	return b.String()
}

// renderCredits formats the shared budget as "used / per-day (pct%)" — the
// account-wide credit figure (a friendly rename of the internal per-model dollar
// cost). A per-day grant of 0 means the budget is unlimited.
func renderCredits(u *llm.AccountUsage) string {
	if u.CreditsPerDay <= 0 {
		return fmt.Sprintf("%d / ∞", u.CreditsUsed)
	}
	return fmt.Sprintf("%d / %d  (%d%% used)", u.CreditsUsed, u.CreditsPerDay, u.PercentUsed)
}

// renderStatus shows the consolidated /status panel: who's logged in and where,
// the plan + live rolling-24h budget, and the session's current settings.
func (m model) renderStatus(msg statusMsg) string {
	auth := m.agent.AuthInfo()
	var b strings.Builder

	// Login + environment.
	b.WriteString(brand.Render(version.Command()+" ") + dim.Render(version.Version) + "\n")
	// On a bring-your-own backend there is no account, so the whole login/plan/
	// usage block is meaningless — and worse than meaningless: it would report
	// "not logged in" as a problem to fix and invent a Free plan with a credit cap.
	// Report the backend instead, which is what actually governs this session.
	if m.agent.BringYourOwn() {
		b.WriteString(dim.Render("provider: ") + brand.Render(m.agent.Provider()) + "\n")
		b.WriteString(dim.Render("endpoint: ") + m.agent.Endpoint() + "\n")
		b.WriteString(dim.Render("usage:    ") + dim.Render("not metered by borg — you own this backend") + "\n")
		b.WriteString(m.sessionSettingsLines())
		return b.String()
	}
	if auth.LoggedIn {
		who := "logged in"
		if msg.user != nil {
			if msg.user.Name != "" && msg.user.Email != "" {
				who = msg.user.Name + " <" + msg.user.Email + ">"
			} else if msg.user.Email != "" {
				who = msg.user.Email
			} else if msg.user.Name != "" {
				who = msg.user.Name
			}
		}
		b.WriteString(dim.Render("user:    ") + brand.Render(who) + "\n")
		if !auth.Expiry.IsZero() {
			b.WriteString(dim.Render("token:   ") + auth.TokenType +
				dim.Render(", expires "+auth.Expiry.Format("2006-01-02 15:04 MST")) + "\n")
		}
	} else {
		b.WriteString(dim.Render("user:    ") + errSty.Render("not logged in — run /login") + "\n")
	}
	b.WriteString(dim.Render("api:     ") + auth.APIBaseURL + "\n")

	// Plan + usage (live, falling back to the cached tier).
	plan := titleCase(m.tier)
	if msg.usage != nil && msg.usage.PlanCode != "" {
		plan = titleCase(msg.usage.PlanCode)
	}
	if plan == "" {
		plan = "Free"
	}
	b.WriteString(dim.Render("plan:    ") + brand.Render(plan) + "\n")
	if u := msg.usage; u != nil {
		b.WriteString(dim.Render(fmt.Sprintf("usage:   rolling-%dh AI budget (all models)", u.WindowHoursOrDefault())) + "\n")
		b.WriteString("  " + dim.Render("credits: ") + renderCredits(u) + "\n")
	} else if cap := planCap(m.tier); cap != "" {
		b.WriteString(dim.Render("usage:   ") + cap + dim.Render(" (live usage unavailable)") + "\n")
	} else {
		b.WriteString(dim.Render("usage:   ") + dim.Render("(live usage unavailable)") + "\n")
	}

	b.WriteString(m.sessionSettingsLines())
	return b.String()
}

// sessionSettingsLines renders the current session's model/think/effort + cwd —
// the tail of /status, shared by the hosted and bring-your-own panels so the two
// can't drift.
func (m model) sessionSettingsLines() string {
	think := "off"
	if m.agent.Think() {
		think = "on"
	}
	effort := m.agent.Effort()
	if effort == "" {
		effort = "default (follows think)"
	}
	// Reasoning is an xShellz-proxy feature: the fields aren't portable, so borg
	// doesn't send them elsewhere. Don't report a think/effort that isn't in force.
	if m.agent.BringYourOwn() {
		return dim.Render("model:    ") + m.agent.Model() +
			dim.Render("   think/effort n/a (xShellz-only)") + "\n" +
			dim.Render("cwd:      ") + m.cwd
	}
	return dim.Render("model:   ") + m.agent.Model() +
		dim.Render("   think ") + think + dim.Render("   effort ") + effort + "\n" +
		dim.Render("cwd:     ") + m.cwd
}

// usageFallback shows the plan + static caps when live usage can't be fetched.
func (m model) usageFallback() string {
	plan := titleCase(m.tier)
	if plan == "" {
		plan = "Free"
	}
	var b strings.Builder
	b.WriteString(dim.Render("plan: ") + brand.Render(plan) + "\n")
	if cap := planCap(m.tier); cap != "" {
		b.WriteString(dim.Render("rolling-24h cap:  ") + cap + "\n")
	} else {
		b.WriteString(dim.Render("rolling-24h cap:  ") + dim.Render("(varies by plan)") + "\n")
	}
	b.WriteString(dim.Render("used (24h):       (live usage unavailable)"))
	return b.String()
}

// tokenLine renders the live per-turn token usage for the working indicator:
// exact tokens accumulated over completed steps plus a live estimate of the
// in-flight step's output (so the ↓ ticks up as text streams). It returns ""
// until there's any usage to show.
// fmtElapsed renders a running turn's duration compactly for the working line:
// "8s", "1m05s", "1h02m". Recomputed each spinner tick, so it counts up live.
func fmtElapsed(d time.Duration) string {
	s := int(d.Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
	}
}

func (m model) tokenLine() string {
	out := m.turnOut + estTokens(m.buf)
	if m.turnIn == 0 && out == 0 {
		return ""
	}
	s := "↓ " + fmtTokens(out) + " tokens"
	if m.turnIn > 0 {
		s = "↑ " + fmtTokens(m.turnIn) + "  " + s
	}
	return s
}

// estTokens is a rough live token estimate (~4 chars/token) for streamed text;
// the proxy's exact count replaces it the moment the step completes.
func estTokens(s string) int { return (len(s) + 3) / 4 }

// contextWarnPct is the context-window fill (percent) at which borg warns the
// user — both in the footer line and in the /context panel — that they're close
// to the model's limit and should consider /compact.
const contextWarnPct = 95

// contextBarWidth is the /context panel's usage-bar column count.
const contextBarWidth = 28

// contextBar renders a fixed-width usage track filled to pct percent (teal,
// turning hot red past the warn threshold) — the visual for /context.
func contextBar(pct, width int) string {
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}
	filled := (pct*width + 50) / 100
	if filled > width {
		filled = width
	}
	fill := barFill
	if pct >= contextWarnPct {
		fill = barFillHot
	}
	return fill.Render(strings.Repeat(" ", filled)) + barEmpty.Render(strings.Repeat(" ", width-filled))
}

// contextLabel is the footer's compact context-usage chip ("ctx 12%"), hot when
// near the limit. "" until there's a measurement (no turn has run yet).
func contextLabel(used, window int) string {
	if used <= 0 || window <= 0 {
		return ""
	}
	pct := used * 100 / window
	if pct > 100 {
		pct = 100
	}
	label := fmt.Sprintf("ctx %d%%", pct)
	if pct >= contextWarnPct {
		return barFillHot.Render(" " + label + " ")
	}
	return dim.Render(label)
}

// contextWarning is the dedicated footer line shown above the input once the
// conversation fills the model's context window past the warn threshold — so an
// imminent overflow is impossible to miss. "" below the threshold.
func (m model) contextWarning() string {
	window := m.agent.ContextWindow()
	if m.ctxTokens <= 0 || window <= 0 {
		return ""
	}
	pct := m.ctxTokens * 100 / window
	if pct < contextWarnPct {
		return ""
	}
	if pct > 100 {
		pct = 100
	}
	return errSty.Render(fmt.Sprintf("⚠ context %d%% full — run /compact to summarize and free space", pct))
}

// renderContext renders the /context panel: a usage bar for the model's context
// window plus an (estimated) token breakdown, so the user can see how full the
// conversation is and what is filling it.
func (m model) renderContext() string {
	s := m.agent.ContextStats()
	pct := s.Percent()
	free := s.Window - s.Used
	if free < 0 {
		free = 0
	}
	var b strings.Builder
	b.WriteString(brand.Render("context") +
		dim.Render("  model "+s.Model+"  ·  window "+fmtTokens(s.Window)+" tokens") + "\n")
	usedNote := fmt.Sprintf("  (%d%% used", pct)
	if !s.Exact {
		usedNote += ", estimated"
	}
	usedNote += ")"
	b.WriteString(contextBar(pct, contextBarWidth) + "  " +
		fmt.Sprintf("%s / %s", fmtTokens(s.Used), fmtTokens(s.Window)) +
		dim.Render(usedNote) + "\n")
	b.WriteString(dim.Render("  free:     ") + fmtTokens(free) + dim.Render(" tokens remaining") + "\n")
	b.WriteString(dim.Render("  messages: ") + strconv.Itoa(s.Messages) + "\n")
	b.WriteString(dim.Render("  breakdown (estimated):") + "\n")
	b.WriteString(dim.Render("    system:  ") + fmtTokens(s.SystemTokens) + dim.Render("  (prompt + project context)") + "\n")
	b.WriteString(dim.Render("    history: ") + fmtTokens(s.MessageTokens) + dim.Render("  (your prompts + borg's replies)") + "\n")
	b.WriteString(dim.Render("    tools:   ") + fmtTokens(s.ToolTokens) + dim.Render("  (file reads, command output)"))
	if s.Cached > 0 {
		b.WriteString("\n" + dim.Render("    cached:  ") + fmtTokens(s.Cached) + dim.Render("  (prompt-cache hits, last step)"))
	}
	switch {
	case pct >= contextWarnPct:
		b.WriteString("\n" + errSty.Render(fmt.Sprintf("  ⚠ %d%% full — run /compact to summarize and free space", pct)))
	case pct >= 50:
		b.WriteString("\n" + dim.Render("  tip: /compact summarizes the conversation to free up space"))
	}
	return b.String()
}

// renderCompact reports the result of a /compact run: how much the context
// shrank and that the recap is now the live context.
func (m model) renderCompact(res agent.CompactResult) string {
	saved := res.BeforeTokens - res.AfterTokens
	if saved < 0 {
		saved = 0
	}
	return tool.Render("✓ ") + dim.Render(fmt.Sprintf(
		"compacted conversation: %s → %s tokens (freed ~%s). The recap is now the context — continue where you left off.",
		fmtTokens(res.BeforeTokens), fmtTokens(res.AfterTokens), fmtTokens(saved)))
}

// fmtTokens renders a token count compactly (1234 -> "1.2k").
func fmtTokens(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// summarize renders a short, single-line preview of a tool call's arguments.
func summarize(args string) string {
	s := strings.Join(strings.Fields(args), " ")
	if len(s) > 100 {
		s = s[:100] + "…"
	}
	return s
}
