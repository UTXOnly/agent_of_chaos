package gen

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"
)

// StreamStats are per-stream cumulative counters. lines are physical
// newline-terminated lines; records are logical events (one aoc= marker each).
type StreamStats struct {
	Lines     atomic.Int64
	Records   atomic.Int64
	Bytes     atomic.Int64
	Rotations atomic.Int64
	Multiline atomic.Int64 // records that carried a stack trace
	Wide      atomic.Int64
	Bursts    atomic.Int64
	Errors    atomic.Int64 // write errors
}

// streamState outlives a Stream so chaos restarts resume the sequence and RNG
// exactly where they left off.
type streamState struct {
	name  string
	host  string
	seq   int64
	rng   *rand.Rand
	pool  []byte
	stats StreamStats
	quota int64 // records; 0 = unlimited
}

func newStreamState(cfg *Config, name, host string) *streamState {
	var rng *rand.Rand
	if cfg.Deterministic {
		sum := sha256.Sum256([]byte(strconv.FormatUint(cfg.Seed, 10) + "\x00" + cfg.Name + "\x00" + name))
		rng = rand.New(rand.NewPCG(binary.BigEndian.Uint64(sum[:8]), binary.BigEndian.Uint64(sum[8:16])))
	} else {
		rng = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	poolSize := 64 << 10
	if cfg.WideLineBytes > poolSize {
		poolSize = cfg.WideLineBytes
	}
	return &streamState{name: name, host: host, rng: rng, pool: randomPool(rng, poolSize)}
}

func (st *streamState) quotaReached() bool {
	return st.quota > 0 && st.stats.Records.Load() >= st.quota
}

// Stream is one running worker: one service name, one or two output files (or
// stdout), one goroutine.
type Stream struct {
	st           *streamState
	name, host   string
	markerPrefix []byte
	cfg          *Config
	pc           *PhaseConfig
	clk          clock
	plain, json  lineWriter
	buf, msg, tr []byte
	startedAt    time.Time
	stop         chan struct{}
	done         chan struct{}
	lastErrLog   time.Time
	events       func(string)
	headerLen    int
}

func newStream(cfg *Config, pc *PhaseConfig, st *streamState, events func(string)) (*Stream, error) {
	s := &Stream{
		st: st, name: st.name, host: st.host, cfg: cfg, pc: pc,
		markerPrefix: []byte(cfg.Name + "/" + st.name + "/"),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
		buf:          make([]byte, 0, 8192),
		msg:          make([]byte, 0, 512),
		tr:           make([]byte, 0, 2048),
		events:       events,
	}
	// timestamp(27) + level(9) + "[aoc.svc] " + marker + space
	s.headerLen = 27 + 9 + len(st.name) + 7 + len(Marker) + len(s.markerPrefix) + 10
	if cfg.Output == OutputStdout {
		switch cfg.Format {
		case FormatPlain:
			s.plain = sharedStdout
		case FormatJSON:
			s.json = sharedStdout
		case FormatBoth:
			s.plain, s.json = sharedStdout, sharedStdout
		}
		return s, nil
	}
	var err error
	if cfg.Format == FormatPlain || cfg.Format == FormatBoth {
		s.plain, err = openRotating(filepath.Join(cfg.LogDir, st.name+".log"), cfg.RotateBytes, cfg.RotateKeep, cfg.RotateMode, cfg.BufferBytes, &st.stats.Rotations)
		if err != nil {
			return nil, err
		}
	}
	if cfg.Format == FormatJSON || cfg.Format == FormatBoth {
		s.json, err = openRotating(filepath.Join(cfg.LogDir, st.name+".json.log"), cfg.RotateBytes, cfg.RotateKeep, cfg.RotateMode, cfg.BufferBytes, &st.stats.Rotations)
		if err != nil {
			if s.plain != nil {
				s.plain.Close()
			}
			return nil, err
		}
	}
	return s, nil
}

func (s *Stream) start() {
	s.startedAt = time.Now()
	go s.run()
}

// stopAndWait signals the worker and blocks until its files are closed.
func (s *Stream) stopAndWait() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
}

func (s *Stream) timestamp() time.Time {
	if s.cfg.Deterministic {
		return synthBase.Add(time.Duration(s.st.seq) * time.Microsecond)
	}
	return time.Now()
}

