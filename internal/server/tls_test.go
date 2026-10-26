package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moshen/bittrench/internal/config"
)

// startTLS serves with `[api] tls` on and the config file - and so the state
// database and the key pair - in dir.
func startTLS(t *testing.T, dir string) *Server {
	t.Helper()
	cfg := config.Defaults()
	cfg.Torrent.SavePath = t.TempDir()
	cfg.API.ListenPort = 0
	cfg.API.TLS = true
	srv, err := New(&cfg, filepath.Join(dir, "config.toml"), &stubEngine{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	return srv
}

// trusting is a client that trusts exactly the certificate in certFile, so a
// passing request proves the listener serves that certificate and that it
// names the loopback address it was reached on.
func trusting(t *testing.T, certFile string) *http.Client {
	t.Helper()
	pem, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("reading the certificate: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("the generated certificate is not valid PEM")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
}

// The pair lands beside the state database, serves, and keeps its key private.
func TestTLSGeneratesACertificateBesideTheDatabase(t *testing.T) {
	dir := t.TempDir()
	srv := startTLS(t, dir)
	if !strings.HasPrefix(srv.URL(), "https://") {
		t.Fatalf("URL = %s, want https", srv.URL())
	}
	resp, err := trusting(t, filepath.Join(dir, "tls.crt")).Get(srv.URL() + "/")
	if err != nil {
		t.Fatalf("GET / over TLS: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / over TLS = %d", resp.StatusCode)
	}

	// Windows has no Unix permission bits to check.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "tls.key"))
		if err != nil {
			t.Fatalf("stat key: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("tls.key mode = %o, want 600", perm)
		}
	}
}

// A restart must serve the same certificate, or every client that was told to
// trust it would have to be told again.
func TestTLSReusesTheCertificateAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	first := startTLS(t, dir)
	before, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("reading the certificate: %v", err)
	}
	first.Shutdown(context.Background())

	second := startTLS(t, dir)
	after, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("reading the certificate: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the certificate was regenerated on restart")
	}
	resp, err := trusting(t, certFile).Get(second.URL() + "/")
	if err != nil {
		t.Fatalf("GET / after restart: %v", err)
	}
	resp.Body.Close()
}

// A client still configured for plain HTTP has to be told why it failed - the
// one thing that tells a Sonarr operator to tick "Use SSL".
func TestTLSAnswersPlainHTTPWithAnError(t *testing.T) {
	srv := startTLS(t, t.TempDir())
	resp, err := http.Get("http://" + srv.Addr().String() + "/")
	if err != nil {
		t.Fatalf("plain GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("plain HTTP to the TLS listener = %d, want 400", resp.StatusCode)
	}
}

// Half a key pair may be half of an operator's own certificate; generating a
// new pair would overwrite the half that is left.
func TestTLSRefusesHalfAKeyPair(t *testing.T) {
	for _, present := range []string{"tls.crt", "tls.key"} {
		t.Run(present, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, present)
			if err := os.WriteFile(path, []byte("operator's own"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := config.Defaults()
			cfg.Torrent.SavePath = t.TempDir()
			cfg.API.ListenPort = 0
			cfg.API.TLS = true
			if _, err := New(&cfg, filepath.Join(dir, "config.toml"), &stubEngine{}); err == nil {
				t.Fatal("New succeeded with half a key pair")
			}
			if got, _ := os.ReadFile(path); string(got) != "operator's own" {
				t.Errorf("%s was overwritten: %q", present, got)
			}
		})
	}
}

// A key someone else can read is no longer a secret, and a warning at startup
// is the only place an operator will hear about it.
func TestTLSWarnsAboutAReadableKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows permission bits say nothing about its ACLs")
	}
	dir := t.TempDir()
	startTLS(t, dir).Shutdown(context.Background())
	if err := os.Chmod(filepath.Join(dir, "tls.key"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	startTLS(t, dir)
	if !strings.Contains(logs.String(), "the API key is readable by other users") {
		t.Errorf("no warning for a world-readable key; logs:\n%s", logs.String())
	}
}

// The logged URLs are what an operator copies into a browser or a client, so
// the scheme has to follow the config and a wildcard bind has to be reachable.
func TestBaseURL(t *testing.T) {
	for _, tc := range []struct {
		addr string
		tls  bool
		want string
	}{
		{"127.0.0.1:6800", false, "http://127.0.0.1:6800"},
		{"127.0.0.1:6800", true, "https://127.0.0.1:6800"},
		{"0.0.0.0:6800", true, "https://localhost:6800"},
		{"[::]:6800", false, "http://localhost:6800"},
		{"[::1]:6800", false, "http://[::1]:6800"},
		{"192.168.1.10:6800", true, "https://192.168.1.10:6800"},
		{"[fe80::1%eth0]:6800", true, "https://[fe80::1%25eth0]:6800"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			got := baseURL(tc.tls, netip.MustParseAddrPort(tc.addr))
			if got != tc.want {
				t.Errorf("baseURL(%v, %s) = %s, want %s", tc.tls, tc.addr, got, tc.want)
			}
			// What is logged has to survive a client parsing it back.
			if _, err := url.Parse(got); err != nil {
				t.Errorf("baseURL(%v, %s) = %s, which does not parse: %v", tc.tls, tc.addr, got, err)
			}
		})
	}
}

