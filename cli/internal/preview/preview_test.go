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

	m.ResolveEnvTemplates()
	want := "https://" + api + ".dev/api/v1"
	got := m.Environments[0].Routes[0].Environment["NEXT_PUBLIC_API_URL"]
	if got != want {
		t.Fatalf("template = %q, want %q", got, want)
	}

	// Main-checkout resolution uses original names.
	m2, _ := loadManifest(t, yamlBase)
	m2.ResolveEnvTemplates()
	if got := m2.Environments[0].Routes[0].Environment["NEXT_PUBLIC_API_URL"]; got != "https://api.verifycate.dev/api/v1" {
		t.Fatalf("main template = %q", got)
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
