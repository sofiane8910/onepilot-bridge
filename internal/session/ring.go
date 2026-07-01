package session

// Ring is a bounded byte buffer of recent terminal output plus a monotonic
// sequence counter. The sequence number equals the total number of output
// bytes ever produced by the shell; the ring retains only the most recent
// `capBytes` of them. This is what lets a reconnecting client replay exactly
// the bytes it missed (the gap between its last applied sequence and now),
// and what gives the phone scrollback that tmux could not.
//
// Ring is not safe for concurrent use; the owning Session guards it with a
// mutex.
type Ring struct {
	buf     []byte
	capBytes int
	// total is the cumulative count of bytes ever appended (the newest seq).
	total uint64
}

// NewRing creates a ring retaining at most capBytes of scrollback.
func NewRing(capBytes int) *Ring {
	if capBytes <= 0 {
		capBytes = 256 << 10 // 256 KiB default
	}
	return &Ring{capBytes: capBytes}
}

// Append records new output bytes and returns the new newest sequence number.
func (r *Ring) Append(p []byte) uint64 {
	r.buf = append(r.buf, p...)
	r.total += uint64(len(p))
	if len(r.buf) > r.capBytes {
		drop := len(r.buf) - r.capBytes
		// Slide the retained window forward. Copy keeps the backing array from
		// growing without bound across many appends.
		r.buf = append(r.buf[:0], r.buf[drop:]...)
	}
	return r.total
}

// Newest returns the sequence number of the most recent byte.
func (r *Ring) Newest() uint64 { return r.total }

// Base returns the sequence number just before the oldest retained byte.
// Bytes with sequence in (Base, Newest] are available for replay.
func (r *Ring) Base() uint64 { return r.total - uint64(len(r.buf)) }

// Since returns the bytes a client needs to catch up from lastSeq, and whether
// the result is a full snapshot (true) rather than an incremental gap (false).
//
//   - lastSeq within (Base, Newest]: incremental gap, snapshot=false.
//   - lastSeq == Newest: already current, nil gap, snapshot=false.
//   - otherwise (fresh client, or fallen behind the retained window): the whole
//     retained buffer as a snapshot, snapshot=true.
//
// The returned slice aliases the ring's backing array; callers must copy before
// releasing the session lock if they hold the bytes past that point.
func (r *Ring) Since(lastSeq uint64) (data []byte, snapshot bool) {
	base := r.Base()
	if lastSeq >= base && lastSeq <= r.total {
		off := lastSeq - base
		return r.buf[off:], false
	}
	return r.buf, true
}
