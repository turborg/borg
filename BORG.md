# borg — per-project context for AI coding agents

## Overview

borg is a **single-binary Go CLI agent** (~18 MB static, no runtime) that reads/edits files, runs shell commands, and works tasks autonomously until done — with all model calls going through xShellz's **metered OAuth proxy** so no provider API key is ever stored on your machine. It runs as an interactive Bubble Tea REPL (inline, no alt screen — finished turns flush to real scrollback) or as a one-shot pipeable command. The agent loop is language-agnostic: it discovers each project's build/test/run commands at runtime instead of hardcoding them. The project is stewarded by xShellz, open source under Apache-2.0, and binaries are published to Cloudflare R2 (not GitHub Releases).

## Build / Test / Lint / Run / Deploy

All Docker targets mount the source read-only and use a named `borg-go-mod` volume; any `go test`/`go run` that executes dependency code MUST run in Docker so a compromised dependency cannot touch your host.

```bash
make docker-test          # race tests in Docker (source mounted ro)
make cover-gate           # tests in Docker + enforce >=90% total coverage
make docker-bin           # build in Docker, extract ./bin/borg
make docker               # build the Docker image as borg:dev
make lint                 # golangci-lint (host — static analysis only, safe)
make fmt                  # gofmt -s -w . (host)
make vet                  # go vet ./... (host)
make tidy                 # go mod tidy (host)
make build                # host build into ./bin/borg (compile-only — tolerable quick check)
make eval                 # eval suite in Docker (needs auth for live evals; cassettes run offline)
```

**Running borg itself requires an xShellz account:**

```bash
./bin/borg auth login            # browser (PKCE); --device for headless/SSH
./bin/borg                       # start the REPL
./bin/borg "fix the failing test" # one-shot
./bin/borg learn                 # study the repo and write BORG.md
```

