<h1 align="center">borg</h1>

<p align="center">
  <strong>A small, fast AI coding agent for your terminal. Bring your own model, or use ours.</strong>
</p>

<p align="center">
  <a href="https://github.com/turborg/borg/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/turborg/borg/actions/workflows/ci.yml/badge.svg"></a>
  <a href="LICENSE"><img alt="License: Apache-2.0" src="https://img.shields.io/badge/license-Apache--2.0-blue.svg"></a>
  <a href="https://turborg.com"><img alt="turborg.com" src="https://img.shields.io/badge/docs-turborg.com-0aa"></a>
</p>

---

borg reads and edits your files, runs your commands, and works a task until it's done and verified —
the way you'd expect a terminal coding agent to.

It talks to **any OpenAI-compatible backend**. Point it at [Ollama](https://ollama.com), LM Studio,
llama.cpp, OpenAI, or OpenRouter and it runs against your models, your endpoint, your key — nothing
of yours reaches us. Or log in to [xShellz](https://www.xshellz.com) and use the hosted models
instead: model calls then go through a **metered proxy**, which means no provider key on your
machine and usage billed against a plan rather than a token bucket you have to manage. That's the
only difference between the two — same agent, same tools, same loop.

It's a **single static Go binary** — roughly 18 MB, no runtime, no `node_modules`, and it idles in
the low tens of MB of RAM.

## Install

```bash
curl -fsSL https://turborg.com/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://turborg.com/install.ps1 | iex
```

The binary installs as **`turborg`**. It also links **`borg`** when that name is free on your
machine — we won't clobber an existing `borg` (e.g. [BorgBackup](https://www.borgbackup.org/)). Both
names are the same binary; it brands itself after whichever name launched it.

Prefer to build it yourself? See [Development](#development).

## Quick start

### With your own model (Ollama)

Nothing to sign up for and no key. Your code, prompts and model calls stay on your machine:

```bash
ollama pull qwen2.5-coder:7b
export BORG_PROVIDER=ollama
export BORG_MODEL=qwen2.5-coder:7b
turborg                     # start the REPL in the current directory
```

Or persist it, so you don't need the exports:

```bash
turborg settings set provider ollama
turborg settings set model qwen2.5-coder:7b
```

Any other OpenAI-compatible server works the same way — give it the API root, including `/v1`:

```bash
turborg settings set provider custom
turborg settings set base_url http://localhost:1234/v1   # LM Studio, llama.cpp, vLLM, a gateway…
```

For a hosted gateway, the key comes from the environment — borg never writes one to disk:

```bash
export BORG_PROVIDER=openrouter
export BORG_API_KEY="$OPENROUTER_API_KEY"
# or: turborg settings set api_key_env OPENROUTER_API_KEY   # stores the NAME, never the key
```

### With the hosted models (xShellz)

```bash
turborg auth login          # opens your browser; use --device on a headless box or over SSH
turborg
```

On first run in a directory, borg asks whether to trust it. The root you grant **scopes the editing
tools** — borg refuses to write outside it.

```bash
turborg "fix the failing test"     # one-shot: agentic, no session, pipeable plain output
turborg learn                      # study this repo and write BORG.md (its project context file)
turborg --attach                   # continue this directory's most recent session
turborg --resume                   # continue the most recent session anywhere
turborg sessions                   # list saved sessions
```

Inside the REPL, type `/` for a live command menu:

| | |
|---|---|
| `/model` `/think` `/effort` | pick the model and how hard it reasons |
| `/learn` | (re)write `BORG.md` for this project |
| `/context` `/compact` | see context usage; compact it when it fills up |
| `/sessions` | switch between saved conversations |
| `/usage` `/settings` `/privacy` | plan usage, persistent settings, data handling |

**Esc** interrupts a running turn. Finished turns flush to real terminal scrollback, so your
history, copy-paste, and resize all behave normally.

## What makes it different

- **No backend lock-in.** The agent, the tools, and the loop are the same wherever the model runs;
  the backend is one setting. Pointed at a local model, borg needs no login and no account, and your
  code and prompts go to that model and nowhere else. (borg does check for a new release once a day,
  which sends nothing but the request — `/update` and `turborg update` do the installing.)
- **A key never lands on disk.** borg reads a provider key from the environment only
  (`BORG_API_KEY`, or `api_key_env` to name a variable you already export) — there is no code path
  that writes one to a config file. On the hosted backend there's no provider key at all: OAuth
  (PKCE loopback, or the RFC 8628 device grant for headless hosts) stores only an xShellz token
  pair in `~/.config/borg/credentials.json` (`0600`), revocable from your account's devices page.
- **Tools run locally; only inference is remote.** Reads, edits, greps, and shell commands happen on
  your machine. Mutating tools are permission-gated per call.
- **Your context is never silently compacted.** borg will not lossily summarize your conversation
  behind your back. Tool output is byte-capped and re-readable by range instead — lossless, not
  lossy. `/compact` exists, but only when *you* ask for it.
- **It closes the loop.** After an edit, borg runs your project's *own* compile/parse check and
  feeds failures back to itself until they're fixed — rather than trusting the model to self-check.
- **It's built to be general.** The harness hardcodes no language, toolchain, or command. It
  discovers how *your* project builds, tests, and runs from your project's own files.
- **Speed is a feature.** The REPL paints from local cache instantly and reconciles in the
  background; read-only tool calls run concurrently; prompt caching is automatic.

## Models

### Your own

Any model your backend serves, named exactly as it names it (`BORG_MODEL=qwen2.5-coder:14b`).

**Tool-calling is the real gate**, not size or benchmark scores. borg's loop is tools-first: it
works by calling `read_file`, `edit_file`, `bash` and friends, so a model that can't emit reliable
structured tool calls can't drive it, however well it writes code in a chat window. If a model
returns nothing at all, borg says so and names this as the likely cause rather than spinning.

| Model | Notes |
|---|---|
| **qwen2.5-coder** 7b | Works. The smallest we'd suggest; expect to keep tasks narrow. |
| **qwen2.5-coder** 14b / 32b | The sweet spot for local use — solid tool-calling, handles multi-step tasks. |
| **llama3.1** 8b+ | Works. Tool-calling is decent; weaker at long multi-file tasks. |
| **mistral-nemo** | Works. Similar profile to llama3.1 8b. |

Hosted frontier models via OpenAI/OpenRouter work too, and are the strongest option — you're just
paying that provider directly instead of us.

Two things to set for a local model:

- **`BORG_CONTEXT`** — borg can't ask a local server how big your model's window is, so it assumes a
  conservative **32k**. If yours is bigger, say so (`BORG_CONTEXT=131072`); if you don't, you only
  lose window, never correctness.
- **Reasoning knobs are off.** `/think` and `/effort` are hosted-only — those fields aren't portable
  and most local servers reject a request carrying them outright.

Expect the **first reply of a turn to be slow** on CPU. borg's system prompt plus tool schemas is a
large prompt (~18 KB), and prefill has to chew through all of it before the first token appears —
minutes, on a big model without a GPU. borg waits up to **10 minutes** for that first byte on a
backend you run (versus 2 against the hosted proxy, where a slow prefill means something's broken).
If that's still not enough, raise it with **`BORG_TTFB`** (e.g. `BORG_TTFB=20m`). It bounds only the
wait *before* the first byte — never generation, which can take as long as it takes.

### Hosted (xShellz)

Stable codenames, so the underlying model can improve without breaking your workflow. They're all
open-weights models, and there's no reason to be coy about which:

| Codename | Weights | Role | Context | Plans |
|----------|---------|------|---------|-------|
| **Chuppa Flash** | DeepSeek-V4 Flash | Everyday coding — the default | 1M | All |
| **Chuppa Pro** | DeepSeek-V4 Pro | Harder, multi-step work | 1M | Starter and up |
| **Floko** | Gemma-class (MoE) | General / chat | 256k | All |
| **Axiom** | DeepSeek-V4 | The hardest problems, deep reasoning | — | Pro and up |

Usage draws from one shared daily pool on your plan — see
[turborg.com/pricing](https://turborg.com/pricing). `/usage` shows what you've spent. (`/usage` and
the plan display are hosted-only: there's nothing for us to meter when you bring your own backend.)

## Configuration

Settings persist in `~/.config/borg/settings.json` (`0600`) and are edited with `/settings` in the
REPL or `turborg settings list|get|set` from the shell. An explicit `export` always wins over the
file, so a one-off `BORG_PROVIDER=ollama turborg …` works without changing anything saved.

| Setting | Env | Default | |
|---|---|---|---|
| `provider` | `BORG_PROVIDER` | `xshellz` | `xshellz`, `ollama`, `openai`, `openrouter`, `custom` |
| `base_url` | `BORG_BASE_URL` | per provider | OpenAI-compatible API root, **including `/v1`** |
| `model` | `BORG_MODEL` | `chuppa` | model new sessions start on |
| `context` | `BORG_CONTEXT` | auto | context window in tokens (see above) |
| `api_key_env` | `BORG_API_KEY_ENV` | — | **name** of the env var holding your key |
| — | `BORG_API_KEY` | — | the key itself — **env only, never a setting** |

`BORG_API_KEY` has no `settings.json` entry and never will: borg will not write a credential to
disk, and refuses to read one from there even if you put it in by hand. `BORG_ACCESS_TOKEN` is an
alias of it, kept for CI.

## Development

borg pulls third-party Go modules, and `go test`/`go run` **execute** that code. Keep that in a
container — the host should only run the final, vetted binary.

```bash
make docker-test   # race test suite in Docker (source mounted read-only)
make cover-gate    # tests + enforce >=90% total coverage
make docker-bin    # build in Docker, extract ./bin/borg
make lint          # golangci-lint (host; static analysis only)
```

You need Go 1.26+ and Docker. The test suite needs **no account and no network** — model
interactions are mocked, replayed from cassettes, or scripted.

Quality is tracked by an **eval suite** scored with objective oracles (does it compile? do the tests
pass?) rather than model-as-judge, with a committed baseline to catch regressions. See
[CONTRIBUTING.md](CONTRIBUTING.md).

## Contributing

PRs welcome — please read [CONTRIBUTING.md](CONTRIBUTING.md) first. Open PRs against **`main`**;
we squash-merge. First-time contributors will be prompted to sign the [CLA](CLA.md).

Found a security issue? **Don't open a public issue** — see [SECURITY.md](SECURITY.md).

## License

[Apache 2.0](LICENSE). Copyright 2026 The turborg Authors.

"turborg" and "xshellz" are trademarks of xshellz — see [NOTICE](NOTICE).

---

<p align="center">
  borg is the terminal agent of the <a href="https://turborg.com"><strong>Turborg</strong></a> project ·
  stewarded by <a href="https://www.xshellz.com"><strong>xshellz</strong></a>
</p>
