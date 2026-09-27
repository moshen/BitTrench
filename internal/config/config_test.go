package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped sample must parse against the real schema.
//
// With unknown keys rejected, a key that exists in the sample but not in the
// structs (or vice versa, once uncommented) is a startup failure for anyone
// who copies it. This pins the two together. It uses Parse rather than Load
// because Load also validates, and the sample's placeholder keys are not
// valid values.
func TestSampleConfigParsesAgainstTheSchema(t *testing.T) {
	sample, err := os.ReadFile(filepath.Join("..", "..", "config.sample.toml"))
	if err != nil {
		t.Fatalf("reading sample: %v", err)
	}
	if _, err := Parse(string(sample)); err != nil {
		t.Fatalf("config.sample.toml does not parse: %v", err)
	}
}

func TestSampleDocumentsTheKeys(t *testing.T) {
	sample, err := os.ReadFile(filepath.Join("..", "..", "config.sample.toml"))
	if err != nil {
		t.Fatalf("reading sample: %v", err)
	}
	for _, key := range []string{
		"max_peers_per_torrent",
		"listen_in_tunnel",
		"enable_pex",
		"enabled",
		"download_queue_size",
		"to_file",
	} {
		if !strings.Contains(string(sample), key) {
			t.Errorf("config.sample.toml does not document %s", key)
		}
	}
}

