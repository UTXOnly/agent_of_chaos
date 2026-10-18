package cli

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const sampleProfile = `
name: sample
duration: 3m
warmup: 20s
generators:
  plain: { streams: 8, rate: 5000 }
  json:  { format: json, streams: 8, rate: 5000, rotate-bytes: 256KiB }
  app:
    output: stdout
    replicas: 3
    streams: 1
    rate: 500
    labels: { team: payments, app: "app-{n}" }
faults:
  - at: 60s
    set: { error_rate: 0.5, error_status: 503 }
  - at: 90s
    clear: true
`

func TestParseProfile(t *testing.T) {
	p, err := parseProfile([]byte(sampleProfile), "x")
	if err != nil {
		t.Fatal(err)
	}
	if p.Drain.String() != "45s" {
		t.Errorf("default drain: %s", p.Drain)
	}
	plain := p.Generators["plain"]
	if plain.Format != "plain" || plain.Output != "file" || plain.Replicas != 1 || plain.Settings["streams"] != 8 {
		t.Errorf("plain defaults: %+v", plain)
	}
	app := p.Generators["app"]
	if app.Output != "stdout" || app.Replicas != 3 || app.Labels["app"] != "app-{n}" {
		t.Errorf("app: %+v", app)
	}
	if got := p.serviceNames(); !reflect.DeepEqual(got, []string{"gen-app-01", "gen-app-02", "gen-app-03", "gen-json", "gen-plain"}) {
		t.Errorf("services: %v", got)
	}
	if s := p.summary(); s != "app ×3: 1 streams, 500 lines/s, plain on stdout; json: 8 streams, 5000 lines/s, json files; plain: 8 streams, 5000 lines/s, plain files" {
		t.Errorf("summary: %s", s)
	}
	if s := p.faultsSummary(); s != "2 steps: HTTP 503 50% at 1m0s, cleared at 1m30s" {
		t.Errorf("faults: %s", s)
	}
}

