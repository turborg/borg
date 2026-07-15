<!--
Thanks for contributing to borg!

Before opening this PR:
- [ ] PR title follows Conventional Commits (e.g. `feat(tools): add glob`)
- [ ] You've read CONTRIBUTING.md
- [ ] You've signed the CLA (the cla-assistant bot will guide you on first PR)
-->

## Summary

<!-- 1–3 sentences: what does this PR do and why? -->

## Type of change

- [ ] `feat` — new feature
- [ ] `fix` — bug fix
- [ ] `refactor` — internal change, no user-visible behavior change
- [ ] `docs` — documentation only
- [ ] `test` — tests only
- [ ] `chore` — tooling, CI, deps
- [ ] `perf` — performance improvement
- [ ] `breaking change` — incompatible change (mark in commit footer)

## Test plan

<!-- How did you verify this works? Bullet list of steps a reviewer can follow. -->

- [ ]
- [ ]

## Checklist

- [ ] Tests added or updated
- [ ] Coverage is still ≥ 90% (`make cover-gate`)
- [ ] `make lint` and `make docker-test` are green locally
- [ ] Documentation updated if user-facing behavior changed

<!--
If this touches the agent loop, tools, or prompts, please confirm:
- [ ] The change is language-agnostic — no hardcoded toolchain, framework, or command names.
      borg must behave identically in a Python/Rust/TS/PHP repo. See CONTRIBUTING.md.
-->
