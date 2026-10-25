package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"os"
	"runtime"
	"time"
)

// certValidity is what Apple platforms accept for a TLS server certificate at
// most, trusted by hand or not. Longer and Safari refuses the certificate
// outright, even for an operator who has added it to their keychain.
const certValidity = 825 * 24 * time.Hour

// loadOrCreateCertificate loads the API listener's key pair, generating a
// self-signed one when neither file exists yet.
//
// Exactly one of the two existing is an error rather than a cue to regenerate:
// the files may be an operator's own certificate, and replacing half of it
// would destroy the half that is still there.
func loadOrCreateCertificate(certFile, keyFile string, hosts []string) (tls.Certificate, error) {
	certExists, err := exists(certFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyExists, err := exists(keyFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	switch {
	case certExists && keyExists:
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("failed to load the API certificate %s: %w", certFile, err)
		}
		warnIfKeyReadable(keyFile)
		if cert.Leaf != nil && time.Now().After(cert.Leaf.NotAfter) {
			slog.Warn("the API certificate has expired; delete it and its key to generate a new one",
				"cert", certFile, "key", keyFile, "expired", cert.Leaf.NotAfter)
		}
		return cert, nil
	case certExists:
		return tls.Certificate{}, fmt.Errorf("the API certificate %s exists but its key %s does not; "+
			"restore the key, or delete the certificate to generate a new pair", certFile, keyFile)
	case keyExists:
		return tls.Certificate{}, fmt.Errorf("the API key %s exists but its certificate %s does not; "+
			"restore the certificate, or delete the key to generate a new pair", keyFile, certFile)
	}

	certPEM, keyPEM, err := generateCertificate(hosts, time.Now())
	if err != nil {
		return tls.Certificate{}, err
	}
	// The key first, so a failure on the certificate can take it back out and
	// leave neither file rather than the half-pair refused above.
	if err := writeNew(keyFile, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := writeNew(certFile, certPEM, 0o644); err != nil {
		os.Remove(keyFile)
		return tls.Certificate{}, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("failed to load the generated API certificate: %w", err)
	}
	sum := sha256.Sum256(cert.Certificate[0])
	slog.Info("generated a self-signed certificate for the API listener",
		"cert", certFile, "hosts", hosts, "sha256", hex.EncodeToString(sum[:]))
	return cert, nil
}

// generateCertificate returns a PEM certificate and PKCS#8 key for hosts,
// each of which is an IP literal or a DNS name.
func generateCertificate(hosts []string, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate the API certificate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate the API certificate serial number: %w", err)
	}
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "BitTrench"},
		// Backdated, so a client whose clock runs a little behind does not
		// reject a certificate generated moments ago as not yet valid.
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create the API certificate: %w", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to encode the API certificate key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), nil
}

// certHosts is what the generated certificate names: loopback under both
// spellings, this machine's hostname, and the listen address when it is a
// specific one. A wildcard bind has no single address to name, and a
// container's own address is one no client outside it uses anyway.
func certHosts(listen netip.Addr) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if name, err := os.Hostname(); err == nil && name != "" && name != "localhost" {
		hosts = append(hosts, name)
	}
	if !listen.IsUnspecified() && !listen.IsLoopback() {
		hosts = append(hosts, listen.String())
	}
	return hosts
}

// warnIfKeyReadable warns about a key that anyone but its owner can read, as
// ssh does. Only a warning: an operator's own key may be group-readable on
// purpose, for a certificate manager that renews it. Windows reports
// permission bits that say nothing about its ACLs, so it is not checked.
func warnIfKeyReadable(keyFile string) {
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(keyFile)
	if err != nil {
		return
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		slog.Warn("the API key is readable by other users; chmod 600 it",
			"key", keyFile, "mode", fmt.Sprintf("%o", perm))
	}
}

// exists reports whether path exists, treating any error other than its
// absence as a failure rather than as "no".
func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("failed to check for the API certificate file %s: %w", path, err)
	}
}

// writeNew creates path and refuses to overwrite it: a file that appeared since
// the check above is someone else's, not ours to replace.
func writeNew(path string, data []byte, perm fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("failed to create the API certificate file %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("failed to write the API certificate file %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("failed to write the API certificate file %s: %w", path, err)
	}
	return nil
}
