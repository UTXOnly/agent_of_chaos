package intake

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"time"
)

// serveTCP accepts the agent's legacy TCP protocol. Each frame is the API
// key, a space, then the log, in one of two framings: the agent's default
// (logs_config.dev_mode_use_proto, true) is a 4-byte big-endian length
// prefix and a protobuf Log (message, status, timestamp, hostname, service,
// source, tags); with it off, a newline-terminated RFC 5424-style line
// "<PRI>0 TIMESTAMP HOSTNAME SERVICE - - [dd ddsource="..." ddsourcecategory="..." ddtags="..."] MESSAGE".
// The framing is detected from a connection's first byte.
//
// The same port also answers HTTP. The agent derives both its TCP endpoint
// and the HTTP endpoint of its connectivity check from one
// logs_config.logs_dd_url (host:port), so an agent pointed at this port
// decides between the transports from what it finds here: a working HTTP
// intake keeps it on HTTP, `outage` fails the check and it falls back to
// TCP, and when the outage clears its periodic re-check upgrades it back.
// A connection that opens with an HTTP request line is handed to the HTTP
// server through httpConns; everything else is read as TCP frames.
func (s *Server) serveTCP(ctx context.Context, ln net.Listener, httpConns *connListener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go s.handleTCPConn(ctx, conn, httpConns)
	}
}

// sniffTimeout bounds the wait for a new connection's first bytes before it
// is treated as a TCP frame stream.
const sniffTimeout = 30 * time.Second

// maxTCPFrame bounds one frame in either framing.
const maxTCPFrame = 8 << 20

func (s *Server) handleTCPConn(ctx context.Context, conn net.Conn, httpConns *connListener) {
	br := bufio.NewReaderSize(conn, 64<<10)
	conn.SetReadDeadline(time.Now().Add(sniffTimeout))
	head, _ := br.Peek(8)
	conn.SetReadDeadline(time.Time{})
	if looksHTTP(head) {
		if httpConns != nil && httpConns.push(&peekedConn{Conn: conn, r: br}) {
			s.logf("[tcp] HTTP request on the TCP port from %s: served by the HTTP intake (the agent's connectivity check, or its HTTP transport)", conn.RemoteAddr())
			return
		}
		conn.Close()
		return
	}
	defer conn.Close()
	if s.faults.get().TCPRefuse {
		s.stats.recordFault("dropped")
		s.logf("[tcp] connection from %s refused (fault)", conn.RemoteAddr())
		return
	}
	s.logf("[tcp] connection from %s", conn.RemoteAddr())
	fr := &frameReader{r: bufio.NewReader(&faultedTCPReader{r: br, s: s})}
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
	sampled := false
	for {
		frame, n, err := fr.next()
		if err != nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		bytes += int64(n)
		e, ok := parseTCPFrame(frame)
		if !ok && !sampled {
			// Neither framing decoded it; one sample per connection says
			// what came.
			sampled = true
			s.logf("[tcp] frame from %s outside the expected framing (%s): %.300q", conn.RemoteAddr(), fr.framing(), frame)
		}
		now := time.Now()
		analyze(&e, b, now)
		frames++
		if frames >= 500 || now.Sub(last) > time.Second {
			flush(b, now, frames, bytes)
			b, frames, bytes, last = &batch{}, 0, 0, now
			f := s.faults.get()
			if f.TCPRefuse || (f.TCPResetRate > 0 && rand.Float64() < f.TCPResetRate) {
				// Frames read so far are counted (they reached us); what the
				// agent has written since sits in a socket buffer and is
				// discarded with the connection, as a real reset would.
				s.stats.recordFault("dropped")
				s.logf("[tcp] connection from %s reset (fault)", conn.RemoteAddr())
				return
			}
		}
	}
	if frames > 0 {
		s.logf("[tcp] connection from %s closed: %s framing", conn.RemoteAddr(), fr.framing())
	}
	flush(b, time.Now(), frames, bytes)
}

// frameReader splits a TCP stream into frames in whichever framing the
// connection uses: a first byte of 0 is a length prefix (an API key or a
// "<PRI>" never starts with NUL), anything else is newline-terminated lines.
type frameReader struct {
	r              *bufio.Reader
	detected       bool
	lengthPrefixed bool
}

func (f *frameReader) framing() string {
	if f.lengthPrefixed {
		return "length-prefixed protobuf"
	}
	return "newline-delimited"
}

