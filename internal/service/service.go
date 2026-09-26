// Package service registers the daemon with the Windows Service Control
// Manager and runs under it.
//
// On every other platform the commands return an error and the rest of the
// binary stays fully portable.
package service

import "context"

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

// RunFunc is the daemon body the service wraps. It must return when its
// context is cancelled - the SCM kills a process whose StopPending runs long.
type RunFunc func(ctx context.Context) error
