<h1 align="center">borg</h1>

<p align="center">
  <strong>A small, fast AI coding agent for your terminal — with no provider API key on your machine.</strong>
</p>

<p align="center">
  <a href="https://github.com/turborg/borg/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/turborg/borg/actions/workflows/ci.yml/badge.svg"></a>
  <a href="LICENSE"><img alt="License: Apache-2.0" src="https://img.shields.io/badge/license-Apache--2.0-blue.svg"></a>
  <a href="https://turborg.com"><img alt="turborg.com" src="https://img.shields.io/badge/docs-turborg.com-0aa"></a>
</p>

---

borg reads and edits your files, runs your commands, and works a task until it's done and verified —
the way you'd expect a terminal coding agent to. The difference is where the model lives: borg logs
in to [xShellz](https://www.xshellz.com) over OAuth and runs every model call through a **metered
proxy**, so **no provider API key is ever stored on your laptop**, and usage is billed against your
plan instead of a raw token bucket you have to manage.

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

```bash
turborg auth login          # opens your browser; use --device on a headless box or over SSH
turborg                     # start the REPL in the current directory
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

- **No API key on disk.** OAuth (PKCE loopback, or the RFC 8628 device grant for headless hosts).
  The only credential stored is an xShellz token pair in `~/.config/borg/credentials.json` (`0600`),
  revocable from your account's devices page.
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

borg exposes models under stable codenames, so the underlying open-weights model can improve without
breaking your workflow:

| Codename | Role | Context | Plans |
|----------|------|---------|-------|
| **Chuppa Flash** | Everyday coding — the default | 1M | All |
| **Chuppa Pro** | Harder, multi-step work | 1M | Starter and up |
| **Floko** | General / chat | 256k | All |
| **Axiom** | The hardest problems, deep reasoning | — | Pro and up |

All are open-weights models. Usage draws from one shared daily pool on your plan — see
[turborg.com/pricing](https://turborg.com/pricing). `/usage` shows what you've spent.

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
