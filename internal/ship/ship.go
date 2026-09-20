// Package ship is a reference log shipper: it tails files and POSTs them to
// an intake the way the Datadog Agent does (JSON arrays on /api/v2/logs,
// gzip or zstd, agent-style headers, retries on 429/5xx). It exists to
// validate the harness without an agent and to serve as a naive baseline.
package ship

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"

	"github.com/UTXOnly/agent_of_chaos/internal/fmtutil"
)

type Config struct {
	LogDir     string
	Globs      []string
	IntakeURL  string
	APIKey     string
	Encoding   string // gzip | zstd | none
	BatchLogs  int
	BatchBytes int
	BatchWait  time.Duration
	FromStart  bool
	Multiline  bool
	Senders    int
	Hostname   string
	Service    string // "" = derived from file name
	Source     string
	Tags       string
	Version    string
	Quiet      bool
}

func Default() Config {
	host, _ := os.Hostname()
	return Config{
		LogDir: "logs", Globs: []string{"*.log", "*.json.log"}, IntakeURL: "http://localhost:8282",
		APIKey: "aoc-ship", Encoding: "gzip", BatchLogs: 1000, BatchBytes: 5 << 20, BatchWait: time.Second,
		Multiline: true, Senders: 4, Hostname: host, Source: "agent-of-chaos", Tags: "shipper:aoc",
	}
}

type entry struct {
	Message   string `json:"message"`
	Status    string `json:"status"`
	Timestamp int64  `json:"timestamp"`
	Hostname  string `json:"hostname"`
	Service   string `json:"service"`
	Source    string `json:"ddsource"`
	Tags      string `json:"ddtags"`
}

type Shipper struct {
	cfg    Config
	out    chan entry
	client *http.Client
	files  atomic.Int64
	sent   atomic.Int64
	bytes  atomic.Int64
	errors atomic.Int64
	drops  atomic.Int64
	zenc   *zstd.Encoder
}

func New(cfg Config) (*Shipper, error) {
	switch cfg.Encoding {
	case "gzip", "zstd", "none", "identity", "":
	default:
		return nil, fmt.Errorf("encoding must be gzip, zstd or none")
	}
	if cfg.BatchLogs <= 0 {
		cfg.BatchLogs = 1000
	}
	if cfg.Senders <= 0 {
		cfg.Senders = 1
	}
	abs, err := filepath.Abs(cfg.LogDir)
	if err != nil {
		return nil, err
	}
	cfg.LogDir = abs
	s := &Shipper{cfg: cfg, out: make(chan entry, 10000), client: &http.Client{Timeout: 30 * time.Second}}
	if cfg.Encoding == "zstd" {
		s.zenc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	}
	return s, nil
}

