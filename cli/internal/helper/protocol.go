// Package helper implements the privileged side of local relay mode: a small
// root daemon that owns /etc/hosts, the local CA trust, and the port-443
// relay, so unprivileged `roxey up`/`down` (including from agents) never
// prompt for sudo after the one-time setup.
//
// The CLI talks to the daemon over a Unix socket (~/.roxey/helper.sock) using
// one JSON request/response per connection. Only a fixed set of operations is
// accepted and every argument is validated server-side; the daemon never
// executes shell commands or accepts file paths or raw file contents.
package helper

import (
	"encoding/json"
	"fmt"
	"io"
)

// ProtocolVersion is bumped whenever the request/response shape changes so a
// stale daemon can be detected and reinstalled.
const ProtocolVersion = 1

// Operations accepted by the daemon.
const (
	OpPing        = "ping"
	OpSyncHosts   = "sync_hosts"
	OpEnsureRelay = "ensure_relay"
	OpTrustCA     = "trust_ca"
	OpRelayStatus = "relay_status"
)

// maxMessage bounds a single request/response to keep a malicious or buggy
// client from exhausting daemon memory.
const maxMessage = 1 << 20 // 1 MiB

// Request is a single helper call.
type Request struct {
	Op    string   `json:"op"`
	Hosts []string `json:"hosts,omitempty"`
}

// Response is the daemon's reply.
type Response struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Version int    `json:"version,omitempty"`

	// relay_status / trust_ca payloads.
	TLD       string `json:"tld,omitempty"`
	RelayUp   bool   `json:"relayUp,omitempty"`
	Trusted   bool   `json:"trusted,omitempty"`
	DaemonPID int    `json:"daemonPid,omitempty"`
}

func writeMessage(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(data) > maxMessage {
		return fmt.Errorf("helper message too large (%d bytes)", len(data))
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

func readMessage(r io.Reader, v any) error {
	dec := json.NewDecoder(io.LimitReader(r, maxMessage))
	return dec.Decode(v)
}
