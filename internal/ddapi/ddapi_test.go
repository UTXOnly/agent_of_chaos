package ddapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/klauspost/compress/gzip"
)

func TestSubmitAndEvent(t *testing.T) {
	var gotSeries int
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header
		switch r.URL.Path {
		case "/api/v2/series":
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("not gzip: %v", err)
			}
			b, _ := io.ReadAll(zr)
			var body struct {
				Series []Series `json:"series"`
			}
			json.Unmarshal(b, &body)
			gotSeries += len(body.Series)
			w.WriteHeader(202)
			w.Write([]byte(`{"errors":[]}`))
		case "/api/v1/validate":
			w.Write([]byte(`{"valid":true}`))
		case "/api/v1/events":
			w.WriteHeader(202)
			w.Write([]byte(`{"status":"ok"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := New("datadoghq.com", "k")
	c.BaseURL = srv.URL
	ctx := context.Background()
	if err := c.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	series := make([]Series, 1500)
	for i := range series {
		series[i] = Series{Metric: "aoc.test", Type: TypeGauge, Points: []Point{{1, 2}}}
	}
	if err := c.SubmitSeries(ctx, series); err != nil {
		t.Fatal(err)
	}
	if gotSeries != 1500 || gotHeaders.Get("DD-API-KEY") != "k" {
		t.Errorf("series=%d headers=%v", gotSeries, gotHeaders)
	}
	if err := c.PostEvent(ctx, Event{Title: "t", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if gotHeaders.Get("DD-APPLICATION-KEY") != "" {
		t.Error("telemetry must not use an application key")
	}
	if AppURL("us3.datadoghq.com") != "https://us3.datadoghq.com" || AppURL("datadoghq.eu") != "https://app.datadoghq.eu" {
		t.Error("AppURL")
	}
	if AppURLFor("datadoghq.com", "bhartford", "") != "https://bhartford.datadoghq.com" || AppURLFor("datadoghq.com", "x", "https://custom.example/") != "https://custom.example" {
		t.Error("AppURLFor")
	}
}
