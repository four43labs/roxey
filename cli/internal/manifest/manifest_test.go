package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "roxey.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadRemoteProxyAndRunRoutes(t *testing.T) {
	path := write(t, `
relay_server:
  tld: f43.run
environments:
  - host: shop
    routes:
      "/": website:3001
      "/api": localhost:8000
      "/shop":
        command: npm run dev
        cwd: webapp
        port: 3000
        environment:
          NODE_ENV: development
`)
	if err := os.MkdirAll(filepath.Join(filepath.Dir(path), "webapp"), 0755); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.RelayServer.TLD != "f43.run" || m.RelayServer.Local {
		t.Fatalf("bad relay_server: %+v", m.RelayServer)
	}
	env := m.Environments[0]
	if env.Host != "shop" || len(env.Routes) != 3 {
		t.Fatalf("bad env: %+v", env)
	}
	if env.Routes[0].Path != "/" || env.Routes[0].Target != "website:3001" || env.Routes[0].IsRun() {
		t.Fatalf("bad root route: %+v", env.Routes[0])
	}
	run := env.Routes[2]
	if !run.IsRun() || run.Command != "npm run dev" || run.Port != 3000 {
		t.Fatalf("bad run route: %+v", run)
	}
	if run.Cwd != filepath.Dir(path)+"/webapp" {
		t.Fatalf("cwd not resolved against manifest dir: %q", run.Cwd)
	}
	if run.Environment["NODE_ENV"] != "development" {
		t.Fatalf("env not decoded: %+v", run.Environment)
	}
}

func TestLocalDefaultsToLocahostTLD(t *testing.T) {
	m, err := Load(write(t, `
relay_server:
  local: true
environments:
  - host: app
    routes:
      "/": localhost:8080
`))
	if err != nil {
		t.Fatal(err)
	}
	if m.RelayServer.TLD != "localhost" {
		t.Fatalf("expected localhost default, got %q", m.RelayServer.TLD)
	}
}

func TestAPIKeyWithLocalRejected(t *testing.T) {
	_, err := Load(write(t, `
relay_server:
  local: true
  api_key: rxy_x
environments:
  - host: a
    routes:
      "/": localhost:1
`))
	if err == nil || !contains(err.Error(), "cannot be combined") {
		t.Fatalf("expected api_key+local error, got %v", err)
	}
}

func TestDuplicateHostAndPathRejected(t *testing.T) {
	_, err := Load(write(t, `
relay_server: {tld: f43.run}
environments:
  - host: a
    routes:
      "/x": localhost:1
      "/x": localhost:2
  - host: a
    routes:
      "/": localhost:3
`))
	// duplicate path within one env is caught by yaml dup-key handling or us;
	// either way it must fail
	if err == nil {
		t.Fatal("expected duplicate host/path error")
	}
}

func TestRunRouteRequiresPort(t *testing.T) {
	_, err := Load(write(t, `
relay_server: {tld: f43.run}
environments:
  - host: a
    routes:
      "/web":
        command: npm run dev
`))
	if err == nil || !contains(err.Error(), "port") {
		t.Fatalf("expected port requirement error, got %v", err)
	}
}

func TestMissingRelayServerRejected(t *testing.T) {
	_, err := Load(write(t, `
environments:
  - host: a
    routes:
      "/": localhost:1
`))
	if err == nil || !contains(err.Error(), "relay_server") {
		t.Fatalf("expected relay_server required error, got %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
