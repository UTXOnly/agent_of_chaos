package intake

import (
	"bufio"
	"context"
	"net"
	"strings"
	"time"
)

// serveTCP accepts the agent's legacy TCP protocol: one frame per line,
// "<api-key> <RFC5424-ish line>\n", where the line is
// "<PRI>0 TIMESTAMP HOSTNAME SERVICE - - [dd ddsource="..." ddsourcecategory="..." ddtags="..."] MESSAGE".
// Multi-line messages arrive as separate frames on this path, so the HTTP
// path is the one to use for multiline accuracy.
func (s *Server) serveTCP(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go s.handleTCPConn(ctx, conn)
	}
}

func (s *Server) handleTCPConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	s.logf("[tcp] connection from %s", conn.RemoteAddr())
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	flush := func(b *batch, now time.Time, frames int, bytes int64) {
		if frames == 0 {
			return
		}
		b.status = 202
		b.path = "tcp"
		b.wire, b.raw = bytes, bytes
		s.stats.merge(b, now)
		s.stats.recordTCP(frames, bytes)
	}
	b := &batch{}
	frames, bytes := 0, int64(0)
	last := time.Now()
	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := sc.Bytes()
		bytes += int64(len(line)) + 1
		e := parseTCPFrame(string(line))
		now := time.Now()
		analyze(&e, b, now)
		frames++
		if frames >= 500 || now.Sub(last) > time.Second {
			flush(b, now, frames, bytes)
			b, frames, bytes, last = &batch{}, 0, 0, now
		}
	}
	flush(b, time.Now(), frames, bytes)
}

// parseTCPFrame decodes one TCP frame into the same shape as an HTTP log.
func parseTCPFrame(line string) logEntry {
	var e logEntry
	// api key prefix
	i := strings.IndexByte(line, ' ')
	if i < 0 {
		e.Message = line
		return e
	}
	rest := line[i+1:]
	if !strings.HasPrefix(rest, "<") {
		e.Message = rest
		return e
	}
	j := strings.IndexByte(rest, '>')
	if j < 0 {
		e.Message = rest
		return e
	}
	rest = rest[j+1:]
	// version timestamp hostname service - -
	fields := strings.SplitN(rest, " ", 7)
	if len(fields) < 7 {
		e.Message = rest
		return e
	}
	if ts, ok := parseTS(fields[1]); ok {
		e.Timestamp = ts.UnixMilli()
	}
	e.Hostname, e.Service = fields[2], fields[3]
	if e.Service == "-" {
		e.Service = ""
	}
	rest = fields[6]
	if strings.HasPrefix(rest, "[dd ") {
		if k := strings.Index(rest, "] "); k > 0 {
			meta := rest[4:k]
			rest = rest[k+2:]
			e.Source = quoted(meta, "ddsource=")
			e.Tags = quoted(meta, "ddtags=")
		}
	}
	e.Message = rest
	return e
}

func quoted(meta, key string) string {
	i := strings.Index(meta, key+`"`)
	if i < 0 {
		return ""
	}
	v := meta[i+len(key)+1:]
	if j := strings.IndexByte(v, '"'); j >= 0 {
		return v[:j]
	}
	return v
}