// Every address the listener can be reached at has to be one the certificate
// names, or a client checking it rejects the connection for a name mismatch.
func TestCertHostsNameEveryReachableAddress(t *testing.T) {
	interfaces := func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
			&net.IPNet{IP: net.ParseIP("192.168.1.10"), Mask: net.CIDRMask(24, 32)},
			&net.IPNet{IP: net.ParseIP("fd00::10"), Mask: net.CIDRMask(64, 128)},
		}, nil
	}
	for _, tc := range []struct {
		listen  string
		want    []string
		notWant []string
	}{
		{listen: "0.0.0.0", want: []string{"192.168.1.10", "fd00::10"}},
		{listen: "::", want: []string{"192.168.1.10", "fd00::10"}},
		{listen: "127.0.0.2", want: []string{"127.0.0.2"}, notWant: []string{"192.168.1.10"}},
		{listen: "192.168.1.10", want: []string{"192.168.1.10"}, notWant: []string{"fd00::10"}},
		{listen: "fe80::1%eth0", want: []string{"fe80::1%eth0"}},
	} {
		t.Run(tc.listen, func(t *testing.T) {
			hosts := certHosts(netip.MustParseAddr(tc.listen), interfaces)
			for _, want := range append([]string{"localhost", "127.0.0.1", "::1"}, tc.want...) {
				if !slices.Contains(hosts, want) {
					t.Errorf("certHosts(%s) = %v, missing %s", tc.listen, hosts, want)
				}
			}
			for _, notWant := range tc.notWant {
				if slices.Contains(hosts, notWant) {
					t.Errorf("certHosts(%s) = %v, should not name %s", tc.listen, hosts, notWant)
				}
			}
			seen := map[string]bool{}
			for _, h := range hosts {
				if seen[h] {
					t.Errorf("certHosts(%s) = %v, names %s twice", tc.listen, hosts, h)
				}
				seen[h] = true
			}
		})
	}
}

// The names have to land where a verifier looks: an IP in IPAddresses, which
// for a scoped address means without its zone, since Go strips the client's.
func TestGeneratedCertificateVerifiesForItsHosts(t *testing.T) {
	hosts := []string{"localhost", "127.0.0.2", "fe80::1%eth0", "192.168.1.10"}
	certPEM, keyPEM, err := generateCertificate(hosts, time.Now())
	if err != nil {
		t.Fatalf("generateCertificate: %v", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	for _, h := range hosts {
		if err := cert.Leaf.VerifyHostname(h); err != nil {
			t.Errorf("VerifyHostname(%s): %v", h, err)
		}
	}
	if slices.Contains(cert.Leaf.DNSNames, "fe80::1%eth0") {
		t.Error("the scoped address was filed as a DNS name")
	}
}
