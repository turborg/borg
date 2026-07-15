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
	h.LLMProxyURL = os.Getenv("BORG_LLM_PROXY_URL") // "" unless explicitly exported
	h.deriveLLMProxy()
	return &h
}
