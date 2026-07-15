package tools

import (
	"context"
	"encoding/json"
)

// finish ------------------------------------------------------------------

// finishTool is a terminal sentinel: the model calls it to end its turn and
// deliver the final answer. It exists so that **guided / required tool-calling**
// (tool_choice=required, which forces every response to be a structured tool
// call — the reliable fix for models that leak text tool calls) still has a clean
// way to finish: the model can't reply with plain text, so it calls finish with
// its answer. In normal (auto) mode the model just replies with text and never
// needs it. Execution is a no-op; the agent loop detects the call, renders the
// summary, and stops.
type finishTool struct{}

func (finishTool) Name() string { return "finish" }
func (finishTool) Description() string {
	return "End your turn and give your final answer. Normally you finish by replying with plain text — only call finish when you cannot reply with text (it does the same thing): put your complete final answer to the user in `summary`. Do NOT call finish if you still have tool work to do."
}
func (finishTool) Mutating() bool { return false }
func (finishTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string","description":"Your complete final answer to the user."}},"required":["summary"]}`)
}

func (finishTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	return "", nil // terminal sentinel — the agent loop handles it
}
