package gen

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// PhaseConfig is the live, lock-free knob set every stream reads on each
// pacing iteration. The conductor mutates it as phases change.
type PhaseConfig struct {
	aggregateRate atomic.Uint64 // float64 bits, lines/s across all streams (0 = unlimited)
	activeStreams atomic.Int64
	burstSize     atomic.Int64
	burstInterval atomic.Int64 // nanoseconds
	multilineRate atomic.Uint64
	wideLineRate  atomic.Uint64
}

func (p *PhaseConfig) AggregateRate() float64 { return math.Float64frombits(p.aggregateRate.Load()) }
func (p *PhaseConfig) SetAggregateRate(r float64) {
	p.aggregateRate.Store(math.Float64bits(math.Max(r, 0)))
}
func (p *PhaseConfig) BurstSize() int     { return int(p.burstSize.Load()) }
func (p *PhaseConfig) SetBurstSize(n int) { p.burstSize.Store(int64(n)) }
func (p *PhaseConfig) BurstInterval() time.Duration {
	return time.Duration(p.burstInterval.Load())
}
func (p *PhaseConfig) SetBurstInterval(d time.Duration) { p.burstInterval.Store(int64(d)) }
func (p *PhaseConfig) MultilineRate() float64 {
	return math.Float64frombits(p.multilineRate.Load())
}
func (p *PhaseConfig) SetMultilineRate(r float64) {
	p.multilineRate.Store(math.Float64bits(clamp01(r)))
}
func (p *PhaseConfig) WideLineRate() float64     { return math.Float64frombits(p.wideLineRate.Load()) }
func (p *PhaseConfig) SetWideLineRate(r float64) { p.wideLineRate.Store(math.Float64bits(clamp01(r))) }

// StreamRate is the per-stream pacing target: aggregate ÷ active streams.
func (p *PhaseConfig) StreamRate() float64 {
	agg := p.AggregateRate()
	if agg <= 0 {
		return 0
	}
	n := p.activeStreams.Load()
	if n <= 0 {
		n = 1
	}
	return agg / float64(n)
}

func clamp01(x float64) float64 { return math.Min(math.Max(x, 0), 1) }

// Snapshot is a plain copy of the live knobs, for status output.
type Snapshot struct {
	Rate          float64       `json:"rate"`
	BurstSize     int           `json:"burst_size"`
	BurstInterval time.Duration `json:"burst_interval"`
	MultilineRate float64       `json:"multiline_rate"`
	WideLineRate  float64       `json:"wide_line_rate"`
}

func (p *PhaseConfig) Snapshot() Snapshot {
	return Snapshot{
		Rate: p.AggregateRate(), BurstSize: p.BurstSize(), BurstInterval: p.BurstInterval(),
		MultilineRate: p.MultilineRate(), WideLineRate: p.WideLineRate(),
	}
}

// Duration is a time.Duration that unmarshals from "90s", "10m", "1h30m", a
// bare number (seconds) or a YAML/JSON number.
type Duration time.Duration

func (d Duration) D() time.Duration             { return time.Duration(d) }
func (d Duration) String() string               { return time.Duration(d).String() }
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }
func (d Duration) MarshalYAML() (any, error)    { return d.String(), nil }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	return d.set(v)
}

func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var v any
	if err := unmarshal(&v); err != nil {
		return err
	}
	return d.set(v)
}

func (d *Duration) set(v any) error {
	switch x := v.(type) {
	case float64:
		*d = Duration(x * float64(time.Second))
	case int:
		*d = Duration(time.Duration(x) * time.Second)
	case int64:
		*d = Duration(time.Duration(x) * time.Second)
	case string:
		p, err := ParseDuration(x)
		if err != nil {
			return err
		}
		*d = Duration(p)
	case nil:
		*d = 0
	default:
		return fmt.Errorf("cannot parse duration from %T", v)
	}
	return nil
}

// ParseDuration accepts Go durations ("1h30m", "500ms") and bare numbers of
// seconds ("90", "0.5").
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(f * float64(time.Second)), nil
	}
	return time.ParseDuration(s)
}

