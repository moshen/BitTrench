//go:build windows

package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// statusInterval is how often StopPending is re-reported while shutting down.
// The SCM kills a service that goes quiet, and our shutdown sequence has a
// deadline of its own, so the two have to be told about each other.
const statusInterval = 2 * time.Second

// Install registers the running binary with the Service Control Manager.
func Install(opts InstallOptions) error {
	if !filepath.IsAbs(opts.ConfigPath) {
		return fmt.Errorf(
			"the config path must be absolute for an installed service, got %q: "+
				"the SCM runs services from C:\\Windows\\System32, so a relative path "+
				"would put the state database and logs somewhere unexpected",
			opts.ConfigPath)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("couldn't locate the running executable: %w", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("couldn't connect to the local Service Control Manager: %w", err)
	}
	defer m.Disconnect()

	if existing, err := m.OpenService(opts.Name); err == nil {
		existing.Close()
		return fmt.Errorf("the service %q is already installed", opts.Name)
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
		return fmt.Errorf("couldn't register the service %q: %w", opts.Name, err)
	}
	defer s.Close()

	if opts.StartNow {
		if err := s.Start(); err != nil {
			return fmt.Errorf("the service was registered but would not start: %w", err)
		}
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

	s, err := m.OpenService(opts.Name)
	if err != nil {
		return fmt.Errorf("the service %q is not installed: %w", opts.Name, err)
	}
	defer s.Close()

	if opts.Stop {
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
		return fmt.Errorf("couldn't remove the service %q: %w", opts.Name, err)
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
