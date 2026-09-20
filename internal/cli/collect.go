package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/intake"
	"github.com/UTXOnly/agent_of_chaos/internal/prof"
	"github.com/UTXOnly/agent_of_chaos/internal/report"
)

// What `aoc run` collects besides the intake's report: the agent's own
// profiles (tee'd by the intake) and a digest of its log.

// profilesList is GET /harness/profiles.
type profilesList struct {
	Status  intake.ProfileStatus   `json:"status"`
	Uploads []intake.ProfileUpload `json:"uploads"`
}

func listProfiles(intakeURL string) (*profilesList, error) {
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(intakeURL + "/harness/profiles")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out profilesList
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// waitProfiles gives the profiler time to upload the period that covers
// the end of the window: uploads happen at period boundaries, so the last
// one can land up to a period after the window closed. Returns as soon as
// an upload ends at or after windowEnd, or after the timeout. When nothing
// has been uploaded at all, it gives up quickly (profiling is off).
func waitProfiles(ctx context.Context, intakeURL string, windowEnd time.Time, timeout time.Duration, logf func(string, ...any)) {
	deadline := time.Now().Add(timeout)
	announced := false
	for {
		l, err := listProfiles(intakeURL)
		if err != nil {
			return
		}
		if len(l.Uploads) == 0 && l.Status.Received == 0 && time.Since(windowEnd) > 15*time.Second {
			return
		}
		for _, u := range l.Uploads {
			if !u.End.Before(windowEnd.Add(-2 * time.Second)) {
				return
			}
		}
		if time.Now().After(deadline) {
			logf("no profile upload covered the end of the window within %s; using the %d received", timeout, len(l.Uploads))
			return
		}
		if !announced {
			logf("waiting for the agent's last profile upload (up to %s)", timeout)
			announced = true
		}
		if !sleepCtx(ctx, 3*time.Second) {
			return
		}
	}
}

// collectProfiles downloads every held upload that overlaps the window into
// <dir>/profiles/<service>/<start>/ and returns the captures.
func collectProfiles(intakeURL, dir string, start, end time.Time) ([]prof.Capture, intake.ProfileStatus, error) {
	l, err := listProfiles(intakeURL)
	if err != nil {
		return nil, intake.ProfileStatus{}, err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	var caps []prof.Capture
	for _, u := range l.Uploads {
		if !(u.End.After(start) && u.Start.Before(end)) {
			continue
		}
		cdir := filepath.Join(dir, "profiles", sanitize(u.Service), u.Start.UTC().Format("20060102T150405Z"))
		if err := os.MkdirAll(cdir, 0o755); err != nil {
			return nil, l.Status, err
		}
		c := prof.Capture{Service: u.Service, Family: u.Family, Start: u.Start, End: u.End, Tags: u.Tags, Dir: cdir}
		for _, f := range u.Files {
			resp, err := client.Get(fmt.Sprintf("%s/harness/profiles/%d/%s", intakeURL, u.ID, f.Name))
			if err != nil {
				return nil, l.Status, err
			}
			b, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != 200 {
				continue
			}
			if err := os.WriteFile(filepath.Join(cdir, f.Name), b, 0o644); err != nil {
				return nil, l.Status, err
			}
			if strings.HasSuffix(f.Name, ".pprof") {
				c.Files = append(c.Files, f.Name)
			}
		}
		caps = append(caps, c)
	}
	return caps, l.Status, nil
}

// ── agent log ────────────────────────────────────────────────────────────────

var (
	agentLogLine = regexp.MustCompile(`^\S+ \S+ \S+ \| (\S+) \| (\S+) \| \(([^)]+)\) \| (.*)$`)
	logNumbers   = regexp.MustCompile(`\b[0-9][0-9a-fx.:\-]*`)
)

// summarizeAgentLog counts levels and repeated warnings/errors in an
// agent.log capture.
func summarizeAgentLog(b []byte, tailLimit int) *report.LogSummary {
	s := &report.LogSummary{}
	counts := map[string]int64{}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		s.Lines++
		m := agentLogLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		level := m[2]
		switch level {
		case "ERROR", "CRITICAL":
			s.Errors++
		case "WARN", "WARNING":
			s.Warnings++
		default:
			continue
		}
		msg := logNumbers.ReplaceAllString(m[4], "#")
		if len(msg) > 160 {
			msg = msg[:157] + "…"
		}
		counts[level+" | "+m[1]+" | "+msg]++
	}
	if tailLimit > 0 && s.Lines >= int64(tailLimit) {
		s.Truncated = true
	}
	for k, n := range counts {
		s.Top = append(s.Top, report.NameCount{Name: k, Count: n})
	}
	sort.Slice(s.Top, func(i, j int) bool {
		if s.Top[i].Count != s.Top[j].Count {
			return s.Top[i].Count > s.Top[j].Count
		}
		return s.Top[i].Name < s.Top[j].Name
	})
	if len(s.Top) > 10 {
		s.Top = s.Top[:10]
	}
	return s
}
