package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