// Phase is one step of a scenario. Unset (nil) fields inherit the running
// value, so a phase can change just the rate, or just the stream count.
type Phase struct {
	Name          string    `yaml:"name" json:"name"`
	Duration      Duration  `yaml:"duration" json:"duration"`
	Streams       *int      `yaml:"streams,omitempty" json:"streams,omitempty"`
	Rate          *float64  `yaml:"rate,omitempty" json:"rate,omitempty"` // aggregate lines/s (0 = unlimited)
	BurstSize     *int      `yaml:"burst_size,omitempty" json:"burst_size,omitempty"`
	BurstInterval *Duration `yaml:"burst_interval,omitempty" json:"burst_interval,omitempty"`
	MultilineRate *float64  `yaml:"multiline_rate,omitempty" json:"multiline_rate,omitempty"`
	WideLineRate  *float64  `yaml:"wide_line_rate,omitempty" json:"wide_line_rate,omitempty"`
	Transition    string    `yaml:"transition,omitempty" json:"transition,omitempty"` // "" | immediate | gradual
	TransitionSec Duration  `yaml:"transition_sec,omitempty" json:"transition_sec,omitempty"`
}

// Scenario is an ordered list of phases, optionally looping forever.
type Scenario struct {
	Name        string  `yaml:"name" json:"name"`
	Description string  `yaml:"description,omitempty" json:"description,omitempty"`
	Loop        bool    `yaml:"loop" json:"loop"`
	Phases      []Phase `yaml:"phases" json:"phases"`
}

// TotalDuration is the wall time of one pass through the phases.
func (s *Scenario) TotalDuration() time.Duration {
	var t time.Duration
	for _, p := range s.Phases {
		t += p.Duration.D()
	}
	return t
}

func (s *Scenario) Validate() error {
	if len(s.Phases) == 0 {
		return fmt.Errorf("scenario %q has no phases", s.Name)
	}
	for i, p := range s.Phases {
		if p.Duration < 0 {
			return fmt.Errorf("phase %d (%s): negative duration", i, p.Name)
		}
		switch p.Transition {
		case "", "immediate", "gradual":
		default:
			return fmt.Errorf("phase %d (%s): transition must be immediate or gradual", i, p.Name)
		}
		if p.Streams != nil && *p.Streams < 0 {
			return fmt.Errorf("phase %d (%s): negative streams", i, p.Name)
		}
	}
	return nil
}

func iptr(i int) *int                { return &i }
func fptr(f float64) *float64        { return &f }
func dptr(d time.Duration) *Duration { x := Duration(d); return &x }

