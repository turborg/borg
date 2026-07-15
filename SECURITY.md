# Security Policy

## Reporting a vulnerability

If you've found a security issue in borg, please **do not** open a public GitHub issue.

The fastest path is GitHub's private security advisory:

→ **[Report a vulnerability privately](https://github.com/turborg/borg/security/advisories/new)**

If you cannot use GitHub for any reason, email **security@xshellz.com** with the same information.

### What to include

- A clear description of the vulnerability and what an attacker could do with it
- Steps to reproduce — proof-of-concept code is highly appreciated
- Affected versions of borg (`turborg --version`)
- Any suggestions for a fix (optional)

### What happens next

| Step             | Timeline                       |
|------------------|--------------------------------|
| Acknowledgment   | Within 48 hours                |
| Initial triage   | Within 7 days                  |
| Fix released     | Within 30 days for high-severity issues; longer for low-severity |
| Public disclosure| Coordinated with the reporter once a fix is released and users have had time to upgrade |

We will credit you in the published advisory unless you prefer to remain anonymous.

## Supported versions

We provide security updates for the latest minor release.

| Version | Supported |
|---------|-----------|
| 0.1.x   | Yes       |

Once 0.2 ships, 0.1.x will receive critical security fixes for 90 days, then move to unsupported.

## Scope

In scope:

- The official source repository at `github.com/turborg/borg`
- Published release artifacts (the `turborg`/`borg` binaries served from `dl.turborg.com`)
- The install scripts at `turborg.com/install.sh` and `turborg.com/install.ps1`

Out of scope:

- The xShellz platform services themselves (accounts API, the metered LLM proxy, the web app).
  Report those to **security@xshellz.com** — they are covered by xShellz's own disclosure process,
  not this repository's.
- Issues in upstream dependencies — please report those to the upstream project, but feel free to
  CC us if borg's usage exposes the issue.

## Security model and assumptions

borg is a coding agent that reads and edits files and runs commands on the developer's machine.
Its threat model is deliberately explicit:

- **No provider API key ever lives on the machine.** borg authenticates to xShellz over OAuth
  (PKCE loopback, or the device grant on headless hosts) and all model calls go through a metered
  proxy. The only credential on disk is an xShellz access/refresh token pair in
  `~/.config/borg/credentials.json` (mode `0600`), revocable from the account's devices page.
- **Tools run locally, with the developer's own privileges.** Only inference is remote. borg is
  not a sandbox: treat it as you would treat running a script you just wrote.
- **Editing is scoped by directory trust.** On first run in a directory borg asks the developer to
  grant a trust root, persisted in `~/.config/borg/trust.json`. `write_file`/`edit_file` refuse
  paths outside that root. Reads are unrestricted, and `bash` cannot be path-scoped — so it stays
  permission-gated per call.
- **Model output is untrusted input.** The agent loop may act on text produced by an LLM that has
  read repository contents. A hostile repository can attempt prompt injection to steer the agent.
  Mutating tools are permission-gated for exactly this reason; do not run borg with `--allow-tools`
  style pre-approval against code you do not trust.
- **Commands borg spawns inherit a cleaned environment** (`config.SubprocessEnv` strips the vars
  borg injected from its own settings file), so a borg setting cannot silently change how a
  project's build or tests behave.

If you've found an issue that the model above does not cover, we want to hear about it.

---

*Part of the [**xshellz**](https://www.xshellz.com) ecosystem.*