The binary installs as `turborg` and symlinks as `borg` when that name is free (won't clobber BorgBackup). Both names are the same binary; it brands itself via `argv[0]`.

**Deployment** is fully automated: Conventional Commits squash-merged into `main` → release-please bumps the version & tag → GoReleaser cross-compiles → `wrangler r2 object put` publishes to `dl.turborg.com/<version>/` and `dl.turborg.com/latest/`.

## Verify

```
make docker-test
```

## Directory layout

| Path | Purpose |
|---|---|
| `cmd/borg/` | CLI entry points: `main.go` (root cobra command, auth wiring, flag parsing), plus subcommands (`auth.go`, `learn.go`, `session.go`, `settings.go`, `update.go`, `gendocs.go`). The thin shell; all logic lives in `internal/`. |
| `internal/agent/` | **The core**: `loop.go` (~2k lines) — the agent loop (`Agent.Ask`), the system prompt, the recovery guards (no-progress, leak, repetition, circuit breaker), and the post-task retrospective. `context.go` — context-window tracking and stats. `ui.go` — the `UI` interface the agent calls (streamed render, permission prompts, ask_user). `harness_test.go` — scripted-model trajectory tests. |
| `internal/tools/` | Every tool the agent can invoke: `fs.go` (read/write/edit_file), `exec.go` (bash/bash_output/kill_shell with footgun guards), `glob.go`, `format.go` (auto-format after edits), `verify.go` (compile/syntax check — never runs code), `ask_user.go` (sentinel), `finish.go` (sentinel), `boundary.go` (trust-root path guard). `tools.go` — the `Registry` and `Tool` interface. `tools_test.go` — exhaustive round-trip tests. |
| `internal/auth/` | OAuth flows: PKCE (browser loopback) and device-code grant (RFC 8628). Token persistence in `~/.config/borg/credentials.json` (0600). `BORG_ACCESS_TOKEN` env var bypasses OAuth for CI/bot usage (a PAT — no expiry, never refreshed). |
| `internal/config/` | Runtime config from `BORG_*` env vars (parsed via `caarlos0/env`). `settings.go` — the registry of user-tweakable knobs (`~/.config/borg/settings.json`), each mapping to a `BORG_*` env var. Load-order: defaults → settings.json (folded into env) → explicit exports win. |
| `internal/tui/` | The Bubble Tea REPL. Inline (no alt screen): finished turns `tea.Printf` to scrollback. `model.go` (~2.9k lines) — the full model/view/update. `bridge.go` — the `uiBridge` that translates agent tool calls into Bubble Tea messages. |
| `internal/llm/` | HTTP client for xShellz's metered proxy (OpenAI-compatible SSE). Repetition guard, token counting, account model catalog fetching. |
| `internal/session/` | Conversation persistence: one JSON file per session under `~/.config/borg/sessions/<id>.json` (0600). 8-hex-char IDs. Sessions store full message history + model + effort + cumulative token usage. |
| `internal/eval/` | Eval harness: task corpus (25 tasks + 2 no-op convergence tests), baseline regression tracking, cassette replay, summary reports. Each task materializes a small Go module, prompts the agent, and scores with an objective oracle (`go build`/`go test`). Deterministic (cassette) and live (real proxy, nightly) variants. |
| `internal/trust/` | Per-directory write-scope persistence (`~/.config/borg/trust.json`, 0600). Edits are confined to the trusted root; reads are unrestricted; bash is permission-gated per-call (unless `auto_approve` is on, which skips the prompt but NOT the trust boundary). |
| `internal/selfupdate/` | Checks `dl.turborg.com/latest/version.json` (throttled to once per day), fetches the correct archive by OS/arch, verifies SHA-256 from `checksums.txt`, performs atomic binary replacement. |
| `internal/account/` | Best-effort cache of plan tier + model catalog (`~/.config/borg/account.json`, 0600) so the REPL paints instantly on startup. |
| `internal/version/` | Single `var Version = "dev"` (overridden by `-ldflags` at release); `Command()` derives the brand from `argv[0]` ("turborg" or "borg"). |
| `dev/` | Architecture docs: `agent-loop.md` (Mermaid diagram + detailed per-node notes on the agent loop), `eval-state.md` (latest live-eval scorecard). |
| `.github/workflows/` | CI (`ci.yml` — lint + race tests + coverage gate), `release.yml` (release-please + GoReleaser + R2 publish), `cla.yml` (contributor licence bot), `nightly-eval.yml` (token-spending live eval on schedule). |
| `bin/` | Build output: `bin/borg` (the binary) + `bin/turborg` (symlink — same binary). |

## Conventions

- **Branching:** trunk-based. One long-lived branch, `main`. PRs target `main` and are squash-merged, so history stays linear. Work on `feat/`, `fix/`, `docs/`, `refactor/`, `chore/` branches.
- **Commit messages:** PR titles follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, `refactor:`, `chore:`, `ci:`, `test:`). The squash-merge inherits the PR title. **Do NOT** include `Co-Authored-By: Claude` or any AI co-author trailer in commits to *this* repo.
- **Testing:** Every package uses `goleak.VerifyTestMain(m)` for goroutine-leak detection. Tests run with `-race -count=1`. Total coverage across `internal/` must be ≥90% (enforced by `go-test-coverage` — `cmd/borg` and `internal/version` are excluded). The REPL is tested by driving `Update`/`View` directly, not via a live Bubble Tea program.
- **Generality:** The agent harness must never hardcode a language, toolchain, or command name. It discovers a project's build/test/run commands at runtime from its own files (BORG.md, project manifests). Harness logic may key only on *general, project-agnostic signals*.
- **Coverage gate excludes** `cmd/borg/` (entry point) and `internal/version/` (a single var) — they carry no meaningful logic to test.
- **Formatting:** `gofmt -s -w .` for Go files. The auto-format hook runs project-appropriate formatters (gofmt, prettier, ruff, black, rustfmt, pint, php-cs-fixer, rubocop -a) gated on evidence the project uses them; any other ecosystem uses the `format` override in settings.json.

## Operational gotchas & footguns

### Agent loop internals

