package session

import (
	"bytes"
	"testing"
)

func TestRingAppendAndNewest(t *testing.T) {
	r := NewRing(1024)
	if r.Newest() != 0 {
		t.Fatalf("empty ring newest=%d", r.Newest())
	}
	seq := r.Append([]byte("hello"))
	if seq != 5 || r.Newest() != 5 {
		t.Fatalf("seq=%d newest=%d", seq, r.Newest())
	}
	seq = r.Append([]byte("world"))
	if seq != 10 {
		t.Fatalf("seq=%d", seq)
	}
}

func TestRingSinceIncremental(t *testing.T) {
	r := NewRing(1024)
	r.Append([]byte("abc"))   // seq 3
	r.Append([]byte("defgh")) // seq 8

	gap, snap := r.Since(3)
	if snap {
		t.Fatal("expected incremental, got snapshot")
	}
	if string(gap) != "defgh" {
		t.Fatalf("gap=%q", gap)
	}

	// Already current.
	gap, snap = r.Since(8)
	if snap || len(gap) != 0 {
		t.Fatalf("expected empty current gap, got snap=%v gap=%q", snap, gap)
	}
}

func TestRingSinceSnapshotWhenBehind(t *testing.T) {
	r := NewRing(8) // tiny cap forces trimming
	r.Append([]byte("0123456789")) // 10 bytes, ring keeps last 8: "23456789", base=2, newest=10

	if got := r.Base(); got != 2 {
		t.Fatalf("base=%d want 2", got)
	}
	// A client that last saw seq 0 has fallen behind the retained window.
	gap, snap := r.Since(0)
	if !snap {
		t.Fatal("expected snapshot for a client behind the window")
	}
	if !bytes.Equal(gap, []byte("23456789")) {
		t.Fatalf("snapshot=%q", gap)
	}
}

func TestRingTrimBounded(t *testing.T) {
	r := NewRing(16)
	for i := 0; i < 100; i++ {
		r.Append([]byte("0123456789")) // 1000 bytes total
	}
	if len(r.buf) > 16 {
		t.Fatalf("ring not bounded: len=%d", len(r.buf))
	}
	if r.Newest() != 1000 {
		t.Fatalf("newest=%d want 1000", r.Newest())
	}
}
