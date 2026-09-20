package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/fmtutil"
	"github.com/UTXOnly/agent_of_chaos/internal/procstat"
	"github.com/UTXOnly/agent_of_chaos/internal/wire"
)

// Config configures the fake intake.
type Config struct {
	Addr    string // HTTP listen address, e.g. ":8282"
	TCPAddr string // TCP (legacy) listen address, "" disables
	Name    string // run label shown in the UI and report

	// APIKey, when set, is required in DD-API-KEY (403 otherwise), like the
	// real intake. Empty accepts any key.
	APIKey string
	// MaxPayloadBytes rejects larger decompressed payloads with 413. The
	// real intake's limit is 5 MB.
	MaxPayloadBytes int64
	// Strict answers 400 to malformed payloads instead of swallowing them.
	Strict bool

	AgentTelemetryURL string        // e.g. http://localhost:5000/telemetry
	TelemetryFilter   string        // regexp of metric names to keep
	ScrapeInterval    time.Duration // telemetry scrape period
	DockerContainer   string        // agent container name for CPU/mem stats
	DockerSocket      string
	AgentImage        string // informational, stamped into the report

	Retention time.Duration // timeseries history to keep
	Faults    Faults        // initial faults
	Verbose   bool          // log every request
	Quiet     bool
	Version   string

	// Datadog submits the harness's measurements as <prefix>.* metrics.
	Datadog DDConfig

	// ProfileForward is where the agent's continuous-profiler uploads are
	// forwarded after a copy is kept: "auto" (the site's profile intake when
	// Datadog submission is on), a URL, or "off".
	ProfileForward string
	// ProfileStoreBytes caps the uploads held in memory (default 256 MiB).
	ProfileStoreBytes int64
}

// DefaultConfig returns sensible defaults: :8282 HTTP, :10516 TCP, 5 MB
// payload cap, 6 h of history.
func DefaultConfig() Config {
	return Config{
		Addr: ":8282", TCPAddr: ":10516", Name: "aoc",
		MaxPayloadBytes: 5 << 20, ScrapeInterval: time.Second,
		DockerSocket: "/var/run/docker.sock", Retention: 6 * time.Hour,
	}
}

// Server is the fake intake.
type Server struct {
	cfg      Config
	stats    *Stats
	faults   *faultState
	agent    *agentObs
	emitter  *emitter
	profiles *profileStore
	proc     procstat.Sampler
	out      *os.File
	mux      *http.ServeMux
	http     *http.Server
	start    time.Time
	mu       sync.Mutex
}

// New builds a server; nothing listens until Run.
func New(cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		return nil, errors.New("--addr is required")
	}
	if cfg.MaxPayloadBytes <= 0 {
		cfg.MaxPayloadBytes = 5 << 20
	}
	if cfg.Retention < time.Minute {
		cfg.Retention = 6 * time.Hour
	}
	if cfg.ScrapeInterval <= 0 {
		cfg.ScrapeInterval = time.Second
	}
	if err := cfg.Faults.Validate(); err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, stats: newStats(cfg.Retention), faults: &faultState{}, out: os.Stderr, start: time.Now()}
	if cfg.AgentTelemetryURL != "" || cfg.DockerContainer != "" {
		obs, err := newAgentObs(cfg.AgentTelemetryURL, cfg.DockerContainer, cfg.TelemetryFilter)
		if err != nil {
			return nil, err
		}
		s.agent = obs
		s.stats.agent = obs
	}
	if cfg.Datadog.Enabled {
		if cfg.Datadog.APIKey == "" {
			return nil, errors.New("datadog metrics enabled but no API key (set DD_API_KEY or --dd-api-key)")
		}
		s.emitter = newEmitter(s, cfg.Datadog)
	}
	if cfg.Faults.Active() {
		s.setFaults(cfg.Faults, "initial")
	}
	s.profiles = newProfileStore(profileForwardURL(cfg.ProfileForward, cfg.Datadog), cfg.Datadog.APIKey, cfg.ProfileStoreBytes, s.logf)
	s.mux = http.NewServeMux()
	s.routes()
	return s, nil
}

