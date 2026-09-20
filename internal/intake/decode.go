package intake

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"

	"github.com/UTXOnly/agent_of_chaos/internal/wire"
)

// logEntry is one element of the agent's /api/v2/logs JSON array.
type logEntry struct {
	Message   string `json:"message"`
	Status    string `json:"status"`
	Timestamp int64  `json:"timestamp"` // agent encode time, ms since epoch
	Hostname  string `json:"hostname"`
	Service   string `json:"service"`
	Source    string `json:"ddsource"`
	Tags      string `json:"ddtags"`
}

var (
	zstdOnce sync.Once
	zstdDec  *zstd.Decoder
	gzipPool = sync.Pool{New: func() any { return new(gzip.Reader) }}
)

var errTooLarge = errors.New("payload exceeds size limit")

// decompress inflates body per Content-Encoding, capping the inflated size
// at limit. lenient=true falls back to the raw body when it already looks
// like JSON, because the agent's HTTP connectivity probe sends a bare "{}"
// under a gzip header.
func decompress(body []byte, encoding string, limit int64, lenient bool) ([]byte, error) {
	enc := strings.ToLower(strings.TrimSpace(encoding))
	var out []byte
	var err error
	switch enc {
	case "", "identity":
		if int64(len(body)) > limit {
			return nil, errTooLarge
		}
		return body, nil
	case "gzip", "x-gzip":
		out, err = gunzip(body, limit)
	case "zstd":
		zstdOnce.Do(func() { zstdDec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0)) })
		out, err = zstdDec.DecodeAll(body, make([]byte, 0, min(int64(len(body))*4, limit+1)))
		if err == nil && int64(len(out)) > limit {
			err = errTooLarge
		}
	case "deflate", "zlib":
		var zr io.ReadCloser
		zr, err = zlib.NewReader(bytes.NewReader(body))
		if err == nil {
			out, err = readLimited(zr, limit)
			zr.Close()
		}
	default:
		err = fmt.Errorf("unsupported content-encoding %q", encoding)
	}
	if err != nil && !errors.Is(err, errTooLarge) && lenient && looksLikeJSON(body) {
		return body, nil
	}
	return out, err
}

func gunzip(body []byte, limit int64) ([]byte, error) {
	zr := gzipPool.Get().(*gzip.Reader)
	defer gzipPool.Put(zr)
	if err := zr.Reset(bytes.NewReader(body)); err != nil {
		return nil, err
	}
	zr.Multistream(true)
	return readLimited(zr, limit)
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	n, err := buf.ReadFrom(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, errTooLarge
	}
	return buf.Bytes(), nil
}

func looksLikeJSON(b []byte) bool {
	b = bytes.TrimLeft(b, " \t\r\n")
	return len(b) > 0 && (b[0] == '{' || b[0] == '[')
}

// payloadKind classifies a decoded body.
type payloadKind int

const (
	kindLogs payloadKind = iota
	kindProbe
	kindMalformed
)

// parsePayload decodes the JSON array. An empty body or "{}" is the agent's
// connectivity probe.
func parsePayload(raw []byte) ([]logEntry, payloadKind) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("{}")) {
		return nil, kindProbe
	}
	var entries []logEntry
	if t[0] == '[' {
		if err := json.Unmarshal(t, &entries); err != nil {
			return nil, kindMalformed
		}
		return entries, kindLogs
	}
	if t[0] == '{' {
		var one logEntry
		if err := json.Unmarshal(t, &one); err != nil {
			return nil, kindMalformed
		}
		if one.Message == "" && one.Service == "" && one.Timestamp == 0 {
			return nil, kindProbe
		}
		return []logEntry{one}, kindLogs
	}
	return nil, kindMalformed
}

const truncatedMarker = "...TRUNCATED..."

// analyze folds one log into the batch: sizes, marker, latencies, tags and
// breakdowns.
func analyze(e *logEntry, b *batch, now time.Time) {
	msg := e.Message
	b.logs++
	b.msgBytes += int64(len(msg))
	newlines := physicalLineBreaks(msg)
	if strings.HasSuffix(msg, truncatedMarker) || strings.HasPrefix(msg, truncatedMarker) {
		b.truncated++
	}
	if e.Tags != "" {
		b.tagBytes += int64(len(e.Tags))
		rest := e.Tags
		for rest != "" {
			var tag string
			if i := strings.IndexByte(rest, ','); i >= 0 {
				tag, rest = rest[:i], rest[i+1:]
			} else {
				tag, rest = rest, ""
			}
			if tag == "" {
				continue
			}
			b.tagCount++
			key := tag
			if i := strings.IndexByte(tag, ':'); i > 0 {
				key = tag[:i]
			}
			if b.tagKeys == nil {
				b.tagKeys = map[string]int64{}
			}
			b.tagKeys[key]++
		}
	}

	// Line and multiline counts are about the generator's streams, so only
	// marked logs contribute; other sources are reported as "unmarked".
	marked := false
	if m, ok := wire.FindMarker(msg); ok {
		marked = true
		b.marked++
		b.lines += int64(1 + newlines)
		if newlines > 0 {
			b.multiline++
		}
		b.obs = append(b.obs, seqObs{gen: m.Gen, stream: m.Stream, seq: m.Seq, bytes: int64(len(msg)), newlines: newlines})
	} else if looksLikeContinuation(msg) {
		b.orphans++
	} else {
		b.unmarked++
	}

	// End-to-end latency: written (timestamp inside the line) → received.
	if ts, ok := writeTimestamp(msg); ok {
		if ts.Year() >= 2010 { // deterministic runs stamp year 2000
			d := now.Sub(ts).Seconds()
			if d < 0 {
				d = 0
			}
			b.e2e = append(b.e2e, d)
		} else {
			b.e2eNoTS++
		}
	} else if marked {
		b.e2eNoTS++
	}
	if e.Timestamp > 0 {
		d := now.Sub(time.UnixMilli(e.Timestamp)).Seconds()
		if d < 0 {
			d = 0
		}
		b.sender = append(b.sender, d)
	}

	if b.services == nil {
		b.services = map[string]*nameCount{}
		b.sources = map[string]*nameCount{}
		b.hosts = map[string]*nameCount{}
		b.statuses = map[string]*nameCount{}
	}
	bump(b.services, e.Service, int64(len(msg)))
	bump(b.sources, e.Source, int64(len(msg)))
	bump(b.hosts, e.Hostname, int64(len(msg)))
	bump(b.statuses, e.Status, int64(len(msg)))
}