// Run tails until ctx is cancelled, then flushes what it has.
func (s *Shipper) Run(ctx context.Context) error {
	if !s.cfg.Quiet {
		fmt.Fprintf(os.Stderr, "\nagent_of_chaos ship  dir=%s  globs=%s  → %s  (%s, batch=%d/%s)\n\n",
			s.cfg.LogDir, strings.Join(s.cfg.Globs, ","), s.cfg.IntakeURL, s.cfg.Encoding, s.cfg.BatchLogs, fmtutil.Duration(s.cfg.BatchWait))
	}
	var senders sync.WaitGroup
	for i := 0; i < s.cfg.Senders; i++ {
		senders.Add(1)
		go func() { defer senders.Done(); s.sender(ctx) }()
	}
	var tailers sync.WaitGroup
	seen := map[string]bool{}
	startup := true
	statusT := time.NewTicker(5 * time.Second)
	defer statusT.Stop()
	scanT := time.NewTicker(500 * time.Millisecond)
	defer scanT.Stop()
	scan := func() {
		for _, g := range s.cfg.Globs {
			matches, _ := filepath.Glob(filepath.Join(s.cfg.LogDir, g))
			for _, m := range matches {
				if seen[m] {
					continue
				}
				seen[m] = true
				s.files.Add(1)
				tailers.Add(1)
				go func(path string, fromStart bool) {
					defer tailers.Done()
					s.tail(ctx, path, fromStart)
				}(m, s.cfg.FromStart || !startup)
			}
		}
		startup = false
	}
	scan()
	for {
		select {
		case <-ctx.Done():
			tailers.Wait()
			close(s.out)
			senders.Wait()
			if !s.cfg.Quiet {
				fmt.Fprintf(os.Stderr, "\n  Done.  files=%d sent=%s bytes=%s errors=%d dropped=%d\n\n",
					s.files.Load(), fmtutil.Int(s.sent.Load()), fmtutil.Bytes(s.bytes.Load()), s.errors.Load(), s.drops.Load())
			}
			return nil
		case <-scanT.C:
			scan()
		case <-statusT.C:
			if !s.cfg.Quiet {
				fmt.Fprintf(os.Stderr, "  %s  files=%d sent=%s wire=%s errors=%d queued=%d\n", time.Now().Format("15:04:05"),
					s.files.Load(), fmtutil.Int(s.sent.Load()), fmtutil.Bytes(s.bytes.Load()), s.errors.Load(), len(s.out))
			}
		}
	}
}

func isTimestampLine(line string) bool {
	return len(line) > 20 && line[4] == '-' && line[7] == '-' && line[10] == 'T' && line[0] >= '0' && line[0] <= '9'
}

func (s *Shipper) service(path string) string {
	if s.cfg.Service != "" {
		return s.cfg.Service
	}
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".json.log")
	base = strings.TrimSuffix(base, ".log")
	return base
}

// tail follows one file across rename rotations (new inode) and truncation
// (size shrinks), aggregating continuation lines into the preceding log.
func (s *Shipper) tail(ctx context.Context, path string, fromStart bool) {
	svc := s.service(path)
	tags := s.cfg.Tags
	if tags != "" {
		tags += ","
	}
	tags += "filename:" + filepath.Base(path) + ",dirname:" + filepath.Dir(path)
	isJSON := strings.HasSuffix(path, ".json.log")

	var f *os.File
	var fi os.FileInfo
	var offset int64
	open := func(start int64) bool {
		var err error
		f, err = os.Open(path)
		if err != nil {
			return false
		}
		fi, _ = f.Stat()
		if start < 0 {
			offset, _ = f.Seek(0, io.SeekEnd)
		} else {
			offset, _ = f.Seek(start, io.SeekStart)
		}
		return true
	}
	start := int64(0)
	if !fromStart {
		start = -1
	}
	for !open(start) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	var pending strings.Builder
	havePending := false
	pendingAt := time.Now()
	emit := func(msg string) {
		e := entry{Message: msg, Status: "info", Hostname: s.cfg.Hostname, Service: svc, Source: s.cfg.Source, Tags: tags}
		select {
		case s.out <- e:
		case <-ctx.Done():
		}
	}
	flushPending := func() {
		if havePending {
			emit(pending.String())
			pending.Reset()
			havePending = false
		}
	}
	buf := make([]byte, 256<<10)
	var partial []byte
	processLine := func(line string) {
		if !s.cfg.Multiline || isJSON || isTimestampLine(line) {
			flushPending()
			pending.WriteString(line)
			havePending, pendingAt = true, time.Now()
			if !s.cfg.Multiline || isJSON {
				flushPending()
			}
		} else if havePending {
			pending.WriteByte('\n')
			pending.WriteString(line)
		} else {
			emit(line)
		}
	}
	// readAvailable consumes everything readable right now.
	readAvailable := func() bool {
		got := false
		for {
			n, err := f.Read(buf)
			if n == 0 {
				return got
			}
			got = true
			offset += int64(n)
			data := buf[:n]
			if len(partial) > 0 {
				data = append(partial, data...)
				partial = nil
			}
			for {
				i := bytes.IndexByte(data, '\n')
				if i < 0 {
					if len(data) > 0 {
						partial = append([]byte(nil), data...)
					}
					break
				}
				processLine(string(data[:i]))
				data = data[i+1:]
			}
			if err != nil {
				return got
			}
		}
	}
	for {
		if readAvailable() {
			continue
		}
		// EOF: flush a multiline log that has been idle, then check for
		// rotation or truncation.
		if havePending && time.Since(pendingAt) > 500*time.Millisecond {
			flushPending()
		}
		select {
		case <-ctx.Done():
			readAvailable()
			if len(partial) > 0 {
				processLine(string(partial))
				partial = nil
			}
			flushPending()
			return
		case <-time.After(100 * time.Millisecond):
		}
		st, statErr := os.Stat(path)
		switch {
		case statErr != nil:
			// removed: keep the handle in case it comes back
		case !os.SameFile(st, fi):
			// rotated by rename: drain the old inode, then follow the new file
			readAvailable()
			if len(partial) > 0 {
				processLine(string(partial))
				partial = nil
			}
			f.Close()
			open(0)
		case st.Size() < offset:
			// truncated in place
			offset, _ = f.Seek(0, io.SeekStart)
			partial = nil
		}
	}
}

