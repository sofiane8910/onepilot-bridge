// Package session owns a persistent shell: a PTY, the child shell process, a
// scrollback ring, and the set of connected clients. It outlives any single
// client connection, which is what makes the terminal survive the phone
// disconnecting or the app closing. Output is fanned out to all clients and
// coalesced per client so a burst becomes one frame (the Mosh idea, at the
// transport level).
package session

import (
	"bufio"
	"errors"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/sofiane8910/onepilot-bridge/internal/proto"
)

// heartbeat is how often the daemon pings an idle client so a dead TCP peer is
// noticed promptly. Mirrors Mosh's few second keepalive.
const heartbeat = 5 * time.Second

// clientQueue bounds how far a single slow client may fall behind before we drop
// its connection (it will reconnect and replay the gap from the ring).
const clientQueue = 512

// Session is one persistent shell.
type Session struct {
	ID string

	mu      sync.Mutex
	ring    *Ring
	ptmx    *os.File
	cmd     *exec.Cmd
	cols    int
	rows    int
	clients map[*client]struct{}
	alive   bool

	onExit func(id string)
}

type outChunk struct {
	seq  uint64
	data []byte
}

type client struct {
	conn      net.Conn
	ch        chan outChunk
	done      chan struct{}
	closeOnce sync.Once
}

func (c *client) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

// New starts a persistent shell for the given id. onExit is called once when the
// shell exits so the daemon can drop the session from its registry. When
// container is non-empty the shell is a `docker exec` into that container.
func New(id string, cols, rows int, cwd, container string, onExit func(id string)) (*Session, error) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	var cmd *exec.Cmd
	if container != "" {
		// Run the shell inside the container. `-it` allocates a tty in the
		// container; our own pty (below) is docker's controlling tty, so resize
		// propagates. Prefer bash, fall back to sh. The daemon (and thus this
		// exec) lives on the host, so the session persists across disconnects as
		// long as the container is running.
		cmd = exec.Command("docker", "exec", "-it", container, "/bin/sh", "-c", "if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi")
	} else {
		shell := os.Getenv("SHELL")
		if shell == "" || !fileExists(shell) {
			switch {
			case fileExists("/bin/bash"):
				shell = "/bin/bash"
			default:
				shell = "/bin/sh"
			}
		}
		cmd = exec.Command(shell)
		if cwd != "" && dirExists(cwd) {
			cmd.Dir = cwd
		}
	}
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "ONEPILOT_BRIDGE=1")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return nil, err
	}
	s := &Session{
		ID:      id,
		ring:    NewRing(0),
		ptmx:    ptmx,
		cmd:     cmd,
		cols:    cols,
		rows:    rows,
		clients: make(map[*client]struct{}),
		alive:   true,
		onExit:  onExit,
	}
	go s.readLoop()
	return s, nil
}

// readLoop pumps shell output into the ring and out to every client until the
// shell exits.
func (s *Session) readLoop() {
	buf := make([]byte, 32<<10)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			cp := make([]byte, n)
			copy(cp, buf[:n])
			s.broadcast(cp)
		}
		if err != nil {
			break
		}
	}
	s.markExit()
}

func (s *Session) broadcast(p []byte) {
	s.mu.Lock()
	seq := s.ring.Append(p)
	for c := range s.clients {
		select {
		case c.ch <- outChunk{seq: seq, data: p}:
		default:
			// Client cannot keep up. Drop it; it reconnects and replays.
			go c.close()
		}
	}
	s.mu.Unlock()
}

func (s *Session) markExit() {
	s.mu.Lock()
	s.alive = false
	cs := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		cs = append(cs, c)
	}
	s.clients = make(map[*client]struct{})
	s.mu.Unlock()

	for _, c := range cs {
		w := bufio.NewWriter(c.conn)
		_ = proto.WriteFrame(w, proto.TypeExit, nil)
		_ = w.Flush()
		c.close()
	}
	_ = s.ptmx.Close()
	if s.onExit != nil {
		s.onExit(s.ID)
	}
}

// Alive reports whether the shell is still running.
func (s *Session) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alive
}

// Kill terminates the shell and all connections.
func (s *Session) Kill() {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = s.ptmx.Close()
}

