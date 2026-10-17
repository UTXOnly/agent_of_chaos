package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/gen"
)

func init() {
	register(command{name: "generate", short: "write synthetic logs (files or stdout) at a controlled rate", run: runGenerate})
}

// generateFlags holds every `aoc generate` flag. The same declarations
// validate a profile's generator settings, so a profile key is exactly a
// flag name.
type generateFlags struct {
	name, mode, format, output, logDir, rotateMode, scenario *string
	streams, burstSize, rotateKeep, wideBytes, padTo         *int
	rampStart, rampStep, spikeStreams                        *int
	pulseBaseStreams, pulsePeakStreams, pulsePeakBurst       *int
	pulseCycles, emitChunk                                   *int
	maxLines                                                 *int64
	rotateBytes, bufferBytes                                 *int64
	rate, multiline, wideRate, crashRate, spikeRate          *float64
	pulseBaseRate, pulsePeakRate, scenarioSpeed              *float64
	deterministic, quiet                                     *bool
	seed                                                     *uint64
	duration, startDelay, burstInterval, flushInterval       *time.Duration
	statsInterval, rampInterval, restartDelay                *time.Duration
	spikeInterval, spikeDuration                             *time.Duration
	pulseRamp, pulsePeak, pulseCooldown, pulseRest           *time.Duration
	pulsePeakBurstInterval                                   *time.Duration
	intake                                                   *string
}

