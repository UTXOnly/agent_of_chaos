package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/report"
)

// Per-process view of the agent container: docker top (ps inside the
// container's pid namespace) every few seconds, summed by command name.
// Container memory is one number; this is what says which process it
// belongs to.

const topInterval = 10 * time.Second

type procSample struct {
	RSS     int64   // bytes
	CPUTime float64 // cumulative seconds
	CPUPct  float64 // over the last interval
}

// procAcc accumulates one process over the window. ps reports CPU time in
// whole seconds, so per-interval percentages are coarse; the window average
// comes from the first and last cumulative values instead.
type procAcc struct {
	rssSum, rssMax    int64
	cpuMax            float64
	firstCPU, lastCPU float64
	firstAt, lastAt   time.Time
	n                 int
}

// runTop polls /containers/{name}/top.
func (a *agentObs) runTop(ctx context.Context, socket string, logf func(string, ...any)) {
	client := dockerClient(socket)
	t := time.NewTicker(topInterval)
	defer t.Stop()
	var prev map[string]procSample
	var prevAt time.Time
	failed := false
	for {
		cur, err := a.top(ctx, client)
		if err != nil {
			if !failed && ctx.Err() == nil {
				logf("[observe] docker top for %q unavailable (%v)", a.dockerContainer, err)
				failed = true
			}
		} else {
			failed = false
			now := time.Now()
			if prev != nil {
				dt := now.Sub(prevAt).Seconds()
				for name, s := range cur {
					if p, ok := prev[name]; ok && dt > 0 && s.CPUTime >= p.CPUTime {
						s.CPUPct = (s.CPUTime - p.CPUTime) / dt * 100
						cur[name] = s
					}
				}
			}
			a.mu.Lock()
			a.procs, a.procsAt = cur, now
			for name, s := range cur {
				acc := a.procAcc[name]
				if acc == nil {
					acc = &procAcc{firstCPU: s.CPUTime, firstAt: now}
					a.procAcc[name] = acc
				}
				acc.rssSum += s.RSS
				if s.RSS > acc.rssMax {
					acc.rssMax = s.RSS
				}
				if prev != nil && s.CPUPct > acc.cpuMax {
					acc.cpuMax = s.CPUPct
				}
				acc.lastCPU, acc.lastAt = s.CPUTime, now
				acc.n++
			}
			a.mu.Unlock()
			prev, prevAt = cur, now
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// top returns the container's processes summed by command name.
func (a *agentObs) top(ctx context.Context, client *http.Client) (map[string]procSample, error) {
	url := "http://docker/containers/" + a.dockerContainer + "/top?ps_args=-eo%20pid,rss,cputimes,args"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Titles    []string   `json:"Titles"`
		Processes [][]string `json:"Processes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return parseTop(out.Titles, out.Processes)
}

// parseTop sums ps rows by command (the basename of the first word of
// args, which unlike comm is not cut at 15 characters). RSS is in KiB, TIME
// (cputimes) in seconds; a ps without those columns yields an error rather
// than zeros.
func parseTop(titles []string, rows [][]string) (map[string]procSample, error) {
	col := map[string]int{}
	for i, t := range titles {
		col[strings.ToUpper(t)] = i
	}
	iRSS, okRSS := col["RSS"]
	iTime, okTime := col["TIME"]
	iCmd, okCmd := col["COMMAND"]
	if !okCmd {
		iCmd, okCmd = col["COMM"]
	}
	if !okRSS || !okTime || !okCmd {
		return nil, fmt.Errorf("unexpected ps columns %v", titles)
	}
	procs := map[string]procSample{}
	for _, r := range rows {
		if len(r) <= iRSS || len(r) <= iTime || len(r) <= iCmd {
			continue
		}
		rss, _ := strconv.ParseInt(r[iRSS], 10, 64)
		secs, _ := strconv.ParseFloat(r[iTime], 64)
		name := procName(r[iCmd])
		s := procs[name]
		s.RSS += rss * 1024
		s.CPUTime += secs
		procs[name] = s
	}
	return procs, nil
}

// procName reduces a ps args column to a process name: the basename of the
// executable, e.g. "/opt/datadog-agent/bin/agent/agent run" → "agent".
func procName(args string) string {
	f := strings.Fields(args)
	if len(f) == 0 {
		return "?"
	}
	name := f[0]
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		name = f[0]
	}
	if len(name) > 40 {
		name = name[:40]
	}
	return name
}

// processes is the per-process summary since the last rebase, largest
// RSS first.
func (a *agentObs) processes() []report.ProcessStat {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]report.ProcessStat, 0, len(a.procAcc))
	for name, acc := range a.procAcc {
		if acc.n == 0 {
			continue
		}
		cpuAvg := 0.0
		if dt := acc.lastAt.Sub(acc.firstAt).Seconds(); dt > 0 && acc.lastCPU >= acc.firstCPU {
			cpuAvg = (acc.lastCPU - acc.firstCPU) / dt * 100
		}
		out = append(out, report.ProcessStat{Name: name, RSSAvg: acc.rssSum / int64(acc.n), RSSMax: acc.rssMax, CPUAvg: cpuAvg, CPUMax: acc.cpuMax, Samples: acc.n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RSSMax != out[j].RSSMax {
			return out[i].RSSMax > out[j].RSSMax
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// currentProcs is the latest per-process sample if fresh.
func (a *agentObs) currentProcs(at time.Time) map[string]procSample {
	a.mu.Lock()
	defer a.mu.Unlock()
	if at.Sub(a.procsAt) > 2*topInterval {
		return nil
	}
	out := make(map[string]procSample, len(a.procs))
	for k, v := range a.procs {
		out[k] = v
	}
	return out
}
