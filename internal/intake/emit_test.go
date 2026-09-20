package intake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/gzip"

	"github.com/UTXOnly/agent_of_chaos/internal/ddapi"
	"github.com/UTXOnly/agent_of_chaos/internal/wire"
)

func TestLabelsToTags(t *testing.T) {
	got := labelsToTags(`{status_code="202",url="http://localhost:8282/api/v2/logs",empty=""}`)
	want := []string{"status_code:202", "url:http://localhost:8282/api/v2/logs"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %v", got)
	}
	if labelsToTags("") != nil {
		t.Error("empty labels")
	}
}

func TestEmitterBuildsSeries(t *testing.T) {
	var mu sync.Mutex
	var received []ddapi.Series
	var events []ddapi.Event
	dd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/validate":
			w.Write([]byte(`{"valid":true}`))
		case "/api/v2/series":
			zr, _ := gzip.NewReader(r.Body)
			b, _ := io.ReadAll(zr)
			var body struct {
				Series []ddapi.Series `json:"series"`
			}
			json.Unmarshal(b, &body)
			mu.Lock()
			received = append(received, body.Series...)
			mu.Unlock()
			w.WriteHeader(202)
		case "/api/v1/events":
			b, _ := io.ReadAll(r.Body)
			var ev ddapi.Event
			json.Unmarshal(b, &ev)
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
			w.WriteHeader(202)
		default:
			w.WriteHeader(404)
		}
	}))
	defer dd.Close()

	cfg := DefaultConfig()
	cfg.Name = "unit"
	cfg.AgentImage = "datadog/agent:7"
	cfg.Datadog = DDConfig{Enabled: true, APIKey: "k", Interval: time.Hour, Tags: []string{"variant:test"}, Events: true}
	s, ts := newTestServer(t, cfg)
	s.emitter.client.BaseURL = dd.URL
	go s.emitter.eventLoop(t.Context())

	// Some traffic: a generator report and a payload with 3 logs (one dup).
	rep := wire.GenReport{Gen: "g1", StartedAt: time.Now(), TS: time.Now(), ActiveStreams: 2, Streams: []wire.GenStream{{Name: "svc", Seq: 4, Active: true}}}
	rep.Totals.Records = 4
	rb, _ := json.Marshal(rep)
	post(t, ts.URL+wire.GenReportPath, rb, map[string]string{"Content-Type": "application/json"})
	now := time.Now().UTC().Add(-100 * time.Millisecond).Format("2006-01-02T15:04:05.000000Z")
	logs := []map[string]any{
		{"message": now + " INFO [aoc.svc] aoc=g1/svc/1 a", "ddtags": "a:1,b:2", "service": "svc"},
		{"message": now + " INFO [aoc.svc] aoc=g1/svc/2 b", "ddtags": "a:1,b:2", "service": "svc"},
		{"message": now + " INFO [aoc.svc] aoc=g1/svc/2 b", "ddtags": "a:1,b:2", "service": "svc"},
	}
	body, _ := json.Marshal(logs)
	post(t, ts.URL+"/api/v2/logs", body, map[string]string{"DD-EVP-ORIGIN-VERSION": "7.99.0"})

	e := s.emitter
	e.prev, e.prevAt = emitCounters{byStatus: map[int]int64{}}, time.Now().Add(-10*time.Second)
	e.flush(t.Context())

	mu.Lock()
	defer mu.Unlock()
	byName := map[string]ddapi.Series{}
	for _, sr := range received {
		byName[sr.Metric+"|"+strings.Join(sr.Tags, ",")] = sr
	}
	get := func(metric string, tagSubstr string) (float64, bool) {
		for k, sr := range byName {
			if strings.HasPrefix(k, metric+"|") && strings.Contains(k, tagSubstr) {
				return sr.Points[0].Value, true
			}
		}
		return 0, false
	}
	checks := map[string]float64{
		"aoc.intake.delivery.unique":     2,
		"aoc.intake.delivery.duplicates": 1,
		"aoc.intake.delivery.missing":    2, // seqs 3,4 outstanding
		"aoc.intake.delivery.generated":  4,
		"aoc.intake.logs_per_sec":        0.3,
		"aoc.intake.tags.per_log":        2,
		"aoc.gen.streams":                2,
		"aoc.intake.faults.active":       0,
	}
	for m, want := range checks {
		got, ok := get(m, "run:unit")
		if !ok || (got-want) > 1e-6 && (got-want) > want*0.01 || (want-got) > want*0.01+1e-6 {
			t.Errorf("%s = %v (ok=%v), want %v", m, got, ok, want)
		}
	}
	if v, ok := get("aoc.intake.latency.e2e.p99", "run:unit"); !ok || v <= 0 || v > 5 {
		t.Errorf("e2e p99 = %v ok=%v", v, ok)
	}
	// Tags: run, variant, agent_image and the agent version seen in headers.
	sr := byName["aoc.intake.delivery.unique|run:unit,variant:test,agent_image:datadog/agent:7,agent_version:7.99.0"]
	if sr.Metric == "" {
		var keys []string
		for k := range byName {
			if strings.HasPrefix(k, "aoc.intake.delivery.unique") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		t.Errorf("expected tagged series, have %v", keys)
	}
	// Events: generator registered.
	deadline := time.Now().Add(2 * time.Second)
	for len(events) == 0 && time.Now().Before(deadline) {
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
	}
	found := false
	for _, ev := range events {
		if strings.Contains(ev.Title, "generator g1 started") && strings.Contains(strings.Join(ev.Tags, ","), "run:unit") {
			found = true
		}
	}
	if !found {
		t.Errorf("no generator event posted: %v", events)
	}
	_ = fmt.Sprint
}