func declareGenerateFlags(fs *flagSet, d gen.Config) *generateFlags {
	g := &generateFlags{}
	fs.section("Workload")
	g.name = fs.Str("name", d.Name, "generator id embedded in every marker; {host} expands to the hostname (default: hostname)")
	g.mode = fs.Str("mode", d.Mode, "steady | ramp | chaos | spike | pulse | scenario")
	g.streams = fs.Int("streams", d.Streams, "streams (one service name, one file each)")
	g.rate = fs.Float("rate", d.Rate, "aggregate lines/s across all streams; 0 = flat out")
	g.duration = fs.Duration("duration", 0, "stop after this long (0 = run until stopped)")
	g.startDelay = fs.Duration("start-delay", 0, "wait this long before writing the first line")
	g.maxLines = fs.Int64("max-lines", 0, "stop after this many records in total (0 = no cap)")
	g.burstSize = fs.Int("burst-size", d.BurstSize, "extra lines per burst event per stream (0 = no bursts)")
	g.burstInterval = fs.Duration("burst-interval", d.BurstInterval, "time between burst events")

	fs.section("Output")
	g.format = fs.Str("format", string(d.Format), "plain | json | both (both writes <svc>.log and <svc>.json.log)")
	g.output = fs.Str("output", string(d.Output), "file | stdout (stdout: all streams interleaved, for container log drivers)")
	g.logDir = fs.Str("log-dir", d.LogDir, "directory for log files ({host} expands to the hostname)")
	g.rotateBytes = fs.Size("rotate-bytes", d.RotateBytes, "rotate a file once it reaches this size (0 = never)")
	g.rotateKeep = fs.Int("rotate-keep", d.RotateKeep, "rotated files to keep per stream")
	g.rotateMode = fs.Str("rotate-mode", string(d.RotateMode), "rename (new inode) | truncate (copytruncate, same inode)")
	g.flushInterval = fs.Duration("flush-interval", d.FlushInterval, "how often buffered lines are flushed to disk")

	fs.section("Content")
	g.multiline = fs.Float("multiline-rate", d.MultilineRate, "fraction of records followed by a multi-line stack trace")
	g.wideRate = fs.Float("wide-line-rate", d.WideLineRate, "fraction of records followed by an oversized line")
	g.wideBytes = fs.Int("wide-line-bytes", d.WideLineBytes, "payload size of oversized lines")
	g.padTo = fs.Int("pad-to", d.PadTo, "pad every line to at least this many bytes (0 = natural size)")
	g.deterministic = fs.Bool("deterministic", false, "seeded content + synthetic timestamps: identical bytes for identical config")
	g.seed = fs.Uint64("seed", 0, "seed for --deterministic")

	fs.section("Reporting")
	g.intake = fs.Str("intake", "", "push stats to this aoc intake (e.g. http://localhost:8282)")
	g.statsInterval = fs.Duration("stats-interval", d.StatsInterval, "status line / stats push period")
	g.quiet = fs.Bool("quiet", false, "no status output on stderr")

	fs.section("Ramp mode")
	g.rampStart = fs.Int("ramp-start", d.RampStart, "streams to start with")
	g.rampStep = fs.Int("ramp-step", d.RampStep, "streams added per step")
	g.rampInterval = fs.Duration("ramp-interval", d.RampInterval, "time between steps")

	fs.section("Chaos mode")
	g.crashRate = fs.Float("chaos-crash-rate", d.ChaosCrashRate, "expected crashes per stream per minute (files close, seq resumes on restart)")
	g.restartDelay = fs.Duration("chaos-restart-delay", d.ChaosRestartDelay, "downtime before a crashed stream restarts")

	fs.section("Spike mode")
	g.spikeInterval = fs.Duration("spike-interval", d.SpikeInterval, "baseline time between spikes")
	g.spikeDuration = fs.Duration("spike-duration", d.SpikeDuration, "spike length")
	g.spikeStreams = fs.Int("spike-streams", d.SpikeStreams, "extra streams during a spike")
	g.spikeRate = fs.Float("spike-rate", d.SpikeRate, "extra lines/s during a spike")

	fs.section("Pulse mode (ramp-up, peak, cooldown, rest, repeat)")
	g.pulseRamp = fs.Duration("pulse-ramp", d.PulseRamp, "ramp-up length")
	g.pulsePeak = fs.Duration("pulse-peak", d.PulsePeak, "peak hold length")
	g.pulseCooldown = fs.Duration("pulse-cooldown", 0, "cooldown length (0 = same as ramp)")
	g.pulseRest = fs.Duration("pulse-rest", d.PulseRest, "rest length")
	g.pulseBaseStreams = fs.Int("pulse-base-streams", d.PulseBaseStreams, "streams at rest")
	g.pulsePeakStreams = fs.Int("pulse-peak-streams", d.PulsePeakStreams, "streams at peak")
	g.pulseBaseRate = fs.Float("pulse-base-rate", d.PulseBaseRate, "lines/s at rest")
	g.pulsePeakRate = fs.Float("pulse-peak-rate", d.PulsePeakRate, "lines/s at peak")
	g.pulsePeakBurst = fs.Int("pulse-peak-burst-size", d.PulsePeakBurstSize, "burst size at peak")
	g.pulsePeakBurstInterval = fs.Duration("pulse-peak-burst-interval", d.PulsePeakBurstInterval, "burst interval at peak")
	g.pulseCycles = fs.Int("pulse-cycles", 0, "cycles to run (0 = forever)")

	fs.section("Scenario mode")
	g.scenario = fs.Str("scenario", "", "built-in name ("+strings.Join(gen.BuiltinNames(), ", ")+") or a YAML/JSON file")
	g.scenarioSpeed = fs.Float("scenario-speed", 1, "divide every phase duration by this (6 → a 6 h scenario in 1 h)")

	fs.section("Tuning")
	g.emitChunk = fs.Int("emit-chunk", d.EmitChunk, "max records written per pacing slice")
	g.bufferBytes = fs.Size("buffer-bytes", int64(d.BufferBytes), "write buffer per file")
	return g
}

