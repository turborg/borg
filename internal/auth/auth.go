package auth

import (
	"context"
	"fmt"

	"golang.org/x/oauth2"

	"github.com/turborg/borg/internal/config"
)

// Authenticator drives the OAuth flows against accounts-api and persists the
// resulting tokens. Login picks PKCE (browser) by default and falls back to the
// device flow when no browser is available or --device is set.
type Authenticator struct {
	cfg   *config.Config
	store store
	oauth *oauth2.Config
}

// New builds an Authenticator wired to the configured accounts-api endpoints.
func New(cfg *config.Config) (*Authenticator, error) {
	path, err := defaultStorePath()
	if err != nil {
		return nil, err
	}
	oc := &oauth2.Config{
		ClientID: cfg.OAuthClientID,
		Scopes:   []string{"read", "write"},
		Endpoint: oauth2.Endpoint{
			AuthURL:       cfg.APIBaseURL + "/oauth/authorize",
			TokenURL:      cfg.APIBaseURL + "/oauth/token",
			DeviceAuthURL: cfg.APIBaseURL + "/oauth/device/code",
		},
	}
	return &Authenticator{cfg: cfg, store: store{path: path}, oauth: oc}, nil
}

// Login runs the appropriate browser/device flow and persists the tokens.
func (a *Authenticator) Login(ctx context.Context) (*Credentials, error) {
	var (
		tok *oauth2.Token
		err error
	)
	if a.cfg.ForceDevice || !browserAvailable() {
		tok, err = a.loginDevice(ctx)
	} else {
		tok, err = a.loginPKCE(ctx)
	}
	if err != nil {
		return nil, err
	}
	creds := credsFromToken(tok)
	// Record the environment this token belongs to, so later commands target it.
	creds.APIBaseURL = a.cfg.APIBaseURL
	creds.AppURL = a.cfg.AppURL
	if err := a.store.save(creds); err != nil {
		return nil, fmt.Errorf("save credentials: %w", err)
	}
	return creds, nil
}

// Status returns the stored credentials, silently refreshing (and persisting)
// the access token via the refresh_token grant when it has expired.
func (a *Authenticator) Status(ctx context.Context) (*Credentials, error) {
	// An injected bearer (BORG_ACCESS_TOKEN, e.g. a PAT for CI/the eval bot) wins
	// over any stored login and is never refreshed — there's no refresh token.
	if c := credentialsFromEnv(); c != nil {
		return c, nil
	}
	creds, err := a.store.load()
	if err != nil {
		return nil, err
	}
	if !creds.Expired() {
		return creds, nil
	}
	tok, err := a.oauth.TokenSource(ctx, creds.token()).Token()
	if err != nil {
		return nil, fmt.Errorf("refresh access token: %w", err)
	}
	refreshed := credsFromToken(tok)
	// Carry over the environment the original login recorded — credsFromToken
	// only copies the token fields, so without this a silent refresh would wipe
	// api_base_url/app_url and later commands would fall back to the prod default.
	refreshed.APIBaseURL = creds.APIBaseURL
	refreshed.AppURL = creds.AppURL
	if err := a.store.save(refreshed); err != nil {
		return nil, fmt.Errorf("save refreshed credentials: %w", err)
	}
	return refreshed, nil
}

// Logout removes the stored credentials.
func (a *Authenticator) Logout() error { return a.store.clear() }

// credsFromToken projects an oauth2.Token into the persisted Credentials shape.
func credsFromToken(t *oauth2.Token) *Credentials {
	return &Credentials{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		TokenType:    t.TokenType,
		Expiry:       t.Expiry,
	}
}

// token rebuilds the oauth2.Token from stored Credentials (for refresh).
func (c *Credentials) token() *oauth2.Token {
	return &oauth2.Token{
		AccessToken:  c.AccessToken,
		RefreshToken: c.RefreshToken,
		TokenType:    c.TokenType,
		Expiry:       c.Expiry,
	}
}