- **The system prompt lives in Go source** — `internal/agent/loop.go` const `systemPrompt` (~35 lines), plus `flokoAddendum` for the small model. Changing the prompt changes every model's behavior, and the prompt prefix is *cache-stable* (deterministic per config). The project context file (BORG.md) is appended after the base prompt and a `"## Verify"` heading is parsed to extract the declared verify command.
- **The verify command in BORG.md is parsed by `parseVerifyCommand`**: it looks for a `## Verify` heading, then takes the first non-empty line inside a fenced code block (or the first non-heading, non-comment line without a fence). If a BORG.md declares a verify command, it is SURFACED to the model as "run THIS" and also run automatically by the loop before a code-editing turn can finish (the `maxAutoVerifyRepairs` backstop, default 2 retries).
- **All recovery guards are bounded**: leak retries (2), repetition retries (2), artifact retries (2), auto-verify repairs (2). When any bound is exhausted, the guard fires a `Struggle` with `Terminal=true` and the post-task retrospective offers to send a harness report to the team. Soft struggles (re-edit thrash, no-progress nudge resolved) offer a BORG.md note instead.
- **No-progress guard canonicalization is load-bearing**: step signatures are canonicalized (tool args sorted, alternations collapsed) so a model reading the same file with `a|b|c` in step N and `c|b|a` in step N+1 is detected as the same step. Without this, a 1.45M-token incident burned 40+ escalated steps.
- **Escalation circuit breaker**: once reasoning is at `xhigh` and 10 steps pass with no successful edit, borg gives up via `escalateGiveUpMsg`. Below `xhigh` the same stuck signal raises the effort rung instead.
- **Artifact flush margin**: when a task must write a file (e.g. `/learn` → BORG.md), the loop fires a "write NOW" nudge at 4 steps from the cap (`artifactFlushMargin`) or after 16 steps with no write (`artifactExploreBudget`), and also when context occupancy crosses 90%. This prevents over-exploration from ending without the deliverable.
- **`BORG_ACCESS_TOKEN` bypasses OAuth entirely**: used for CI/eval bots. It's a static PAT with no refresh token, never persisted, never refreshed. When set, it wins over any stored credentials. The eval workflow (nightly) uses this.
- **`BORG_ESCALATE_MODEL` is opt-in and off by default**: auto-tiering to axiom fires only when explicitly configured. The first premium-tier call after escalation starts cache-cold, so the entire accumulated context re-bills uncached at the premium rate.
- **`--think` defaults from `BORG_THINK` env var** (or `"think"` setting in settings.json); explicit `--think` on the command line overrides. Reasoning is off by default to keep Floko fast and cheap.
- **Session IDs are 8 hex chars** (4 random bytes). Short enough to type, long enough to disambiguate by prefix. Sessions are stored per-directory and can be attached by directory (`--attach` with no id) or globally (`--resume`).

### Tools

- **verify tool is compile/PARSE-only**: it never executes the project's code or tests. For Go: `go build ./...`; for TS: `tsc --noEmit`; for Python: `compileall`; for PHP: `php -l`; for JS: `node --check`; for Ruby: `ruby -c`. Anything else falls through to bash. The verify tool's schema must be `{"type":"object"}` — an empty `"properties":{}` is mangled by the PHP proxy into `[]` which breaks Floko.
- **bash tool footgun guards**: (1) `git commit -m "..."` with backticks or `$()` inside double quotes is refused — shell command substitution would execute the embedded text. (2) `pkill borg` / `pkill turborg` is refused — it kills the running agent session; the atomic install (via `install` or `mv`) is suggested instead.
- **`hermeticCmd` strips borg's own env vars from subprocesses**: every command the harness runs (builds, tests, bash tool) goes through `hermeticCmd`, which uses `config.SubprocessEnv()` — the user's real shell environment without any `BORG_*` vars injected from settings.json. This prevents borg's configuration from leaking into user tooling.
- **Format auto-detection is gated on project evidence**: gofmt is always used for `.go` files (toolchain-bundled). For everything else, the formatter is only used when evidence exists (a `node_modules/.bin/prettier` binary plus a config file, a `vendor/bin/pint` + pint.json, a `pyproject.toml` + ruff/black, etc.). The `format` setting (`BORG_FORMAT_CMD`) overrides auto-detection entirely.
- **`edit_file` strips line-number gutters**: the `gutterRE` regex strips `N\t` prefixes when the model copies a numbered line from `read_file` into `old_string`. This means numbered text IS accepted as matching input — the model doesn't need to strip the gutter manually.
- **`read_file` is byte-capped at 96 KiB** and warns when truncated: `"[… of Z lines; truncated — call read_file with offset/limit]"`. The model is expected to use targeted ranges.
- **`glob` skips `.git`, `node_modules`, `vendor`** during directory traversal and caps at 200 results. It returns paths sorted most-recently-modified first.
- **`bash` foreground timeout is 120s**; `projectVerifyTimeout` is 5 minutes. Background shells (`run_in_background`) have no timeout — the model must poll with `bash_output` and clean up with `kill_shell`.

### Testing & eval

