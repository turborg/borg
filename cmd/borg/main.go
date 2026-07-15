// Command borg is an authenticated, metered AI coding-agent CLI.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/turborg/borg/internal/agent"
	"github.com/turborg/borg/internal/auth"
	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/session"
	"github.com/turborg/borg/internal/trust"
	"github.com/turborg/borg/internal/tui"
	"github.com/turborg/borg/internal/version"
)

// applyStoredTrust confines the agent's edits to the trust scope recorded for the
// working directory, or — when none is recorded yet — to the working directory
// itself (the safe default for non-interactive runs; the REPL prompts instead).
func applyStoredTrust(ag *agent.Agent) {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	scope, ok := trust.Lookup(cwd)
	if !ok {
		scope = trust.ScopeDir
	}
	ag.SetTrustRoot(trust.Root(cwd, scope))
}

func main() {
	config.LoadSettingsFile() // fold ~/.config/borg/settings.json into BORG_* (explicit exports still win)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, version.Command()+":", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var (
		think  bool
		model  string
		attach string
		resume bool // attach the most-recently-active session (any directory)
		debug  bool
	)
	root := &cobra.Command{
		Use:           version.Command() + " [question]",
		Short:         "Turborg — an authenticated, metered AI coding agent (run as `turborg` or `borg`)",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		// With a question, answer it and exit; with no args, start the loop.
		RunE: func(cmd *cobra.Command, args []string) error {
			attachSet := cmd.Flags().Changed("attach")
			return runAgent(cmd.Context(), args, model, think, attachSet, attach, resume, debug)
		},
	}
	root.Flags().BoolVar(&think, "think", config.ThinkDefault(), "enable reasoning/thinking mode (also via the 'think' setting)")
	root.Flags().StringVar(&model, "model", "", "model codename (e.g. floko, chuppa); overrides BORG_MODEL")
	root.Flags().StringVar(&attach, "attach", "", "attach to a saved session by id or prefix, or no value for this dir's latest (run 'borg sessions')")
	root.Flags().BoolVar(&resume, "resume", false, "resume the most-recently-active session (any directory)")
	root.Flags().BoolVar(&debug, "debug", config.DebugDefault(), "verbose debug diagnostics (also via the 'debug' setting / BORG_DEBUG_ENABLED)")
	root.AddCommand(newAuthCmd(), newSessionsCmd(), newPurgeCmd(), newLearnCmd(), newSettingsCmd(), newUpdateCmd(), newGenDocsCmd())
	return root
}

// newAuthedAgent loads config, targets the token's environment, requires a
// logged-in session, and returns a ready agent. Shared by the REPL/one-shot path
// and `borg install`.
func newAuthedAgent(ctx context.Context, model string, think bool) (*agent.Agent, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if model != "" {
		cfg.Model = model
	}
	// Target the environment the stored token came from (unless env-overridden).
	if creds, cerr := auth.LoadCredentials(); cerr == nil {
		cfg.ApplyEndpointFallback(creds.APIBaseURL, creds.AppURL)
	}
	a, err := auth.New(cfg)
	if err != nil {
		return nil, err
	}
	creds, err := a.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("not logged in — run `borg auth login`: %w", err)
	}
	ag := agent.New(cfg, creds)
	ag.SetThink(think)
	// No reasoning by default — keeps the cheap Floko tier fast and near-free at
	// scale (reasoning tokens are billed output). Users opt in with /effort or
	// --think; a resumed session restores its stored level.
	return ag, nil
}

// runAgent loads config, requires a logged-in session, and answers a one-shot
// question (args) or starts the interactive loop — optionally resuming a saved
// session.
func runAgent(ctx context.Context, args []string, model string, think bool, attachSet bool, attach string, resume bool, debug bool) error {
	ag, err := newAuthedAgent(ctx, model, think)
	if err != nil {
		return err
	}
	// Wire the debug flag into the running agent so the LLM emits diagnostic
	// lines into the UI's Debug sink (plain UI => stderr; TUI => scrollback dimmed).
	ag.SetDebug(debug)

	// A one-shot question runs without a persisted session (clean, pipeable).
	if len(args) > 0 {
		applyStoredTrust(ag) // non-interactive: stored scope, or cwd by default
		return ag.Ask(ctx, strings.Join(args, " "))
	}

	// Interactive REPL: attach to an existing session or start a fresh one.
	sess, err := loadOrNewSession(ag, attachSet, attach, resume)
	if err != nil {
		return err
	}
	return tui.Run(ctx, ag, sess)
}

// loadOrNewSession attaches a saved session into the agent when --resume or
// --attach is set (--resume = the most-recent session anywhere; --attach = by
// id/prefix, or — with no id — this directory's most-recent session), otherwise
// mints a fresh one for the current settings.
func loadOrNewSession(ag *agent.Agent, attachSet bool, attach string, resume bool) (*session.Session, error) {
	var (
		sess *session.Session
		err  error
	)
	switch {
	case resume:
		// --resume: the most-recently-active session across every directory.
		if sess, err = session.Latest(); err != nil {
			return nil, fmt.Errorf("%w — start one with `borg`", err)
		}
	case !attachSet:
		sess = session.New(ag.Model(), ag.Think())
		ag.SnapshotSession(sess) // capture the full settings set (incl. effort) in one place
		return sess, nil
	case attach == "":
		// --attach with no id: this directory's most-recent session.
		if sess, err = session.LatestForDir(session.CurrentDir()); err != nil {
			return nil, fmt.Errorf("%w — start one with `borg`, or attach by id (see `borg sessions`)", err)
		}
	default:
		if sess, err = session.Load(attach); err != nil {
			return nil, err
		}
	}
	ag.RestoreSession(sess)
	fmt.Printf("Attached to session %s (last active %s)\n", sess.ID, session.HumanTime(sess.LastActive))
	return sess, nil
}
