package helper

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/four43labs/roxey/cli/internal/config"
	"github.com/four43labs/roxey/cli/internal/localca"
	"github.com/four43labs/roxey/cli/internal/localrelay"
	"github.com/four43labs/roxey/cli/internal/projects"
)

// Server answers helper RPCs on the daemon side. It is deliberately small:
// a fixed operation whitelist, server-side validation, and no shell access.
type Server struct {
	StateDir string
	// UID is the only peer allowed to talk to the socket (the state-dir owner).
	UID uint32
	// Relay is the supervised port-443 child; may be nil in tests.
	Relay *RelaySupervisor

	mu sync.Mutex // serializes /etc/hosts writes across concurrent callers
}

// NewServer builds a server for stateDir, accepting only uid.
func NewServer(stateDir string, uid uint32, relay *RelaySupervisor) *Server {
	return &Server{StateDir: stateDir, UID: uid, Relay: relay}
}

// Listen creates the helper socket, owned by the target uid and readable only
// by its owner.
func (s *Server) Listen() (net.Listener, error) {
	path := config.SocketPath()
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		l.Close()
		return nil, err
	}
	// Best-effort: as root this hands the socket to the target user; the
	// peer-UID check below is the real authorization gate.
	_ = os.Chown(path, int(s.UID), -1)
	return l, nil
}

// Serve accepts connections until the listener is closed.
func (s *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return
	}
	uid, err := peerUID(uc)
	if err != nil || uid != s.UID {
		_ = writeMessage(conn, Response{Error: "unauthorized peer"})
		return
	}

	var req Request
	if err := readMessage(conn, &req); err != nil {
		_ = writeMessage(conn, Response{Error: "bad request: " + err.Error()})
		return
	}
	_ = writeMessage(conn, s.handle(req))
}

func (s *Server) handle(req Request) Response {
	switch req.Op {
	case OpPing:
		return Response{OK: true, Version: ProtocolVersion, DaemonPID: os.Getpid()}
	case OpSyncHosts:
		return s.syncHosts(req.Hosts)
	case OpEnsureRelay:
		return s.ensureRelay()
	case OpTrustCA:
		return s.trustCA()
	case OpRelayStatus:
		return s.relayStatus()
	default:
		return Response{Error: fmt.Sprintf("unknown op %q", req.Op)}
	}
}

func (s *Server) syncHosts(hosts []string) Response {
	if err := ValidateHosts(hosts, s.allowedTLDs()); err != nil {
		return Response{Error: err.Error()}
	}

	s.mu.Lock()
	err := localrelay.WriteManagedBlock(localrelay.RenderBlock(hosts))
	s.mu.Unlock()
	if err != nil {
		return Response{Error: "update /etc/hosts: " + err.Error()}
	}

	// Keep the relay serving whatever the registry now considers primary.
	if s.Relay != nil {
		if tld := s.primaryTLD(); tld != "" {
			if err := s.Relay.Ensure(tld); err != nil {
				return Response{Error: "start relay: " + err.Error()}
			}
		}
	}
	return Response{OK: true}
}

func (s *Server) ensureRelay() Response {
	if s.Relay == nil {
		return Response{OK: true}
	}
	tld := s.primaryTLD()
	if tld == "" {
		return Response{Error: "no local projects registered"}
	}
	if err := s.Relay.Ensure(tld); err != nil {
		return Response{Error: "start relay: " + err.Error()}
	}
	return Response{OK: true, TLD: tld}
}

func (s *Server) trustCA() Response {
	caPath := filepath.Join(config.CertDir(), "ca.crt")
	if _, err := os.Stat(caPath); err != nil {
		return Response{Error: "local CA missing: " + err.Error()}
	}
	if !localca.Trusted(caPath) {
		if err := localca.TrustCA(caPath); err != nil {
			return Response{Error: err.Error()}
		}
	}
	return Response{OK: true, Trusted: true}
}

func (s *Server) relayStatus() Response {
	tld := s.primaryTLD()
	if s.Relay != nil {
		if rtld, _ := s.Relay.Status(); rtld != "" {
			tld = rtld
		}
	}
	up := tld != "" && localrelay.HealthOK(tld)
	return Response{OK: true, TLD: tld, RelayUp: up}
}

func (s *Server) primaryTLD() string {
	reg, err := projects.Load()
	if err != nil {
		return ""
	}
	if len(reg.Projects) == 0 {
		return ""
	}
	return reg.PrimaryLocalTLD()
}

// allowedTLDs is the set of TLDs the registry currently considers local; the
// daemon refuses hosts outside it.
func (s *Server) allowedTLDs() []string {
	reg, err := projects.Load()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range reg.Projects {
		if p.Local && !seen[p.TLD] {
			seen[p.TLD] = true
			out = append(out, p.TLD)
		}
	}
	return out
}