- **`make docker-test` mounts source read-only** and uses `borg-go-mod` named volume (Docker creates it automatically on first run). The container image is `golang:1.26`.
- **`make cover-gate` runs `go-test-coverage`** — total coverage across `internal/` must be ≥90%. It does NOT run `-race` (the profile needs a non-race binary). The eval test (`internal/eval/`) is excluded from coverage because it runs as a separate binary.
- **The eval suite has three tiers**: (1) unit tests/mechanics — deterministic, mocked model, every PR; (2) deterministic evals — scripted trajectories + cassette replay, 0 tokens; (3) live evals — the same corpus on real models, scored by objective oracles (never model-as-judge). Only tier 3 spends tokens.
- **Eval tasks are small, stdlib-only Go modules** materialized in a temp dir. The corpus has 25 tasks. No-op tasks (where the code already works) test convergence — the model must finish fast, not explore to the cap.
- **Every test package has `goleak.VerifyTestMain(m)`** — goroutine leaks fail tests. The Bubble Tea tests drive `Update`/`View` directly (no live program) to stay goleak-clean.
- **The loop is testable via the `LLM` interface seam**: `harness_test.go` uses a `scriptedLLM` that returns exact message sequences; the eval harness uses cassette-based replay. Both exercise the full `Agent.Ask` path deterministically.

### CI & release

- **CI runs tests natively (not in Docker)** because the GitHub Actions runner is an ephemeral VM — the host-protection rationale doesn't apply. Locally, always use `make docker-test`.
- **Branch protection on `main`** requires the `lint`, `test` and `cla-check` contexts plus linear history, and blocks force-push. First-time contributors are prompted by the CLA bot to sign before a PR can merge.
- **Release uses a PAT (`RELEASE_PLEASE_TOKEN`)**, not `GITHUB_TOKEN`, so release-please's PRs trigger CI (GitHub-token-created PRs don't).
- **No manual `git tag`** — release-please drives all versioning. GoReleaser's `release.disable: true` in `.goreleaser.yml` means no GitHub Release is created by GoReleaser (release-please owns the tag); publish goes to R2 via `wrangler r2 object put`.
- **CHANGELOG.md carries no PR/commit links.** The repo's git history starts at a squashed initial commit, so links to pre-open-source SHAs and issue numbers would 404 — and worse, the issue numbers would eventually resolve to unrelated PRs as new ones land.
- **Versioned docs are published per-release** (`docs/v/<version>/reference.md` + `docs/v/<version>/CHANGELOG.md`) plus a `docs/latest/` alias and a `versions.json` manifest for the docs site's version picker.

### OAuth & credentials

- **Credentials are stored at `~/.config/borg/credentials.json` (0600)**, `~/.config/borg/sessions/` dir at 0700, individual session files at 0600, and `~/.config/borg/settings.json` and `~/.config/borg/trust.json` and `~/.config/borg/account.json` all at 0600. All under the user's config dir (`os.UserConfigDir()`).
- **Silent token refresh preserves the environment**: when an access token expires, `auth.Authenticator.Status` refreshes it via the OAuth refresh token grant. The `refresh()` method must carry forward `APIBaseURL` and `AppURL` from the original login — `credsFromToken` only copies the OAuth fields, so forgetting this would lose the environment and fall back to the prod default.
- **`borg auth login` warms the account cache** (plan tier + model catalog) so the next `borg` launch paints instantly. `borg auth logout` clears the cache.

### File system

- **`guardPath` is a guard rail, not a sandbox**: it does a lexical comparison after `filepath.Abs` and `filepath.Rel`. It does NOT resolve symlinks. It prevents accidental writes outside the trust scope but is not a security boundary.
- **`trust.json` stores per-directory scope** (`"dir"` = working directory only; `"parent"` = parent too). One-shot mode uses stored scope or cwd by default; the REPL prompts interactively.
- **The installed binary is `turborg`** (primary name); `borg` is a symlink created only when `borg` is free (won't clobber BorgBackup). The binary detects its name from `argv[0]` via `version.Command()`.

### Docker

- **Dockerfile has two stages**: `golang:1.26-alpine` builder (with mod cache) → `gcr.io/distroless/static-debian12:nonroot` runtime. The runtime image has no shell or package manager — only the CA bundle and tzdata.
- **`make docker-bin` uses `docker create` + `docker cp`** rather than `docker run` to extract the binary from the built image — no need to run the container.

---

*Written 2026-07-15 by exploring every source directory, key internal files, CI workflows, and the agent loop internals. Supersedes any prior context file: this captures the project's full architecture, operational gotchas, and conventions as of the initial open-source snapshot.*