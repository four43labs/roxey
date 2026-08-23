// Package localrelay manages a relay backend running on this machine for
// `relay_server.local` manifests: it downloads the prebuilt roxey-relay
// binary from GitHub Releases, provisions TLS certs (via internal/localca),
// spawns the relay as root on port 443, and auto-creates an API key through
// its admin API.
package localrelay

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"roxey/internal/config"
)

const (
	githubRepo  = "four43labs/roxey"
	binaryName  = "roxey-relay"
	listenPort  = "443"
	spawnExpiry = 30 * time.Second
)

// BinPath returns where the relay binary lives (or will live).
func BinPath() string { return filepath.Join(config.BinDir(), binaryName) }

// HealthOK probes https://relay.<tld>/healthz through our CA without
// relying on the OS trust store.
func HealthOK(tld string) bool {
	client, err := healthClient(tld)
	if err != nil {
		return false
	}
	req, err := http.NewRequest(http.MethodGet, "https://127.0.0.1/healthz", nil)
	if err != nil {
		return false
	}
	req.Host = RelayHost(tld)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func RelayHost(tld string) string { return "relay." + tld }

// healthClient builds an HTTP client that dials 127.0.0.1 but presents SNI
// for relay.<tld> and verifies against our local CA.
func healthClient(tld string) (*http.Client, error) {
	caPEM, err := os.ReadFile(filepath.Join(config.CertDir(), "ca.crt"))
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse local CA")
	}
	tlsCfg := &tls.Config{RootCAs: roots, ServerName: RelayHost(tld)}
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", listenPort))
			},
		},
	}, nil
}

// EnsureBinary downloads the prebuilt relay from the latest GitHub release
// unless it already exists locally (or ROXEY_RELAY_BIN overrides it).
func EnsureBinary() (string, error) {
	if p := os.Getenv("ROXEY_RELAY_BIN"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("ROXEY_RELAY_BIN=%s does not exist", p)
	}
	dst := BinPath()
	if st, err := os.Stat(dst); err == nil && st.Mode().IsRegular() && st.Mode()&0111 != 0 {
		return dst, nil
	}

	asset := fmt.Sprintf("%s_%s_%s.tar.gz", binaryName, runtime.GOOS, runtime.GOARCH)
	url, err := latestReleaseAsset(asset)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(config.BinDir(), 0700); err != nil {
		return "", err
	}
	tmp := dst + ".download"
	if err := downloadExtract(url, tmp); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	if err := os.Chmod(dst, 0755); err != nil {
		return "", err
	}
	return dst, nil
}

func latestReleaseAsset(name string) (string, error) {
	req, _ := http.NewRequest(http.MethodGet,
		"https://api.github.com/repos/"+githubRepo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github releases: %s", resp.Status)
	}
	var rel struct {
		Assets []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.BrowserDownloadURL, nil
		}
	}
	return "", fmt.Errorf("release asset %s not found; is your platform supported?", name)
}

func downloadExtract(url, dstFile string) error {
	resp, err := http.Get(url) //nolint:gosec // URL comes from the GitHub API
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("binary not found in archive")
		}
		if err != nil {
			return err
		}
		if filepath.Base(hdr.Name) == binaryName {
			out, err := os.OpenFile(dstFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
			if err != nil {
				return err
			}
			defer out.Close()
			_, err = io.Copy(out, tr)
			return err
		}
	}
}

// EnsureRunning starts the relay as root on port 443 if it isn't healthy
// already, using cert/key files previously written by localca.Ensure.
func EnsureRunning(tld string) error {
	if HealthOK(tld) {
		fmt.Printf("[relay] reusing running relay at %s\n", RelayHost(tld))
		return nil
	}

	// If something else owns port 443 we can't start ours.
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+listenPort, time.Second); err == nil {
		conn.Close()
		return fmt.Errorf("port %s is in use by another process; stop it or point relay_server at a remote relay", listenPort)
	}

	bin, err := EnsureBinary()
	if err != nil {
		return fmt.Errorf("fetching relay binary: %w", err)
	}

	sc, _ := config.ServerFor(tld)
	adminUser, adminPass := sc.AdminUser, sc.AdminPass
	if adminUser == "" || adminPass == "" {
		adminUser, adminPass = randomToken(), randomToken()
	}

	certDir := config.CertDir()
	envs := []string{
		"ROXEY_DOMAIN=" + tld,
		"ROXEY_ADMIN_HOST=" + RelayHost(tld),
		"ROXEY_ADMIN_USER=" + adminUser,
		"ROXEY_ADMIN_PASS=" + adminPass,
		"ROXEY_DB_PATH=" + filepath.Join(config.Dir(), "local_"+strings.ReplaceAll(tld, ".", "_")+".db"),
		"PORT=" + listenPort,
		"ROXEY_TLS_CERT=" + filepath.Join(certDir, "leaf.crt"),
		"ROXEY_TLS_KEY=" + filepath.Join(certDir, "leaf.key"),
	}

	fmt.Println("[relay] starting local relay (sudo required to bind port 443)...")
	args := append([]string{"-b", "env"}, envs...)
	args = append(args, bin)
	cmd := exec.Command("sudo", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("spawn relay via sudo: %w", err)
	}

	deadline := time.Now().Add(spawnExpiry)
	for time.Now().Before(deadline) {
		if HealthOK(tld) {
			if err := config.SetServer(tld, config.ServerConfig{
				Local: true, AdminUser: adminUser, AdminPass: adminPass, APIKey: sc.APIKey,
			}); err != nil {
				return err
			}
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("local relay did not become healthy within %s (check sudo output)", spawnExpiry)
}

// CreateAPIKey creates a fresh key via the relay's Basic-Auth admin API and
// returns its plaintext (shown once).
func CreateAPIKey(tld, label string) (string, error) {
	sc, ok := config.ServerFor(tld)
	if !ok || sc.AdminUser == "" || sc.AdminPass == "" {
		return "", fmt.Errorf("no admin credentials saved for %s", tld)
	}
	client, err := healthClient(tld)
	if err != nil {
		return "", err
	}
	body := strings.NewReader(`{"label":` + mustJSON(label) + `}`)
	req, err := http.NewRequest(http.MethodPost, "https://127.0.0.1/api/keys", body)
	if err != nil {
		return "", err
	}
	req.Host = RelayHost(tld)
	req.SetBasicAuth(sc.AdminUser, sc.AdminPass)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf("create api key: %s: %s", resp.Status, raw)
	}
	var out struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.APIKey == "" {
		return "", fmt.Errorf("relay returned empty api key")
	}
	return out.APIKey, nil
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func randomToken() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 24)
	now := time.Now().UnixNano()
	for i := range b {
		now = now*6364136223846793005 + 1442695040888963407
		b[i] = charset[int(now>>33)%len(charset)]
	}
	return string(b)
}
