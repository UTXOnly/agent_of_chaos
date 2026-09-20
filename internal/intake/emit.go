package intake

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/ddapi"
)

// DDConfig configures submission of the harness's measurements to Datadog.
type DDConfig struct {
	Enabled  bool
	Site     string
	APIKey   string
	Interval time.Duration
	Prefix   string   // metric prefix, default "aoc"
	Tags     []string // added to every series and event
	Events   bool     // post events for window/faults/generator lifecycle
}

// EmitterStatus is the emitter's health, for /harness/status.
type EmitterStatus struct {
	Enabled    bool      `json:"enabled"`
	Site       string    `json:"site,omitempty"`
	Interval   string    `json:"interval,omitempty"`
	Tags       []string  `json:"tags,omitempty"`
	KeyValid   bool      `json:"key_valid"`
	Batches    int64     `json:"batches"`
	Series     int64     `json:"series"`
	Events     int64     `json:"events"`
	Errors     int64     `json:"errors"`
	LastError  string    `json:"last_error,omitempty"`
	LastSubmit time.Time `json:"last_submit,omitempty"`
	LastCount  int       `json:"last_count"`
}

// emitCounters are the cumulative intake counters the emitter differentiates.
type emitCounters struct {
	logs, lines, msgBytes, wireBytes, rawBytes, requests int64
	byStatus                                             map[int]int64
	tagCount, tagBytes                                   int64
	faultDropped, faultErrored, faultDelayed             int64
	orphans, unmarked, truncated, tcpFrames              int64
	payloads                                             int64 // requests with logs
	e2e, sender                                          Quantiles
}

type emitter struct {
	s      *Server
	cfg    DDConfig
	client *ddapi.Client
	tags   []string

	mu       sync.Mutex
	st       EmitterStatus
	prev     emitCounters
	prevAt   time.Time
	prevTele map[string]teleSample
	events   chan ddapi.Event
	lastErr  time.Time
}

type teleSample struct {
	value float64
	at    time.Time
}

func newEmitter(s *Server, cfg DDConfig) *emitter {
	if cfg.Prefix == "" {
		cfg.Prefix = "aoc"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Second
	}
	e := &emitter{
		s: s, cfg: cfg, client: ddapi.New(cfg.Site, cfg.APIKey, ""),
		prevTele: map[string]teleSample{}, events: make(chan ddapi.Event, 256),
	}
	e.st = EmitterStatus{Enabled: true, Site: e.client.Site, Interval: cfg.Interval.String()}
	e.tags = append([]string{"run:" + s.cfg.Name}, cfg.Tags...)
	if s.cfg.AgentImage != "" {
		e.tags = append(e.tags, "agent_image:"+s.cfg.AgentImage)
	}
	e.st.Tags = e.tags
	return e
}

func (e *emitter) status() EmitterStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.st
}

// baseTags are the run tags plus the agent version once it is known.
func (e *emitter) baseTags() []string {
	tags := append([]string(nil), e.tags...)
	st := e.s.stats
	st.mu.Lock()
	best, bestN := "", int64(0)
	for v, n := range st.agentVersions {
		if n > bestN {
			best, bestN = v, n
		}
	}
	st.mu.Unlock()
	if best != "" {
		tags = append(tags, "agent_version:"+best)
	}
	return tags
}

func (e *emitter) run(ctx context.Context) {
	if err := e.client.Validate(ctx); err != nil {
		e.s.logf("[datadog] API key check failed (%v); metrics submission will keep retrying", err)
	} else {
		e.mu.Lock()
		e.st.KeyValid = true
		e.mu.Unlock()
		e.s.logf("[datadog] submitting %s.* metrics to %s every %s with tags %s", e.cfg.Prefix, e.client.Site, e.cfg.Interval, strings.Join(e.tags, ","))
	}
	go e.eventLoop(ctx)
	e.prev, e.prevAt = e.snapshot(), time.Now()
	t := time.NewTicker(e.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			e.flush(context.Background())
			return
		case <-t.C:
			e.flush(ctx)
		}
	}
}

