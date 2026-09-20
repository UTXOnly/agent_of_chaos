package intake

import (
	"math/bits"
	"sort"
	"sync"
	"time"
)

// maxTrackedSeq bounds the per-stream bitset (200M seqs = 25 MB). Beyond it
// the stream is still counted but gaps are no longer resolvable.
const maxTrackedSeq = 200_000_000

// streamTrack is the delivery ledger for one (generator, stream) pair.
// Sequence numbers are stored relative to base, the generator's last seq at
// the most recent window reset, so a reset mid-run stays exact.
type streamTrack struct {
	Gen        string
	Stream     string
	bits       []uint64
	base       int64
	PreWindow  int64 // records from before the window (seq <= base), ignored
	Received   int64 // marked logs received, including duplicates
	Unique     int64 // distinct sequence numbers seen
	Dups       int64
	OutOfOrder int64 // seq lower than the previous one from this stream
	MaxSeq     int64
	lastSeq    int64
	Untracked  int64 // seqs beyond maxTrackedSeq
	FirstAt    time.Time
	LastAt     time.Time
	Lines      int64 // physical lines (1 + newlines) in received messages
	Multiline  int64 // messages containing a newline
	Bytes      int64
}

// record returns false when seq belongs to the previous window.
func (t *streamTrack) record(seq int64, now time.Time, msgBytes int64, newlines int) bool {
	if seq <= t.base {
		t.PreWindow++
		return false
	}
	seq -= t.base
	t.Received++
	t.Bytes += msgBytes
	t.Lines += int64(1 + newlines)
	if newlines > 0 {
		t.Multiline++
	}
	if t.FirstAt.IsZero() {
		t.FirstAt = now
	}
	t.LastAt = now
	if seq < t.lastSeq {
		t.OutOfOrder++
	}
	t.lastSeq = seq
	if seq > t.MaxSeq {
		t.MaxSeq = seq
	}
	if seq <= 0 || seq > maxTrackedSeq {
		t.Untracked++
		return true
	}
	w, b := uint64(seq)>>6, uint64(seq)&63
	if int(w) >= len(t.bits) {
		grow := make([]uint64, int(w)+1+len(t.bits)/2)
		copy(grow, t.bits)
		t.bits = grow
	}
	if t.bits[w]&(1<<b) != 0 {
		t.Dups++
		return true
	}
	t.bits[w] |= 1 << b
	t.Unique++
	return true
}

// SeqRange is a closed interval of missing sequence numbers.
type SeqRange struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// gaps lists up to limit missing ranges in [1, upTo]. upTo is the generator's
// last assigned seq when known, else the highest seq received.
func (t *streamTrack) gaps(upTo int64, limit int) []SeqRange {
	if upTo <= 0 || limit <= 0 {
		return nil
	}
	if upTo > maxTrackedSeq {
		upTo = maxTrackedSeq
	}
	var out []SeqRange
	inGap := false
	var start int64
	for seq := int64(1); seq <= upTo; {
		w := uint64(seq) >> 6
		var word uint64
		if int(w) < len(t.bits) {
			word = t.bits[w]
		}
		// Fast path: whole word present or absent.
		if uint64(seq)&63 == 0 && seq+63 <= upTo {
			if word == ^uint64(0) {
				if inGap {
					out = append(out, SeqRange{start, seq - 1})
					inGap = false
					if len(out) >= limit {
						return out
					}
				}
				seq += 64
				continue
			}
			if word == 0 {
				if !inGap {
					inGap, start = true, seq
				}
				seq += 64
				continue
			}
		}
		present := word&(1<<(uint64(seq)&63)) != 0
		if present && inGap {
			out = append(out, SeqRange{start, seq - 1})
			inGap = false
			if len(out) >= limit {
				return out
			}
		} else if !present && !inGap {
			inGap, start = true, seq
		}
		seq++
	}
	if inGap {
		out = append(out, SeqRange{start, upTo})
	}
	return out
}