func (s *Shipper) sender(ctx context.Context) {
	var batch []entry
	var size int
	timer := time.NewTimer(s.cfg.BatchWait)
	defer timer.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.send(ctx, batch)
		batch, size = nil, 0
	}
	for {
		select {
		case e, ok := <-s.out:
			if !ok {
				flush()
				return
			}
			e.Timestamp = time.Now().UnixMilli()
			batch = append(batch, e)
			size += len(e.Message) + 200
			if len(batch) >= s.cfg.BatchLogs || size >= s.cfg.BatchBytes {
				flush()
				timer.Reset(s.cfg.BatchWait)
			}
		case <-timer.C:
			flush()
			timer.Reset(s.cfg.BatchWait)
		}
	}
}

func (s *Shipper) send(ctx context.Context, batch []entry) {
	body, err := json.Marshal(batch)
	if err != nil {
		return
	}
	enc := ""
	switch s.cfg.Encoding {
	case "gzip":
		var b bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&b, gzip.BestSpeed)
		zw.Write(body)
		zw.Close()
		body, enc = b.Bytes(), "gzip"
	case "zstd":
		body, enc = s.zenc.EncodeAll(body, nil), "zstd"
	}
	url := strings.TrimRight(s.cfg.IntakeURL, "/") + "/api/v2/logs"
	backoff := 500 * time.Millisecond
	for attempt := 0; attempt < 10; attempt++ {
		req, _ := http.NewRequestWithContext(context.Background(), "POST", url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if enc != "" {
			req.Header.Set("Content-Encoding", enc)
		}
		req.Header.Set("DD-API-KEY", s.cfg.APIKey)
		req.Header.Set("DD-EVP-ORIGIN", "aoc-ship")
		req.Header.Set("DD-EVP-ORIGIN-VERSION", s.cfg.Version)
		req.Header.Set("dd-message-timestamp", strconv.FormatInt(batch[len(batch)-1].Timestamp, 10))
		req.Header.Set("dd-current-timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
		resp, err := s.client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 300 {
				s.sent.Add(int64(len(batch)))
				s.bytes.Add(int64(len(body)))
				return
			}
			s.errors.Add(1)
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 && resp.StatusCode != 408 {
				s.drops.Add(int64(len(batch))) // non-retryable, like the agent
				return
			}
		} else {
			s.errors.Add(1)
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			// keep trying briefly during shutdown so the tail is delivered
			if attempt >= 3 {
				s.drops.Add(int64(len(batch)))
				return
			}
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
	s.drops.Add(int64(len(batch)))
}
