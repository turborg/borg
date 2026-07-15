package config

import (
	"fmt"
	"os"
	"strings"
)

// The provider kinds borg can send model calls to. Every one of them speaks the
// same OpenAI-compatible chat-completions API — the kind only selects the default
// endpoint and which non-standard extras are safe to send (see llm.Capabilities).
//
// These live in config rather than in internal/llm because config is the lowest
// layer: the settings registry needs the list, and llm already imports config
// (so it re-exports them as llm.Kind* for readability at the call site).
const (
	// ProviderXShellz is the hosted, metered xShellz proxy — the default. It is the
	// only backend with an account behind it (OAuth login, plan tiers, a credit
	// budget), and the only one where no provider API key touches the machine.
	ProviderXShellz = "xshellz"
	// ProviderOllama is a local Ollama daemon (also the right pick for any
	// OpenAI-compatible local server with a conservative extension surface).
	ProviderOllama = "ollama"
	// ProviderOpenAI is api.openai.com (or an Azure/compatible deployment via BaseURL).
	ProviderOpenAI = "openai"
	// ProviderOpenRouter is openrouter.ai's OpenAI-compatible gateway.
	ProviderOpenRouter = "openrouter"
	// ProviderCustom is any other OpenAI-compatible endpoint (LM Studio, llama.cpp,
	// vLLM, a self-hosted gateway). It requires an explicit BaseURL and assumes the
	// smallest possible extension surface.
	ProviderCustom = "custom"
)

// Providers lists every valid BORG_PROVIDER value, in the order the /settings
// picker cycles them.
var Providers = []string{ProviderXShellz, ProviderOllama, ProviderOpenAI, ProviderOpenRouter, ProviderCustom}

// The xShellz model codenames. They are stable public names for models the proxy
// resolves to real weights, so they mean something ONLY to the hosted backend —
// nobody else's server has heard of "chuppa".
//
// This is the single source for the list. It lives here because config is the
// lowest layer: the agent's context-window table keys off these, and the agent
// imports config (never the reverse), so anywhere else would be a cycle.
const (
	CodenameFloko       = "floko"
	CodenameChuppa      = "chuppa"
	CodenameChuppaFlash = "chuppa_flash"
	CodenameChuppaPro   = "chuppa_pro"
	CodenameAxiom       = "axiom"
)

// Codenames lists every xShellz codename, for callers that must tell a codename
// apart from a real model id.
var Codenames = []string{CodenameFloko, CodenameChuppa, CodenameChuppaFlash, CodenameChuppaPro, CodenameAxiom}

// IsCodename reports whether model is an xShellz codename rather than a model id
// a backend could actually serve.
func IsCodename(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, c := range Codenames {
		if m == c {
			return true
		}
	}
	return false
}

// ValidProvider reports whether name is a known provider kind.
func ValidProvider(name string) bool {
	for _, p := range Providers {
		if p == name {
			return true
		}
	}
	return false
}

// DefaultBaseURL is the OpenAI-compatible API root assumed for a provider kind
// when BORG_BASE_URL isn't set. It returns "" for kinds that have no sensible
// default (xshellz derives its endpoint from the login; custom must be told).
func DefaultBaseURL(provider string) string {
	switch provider {
	case ProviderOllama:
		return "http://localhost:11434/v1"
	case ProviderOpenAI:
		return "https://api.openai.com/v1"
	case ProviderOpenRouter:
		return "https://openrouter.ai/api/v1"
	}
	return ""
}

// normalizeProvider lower-cases/trims the configured provider and rejects an
// unknown one. An unknown value must NOT silently fall back to the default: that
// would send a conversation the user meant for their own backend to the hosted
// one instead. Fail loudly.
func (c *Config) normalizeProvider() error {
	c.Provider = strings.ToLower(strings.TrimSpace(c.Provider))
	if c.Provider == "" {
		c.Provider = ProviderXShellz
	}
	if !ValidProvider(c.Provider) {
		return fmt.Errorf("unknown BORG_PROVIDER %q — expected one of: %s", c.Provider, strings.Join(Providers, ", "))
	}
	return nil
}

// BringYourOwn reports whether the active provider is a user-supplied backend
// rather than the hosted xShellz proxy.
func (c *Config) BringYourOwn() bool { return c.Provider != "" && c.Provider != ProviderXShellz }

// validateModel rejects an xShellz codename on a backend that has never heard of
// one. BORG_MODEL defaults to a codename, so without this the FIRST run of the
// people this feature exists for — `BORG_PROVIDER=ollama`, nothing else set —
// posts {"model":"chuppa"} at their daemon and gets back `model 'chuppa' not
// found`: a 404 naming a hosted catalog entry in the middle of a fully local
// session, with nothing to tell them the fix is BORG_MODEL. Catching it in config
// turns that into one sentence, before a request is ever built.
//
// It fires on any codename, not just the default, so a name copy-pasted out of
// the hosted docs is caught the same way.
func (c *Config) validateModel() error {
	if !c.BringYourOwn() || !IsCodename(c.Model) {
		return nil
	}
	hint := "e.g. BORG_MODEL=gpt-4o"
	if c.Provider == ProviderOllama {
		hint = "e.g. BORG_MODEL=qwen2.5-coder:7b — `ollama list` shows what you have"
	}
	return fmt.Errorf("BORG_MODEL=%q is an xShellz model codename and means nothing to your %s backend — set it to a model %s actually serves (%s)", c.Model, c.Provider, c.Provider, hint)
}

// Hosted returns a copy of c pinned to the xShellz backend, with the metered-proxy
// endpoint re-derived (still honoring an explicit BORG_LLM_PROXY_URL).
//
// A few operations are about the xShellz ACCOUNT rather than about whichever model
// backend is configured — `auth login` and the plan/catalog cache it warms — and
// must target the platform even on a machine whose default provider is local.
// Without this, logging in while BORG_PROVIDER=ollama would "warm" the account
// cache by asking localhost for the plan and the catalog, and store the answer as
// if it came from xShellz.
func (c *Config) Hosted() *Config {
	h := *c
	if !h.BringYourOwn() {
		return &h
	}
	h.Provider = ProviderXShellz
	h.BaseURL = ""
	// Drop the key with the endpoint it belonged to. APIKey here is the user's
	// OpenAI/OpenRouter credential, scoped to THEIR vendor; a config aimed at the
	// platform must not carry it, or a caller that reasonably reads cfg.APIKey as
	// "the bearer" would hand someone else's secret to xShellz. Callers that need
	// to authenticate to the platform pass the account's own token explicitly.
	h.APIKey = ""
	h.APIKeyEnv = ""
	h.LLMProxyURL = os.Getenv("BORG_LLM_PROXY_URL") // "" unless explicitly exported
	h.deriveLLMProxy()
	return &h
}