func (s *Server) logf(format string, args ...any) {
	if s.cfg.Quiet {
		return
	}
	fmt.Fprintf(s.out, "  %s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func (s *Server) routes() {
	m := s.mux
	// Agent → intake.
	m.HandleFunc("POST /api/v2/logs", s.handleLogs)
	m.HandleFunc("POST /v1/input", s.handleLogs)
	m.HandleFunc("POST /v1/input/{key}", s.handleLogs)
	m.HandleFunc("POST /api/v1/logs", s.handleLogs)
	m.HandleFunc("/api/v1/validate", s.handleValidate)
	// The trace-agent's profiling proxy (apm_config.profiling_dd_url) and
	// dd-trace-go's agentless path.
	m.HandleFunc("POST /api/v2/profile", s.handleProfileUpload)
	m.HandleFunc("POST /profiling/v1/input", s.handleProfileUpload)
	// Harness API.
	m.HandleFunc("POST "+wire.GenReportPath, s.handleGenReport)
	m.HandleFunc("GET /harness/status", s.handleStatus)
	m.HandleFunc("GET /harness/timeseries", s.handleTimeseries)
	m.HandleFunc("GET /harness/report", s.handleReport)
	m.HandleFunc("GET /harness/faults", s.handleFaultsGet)
	m.HandleFunc("POST /harness/faults", s.handleFaultsSet)
	m.HandleFunc("DELETE /harness/faults", s.handleFaultsClear)
	m.HandleFunc("POST /harness/reset", s.handleReset)
	m.HandleFunc("POST /harness/mark", s.handleMark)
	m.HandleFunc("GET /harness/profiles", s.handleProfilesList)
	m.HandleFunc("GET /harness/profiles/{id}/{file}", s.handleProfileFile)
	m.HandleFunc("GET /metrics", s.handleMetrics)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	// Everything else the agent may send (metrics, metadata, check runs,
	// sketches, remote config...): swallow with 202 so a run can be fully
	// offline.
	m.HandleFunc("/", s.handleOther)
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.Addr, err)
	}
	s.http = &http.Server{Handler: s.mux, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}

	var tcpLn net.Listener
	if s.cfg.TCPAddr != "" {
		tcpLn, err = net.Listen("tcp", s.cfg.TCPAddr)
		if err != nil {
			ln.Close()
			return fmt.Errorf("listen tcp %s: %w", s.cfg.TCPAddr, err)
		}
		go s.serveTCP(ctx, tcpLn)
	}
	if s.agent != nil {
		if s.cfg.AgentTelemetryURL != "" {
			go s.agent.runTelemetry(ctx, s.cfg.ScrapeInterval, s.logf)
		}
		if s.cfg.DockerContainer != "" {
			go s.agent.runDocker(ctx, s.cfg.DockerSocket, s.logf)
		}
	}
	go s.ticker(ctx)
	if s.emitter != nil {
		go s.emitter.run(ctx)
		s.emitter.event("aoc: intake started", fmt.Sprintf("Run %s: intake listening on %s", s.cfg.Name, s.cfg.Addr), "info")
	}
	s.banner(ln.Addr().String(), tcpLn)

	errc := make(chan error, 1)
	go func() { errc <- s.http.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutCtx, c2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer c2()
	s.http.Shutdown(shutCtx)
	if tcpLn != nil {
		tcpLn.Close()
	}
	if s.emitter != nil {
		// One last batch so the final window state lands in Datadog.
		s.emitter.flush(shutCtx)
	}
	s.summary()
	return nil
}

