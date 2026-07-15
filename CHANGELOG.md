# Changelog

## 0.1.0 (2026-06-25)


### Features

* /effort command + reasoning_effort levels (none..xhigh), learn=high
* `borg learn` → combined BORG.md project context (folding in existing project guides)
* add /upgrade slash command for plan upgrades
* **agent,eval:** last-lever model tiering + edits-landed/cache-floor eval signals
* **agent:** ask_user — "something else" free-text option + force the tool over prose
* **agent:** ask_user tool — agent-initiated multiple-choice prompt
* **agent:** auto-escalate reasoning effort when a turn is struggling
* **agent:** auto-verify edits before a turn ends (small-model backstop)
* **agent:** auto-verify runs the project's own test command (BORG.md "## Verify")
* **agent:** bias the harness toward autonomy + document the principle
* **agent:** climb the reasoning ladder (none→medium→high→xhigh) when stuck
* **agent:** convergence brakes to cut token waste + honest feedback errors
* **agent:** default reasoning effort to medium
* **agent:** default tool_choice=auto, escalate to required only on a leak
* **agent:** explore-then-implement prompt + 3 discovery/adaptation evals
* **agent:** make /learn reliable on big repos — batch, higher caps, artifact flush
* **agent:** much deeper `borg learn` — read all docs, compare dirs, find decoys
* **agent:** no-progress guard — stop a model stuck repeating the same call
* **agent:** nudge model to verify a query before concluding absence
* **agent:** nudge the model to batch independent tool calls (parallelism)
* **agent:** nudge the model to use glob + background bash
* **agent:** nudge token-efficient exploration (grep-first, ranged reads)
* **agent:** opt-in model tiering (chuppa-&gt;axiom) after effort tops out
* **agent:** per-model system-prompt addendum (Floko-specific guidance)
* **agent:** polish system prompt (agent best practices) + Floko direct-finish
* **agent:** push Floko to batch independent reads (parallel tool exec)
* **agent:** resumed sessions adopt the current system prompt
* **agent:** self-heal retrospective — learn to BORG.md or report harness problems
* **agent:** show parallel tool batches in the UI (⚡ N tools in parallel)
* **agent:** teach the harness to batch git and discover PRs
* **agent:** tool-using coding-agent loop
* **agent:** turborg co-author attribution on commits/PRs (on by default)
* **agent:** wire borg to the metered LLM proxy + one-shot ask
* **auth:** always print the auth URL and honor $BROWSER
* **auth:** BORG_ACCESS_TOKEN env to inject a bearer (PAT) for headless use
* **auth:** brand the PKCE callback page (xShellz logo + theme)
* **auth:** implement PKCE + device login flows with silent refresh
* **auth:** send device-flow users to the app's /device page
* **auth:** show configured endpoints in auth status
* brand as Turborg, run as turborg or borg, co-author commits as Turborg
* cache plan/catalog for instant startup, refresh in background
* **config,tui:** ~/.config/borg/env loader + show model/effort in footer
* **debug:** add debug plumbing to LLM/UI, TUI, and CLI flag
* directory trust matrix (scoped edits) + /learn REPL command
* **docs:** generate a CLI reference + publish it and the changelog to R2
* **docs:** publish per-version reference + versions.json for versioned docs
* **eval:** 60m timeout + model dropdown (nightly = chuppa only)
* **eval:** efficiency-aware baseline — flag step/token drift, not just pass/fail
* **eval:** gated Axiom measurement path (opt-in, subset-capped)
* **eval:** GofmtClean oracle + auto-format guard tasks; seed efficiency baseline
* **eval:** N-run measurement harness + parallel-batch metric; discard batching prompt
* **eval:** parallel + token-frugal evals, cache-ratio report
* **eval:** regression baseline, run-summary graphs, +4 coding tasks
* **eval:** report token totals in repeated-run aggregate; doc OpenRouter billing
* **eval:** rich deterministic report — graphs, per-task metrics, winner
* **eval:** run floko + chuppa nightly and compare results
* **eval:** two harder edge-case corpus tasks (22 total)
* **eval:** two hermetic discovery/search corpus tasks (20 total)
* guided tool-calls for gemma — finish tool + finish_reason handling
* **learn:** cover deploy/ops gotchas + verify-before-asserting (small-model tuning)
* live /usage from GET /v1/llm/usage (account-wide rolling-24h budget)
* **llm,agent:** retry transient mid-stream errors + per-session debug logs
* **llm:** bounded retry-with-backoff for transient request failures
* **llm:** send a stable prompt_cache_key derived from the system prompt
* **llm:** send X-Turborg-Version so the proxy can gate old clients
* **llm:** stream-friendly timeout + per-turn token/timing footer
* **models:** rename to chuppa/axiom + budget-burn switch hint
* parallel read-only tool execution + ranged/bounded reads
* **release:** automate versioning with release-please
* **release:** GoReleaser + R2 publish pipeline
* rename --resume to --attach, in-REPL /attach, cwd-scoped sessions
* scaffold borg authenticated coding-agent CLI
* **sessions:** /sessions picker, dir-scoped listing, titles + relative time
* **settings:** typed settings.json + /settings command
* surface prompt-cache hits + guard prefix determinism
* **tools,agent:** bash_output blocks for completion; polling never escalates
* **tools:** add glob + background bash (bash_output / kill_shell)
* **tools:** auto-format edits (project's own formatter) + finish /usage fix
* **tools:** broaden verify to all popular languages (safe compile/parse-only)
* **tools:** numbered reads + edit_lines + verify (small-model file editing)
* **tools:** read_file reports total line count
* **tools:** sharper file edits/writes for small-model reliability
* **tui:** /context usage bar, /compact, and a near-full context warning
* **tui:** /login, /logout, /status REPL commands
* **tui:** /purge (rename), /privacy, /usage + plan-correct availability
* **tui:** borderless shaded input box, ~/dir footer, paste placeholders
* **tui:** Esc dismisses the slash menu + windowed (paged) menu
* **tui:** Esc-interrupt, paste echo expansion, markdown headings, box polish
* **tui:** frame the input/working line in a rounded box
* **tui:** growing multi-line input box (textarea), pronounced shade, padding
* **tui:** inline Charm REPL + tests, enforce &gt;=90% coverage
* **tui:** interactive /effort + /attach pickers, narrow-width clipping
* **tui:** keep input live while a turn streams; queue follow-ups
* **tui:** live 'working…' indicator during silent model output
* **tui:** live elapsed timer in working line + /learn no-re-read nudge
* **tui:** live per-turn token counter on the working indicator
* **tui:** migrate to Bubble Tea v2 for a native, non-clobbering cursor
* **tui:** navigable slash menu + per-session input history
* **tui:** nudge to re-run /learn when BORG.md drifts behind HEAD (git-only, configurable)
* **tui:** refresh footer git branch on focus/submit/turn-end, not just startup
* **tui:** replay transcript on resume + live slash-command menu
* **tui:** resumable sessions + inline Bubble Tea REPL with live markdown
* **tui:** richer status footer + highlighted prompt echo
* **tui:** startup plan/version/model banner + interactive model picker
* **tui:** Visor Humanoid launch mascot with a settling blink
* **ui:** clean tool-call lines + green/red result status
* **update:** self-updater + startup version nudge
* **usage:** show credit budget instead of input/output token meters


### Bug Fixes

* **agent,tools:** make git commits shell-safe — heredoc nudge + deny -m with substitution
* **agent:** /learn back to high effort — BORG.md is a write-once, high-leverage artifact
* **agent:** /learn prompt favors depth over speed (generic, any repo)
* **agent:** /learn uses medium effort + a timed step-budget smoke check
* **agent:** arm the /learn write-guard in the REPL + kill CLI/REPL config drift
* **agent:** bound /learn exploration in the harness, not just the prompt
* **agent:** break degenerate prose loops and force the model to act
* **agent:** bulletproof the loop — cycle detection, run-on/step-cap, bash timeout
* **agent:** declared verify command always runs before finishing
* **agent:** escalate + nudge on in-turn re-edit thrash
* **agent:** guard the reasoning channel for loops too + fix lint
* **agent:** harden the loop against token-thrash, self-kill, and edit blindness
* **agent:** instruct a proportional commit-message body, not just a subject
* **agent:** recover from text-leaked tool calls; learn enriches existing BORG.md
* **agent:** reply in the user's language, answer directly
* **agent:** require the deliverable to be write_file'd before finishing
* **agent:** revert default reasoning to none (cost)
* **agent:** salvage truncated finish calls, flag output-limit cutoffs
* **agent:** verify-command guidance is language-agnostic ("run this")
* **auth:** preserve token environment on silent refresh; clearer 401
* **debug:** complete + green the debug feature
* **eval:** close live client idle conns so goleak stays green
* **llm:** decode /users/me plan as plan_code (was planCode → always "free")
* **settings:** attribution is on/off only, drop name/email from /settings
* **test:** check os.Unsetenv return (errcheck) in config test
* **tools:** grep uses extended regex (-E) so `a|b` alternation works
* **tools:** hermetic subprocess env + whitespace-tolerant edit_file
* **tools:** never emit empty tool output (defense in depth for the 422)
* **tools:** verify schema breaks Floko guided decoding (empty properties)
* **tui:** align picker columns to the widest label, not a hardcoded width
* **tui:** bound verbose debug output so it can't flood/corrupt the inline render
* **tui:** clear the tool label when the next step starts thinking
* **tui:** echo the invoked name (turborg/borg) in the REPL banner and prompts
* **tui:** extend row highlight to pickers, chevron affordance, drop /borg-purge
* **tui:** highlight selected slash-command description too
* **tui:** slash menu vanished when filtering (inline-render shrink)
* **tui:** tighter input padding, paste \r detection, catalog spacing


### Documentation

* /effort + reasoning levels, learn=high, per-model token windows
* add BORG.md — project context for AI coding agents
* add dev/PLANS/PLAN-harness-gemma4 (harness Floko to the limit)
* committed loop SVG + standing harness-state snapshot
* correct Floko model id + realistic harness positioning
* document the automated release pipeline (release-please → GoReleaser → R2)
* document the full built feature set + models/metering + POC status
* **eval:** document running the eval + regression checks locally
* how to run the live eval against the LOCAL env (host go test + PAT)
* record the slim authorize contract + large-context chain caps
* require Docker for build/test; add docker-bin extract target
* standing rule — ship to staging, never main (don't ask to promote)
* sync project docs with the per-account credit budget
