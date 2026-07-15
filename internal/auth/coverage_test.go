package auth

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/turborg/borg/internal/config"
)

func TestNew(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a, err := New(&config.Config{APIBaseURL: "https://api.x", AppURL: "https://app.x", OAuthClientID: "cid"})
	require.NoError(t, err)
	require.NotNil(t, a)
	require.Equal(t, "https://api.x/oauth/authorize", a.oauth.Endpoint.AuthURL)
}

func TestStatusNotLoggedIn(t *testing.T) {
	a := testAuth(t, "https://api.example") // empty temp store, no creds
	_, err := a.Status(context.Background())
	require.Error(t, err)
}

func TestLogoutRemovesCredentials(t *testing.T) {
	a := testAuth(t, "https://api.example")
	require.NoError(t, a.store.save(&Credentials{AccessToken: "x"}))
	require.NoError(t, a.Logout())

	_, err := a.store.load()
	require.Error(t, err) // file gone
}

func TestLoadCredentialsRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	_, err := LoadCredentials()
	require.Error(t, err) // none yet

	path, err := defaultStorePath()
	require.NoError(t, err)
	require.NoError(t, store{path: path}.save(&Credentials{
		AccessToken: "abc",
		APIBaseURL:  "https://api.local.xshellz.com",
		AppURL:      "https://app.local.xshellz.com:4200",
	}))

	creds, err := LoadCredentials()
	require.NoError(t, err)
	require.Equal(t, "abc", creds.AccessToken)
	require.Equal(t, "https://api.local.xshellz.com", creds.APIBaseURL)
}

func TestEnvAccessTokenWinsOverStore(t *testing.T) {
	// BORG_ACCESS_TOKEN (a PAT for CI/the eval bot) is used directly, bypassing
	// the credentials file and the OAuth flow.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(EnvAccessToken, "pat-12345")

	// No login on disk, yet LoadCredentials yields the env token (Bearer, no expiry).
	creds, err := LoadCredentials()
	require.NoError(t, err)
	require.Equal(t, "pat-12345", creds.AccessToken)
	require.Equal(t, "Bearer", creds.TokenType)
	require.True(t, creds.Expiry.IsZero())
	require.False(t, creds.Expired()) // zero expiry => never refreshed

	// Status returns it too, without touching the (empty) store or refreshing.
	a := testAuth(t, "https://api.example")
	got, err := a.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, "pat-12345", got.AccessToken)
}

func TestEnvAccessTokenBlankIgnored(t *testing.T) {
	// A whitespace-only env value is treated as unset, so the store still governs.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(EnvAccessToken, "   ")
	_, err := LoadCredentials()
	require.Error(t, err) // falls through to the empty store
}

func TestLoginPKCEStateMismatch(t *testing.T) {
	ts := fakeAccountsAPI(t)
	defer ts.Close()

	restore := openBrowser
	defer func() { openBrowser = restore }()
	openBrowser = func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		cb, err := url.Parse(u.Query().Get("redirect_uri"))
		if err != nil {
			return err
		}
		q := cb.Query()
		q.Set("code", "x")
		q.Set("state", "WRONG-STATE")
		cb.RawQuery = q.Encode()
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		resp, err := client.Get(cb.String())
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}

	t.Setenv("DISPLAY", ":0")
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")

	a := testAuth(t, ts.URL)
	_, err := a.Login(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "state mismatch")
}

func TestLoginPKCEExchangeError(t *testing.T) {
	ts := fakeAccountsAPI(t)
	defer ts.Close()

	restore := openBrowser
	defer func() { openBrowser = restore }()
	openBrowser = func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		cb, err := url.Parse(u.Query().Get("redirect_uri"))
		if err != nil {
			return err
		}
		q := cb.Query()
		q.Set("code", "wrong-code") // correct state, bad code -> token exchange 400
		q.Set("state", u.Query().Get("state"))
		cb.RawQuery = q.Encode()
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		resp, err := client.Get(cb.String())
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}

	t.Setenv("DISPLAY", ":0")
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")

	a := testAuth(t, ts.URL)
	_, err := a.Login(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "exchange")
}

func TestBrowserAvailable(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "1.2.3.4 5 6.7.8.9 22")
	require.False(t, browserAvailable())

	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
	t.Setenv("DISPLAY", ":0")
	require.True(t, browserAvailable())

	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	require.False(t, browserAvailable()) // linux, no display
}

func TestOpenBrowser(t *testing.T) {
	orig := openBrowser
	defer func() { openBrowser = orig }()

	t.Setenv("BROWSER", "true") // the no-op /usr/bin/true binary
	require.NoError(t, openBrowser("https://example.com"))

	// Empty BROWSER falls to the platform opener (xdg-open on linux). Scrub PATH
	// so that opener can't be resolved: Start() then fails harmlessly, exercising
	// the platform-opener branch WITHOUT launching a real browser. (Without this,
	// on a desktop with a display the test ran the real xdg-open and popped open
	// example.com in the user's browser.)
	t.Setenv("BROWSER", "")
	t.Setenv("PATH", "")
	require.Error(t, openBrowser("https://example.com"))
}
