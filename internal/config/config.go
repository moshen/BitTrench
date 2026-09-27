// Package config parses the unified `config.toml` that drives the daemon.
//
// The schema is a compatibility surface: operators have live configuration
// files, and a key that silently does nothing is the exact failure this schema
// is built to prevent. Unknown keys are therefore a startup error rather than a
// warning, after three keys in a live config once did nothing for months.
package config

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// AppConfig is the whole of `config.toml`.
type AppConfig struct {
	WireGuard WireGuardConfig `toml:"wireguard"`
	API       APIConfig       `toml:"api"`
	Torrent   TorrentConfig   `toml:"torrent"`
	Logging   LoggingConfig   `toml:"logging"`
}

// WireGuardConfig is the `[wireguard]` table.
type WireGuardConfig struct {
	// PrivateKey is the base64-encoded WireGuard private key (client side).
	PrivateKey string `toml:"private_key"`
	// PeerPublicKey is the base64-encoded peer public key (VPN provider side).
	PeerPublicKey string `toml:"peer_public_key"`
	// Endpoint is the UDP endpoint of the VPN provider, e.g. `198.51.100.5:51820`.
	Endpoint string `toml:"endpoint"`
	// ClientIP is the virtual client IP assigned by the provider, with CIDR,
	// e.g. `10.0.0.2/32`.
	ClientIP string `toml:"client_ip"`
	// KeepaliveSeconds is the persistent keepalive interval. 0 disables it.
	KeepaliveSeconds uint32 `toml:"keepalive_seconds"`
	// DNS is a comma-separated list of DNS server IPs the tunnel-resident
	// resolver queries over UDP/53 through the user-space stack. Required:
	// without it every lookup a torrent implies would fall back to the host
	// resolver and the tunnel would be decorative.
	DNS string `toml:"dns"`
}

// APIConfig is the `[api]` table: the one listener that intentionally lives on
// the host stack.
type APIConfig struct {
	// Enabled serves the Transmission RPC endpoint and the web UI. Turning it
	// off opens no host listener at all, which leaves the WireGuard bind as
	// the process's only host socket - and leaves nothing to drive the daemon
	// with, so it is for unattended and one-shot runs. `get` forces it off.
	Enabled         bool   `toml:"enabled"`
	ListenInterface string `toml:"listen_interface"`
	ListenPort      uint16 `toml:"listen_port"`
	// Username and Password are HTTP Basic Auth credentials shared by the
	// Transmission RPC endpoint and the web UI. Empty disables auth.
	Username string `toml:"username"`
	Password string `toml:"password"`
}

// TorrentConfig is the `[torrent]` table.
type TorrentConfig struct {
	SavePath               string `toml:"save_path"`
	FastResume             bool   `toml:"fast_resume"`
	EnableDHT              bool   `toml:"enable_dht"`
	PeerConnectTimeoutSecs uint32 `toml:"peer_connect_timeout_secs"`
	// StateDBPath overrides the SQLite state database location. Defaults to
	// `state.db` in the same directory as the config file.
	StateDBPath string `toml:"state_db_path"`
	// AllowedExtensions is an optional allow-list of file extensions. When
	// non-empty, only files whose extension is listed are downloaded.
	// Matched case-insensitively, with or without a leading dot.
	AllowedExtensions []string `toml:"allowed_extensions"`
	// DownloadQueueSize is how many torrents may download at once. The rest
	// wait in queue-position order and report Transmission's download-wait
	// status. 0 disables the queue, so everything downloads at once.
	//
	// Completed torrents seed without occupying a slot, and a torrent stopped
	// by hand does not hold one either.
	DownloadQueueSize uint32 `toml:"download_queue_size"`
	// ListenInTunnel accepts inbound BitTorrent connections *inside* the
	// tunnel. No host socket is opened either way, so this is not a leak.
	ListenInTunnel bool `toml:"listen_in_tunnel"`
	// ListenPort is the in-tunnel listen port used when ListenInTunnel is set.
	ListenPort uint16 `toml:"listen_port"`
	// EnablePex trades peer lists with peers already reached through the
	// tunnel. Not a leak, but it widens who learns we are in a swarm, so it
	// is off by default.
	EnablePex bool          `toml:"enable_pex"`
	Limits    TorrentLimits `toml:"limits"`
}

