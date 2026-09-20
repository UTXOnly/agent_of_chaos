package intake

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/wire"
)

const maxCardinality = 2000

// batch is the per-request aggregate, built without holding the stats lock.
type batch struct {
	status    int
	path      string
	encoding  string
	wire      int64
	raw       int64
	probe     bool
	malformed bool
	procTime  time.Duration

	logs, msgBytes, lines              int64
	marked, unmarked, orphans          int64
	multiline, truncated               int64
	tagCount, tagBytes                 int64
	e2eNoTS                            int64
	e2e, sender                        []float64
	obs                                []seqObs
	services, sources, hosts, statuses map[string]*nameCount
	tagKeys                            map[string]int64
}

func bump(m map[string]*nameCount, name string, bytes int64) {
	if name == "" {
		name = "(none)"
	}
	if c := m[name]; c != nil {
		c.Count++
		c.Bytes += bytes
		return
	}
	if len(m) >= maxCardinality {
		name = "(other)"
		if c := m[name]; c != nil {
			c.Count++
			c.Bytes += bytes
			return
		}
	}
	m[name] = &nameCount{Name: name, Count: 1, Bytes: bytes}
}

// genState is what we know about one generator from its stats pushes.
type genState struct {
	Last       wire.GenReport
	prev       wire.GenReport
	hasPrev    bool
	RecordRate float64 // records/s over the last two pushes
	LineRate   float64
	ByteRate   float64
	FirstSeen  time.Time
	LastSeen   time.Time
	MaxStreams int
	cpuSum     float64
	cpuN       int64
	RSSMax     int64
	// Values at the last reset, so the report can subtract them.
	base    wire.GenTotals
	baseSeq map[string]int64
}

// Stats is the intake's full observational state.
type Stats struct {
	mu        sync.Mutex
	startedAt time.Time
	resetAt   time.Time
	inflight  atomic.Int64

	requests       int64
	byStatus       map[int]int64
	byEncoding     map[string]int64
	byPath         map[string]int64
	otherRequests  map[string]int64
	probes         int64
	malformed      int64
	wireBytes      int64
	rawBytes       int64
	payloadWire    *hist
	payloadRaw     *hist
	logsPerPayload *hist
	procTime       *hist
	agentVersions  map[string]int64
	userAgents     map[string]int64
	origins        map[string]int64
	apiKeys        map[string]int64
	faultDropped   int64
	faultErrored   int64
	faultDelayed   int64
	faultSlowed    int64
	rejected       map[string]int64 // reason → count (413, 403, 400 strict)

	logs      int64
	msgBytes  int64
	lines     int64
	marked    int64
	unmarked  int64
	orphans   int64
	multiline int64
	truncated int64
	tagCount  int64
	tagBytes  int64
	e2eNoTS   int64
	tagKeys   map[string]*nameCount
	services  map[string]*nameCount
	sources   map[string]*nameCount
	hosts     map[string]*nameCount
	statuses  map[string]*nameCount
	e2e       *hist
	sender    *hist
	tcpFrames int64
	tcpBytes  int64

	gens map[string]*genState

	track *tracker
	ts    *timeseries

	// Interval histograms feed the Datadog metric emitter; it resets them
	// after every submission.
	e2eEmit    *hist
	senderEmit *hist

	faultLog []FaultEvent
	agent    *agentObs
}

func newStats(retention time.Duration) *Stats {
	now := time.Now()
	s := &Stats{startedAt: now, resetAt: now, track: newTracker(), ts: newTimeseries(retention)}
	s.initMaps()
	return s
}

func (s *Stats) initMaps() {
	s.byStatus = map[int]int64{}
	s.byEncoding = map[string]int64{}
	s.byPath = map[string]int64{}
	s.otherRequests = map[string]int64{}
	s.agentVersions = map[string]int64{}
	s.userAgents = map[string]int64{}
	s.origins = map[string]int64{}
	s.apiKeys = map[string]int64{}
	s.rejected = map[string]int64{}
	s.tagKeys = map[string]*nameCount{}
	s.services = map[string]*nameCount{}
	s.sources = map[string]*nameCount{}
	s.hosts = map[string]*nameCount{}
	s.statuses = map[string]*nameCount{}
	s.payloadWire = newSizeHist()
	s.payloadRaw = newSizeHist()
	s.logsPerPayload = newSizeHist()
	s.procTime = newLatencyHist()
	s.e2e = newLatencyHist()
	s.sender = newLatencyHist()
	s.e2eEmit = newLatencyHist()
	s.senderEmit = newLatencyHist()
	if s.gens == nil {
		s.gens = map[string]*genState{}
	}
}

