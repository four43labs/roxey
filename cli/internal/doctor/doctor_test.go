package doctor

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"

	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeTarget(t *testing.T) {
	cases := map[string]string{
		"localhost:8000": "localhost:8000",
		"website:3001":   "website:3001",
		"http://x:80":    "x:80",
		"https://y:443":  "y:443",
		"3000":           "localhost:3000", // bare port
	}
	for in, want := range cases {
		if got := normalizeTarget(in); got != want {
			t.Errorf("normalizeTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseManagedHostsTokenizesAliases(t *testing.T) {
	block := strings.Join([]string{
		"127.0.0.1 localhost",
		"",
		"# BEGIN roxey (managed by `roxey up`; do not edit)",
		"127.0.0.1 api.pixinoid.dev roxey.dev",
		"127.0.0.1 jobs.pixinoid.dev",
		"# END roxey",
	}, "\n")
	hosts, maxAliases, err := parseManagedHosts(strings.NewReader(block))
	if err != nil {
		t.Fatal(err)
	}
	if hosts["pixinoid.dev"] {
		t.Fatal("bare pixinoid.dev matched a longer alias")
	}
	for _, want := range []string{"api.pixinoid.dev", "roxey.dev", "jobs.pixinoid.dev"} {
		if !hosts[want] {
			t.Errorf("missing host %q in %v", want, hosts)
		}
	}
	if maxAliases != 2 {
		t.Fatalf("maxAliases = %d, want 2", maxAliases)
	}
}

func TestParseManagedHostsReportsLongLines(t *testing.T) {
	var b strings.Builder
	b.WriteString("# BEGIN roxey (managed by `roxey up`; do not edit)\n")
	b.WriteString("127.0.0.1")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, " host-%d.dev", i)
	}
	b.WriteString("\n# END roxey\n")
	_, maxAliases, err := parseManagedHosts(strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if maxAliases != 40 {
		t.Fatalf("maxAliases = %d, want 40", maxAliases)
	}
}

func TestCheckManifestRoutes(t *testing.T) {
	existing := t.TempDir()

	// Occupied port owned by someone else.
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer foreign.Close()
	l := foreign.Listener.Addr().(*net.TCPAddr)
	foreignPort := l.Port

	// Reachable proxy target.
	reachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer reachable.Close()
	rport := reachable.Listener.Addr().(*net.TCPAddr).Port

	mi := ManifestInfo{
		TLD:             "test.local",
		Local:           true,
		HasAPIKey:       true,
		SkipRelayChecks: true,
		RunRoutes: []RunRoute{
			{Name: "good", Cwd: existing, Command: "go version", Port: freePort(t)},
			{Name: "badcwd", Cwd: filepath.Join(existing, "nope"), Command: "go version", Port: freePort(t)},
			{Name: "badcmd", Cwd: existing, Command: "definitely-not-a-real-binary-xyz", Port: freePort(t)},
			{Name: "busy", Cwd: existing, Command: "go version", Port: foreignPort},
			{Name: "shell", Cwd: existing, Command: `bash -c 'exit 0'`, Port: freePort(t)},
		},
		ProxyTargs: []string{fmt.Sprintf("127.0.0.1:%d", rport), "127.0.0.1:1"},
	}
	rep := &Report{}
	CheckManifest(rep, mi)

	var fails, warns []string
	for _, res := range rep.Results {
		switch res.Status {
		case Fail:
			fails = append(fails, res.Msg)
		case Warn:
			warns = append(warns, res.Msg)
		}
	}

	expectFail := func(substr string) {
		t.Helper()
		for _, f := range fails {
			if contains(f, substr) {
				return
			}
		}
		t.Errorf("expected failure containing %q; got fails=%q", substr, fails)
	}
	expectFail("badcwd")
	expectFail("badcmd")
	expectFail("already bound")
	if len(warns) != 1 { // only the unreachable proxy target should warn
		t.Errorf("expected exactly 1 warn (dead proxy target), got %d: %q", len(warns), warns)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