// TorrentLimits is the `[torrent.limits]` table.
type TorrentLimits struct {
	DownloadRateLimitKbps uint32 `toml:"download_rate_limit_kbps"`
	UploadRateLimitKbps   uint32 `toml:"upload_rate_limit_kbps"`
	// SeedRatioLimit auto-pauses a finished torrent once uploaded/total
	// reaches it. 0 disables the cap.
	SeedRatioLimit float64 `toml:"seed_ratio_limit"`
	// SeedTimeLimitSecs auto-pauses a torrent this long after it finishes.
	// 0 disables the cap.
	SeedTimeLimitSecs uint64 `toml:"seed_time_limit_secs"`
	// MaxPeersPerTorrent caps concurrently connected peers per torrent.
	// Nil means "use the engine's own default".
	MaxPeersPerTorrent *int `toml:"max_peers_per_torrent"`
}

// LoggingConfig is the `[logging]` table. Drives the rolling-file writer used
// both for inline runs and under the Windows SCM, where stdout is disconnected
// and a file is the only way to see what is happening.
type LoggingConfig struct {
	// LogDir defaults to a `logs/` subdirectory next to the config file.
	LogDir string `toml:"log_dir"`
	// MaxAgeDays deletes log files older than this. 0 disables cleanup.
	MaxAgeDays uint32 `toml:"max_age_days"`
}

// Defaults returns an AppConfig pre-populated with every default value.
//
// Decoding into this rather than into a zero value is what lets an absent key
// keep its default: BurntSushi/toml only assigns the fields a document actually
// names, so an absent key keeps whatever is here, and an explicit
// `seed_ratio_limit = 0.0` still means zero instead of being overwritten by the
// default.
func Defaults() AppConfig {
	return AppConfig{
		API: APIConfig{
			Enabled:         true,
			ListenInterface: "127.0.0.1",
			ListenPort:      6800,
		},
		Torrent: TorrentConfig{
			FastResume:             true,
			EnableDHT:              true,
			PeerConnectTimeoutSecs: 10,
			DownloadQueueSize:      5,
			ListenPort:             6881,
			Limits: TorrentLimits{
				UploadRateLimitKbps: 500,
				SeedRatioLimit:      2.0,
			},
		},
		Logging: LoggingConfig{MaxAgeDays: 7},
	}
}

// Parse decodes TOML text against the schema, rejecting unknown keys.
//
// Split out from Load so tests (and the sample-config check) can exercise the
// schema without the network lookups Validate performs.
func Parse(text string) (AppConfig, error) {
	cfg := Defaults()
	md, err := toml.Decode(text, &cfg)
	if err != nil {
		return cfg, err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return cfg, fmt.Errorf("unknown config keys: %s", strings.Join(keys, ", "))
	}
	return cfg, nil
}

// Load reads, parses and validates the config at path.
func Load(ctx context.Context, path string) (AppConfig, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return AppConfig{}, fmt.Errorf("failed to read config file %s: %w", path, err)
	}
	cfg, err := Parse(string(contents))
	if err != nil {
		return cfg, fmt.Errorf("failed to parse config file %s: %w", path, err)
	}
	if err := cfg.Validate(ctx); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate runs the sync checks plus the one deliberate host-stack lookup
// (the WireGuard endpoint), so unusable keys surface at startup rather than
// deep inside the device or the torrent client.
func (c *AppConfig) Validate(ctx context.Context) error {
	if _, err := c.WireGuard.PrivateKeyBytes(); err != nil {
		return err
	}
	if _, err := c.WireGuard.PeerPublicKeyBytes(); err != nil {
		return err
	}
	if _, err := c.WireGuard.EndpointAddr(ctx); err != nil {
		return err
	}
	if _, err := c.WireGuard.ClientPrefix(); err != nil {
		return err
	}
	servers, err := c.WireGuard.DNSServers()
	if err != nil {
		return err
	}
	// Mandatory, not merely recommended: an empty server list produces a
	// netstack that cannot resolve at all, and every lookup would otherwise
	// have to fall back to the host resolver - which is the leak this daemon
	// exists to close.
	if len(servers) == 0 {
		return fmt.Errorf("[wireguard] dns is required: without it no name can be resolved inside the tunnel")
	}
	if c.API.Enabled {
		if _, err := c.API.ListenAddr(); err != nil {
			return err
		}
	}
	if c.Torrent.SavePath == "" {
		return fmt.Errorf("[torrent] save_path is required")
	}
	return c.Torrent.Limits.Validate()
}

// Validate performs the sync, network-free checks on the limit values.
func (l *TorrentLimits) Validate() error {
	if l.MaxPeersPerTorrent != nil && *l.MaxPeersPerTorrent == 0 {
		return fmt.Errorf("[torrent.limits] max_peers_per_torrent = 0 would allow no peer " +
			"connections at all; omit the key to use the engine's default")
	}
	return nil
}