// Attach registers a client connection and serves it until the connection
// closes. It first sends the catch-up (an incremental gap from hello.LastSeq, or
// a full snapshot for a fresh client), then streams live output. Attach blocks
// for the lifetime of the connection.
//
// Attach owns its own buffered reader on conn. Callers that already read the
// Hello through a buffered reader must use AttachReader with that same reader:
// bytes the handshake reader buffered past the Hello (a client is free to send
// Input immediately after it) would otherwise be dropped, desyncing the frame
// stream and killing the connection.
func (s *Session) Attach(conn net.Conn, h proto.Hello) error {
	return s.AttachReader(conn, bufio.NewReader(conn), h)
}

// AttachReader is Attach for callers that already consumed the Hello frame from
// `r`; reading continues from `r`, writes go to `conn`.
func (s *Session) AttachReader(conn net.Conn, r *bufio.Reader, h proto.Hello) error {
	s.mu.Lock()
	if !s.alive {
		s.mu.Unlock()
		return errors.New("session not alive")
	}
	// Apply the client's terminal size so the shell matches the phone.
	if h.Cols > 0 && h.Rows > 0 {
		s.cols, s.rows = h.Cols, h.Rows
		_ = pty.Setsize(s.ptmx, &pty.Winsize{Rows: uint16(h.Rows), Cols: uint16(h.Cols)})
	}
	gap, snap := s.ring.Since(h.LastSeq)
	catchUp := make([]byte, len(gap))
	copy(catchUp, gap)
	catchSeq := s.ring.Newest()

	c := &client{
		conn: conn,
		ch:   make(chan outChunk, clientQueue),
		done: make(chan struct{}),
	}
	s.clients[c] = struct{}{}
	s.mu.Unlock()

	go s.writeLoop(c, catchUp, snap, catchSeq)
	s.readFromClient(c, r)

	// readFromClient returned: the connection is gone.
	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
	c.close()
	return nil
}

func (s *Session) writeLoop(c *client, catchUp []byte, snap bool, catchSeq uint64) {
	w := bufio.NewWriter(c.conn)

	if len(catchUp) > 0 {
		t := proto.TypeData
		if snap {
			t = proto.TypeSnapshot
		}
		if err := proto.EncodeOutput(w, t, catchSeq, catchUp); err != nil {
			c.close()
			return
		}
		if err := w.Flush(); err != nil {
			c.close()
			return
		}
	}

	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return
		case oc := <-c.ch:
			// Coalesce everything currently queued into a single Data frame so a
			// burst of shell output is one frame, not hundreds.
			data := oc.data
			seq := oc.seq
		drain:
			for {
				select {
				case more := <-c.ch:
					data = append(data, more.data...)
					seq = more.seq
				default:
					break drain
				}
			}
			if err := proto.EncodeOutput(w, proto.TypeData, seq, data); err != nil {
				c.close()
				return
			}
			if err := w.Flush(); err != nil {
				c.close()
				return
			}
		case <-ticker.C:
			if err := proto.WriteFrame(w, proto.TypePing, nil); err != nil {
				c.close()
				return
			}
			if err := w.Flush(); err != nil {
				c.close()
				return
			}
		}
	}
}

func (s *Session) readFromClient(c *client, r *bufio.Reader) {
	for {
		t, payload, err := proto.ReadFrame(r)
		if err != nil {
			return
		}
		switch t {
		case proto.TypeInput:
			s.mu.Lock()
			ptmx := s.ptmx
			s.mu.Unlock()
			if _, werr := ptmx.Write(payload); werr != nil {
				return
			}
		case proto.TypeResize:
			cols, rows, derr := proto.DecodeResize(payload)
			if derr == nil && cols > 0 && rows > 0 {
				s.mu.Lock()
				s.cols, s.rows = cols, rows
				_ = pty.Setsize(s.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
				s.mu.Unlock()
			}
		case proto.TypeAck, proto.TypePong:
			// Ring is byte bounded, so acks need no trimming in Phase 1. Pong is
			// liveness only; reading it already proves the peer is alive.
		default:
			// Ignore unknown frame types for forward compatibility.
		}
	}
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
