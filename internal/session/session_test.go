package session

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sofiane8910/onepilot-bridge/internal/proto"
)

// readOutputUntil reads frames until `marker` appears in accumulated output or
// the deadline passes. It returns the accumulated output, the highest sequence
// seen, and whether the marker was found.
func readOutputUntil(conn net.Conn, r *bufio.Reader, marker string, timeout time.Duration) (string, uint64, bool) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var sb strings.Builder
	var maxSeq uint64
	for {
		t, p, err := proto.ReadFrame(r)
		if err != nil {
			return sb.String(), maxSeq, strings.Contains(sb.String(), marker)
		}
		switch t {
		case proto.TypeData, proto.TypeSnapshot:
			seq, data, derr := proto.DecodeOutput(p)
			if derr == nil {
				if seq > maxSeq {
					maxSeq = seq
				}
				sb.Write(data)
			}
		}
		if strings.Contains(sb.String(), marker) {
			return sb.String(), maxSeq, true
		}
	}
}

func TestSessionPersistenceAndReplay(t *testing.T) {
	s, err := New("t-persist", 80, 24, "", "", func(string) {})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer s.Kill()

	// --- connection 1: run a marker command ---
	srv1, cli1 := net.Pipe()
	go s.Attach(srv1, proto.Hello{SessionID: "t-persist", Cols: 80, Rows: 24})
	r1 := bufio.NewReader(cli1)

	if err := proto.WriteFrame(cli1, proto.TypeInput, []byte("printf 'MARK_%s\\n' AAA\n")); err != nil {
		t.Fatal(err)
	}
	out1, seqAfterAAA, ok := readOutputUntil(cli1, r1, "MARK_AAA", 4*time.Second)
	if !ok {
		t.Fatalf("did not see MARK_AAA; got: %q", out1)
	}

	// Run a second marker so we can prove incremental replay skips the first.
	if err := proto.WriteFrame(cli1, proto.TypeInput, []byte("printf 'MARK_%s\\n' BBB\n")); err != nil {
		t.Fatal(err)
	}
	_, _, ok = readOutputUntil(cli1, r1, "MARK_BBB", 4*time.Second)
	if !ok {
		t.Fatal("did not see MARK_BBB")
	}

	// --- disconnect ---
	_ = cli1.Close()
	time.Sleep(100 * time.Millisecond)

	if !s.Alive() {
		t.Fatal("session died after client disconnect; persistence broken")
	}

	// --- connection 2: fresh client (lastSeq 0) must get a snapshot with history ---
	srv2, cli2 := net.Pipe()
	go s.Attach(srv2, proto.Hello{SessionID: "t-persist", LastSeq: 0, Cols: 80, Rows: 24})
	r2 := bufio.NewReader(cli2)
	snap, _, ok := readOutputUntil(cli2, r2, "MARK_AAA", 4*time.Second)
	if !ok {
		t.Fatalf("fresh reattach did not replay scrollback; got: %q", snap)
	}
	if !strings.Contains(snap, "MARK_BBB") {
		t.Fatalf("snapshot missing later output; got: %q", snap)
	}
	_ = cli2.Close()
	time.Sleep(100 * time.Millisecond)

	// --- connection 3: incremental reattach from seqAfterAAA must NOT resend AAA ---
	srv3, cli3 := net.Pipe()
	go s.Attach(srv3, proto.Hello{SessionID: "t-persist", LastSeq: seqAfterAAA, Cols: 80, Rows: 24})
	r3 := bufio.NewReader(cli3)
	gap, _, ok := readOutputUntil(cli3, r3, "MARK_BBB", 4*time.Second)
	if !ok {
		t.Fatalf("incremental reattach did not replay the gap; got: %q", gap)
	}
	if strings.Contains(gap, "MARK_AAA") {
		t.Fatalf("incremental reattach resent already seen output: %q", gap)
	}
	_ = cli3.Close()
}

// TestSessionContainer proves a session can run inside a container via
// `docker exec`. Guarded on a usable Docker; spins up a throwaway alpine
// container and asserts the shell responds from INSIDE it (apk-tools exists in
// alpine, not on the host), which is what makes the container path persistent.
func TestSessionContainer(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not usable")
	}
	name := fmt.Sprintf("op-bridge-test-%d", os.Getpid())
	_ = exec.Command("docker", "rm", "-f", name).Run()
	if err := exec.Command("docker", "run", "-d", "--name", name, "alpine", "sleep", "300").Run(); err != nil {
		t.Skipf("could not start test container: %v", err)
	}
	defer func() { _ = exec.Command("docker", "rm", "-f", name).Run() }()

	s, err := New("t-container", 80, 24, "", name, func(string) {})
	if err != nil {
		t.Fatalf("new container session: %v", err)
	}
	defer s.Kill()

	srv, cli := net.Pipe()
	go s.Attach(srv, proto.Hello{SessionID: "t-container", Cols: 80, Rows: 24, Container: name})
	r := bufio.NewReader(cli)

	// `apk --version` only succeeds inside the alpine container; "apk-tools" is in
	// the output, never in the typed command, so a match proves we are in the box.
	// Send CR (\r) for Enter, which is what a real terminal / SwiftTerm sends and
	// what the container's line editor (busybox ash) expects.
	if err := proto.WriteFrame(cli, proto.TypeInput, []byte("apk --version\r")); err != nil {
		t.Fatal(err)
	}
	out, _, ok := readOutputUntil(cli, r, "apk-tools", 10*time.Second)
	if !ok {
		t.Fatalf("container shell did not respond from inside the container; got: %q", out)
	}
	_ = cli.Close()
}

func TestSessionResize(t *testing.T) {
	s, err := New("t-resize", 80, 24, "", "", func(string) {})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer s.Kill()

	srv, cli := net.Pipe()
	go s.Attach(srv, proto.Hello{SessionID: "t-resize", Cols: 80, Rows: 24})
	r := bufio.NewReader(cli)

	if err := proto.EncodeResize(cli, 120, 40); err != nil {
		t.Fatal(err)
	}
	// `stty size` prints "rows cols" straight from the tty ioctl, so a correct
	// resize yields "40 120". The typed command contains no "120", so if it
	// appears in the output it can only come from the real resized tty. We
	// accumulate for a fixed window rather than stopping on a marker (the PTY
	// echoes the typed line, which would race the real result).
	if err := proto.WriteFrame(cli, proto.TypeInput, []byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	out, _, _ := readOutputUntil(cli, r, "\x00never\x00", 1500*time.Millisecond)
	if !strings.Contains(out, "120") {
		t.Fatalf("resize not applied; output: %q", out)
	}
	_ = cli.Close()
}
