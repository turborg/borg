package tools

import (
	"context"
	"encoding/json"
)

// ask_user ----------------------------------------------------------------

// askUser lets the agent pause and put a genuine decision back to the user as a
// small multiple-choice prompt (rendered as an interactive modal in the REPL, or
// a numbered list in one-shot mode). It is the structured counterpart to borg's
// autonomy-first stance: borg infers and proceeds by default, and only reaches
// for this on a real fork — where the options diverge materially and a wrong
// guess would be costly or hard to undo.
//
// Like finishTool, Execute is a no-op sentinel: the agent loop intercepts the
// call, drives the UI's AskUser prompt (which blocks until the user picks), and
// feeds the chosen option back as the tool result. It's read-only (no permission
// gate) — the prompt itself IS the user interaction.
type askUser struct{}

func (askUser) Name() string { return "ask_user" }
func (askUser) Description() string {
	return "Ask the user ONE multiple-choice question and wait for their answer. Use this whenever you would otherwise present a lettered/numbered list of options in prose (A/B/C, 'which direction?') — a genuine fork where the options diverge materially and a wrong guess would be costly or hard to undo. Stream any reasoning as normal text first, then call this with 2–4 concrete `options`, each a short `label` plus a one-line `description`. The UI also offers a built-in 'something else' choice, so the user can pick one of your options OR reply in their own words (which may refine, combine, or override them) — either way their response is returned to you to continue with. Never use it for details you can decide or discover yourself, or to confirm something obvious. Default to deciding autonomously; this is the exception, not a habit."
}
func (askUser) Mutating() bool { return false }
func (askUser) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","description":"The single, specific question to put to the user."},"options":{"type":"array","minItems":2,"maxItems":4,"description":"The 2–4 choices the user picks from.","items":{"type":"object","properties":{"label":{"type":"string","description":"A short option label (1–5 words)."},"description":{"type":"string","description":"One line on what this option means or its trade-off."}},"required":["label"]}}},"required":["question","options"]}`)
}

func (askUser) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	return "", nil // terminal sentinel — the agent loop drives the UI prompt
}
