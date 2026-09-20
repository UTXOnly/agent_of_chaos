package gen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Format selects which log encodings a stream writes.
type Format string

const (
	FormatPlain Format = "plain"
	FormatJSON  Format = "json"
	FormatBoth  Format = "both"
)

// Output selects where lines go.
type Output string

const (
	OutputFile   Output = "file"
	OutputStdout Output = "stdout"
)

// Modes.
const (
	ModeSteady   = "steady"
	ModeRamp     = "ramp"
	ModeChaos    = "chaos"
	ModeSpike    = "spike"
	ModePulse    = "pulse"
	ModeScenario = "scenario"
)

var Modes = []string{ModeSteady, ModeRamp, ModeChaos, ModeSpike, ModePulse, ModeScenario}

// Config is the full generator configuration. Zero values are filled in by
// Default(); Validate() rejects nonsense.
type Config struct {
	Name     string        // generator id in the aoc= marker (default: hostname)
	Mode     string        // see Modes
	Streams  int           // target stream count (steady/ramp/chaos/spike)
	Rate     float64       // aggregate lines/s across streams; 0 = unlimited
	Duration time.Duration // stop after this long; 0 = run forever
	MaxLines int64         // stop after this many records in total; 0 = no cap

	Format Format
	Output Output
	LogDir string

	RotateBytes int64
	RotateKeep  int
	RotateMode  RotateMode

	BurstSize     int
	BurstInterval time.Duration

	MultilineRate float64
	WideLineRate  float64
	WideLineBytes int
	PadTo         int

	Deterministic bool
	Seed          uint64

	FlushInterval time.Duration
	BufferBytes   int
	EmitChunk     int

	IntakeURL     string // push generator stats here (optional)
	StatsInterval time.Duration
	Quiet         bool

	RampStart    int
	RampStep     int
	RampInterval time.Duration

	ChaosCrashRate    float64 // expected crashes per stream per minute
	ChaosRestartDelay time.Duration

	SpikeInterval time.Duration
	SpikeDuration time.Duration
	SpikeStreams  int
	SpikeRate     float64

	PulseRamp              time.Duration
	PulsePeak              time.Duration
	PulseCooldown          time.Duration
	PulseRest              time.Duration
	PulseBaseStreams       int
	PulsePeakStreams       int
	PulseBaseRate          float64
	PulsePeakRate          float64
	PulsePeakBurstSize     int
	PulsePeakBurstInterval time.Duration
	PulseCycles            int

	Scenario      string // builtin name or path to a YAML/JSON file
	ScenarioSpeed float64

	// Version is stamped into stats pushes.
	Version string
}

// Default returns the baseline configuration: 5 streams, 1,000 lines/s,
// plain text files in ./logs, 512 KiB rotation × 5 backups.
func Default() Config {
	host, _ := os.Hostname()
	if host == "" {
		host = "gen"
	}
	return Config{
		Name:          host,
		Mode:          ModeSteady,
		Streams:       5,
		Rate:          1000,
		Format:        FormatPlain,
		Output:        OutputFile,
		LogDir:        "logs",
		RotateBytes:   512 << 10,
		RotateKeep:    5,
		RotateMode:    RotateRename,
		BurstSize:     0,
		BurstInterval: 5 * time.Second,
		MultilineRate: 0.05,
		WideLineRate:  0,
		WideLineBytes: 4096,
		FlushInterval: 50 * time.Millisecond,
		BufferBytes:   256 << 10,
		EmitChunk:     256,
		StatsInterval: time.Second,

		RampStart: 1, RampStep: 1, RampInterval: 30 * time.Second,
		ChaosCrashRate: 2, ChaosRestartDelay: 5 * time.Second,
		SpikeInterval: 30 * time.Second, SpikeDuration: 10 * time.Second, SpikeStreams: 5, SpikeRate: 5000,
		PulseRamp: 10 * time.Minute, PulsePeak: 3 * time.Minute, PulseRest: 35 * time.Minute,
		PulseBaseStreams: 2, PulsePeakStreams: 10, PulseBaseRate: 200, PulsePeakRate: 10000,
		PulsePeakBurstSize: 2000, PulsePeakBurstInterval: 4 * time.Second,
		ScenarioSpeed: 1,
	}
}

// expandHost replaces "{host}" with the short hostname (the container id
// under Docker), so scaled replicas get distinct names and directories.
func expandHost(s string) string {
	if !strings.Contains(s, "{host}") {
		return s
	}
	host, _ := os.Hostname()
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	if host == "" {
		host = "gen"
	}
	return strings.ReplaceAll(s, "{host}", host)
}

func (c *Config) Validate() error {
	c.Name = expandHost(strings.TrimSpace(c.Name))
	c.LogDir = expandHost(c.LogDir)
	if c.Name == "" || strings.ContainsAny(c.Name, "/ \t\n\"") {
		return fmt.Errorf("--name %q must be non-empty and contain no '/', quotes or whitespace", c.Name)
	}
	found := false
	for _, m := range Modes {
		if c.Mode == m {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("--mode %q: must be one of %s", c.Mode, strings.Join(Modes, ", "))
	}
	if c.Streams < 0 {
		return fmt.Errorf("--streams must be >= 0")
	}
	if c.Mode != ModeScenario && c.Mode != ModePulse && c.Streams == 0 {
		return fmt.Errorf("--streams must be >= 1 in %s mode", c.Mode)
	}
	if c.Rate < 0 {
		return fmt.Errorf("--rate must be >= 0 (0 = unlimited)")
	}
	switch c.Format {
	case FormatPlain, FormatJSON, FormatBoth:
	default:
		return fmt.Errorf("--format %q: must be plain, json or both", c.Format)
	}
	switch c.Output {
	case OutputFile, OutputStdout:
	default:
		return fmt.Errorf("--output %q: must be file or stdout", c.Output)
	}
	switch c.RotateMode {
	case RotateRename, RotateTruncate:
	default:
		return fmt.Errorf("--rotate-mode %q: must be rename or truncate", c.RotateMode)
	}
	if c.RotateKeep < 0 {
		return fmt.Errorf("--rotate-keep must be >= 0")
	}
	if c.MultilineRate < 0 || c.MultilineRate > 1 || c.WideLineRate < 0 || c.WideLineRate > 1 {
		return fmt.Errorf("rates must be within [0, 1]")
	}
	if c.WideLineBytes < 16 {
		return fmt.Errorf("--wide-line-bytes must be >= 16")
	}
	if c.EmitChunk < 1 {
		c.EmitChunk = 1
	}
	if c.BufferBytes < 4096 {
		c.BufferBytes = 4096
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = 50 * time.Millisecond
	}
	if c.StatsInterval <= 0 {
		c.StatsInterval = time.Second
	}
	if c.ScenarioSpeed <= 0 {
		return fmt.Errorf("--scenario-speed must be > 0")
	}
	if c.Mode == ModeScenario && c.Scenario == "" {
		return fmt.Errorf("--mode scenario needs --scenario NAME|FILE (names: %s)", strings.Join(BuiltinNames(), ", "))
	}
	if c.Output == OutputFile {
		if c.LogDir == "" {
			return fmt.Errorf("--log-dir must be set for file output")
		}
		abs, err := filepath.Abs(c.LogDir)
		if err != nil {
			return err
		}
		c.LogDir = abs
	}
	return nil
}

// FilesPerStream is how many log files one stream writes.
func (c *Config) FilesPerStream() int {
	if c.Output != OutputFile {
		return 0
	}
	if c.Format == FormatBoth {
		return 2
	}
	return 1
}
