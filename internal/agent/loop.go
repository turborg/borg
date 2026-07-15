// Package agent is the coding-agent loop: it sends the conversation to the LLM,
// executes the tool calls the model requests (mutating ones behind a permission
// prompt), feeds the results back, and repeats until the model is done.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/turborg/borg/internal/auth"
	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/llm"
	"github.com/turborg/borg/internal/session"
	"github.com/turborg/borg/internal/tools"
	"github.com/turborg/borg/internal/version"
)

const (
	// defaultMaxSteps is a HIGH safety backstop, not a task budget. Real runaway
	// loops are caught by the no-progress guard (same call+result recurring) long
	// before this; this only stops a model doing genuinely-distinct work forever.
	// Keep it generous so a legitimately long task finishes (Claude-style: lean on
	// stuck-detection + the task finishing, never a low per-task ceiling).
	defaultMaxSteps = 60
	// learnMaxSteps is the cap for a `learn` run, which is broad by nature (read a
	// whole repo once, then write BORG.md). It needs far more reads than a scoped
	// edit, so it gets its own generous backstop via ConfigureLearn.
	learnMaxSteps = 120
	// artifactFlushMargin: when a task REQUIRES a file and is this many steps from
	// the cap without having written it, stop exploring and force the write now —
	// so a broad run (e.g. /learn) always ends WITH the deliverable, never empty.
	artifactFlushMargin = 4
	// artifactExploreBudget: a soft "you've explored enough — write now" cap for a
	// required-artifact task, INDEPENDENT of the (much higher) hard step cap. A good
	// /learn writes by ~step 8; past this many steps without a write the model is
	// over-exploring (the defer-the-write wander), so force the write. This bounds
	// wall-clock deterministically — wording in the prompt can't, on a stochastic
	// model. The hard cap + context-flush still backstop genuinely huge repos.
	artifactExploreBudget = 16
	// contextFlushPct: the same flush also fires when context occupancy crosses
	// this — the NON-LOSSY answer to a huge repo (write from the real files still
	// in context, rather than silently compacting them away). Below the 95% warn
	// line so there's still room to generate the file.
	contextFlushPct  = 90
	artifactFlushMsg = "You are nearly out of room and have NOT written %[1]s yet. STOP reading or searching — call write_file NOW with path %[1]s and the most complete %[1]s you can produce from what you've already learned. Then, in your final reply, be HONEST about coverage: say it was written under a size/step limit and briefly name any directories or topics you did not get to fully examine, so the user knows what to double-check. Never claim it is exhaustive if it is not."
	// maxLeakRetries bounds how many times one turn re-prompts a model that wrote
	// a tool call as text instead of a structured tool_call, before giving up.
	maxLeakRetries = 2
	// toolCallCorrection nudges a model that leaked a text tool call to re-issue it
	// through the real tool interface.
	toolCallCorrection = "Your previous reply wrote a tool call as plain text, so nothing ran. Do NOT print tool calls as text or in <tool_call> blocks. Re-issue the intended tool call now through the function/tool-calling interface so it executes."
	// maxRepetitionRetries bounds how many times one turn is re-issued under forced
	// tool-calling after the stream guard cut a degenerate prose loop short — a
	// small model deliberating in circles ("Wait… Actually… I'm ready…") instead of
	// acting. The guard lives in internal/llm (FinishReasonRepetition).
	maxRepetitionRetries = 2
	repetitionRetryMsg   = "You were repeating yourself without acting — that loop was stopped. Stop deliberating and act NOW: call the tool that performs the next concrete step (e.g. write_file to create/replace the file, or finish with your final answer). Emit the tool call only — no further narration or planning."
	// maxArtifactRetries bounds how many times a turn is re-issued when the task
	// must write a file (e.g. /learn → BORG.md) but the model tried to finish
	// without ever calling write_file (it printed the file inline instead).
	maxArtifactRetries    = 2
	artifactNotWrittenMsg = "You have NOT created %[1]s yet — printing its contents in your reply does NOT create the file. Call the write_file tool now with path %[1]s and the FULL file contents as the `content` argument. Emit only that tool call; do not paste the file into your reply."

	// No-progress guard — a UNIVERSAL backstop in the shared loop, applies to BOTH
	// models (like the leak-retry and the step cap; unlike guided tool-calls and
	// the prompt addendum, which are Floko-only). A step that recurs (same tool
	// calls AND same results) means the model is stuck — whether it repeats the
	// SAME step back-to-back or OSCILLATES between a few (read X, read Y, read X…).
	// We count how often the current step signature recurs within a sliding window,
	// so a 2-cycle is caught too, not just consecutive repeats. Nudge once, then
	// bail before the step cap.
	noProgressWindow  = 12 // recent step signatures kept for cycle detection
	noProgressNudgeAt = 3  // a signature seen this many times within the window → nudge
	noProgressStopAt  = 5  // …and this many → bail
	noProgressNudge   = "You're going in circles — repeating tool calls that return the same results without making progress. Do NOT continue this pattern. Either try a materially different approach (a different path, pattern, or tool), or call the finish tool with your conclusion (e.g. that nothing matched)."
	noProgressStopMsg = "Stopped — I kept repeating the same actions without making progress (check the working directory and paths). Tell me how you'd like to proceed."
	// stepCapMsg ends a run that hit the step cap gracefully (with what it got to)
	// rather than surfacing a raw error — the model simply ran long.
	stepCapMsg = "Stopped — I reached the internal step limit before finishing. Here's where I got to; tell me how you'd like to proceed (or narrow the task)."

	// Escalation circuit breaker — once auto-effort has climbed to the TOP reasoning
	// rung (xhigh), steps are slow and token-heavy. If reasoning is maxed and this
	// many steps pass without a single successful edit, more thinking isn't paying
	// off: give up gracefully rather than burn xhigh-effort steps to the cap (the
	// 1.45M-token incident ran 40+ escalated steps, several at 60–90s each, landing
	// no edit). BELOW the top rung the same "stuck" signal instead CLIMBS a rung
	// (harder problem → more reasoning), per the none→medium→high→xhigh ladder.
	escalateGiveUpSteps = 10
	escalateGiveUpMsg   = "You've spent many max-reasoning steps without landing a working change. Stop expanding the search. Either make the single most direct edit now and verify it, or call finish and report concisely what you found and what's blocking you."
	// exploreGiveUpMsg ends a pure-search task that NEVER made an edit and still
	// didn't converge after the act-or-finish nudge — gracefully, recommending the
	// next action (it did NOT spend reasoning, so escalateGiveUpMsg's wording is wrong).
	exploreGiveUpMsg = "Stopped — I explored the project but couldn't land a working change on my own. Here's what I found; tell me how you'd like to proceed, or narrow the task."
	// escalateRetryMsg accompanies a reasoning-rung climb when borg is stuck: it tells
	// the model its effort was raised and to take a materially different, more careful
	// approach now. %s is the new effort level.
	escalateRetryMsg = "You're stuck. I've raised your reasoning effort to %s — stop repeating what didn't work, think more carefully, and take a materially different approach to solve it now (or call finish if it genuinely can't be done)."
	// modelTierRetryMsg accompanies the last-lever model tier (chuppa→a stronger model,
	// opt-in via escalate_model): the cheaper model exhausted its options, so a stronger
	// one gets ONE fresh, bounded attempt before borg gives up. %s is the new model.
	modelTierRetryMsg = "I've switched to a stronger model (%s) for one more attempt — the cheaper model couldn't land it. Take a fresh, careful look and either make the fix now or call finish if it genuinely can't be done."

	// maxReEdits: editing the SAME file more than this many times in one task is
	// re-edit thrash (the indentation-fight signature) — a struggle signal even when
	// the task ultimately succeeds and trips no terminal guard.
	maxReEdits = 4

	// editThrashNudge fires IN-TURN the first time one file crosses maxReEdits: the
	// model is making "progress" (so the cycle detector and circuit breaker stay quiet)
	// yet not resolving the problem — the exact shape of a debug loop that fights a
	// failing test/check edit-by-edit and burns steps (each step re-sends the whole
	// context). It redirects from incremental churn to root-cause reasoning, and the
	// loop also raises reasoning effort alongside it. General: keyed only on repeated
	// edits to one file, never on a language, tool, or command.
	editThrashNudge = "You've edited the same file several times without resolving it — that's churning, not progress. STOP making incremental edits. Step back and reason about the ROOT CAUSE: exactly why does the check/test still fail? Is the expectation itself wrong, does the runtime or test environment differ from what you assumed (e.g. a value that's set at run time but empty under test), or is the change incomplete elsewhere? State the cause in one line, then make ONE correct edit that fixes it."

	// postVerifyIdleBudget bounds over-verification: once an edit has LANDED and a
	// verify/compile check has gone GREEN after it, this many consecutive read-only
	// steps with no new edit means the change is in place and the model is just
	// re-reading/re-checking — nudge it to finish. This is the missing brake for
	// ORDINARY (no-artifact) tasks: artifactExploreBudget only caps required-artifact
	// runs, so without this a finished coding task keeps re-sending the whole growing
	// context (n-squared cost) until a higher backstop trips. General: keys only on
	// "an edit landed", the project's OWN green verify signal, and read-only-vs-
	// mutating steps — never on a language, tool, or command.
	postVerifyIdleBudget = 3
	finishBrakeMsg       = "Your change is in place and the verification you ran passed. Call the finish tool now with a one-line summary of what you did — unless you have a CONCRETE remaining edit to make (then make it, don't re-read)."

	// exploreActOrFinishMsg fires when a task has run many steps WITHOUT ever landing
	// an edit (open-ended search). More reasoning can't focus an unfocused search — it
	// just prolongs it — so this nudges the model to commit to an action or finish,
	// WITHOUT climbing the reasoning ladder. General: keyed only on "no edit landed".
	exploreActOrFinishMsg = "You've gathered plenty of context but haven't made any change yet. Stop searching. Either make the single most direct edit now, or call the finish tool and report concisely what you found and what's blocking you."

	// Retrospective — runs ONCE after a task that fired a recovery guard (see
	// Struggle). The struggle's SEVERITY picks the path (not the model): a TERMINAL
	// give-up → a harness report to the team; a soft thrash-but-finished → a BORG.md
	// note. Kinds the retrospective yields:
	RetroKindBorgMD   = "borg_md" // a project-knowledge note to append to BORG.md
	RetroKindHarness  = "harness" // a problem report for borg's developers
	RetroKindNone     = "none"    // nothing actionable
	retroNoneSentinel = "NONE"    // the model emits this (alone) when there's nothing useful

	// retroReportPrompt drives the TERMINAL case: borg gave up, so this is for the
	// developers. Write a crisp problem report, or NONE.
	retroReportPrompt = `A coding agent (borg) just GAVE UP on a task — it exhausted its recovery options without finishing. You are writing a short problem report for borg's own developers so they can improve the agent.

Write 2-5 sentences: what went wrong (the failure mode), and what in the TOOLS or AGENT LOOP would have prevented it (a guard that didn't catch a loop, a tool that misbehaved, a missing capability). Be specific and honest; do not invent facts you can't see in the trajectory. This is about borg's harness, NOT the user's project.

If there is genuinely nothing actionable, reply with exactly: NONE`

	// retroLearnPrompt drives the SOFT case: borg thrashed but finished, so the fix is
	// usually project knowledge. Propose a DURABLE, GENERAL BORG.md note — or NONE, to
	// keep BORG.md from bloating with task trivia or things it already says.
	retroLearnPrompt = `A coding agent (borg) finished a task but thrashed on the way (it tripped its struggle guards). You are deciding whether a note in BORG.md (the project's guidance file for the agent) would make similar future tasks smoother.

Propose a note ONLY if it is a DURABLE, GENERAL project lesson (a convention, a path, a build/format/test quirk) that would actually help next time. Write 1-3 sentences of guidance — NOT a play-by-play of this one task. The "EXISTING BORG.md LESSONS" below are already recorded; do NOT repeat or restate them.

If a good note would just be task trivia, or BORG.md already covers it, reply with exactly: NONE`

	// Auto-verify — a UNIVERSAL backstop (both models). The model is told to run
	// verify after edits, but a small model forgets, so the loop runs it ITSELF
	// before a turn that edited source can end: if the compile check FAILS, the
	// failure is fed back and the model must fix it before finishing. This closes
	// the read→edit→build loop the way Claude Code's PostToolUse hook does, instead
	// of trusting the model to self-check. Bounded so a persistently-broken edit
	// can't loop forever; only edits to verifiable source extensions arm it (docs/
	// config writes — e.g. `borg learn` writing BORG.md — never do).
	maxAutoVerifyRepairs = 2
	autoVerifyFailMsg    = "Before finishing I ran the project's verify check and it FAILED — do not end the turn with it failing. Fix the errors below, then finish.\n\n"

	// truncatedNote is appended when a turn hit the model's output-token cap
	// (finish_reason "length"), so the cut-off answer is clearly flagged rather
	// than appearing as a silent/partial reply.
	truncatedNote = "\n\n_⚠ Response hit the output limit and was cut off. For a shorter answer, lower reasoning with `/effort low` (or `/effort none`), or ask a more focused question._"

	// systemPrompt is the SHARED base prompt — one agent loop, one prompt, used by
	// every model. Model-specific guidance is appended per model by modelAddendum
	// (see below): there is no separate "gemma harness" / "deepseek harness". The
	// tool-call-discipline + finish lines below are primarily Floko/gemma-driven
	// (small models leak tool calls / run under required tool-calling) but are
	// harmless for Chuppa, which finishes with plain text.
	systemPrompt = `You are borg, an AI coding agent that helps with software-engineering tasks from the user's current working directory, using tools to read, search, and edit files and run shell commands.

# Tone and replies
- Reply in the SAME language the user writes in. Answer directly: no preamble like "Here's what I'll do" and no postamble like "I have now done X", and don't restate or translate their question back.
- Be concise — output goes to a terminal. Match depth to the task: a quick question gets a short answer, a substantive change gets the explanation it needs. Skip filler, lead with what matters, and use GitHub-flavored Markdown.

# Doing the work
- Do exactly what's asked — no more, no less. Make the smallest, most targeted change that solves it; don't refactor, rename, reformat, or add features that weren't requested.
- Inspect before you act, and follow the project's existing conventions, style, and libraries. Never assume a dependency is available — confirm it's already used before relying on it.
- For a non-trivial change — implementing a feature or altering behavior, especially across files — EXPLORE before you edit: read the relevant code, types, and existing tests until you understand how it works and what the change must fit, then implement it in clear steps. Keep this proportional — a one-line fix needs a glance; a feature needs real exploration first. When a name, path, or schema is unknown, discover it (grep, list, inspect) rather than guessing — a wrong guess wastes a whole round-trip.
- Don't add comments, docs, or license headers unless asked or the surrounding code clearly expects them.
- Gather facts with tools instead of guessing: if a path, symbol, or command is unknown, search for it, and only rely on commands/APIs you've verified from the files.
- Before you report that something doesn't exist or there's nothing to do, sanity-check the query that told you so: an empty result from a filter, flag, or pattern YOU chose often means the filter is wrong, not that nothing matches — re-check it against the unfiltered set or a broader query before concluding absence. When unsure of a command's exact flags or arguments, consult its own help (its --help / usage) rather than guessing one.
- After editing code, verify it before finishing. If the project declares a verify command (shown below when it does), run THAT exact command — it builds/tests the project the way the project requires (often inside a container or via its own task runner); otherwise use the verify tool for a quick compile check. Either way, fix any failures before finishing. Do NOT improvise your own build or test by calling the toolchain directly in the shell (an ad-hoc compile/test command) — that bypasses the project's declared method and may run code in the wrong environment. Don't commit, push, or run irreversible commands unless the user explicitly asks.
- Work autonomously from terse or vague instructions: infer the intent, pick the most reasonable interpretation, and proceed — don't stop to ask for details you can reasonably decide or discover yourself. Only ask the user when you're genuinely blocked or a wrong guess would be costly or irreversible.
- When you DO need a decision from the user — a real fork where the options diverge materially and a wrong guess is costly or hard to undo — get it through the ask_user TOOL, never as prose. Concretely: if you are about to present a lettered or numbered list of options for the user to choose among (A/B/C, "option 1/2/3", "which direction?"), you MUST deliver that choice via ask_user. Stream any analysis/reasoning first as normal text (the user sees it), THEN call ask_user with 2–4 concise options — do NOT also write the options out in prose or ask "which do you want?" in text, because that leaves the user nothing to click. The user can pick an option or answer in their own words. Reserve this for genuine, consequential choices — never to confirm something obvious or to ask for a detail you can decide or discover yourself. One sound autonomous decision beats a needless question.
- Be resourceful when something goes wrong. A failed command, a timeout, a missing file, or a failing check is information, not a dead end: diagnose it and try the next best approach yourself (a different path, tool, or command) rather than handing the problem back. Carry the task to a finished, verified state on your own; if you truly must stop, say what you'd try next, don't just report the failure. Assist only with defensive, authorized security work.

# Using tools well
- Be token-efficient: use grep and glob (e.g. **/*.go) to locate code, then read_file in targeted ranges (offset/limit) rather than whole files — the whole conversation is re-sent every step, so a big file read once weighs on every later step.
- When you need several INDEPENDENT read-only operations (reading or grepping multiple files), request them as multiple tool calls in a SINGLE response — they run in parallel. Don't call one, wait, then call the next when they don't depend on each other.
- To change code: edit_lines replaces an inclusive line range by number (precise); edit_file replaces a unique exact snippet; read_file shows each line's number to guide both. For a long-running command (dev server, watch), pass run_in_background to bash and poll bash_output, then kill_shell when done.
- Your edits are AUTO-FORMATTED with the project's own formatter (when it has one) right after they apply, and every edit returns a diff of the final result. So do NOT fuss over exact indentation/whitespace, and never re-edit (or run sed/cat to inspect spacing) just to fix formatting — get the content right and let the formatter handle the rest. If two edits would land the same content, you're done; don't repeat it.
- ALWAYS invoke tools through the function/tool-calling interface so they actually run. NEVER write a tool call as plain text (e.g. "call:read_file{...}" or a <tool_call> block) — text is not executed, it just ends your turn.
- Printing a file's contents in your reply does NOT create or change it — to create or edit a file you MUST call write_file/edit_file. Never say you "wrote", "created", or "updated" a file unless you actually called the tool to do it.

# Shell and git
- Chain independent shell steps into ONE bash call with && (e.g. ` + "`git branch --show-current && git status -s && git log --oneline -10`" + `) rather than one command per turn — every turn is a full round-trip, so single-stepping read-only inspection is slow. Keep a mutating step (commit, push, merge, checkout) as its own call so a failure is unambiguous.
- Discover state before asking. When told to merge, promote, or "ship/land a PR", find the relevant PR yourself — ` + "`gh pr list`/`gh pr view`" + ` for open PRs, ` + "`git log`/`git status`" + ` for local state — and act on what you find instead of bouncing "which PR?" back. Only ask when it's genuinely ambiguous (several plausible PRs) or the action is irreversible and a wrong guess is costly. When a PR exists, prefer ` + "`gh pr merge`" + ` over a manual checkout+merge.
- ALWAYS write a git commit message through a quoted heredoc so the shell never interprets it: ` + "`git commit -F - <<'EOF'`" + ` then the message lines then ` + "`EOF`" + `. NEVER pass the message via ` + "`git commit -m \"...\"`" + ` when it spans multiple lines or contains a backtick, ` + "`$`" + `, or ` + "`$(...)`" + ` — inside double quotes the shell treats those as command substitution and EXECUTES them (e.g. a literal ` + "`` `borg learn` ``" + ` in the message runs that command, which can hang the commit forever or run something destructive). The single-quoted ` + "`<<'EOF'`" + ` delimiter makes every line literal, so any message text is safe.
- Write a real commit message, not just a subject: a concise Conventional-Commits subject (` + "`type: summary`" + `), then — for any change beyond a trivial/mechanical one — a blank line and a short body explaining WHAT changed and WHY. Keep it proportional (a one-line fix can be subject-only), but don't flatten a substantive change to a bare title. ("Be concise" governs your chat replies, not commit bodies.)
- You are yourself a running ` + "`borg`/`turborg`" + ` process. NEVER run ` + "`pkill`/`killall`/`kill`" + ` against ` + "`borg`" + ` or ` + "`turborg`" + ` — that kills the session you are running in. To install a freshly built binary OVER one that's currently running, do NOT ` + "`cp`" + ` onto it (that truncates the file in place and fails with "Text file busy"); use an atomic replace that swaps the inode instead — ` + "`install -m755 <src> <dst>`" + ` or ` + "`mv <src> <dst>`" + ` — which works fine while the old binary is executing. The new binary takes effect on the user's next launch.

# Finishing
When the task is done, give a brief summary of what you changed and anything the user should know. (If you can't reply with plain text, call the finish tool with that summary instead — it ends your turn the same way.)`

	// ProjectContextFile is borg's per-project context file (its CLAUDE.md): if
	// present in the working directory, its contents are appended to the system
	// prompt so project conventions reach the model. `borg install` generates it.
	ProjectContextFile = "BORG.md"
)

