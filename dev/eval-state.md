# Harness state — current snapshot

A standing, glanceable view of where borg's agent harness stands: how the
loop works, and the latest live-eval scorecard it's measured against. Refresh
after a meaningful harness change (commands at the bottom).

_Last refreshed: 2026-06-21 · model: **chuppa** · borg: staging `a46de19`_

## How the loop works

- **Diagram:** [`agent-loop.md`](agent-loop.md) (Mermaid source, per-node notes)
- **Rendered:** [`agent-loop.svg`](agent-loop.svg) — open in any browser/image viewer

## Test tiers (all green)

1. **Unit/mechanics** — deterministic, mocked model, ≥90% coverage, every PR.
2. **Deterministic evals** — scripted trajectories + cassettes + the corpus scored
   by real `go build`/`go test` (and `gofmt -l`) oracles via a fake/cassette → 0 tokens.
3. **Live (non-deterministic) evals** — the same corpus on the real chuppa model,
   scored by the same objective oracles, checked vs the committed baseline below.

## Latest live chuppa run

**25/25 passed · 0 regressions · 624.9k input · 50% cached · avg 4.8 steps/task · ~4m26s**

Per-task baseline (the reference the regression + efficiency checks compare to —
a still-passing task that drifts >30% on steps or input tokens is flagged):

| task | pass | steps | input tokens |
|------|:----:|------:|-------------:|
| fix-across-files | ✓ | 5 | 25.6k |
| fix-bounds-panic | ✓ | 4 | 19.3k |
| fix-clamp-logic | ✓ | 5 | 24.8k |
| fix-compile-error | ✓ | 5 | 24.0k |
| fix-failing-test | ✓ | 4 | 19.4k |
| fix-json-tag | ✓ | 5 | 24.5k |
| fix-merge-sorted | ✓ | 5 | 26.0k |
| fix-nil-map-panic | ✓ | 4 | 19.3k |
| fix-off-by-one | ✓ | 4 | 19.4k |
| fix-rotate-slice | ✓ | 4 | 20.3k |
| fix-two-bugs-two-files | ✓ | 7 | 38.4k |
| grep-and-fix | ✓ | 5 | 25.7k |
| implement-from-context | ✓ | 4 | 19.9k |
| implement-interface | ✓ | 5 | 24.2k |
| implement-missing-function | ✓ | 5 | 23.9k |
| keeps-gofmt-clean-edit | ✓ | 4 | 19.1k |
| keeps-gofmt-clean-implement | ✓ | 5 | 23.8k |
| learn-project-context | ✓ | 5 | 32.6k |
| learn-stays-lean | ✓ | 7 | 50.2k |
| locate-fix-constant | ✓ | 6 | 31.2k |
| merge-feature-branch | ✓ | 5 | 23.6k |
| red-herring-fix | ✓ | 5 | 24.9k |
| refactor-signature | ✓ | 5 | 25.1k |
| search-find-value | ✓ | 3 | 13.8k |
| trace-call-chain | ✓ | 5 | 26.0k |
| **total (25)** | **25/25** | **avg 4.8** | **624.9k** |

## Refresh this snapshot

```bash
# Re-render the loop diagram to SVG:
docker run --rm -u "$(id -u):$(id -g)" -v "$PWD/dev":/data \
  minlag/mermaid-cli -i /data/agent-loop.md -o /data/agent-loop.svg

# Re-run the live eval and re-seed the baseline (needs BORG_ACCESS_TOKEN = a PAT):
BORG_EVAL=1 BORG_EVAL_MODELS=chuppa BORG_EVAL_SAVE_BASELINE=1 make eval
# then regenerate this table from internal/eval/testdata/baseline.json
```