func (s *Server) banner(addr string, tcpLn net.Listener) {
	if s.cfg.Quiet {
		return
	}
	_, port, _ := net.SplitHostPort(addr)
	fmt.Fprintf(s.out, "\nagent_of_chaos intake  name=%s\n", s.cfg.Name)
	fmt.Fprintf(s.out, "  logs (HTTP) http://<this-host>:%s/api/v2/logs   ← DD_LOGS_CONFIG_LOGS_DD_URL=http://<this-host>:%s\n", port, port)
	if tcpLn != nil {
		_, tp, _ := net.SplitHostPort(tcpLn.Addr().String())
		fmt.Fprintf(s.out, "  logs (TCP)  <this-host>:%s\n", tp)
	}
	fmt.Fprintf(s.out, "  report      http://localhost:%s/harness/report   metrics http://localhost:%s/metrics\n", port, port)
	if s.cfg.APIKey != "" {
		fmt.Fprintf(s.out, "  api key     required (%s)\n", maskKey(s.cfg.APIKey))
	}
	if s.cfg.AgentTelemetryURL != "" {
		fmt.Fprintf(s.out, "  observing   agent telemetry %s\n", s.cfg.AgentTelemetryURL)
	}
	if s.cfg.DockerContainer != "" {
		fmt.Fprintf(s.out, "  observing   docker stats for container %q\n", s.cfg.DockerContainer)
	}
	if s.emitter != nil {
		fmt.Fprintf(s.out, "  datadog     %s.* metrics + events → %s (tags %s)\n", s.cfg.Datadog.Prefix, s.emitter.client.Site, strings.Join(s.emitter.tags, ","))
	} else {
		fmt.Fprintf(s.out, "  datadog     off (set DD_API_KEY to submit %s.* metrics)\n", "aoc")
	}
	if f := s.faults.get(); f.Active() {
		fmt.Fprintf(s.out, "  faults      %s\n", f)
	}
	fmt.Fprintln(s.out)
}

// ticker rolls the timeseries every second and prints a status line every
// 5 s while traffic is flowing.
func (s *Server) ticker(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var prevLogs, prevWire int64
	var prevGen int64
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.proc.Sample()
			s.stats.tick(now)
			n++
			if n%5 != 0 || s.cfg.Quiet {
				continue
			}
			st := s.stats
			st.mu.Lock()
			logs, wireB := st.logs, st.wireBytes
			var gen int64
			for _, g := range st.gens {
				gen += g.Last.Totals.Records - g.base.Records
			}
			inflight := st.inflight.Load()
			e2e := st.ts.e2eOpen.summary()
			st.mu.Unlock()
			if logs == prevLogs && gen == prevGen {
				continue
			}
			cpu, mem := "n/a", "n/a"
			if s.agent != nil {
				if c, m, _, _ := s.agent.current(now); c >= 0 {
					cpu, mem = fmt.Sprintf("%.0f%%", c), fmtutil.Bytes(m)
				}
			}
			fmt.Fprintf(s.out, "  %s  recv=%-10s wire=%-10s gen=%-10s total=%-12s outstanding=%-9s e2e_p99=%-8s inflight=%-3d agent cpu=%s mem=%s\n",
				now.Format("15:04:05"), fmtutil.Rate(float64(logs-prevLogs)/5), fmtutil.BytesF(float64(wireB-prevWire)/5)+"/s",
				fmtutil.Rate(float64(gen-prevGen)/5), fmtutil.Int(logs), fmtutil.Int(max(gen-logs, 0)),
				fmtutil.Duration(time.Duration(e2e.P99*float64(time.Second))), inflight, cpu, mem)
			prevLogs, prevWire, prevGen = logs, wireB, gen
		}
	}
}

func (s *Server) summary() {
	if s.cfg.Quiet {
		return
	}
	r := s.buildReport(false)
	fmt.Fprintf(s.out, "\n  Done.  requests=%s  logs=%s  wire=%s  raw=%s  generated=%s  unique=%s  dups=%s  missing=%s\n\n",
		fmtutil.Int(r.HTTP.Requests), fmtutil.Int(r.Delivery.ReceivedLogs), fmtutil.Bytes(r.HTTP.WireBytes), fmtutil.Bytes(r.HTTP.RawBytes),
		fmtutil.Int(r.Delivery.GeneratedRecords), fmtutil.Int(r.Delivery.Unique), fmtutil.Int(r.Delivery.Duplicates), fmtutil.Int(r.Delivery.Missing))
}

// ── agent-facing handlers ────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.Encode(v)
}

func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("DD-API-KEY")
	if key == "" {
		key = r.URL.Query().Get("api_key")
	}
	s.stats.recordOther(r.URL.Path)
	if s.cfg.APIKey != "" && key != s.cfg.APIKey {
		writeJSON(w, 403, map[string]any{"errors": []string{"Forbidden"}})
		return
	}
	writeJSON(w, 200, map[string]bool{"valid": true})
}