// LearnPrompt drives the agent to study the working directory and write a
// project-context file (BORG.md) of genuine depth — the synthesized, non-obvious
// understanding a newcomer could only get by reading many files, not a directory
// listing. Shared by `borg learn` and the REPL's /learn command; it narrates each
// step so the UI shows the chain of thought.
const LearnPrompt = `You are writing ` + ProjectContextFile + `, the file an AI coding agent will rely on to work in this project. Aim for INSIGHT, not a file listing: capture what someone could only learn by reading several files and connecting them. Use list_dir, read_file, and grep (you do not need bash). Narrate each step in one short line as you go.

WORK IN PARALLEL BURSTS — this is a broad task, so be fast: whenever you need several INDEPENDENT reads or searches (read_file on many files, or list_dir + grep on different targets), issue them ALL in ONE response as multiple tool calls. They run concurrently and count as one step, so a whole directory's files come back at once instead of one slow file per turn. Only go one-at-a-time when a step genuinely needs the previous result.

BE THOROUGH, THEN WRITE ONCE. The highest-value parts of this file — the non-obvious gotchas — live inside the project's core SOURCE files (the main modules, the trickiest logic, the seams where components integrate), not just READMEs and config. Open the most important source files and read them deeply enough to capture what someone could only learn by reading them; don't settle for a shallow skim. Cover the four areas below. Don't waste steps — never re-read a file you already have (its contents are in front of you), and don't loop on the same searches — but favor DEPTH: a missed gotcha misleads every future session, and the harness already bounds exploration so you can't loop forever. When you've genuinely covered the areas, write the whole file in one decisive pass.

Precision over confidence: state ONLY what you have verified from the files. NEVER claim two files are identical, duplicates, or "redundant" unless you actually compared them (grep a distinctive line from one against the other, or read both) — write "appear similar" if you are only inferring. A wrong or guessed fact here misleads every future session, so accuracy beats brevity.

Be thorough — do not write the file until you've done all of this:

1. EXISTING CONTEXT — check for an existing ` + ProjectContextFile + `, plus CLAUDE.md, AGENTS.md, GEMINI.md, .cursorrules, .cursor/rules/, .github/copilot-instructions.md, .windsurfrules. read_file the ones that exist. An existing CLAUDE.md / AGENTS.md is HARD-WON knowledge: carry ALL of its operational substance forward — deploy steps, incident post-mortems, gotchas, exact commands, env quirks — do NOT dismiss it as redundant or compress away the details that prevent breakage. ` + ProjectContextFile + ` must end up at least as COMPLETE as the best existing agent doc, plus your own synthesis. If ` + ProjectContextFile + ` already exists, treat this as an ENRICH/UPDATE pass: keep what's accurate, correct what's wrong, add what's missing — never replace a good file with a thinner one. SUPERSEDE only content you've confirmed is outdated or wrong.

2. INTENT & DOCS — read EVERY brief/readme/docs file (README*, every *.md, docs/). Never dismiss a markdown file as "miscellaneous" — these usually state the project's GOAL. Work out what the project is actually FOR.

3. STRUCTURE — list_dir each significant subdirectory and read a representative file from each, so you describe what's really inside (not "a directory with web content"). When several files or directories look similar (e.g. v1/v2/v3, website/website2/website3), COMPARE them (read or grep — don't guess) and determine the relationship: are they iterations of one thing — and which is canonical/latest — or separate features? Identify the real deliverable versus DECOYS (experiments, demos, templates, unrelated files), and say which is which.

4. HOW TO RUN & SHIP — find the build, test, lint, and run commands, AND how the project is deployed/released (CI workflows, Docker, blue-green/rollout, migrations, env-specific steps). If there's no formal tooling, state how the project is ACTUALLY run (e.g. "open the .html in a browser" or "python3 -m http.server"). Only state commands you can verify from the files — never invent them.

Then WRITE ` + ProjectContextFile + ` (enriching the existing one if present — keep its accurate parts, fix the wrong ones, fill the gaps) covering:
- a precise one-paragraph overview of what the project is and its goal;
- build / test / lint / run / deploy commands;
- a "## Verify" section containing ONLY the single canonical command this project uses to confirm a change is good — however THIS project checks itself (its tests, plus a build/lint/typecheck step only if that is part of its check). DISCOVER it from the project's OWN sources — its build file / task runner, its package or dependency manifest, its CI config, its README / contributor docs — do not assume a language or tool. Prefer the project's own wrapper (a task, target, or script) over a bare toolchain invocation, and honor any rule about HOW it must run (e.g. inside a container). Put it in a fenced code block. The harness runs this automatically to check edits, so it must be the real, correct command — one line, no commentary;
- the directory layout giving each key path's real purpose AND relationships (iterations, decoys);
- conventions a contributor must follow;
- OPERATIONAL GOTCHAS & FOOTGUNS — the highest-value section: the non-obvious things that bite an agent — deploy/runtime pitfalls, steps required after a change (reload, regenerate caches, recreate a worker), environment quirks, past incidents, and cross-file "why" connections (X needs Y because Z). Preserve every such gotcha from the existing docs; never drop this section to save space.
Favor synthesized, non-obvious findings over restating file names. Be COMPLETE on gotchas, concise elsewhere. Finish by stating what you wrote and which existing files you incorporated or superseded.`

