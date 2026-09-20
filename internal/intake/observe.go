package intake

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MetricSeries is one telemetry series (name + label set) tracked over time.
type MetricSeries struct {
	Name    string    `json:"name"`
	Labels  string    `json:"labels"` // raw "{a="b",c="d"}" or ""
	Type    string    `json:"type"`   // counter | gauge | histogram | summary | untyped
	Value   float64   `json:"value"`
	First   float64   `json:"first"` // value at rebase (run start)
	FirstAt time.Time `json:"first_at"`
	LastAt  time.Time `json:"last_at"`
	Rate    float64   `json:"rate"` // per-second change over the last scrape interval (counters)
	prev    float64
	prevAt  time.Time
}

// agentObs holds what the intake observes about the agent under test: its
// container's CPU/memory from the Docker API and its internal telemetry.
type agentObs struct {
	mu sync.Mutex

	dockerContainer string
	dockerOK        bool
	dockerErr       string
	dockerAt        time.Time
	cpuPct          float64
	memBytes        int64
	memLimit        int64
	pids            int64

	telemetryURL string
	telemetryOK  bool
	telemetryErr string
	telemetryAt  time.Time
	metrics      map[string]*MetricSeries
	keep         *regexp.Regexp
	types        map[string]string

	procCPUPct float64 // from process_cpu_seconds_total deltas
	procRSS    int64
	procAt     time.Time
	scrapes    int64
}

// DefaultTelemetryFilter keeps the logs pipeline's own metrics plus the
// process-level series that describe the core agent.
const DefaultTelemetryFilter = `^(logs|process_cpu_seconds_total|process_resident_memory_bytes|process_open_fds|go_goroutines|go_memstats_(heap_inuse|sys|alloc)_bytes|go_gc_duration_seconds)`

func newAgentObs(telemetryURL, dockerContainer, filter string) (*agentObs, error) {
	if filter == "" {
		filter = DefaultTelemetryFilter
	}
	re, err := regexp.Compile(filter)
	if err != nil {
		return nil, fmt.Errorf("telemetry filter: %w", err)
	}
	return &agentObs{
		telemetryURL: telemetryURL, dockerContainer: dockerContainer,
		metrics: map[string]*MetricSeries{}, keep: re, types: map[string]string{},
	}, nil
}

// current returns the latest samples if they are fresh (<3s old), else -1s.
func (a *agentObs) current(at time.Time) (cpu float64, mem int64, procCPU float64, procRSS int64) {
	cpu, mem, procCPU, procRSS = -1, -1, -1, -1
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dockerOK && at.Sub(a.dockerAt) < 3*time.Second {
		cpu, mem = a.cpuPct, a.memBytes
	}
	if a.telemetryOK && at.Sub(a.procAt) < 3*time.Second {
		procCPU, procRSS = a.procCPUPct, a.procRSS
	}
	return
}

// rebase marks the current counter values as the run's starting point.
func (a *agentObs) rebase() {
	a.mu.Lock()
	now := time.Now()
	for _, m := range a.metrics {
		m.First, m.FirstAt = m.Value, now
	}
	a.mu.Unlock()
}

// Status is the observer state for the UI.
type ObserverStatus struct {
	DockerContainer string    `json:"docker_container,omitempty"`
	DockerOK        bool      `json:"docker_ok"`
	DockerError     string    `json:"docker_error,omitempty"`
	DockerAt        time.Time `json:"docker_at,omitempty"`
	CPUPercent      float64   `json:"cpu_percent"`
	MemBytes        int64     `json:"mem_bytes"`
	MemLimit        int64     `json:"mem_limit"`
	PIDs            int64     `json:"pids"`
	TelemetryURL    string    `json:"telemetry_url,omitempty"`
	TelemetryOK     bool      `json:"telemetry_ok"`
	TelemetryError  string    `json:"telemetry_error,omitempty"`
	TelemetryAt     time.Time `json:"telemetry_at,omitempty"`
	Scrapes         int64     `json:"scrapes"`
	ProcCPUPercent  float64   `json:"proc_cpu_percent"`
	ProcRSS         int64     `json:"proc_rss"`
	Series          int       `json:"series"`
}

func (a *agentObs) status() ObserverStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return ObserverStatus{
		DockerContainer: a.dockerContainer, DockerOK: a.dockerOK, DockerError: a.dockerErr, DockerAt: a.dockerAt,
		CPUPercent: a.cpuPct, MemBytes: a.memBytes, MemLimit: a.memLimit, PIDs: a.pids,
		TelemetryURL: a.telemetryURL, TelemetryOK: a.telemetryOK, TelemetryError: a.telemetryErr, TelemetryAt: a.telemetryAt,
		Scrapes: a.scrapes, ProcCPUPercent: a.procCPUPct, ProcRSS: a.procRSS, Series: len(a.metrics),
	}
}

// series returns a sorted copy of every tracked telemetry series.
func (a *agentObs) series() []MetricSeries {
	a.mu.Lock()
	out := make([]MetricSeries, 0, len(a.metrics))
	for _, m := range a.metrics {
		out = append(out, *m)
	}
	a.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Labels < out[j].Labels
	})
	return out
}

// ── telemetry scraping ───────────────────────────────────────────────────────

