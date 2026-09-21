// Package report defines the experiment report produced by the intake and
// consumed by `aoc report` / `aoc compare`.
package report

import "time"

// Schema is bumped when the JSON layout changes incompatibly.
const Schema = 1

// Report is a complete, self-describing summary of one run window.
type Report struct {
	Schema      int       `json:"schema"`
	Name        string    `json:"name"`
	GeneratedAt time.Time `json:"generated_at"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	Seconds     float64   `json:"seconds"`
	Notes       []string  `json:"notes,omitempty"`

	Agent      Agent        `json:"agent"`
	Generators []Generator  `json:"generators"`
	Delivery   Delivery     `json:"delivery"`
	Throughput Throughput   `json:"throughput"`
	Latency    Latency      `json:"latency"`
	HTTP       HTTP         `json:"http"`
	Tags       Tags         `json:"tags"`
	Breakdown  Breakdown    `json:"breakdown"`
	Streams    []Stream     `json:"streams"`
	Resources  Resources    `json:"resources"`
	Telemetry  []Telemetry  `json:"telemetry"`
	Faults     []FaultEvent `json:"faults"`
	Minutes    []Minute     `json:"minutes"`
	// Profiles are the agent's own continuous-profiler uploads over the
	// window, reduced to per-function tables (aoc run keeps the pprof files
	// under profiles/ in the results directory).
	Profiles []ProfileSummary `json:"profiles,omitempty"`
	// AgentLog summarises the agent's log output (agent.log) over the run.
	AgentLog *LogSummary `json:"agent_log,omitempty"`
}

// Agent identifies the agent under test from what it sent.
type Agent struct {
	Versions  map[string]int64 `json:"versions"`   // DD-EVP-ORIGIN-VERSION → requests
	Origins   map[string]int64 `json:"origins"`    // DD-EVP-ORIGIN
	UserAgent map[string]int64 `json:"user_agent"` // User-Agent
	APIKeys   map[string]int64 `json:"api_keys"`   // masked
	Encodings map[string]int64 `json:"encodings"`  // Content-Encoding → requests
	Paths     map[string]int64 `json:"paths"`      // logs endpoint → requests
	Image     string           `json:"image,omitempty"`
	ImageID   string           `json:"image_id,omitempty"`  // docker image ID the container ran (aoc run)
	Digest    string           `json:"digest,omitempty"`    // registry digest, repo@sha256:… (aoc run, pulled images)
	Commit    string           `json:"commit,omitempty"`    // git.commit.sha the agent's profiler reported
	Repo      string           `json:"repo,omitempty"`      // git.repository_url from the same tags
	Container string           `json:"container,omitempty"` // docker container name observed for CPU/memory
	Hostname  string           `json:"hostname,omitempty"`  // most common hostname in received logs
}

// Generator summarizes one generator process over the window.
type Generator struct {
	Name          string    `json:"name"`
	Host          string    `json:"host"`
	Mode          string    `json:"mode"`
	Scenario      string    `json:"scenario"`
	Phase         string    `json:"phase"`
	Format        string    `json:"format"`
	Output        string    `json:"output"`
	ActiveStreams int       `json:"active_streams"` // peak concurrent streams seen
	TargetRate    float64   `json:"target_rate"`
	Deterministic bool      `json:"deterministic"`
	Seed          uint64    `json:"seed"`
	RotateBytes   int64     `json:"rotate_bytes"`
	RotateKeep    int       `json:"rotate_keep"`
	RotateMode    string    `json:"rotate_mode"`
	Records       int64     `json:"records"`
	Lines         int64     `json:"lines"`
	Bytes         int64     `json:"bytes"`
	Rotations     int64     `json:"rotations"`
	Multiline     int64     `json:"multiline"`
	Wide          int64     `json:"wide"`
	Bursts        int64     `json:"bursts"`
	WriteErrors   int64     `json:"write_errors"`
	CPUAvgPercent float64   `json:"cpu_avg_percent"`
	RSSMaxBytes   int64     `json:"rss_max_bytes"`
	FirstSeen     time.Time `json:"first_seen"`
	LastSeen      time.Time `json:"last_seen"`
	Final         bool      `json:"final"`
}

// Delivery is the core question: did everything the generators wrote arrive
// exactly once?
type Delivery struct {
	GeneratedRecords int64   `json:"generated_records"`
	GeneratedLines   int64   `json:"generated_lines"`
	GeneratedBytes   int64   `json:"generated_bytes"`
	ReceivedLogs     int64   `json:"received_logs"`   // every log the intake accepted
	ReceivedMarked   int64   `json:"received_marked"` // logs carrying an aoc= marker (incl. dups)
	Unique           int64   `json:"unique"`          // distinct markers
	Duplicates       int64   `json:"duplicates"`
	Missing          int64   `json:"missing"` // generated but never received (outstanding while generators run)
	OutOfOrder       int64   `json:"out_of_order"`
	Orphans          int64   `json:"orphans"`  // continuation lines delivered as separate logs
	Unmarked         int64   `json:"unmarked"` // logs from elsewhere (agent's own container logs, etc.)
	Multiline        int64   `json:"multiline"`
	MultilineWritten int64   `json:"multiline_written"`
	ReceivedLines    int64   `json:"received_lines"` // physical lines inside received messages
	Truncated        int64   `json:"truncated"`
	Ratio            float64 `json:"ratio"` // unique / generated
	AllFinal         bool    `json:"all_generators_final"`
}

// Throughput are window averages plus the peak second.
type Throughput struct {
	GenRecordsPerSec     float64 `json:"gen_records_per_sec"`
	GenLinesPerSec       float64 `json:"gen_lines_per_sec"`
	GenBytesPerSec       float64 `json:"gen_bytes_per_sec"`
	RecvLogsPerSec       float64 `json:"recv_logs_per_sec"`
	RecvRawBytesPerSec   float64 `json:"recv_raw_bytes_per_sec"`
	RecvWireBytesPerSec  float64 `json:"recv_wire_bytes_per_sec"`
	CompressionRatio     float64 `json:"compression_ratio"` // raw / wire
	PeakRecvLogsPerSec   int64   `json:"peak_recv_logs_per_sec"`
	PeakGenRecordsPerSec float64 `json:"peak_gen_records_per_sec"`
	RequestsPerSec       float64 `json:"requests_per_sec"`
}

// Quantiles summarize a distribution (seconds for latencies, bytes/counts
// for sizes).
type Quantiles struct {
	Count uint64  `json:"count"`
	Mean  float64 `json:"mean"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P99   float64 `json:"p99"`
	P999  float64 `json:"p999"`
	Max   float64 `json:"max"`
	Min   float64 `json:"min"`
}

