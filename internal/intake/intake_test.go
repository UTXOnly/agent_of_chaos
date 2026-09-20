package intake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"

	"github.com/UTXOnly/agent_of_chaos/internal/wire"
)

func TestParseTS(t *testing.T) {
	good := map[string]string{
		"2026-09-20T02:02:24.123456Z rest":     "2026-09-20T02:02:24.123456Z",
		"2026-09-20T02:02:24Z":                 "2026-09-20T02:02:24Z",
		"2026-09-20T02:02:24.5+02:00":          "2026-09-20T00:02:24.5Z",
		"2026-09-20T02:02:24.123456789Z":       "2026-09-20T02:02:24.123456789Z",
		"2026-09-20T02:02:24.1234567890123Z x": "2026-09-20T02:02:24.123456789Z",
	}
	for in, want := range good {
		got, ok := parseTS(in)
		if !ok {
			t.Errorf("parseTS(%q) failed", in)
			continue
		}
		w, _ := time.Parse(time.RFC3339Nano, want)
		if !got.Equal(w) {
			t.Errorf("parseTS(%q) = %s; want %s", in, got.Format(time.RFC3339Nano), want)
		}
	}
	for _, bad := range []string{"", "Traceback (most", "2026-09-20 02:02:24Z", "2026-13-20T02:02:24Z", "2026-09-20T02:02:24"} {
		if _, ok := parseTS(bad); ok {
			t.Errorf("parseTS(%q) should fail", bad)
		}
	}
}

func TestPhysicalLineBreaks(t *testing.T) {
	cases := map[string]int{
		"plain":   0,
		"a\nb\nc": 2,
		`agent joined\nwith literal\nbackslash-n`:    2,
		`{"message":"json\nescaped","stack":"x\ny"}`: 0,
	}
	for in, want := range cases {
		if got := physicalLineBreaks(in); got != want {
			t.Errorf("physicalLineBreaks(%q) = %d; want %d", in, got, want)
		}
	}
}

func TestWriteTimestampJSON(t *testing.T) {
	ts, ok := writeTimestamp(`{"timestamp":"2026-09-20T02:02:24.123456Z","level":"INFO","aoc":"g/s/1"}`)
	if !ok || ts.Nanosecond() != 123456000 {
		t.Fatalf("json timestamp: %v %v", ts, ok)
	}
}

func TestHistQuantiles(t *testing.T) {
	h := newLatencyHist()
	for i := 1; i <= 1000; i++ {
		h.add(float64(i) / 1000) // 1ms..1s uniform
	}
	q := h.summary()
	check := func(name string, got, want float64) {
		if math.Abs(got-want)/want > 0.06 {
			t.Errorf("%s = %.4f; want ≈ %.4f", name, got, want)
		}
	}
	check("p50", q.P50, 0.5)
	check("p90", q.P90, 0.9)
	check("p99", q.P99, 0.99)
	if q.Max != 1 || q.Count != 1000 || math.Abs(q.Mean-0.5005) > 1e-9 {
		t.Errorf("max=%v count=%v mean=%v", q.Max, q.Count, q.Mean)
	}
}

func TestTracker(t *testing.T) {
	tr := newTracker()
	now := time.Now()
	var obs []seqObs
	for _, seq := range []int64{1, 2, 3, 5, 6, 6, 4, 10} {
		obs = append(obs, seqObs{gen: "g", stream: "s", seq: seq, bytes: 10})
	}
	tr.apply(obs, now)
	snap := tr.snapshot()
	if len(snap) != 1 {
		t.Fatalf("streams: %d", len(snap))
	}
	s := snap[0]
	if s.Received != 8 || s.Unique != 7 || s.Dups != 1 || s.MaxSeq != 10 || s.OutOfOrder != 1 {
		t.Errorf("ledger: %+v", s)
	}
	tr.withStream("g", "s", func(st *streamTrack) {
		if m := st.missingUpTo(10); m != 3 { // 7, 8, 9
			t.Errorf("missingUpTo(10) = %d; want 3", m)
		}
		if m := st.missingUpTo(12); m != 5 {
			t.Errorf("missingUpTo(12) = %d; want 5", m)
		}
		gaps := st.gaps(12, 10)
		want := []SeqRange{{7, 9}, {11, 12}}
		if fmt.Sprint(gaps) != fmt.Sprint(want) {
			t.Errorf("gaps = %v; want %v", gaps, want)
		}
	})

	// Re-base: seqs at or below the base are pre-window.
	tr.reset(map[string]map[string]int64{"g": {"s": 100}})
	tr.apply([]seqObs{{gen: "g", stream: "s", seq: 100}, {gen: "g", stream: "s", seq: 101}, {gen: "g", stream: "s", seq: 103}}, now)
	s = tr.snapshot()[0]
	if s.PreWindow != 1 || s.Unique != 2 || s.MaxSeq != 3 {
		t.Errorf("after reset: %+v", s)
	}
}

