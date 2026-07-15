package tui

// SlashCommand is a documented REPL slash command, exposed for the docs
// generator (`turborg gen-docs`) so the published reference stays in sync with
// the actual command registry.
type SlashCommand struct{ Name, Desc string }

// SlashCommands returns the REPL slash commands in display order.
func SlashCommands() []SlashCommand {
	out := make([]SlashCommand, len(slashCmds))
	for i, c := range slashCmds {
		out[i] = SlashCommand{Name: c.name, Desc: c.desc}
	}
	return out
}