func TestParseProfileErrors(t *testing.T) {
	cases := map[string]string{
		"no duration":     "generators: { plain: { streams: 1 } }\n",
		"no generators":   "duration: 1m\n",
		"old enabled key": "duration: 1m\ngenerators: { plain: { enabled: false } }\n",
		"typo":            "duration: 1m\ngenerators: { plain: { strems: 4 } }\n",
		"reserved":        "duration: 1m\ngenerators: { plain: { log-dir: /tmp } }\n",
		"bad format":      "duration: 1m\ngenerators: { plain: { format: xml } }\n",
		"bad replicas":    "duration: 1m\ngenerators: { plain: { replicas: 0 } }\n",
		"unknown top key": "duration: 1m\ngenerators: { plain: {} }\nnotes: [x]\n",
		"bad fault":       "duration: 1m\ngenerators: { plain: {} }\nfaults: [{ at: 1s, set: { drop_rate: 2 } }]\n",
		"unknown fault":   "duration: 1m\ngenerators: { plain: {} }\nfaults: [{ at: 1s, set: { latency: 2 } }]\n",
		"name with a dot": "duration: 1m\ngenerators: { a.b: {} }\n",
	}
	for name, src := range cases {
		if _, err := parseProfile([]byte(src), "x"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestProfileRoundTrip(t *testing.T) {
	p, err := parseProfile([]byte(sampleProfile), "x")
	if err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	back, err := parseProfile(out, "x")
	if err != nil {
		t.Fatalf("resolved profile does not load: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(back, p) {
		t.Errorf("round trip changed the profile:\n%s", out)
	}
	if !strings.Contains(string(out), "plain: {format: plain, output: file, streams: 8, rate: 5000}") {
		t.Errorf("generator rendering:\n%s", out)
	}
}

func TestProfileSet(t *testing.T) {
	p, err := parseProfile([]byte(sampleProfile), "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.set([]string{"rate=20000", "json.streams=16", "app.replicas=2", "plain.rotate-bytes=1MiB", "json.deterministic=true"}); err != nil {
		t.Fatal(err)
	}
	if p.Generators["plain"].Settings["rate"] != 20000 || p.Generators["json"].Settings["rate"] != 20000 || p.Generators["app"].Settings["rate"] != 20000 {
		t.Errorf("rate for every generator: %+v", p.Generators)
	}
	if p.Generators["json"].Settings["streams"] != 16 || p.Generators["plain"].Settings["streams"] != 8 {
		t.Errorf("json.streams: %+v", p.Generators)
	}
	if p.Generators["app"].Replicas != 2 || p.Generators["plain"].Settings["rotate-bytes"] != "1MiB" || p.Generators["json"].Settings["deterministic"] != true {
		t.Errorf("typed values: %+v", p.Generators)
	}
	for _, bad := range []string{"nosuch=1", "other.rate=1", "plain.log-dir=/x", "app.replicas=0", "rate"} {
		if err := p.set([]string{bad}); err == nil {
			t.Errorf("set(%q) accepted", bad)
		}
	}
}

func TestOverrideCompose(t *testing.T) {
	p, err := parseProfile([]byte(sampleProfile), "x")
	if err != nil {
		t.Fatal(err)
	}
	out, names := overrideCompose(p, agentSpec{Image: "datadog/agent:7", Pull: "always", Env: map[string]string{"DD_LOG_LEVEL": "debug"}}, "aoc")
	if len(names) != 5 {
		t.Errorf("services: %v", names)
	}
	var doc struct {
		Services map[string]composeService `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("override is not YAML: %v\n%s", err, out)
	}
	plain := doc.Services["gen-plain"]
	if plain.ContainerName != "aoc-gen-plain" || plain.Restart != "on-failure" || plain.Environment["AOC_LOG_DIR"] != "/var/log/agent-of-chaos/plain" || plain.Environment["AOC_RATE"] != "5000" || len(plain.Volumes) != 1 || plain.Labels != nil {
		t.Errorf("gen-plain: %+v", plain)
	}
	if doc.Services["gen-json"].Environment["AOC_ROTATE_BYTES"] != "256KiB" || doc.Services["gen-json"].Environment["AOC_FORMAT"] != "json" {
		t.Errorf("gen-json: %+v", doc.Services["gen-json"])
	}
	app := doc.Services["gen-app-02"]
	if app.ContainerName != "aoc-gen-app-02" || app.Environment["AOC_NAME"] != "app-02" || app.Environment["AOC_OUTPUT"] != "stdout" || app.Environment["AOC_START_DELAY"] != "20s" || app.Volumes != nil {
		t.Errorf("gen-app-02: %+v", app)
	}
	if app.Labels["app"] != "app-02" || app.Labels["team"] != "payments" || !strings.Contains(app.Labels["com.datadoghq.ad.logs"], `"service":"app-02"`) || !strings.Contains(app.Labels["com.datadoghq.ad.logs"], "multi_line") {
		t.Errorf("gen-app-02 labels: %+v", app.Labels)
	}
	agent := doc.Services["datadog-agent"]
	if agent.Image != "datadog/agent:7" || agent.PullPolicy != "always" || agent.Environment["DD_LOG_LEVEL"] != "debug" {
		t.Errorf("agent: %+v", agent)
	}
}

func TestParseFaults(t *testing.T) {
	got, err := parseFaults([]string{"latency_ms=300", "drop_rate=0.05", "outage=false", "note=slow uplink"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"latency_ms": 300.0, "drop_rate": 0.05, "outage": false, "note": "slow uplink"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseFaults = %v, want %v", got, want)
	}
	if s := describeFaults(got); s != "drop 5%, latency 300ms (slow uplink)" {
		t.Errorf("describeFaults = %q", s)
	}
	for _, bad := range []string{"latency=300", "drop_rate=2", "error_status=200", "latency_ms=-1"} {
		if _, err := parseFaults([]string{bad}); err == nil {
			t.Errorf("parseFaults(%q) accepted", bad)
		}
	}
}

func TestWorkloadSetAndFaults(t *testing.T) {
	cfg := ABConfig{Profile: workloadRef("intake-outage"), Faults: map[string]any{"latency_ms": 200.0}, Set: []string{"rate=100"}}
	cfg.Profile.Path = filepath.Join("..", "..", cfg.Profile.Path)
	p, err := cfg.workload()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Faults) != 1 || p.Faults[0].At != 0 || p.Faults[0].Set["latency_ms"] != 200.0 {
		t.Errorf("faults = %+v, want one step at 0 with latency_ms=200", p.Faults)
	}
	for _, g := range p.Generators {
		if g.Settings["rate"] != 100 {
			t.Errorf("rate not applied: %+v", g)
		}
	}
}

func TestProfilePath(t *testing.T) {
	for in, want := range map[string]string{"baseline": filepath.Join("profiles", "baseline.yaml"), "x/y.yaml": "x/y.yaml", "mine.yml": "mine.yml"} {
		if got := profilePath(in); got != want {
			t.Errorf("profilePath(%q) = %q, want %q", in, got, want)
		}
	}
}
