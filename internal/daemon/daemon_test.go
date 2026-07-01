package daemon

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sofiane8910/onepilot-bridge/internal/proto"
)

// A client is free to send Input immediately after Hello; both often land in
// the same TCP segment, so the daemon's handshake reader buffers past the
// Hello. Regression test for the bug where handle() handed the RAW conn to
// Attach, dropping those buffered bytes: the frame stream desynced and the
// connection died instantly (first attach saw no output, input never ran).
func TestInputImmediatelyAfterHelloSameSegment(t *testing.T) {
	d := New()
	port, err := d.Listen(34544)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = d.Serve() }()
	defer d.Close()
	defer d.kill("t-race")

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Hello + Input in ONE write, so they arrive in one segment and the
	// handshake reader is guaranteed to buffer the Input frame.
	var buf bytes.Buffer
	if err := proto.EncodeHello(&buf, proto.Hello{SessionID: "t-race", Cols: 80, Rows: 24}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := proto.WriteFrame(&buf, proto.TypeInput, []byte("echo BRIDGE_RACE_MARK\r")); err != nil {
		t.Fatalf("input: %v", err)
	}
	if _, err := conn.Write(buf.Bytes()); err != nil {
		t.Fatalf("write: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	var sb strings.Builder
	for !strings.Contains(sb.String(), "BRIDGE_RACE_MARK") {
		ft, p, err := proto.ReadFrame(r)
		if err != nil {
			t.Fatalf("connection died before the immediate input ran (got %q): %v", sb.String(), err)
		}
		if ft == proto.TypeData || ft == proto.TypeSnapshot {
			if _, data, derr := proto.DecodeOutput(p); derr == nil {
				sb.Write(data)
			}
		}
	}
}
