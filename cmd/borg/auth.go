package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/turborg/borg/internal/account"
	"github.com/turborg/borg/internal/auth"
	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/llm"
)

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage borg authentication",
	}
	cmd.AddCommand(newAuthLoginCmd(), newAuthLogoutCmd(), newAuthStatusCmd())
	return cmd
}

func newAuthLoginCmd() *cobra.Command {
	var device bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in via the browser (or --device for headless/SSH)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if device {
				cfg.ForceDevice = true
			}
			a, err := auth.New(cfg)
			if err != nil {
				return err
			}
			creds, err := a.Login(cmd.Context())
			if err != nil {
				return err
			}
			// Warm the account cache so the first `borg` paints plan/models
			// instantly (best-effort — login still succeeds if this fails).
			warmAccountCache(cmd.Context(), cfg, creds.AccessToken)
			fmt.Println("✓ Authorized.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&device, "device", false, "use the device-authorization flow instead of the browser")
	return cmd
}

// warmAccountCache fetches the plan tier + model catalog and stores them so the
// next REPL launch shows them instantly. Best-effort: any error is ignored.
func warmAccountCache(ctx context.Context, cfg *config.Config, token string) {
	c := llm.New(cfg, token)
	tier, errT := c.Tier(ctx)
	models, errM := c.Models(ctx)
	if errT != nil && errM != nil {
		return
	}
	_ = account.Save(&account.Info{Tier: tier, Models: models})
}

func newAuthLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the stored credentials",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			a, err := auth.New(cfg)
			if err != nil {
				return err
			}
			if err := a.Logout(); err != nil {
				return err
			}
			_ = account.Clear() // drop the cached plan/catalog too
			fmt.Println("✓ Logged out.")
			return nil
		},
	}
}

func newAuthStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the configured endpoints and login status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			// Reflect the environment the stored token belongs to (unless the
			// env vars override it), so status shows where you're actually logged in.
			if creds, cerr := auth.LoadCredentials(); cerr == nil {
				cfg.ApplyEndpointFallback(creds.APIBaseURL, creds.AppURL)
			}
			fmt.Printf("API:    %s\n", cfg.APIBaseURL)
			fmt.Printf("App:    %s\n", cfg.AppURL)
			fmt.Printf("Client: %s\n", cfg.OAuthClientID)

			a, err := auth.New(cfg)
			if err != nil {
				return err
			}
			creds, err := a.Status(cmd.Context())
			if err != nil {
				fmt.Println("Status: not logged in (run `borg auth login`)")
				return nil
			}
			fmt.Printf("Status: logged in (token %s, expires %s)\n", creds.TokenType, creds.Expiry.Format("2006-01-02 15:04 MST"))
			return nil
		},
	}
}