// flokoAddendum is the Floko (gemma) -specific tail of the system prompt: a small
// model running under guided/required tool-calling benefits from extra discipline
// — methodical single steps, strict structured tool use, and the `finish`-to-end
// contract. Chuppa (DeepSeek) is strong enough to stay lean (no addendum). This is
// the "format exemplars / per-model guidance" lever.
const flokoAddendum = `

You are Floko, a fast and compact model — be rigorous and methodical to make every step count:
- Invoke EVERY tool through the function/tool-calling interface; never write a tool call as text.
- ANSWER DIRECTLY when no tools are needed: for a general question, or once you already have everything required, call the finish tool IMMEDIATELY with your answer. Do NOT read files or run commands first just to have called a tool — an unneeded read wastes a whole round-trip.
- The project context file (BORG.md), when present, is ALREADY included in this prompt above — never read_file it; you already have its contents.
- BATCH FOR SPEED: when you need several INDEPENDENT reads/searches (e.g. read_file on several files, or glob + grep + list_dir on different targets), put them ALL in ONE response as multiple tool calls — they run in PARALLEL and that is far faster than one tool per turn. Only go one-at-a-time when a step genuinely needs the previous result.
- Don't repeat a call that already failed, returned nothing, OR already succeeded — you already have that result; use it or finish. Never read the same file twice.
- You may be running under strict tool-calling where a plain-text reply isn't possible; to end your turn, call the finish tool with your complete answer as summary.`

// modelAddendum returns the per-model tail appended to the shared base prompt.
// Empty for models that need no extra guidance (e.g. Chuppa).
func modelAddendum(model string) string {
	switch strings.ToLower(model) {
	case "floko":
		return flokoAddendum
	default:
		return ""
	}
}

// attributionAddendum returns the prompt section that makes the model credit
// turborg on commits/PRs it creates, or "" when attribution is disabled. Kept in
// the system prompt (not enforced via a hook) because borg only commits when the
// user asks — the same approach Claude Code uses for its own trailer. Deterministic
// per config, so the prompt prefix stays cache-stable.
func attributionAddendum(cfg *config.Config) string {
	if cfg == nil || !cfg.GitAttribution {
		return ""
	}
	name, email := cfg.GitAttributionName, cfg.GitAttributionEmail
	if name == "" || email == "" {
		return ""
	}
	return "\n\n# Attribution\n" +
		"- When you create a git commit, write the message through a quoted heredoc (`git commit -F - <<'EOF'` … `EOF`, never `-m \"...\"`) so nothing in it is shell-interpreted, and add this trailer as the LAST line, after a blank line: `Co-Authored-By: " +
		name + " <" + email + ">`. Write the rest of the message yourself; never drop or alter this trailer.\n" +
		"- When you open a pull request (e.g. `gh pr create`), end the PR body with the footer line: `🤖 Opened with borg`."
}

// composeSystemPrompt builds the system prompt for a config: the shared base, the
// model-specific addendum, the turborg attribution section (if enabled), then the
// working directory's project-context file (if any). It's deterministic per config,
// so the prompt prefix stays cache-stable.
func composeSystemPrompt(cfg *config.Config) string {
	base := systemPrompt + environmentAddendum() + modelAddendum(cfg.Model) + attributionAddendum(cfg)
	body, err := os.ReadFile(ProjectContextFile)
	if err != nil || len(body) == 0 {
		return base
	}
	// Surface a declared verify command SALIENTLY (BORG.md alone is long and a small
	// model skims it). This is how the project says "build/test happens THIS way" —
	// so the model runs THIS command instead of improvising an ad-hoc host test run.
	// (Phrased as "run this command", NOT "use the verify tool": the verify tool is a
	// compile-only check; the project's real test command runs via bash / the loop's
	// auto-verify backstop, so telling the model to "use verify" made it waste a
	// compile-only step and then run the command anyway.)
	if cmd := parseVerifyCommand(string(body)); cmd != "" {
		base += "\n\n# Verifying your work\nThis project's verify command is `" + cmd + "` — run THAT to check your work. " +
			"Do NOT substitute or hand-roll a different build/test command (e.g. invoking the language toolchain directly); this exact command is declared because the project requires that method (a specific test runner, containerized execution, etc.), and improvising one runs code the wrong way. The loop also runs `" + cmd + "` automatically before a turn that edited code can finish."
	}
	return base + "\n\n# Project context (" + ProjectContextFile + ")\n" + string(body)
}

// verifyHeadingRE matches a "## Verify" heading in BORG.md (case-insensitive), the
// project's declaration of how to build/test itself.
var verifyHeadingRE = regexp.MustCompile(`(?im)^#{1,6}\s+verify\s*$`)

// parseVerifyCommand extracts a project's declared verify command from its
// BORG.md: the first non-empty line of the first fenced code block (or the first
// non-empty, non-comment line) under a "## Verify" heading. Returns "" when none
// is declared — the harness then falls back to its built-in compile-only check.
// General by design and language-agnostic: a repo declares whatever command IT
// uses to verify itself (a task-runner target, a test/build script, …), and the
// harness runs exactly THAT — borg assumes no language, tool, or command.
func parseVerifyCommand(borgmd string) string {
	loc := verifyHeadingRE.FindStringIndex(borgmd)
	if loc == nil {
		return ""
	}
	section := borgmd[loc[1]:]
	// Stop at the next heading so we only read this section.
	if next := regexp.MustCompile(`(?m)^#{1,6}\s+\S`).FindStringIndex(section); next != nil {
		section = section[:next[0]]
	}
	inFence := false
	for _, ln := range strings.Split(section, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") {
			if inFence { // closing fence with no command found inside
				break
			}
			inFence = true
			continue
		}
		if t == "" {
			continue
		}
		if inFence {
			return t // first command line inside the fence
		}
		// No fence: take the first real line (skip prose bullets/blockquotes).
		if !strings.HasPrefix(t, "#") && !strings.HasPrefix(t, ">") && !strings.HasPrefix(t, "-") {
			return strings.TrimPrefix(t, "$ ")
		}
	}
	return ""
}

// environmentAddendum states the actual working directory so the model anchors
// relative paths to it instead of inventing an absolute path. (A run thrashed for
// several steps after grepping a hallucinated absolute project path that didn't
// exist; tools run from this cwd, so naming it removes the guesswork.)
func environmentAddendum() string {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		return ""
	}
	return "\n\n# Environment\n" +
		"- Working directory: `" + cwd + "`. All tools (read_file, grep, list_dir, bash) run from here.\n" +
		"- Resolve paths RELATIVE to this directory. Do NOT invent or assume an absolute path — if you're unsure where something is, grep or list_dir from here rather than guessing a path that may not exist."
}

// LLM is the slice of *llm.Client the agent loop actually depends on. Making it
// an interface is the seam the eval harness relies on: it can swap in a scripted
// or replayed model for the live metered proxy, driving exact trajectories
// deterministically and without spending tokens. *llm.Client satisfies it as-is.
type LLM interface {
	Chat(ctx context.Context, msgs []llm.Message, tools []llm.Tool, think bool, onDelta func(string), opts ...llm.ChatOption) (*llm.Message, error)
	Models(ctx context.Context) ([]llm.ModelInfo, error)
	Tier(ctx context.Context) (string, error)
	Usage(ctx context.Context) (*llm.AccountUsage, error)
	SetModel(model string)
	SetEffort(effort string)
	SetDebug(func(string))
}

// Agent ties together the metered LLM client and the tool registry, and keeps
// the running conversation so REPL turns build on each other.
type Agent struct {
	cfg           *config.Config
	llm           LLM
	tools         *tools.Registry
	think         bool
	ui            UI
	messages      []llm.Message
	always        map[string]bool // tools approved for the whole session
	trustRoot     string          // edits confined to this dir ("" = unrestricted)
	effort        string          // explicit reasoning_effort ("" = follow the think toggle)
	debug         bool            // verbose diagnostics (full tool I/O, per-step trace, reasoning, raw HTTP)
	dbgLog        debugLog        // per-session diagnostics file sink (open only while debug is on)
	artifact      string          // basename the task MUST write_file before finishing ("" = none); e.g. BORG.md for /learn
	maxSteps      int             // per-Ask tool-call step cap (0 = defaultMaxSteps); only the eval harness lowers it
	escalateModel string          // opt-in model to tier up to once effort tops out and the task still struggles ("" = off)

	lastPromptTokens int // prompt tokens billed on the most recent step (current context occupancy)
	lastCachedTokens int // input tokens served from the prompt cache on that step

	modelWindows map[string]int // model id → context-window cap, from the live catalog (SetModelWindows)

	lastStruggle *Struggle // hard-thrash record of the most recent Ask (nil = the turn was clean)
}

// Struggle records that the most recent task triggered one or more of borg's
// recovery guards — i.e. the harness detected real thrash (a defeated/insufficient
// guard, a novel failure mode, or wasteful re-editing). It feeds the post-turn
// retrospective, which proposes a BORG.md note or a harness-problem report. nil
// when a turn completed cleanly, so the retrospective stays silent on smooth runs.
type Struggle struct {
	Task    string   // the user task that struggled
	Reasons []string // human-readable guard-fired reasons, deduped
	Steps   int      // tool-call steps the task took
	// Terminal is true when borg actually EXHAUSTED its options (gave up: no-progress
	// stop, step cap, or a guard's retries maxed) rather than thrashing-but-finishing.
	// It picks the retrospective's path: terminal → offer a harness report to the
	// team; soft (false) → offer a BORG.md note. Reporting is reserved for genuine
	// dead-ends so the team only hears about real, unresolved failures.
	Terminal bool
}

// Retro is the result of the post-thrash retrospective: a classification plus the
// proposed text. Kind is "borg_md" (a project-knowledge note to add to BORG.md),
// "harness" (a limitation borg can't fix via docs — offer to report it), or "none".
type Retro struct {
	Kind string
	Text string
}

// New builds an Agent for the given config and authenticated session, backed by
// the live metered LLM proxy.
func New(cfg *config.Config, creds *auth.Credentials) *Agent {
	return NewWithLLM(cfg, llm.New(cfg, creds.AccessToken))
}

// NewWithLLM builds an Agent backed by a caller-supplied model client instead of
// the metered proxy. This is the constructor the eval harness uses to drive the
// loop with a scripted or replayed model. Production code uses New.
func NewWithLLM(cfg *config.Config, client LLM) *Agent {
	return &Agent{
		cfg:           cfg,
		llm:           client,
		tools:         tools.DefaultRegistry(),
		ui:            newPlainUI(),
		messages:      []llm.Message{{Role: "system", Content: composeSystemPrompt(cfg)}},
		always:        map[string]bool{},
		maxSteps:      defaultMaxSteps,
		escalateModel: cfg.EscalateModel,
	}
}

// AllowTools pre-approves the named tools for this session (no permission prompt).
// `borg learn` uses it so generating BORG.md doesn't prompt for the write.
func (a *Agent) AllowTools(names ...string) {
	for _, n := range names {
		a.always[n] = true
	}
}

