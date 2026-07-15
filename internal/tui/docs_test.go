package tui

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSlashCommands(t *testing.T) {
	cmds := SlashCommands()
	require.Equal(t, len(slashCmds), len(cmds))
	require.Equal(t, slashCmds[0].name, cmds[0].Name)
	require.Equal(t, slashCmds[0].desc, cmds[0].Desc)
	// /update should be documented (added with the self-updater).
	found := false
	for _, c := range cmds {
		if c.Name == "/update" {
			found = true
		}
	}
	require.True(t, found)
}
