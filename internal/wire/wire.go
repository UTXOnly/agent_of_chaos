// Package wire holds the JSON contract between the generator and the intake:
// the periodic stats push, and the marker embedded in every log line.
package wire

import (
	"strconv"
	"strings"
	"time"
)

// GenReportPath is where generators POST their GenReport.
const GenReportPath = "/api/v1/gen/report"

// GenTotals are cumulative counters since the generator started.
type GenTotals struct {
	Lines     int64 `json:"lines"`     // physical newline-terminated lines written
	Records   int64 `json:"records"`   // logical events (one aoc= marker each)
	Bytes     int64 `json:"bytes"`     // bytes written (all formats)
	Rotations int64 `json:"rotations"` // file rotations
	Multiline int64 `json:"multiline"` // plain-text records that spanned several physical lines
	Wide      int64 `json:"wide"`      // wide (oversized) records
	Bursts    int64 `json:"bursts"`    // burst events
	Errors    int64 `json:"errors"`    // write errors
}

func (t *GenTotals) Add(o GenTotals) {
	t.Lines += o.Lines
	t.Records += o.Records
	t.Bytes += o.Bytes
	t.Rotations += o.Rotations
	t.Multiline += o.Multiline
	t.Wide += o.Wide
	t.Bursts += o.Bursts
	t.Errors += o.Errors
}

// GenStream is one stream's state inside a GenReport.
type GenStream struct {
	Name   string `json:"name"`
	Host   string `json:"host"`
	Active bool   `json:"active"`
	Seq    int64  `json:"seq"` // last sequence number assigned
	GenTotals
}

// GenKnobs is the live phase configuration at report time.
type GenKnobs struct {
	Rate          float64 `json:"rate"` // aggregate target lines/s (0 = unlimited)
	BurstSize     int     `json:"burst_size"`
	BurstInterval string  `json:"burst_interval"`
	MultilineRate float64 `json:"multiline_rate"`
	WideLineRate  float64 `json:"wide_line_rate"`
}

// GenReport is the generator's periodic self-report.
type GenReport struct {
	Gen       string    `json:"gen"`
	Host      string    `json:"host"`
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	TS        time.Time `json:"ts"`
	Final     bool      `json:"final"`

	Mode          string   `json:"mode"`
	Scenario      string   `json:"scenario"`
	Phase         string   `json:"phase"`
	ActiveStreams int      `json:"active_streams"`
	Knobs         GenKnobs `json:"knobs"`

	Format        string `json:"format"`
	Output        string `json:"output"`
	LogDir        string `json:"log_dir"`
	Deterministic bool   `json:"deterministic"`
	Seed          uint64 `json:"seed"`
	RotateBytes   int64  `json:"rotate_bytes"`
	RotateKeep    int    `json:"rotate_keep"`
	RotateMode    string `json:"rotate_mode"`

	Totals  GenTotals   `json:"totals"`
	Streams []GenStream `json:"streams"`

	CPUSeconds float64 `json:"cpu_seconds"`
	CPUPercent float64 `json:"cpu_percent"` // over the last report interval
	RSSBytes   int64   `json:"rss_bytes"`
}

// Marker is the token that precedes "<gen>/<stream>/<seq>" in plain lines.
const Marker = "aoc="

// JSONMarker is the JSON key/value prefix in JSON lines.
const JSONMarker = `"aoc":"`

// ParsedMarker is a decoded marker.
type ParsedMarker struct {
	Gen    string
	Stream string
	Seq    int64
}

// FindMarker locates and parses the first aoc marker in a log message. It
// works for plain lines (aoc=gen/stream/seq) and JSON lines ("aoc":"...").
// ok is false when no well-formed marker exists.
func FindMarker(msg string) (m ParsedMarker, ok bool) {
	i := strings.Index(msg, Marker)
	if i >= 0 {
		return parseMarkerValue(msg[i+len(Marker):])
	}
	i = strings.Index(msg, JSONMarker)
	if i >= 0 {
		return parseMarkerValue(msg[i+len(JSONMarker):])
	}
	return m, false
}

const maxMarkerPart = 128

func parseMarkerValue(b string) (m ParsedMarker, ok bool) {
	s1 := strings.IndexByte(b, '/')
	if s1 <= 0 || s1 > maxMarkerPart {
		return m, false
	}
	rest := b[s1+1:]
	s2 := strings.IndexByte(rest, '/')
	if s2 <= 0 || s2 > maxMarkerPart {
		return m, false
	}
	digits := rest[s2+1:]
	n := 0
	for n < len(digits) && digits[n] >= '0' && digits[n] <= '9' {
		n++
	}
	if n == 0 || n > 18 {
		return m, false
	}
	seq, err := strconv.ParseInt(digits[:n], 10, 64)
	if err != nil {
		return m, false
	}
	return ParsedMarker{Gen: b[:s1], Stream: rest[:s2], Seq: seq}, true
}
