package preview

import (
	"os"
	"testing"

	"roxey/internal/manifest"
)

const yamlBase = `
relay_server: {tld: dev}
environments:
  - host: app.verifycate
    routes:
      "/":
        command: pnpm dev
        environment:
          NEXT_PUBLIC_API_URL: https://{{host:api.verifycate}}/api/v1
  - host: api.verifycate
    routes:
      "/": localhost:8000
`

func loadManifest(t *testing.T, content string) (*manifest.Manifest, string) {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/roxey.yaml"
	if err := writeFile(path, []byte(content)); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return m, path
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"issue-012/auth-fix": "issue-012-auth-fix",
		"Feature_Big UI":     "feature-big-ui",
		"---weird---":        "weird",
		"":                   "preview",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
	long := Slugify(stringsRepeat("a", 80))
	if len(long) != 47 || long[:40] != stringsRepeat("a", 40) || long[40] != '-' {
		t.Errorf("long slug not truncated+hashed: %q", long)
	}
	if Slugify(stringsRepeat("a", 80)) == Slugify(stringsRepeat("a", 81)) {
		t.Error("distinct long inputs must hash differently")
	}
}

func TestApplyFlattensHostsAndKeepsTemplates(t *testing.T) {
	m, _ := loadManifest(t, yamlBase)

	name := Apply(m, "verifycate", "fix-13")
	if name != "verifycate--fix-13" {
		t.Fatalf("registry name = %q", name)
	}
	app := m.Environments[0].Host
	api := m.Environments[1].Host
	if app != "fix-13-app-verifycate" || api != "fix-13-api-verifycate" {
		t.Fatalf("hosts not flattened: %q %q", app, api)
	}

	if err := m.ResolveEnvTemplates(false); err != nil {
		t.Fatal(err)
	}
	want := "https://" + api + ".dev/api/v1"
	got := m.Environments[0].Routes[0].Environment["NEXT_PUBLIC_API_URL"]
	if got != want {
		t.Fatalf("template = %q, want %q", got, want)
	}

	// Main-checkout resolution uses original names.
	m2, _ := loadManifest(t, yamlBase)
	if err := m2.ResolveEnvTemplates(false); err != nil {
		t.Fatal(err)
	}
	if got := m2.Environments[0].Routes[0].Environment["NEXT_PUBLIC_API_URL"]; got != "https://api.verifycate.dev/api/v1" {
		t.Fatalf("main template = %q", got)
	}
}

func TestDetachedSlugUsesWorktreePath(t *testing.T) {
	one := detachedSlug("abc1234", "/tmp/repo/worktrees/one")
	two := detachedSlug("abc1234", "/tmp/repo/worktrees/two")
	if one == two {
		t.Fatalf("detached worktrees at the same commit got the same slug %q", one)
	}
	if one != detachedSlug("abc1234", "/tmp/repo/worktrees/one") {
		t.Fatal("detached slug is not stable")
	}
}

func TestPathSlugIsStablePerCheckout(t *testing.T) {
	one := PathSlug("verifycate", "/tmp/worktrees/one")
	if one != PathSlug("verifycate", "/tmp/worktrees/one") {
		t.Fatal("path slug is not stable")
	}
	if one == PathSlug("verifycate", "/tmp/worktrees/two") {
		t.Fatalf("different checkouts got the same path slug %q", one)
	}
}

func TestAssignPortsRemapsPreviewRunsAndLocalAliases(t *testing.T) {
	m, _ := loadManifest(t, `
relay_server: {tld: dev}
environments:
  - host: portal
    routes:
      "/": localhost:3001
      "/api": 127.0.0.1:4000
  - host: web
    routes:
      "/": {command: web, port: 3001}
  - host: api
    routes:
      "/": {command: api, port: 4000}
  - host: worker
    routes:
      "/": {command: worker}
`)
	ports := []int{5100, 5100, 5101, 5102}
	next := 0
	allocate := func() (int, error) {
		port := ports[next]
		next++
		return port, nil
	}
	if err := AssignPorts(m, true, allocate); err != nil {
		t.Fatal(err)
	}
	if got := m.Environments[1].Routes[0].Port; got != 5100 {
		t.Fatalf("web port = %d", got)
	}
	if got := m.Environments[2].Routes[0].Port; got != 5101 {
		t.Fatalf("api port = %d", got)
	}
	if got := m.Environments[3].Routes[0].Port; got != 5102 {
		t.Fatalf("worker port = %d", got)
	}
	if got := m.Environments[0].Routes[0].Target; got != "localhost:5100" {
		t.Fatalf("localhost alias = %q", got)
	}
	if got := m.Environments[0].Routes[1].Target; got != "127.0.0.1:5101" {
		t.Fatalf("127.0.0.1 alias = %q", got)
	}
}

func TestAssignPortsKeepsDeclaredPortsOutsidePreview(t *testing.T) {
	m, _ := loadManifest(t, `
relay_server: {tld: dev}
environments:
  - host: web
    routes:
      "/": {command: web, port: 3000}
  - host: worker
    routes:
      "/": {command: worker}
`)
	ports := []int{3000, 5200}
	next := 0
	if err := AssignPorts(m, false, func() (int, error) {
		port := ports[next]
		next++
		return port, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := m.Environments[0].Routes[0].Port; got != 3000 {
		t.Fatalf("declared port changed to %d", got)
	}
	if got := m.Environments[1].Routes[0].Port; got != 5200 {
		t.Fatalf("auto port = %d", got)
	}
}

func stringsRepeat(s string, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s[0])
	}
	return string(out)
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0644)
}
