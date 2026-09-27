// Package service registers the daemon with the Windows Service Control
// Manager and runs under it.
//
// On every other platform the commands return an error and the rest of the
// binary stays fully portable.
package service

import (
	"context"
	"strings"
)

// Defaults for the service registration. Changing the name orphans an existing
// installation, so it is a constant rather than a config key.
const (
	DefaultName        = "bittrench"
	DefaultDisplayName = "bittrench"
	DefaultDescription = "User-space WireGuard BitTorrent engine (Transmission-compatible JSON-RPC)"
)

// InstallOptions are the knobs `install` accepts.
type InstallOptions struct {
	Name        string
	DisplayName string
	Description string
	// AutoStart registers the service to start at boot rather than on demand.
	AutoStart bool
	// StartNow starts the service immediately after registering it.
	StartNow bool
	// ConfigPath must be absolute: the SCM launches services with the working
	// directory set to C:\Windows\System32, so a relative path - and with it
	// the state database and log directory resolved beside it - would land
	// somewhere wrong.
	ConfigPath string
}

// UninstallOptions are the knobs `uninstall` accepts.
type UninstallOptions struct {
	Name string
	// Stop stops the service first if it is running.
	Stop bool
}

// commandLineRefersTo reports whether a registered service command line runs the
// executable at exe.
//
// The registered value is a command line, not a path: the executable comes
// first, quoted if it contains a space, followed by the arguments. Two forms
// have to be accepted, because a bare unquoted path containing spaces is
// indistinguishable from a path plus arguments: the whole string, and its first
// token. Comparing only the first token is what stops an unrelated service that
// merely mentions our path in an argument from counting.
//
// Paths are normalised by hand rather than with filepath, which follows the host
// platform: this parses Windows paths, and the tests for it run everywhere.
func commandLineRefersTo(commandLine, exe string) bool {
	if commandLine == "" || exe == "" {
		return false
	}
	if windowsPathsEqual(strings.TrimSpace(commandLine), exe) {
		return true
	}
	return windowsPathsEqual(firstCommandLineToken(commandLine), exe)
}

func firstCommandLineToken(commandLine string) string {
	if rest, ok := strings.CutPrefix(commandLine, `"`); ok {
		quoted, _, _ := strings.Cut(rest, `"`)
		return quoted
	}
	token, _, _ := strings.Cut(commandLine, " ")
	token, _, _ = strings.Cut(token, "\t")
	return token
}

// windowsPathsEqual compares two Windows paths the way Windows does: separators
// interchangeable, case insignificant, a trailing separator irrelevant.
func windowsPathsEqual(a, b string) bool {
	norm := func(p string) string {
		return strings.TrimRight(strings.ReplaceAll(p, "/", `\`), `\`)
	}
	return strings.EqualFold(norm(a), norm(b))
}

// RunFunc is the daemon body the service wraps. It must return when its
// context is cancelled - the SCM kills a process whose StopPending runs long.
type RunFunc func(ctx context.Context) error
