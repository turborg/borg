// Package version exposes the build version, stamped via -ldflags at release time.
package version

import (
	"os"
	"path/filepath"
	"strings"
)

// Version is the build version. The Makefile and goreleaser override it with
// the real tag; it stays "dev" for local builds.
var Version = "dev"

// Command is the command name the user typed to launch this binary — "turborg"
// or "borg" (the same binary installs under both names). Derived from argv[0];
// anything unexpected falls back to "borg". Used so help, the REPL banner, and
// prompts echo whichever name actually launched the process.
func Command() string {
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if name == "turborg" {
		return "turborg"
	}
	return "borg"
}