func TestUnknownKeysAreRejected(t *testing.T) {
	_, err := Parse("[torrent]\nsave_path = \"/tmp\"\nnot_a_real_key = 1\n")
	if err == nil {
		t.Fatal("expected an error for an unknown key")
	}
	if !strings.Contains(err.Error(), "not_a_real_key") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestDefaultsSurviveAnAbsentKey(t *testing.T) {
	cfg, err := Parse("[torrent]\nsave_path = \"/tmp\"\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.Torrent.Limits.SeedRatioLimit; got != 2.0 {
		t.Errorf("seed_ratio_limit default = %v, want 2.0", got)
	}
	if got := cfg.Torrent.Limits.UploadRateLimitKbps; got != 500 {
		t.Errorf("upload_rate_limit_kbps default = %v, want 500", got)
	}
	if !cfg.Torrent.FastResume || !cfg.Torrent.EnableDHT {
		t.Errorf("boolean defaults should be true, got %+v", cfg.Torrent)
	}
	if got := cfg.Torrent.PeerConnectTimeoutSecs; got != 10 {
		t.Errorf("peer_connect_timeout_secs default = %v, want 10", got)
	}
	if got := cfg.API.ListenPort; got != 6800 {
		t.Errorf("listen_port default = %v, want 6800", got)
	}
	if got := cfg.Logging.MaxAgeDays; got != 7 {
		t.Errorf("max_age_days default = %v, want 7", got)
	}
	if cfg.Torrent.EnablePex {
		t.Error("enable_pex should default to false")
	}
	if cfg.Torrent.ListenInTunnel {
		t.Error("listen_in_tunnel should default to false")
	}
	// An existing config that has never heard of the key must keep its API.
	if !cfg.API.Enabled {
		t.Error("api.enabled should default to true")
	}
	if got := cfg.Torrent.DownloadQueueSize; got != 5 {
		t.Errorf("download_queue_size default = %d, want 5", got)
	}
}

// A queue of zero is how an operator gets the pre-queue behaviour back, so an
// explicit zero must not be replaced by the default.
func TestDownloadQueueCanBeDisabled(t *testing.T) {
	cfg, err := Parse("[torrent]\nsave_path = \"/tmp\"\ndownload_queue_size = 0\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Torrent.DownloadQueueSize != 0 {
		t.Errorf("explicit download_queue_size = 0 became %d", cfg.Torrent.DownloadQueueSize)
	}
}

// Turning the API off must not then be rejected for the bind address it is no
// longer going to use, and must survive as an explicit false.
func TestAPICanBeDisabled(t *testing.T) {
	cfg, err := Parse("[api]\nenabled = false\nlisten_interface = \"not-an-ip\"\n" +
		"[torrent]\nsave_path = \"/tmp\"\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.API.Enabled {
		t.Fatal("explicit enabled = false became true")
	}
	cfg.WireGuard = WireGuardConfig{
		PrivateKey:    "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		PeerPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		Endpoint:      "198.51.100.5:51820",
		ClientIP:      "10.0.0.2/32",
		DNS:           "10.0.0.1",
	}
	if err := cfg.Validate(context.Background()); err != nil {
		t.Errorf("a disabled API should not be validated for its bind address: %v", err)
	}

	// With the API on, that same address is a startup error.
	cfg.API.Enabled = true
	if err := cfg.Validate(context.Background()); err == nil {
		t.Error("an enabled API with an unparseable listen_interface should be rejected")
	}
}

// An operator writing an explicit zero to disable a cap must not have it
// silently replaced by the default. This is the trap that pre-populating
// defaults before decoding exists to avoid getting wrong.
func TestExplicitZeroBeatsTheDefault(t *testing.T) {
	cfg, err := Parse("[torrent]\nsave_path = \"/tmp\"\n[torrent.limits]\nseed_ratio_limit = 0.0\nupload_rate_limit_kbps = 0\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.Torrent.Limits.SeedRatioLimit; got != 0 {
		t.Errorf("explicit seed_ratio_limit = 0.0 became %v", got)
	}
	if got := cfg.Torrent.Limits.UploadRateLimitKbps; got != 0 {
		t.Errorf("explicit upload_rate_limit_kbps = 0 became %v", got)
	}
}

func limits(t *testing.T, body string) *TorrentLimits {
	t.Helper()
	cfg, err := Parse("[torrent]\nsave_path = \"/tmp\"\n[torrent.limits]\n" + body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return &cfg.Torrent.Limits
}

func TestMaxPeersPerTorrent(t *testing.T) {
	if got := limits(t, "max_peers_per_torrent = 100\n").MaxPeersPerTorrent; got == nil || *got != 100 {
		t.Errorf("max_peers_per_torrent = %v, want 100", got)
	}
	if got := limits(t, "").MaxPeersPerTorrent; got != nil {
		t.Errorf("absent max_peers_per_torrent = %v, want nil so the engine picks", *got)
	}
	err := limits(t, "max_peers_per_torrent = 0\n").Validate()
	if err == nil || !strings.Contains(err.Error(), "max_peers_per_torrent") {
		t.Errorf("zero peers should be rejected, got: %v", err)
	}
	if err := limits(t, "max_peers_per_torrent = 1\n").Validate(); err != nil {
		t.Errorf("one peer should validate, got: %v", err)
	}
}

func TestDNSServers(t *testing.T) {
	w := WireGuardConfig{DNS: "203.0.113.1, 203.0.113.2"}
	got, err := w.DNSServers()
	if err != nil {
		t.Fatalf("DNSServers: %v", err)
	}
	if len(got) != 2 || got[0].String() != "203.0.113.1" || got[1].String() != "203.0.113.2" {
		t.Errorf("DNSServers = %v", got)
	}
	if _, err := (&WireGuardConfig{DNS: "not-an-ip"}).DNSServers(); err == nil {
		t.Error("expected an error for a non-IP DNS server")
	}
	if got, err := (&WireGuardConfig{}).DNSServers(); err != nil || len(got) != 0 {
		t.Errorf("empty dns = %v, %v; want empty and no error", got, err)
	}
}

// wireguard.dns is mandatory: without it nothing can be resolved in-tunnel and
// every lookup would fall back to the host resolver.
func TestMissingDNSIsRejected(t *testing.T) {
	cfg := validConfig()
	cfg.WireGuard.DNS = ""
	err := cfg.Validate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dns") {
		t.Errorf("empty dns should be rejected, got: %v", err)
	}
}

func TestKeyValidation(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(context.Background()); err != nil {
		t.Fatalf("the valid config should validate: %v", err)
	}

	short := validConfig()
	short.WireGuard.PrivateKey = "AAAA"
	if err := short.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Errorf("a short key should be rejected, got: %v", err)
	}

	garbage := validConfig()
	garbage.WireGuard.PeerPublicKey = "!!!not base64!!!"
	if err := garbage.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "base64") {
		t.Errorf("invalid base64 should be rejected, got: %v", err)
	}

	badCIDR := validConfig()
	badCIDR.WireGuard.ClientIP = "10.0.0.2"
	if err := badCIDR.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "client_ip") {
		t.Errorf("a client_ip without a prefix should be rejected, got: %v", err)
	}
}