// PrivateKeyBytes decodes the private key into the 32-byte form WireGuard expects.
func (w *WireGuardConfig) PrivateKeyBytes() ([32]byte, error) {
	return decodeKey(w.PrivateKey, "private_key")
}

// PeerPublicKeyBytes decodes the peer public key into its 32-byte form.
func (w *WireGuardConfig) PeerPublicKeyBytes() ([32]byte, error) {
	return decodeKey(w.PeerPublicKey, "peer_public_key")
}

func decodeKey(src, name string) ([32]byte, error) {
	var out [32]byte
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(src))
	if err != nil {
		return out, fmt.Errorf("%s: invalid base64: %w", name, err)
	}
	if len(raw) != 32 {
		return out, fmt.Errorf("%s: expected 32 bytes after base64 decode, got %d", name, len(raw))
	}
	copy(out[:], raw)
	return out, nil
}

// EndpointAddr resolves the VPN provider's UDP endpoint.
//
// Accepts either an `IP:port` literal or a `hostname:port` string; the latter
// is resolved through the host OS resolver. This is the one hostname lookup
// that legitimately happens outside the tunnel: the WireGuard UDP socket is
// the only thing that touches the host network stack, and the peer's IP is
// needed before the tunnel exists so that every later lookup can go out over
// UDP/53 through the provider's DNS. The chicken-and-egg is unavoidable and is
// exactly how `wg-quick` behaves.
//
// IPv4 results are preferred so the host UDP socket can deliver datagrams;
// every mainstream provider has IPv4 endpoints.
func (w *WireGuardConfig) EndpointAddr(ctx context.Context) (netip.AddrPort, error) {
	// Fast path: an IP:port literal needs no resolver at all.
	if ap, err := netip.ParseAddrPort(w.Endpoint); err == nil {
		return ap, nil
	}
	host, portStr, err := net.SplitHostPort(w.Endpoint)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("invalid wireguard.endpoint %q: %w", w.Endpoint, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("invalid wireguard.endpoint port %q: %w", portStr, err)
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("invalid wireguard.endpoint %q: %w", w.Endpoint, err)
	}
	if len(addrs) == 0 {
		return netip.AddrPort{}, fmt.Errorf("wireguard.endpoint resolved to no addresses: %s", w.Endpoint)
	}
	chosen := addrs[0]
	for _, a := range addrs {
		if a.Unmap().Is4() {
			chosen = a
			break
		}
	}
	return netip.AddrPortFrom(chosen.Unmap(), uint16(port)), nil
}

// ClientPrefix parses the virtual client IP/CIDR (e.g. 10.0.0.2/32).
func (w *WireGuardConfig) ClientPrefix() (netip.Prefix, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(w.ClientIP))
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("invalid wireguard.client_ip %q: %w", w.ClientIP, err)
	}
	return p, nil
}

// Keepalive is the persistent keepalive interval, or 0 when disabled.
func (w *WireGuardConfig) Keepalive() time.Duration {
	return time.Duration(w.KeepaliveSeconds) * time.Second
}

// DNSServers parses the comma-separated DNS server list.
func (w *WireGuardConfig) DNSServers() ([]netip.Addr, error) {
	var out []netip.Addr
	for _, field := range strings.Split(w.DNS, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		addr, err := netip.ParseAddr(field)
		if err != nil {
			return nil, fmt.Errorf("invalid DNS server IP %q: %w", field, err)
		}
		out = append(out, addr)
	}
	return out, nil
}

// ListenAddr is the host bind address for the RPC/UI server, e.g. `127.0.0.1:6800`.
func (a *APIConfig) ListenAddr() (netip.AddrPort, error) {
	addr, err := netip.ParseAddr(a.ListenInterface)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("invalid api.listen_interface %q: %w", a.ListenInterface, err)
	}
	return netip.AddrPortFrom(addr, a.ListenPort), nil
}

// StateDBFile is the resolved SQLite state database path, defaulting to
// `state.db` beside the config file.
func (c *AppConfig) StateDBFile(configPath string) string {
	if c.Torrent.StateDBPath != "" {
		return c.Torrent.StateDBPath
	}
	return filepath.Join(configDir(configPath), "state.db")
}

// LogDir is the resolved log directory, defaulting to `logs/` beside the
// config file.
func (c *AppConfig) LogDir(configPath string) string {
	if c.Logging.LogDir != "" {
		return c.Logging.LogDir
	}
	return filepath.Join(configDir(configPath), "logs")
}

func configDir(configPath string) string {
	dir := filepath.Dir(configPath)
	if dir == "" {
		return "."
	}
	return dir
}
