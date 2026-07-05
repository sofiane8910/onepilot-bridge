// Command onepilot-bridge is the host side of Onepilot's persistent terminal.
//
// It runs on the user's own server (never on Onepilot infrastructure). The
// `serve` subcommand is a self detaching daemon that owns persistent shells and
// listens on a loopback port; the phone reaches it over the SSH port forward the
// app already establishes. No inbound network port is opened.
//
// Subcommands:
//
//	serve [--port N]        start (or confirm) the daemon; prints the bound port
//	attach --session ID     connect stdin/stdout to a session (raw passthrough)
//	new [--session ID]      create a session and print its id
//	list                    print live session ids
//	kill --session ID       terminate a session
//	--version               print the version
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/sofiane8910/onepilot-bridge/internal/daemon"
	"github.com/sofiane8910/onepilot-bridge/internal/proto"
)

const version = "0.1.2"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "--version", "-version", "version":
		fmt.Println(version)
	case "serve":
		cmdServe(os.Args[2:])
	case "attach":
		cmdAttach(os.Args[2:])
	case "new":
		cmdNew(os.Args[2:])
	case "list":
		cmdList(os.Args[2:])
	case "kill":
		cmdKill(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `onepilot-bridge `+version+`
usage:
  onepilot-bridge serve [--port N]
  onepilot-bridge attach --session ID [--cols N --rows N]
  onepilot-bridge new [--session ID]
  onepilot-bridge list
  onepilot-bridge kill --session ID
  onepilot-bridge --version
`)
}

// --- paths ---

func stateDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".onepilot")
}

func portFilePath() string { return filepath.Join(stateDir(), "bridge.port") }
func logFilePath() string  { return filepath.Join(stateDir(), "bridge.log") }

func ensureStateDir() error { return os.MkdirAll(stateDir(), 0o700) }

func writePortFile(port int) error {
	tmp := portFilePath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(port)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, portFilePath())
}

func readPortFile() (int, error) {
	b, err := os.ReadFile(portFilePath())
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(trimSpace(b)))
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\n' || b[i] == '\r' || b[i] == '\t') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\n' || b[j-1] == '\r' || b[j-1] == '\t') {
		j--
	}
	return b[i:j]
}

// --- serve ---

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", daemon.DefaultPort, "preferred loopback port")
	_ = fs.Parse(args)

	if err := ensureStateDir(); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}

	// The detached child runs the real daemon.
	if os.Getenv("ONEPILOT_DAEMONIZED") == "1" {
		runDaemonChild(*port)
		return
	}

	// Parent: idempotent. If a healthy daemon is already up, just report it.
	if p, err := readPortFile(); err == nil && probeHealthy(p) {
		fmt.Println(p)
		return
	}

	if err := spawnChild(*port); err != nil {
		fmt.Fprintln(os.Stderr, "serve: spawn:", err)
		os.Exit(1)
	}

	// Wait for the child to bind and publish its port.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p, err := readPortFile(); err == nil && probeHealthy(p) {
			fmt.Println(p)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "serve: daemon did not come up in time")
	os.Exit(1)
}

func runDaemonChild(port int) {
	// New session already; ignore SIGHUP so an SSH login session closing does not
	// take the daemon with it.
	signal.Ignore(syscall.SIGHUP)

	d := daemon.New()
	actual, err := d.Listen(port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon: listen:", err)
		os.Exit(1)
	}
	if err := writePortFile(actual); err != nil {
		fmt.Fprintln(os.Stderr, "daemon: port file:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "onepilot-bridge daemon listening on 127.0.0.1:%d\n", actual)
	if err := d.Serve(); err != nil {
		fmt.Fprintln(os.Stderr, "daemon: serve:", err)
		os.Exit(1)
	}
}

func spawnChild(port int) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(logFilePath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "serve", "--port", strconv.Itoa(port))
	cmd.Env = append(os.Environ(), "ONEPILOT_DAEMONIZED=1")
	cmd.Stdin = devnull
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Detach: do not wait. The child lives on in its own session.
	return nil
}

// probeHealthy dials the port and does a Ping/Pong health check.
func probeHealthy(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if err := proto.WriteFrame(conn, proto.TypePing, nil); err != nil {
		return false
	}
	r := bufio.NewReader(conn)
	t, _, err := proto.ReadFrame(r)
	return err == nil && t == proto.TypePong
}

// --- client subcommands ---

func dialDaemon() net.Conn {
	port, err := readPortFile()
	if err != nil {
		fmt.Fprintln(os.Stderr, "no daemon: run `onepilot-bridge serve` first:", err)
		os.Exit(1)
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial daemon:", err)
		os.Exit(1)
	}
	return conn
}

func genID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "op-" + hex.EncodeToString(b[:])
}