// A literal IP:port endpoint must not touch the resolver at all.
func TestEndpointLiteralNeedsNoResolver(t *testing.T) {
	w := WireGuardConfig{Endpoint: "198.51.100.5:51820"}
	got, err := w.EndpointAddr(context.Background())
	if err != nil {
		t.Fatalf("EndpointAddr: %v", err)
	}
	if got.String() != "198.51.100.5:51820" {
		t.Errorf("EndpointAddr = %v", got)
	}
}

func TestResolvedPathsFollowTheConfigFile(t *testing.T) {
	cfg := Defaults()
	if got, want := cfg.StateDBFile(filepath.Join("/etc", "wg-bt", "config.toml")),
		filepath.Join("/etc", "wg-bt", "state.db"); got != want {
		t.Errorf("StateDBFile = %q, want %q", got, want)
	}
	if got, want := cfg.LogDir(filepath.Join("/etc", "wg-bt", "config.toml")),
		filepath.Join("/etc", "wg-bt", "logs"); got != want {
		t.Errorf("LogDir = %q, want %q", got, want)
	}
	cfg.Torrent.StateDBPath = "/var/lib/wg/state.db"
	cfg.Logging.LogDir = "/var/log/wg"
	if got := cfg.StateDBFile("config.toml"); got != "/var/lib/wg/state.db" {
		t.Errorf("explicit state_db_path ignored: %q", got)
	}
	if got := cfg.LogDir("config.toml"); got != "/var/log/wg" {
		t.Errorf("explicit log_dir ignored: %q", got)
	}
}

// validConfig is a fully-populated config whose every value is valid and whose
// endpoint is a literal, so validation performs no DNS lookup.
func validConfig() AppConfig {
	cfg := Defaults()
	cfg.WireGuard = WireGuardConfig{
		PrivateKey:       "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		PeerPublicKey:    "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		Endpoint:         "198.51.100.5:51820",
		ClientIP:         "10.0.0.2/32",
		KeepaliveSeconds: 25,
		DNS:              "203.0.113.1",
	}
	cfg.Torrent.SavePath = "/tmp/torrents"
	return cfg
}

// The environment overrides only the API listener, and only when it says
// something. Table-driven over a fake getter rather than t.Setenv, so the cases
// cannot interact and nothing depends on the process environment.
func TestApplyEnvOverridesTheListener(t *testing.T) {
	for _, tc := range []struct {
		name          string
		env           map[string]string
		wantInterface string
		wantPort      uint16
		wantErr       string
	}{
		{
			name:          "absent leaves the file alone",
			wantInterface: "127.0.0.1",
			wantPort:      6800,
		},
		{
			name:          "empty leaves the file alone",
			env:           map[string]string{EnvListenInterface: "", EnvListenPort: ""},
			wantInterface: "127.0.0.1",
			wantPort:      6800,
		},
		{
			name:          "both override",
			env:           map[string]string{EnvListenInterface: "0.0.0.0", EnvListenPort: "9091"},
			wantInterface: "0.0.0.0",
			wantPort:      9091,
		},
		{
			name:          "the interface alone overrides",
			env:           map[string]string{EnvListenInterface: "::"},
			wantInterface: "::",
			wantPort:      6800,
		},
		{
			name:          "surrounding space is ignored",
			env:           map[string]string{EnvListenPort: " 6801 "},
			wantInterface: "127.0.0.1",
			wantPort:      6801,
		},
		{
			name:    "a hostname is not an interface",
			env:     map[string]string{EnvListenInterface: "localhost"},
			wantErr: EnvListenInterface,
		},
		{
			name:    "a non-numeric port is reported",
			env:     map[string]string{EnvListenPort: "http"},
			wantErr: EnvListenPort,
		},
		{
			name:    "a port above 65535 is reported",
			env:     map[string]string{EnvListenPort: "70000"},
			wantErr: EnvListenPort,
		},
		{
			name:    "port 0 is reported",
			env:     map[string]string{EnvListenPort: "0"},
			wantErr: EnvListenPort,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			err := cfg.applyEnv(func(k string) string { return tc.env[k] })
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error naming %s", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error should name %s, got: %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("applyEnv: %v", err)
			}
			if cfg.API.ListenInterface != tc.wantInterface {
				t.Errorf("listen_interface = %q, want %q", cfg.API.ListenInterface, tc.wantInterface)
			}
			if cfg.API.ListenPort != tc.wantPort {
				t.Errorf("listen_port = %d, want %d", cfg.API.ListenPort, tc.wantPort)
			}
		})
	}
}