func (e *emitter) snapshot() emitCounters {
	st := e.s.stats
	st.mu.Lock()
	defer st.mu.Unlock()
	c := emitCounters{
		logs: st.logs, lines: st.lines, msgBytes: st.msgBytes, wireBytes: st.wireBytes, rawBytes: st.rawBytes, requests: st.requests,
		byStatus: make(map[int]int64, len(st.byStatus)), tagCount: st.tagCount, tagBytes: st.tagBytes,
		faultDropped: st.faultDropped, faultErrored: st.faultErrored, faultDelayed: st.faultDelayed,
		orphans: st.orphans, unmarked: st.unmarked, truncated: st.truncated, tcpFrames: st.tcpFrames,
		payloads: int64(st.payloadWire.count),
		e2e:      st.e2eEmit.summary(), sender: st.senderEmit.summary(),
	}
	for k, v := range st.byStatus {
		c.byStatus[k] = v
	}
	st.e2eEmit.reset()
	st.senderEmit.reset()
	return c
}

// flush builds one batch of series and submits it.
func (e *emitter) flush(ctx context.Context) {
	now := time.Now()
	cur := e.snapshot()
	dt := now.Sub(e.prevAt).Seconds()
	if dt <= 0 {
		dt = e.cfg.Interval.Seconds()
	}
	series := e.build(now, cur, e.prev, dt)
	e.prev, e.prevAt = cur, now
	if len(series) == 0 {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	err := e.client.SubmitSeries(cctx, series)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.st.Errors++
		e.st.LastError = err.Error()
		if time.Since(e.lastErr) > 60*time.Second {
			e.lastErr = time.Now()
			e.s.logf("[datadog] metric submission failed: %v", err)
		}
		return
	}
	if e.st.LastError != "" {
		e.s.logf("[datadog] metric submission recovered")
	}
	e.st.LastError = ""
	e.st.KeyValid = true
	e.st.Batches++
	e.st.Series += int64(len(series))
	e.st.LastCount = len(series)
	e.st.LastSubmit = now
}

type seriesBuilder struct {
	prefix string
	ts     int64
	base   []string
	out    []ddapi.Series
}

func (b *seriesBuilder) gauge(name string, v float64, tags ...string) {
	if v != v { // NaN
		return
	}
	all := b.base
	if len(tags) > 0 {
		all = append(append([]string(nil), b.base...), tags...)
	}
	b.out = append(b.out, ddapi.Series{Metric: b.prefix + "." + name, Type: ddapi.TypeGauge, Points: []ddapi.Point{{Timestamp: b.ts, Value: v}}, Tags: all})
}

func rate(cur, prev int64, dt float64) float64 { return float64(cur-prev) / dt }

