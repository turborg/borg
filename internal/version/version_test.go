package version

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// Command echoes the name the binary was launched as ("turborg" or "borg"),
// derived from argv[0], and falls back to "borg" for anything else.
func TestCommand(t *testing.T) {
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })

	cases := map[string]string{
		"/usr/local/bin/turborg": "turborg",
		"/home/x/go/bin/borg":    "borg",
		"./borg":                 "borg",
		"turborg":                "turborg",
		"turborg.exe":            "turborg",
		"borg.exe":               "borg",
		"some-test-binary":       "borg", // unexpected ⇒ default
	}
	for argv0, want := range cases {
		os.Args = []string{argv0}
		require.Equalf(t, want, Command(), "argv0=%q", argv0)
	}
}
