// Package relay holds the live routing table (service/path -> connected CLI).
package relay

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsNetConn adapts a gorilla websocket to net.Conn so yamux can multiplex
// binary frames over it as one long-lived byte stream.
type wsNetConn struct {
	conn    *websocket.Conn
	reader  io.Reader
	writeMu sync.Mutex
}

func NewWSNetConn(c *websocket.Conn) net.Conn { return &wsNetConn{conn: c} }
func (w *wsNetConn) Read(p []byte) (int, error) {
	for {
		if w.reader == nil {
			_, r, err := w.conn.NextReader()
			if err != nil {
				return 0, err
			}
			w.reader = r
		}
		n, err := w.reader.Read(p)
		if err == io.EOF {
			w.reader = nil
			if n == 0 {
				continue
			}
			return n, nil
		}
		return n, err
	}
}

func (w *wsNetConn) Write(p []byte) (int, error) {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if err := w.conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *wsNetConn) Close() error                       { return w.conn.Close() }
func (w *wsNetConn) LocalAddr() net.Addr                { return noopAddr{} }
func (w *wsNetConn) RemoteAddr() net.Addr               { return noopAddr{} }
func (w *wsNetConn) SetReadDeadline(t time.Time) error  { return w.conn.SetReadDeadline(t) }
func (w *wsNetConn) SetWriteDeadline(t time.Time) error { return w.conn.SetWriteDeadline(t) }

func (w *wsNetConn) SetDeadline(t time.Time) error {
	if err := w.SetReadDeadline(t); err != nil {
		return err
	}
	return w.SetWriteDeadline(t)
}

type noopAddr struct{}

func (noopAddr) Network() string { return "ws" }
func (noopAddr) String() string  { return "ws" }
