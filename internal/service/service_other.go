//go:build !windows

package service

import "errors"

// errNotWindows is what every service command reports off Windows. The
// subcommands still exist so the CLI surface is identical on every platform.
var errNotWindows = errors.New("service management is only supported on Windows")

// Install is unsupported off Windows.
func Install(InstallOptions) (bool, error) { return false, errNotWindows }

// Uninstall is unsupported off Windows.
func Uninstall(UninstallOptions) error { return errNotWindows }

// Serve is unsupported off Windows.
func Serve(string, RunFunc) error { return errNotWindows }

// IsWindowsService is always false off Windows.
func IsWindowsService() bool { return false }
