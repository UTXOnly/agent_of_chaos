package intake

import (
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

// Faults is the runtime fault-injection configuration. Every field is
// independent; the zero value injects nothing.
type Faults struct {
	// LatencyMs delays every response by this much (+ up to JitterMs).
	LatencyMs int `json:"latency_ms"`
	JitterMs  int `json:"jitter_ms"`
	// ErrorRate is the probability (0..1) of answering ErrorStatus instead
	// of 202. The payload is NOT ingested. 429/5xx make the agent retry;
	// other 4xx make it drop the batch.
	ErrorRate   float64 `json:"error_rate"`
	ErrorStatus int     `json:"error_status"`
	// DropRate is the probability of closing the connection after reading
	// the request, without any response (simulates a flaky network).
	DropRate float64 `json:"drop_rate"`
	// Outage drops every request (DropRate 1.0) — a dead intake.
	Outage bool `json:"outage"`
	// ReadBps throttles how fast request bodies are read (bytes/s); 0 = off.
	// Models a saturated uplink: the agent's sends take longer and back up.
	ReadBps int64 `json:"read_bps"`
	// FlapOnMs and FlapOffMs make the faults above intermittent: they are
	// applied in episodes lasting about FlapOnMs, separated by quiet gaps of
	// about FlapOffMs, each randomized by ±50%. Random network blips rather
	// than a steady degradation. Both or neither.
	FlapOnMs  int `json:"flap_on_ms,omitempty"`
	FlapOffMs int `json:"flap_off_ms,omitempty"`
	// Note is free text shown in the UI and report ("simulating region outage").
	Note string `json:"note,omitempty"`
}

// Flapping reports whether the faults come and go in episodes.
func (f Faults) Flapping() bool { return f.FlapOnMs > 0 && f.FlapOffMs > 0 }

// Active reports whether any fault is configured.
func (f Faults) Active() bool {
	return f.LatencyMs > 0 || f.ErrorRate > 0 || f.DropRate > 0 || f.Outage || f.ReadBps > 0
}

func (f Faults) Validate() error {
	if f.LatencyMs < 0 || f.JitterMs < 0 || f.ReadBps < 0 {
		return fmt.Errorf("latency, jitter and read_bps must be >= 0")
	}
	if f.ErrorRate < 0 || f.ErrorRate > 1 || f.DropRate < 0 || f.DropRate > 1 {
		return fmt.Errorf("error_rate and drop_rate must be within [0, 1]")
	}
	if f.ErrorStatus != 0 && (f.ErrorStatus < 400 || f.ErrorStatus > 599) {
		return fmt.Errorf("error_status must be a 4xx or 5xx code")
	}
	if f.FlapOnMs < 0 || f.FlapOffMs < 0 || (f.FlapOnMs > 0) != (f.FlapOffMs > 0) {
		return fmt.Errorf("flap_on_ms and flap_off_ms must both be > 0 (intermittent) or both 0")
	}
	if f.Flapping() && !f.Active() {
		return fmt.Errorf("flap_on_ms/flap_off_ms need a fault to flap (latency_ms, error_rate, drop_rate, outage or read_bps)")
	}
	return nil
}

// String is the one-line description used in events and the report.
func (f Faults) String() string {
	if !f.Active() {
		return "none"
	}
	var parts []string
	if f.Outage {
		parts = append(parts, "outage")
	}
	if f.DropRate > 0 && !f.Outage {
		parts = append(parts, fmt.Sprintf("drop %.0f%%", f.DropRate*100))
	}
	if f.ErrorRate > 0 {
		st := f.ErrorStatus
		if st == 0 {
			st = 500
		}
		parts = append(parts, fmt.Sprintf("HTTP %d %.0f%%", st, f.ErrorRate*100))
	}
	if f.LatencyMs > 0 {
		s := fmt.Sprintf("latency %dms", f.LatencyMs)
		if f.JitterMs > 0 {
			s += fmt.Sprintf("±%dms", f.JitterMs)
		}
		parts = append(parts, s)
	}
	if f.ReadBps > 0 {
		parts = append(parts, fmt.Sprintf("read %d B/s", f.ReadBps))
	}
	s := strings.Join(parts, ", ")
	if f.Flapping() {
		s += fmt.Sprintf(", flapping ~%s on / ~%s off", time.Duration(f.FlapOnMs)*time.Millisecond, time.Duration(f.FlapOffMs)*time.Millisecond)
	}
	if f.Note != "" {
		s += " (" + f.Note + ")"
	}
	return s
}

// FaultEvent records a change to the fault configuration.
type FaultEvent struct {
	At     time.Time `json:"at"`
	Faults Faults    `json:"faults"`
	Desc   string    `json:"desc"`
}

type faultState struct {
	mu    sync.Mutex
	f     Faults
	since time.Time
	// Flapping: whether the current episode is on, and when it ends.
	on    bool
	until time.Time
}

// configured is the fault set as it was set.
func (s *faultState) configured() Faults {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f
}

// get is the fault set to apply to a request now: the configured one, or,
// while flapping, the configured one during an episode and nothing in the
// gaps. Episode and gap lengths are drawn in [0.5, 1.5) × the configured
// value, so the agent sees blips at irregular times.
func (s *faultState) get() Faults {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.f.Flapping() {
		return s.f
	}
	now := time.Now()
	for !now.Before(s.until) {
		s.on = !s.on
		base := s.f.FlapOffMs
		if s.on {
			base = s.f.FlapOnMs
		}
		d := time.Duration(float64(base)*(0.5+rand.Float64())) * time.Millisecond
		if s.until.IsZero() {
			s.until = now
		}
		s.until = s.until.Add(d)
	}
	if s.on {
		return s.f
	}
	return Faults{FlapOnMs: s.f.FlapOnMs, FlapOffMs: s.f.FlapOffMs, Note: s.f.Note}
}

func (s *faultState) set(f Faults) {
	s.mu.Lock()
	s.f = f
	s.since = time.Now()
	s.on, s.until = true, time.Time{} // the first toggle in get() starts a gap, so the agent sees normal service before the first blip
	s.mu.Unlock()
}

// throttledReader caps read throughput at bps by sleeping between chunks.
type throttledReader struct {
	r     io.Reader
	bps   int64
	start time.Time
	read  int64
}

func (t *throttledReader) Read(p []byte) (int, error) {
	if len(p) > 16<<10 {
		p = p[:16<<10]
	}
	n, err := t.r.Read(p)
	t.read += int64(n)
	// Sleep until the bytes read so far would have taken this long at bps.
	due := t.start.Add(time.Duration(float64(t.read) / float64(t.bps) * float64(time.Second)))
	if d := time.Until(due); d > 0 {
		time.Sleep(d)
	}
	return n, err
}
