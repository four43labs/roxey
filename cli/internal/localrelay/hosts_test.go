package localrelay

import (
	"fmt"
	"strings"
	"testing"
)

func TestMergeHostsBlockIsIdempotent(t *testing.T) {
	block := RenderBlock([]string{"app.example.dev", "roxey.dev"})
	existing := "127.0.0.1 localhost\n\n" + block

	if got := mergeHostsBlock(existing, block); got != existing {
		t.Fatalf("unchanged hosts file was rewritten:\n%q\nwant:\n%q", got, existing)
	}
}

func TestMergeHostsBlockReplacesManagedEntries(t *testing.T) {
	existing := "127.0.0.1 localhost\n\n" + hostsBegin +
		"\n127.0.0.1 old.example.dev\n" + hostsEnd + "\n"
	block := RenderBlock([]string{"app.example.dev", "roxey.dev"})
	want := "127.0.0.1 localhost\n\n" + block

	if got := mergeHostsBlock(existing, block); got != want {
		t.Fatalf("managed hosts block =\n%q\nwant:\n%q", got, want)
	}
}

func TestMergeHostsBlockReplacesLegacyLongLine(t *testing.T) {
	entries := manyHosts(130)
	legacy := hostsBegin + "\n127.0.0.1 " + strings.Join(entries, " ") + "\n" + hostsEnd + "\n"
	existing := "127.0.0.1 localhost\n\n" + legacy
	block := RenderBlock(entries)
	want := "127.0.0.1 localhost\n\n" + block

	if got := mergeHostsBlock(existing, block); got != want {
		t.Fatalf("legacy long line was not replaced:\n%q\nwant:\n%q", got, want)
	}
}

func TestRenderBlock(t *testing.T) {
	if got := RenderBlock(nil); got != "" {
		t.Fatalf("RenderBlock(nil) = %q, want empty", got)
	}
	if got := RenderBlock([]string{"", ""}); got != "" {
		t.Fatalf("RenderBlock(empty entries) = %q, want empty", got)
	}
	got := RenderBlock([]string{"app.dev", "roxey.dev"})
	want := hostsBegin + "\n127.0.0.1 app.dev\n127.0.0.1 roxey.dev\n" + hostsEnd + "\n"
	if got != want {
		t.Fatalf("RenderBlock = %q, want %q", got, want)
	}
}

func TestRenderBlockDedupesEntries(t *testing.T) {
	got := RenderBlock([]string{"app.dev", "app.dev", "roxey.dev"})
	want := hostsBegin + "\n127.0.0.1 app.dev\n127.0.0.1 roxey.dev\n" + hostsEnd + "\n"
	if got != want {
		t.Fatalf("RenderBlock = %q, want %q", got, want)
	}
}

// TestRenderBlockOneHostPerLine guards against the macOS resolver ignoring
// aliases on a very long line. Every host must be on its own short line.
func TestRenderBlockOneHostPerLine(t *testing.T) {
	entries := manyHosts(130)
	got := RenderBlock(entries)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != len(entries)+2 {
		t.Fatalf("block has %d lines, want %d", len(lines), len(entries)+2)
	}
	if lines[0] != hostsBegin || lines[len(lines)-1] != hostsEnd {
		t.Fatalf("block markers = %q / %q", lines[0], lines[len(lines)-1])
	}
	for i, entry := range entries {
		if want := "127.0.0.1 " + entry; lines[i+1] != want {
			t.Fatalf("line %d = %q, want %q", i+1, lines[i+1], want)
		}
	}
}

func TestMergeHostsBlockRemovesWhenEmpty(t *testing.T) {
	existing := "127.0.0.1 localhost\n\n" + hostsBegin +
		"\n127.0.0.1 app.dev\n" + hostsEnd + "\n"
	got := mergeHostsBlock(existing, RenderBlock(nil))
	if got != "127.0.0.1 localhost\n\n" {
		t.Fatalf("removing managed block left %q", got)
	}
}

func manyHosts(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("host-%d.pixinoid.dev", i))
	}
	return out
}
