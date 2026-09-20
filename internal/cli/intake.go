package cli

import (
	"os"
	"strings"

	"github.com/UTXOnly/agent_of_chaos/internal/intake"
)

func init() {
	register(command{name: "intake", short: "run the fake Datadog logs intake (reports to Datadog as aoc.* metrics)", run: runIntake})
}

func runIntake(args []string) int {
	d := intake.DefaultConfig()
	fs := newFlagSet("intake",
		"Stand in for the Datadog logs intake. Accepts what the Agent sends\n"+
			"(/api/v2/logs, gzip/zstd, legacy TCP), keeps exact delivery ledgers per\n"+
			"stream, measures latency, scrapes the agent's own telemetry and container\n"+
			"stats, injects faults on demand, submits everything to Datadog as aoc.*\n"+
			"metrics/events and serves a JSON report.",
		"  aoc intake                                        # :8282 HTTP, :10516 TCP\n"+
			"  aoc intake --agent-telemetry http://localhost:5000/telemetry --docker-container dd-agent\n"+
			"  aoc intake --api-key $DD_API_KEY --strict         # behave like the real thing\n"+
			"  aoc intake --fault-error-rate 0.2 --fault-error-status 503\n\n"+
			"Point an agent at it:\n"+
			"  DD_LOGS_CONFIG_LOGS_DD_URL=http://<host>:8282 DD_LOGS_CONFIG_LOGS_NO_SSL=true DD_LOGS_CONFIG_USE_HTTP=true\n"+
			"  DD_DD_URL=http://<host>:8282   # optional: also swallow metrics/metadata → fully offline")

	fs.section("Listen")
	addr := fs.Str("addr", d.Addr, "HTTP listen address")
	tcpAddr := fs.Str("tcp-addr", d.TCPAddr, "legacy TCP listen address (empty = off)")
	name := fs.Str("name", d.Name, "run name: the run:<name> tag on every metric/event, and the report name")

	fs.section("Intake behaviour")
	apiKey := fs.Str("api-key", "", "require this DD-API-KEY (403 otherwise); empty accepts any")
	maxPayload := fs.Size("max-payload-bytes", d.MaxPayloadBytes, "413 above this decompressed payload size (real intake: 5MiB)")
	strict := fs.Bool("strict", false, "400 on malformed payloads instead of swallowing them")

	fs.section("Observe the agent")
	telemetry := fs.Str("agent-telemetry", "", "agent telemetry endpoint to scrape, e.g. http://localhost:5000/telemetry (needs DD_TELEMETRY_ENABLED=true)")
	telemetryFilter := fs.Str("telemetry-filter", intake.DefaultTelemetryFilter, "regexp of telemetry metric names to keep")
	scrape := fs.Duration("scrape-interval", d.ScrapeInterval, "telemetry scrape period")
	container := fs.Str("docker-container", "", "agent container name for CPU/memory via the Docker API")
	socket := fs.Str("docker-socket", d.DockerSocket, "Docker socket path")
	image := fs.Str("agent-image", "", "agent image name, recorded in the report")

	fs.section("Faults (initial; change live with POST /harness/faults, or a profile timeline)")
	fLatency := fs.Int("fault-latency-ms", 0, "delay every response by this many ms")
	fJitter := fs.Int("fault-jitter-ms", 0, "add up to this many ms of random jitter")
	fErrRate := fs.Float("fault-error-rate", 0, "probability of answering --fault-error-status instead of 202")
	fErrStatus := fs.Int("fault-error-status", 500, "status for injected errors (429/5xx → agent retries, other 4xx → agent drops)")
	fDrop := fs.Float("fault-drop-rate", 0, "probability of closing the connection without responding")
	fOutage := fs.Bool("fault-outage", false, "drop every request")
	fReadBps := fs.Int64("fault-read-bps", 0, "throttle request body reads to this many bytes/s")

	fs.section("Datadog (metrics + events for every run; the system of record)")
	ddMetrics := fs.Bool("dd-metrics", os.Getenv("DD_API_KEY") != "", "submit <prefix>.* metrics and events to Datadog (default: on when DD_API_KEY is set)")
	ddSite := fs.Str("dd-site", envOr("DD_SITE", "datadoghq.com"), "Datadog site (env DD_SITE)")
	ddAPIKey := fs.Str("dd-api-key", os.Getenv("DD_API_KEY"), "API key (env DD_API_KEY)")
	ddInterval := fs.Duration("dd-interval", 10e9, "submission period")
	ddPrefix := fs.Str("dd-prefix", "aoc", "metric prefix")
	ddTags := fs.Str("dd-tags", "", "extra tags for every series/event, comma-separated (run:<name> is always added)")
	ddEvents := fs.Bool("dd-events", true, "post events for window open/close, faults and generator lifecycle")

	fs.section("Misc")
	retention := fs.Duration("retention", d.Retention, "per-second history to keep in memory")
	verbose := fs.Bool("verbose", false, "log every request")
	quiet := fs.Bool("quiet", false, "no status output")

	if !fs.parse(args) {
		return 2
	}
	cfg := d
	cfg.Addr, cfg.TCPAddr, cfg.Name = *addr, *tcpAddr, *name
	cfg.APIKey, cfg.MaxPayloadBytes, cfg.Strict = *apiKey, *maxPayload, *strict
	cfg.AgentTelemetryURL, cfg.TelemetryFilter, cfg.ScrapeInterval = *telemetry, *telemetryFilter, *scrape
	cfg.DockerContainer, cfg.DockerSocket, cfg.AgentImage = *container, *socket, *image
	cfg.Faults = intake.Faults{LatencyMs: *fLatency, JitterMs: *fJitter, ErrorRate: *fErrRate, ErrorStatus: *fErrStatus, DropRate: *fDrop, Outage: *fOutage, ReadBps: *fReadBps}
	if !cfg.Faults.Active() {
		cfg.Faults = intake.Faults{}
	}
	cfg.Retention, cfg.Verbose, cfg.Quiet, cfg.Version = *retention, *verbose, *quiet, buildVersion
	cfg.Datadog = intake.DDConfig{Enabled: *ddMetrics, Site: *ddSite, APIKey: *ddAPIKey, Interval: *ddInterval, Prefix: *ddPrefix, Events: *ddEvents}
	for _, t := range strings.Split(*ddTags, ",") {
		if t = strings.TrimSpace(t); t != "" {
			cfg.Datadog.Tags = append(cfg.Datadog.Tags, t)
		}
	}

	srv, err := intake.New(cfg)
	if err != nil {
		return fail("intake: %v", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	if err := srv.Run(ctx); err != nil {
		return fail("intake: %v", err)
	}
	return 0
}
