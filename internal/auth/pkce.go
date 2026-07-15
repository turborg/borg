package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"golang.org/x/oauth2"
)

// pkceTimeout bounds how long borg waits for the user to approve in the browser.
const pkceTimeout = 5 * time.Minute

// loginPKCE runs Authorization Code + PKCE with a loopback redirect — the same
// flow Claude Code uses for `/login`. It binds a throwaway 127.0.0.1 listener,
// opens the browser to the consent page, captures the redirected code on the
// loopback server, and exchanges it (with the PKCE verifier) for tokens.
func (a *Authenticator) loginPKCE(ctx context.Context) (*oauth2.Token, error) {
	ctx, cancel := context.WithTimeout(ctx, pkceTimeout)
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind loopback listener: %w", err)
	}

	oc := *a.oauth // copy so the per-attempt RedirectURL doesn't leak
	oc.RedirectURL = "http://" + ln.Addr().String() + "/callback"

	verifier := oauth2.GenerateVerifier()
	state, err := randomState()
	if err != nil {
		return nil, err
	}
	authURL := oc.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(verifier),
	)

	type result struct {
		code string
		err  error
	}
	resCh := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Get("error") != "":
			writeBrowserPage(w, false, "Authorization failed", q.Get("error"))
			resCh <- result{err: fmt.Errorf("authorization denied: %s", q.Get("error"))}
		case q.Get("state") != state:
			writeBrowserPage(w, false, "Authorization failed", "Security check failed (state mismatch). Please try again.")
			resCh <- result{err: errors.New("state mismatch (possible CSRF)")}
		default:
			writeBrowserPage(w, true, "You're all set", "borg is now signed in to your xShellz account.")
			resCh <- result{code: q.Get("code")}
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	}()

	// Always show the URL (so it can be pasted into the right browser/profile),
	// then best-effort open the default browser — same as Claude/gh.
	fmt.Fprintf(os.Stderr, "To authorize borg, open this URL in the browser where you're signed in:\n  %s\n", authURL)
	fmt.Fprintln(os.Stderr, "(attempting to open your browser…)")
	_ = openBrowser(authURL)

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for browser approval: %w", ctx.Err())
	case res := <-resCh:
		if res.err != nil {
			return nil, res.err
		}
		tok, err := oc.Exchange(ctx, res.code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, fmt.Errorf("exchange authorization code: %w", err)
		}
		return tok, nil
	}
}

// randomState returns a 128-bit hex string for the OAuth `state` parameter.
func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}
	return hex.EncodeToString(b), nil
}
