package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/turborg/borg/internal/session"
)

func newSessionsCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "List saved REPL sessions for this directory (--all for every directory)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			here := session.CurrentDir()
			// Default to this directory's sessions; --all lists every directory's.
			var (
				metas []session.Meta
				err   error
			)
			if all {
				metas, err = session.List()
			} else {
				metas, err = session.ListForDir(here)
			}
			if err != nil {
				return err
			}
			if len(metas) == 0 {
				if all {
					fmt.Println("No saved sessions yet.")
				} else {
					fmt.Println("No saved sessions for this directory (try `borg sessions --all`).")
				}
				return nil
			}
			for _, m := range metas {
				name := m.Name
				if name == "" {
					name = "(untitled)"
				}
				when := session.HumanTime(m.LastActive)
				if all {
					// Mark sessions started in the current directory ("borg --attach"
					// with no id picks the newest of these).
					mark := " "
					if m.Dir != "" && m.Dir == here {
						mark = "*"
					}
					fmt.Printf("%s %s  %s  ·  %s\n", mark, m.ID, name, when)
				} else {
					fmt.Printf("%s  %s  ·  %s\n", m.ID, name, when)
				}
			}
			if all {
				fmt.Println("\n* = started in this directory (borg --attach with no id resumes the newest)")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "list sessions from every directory, not just this one")
	return cmd
}

func newPurgeCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "purge",
		Short: "Delete all saved REPL sessions",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !yes && !confirm("Delete ALL saved sessions? [y/N]: ") {
				fmt.Println("Cancelled.")
				return nil
			}
			n, err := session.Purge()
			if err != nil {
				return err
			}
			fmt.Printf("Purged %d session(s).\n", n)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

// confirm prompts on stdout and reads a y/N answer from stdin.
func confirm(msg string) bool {
	fmt.Print(msg)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y")
}
