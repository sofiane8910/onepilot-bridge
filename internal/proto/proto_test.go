package proto

import (
	"bufio"
	"bytes"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, TypeInput, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(&buf)
	ty, p, err := ReadFrame(r)
	if err != nil {
		t.Fatal(err)
	}
	if ty != TypeInput || string(p) != "hello" {
		t.Fatalf("got type=%d payload=%q", ty, p)
	}
}

func TestHelloRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Hello{SessionID: "op-abc", LastSeq: 42, Cols: 120, Rows: 40, Cwd: "/tmp"}
	if err := EncodeHello(&buf, in); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(&buf)
	ty, p, err := ReadFrame(r)
	if err != nil || ty != TypeHello {
		t.Fatalf("read: ty=%d err=%v", ty, err)
	}
	out, err := DecodeHello(p)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("got %+v want %+v", out, in)
	}
}

func TestOutputRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeOutput(&buf, TypeData, 1234, []byte("xyz")); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(&buf)
	ty, p, err := ReadFrame(r)
	if err != nil || ty != TypeData {
		t.Fatalf("read: ty=%d err=%v", ty, err)
	}
	seq, data, err := DecodeOutput(p)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1234 || string(data) != "xyz" {
		t.Fatalf("got seq=%d data=%q", seq, data)
	}
}

func TestResizeRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeResize(&buf, 200, 50); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(&buf)
	_, p, err := ReadFrame(r)
	if err != nil {
		t.Fatal(err)
	}
	cols, rows, err := DecodeResize(p)
	if err != nil || cols != 200 || rows != 50 {
		t.Fatalf("got cols=%d rows=%d err=%v", cols, rows, err)
	}
}
