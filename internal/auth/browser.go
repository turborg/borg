package auth

import (
	"os"
	"os/exec"
	"runtime"
)

// browserAvailable reports whether borg can plausibly open a desktop browser.
// It returns false on headless/SSH boxes, where Login falls back to the device
// flow.
func browserAvailable() bool {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return false
	}
	if runtime.GOOS == "linux" {
		// No display server => no browser.
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return false
		}
	}
	return true
}

// openBrowser best-effort opens url in the user's browser. It honors $BROWSER
// (so a non-default browser/profile can be forced) before falling back to the
// platform opener. It is a var so tests can substitute the browser step.
var openBrowser = func(url string) error {
	if b := os.Getenv("BROWSER"); b != "" {
		return exec.Command(b, url).Start()
	}
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		name = "xdg-open"
	}
	args = append(args, url)
	return exec.Command(name, args...).Start()
}
