// Package proto defines the framed wire protocol spoken between the phone
// client and the onepilot-bridge daemon over the SSH forwarded loopback port.
//
// Every frame is length prefixed so framing is unambiguous over a TCP stream:
//
//	type   (1 byte)
//	length (uint32, big endian)  -- number of payload bytes that follow
//	payload(length bytes)
//
// The hot path (Data, Input) carries raw terminal bytes; control frames
// (Hello, Resize) carry a tiny JSON or fixed layout payload. Output frames
// (Data, Snapshot) are prefixed with an 8 byte big endian sequence number:
// the cumulative count of terminal output bytes up to and including the frame.
// The client remembers the highest sequence it has applied and sends it back
// in Hello on reconnect so the daemon can replay only the missed gap.
package proto

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

// FrameType is the 1 byte discriminator at the head of every frame.
type FrameType byte

const (
	// Client to daemon.
	TypeHello  FrameType = 1 // payload: JSON Hello
	TypeInput  FrameType = 2 // payload: raw bytes to write to the PTY
	TypeResize FrameType = 3 // payload: cols(uint16) rows(uint16)
	TypeAck    FrameType = 4 // payload: seq(uint64) -- highest applied output seq
	TypePong   FrameType = 5 // payload: empty

	// Daemon to client.
	TypeData     FrameType = 6 // payload: seq(uint64) + raw output bytes (incremental)
	TypeSnapshot FrameType = 7 // payload: seq(uint64) + full scrollback bytes (fresh attach)
	TypePing     FrameType = 8 // payload: empty (also a leading health probe from a client)
	TypeExit     FrameType = 9 // payload: empty -- the shell exited, session is gone

	// Admin (leading frame from a CLI client; loopback only).
	TypeList       FrameType = 10 // client to daemon: list live sessions
	TypeKill       FrameType = 11 // client to daemon: payload = session id to kill
	TypeListResult FrameType = 12 // daemon to client: payload = newline joined ids
)

// maxFrame caps a single frame payload so a corrupt length cannot make us
// allocate unbounded memory. Terminal bursts are small; snapshots are bounded
// by the scrollback ring, which is far below this ceiling.
const maxFrame = 16 << 20 // 16 MiB

// ErrFrameTooLarge is returned when a frame advertises a payload above maxFrame.
var ErrFrameTooLarge = errors.New("proto: frame exceeds maximum size")

// Hello is the first frame a client sends. A daemon looks up SessionID and
// attaches to the existing session, or creates one if it does not exist.
type Hello struct {
	SessionID string `json:"sessionId"`
	LastSeq   uint64 `json:"lastSeq"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
	Cwd       string `json:"cwd,omitempty"`
	// Container, when set, makes the session shell a `docker exec` into that
	// container on this host rather than a plain host shell. The daemon still
	// runs on the host and owns the exec, so the container session is persistent
	// and scrollable like any other. Empty means a host shell.
	Container string `json:"container,omitempty"`
}

// WriteFrame writes a single length prefixed frame.
func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	if len(payload) > maxFrame {
		return ErrFrameTooLarge
	}
	var hdr [5]byte
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrame reads a single frame. The returned payload is freshly allocated.
func ReadFrame(r *bufio.Reader) (FrameType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, ErrFrameTooLarge
	}
	payload := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	return FrameType(hdr[0]), payload, nil
}

// --- helpers for typed payloads ---

// EncodeHello marshals a Hello and writes it as a TypeHello frame.
func EncodeHello(w io.Writer, h Hello) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return WriteFrame(w, TypeHello, b)
}

// DecodeHello parses a TypeHello payload.
func DecodeHello(payload []byte) (Hello, error) {
	var h Hello
	err := json.Unmarshal(payload, &h)
	return h, err
}

// EncodeOutput writes a Data or Snapshot frame carrying seq + bytes.
func EncodeOutput(w io.Writer, t FrameType, seq uint64, data []byte) error {
	buf := make([]byte, 8+len(data))
	binary.BigEndian.PutUint64(buf[:8], seq)
	copy(buf[8:], data)
	return WriteFrame(w, t, buf)
}

// DecodeOutput splits a Data/Snapshot payload into its seq and bytes.
// The returned slice aliases payload.
func DecodeOutput(payload []byte) (seq uint64, data []byte, err error) {
	if len(payload) < 8 {
		return 0, nil, errors.New("proto: output frame too short")
	}
	return binary.BigEndian.Uint64(payload[:8]), payload[8:], nil
}

// EncodeResize writes a TypeResize frame.
func EncodeResize(w io.Writer, cols, rows int) error {
	var b [4]byte
	binary.BigEndian.PutUint16(b[0:2], uint16(cols))
	binary.BigEndian.PutUint16(b[2:4], uint16(rows))
	return WriteFrame(w, TypeResize, b[:])
}

// DecodeResize parses a TypeResize payload.
func DecodeResize(payload []byte) (cols, rows int, err error) {
	if len(payload) < 4 {
		return 0, 0, errors.New("proto: resize frame too short")
	}
	return int(binary.BigEndian.Uint16(payload[0:2])), int(binary.BigEndian.Uint16(payload[2:4])), nil
}

// EncodeSeq writes a bare uint64 (used by TypeAck).
func EncodeSeq(w io.Writer, t FrameType, seq uint64) error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], seq)
	return WriteFrame(w, t, b[:])
}

// DecodeSeq parses a bare uint64 payload.
func DecodeSeq(payload []byte) (uint64, error) {
	if len(payload) < 8 {
		return 0, errors.New("proto: seq frame too short")
	}
	return binary.BigEndian.Uint64(payload[:8]), nil
}
