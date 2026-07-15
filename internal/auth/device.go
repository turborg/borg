package auth

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"golang.org/x/oauth2"
)

// loginDevice runs the RFC 8628 device-authorization flow for headless/SSH
// boxes: it requests a device + user code, prints the verification URL (opening
// it when a browser is available), and polls the token endpoint until the user
// approves in a browser on any device.
func (a *Authenticator) loginDevice(ctx context.Context) (*oauth2.Token, error) {
	da, err := a.oauth.DeviceAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("request device code: %w", err)
	}

	verifyURL := a.verificationURL(da)
	fmt.Fprintf(os.Stderr, "To authorize borg, open this on any device:\n  %s\n", verifyURL)
	if da.UserCode != "" {
		fmt.Fprintf(os.Stderr, "Code: %s\n", da.UserCode)
	}
	if browserAvailable() {
		_ = openBrowser(verifyURL)
	}
	fmt.Fprintln(os.Stderr, "Waiting for approval…")

	// DeviceAccessToken polls TokenURL at the server-advertised interval,
	// honoring authorization_pending / slow_down until approval or expiry.
	tok, err := a.oauth.DeviceAccessToken(ctx, da)
	if err != nil {
		return nil, fmt.Errorf("device authorization: %w", err)
	}
	return tok, nil
}

// verificationURL prefers the xShellz web app's branded /device approval page
// over the OAuth server's default verification URL, so users land on the app UI.
// Falls back to the server-advertised URL when no app URL is configured.
func (a *Authenticator) verificationURL(da *oauth2.DeviceAuthResponse) string {
	if a.cfg.AppURL != "" && da.UserCode != "" {
		return strings.TrimRight(a.cfg.AppURL, "/") + "/device?user_code=" + url.QueryEscape(da.UserCode)
	}
	if da.VerificationURIComplete != "" {
		return da.VerificationURIComplete
	}
	return da.VerificationURI
}
