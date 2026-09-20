package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/UTXOnly/agent_of_chaos/internal/gen"
)

func init() {
	register(command{name: "generate", short: "write synthetic logs (files or stdout) at a controlled rate", run: runGenerate})
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

	fs.section("Workload")
	name := fs.Str("name", d.Name, "generator id embedded in every marker; {host} expands to the hostname (default: hostname)")
	mode := fs.Str("mode", d.Mode, "steady | ramp | chaos | spike | pulse | scenario")
	streams := fs.Int("streams", d.Streams, "streams (one service name, one file each)")
	rate := fs.Float("rate", d.Rate, "aggregate lines/s across all streams; 0 = flat out")
	duration := fs.Duration("duration", 0, "stop after this long (0 = run until stopped)")
	maxLines := fs.Int64("max-lines", 0, "stop after this many records in total (0 = no cap)")
	burstSize := fs.Int("burst-size", d.BurstSize, "extra lines per burst event per stream (0 = no bursts)")
	burstInterval := fs.Duration("burst-interval", d.BurstInterval, "time between burst events")

	fs.section("Output")
	format := fs.Str("format", string(d.Format), "plain | json | both (both writes <svc>.log and <svc>.json.log)")
	output := fs.Str("output", string(d.Output), "file | stdout (stdout: all streams interleaved, for container log drivers)")
	logDir := fs.Str("log-dir", d.LogDir, "directory for log files ({host} expands to the hostname)")
	rotateBytes := fs.Size("rotate-bytes", d.RotateBytes, "rotate a file once it reaches this size (0 = never)")
	rotateKeep := fs.Int("rotate-keep", d.RotateKeep, "rotated files to keep per stream")
	rotateMode := fs.Str("rotate-mode", string(d.RotateMode), "rename (new inode) | truncate (copytruncate, same inode)")
	flushInterval := fs.Duration("flush-interval", d.FlushInterval, "how often buffered lines are flushed to disk")

	fs.section("Content")
	multiline := fs.Float("multiline-rate", d.MultilineRate, "fraction of records followed by a multi-line stack trace")
	wideRate := fs.Float("wide-line-rate", d.WideLineRate, "fraction of records followed by an oversized line")
	wideBytes := fs.Int("wide-line-bytes", d.WideLineBytes, "payload size of oversized lines")
	padTo := fs.Int("pad-to", d.PadTo, "pad every line to at least this many bytes (0 = natural size)")
	deterministic := fs.Bool("deterministic", false, "seeded content + synthetic timestamps: identical bytes for identical config")
	seed := fs.Uint64("seed", 0, "seed for --deterministic")

	fs.section("Reporting")
	intake := fs.Str("intake", "", "push stats to this aoc intake (e.g. http://localhost:8282)")
	statsInterval := fs.Duration("stats-interval", d.StatsInterval, "status line / stats push period")
	quiet := fs.Bool("quiet", false, "no status output on stderr")

	fs.section("Ramp mode")
	rampStart := fs.Int("ramp-start", d.RampStart, "streams to start with")
	rampStep := fs.Int("ramp-step", d.RampStep, "streams added per step")
	rampInterval := fs.Duration("ramp-interval", d.RampInterval, "time between steps")

	fs.section("Chaos mode")
	crashRate := fs.Float("chaos-crash-rate", d.ChaosCrashRate, "expected crashes per stream per minute (files close, seq resumes on restart)")
	restartDelay := fs.Duration("chaos-restart-delay", d.ChaosRestartDelay, "downtime before a crashed stream restarts")

	fs.section("Spike mode")
	spikeInterval := fs.Duration("spike-interval", d.SpikeInterval, "baseline time between spikes")
	spikeDuration := fs.Duration("spike-duration", d.SpikeDuration, "spike length")
	spikeStreams := fs.Int("spike-streams", d.SpikeStreams, "extra streams during a spike")
	spikeRate := fs.Float("spike-rate", d.SpikeRate, "extra lines/s during a spike")

	fs.section("Pulse mode (ramp-up → peak → cooldown → rest, repeat)")
	pulseRamp := fs.Duration("pulse-ramp", d.PulseRamp, "ramp-up length")
	pulsePeak := fs.Duration("pulse-peak", d.PulsePeak, "peak hold length")
	pulseCooldown := fs.Duration("pulse-cooldown", 0, "cooldown length (0 = same as ramp)")
	pulseRest := fs.Duration("pulse-rest", d.PulseRest, "rest length")
	pulseBaseStreams := fs.Int("pulse-base-streams", d.PulseBaseStreams, "streams at rest")
	pulsePeakStreams := fs.Int("pulse-peak-streams", d.PulsePeakStreams, "streams at peak")
	pulseBaseRate := fs.Float("pulse-base-rate", d.PulseBaseRate, "lines/s at rest")
	pulsePeakRate := fs.Float("pulse-peak-rate", d.PulsePeakRate, "lines/s at peak")
	pulsePeakBurst := fs.Int("pulse-peak-burst-size", d.PulsePeakBurstSize, "burst size at peak")
	pulsePeakBurstInterval := fs.Duration("pulse-peak-burst-interval", d.PulsePeakBurstInterval, "burst interval at peak")
	pulseCycles := fs.Int("pulse-cycles", 0, "cycles to run (0 = forever)")

	fs.section("Scenario mode")
	scenario := fs.Str("scenario", "", "built-in name ("+strings.Join(gen.BuiltinNames(), ", ")+") or a YAML/JSON file")
	scenarioSpeed := fs.Float("scenario-speed", 1, "divide every phase duration by this (6 → a 6 h scenario in 1 h)")

	fs.section("Tuning")
	emitChunk := fs.Int("emit-chunk", d.EmitChunk, "max records written per pacing slice")
	bufferBytes := fs.Size("buffer-bytes", int64(d.BufferBytes), "write buffer per file")

	if !fs.parse(args) {
		return 2
	}
	if fs.NArg() > 0 {
		return fail("generate: unexpected argument %q", fs.Arg(0))
	}

	cfg := d
	cfg.Name, cfg.Mode, cfg.Streams, cfg.Rate, cfg.Duration, cfg.MaxLines = *name, *mode, *streams, *rate, *duration, *maxLines
	cfg.BurstSize, cfg.BurstInterval = *burstSize, *burstInterval
	cfg.Format, cfg.Output, cfg.LogDir = gen.Format(*format), gen.Output(*output), *logDir
	cfg.RotateBytes, cfg.RotateKeep, cfg.RotateMode, cfg.FlushInterval = *rotateBytes, *rotateKeep, gen.RotateMode(*rotateMode), *flushInterval
	cfg.MultilineRate, cfg.WideLineRate, cfg.WideLineBytes, cfg.PadTo = *multiline, *wideRate, *wideBytes, *padTo
	cfg.Deterministic, cfg.Seed = *deterministic, *seed
	cfg.IntakeURL, cfg.StatsInterval, cfg.Quiet = *intake, *statsInterval, *quiet
	cfg.RampStart, cfg.RampStep, cfg.RampInterval = *rampStart, *rampStep, *rampInterval
	cfg.ChaosCrashRate, cfg.ChaosRestartDelay = *crashRate, *restartDelay
	cfg.SpikeInterval, cfg.SpikeDuration, cfg.SpikeStreams, cfg.SpikeRate = *spikeInterval, *spikeDuration, *spikeStreams, *spikeRate
	cfg.PulseRamp, cfg.PulsePeak, cfg.PulseCooldown, cfg.PulseRest = *pulseRamp, *pulsePeak, *pulseCooldown, *pulseRest
	cfg.PulseBaseStreams, cfg.PulsePeakStreams, cfg.PulseBaseRate, cfg.PulsePeakRate = *pulseBaseStreams, *pulsePeakStreams, *pulseBaseRate, *pulsePeakRate
	cfg.PulsePeakBurstSize, cfg.PulsePeakBurstInterval, cfg.PulseCycles = *pulsePeakBurst, *pulsePeakBurstInterval, *pulseCycles
	cfg.Scenario, cfg.ScenarioSpeed = *scenario, *scenarioSpeed
	cfg.EmitChunk, cfg.BufferBytes = *emitChunk, int(*bufferBytes)
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
