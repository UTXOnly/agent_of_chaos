package cli

import (
	"strings"

	"github.com/UTXOnly/agent_of_chaos/internal/ship"
)

func init() {
	register(command{name: "ship", short: "reference shipper: tail log files and POST them to an intake (no agent needed)", run: runShip})
}

func runShip(args []string) int {
	d := ship.Default()
	fs := newFlagSet("ship",
		"Tail generator output and send it to an intake the way the Agent does\n"+
			"(JSON arrays on /api/v2/logs, gzip/zstd, retries on 429/5xx). Use it to\n"+
			"validate the harness without an agent, or as a naive baseline sender.",
		"  aoc ship --log-dir ./logs --intake http://localhost:8282\n"+
			"  aoc ship --from-start --encoding zstd --batch-logs 500")
	fs.section("Input")
	dir := fs.Str("log-dir", d.LogDir, "directory to watch")
	globs := fs.Str("globs", strings.Join(d.Globs, ","), "comma-separated file patterns")
	fromStart := fs.Bool("from-start", false, "read existing files from the beginning instead of the end")
	multiline := fs.Bool("multiline", d.Multiline, "aggregate lines that do not start with a timestamp into the previous log")
	fs.section("Output")
	intake := fs.Str("intake", d.IntakeURL, "intake base URL")
	apiKey := fs.Str("api-key", d.APIKey, "DD-API-KEY to send")
	encoding := fs.Str("encoding", d.Encoding, "gzip | zstd | none")
	batchLogs := fs.Int("batch-logs", d.BatchLogs, "max logs per payload")
	batchBytes := fs.Size("batch-bytes", int64(d.BatchBytes), "max (approximate, uncompressed) bytes per payload")
	batchWait := fs.Duration("batch-wait", d.BatchWait, "max time a log waits for a full batch")
	senders := fs.Int("senders", d.Senders, "concurrent HTTP senders")
	fs.section("Metadata")
	host := fs.Str("hostname", d.Hostname, "hostname field")
	service := fs.Str("service", "", "service field (default: derived from the file name)")
	source := fs.Str("source", d.Source, "ddsource field")
	tags := fs.Str("tags", d.Tags, "ddtags (filename:/dirname: are appended)")
	quiet := fs.Bool("quiet", false, "no status output")
	if !fs.parse(args) {
		return 2
	}
	cfg := d
	cfg.LogDir, cfg.Globs, cfg.FromStart, cfg.Multiline = *dir, strings.Split(*globs, ","), *fromStart, *multiline
	cfg.IntakeURL, cfg.APIKey, cfg.Encoding, cfg.BatchLogs, cfg.BatchBytes, cfg.BatchWait, cfg.Senders = *intake, *apiKey, *encoding, *batchLogs, int(*batchBytes), *batchWait, *senders
	cfg.Hostname, cfg.Service, cfg.Source, cfg.Tags, cfg.Quiet, cfg.Version = *host, *service, *source, *tags, *quiet, buildVersion
	sh, err := ship.New(cfg)
	if err != nil {
		return fail("ship: %v", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	if err := sh.Run(ctx); err != nil {
		return fail("ship: %v", err)
	}
	return 0
}