// SetTrustRoot confines the editing tools to root (write_file/edit_file refuse
// paths outside it). "" leaves edits unrestricted. Reads and bash are unaffected.
func (a *Agent) SetTrustRoot(root string) { a.trustRoot = root }

// RequireArtifact marks a file the task MUST actually write_file before it can
// finish (by basename). It stops a model from "completing" by printing the file's
// contents inline and claiming it wrote it — the loop forces a real write_file
// call instead. `borg learn` requires BORG.md. This is a completion guard, not a
// permission one: the user's write_file y/n prompt still approves the call.
func (a *Agent) RequireArtifact(name string) { a.artifact = name }

// ConfigureLearn arms the agent for a `learn` task — the SINGLE source of truth
// for what a learn run means at the harness level, shared by `borg learn` (CLI)
// and the REPL's /learn so the two can't drift: require BORG.md to actually be
// written before finishing, with a generous step backstop for a broad read.
//
// Effort is HIGH: BORG.md is a write-once, high-leverage artifact the agent relies
// on for every future session, so it's worth the deepest synthesis. High effort
// was previously slow (~13 min) because the upstream model streamed at ~13 tok/s;
// with the faster provider that penalty is gone, so we pay for the best doc.
// Surface-specific concerns (permission pre-approval, echo lines) stay at each
// call site; pair this with Ask(ctx, agent.LearnPrompt).
func (a *Agent) ConfigureLearn() {
	a.SetEffort("high")
	a.SetMaxSteps(learnMaxSteps) // broad task: read the whole repo, then write BORG.md
	a.RequireArtifact(ProjectContextFile)
}

// SetUI swaps the renderer (the REPL injects a styled one).
func (a *Agent) SetUI(ui UI) { a.ui = ui }

// SetThink toggles reasoning for subsequent requests.
func (a *Agent) SetThink(think bool) { a.think = think }

// Think reports whether reasoning is on.
func (a *Agent) Think() bool { return a.think }

// SetEffort sets an explicit reasoning_effort (none|low|medium|high|xhigh) for
// subsequent requests; "" follows the think toggle (the model's default level).
func (a *Agent) SetEffort(effort string) {
	a.effort = effort
	a.llm.SetEffort(effort)
}

// Effort returns the current explicit reasoning effort ("" = follow think).
func (a *Agent) Effort() string { return a.effort }

// SetMaxSteps overrides the per-Ask tool-call step cap (n <= 0 restores the
// default of 30). Production never calls this — it exists for the eval harness,
// which bounds the worst-case token blow-up of a task that wanders toward the cap.
func (a *Agent) SetMaxSteps(n int) { a.maxSteps = n }

// SetEscalateModel opts into model tiering: the model the agent tiers up to once
// reasoning effort has topped out and the task is still struggling ("" = off, the
// default — no surprise premium spend).
func (a *Agent) SetEscalateModel(m string) { a.escalateModel = m }

// ApplySetting applies a changed persistent setting (by its config.Setting key) to
// the running agent, so a /settings tweak takes effect without a restart. It's the
// single live-apply seam shared by both front-ends, mirroring the cfg fields that
// composeSystemPrompt and the escalation ladder read so a running session matches
// what a fresh start would load from settings.json. Restart-only settings (e.g.
// force_device, which only matters during login) are intentionally no-ops here.
func (a *Agent) ApplySetting(key, value string) {
	truthy := func(v string) bool { b, _ := strconv.ParseBool(v); return b }
	switch key {
	case "escalate_model":
		a.SetEscalateModel(value)
	case "think":
		a.SetThink(truthy(value))
	case "debug":
		a.SetDebug(truthy(value))
	case "git_attribution":
		a.cfg.GitAttribution = truthy(value)
		a.refreshSystemPrompt()
	}
}

// refreshSystemPrompt rebuilds the system message from the current cfg, so a
// mid-session change to an input of composeSystemPrompt (model, attribution) is
// reflected without disturbing the rest of the conversation.
func (a *Agent) refreshSystemPrompt() {
	if len(a.messages) > 0 && a.messages[0].Role == "system" {
		a.messages[0].Content = composeSystemPrompt(a.cfg)
	}
}

// Artifact returns the file the current task must write_file before finishing
// ("" = none). Exposed so callers/tests can confirm the completion guard is armed.
func (a *Agent) Artifact() string { return a.artifact }

// SetDebug toggles verbose diagnostics: full tool arguments/results with timing
// and a per-step request trace (this file), plus the LLM client's reasoning and
// raw-HTTP traces, all routed to the UI's Debug sink. Safe for users to enable —
// it only surfaces their own session data, never server-side secrets (the
// provider key never reaches the client) or the bearer token.
func (a *Agent) SetDebug(on bool) {
	a.debug = on
	if on {
		a.openDebugLog()          // best-effort per-session file under ~/.config/borg/logs
		a.llm.SetDebug(a.dbgEmit) // route the client's traces through the tee too
	} else {
		a.llm.SetDebug(nil)
		a.dbgLog.close()
	}
}

// Debug reports whether verbose diagnostics are on.
func (a *Agent) Debug() bool { return a.debug }

// dbgEmit tees one diagnostic line to the UI and (while debug is on) the
// per-session log file, so both the live view and the saved trace see everything.
func (a *Agent) dbgEmit(s string) {
	a.ui.Debug(s)
	a.dbgLog.write(s)
}

// openDebugLog opens a fresh per-session diagnostics file under
// ~/.config/borg/logs (0700 dir, 0600 file). Best-effort: any failure here never
// blocks the session — debug still streams to the UI; the file is just a bonus.
// The path is surfaced so the user knows where the trace lives.
func (a *Agent) openDebugLog() {
	dir, err := os.UserConfigDir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, "borg", "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	path := filepath.Join(dir, "borg-"+time.Now().Format("20060102-150405")+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	a.dbgLog.open(f)
	a.ui.Debug("debug log → " + path)
}

// debugLog is a concurrency-safe per-session file sink for verbose diagnostics
// (the agent calls it from parallel tool goroutines). Open only while debug is on.
type debugLog struct {
	mu sync.Mutex
	f  *os.File
}

func (d *debugLog) open(f *os.File) {
	d.close() // replace any prior file (debug toggled off→on again)
	d.mu.Lock()
	d.f = f
	d.mu.Unlock()
}

func (d *debugLog) write(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f != nil {
		fmt.Fprintf(d.f, "%s  %s\n", time.Now().Format("15:04:05.000"), s)
	}
}

func (d *debugLog) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f != nil {
		_ = d.f.Close()
		d.f = nil
	}
}

// SetModel switches the model codename for subsequent requests, refreshing the
// system prompt's per-model addendum (so a mid-session /model switch gets the new
// model's guidance). Leaves any resumed/edited conversation otherwise intact.
func (a *Agent) SetModel(model string) {
	a.cfg.Model = model
	a.llm.SetModel(model)
	if len(a.messages) > 0 && a.messages[0].Role == "system" {
		a.messages[0].Content = composeSystemPrompt(a.cfg)
	}
}

// Model returns the current model codename.
func (a *Agent) Model() string { return a.cfg.Model }

// Models returns the model catalog (labels, versions, per-plan availability).
func (a *Agent) Models(ctx context.Context) ([]llm.ModelInfo, error) { return a.llm.Models(ctx) }

// Tier returns the caller's plan code (free, starter, pro, max).
func (a *Agent) Tier(ctx context.Context) (string, error) { return a.llm.Tier(ctx) }

// Usage returns the account's plan, caps, and rolling-24h token usage.
func (a *Agent) Usage(ctx context.Context) (*llm.AccountUsage, error) { return a.llm.Usage(ctx) }

// UserInfo returns the logged-in user's identity (name/email) and plan. It is
// only available on the live metered client (not the eval/scripted seams), so it
// returns (nil, nil) when the agent is backed by a substitute LLM.
func (a *Agent) UserInfo(ctx context.Context) (*llm.UserInfo, error) {
	if c, ok := a.llm.(*llm.Client); ok {
		return c.UserInfo(ctx)
	}
	return nil, nil
}

// AuthInfo summarizes the stored login for /status: the environment the token
// targets and when it expires. LoggedIn is false (and Expiry zero) when there
// are no stored credentials.
type AuthInfo struct {
	APIBaseURL string
	AppURL     string
	LoggedIn   bool
	TokenType  string
	Expiry     time.Time
}

// AuthInfo reports the current environment and stored-credential state.
func (a *Agent) AuthInfo() AuthInfo {
	info := AuthInfo{APIBaseURL: a.cfg.APIBaseURL, AppURL: a.cfg.AppURL}
	if creds, err := auth.LoadCredentials(); err == nil {
		info.LoggedIn = true
		info.TokenType = creds.TokenType
		info.Expiry = creds.Expiry
	}
	return info
}

// Login runs the OAuth flow for the configured environment (browser PKCE, or the
// device flow when no browser is available) and, on success, swaps the agent's
// metered client to use the freshly issued token.
func (a *Agent) Login(ctx context.Context) error {
	au, err := auth.New(a.cfg)
	if err != nil {
		return err
	}
	creds, err := au.Login(ctx)
	if err != nil {
		return err
	}
	c := llm.New(a.cfg, creds.AccessToken)
	c.SetEffort(a.effort) // preserve the explicit effort across the client swap
	a.llm = c
	return nil
}

// Logout removes the stored credentials (local only — it does not revoke the
// token server-side, and the in-memory client keeps working until the process
// exits).
func (a *Agent) Logout() error {
	au, err := auth.New(a.cfg)
	if err != nil {
		return err
	}
	return au.Logout()
}

// Reset clears the conversation, keeping only the system prompt.
func (a *Agent) Reset() {
	a.messages = a.messages[:1]
	a.lastPromptTokens, a.lastCachedTokens = 0, 0
}

// Messages returns a copy of the running conversation, for snapshotting into a
// resumable session.
func (a *Agent) Messages() []llm.Message {
	out := make([]llm.Message, len(a.messages))
	copy(out, a.messages)
	return out
}

// LastToolOutput returns the full result of the most recent tool call (the
// complete content the model saw, byte-capped by the tool itself), or "" if no
// tool has run. The REPL's `/output` surfaces it so the user can see a long bash
// output / diff in full, not just the one-line preview shown inline.
func (a *Agent) LastToolOutput() (name, output string) {
	for i := len(a.messages) - 1; i >= 0; i-- {
		if a.messages[i].Role == "tool" {
			return a.messages[i].Name, a.messages[i].Content
		}
	}
	return "", ""
}

// LastStruggle returns the hard-thrash record of the most recent Ask, or nil if
// that turn completed cleanly. The REPL reads it after a turn to decide whether to
// offer the retrospective; ClearStruggle resets it once handled.
func (a *Agent) LastStruggle() *Struggle { return a.lastStruggle }

// ClearStruggle drops the pending struggle record (after the retrospective has been
// offered/handled, or declined) so it isn't re-offered.
func (a *Agent) ClearStruggle() { a.lastStruggle = nil }

// RetrospectInput builds the compact, factual input for the reflection — the task,
// the guard-fired reasons, and the sequence of tool calls (names + short args) so
// the model can see HOW the task thrashed without re-sending the whole transcript.
// Returns "" when there's nothing to reflect on. Call it on the event loop (it
// reads conversation state); the actual LLM call (ReflectOn) then runs off it with
// just this string, so there's no data race with a subsequent turn.
func (a *Agent) RetrospectInput() string {
	if a.lastStruggle == nil {
		return ""
	}
	var b strings.Builder
	s := a.lastStruggle
	fmt.Fprintf(&b, "TASK: %s\n\nSTEPS: %d\n\nGUARDS THAT FIRED:\n", s.Task, s.Steps)
	for _, r := range s.Reasons {
		fmt.Fprintf(&b, "- %s\n", r)
	}
	b.WriteString("\nTOOL CALLS (in order):\n")
	for _, m := range a.messages {
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "- %s\n", firstLine(ToolCallLine(tc.Function.Name, tc.Function.Arguments)))
		}
	}
	// Include any already-recorded auto-lessons so a soft-case reflection can avoid
	// proposing a duplicate (anti-BORG.md-bloat).
	if l := existingLessons(); l != "" {
		fmt.Fprintf(&b, "\nEXISTING BORG.md LESSONS (do not repeat):\n%s\n", l)
	}
	return b.String()
}