func (s *Server) handleOther(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "POST", "PUT", "PATCH":
		io.Copy(io.Discard, io.LimitReader(r.Body, 64<<20))
		s.stats.recordOther(r.URL.Path)
		if s.cfg.Verbose {
			s.logf("%s %s → 202 (sink)", r.Method, r.URL.Path)
		}
		writeJSON(w, 202, map[string]any{})
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	s.stats.inflight.Add(1)
	defer s.stats.inflight.Add(-1)

	apiKey := r.Header.Get("DD-API-KEY")
	if apiKey == "" {
		apiKey = r.PathValue("key")
	}
	if apiKey == "" {
		apiKey = r.URL.Query().Get("api_key")
	}
	s.stats.recordHeaders(r.Header.Get("DD-EVP-ORIGIN-VERSION"), r.UserAgent(), r.Header.Get("DD-EVP-ORIGIN"), apiKey)
	encoding := strings.ToLower(r.Header.Get("Content-Encoding"))
	b := batch{path: r.URL.Path, encoding: encoding}
	now := time.Now()
	f := s.faults.get()

	finish := func(status int, reason string) {
		b.status = status
		b.procTime = time.Since(start)
		s.stats.merge(&b, now)
		if s.cfg.Verbose {
			s.logf("%s %s enc=%s wire=%d raw=%d logs=%d → %d %s", r.Method, r.URL.Path, encoding, b.wire, b.raw, b.logs, status, reason)
		}
	}
	reject := func(status int, reason, detail string) {
		s.stats.recordRejected(strconv.Itoa(status))
		writeJSON(w, status, map[string]any{"errors": []map[string]string{{"status": strconv.Itoa(status), "title": reason, "detail": detail}}})
		finish(status, reason)
	}

	if s.cfg.APIKey != "" && apiKey != s.cfg.APIKey {
		io.Copy(io.Discard, r.Body)
		reject(403, "Forbidden", "Invalid API key")
		return
	}

	// Drop: read the whole request (the agent finishes sending), then kill
	// the connection with no response. Not ingested.
	if f.Outage || (f.DropRate > 0 && rand.Float64() < f.DropRate) {
		n, _ := io.Copy(io.Discard, io.LimitReader(r.Body, s.cfg.MaxPayloadBytes*4))
		b.wire = n
		s.stats.recordFault("dropped")
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
			}
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		finish(0, "dropped")
		return
	}

	var body io.Reader = r.Body
	if f.ReadBps > 0 {
		body = &throttledReader{r: r.Body, bps: f.ReadBps, start: time.Now()}
		s.stats.recordFault("slowed")
	}
	raw, err := readLimited(body, s.cfg.MaxPayloadBytes*4)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			reject(413, "Payload Too Large", "compressed payload exceeds limit")
			return
		}
		finish(0, "read error: "+err.Error())
		return
	}
	b.wire = int64(len(raw))

	dec, err := decompress(raw, encoding, s.cfg.MaxPayloadBytes, !s.cfg.Strict)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			reject(413, "Payload Too Large", fmt.Sprintf("decompressed payload exceeds %d bytes", s.cfg.MaxPayloadBytes))
			return
		}
		if s.cfg.Strict {
			reject(400, "Bad Request", "cannot decode body: "+err.Error())
			return
		}
		b.malformed = true
		writeJSON(w, 202, map[string]any{})
		finish(202, "undecodable body (lenient)")
		return
	}
	b.raw = int64(len(dec))

	entries, kind := parsePayload(dec)
	switch kind {
	case kindProbe:
		b.probe = true
	case kindMalformed:
		if s.cfg.Strict {
			reject(400, "Bad Request", "body is not a JSON array of logs")
			return
		}
		b.malformed = true
	}

	if f.LatencyMs > 0 {
		d := time.Duration(f.LatencyMs) * time.Millisecond
		if f.JitterMs > 0 {
			d += time.Duration(rand.IntN(f.JitterMs+1)) * time.Millisecond
		}
		time.Sleep(d)
		s.stats.recordFault("delayed")
	}
	if f.ErrorRate > 0 && rand.Float64() < f.ErrorRate {
		status := f.ErrorStatus
		if status == 0 {
			status = 500
		}
		s.stats.recordFault("errored")
		writeJSON(w, status, map[string]any{"errors": []map[string]string{{"status": strconv.Itoa(status), "title": "injected fault"}}})
		finish(status, "injected fault")
		return
	}

	for i := range entries {
		analyze(&entries[i], &b, now)
	}
	writeJSON(w, 202, map[string]any{})
	finish(202, "ok")
}