// missingUpTo counts absent seqs in [1, upTo].
func (t *streamTrack) missingUpTo(upTo int64) int64 {
	if upTo <= 0 {
		return 0
	}
	if upTo > maxTrackedSeq {
		return upTo - t.Unique - t.Untracked
	}
	var present int64
	full := int(uint64(upTo) >> 6)
	for w := 0; w < full && w < len(t.bits); w++ {
		present += int64(bits.OnesCount64(t.bits[w]))
	}
	if rem := uint64(upTo) & 63; rem != 0 && full < len(t.bits) {
		mask := (uint64(1) << (rem + 1)) - 1 // seqs full*64 .. full*64+rem
		present += int64(bits.OnesCount64(t.bits[full] & mask))
	}
	// seq 0 is never used; it sits in word 0 bit 0 and is always absent.
	m := upTo - present
	if m < 0 {
		m = 0
	}
	return m
}

// tracker owns all stream ledgers, keyed by generator then stream.
type tracker struct {
	mu   sync.Mutex
	gens map[string]map[string]*streamTrack
}

const maxTrackedStreams = 10000

func newTracker() *tracker { return &tracker{gens: map[string]map[string]*streamTrack{}} }

type seqObs struct {
	gen      string
	stream   string
	seq      int64
	bytes    int64
	newlines int
}

// preWindow tallies observations that belonged to the previous window, so
// the caller can leave them out of the window's totals.
type preWindow struct {
	count, lines, bytes, multiline int64
}

// apply records a batch of observations under one lock acquisition.
func (tr *tracker) apply(obs []seqObs, now time.Time) (pre preWindow) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var lastGen string
	var streams map[string]*streamTrack
	for i := range obs {
		o := &obs[i]
		if streams == nil || o.gen != lastGen {
			streams = tr.gens[o.gen]
			if streams == nil {
				if len(tr.gens) >= maxTrackedStreams {
					continue
				}
				streams = map[string]*streamTrack{}
				tr.gens[o.gen] = streams
			}
			lastGen = o.gen
		}
		t := streams[o.stream]
		if t == nil {
			if len(streams) >= maxTrackedStreams {
				continue
			}
			t = &streamTrack{Gen: o.gen, Stream: o.stream}
			streams[o.stream] = t
		}
		if !t.record(o.seq, now, o.bytes, o.newlines) {
			pre.count++
			pre.lines += int64(1 + o.newlines)
			pre.bytes += o.bytes
			if o.newlines > 0 {
				pre.multiline++
			}
		}
	}
	return pre
}

// reset clears every ledger and re-bases the given streams: bases maps
// gen → stream → last seq the generator reported. Streams not listed start
// from base 0.
func (tr *tracker) reset(bases map[string]map[string]int64) {
	tr.mu.Lock()
	tr.gens = map[string]map[string]*streamTrack{}
	for gen, streams := range bases {
		m := map[string]*streamTrack{}
		for name, seq := range streams {
			m[name] = &streamTrack{Gen: gen, Stream: name, base: seq}
		}
		tr.gens[gen] = m
	}
	tr.mu.Unlock()
}

// rebaseGen forgets a generator's bases (it restarted, so its seqs begin at 1
// again).
func (tr *tracker) rebaseGen(gen string) {
	tr.mu.Lock()
	delete(tr.gens, gen)
	tr.mu.Unlock()
}

// snapshot copies the ledgers (without bitsets) in stable order.
func (tr *tracker) snapshot() []streamTrack {
	tr.mu.Lock()
	var out []streamTrack
	for _, streams := range tr.gens {
		for _, t := range streams {
			c := *t
			c.bits = nil
			out = append(out, c)
		}
	}
	tr.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Gen != out[j].Gen {
			return out[i].Gen < out[j].Gen
		}
		return out[i].Stream < out[j].Stream
	})
	return out
}

// withStream runs fn with the ledger locked, for gap computations.
func (tr *tracker) withStream(gen, stream string, fn func(t *streamTrack)) {
	tr.mu.Lock()
	if streams := tr.gens[gen]; streams != nil {
		if t := streams[stream]; t != nil {
			fn(t)
		}
	}
	tr.mu.Unlock()
}
