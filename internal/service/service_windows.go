//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// statusInterval is how often StopPending is re-reported while shutting down.
// The SCM kills a service that goes quiet, and our shutdown sequence has a
// deadline of its own, so the two have to be told about each other.
const statusInterval = 2 * time.Second

// Install registers the running binary with the Service Control Manager,
// replacing an existing registration if there is one.
//
// replaced reports that an existing service was removed and recreated, which is
// how an update is done here: one code path decides what a registration looks
// like. Reconfiguring in place would mean reading the existing configuration,
// changing the fields we own and writing the rest back untouched, because
// ChangeServiceConfig takes every field at once. Recreating keeps the definition
// in one place at the cost of resetting anything customised by hand in
// services.msc, such as recovery actions or a non-default log-on account.
//
// Because the registration is rewritten, the executable path is rewritten too:
// moving the binary and re-running install is enough to point the service at it.
func Install(opts InstallOptions) (replaced bool, err error) {
	if !filepath.IsAbs(opts.ConfigPath) {
		return false, fmt.Errorf(
			"the config path must be absolute for an installed service, got %q: "+
				"the SCM runs services from C:\\Windows\\System32, so a relative path "+
				"would put the state database and logs somewhere unexpected",
			opts.ConfigPath)
	}
	// A service is always registered by full path. os.Executable gives one, and
	// it is resolved afresh on every install so a moved binary is picked up.
	exe, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("couldn't locate the running executable: %w", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		return false, fmt.Errorf("couldn't connect to the local Service Control Manager: %w", err)
	}
	defer m.Disconnect()

	if err := refuseStrayInstalls(m, exe, opts.Name); err != nil {
		return false, err
	}

	// Whether it was running decides whether to start it again: an update should
	// leave the service in the state it found it, and removing it necessarily
	// stops it first.
	wasRunning := false
	if existing, openErr := m.OpenService(opts.Name); openErr == nil {
		if status, queryErr := existing.Query(); queryErr == nil {
			wasRunning = status.State == svc.Running || status.State == svc.StartPending
		}
		existing.Close()

		if err := remove(m, opts.Name, true); err != nil {
			return false, fmt.Errorf("couldn't remove the existing service before "+
				"reinstalling it: %w", err)
		}
		replaced = true
	}

	startType := uint32(mgr.StartManual)
	if opts.AutoStart {
		startType = mgr.StartAutomatic
	}
	s, err := m.CreateService(opts.Name, exe, mgr.Config{
		DisplayName:  opts.DisplayName,
		Description:  opts.Description,
		StartType:    startType,
		ErrorControl: mgr.ErrorNormal,
	}, "serve", "-config", opts.ConfigPath)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			// The old service is deleted but not gone: Windows keeps it until
			// every handle on it closes, which means the daemon it was running
			// has not exited yet. This is the one failure that leaves nothing
			// registered, so say what to do about it.
			return replaced, fmt.Errorf("the previous service %q is still shutting "+
				"down, so the new one could not be registered yet: wait for it to "+
				"stop and run install again", opts.Name)
		}
		return replaced, fmt.Errorf("couldn't register the service %q: %w", opts.Name, err)
	}
	defer s.Close()

	if opts.StartNow || wasRunning {
		if err := s.Start(); err != nil {
			return replaced, fmt.Errorf("the service was registered but would not start: %w", err)
		}
	}
	return replaced, nil
}

// refuseStrayInstalls fails if any other service runs this same executable.
//
// One machine, one daemon: two services sharing a binary share its configuration
// too, which means one state database and one API port between them. The second
// to start normally fails to bind the port, but with the API disabled both run
// and contend over the same SQLite file and the same download directory.
//
// Best effort by necessity. Enumerating services is one call, but reading a
// service's configuration means opening it, and plenty of them refuse even an
// administrator. Those are skipped: the ones this is looking for were installed
// by this binary and open fine.
func refuseStrayInstalls(m *mgr.Mgr, exe, name string) error {
	names, err := m.ListServices()
	if err != nil {
		// Not fatal. Failing the install because the machine would not hand over
		// a service list would be worse than the duplicate this is looking for.
		slog.Warn("couldn't list services to check for another installation", "error", err)
		return nil
	}
	for _, other := range names {
		if strings.EqualFold(other, name) {
			continue
		}
		s, err := m.OpenService(other)
		if err != nil {
			continue
		}
		cfg, err := s.Config()
		s.Close()
		if err != nil || !commandLineRefersTo(cfg.BinaryPathName, exe) {
			continue
		}
		return fmt.Errorf("the service %q already runs this binary (%s): "+
			"two services sharing it would share one state database and one API "+
			"port, so remove it first with `bittrench uninstall -name %s`",
			other, exe, other)
	}
	return nil
}

// Uninstall removes a previously installed service.
func Uninstall(opts UninstallOptions) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("couldn't connect to the local Service Control Manager: %w", err)
	}
	defer m.Disconnect()
	return remove(m, opts.Name, opts.Stop)
}

// remove stops and deletes a service. Shared by uninstall and by the reinstall
// half of an update, so both wait for a stop the same way.
func remove(m *mgr.Mgr, name string, stop bool) error {
	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("the service %q is not installed: %w", name, err)
	}
	defer s.Close()

	if stop {
		if status, err := s.Control(svc.Stop); err == nil {
			deadline := time.Now().Add(30 * time.Second)
			for status.State != svc.Stopped && time.Now().Before(deadline) {
				time.Sleep(300 * time.Millisecond)
				if status, err = s.Query(); err != nil {
					break
				}
			}
		}
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("couldn't remove the service %q: %w", name, err)
	}
	return nil
}

// Serve runs under the SCM, translating its Stop control into a cancelled
// context on the same run function the foreground path uses.
func Serve(name string, run RunFunc) error {
	return svc.Run(name, &handler{run: run})
}

// IsWindowsService reports whether the process was launched by the SCM.
func IsWindowsService() bool {
	is, err := svc.IsWindowsService()
	return err == nil && is
}

type handler struct{ run RunFunc }

func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()

	status <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case err := <-done:
			// The daemon exited on its own.
			status <- svc.Status{State: svc.Stopped}
			if err != nil {
				slog.Error("the service stopped with an error", "error", err)
				return false, 1
			}
			return false, 0

		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				cancel()
				return h.waitForShutdown(done, status, accepted)
			default:
				slog.Debug("ignoring an unexpected service control", "cmd", c.Cmd)
			}
		}
	}
}

// waitForShutdown keeps reporting StopPending while the daemon drains, because
// a service that goes quiet during a long stop is killed outright.
func (h *handler) waitForShutdown(done <-chan error, status chan<- svc.Status, accepted svc.Accepted) (bool, uint32) {
	checkpoint := uint32(1)
	status <- svc.Status{State: svc.StopPending, Accepts: accepted,
		CheckPoint: checkpoint, WaitHint: uint32(statusInterval / time.Millisecond * 2)}

	ticker := time.NewTicker(statusInterval)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			status <- svc.Status{State: svc.Stopped}
			if err != nil {
				slog.Error("the service stopped with an error", "error", err)
				return false, 1
			}
			return false, 0
		case <-ticker.C:
			checkpoint++
			status <- svc.Status{State: svc.StopPending, Accepts: accepted,
				CheckPoint: checkpoint, WaitHint: uint32(statusInterval / time.Millisecond * 2)}
		}
	}
}