func (s *Stream) write(w lineWriter, p []byte) {
	if err := w.Write(p); err != nil {
		s.st.stats.Errors.Add(1)
		if now := time.Now(); now.Sub(s.lastErrLog) > 10*time.Second {
			s.lastErrLog = now
			s.events(fmt.Sprintf("[!] %s: write %s: %v", s.name, w.Path(), err))
		}
	}
}

// emit assigns the next sequence number and timestamp to r, renders it in
// every enabled format and writes it.
func (s *Stream) emit(r *record) {
	s.st.seq++
	r.seq = s.st.seq
	r.ts = s.timestamp()
	st := &s.st.stats
	if s.plain != nil {
		var n int
		s.buf, n = s.appendPlain(s.buf[:0], r)
		s.write(s.plain, s.buf)
		st.Lines.Add(int64(n))
		st.Bytes.Add(int64(len(s.buf)))
	}
	if s.json != nil {
		s.buf = s.appendJSON(s.buf[:0], r)
		s.write(s.json, s.buf)
		st.Lines.Add(1)
		st.Bytes.Add(int64(len(s.buf)))
	}
	st.Records.Add(1)
	// Only plain output turns a trace into extra physical lines; JSON keeps
	// it inside one line, so it is not a multiline record for the tailer.
	if len(r.trace) > 0 && s.plain != nil {
		st.Multiline.Add(1)
	}
}

// emitNormal writes one weighted-random message, then rolls for an extra
// stack-trace record and an extra wide record. It returns the number of
// records written (1–3) so the pacer can count them against the rate.
func (s *Stream) emitNormal() int {
	rng := s.st.rng
	lvl := levelWeights[rng.IntN(len(levelWeights))]
	tmpls := messages[lvl]
	s.msg = appendTemplate(s.msg[:0], tmpls[rng.IntN(len(tmpls))], s.name, rng)
	s.msg = s.pad(s.msg)
	s.emit(&record{level: lvl, msg: s.msg})
	n := 1

	if mr := s.pc.MultilineRate(); mr > 0 && rng.Float64() < mr {
		exc := pickException(rng)
		s.msg = append(s.msg[:0], "Unhandled exception in "...)
		s.msg = append(s.msg, s.name...)
		s.msg = append(s.msg, ": "...)
		s.msg = append(s.msg, exc.msg...)
		s.tr = appendStackTrace(s.tr[:0], s.name, rng)
		s.emit(&record{level: Error, msg: s.msg, trace: s.tr, exc: exc})
		n++
	}
	if wr := s.pc.WideLineRate(); wr > 0 && rng.Float64() < wr {
		s.msg = append(s.msg[:0], "WIDE-LINE "...)
		s.msg = append(s.msg, s.name...)
		s.msg = append(s.msg, " x-trace-context: "...)
		s.msg = s.appendPool(s.msg, s.cfg.WideLineBytes)
		s.emit(&record{level: Debug, msg: s.msg})
		s.st.stats.Wide.Add(1)
		n++
	}
	return n
}

// pad grows msg with a random payload so the rendered line reaches ~PadTo bytes.
func (s *Stream) pad(msg []byte) []byte {
	if s.cfg.PadTo <= 0 {
		return msg
	}
	need := s.cfg.PadTo - (s.headerLen + len(msg))
	if need <= 5 {
		return msg
	}
	msg = append(msg, " pad="...)
	return s.appendPool(msg, need-5)
}

// appendPool appends n bytes taken from random windows of the stream's
// pre-generated random pool.
func (s *Stream) appendPool(buf []byte, n int) []byte {
	pool := s.st.pool
	for n > 0 {
		chunk := n
		if chunk > len(pool) {
			chunk = len(pool)
		}
		off := s.st.rng.IntN(len(pool) - chunk + 1)
		buf = append(buf, pool[off:off+chunk]...)
		n -= chunk
	}
	return buf
}

var burstLevels = [...]Level{Debug, Info, Warn, Error, Critical}

