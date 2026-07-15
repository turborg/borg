package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/turborg/borg/internal/config"
)

// fakeAccountsAPI stands in for accounts-api's Passport OAuth endpoints.
func fakeAccountsAPI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/oauth/device/code", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"device_code":               "dev-123",
			"user_code":                 "WDJB-MJHT",
			"verification_uri":          "http://" + r.Host + "/oauth/device",
			"verification_uri_complete": "http://" + r.Host + "/oauth/device?user_code=WDJB-MJHT",
			"expires_in":                300,
			"interval":                  1,
		})
	})

	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code_verifier") == "" {
				http.Error(w, "missing PKCE verifier", http.StatusBadRequest)
				return
			}
			if r.Form.Get("code") != "test-auth-code" {
				http.Error(w, "bad code", http.StatusBadRequest)
				return
			}
			writeToken(w, "initial-access", "initial-refresh")
		case "urn:ietf:params:oauth:grant-type:device_code":
			if r.Form.Get("device_code") == "" {
				http.Error(w, "missing device_code", http.StatusBadRequest)
				return
			}
			writeToken(w, "initial-access", "initial-refresh")
		case "refresh_token":
			if r.Form.Get("refresh_token") == "" {
				http.Error(w, "missing refresh_token", http.StatusBadRequest)
				return
			}
			writeToken(w, "refreshed-access", "refreshed-refresh")
		default:
			http.Error(w, "unsupported grant", http.StatusBadRequest)
		}
	})

	return httptest.NewServer(mux)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeToken(w http.ResponseWriter, access, refresh string) {
	writeJSON(w, map[string]any{
		"access_token":  access,
		"refresh_token": refresh,
		"token_type":    "Bearer",
		"expires_in":    3600,
	})
}

func testAuth(t *testing.T, baseURL string) *Authenticator {
	t.Helper()
	return &Authenticator{
		cfg:   &config.Config{OAuthClientID: "borg-test", APIBaseURL: baseURL},
		store: store{path: filepath.Join(t.TempDir(), "creds.json")},
		oauth: &oauth2.Config{
			ClientID: "borg-test",
			Scopes:   []string{"read", "write"},
			Endpoint: oauth2.Endpoint{
				AuthURL:       baseURL + "/oauth/authorize",
				TokenURL:      baseURL + "/oauth/token",
				DeviceAuthURL: baseURL + "/oauth/device/code",
			},
		},
	}
}

func TestLoginDeviceFlow(t *testing.T) {
	ts := fakeAccountsAPI(t)
	defer ts.Close()

	restore := openBrowser
	defer func() { openBrowser = restore }()
	openBrowser = func(string) error { return nil }

	a := testAuth(t, ts.URL)
	a.cfg.ForceDevice = true

	creds, err := a.Login(context.Background())
	require.NoError(t, err)
	require.Equal(t, "initial-access", creds.AccessToken)
	require.Equal(t, "initial-refresh", creds.RefreshToken)

	// Persisted to the store.
	got, err := a.store.load()
	require.NoError(t, err)
	require.Equal(t, "initial-access", got.AccessToken)
}

func TestLoginPKCEFlow(t *testing.T) {
	ts := fakeAccountsAPI(t)
	defer ts.Close()

	restore := openBrowser
	defer func() { openBrowser = restore }()
	// Stand in for the browser: hit the loopback callback with a code + the
	// returned state, synchronously, so the flow completes deterministically.
	openBrowser = func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		q := u.Query()
		cb, err := url.Parse(q.Get("redirect_uri"))
		if err != nil {
			return err
		}
		cbq := cb.Query()
		cbq.Set("code", "test-auth-code")
		cbq.Set("state", q.Get("state"))
		cb.RawQuery = cbq.Encode()

		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		resp, err := client.Get(cb.String())
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}

	// Force the browser branch of Login regardless of the host environment.
	t.Setenv("DISPLAY", ":0")
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")

	a := testAuth(t, ts.URL)
	creds, err := a.Login(context.Background())
	require.NoError(t, err)
	require.Equal(t, "initial-access", creds.AccessToken)

	got, err := a.store.load()
	require.NoError(t, err)
	require.Equal(t, "initial-access", got.AccessToken)
}

func TestStatusRefreshesExpiredToken(t *testing.T) {
	ts := fakeAccountsAPI(t)
	defer ts.Close()

	a := testAuth(t, ts.URL)
	require.NoError(t, a.store.save(&Credentials{
		AccessToken:  "stale-access",
		RefreshToken: "stale-refresh",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(-time.Hour),
		APIBaseURL:   "https://api.local.xshellz.com",
		AppURL:       "https://app.local.xshellz.com",
	}))

	creds, err := a.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, "refreshed-access", creds.AccessToken)
	// The environment the original login recorded survives the refresh, so later
	// commands keep targeting it instead of falling back to the prod default.
	require.Equal(t, "https://api.local.xshellz.com", creds.APIBaseURL)
	require.Equal(t, "https://app.local.xshellz.com", creds.AppURL)

	// The refreshed token (and its environment) is persisted.
	got, err := a.store.load()
	require.NoError(t, err)
	require.Equal(t, "refreshed-access", got.AccessToken)
	require.Equal(t, "https://api.local.xshellz.com", got.APIBaseURL)
	require.Equal(t, "https://app.local.xshellz.com", got.AppURL)
}

func TestVerificationURLPrefersAppURL(t *testing.T) {
	a := testAuth(t, "https://api.example")
	a.cfg.AppURL = "https://app.example/"
	got := a.verificationURL(&oauth2.DeviceAuthResponse{
		UserCode:                "WDJB-MJHT",
		VerificationURIComplete: "https://api.example/oauth/device?user_code=WDJB-MJHT",
	})
	require.Equal(t, "https://app.example/device?user_code=WDJB-MJHT", got)
}

func TestVerificationURLFallsBackToServer(t *testing.T) {
	a := testAuth(t, "https://api.example")
	a.cfg.AppURL = ""
	got := a.verificationURL(&oauth2.DeviceAuthResponse{
		UserCode:                "X",
		VerificationURIComplete: "https://api.example/oauth/device?user_code=X",
	})
	require.Equal(t, "https://api.example/oauth/device?user_code=X", got)
}

func TestStatusReturnsValidTokenWithoutRefresh(t *testing.T) {
	ts := fakeAccountsAPI(t)
	defer ts.Close()

	a := testAuth(t, ts.URL)
	require.NoError(t, a.store.save(&Credentials{
		AccessToken: "good-access",
		TokenType:   "Bearer",
		Expiry:      time.Now().Add(time.Hour),
	}))

	creds, err := a.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, "good-access", creds.AccessToken)
}