func TestTrackerLongGaps(t *testing.T) {
	tr := newTracker()
	var obs []seqObs
	for seq := int64(1); seq <= 100000; seq++ {
		if seq%1000 == 0 {
			continue // one hole every 1000
		}
		obs = append(obs, seqObs{gen: "g", stream: "s", seq: seq})
	}
	tr.apply(obs, time.Now())
	tr.withStream("g", "s", func(st *streamTrack) {
		if m := st.missingUpTo(100000); m != 100 {
			t.Errorf("missing = %d; want 100", m)
		}
		g := st.gaps(100000, 5)
		if len(g) != 5 || g[0].From != 1000 || g[0].To != 1000 || g[4].From != 5000 {
			t.Errorf("gaps = %v", g)
		}
	})
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func zs(b []byte) []byte {
	enc, _ := zstd.NewWriter(nil)
	return enc.EncodeAll(b, nil)
}

func TestDecompress(t *testing.T) {
	payload := []byte(`[{"message":"hi"}]`)
	for enc, body := range map[string][]byte{"gzip": gz(payload), "zstd": zs(payload), "identity": payload, "": payload} {
		out, err := decompress(body, enc, 1<<20, false)
		if err != nil || !bytes.Equal(out, payload) {
			t.Errorf("%s: %v %q", enc, err, out)
		}
	}
	// The agent's connectivity probe: "{}" with a gzip header.
	if out, err := decompress([]byte("{}"), "gzip", 1<<20, true); err != nil || string(out) != "{}" {
		t.Errorf("lenient probe: %v %q", err, out)
	}
	if _, err := decompress([]byte("{}"), "gzip", 1<<20, false); err == nil {
		t.Error("strict mode should reject a non-gzip body")
	}
	big := bytes.Repeat([]byte("x"), 2000)
	if _, err := decompress(gz(big), "gzip", 1000, true); err != errTooLarge {
		t.Errorf("limit: %v", err)
	}
}

func TestParsePayload(t *testing.T) {
	if _, k := parsePayload([]byte("{}")); k != kindProbe {
		t.Error("{} should be a probe")
	}
	if _, k := parsePayload([]byte("")); k != kindProbe {
		t.Error("empty should be a probe")
	}
	if _, k := parsePayload([]byte("nope")); k != kindMalformed {
		t.Error("garbage should be malformed")
	}
	e, k := parsePayload([]byte(`[{"message":"a","ddtags":"x:1"},{"message":"b"}]`))
	if k != kindLogs || len(e) != 2 || e[0].Tags != "x:1" {
		t.Errorf("array: %v %v", k, e)
	}
}

func newTestServer(t *testing.T, cfg Config) (*Server, *httptest.Server) {
	t.Helper()
	if cfg.Addr == "" {
		cfg.Addr = ":0"
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Quiet = true
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)
	return s, ts
}

func post(t *testing.T, url string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func TestLogsEndpointAccounting(t *testing.T) {
	s, ts := newTestServer(t, DefaultConfig())
	// Generator report so "generated" is known.
	rep := wire.GenReport{Gen: "g1", StartedAt: time.Now(), TS: time.Now(), Streams: []wire.GenStream{{Name: "svc", Seq: 5, Active: true}}}
	rep.Totals.Records = 5
	rb, _ := json.Marshal(rep)
	if r := post(t, ts.URL+wire.GenReportPath, rb, map[string]string{"Content-Type": "application/json"}); r.StatusCode != 204 {
		t.Fatalf("gen report: %d", r.StatusCode)
	}
	now := time.Now().UTC()
	line := func(seq int, extra string) map[string]any {
		return map[string]any{
			"message":   fmt.Sprintf("%s INFO     [aoc.svc] aoc=g1/svc/%d hello%s", now.Add(-200*time.Millisecond).Format("2006-01-02T15:04:05.000000Z"), seq, extra),
			"status":    "info",
			"timestamp": now.Add(-50 * time.Millisecond).UnixMilli(),
			"hostname":  "h",
			"service":   "svc",
			"ddsource":  "agent-of-chaos",
			"ddtags":    "env:test,filename:svc.log,dirname:/var/log",
		}
	}
	logs := []map[string]any{line(1, ""), line(2, "\nTraceback (most recent call last):\n  File x"), line(2, ""), line(4, ""), {"message": "  at com.foo.Bar(Bar.java:1)", "service": "svc"}}
	body, _ := json.Marshal(logs)
	r := post(t, ts.URL+"/api/v2/logs", gz(body), map[string]string{"Content-Type": "application/json", "Content-Encoding": "gzip", "DD-API-KEY": "abcdefghijkl", "DD-EVP-ORIGIN": "agent", "DD-EVP-ORIGIN-VERSION": "7.99.0"})
	if r.StatusCode != 202 {
		t.Fatalf("logs: %d", r.StatusCode)
	}
	// probe
	if r := post(t, ts.URL+"/api/v2/logs", []byte("{}"), map[string]string{"Content-Encoding": "gzip"}); r.StatusCode != 202 {
		t.Fatalf("probe: %d", r.StatusCode)
	}
	rep2 := s.buildReport(true)
	d := rep2.Delivery
	if d.ReceivedLogs != 5 || d.ReceivedMarked != 4 || d.Unique != 3 || d.Duplicates != 1 || d.Orphans != 1 || d.Multiline != 1 {
		t.Errorf("delivery: %+v", d)
	}
	if d.GeneratedRecords != 5 || d.Missing != 2 {
		t.Errorf("generated=%d missing=%d", d.GeneratedRecords, d.Missing)
	}
	if len(rep2.Streams) != 1 || fmt.Sprint(rep2.Streams[0].Gaps) != "[{3 3} {5 5}]" {
		t.Errorf("streams: %+v", rep2.Streams)
	}
	if rep2.HTTP.Probes != 1 || rep2.HTTP.Requests != 2 || rep2.HTTP.ByStatus["202"] != 2 {
		t.Errorf("http: %+v", rep2.HTTP)
	}
	if rep2.Agent.Versions["7.99.0"] != 1 || rep2.Agent.Encodings["gzip"] != 2 || rep2.Agent.APIKeys["abcd…ijkl"] != 1 {
		t.Errorf("agent: %+v", rep2.Agent)
	}
	if rep2.Tags.UniqueKeys != 3 || rep2.Tags.TotalTags != 12 {
		t.Errorf("tags: %+v", rep2.Tags)
	}
	l := rep2.Latency
	if l.EndToEnd.Count != 4 || l.EndToEnd.P50 < 0.15 || l.EndToEnd.P50 > 0.4 || l.Sender.Count != 4 {
		t.Errorf("latency: %+v", l)
	}
}

func TestFaultsAndRejections(t *testing.T) {
	cfg := DefaultConfig()
	cfg.APIKey = "secret"
	cfg.MaxPayloadBytes = 1024
	s, ts := newTestServer(t, cfg)
	body, _ := json.Marshal([]map[string]any{{"message": "aoc=g/s/1 x"}})
	if r := post(t, ts.URL+"/api/v2/logs", body, map[string]string{"DD-API-KEY": "wrong"}); r.StatusCode != 403 {
		t.Errorf("bad key: %d", r.StatusCode)
	}
	big, _ := json.Marshal([]map[string]any{{"message": strings.Repeat("x", 2000)}})
	if r := post(t, ts.URL+"/api/v2/logs", gz(big), map[string]string{"DD-API-KEY": "secret", "Content-Encoding": "gzip"}); r.StatusCode != 413 {
		t.Errorf("too large: %d", r.StatusCode)
	}
	s.setFaults(Faults{ErrorRate: 1, ErrorStatus: 503}, "test")
	if r := post(t, ts.URL+"/api/v2/logs", body, map[string]string{"DD-API-KEY": "secret"}); r.StatusCode != 503 {
		t.Errorf("injected: %d", r.StatusCode)
	}
	s.setFaults(Faults{}, "clear")
	if r := post(t, ts.URL+"/api/v2/logs", body, map[string]string{"DD-API-KEY": "secret"}); r.StatusCode != 202 {
		t.Errorf("after clear: %d", r.StatusCode)
	}
	rep := s.buildReport(false)
	if rep.Delivery.Unique != 1 || rep.HTTP.FaultErrored != 1 || rep.HTTP.Rejected["403"] != 1 || rep.HTTP.Rejected["413"] != 1 {
		t.Errorf("report: delivery=%+v http=%+v", rep.Delivery, rep.HTTP)
	}
	// Faults API round trip.
	fb, _ := json.Marshal(Faults{LatencyMs: 5, Note: "n"})
	if r := post(t, ts.URL+"/harness/faults", fb, map[string]string{"Content-Type": "application/json"}); r.StatusCode != 200 {
		t.Errorf("set faults: %d", r.StatusCode)
	}
	if f := s.faults.get(); f.LatencyMs != 5 || f.Note != "n" {
		t.Errorf("faults not applied: %+v", f)
	}
	bad, _ := json.Marshal(Faults{ErrorRate: 2})
	if r := post(t, ts.URL+"/harness/faults", bad, map[string]string{"Content-Type": "application/json"}); r.StatusCode != 400 {
		t.Errorf("invalid faults accepted: %d", r.StatusCode)
	}
}

func TestSinkAndValidate(t *testing.T) {
	s, ts := newTestServer(t, DefaultConfig())
	if r := post(t, ts.URL+"/api/v2/series", []byte("whatever"), nil); r.StatusCode != 202 {
		t.Errorf("sink: %d", r.StatusCode)
	}
	resp, err := http.Get(ts.URL + "/api/v1/validate?api_key=x")
	if err != nil || resp.StatusCode != 200 {
		t.Errorf("validate: %v %v", err, resp)
	}
	resp.Body.Close()
	resp, _ = http.Get(ts.URL + "/metrics")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "aoc_intake_logs_total 0") {
		t.Errorf("metrics: %s", b)
	}
	resp, _ = http.Get(ts.URL + "/harness/status")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status: %d", resp.StatusCode)
	}
	if rep := s.buildReport(false); rep.HTTP.OtherRequests["/api/v2/series"] != 1 {
		t.Errorf("other requests: %+v", rep.HTTP.OtherRequests)
	}
}