func TestTelemetryForwarding(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Name = "u"
	cfg.Datadog = DDConfig{Enabled: true, APIKey: "k"}
	cfg.AgentTelemetryURL = "http://127.0.0.1:1/telemetry" // never scraped in this test
	s, _ := newTestServer(t, cfg)
	now := time.Now()
	series := []MetricSeries{
		{Name: "logs__bytes_sent", Labels: `{source="logs"}`, Type: "counter", Value: 1000, LastAt: now.Add(-10 * time.Second)},
		{Name: "logs_component_utilization__ratio", Labels: `{name="processor"}`, Type: "gauge", Value: 0.5, LastAt: now},
		{Name: "logs__sender_latency_bucket", Labels: `{le="1"}`, Type: "histogram", Value: 5, LastAt: now},
		{Name: "logs__sender_latency_sum", Labels: "", Type: "histogram", Value: 12, LastAt: now.Add(-10 * time.Second)},
	}
	b := &seriesBuilder{prefix: "aoc", ts: now.Unix()}
	s.emitter.telemetrySeries(b, series, now)
	names := map[string]bool{}
	for _, sr := range b.out {
		names[sr.Metric] = true
	}
	if !names["aoc.agent.telemetry.logs_component_utilization.ratio"] || names["aoc.agent.telemetry.logs.sender_latency_bucket"] || names["aoc.agent.telemetry.logs.bytes_sent.rate"] {
		t.Errorf("first pass names: %v", names)
	}
	// Second scrape: counters now have a previous sample → rates.
	series[0].Value, series[0].LastAt = 2000, now
	series[3].Value, series[3].LastAt = 22, now
	b = &seriesBuilder{prefix: "aoc", ts: now.Unix()}
	s.emitter.telemetrySeries(b, series, now)
	var rateV float64
	for _, sr := range b.out {
		if sr.Metric == "aoc.agent.telemetry.logs.bytes_sent.rate" {
			rateV = sr.Points[0].Value
			if sr.Tags[0] != "source:logs" {
				t.Errorf("tags: %v", sr.Tags)
			}
		}
	}
	if rateV < 99 || rateV > 101 {
		t.Errorf("bytes_sent rate = %v, want 100/s", rateV)
	}
}