// ── harness handlers ─────────────────────────────────────────────────────────

func (s *Server) handleGenReport(w http.ResponseWriter, r *http.Request) {
	var rep wire.GenReport
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&rep); err != nil {
		http.Error(w, "bad report: "+err.Error(), 400)
		return
	}
	if rep.Gen == "" {
		http.Error(w, "missing gen", 400)
		return
	}
	first := false
	s.stats.mu.Lock()
	_, known := s.stats.gens[rep.Gen]
	s.stats.mu.Unlock()
	first = !known
	s.stats.genReport(&rep, time.Now())
	if first {
		s.logf("[gen] %s registered: mode=%s format=%s output=%s streams=%d rate=%s", rep.Gen, rep.Mode, rep.Format, rep.Output, rep.ActiveStreams, fmtutil.Rate(rep.Knobs.Rate))
		s.emitter.event("aoc: generator "+rep.Gen+" started",
			fmt.Sprintf("mode=%s format=%s output=%s streams=%d rate=%s", rep.Mode, rep.Format, rep.Output, rep.ActiveStreams, fmtutil.Rate(rep.Knobs.Rate)), "info", "gen:"+rep.Gen)
	}
	if rep.Final {
		s.logf("[gen] %s finished: records=%s bytes=%s", rep.Gen, fmtutil.Int(rep.Totals.Records), fmtutil.Bytes(rep.Totals.Bytes))
		s.emitter.event("aoc: generator "+rep.Gen+" finished",
			fmt.Sprintf("records=%s bytes=%s rotations=%d", fmtutil.Int(rep.Totals.Records), fmtutil.Bytes(rep.Totals.Bytes), rep.Totals.Rotations), "info", "gen:"+rep.Gen)
	}
	w.WriteHeader(204)
}

// Status is the small live view `aoc run` polls while draining.
type Status struct {
	Now           time.Time `json:"now"`
	Name          string    `json:"name"`
	Version       string    `json:"version"`
	UptimeSeconds float64   `json:"uptime_seconds"`
	WindowSeconds float64   `json:"window_seconds"`
	Faults        Faults    `json:"faults"`
	FaultsActive  bool      `json:"faults_active"`
	Inflight      int64     `json:"inflight"`
	// RecentLogsPerSec is the receive rate over the last 10 s.
	RecentLogsPerSec float64        `json:"recent_logs_per_sec"`
	Missing          int64          `json:"missing"`
	Unique           int64          `json:"unique"`
	Generated        int64          `json:"generated"`
	Generators       []GenStatus    `json:"generators"`
	Observer         ObserverStatus `json:"observer"`
	Profiles         ProfileStatus  `json:"profiles"`
	DDMetrics        EmitterStatus  `json:"dd_metrics"`
}

// GenStatus is one generator's liveness.
type GenStatus struct {
	Name          string  `json:"name"`
	Mode          string  `json:"mode"`
	Phase         string  `json:"phase"`
	ActiveStreams int     `json:"active_streams"`
	TargetRate    float64 `json:"target_rate"`
	RecordRate    float64 `json:"record_rate"`
	AgoSeconds    float64 `json:"ago_seconds"`
	Final         bool    `json:"final"`
}

func (s *Server) status() *Status {
	rep := s.buildReport(false)
	now := time.Now()
	st := s.stats
	out := &Status{
		Now: now, Name: s.cfg.Name, Version: s.cfg.Version, UptimeSeconds: now.Sub(s.start).Seconds(),
		WindowSeconds: rep.Seconds, Inflight: st.inflight.Load(),
		Missing: rep.Delivery.Missing, Unique: rep.Delivery.Unique, Generated: rep.Delivery.GeneratedRecords,
	}
	out.Faults = s.faults.get()
	out.FaultsActive = out.Faults.Active()
	if s.agent != nil {
		out.Observer = s.agent.status()
	}
	if s.emitter != nil {
		out.DDMetrics = s.emitter.status()
	}
	out.Profiles = s.profiles.status()
	st.mu.Lock()
	pts := st.ts.since(now.Unix() - 10)
	for name, g := range st.gens {
		out.Generators = append(out.Generators, GenStatus{
			Name: name, Mode: g.Last.Mode, Phase: g.Last.Phase, ActiveStreams: g.Last.ActiveStreams, TargetRate: g.Last.Knobs.Rate,
			RecordRate: g.RecordRate, AgoSeconds: now.Sub(g.LastSeen).Seconds(), Final: g.Last.Final,
		})
	}
	st.mu.Unlock()
	sort.Slice(out.Generators, func(i, j int) bool { return out.Generators[i].Name < out.Generators[j].Name })
	if len(pts) > 0 {
		var logs int64
		for _, p := range pts {
			logs += p.Logs
		}
		out.RecentLogsPerSec = float64(logs) / float64(len(pts))
	}
	return out
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.status())
}