// ReflectOn makes ONE metered reflection call over a pre-built input (from
// RetrospectInput). The struggle's SEVERITY (terminal) picks the path: a terminal
// give-up yields a harness report; a soft thrash yields a BORG.md note. It uses a
// throwaway message set (never the conversation), so it can't corrupt the session.
// A model reply of "NONE" (or empty) yields Kind "none" — the retrospective stays
// silent, which is the common, desired outcome.
func (a *Agent) ReflectOn(ctx context.Context, input string, terminal bool) (*Retro, error) {
	if strings.TrimSpace(input) == "" {
		return &Retro{Kind: RetroKindNone}, nil
	}
	prompt, kind := retroLearnPrompt, RetroKindBorgMD
	if terminal {
		prompt, kind = retroReportPrompt, RetroKindHarness
	}
	reply, err := a.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: prompt},
		{Role: "user", Content: input},
	}, nil, false, func(string) {})
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(reply.Content)
	if text == "" || strings.EqualFold(strings.Trim(text, ". \n"), retroNoneSentinel) {
		return &Retro{Kind: RetroKindNone}, nil
	}
	return &Retro{Kind: kind, Text: text}, nil
}

// existingLessons returns the bullet lines under BORG.md's auto-lessons section (or
// "") so a reflection can dedup against them.
func existingLessons() string {
	body, err := os.ReadFile(ProjectContextFile)
	if err != nil {
		return ""
	}
	const header = "## Lessons (auto-added by borg)"
	_, after, found := strings.Cut(string(body), header)
	if !found {
		return ""
	}
	var lines []string
	for _, ln := range strings.Split(after, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "## ") { // next section ends the block
			break
		}
		if strings.HasPrefix(t, "- ") {
			lines = append(lines, t)
		}
	}
	return strings.Join(lines, "\n")
}

