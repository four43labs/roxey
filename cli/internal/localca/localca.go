// Package localca provisions a local certificate authority for local relay
// mode: it generates (once) a CA under ~/.roxey, issues leaf certificates
// covering the hosts a manifest exposes, and installs the CA into the OS
// trust store so browsers and the CLI itself trust https://<host>.<tld>.
package localca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"roxey/internal/config"
)

const (
	caKeyFile  = "ca.key"
	caCertFile = "ca.crt"

	leafKeyFile  = "leaf.key"
	leafCertFile = "leaf.crt"

	orgName   = "roxey local dev CA"
	validity  = 825 * 24 * time.Hour // macOS caps untrusted-exception lifetimes at 825 days
	trustWait = 10 * time.Second
)

// Result bundles what provisioning produced.
type Result struct {
	// Cert is the leaf covering all hosts, ready for ListenAndServeTLS.
	Cert tls.Certificate
	// CAPath is the on-disk CA certificate (for trust installation).
	CAPath string
}

// Ensure returns a TLS leaf certificate covering hosts, creating/trusting
// the backing CA as needed. hostnames must include every name clients use
// (e.g. "relay.dev.f43.run", "shop.localhost", "localhost").
func Ensure(hosts []string) (Result, error) {
	dir := certDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Result{}, err
	}

	caCert, caKey, caPath, err := loadOrCreateCA(dir)
	if err != nil {
		return Result{}, err
	}

	leafPath := filepath.Join(dir, leafCertFile)
	keyPath := filepath.Join(dir, leafKeyFile)

	sorted := append([]string(nil), hosts...)
	sort.Strings(sorted)

	if pair, err := tls.LoadX509KeyPair(leafPath, keyPath); err == nil && covers(pair.Leaf, sorted) {
		return Result{Cert: pair, CAPath: caPath}, nil
	}

	leafDER, keyDER, err := issueLeaf(caCert, caKey, sorted)
	if err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(leafPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0644); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		return Result{}, err
	}

	pair, err := tls.LoadX509KeyPair(leafPath, keyPath)
	if err != nil {
		return Result{}, err
	}
	return Result{Cert: pair, CAPath: caPath}, nil
}

func certDir() string {
	return config.CertDir()
}

// covers reports whether the parsed leaf includes every hostname.
func covers(leaf *x509.Certificate, hosts []string) bool {
	if leaf == nil {
		return false
	}
	have := map[string]bool{}
	for _, h := range leaf.DNSNames {
		have[h] = true
	}
	for _, ip := range leaf.IPAddresses {
		have[ip.String()] = true
	}
	for _, h := range hosts {
		if !have[h] {
			return false
		}
	}
	return true
}

func loadOrCreateCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, string, error) {
	certPath := filepath.Join(dir, caCertFile)
	keyPath := filepath.Join(dir, caKeyFile)

	if pair, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil && pair.Leaf != nil &&
		time.Now().Before(pair.Leaf.NotAfter) {
		if key, ok := pair.PrivateKey.(*ecdsa.PrivateKey); ok {
			return pair.Leaf, key, certPath, nil
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{Organization: []string{orgName}, CommonName: "roxey-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, "", err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, "", err
	}
	if err := writePEM(certPath, der); err != nil {
		return nil, nil, "", err
	}
	if err := writeKey(keyPath, key); err != nil {
		return nil, nil, "", err
	}
	return cert, key, certPath, nil
}

func issueLeaf(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, hosts []string) (leafDER, keyDER []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{Organization: []string{orgName}, CommonName: hosts[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames(hosts),
		IPAddresses:  ipAddrs(hosts),
	}
	leafDER, err = x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err = x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return leafDER, keyDER, nil
}

// Trusted reports whether certificates signed by our CA verify against the
// system trust store. It verifies the issued leaf (not the CA itself —
// self-signed roots fail self-verification quirks) against the OS pool.
func Trusted(caPath string) bool {
	leafPath := filepath.Join(filepath.Dir(caPath), leafCertFile)
	leafPEM, err := os.ReadFile(leafPath)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(leafPEM)
	if block == nil {
		return false
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return false
	}
	opts := x509.VerifyOptions{Roots: roots} // no DNSName: any covered host proves trust
	_, err = leaf.Verify(opts)
	return err == nil
}

// TrustCA installs the CA into the system trust store. It runs the command
// directly when already root (the helper daemon) and elevates with sudo
// otherwise.
func TrustCA(caPath string) error {
	switch runtime.GOOS {
	case "darwin":
		return runTrust("security", "add-trusted-cert", "-d", "-r", "trustRoot",
			"-k", "/Library/Keychains/System.keychain", caPath)
	case "linux":
		if _, err := os.Stat("/usr/bin/update-ca-certificates"); err == nil {
			dst := "/usr/local/share/ca-certificates/roxey-local-ca.crt"
			if err := runTrust("cp", caPath, dst); err != nil {
				return fmt.Errorf("copy CA: %w", err)
			}
			if err := runTrust("update-ca-certificates"); err != nil {
				return fmt.Errorf("update-ca-certificates: %w", err)
			}
			return nil
		}
		dst := "/etc/pki/ca-trust/source/anchors/roxey-local-ca.crt"
		if err := runTrust("cp", caPath, dst); err != nil {
			return fmt.Errorf("copy CA: %w", err)
		}
		if err := runTrust("update-ca-trust", "extract"); err != nil {
			return fmt.Errorf("update-ca-trust: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported OS %s: add %s to your trust store manually", runtime.GOOS, caPath)
	}
}

// runTrust executes a trust-store command, prepending sudo only when not root.
func runTrust(name string, args ...string) error {
	if os.Geteuid() == 0 {
		cmd := exec.Command(name, args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd.Run()
	}
	cmd := exec.Command("sudo", append([]string{"-p", "roxey needs admin rights to trust its local CA: ", name}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func dnsNames(hosts []string) []string {
	var out []string
	for _, h := range hosts {
		if !isIP(h) {
			out = append(out, h)
		}
	}
	return out
}

func ipAddrs(hosts []string) []net.IP {
	var out []net.IP
	for _, h := range hosts {
		if isIP(h) {
			out = append(out, net.ParseIP(h))
		}
	}
	return out
}

func isIP(s string) bool { return net.ParseIP(s) != nil }

func randomSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		panic(err)
	}
	return n
}

func writePEM(path string, der []byte) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644)
}

func writeKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0600)
}