type Latency struct {
	EndToEnd    Quantiles `json:"end_to_end"`   // line written → intake received
	Sender      Quantiles `json:"sender"`       // agent encode time → intake received
	Processing  Quantiles `json:"processing"`   // intake handler time (sanity)
	NoTimestamp int64     `json:"no_timestamp"` // marked logs without a parseable write time
}

type HTTP struct {
	Requests       int64            `json:"requests"`
	ByStatus       map[string]int64 `json:"by_status"`
	Probes         int64            `json:"probes"`
	Malformed      int64            `json:"malformed"`
	Rejected       map[string]int64 `json:"rejected"`
	WireBytes      int64            `json:"wire_bytes"`
	RawBytes       int64            `json:"raw_bytes"`
	PayloadWire    Quantiles        `json:"payload_wire_bytes"`
	PayloadRaw     Quantiles        `json:"payload_raw_bytes"`
	LogsPerPayload Quantiles        `json:"logs_per_payload"`
	FaultDropped   int64            `json:"fault_dropped"`
	FaultErrored   int64            `json:"fault_errored"`
	FaultDelayed   int64            `json:"fault_delayed"`
	FaultSlowed    int64            `json:"fault_slowed"`
	OtherRequests  map[string]int64 `json:"other_requests"` // non-logs agent traffic by path
	TCPFrames      int64            `json:"tcp_frames"`
	TCPBytes       int64            `json:"tcp_bytes"`
}

type Tags struct {
	TotalTags         int64       `json:"total_tags"`
	TotalTagBytes     int64       `json:"total_tag_bytes"`
	AvgTagsPerLog     float64     `json:"avg_tags_per_log"`
	AvgTagBytesPerLog float64     `json:"avg_tag_bytes_per_log"`
	UniqueKeys        int         `json:"unique_keys"`
	Keys              []NameCount `json:"keys"`
}

type NameCount struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
	Bytes int64  `json:"bytes,omitempty"`
}

type Breakdown struct {
	Services []NameCount `json:"services"`
	Sources  []NameCount `json:"sources"`
	Hosts    []NameCount `json:"hosts"`
	Statuses []NameCount `json:"statuses"`
}

// Stream is the delivery ledger for one generator stream.
type Stream struct {
	Gen        string     `json:"gen"`
	Stream     string     `json:"stream"`
	Generated  int64      `json:"generated"` // last seq assigned by the generator (0 if unknown)
	Received   int64      `json:"received"`
	Unique     int64      `json:"unique"`
	Duplicates int64      `json:"duplicates"`
	Missing    int64      `json:"missing"`
	OutOfOrder int64      `json:"out_of_order"`
	MaxSeq     int64      `json:"max_seq"`
	Bytes      int64      `json:"bytes"`
	Multiline  int64      `json:"multiline"`
	FirstAt    time.Time  `json:"first_at"`
	LastAt     time.Time  `json:"last_at"`
	Gaps       []SeqRange `json:"gaps,omitempty"`
	Active     bool       `json:"active"`
}