func (s *Stream) emitBurst(n int) {
	rng := s.st.rng
	s.msg = append(s.msg[:0], "--- BURST START service="...)
	s.msg = append(s.msg, s.name...)
	s.msg = append(s.msg, " lines="...)
	s.msg = strconv.AppendInt(s.msg, int64(n), 10)
	s.msg = append(s.msg, " ---"...)
	s.emit(&record{level: Warn, msg: s.msg})
	for i := 1; i <= n; i++ {
		lvl := burstLevels[i%len(burstLevels)]
		tmpls := messages[lvl]
		s.msg = append(s.msg[:0], "[burst "...)
		s.msg = strconv.AppendInt(s.msg, int64(i), 10)
		s.msg = append(s.msg, '/')
		s.msg = strconv.AppendInt(s.msg, int64(n), 10)
		s.msg = append(s.msg, "] "...)
		s.msg = appendTemplate(s.msg, tmpls[rng.IntN(len(tmpls))], s.name, rng)
		s.emit(&record{level: lvl, msg: s.msg})
	}
	s.msg = append(s.msg[:0], "--- BURST END service="...)
	s.msg = append(s.msg, s.name...)
	s.msg = append(s.msg, " ---"...)
	s.emit(&record{level: Warn, msg: s.msg})
	s.st.stats.Bursts.Add(1)
}

func (s *Stream) emitLifecycle(text string) {
	s.msg = append(s.msg[:0], text...)
	s.emit(&record{level: Info, msg: s.msg})
}

func (s *Stream) flush() {
	if s.plain != nil {
		if err := s.plain.Flush(); err != nil {
			s.st.stats.Errors.Add(1)
		}
	}
	if s.json != nil && s.json != s.plain {
		if err := s.json.Flush(); err != nil {
			s.st.stats.Errors.Add(1)
		}
	}
}

func (s *Stream) closeAll() {
	if s.plain != nil {
		s.plain.Close()
	}
	if s.json != nil && s.json != s.plain {
		s.json.Close()
	}
}

// run is the pacing loop. With a positive rate it emits whatever the wall
// clock says is owed (never more than EmitChunk at once) and sleeps briefly
// when ahead; with rate 0 it writes flat out. Bursts ride on top of the paced
// rate. The loop re-reads the live PhaseConfig every iteration, so phase
// changes take effect within milliseconds.
func (s *Stream) run() {
	defer close(s.done)
	defer s.closeAll()

	s.emitLifecycle(fmt.Sprintf("worker started host=%s gen=%s", s.host, s.cfg.Name))

	chunk := s.cfg.EmitChunk
	now := time.Now()
	lastFlush, lastBurst := now, now
	baseT, baseEmitted, lastRate := now, int64(0), -1.0
	stopped := false

	for !stopped && !s.st.quotaReached() {
		select {
		case <-s.stop:
			stopped = true
			continue
		default:
		}
		now = time.Now()
		rate := s.pc.StreamRate()
		if rate != lastRate {
			baseT, baseEmitted, lastRate = now, 0, rate
		}
		if rate <= 0 {
			for i := 0; i < chunk && !s.st.quotaReached(); i++ {
				s.emitNormal()
			}
		} else {
			owed := int64(now.Sub(baseT).Seconds()*rate) - baseEmitted
			if owed <= 0 {
				if now.Sub(lastFlush) >= s.cfg.FlushInterval {
					s.flush()
					lastFlush = now
				}
				wait := time.Duration(float64(time.Second) / rate)
				if wait > 5*time.Millisecond {
					wait = 5 * time.Millisecond
				} else if wait < 50*time.Microsecond {
					wait = 50 * time.Microsecond
				}
				select {
				case <-s.stop:
					stopped = true
				case <-time.After(wait):
				}
				continue
			}
			// Stalled for over a second (disk hiccup, CPU starvation): don't
			// try to make it all up in one go, just resume at the target pace.
			if float64(owed) > rate+float64(chunk) {
				baseT, baseEmitted = now, 0
				owed = int64(chunk)
			}
			n := int(min(owed, int64(chunk)))
			emitted := 0
			for emitted < n && !s.st.quotaReached() {
				emitted += s.emitNormal()
			}
			baseEmitted += int64(emitted)
		}
		if bs := s.pc.BurstSize(); bs > 0 {
			if iv := s.pc.BurstInterval(); iv > 0 && now.Sub(lastBurst) >= iv {
				s.emitBurst(bs)
				lastBurst = now
			}
		}
		if now.Sub(lastFlush) >= s.cfg.FlushInterval {
			s.flush()
			lastFlush = now
		}
	}
	s.emitLifecycle(fmt.Sprintf("worker stopped host=%s gen=%s", s.host, s.cfg.Name))
	s.flush()
}

// ensureDir creates the output directory.
func ensureDir(dir string) error { return os.MkdirAll(dir, 0o755) }