// dedupStrings returns s with duplicates removed, preserving first-seen order.
func dedupStrings(s []string) []string {
	seen := map[string]bool{}
	out := s[:0]
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// ApplyRetroLearn appends a retrospective's BORG.md note under an auto-maintained
// section, so a project-knowledge lesson persists into future sessions. It creates
// BORG.md if absent. The exact text was shown to and approved by the user.
func (a *Agent) ApplyRetroLearn(note string) error {
	note = strings.TrimSpace(note)
	if note == "" {
		return nil
	}
	const header = "## Lessons (auto-added by borg)"
	body, _ := os.ReadFile(ProjectContextFile)
	content := string(body)
	if strings.Contains(content, note) {
		return nil // already recorded — don't duplicate (anti-bloat)
	}
	if strings.Contains(content, header) {
		content = strings.TrimRight(content, "\n") + "\n- " + note + "\n"
	} else {
		if content != "" {
			content = strings.TrimRight(content, "\n") + "\n\n"
		}
		content += header + "\n- " + note + "\n"
	}
	return os.WriteFile(ProjectContextFile, []byte(content), 0o644)
}

// feedbackSubmitter is satisfied by *llm.Client; type-asserting it (rather than
// widening the LLM interface) keeps the eval/test fakes untouched.
type feedbackSubmitter interface {
	SubmitFeedback(ctx context.Context, kind, report string, meta map[string]string) error
}

// SubmitHarnessReport sends an approved harness-problem report to the borg team's
// feedback endpoint. It returns an error the UI surfaces if the endpoint isn't
// available (e.g. not deployed yet) — nothing is ever sent without this call, which
// the UI only makes after an explicit user y/N.
func (a *Agent) SubmitHarnessReport(ctx context.Context, report string, s *Struggle) error {
	fs, ok := a.llm.(feedbackSubmitter)
	if !ok {
		return errors.New("feedback reporting is unavailable in this build")
	}
	meta := map[string]string{"model": a.cfg.Model, "version": version.Version}
	if s != nil {
		meta["task"] = s.Task
		meta["reasons"] = strings.Join(s.Reasons, "; ")
	}
	return fs.SubmitFeedback(ctx, "harness_problem", report, meta)
}

// SetMessages replaces the conversation history (used when resuming a session).
// The passed slice is expected to already include the system prompt; an empty
// slice falls back to a fresh system-only conversation.
//
// The system prompt is harness config, not conversation, so a resumed session
// always adopts the CURRENT prompt (composed for the current model) rather than
// replaying the stale one saved at the session's creation — that way prompt
// improvements (language rules, tool guidance) reach old sessions too. The rest
// of the transcript is preserved verbatim.
func (a *Agent) SetMessages(m []llm.Message) {
	if len(m) == 0 {
		a.Reset()
		return
	}
	a.messages = append([]llm.Message(nil), m...)
	sys := llm.Message{Role: "system", Content: composeSystemPrompt(a.cfg)}
	if a.messages[0].Role == "system" {
		a.messages[0] = sys // refresh in place
	} else {
		a.messages = append([]llm.Message{sys}, a.messages...) // none stored — prepend
	}
}

// RestoreSession applies a saved session's settings + transcript to the agent —
// the SINGLE source of truth for which fields a resume carries, shared by the CLI
// (--attach) and the REPL (/sessions) so the two can't drift. SnapshotSession is
// the exact inverse.
func (a *Agent) RestoreSession(s *session.Session) {
	a.SetModel(s.Model)
	a.SetThink(s.Think)
	a.SetEffort(s.Effort)
	a.SetMessages(s.Messages)
	// SetMessages reset the live measurement; restore the last-known context size
	// so /context reads exact (not an estimate) immediately after attach.
	a.lastPromptTokens = s.ContextTokens
}

// SnapshotSession captures the agent's current settings + transcript into the
// session for persistence. Inverse of RestoreSession; keep the two in lockstep so
// a newly-persisted setting always round-trips on resume.
func (a *Agent) SnapshotSession(s *session.Session) {
	s.Model = a.Model()
	s.Think = a.Think()
	s.Effort = a.Effort()
	s.ContextTokens = a.lastPromptTokens
	s.Messages = a.Messages()
}

// Ask runs the agent loop on a single task, building on the conversation so far.
func (a *Agent) Ask(ctx context.Context, task string) error {
	ctx = tools.WithRoot(ctx, a.trustRoot) // confine edits to the trusted directory
	// A required artifact is a property of THIS task, not the agent: clear it when
	// the task ends so a persistent REPL agent doesn't carry "must write BORG.md"
	// into the next, unrelated turn after /learn.
	defer func() { a.artifact = "" }()
	a.messages = append(a.messages, llm.Message{Role: "user", Content: task})
	defs := toolDefs(a.tools)

	// Struggle tracking: record which recovery guards fire so the post-turn
	// retrospective can offer to learn from a hard-thrash task. nil-by-default; set
	// only if a guard fired (so a clean turn stays silent).
	a.lastStruggle = nil
	var struggleReasons []string
	terminal := false           // borg actually gave up (vs thrashed-but-finished)
	reEdits := map[string]int{} // successful edits per file (re-edit thrash signal)
	stepsTaken := 0

	leakRetries := 0
	repetitionRetries := 0          // turns re-issued after the stream guard cut a prose loop short
	forceStructured := false        // one-shot: escalate the NEXT turn to required tool-calling after a detected leak
	var recentSigs []string         // sliding window of recent step signatures, for cycle detection
	var recentResults []string      // sliding window of read-only step result-signatures (result-only loop net)
	nudgedSigs := map[string]bool{} // signatures already nudged for (nudge once per cycle)
	lastEditStep := 0               // step index of the last successful mutating edit (circuit breaker)
	codeDirty := false              // source was edited but not yet confirmed to compile
	verifyRepairs := 0              // auto-verify failures fed back this turn (bounded)
	artifactWritten := false        // a write_file call has targeted a.artifact (the required deliverable)
	artifactRetries := 0            // turns re-issued because the required artifact wasn't written yet
	artifactFlushed := false        // the near-the-cap "write the file NOW" nudge has fired (once)
	editThrashHandled := false      // the in-turn re-edit-thrash escalation+nudge has fired (once per task)
	lastVerifyGreenStep := -1       // step at which a verify/compile check last passed green
	readOnlyStreak := 0             // consecutive read-only-only steps (over-verification / search signal)
	everEdited := false             // at least one successful edit has landed this task
	finishBrakeFired := false       // the post-verify-green finish nudge has fired (once per task)
	exploreNudged := false          // the explore-without-acting nudge has fired (once per task)

	// Finalize the struggle record once the task ends (any return path): if a
	// recovery guard fired, the post-turn retrospective will offer to learn from it.
	defer func() {
		for path, n := range reEdits {
			if n > maxReEdits {
				struggleReasons = append(struggleReasons, fmt.Sprintf("re-edited %s %d times (whitespace/format churn?)", filepath.Base(path), n))
			}
		}
		if leakRetries >= maxLeakRetries {
			struggleReasons = append(struggleReasons, "the model repeatedly emitted tool calls as plain text (leaked), needing forced re-issues")
			terminal = true
		}
		if repetitionRetries >= maxRepetitionRetries {
			struggleReasons = append(struggleReasons, "the model looped in prose without acting, needing forced re-issues")
			terminal = true
		}
		if verifyRepairs >= maxAutoVerifyRepairs {
			struggleReasons = append(struggleReasons, "edits kept failing the compile check and had to be repaired repeatedly")
			terminal = true
		}
		if len(struggleReasons) > 0 {
			a.lastStruggle = &Struggle{Task: task, Reasons: dedupStrings(struggleReasons), Steps: stepsTaken, Terminal: terminal}
		}
	}()

	// Effort escalation: start on the cheap default and raise reasoning only when
	// the turn is visibly struggling (no progress, or a failed compile check fed
	// back). Active ONLY when the dev hasn't pinned effort/think themselves — an
	// explicit choice is always respected, never overridden. Once raised it stays
	// raised for the rest of the task (if it got hard, it's probably still hard).
	autoEffort := a.effort == "" && !a.think
	effortRung := 0
	effortLadder := []string{"medium", "high", "xhigh"}
	modelEscalated := false
	// escalateModelTier tiers up to the configured premium model, once, when opted
	// in via escalateModel. Default (escalateModel "") never tiers, so there is no
	// surprise premium spend. Returns true if it tiered on this call.
	escalateModelTier := func() bool {
		if a.escalateModel != "" && !modelEscalated {
			modelEscalated = true
			a.llm.SetModel(a.escalateModel)
			a.dbgEmit("escalated to model " + a.escalateModel + " (task still struggling)")
			return true
		}
		return false
	}
	// escalate raises reasoning effort when a task is struggling — climbing the
	// none→medium→high→xhigh ladder, but ONLY while borg is auto-managing effort and
	// has headroom. It fires on a failed compile fed back (a genuine reasoning need)
	// and on going-in-circles; the xhigh-then-give-up backstop (the circuit breaker +
	// the stop branch) caps the downside so it can't burn indefinitely. Model tiering
	// is deliberately NOT done here: it's the LAST lever, fired only at a terminal
	// give-up via tierAndRetry, so the premium model is engaged only when borg would
	// otherwise fail (cost-safe), never on an ordinary mid-task nudge.
	escalate := func() {
		if autoEffort && effortRung < len(effortLadder) {
			effortRung++
		}
	}
	// tierAndRetry is the LAST lever before a terminal give-up: if a stronger model is
	// configured (opt-in) and not yet used, switch to it ONCE and grant a fresh window
	// (reset the loop/cycle state) so it can finish what the cheaper model couldn't,
	// then `continue` instead of returning. Returns false when there's no lever left
	// (no model configured or already tiered) — the caller then gives up for real.
	// Cost-safe by construction: it only ever engages the premium model when it was
	// explicitly configured AND borg was otherwise about to fail. curStep re-arms the
	// circuit-breaker window at the new model (the loop var can't be captured directly).
	tierAndRetry := func(curStep int) bool {
		if !escalateModelTier() {
			return false
		}
		recentSigs = recentSigs[:0]
		recentResults = recentResults[:0]
		nudgedSigs = map[string]bool{}
		lastEditStep = curStep
		exploreNudged = false
		a.messages = append(a.messages, llm.Message{Role: "user", Content: fmt.Sprintf(modelTierRetryMsg, a.escalateModel)})
		return true
	}
	// forceArtifact intercepts a finish when the task must write a file but no
	// write_file call has targeted it yet — the model described/pasted it instead
	// of creating it. Re-issue under forced tool-calling, bounded.
	forceArtifact := func() bool {
		if a.artifact == "" || artifactWritten || artifactRetries >= maxArtifactRetries {
			return false
		}
		artifactRetries++
		forceStructured = true
		a.messages = append(a.messages, llm.Message{Role: "user", Content: fmt.Sprintf(artifactNotWrittenMsg, a.artifact)})
		return true
	}
	stepCap := a.maxSteps
	if stepCap <= 0 {
		stepCap = defaultMaxSteps
	}
	for step := 0; step < stepCap; step++ {
		stepsTaken = step + 1
		// Budget-aware artifact flush: if the task REQUIRES a file (e.g. /learn →
		// BORG.md) and we're nearly out of budget — either steps OR context window —
		// without having written it, stop exploring and force the write now, so a
		// broad run always ends WITH the deliverable rather than stopping empty. The
		// context trigger is the non-lossy answer to a huge repo: write from the
		// real files still in context instead of silently compacting them away.
		nearCap := step >= stepCap-artifactFlushMargin
		ctxFull := a.ContextStats().Percent() >= contextFlushPct
		explored := step >= artifactExploreBudget // over-exploring: force the write
		if a.artifact != "" && !artifactWritten && !artifactFlushed && (nearCap || ctxFull || explored) {
			artifactFlushed = true
			forceStructured = true // make the next turn call write_file, not narrate the file
			a.messages = append(a.messages, llm.Message{Role: "user", Content: fmt.Sprintf(artifactFlushMsg, a.artifact)})
		}
		start := time.Now()
		a.ui.ThinkingStart()
		// Default to the model's "auto" tool_choice so an ordinary turn can stream a
		// plain-text answer (or call tools as it sees fit), like Claude. Only force
		// guided/required tool-calling on the turn that follows a detected leak.
		var chatOpts []llm.ChatOption
		if forceStructured {
			chatOpts = append(chatOpts, llm.ForceToolChoice("required"))
		}
		forceStructured = false // consume the one-shot
		if autoEffort && effortRung > 0 {
			chatOpts = append(chatOpts, llm.WithEffort(effortLadder[effortRung-1]))
		}
		reply, err := a.llm.Chat(ctx, a.messages, defs, a.think, a.ui.Delta, chatOpts...)
		stats := Stats{Elapsed: time.Since(start)}
		if reply != nil && reply.Usage != nil {
			stats.InTokens = reply.Usage.PromptTokens
			stats.OutTokens = reply.Usage.CompletionTokens
			stats.CachedTokens = reply.Usage.CachedTokens
			// Track the most recent prompt size as the current context occupancy
			// (each step re-sends the whole conversation), for /context + the footer.
			if reply.Usage.PromptTokens > 0 {
				a.lastPromptTokens = reply.Usage.PromptTokens
				a.lastCachedTokens = reply.Usage.CachedTokens
			}
		}
		a.ui.AssistantEnd(stats) // always runs, so the indicator is always stopped
		if err != nil {
			return err
		}
		a.messages = append(a.messages, *reply)
		if a.debug {
			eff := a.effort
			if eff == "" {
				if a.think {
					eff = "think-on"
				} else {
					eff = "none"
				}
			}
			if autoEffort && effortRung > 0 {
				eff = effortLadder[effortRung-1] + " (auto-escalated)"
			}
			a.dbgEmit(fmt.Sprintf("step %d  %s effort=%s  in=%d out=%d cached=%d  finish=%s  %.1fs",
				step+1, a.cfg.Model, eff, stats.InTokens, stats.OutTokens, stats.CachedTokens, reply.FinishReason, stats.Elapsed.Seconds()))
		}

		// The stream guard cut a degenerate prose loop short (the model was
		// repeating itself instead of acting). Re-issue the turn under forced
		// tool-calling with a directive to act — bounded, so a model that simply
		// can't proceed still falls through to a normal finish rather than looping.
		if reply.FinishReason == llm.FinishReasonRepetition && repetitionRetries < maxRepetitionRetries {
			repetitionRetries++
			// Force a structured tool call rather than more reasoning — extra effort
			// would just feed the deliberation loop. Guided decoding makes the model act.
			forceStructured = true
			a.messages = append(a.messages, llm.Message{Role: "user", Content: repetitionRetryMsg})
			continue
		}

		if len(reply.ToolCalls) == 0 {
			// Some models (notably gemma-class) occasionally botch a tool call —
			// emitting it as plain TEXT, or as a malformed function call — which
			// would silently end the turn. DeepInfra flags the latter explicitly
			// via finish_reason=malformed_function_call; we also sniff a leaked text
			// tool call. Either way, nudge the model to re-issue it properly, a
			// bounded number of times, so a flaky serialization can't strand the
			// loop. (The durable fix is guided/required tool-calling — see BORG.md.)
			malformed := reply.FinishReason == "malformed_function_call" ||
				looksLikeTextToolCall(reply.Content, a.tools.Names())
			if leakRetries < maxLeakRetries && malformed {
				leakRetries++
				forceStructured = true // re-issue the turn under required tool-calling so guided decoding can't leak
				a.messages = append(a.messages, llm.Message{Role: "user", Content: toolCallCorrection})
				continue
			}
			if forceArtifact() {
				continue // tried to finish without ever writing the required file
			}
			if a.finishOrRepair(ctx, &codeDirty, &verifyRepairs) {
				escalate() // a failed compile check fed back — give the fix more reasoning
				continue
			}
			return nil
		}
		leakRetries = 0
		repetitionRetries = 0
		results := a.runToolCalls(ctx, reply.ToolCalls)
		for i, tc := range reply.ToolCalls {
			a.messages = append(a.messages, llm.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    results[i],
			})
		}
		// Track whether source was edited (arms auto-verify) and clear the flag if
		// the model verified it itself this step — order-preserving so an
		// edit-then-verify in one turn ends clean. A model-issued compile-only
		// `verify{}` only stands in for the backstop when the project declares NO
		// verify command of its own (then the built-in compile check IS the
		// backstop). When the project DOES declare one (BORG.md "## Verify", e.g. a
		// containerized `make docker-test`), a compile-only pass is NOT that real
		// check — keep the edit dirty so finishOrRepair still runs the declared
		// command before the turn ends. The model running that exact declared
		// command itself (via bash) DOES satisfy it, so a clean run dedups it.
		declaredVerify := a.projectVerifyCommand()
		for i, tc := range reply.ToolCalls {
			switch {
			case editTouchesSource(tc.Function.Name, tc.Function.Arguments):
				codeDirty = true
			case tc.Function.Name == "verify":
				if declaredVerify == "" {
					if strings.HasPrefix(results[i], "FAIL") {
						codeDirty = true
					} else {
						codeDirty = false
						lastVerifyGreenStep = step // compile check passed (it IS the backstop here)
					}
				}
			case declaredVerify != "" && tc.Function.Name == "bash" &&
				commandRunsVerify(tc.Function.Arguments, declaredVerify):
				if bashSucceeded(results[i]) {
					codeDirty = false
					lastVerifyGreenStep = step // the project's OWN declared verify passed
				} else {
					codeDirty = true
				}
			}
			if writesArtifact(tc.Function.Name, tc.Function.Arguments, a.artifact) {
				artifactWritten = true // the required deliverable was actually write_file'd
			}
			// A successful mutating tool is real forward progress — note the step so
			// the escalation circuit breaker can tell "still working" from "stuck while
			// burning expensive reasoning". But a repeated edit to an ALREADY-thrashed
			// file is churn, not progress: once a path crosses maxReEdits, further edits
			// to it must NOT keep re-arming the breaker, or a debug loop can re-arm it
			// forever and the give-up never trips. Keyed only on edit-tool + path count.
			if isEditTool(tc.Function.Name) && !strings.HasPrefix(results[i], "error:") {
				everEdited = true
				p := argField(tc.Function.Arguments, "path")
				reEdits[p]++
				if reEdits[p] <= maxReEdits {
					lastEditStep = step
				}
			}
		}
		// Track consecutive read-only-only steps (a step with no mutating call) — the
		// shared signal for over-verification (after a green) and open-ended search.
		if a.readOnlyStep(reply.ToolCalls) {
			readOnlyStreak++
		} else {
			readOnlyStreak = 0
		}
		// Re-edit thrash → reason, don't churn. Editing the SAME file enough times in
		// one task is the debug-loop signature: the model keeps making "progress" (so
		// neither the cycle detector nor the circuit breaker fire) yet isn't resolving
		// the problem, burning a full round-trip per incremental edit. The first time a
		// file crosses the threshold, raise reasoning effort and redirect to root-cause
		// reasoning. Fires once per task so it can't over-escalate.
		if !editThrashHandled {
			for path, n := range reEdits {
				if n >= maxReEdits {
					editThrashHandled = true
					escalate()
					a.messages = append(a.messages, llm.Message{Role: "user", Content: editThrashNudge})
					a.dbgEmit(fmt.Sprintf("re-edit thrash on %s (%d edits) → escalated + root-cause nudge", filepath.Base(path), n))
					break
				}
			}
		}
		// Under guided/required tool-calling the model can't reply with plain text,
		// so it ends by calling the `finish` tool. Surface its summary as the final
		// answer and stop. (In auto mode the model finishes with text instead and
		// this never fires.)
		if summary, done := finishCall(reply.ToolCalls); done {
			if a.finishOrRepair(ctx, &codeDirty, &verifyRepairs) {
				escalate() // edits don't compile — give the fix more reasoning
				continue   // fix before honoring finish
			}
			if forceArtifact() {
				continue // called finish but never wrote the required file
			}
			// finish_reason "length" means the model hit its output-token cap — the
			// answer (and its finish JSON) is cut off. Show the salvaged partial plus
			// a note, so a truncated turn never renders as silent "no output".
			if reply.FinishReason == "length" {
				summary = strings.TrimRight(summary, " \n") + truncatedNote
			}
			if summary != "" {
				a.ui.Delta(summary)
				a.ui.AssistantEnd(Stats{}) // flush the summary to the transcript
			}
			return nil
		}

		// Waiting on a background command (a step that's purely bash_output, getting
		// "[running]" back) is NORMAL polling, not circling — exempt it from the
		// no-progress guard AND the escalation breaker so a long test/build run can't
		// masquerade as a stuck loop and burn reasoning. (bash_output now blocks for
		// completion, so this rarely repeats, but the exemption keeps it honest.)
		if isPollStep(reply.ToolCalls) {
			continue
		}

		// Post-verify finish brake: the change has LANDED and a verify/compile check
		// passed AFTER the last edit, yet the model keeps running read-only steps
		// (re-reading, re-grepping) instead of finishing — the n-squared re-send tail
		// that ran a done task from ~step 39 to 45. Nudge it to finish once, forcing a
		// structured tool call so it acts (finish, or a concrete edit) rather than
		// narrating. General: no language/tool/command assumptions; never fires on a
		// project with no verify signal (lastVerifyGreenStep stays -1).
		if !finishBrakeFired && everEdited && lastVerifyGreenStep >= lastEditStep &&
			readOnlyStreak >= postVerifyIdleBudget {
			finishBrakeFired = true
			forceStructured = true
			a.messages = append(a.messages, llm.Message{Role: "user", Content: finishBrakeMsg})
			a.dbgEmit(fmt.Sprintf("post-verify finish brake (%d read-only steps after a green verify) → nudged to finish", readOnlyStreak))
			continue
		}

		// No-progress guard: a step signature (same calls AND same results) that
		// recurs within the window means the model is stuck — repeating the same
		// step OR oscillating between a few. Count occurrences in the window so a
		// 2-cycle (A,B,A,B…) is caught, not just consecutive repeats. Nudge once
		// per signature, then bail rather than loop to the step cap.
		sig := stepSig(reply.ToolCalls, results)
		recentSigs = append(recentSigs, sig)
		if len(recentSigs) > noProgressWindow {
			recentSigs = recentSigs[1:]
		}
		occ := 0
		for _, s := range recentSigs {
			if s == sig {
				occ++
			}
		}
		switch {
		case occ >= noProgressStopAt:
			// Still circling. Exhaust reasoning before giving up: if borg is
			// auto-managing effort and the ladder has headroom, climb a rung and give
			// the (apparently harder) problem a FRESH window at the higher effort
			// rather than quitting. Only when reasoning is maxed (or the dev pinned
			// effort) do we stop — then tier the model if the dev opted in.
			if autoEffort && effortRung < len(effortLadder) {
				escalate()
				recentSigs = recentSigs[:0]
				nudgedSigs = map[string]bool{}
				a.dbgEmit("escalated reasoning to " + effortLadder[effortRung-1] + " (still circling)")
				a.messages = append(a.messages, llm.Message{Role: "user", Content: fmt.Sprintf(escalateRetryMsg, effortLadder[effortRung-1])})
				break // continue the task at the higher rung
			}
			// Reasoning maxed → a stronger model is the last lever (opt-in): tier and
			// retry with a fresh window rather than tiering uselessly then quitting.
			if tierAndRetry(step) {
				a.dbgEmit("tiered to " + a.escalateModel + " on a no-progress stop (fresh attempt)")
				break
			}
			struggleReasons = append(struggleReasons, "gave up after repeating the same actions with no progress (a loop the no-progress guard had to stop)")
			terminal = true
			a.ui.Delta(noProgressStopMsg)
			a.ui.AssistantEnd(Stats{})
			return nil
		case occ >= noProgressNudgeAt && !nudgedSigs[sig]:
			nudgedSigs[sig] = true
			// Stuck → climb the reasoning ladder (none→medium→high→xhigh) so a harder
			// problem gets more thinking, then nudge to change approach. Once effort
			// tops out, escalate() tiers the model instead (opt-in via escalateModel).
			escalate()
			a.messages = append(a.messages, llm.Message{Role: "user", Content: noProgressNudge})
		default:
			// Result-only loop net: a read-only step whose RESULTS recur — even with
			// DIFFERENT args (greps for different patterns all returning "(no matches)")
			// — is also no-progress, which the arg-sensitive signature above can miss.
			// Nudge once (never hard-stop on this weaker signal, to avoid a false trip
			// on benign identical output).
			if a.readOnlyStep(reply.ToolCalls) {
				rsig := "R:" + strings.Join(results, "\x00")
				recentResults = append(recentResults, rsig)
				if len(recentResults) > noProgressWindow {
					recentResults = recentResults[1:]
				}
				rocc := 0
				for _, s := range recentResults {
					if s == rsig {
						rocc++
					}
				}
				if rocc >= noProgressNudgeAt && !nudgedSigs[rsig] {
					nudgedSigs[rsig] = true
					a.messages = append(a.messages, llm.Message{Role: "user", Content: noProgressNudge})
				}
			}
		}

		// Escalation circuit breaker — the catch-all for a NON-repeating thrash the
		// cycle detector can't see (distinct calls every step — e.g. an open-ended
		// investigation that keeps grepping/reading without landing a fix; this is the
		// shape of the session that burned ~1M tokens). After escalateGiveUpSteps with
		// no forward edit, the response DEPENDS on whether the task has acted at all:
		//   - Never edited (pure search): more reasoning can't focus an unfocused
		//     search, it just prolongs the n-squared re-send. Nudge to act-or-finish
		//     WITHOUT climbing the reasoning ladder; give up if it still won't converge.
		//   - Edited then stalled (a genuinely hard fix): climb a reasoning rung — a
		//     harder problem deserves more thinking — then give up gracefully at the top.
		// This is the upper backstop, so a stuck run can't escalate or wander unbounded.
		if autoEffort && step-lastEditStep >= escalateGiveUpSteps {
			lastEditStep = step // re-arm the window at the new rung / after the nudge
			switch {
			case !everEdited:
				if !exploreNudged {
					exploreNudged = true
					forceStructured = true // act or finish — don't narrate more searching
					a.messages = append(a.messages, llm.Message{Role: "user", Content: exploreActOrFinishMsg})
				} else if tierAndRetry(step) {
					a.dbgEmit("tiered to " + a.escalateModel + " after an unproductive search (fresh attempt)")
				} else {
					struggleReasons = append(struggleReasons, "searched many steps without ever making an edit and didn't converge")
					terminal = true
					a.ui.Delta(exploreGiveUpMsg)
					a.ui.AssistantEnd(Stats{})
					return nil
				}
			case effortRung < len(effortLadder):
				escalate()
				a.messages = append(a.messages, llm.Message{Role: "user", Content: fmt.Sprintf(escalateRetryMsg, effortLadder[effortRung-1])})
			case tierAndRetry(step):
				a.dbgEmit("tiered to " + a.escalateModel + " after maxing reasoning (fresh attempt)")
			default:
				struggleReasons = append(struggleReasons, "burned many max-reasoning steps without landing a working change (circuit breaker tripped)")
				terminal = true
				a.ui.Delta(escalateGiveUpMsg)
				a.ui.AssistantEnd(Stats{})
				return nil
			}
		}
	}
	// Hit the step cap. End gracefully with what we have rather than surfacing a
	// raw error — a long task simply ran out of steps; the transcript stands.
	struggleReasons = append(struggleReasons, "ran out of steps (hit the internal step cap) before finishing")
	terminal = true
	a.ui.Delta(stepCapMsg)
	a.ui.AssistantEnd(Stats{})
	return nil
}

