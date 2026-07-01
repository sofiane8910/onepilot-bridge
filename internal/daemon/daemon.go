// Package daemon is the long lived `serve` process. It listens on a loopback
// TCP port (reached by the phone over the SSH port forward), keeps a registry
// of persistent sessions, and routes each incoming connection to its session.
// It binds 127.0.0.1 only, so it never opens an inbound port to the network.
package daemon

import (
	"bufio"
	"fmt"
	"net"
	"sync"

	"github.com/sofiane8910/onepilot-bridge/internal/proto"
	"github.com/sofiane8910/onepilot-bridge/internal/session"
)

// DefaultPort is the preferred loopback port. If taken, Serve probes upward.
const DefaultPort = 24544

// portProbeRange is how many ports above DefaultPort to try before giving up.
const portProbeRange = 16

// Daemon holds the session registry and the loopback listener.
type Daemon struct {
	mu       sync.Mutex
	sessions map[string]*session.Session
	ln       net.Listener
}

// New creates an empty daemon.
func New() *Daemon {
	return &Daemon{sessions: make(map[string]*session.Session)}
}

// Listen binds a loopback TCP port, preferring `preferred` and probing upward if
// it is taken. It returns the bound port.
func (d *Daemon) Listen(preferred int) (int, error) {
	if preferred <= 0 {
		preferred = DefaultPort
	}
	var lastErr error
	for p := preferred; p < preferred+portProbeRange; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			d.ln = ln
			return p, nil
		}
		lastErr = err
	}
	return 0, fmt.Errorf("no free loopback port in [%d,%d): %w", preferred, preferred+portProbeRange, lastErr)
}

// Serve accepts connections until the listener is closed.
func (d *Daemon) Serve() error {
	if d.ln == nil {
		return fmt.Errorf("daemon: Listen must be called before Serve")
	}
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			return err
		}
		go d.handle(conn)
	}
}

// Close stops accepting new connections.
func (d *Daemon) Close() error {
	if d.ln != nil {
		return d.ln.Close()
	}
	return nil
}

func (d *Daemon) handle(conn net.Conn) {
	r := bufio.NewReader(conn)
	t, payload, err := proto.ReadFrame(r)
	if err != nil {
		_ = conn.Close()
		return
	}
	// Admin and health frames are handled inline and close the connection.
	switch t {
	case proto.TypePing:
		w := bufio.NewWriter(conn)
		_ = proto.WriteFrame(w, proto.TypePong, nil)
		_ = w.Flush()
		_ = conn.Close()
		return
	case proto.TypeList:
		ids := d.List()
		w := bufio.NewWriter(conn)
		_ = proto.WriteFrame(w, proto.TypeListResult, []byte(joinLines(ids)))
		_ = w.Flush()
		_ = conn.Close()
		return
	case proto.TypeKill:
		d.kill(string(payload))
		_ = conn.Close()
		return
	case proto.TypeHello:
		// fall through to session attach below
	default:
		_ = conn.Close()
		return
	}
	h, err := proto.DecodeHello(payload)
	if err != nil || h.SessionID == "" {
		_ = conn.Close()
		return
	}
	s, err := d.getOrCreate(h)
	if err != nil {
		_ = conn.Close()
		return
	}
	// Attach consumes the connection until it closes. The Hello was read through
	// `r`, which may have buffered frames the client sent right behind it (e.g.
	// Input immediately after Hello), so the SAME reader must keep the stream:
	// a fresh reader on conn would drop those bytes and desync the framing.
	_ = s.AttachReader(conn, r, h)
}

func (d *Daemon) getOrCreate(h proto.Hello) (*session.Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s := d.sessions[h.SessionID]; s != nil && s.Alive() {
		return s, nil
	}
	s, err := session.New(h.SessionID, h.Cols, h.Rows, h.Cwd, h.Container, d.remove)
	if err != nil {
		return nil, err
	}
	d.sessions[h.SessionID] = s
	return s, nil
}

func (d *Daemon) remove(id string) {
	d.mu.Lock()
	delete(d.sessions, id)
	d.mu.Unlock()
}

// List returns the ids of live sessions.
func (d *Daemon) List() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := make([]string, 0, len(d.sessions))
	for id, s := range d.sessions {
		if s.Alive() {
			ids = append(ids, id)
		}
	}
	return ids
}

func (d *Daemon) kill(id string) {
	d.mu.Lock()
	s := d.sessions[id]
	d.mu.Unlock()
	if s != nil {
		s.Kill()
	}
}

func joinLines(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "\n"
		}
		out += s
	}
	return out
}