// physicalLineBreaks counts the line breaks the agent folded into one log.
// The agent's multi_line aggregation joins lines with a literal backslash-n
// (two characters), while other paths keep real newlines, so both count.
// JSON records are single-line by construction: their escaped "\n" inside
// string values are content, not line breaks.
func physicalLineBreaks(msg string) int {
	if len(msg) > 0 && msg[0] == '{' {
		return 0
	}
	return strings.Count(msg, "\n") + strings.Count(msg, `\n`)
}

// looksLikeContinuation guesses whether an unmarked message is a stack-trace
// line that the agent failed to aggregate into its parent log.
func looksLikeContinuation(msg string) bool {
	if msg == "" {
		return false
	}
	switch msg[0] {
	case ' ', '\t':
		return true
	}
	for _, p := range [...]string{"Traceback (", "Caused by:", "at ", "panic:", "goroutine ", "File \"", "... ", "main.", "net/http"} {
		if strings.HasPrefix(msg, p) {
			return true
		}
	}
	// "ValueError: ..." / "java.lang.Foo: ..." / "KeyError: ..."
	if i := strings.IndexByte(msg, ':'); i > 0 && i < 80 {
		head := msg[:i]
		if strings.HasSuffix(head, "Error") || strings.HasSuffix(head, "Exception") {
			return true
		}
	}
	return false
}

// writeTimestamp extracts the generator's write time from a plain line
// ("2026-09-20T02:02:24.123456Z ...") or a JSON line ({"timestamp":"..."}).
func writeTimestamp(msg string) (time.Time, bool) {
	if t, ok := parseTS(msg); ok {
		return t, true
	}
	if strings.HasPrefix(msg, "{") {
		const key = `"timestamp":"`
		if i := strings.Index(msg, key); i >= 0 && i < 64 {
			return parseTS(msg[i+len(key):])
		}
	}
	return time.Time{}, false
}

// parseTS parses a leading "YYYY-MM-DDTHH:MM:SS[.fraction]Z" without
// allocating. It stops at the first byte that is not part of the timestamp.
func parseTS(s string) (time.Time, bool) {
	if len(s) < 20 {
		return time.Time{}, false
	}
	num := func(a, b int) (int, bool) {
		n := 0
		for i := a; i < b; i++ {
			c := s[i]
			if c < '0' || c > '9' {
				return 0, false
			}
			n = n*10 + int(c-'0')
		}
		return n, true
	}
	if s[4] != '-' || s[7] != '-' || s[10] != 'T' || s[13] != ':' || s[16] != ':' {
		return time.Time{}, false
	}
	y, ok1 := num(0, 4)
	mo, ok2 := num(5, 7)
	d, ok3 := num(8, 10)
	h, ok4 := num(11, 13)
	mi, ok5 := num(14, 16)
	sec, ok6 := num(17, 19)
	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6) {
		return time.Time{}, false
	}
	i := 19
	nanos := 0
	if i < len(s) && s[i] == '.' {
		i++
		scale := 100_000_000
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			if scale > 0 {
				nanos += int(s[i]-'0') * scale
				scale /= 10
			}
			i++
		}
	}
	loc := time.UTC
	if i < len(s) && s[i] == 'Z' {
		i++
	} else if i < len(s) && (s[i] == '+' || s[i] == '-') && i+6 <= len(s) {
		// ±HH:MM offset
		oh, okh := num(i+1, i+3)
		om, okm := num(i+4, i+6)
		if !okh || !okm || s[i+3] != ':' {
			return time.Time{}, false
		}
		off := (oh*60 + om) * 60
		if s[i] == '-' {
			off = -off
		}
		loc = time.FixedZone("", off)
	} else {
		return time.Time{}, false
	}
	if mo < 1 || mo > 12 || d < 1 || d > 31 || h > 23 || mi > 59 || sec > 60 {
		return time.Time{}, false
	}
	return time.Date(y, time.Month(mo), d, h, mi, sec, nanos, loc), true
}
