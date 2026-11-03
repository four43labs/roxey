package main

import (
	"strings"
	"testing"

	"github.com/four43labs/roxey/cli/manifest"
	"github.com/four43labs/roxey/cli/preview"
)

func TestParseUpOptionsOnlineProtectAndPreview(t *testing.T) {
	opts, err := parseUpOptions([]string{"-d", "--online", "--preview=review-12", "--protect", "swordfish", "custom.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.detach || !opts.online || !opts.preview || opts.previewSlug != "review-12" || opts.protect != "swordfish" || opts.manifestFile != "custom.yaml" {
		t.Fatalf("unexpected options: %+v", opts)
	}

	opts, err = parseUpOptions([]string{"--protect=hunter2"})
	if err != nil || opts.protect != "hunter2" {
		t.Fatalf("equals protect: %+v, %v", opts, err)
	}
}

func TestParseUpOptionsRejectsMissingProtectSecret(t *testing.T) {
	for _, args := range [][]string{{"--protect"}, {"--protect="}, {"--protect", "--online"}} {
		if _, err := parseUpOptions(args); err == nil || !strings.Contains(err.Error(), "secret") {
			t.Fatalf("parseUpOptions(%v) error = %v", args, err)
		}
	}
}

func TestPreviewModeFollowsRelayShape(t *testing.T) {
	local := &manifest.Manifest{RelayServer: manifest.RelayServer{Local: true}}
	if got := previewMode(local); got != preview.ModeSubdomain {
		t.Fatalf("local relay preview mode = %v, want ModeSubdomain", got)
	}
	hosted := &manifest.Manifest{RelayServer: manifest.RelayServer{TLD: "f43.run"}}
	if got := previewMode(hosted); got != preview.ModeFlatten {
		t.Fatalf("hosted relay preview mode = %v, want ModeFlatten", got)
	}
}

func TestGateGroupsAreOpaqueAndUnique(t *testing.T) {
	one, err := newGateGroup()
	if err != nil {
		t.Fatal(err)
	}
	two, err := newGateGroup()
	if err != nil {
		t.Fatal(err)
	}
	if one == two || !strings.HasPrefix(one, "up_") || len(one) != 35 {
		t.Fatalf("unexpected gate groups %q %q", one, two)
	}
}