// Reset zeroes every counter and ledger. Generator cumulative totals are
// re-based rather than dropped, so delivery math keeps working.
func (s *Stats) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests, s.probes, s.malformed, s.wireBytes, s.rawBytes = 0, 0, 0, 0, 0
	s.faultDropped, s.faultErrored, s.faultDelayed, s.faultSlowed = 0, 0, 0, 0
	s.logs, s.msgBytes, s.lines, s.marked, s.unmarked, s.orphans = 0, 0, 0, 0, 0, 0
	s.multiline, s.truncated, s.tagCount, s.tagBytes, s.e2eNoTS = 0, 0, 0, 0, 0
	s.tcpFrames, s.tcpBytes = 0, 0
	s.initMaps()
	s.faultLog = nil
	s.ts.reset()
	bases := map[string]map[string]int64{}
	for name, g := range s.gens {
		g.base = g.Last.Totals
		g.baseSeq = map[string]int64{}
		for _, st := range g.Last.Streams {
			g.baseSeq[st.Name] = st.Seq
		}
		bases[name] = g.baseSeq
		g.cpuSum, g.cpuN, g.RSSMax = 0, 0, 0
	}
	s.track.reset(bases)
	s.resetAt = time.Now()
	if s.agent != nil {
		s.agent.rebase()
	}
}

// merge folds one request's batch into the totals. Marker observations are
// applied to the ledgers first so records from before the current window
// are left out of every counter.
func (s *Stats) merge(b *batch, now time.Time) {
	if len(b.obs) > 0 {
		pre := s.track.apply(b.obs, now)
		b.logs -= pre.count
		b.marked -= pre.count
		b.lines -= pre.lines
		b.msgBytes -= pre.bytes
		b.multiline -= pre.multiline
	}
	s.mu.Lock()
	s.ts.roll(now, s)

	s.requests++
	s.byStatus[b.status]++
	s.byPath[b.path]++
	if b.encoding != "" {
		s.byEncoding[b.encoding]++
	}
	s.wireBytes += b.wire
	s.rawBytes += b.raw
	s.procTime.addDuration(b.procTime)
	if b.probe {
		s.probes++
	}
	if b.malformed {
		s.malformed++
	}
	if b.logs > 0 || b.raw > 0 {
		s.payloadWire.add(float64(b.wire))
		s.payloadRaw.add(float64(b.raw))
		s.logsPerPayload.add(float64(b.logs))
	}

	s.logs += b.logs
	s.msgBytes += b.msgBytes
	s.lines += b.lines
	s.marked += b.marked
	s.unmarked += b.unmarked
	s.orphans += b.orphans
	s.multiline += b.multiline
	s.truncated += b.truncated
	s.tagCount += b.tagCount
	s.tagBytes += b.tagBytes
	s.e2eNoTS += b.e2eNoTS
	for _, v := range b.e2e {
		s.e2e.add(v)
		s.ts.e2eOpen.add(v)
		s.e2eEmit.add(v)
	}
	for _, v := range b.sender {
		s.sender.add(v)
		s.ts.senderOpen.add(v)
		s.senderEmit.add(v)
	}
	for k, c := range b.services {
		bumpN(s.services, k, c.Count, c.Bytes)
	}
	for k, c := range b.sources {
		bumpN(s.sources, k, c.Count, c.Bytes)
	}
	for k, c := range b.hosts {
		bumpN(s.hosts, k, c.Count, c.Bytes)
	}
	for k, c := range b.statuses {
		bumpN(s.statuses, k, c.Count, c.Bytes)
	}
	for k, n := range b.tagKeys {
		bumpN(s.tagKeys, k, n, 0)
	}

	p := &s.ts.open
	p.Requests++
	p.Logs += b.logs
	p.Lines += b.lines
	p.RawBytes += b.msgBytes
	p.WireBytes += b.wire
	switch {
	case b.status >= 500:
		p.Err5xx++
	case b.status >= 400:
		p.Err4xx++
	}
	s.mu.Unlock()
}