// next returns the next frame's content and its size on the wire
// including the delimiter.
func (f *frameReader) next() ([]byte, int, error) {
	if !f.detected {
		b, err := f.r.Peek(1)
		if err != nil {
			return nil, 0, err
		}
		f.lengthPrefixed, f.detected = b[0] == 0, true
	}
	if f.lengthPrefixed {
		var hdr [4]byte
		if _, err := io.ReadFull(f.r, hdr[:]); err != nil {
			return nil, 0, err
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n > maxTCPFrame {
			return nil, 0, errors.New("tcp frame too large")
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(f.r, buf); err != nil {
			return nil, 0, err
		}
		return buf, 4 + int(n), nil
	}
	line, err := f.r.ReadBytes('\n')
	if len(line) == 0 {
		return nil, 0, err
	}
	n := len(line)
	line = bytes.TrimRight(line, "\r\n")
	return line, n, nil
}

// parseTCPFrame decodes one frame into the same shape as an HTTP log: the
// API key prefix is dropped, then the rest is RFC 5424 text if it starts
// with "<", a protobuf Log otherwise. ok is false when neither decoded.
func parseTCPFrame(frame []byte) (logEntry, bool) {
	var e logEntry
	i := bytes.IndexByte(frame, ' ')
	if i < 0 {
		e.Message = string(frame)
		return e, false
	}
	rest := frame[i+1:]
	if len(rest) > 0 && rest[0] == '<' {
		return parseRFC5424(string(rest))
	}
	if pe, ok := decodeProtoLog(rest); ok {
		return pe, true
	}
	e.Message = string(rest)
	return e, false
}

// parseRFC5424 decodes "<PRI>0 TIMESTAMP HOSTNAME SERVICE - - [dd …] MESSAGE".
func parseRFC5424(rest string) (logEntry, bool) {
	var e logEntry
	j := strings.IndexByte(rest, '>')
	if j < 0 {
		e.Message = rest
		return e, false
	}
	rest = rest[j+1:]
	// version timestamp hostname service - -
	fields := strings.SplitN(rest, " ", 7)
	if len(fields) < 7 {
		e.Message = rest
		return e, false
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
	return e, true
}

// decodeProtoLog decodes the agent's pb.Log (agent-payload): 1 message,
// 2 status, 3 timestamp (ns), 4 hostname, 5 service, 6 source, 7 tags; all
// strings but the varint timestamp. A hand-rolled reader keeps protobuf out
// of the dependencies.
func decodeProtoLog(b []byte) (logEntry, bool) {
	var e logEntry
	var tags []string
	seen := false
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return e, false
		}
		b = b[n:]
		field, wt := key>>3, key&7
		switch wt {
		case 0: // varint
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return e, false
			}
			b = b[n:]
			if field == 3 {
				e.Timestamp = int64(v / 1e6)
				seen = true
			}
		case 2: // length-delimited
			l, n := binary.Uvarint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return e, false
			}
			v := string(b[n : n+int(l)])
			b = b[n+int(l):]
			switch field {
			case 1:
				e.Message = v
			case 2:
				e.Status = v
			case 4:
				e.Hostname = v
			case 5:
				e.Service = v
			case 6:
				e.Source = v
			case 7:
				tags = append(tags, v)
			}
			seen = true
		case 1:
			if len(b) < 8 {
				return e, false
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return e, false
			}
			b = b[4:]
		default:
			return e, false
		}
	}
	e.Tags = strings.Join(tags, ",")
	return e, seen
}

// faultedTCPReader applies read_bps to a TCP stream, picking the current
// fault state up at every read so a flapping fault comes and goes.
type faultedTCPReader struct {
	r *bufio.Reader
	s *Server
	t *throttledReader
}

func (f *faultedTCPReader) Read(p []byte) (int, error) {
	bps := f.s.faults.get().ReadBps
	if bps <= 0 {
		f.t = nil
		return f.r.Read(p)
	}
	if f.t == nil || f.t.bps != bps {
		f.t = &throttledReader{r: f.r, bps: bps, start: time.Now()}
		f.s.stats.recordFault("slowed")
	}
	return f.t.Read(p)
}

var httpMethods = [][]byte{[]byte("POST "), []byte("GET "), []byte("PUT "), []byte("HEAD "), []byte("DELETE "), []byte("OPTIONS "), []byte("PATCH ")}

// looksHTTP reports whether a connection's first bytes are an HTTP request
// line rather than a TCP frame (which starts with the API key).
func looksHTTP(head []byte) bool {
	for _, m := range httpMethods {
		if bytes.HasPrefix(head, m) {
			return true
		}
	}
	return false
}

// peekedConn is a connection whose first bytes were already buffered.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// connListener is a net.Listener fed by hand: the HTTP server serves on it
// and the TCP accept loop pushes the connections that turned out to be HTTP.
type connListener struct {
	addr  net.Addr
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newConnListener(addr net.Addr) *connListener {
	return &connListener{addr: addr, conns: make(chan net.Conn), done: make(chan struct{})}
}

// push hands a connection to the HTTP server; false once the listener is closed.
func (l *connListener) push(c net.Conn) bool {
	select {
	case l.conns <- c:
		return true
	case <-l.done:
		return false
	}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, errors.New("listener closed")
	}
}

func (l *connListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *connListener) Addr() net.Addr { return l.addr }

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
