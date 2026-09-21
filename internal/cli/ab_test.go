package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadABConfigDefaults(t *testing.T) {
	prof := writeTemp(t, "baseline.yaml", "duration: 2m\ngenerators:\n  gen-plain: { streams: 4, rate: 100 }\n")
	cfgPath := writeTemp(t, "aoc.yaml", "profile: "+prof+"\nb:\n  image: datadog/agent-dev:my-branch-py3\n  pull: never\n")
	c, err := loadABConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.A.Image != defaultAImage || c.A.Name != "a" || c.A.Pull != "always" {
		t.Errorf("a defaults: %+v", c.A)
	}
	if c.B.Name != "b" || c.B.Pull != "never" {
		t.Errorf("b: %+v", c.B)
	}
	if c.Runs != 1 || c.Pause.D() != 10*time.Second || c.Results != "results" || c.Threshold != 10 {
		t.Errorf("defaults: runs=%d pause=%s results=%s threshold=%v", c.Runs, c.Pause, c.Results, c.Threshold)
	}
	p, psrc, err := c.workload()
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "baseline" || p.Duration.D() != 2*time.Minute || p.Drain.D() != 45*time.Second || len(psrc) == 0 {
		t.Errorf("workload: %+v", p)
	}
	runs := c.plan("ab-" + p.Name)
	if len(runs) != 2 || runs[0].name != "ab-baseline-a" || runs[1].name != "ab-baseline-b" || runs[1].dir != filepath.Join("results", "ab-baseline", "b") {
		t.Errorf("plan: %+v", runs)
	}
}

func TestLoadABConfigInlineProfileAndOverrides(t *testing.T) {
	cfgPath := writeTemp(t, "aoc.yaml", `
name: tag filters
profile:
  duration: 1m
  warmup: 5s
  agent:
    env: { DD_LOG_LEVEL: debug }
  generators:
    gen-plain: { streams: 2, rate: 10 }
duration: 90s
runs: 2
env: { DD_LOGS_CONFIG_CONTAINER_COLLECT_ALL: "false" }
a: { name: release }
b: { name: candidate, image: datadog/agent-dev:x, env: { DD_LOGS_CONFIG_TAG_FILTERS: '{"exclude":["env:*"]}' } }
`)
	c, err := loadABConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	p, src, err := c.workload()
	if err != nil {
		t.Fatal(err)
	}
	if c.Profile.String() != "inline" || p.Name != "inline" || p.Duration.D() != 90*time.Second || p.Warmup.D() != 5*time.Second {
		t.Errorf("inline profile: %s %+v", c.Profile, p)
	}
	if !strings.Contains(string(src), "gen-plain") {
		t.Errorf("inline source not kept: %s", src)
	}
	if p.Agent.Env["DD_LOG_LEVEL"] != "debug" || p.Agent.Env["DD_LOGS_CONFIG_CONTAINER_COLLECT_ALL"] != "false" {
		t.Errorf("shared env not merged: %v", p.Agent.Env)
	}
	runs := c.plan(sanitize("tag filters"))
	want := []string{"tag-filters-release", "tag-filters-candidate", "tag-filters-release-2", "tag-filters-candidate-2"}
	for i, r := range runs {
		if r.name != want[i] {
			t.Errorf("run %d = %s, want %s", i, r.name, want[i])
		}
	}
	if runs[2].dir != filepath.Join("results", "tag-filters", "release-2") || runs[2].side != "a" || runs[2].round != 2 {
		t.Errorf("round 2: %+v", runs[2])
	}
	// The candidate's env lands on top of the profile's in the compose override.
	override, _ := overrideCompose(p, agentSpec{Image: c.B.Image, Pull: c.B.Pull, Env: c.B.Env})
	for _, s := range []string{`image: "datadog/agent-dev:x"`, "pull_policy: always", `DD_LOG_LEVEL: "debug"`, `DD_LOGS_CONFIG_TAG_FILTERS: "{\"exclude\":[\"env:*\"]}"`, `DD_LOGS_CONFIG_CONTAINER_COLLECT_ALL: "false"`} {
		if !strings.Contains(override, s) {
			t.Errorf("override missing %s:\n%s", s, override)
		}
	}
}

