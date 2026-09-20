// Package prof reads the continuous-profiler uploads the intake kept for a
// run (pprof files per profiling period), merges them over the measured
// window and reduces them to per-function tables that two runs can be
// diffed on. It is what turns "memory went up 160%" into "in
// pkg/logs/…/tagfilter.(*Filter).compile".
package prof

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/pprof/profile"
	"github.com/klauspost/compress/zstd"

	"github.com/UTXOnly/agent_of_chaos/internal/fmtutil"
	"github.com/UTXOnly/agent_of_chaos/internal/report"
)

// Capture is one profiler upload: the pprof files of one period, as written
// by `aoc run` under <results>/profiles/<service>/<start>/.
type Capture struct {
	Service string    `json:"service"`
	Family  string    `json:"family,omitempty"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Tags    []string  `json:"tags,omitempty"`
	Dir     string    `json:"dir"`
	Files   []string  `json:"files"`
}

// Event is the event.json the profiler sends with every upload (the
// subset we use).
type Event struct {
	Start       string   `json:"start"`
	End         string   `json:"end"`
	Family      string   `json:"family"`
	Attachments []string `json:"attachments"`
	Tags        string   `json:"tags_profiler"`
}

// ParseEvent decodes an event.json.
func ParseEvent(b []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(b, &ev); err != nil {
		return ev, err
	}
	return ev, nil
}

// Service is the service:<x> tag of an upload, or "unknown".
func (ev Event) Service() string {
	for _, t := range strings.Split(ev.Tags, ",") {
		if strings.HasPrefix(t, "service:") {
			return strings.TrimPrefix(t, "service:")
		}
	}
	return "unknown"
}

// Times parses the upload's start/end.
func (ev Event) Times() (start, end time.Time) {
	start, _ = time.Parse(time.RFC3339Nano, ev.Start)
	end, _ = time.Parse(time.RFC3339Nano, ev.End)
	return
}

// LoadDir reads every capture under <runDir>/profiles.
func LoadDir(runDir string) ([]Capture, error) {
	root := filepath.Join(runDir, "profiles")
	events, err := filepath.Glob(filepath.Join(root, "*", "*", "event.json"))
	if err != nil {
		return nil, err
	}
	var caps []Capture
	for _, evPath := range events {
		b, err := os.ReadFile(evPath)
		if err != nil {
			continue
		}
		ev, err := ParseEvent(b)
		if err != nil {
			continue
		}
		dir := filepath.Dir(evPath)
		c := Capture{Service: ev.Service(), Family: ev.Family, Dir: dir}
		if ev.Tags != "" {
			c.Tags = strings.Split(ev.Tags, ",")
		}
		c.Start, c.End = ev.Times()
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".pprof") {
				c.Files = append(c.Files, e.Name())
			}
		}
		caps = append(caps, c)
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i].Start.Before(caps[j].Start) })
	return caps, nil
}

// Overlapping keeps the captures whose period overlaps [start, end].
func Overlapping(caps []Capture, start, end time.Time) []Capture {
	var out []Capture
	for _, c := range caps {
		if c.End.After(start) && c.Start.Before(end) {
			out = append(out, c)
		}
	}
	return out
}

// Kind is one number we can derive from a profile file: a sample type,
// normalised either per wall-clock second (rates: CPU, allocations) or per
// capture (levels: heap in use, goroutines).
type Kind struct {
	File       string  // cpu.pprof, delta-heap.pprof, …
	SampleType string  // cpu, inuse_space, …
	Rate       bool    // divide by wall seconds (else by captures)
	Label      string  // for tables
	Unit       string  // %, B, B/s, ""
	Scale      float64 // applied after normalisation
}

// Kinds are the profile views the comparison is built on, in the order they
// are reported. cpu is nanoseconds of CPU per wall second, scaled to percent
// of one core; the heap views are dd-trace-go's delta-heap profile, where
// inuse_* is the live heap at the end of the period and alloc_* the
// allocations during it.
var Kinds = []Kind{
	{"cpu.pprof", "cpu", true, "CPU", "%", 100 / 1e9},
	{"delta-heap.pprof", "inuse_space", false, "heap in use", "B", 1},
	{"delta-heap.pprof", "alloc_space", true, "allocation rate", "B/s", 1},
	{"goroutines.pprof", "goroutines", false, "goroutines", "", 1},
	{"goroutines.pprof", "goroutine", false, "goroutines", "", 1},
	{"block.pprof", "delay", true, "blocking (goroutine-seconds per second)", "", 1 / 1e9},
	{"mutex.pprof", "delay", true, "mutex wait (goroutine-seconds per second)", "", 1 / 1e9},
}

// Row is one function's share of a profile.
type Row struct {
	Function string
	Flat     float64 // samples whose leaf frame is this function
	Cum      float64 // samples with this function anywhere on the stack
}

// Analysis is one Kind over a set of captures.
type Analysis struct {
	Kind     Kind
	Service  string
	Captures int
	Wall     float64 // seconds the samples cover
	Total    float64 // normalised sum over every sample
	Rows     map[string]*Row
}

// Analyze merges every capture's file for each Kind and aggregates the
// samples by function. Kinds without data are left out.
func Analyze(caps []Capture) []*Analysis {
	var out []*Analysis
	byService := map[string][]Capture{}
	var services []string
	for _, c := range caps {
		if _, ok := byService[c.Service]; !ok {
			services = append(services, c.Service)
		}
		byService[c.Service] = append(byService[c.Service], c)
	}
	sort.Strings(services)
	for _, svc := range services {
		for _, k := range Kinds {
			if a := analyze(svc, byService[svc], k); a != nil {
				out = append(out, a)
			}
		}
	}
	return out
}

func analyze(service string, caps []Capture, k Kind) *Analysis {
	var profs []*profile.Profile
	var wall float64
	for _, c := range caps {
		has := false
		for _, f := range c.Files {
			if f == k.File {
				has = true
			}
		}
		if !has {
			continue
		}
		p, err := ReadFile(filepath.Join(c.Dir, k.File))
		if err != nil {
			continue
		}
		if idx(p, k.SampleType) < 0 {
			continue
		}
		profs = append(profs, p)
		switch {
		case k.File == "cpu.pprof" && p.DurationNanos > 0:
			wall += float64(p.DurationNanos) / 1e9
		case !c.End.IsZero() && c.End.After(c.Start):
			wall += c.End.Sub(c.Start).Seconds()
		case p.DurationNanos > 0:
			wall += float64(p.DurationNanos) / 1e9
		}
	}
	if len(profs) == 0 {
		return nil
	}
	merged, err := profile.Merge(profs)
	if err != nil {
		return nil
	}
	i := idx(merged, k.SampleType)
	if i < 0 {
		return nil
	}
	div := float64(len(profs))
	if k.Rate {
		if wall <= 0 {
			return nil
		}
		div = wall
	}
	a := &Analysis{Kind: k, Service: service, Captures: len(profs), Wall: wall, Rows: map[string]*Row{}}
	for _, s := range merged.Sample {
		v := float64(s.Value[i]) / div * k.Scale
		if v == 0 {
			continue
		}
		a.Total += v
		if len(s.Location) == 0 {
			continue
		}
		leaf := fnName(s.Location[0], true)
		a.row(leaf).Flat += v
		seen := map[string]bool{}
		for _, loc := range s.Location {
			for _, ln := range loc.Line {
				name := "?"
				if ln.Function != nil {
					name = ln.Function.Name
				}
				if !seen[name] {
					seen[name] = true
					a.row(name).Cum += v
				}
			}
		}
	}
	return a
}

// ReadFile parses a pprof file. dd-trace-go v2 uploads zstd-compressed
// profiles (still named .pprof); older versions and Go's runtime write
// gzip, which profile.Parse handles itself.
func ReadFile(path string) (*profile.Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) >= 4 && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd {
		dec, err := zstd.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		defer dec.Close()
		return profile.Parse(dec)
	}
	return profile.Parse(bytes.NewReader(b))
}

func (a *Analysis) row(name string) *Row {
	r := a.Rows[name]
	if r == nil {
		r = &Row{Function: name}
		a.Rows[name] = r
	}
	return r
}

func idx(p *profile.Profile, sampleType string) int {
	for i, st := range p.SampleType {
		if st.Type == sampleType {
			return i
		}
	}
	return -1
}

// fnName is the function of a location; leaf picks the innermost inlined
// frame (Line[0]), which is how pprof orders inlined lines.
func fnName(loc *profile.Location, leaf bool) string {
	if len(loc.Line) == 0 {
		return "?"
	}
	ln := loc.Line[0]
	if !leaf {
		ln = loc.Line[len(loc.Line)-1]
	}
	if ln.Function == nil {
		return "?"
	}
	return ln.Function.Name
}

// Top returns the n functions with the most flat value.
func (a *Analysis) Top(n int) []Row {
	rows := make([]Row, 0, len(a.Rows))
	for _, r := range a.Rows {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Flat != rows[j].Flat {
			return rows[i].Flat > rows[j].Flat
		}
		return rows[i].Function < rows[j].Function
	})
	if len(rows) > n {
		rows = rows[:n]
	}
	return rows
}

// Summary reduces an analysis to what report.json keeps.
func (a *Analysis) Summary(n int) report.ProfileSummary {
	s := report.ProfileSummary{Service: a.Service, File: a.Kind.File, SampleType: a.Kind.SampleType, Label: a.Kind.Label, Unit: a.Kind.Unit, Captures: a.Captures, WallSeconds: a.Wall, Total: a.Total}
	for _, r := range a.Top(n) {
		s.Top = append(s.Top, report.Frame{Function: r.Function, Flat: r.Flat, Cum: r.Cum})
	}
	return s
}

// Summaries is Analyze + Summary for every kind found.
func Summaries(caps []Capture, n int) []report.ProfileSummary {
	var out []report.ProfileSummary
	for _, a := range Analyze(caps) {
		out = append(out, a.Summary(n))
	}
	return out
}

// DiffRow is one function in two runs.
type DiffRow struct {
	Function string  `json:"function"`
	A        float64 `json:"a"`
	B        float64 `json:"b"`
	Delta    float64 `json:"delta"`
}

// Diff is two analyses of the same kind side by side.
type Diff struct {
	Service  string    `json:"service"`
	Label    string    `json:"label"`
	Unit     string    `json:"unit"`
	ATotal   float64   `json:"a_total"`
	BTotal   float64   `json:"b_total"`
	ACaps    int       `json:"a_captures"`
	BCaps    int       `json:"b_captures"`
	Rows     []DiffRow `json:"rows"` // largest |Δ| first
	Cum      bool      `json:"cum"`  // rows are cumulative values (else flat)
	Selected string    `json:"-"`
}

// Compare pairs the analyses of two runs by service and kind and lists the
// n functions whose flat value moved the most.
func Compare(a, b []*Analysis, n int) []Diff {
	var out []Diff
	for _, x := range a {
		for _, y := range b {
			if x.Service != y.Service || x.Kind != y.Kind {
				continue
			}
			out = append(out, diff(x, y, n, false))
		}
	}
	return out
}

func diff(x, y *Analysis, n int, cum bool) Diff {
	d := Diff{Service: x.Service, Label: x.Kind.Label, Unit: x.Kind.Unit, ATotal: x.Total, BTotal: y.Total, ACaps: x.Captures, BCaps: y.Captures, Cum: cum}
	names := map[string]bool{}
	for k := range x.Rows {
		names[k] = true
	}
	for k := range y.Rows {
		names[k] = true
	}
	val := func(a *Analysis, name string) float64 {
		r := a.Rows[name]
		if r == nil {
			return 0
		}
		if cum {
			return r.Cum
		}
		return r.Flat
	}
	for name := range names {
		av, bv := val(x, name), val(y, name)
		d.Rows = append(d.Rows, DiffRow{Function: name, A: av, B: bv, Delta: bv - av})
	}
	sort.Slice(d.Rows, func(i, j int) bool {
		di, dj := math.Abs(d.Rows[i].Delta), math.Abs(d.Rows[j].Delta)
		if di != dj {
			return di > dj
		}
		return d.Rows[i].Function < d.Rows[j].Function
	})
	if len(d.Rows) > n {
		d.Rows = d.Rows[:n]
	}
	return d
}

// Format renders a value of a kind for tables.
func Format(v float64, unit string) string {
	switch unit {
	case "%":
		return fmt.Sprintf("%.1f%%", v)
	case "B":
		return fmtutil.BytesF(v)
	case "B/s":
		return fmtutil.BytesF(v) + "/s"
	default:
		if math.Abs(v) >= 100 || v == math.Trunc(v) {
			return fmt.Sprintf("%.0f", v)
		}
		return fmt.Sprintf("%.2f", v)
	}
}

// FormatDelta renders a signed difference.
func FormatDelta(v float64, unit string) string {
	s := Format(math.Abs(v), unit)
	switch {
	case v > 0:
		return "+" + s
	case v < 0:
		return "−" + s
	}
	return "="
}

// ShortFunc trims a Go function name for tables: the package path is cut to
// its last element (github.com/DataDog/datadog-agent/pkg/logs/sender.(*Sender).run
// → sender.(*Sender).run) unless it is ambiguous.
func ShortFunc(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 && i < len(name)-1 {
		return name[i+1:]
	}
	return name
}
