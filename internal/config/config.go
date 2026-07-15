// Package config loads borg's runtime settings from BORG_* environment variables.
package config

import (
	"os"
	"strconv"
	"strings"

	"github.com/caarlos0/env/v11"
)

// DebugDefault reports whether BORG_DEBUG_ENABLED is set truthy. It's the default
// for the --debug flag, so debug can be enabled via settings.json (the "debug"
// setting) or an export and still overridden by passing --debug explicitly.
func DebugDefault() bool {
	v, _ := strconv.ParseBool(os.Getenv("BORG_DEBUG_ENABLED"))
	return v
}

// ThinkDefault reports whether BORG_THINK is set truthy. It's the default for the
// --think flag, so reasoning-by-default can be set via settings.json (the "think"
// setting) or an export and still overridden by passing --think explicitly.
func ThinkDefault() bool {
	v, _ := strconv.ParseBool(os.Getenv("BORG_THINK"))
	return v
}

// LearnStaleThreshold is the commit distance at which borg nudges the user to
// re-run /learn (the "learn_stale_after" setting); 0 disables the nudge. Reads the
// effective value — an export or settings.json, else the built-in default.
func LearnStaleThreshold() int {
	s, ok := SettingByKey("learn_stale_after")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(s.Effective())
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Config holds borg's runtime settings. Defaults point at the xShellz hosted
// endpoints; every field is overridable via its BORG_* env var.
type Config struct {
	// APIBaseURL is the accounts-api base used for the OAuth flows.
	// TODO(borg): confirm the production hostname.
	APIBaseURL string `env:"BORG_API_BASE_URL" envDefault:"https://api.xshellz.com"`

	// AppURL is the xShellz web app base. The device flow sends users to its
	// /device approval page rather than the OAuth server's default page.
	AppURL string `env:"BORG_APP_URL" envDefault:"https://app.xshellz.com"`

	// InstallBase is the release host the self-updater fetches from — the same
	// host install.sh uses (dl.turborg.com/latest/...). Mirrors install.sh's
	// TURBORG_INSTALL_BASE; override for a mirror or in tests.
	InstallBase string `env:"BORG_INSTALL_BASE" envDefault:"https://dl.turborg.com"`

	// LLMProxyURL is the metered, OpenAI-compatible proxy base URL. Defaults to
	// {APIBaseURL}/v1/llm so it tracks the API host unless explicitly overridden.
	LLMProxyURL string `env:"BORG_LLM_PROXY_URL"`

	// OAuthClientID is the public "borg CLI" client seeded in accounts-api
	// (OauthPassportSeeder). Overridable per environment via BORG_OAUTH_CLIENT_ID.
	OAuthClientID string `env:"BORG_OAUTH_CLIENT_ID" envDefault:"90228cb6-3de1-472f-bb30-cd5b44101d6d"`

	// Model is the public model codename the agent requests through the proxy.
	// Chuppa is the default — a cheap, fast reasoning model available on every tier
	// (free included). Axiom is the premium tier.
	Model string `env:"BORG_MODEL" envDefault:"chuppa"`

	// EscalateModel opts into model tiering: when set (e.g. "axiom"), the agent
	// auto-escalates to it ONCE after reasoning effort has topped out and the task
	// is still visibly struggling — the model-tier analogue of effort escalation.
	// Empty (the default) disables it entirely, so there is never surprise spend on
	// the premium tier; tiering stays manual (/model) unless explicitly opted in.
	//
	// CAVEAT (why it's opt-in + sparse, not default): switching models mid-task is
	// mechanically free (borg is stateless — every step re-sends the full context),
	// but the prompt cache is keyed PER-MODEL, so the new tier starts cache-COLD —
	// the whole accumulated context re-bills UNCACHED at the premium rate. Tiering
	// fires only when a task is already struggling (big context), so that first
	// premium call is the worst case. Whether the higher solve-rate justifies that
	// cost is unproven — measure it before ever defaulting this on.
	EscalateModel string `env:"BORG_ESCALATE_MODEL"`

	// ForceDevice skips the browser/PKCE flow and uses the device flow.
	ForceDevice bool `env:"BORG_FORCE_DEVICE"`

	// GitAttribution controls whether borg adds a turborg Co-Authored-By trailer to
	// commits it creates (and a footer to PRs it opens). On by default — branding —
	// and disableable with BORG_GIT_ATTRIBUTION=0 for orgs that forbid extra trailers.
	GitAttribution bool `env:"BORG_GIT_ATTRIBUTION" envDefault:"true"`

	// GitAttributionName/Email are the co-author identity used in that trailer.
	// Overridable so the final GitHub machine-user handle/email can change without a
	// rebuild; for GitHub to LINK the co-author, the email must be verified on a real
	// user account (an org cannot be a commit co-author).
	GitAttributionName  string `env:"BORG_GIT_ATTRIBUTION_NAME" envDefault:"Turborg"`
	GitAttributionEmail string `env:"BORG_GIT_ATTRIBUTION_EMAIL" envDefault:"noreply@turborg.com"`
}

// Load parses BORG_* env vars into a Config, applying defaults. Persistent
// settings live in ~/.config/borg/settings.json (see settings.go); LoadSettingsFile
// folds them into the environment before this runs, and an explicit export wins.
func Load() (*Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return nil, err
	}
	c.deriveLLMProxy()
	return &c, nil
}

// deriveLLMProxy points the LLM proxy at {APIBaseURL}/v1/llm unless the operator
// set BORG_LLM_PROXY_URL explicitly.
func (c *Config) deriveLLMProxy() {
	if _, set := os.LookupEnv("BORG_LLM_PROXY_URL"); !set || c.LLMProxyURL == "" {
		c.LLMProxyURL = strings.TrimRight(c.APIBaseURL, "/") + "/v1/llm"
	}
}

// ApplyEndpointFallback fills the API/app endpoints from a previously stored
// login (the token's home) when the corresponding env var wasn't set — so after
// `borg auth login` against one environment, later commands target it without
// re-passing the env vars. Explicit env vars always win.
func (c *Config) ApplyEndpointFallback(apiBase, app string) {
	if _, set := os.LookupEnv("BORG_API_BASE_URL"); !set && apiBase != "" {
		c.APIBaseURL = apiBase
	}
	if _, set := os.LookupEnv("BORG_APP_URL"); !set && app != "" {
		c.AppURL = app
	}
	c.deriveLLMProxy()
}