func bumpN(m map[string]*nameCount, name string, n, bytes int64) {
	if c := m[name]; c != nil {
		c.Count += n
		c.Bytes += bytes
		return
	}
	if len(m) >= maxCardinality {
		name = "(other)"
		if c := m[name]; c != nil {
			c.Count += n
			c.Bytes += bytes
			return
		}
	}
	m[name] = &nameCount{Name: name, Count: n, Bytes: bytes}
}

// recordOther counts non-logs agent traffic (metrics, metadata, validate...).
func (s *Stats) recordOther(path string) {
	s.mu.Lock()
	s.otherRequests[path]++
	s.mu.Unlock()
}

func (s *Stats) recordHeaders(version, ua, origin, apiKey string) {
	s.mu.Lock()
	if version != "" {
		s.agentVersions[version]++
	}
	if ua != "" {
		s.userAgents[ua]++
	}
	if origin != "" {
		s.origins[origin]++
	}
	if apiKey != "" {
		s.apiKeys[maskKey(apiKey)]++
	}
	s.mu.Unlock()
}

func maskKey(k string) string {
	if len(k) <= 8 {
		return "****"
	}
	return k[:4] + "…" + k[len(k)-4:]
}

func (s *Stats) recordFault(kind string) {
	s.mu.Lock()
	switch kind {
	case "dropped":
		s.faultDropped++
		s.ts.open.Dropped++
	case "errored":
		s.faultErrored++
	case "delayed":
		s.faultDelayed++
	case "slowed":
		s.faultSlowed++
	}
	s.mu.Unlock()
}

func (s *Stats) recordRejected(reason string) {
	s.mu.Lock()
	s.rejected[reason]++
	s.mu.Unlock()
}

func (s *Stats) recordTCP(frames int, bytes int64) {
	s.mu.Lock()
	s.tcpFrames += int64(frames)
	s.tcpBytes += bytes
	s.mu.Unlock()
}

// genReport ingests a generator stats push.
func (s *Stats) genReport(r *wire.GenReport, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.gens[r.Gen]
	if g == nil {
		g = &genState{FirstSeen: now}
		s.gens[r.Gen] = g
	} else if r.StartedAt.After(g.Last.StartedAt.Add(time.Second)) {
		// Generator restarted: its counters start over. Re-base so totals
		// since the reset stay monotonic.
		g.base = wire.GenTotals{}
		g.baseSeq = nil
		g.hasPrev = false
		g.FirstSeen = now
		s.track.rebaseGen(r.Gen)
	}
	if g.hasPrev {
		if dt := r.TS.Sub(g.prev.TS).Seconds(); dt > 0 {
			g.RecordRate = float64(r.Totals.Records-g.prev.Totals.Records) / dt
			g.LineRate = float64(r.Totals.Lines-g.prev.Totals.Lines) / dt
			g.ByteRate = float64(r.Totals.Bytes-g.prev.Totals.Bytes) / dt
		}
	}
	g.prev, g.hasPrev = g.Last, true
	g.Last = *r
	g.LastSeen = now
	if r.ActiveStreams > g.MaxStreams {
		g.MaxStreams = r.ActiveStreams
	}
	g.cpuSum += r.CPUPercent
	g.cpuN++
	if r.RSSBytes > g.RSSMax {
		g.RSSMax = r.RSSBytes
	}
	if r.Final {
		g.RecordRate, g.LineRate, g.ByteRate = 0, 0, 0
	}
}

// genRates sums the live generator rates; stale generators (no push for 5s)
// count as zero.
func (s *Stats) genRates(now time.Time) (records, lines, bytes float64) {
	for _, g := range s.gens {
		if now.Sub(g.LastSeen) > 5*time.Second || g.Last.Final {
			continue
		}
		records += g.RecordRate
		lines += g.LineRate
		bytes += g.ByteRate
	}
	return
}

// tick is called every second so idle seconds still produce points.
func (s *Stats) tick(now time.Time) {
	s.mu.Lock()
	s.ts.roll(now, s)
	s.mu.Unlock()
}

// ── timeseries ───────────────────────────────────────────────────────────────