// An override has to survive the whole of Load, so ListenAddr reports the
// overridden address and not the file's.
func TestLoadAppliesTheListenerOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[api]\nlisten_interface = \"127.0.0.1\"\nlisten_port = 6800\n"), 0o600); err != nil {
		t.Fatalf("writing the config: %v", err)
	}
	t.Setenv(EnvListenInterface, "0.0.0.0")
	t.Setenv(EnvListenPort, "6800")

	// Load validates, and this config names no WireGuard peer, so it fails -
	// after applyEnv has run, which is what this asserts.
	cfg, err := Load(context.Background(), path)
	if err == nil {
		t.Fatal("expected validation to fail on a config with no [wireguard] section")
	}
	addr, err := cfg.API.ListenAddr()
	if err != nil {
		t.Fatalf("ListenAddr: %v", err)
	}
	if got := addr.String(); got != "0.0.0.0:6800" {
		t.Errorf("listen addr = %s, want 0.0.0.0:6800", got)
	}
}

// The save path and the file-logging switch, the other two things a container
// image pins. Defaults() has no save_path and logs to files, so every case here
// starts from "the file said nothing".
func TestApplyEnvOverridesTheSavePathAndFileLogging(t *testing.T) {
	for _, tc := range []struct {
		name         string
		env          map[string]string
		wantSavePath string
		wantToFile   bool
		wantErr      string
	}{
		{
			name:       "absent leaves the file alone",
			wantToFile: true,
		},
		{
			name:         "the save path overrides",
			env:          map[string]string{EnvSavePath: "/downloads"},
			wantSavePath: "/downloads",
			wantToFile:   true,
		},
		{
			name:       "file logging off",
			env:        map[string]string{EnvLogToFile: "0"},
			wantToFile: false,
		},
		{
			name:       "file logging off, spelled false",
			env:        map[string]string{EnvLogToFile: "false"},
			wantToFile: false,
		},
		{
			name:       "file logging back on",
			env:        map[string]string{EnvLogToFile: "1"},
			wantToFile: true,
		},
		{
			name:    "a non-boolean is reported",
			env:     map[string]string{EnvLogToFile: "sometimes"},
			wantErr: EnvLogToFile,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			err := cfg.applyEnv(func(k string) string { return tc.env[k] })
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error naming %s", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error should name %s, got: %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("applyEnv: %v", err)
			}
			if cfg.Torrent.SavePath != tc.wantSavePath {
				t.Errorf("save_path = %q, want %q", cfg.Torrent.SavePath, tc.wantSavePath)
			}
			if cfg.Logging.ToFile != tc.wantToFile {
				t.Errorf("to_file = %v, want %v", cfg.Logging.ToFile, tc.wantToFile)
			}
		})
	}
}

// An overridden save_path satisfies the requirement that one be set, so a config
// that names none is usable in a container that names one.
func TestSavePathFromTheEnvironmentSatisfiesValidation(t *testing.T) {
	cfg := validConfig()
	cfg.Torrent.SavePath = ""
	if err := cfg.Validate(context.Background()); err == nil {
		t.Fatal("expected a missing save_path to be rejected")
	}
	if err := cfg.applyEnv(func(k string) string {
		if k == EnvSavePath {
			return "/downloads"
		}
		return ""
	}); err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if err := cfg.Validate(context.Background()); err != nil {
		t.Errorf("an overridden save_path should validate: %v", err)
	}
}

// to_file off is reported to logging.Setup as an empty directory, which is what
// keeps the rolling writer from being started at all.
func TestLogDirIsEmptyWhenFileLoggingIsOff(t *testing.T) {
	cfg := Defaults()
	cfg.Logging.ToFile = false
	if got := cfg.LogDir(filepath.Join("/etc", "bittrench", "config.toml")); got != "" {
		t.Errorf("LogDir = %q, want the empty string", got)
	}
	// An explicit log_dir does not bring it back: to_file is the switch.
	cfg.Logging.LogDir = "/var/log/bittrench"
	if got := cfg.LogDir("config.toml"); got != "" {
		t.Errorf("LogDir = %q, want the empty string", got)
	}
}