func TestLoadABConfigErrors(t *testing.T) {
	cases := map[string]string{
		"unknown key":      "profile: p.yaml\nb: { image: x }\nrunz: 3\n",
		"same names":       "profile: p.yaml\na: { name: same }\nb: { name: same, image: x }\n",
		"bad pull":         "profile: p.yaml\nb: { image: x, pull: sometimes }\n",
		"profile list":     "profile: [a, b]\nb: { image: x }\n",
		"unknown signal":   "profile: p.yaml\nb: { image: x }\nthresholds: { cpu: 5, latency: 3 }\n",
		"signal typo":      "profile: p.yaml\nb: { image: x }\nthresholds: { troughput: 5 }\n",
		"no such config":   "",
		"bad threshold ty": "profile: p.yaml\nb: { image: x }\nthresholds: { cpu: fast }\n",
	}
	for name, content := range cases {
		path := writeTemp(t, "aoc.yaml", content)
		if name == "no such config" {
			path = filepath.Join(filepath.Dir(path), "missing.yaml")
		}
		if _, err := loadABConfig(path); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// The image under test and the workload may come from the flags, so the
// config alone is allowed to leave them out — until the run starts.
func TestValidateNamesTheFlags(t *testing.T) {
	c, err := loadABConfig(writeTemp(t, "aoc.yaml", "profile: profiles/baseline.yaml\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = c.validate("aoc.yaml")
	if err == nil || !strings.Contains(err.Error(), "--b") {
		t.Errorf("missing b.image: %v", err)
	}
	c.B.Image = "datadog/agent-dev:x"
	if err := c.validate("aoc.yaml"); err != nil {
		t.Errorf("with --b: %v", err)
	}

	c, err = loadABConfig(writeTemp(t, "aoc.yaml", "b: { image: datadog/agent-dev:x }\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = c.validate("aoc.yaml")
	if err == nil || !strings.Contains(err.Error(), "--workload") {
		t.Errorf("missing workload: %v", err)
	}
}

func TestExperimentName(t *testing.T) {
	cases := []struct{ image, workload, want string }{
		{"datadog/agent-dev:log-tag-filtering-9e35a50b-full", "baseline", "log-tag-filtering-9e35a50b-full-baseline"},
		{"datadog/agent-dev:x-py3", "high-throughput", "x-py3-high-throughput"},
		{"datadog/agent:7", "tag filters", "7-tag-filters"},
		{"localhost:5000/datadog/agent", "baseline", "agent-baseline"}, // a registry port is not a tag
	}
	for _, c := range cases {
		if got := experimentName(c.image, c.workload); got != c.want {
			t.Errorf("experimentName(%q, %q) = %q, want %q", c.image, c.workload, got, c.want)
		}
	}
}

func TestWorkloadRef(t *testing.T) {
	cases := map[string]string{
		"baseline":                   filepath.Join("profiles", "baseline.yaml"),
		"high-throughput":            filepath.Join("profiles", "high-throughput.yaml"),
		"profiles/baseline.yaml":     "profiles/baseline.yaml",
		"tests/tag-filter.yml":       "tests/tag-filter.yml",
		"/tmp/my-workload.yaml":      "/tmp/my-workload.yaml",
		"../elsewhere/workload.yaml": "../elsewhere/workload.yaml",
	}
	for in, want := range cases {
		if got := workloadRef(in); got.Path != want || got.Inline != nil {
			t.Errorf("workloadRef(%q) = %q, want %q", in, got.Path, want)
		}
	}
}

func TestParseKVAndMerge(t *testing.T) {
	filters := `{"exclude":["dirname:*","filename:*"]}`
	got, err := parseKV([]string{"DD_LOGS_CONFIG_TAG_FILTERS=" + filters, "DD_LOG_LEVEL=debug", "EMPTY="})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"DD_LOGS_CONFIG_TAG_FILTERS": filters, "DD_LOG_LEVEL": "debug", "EMPTY": ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseKV = %v, want %v", got, want)
	}
	for _, bad := range []string{"DD_LOG_LEVEL", "=debug", " =x"} {
		if _, err := parseKV([]string{bad}); err == nil {
			t.Errorf("parseKV(%q): expected an error", bad)
		}
	}

	base := map[string]string{"DD_LOG_LEVEL": "info", "KEEP": "1"}
	merged := mergeEnv(base, map[string]string{"DD_LOG_LEVEL": "debug"})
	if merged["DD_LOG_LEVEL"] != "debug" || merged["KEEP"] != "1" {
		t.Errorf("mergeEnv = %v", merged)
	}
	if base["DD_LOG_LEVEL"] != "info" {
		t.Errorf("mergeEnv changed the config's env: %v", base)
	}
}

func TestListFlag(t *testing.T) {
	code := &listFlag{split: true}
	code.Set("pkg/logs/sender, pkg/logs/client")
	code.Set("comp/logs")
	if want := []string{"pkg/logs/sender", "pkg/logs/client", "comp/logs"}; !reflect.DeepEqual(code.vals, want) {
		t.Errorf("--code = %v, want %v", code.vals, want)
	}
	// A feature flag's JSON holds commas; --b-env never splits.
	env := &listFlag{}
	env.Set(`DD_LOGS_CONFIG_TAG_FILTERS={"exclude":["a","b"]}`)
	if len(env.vals) != 1 {
		t.Errorf("--b-env = %v", env.vals)
	}
}

func TestThresholdSummary(t *testing.T) {
	c, err := loadABConfig(writeTemp(t, "aoc.yaml", "profile: p.yaml\nb: { image: x }\nthreshold: 10\nthresholds: { cpu: 5, memory: 2.5 }\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := "throughput 10% · saturation 10% · cpu 5% · memory 2.5%"
	if got := c.thresholdSummary(); got != want {
		t.Errorf("thresholds = %q, want %q", got, want)
	}
}

func TestPackagingNote(t *testing.T) {
	release := Variant{Name: "release", Image: "datadog/agent:7"}
	full := Variant{Name: "tagfilter", Image: "datadog/agent-dev:log-tag-filtering-9e35a50b-full"}
	note := packagingNote(release, full)
	if !strings.Contains(note, "tagfilter is a full image") || !strings.Contains(note, "release is not") {
		t.Errorf("mismatch note = %q", note)
	}
	if note := packagingNote(full, release); !strings.Contains(note, "tagfilter is a full image") {
		t.Errorf("order matters: %q", note)
	}
	if note := packagingNote(release, Variant{Name: "b", Image: "datadog/agent-dev:x-py3"}); note != "" {
		t.Errorf("same packaging: %q", note)
	}
	if note := packagingNote(Variant{Name: "a", Image: "datadog/agent:7-jmx"}, Variant{Name: "b", Image: "datadog/agent-dev:x-jmx"}); note != "" {
		t.Errorf("both jmx: %q", note)
	}
	if note := packagingNote(release, Variant{Name: "b", Image: "datadog/agent-dev:x-jmx"}); !strings.Contains(note, "b is a jmx image") {
		t.Errorf("jmx note = %q", note)
	}
}

// The results keep the config as it ran: loading it back must give the
// same test, so --compare-only and --only work from it.
func TestEffectiveConfigRoundTrip(t *testing.T) {
	prof := writeTemp(t, "baseline.yaml", "name: baseline\nduration: 2m\ngenerators:\n  gen-plain: { streams: 4, rate: 100 }\n")
	c, err := loadABConfig(writeTemp(t, "aoc.yaml", "a: { image: datadog/agent:7 }\nenv: { DD_LOG_LEVEL: info }\nthresholds: { cpu: 5 }\nruns: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	// What the flags do to it.
	c.B.Image = "datadog/agent-dev:log-tag-filtering-9e35a50b-full"
	c.Profile = workloadRef(prof)
	c.Focus = "does the tag filter cost CPU?"
	c.Code = []string{"comp/logs-library/tagfilter"}
	c.Watch = []string{"tag bytes per log"}
	c.B.Env = mergeEnv(c.B.Env, map[string]string{"DD_LOGS_CONFIG_TAG_FILTERS": `{"exclude":["dirname:*"]}`})
	c.Env = mergeEnv(c.Env, map[string]string{"DD_LOGS_CONFIG_CONTAINER_COLLECT_ALL": "false"})
	parallel := false
	c.Parallel = &parallel
	c.Name = experimentName(c.B.Image, "baseline")
	if err := c.validate("aoc.yaml"); err != nil {
		t.Fatal(err)
	}

	eff, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := loadABConfig(writeTemp(t, "effective.yaml", string(eff)))
	if err != nil {
		t.Fatalf("saved config does not load: %v\n%s", err, eff)
	}
	if !reflect.DeepEqual(got, c) {
		t.Errorf("round trip changed the test:\n got %+v\nwant %+v\n%s", got, c, eff)
	}
	if got.Name != "log-tag-filtering-9e35a50b-full-baseline" || got.Focus != c.Focus {
		t.Errorf("name/focus: %q %q", got.Name, got.Focus)
	}
	p, _, err := got.workload()
	if err != nil {
		t.Fatal(err)
	}
	if runs := got.plan(got.Name); len(runs) != 4 || p.Name != "baseline" {
		t.Errorf("saved config reruns differently: %d runs, workload %s", len(runs), p.Name)
	}
	// An inline workload survives the same trip.
	c.Profile = ProfileRef{Inline: p}
	eff, err = yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	inline, err := loadABConfig(writeTemp(t, "inline.yaml", string(eff)))
	if err != nil {
		t.Fatalf("saved inline workload does not load: %v\n%s", err, eff)
	}
	ip, _, err := inline.workload()
	if err != nil {
		t.Fatal(err)
	}
	if ip.Name != "baseline" || ip.Duration != p.Duration || len(ip.Generators) != len(p.Generators) {
		t.Errorf("inline workload: %+v", ip)
	}
}

// Old results directories hold a copy of the aoc.yaml that ran; they must
// keep loading, for --compare-only.
func TestLoadsOldSavedConfig(t *testing.T) {
	old := `name: tagfilter
profile: profiles/baseline.yaml
duration: 10m
a: { image: datadog/agent:7, name: release }
b: { image: datadog/agent-dev:log-tag-filtering-9e35a50b-full, name: tagfilter, pull: never, env: { DD_LOGS_CONFIG_TAG_FILTERS: '{"exclude":["env:*"]}' } }
`
	c, err := loadABConfig(writeTemp(t, "aoc.yaml", old))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.validate("aoc.yaml"); err != nil {
		t.Fatal(err)
	}
	if c.Name != "tagfilter" || c.Duration.D() != 10*time.Minute || c.B.Pull != "never" {
		t.Errorf("old config: %+v", c)
	}
	if runs := c.plan(c.Name); len(runs) != 2 || runs[1].dir != filepath.Join("results", "tagfilter", "tagfilter") {
		t.Errorf("plan: %+v", runs)
	}
}
