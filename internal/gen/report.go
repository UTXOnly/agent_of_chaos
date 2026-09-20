package gen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/fmtutil"
	"github.com/UTXOnly/agent_of_chaos/internal/procstat"
	"github.com/UTXOnly/agent_of_chaos/internal/wire"
)

// procSampler is the process CPU/RSS sampler shared with the intake.
type procSampler = procstat.Sampler

// Report is the generator's self-report, shared with the intake.
type Report = wire.GenReport

// Totals is the aggregate counter set.
type Totals = wire.GenTotals

// report snapshots every stream's counters.
func (c *Conductor) report(final bool) *Report {
	cfg := c.cfg
	host, _ := os.Hostname()
	snap := c.pc.Snapshot()
	rep := &Report{
		Gen: cfg.Name, Host: host, PID: os.Getpid(), Version: cfg.Version,
		StartedAt: c.startedAt, TS: time.Now(), Final: final,
		Mode: cfg.Mode, Scenario: c.sc.Name, Phase: c.Phase(),
		Knobs: wire.GenKnobs{
			Rate: snap.Rate, BurstSize: snap.BurstSize, BurstInterval: fmtutil.Duration(snap.BurstInterval),
			MultilineRate: snap.MultilineRate, WideLineRate: snap.WideLineRate,
		},
		Format: string(cfg.Format), Output: string(cfg.Output), LogDir: cfg.LogDir,
		Deterministic: cfg.Deterministic, Seed: cfg.Seed,
		RotateBytes: cfg.RotateBytes, RotateKeep: cfg.RotateKeep, RotateMode: string(cfg.RotateMode),
	}
	c.mu.Lock()
	rep.ActiveStreams = len(c.active)
	for name, st := range c.states {
		_, active := c.active[name]
		s := wire.GenStream{Name: name, Host: st.host, Active: active, Seq: st.seq}
		s.Lines = st.stats.Lines.Load()
		s.Records = st.stats.Records.Load()
		s.Bytes = st.stats.Bytes.Load()
		s.Rotations = st.stats.Rotations.Load()
		s.Multiline = st.stats.Multiline.Load()
		s.Wide = st.stats.Wide.Load()
		s.Bursts = st.stats.Bursts.Load()
		s.Errors = st.stats.Errors.Load()
		rep.Streams = append(rep.Streams, s)
		rep.Totals.Add(s.GenTotals)
	}
	c.mu.Unlock()
	sort.Slice(rep.Streams, func(i, j int) bool { return rep.Streams[i].Name < rep.Streams[j].Name })
	rep.CPUSeconds, rep.CPUPercent, rep.RSSBytes = c.proc.Sample()
	return rep
}

// pusher POSTs reports to the intake, logging failures at most every 30s so
// an absent intake never floods the console.
type pusher struct {
	url      string
	client   *http.Client
	events   func(string)
	mu       sync.Mutex
	lastErr  time.Time
	failed   int
	inflight bool
}

func newPusher(base string, events func(string)) *pusher {
	base = strings.TrimRight(base, "/")
	return &pusher{
		url:    base + wire.GenReportPath,
		client: &http.Client{Timeout: 3 * time.Second},
		events: events,
	}
}

// send is non-blocking for periodic reports and synchronous for the final one.
func (p *pusher) send(rep *Report, final bool) {
	body, err := json.Marshal(rep)
	if err != nil {
		return
	}
	if final {
		p.post(body)
		return
	}
	p.mu.Lock()
	if p.inflight {
		p.mu.Unlock()
		return
	}
	p.inflight = true
	p.mu.Unlock()
	go func() {
		p.post(body)
		p.mu.Lock()
		p.inflight = false
		p.mu.Unlock()
	}()
}

func (p *pusher) post(body []byte) {
	resp, err := p.client.Post(p.url, "application/json", bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.failed++
		if time.Since(p.lastErr) > 30*time.Second {
			p.lastErr = time.Now()
			p.events(fmt.Sprintf("[stats] push to %s failed: %v (will keep retrying)", p.url, err))
		}
		return
	}
	if p.failed > 0 {
		p.events(fmt.Sprintf("[stats] push to %s recovered", p.url))
		p.failed = 0
	}
}
