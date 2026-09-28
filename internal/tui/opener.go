package tui

import (
	"os/exec"
	"runtime"
)

// openURL opens url in the user's default browser. This is the production
// default for Deps.Opener; every test injects a fake instead (AGENTS.md
// hard testing rule, and the task's own "no browser launched" instruction),
// so this function is never exercised by a test.
func openURL(url string) error {
	return openWith(url)
}

// openFolder opens the directory at path in the OS file manager. This is
// the production default for Deps.OpenFolder; every test injects a fake for
// the same reason openURL's default is never used in a test.
func openFolder(path string) error {
	return openWith(path)
}

// openWith launches the OS's registered "open" handler for target, which
// works the same way for a URL and for a directory path on every platform
// this project ships for.
func openWith(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target).Start()
	case "windows":
		// "start" is a cmd builtin, not an executable; the empty first
		// argument is the window title cmd expects before the target.
		return exec.Command("cmd", "/c", "start", "", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}
