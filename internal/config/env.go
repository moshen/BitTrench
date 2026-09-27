package config

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// The environment overrides four keys, each named BITTRENCH_<TABLE>_<KEY> after
// the one it overrides, and no others.
//
// This is not a general "any key can come from the environment" mechanism, and
// deliberately not: the file is the schema and the compatibility surface, and a
// value that can arrive from two places is a value nobody can find. What these
// four have in common is that they are the container's business rather than the
// operator's - the image has to pin them so a mounted config, written for a host
// or copied between deployments, cannot change how the container is wired:
//
//   - the listener's interface and port, because a published port has to reach a
//     listener on a routable interface and 127.0.0.1 inside a container reaches
//     nothing outside it - and because `docker run -p` should map the same port
//     every time;
//   - the save path, because the writable mount is the image's to name, not the
//     config file's;
//   - file logging, because a container's logs belong on stderr where the
//     runtime collects them, and rolling files in a volume nobody reads are
//     just growth.
//
// A fifth needs the same argument.
const (
	// EnvListenInterface overrides `[api] listen_interface`.
	EnvListenInterface = "BITTRENCH_API_LISTEN_INTERFACE"
	// EnvListenPort overrides `[api] listen_port`.
	EnvListenPort = "BITTRENCH_API_LISTEN_PORT"
	// EnvSavePath overrides `[torrent] save_path`.
	EnvSavePath = "BITTRENCH_TORRENT_SAVE_PATH"
	// EnvLogToFile overrides `[logging] to_file`.
	EnvLogToFile = "BITTRENCH_LOGGING_TO_FILE"
)

// applyEnv overlays the overrides above onto an already-parsed config. env is a
// getter so tests need not touch the process environment.
//
// Unset and empty are both "leave the file alone", so an image that passes the
// variables through without a value behaves like one that does not set them.
// Values are checked here rather than left to Validate: the error has to name
// the variable the operator actually set, not the config key it landed on.
func (c *AppConfig) applyEnv(env func(string) string) error {
	if v := strings.TrimSpace(env(EnvListenInterface)); v != "" {
		if _, err := netip.ParseAddr(v); err != nil {
			return fmt.Errorf("invalid %s %q: %w", EnvListenInterface, v, err)
		}
		c.API.ListenInterface = v
	}
	if v := strings.TrimSpace(env(EnvListenPort)); v != "" {
		port, err := strconv.ParseUint(v, 10, 16)
		if err != nil {
			return fmt.Errorf("invalid %s %q: %w", EnvListenPort, v, err)
		}
		// Port 0 binds whatever the kernel hands out, which cannot be mapped
		// or found again - the opposite of why these variables exist.
		if port == 0 {
			return fmt.Errorf("invalid %s %q: 0 would bind an arbitrary port", EnvListenPort, v)
		}
		c.API.ListenPort = uint16(port)
	}
	if v := strings.TrimSpace(env(EnvSavePath)); v != "" {
		// No check that it exists or is writable: the file's own save_path gets
		// none either, and the engine reports what it cannot write to. Checking
		// only here would make the two spellings of one key behave differently.
		c.Torrent.SavePath = v
	}
	if v := strings.TrimSpace(env(EnvLogToFile)); v != "" {
		// strconv.ParseBool, so 1/0, true/false and t/f all work - and anything
		// else is an error rather than a quiet "false" that loses the log files
		// an operator asked for.
		toFile, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid %s %q: want a boolean, e.g. 1 or 0: %w", EnvLogToFile, v, err)
		}
		c.Logging.ToFile = toFile
	}
	return nil
}
