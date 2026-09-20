package intake

import (
	"fmt"
	"io"
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
	// Note is free text shown in the UI and report ("simulating region outage").
	Note string `json:"note,omitempty"`
}

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
	mu    sync.RWMutex
	f     Faults
	since time.Time
}

func (s *faultState) get() Faults {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.f
}

func (s *faultState) set(f Faults) {
	s.mu.Lock()
	s.f = f
	s.since = time.Now()
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