// stepSig is a signature of one step — the tool calls (name+args) and their
// results — so the loop can detect a model stuck repeating the identical step.
// Arguments are CANONICALIZED (canonicalArgs) so a call that differs only in
// surface form — JSON key order, or a reordered regex alternation — collapses to
// the same signature. Without this, a model re-issuing the SAME grep with the
// pattern's `a|b|c` reshuffled to `c|a|b` slips past the no-progress guard and
// thrashes (observed: 1.45M tokens on a one-line fix).
func stepSig(calls []llm.ToolCall, results []string) string {
	var b strings.Builder
	for i, tc := range calls {
		b.WriteString(tc.Function.Name)
		b.WriteByte(0)
		b.WriteString(canonicalArgs(tc.Function.Arguments))
		b.WriteByte(0)
		if i < len(results) {
			b.WriteString(results[i])
		}
		b.WriteByte(1)
	}
	return b.String()
}

// canonicalArgs normalizes a tool call's JSON arguments to a stable form so two
// semantically-identical calls hash the same: object keys are sorted (json.Marshal
// of a map sorts keys), and a `pattern` field that is a regex alternation (a|b|c)
// has its alternatives sorted. Non-object or unparseable args are returned as-is.
func canonicalArgs(raw string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return raw
	}
	if p, ok := m["pattern"].(string); ok && strings.Contains(p, "|") && !strings.ContainsAny(p, "()[]{}\\") {
		parts := strings.Split(p, "|")
		sort.Strings(parts)
		m["pattern"] = strings.Join(parts, "|")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return string(b)
}

// isPollStep reports whether a step is PURELY waiting on a background command
// (every call is bash_output). Such a step is exempt from the no-progress /
// escalation guards: getting "[running]" back repeatedly is normal waiting, not a
// stuck loop, and more reasoning can't make the command finish faster.
func isPollStep(calls []llm.ToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	for _, tc := range calls {
		if tc.Function.Name != "bash_output" {
			return false
		}
	}
	return true
}

// readOnlyStep reports whether every call in a step is a non-mutating tool — used
// by the result-only loop net: a read-only step whose RESULTS recur (even with
// different args, e.g. greps for different patterns all returning "(no matches)")
// is still no-progress.
func (a *Agent) readOnlyStep(calls []llm.ToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	for _, tc := range calls {
		t, ok := a.tools.Get(tc.Function.Name)
		if !ok || t.Mutating() {
			return false
		}
	}
	return true
}

// verifiableExts are source extensions whose edits a verify check can catch — the
// only edits worth auto-verifying (mirrors the languages detectVerify supports:
// Go, TypeScript, JavaScript, Python, PHP, Ruby). Editing docs/config (e.g.
// `borg learn` writing BORG.md, or the .txt-writing trajectory tests) never arms
// auto-verify; and VerifyApplicable is the real guard — if the toolchain isn't
// present, auto-verify is a silent no-op even for a matching extension.
var verifiableExts = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".mts": true, ".cts": true,
	".js": true, ".mjs": true, ".cjs": true, ".py": true, ".php": true, ".rb": true,
}

// isEditTool reports whether a tool name is one of the file-mutating edit tools
// (used to recognize forward progress for the escalation circuit breaker and to
// render a diff of what changed).
func isEditTool(name string) bool {
	switch name {
	case "write_file", "edit_file", "edit_lines":
		return true
	}
	return false
}

// IsEditTool reports whether name is a file-mutating edit tool. Exported so the eval
// harness can count edits-landed without duplicating the tool-name set (one source).
func IsEditTool(name string) bool { return isEditTool(name) }

// editTouchesSource reports whether a tool call edited a file the compile check
// would catch — used to decide whether to auto-verify before a turn ends.
func editTouchesSource(name, args string) bool {
	switch name {
	case "write_file", "edit_file", "edit_lines":
		return verifiableExts[strings.ToLower(filepath.Ext(argField(args, "path")))]
	}
	return false
}

// commandRunsVerify reports whether a bash tool call's command runs the project's
// declared verify command — so the model invoking it itself (e.g. `make docker-test`,
// or `cd sub && make docker-test`) dedups the auto-verify backstop instead of running
// the slow suite twice. A substring match on the full declared line is robust to a
// leading `cd …  &&` without matching a bare shared token. Language-agnostic: it keys
// on the project's OWN declaration, never a hardcoded toolchain name.
func commandRunsVerify(args, declaredVerify string) bool {
	cmd := strings.TrimSpace(argField(args, "command"))
	dv := strings.TrimSpace(declaredVerify)
	return dv != "" && (cmd == dv || strings.Contains(cmd, dv))
}

// bashSucceeded reports whether a bash tool result indicates a clean (zero-exit)
// run. The bash tool appends "\n[exit: …]" on a non-zero exit and a "[timed out
// after …]" note on a kill, and returns an "error: …"/permission-denial string when
// it never ran — none of which count as a passing verify.
func bashSucceeded(result string) bool {
	switch {
	case strings.HasPrefix(result, "error:"):
		return false
	case strings.Contains(result, "\n[exit: "):
		return false
	case strings.Contains(result, "[timed out after "):
		return false
	}
	return true
}

// writesArtifact reports whether a tool call is a write_file targeting the file
// named `artifact` (by basename) — used to confirm a required deliverable (e.g.
// /learn's BORG.md) was actually written, not just printed in the reply.
func writesArtifact(name, args, artifact string) bool {
	return artifact != "" && name == "write_file" && filepath.Base(argField(args, "path")) == artifact
}

// finishOrRepair runs the compile check itself when source was edited but not yet
// confirmed building, just before a turn would end. It returns true if the loop
// should CONTINUE — the build FAILED and the failure was fed back for the model
// to fix — and false if it's safe to finish. Bounded by maxAutoVerifyRepairs so a
// persistently-broken edit can't loop. codeDirty is cleared each call and re-armed
// only when the model edits source again.
func (a *Agent) finishOrRepair(ctx context.Context, codeDirty *bool, repairs *int) bool {
	if !*codeDirty || *repairs >= maxAutoVerifyRepairs {
		return false
	}
	out, failed := a.autoVerify(ctx)
	*codeDirty = false
	if !failed {
		return false
	}
	*repairs++
	a.messages = append(a.messages, llm.Message{Role: "user", Content: autoVerifyFailMsg + out})
	return true
}

// autoVerify checks the edited code before a turn ends and reports its output and
// whether it FAILED, rendering a normal tool line. It PREFERS the project's own
// declared verify command (BORG.md "## Verify" — the real tests, run the project's
// way) and falls back to the built-in compile-only check. A silent no-op when
// neither applies.
func (a *Agent) autoVerify(ctx context.Context) (string, bool) {
	// The project's OWN verify command runs the real tests its way (e.g. `make
	// docker-test`), so the model never needs an ad-hoc host `go test`. It executes
	// project code, so it's gated once (AllowAlways-able); a denial / non-interactive
	// pipe skips it (safe to finish).
	if cmd := a.projectVerifyCommand(); cmd != "" {
		return a.runProjectVerify(ctx, cmd)
	}
	t, ok := a.tools.Get("verify")
	if !ok || !tools.VerifyApplicable() {
		return "", false
	}
	a.ui.ToolCall("verify", "{}")
	out, err := t.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		out = "error: " + err.Error()
	}
	failed := strings.HasPrefix(out, "FAIL")
	a.ui.ToolResult("verify", !failed, firstLine(out))
	return out, failed
}

// projectVerifyCommand reads the project's declared verify command fresh from
// BORG.md each call, so a /learn that adds it mid-session takes effect at once.
func (a *Agent) projectVerifyCommand() string {
	body, err := os.ReadFile(ProjectContextFile)
	if err != nil {
		return ""
	}
	return parseVerifyCommand(string(body))
}

// verifyCmdPermitKey is the always-allow key for a project's declared verify
// command (one consent covers re-runs this session).
const verifyCmdPermitKey = "__verify_cmd__"