type SeqRange struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// Resources are the agent's CPU/memory over the window.
type Resources struct {
	Samples            int     `json:"samples"`
	ContainerCPUAvg    float64 `json:"container_cpu_avg_percent"`
	ContainerCPUMax    float64 `json:"container_cpu_max_percent"`
	ContainerMemAvg    int64   `json:"container_mem_avg_bytes"`
	ContainerMemMax    int64   `json:"container_mem_max_bytes"`
	ProcessCPUAvg      float64 `json:"process_cpu_avg_percent"`
	ProcessCPUMax      float64 `json:"process_cpu_max_percent"`
	ProcessRSSMax      int64   `json:"process_rss_max_bytes"`
	ProcessCPUSecs     float64 `json:"process_cpu_seconds"` // delta of process_cpu_seconds_total
	IntakeCPUAvg       float64 `json:"intake_cpu_avg_percent"`
	IntakeRSSMax       int64   `json:"intake_rss_max_bytes"`
	GenCPUAvg          float64 `json:"gen_cpu_avg_percent"`          // summed across generators
	CPUSecondsPerMLogs float64 `json:"cpu_seconds_per_million_logs"` // process CPU per 1M logs delivered
	// cgroup memory breakdown of the container: anon is the processes' own
	// memory, file the page cache charged to the container.
	ContainerAnonAvg int64 `json:"container_anon_avg_bytes,omitempty"`
	ContainerAnonMax int64 `json:"container_anon_max_bytes,omitempty"`
	ContainerFileAvg int64 `json:"container_file_avg_bytes,omitempty"`
	ContainerFileMax int64 `json:"container_file_max_bytes,omitempty"`
	// Processes is every process in the container (docker top), largest
	// RSS first: which process a container-level change belongs to.
	Processes []ProcessStat `json:"processes,omitempty"`
}

// ProcessStat is one process (by command name; same-named processes are
// summed) in the agent container over the window.
type ProcessStat struct {
	Name    string  `json:"name"`
	RSSAvg  int64   `json:"rss_avg_bytes"`
	RSSMax  int64   `json:"rss_max_bytes"`
	CPUAvg  float64 `json:"cpu_avg_percent"`
	CPUMax  float64 `json:"cpu_max_percent"`
	Samples int     `json:"samples"`
}

// ProfileSummary is one profile view (a service, a file, a sample type)
// merged over the window: the total and the top functions by flat value.
// Rates (CPU %, allocation B/s) are per wall-clock second; levels (heap in
// use, goroutines) are averages over the captures.
type ProfileSummary struct {
	Service     string  `json:"service"`
	File        string  `json:"file"`
	SampleType  string  `json:"sample_type"`
	Label       string  `json:"label"`
	Unit        string  `json:"unit"`
	Captures    int     `json:"captures"`
	WallSeconds float64 `json:"wall_seconds"`
	Total       float64 `json:"total"`
	Top         []Frame `json:"top"`
}

// Frame is one function's flat and cumulative value in a ProfileSummary,
// with where it lives (the source file and line of the sampled leaf).
type Frame struct {
	Function string  `json:"function"`
	File     string  `json:"file,omitempty"`
	Line     int64   `json:"line,omitempty"`
	Flat     float64 `json:"flat"`
	Cum      float64 `json:"cum"`
}

// LogSummary counts the agent's log lines by level and keeps the most
// repeated warnings and errors (message with numbers replaced by #).
type LogSummary struct {
	Lines     int64       `json:"lines"`
	Errors    int64       `json:"errors"`
	Warnings  int64       `json:"warnings"`
	Truncated bool        `json:"truncated,omitempty"` // only the tail of the log was captured
	Top       []NameCount `json:"top,omitempty"`       // "LEVEL | message" → count
}

// Telemetry is one agent-internal metric over the window.
type Telemetry struct {
	Name   string  `json:"name"`
	Labels string  `json:"labels"`
	Type   string  `json:"type"`
	First  float64 `json:"first"`
	Last   float64 `json:"last"`
	Delta  float64 `json:"delta"`
	Rate   float64 `json:"rate"`           // delta / seconds for counters
	Mean   float64 `json:"mean,omitempty"` // average over the window's scrapes, gauges only
}

type FaultEvent struct {
	At   time.Time `json:"at"`
	Desc string    `json:"desc"`
}

// Minute is a per-minute rollup for tables and comparisons.
type Minute struct {
	T          int64   `json:"t"`
	GenRecords float64 `json:"gen_records_per_sec"`
	RecvLogs   float64 `json:"recv_logs_per_sec"`
	RawBytes   float64 `json:"raw_bytes_per_sec"`
	WireBytes  float64 `json:"wire_bytes_per_sec"`
	Requests   float64 `json:"requests_per_sec"`
	Errors     int64   `json:"errors"`
	E2EP50     float64 `json:"e2e_p50"`
	E2EP99     float64 `json:"e2e_p99"`
	AgentCPU   float64 `json:"agent_cpu"`
	AgentMem   int64   `json:"agent_mem"`
	ProcCPU    float64 `json:"proc_cpu"`
}
