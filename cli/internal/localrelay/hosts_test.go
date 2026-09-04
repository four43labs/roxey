package localrelay

import "testing"

func TestMergeHostsBlockIsIdempotent(t *testing.T) {
	block := hostsBegin + "\n127.0.0.1 app.example.dev roxey.dev\n" + hostsEnd + "\n"
	existing := "127.0.0.1 localhost\n\n" + block

	if got := mergeHostsBlock(existing, block); got != existing {
		t.Fatalf("unchanged hosts file was rewritten:\n%q\nwant:\n%q", got, existing)
	}
}

func TestMergeHostsBlockReplacesManagedEntries(t *testing.T) {
	existing := "127.0.0.1 localhost\n\n" + hostsBegin +
		"\n127.0.0.1 old.example.dev\n" + hostsEnd + "\n"
	block := hostsBegin + "\n127.0.0.1 app.example.dev roxey.dev\n" + hostsEnd + "\n"
	want := "127.0.0.1 localhost\n\n" + block

	if got := mergeHostsBlock(existing, block); got != want {
		t.Fatalf("managed hosts block =\n%q\nwant:\n%q", got, want)
	}
}