// Builtin scenarios. Rates are aggregate lines/s across all streams.
var Builtin = map[string]*Scenario{
	"wave": {
		Name:        "wave",
		Description: "20-min cycle alternating a quiet trough and a busy peak; streams grow and shrink each swing",
		Loop:        true,
		Phases: []Phase{
			{Name: "trough", Duration: Duration(10 * time.Minute), Streams: iptr(2), Rate: fptr(400),
				BurstSize: iptr(200), BurstInterval: dptr(60 * time.Second), Transition: "gradual", TransitionSec: Duration(90 * time.Second)},
			{Name: "peak", Duration: Duration(10 * time.Minute), Streams: iptr(8), Rate: fptr(8000),
				BurstSize: iptr(1500), BurstInterval: dptr(5 * time.Second), Transition: "gradual", TransitionSec: Duration(90 * time.Second)},
		},
	},
	"business-day": {
		Name:        "business-day",
		Description: "~3 h cycle: overnight quiet → morning ramp → peak → lunch dip → afternoon surge → EOD (use --scenario-speed to compress)",
		Loop:        true,
		Phases: []Phase{
			{Name: "overnight", Duration: Duration(15 * time.Minute), Streams: iptr(1), Rate: fptr(50), BurstSize: iptr(50), BurstInterval: dptr(120 * time.Second)},
			{Name: "morning-ramp", Duration: Duration(30 * time.Minute), Streams: iptr(7), Rate: fptr(3000), BurstSize: iptr(600), BurstInterval: dptr(15 * time.Second), Transition: "gradual", TransitionSec: Duration(10 * time.Minute)},
			{Name: "morning-peak", Duration: Duration(60 * time.Minute), Streams: iptr(10), Rate: fptr(8000), BurstSize: iptr(1200), BurstInterval: dptr(5 * time.Second), Transition: "gradual", TransitionSec: Duration(5 * time.Minute)},
			{Name: "lunch-dip", Duration: Duration(20 * time.Minute), Streams: iptr(4), Rate: fptr(1200), BurstSize: iptr(300), BurstInterval: dptr(30 * time.Second), Transition: "gradual", TransitionSec: Duration(5 * time.Minute)},
			{Name: "afternoon-surge", Duration: Duration(60 * time.Minute), Streams: iptr(12), Rate: fptr(12000), BurstSize: iptr(2000), BurstInterval: dptr(4 * time.Second), Transition: "gradual", TransitionSec: Duration(5 * time.Minute)},
			{Name: "eod-cooldown", Duration: Duration(30 * time.Minute), Streams: iptr(3), Rate: fptr(600), BurstSize: iptr(200), BurstInterval: dptr(60 * time.Second), Transition: "gradual", TransitionSec: Duration(15 * time.Minute)},
		},
	},
	"incident": {
		Name:        "incident",
		Description: "45-min cycle: normal ops → sudden incident spike (many streams, heavy bursts) → slow recovery",
		Loop:        true,
		Phases: []Phase{
			{Name: "normal", Duration: Duration(15 * time.Minute), Streams: iptr(4), Rate: fptr(1500), BurstSize: iptr(400), BurstInterval: dptr(20 * time.Second)},
			{Name: "incident-onset", Duration: Duration(2 * time.Minute), Streams: iptr(14), Rate: fptr(25000), BurstSize: iptr(4000), BurstInterval: dptr(2 * time.Second), MultilineRate: fptr(0.35), Transition: "gradual", TransitionSec: Duration(30 * time.Second)},
			{Name: "incident-peak", Duration: Duration(10 * time.Minute), Streams: iptr(14), Rate: fptr(25000), BurstSize: iptr(4000), BurstInterval: dptr(2 * time.Second), MultilineRate: fptr(0.35)},
			{Name: "recovery", Duration: Duration(18 * time.Minute), Streams: iptr(4), Rate: fptr(1800), BurstSize: iptr(500), BurstInterval: dptr(15 * time.Second), MultilineRate: fptr(0.2), Transition: "gradual", TransitionSec: Duration(15 * time.Minute)},
		},
	},
	"longhaul": {
		Name:        "longhaul",
		Description: "6-hour arc with no sharp edges: slow warmup → sustained peak → churn → sustained low → overnight (use --scenario-speed 6 for a 1-hour run)",
		Loop:        true,
		Phases: []Phase{
			{Name: "warmup", Duration: Duration(60 * time.Minute), Streams: iptr(5), Rate: fptr(2000), BurstSize: iptr(400), BurstInterval: dptr(20 * time.Second), Transition: "gradual", TransitionSec: Duration(30 * time.Minute)},
			{Name: "sustained-peak", Duration: Duration(120 * time.Minute), Streams: iptr(12), Rate: fptr(10000), BurstSize: iptr(1500), BurstInterval: dptr(5 * time.Second), Transition: "gradual", TransitionSec: Duration(30 * time.Minute)},
			{Name: "rolling-churn", Duration: Duration(60 * time.Minute), Streams: iptr(8), Rate: fptr(5000), BurstSize: iptr(800), BurstInterval: dptr(10 * time.Second), Transition: "gradual", TransitionSec: Duration(15 * time.Minute)},
			{Name: "sustained-low", Duration: Duration(120 * time.Minute), Streams: iptr(3), Rate: fptr(500), BurstSize: iptr(150), BurstInterval: dptr(90 * time.Second), Transition: "gradual", TransitionSec: Duration(30 * time.Minute)},
			{Name: "overnight", Duration: Duration(60 * time.Minute), Streams: iptr(1), Rate: fptr(100), BurstSize: iptr(50), BurstInterval: dptr(300 * time.Second), Transition: "gradual", TransitionSec: Duration(30 * time.Minute)},
		},
	},
}

