package intake

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/prof"
)

// The agent's continuous profiler uploads through the trace-agent, and the
// trace-agent's profiling proxy is pointed at the intake
// (apm_config.profiling_dd_url). The intake tees every upload: a copy is kept
// in memory for `aoc run` to collect into the results directory, and the
// request is forwarded unchanged (same headers, same body, the API key the
// trace-agent added) to the real profile intake, so the profiles land in
// Datadog exactly as they would have.

const maxProfileUpload = 64 << 20

// ProfileFile is one attachment of an upload.
type ProfileFile struct {
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
}

// ProfileUpload is one profiler upload (one period of one service).
type ProfileUpload struct {
	ID        int           `json:"id"`
	At        time.Time     `json:"at"`
	Service   string        `json:"service"`
	Family    string        `json:"family,omitempty"`
	Start     time.Time     `json:"start"`
	End       time.Time     `json:"end"`
	Tags      []string      `json:"tags,omitempty"`
	Files     []ProfileFile `json:"files"`
	Forwarded int           `json:"forwarded_status,omitempty"` // HTTP status from the sink, 0 = not forwarded
	data      map[string][]byte
}

// ProfileStatus is the tee's health, for /harness/status.
type ProfileStatus struct {
	Forward       string    `json:"forward,omitempty"`
	Received      int64     `json:"received"`
	Forwarded     int64     `json:"forwarded"`
	ForwardErrors int64     `json:"forward_errors"`
	LastError     string    `json:"last_error,omitempty"`
	LastAt        time.Time `json:"last_at,omitempty"`
	Held          int       `json:"held"`
	HeldBytes     int64     `json:"held_bytes"`
}

type profileStore struct {
	mu       sync.Mutex
	next     int
	uploads  []*ProfileUpload
	bytes    int64
	maxBytes int64
	forward  string
	apiKey   string
	client   *http.Client
	st       ProfileStatus
	logf     func(string, ...any)
}

func newProfileStore(forward, apiKey string, maxBytes int64, logf func(string, ...any)) *profileStore {
	if maxBytes <= 0 {
		maxBytes = 256 << 20
	}
	return &profileStore{
		forward: forward, apiKey: apiKey, maxBytes: maxBytes, logf: logf,
		client: &http.Client{Timeout: 60 * time.Second},
		st:     ProfileStatus{Forward: forward},
	}
}

func (p *profileStore) status() ProfileStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.st
	st.Held, st.HeldBytes = len(p.uploads), p.bytes
	return st
}

// reset drops the held uploads (a new measured window starts).
func (p *profileStore) reset() {
	p.mu.Lock()
	p.uploads, p.bytes = nil, 0
	p.mu.Unlock()
}

// list returns the held uploads' metadata, oldest first.
func (p *profileStore) list() []ProfileUpload {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ProfileUpload, 0, len(p.uploads))
	for _, u := range p.uploads {
		out = append(out, *u)
	}
	return out
}

func (p *profileStore) file(id int, name string) ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, u := range p.uploads {
		if u.ID == id {
			b, ok := u.data[name]
			return b, ok
		}
	}
	return nil, false
}

// handleUpload is POST /api/v2/profile (and /profiling/v1/input).
func (s *Server) handleProfileUpload(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxProfileUpload+1))
	if err != nil || len(body) > maxProfileUpload {
		http.Error(w, "profile upload too large", http.StatusRequestEntityTooLarge)
		return
	}
	up, perr := parseProfileUpload(body, r.Header)
	if perr != nil {
		s.logf("[profiles] upload not understood (%v); forwarding anyway", perr)
	}
	ps := s.profiles
	status := 0
	if ps.forward != "" {
		status, err = ps.forwardUpload(r.Context(), ps.forward, body, r.Header)
		ps.mu.Lock()
		if err != nil {
			ps.st.ForwardErrors++
			ps.st.LastError = err.Error()
		} else {
			ps.st.Forwarded++
			if status >= 300 {
				ps.st.ForwardErrors++
				ps.st.LastError = fmt.Sprintf("sink answered HTTP %d", status)
			}
		}
		ps.mu.Unlock()
		if err != nil {
			s.logf("[profiles] forward to %s failed: %v (profile kept locally)", ps.forward, err)
		}
	}
	if up != nil {
		up.Forwarded = status
		ps.add(up)
		s.logf("[profiles] %s profile %s → %s (%d files, %s)%s", up.Service, up.Start.UTC().Format("15:04:05"), up.End.UTC().Format("15:04:05"), len(up.Files), fmtBytes(int64(len(body))), forwardNote(status, ps.forward))
	}
	ps.mu.Lock()
	ps.st.Received++
	ps.st.LastAt = time.Now()
	ps.mu.Unlock()
	if status >= 200 && status < 300 {
		w.WriteHeader(status)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	io.WriteString(w, "{}")
}

