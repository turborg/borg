# Contributing to borg

Thanks for your interest in borg. The maintainers are happy to review high-quality patches — bug fixes with tests, agent-harness improvements, new tools, and documentation.

## Code of Conduct

This project follows the [Contributor Covenant 2.1](CODE_OF_CONDUCT.md). By participating, you agree to abide by it.

## Contributor License Agreement (CLA)

borg uses a CLA so the maintainers can keep the option of relicensing future versions if a strategic need arises (see [LICENSE](LICENSE) for current terms — Apache 2.0). Already-released versions stay under their published license forever; the CLA only affects what license future versions can ship under.

The first time you open a PR, the [cla-assistant](https://github.com/contributor-assistant/github-action) bot will ask you to comment

> I have read the CLA Document and I hereby sign the CLA

The full text is at [CLA.md](CLA.md). One signature per contributor — subsequent PRs don't re-prompt.

## Development setup

You need Go 1.26 or later (`go version`) and Docker.

```bash
git clone https://github.com/turborg/borg.git
cd borg
go mod download
```

### Build and test in Docker, not on the host

borg pulls third-party Go modules, and `go test`/`go run` **execute** that code. Running them
directly means a compromised dependency runs in your user account. Keep that execution in a
container; the host should only ever run the final, vetted binary.

| Want to… | Use | Why it's safe |
|----------|-----|---------------|
| Run the test suite | `make docker-test` | source mounted read-only, modules in a named volume, race tests run in a `golang` image — dependency code never touches the host |
| Enforce the coverage gate | `make cover-gate` | tests in Docker + `>=90%` total coverage (`.testcoverage.yml`) |
| Get a host-runnable binary | `make docker-bin` | builds in Docker, then extracts `./bin/borg` |

`go build` and `go vet` are compile-only (they don't execute dependency code), so they're a
tolerable quick host check:

```bash
make lint   # golangci-lint run ./...
make fmt    # gofmt -s -w .
make vet    # go vet ./...
make tidy   # go mod tidy
```

`make build` and `make test` exist for parity but are the **host** path — prefer the `docker-*`
targets.

### Running borg itself

Building borg needs nothing but Go. *Running* the agent needs an xShellz account, because borg has
no local provider key by design — it authenticates over OAuth and calls a metered proxy:

```bash
./bin/borg auth login    # browser (PKCE), or --device on a headless box
./bin/borg               # start the REPL in the current directory
```

The test suite does **not** require an account or network: model interactions are mocked, replayed
from recorded cassettes, or driven by scripted stubs.

## Branching strategy

borg has **two long-lived branches**:

- **`staging`** — the integration branch. **Open your PR against `staging`.**
- **`main`** — the release branch. Only ever updated by promoting `staging`; release-please cuts
  versions from it.

Work on short-lived branches: `feat/<topic>`, `fix/<topic>`, `docs/<topic>`, `refactor/<topic>`,
`chore/<topic>`. Feature PRs are **squash-merged** into `staging`. The `staging → main` promotion
uses a merge commit so the release keeps its history.

## Commit messages

PR titles must follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat(tools): add glob + background bash (bash_output / kill_shell)
fix(agent): recover from text-leaked tool calls
docs(readme): clarify env var precedence
refactor(llm): extract retry logic into shared helper
chore(deps): bump bubbletea to v1.4
```

The squash commit inherits the PR title — it becomes the project's history. Make it count.

**Do not include `Co-Authored-By: Claude` (or any AI co-author trailer) in commit messages, and do
not add "Generated with …" footers to PR bodies.** Every commit is attributed to a human
contributor.

> Note: borg *itself* adds a `Co-Authored-By: Turborg <noreply@turborg.com>` trailer to commits it
> is asked to make on a user's behalf. That's the agent disclosing its own authorship in someone
> else's repository, which is the opposite case from this rule — contributions to *this* repo are
> yours.

## Releases

We use [release-please](https://github.com/googleapis/release-please): Conventional Commits landing
on `main` drive automated `CHANGELOG.md` updates, the version bump, and the tag. There is **no
manual `git tag`**. [GoReleaser](https://goreleaser.com/) then cross-compiles the binaries for
linux/darwin/windows × amd64/arm64 and publishes them to the CDN behind `dl.turborg.com`.

You don't need to update `CHANGELOG.md` or the version constant manually — release-please does it.

## Pull request checklist

Before opening a PR:

- [ ] PR targets `staging` (not `main`)
- [ ] PR title follows Conventional Commits
- [ ] Tests added or updated to cover the change
- [ ] Coverage stays at or above 90% (`make cover-gate`)
- [ ] `make lint` and `make docker-test` are green locally
- [ ] Documentation updated if user-facing behavior changed
- [ ] CLA signed (the cla-assistant bot will prompt you)

## Testing

borg uses Go's stdlib `testing` package and [goleak](https://github.com/uber-go/goleak) for
goroutine-leak detection — every test package has `goleak.VerifyTestMain(m)` in its `TestMain`.
Tests run with `-race -count=1`. Coverage is enforced in total via
[`go-test-coverage`](https://github.com/vladopajic/go-test-coverage) against `.testcoverage.yml`.

Tests live alongside their packages. The Bubble Tea REPL is tested by driving `Update`/`View`
directly rather than running a live program, which keeps the suite goleak-clean and headless.

Run a subset (in Docker for anything that executes dependency code):

```bash
make docker-test                             # everything
go test -race ./internal/agent/...           # just the agent loop (host; compile+run — see above)
go test -race -run TestToolCallLine ./...    # one test by name
```

## The agent harness is general — any language, any repo

borg is written in Go, but that is **incidental**. The harness — the agent loop, the tools, the
system prompt, the recovery backstops — must behave just as well in a Python, Rust, TypeScript,
PHP, or polyglot repository. So, for any change to the harness or its prompts:

- **Never bake a language, toolchain, framework, or command name into the harness.** No hardcoded
  `go test`/`npm`/`cargo`, no file extensions used as control flow. The agent *discovers* each
  project's build/test/run commands from that project's own files at runtime.
- **No overfit prompts.** Prompt text that solves exactly one task, command, or language is a
  regression in generality even if it makes a test pass.
- Harness logic may key only on **general, project-agnostic signals** ("a verify command was
  declared", "an edit touched a file the compile check covers", "a command exited non-zero").

When in doubt: would this change still be correct in a repo whose language you've never seen?

## Reporting issues

- **Bug**: open a [bug report](https://github.com/turborg/borg/issues/new?template=bug_report.yml). Include `turborg --version`, `go version`, OS, and a minimal repro.
- **Feature**: open a [feature request](https://github.com/turborg/borg/issues/new?template=feature_request.yml).
- **Security vulnerability**: see [SECURITY.md](SECURITY.md) — do **not** open a public issue.

## Questions

For usage questions or design discussions, please use
[GitHub Discussions](https://github.com/turborg/borg/discussions). The issue tracker is for bugs
and concrete proposals only.

---

Thank you for contributing.