// Point is one second of activity.
type Point struct {
	T          int64   `json:"t"`
	Requests   int64   `json:"requests"`
	Logs       int64   `json:"logs"`
	Lines      int64   `json:"lines"`
	RawBytes   int64   `json:"raw_bytes"`
	WireBytes  int64   `json:"wire_bytes"`
	Err4xx     int64   `json:"err_4xx"`
	Err5xx     int64   `json:"err_5xx"`
	Dropped    int64   `json:"dropped"`
	GenRecords float64 `json:"gen_records"` // generator records/s at this second
	GenLines   float64 `json:"gen_lines"`
	GenBytes   float64 `json:"gen_bytes"`
	E2EP50     float64 `json:"e2e_p50"` // seconds; 0 when no samples
	E2EP99     float64 `json:"e2e_p99"`
	E2EMax     float64 `json:"e2e_max"`
	SenderP50  float64 `json:"sender_p50"`
	SenderP99  float64 `json:"sender_p99"`
	AgentCPU   float64 `json:"agent_cpu"` // container CPU %, -1 when unknown
	AgentMem   int64   `json:"agent_mem"` // container memory bytes, -1 when unknown
	ProcCPU    float64 `json:"proc_cpu"`  // core-agent process CPU %, from telemetry; -1 unknown
	ProcRSS    int64   `json:"proc_rss"`
	Inflight   int64   `json:"inflight"`
	AgentAnon  int64   `json:"agent_mem_anon"` // container anon memory (processes' own), -1 unknown
	AgentFile  int64   `json:"agent_mem_file"` // container file cache, -1 unknown
}

type timeseries struct {
	ring       []Point
	head, n    int
	open       Point
	e2eOpen    *hist
	senderOpen *hist
}

func newTimeseries(retention time.Duration) *timeseries {
	n := int(retention.Seconds())
	if n < 60 {
		n = 60
	}
	return &timeseries{ring: make([]Point, n), e2eOpen: newHist(50e-6, 3600, 1.25), senderOpen: newHist(50e-6, 3600, 1.25)}
}

func (t *timeseries) reset() {
	t.head, t.n = 0, 0
	t.open = Point{}
	t.e2eOpen.reset()
	t.senderOpen.reset()
}

// roll finalizes the open second if the clock moved on. Called with the
// stats lock held.
func (t *timeseries) roll(now time.Time, s *Stats) {
	sec := now.Unix()
	if t.open.T == 0 {
		t.open.T = sec
		return
	}
	for t.open.T < sec {
		p := t.open
		if t.e2eOpen.count > 0 {
			p.E2EP50, p.E2EP99, p.E2EMax = t.e2eOpen.quantile(0.5), t.e2eOpen.quantile(0.99), t.e2eOpen.max
		}
		if t.senderOpen.count > 0 {
			p.SenderP50, p.SenderP99 = t.senderOpen.quantile(0.5), t.senderOpen.quantile(0.99)
		}
		p.GenRecords, p.GenLines, p.GenBytes = s.genRates(time.Unix(p.T, 0))
		p.AgentCPU, p.AgentMem, p.ProcCPU, p.ProcRSS, p.AgentAnon, p.AgentFile = -1, -1, -1, -1, -1, -1
		if s.agent != nil {
			p.AgentCPU, p.AgentMem, p.ProcCPU, p.ProcRSS = s.agent.current(time.Unix(p.T+1, 0))
			p.AgentAnon, p.AgentFile = s.agent.memBreakdown(time.Unix(p.T+1, 0))
		}
		p.Inflight = s.inflight.Load()
		t.ring[t.head] = p
		t.head = (t.head + 1) % len(t.ring)
		if t.n < len(t.ring) {
			t.n++
		}
		t.open = Point{T: p.T + 1}
		t.e2eOpen.reset()
		t.senderOpen.reset()
		// If we fell far behind (sleeping laptop), skip ahead instead of
		// emitting thousands of empty points.
		if sec-t.open.T > int64(len(t.ring)) {
			t.open.T = sec
		}
	}
}

// since returns points with T >= from, oldest first.
func (t *timeseries) since(from int64) []Point {
	out := make([]Point, 0, t.n)
	start := (t.head - t.n + len(t.ring)) % len(t.ring)
	for i := 0; i < t.n; i++ {
		p := t.ring[(start+i)%len(t.ring)]
		if p.T >= from {
			out = append(out, p)
		}
	}
	return out
}