// build turns the intake's state into one batch of gauges.
func (e *emitter) build(now time.Time, cur, prev emitCounters, dt float64) []ddapi.Series {
	s := e.s
	rep := s.buildReport(false)
	b := &seriesBuilder{prefix: e.cfg.Prefix, ts: now.Unix(), base: e.baseTags()}

	// Generators (their own pushes).
	st := s.stats
	st.mu.Lock()
	for name, g := range st.gens {
		tag := "gen:" + name
		b.gauge("gen.records_per_sec", g.RecordRate, tag)
		b.gauge("gen.lines_per_sec", g.LineRate, tag)
		b.gauge("gen.bytes_per_sec", g.ByteRate, tag)
		b.gauge("gen.streams", float64(g.Last.ActiveStreams), tag)
		b.gauge("gen.target_rate", g.Last.Knobs.Rate, tag)
		b.gauge("gen.cpu_percent", g.Last.CPUPercent, tag)
		b.gauge("gen.rss_bytes", float64(g.Last.RSSBytes), tag)
		b.gauge("gen.records", float64(g.Last.Totals.Records-g.base.Records), tag)
		b.gauge("gen.rotations", float64(g.Last.Totals.Rotations-g.base.Rotations), tag)
		b.gauge("gen.write_errors", float64(g.Last.Totals.Errors-g.base.Errors), tag)
	}
	inflight := st.inflight.Load()
	st.mu.Unlock()

	// Intake rates over the interval.
	b.gauge("intake.logs_per_sec", rate(cur.logs, prev.logs, dt))
	b.gauge("intake.lines_per_sec", rate(cur.lines, prev.lines, dt))
	b.gauge("intake.bytes.wire_per_sec", rate(cur.wireBytes, prev.wireBytes, dt))
	b.gauge("intake.bytes.raw_per_sec", rate(cur.rawBytes, prev.rawBytes, dt))
	b.gauge("intake.bytes.message_per_sec", rate(cur.msgBytes, prev.msgBytes, dt))
	b.gauge("intake.requests_per_sec", rate(cur.requests, prev.requests, dt))
	for code, n := range cur.byStatus {
		label := strconv.Itoa(code)
		if code == 0 {
			label = "dropped"
		}
		b.gauge("intake.responses_per_sec", rate(n, prev.byStatus[code], dt), "status:"+label)
	}
	if dw := cur.wireBytes - prev.wireBytes; dw > 0 {
		b.gauge("intake.compression_ratio", float64(cur.rawBytes-prev.rawBytes)/float64(dw))
	}
	if dp := cur.payloads - prev.payloads; dp > 0 {
		b.gauge("intake.logs_per_payload", float64(cur.logs-prev.logs)/float64(dp))
		b.gauge("intake.payload.wire_bytes", float64(cur.wireBytes-prev.wireBytes)/float64(dp))
	}
	if dl := cur.logs - prev.logs; dl > 0 {
		b.gauge("intake.tags.per_log", float64(cur.tagCount-prev.tagCount)/float64(dl))
		b.gauge("intake.tags.bytes_per_log", float64(cur.tagBytes-prev.tagBytes)/float64(dl))
	}
	b.gauge("intake.tags.unique_keys", float64(rep.Tags.UniqueKeys))
	b.gauge("intake.inflight", float64(inflight))
	b.gauge("intake.orphans_per_sec", rate(cur.orphans, prev.orphans, dt))
	b.gauge("intake.unmarked_per_sec", rate(cur.unmarked, prev.unmarked, dt))
	b.gauge("intake.truncated_per_sec", rate(cur.truncated, prev.truncated, dt))
	if cur.tcpFrames > 0 {
		b.gauge("intake.tcp_frames_per_sec", rate(cur.tcpFrames, prev.tcpFrames, dt))
	}

	// Latency over the interval (seconds).
	if cur.e2e.Count > 0 {
		b.gauge("intake.latency.e2e.p50", cur.e2e.P50)
		b.gauge("intake.latency.e2e.p90", cur.e2e.P90)
		b.gauge("intake.latency.e2e.p99", cur.e2e.P99)
		b.gauge("intake.latency.e2e.max", cur.e2e.Max)
		b.gauge("intake.latency.e2e.avg", cur.e2e.Mean)
	}
	if cur.sender.Count > 0 {
		b.gauge("intake.latency.sender.p50", cur.sender.P50)
		b.gauge("intake.latency.sender.p99", cur.sender.P99)
		b.gauge("intake.latency.sender.max", cur.sender.Max)
	}

	// Delivery ledger (cumulative over the window).
	d := rep.Delivery
	b.gauge("intake.delivery.generated", float64(d.GeneratedRecords))
	b.gauge("intake.delivery.received", float64(d.ReceivedLogs))
	b.gauge("intake.delivery.unique", float64(d.Unique))
	b.gauge("intake.delivery.missing", float64(d.Missing))
	b.gauge("intake.delivery.duplicates", float64(d.Duplicates))
	b.gauge("intake.delivery.out_of_order", float64(d.OutOfOrder))
	b.gauge("intake.delivery.orphans", float64(d.Orphans))
	b.gauge("intake.delivery.unmarked", float64(d.Unmarked))
	b.gauge("intake.delivery.truncated", float64(d.Truncated))
	b.gauge("intake.delivery.multiline", float64(d.Multiline))
	b.gauge("intake.delivery.multiline_written", float64(d.MultilineWritten))
	if d.GeneratedRecords > 0 {
		b.gauge("intake.delivery.ratio", d.Ratio)
	}

	// Faults.
	f := s.faults.get()
	active := 0.0
	if f.Active() {
		active = 1
	}
	b.gauge("intake.faults.active", active)
	b.gauge("intake.faults.dropped_per_sec", rate(cur.faultDropped, prev.faultDropped, dt))
	b.gauge("intake.faults.errored_per_sec", rate(cur.faultErrored, prev.faultErrored, dt))
	b.gauge("intake.faults.delayed_per_sec", rate(cur.faultDelayed, prev.faultDelayed, dt))

	// Streams with problems (bounded).
	n := 0
	for _, str := range rep.Streams {
		if str.Missing == 0 && str.Duplicates == 0 {
			continue
		}
		if n++; n > 300 {
			break
		}
		tags := []string{"gen:" + str.Gen, "stream:" + str.Stream}
		b.gauge("stream.missing", float64(str.Missing), tags...)
		b.gauge("stream.duplicates", float64(str.Duplicates), tags...)
	}

	// Agent observations.
	if s.agent != nil {
		o := s.agent.status()
		if o.DockerOK {
			b.gauge("agent.container.cpu_percent", o.CPUPercent)
			b.gauge("agent.container.memory_bytes", float64(o.MemBytes))
			b.gauge("agent.container.pids", float64(o.PIDs))
		}
		if o.TelemetryOK {
			b.gauge("agent.process.cpu_percent", o.ProcCPUPercent)
			b.gauge("agent.process.rss_bytes", float64(o.ProcRSS))
			e.telemetrySeries(b, s.agent.series(), now)
		}
	}
	return b.out
}

