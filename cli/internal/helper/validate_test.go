package helper

import "testing"

func TestValidHostname(t *testing.T) {
	valid := []string{"dev", "app.dev", "api.pratizo.dev", "a-b.c-d.dev", "localhost"}
	for _, h := range valid {
		if !ValidHostname(h) {
			t.Errorf("ValidHostname(%q) = false, want true", h)
		}
	}
	invalid := []string{"", "-bad.dev", "bad-.dev", "UPPER.dev", "sp ace.dev", "a/b.dev", "..", "a..b"}
	for _, h := range invalid {
		if ValidHostname(h) {
			t.Errorf("ValidHostname(%q) = true, want false", h)
		}
	}
}

func TestValidateHostsAcceptsRegisteredTLD(t *testing.T) {
	hosts := []string{"roxey.dev", "app.dev", "api.pratizo.dev", "dev"}
	if err := ValidateHosts(hosts, []string{"dev", "pratizo.dev"}); err != nil {
		t.Fatalf("ValidateHosts = %v, want nil", err)
	}
}

func TestValidateHostsRejectsForeignHost(t *testing.T) {
	cases := [][]string{
		{"evil.com"},
		{"app.example.test"}, // not under a registered TLD
		{"../etc/passwd"},
		{"app.dev\n127.0.0.1 evil"},
		{"APP.dev"},
	}
	for _, hosts := range cases {
		if err := ValidateHosts(hosts, []string{"dev"}); err == nil {
			t.Errorf("ValidateHosts(%q) = nil, want error", hosts)
		}
	}
}

func TestValidateHostsLocalhostOnlyWhenAllowed(t *testing.T) {
	if err := ValidateHosts([]string{"localhost"}, []string{"localhost"}); err != nil {
		t.Fatalf("localhost should be allowed for localhost TLD: %v", err)
	}
	if err := ValidateHosts([]string{"localhost"}, []string{"dev"}); err == nil {
		t.Fatal("localhost should be rejected when not a registered TLD")
	}
}

func TestValidateHostsEmptyIsAllowed(t *testing.T) {
	if err := ValidateHosts(nil, []string{"dev"}); err != nil {
		t.Fatalf("empty host list should be allowed: %v", err)
	}
}