func TestTCPFrame(t *testing.T) {
	e := parseTCPFrame(`apikey123 <46>0 2026-09-20T02:02:24.123456789Z myhost auth-service - - [dd ddsource="agent-of-chaos" ddsourcecategory="" ddtags="a:b,c:d"] 2026-09-20T02:02:24.123456Z INFO [aoc.auth-service] aoc=g/auth-service/9 hi`)
	if e.Hostname != "myhost" || e.Service != "auth-service" || e.Source != "agent-of-chaos" || e.Tags != "a:b,c:d" || !strings.HasSuffix(e.Message, "aoc=g/auth-service/9 hi") || e.Timestamp == 0 {
		t.Errorf("tcp frame: %+v", e)
	}
}

func TestParseSample(t *testing.T) {
	n, l, v, ok := parseSample(`logs__bytes_sent{destination="x"} 1234.5`)
	if !ok || n != "logs__bytes_sent" || l != `{destination="x"}` || v != 1234.5 {
		t.Errorf("%s %s %v %v", n, l, v, ok)
	}
	n, l, v, ok = parseSample(`go_goroutines 42`)
	if !ok || n != "go_goroutines" || l != "" || v != 42 {
		t.Errorf("%s %s %v %v", n, l, v, ok)
	}
}

func TestProfileTee(t *testing.T) {
	var got []string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("DD-API-KEY")+" "+r.Header.Get("Content-Type"))
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "cpu.pprof") {
			t.Errorf("forwarded body lost the attachment")
		}
		w.WriteHeader(202)
	}))
	defer sink.Close()
	cfg := DefaultConfig()
	cfg.ProfileForward = sink.URL + "/api/v2/profile"
	s, ts := newTestServer(t, cfg)
	defer ts.Close()

	build := func(start string) ([]byte, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		ev, _ := mw.CreateFormFile("event", "event.json")
		ev.Write([]byte(`{"start":"` + start + `","end":"2026-09-20T12:01:00Z","family":"go","tags_profiler":"service:datadog-agent,run:x","attachments":["cpu.pprof"]}`))
		f, _ := mw.CreateFormFile("cpu.pprof", "cpu.pprof")
		f.Write([]byte("fake-pprof-bytes"))
		mw.Close()
		return buf.Bytes(), mw.FormDataContentType()
	}
	body, ct := build("2026-09-20T12:00:00Z")
	resp := post(t, ts.URL+"/api/v2/profile", body, map[string]string{"Content-Type": ct, "DD-API-KEY": "k"})
	if resp.StatusCode != 202 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	// A re-sent period replaces, not duplicates.
	post(t, ts.URL+"/api/v2/profile", body, map[string]string{"Content-Type": ct, "DD-API-KEY": "k"})
	body2, ct2 := build("2026-09-20T12:01:00Z")
	post(t, ts.URL+"/api/v2/profile", body2, map[string]string{"Content-Type": ct2, "DD-API-KEY": "k"})

	st := s.profiles.status()
	if st.Received != 3 || st.Forwarded != 3 || st.ForwardErrors != 0 || st.Held != 2 {
		t.Fatalf("status %+v", st)
	}
	if len(got) != 3 || !strings.HasPrefix(got[0], "k multipart/form-data") {
		t.Errorf("sink saw %v", got)
	}
	r, _ := http.Get(ts.URL + "/harness/profiles")
	var list struct {
		Uploads []ProfileUpload `json:"uploads"`
	}
	json.NewDecoder(r.Body).Decode(&list)
	r.Body.Close()
	if len(list.Uploads) != 2 || list.Uploads[0].Service != "datadog-agent" || list.Uploads[0].Start.Format(time.RFC3339) != "2026-09-20T12:00:00Z" || len(list.Uploads[0].Files) != 2 {
		t.Fatalf("list: %+v", list.Uploads)
	}
	fr, _ := http.Get(fmt.Sprintf("%s/harness/profiles/%d/cpu.pprof", ts.URL, list.Uploads[0].ID))
	fb, _ := io.ReadAll(fr.Body)
	fr.Body.Close()
	if string(fb) != "fake-pprof-bytes" {
		t.Errorf("file: %q", fb)
	}
	// Reset drops the held uploads.
	post(t, ts.URL+"/harness/reset", nil, nil)
	if st := s.profiles.status(); st.Held != 0 {
		t.Errorf("after reset held=%d", st.Held)
	}
}

func TestParseTop(t *testing.T) {
	procs, err := parseTop([]string{"PID", "RSS", "TIME", "COMMAND"}, [][]string{
		{"1", "1000", "12", "/opt/datadog-agent/bin/agent/agent run -p /opt/datadog-agent/run/agent.pid"},
		{"2", "500", "3", "/opt/datadog-agent/embedded/bin/trace-agent --config=/etc/datadog-agent/datadog.yaml"},
		{"3", "200", "1", "agent"},
		{"4", "300", "0", "/opt/datadog-agent/embedded/bin/system-probe-lite-with-a-long-name run"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if procs["agent"].RSS != 1200*1024 || procs["agent"].CPUTime != 13 || procs["trace-agent"].RSS != 500*1024 || procs["system-probe-lite-with-a-long-name"].RSS != 300*1024 {
		t.Errorf("%+v", procs)
	}
	if _, err := parseTop([]string{"PID", "COMMAND"}, nil); err == nil {
		t.Error("missing columns should error")
	}
}