// telemetrySeries forwards the agent's internal series: counters as
// per-second rates over the emit interval, gauges as-is. Histogram buckets
// are skipped; their _sum/_count go through as rates.
func (e *emitter) telemetrySeries(b *seriesBuilder, series []MetricSeries, now time.Time) {
	seen := map[string]bool{}
	for _, m := range series {
		if strings.HasSuffix(m.Name, "_bucket") {
			continue
		}
		key := m.Name + m.Labels
		seen[key] = true
		name := "agent.telemetry." + strings.ReplaceAll(m.Name, "__", ".")
		tags := labelsToTags(m.Labels)
		isCounter := m.Type == "counter" || strings.HasSuffix(m.Name, "_total") ||
			(m.Type == "histogram" || m.Type == "summary") && (strings.HasSuffix(m.Name, "_sum") || strings.HasSuffix(m.Name, "_count"))
		if !isCounter {
			b.gauge(name, m.Value, tags...)
			continue
		}
		prev, ok := e.prevTele[key]
		e.prevTele[key] = teleSample{value: m.Value, at: m.LastAt}
		if !ok || !m.LastAt.After(prev.at) {
			continue
		}
		if dt := m.LastAt.Sub(prev.at).Seconds(); dt > 0 && m.Value >= prev.value {
			b.gauge(name+".rate", (m.Value-prev.value)/dt, tags...)
		}
	}
	for k := range e.prevTele {
		if !seen[k] {
			delete(e.prevTele, k)
		}
	}
}

// labelsToTags turns {a="b",c="d"} into ["a:b","c:d"].
func labelsToTags(labels string) []string {
	labels = strings.TrimSuffix(strings.TrimPrefix(labels, "{"), "}")
	if labels == "" {
		return nil
	}
	var tags []string
	for _, kv := range splitLabels(labels) {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		v := strings.Trim(kv[i+1:], `"`)
		if v == "" {
			continue
		}
		tags = append(tags, kv[:i]+":"+v)
	}
	sort.Strings(tags)
	return tags
}

// splitLabels splits on commas outside quotes.
func splitLabels(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			cur.WriteByte(s[i+1])
			i++
		case c == '"':
			inQ = !inQ
			cur.WriteByte(c)
		case c == ',' && !inQ:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// ── events ───────────────────────────────────────────────────────────────────

// event queues a Datadog event; it never blocks the caller.
func (e *emitter) event(title, text, alertType string, extra ...string) {
	if e == nil || !e.cfg.Events {
		return
	}
	ev := ddapi.Event{
		Title: title, Text: text, AlertType: alertType, SourceTypeName: "aoc",
		Tags: append(append([]string{"source:aoc"}, e.baseTags()...), extra...), DateHappened: time.Now().Unix(),
	}
	select {
	case e.events <- ev:
	default:
	}
}

func (e *emitter) eventLoop(ctx context.Context) {
	for {
		var ev ddapi.Event
		select {
		case <-ctx.Done():
			return
		case ev = <-e.events:
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := e.client.PostEvent(cctx, ev)
		cancel()
		e.mu.Lock()
		if err != nil {
			e.st.Errors++
			e.st.LastError = "event: " + err.Error()
		} else {
			e.st.Events++
		}
		e.mu.Unlock()
	}
}

// handleMark posts a free-text annotation event (e.g. "deployed build X").
func (s *Server) handleMark(w http.ResponseWriter, r *http.Request) {
	text := r.URL.Query().Get("text")
	if text == "" {
		http.Error(w, "text query parameter is required", 400)
		return
	}
	s.logf("[mark] %s", text)
	if s.emitter == nil {
		writeJSON(w, 200, map[string]any{"queued": false, "reason": "datadog metrics not configured"})
		return
	}
	s.emitter.event("aoc: "+text, fmt.Sprintf("Run %s: %s", s.cfg.Name, text), "info")
	writeJSON(w, 200, map[string]any{"queued": true})
}
