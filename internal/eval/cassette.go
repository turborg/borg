// Package eval is borg's agent-eval foundation. Tier 2 lives here: record/replay
// "cassettes" — a captured sequence of a model's assistant replies for one task,
// replayed byte-for-byte through the agent.LLM seam so the real loop can be
// driven deterministically, with no network and no tokens.
//
// A cassette is bound to a fixture: replay is only valid against the same task +
// workspace it was recorded against (tool results must match), so cassettes are
// committed next to their fixtures. Capture real cassettes with a Recorder
// wrapping a live client; the recorder reads its credentials/model from the
// caller — borg never embeds a provider key.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/turborg/borg/internal/agent"
	"github.com/turborg/borg/internal/llm"
)

// Cassette is a recorded model session for a single task: the ordered assistant
// replies the model produced, one per agent step.
type Cassette struct {
	Task    string        `json:"task"`
	Model   string        `json:"model"`
	Replies []llm.Message `json:"replies"`
}

// Load reads a cassette from a JSON file.
func Load(path string) (*Cassette, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Cassette
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse cassette %s: %w", path, err)
	}
	return &c, nil
}

// Save writes the cassette to a JSON file (0644), pretty-printed for reviewable
// diffs.
func (c *Cassette) Save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Player replays a cassette as an agent.LLM: each Chat call returns the next
// recorded reply, streaming its text through onDelta exactly as the live client
// would. It does not consult the conversation it's handed — fidelity comes from
// replaying against the same fixture the cassette was recorded on.
type Player struct {
	replies []llm.Message
	i       int
}

// NewPlayer builds a Player over a cassette's replies.
func NewPlayer(c *Cassette) *Player { return &Player{replies: c.Replies} }

// Chat returns the next recorded reply, or an error if the cassette is exhausted
// (which means the loop took more steps than were recorded — a real divergence
// worth failing on).
func (p *Player) Chat(_ context.Context, _ []llm.Message, _ []llm.Tool, _ bool, onDelta func(string), _ ...llm.ChatOption) (*llm.Message, error) {
	if p.i >= len(p.replies) {
		return nil, fmt.Errorf("cassette exhausted after %d replies: the loop diverged from the recording", len(p.replies))
	}
	reply := p.replies[p.i]
	p.i++
	if reply.Content != "" && onDelta != nil {
		onDelta(reply.Content)
	}
	return &reply, nil
}

func (p *Player) Models(context.Context) ([]llm.ModelInfo, error)  { return nil, nil }
func (p *Player) Tier(context.Context) (string, error)             { return "", nil }
func (p *Player) Usage(context.Context) (*llm.AccountUsage, error) { return nil, nil }
func (p *Player) SetEffort(string)                                 {}
func (p *Player) SetModel(string)                                  {}
func (p *Player) SetDebug(func(string))                            {}

// Recorder wraps a live agent.LLM, passing every call straight through while
// appending each assistant reply to a growing Cassette. Run the agent once with
// a Recorder in place, then Save the cassette for deterministic replay later.
type Recorder struct {
	inner    agent.LLM
	Cassette Cassette
}

// NewRecorder wraps a live client, tagging the cassette with the task + model.
func NewRecorder(inner agent.LLM, task, model string) *Recorder {
	return &Recorder{inner: inner, Cassette: Cassette{Task: task, Model: model}}
}

// Chat delegates to the wrapped client and records the reply.
func (r *Recorder) Chat(ctx context.Context, msgs []llm.Message, tools []llm.Tool, think bool, onDelta func(string), opts ...llm.ChatOption) (*llm.Message, error) {
	reply, err := r.inner.Chat(ctx, msgs, tools, think, onDelta, opts...)
	if reply != nil {
		r.Cassette.Replies = append(r.Cassette.Replies, *reply)
	}
	return reply, err
}

func (r *Recorder) Models(ctx context.Context) ([]llm.ModelInfo, error) { return r.inner.Models(ctx) }
func (r *Recorder) Tier(ctx context.Context) (string, error)            { return r.inner.Tier(ctx) }
func (r *Recorder) Usage(ctx context.Context) (*llm.AccountUsage, error) {
	return r.inner.Usage(ctx)
}
func (r *Recorder) SetModel(model string)    { r.inner.SetModel(model) }
func (r *Recorder) SetEffort(effort string)  { r.inner.SetEffort(effort) }
func (r *Recorder) SetDebug(fn func(string)) { r.inner.SetDebug(fn) }

// Compile-time proof both ends satisfy the agent's model seam.
var (
	_ agent.LLM = (*Player)(nil)
	_ agent.LLM = (*Recorder)(nil)
)