func (a *agentObs) runTelemetry(ctx context.Context, interval time.Duration, logf func(string, ...any)) {
	client := &http.Client{Timeout: 5 * time.Second}
	t := time.NewTicker(interval)
	defer t.Stop()
	wasOK := false
	for {
		err := a.scrape(ctx, client)
		a.mu.Lock()
		if err != nil {
			a.telemetryOK, a.telemetryErr = false, err.Error()
		} else {
			a.telemetryOK, a.telemetryErr, a.telemetryAt = true, "", time.Now()
			a.scrapes++
		}
		ok := a.telemetryOK
		a.mu.Unlock()
		if ok != wasOK {
			if ok {
				logf("[observe] agent telemetry reachable at %s", a.telemetryURL)
			} else {
				logf("[observe] agent telemetry unavailable (%v); retrying every %s", err, interval)
			}
			wasOK = ok
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *agentObs) scrape(ctx context.Context, client *http.Client) error {
	req, err := http.NewRequestWithContext(ctx, "GET", a.telemetryURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	now := time.Now()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	a.mu.Lock()
	defer a.mu.Unlock()
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		if line[0] == '#' {
			if strings.HasPrefix(line, "# TYPE ") {
				f := strings.Fields(line[7:])
				if len(f) == 2 {
					a.types[f[0]] = f[1]
				}
			}
			continue
		}
		name, labels, value, ok := parseSample(line)
		if !ok || !a.keep.MatchString(name) {
			continue
		}
		key := name + labels
		m := a.metrics[key]
		if m == nil {
			if len(a.metrics) >= 5000 {
				continue
			}
			typ := a.types[name]
			if typ == "" {
				// histogram/summary children carry the parent's TYPE
				for _, suf := range []string{"_bucket", "_sum", "_count"} {
					if t, ok := a.types[strings.TrimSuffix(name, suf)]; ok {
						typ = t
					}
				}
				if typ == "" {
					typ = "untyped"
				}
			}
			m = &MetricSeries{Name: name, Labels: labels, Type: typ, First: value, FirstAt: now}
			a.metrics[key] = m
		} else if !m.prevAt.IsZero() {
			if dt := now.Sub(m.prevAt).Seconds(); dt > 0 && (m.Type == "counter" || strings.HasSuffix(name, "_total")) {
				m.Rate = (value - m.prev) / dt
			}
		}
		m.prev, m.prevAt = m.Value, m.LastAt
		if m.prevAt.IsZero() {
			m.prev, m.prevAt = value, now
		}
		m.Value, m.LastAt = value, now
	}
	if m := a.metrics["process_cpu_seconds_total"]; m != nil && m.Rate >= 0 && !m.prevAt.IsZero() {
		a.procCPUPct = m.Rate * 100
		a.procAt = now
	}
	if m := a.metrics["process_resident_memory_bytes"]; m != nil {
		a.procRSS = int64(m.Value)
		a.procAt = now
	}
	return sc.Err()
}

// parseSample parses `name{labels} value [timestamp]`.
func parseSample(line string) (name, labels string, value float64, ok bool) {
	i := strings.IndexAny(line, "{ ")
	if i <= 0 {
		return
	}
	name = line[:i]
	rest := line[i:]
	if rest[0] == '{' {
		j := strings.IndexByte(rest, '}')
		if j < 0 {
			return
		}
		labels = rest[:j+1]
		rest = rest[j+1:]
	}
	f := strings.Fields(rest)
	if len(f) == 0 {
		return
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return
	}
	return name, labels, v, true
}

// ── docker stats ─────────────────────────────────────────────────────────────

type dockerStats struct {
	Read     time.Time `json:"read"`
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs     uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	PidsStats struct {
		Current uint64 `json:"current"`
	} `json:"pids_stats"`
}

// runDocker streams /containers/{name}/stats and keeps the latest sample.
func (a *agentObs) runDocker(ctx context.Context, socket string, logf func(string, ...any)) {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	backoff := time.Second
	wasOK := false
	for {
		err := a.streamDocker(ctx, client, logf, &wasOK)
		if ctx.Err() != nil {
			return
		}
		a.mu.Lock()
		a.dockerOK = false
		if err != nil {
			a.dockerErr = err.Error()
		}
		a.mu.Unlock()
		if wasOK || backoff == time.Second {
			logf("[observe] docker stats for %q unavailable (%v); retrying in %s", a.dockerContainer, err, backoff)
		}
		wasOK = false
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

func (a *agentObs) streamDocker(ctx context.Context, client *http.Client, logf func(string, ...any), wasOK *bool) error {
	url := "http://docker/containers/" + a.dockerContainer + "/stats?stream=true"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var st dockerStats
		if err := dec.Decode(&st); err != nil {
			return err
		}
		cpu := 0.0
		cpuDelta := float64(st.CPUStats.CPUUsage.TotalUsage) - float64(st.PreCPUStats.CPUUsage.TotalUsage)
		sysDelta := float64(st.CPUStats.SystemCPUUsage) - float64(st.PreCPUStats.SystemCPUUsage)
		if cpuDelta > 0 && sysDelta > 0 {
			n := float64(st.CPUStats.OnlineCPUs)
			if n == 0 {
				n = 1
			}
			cpu = cpuDelta / sysDelta * n * 100
		}
		mem := st.MemoryStats.Usage
		if v, ok := st.MemoryStats.Stats["inactive_file"]; ok && v < mem { // cgroup v2
			mem -= v
		} else if v, ok := st.MemoryStats.Stats["total_inactive_file"]; ok && v < mem { // cgroup v1
			mem -= v
		}
		a.mu.Lock()
		first := !a.dockerOK
		a.dockerOK, a.dockerErr, a.dockerAt = true, "", time.Now()
		a.cpuPct, a.memBytes, a.memLimit, a.pids = cpu, int64(mem), int64(st.MemoryStats.Limit), int64(st.PidsStats.Current)
		a.mu.Unlock()
		if first && !*wasOK {
			logf("[observe] docker stats streaming for container %q", a.dockerContainer)
			*wasOK = true
		}
	}
}