func forwardNote(status int, forward string) string {
	switch {
	case forward == "":
		return ""
	case status == 0:
		return ", forward failed"
	default:
		return fmt.Sprintf(", forwarded (HTTP %d)", status)
	}
}

func fmtBytes(n int64) string {
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10) + " B"
	case n < 1e6:
		return fmt.Sprintf("%.1f kB", float64(n)/1e3)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	}
}

// parseProfileUpload picks the upload apart: event.json for the period and
// the pprof attachments.
func parseProfileUpload(body []byte, h http.Header) (*ProfileUpload, error) {
	if strings.EqualFold(h.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if body, err = io.ReadAll(io.LimitReader(zr, maxProfileUpload)); err != nil {
			return nil, err
		}
	}
	mt, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(mt, "multipart/") {
		return nil, fmt.Errorf("content type %s", mt)
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	up := &ProfileUpload{At: time.Now(), data: map[string][]byte{}}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(part, maxProfileUpload))
		if err != nil {
			return nil, err
		}
		name := part.FileName()
		if name == "" {
			name = part.FormName()
		}
		if name == "event" || name == "event.json" {
			name = "event.json"
			ev, err := prof.ParseEvent(data)
			if err == nil {
				up.Service, up.Family = ev.Service(), ev.Family
				up.Start, up.End = ev.Times()
				if ev.Tags != "" {
					up.Tags = strings.Split(ev.Tags, ",")
				}
			}
		}
		up.data[name] = data
		up.Files = append(up.Files, ProfileFile{Name: name, Bytes: len(data)})
	}
	if len(up.Files) == 0 {
		return nil, fmt.Errorf("no parts")
	}
	if up.Service == "" {
		up.Service = "unknown"
	}
	sort.Slice(up.Files, func(i, j int) bool { return up.Files[i].Name < up.Files[j].Name })
	return up, nil
}

// forwardUpload replays the request against the real intake.
func (p *profileStore) forwardUpload(ctx context.Context, target string, body []byte, h http.Header) (int, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", target, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	for k, vs := range h {
		switch strings.ToLower(k) {
		case "host", "connection", "content-length", "transfer-encoding", "keep-alive", "te", "trailer", "upgrade", "proxy-connection":
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("DD-API-KEY") == "" && p.apiKey != "" {
		req.Header.Set("DD-API-KEY", p.apiKey)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

// add keeps an upload, replacing a re-sent one (same service and period)
// and evicting the oldest when over the memory cap.
func (p *profileStore) add(up *ProfileUpload) {
	size := int64(0)
	for _, f := range up.Files {
		size += int64(f.Bytes)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, old := range p.uploads {
		if old.Service == up.Service && old.Start.Equal(up.Start) && old.End.Equal(up.End) {
			p.bytes -= uploadSize(old)
			p.uploads = append(p.uploads[:i], p.uploads[i+1:]...)
			break
		}
	}
	p.next++
	up.ID = p.next
	p.uploads = append(p.uploads, up)
	p.bytes += size
	for p.bytes > p.maxBytes && len(p.uploads) > 1 {
		p.bytes -= uploadSize(p.uploads[0])
		p.uploads = p.uploads[1:]
	}
}

func uploadSize(u *ProfileUpload) int64 {
	n := int64(0)
	for _, f := range u.Files {
		n += int64(f.Bytes)
	}
	return n
}

// handleProfilesList is GET /harness/profiles.
func (s *Server) handleProfilesList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"status": s.profiles.status(), "uploads": s.profiles.list()})
}

// handleProfileFile is GET /harness/profiles/{id}/{file}.
func (s *Server) handleProfileFile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	b, ok := s.profiles.file(id, r.PathValue("file"))
	if !ok {
		http.Error(w, "no such profile file", 404)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Write(b)
}

// profileForwardURL resolves --profile-forward: "auto" is the site's
// profile intake when Datadog submission is on, "off"/"" keeps profiles
// local only.
func profileForwardURL(flag string, dd DDConfig) string {
	switch strings.ToLower(flag) {
	case "", "off", "none", "false":
		return ""
	case "auto":
		if !dd.Enabled {
			return ""
		}
		site := dd.Site
		if site == "" {
			site = "datadoghq.com"
		}
		return "https://intake.profile." + site + "/api/v2/profile"
	}
	return flag
}

// marshal helper kept local so tests can compare list output.
func (u ProfileUpload) String() string {
	b, _ := json.Marshal(u)
	return string(b)
}
