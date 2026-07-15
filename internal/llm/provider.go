package llm

import (
	"context"
	"errors"

	"github.com/turborg/borg/internal/config"
)

// The provider kinds, re-exported from config (which owns them, being the layer
// the settings registry lives in) so call sites in and around this package read
// as llm.KindOllama rather than reaching across for a config constant.
const (
	KindXShellz    = config.ProviderXShellz
	KindOllama     = config.ProviderOllama
	KindOpenAI     = config.ProviderOpenAI
	KindOpenRouter = config.ProviderOpenRouter
	KindCustom     = config.ProviderCustom
)

// Capabilities says what a backend supports BEYOND the OpenAI-compatible chat
// API every provider speaks. It exists because borg's request carries a few
// fields that are NOT standard OpenAI — they're xShellz-proxy conventions — and a
// strict server rejects a request with unknown fields outright (400) rather than
// ignoring them. So the transport is shared by every provider and the differences
// collapse to these flags, checked at one choke point (Client.applyCaps) instead
// of being scattered through per-provider client copies.
//
// Everything false is the safe assumption for an unknown backend: send plain,
// standard OpenAI and nothing else.
type Capabilities struct {
	// RequiresAuth: the backend authenticates borg against an ACCOUNT (an OAuth
	// login), rather than an API key the user supplies. Only xShellz does; on
	// everything else borg must never mention logging in.
	RequiresAuth bool
	// AccountEndpoints: the backend serves the non-standard account routes borg
	// uses for the plan tier (/v1/users/me), the credit budget (/usage), and
	// feedback reporting. Off ⇒ borg must not issue those requests at all: a plain
	// OpenAI-compatible server has no such routes and would only 404.
	AccountEndpoints bool
	// ReasoningEffort: the backend understands the `think` and `reasoning_effort`
	// request fields. `think` is an xShellz-proxy invention; `reasoning_effort` is
	// an OpenAI-ism that most local servers still reject as an unknown field.
	ReasoningEffort bool
	// ForcedToolChoice: the backend honors an explicit tool_choice ("required") to
	// guided-decode a structured tool call. Off ⇒ borg leaves the choice to the
	// model ("auto"), losing only the leak-retry optimization.
	ForcedToolChoice bool
	// PromptCacheKey: the backend accepts the prompt_cache_key prefix-routing hint.
	PromptCacheKey bool
}

// CapabilitiesFor returns what the named provider kind supports. An empty or
// unrecognized name is treated as xShellz, matching the config default — Config
// rejects an unknown BORG_PROVIDER long before a Client is ever built.
func CapabilitiesFor(kind string) Capabilities {
	switch kind {
	case KindOllama, KindCustom:
		// A local daemon or an arbitrary compatible server: assume nothing beyond
		// the standard API. Ollama ignores tool_choice rather than honoring it, and
		// llama.cpp/LM Studio builds vary — so borg lets the model choose.
		return Capabilities{}
	case KindOpenAI, KindOpenRouter:
		// Hosted gateways: no account routes of borg's shape and no `think`, but
		// tool_choice is real and reliable. reasoning_effort is deliberately off —
		// it's valid only on some models and 400s on the rest, and borg has no way
		// to know which from a bare catalog.
		return Capabilities{ForcedToolChoice: true}
	default: // KindXShellz
		return Capabilities{
			RequiresAuth:     true,
			AccountEndpoints: true,
			ReasoningEffort:  true,
			ForcedToolChoice: true,
			PromptCacheKey:   true,
		}
	}
}

// ErrNoMetering reports that an account-hosted feature was asked of a backend
// that has none. It is returned WITHOUT making a request, so callers can say
// "that's xShellz-only" instead of surfacing a bogus 404 from someone else's
// server. Test for it with errors.Is.
var ErrNoMetering = errors.New("not available on this provider: the plan, the credit budget and feedback reporting are xShellz-hosted features")

// Provider is the backend seam: everything the agent loop and the REPL need from
// a model backend, regardless of who runs it. *Client implements it for every
// provider kind — there is deliberately ONE implementation, because the xShellz
// proxy is itself OpenAI-compatible, so streaming, SSE parsing, retries and the
// idle watchdog are identical everywhere and duplicating them per-provider would
// only let them drift. What differs is Capabilities, and that's data, not code.
type Provider interface {
	Chat(ctx context.Context, msgs []Message, tools []Tool, think bool, onDelta func(string), opts ...ChatOption) (*Message, error)
	Models(ctx context.Context) ([]ModelInfo, error)
	Tier(ctx context.Context) (string, error)
	Usage(ctx context.Context) (*AccountUsage, error)
	SetModel(model string)
	SetEffort(effort string)
	SetDebug(fn func(string))
	// RequiresAuth reports whether this backend needs an xShellz login before it
	// can be used. The CLI gates its "not logged in" check on it, so a
	// bring-your-own backend never mentions an account the user doesn't have.
	RequiresAuth() bool
	// Name is the provider kind driving this client (e.g. "ollama"), for diagnostics.
	Name() string
}

// *Client is the single Provider implementation; it also satisfies agent.LLM.
var _ Provider = (*Client)(nil)

// RequiresAuth reports whether the active backend authenticates against an
// xShellz account.
func (c *Client) RequiresAuth() bool { return c.caps.RequiresAuth }

// Name returns the active provider kind.
func (c *Client) Name() string { return c.name }

// Caps returns the active backend's capabilities.
func (c *Client) Caps() Capabilities { return c.caps }