func (g *generateFlags) config(d gen.Config) gen.Config {
	cfg := d
	cfg.Name, cfg.Mode, cfg.Streams, cfg.Rate, cfg.Duration, cfg.StartDelay, cfg.MaxLines = *g.name, *g.mode, *g.streams, *g.rate, *g.duration, *g.startDelay, *g.maxLines
	cfg.BurstSize, cfg.BurstInterval = *g.burstSize, *g.burstInterval
	cfg.Format, cfg.Output, cfg.LogDir = gen.Format(*g.format), gen.Output(*g.output), *g.logDir
	cfg.RotateBytes, cfg.RotateKeep, cfg.RotateMode, cfg.FlushInterval = *g.rotateBytes, *g.rotateKeep, gen.RotateMode(*g.rotateMode), *g.flushInterval
	cfg.MultilineRate, cfg.WideLineRate, cfg.WideLineBytes, cfg.PadTo = *g.multiline, *g.wideRate, *g.wideBytes, *g.padTo
	cfg.Deterministic, cfg.Seed = *g.deterministic, *g.seed
	cfg.IntakeURL, cfg.StatsInterval, cfg.Quiet = *g.intake, *g.statsInterval, *g.quiet
	cfg.RampStart, cfg.RampStep, cfg.RampInterval = *g.rampStart, *g.rampStep, *g.rampInterval
	cfg.ChaosCrashRate, cfg.ChaosRestartDelay = *g.crashRate, *g.restartDelay
	cfg.SpikeInterval, cfg.SpikeDuration, cfg.SpikeStreams, cfg.SpikeRate = *g.spikeInterval, *g.spikeDuration, *g.spikeStreams, *g.spikeRate
	cfg.PulseRamp, cfg.PulsePeak, cfg.PulseCooldown, cfg.PulseRest = *g.pulseRamp, *g.pulsePeak, *g.pulseCooldown, *g.pulseRest
	cfg.PulseBaseStreams, cfg.PulsePeakStreams, cfg.PulseBaseRate, cfg.PulsePeakRate = *g.pulseBaseStreams, *g.pulsePeakStreams, *g.pulseBaseRate, *g.pulsePeakRate
	cfg.PulsePeakBurstSize, cfg.PulsePeakBurstInterval, cfg.PulseCycles = *g.pulsePeakBurst, *g.pulsePeakBurstInterval, *g.pulseCycles
	cfg.Scenario, cfg.ScenarioSpeed = *g.scenario, *g.scenarioSpeed
	cfg.EmitChunk, cfg.BufferBytes = *g.emitChunk, int(*g.bufferBytes)
	return cfg
}

// generateSettings lists the `aoc generate` flag names, i.e. the keys a
// profile's generator may set.
func generateSettings() map[string]bool {
	fs := newFlagSet("generate", "", "")
	declareGenerateFlags(fs, gen.Default())
	names := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) { names[f.Name] = true })
	return names
}

func runGenerate(args []string) int {
	d := gen.Default()
	fs := newFlagSet("generate",
		"Write realistic, sequence-marked log lines for the Datadog Agent to tail.\n"+
			"Every record carries aoc=<name>/<stream>/<seq>; the intake uses it to\n"+
			"account for delivery exactly (missing, duplicated, reordered).",
		"  aoc generate --streams 8 --rate 5000 --log-dir ./logs\n"+
			"  aoc generate --format json --rate 20000 --intake http://localhost:8282\n"+
			"  aoc generate --output stdout --rate 500                # for docker log collection\n"+
			"  aoc generate --mode scenario --scenario incident --scenario-speed 5\n"+
			"  aoc generate --deterministic --seed 42 --max-lines 1000000 --rate 0   # byte-identical, flat out\n"+
			"  aoc generate --rotate-mode truncate --rotate-bytes 1MiB   # copytruncate-style rotation")
	g := declareGenerateFlags(fs, d)
	if !fs.parse(args) {
		return 2
	}
	if fs.NArg() > 0 {
		return fail("generate: unexpected argument %q", fs.Arg(0))
	}
	cfg := g.config(d)
	cfg.Version = buildVersion
	if cfg.Scenario == "list" {
		listScenarios(os.Stdout)
		return 0
	}

	c, err := gen.New(cfg)
	if err != nil {
		return fail("generate: %v", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	rep, err := c.Run(ctx)
	if err != nil {
		return fail("generate: %v", err)
	}
	if rep.Totals.Errors > 0 {
		fmt.Fprintf(os.Stderr, "aoc: generate finished with %d write errors\n", rep.Totals.Errors)
		return 1
	}
	return 0
}
