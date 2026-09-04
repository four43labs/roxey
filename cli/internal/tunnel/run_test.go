package tunnel

import "testing"

func TestWebsocketURLIncludesGateGroup(t *testing.T) {
	u := websocketURL("wss", "roxey.f43.run", "app", "/api", "secret", "up_group")
	q := u.Query()
	if q.Get("service") != "app" || q.Get("path") != "/api" || q.Get("protect") != "secret" {
		t.Fatalf("missing tunnel query values: %s", u.String())
	}
	if got := q.Get("gate_group"); got != "up_group" {
		t.Fatalf("gate_group = %q", got)
	}
}