func (s *Server) handleTimeseries(w http.ResponseWriter, r *http.Request) {
	since := time.Now().Unix() - 600
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			since = n
		}
	}
	s.stats.mu.Lock()
	s.stats.ts.roll(time.Now(), s.stats)
	pts := s.stats.ts.since(since)
	s.stats.mu.Unlock()
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv")
		writeCSV(w, pts)
		return
	}
	writeJSON(w, 200, pts)
}

func writeCSV(w io.Writer, pts []Point) {
	fmt.Fprintln(w, "t,requests,logs,lines,raw_bytes,wire_bytes,err_4xx,err_5xx,dropped,gen_records,gen_lines,gen_bytes,e2e_p50,e2e_p99,e2e_max,sender_p50,sender_p99,agent_cpu,agent_mem,proc_cpu,proc_rss,inflight,agent_mem_anon,agent_mem_file")
	for _, p := range pts {
		fmt.Fprintf(w, "%d,%d,%d,%d,%d,%d,%d,%d,%d,%.1f,%.1f,%.1f,%.6f,%.6f,%.6f,%.6f,%.6f,%.2f,%d,%.2f,%d,%d,%d,%d\n",
			p.T, p.Requests, p.Logs, p.Lines, p.RawBytes, p.WireBytes, p.Err4xx, p.Err5xx, p.Dropped, p.GenRecords, p.GenLines, p.GenBytes,
			p.E2EP50, p.E2EP99, p.E2EMax, p.SenderP50, p.SenderP99, p.AgentCPU, p.AgentMem, p.ProcCPU, p.ProcRSS, p.Inflight, p.AgentAnon, p.AgentFile)
	}
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	gaps := r.URL.Query().Get("gaps") != "0"
	rep := s.buildReport(gaps)
	if name := r.URL.Query().Get("name"); name != "" {
		rep.Name = name
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(rep)
}

func (s *Server) setFaults(f Faults, desc string) {
	prev := s.faults.get()
	s.faults.set(f)
	if !f.Active() && !prev.Active() {
		return // clearing nothing is not an event
	}
	ev := FaultEvent{At: time.Now(), Faults: f, Desc: desc + ": " + f.String()}
	s.stats.mu.Lock()
	s.stats.faultLog = append(s.stats.faultLog, ev)
	if len(s.stats.faultLog) > 500 {
		s.stats.faultLog = s.stats.faultLog[len(s.stats.faultLog)-500:]
	}
	s.stats.mu.Unlock()
	s.logf("[faults] %s", ev.Desc)
	alert := "warning"
	if !f.Active() {
		alert = "success"
	}
	s.emitter.event("aoc: faults "+desc, ev.Desc, alert)
}

func (s *Server) handleFaultsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.faults.get())
}

func (s *Server) handleFaultsSet(w http.ResponseWriter, r *http.Request) {
	var f Faults
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&f); err != nil {
		http.Error(w, "bad faults: "+err.Error(), 400)
		return
	}
	if err := f.Validate(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.setFaults(f, "set")
	writeJSON(w, 200, f)
}

func (s *Server) handleFaultsClear(w http.ResponseWriter, r *http.Request) {
	s.setFaults(Faults{}, "clear")
	writeJSON(w, 200, Faults{})
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.stats.Reset()
	s.proc.Reset()
	s.profiles.reset()
	if name := r.URL.Query().Get("name"); name != "" {
		s.mu.Lock()
		s.cfg.Name = name
		s.mu.Unlock()
	}
	s.logf("[reset] counters zeroed (window starts now)")
	s.emitter.event("aoc: measurement window opened", fmt.Sprintf("Run %s: counters reset, window starts now", s.cfg.Name), "info")
	writeJSON(w, 200, map[string]any{"reset_at": time.Now()})
}
