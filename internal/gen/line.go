package gen

import (
	"strconv"
	"time"
)

// Marker is the prefix of the per-record sequence marker embedded in every
// generated log line. The intake parses it to account for delivery.
//
//	plain: ... [auth-service] aoc=plain-01/auth-service/1234 message
//	json:  {"timestamp":"...","level":"INFO","aoc":"plain-01/auth-service/1234",...}
const Marker = "aoc="

// synthBase is the timestamp origin for deterministic runs. Timestamps are
// base + seq microseconds, so file bytes only depend on seed + config.
var synthBase = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

const tsLayout = "2006-01-02T15:04:05"

// clock produces RFC3339-ish microsecond timestamps with a cached per-second
// prefix, so the hot path only appends six digits and a 'Z'.
type clock struct {
	lastSec int64
	prefix  []byte // "2026-09-20T02:02:24."
}

func (c *clock) append(buf []byte, t time.Time) []byte {
	sec := t.Unix()
	if sec != c.lastSec || c.prefix == nil {
		c.prefix = t.UTC().AppendFormat(c.prefix[:0], tsLayout)
		c.prefix = append(c.prefix, '.')
		c.lastSec = sec
	}
	buf = append(buf, c.prefix...)
	buf = appendPadInt(buf, t.Nanosecond()/1000, 6)
	return append(buf, 'Z')
}

// appendJSONString appends s as a quoted JSON string. Non-ASCII UTF-8 is
// written raw (valid JSON); only quotes, backslashes and control characters
// are escaped.
func appendJSONString(buf []byte, s []byte) []byte {
	buf = append(buf, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		buf = append(buf, s[start:i]...)
		switch c {
		case '"':
			buf = append(buf, '\\', '"')
		case '\\':
			buf = append(buf, '\\', '\\')
		case '\n':
			buf = append(buf, '\\', 'n')
		case '\r':
			buf = append(buf, '\\', 'r')
		case '\t':
			buf = append(buf, '\\', 't')
		default:
			buf = append(buf, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
		}
		start = i + 1
	}
	buf = append(buf, s[start:]...)
	return append(buf, '"')
}

const hexDigits = "0123456789abcdef"

// record is one logical log event: a primary message plus an optional
// multi-line stack trace. It renders to one plain-text line group and/or one
// JSON line.
type record struct {
	level Level
	msg   []byte // rendered message
	trace []byte // rendered stack trace ('\n'-separated), or empty
	exc   exception
	seq   int64
	ts    time.Time
}

// appendPlain renders r as plain text (one or more physical lines, each
// newline terminated) and returns the buffer plus the number of physical lines.
func (s *Stream) appendPlain(buf []byte, r *record) ([]byte, int) {
	buf = s.clk.append(buf, r.ts)
	buf = append(buf, ' ')
	buf = append(buf, levelNames[r.level]...)
	buf = append(buf, " [aoc."...)
	buf = append(buf, s.name...)
	buf = append(buf, "] "...)
	buf = s.appendMarker(buf, r.seq)
	buf = append(buf, ' ')
	buf = append(buf, r.msg...)
	lines := 1
	if len(r.trace) > 0 {
		buf = append(buf, '\n')
		buf = append(buf, r.trace...)
		lines += countByte(r.trace, '\n') + 1
	}
	buf = append(buf, '\n')
	return buf, lines
}

// appendJSON renders r as a single JSON line.
func (s *Stream) appendJSON(buf []byte, r *record) []byte {
	buf = append(buf, `{"timestamp":"`...)
	buf = s.clk.append(buf, r.ts)
	buf = append(buf, `","level":"`...)
	buf = append(buf, levelNamesTrim[r.level]...)
	buf = append(buf, `","aoc":"`...)
	buf = s.appendMarkerValue(buf, r.seq)
	buf = append(buf, `","service":"`...)
	buf = append(buf, s.name...)
	buf = append(buf, `","host":"`...)
	buf = append(buf, s.host...)
	buf = append(buf, `","env":"stress-test","logger":"aoc.`...)
	buf = append(buf, s.name...)
	buf = append(buf, `","message":`...)
	buf = appendJSONString(buf, r.msg)
	if len(r.trace) > 0 {
		buf = append(buf, `,"error":{"kind":`...)
		buf = appendJSONString(buf, []byte(r.exc.kind))
		buf = append(buf, `,"message":`...)
		buf = appendJSONString(buf, []byte(r.exc.msg))
		buf = append(buf, `,"stack":`...)
		buf = appendJSONString(buf, r.trace)
		buf = append(buf, '}')
	}
	buf = append(buf, '}', '\n')
	return buf
}

func (s *Stream) appendMarker(buf []byte, seq int64) []byte {
	buf = append(buf, Marker...)
	return s.appendMarkerValue(buf, seq)
}

func (s *Stream) appendMarkerValue(buf []byte, seq int64) []byte {
	buf = append(buf, s.markerPrefix...) // "gen/stream/"
	return strconv.AppendInt(buf, seq, 10)
}

func countByte(b []byte, c byte) int {
	n := 0
	for _, x := range b {
		if x == c {
			n++
		}
	}
	return n
}