// runProjectVerify runs the project's declared verify command through the
// permission gate — it executes project code, so borg never silently runs a command
// from a repo file (the safe, Claude-hook-faithful bit: consented, not auto-pulled).
// A denial or a non-interactive pipe skips it and reports "not failed" so the turn
// can still finish; an allow runs it and any failure is fed back to be fixed.
func (a *Agent) runProjectVerify(ctx context.Context, cmd string) (string, bool) {
	if !a.always[verifyCmdPermitKey] {
		switch a.ui.Permit("verify: " + cmd) {
		case DenyOnce:
			return "", false
		case AllowAlways:
			a.always[verifyCmdPermitKey] = true
		case AllowOnce:
		}
	}
	a.ui.ToolCall("verify", cmd)
	out, failed := tools.RunCommand(ctx, cmd)
	if out == "" {
		out = "(no output)"
	}
	a.ui.ToolResult("verify", !failed, firstLine(out))
	return out, failed
}

// finishCall returns the summary and true if the model called the `finish` tool
// to end its turn (used by guided/required tool-calling).
func finishCall(calls []llm.ToolCall) (string, bool) {
	for _, tc := range calls {
		if tc.Function.Name == "finish" {
			return extractSummary(tc.Function.Arguments), true
		}
	}
	return "", false
}

// extractSummary pulls the `summary` out of a finish call's JSON arguments. It
// tolerates a TRUNCATED call: when the model hits its output-token cap mid-finish,
// the arguments JSON is cut off (no closing quote/brace) and a strict parse yields
// nothing — losing the whole answer. So on parse failure we salvage the partial
// summary string by hand, so the user always sees what the model produced.
func extractSummary(args string) string {
	var p struct {
		Summary string `json:"summary"`
	}
	if json.Unmarshal([]byte(args), &p) == nil {
		return p.Summary // well-formed (the common case)
	}
	// Salvage: locate the summary value and decode its (possibly unterminated) body.
	i := strings.Index(args, `"summary"`)
	if i < 0 {
		return ""
	}
	rest := args[i+len(`"summary"`):]
	if c := strings.IndexByte(rest, ':'); c >= 0 {
		rest = rest[c+1:]
	} else {
		return ""
	}
	if q := strings.IndexByte(rest, '"'); q >= 0 {
		rest = rest[q+1:]
	} else {
		return ""
	}
	return decodeJSONStringBody(rest)
}

// decodeJSONStringBody decodes the inside of a JSON string (after the opening
// quote), handling escapes and stopping at the first unescaped quote OR the end of
// input — so a truncated string still yields its decoded prefix.
func decodeJSONStringBody(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; ch {
		case '\\':
			if i+1 >= len(s) {
				return b.String() // dangling escape at the truncation point
			}
			i++
			switch e := s[i]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'u':
				if i+4 < len(s) {
					if r, err := strconv.ParseUint(s[i+1:i+5], 16, 32); err == nil {
						b.WriteRune(rune(r))
						i += 4
					}
				}
			default: // ", \, /, and anything else → literal
				b.WriteByte(e)
			}
		case '"':
			return b.String() // unescaped closing quote → end of value
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// looksLikeTextToolCall reports whether assistant content appears to be a tool
// call written as plain text (a model serialization leak) rather than prose — so
// the loop can re-prompt instead of silently ending the turn. Kept tight to avoid
// false positives on a reply that merely mentions a tool by name.
func looksLikeTextToolCall(content string, names []string) bool {
	if content == "" {
		return false
	}
	if strings.Contains(content, "<tool_call") || strings.Contains(content, "[TOOL_CALLS]") {
		return true
	}
	for _, n := range names {
		// e.g. `call:read_file{` or `{"name": "read_file"` — a structured call
		// that ended up in the text channel.
		if strings.Contains(content, "call:"+n) ||
			strings.Contains(content, `"name": "`+n+`"`) ||
			strings.Contains(content, `"name":"`+n+`"`) {
			return true
		}
	}
	return false
}

// runToolCalls executes a turn's tool calls and returns their results in order.
// When every call is a known read-only tool (read_file/list_dir/grep) it runs
// them CONCURRENTLY — independent reads, no permission, no shared mutation, so
// any order is correct and results are collected per index. This is local
// parallelism only: the model already returned all calls in one response, and
// all results go back in the next single request (no extra LLM round-trips). Any
// mutating tool in the batch forces sequential execution to preserve ordering.
func (a *Agent) runToolCalls(ctx context.Context, calls []llm.ToolCall) []string {
	results := make([]string, len(calls))
	if len(calls) > 1 && a.allReadOnly(calls) {
		a.ui.ToolBatch(len(calls)) // show that this batch runs concurrently
		var wg sync.WaitGroup
		for i := range calls {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i] = a.runTool(ctx, calls[i])
			}(i)
		}
		wg.Wait()
		return results
	}
	for i := range calls {
		results[i] = a.runTool(ctx, calls[i])
	}
	return results
}

// allReadOnly reports whether every call is a known, non-mutating tool — the
// condition for running the batch in parallel.
func (a *Agent) allReadOnly(calls []llm.ToolCall) bool {
	for _, tc := range calls {
		// ask_user is non-mutating but INTERACTIVE — it must run on the sequential
		// path so its modal isn't raced against other prompts in a parallel batch.
		if tc.Function.Name == "ask_user" {
			return false
		}
		t, ok := a.tools.Get(tc.Function.Name)
		if !ok || t.Mutating() {
			return false
		}
	}
	return true
}

// runTool executes one tool call, prompting for permission on mutating tools,
// and reports the call + outcome to the UI.
func (a *Agent) runTool(ctx context.Context, tc llm.ToolCall) string {
	t, ok := a.tools.Get(tc.Function.Name)
	if !ok {
		return "error: unknown tool " + tc.Function.Name
	}
	// ask_user is an interactive sentinel: the loop drives the UI prompt directly
	// (no tool line, no permission gate — the prompt itself is the interaction)
	// and feeds the user's choice back as the result.
	if tc.Function.Name == "ask_user" {
		return a.askUser(tc.Function.Arguments)
	}
	a.ui.ToolCall(tc.Function.Name, tc.Function.Arguments)

	if t.Mutating() && !a.always[t.Name()] {
		switch a.ui.Permit(t.Name()) {
		case DenyOnce:
			a.ui.ToolResult(t.Name(), false, "permission denied")
			return "error: the user denied permission to run this tool"
		case AllowAlways:
			a.always[t.Name()] = true
		case AllowOnce:
		}
	}

	if a.debug {
		a.dbgEmit("tool " + t.Name() + " args: " + tc.Function.Arguments)
	}
	start := time.Now()
	out, err := t.Execute(ctx, json.RawMessage(tc.Function.Arguments))
	result := out
	if err != nil {
		result = "error: " + err.Error()
	}
	// A tool result becomes a `tool` message; empty content serializes away
	// (omitempty) and some providers reject a tool message without content
	// ("Field required"). Guarantee a non-empty result for every tool.
	if result == "" {
		result = "(no output)"
	}
	a.ui.ToolResult(t.Name(), err == nil, firstLine(result))
	// A successful edit tool returns "<summary>\n<unified diff>" — surface the diff
	// as a colored preview so the user (and a TTY) SEE what changed, instead of just
	// the one-line summary. The model receives the same diff in the tool result.
	if err == nil && isEditTool(t.Name()) {
		if _, diff, ok := strings.Cut(result, "\n"); ok {
			a.ui.ToolDiff(diff)
		}
	}
	if a.debug {
		a.dbgEmit(fmt.Sprintf("tool %s done in %s:\n%s", t.Name(), time.Since(start).Round(time.Millisecond), result))
	}
	return result
}

// maxAskOptions caps how many choices an ask_user prompt presents (matches the
// tool schema's maxItems); extra options are dropped rather than rejected.
const maxAskOptions = 4

// askUser drives the ask_user tool: it parses the model's question/options, runs
// the UI prompt (which blocks until the user picks), and returns the result the
// model sees — the chosen label, or an autonomy nudge if the user dismissed it or
// the call was malformed. Every branch is actionable so the model self-recovers
// (it never just stalls waiting on a question that can't be answered).
func (a *Agent) askUser(args string) string {
	req, err := parseAskRequest(args)
	if err != nil {
		return "error: " + err.Error() + " — don't retry the question; proceed using your best judgment."
	}
	res := a.ui.AskUser(req)
	switch {
	case res.Freeform && strings.TrimSpace(res.Choice) != "":
		// The user answered in their own words instead of picking a listed option —
		// it may refine, combine, or override them, so tell the model to treat it as
		// the decision and continue (or, if it's a question, to engage with it).
		return "The user didn't pick a listed option — they responded in their own words:\n" + res.Choice +
			"\nTreat this as their direction: it may choose a blend, add a constraint, override the options, or open a discussion. Continue accordingly."
	case res.Choice != "":
		return "The user chose: " + res.Choice
	default:
		return "The user dismissed the question without choosing. Proceed using your best judgment; don't ask again unless you're still genuinely blocked."
	}
}

// parseAskRequest validates an ask_user call's arguments into an AskRequest,
// trimming blank options and capping to maxAskOptions. It errors (so askUser can
// feed an actionable message back) when the question is empty or fewer than two
// real options remain — a degenerate prompt the user couldn't meaningfully answer.
func parseAskRequest(args string) (AskRequest, error) {
	var req AskRequest
	if err := json.Unmarshal([]byte(args), &req); err != nil {
		return req, fmt.Errorf("ask_user: invalid arguments (%v)", err)
	}
	req.Question = strings.TrimSpace(req.Question)
	if req.Question == "" {
		return req, fmt.Errorf("ask_user: a non-empty question is required")
	}
	opts := req.Options[:0:0]
	for _, o := range req.Options {
		o.Label = strings.TrimSpace(o.Label)
		o.Description = strings.TrimSpace(o.Description)
		if o.Label != "" {
			opts = append(opts, o)
		}
	}
	if len(opts) < 2 {
		return req, fmt.Errorf("ask_user: at least two labelled options are required")
	}
	if len(opts) > maxAskOptions {
		opts = opts[:maxAskOptions]
	}
	req.Options = opts
	return req, nil
}

// ToolCallLine renders a tool call as a clean one-liner for display — the actual
// command/path rather than raw JSON. Falls back to a generic arg preview.
func ToolCallLine(name, args string) string {
	switch name {
	case "bash":
		if c := argField(args, "command"); c != "" {
			return "bash $ " + bashDisplay(c)
		}
	case "read_file", "write_file", "edit_file", "list_dir":
		if p := argField(args, "path"); p != "" {
			return name + " " + p
		}
	case "grep":
		if pat := argField(args, "pattern"); pat != "" {
			if p := argField(args, "path"); p != "" {
				return "grep " + pat + " in " + p
			}
			return "grep " + pat
		}
	}
	return name + " " + summarize(args)
}

// argField pulls a string field out of a tool call's JSON arguments.
func argField(args, key string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// maxBashDisplay caps the bash command shown in a tool-call line — generous, so a
// realistic multi-line script (the kind that was being clipped to its first 100
// chars) is shown in FULL, while a pathological megabyte command can't flood the
// terminal. The complete command is always in ~/.config/borg/logs regardless.
const maxBashDisplay = 4000

// bashDisplay renders a bash command for its tool-call line: the WHOLE command,
// newlines and all (finished tool lines flush to scrollback, so multi-line is
// fine), trimmed and capped at maxBashDisplay with an explicit truncation marker.
func bashDisplay(c string) string {
	c = strings.TrimRight(c, "\n")
	if len(c) > maxBashDisplay {
		return c[:maxBashDisplay] + "\n… (command truncated — full text in ~/.config/borg/logs)"
	}
	return c
}

// firstLine returns s's first non-empty line, collapsed and truncated for a
// single-line status.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			if len(t) > 100 {
				return t[:100] + "…"
			}
			return t
		}
	}
	return ""
}

// toolDefs converts the registry's definitions to the LLM wire shape.
func toolDefs(r *tools.Registry) []llm.Tool {
	var out []llm.Tool
	for _, d := range r.Definitions() {
		out = append(out, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        d.Function.Name,
				Description: d.Function.Description,
				Parameters:  d.Function.Parameters,
			},
		})
	}
	return out
}

// summarize renders a short, single-line preview of a tool call's arguments.
func summarize(args string) string {
	s := strings.Join(strings.Fields(args), " ")
	if len(s) > 100 {
		s = s[:100] + "…"
	}
	return s
}
