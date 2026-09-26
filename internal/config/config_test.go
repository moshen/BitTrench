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
	if !cfg.Torrent.FastResume || !cfg.Torrent.EnableDHT || !cfg.Torrent.DisableUPnPPortForward {
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
