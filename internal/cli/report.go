package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/report"
)

func init() {
	register(command{name: "report", short: "pull a report from a running intake and write report.json + report.md", run: runReport})
	register(command{name: "compare", short: "diff two or more reports side by side (Markdown)", run: runCompare})
}

// fetchReport downloads the JSON report (with gap analysis) from an intake.
func fetchReport(intakeURL, name string) (*report.Report, []byte, error) {
	u := strings.TrimRight(intakeURL, "/") + "/harness/report?gaps=1"
	if name != "" {
		u += "&name=" + name
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != 200 {
		return nil, nil, fmt.Errorf("%s → HTTP %d: %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var r report.Report
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, nil, fmt.Errorf("decode report: %w", err)
	}
	return &r, body, nil
}

// fetchReportFor is fetchReport without the raw bytes.
func fetchReportFor(intakeURL, name string) (*report.Report, error) {
	r, _, err := fetchReport(intakeURL, name)
	return r, err
}

func fetchTimeseriesCSV(intakeURL string, since int64) ([]byte, error) {
	u := fmt.Sprintf("%s/harness/timeseries?format=csv&since=%d", strings.TrimRight(intakeURL, "/"), since)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// writeReportFiles writes report.json, report.md and timeseries.csv into dir.
func writeReportFiles(dir string, r *report.Report, raw []byte, csv []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), raw, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(report.Markdown(r)), 0o644); err != nil {
		return err
	}
	if len(csv) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "timeseries.csv"), csv, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func runReport(args []string) int {
	fs := newFlagSet("report",
		"Fetch the current measurement window from a running intake and write\n"+
			"report.json (machine-readable, for aoc compare), report.md and timeseries.csv.",
		"  aoc report --intake http://localhost:8282 --out results/baseline\n"+
			"  aoc report --intake http://localhost:8282 --stdout | less")
	intake := fs.Str("intake", "http://localhost:8282", "intake base URL")
	name := fs.Str("name", "", "run name to stamp into the report (default: the intake's --name)")
	out := fs.Str("out", "", "output directory (default results/<name>-<timestamp>)")
	stdout := fs.Bool("stdout", false, "print the Markdown report instead of writing files")
	if !fs.parse(args) {
		return 2
	}
	r, raw, err := fetchReport(*intake, *name)
	if err != nil {
		return fail("report: %v", err)
	}
	if *stdout {
		fmt.Print(report.Markdown(r))
		return 0
	}
	csv, err := fetchTimeseriesCSV(*intake, r.WindowStart.Unix())
	if err != nil {
		fmt.Fprintf(os.Stderr, "aoc: timeseries download failed: %v\n", err)
	}
	dir := *out
	if dir == "" {
		dir = filepath.Join("results", fmt.Sprintf("%s-%s", sanitize(r.Name), time.Now().Format("20060102-150405")))
	}
	if err := writeReportFiles(dir, r, raw, csv); err != nil {
		return fail("report: %v", err)
	}
	fmt.Printf("wrote %s/report.json, report.md, timeseries.csv\n\n", dir)
	printVerdict(r)
	fmt.Printf("notebook: aoc notebook --results %s\n", dir)
	return 0
}

func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, s)
	if s == "" {
		return "run"
	}
	return s
}

// printVerdict prints the handful of numbers that answer "did it work".
func printVerdict(r *report.Report) {
	d, t, l := r.Delivery, r.Throughput, r.Latency
	fmt.Printf("  %-28s %s\n", "generated records", fmtInt(d.GeneratedRecords))
	fmt.Printf("  %-28s %s (%s)\n", "delivered unique", fmtInt(d.Unique), pct(d.Unique, d.GeneratedRecords))
	label := "lost"
	if !d.AllFinal {
		label = "missing (still running)"
	}
	fmt.Printf("  %-28s %s   duplicates %s   out of order %s   orphans %s\n", label, fmtInt(d.Missing), fmtInt(d.Duplicates), fmtInt(d.OutOfOrder), fmtInt(d.Orphans))
	fmt.Printf("  %-28s gen %s   recv %s   wire %s/s   compression %.1f×\n", "throughput", rate(t.GenRecordsPerSec), rate(t.RecvLogsPerSec), bytesF(t.RecvWireBytesPerSec), t.CompressionRatio)
	fmt.Printf("  %-28s p50 %s   p99 %s   max %s\n", "latency written→intake", secs(l.EndToEnd.P50), secs(l.EndToEnd.P99), secs(l.EndToEnd.Max))
	res := r.Resources
	if res.ContainerCPUMax > 0 {
		fmt.Printf("  %-28s cpu avg %.0f%% max %.0f%%   mem avg %s max %s\n", "agent container", res.ContainerCPUAvg, res.ContainerCPUMax, bytesF(float64(res.ContainerMemAvg)), bytesF(float64(res.ContainerMemMax)))
	}
	if res.ProcessCPUMax > 0 {
		fmt.Printf("  %-28s cpu avg %.0f%% max %.0f%%   rss max %s   %.2f cpu-s per 1M logs\n", "core agent process", res.ProcessCPUAvg, res.ProcessCPUMax, bytesF(float64(res.ProcessRSSMax)), res.CPUSecondsPerMLogs)
	}
	fmt.Println()
}

func runCompare(args []string) int {
	fs := newFlagSet("compare",
		"Put two or more reports side by side. The first is the baseline; every\n"+
			"other column shows its delta. Arguments are report.json files or the\n"+
			"directories aoc report / aoc run wrote them to.",
		"  aoc compare results/baseline results/tag-filters\n"+
			"  aoc compare a/report.json b/report.json c/report.json --out compare.md")
	out := fs.Str("out", "", "write the Markdown here instead of stdout")
	if !fs.parse(args) {
		return 2
	}
	if fs.NArg() < 2 {
		return fail("compare: need at least two reports")
	}
	var reports []*report.Report
	for _, a := range fs.Args() {
		path := a
		if st, err := os.Stat(a); err == nil && st.IsDir() {
			path = filepath.Join(a, "report.json")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return fail("compare: %v", err)
		}
		var r report.Report
		if err := json.Unmarshal(b, &r); err != nil {
			return fail("compare: %s: %v", path, err)
		}
		if r.Name == "" || r.Name == "aoc" {
			r.Name = filepath.Base(filepath.Dir(path))
		}
		reports = append(reports, &r)
	}
	md := report.Compare(reports)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
			return fail("compare: %v", err)
		}
		fmt.Printf("wrote %s\n", *out)
		return 0
	}
	fmt.Print(md)
	return 0
}
