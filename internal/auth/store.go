// Package auth drives the OAuth login flows against accounts-api and persists
// the resulting tokens.
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/turborg/borg/internal/config"
)

// EnvAPIKey injects a bearer token directly, bypassing the OAuth flow and the
// credentials file. It's meant for headless surfaces — CI and the eval bot —
// where a long-lived Passport personal access token is the right credential
// instead of a rotating refresh token. When set it wins over any stored login.
//
// It is the canonical name for "the bearer to send to the active provider",
// shared with the bring-your-own providers (where the same variable carries an
// OpenAI/OpenRouter key instead) — hence the generic name.
const EnvAPIKey = config.EnvAPIKey

// EnvAccessToken is the ORIGINAL name for the same thing and remains a fully
// supported ALIAS of EnvAPIKey — it is not a legacy hack to clean up: CI
// (.github/workflows/nightly-eval.yml) feeds the eval bot's PAT through it and
// infra provisions it, so removing it would break those. EnvAPIKey wins if both
// are set.
const EnvAccessToken = config.EnvAccessToken

// credentialsFromEnv builds Credentials from EnvAPIKey (or its EnvAccessToken
// alias) when either is set, else returns nil. The token is treated as a
// non-expiring bearer: no expiry (so the silent-refresh path never tries to
// refresh it — a PAT has no refresh token to exchange) and no recorded
// environment (the caller sets BORG_API_BASE_URL).
func credentialsFromEnv() *Credentials {
	tok := strings.TrimSpace(os.Getenv(EnvAPIKey))
	if tok == "" {
		tok = strings.TrimSpace(os.Getenv(EnvAccessToken))
	}
	if tok == "" {
		return nil
	}
	return &Credentials{AccessToken: tok, TokenType: "Bearer"}
}

// Credentials is the persisted OAuth token set. The tokens are JWTs issued by
// Passport, but borg treats them as opaque strings. APIBaseURL/AppURL record the
// environment the token was obtained from, so later commands target it.
type Credentials struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	Expiry       time.Time `json:"expiry"`
	APIBaseURL   string    `json:"api_base_url,omitempty"`
	AppURL       string    `json:"app_url,omitempty"`
}

// LoadCredentials reads the stored credentials from the default location. It
// returns an error when no login exists yet.
func LoadCredentials() (*Credentials, error) {
	if c := credentialsFromEnv(); c != nil {
		return c, nil
	}
	path, err := defaultStorePath()
	if err != nil {
		return nil, err
	}
	return store{path: path}.load()
}

// Expired reports whether the access token is at or past its expiry.
func (c *Credentials) Expired() bool {
	return !c.Expiry.IsZero() && !time.Now().Before(c.Expiry)
}

// store persists credentials to ~/.config/borg/credentials.json at mode 0600.
type store struct{ path string }

func defaultStorePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "borg", "credentials.json"), nil
}

func (s store) load() (*Credentials, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	return &c, nil
}

func (s store) save(c *Credentials) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}

func (s store) clear() error {
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
