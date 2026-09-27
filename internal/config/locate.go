package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// FileName is what the configuration file is called in every standard location.
const FileName = "config.toml"

// appDir is the per-application directory inside a standard config location.
const appDir = "bittrench"

// SearchPaths returns the configuration paths tried when none is given on the
// command line, in the order they are tried.
//
// The working directory comes first, so running the binary beside its own
// config keeps working and a checkout's config wins over an installed one. Then
// the XDG location, on every platform including Windows, so there is one
// documented answer to "where does my config go" regardless of where the daemon
// runs. Then, on Windows only, the machine-wide location, which is where a
// service installed for all users should read from: it is last because a
// per-user file should override it, not the other way round.
func SearchPaths() []string {
	return searchPaths(runtime.GOOS, os.Getenv, os.UserHomeDir)
}

func searchPaths(goos string, env func(string) string, home func() (string, error)) []string {
	paths := []string{FileName}

	// XDG says a relative XDG_CONFIG_HOME is invalid and must be ignored, which
	// matters here because a relative one would resolve against the working
	// directory and silently shadow the entry above.
	if dir := env("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		paths = append(paths, filepath.Join(dir, appDir, FileName))
	} else if h, err := home(); err == nil && h != "" {
		paths = append(paths, filepath.Join(h, ".config", appDir, FileName))
	}

	if goos == "windows" {
		programData := env("ProgramData")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		paths = append(paths, filepath.Join(programData, appDir, FileName))
	}
	return paths
}

// Discover returns the first configuration file that exists among SearchPaths.
//
// The error names everywhere it looked, because "no configuration file found" on
// its own leaves an operator guessing at exactly the moment they need telling.
func Discover() (string, error) {
	return discover(SearchPaths())
}

func discover(paths []string) (string, error) {
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no configuration file found; looked for %s",
		strings.Join(paths, ", "))
}