// BuiltinNames returns the scenario names in stable order.
func BuiltinNames() []string {
	names := make([]string, 0, len(Builtin))
	for n := range Builtin {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ── mode → scenario builders ─────────────────────────────────────────────────

// steadyScenario is one phase at the configured streams/rate for Duration
// (or forever).
func steadyScenario(c *Config) *Scenario {
	return &Scenario{Name: "steady", Phases: []Phase{{
		Name: "steady", Duration: Duration(c.Duration), Streams: iptr(c.Streams), Rate: fptr(c.Rate),
		BurstSize: iptr(c.BurstSize), BurstInterval: dptr(c.BurstInterval),
	}}}
}

// rampScenario adds RampStep streams every RampInterval until Streams is
// reached. The aggregate rate scales with the stream count so each stream
// carries Rate/Streams lines/s throughout, matching "more services = more
// volume".
func rampScenario(c *Config) *Scenario {
	sc := &Scenario{Name: "ramp"}
	start := c.RampStart
	if start > c.Streams {
		start = c.Streams
	}
	step := c.RampStep
	if step < 1 {
		step = 1
	}
	perStream := c.Rate / float64(max(c.Streams, 1))
	for n := start; ; n += step {
		if n > c.Streams {
			n = c.Streams
		}
		p := Phase{Name: fmt.Sprintf("ramp-%d", n), Duration: Duration(c.RampInterval), Streams: iptr(n), Rate: fptr(perStream * float64(n))}
		if n == c.Streams {
			p.Name = "ramp-full"
			p.Duration = Duration(c.Duration)
			sc.Phases = append(sc.Phases, p)
			break
		}
		sc.Phases = append(sc.Phases, p)
	}
	return sc
}

// spikeScenario alternates a baseline with a coordinated spike that adds
// SpikeStreams streams and SpikeRate lines/s for SpikeDuration.
func spikeScenario(c *Config) *Scenario {
	return &Scenario{Name: "spike", Loop: true, Phases: []Phase{
		{Name: "spike-baseline", Duration: Duration(c.SpikeInterval), Streams: iptr(c.Streams), Rate: fptr(c.Rate)},
		{Name: "spike-active", Duration: Duration(c.SpikeDuration), Streams: iptr(c.Streams + c.SpikeStreams), Rate: fptr(c.Rate + c.SpikeRate)},
	}}
}

// pulseScenario is the repeating pressure test: base → gradual ramp-up →
// sustained peak → gradual cooldown → rest.
func pulseScenario(c *Config) *Scenario {
	cool := c.PulseCooldown
	if cool <= 0 {
		cool = c.PulseRamp
	}
	base := Phase{Streams: iptr(c.PulseBaseStreams), Rate: fptr(c.PulseBaseRate), BurstSize: iptr(c.BurstSize), BurstInterval: dptr(c.BurstInterval)}
	peak := Phase{Streams: iptr(c.PulsePeakStreams), Rate: fptr(c.PulsePeakRate), BurstSize: iptr(c.PulsePeakBurstSize), BurstInterval: dptr(c.PulsePeakBurstInterval)}
	init := base
	init.Name, init.Duration = "base", Duration(2*time.Second)
	rampUp := peak
	rampUp.Name, rampUp.Duration, rampUp.Transition, rampUp.TransitionSec = "ramp-up", Duration(c.PulseRamp), "gradual", Duration(c.PulseRamp)
	hold := peak
	hold.Name, hold.Duration = "peak", Duration(c.PulsePeak)
	cooldown := base
	cooldown.Name, cooldown.Duration, cooldown.Transition, cooldown.TransitionSec = "cooldown", Duration(cool), "gradual", Duration(cool)
	rest := base
	rest.Name, rest.Duration = "rest", Duration(c.PulseRest)
	core := []Phase{rampUp, hold, cooldown, rest}
	sc := &Scenario{Name: "pulse", Phases: []Phase{init}}
	if c.PulseCycles <= 0 {
		sc.Loop = true
		sc.Phases = append(sc.Phases, core...)
		return sc
	}
	for i := 0; i < c.PulseCycles; i++ {
		sc.Phases = append(sc.Phases, core...)
	}
	return sc
}