func cmdAttach(args []string) {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	sess := fs.String("session", "", "session id (generated if empty)")
	cols := fs.Int("cols", 80, "terminal columns")
	rows := fs.Int("rows", 24, "terminal rows")
	_ = fs.Parse(args)
	if *sess == "" {
		*sess = genID()
	}
	cwd, _ := os.Getwd()

	conn := dialDaemon()
	defer conn.Close()

	var wmu sync.Mutex
	send := func(t proto.FrameType, p []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return proto.WriteFrame(conn, t, p)
	}

	if err := func() error {
		wmu.Lock()
		defer wmu.Unlock()
		return proto.EncodeHello(conn, proto.Hello{SessionID: *sess, LastSeq: 0, Cols: *cols, Rows: *rows, Cwd: cwd})
	}(); err != nil {
		fmt.Fprintln(os.Stderr, "attach: hello:", err)
		os.Exit(1)
	}

	// Reader: daemon output to stdout.
	go func() {
		r := bufio.NewReader(conn)
		for {
			t, p, err := proto.ReadFrame(r)
			if err != nil {
				os.Exit(0)
			}
			switch t {
			case proto.TypeData, proto.TypeSnapshot:
				if _, data, derr := proto.DecodeOutput(p); derr == nil {
					_, _ = os.Stdout.Write(data)
				}
			case proto.TypePing:
				_ = send(proto.TypePong, nil)
			case proto.TypeExit:
				os.Exit(0)
			}
		}
	}()

	// stdin to daemon input.
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			if serr := send(proto.TypeInput, buf[:n]); serr != nil {
				return
			}
		}
		if err != nil {
			break
		}
	}
	// stdin closed; keep streaming output until the connection ends.
	select {}
}

func cmdNew(args []string) {
	fs := flag.NewFlagSet("new", flag.ExitOnError)
	sess := fs.String("session", "", "session id (generated if empty)")
	cols := fs.Int("cols", 80, "terminal columns")
	rows := fs.Int("rows", 24, "terminal rows")
	_ = fs.Parse(args)
	if *sess == "" {
		*sess = genID()
	}
	cwd, _ := os.Getwd()

	conn := dialDaemon()
	if err := proto.EncodeHello(conn, proto.Hello{SessionID: *sess, Cols: *cols, Rows: *rows, Cwd: cwd}); err != nil {
		fmt.Fprintln(os.Stderr, "new:", err)
		os.Exit(1)
	}
	// Give the daemon a moment to create the session, then drop the connection.
	time.Sleep(150 * time.Millisecond)
	_ = conn.Close()
	fmt.Println(*sess)
}

func cmdList(args []string) {
	conn := dialDaemon()
	defer conn.Close()
	if err := proto.WriteFrame(conn, proto.TypeList, nil); err != nil {
		fmt.Fprintln(os.Stderr, "list:", err)
		os.Exit(1)
	}
	r := bufio.NewReader(conn)
	t, p, err := proto.ReadFrame(r)
	if err != nil || t != proto.TypeListResult {
		return
	}
	if len(p) > 0 {
		fmt.Println(string(p))
	}
}

func cmdKill(args []string) {
	fs := flag.NewFlagSet("kill", flag.ExitOnError)
	sess := fs.String("session", "", "session id to kill")
	_ = fs.Parse(args)
	if *sess == "" {
		fmt.Fprintln(os.Stderr, "kill: --session required")
		os.Exit(2)
	}
	conn := dialDaemon()
	defer conn.Close()
	_ = proto.WriteFrame(conn, proto.TypeKill, []byte(*sess))
	time.Sleep(100 * time.Millisecond)
}
