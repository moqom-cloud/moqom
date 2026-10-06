// Copyright 2026 Elliott Wave LLC. Licensed under BUSL-1.1; see LICENSING.md.
// CONFIDENTIAL: see doc.go.

package trust

import (
	"encoding/binary"
	"hash/fnv"
)

// ReplayIndex detects a recording being replayed as a live stream, from object
// sizes alone.
//
// The cheapest fake streamer pushes a pre-encoded file in a loop without
// re-encoding it ("-stream_loop -1 -c copy"). The bytes are the same every time
// round, so the sequence of object sizes is the same, exactly, to the byte. A
// live encoder never produces the same run of exact sizes twice: they depend on
// sensor noise as well as the scene. So the index keeps hashes of every run of
// Shingle consecutive delta-object sizes over a long horizon -- minutes, far
// longer than LoopScore can see -- and reports what fraction of recent runs it
// has seen before.
//
// Runs with few distinct sizes are skipped: a black screen or a static slide
// produces near-identical tiny frames that repeat honestly.
//
// It needs no decoding, so it runs on the relay's view of an E2EE stream. A
// loop that is re-encoded on every pass (a virtual camera in front of a real
// encoder) defeats it; LoopScore and liveness challenges are the answer there,
// and a weaker one.
type ReplayIndex struct {
	// Shingle is the run length. Default 8.
	Shingle int
	// Horizon is how many runs are remembered. Default 1<<16, about 36 minutes
	// at 30 frames a second.
	Horizon int

	window  []uint32
	ring    []uint64
	next    int
	counts  map[uint64]int
	recent  []bool
	rnext   int
	repeats int
	filled  int
}

const recentRuns = 512

func (r *ReplayIndex) defaults() {
	if r.Shingle == 0 {
		r.Shingle = 8
	}
	if r.Horizon == 0 {
		r.Horizon = 1 << 16
	}
	if r.counts == nil {
		r.counts = map[uint64]int{}
		r.ring = make([]uint64, 0, r.Horizon)
		r.recent = make([]bool, recentRuns)
	}
}

// Add records one delta-object size.
func (r *ReplayIndex) Add(size uint32) {
	r.defaults()
	r.window = append(r.window, size)
	if len(r.window) > r.Shingle {
		r.window = r.window[1:]
	}
	if len(r.window) < r.Shingle {
		return
	}
	distinct := map[uint32]bool{}
	lo, hi := r.window[0], r.window[0]
	for _, s := range r.window {
		distinct[s] = true
		lo, hi = min(lo, s), max(hi, s)
	}
	if len(distinct) < r.Shingle/2 || hi-lo < 64 {
		return
	}
	h := fnv.New64a()
	var b [4]byte
	for _, s := range r.window {
		binary.LittleEndian.PutUint32(b[:], s)
		h.Write(b[:])
	}
	sum := h.Sum64()
	seen := r.counts[sum] > 0

	if r.filled == recentRuns && r.recent[r.rnext] {
		r.repeats--
	}
	r.recent[r.rnext] = seen
	if seen {
		r.repeats++
	}
	r.rnext = (r.rnext + 1) % recentRuns
	r.filled = min(r.filled+1, recentRuns)

	if len(r.ring) < r.Horizon {
		r.ring = append(r.ring, sum)
	} else {
		old := r.ring[r.next]
		if r.counts[old]--; r.counts[old] <= 0 {
			delete(r.counts, old)
		}
		r.ring[r.next] = sum
		r.next = (r.next + 1) % r.Horizon
	}
	r.counts[sum]++
}

// Recurrence is the fraction of the last 512 runs that had been seen before,
// or 0 before enough runs to say.
func (r *ReplayIndex) Recurrence() float64 {
	if r.filled < 64 {
		return 0
	}
	return float64(r.repeats) / float64(r.filled)
}
